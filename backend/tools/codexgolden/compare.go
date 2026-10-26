//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// compareMain implements `codexgolden compare`: it reports wire differences
// between a recorded baseline and a candidate recording. Dynamic IDs and clocks
// are normalized while preserving identity relationships, body values, header
// order, compression, and TLS ClientHello structure.
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
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "compare: baseline has no scenario recordings")
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
			fmt.Fprintf(os.Stderr, "compare: %s: baseline missing/unreadable: %v\n", name, err)
			diffs++
			continue
		}
		cand, err := readJSONFile(filepath.Join(*candidate, name))
		if err != nil {
			fmt.Fprintf(os.Stderr, "compare: %s: candidate missing/unreadable: %v\n", name, err)
			diffs++
			continue
		}
		if !tlsStructuresEqual(base, cand) {
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
		key := recordKey{Conn: rec.Conn, Seq: rec.Seq}
		if _, exists := out[key]; exists {
			return nil, fmt.Errorf("duplicate record conn=%d seq=%d", rec.Conn, rec.Seq)
		}
		out[key] = rec
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("empty recording")
	}
	return out, nil
}

func compareRecords(file string, base, cand map[recordKey]record, verbose bool) int {
	a, b := normalizedRecordGroups(base), normalizedRecordGroups(cand)
	diffs := 0
	for key, values := range a {
		if string(mustJSON(values)) != string(mustJSON(b[key])) {
			fmt.Fprintf(os.Stderr, "compare: %s %s: %s\n", file, key, firstJSONDifference(values, b[key]))
			diffs++
		} else if verbose {
			fmt.Printf("%s %s: %d requests match\n", file, key, len(values))
		}
	}
	for key := range b {
		if _, ok := a[key]; !ok {
			fmt.Fprintf(os.Stderr, "compare: %s %s: extra request group\n", file, key)
			diffs++
		}
	}
	return diffs
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

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}
