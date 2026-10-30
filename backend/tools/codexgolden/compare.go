//go:build linux

package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	baseInfo, baseErr := os.Stat(*golden)
	candInfo, candErr := os.Stat(*candidate)
	if baseErr != nil || candErr != nil || !baseInfo.IsDir() || !candInfo.IsDir() || os.SameFile(baseInfo, candInfo) {
		fmt.Fprintln(os.Stderr, "compare: inputs must be distinct, readable recording directories")
		return 2
	}
	diffs := 0
	invalid := 0
	gatewayCapture := false
	if metadata, err := readJSONFile(filepath.Join(*candidate, "meta.json")); err == nil {
		if fields, ok := metadata.(map[string]any); ok {
			gatewayCapture = fields["recorder"] == "backend/tools/codexgolden gateway"
		}
	}
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
	candidateFiles, err := filepath.Glob(filepath.Join(*candidate, "*.jsonl"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "compare:", err)
		return 2
	}
	names := make(map[string]bool, len(files))
	for _, path := range files {
		names[filepath.Base(path)] = true
	}
	for _, path := range candidateFiles {
		if !names[filepath.Base(path)] {
			fmt.Fprintf(os.Stderr, "compare: unexpected candidate scenario %s\n", filepath.Base(path))
			invalid++
		}
	}
	for _, basePath := range files {
		name := filepath.Base(basePath)
		candPath := filepath.Join(*candidate, name)
		baseRecords, err := readRecordMap(basePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "compare: %s: %v\n", name, err)
			invalid++
			continue
		}
		candRecords, err := readRecordMap(candPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "compare: %s: candidate missing/unreadable: %v\n", name, err)
			invalid++
			continue
		}
		if !gatewayCapture {
			for _, rec := range candRecords {
				if rec.Hop != "" {
					err = errors.New("gateway sender markers require valid gateway meta.json")
					break
				}
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "compare: %s: invalid gateway provenance: %v\n", name, err)
				invalid++
				continue
			}
		}
		if gatewayCapture {
			baseRecords, candRecords, err = gatewayComparisonInputs(baseRecords, candRecords)
			if err != nil {
				fmt.Fprintf(os.Stderr, "compare: %s: invalid gateway provenance: %v\n", name, err)
				invalid++
				continue
			}
		}
		diffs += compareRecords(name, baseRecords, candRecords, *verbose)
	}
	for _, name := range []string{"tls_clienthellos.json", "tls_clienthellos_custom_ca.json"} {
		base, err := readJSONFile(filepath.Join(*golden, name))
		if err != nil {
			fmt.Fprintf(os.Stderr, "compare: %s: baseline missing/unreadable: %v\n", name, err)
			invalid++
			continue
		}
		cand, err := readJSONFile(filepath.Join(*candidate, name))
		if err != nil {
			fmt.Fprintf(os.Stderr, "compare: %s: candidate missing/unreadable: %v\n", name, err)
			invalid++
			continue
		}
		if err := validateTLSRecording(base); err != nil {
			fmt.Fprintf(os.Stderr, "compare: %s: invalid baseline TLS recording: %v\n", name, err)
			invalid++
			continue
		}
		if err := validateTLSRecording(cand); err != nil {
			fmt.Fprintf(os.Stderr, "compare: %s: invalid candidate TLS recording: %v\n", name, err)
			invalid++
			continue
		}
		if !tlsStructuresEqual(base, cand) {
			fmt.Fprintf(os.Stderr, "compare: %s: TLS ClientHello structure differs\n", name)
			diffs++
		} else if *verbose {
			fmt.Printf("%s: TLS ClientHello matches\n", name)
		}
	}
	if invalid > 0 {
		fmt.Fprintf(os.Stderr, "compare: %d invalid input(s); product comparison is incomplete\n", invalid)
		return 2
	}
	if diffs > 0 {
		fmt.Fprintf(os.Stderr, "compare: %d difference group(s)\n", diffs)
		return 1
	}
	fmt.Println("compare: no differences")
	return 0
}

func gatewayCoreTarget(target string) bool {
	path, _, _ := strings.Cut(target, "?")
	return path == "/backend-api/codex/models" || path == "/backend-api/codex/responses" || path == "/oauth/token"
}

func gatewayDriverTarget(target string) bool {
	path, _, _ := strings.Cut(target, "?")
	return strings.HasPrefix(path, "/backend-api/ps/") || path == "/backend-api/plugins/featured" || path == "/backend-api/wham/accounts/check" || path == "/backend-api/wham/settings/user" || path == "/backend-api/codex/analytics-events/events"
}

// The test-driver CLI needs its own bootstrap services to reach response.create.
// Those direct loopback calls never enter sub2api and cannot establish product
// coverage (in particular, its AuthManager does not own gateway token refresh).
// Compare the official projection for the actual gateway paths, with ONE ID
// map across every gateway-owned endpoint. Keep all raw records and reject core
// bypasses; never drop an unexpected request sent by the gateway itself.
func gatewayComparisonInputs(reference, candidate map[recordKey]record) (map[recordKey]record, map[recordKey]record, error) {
	base := make(map[recordKey]record)
	for key, rec := range reference {
		if gatewayCoreTarget(rec.Target) || rec.Kind == "ws_message" {
			base[key] = rec
		} else if !gatewayDriverTarget(rec.Target) {
			return nil, nil, fmt.Errorf("unknown reference endpoint %s", rec.Target)
		}
	}
	actual := make(map[recordKey]record)
	for key, rec := range candidate {
		switch rec.Hop {
		case "gateway_upstream":
			actual[key] = rec
		case "cli_bootstrap":
			if gatewayCoreTarget(rec.Target) || rec.Kind == "ws_message" {
				return nil, nil, errors.New("core traffic bypassed the gateway")
			}
			if !gatewayDriverTarget(rec.Target) {
				return nil, nil, fmt.Errorf("unknown test-driver endpoint %s", rec.Target)
			}
		default:
			return nil, nil, fmt.Errorf("missing/unknown sender for record %d", rec.Seq)
		}
	}
	if len(base) == 0 || len(actual) == 0 {
		return nil, nil, errors.New("empty gateway comparison projection")
	}
	return base, actual, nil
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
		if err := validateRecord(rec); err != nil {
			return nil, fmt.Errorf("record %d: %w", len(out)+1, err)
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

func validateRecord(rec record) error {
	if rec.Seq <= 0 || rec.Conn <= 0 || rec.ConnReq < 0 || rec.BodyWireLen < 0 {
		return errors.New("invalid sequence, connection, request index or wire length")
	}
	switch rec.Kind {
	case "http_request", "ws_handshake":
		if rec.ConnReq == 0 || rec.Method == "" || rec.Target == "" || rec.Proto == "" || len(rec.Headers) == 0 || rec.Frame != nil {
			return errors.New("incomplete HTTP request/handshake")
		}
	case "ws_message":
		if rec.Frame == nil || rec.Frame.Fragments <= 0 || rec.Frame.WireLen <= 0 || len(rec.BodyJSON) == 0 || string(rec.BodyJSON) == "null" {
			return errors.New("incomplete WebSocket message")
		}
	default:
		return fmt.Errorf("unknown record kind %q", rec.Kind)
	}
	if len(rec.BodyJSON) > 0 {
		var body map[string]any
		if err := json.Unmarshal(rec.BodyJSON, &body); err != nil || body == nil {
			return errors.New("request JSON must be an object")
		}
	}
	for _, header := range rec.Headers {
		switch strings.ToLower(header.Name) {
		case "content-length":
			length, err := strconv.Atoi(header.Value)
			if err != nil || length != rec.BodyWireLen {
				return errors.New("Content-Length disagrees with recorded wire body length")
			}
		case "sec-websocket-key":
			key, err := base64.StdEncoding.DecodeString(header.Value)
			if err != nil || len(key) != 16 {
				return errors.New("invalid WebSocket nonce")
			}
		}
	}
	return nil
}

func validateTLSRecording(value any) error {
	rows, ok := value.([]any)
	if !ok || len(rows) == 0 {
		return errors.New("empty or invalid TLS recording")
	}
	for i, row := range rows {
		fields, ok := row.(map[string]any)
		if !ok {
			return fmt.Errorf("TLS record %d is not an object", i)
		}
		for _, key := range []string{"lane", "record_version", "legacy_version", "session_id_len", "cipher_suites", "compression_methods", "extensions", "status_request"} {
			if _, ok := fields[key]; !ok {
				return fmt.Errorf("TLS record %d missing %s", i, key)
			}
		}
		var hello clientHello
		if err := json.Unmarshal(mustJSON(fields), &hello); err != nil {
			return err
		}
		if (hello.Lane != "openssl" && hello.Lane != "rustls") || hello.RecordVersion == 0 || hello.LegacyVersion == 0 || len(hello.CipherSuites) == 0 || len(hello.Compression) == 0 || len(hello.Extensions) == 0 {
			return fmt.Errorf("TLS record %d has an incomplete ClientHello", i)
		}
	}
	return nil
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
	data, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Errorf("cannot encode comparison value: %w", err))
	}
	return data
}
