//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

const fingerprintNativeSession = "019a6400-0000-7000-8000-000000000001"
const fingerprintOutputSession = "019a6400-0000-7000-8000-000000000002"
const fingerprintNativeInstallation = "22222222-2222-4222-8222-222222222222"
const fingerprintNativeTurn = "019a6400-0001-7000-8000-000000000001"
const fingerprintOutputTurn = "019a6400-0001-7000-8000-000000000002"

func fingerprintTestBody(session, installation, turn string, window int, ws bool) []byte {
	cm := map[string]any{"session_id": session, "thread_id": session, "x-codex-installation-id": installation, "x-codex-window-id": fmt.Sprintf("%s:%d", session, window), "turn_id": turn}
	kind := "turn"
	if turn == "" {
		kind = "prewarm"
	}
	cm["x-codex-turn-metadata"] = string(mustJSON(map[string]any{"installation_id": installation, "session_id": session, "thread_id": session, "turn_id": turn, "window_id": cm["x-codex-window-id"], "window_number": window, "request_kind": kind}))
	body := map[string]any{"model": "gpt-6-astra", "client_metadata": cm, "prompt_cache_key": session, "input": []any{map[string]any{"type": "message", "role": "user", "content": "synthetic verifier input"}}}
	if ws {
		body["type"] = "response.create"
	}
	return mustJSON(body)
}
func fingerprintTestHeaders(t *testing.T, body []byte) http.Header {
	t.Helper()
	snapshot, err := gatewayFingerprintBody(body)
	require.NoError(t, err)
	h := http.Header{"Host": []string{"localhost"}}
	h.Set("session-id", snapshot.Metadata["session_id"])
	h.Set("thread-id", snapshot.Metadata["thread_id"])
	h.Set("x-client-request-id", snapshot.Metadata["thread_id"])
	h.Set("x-codex-window-id", snapshot.Metadata["x-codex-window-id"])
	h.Set("x-codex-turn-metadata", string(mustJSON(snapshot.TurnMetadata)))
	return h
}
func fingerprintTestHeaderRows(headers http.Header) []headerKV {
	var result []headerKV
	// Keep a reproducible order, including the lack of an installation header.
	for _, key := range []string{"Host", "session-id", "thread-id", "x-client-request-id", "x-codex-window-id", "x-codex-turn-metadata"} {
		for _, value := range headers.Values(key) {
			result = append(result, headerKV{Name: key, Value: value})
		}
	}
	return result
}
func fingerprintTestFixture(t *testing.T) (gatewayFingerprintEvidence, []record) {
	t.Helper()
	firstNative := fingerprintTestBody(fingerprintNativeSession, fingerprintNativeInstallation, "", 0, true)
	firstOutput := fingerprintTestBody(fingerprintOutputSession, gatewayGoldenInstallationID, "", 0, true)
	httpNative := fingerprintTestBody(fingerprintNativeSession, fingerprintNativeInstallation, fingerprintNativeTurn, 1, false)
	httpOutput := fingerprintTestBody(fingerprintOutputSession, gatewayGoldenInstallationID, fingerprintOutputTurn, 1, false)
	wsInput, err := gatewayIngressFingerprint("ws", fingerprintTestHeaders(t, firstNative), firstNative)
	require.NoError(t, err)
	wsInput.Sequence = 1
	httpInput, err := gatewayIngressFingerprint("http", fingerprintTestHeaders(t, httpNative), httpNative)
	require.NoError(t, err)
	httpInput.Sequence = 2
	evidence := gatewayFingerprintEvidence{Contract: gatewayFingerprintContract, Mode: "full", Seed: gatewayGoldenFingerprintSeed, Ingress: []gatewayFingerprintIngress{wsInput, httpInput}}
	records := []record{
		{Seq: 1, Conn: 1, ConnReq: 1, Hop: "gateway_upstream", Kind: "ws_handshake", Method: "GET", Target: "/backend-api/codex/responses", Proto: "HTTP/1.1", Headers: fingerprintTestHeaderRows(fingerprintTestHeaders(t, firstOutput))},
		{Seq: 2, Conn: 1, Hop: "gateway_upstream", Kind: "ws_message", BodyJSON: firstOutput, Frame: &frameInfo{Opcode: 1, Masked: true, Fragments: 1, WireLen: 1}},
		{Seq: 3, Conn: 1, Hop: "gateway_upstream", Kind: "ws_message", BodyJSON: fingerprintTestBody(fingerprintOutputSession, gatewayGoldenInstallationID, fingerprintOutputTurn, 1, true), Frame: &frameInfo{Opcode: 1, Masked: true, Fragments: 1, WireLen: 1}},
		{Seq: 4, Conn: 2, ConnReq: 1, Hop: "gateway_upstream", Kind: "http_request", Method: "POST", Target: "/backend-api/codex/responses", Proto: "HTTP/1.1", Headers: fingerprintTestHeaderRows(fingerprintTestHeaders(t, httpOutput)), BodyJSON: httpOutput},
	}
	return evidence, records
}
func mutateFingerprintBody(t *testing.T, row *record, change func(map[string]any, map[string]any, map[string]any)) {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(row.BodyJSON, &body))
	cm := body["client_metadata"].(map[string]any)
	var embedded map[string]any
	if text, ok := cm["x-codex-turn-metadata"].(string); ok {
		require.NoError(t, json.Unmarshal([]byte(text), &embedded))
	}
	change(body, cm, embedded)
	if _, ok := cm["x-codex-turn-metadata"]; ok {
		cm["x-codex-turn-metadata"] = string(mustJSON(embedded))
	}
	row.BodyJSON = mustJSON(body)
}
func replaceFingerprintHeader(row *record, key, value string) {
	for i, header := range row.Headers {
		if strings.EqualFold(header.Name, key) {
			row.Headers[i].Value = value
			return
		}
	}
	row.Headers = append(row.Headers, headerKV{Name: key, Value: value})
}

func TestGatewayFingerprintFixtureExplicitSeedAndIdentityOnlyIngress(t *testing.T) {
	account := &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Extra: gatewayFingerprintExtra(map[string]any{"codex_fingerprint_mode": "off", "codex_fingerprint_seed": "", "other": "kept"})}
	require.Equal(t, "full", string(account.GetCodexFingerprintMode()))
	require.Equal(t, gatewayGoldenFingerprintSeed, account.GetExtraString("codex_fingerprint_seed"))
	require.Equal(t, "kept", account.Extra["other"])
	evidence, _ := fingerprintTestFixture(t)
	raw := fingerprintTestBody(fingerprintNativeSession, fingerprintNativeInstallation, "", 0, true)
	headers := fingerprintTestHeaders(t, raw)
	headers.Set("Authorization", "synthetic-secret-must-not-be-saved")
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	body["input"] = "sensitive-prompt-must-not-be-saved"
	body["client_metadata"].(map[string]any)["private"] = "private-metadata-must-not-be-saved"
	snapshot, err := gatewayIngressFingerprint("ws", headers, mustJSON(body))
	require.NoError(t, err)
	encoded := string(mustJSON(snapshot))
	require.NotContains(t, encoded, "must-not-be-saved")
	require.Equal(t, evidence.Ingress[0].Body, snapshot.Body)
	require.NotContains(t, snapshot.Headers.Identity, "x-codex-installation-id", "official Responses does not emit a direct installation header")
}

func TestGatewayFingerprintExecutionPresenceAndBindingCalibration(t *testing.T) {
	cases := []struct {
		name  string
		valid bool
	}{
		{"observed rewrite without installation header", true}, {"optional embedded metadata absent", true},
		{"startup prewarm turn omitted", true}, {"in-turn prewarm turn present", true}, {"reused WS handshake window and turn differ", true}, {"failed retry handshake has no body", true},
		{"missing seed", false}, {"malformed seed", false}, {"different legal seed", false}, {"off label", false}, {"missing ingress", false}, {"zero records", false}, {"only driver records", false},
		{"wrong installation", false}, {"unchanged native session", false}, {"wrong optional installation header", false},
		{"flat embedded divergence", false}, {"wrong session UUID version", false}, {"HTTP header body divergence", false},
		{"missing session header", false}, {"WS without prior handshake", false}, {"session mode root inequality", false}, {"unknown message type", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evidence, records := fingerprintTestFixture(t)
			switch tc.name {
			case "optional embedded metadata absent":
				for i := range records {
					if len(records[i].BodyJSON) > 0 {
						mutateFingerprintBody(t, &records[i], func(_ map[string]any, cm, _ map[string]any) { delete(cm, "x-codex-turn-metadata") })
					}
					filtered := records[i].Headers[:0]
					for _, h := range records[i].Headers {
						if !strings.EqualFold(h.Name, "x-codex-turn-metadata") {
							filtered = append(filtered, h)
						}
					}
					records[i].Headers = filtered
				}
			case "startup prewarm turn omitted":
				mutateFingerprintBody(t, &records[1], func(_ map[string]any, cm, em map[string]any) { delete(cm, "turn_id"); delete(em, "turn_id") })
			case "in-turn prewarm turn present":
				mutateFingerprintBody(t, &records[1], func(_ map[string]any, cm, em map[string]any) {
					cm["turn_id"] = fingerprintOutputTurn
					em["turn_id"] = fingerprintOutputTurn
					em["request_kind"] = "prewarm"
				})
			case "failed retry handshake has no body":
				retry := records[0]
				retry.Conn = 3
				retry.Seq = 1
				for i := range records {
					records[i].Seq++
				}
				records = append(records, retry)
			case "missing seed":
				evidence.Seed = ""
			case "malformed seed":
				evidence.Seed = "not-a-uuid"
			case "different legal seed":
				evidence.Seed = fingerprintNativeInstallation
			case "off label":
				evidence.Mode = "off"
			case "missing ingress":
				evidence.Ingress = nil
			case "zero records":
				records = nil
			case "only driver records":
				for i := range records {
					records[i].Hop = "driver_direct"
				}
			case "wrong installation":
				mutateFingerprintBody(t, &records[1], func(_ map[string]any, cm, em map[string]any) {
					cm["x-codex-installation-id"] = fingerprintNativeInstallation
					em["installation_id"] = fingerprintNativeInstallation
				})
			case "unchanged native session":
				for i := range records {
					raw := string(mustJSON(records[i]))
					raw = strings.ReplaceAll(raw, fingerprintOutputSession, fingerprintNativeSession)
					require.NoError(t, json.Unmarshal([]byte(raw), &records[i]))
				}
			case "wrong optional installation header":
				replaceFingerprintHeader(&records[0], "x-codex-installation-id", fingerprintNativeInstallation)
			case "flat embedded divergence":
				mutateFingerprintBody(t, &records[1], func(_ map[string]any, _ map[string]any, em map[string]any) {
					em["session_id"] = fingerprintNativeSession
				})
			case "wrong session UUID version":
				mutateFingerprintBody(t, &records[1], func(_ map[string]any, cm, _ map[string]any) { cm["session_id"] = fingerprintNativeInstallation })
			case "HTTP header body divergence":
				replaceFingerprintHeader(&records[3], "x-codex-window-id", fingerprintOutputSession+":0")
			case "missing session header":
				replaceFingerprintHeader(&records[0], "session-id", "")
			case "WS without prior handshake":
				records = records[1:]
			case "session mode root inequality":
				mutateFingerprintBody(t, &records[1], func(b, cm, em map[string]any) {
					cm["session_id"] = fingerprintOutputTurn
					em["session_id"] = fingerprintOutputTurn
					b["prompt_cache_key"] = fingerprintOutputTurn
				})
				replaceFingerprintHeader(&records[0], "session-id", fingerprintOutputTurn)
				for i, h := range records[0].Headers {
					if strings.EqualFold(h.Name, "x-codex-turn-metadata") {
						var em map[string]any
						require.NoError(t, json.Unmarshal([]byte(h.Value), &em))
						em["session_id"] = fingerprintOutputTurn
						records[0].Headers[i].Value = string(mustJSON(em))
					}
				}
			case "unknown message type":
				mutateFingerprintBody(t, &records[1], func(b, _, _ map[string]any) { b["type"] = "unrecognized" })
			}
			proof, err := validateGatewayFingerprintExecution(evidence, records)
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 3, proof.BodyBindings)
			require.Equal(t, 2, proof.HeaderBindings)
			require.Equal(t, 3, proof.ChangedSessions)
			require.Equal(t, 3, proof.ChangedInstallations)
			wantHandshakes := 1
			if tc.name == "failed retry handshake has no body" {
				wantHandshakes = 2
			}
			require.Equal(t, wantHandshakes, proof.Handshakes)
		})
	}
}

func fingerprintTestRecordMap(records []record) map[recordKey]record {
	result := map[recordKey]record{}
	for _, row := range records {
		result[recordKey{Conn: row.Conn, Seq: row.Seq}] = row
	}
	return result
}
func writeFingerprintTestProof(t *testing.T, dir string, evidence gatewayFingerprintEvidence, records []record) captureMetadata {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "raw"), 0700))
	var err error
	evidence.Execution, err = validateGatewayFingerprintExecution(evidence, records)
	require.NoError(t, err)
	for _, name := range fullScenarioNames {
		writeJSONL(filepath.Join(dir, "raw", name+".jsonl"), records)
		writeJSON(filepath.Join(dir, name+"-provenance.json"), map[string]any{"scenario": name, "fingerprint_evidence": evidence})
	}
	return refreshFingerprintTestHashes(t, dir)
}
func refreshFingerprintTestHashes(t *testing.T, dir string) captureMetadata {
	t.Helper()
	meta := map[string]any{"recorder": "backend/tools/codexgolden gateway"}
	require.NoError(t, finalizeGatewayFingerprintHashes(dir, meta))
	var typed captureMetadata
	require.NoError(t, json.Unmarshal(mustJSON(meta), &typed))
	return typed
}

func TestGatewayFingerprintReadRecomputesRawAndIngressInsteadOfTrustingCounts(t *testing.T) {
	for _, fault := range []string{"none", "raw missing", "raw hash changed", "provenance missing", "missing hash", "unexpected hash", "claimed counters forged", "old evidence without seed", "rebound raw not rewritten", "rebound ingress already rewritten", "different normalized identity", "only endpoint normalization"} {
		t.Run(fault, func(t *testing.T) {
			evidence, records := fingerprintTestFixture(t)
			dir := t.TempDir()
			meta := writeFingerprintTestProof(t, dir, evidence, records)
			normalized := fingerprintTestRecordMap(records)
			path := filepath.Join(dir, "ws_initial-provenance.json")
			rawPath := filepath.Join(dir, "raw", "ws_initial.jsonl")
			valid := false
			switch fault {
			case "none":
				valid = true
			case "raw missing":
				require.NoError(t, os.Remove(rawPath))
			case "raw hash changed":
				f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0600)
				require.NoError(t, err)
				_, err = f.WriteString("\n")
				require.NoError(t, err)
				require.NoError(t, f.Close())
			case "provenance missing":
				require.NoError(t, os.Remove(path))
			case "missing hash":
				delete(meta.GatewayFingerprintHashes, "raw/ws_compaction.jsonl")
			case "unexpected hash":
				meta.GatewayFingerprintHashes["unexpected"] = strings.Repeat("a", 64)
			case "claimed counters forged", "old evidence without seed", "rebound ingress already rewritten":
				raw, err := os.ReadFile(path)
				require.NoError(t, err)
				var proof struct {
					Scenario    string                     `json:"scenario"`
					Fingerprint gatewayFingerprintEvidence `json:"fingerprint_evidence"`
				}
				require.NoError(t, json.Unmarshal(raw, &proof))
				if fault == "claimed counters forged" {
					proof.Fingerprint.Execution.BodyBindings = 999
				}
				if fault == "old evidence without seed" {
					proof.Fingerprint.Seed = ""
				}
				if fault == "rebound ingress already rewritten" {
					for i := range proof.Fingerprint.Ingress {
						proof.Fingerprint.Ingress[i].Body.Metadata["x-codex-installation-id"] = gatewayGoldenInstallationID
						proof.Fingerprint.Ingress[i].Body.TurnMetadata["installation_id"] = mustJSON(gatewayGoldenInstallationID)
					}
				}
				writeJSON(path, proof)
				meta = refreshFingerprintTestHashes(t, dir)
			case "rebound raw not rewritten":
				mutateFingerprintBody(t, &records[1], func(_ map[string]any, cm, em map[string]any) {
					cm["x-codex-installation-id"] = fingerprintNativeInstallation
					em["installation_id"] = fingerprintNativeInstallation
				})
				writeJSONL(rawPath, records)
				meta = refreshFingerprintTestHashes(t, dir)
				normalized = fingerprintTestRecordMap(records)
			case "different normalized identity":
				mutateFingerprintBody(t, &records[1], func(_ map[string]any, cm, em map[string]any) {
					cm["turn_id"] = fingerprintOutputTurn
					em["turn_id"] = fingerprintOutputTurn
				})
				normalized = fingerprintTestRecordMap(records)
			case "only endpoint normalization":
				replaceFingerprintHeader(&records[0], "Host", "<UPSTREAM>")
				normalized = fingerprintTestRecordMap(records)
				valid = true
			}
			err := readGatewayFingerprintEvidence(dir, "ws_initial", meta, normalized)
			if valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

// Fresh synthetic unit fixtures exercise compareMain end to end. They are not
// a replay of official traffic and never serve as acceptance source evidence.
func fingerprintCompareFixture(t *testing.T, base, candidate string) {
	t.Helper()
	comparatorFixture(t, base)
	comparatorFixture(t, candidate)
	original, err := readRecordMap(filepath.Join(base, "ws_initial.jsonl"))
	require.NoError(t, err)
	var rustls, openssl *clientHello
	for _, row := range original {
		if row.TLS != nil {
			if row.TLS.Lane == "rustls" {
				rustls = row.TLS
			}
			if row.TLS.Lane == "openssl" {
				openssl = row.TLS
			}
		}
	}
	require.NotNil(t, rustls)
	require.NotNil(t, openssl)
	evidence, records := fingerprintTestFixture(t)
	evidence.Execution, err = validateGatewayFingerprintExecution(evidence, records)
	require.NoError(t, err)
	for i := range records {
		if records[i].Kind == "http_request" {
			records[i].TLS = openssl
		} else {
			records[i].TLS = rustls
		}
	}
	for _, name := range fullScenarioNames {
		writeJSONL(filepath.Join(candidate, name+".jsonl"), records)
		raw := string(mustJSON(records))
		raw = strings.ReplaceAll(raw, gatewayGoldenInstallationID, fingerprintNativeInstallation)
		raw = strings.ReplaceAll(raw, fingerprintOutputSession, fingerprintNativeSession)
		raw = strings.ReplaceAll(raw, fingerprintOutputTurn, fingerprintNativeTurn)
		var native []record
		require.NoError(t, json.Unmarshal([]byte(raw), &native))
		for i := range native {
			native[i].Hop = ""
		}
		writeJSONL(filepath.Join(base, name+".jsonl"), native)
	}
	// Frame wire lengths must describe each fixture's actual synthetic payload.
	writeSyntheticWSLifecycleFixture(t, base)
	writeSyntheticWSLifecycleFixture(t, candidate)
	require.NoError(t, finalizeCaptureTraces(base))
	require.NoError(t, finalizeCaptureTraces(candidate))
	meta, err := readCaptureMetadata(candidate)
	require.NoError(t, err)
	meta.Recorder = "backend/tools/codexgolden gateway"
	meta.GatewayMode = "passthrough"
	meta.GatewayConfigHash = strings.Repeat("a", 64)
	trace, err := os.ReadFile(filepath.Join(candidate, "traces", "tls.strace"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(candidate, "gateway-connect.strace"), trace, 0600))
	for _, name := range fullScenarioNames {
		rows, err := readRecordMap(filepath.Join(candidate, name+".jsonl"))
		require.NoError(t, err)
		var sorted []record
		for _, row := range rows {
			sorted = append(sorted, row)
		}
		writeJSONL(filepath.Join(candidate, "raw", name+".jsonl"), sorted)
		observation := gatewayIngressObservation{Sequence: 1, ActualMode: "passthrough", AccountMode: "passthrough", ResolverTransport: "responses_websockets_v2"}
		writeJSON(filepath.Join(candidate, name+"-provenance.json"), map[string]any{"scenario": name, "candidate_ingress_mode": meta.GatewayMode, "gateway_config_sha256": meta.GatewayConfigHash, "ingress_observations": []gatewayIngressObservation{observation}, "service_routes": map[string]int{"WS /backend-api/codex/responses": 1}, "fingerprint_evidence": evidence})
	}
	writeJSON(filepath.Join(candidate, "meta.json"), meta)
	require.NoError(t, finalizeCaptureTraces(candidate))
}

func TestGatewayFingerprintFullCompareRejectsBypassAndKeepsOfficialIDMapping(t *testing.T) {
	for _, fault := range []string{"none", "seed removed with fresh hashes", "counters forged with fresh hashes", "raw removed", "normalized identity changed"} {
		t.Run(fault, func(t *testing.T) {
			base, candidate := t.TempDir(), t.TempDir()
			fingerprintCompareFixture(t, base, candidate)
			want := 2
			switch fault {
			case "none":
				want = 0
			case "seed removed with fresh hashes", "counters forged with fresh hashes":
				path := filepath.Join(candidate, "ws_initial-provenance.json")
				raw, err := os.ReadFile(path)
				require.NoError(t, err)
				var proof map[string]any
				require.NoError(t, json.Unmarshal(raw, &proof))
				evidence := proof["fingerprint_evidence"].(map[string]any)
				if fault == "seed removed with fresh hashes" {
					delete(evidence, "seed")
				} else {
					evidence["execution"].(map[string]any)["body_bindings"] = 999
				}
				writeJSON(path, proof)
				require.NoError(t, finalizeCaptureTraces(candidate))
			case "raw removed":
				require.NoError(t, os.Remove(filepath.Join(candidate, "raw", "ws_initial.jsonl")))
			case "normalized identity changed":
				path := filepath.Join(candidate, "ws_initial.jsonl")
				raw, err := os.ReadFile(path)
				require.NoError(t, err)
				raw = []byte(strings.ReplaceAll(string(raw), fingerprintOutputTurn, fingerprintNativeTurn))
				require.NoError(t, os.WriteFile(path, raw, 0600))
			}
			require.Equal(t, want, compareMain([]string{"-golden", base, "-candidate", candidate}))
		})
	}
}
