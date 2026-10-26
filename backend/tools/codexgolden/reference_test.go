//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/codexwire"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

const reference164 = "../../internal/pkg/codexwire/testdata/golden/0.164.0"

func referenceRecords(t *testing.T, name string) []record {
	t.Helper()
	records, err := readRecordMap(filepath.Join(reference164, name+".jsonl"))
	require.NoError(t, err)
	out := make([]record, 0, len(records))
	for _, rec := range records {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

func TestOfficial164ScenarioContracts(t *testing.T) {
	meta, err := readJSONFile(filepath.Join(reference164, "meta.json"))
	require.NoError(t, err)
	m := meta.(map[string]any)
	require.Equal(t, "codex-cli 0.164.0", m["codex_version"])
	require.Equal(t, "6aaac694803004a63c6f393d153c96d22521a6b9", m["source_commit"])
	require.Equal(t, "b93f6811d3f653f0fb83be4050d857fb9abbb111dfa2ce160563327daff7f306", m["binary_sha256"])
	require.Equal(t, true, m["strace_guard"])
	require.Empty(t, m["outbound_leaks"])

	for _, name := range []string{"ws_initial", "ws_tool_loop", "http_fallback_tool_loop", "http_unauthorized_refresh", "ws_unauthorized_refresh", "ws_compaction"} {
		t.Run(name, func(t *testing.T) {
			recs := referenceRecords(t, name)
			sc := scenario{Name: name, RejectWebSocket: strings.HasPrefix(name, "http_"), ToolLoop: strings.Contains(name, "tool_loop") || name == "ws_compaction", UnauthorizedUntilRefresh: strings.Contains(name, "unauthorized"), Compaction: name == "ws_compaction"}
			require.NoError(t, validateScenario(sc, recs))
			var ws, httpTurns []record
			var refresh, models, accounts, analytics int
			var toolOutput, compactTrigger, compactResult bool
			for _, r := range recs {
				switch {
				case r.Kind == "ws_message":
					ws = append(ws, r)
				case r.Method == "POST" && strings.HasSuffix(r.Target, "/codex/responses"):
					httpTurns = append(httpTurns, r)
					require.Equal(t, "zstd", r.BodyEncoding)
				case strings.HasSuffix(r.Target, "/oauth/token"):
					refresh++
					require.Contains(t, string(r.BodyJSON)+r.BodyText, "rt-golden")
				case strings.HasSuffix(r.Target, "/wham/accounts/check"):
					accounts++
				case strings.Contains(r.Target, "/codex/models?client_version=0.164.0"):
					models++
				case strings.HasSuffix(r.Target, "/codex/analytics-events/events"):
					analytics++
				}
				if len(r.BodyJSON) == 0 {
					continue
				}
				var body map[string]any
				require.NoError(t, json.Unmarshal(r.BodyJSON, &body))
				input, _ := body["input"].([]any)
				for _, item := range input {
					value, _ := item.(map[string]any)
					switch value["type"] {
					case "function_call_output":
						toolOutput = true
						require.Contains(t, value["call_id"], "call_golden_")
						require.Contains(t, value["output"], "golden")
					case "compaction_trigger":
						compactTrigger = true
					case "compaction":
						compactResult = true
					}
				}
			}
			require.Positive(t, accounts)
			require.Positive(t, models)
			require.Positive(t, analytics)
			if strings.HasPrefix(name, "ws_") {
				require.GreaterOrEqual(t, len(ws), 2)
				var first map[string]any
				require.NoError(t, json.Unmarshal(ws[0].BodyJSON, &first))
				require.Equal(t, false, first["generate"])
				for _, r := range ws {
					require.Equal(t, ws[0].Conn, r.Conn, "prewarm and turns reuse one WebSocket")
					require.True(t, r.Frame.Masked)
					require.True(t, r.Frame.Compressed)
				}
				require.Empty(t, httpTurns)
			} else {
				require.Empty(t, ws)
				require.GreaterOrEqual(t, len(httpTurns), 2)
			}
			if strings.Contains(name, "tool_loop") {
				require.True(t, toolOutput)
			}
			if strings.Contains(name, "unauthorized") {
				require.Equal(t, 1, refresh)
			} else {
				require.Zero(t, refresh)
			}
			if name == "ws_compaction" {
				require.True(t, compactTrigger)
				require.True(t, compactResult)
				var last map[string]any
				require.NoError(t, json.Unmarshal(ws[len(ws)-1].BodyJSON, &last))
				require.NotContains(t, last, "previous_response_id", "compaction starts a fresh context window")
			}
		})
	}
}

func TestOfficial164HeaderTemplates(t *testing.T) {
	for _, name := range []string{"ws_initial", "ws_tool_loop", "http_fallback_tool_loop", "http_unauthorized_refresh", "ws_unauthorized_refresh", "ws_compaction"} {
		t.Run(name, func(t *testing.T) {
			checked := 0
			for _, r := range referenceRecords(t, name) {
				if r.Kind != "ws_handshake" && !(r.Kind == "http_request" && strings.Contains(r.Target, "/codex/responses")) {
					continue
				}
				checked++
				headers := make(http.Header)
				var want []string
				for _, h := range r.Headers {
					want = append(want, h.Name)
					lower := strings.ToLower(h.Name)
					if lower == "host" || lower == "content-length" {
						continue
					}
					if r.Kind == "ws_handshake" && (lower == "connection" || lower == "upgrade" || strings.HasPrefix(lower, "sec-websocket-")) {
						continue
					}
					headers.Add(h.Name, h.Value)
				}
				var got []string
				if r.Kind == "ws_handshake" {
					ordered, leftover := codexwire.SplitCodexWSHeaders(headers)
					require.Empty(t, leftover)
					raw, _, err := codexwire.BuildWSUpgradeRequest("wss://localhost/backend-api/codex/responses", ordered, "AAAAAAAAAAAAAAAAAAAAAA==")
					require.NoError(t, err)
					for _, line := range strings.Split(string(raw), "\r\n")[1:] {
						if line == "" {
							break
						}
						header, _, _ := strings.Cut(line, ":")
						got = append(got, header)
					}
				} else {
					for _, h := range codexwire.OrderCodexHTTPHeaders(headers) {
						got = append(got, h.Name)
					}
					got = append(got, "host", "content-length")
				}
				require.Equal(t, want, got)
			}
			require.Positive(t, checked)
		})
	}
}

func TestOfficial164TLSProfiles(t *testing.T) {
	for _, tc := range []struct {
		profile, file, lane string
		alpn                bool
	}{
		{"openssl36", "tls_clienthellos.json", "openssl", false},
		{"rustls", "tls_clienthellos.json", "rustls", false},
		{"rustls-h2", "tls_clienthellos_custom_ca.json", "rustls", true},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			value, err := readJSONFile(filepath.Join(reference164, tc.file))
			require.NoError(t, err)
			var expected any
			for _, row := range value.([]any) {
				v := row.(map[string]any)
				alpn, _ := v["alpn"].([]any)
				if v["lane"] != tc.lane || (len(alpn) > 0) != tc.alpn {
					continue
				}
				resumed := false
				for _, ext := range v["extensions"].([]any) {
					if ext == float64(41) {
						resumed = true
					}
				}
				if !resumed {
					expected = row
					break
				}
			}
			require.NotNil(t, expected)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan struct{})
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			base := func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, listener.Addr().String())
			}
			go func() {
				conn, _ := tlsfingerprint.NewDialer(tlsfingerprint.BuiltinProfile(tc.profile), base).DialTLSContext(ctx, "tcp", net.JoinHostPort("localhost", port))
				if conn != nil {
					_ = conn.Close()
				}
				close(done)
			}()
			conn, err := listener.Accept()
			require.NoError(t, err)
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			raw, err := readTLSRecord(bufio.NewReader(conn))
			_ = conn.Close()
			require.NoError(t, err)
			hello, err := parseClientHello(raw)
			require.NoError(t, err)
			var actual any
			require.NoError(t, json.Unmarshal(mustJSON(hello), &actual))
			require.True(t, tlsStructuresEqual([]any{expected}, []any{actual}), "recorded official TLS profile differs from gateway")
			<-done
		})
	}
}

func TestCompareNormalizesRandomnessButRejectsBehaviorChanges(t *testing.T) {
	recs := referenceRecords(t, "ws_tool_loop")
	base := make(map[recordKey]record)
	for _, r := range recs {
		base[recordKey{r.Conn, r.Seq}] = r
	}
	raw := string(mustJSON(recs))
	ids := map[string]string{}
	raw = dynamicUUID.ReplaceAllStringFunc(raw, func(id string) string {
		if ids[id] == "" {
			ids[id] = strings.ReplaceAll(id, "a", "b")
		}
		return ids[id]
	})
	var copies []record
	require.NoError(t, json.Unmarshal([]byte(raw), &copies))
	candidate := make(map[recordKey]record)
	for _, r := range copies {
		r.Seq += 50
		r.Conn += 50
		candidate[recordKey{r.Conn, r.Seq}] = r
	}
	require.Zero(t, compareRecords("randomness", base, candidate, false))
	for key, r := range candidate {
		if r.Kind == "ws_message" {
			r.Conn += 100
			candidate[key] = r
			break
		}
	}
	require.Positive(t, compareRecords("changed connection reuse", base, candidate, false))
	for key, r := range candidate {
		if r.Kind == "ws_message" {
			r.BodyJSON = []byte(strings.Replace(string(r.BodyJSON), `"generate":false`, `"generate":true`, 1))
			candidate[key] = r
		}
	}
	require.Positive(t, compareRecords("changed prewarm", base, candidate, false))
	require.Error(t, validateScenario(scenario{Name: "tool", ToolLoop: true}, []record{{Kind: "ws_message", BodyJSON: json.RawMessage(`{"generate":false}`)}}))
	require.False(t, tlsStructuresEqual([]any{}, []any{}))
	t.Run("empty baseline is not success", func(t *testing.T) {
		dir := t.TempDir()
		require.Equal(t, 2, compareMain([]string{"-golden", dir, "-candidate", dir}))
	})
	t.Run("invalid capture is rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.jsonl")
		require.NoError(t, os.WriteFile(path, nil, 0600))
		_, err := readRecordMap(path)
		require.Error(t, err)
	})
}
