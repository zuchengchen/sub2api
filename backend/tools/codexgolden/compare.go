//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// compareMain implements `codexgolden compare`: it reports wire differences
// between a recorded baseline and a candidate recording. Dynamic IDs, clocks
// and payload values are ignored; header name/order, body top-level key order,
// and TLS ClientHello structure are compared.
func compareMain(args []string) int {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	golden := fs.String("golden", "", "baseline golden directory (required)")
	candidate := fs.String("candidate", "", "candidate golden directory (required)")
	verbose := fs.Bool("v", false, "also print records that match")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *golden == "" || *candidate == "" {
		fmt.Fprintln(os.Stderr, "compare: -golden and -candidate are required")
		return 2
	}
	diffs := 0
	files, err := filepath.Glob(filepath.Join(*golden, "*.jsonl"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "compare:", err)
		return 2
	}
	sort.Strings(files)
	for _, basePath := range files {
		name := filepath.Base(basePath)
		candPath := filepath.Join(*candidate, name)
		baseRecords, err := readRecordMap(basePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "compare: %s: %v\n", name, err)
			diffs++
			continue
		}
		candRecords, err := readRecordMap(candPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "compare: %s: candidate missing/unreadable: %v\n", name, err)
			diffs++
			continue
		}
		diffs += compareRecords(name, baseRecords, candRecords, *verbose)
	}
	for _, name := range []string{"tls_clienthellos.json", "tls_clienthellos_custom_ca.json"} {
		base, err := readJSONFile(filepath.Join(*golden, name))
		if err != nil {
			continue
		}
		cand, err := readJSONFile(filepath.Join(*candidate, name))
		if err != nil {
			fmt.Fprintf(os.Stderr, "compare: %s: candidate missing/unreadable: %v\n", name, err)
			diffs++
			continue
		}
		if !jsonEqualIgnoringRecordedAt(base, cand) {
			fmt.Fprintf(os.Stderr, "compare: %s: TLS ClientHello structure differs\n", name)
			diffs++
		} else if *verbose {
			fmt.Printf("%s: TLS ClientHello matches\n", name)
		}
	}
	if diffs > 0 {
		fmt.Fprintf(os.Stderr, "compare: %d difference group(s)\n", diffs)
		return 1
	}
	fmt.Println("compare: no differences")
	return 0
}

type recordKey struct {
	Conn int
	Seq  int
}

func readRecordMap(path string) (map[recordKey]record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	out := map[recordKey]record{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		var rec record
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			return nil, err
		}
		out[recordKey{Conn: rec.Conn, Seq: rec.Seq}] = rec
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func compareRecords(file string, base, cand map[recordKey]record, verbose bool) int {
	diffs := 0
	keys := make([]recordKey, 0, len(base))
	for key := range base {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Conn != keys[j].Conn {
			return keys[i].Conn < keys[j].Conn
		}
		return keys[i].Seq < keys[j].Seq
	})
	for _, key := range keys {
		b := base[key]
		c, ok := cand[key]
		if !ok {
			fmt.Fprintf(os.Stderr, "compare: %s conn=%d seq=%d: missing in candidate\n", file, key.Conn, key.Seq)
			diffs++
			continue
		}
		if b.Kind != c.Kind || b.Method != c.Method || b.Target != c.Target {
			fmt.Fprintf(os.Stderr, "compare: %s seq=%d: kind/method/target %s %s %s -> %s %s %s\n",
				file, b.Seq, b.Kind, b.Method, b.Target, c.Kind, c.Method, c.Target)
			diffs++
		}
		baseHeaders := headerNames(b.Headers)
		candHeaders := headerNames(c.Headers)
		if strings.Join(baseHeaders, "\n") != strings.Join(candHeaders, "\n") {
			fmt.Fprintf(os.Stderr, "compare: %s seq=%d: header order differs\n  base: %v\n  cand: %v\n",
				file, b.Seq, baseHeaders, candHeaders)
			diffs++
		}
		baseKeys := bodyTopLevelKeys(b.BodyJSON)
		candKeys := bodyTopLevelKeys(c.BodyJSON)
		if strings.Join(baseKeys, "\n") != strings.Join(candKeys, "\n") {
			fmt.Fprintf(os.Stderr, "compare: %s seq=%d: body top-level key order differs\n  base: %v\n  cand: %v\n",
				file, b.Seq, baseKeys, candKeys)
			diffs++
		}
		if verbose && b.Kind == c.Kind && b.Target == c.Target {
			fmt.Printf("%s seq=%d: match\n", file, b.Seq)
		}
	}
	for key := range cand {
		if _, ok := base[key]; !ok {
			fmt.Fprintf(os.Stderr, "compare: %s conn=%d seq=%d: extra record in candidate\n", file, key.Conn, key.Seq)
			diffs++
		}
	}
	return diffs
}

func headerNames(headers []headerKV) []string {
	out := make([]string, 0, len(headers))
	for _, header := range headers {
		name := strings.ToLower(header.Name)
		// Host/port and content length vary with the fake listener.
		if name == "host" || name == "content-length" || name == "date" {
			continue
		}
		out = append(out, name)
	}
	return out
}

func bodyTopLevelKeys(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil
	}
	keys := make([]string, 0, 16)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return keys
		}
		s, _ := key.(string)
		keys = append(keys, s)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return keys
		}
	}
	return keys
}

func readJSONFile(path string) (any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func jsonEqualIgnoringRecordedAt(a, b any) bool {
	return string(mustJSON(stripVolatileJSON(a))) == string(mustJSON(stripVolatileJSON(b)))
}

func stripVolatileJSON(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, child := range v {
			switch key {
			case "recorded_at", "seq", "conn", "conn_request_index":
				continue
			}
			out[key] = stripVolatileJSON(child)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = stripVolatileJSON(child)
		}
		return out
	default:
		return value
	}
}

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}
