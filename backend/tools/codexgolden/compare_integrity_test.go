//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHelloAggregationPreservesEveryNonRandomField(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*clientHello)
		want   int
	}{
		{"record_version", func(h *clientHello) { h.RecordVersion++ }, 2},
		{"legacy_version", func(h *clientHello) { h.LegacyVersion++ }, 2},
		{"session_length", func(h *clientHello) { h.SessionIDLen++ }, 2},
		{"sni", func(h *clientHello) { h.ServerName = "other" }, 2},
		{"versions", func(h *clientHello) { h.SupportedVersions = []uint16{0x0303} }, 2},
		{"hybrid_reuse", func(h *clientHello) { v := true; h.HybridReusesX25519 = &v }, 2},
		{"rustls_random_order", func(h *clientHello) { h.Extensions = []uint16{13, 10, 0} }, 1},
		{"random_bytes", func(h *clientHello) { h.Raw = "different randomness" }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := clientHello{Lane: "rustls", RecordVersion: 0x0301, LegacyVersion: 0x0303, SessionIDLen: 32, ServerName: "localhost", SupportedVersions: []uint16{0x0304, 0x0303}, Extensions: []uint16{0, 10, 13}}
			other := base
			tc.mutate(&other)
			set := newHelloSet()
			set.add(&base)
			set.add(&other)
			if len(set.export()) != tc.want {
				t.Fatal("aggregation discarded a structural difference")
			}
		})
	}
	set := newHelloSet()
	set.add(&clientHello{Lane: "openssl", Extensions: []uint16{0, 10, 13}})
	set.add(&clientHello{Lane: "openssl", Extensions: []uint16{13, 10, 0}})
	if len(set.export()) != 2 {
		t.Fatal("OpenSSL extension order must remain observable")
	}
}

func comparisonRecord(body string) map[recordKey]record {
	return map[recordKey]record{{Conn: 1, Seq: 1}: {
		Seq: 1, Conn: 1, ConnReq: 1, Kind: "ws_message", BodyJSON: json.RawMessage(body),
		Frame: &frameInfo{Opcode: 1, Fragments: 1, Masked: true, Compressed: true, WireLen: len(body)},
	}}
}

func TestCompareIndependentStartupModelsPreservesIdentityAndConnections(t *testing.T) {
	makeRecords := func(reverse bool) map[recordKey]record {
		bootstrap := record{Seq: 1, Conn: 11, ConnReq: 1, Kind: "http_request", Method: "GET", Target: "/backend-api/codex/models?client_version=0.166.0", Proto: "HTTP/2.0", Headers: []headerKV{{Name: "user-agent", Value: "codex_exec/0.166.0 terminal"}}}
		initialized := bootstrap
		initialized.Seq, initialized.Conn = 2, 22
		initialized.Headers = []headerKV{{Name: "user-agent", Value: "codex_exec/0.166.0 terminal (codex_exec; 0.166.0)"}}
		if reverse {
			bootstrap.Seq, initialized.Seq = 2, 1
		}
		return map[recordKey]record{
			{Conn: 11, Seq: bootstrap.Seq}:   bootstrap,
			{Conn: 22, Seq: initialized.Seq}: initialized,
			{Conn: 22, Seq: 3}:               {Seq: 3, Conn: 22, ConnReq: 2, Kind: "http_request", Method: "POST", Target: "/backend-api/codex/responses", BodyJSON: json.RawMessage(`{"model":"gpt-5.5"}`)},
		}
	}
	base := makeRecords(false)
	require.Zero(t, compareRecords("independent models arrival", base, makeRecords(true), false))
	for _, mutation := range []string{"header", "missing", "extra", "shared connection", "later reuse", "not first request"} {
		t.Run(mutation, func(t *testing.T) {
			candidate := makeRecords(true)
			key := recordKey{Conn: 11, Seq: 2}
			r := candidate[key]
			switch mutation {
			case "header":
				r.Headers[0].Value += " changed"
			case "missing":
				delete(candidate, key)
			case "extra":
				extra := r
				extra.Conn, extra.Seq = 33, 4
				candidate[recordKey{Conn: 33, Seq: 4}] = extra
			case "shared connection":
				r.Conn, r.ConnReq = 22, 2
			case "later reuse":
				laterKey := recordKey{Conn: 22, Seq: 3}
				later := candidate[laterKey]
				later.Conn = 11
				candidate[laterKey] = later
			case "not first request":
				r.ConnReq = 2
			}
			if mutation != "missing" {
				candidate[key] = r
			}
			require.Positive(t, compareRecords(mutation, base, candidate, false))
		})
	}
}

func TestComparePreservesUserContentAndIdentityShape(t *testing.T) {
	const first = "01900000-1234-7123-8123-123456789abc"
	const second = "01900001-5678-7456-a456-abcdef123456"
	for _, body := range []string{
		`{"input":[{"role":"user","content":[{"type":"input_text","text":"` + first + `"}]}]}`,
		`{"input":[{"type":"function_call_output","output":"` + first + `"}]}`,
		`{"input":[{"type":"function_call","arguments":"{\"thread_id\":\"` + first + `\",\"created_at\":123}"}]}`,
	} {
		require.Positive(t, compareRecords("user UUID", comparisonRecord(body), comparisonRecord(strings.ReplaceAll(body, first, second)), false))
	}
	base := `{"prompt_cache_key":"` + first + `","client_metadata":{"thread_id":"` + first + `","x-codex-window-id":"` + first + `:0"}}`
	require.Zero(t, compareRecords("fresh identity", comparisonRecord(base), comparisonRecord(strings.ReplaceAll(base, first, second)), false))
	require.Positive(t, compareRecords("UUID version", comparisonRecord(base), comparisonRecord(strings.ReplaceAll(base, first, "01900001-5678-4456-a456-abcdef123456")), false))
	require.Positive(t, compareRecords("UUID variant", comparisonRecord(base), comparisonRecord(strings.ReplaceAll(base, first, "01900001-5678-7456-7456-abcdef123456")), false))
	require.Positive(t, compareRecords("identity linkage", comparisonRecord(base), comparisonRecord(strings.Replace(base, first, second, 1)), false))
	for _, text := range []string{"<current_date>2026-10-29</current_date>", "{\"created_at\":123}", "Chunk ID: 123abc\nWall time: 0.1 seconds"} {
		body, err := json.Marshal(map[string]any{"input": []any{map[string]any{"role": "user", "content": text}}})
		require.NoError(t, err)
		changed := strings.ReplaceAll(string(body), "123", "456")
		changed = strings.ReplaceAll(changed, "2026-10-29", "2026-10-30")
		require.Positive(t, compareRecords("normal text collision", comparisonRecord(string(body)), comparisonRecord(changed), false))
	}
}

func TestCompareRejectsInvalidEvidence(t *testing.T) {
	for _, contents := range []string{"", "{}\n", "null\n", "{broken}\n", `{"seq":1,"conn":1,"kind":"ws_message","body_json":null}` + "\n"} {
		path := filepath.Join(t.TempDir(), "invalid.jsonl")
		require.NoError(t, os.WriteFile(path, []byte(contents), 0600))
		_, err := readRecordMap(path)
		require.Error(t, err, contents)
	}
	require.False(t, tlsStructuresEqual([]any{map[string]any{}}, []any{map[string]any{}}))
	for _, fault := range []string{"missing scenario", "extra scenario", "broken TLS", "same directory"} {
		t.Run(fault, func(t *testing.T) {
			base, candidate := t.TempDir(), t.TempDir()
			for _, name := range []string{"ws_initial.jsonl", "tls_clienthellos.json", "tls_clienthellos_custom_ca.json"} {
				data, err := os.ReadFile(filepath.Join(reference164, name))
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(base, name), data, 0600))
				require.NoError(t, os.WriteFile(filepath.Join(candidate, name), data, 0600))
			}
			switch fault {
			case "missing scenario":
				require.NoError(t, os.Remove(filepath.Join(candidate, "ws_initial.jsonl")))
			case "extra scenario":
				require.NoError(t, os.WriteFile(filepath.Join(candidate, "extra.jsonl"), []byte("{}\n"), 0600))
			case "broken TLS":
				require.NoError(t, os.WriteFile(filepath.Join(candidate, "tls_clienthellos.json"), []byte("[{}]"), 0600))
			case "same directory":
				candidate = base
			}
			require.Equal(t, 2, compareMain([]string{"-golden", base, "-candidate", candidate}), "invalid evidence must be distinct from a product difference (exit 1)")
		})
	}
}

func TestScenarioRejectsFailedToolExecution(t *testing.T) {
	recs := []record{
		{Kind: "ws_message", BodyJSON: json.RawMessage(`{"generate":false}`)},
		{Kind: "ws_message", BodyJSON: json.RawMessage(`{"input":[{"type":"function_call_output","call_id":"call_golden_1","output":"unknown tool exec_command"}]}`)},
	}
	require.Error(t, validateScenario(scenario{Name: "tool", ToolLoop: true}, recs))
}

func TestRecorderNormalizationPreservesFixtureTextCollisions(t *testing.T) {
	sb := &sandbox{root: "/tmp/recording", home: "/tmp/recording/home", codexHome: "/tmp/recording/home/.codex", cwd: "/tmp/recording/work"}
	environment := "<environment_context>\n  <cwd>" + sb.cwd + "</cwd>\n</environment_context>"
	message := func(role, text string) map[string]any {
		return map[string]any{"role": role, "content": []any{map[string]any{"type": "input_text", "text": text}}}
	}
	collision := sb.cwd + " localhost:1234 " + sb.codexHome
	body, err := json.Marshal(map[string]any{"input": []any{
		message("user", environment), message("user", "Reply with ok."),
		message("user", collision), message("user", environment),
		map[string]any{"type": "function_call_output", "output": collision},
		map[string]any{"type": "function_call", "arguments": `{"cwd":"` + sb.cwd + `"}`},
	}})
	require.NoError(t, err)
	recs := []record{{Seq: 1, BodyJSON: body, Headers: []headerKV{{Name: "Host", Value: "localhost:1234"}, {Name: "X-Text", Value: collision}}}}
	got, err := normalizeRecords(recs, sb, 1234)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "<UPSTREAM>", got[0].Headers[0].Value)
	require.Equal(t, collision, got[0].Headers[1].Value)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(got[0].BodyJSON, &decoded))
	input := decoded["input"].([]any)
	text := func(i int) string {
		return input[i].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	}
	require.Contains(t, text(0), "<cwd><CWD></cwd>")
	require.Equal(t, collision, text(2))
	require.Equal(t, environment, text(3))
	require.Equal(t, collision, input[4].(map[string]any)["output"])
	require.Contains(t, input[5].(map[string]any)["arguments"], sb.cwd)
	require.Equal(t, "localhost:1234", recs[0].Headers[0].Value, "do not mutate raw capture")
	_, err = normalizeRecords([]record{{Seq: 2, BodyJSON: json.RawMessage(`{broken`)}}, sb, 1234)
	require.Error(t, err)
}

func TestTraceRequiresExecutedLocalConnections(t *testing.T) {
	local := `123 connect(7, {sa_family=AF_INET, sin_port=htons(44221), sin_addr=inet_addr("127.0.0.1")}, 16) = -1 EINPROGRESS (Operation now in progress)`
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{"local", local, true},
		{"empty", "", false},
		{"signal only", "123 --- SIGURG {si_signo=SIGURG} ---", false},
		{"external", local + "\n" + strings.Replace(local, "127.0.0.1", "203.0.113.1", 1), false},
		{"dns", strings.Replace(local, "44221", "53", 1), false},
		{"corrupt", local + "\n123 connect(7, {sa_family=AF_INET, sin_port=htons(?)}, 16) = 0", false},
		{"continued", local + " <unfinished ...>\n123 <... connect resumed>) = 0", true},
		{"quoted collision", local + "\n123 connect(8, {sa_family=AF_UNIX, sun_path=\"literal sin_addr=inet_addr(\\\"203.0.113.1\\\")\"}, 42) = -1 ENOENT", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "connect.strace")
			require.NoError(t, os.WriteFile(path, []byte(tc.data), 0600))
			require.Equal(t, tc.valid, len(checkTrace(path)) == 0)
		})
	}
	require.NotEmpty(t, checkTrace(filepath.Join(t.TempDir(), "missing")))
}

func TestGatewayProjectionKeepsUnexpectedProductTrafficAndRejectsBypass(t *testing.T) {
	core := record{Seq: 1, Kind: "http_request", Method: "POST", Target: "/backend-api/codex/responses", Hop: "gateway_upstream"}
	aux := record{Seq: 2, Kind: "http_request", Method: "POST", Target: "/backend-api/codex/analytics-events/events", Hop: "cli_bootstrap"}
	base := map[recordKey]record{{Conn: 1, Seq: 1}: core, {Conn: 2, Seq: 2}: aux}
	actual := map[recordKey]record{{Conn: 1, Seq: 1}: core, {Conn: 2, Seq: 2}: aux}
	x, y, err := gatewayComparisonInputs(base, actual)
	require.NoError(t, err)
	require.Len(t, x, 1)
	require.Len(t, y, 1)
	aux.Hop = "gateway_upstream"
	actual[recordKey{Conn: 2, Seq: 2}] = aux
	_, y, err = gatewayComparisonInputs(base, actual)
	require.NoError(t, err)
	require.Len(t, y, 2, "an actual gateway analytics request must remain a product difference")
	core.Hop = "cli_bootstrap"
	actual[recordKey{Conn: 1, Seq: 1}] = core
	_, _, err = gatewayComparisonInputs(base, actual)
	require.ErrorContains(t, err, "bypassed")
	core.Hop = "unidentified"
	actual[recordKey{Conn: 1, Seq: 1}] = core
	_, _, err = gatewayComparisonInputs(base, actual)
	require.Error(t, err)
	core.Hop = "gateway_upstream"
	actual[recordKey{Conn: 1, Seq: 1}] = core
	aux.Target = "/backend-api/unclassified-new-endpoint"
	base[recordKey{Conn: 2, Seq: 2}] = aux
	_, _, err = gatewayComparisonInputs(base, actual)
	require.ErrorContains(t, err, "unknown reference endpoint", "new official calls require an explicit coverage decision")
}
