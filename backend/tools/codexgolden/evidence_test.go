//go:build linux

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Historical synthetic recordings calibrate the verifier only. Current release
// acceptance always runs a newly acquired binary and creates independent files.
func comparatorFixture(t *testing.T, dir string) {
	t.Helper()
	var native []clientHello
	helloData, err := os.ReadFile(filepath.Join(reference164, "tls_clienthellos.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(helloData, &native))
	shapes := map[string]*clientHello{}
	for _, h := range native {
		hello := h
		hello.Raw = ""
		hello.ServerName = "<UPSTREAM>"
		shapes[h.Lane] = &hello
	}
	for _, name := range fullScenarioNames {
		data, err := os.ReadFile(filepath.Join(reference164, name+".jsonl"))
		require.NoError(t, err)
		var normalized bytes.Buffer
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var rec record
			require.NoError(t, json.Unmarshal([]byte(line), &rec))
			// Add explicit synthetic native TLS ownership; these verifier unit
			// fixtures are never used as a current official capture.
			if gatewayCoreTarget(rec.Target) || rec.Kind == "ws_message" {
				lane := "rustls"
				if rec.Kind == "http_request" {
					lane = "openssl"
				}
				rec.TLS = shapes[lane]
			}
			normalized.Write(mustJSON(rec))
			normalized.WriteByte('\n')
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, name+".jsonl"), normalized.Bytes(), 0600))
	}
	for dst, src := range map[string]string{"tls_clienthellos.json": "tls_clienthellos.json", "tls_clienthellos_scenarios.json": "tls_clienthellos.json"} {
		data, err := os.ReadFile(filepath.Join(reference164, src))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, dst), data, 0600))
	}
	writeSyntheticWSLifecycleFixture(t, dir)
	trace := []byte("123 connect(7, {sa_family=AF_INET, sin_port=htons(44221), sin_addr=inet_addr(\"127.0.0.1\")}, 16) = 0\n")
	for _, path := range expectedTracePaths("backend/tools/codexgolden") {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, path), trace, 0600))
	}
	meta := captureMetadata{Contract: recordingContract, Version: "codex-cli 0.164.0", Commit: "6aaac694803004a63c6f393d153c96d22521a6b9", BinaryHash: strings.Repeat("a", 64), ModelsHash: strings.Repeat("b", 64), ConfigHash: strings.Repeat("c", 64), CAHash: strings.Repeat("d", 64), TLSMode: "system_roots_namespace", Model: "gpt-6-astra", Reasoning: "xhigh", RecordedAt: "2026-11-03T00:00:00Z", Recorder: "backend/tools/codexgolden", Scenarios: fullScenarioNames, Strace: true}
	writeJSON(filepath.Join(dir, "meta.json"), meta)
	require.NoError(t, finalizeCaptureTraces(dir))
}

func TestCompareRejectsMixedEnvironmentAndIncompleteTrace(t *testing.T) {
	for _, fault := range []string{"none", "TLS mode", "configuration", "commit", "missing metadata", "truncated metadata", "trace missing", "trace changed", "malformed trace with matching hash", "missing scenario", "source text changed", "map order"} {
		t.Run(fault, func(t *testing.T) {
			base, candidate := t.TempDir(), t.TempDir()
			comparatorFixture(t, base)
			comparatorFixture(t, candidate)
			meta, err := readCaptureMetadata(candidate)
			require.NoError(t, err)
			want := 2
			switch fault {
			case "none":
				want = 0
			case "TLS mode":
				meta.TLSMode = "custom_ca_rustls_h2"
			case "configuration":
				meta.ConfigHash = strings.Repeat("f", 64)
			case "commit":
				meta.Commit = strings.Repeat("f", 40)
			case "missing metadata":
				require.NoError(t, os.Remove(filepath.Join(candidate, "meta.json")))
			case "truncated metadata":
				require.NoError(t, os.WriteFile(filepath.Join(candidate, "meta.json"), []byte(`{"`), 0600))
			case "trace missing":
				require.NoError(t, os.Remove(filepath.Join(candidate, "traces", "tls.strace")))
			case "trace changed":
				require.NoError(t, os.WriteFile(filepath.Join(candidate, "traces", "tls.strace"), []byte("truncated"), 0600))
			case "malformed trace with matching hash":
				bad := []byte("123 <... connect resumed>) = 0\n")
				require.NoError(t, os.WriteFile(filepath.Join(candidate, "traces", "tls.strace"), bad, 0600))
				meta.Traces[filepath.Join("traces", "tls.strace")] = fmt.Sprintf("%x", sha256.Sum256(bad))
			case "missing scenario":
				require.NoError(t, os.Remove(filepath.Join(candidate, "ws_initial.jsonl")))
			case "source text changed":
				path := filepath.Join(candidate, "ws_initial.jsonl")
				raw, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Contains(t, string(raw), "Reply with ok.")
				raw = []byte(strings.ReplaceAll(string(raw), "Reply with ok.", "Reply with changed content."))
				require.NoError(t, os.WriteFile(path, raw, 0600))
				want = 1
			case "map order":
				path := filepath.Join(candidate, "ws_initial.jsonl")
				records, err := readRecordMap(path)
				require.NoError(t, err)
				// Re-encode only an ordinary client_metadata map, preserving typed body
				// key order. Metadata ordering can differ between official Rust runs.
				rows := make([]record, len(records))
				for _, rec := range records {
					rows[rec.Seq-1] = rec
				}
				for i := range rows {
					if rows[i].Kind != "ws_message" {
						continue
					}
					var body map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(rows[i].BodyJSON, &body))
					var metadata map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(body["client_metadata"], &metadata))
					if len(metadata) == 0 {
						continue
					}
					old := string(body["client_metadata"])
					reordered := string(mustJSON(metadata))
					rows[i].BodyJSON = json.RawMessage(strings.Replace(string(rows[i].BodyJSON), old, reordered, 1))
				}
				writeJSONL(path, rows)
				want = 0
			}
			if fault != "missing metadata" && fault != "truncated metadata" {
				writeJSON(filepath.Join(candidate, "meta.json"), meta)
			}
			require.Equal(t, want, compareMain([]string{"-golden", base, "-candidate", candidate}))
		})
	}
}

func TestTraceReassemblesActualSplitSockaddrAndRejectsMissingHalves(t *testing.T) {
	local := "{sa_family=AF_INET, sin_port=htons(12345), sin_addr=inet_addr(\"127.0.0.1\")}, 16) = 0"
	valid := "123 connect(7, <unfinished ...>\n124 connect(8, " + local + "\n123 <... connect resumed>" + local + "\n"
	for _, tc := range []struct {
		name, text string
		valid      bool
	}{
		{"split sockaddr", valid, true},
		{"unmatched resume", "123 <... connect resumed>" + local, false},
		{"missing resume", "124 connect(8, " + local + "\n123 connect(7, <unfinished ...>", false},
		{"external resumed", strings.Replace(valid, "123 <... connect resumed>"+local, "123 <... connect resumed>"+strings.Replace(local, "127.0.0.1", "203.0.113.10", 1), 1), false},
		{"escaped UNIX collision", "124 connect(8, " + local + "\n123 connect(7, {sa_family=AF_UNIX, sun_path=\"literal \\\"connect(\\n)\\\" {sa_family=AF_INET}\"}, 99) = -1 ENOENT", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "trace")
			require.NoError(t, os.WriteFile(p, []byte(tc.text), 0600))
			require.Equal(t, tc.valid, len(checkTrace(p)) == 0)
		})
	}
}
