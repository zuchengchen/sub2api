//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGatewayIngressModeValidationBeforeCapture(t *testing.T) {
	for _, mode := range []string{"", "shared", "dedicated", "http_bridge", "CTX_POOL", "ctx_pool ", "passthrough,ctx_pool"} {
		t.Run(mode, func(t *testing.T) {
			require.Error(t, validateGatewayIngressMode(mode))
			require.Equal(t, 2, gatewayMain([]string{"-ingress-mode", mode}))
		})
	}
}
func TestGatewayIngressModeResolvesAccountAndChangesOnlyCandidateFingerprint(t *testing.T) {
	referenceHash := recordingConfigHash()
	hashes := map[string]string{}
	for _, mode := range []string{"passthrough", "ctx_pool"} {
		cfg := gatewayScenarioConfig()
		account := &service.Account{ID: 123, Concurrency: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Extra: map[string]any{"openai_oauth_responses_websockets_v2_enabled": true}}
		require.NoError(t, configureGatewayIngressMode(cfg, account, mode))
		require.Equal(t, mode, account.ResolveOpenAIResponsesWebSocketV2Mode(cfg.Gateway.OpenAIWS.IngressModeDefault))
		require.NoError(t, verifyGatewayModeResolution(cfg, account, mode))
		hashes[mode] = gatewayConfigFingerprint(cfg)
		require.Equal(t, referenceHash, recordingConfigHash(), "candidate mode cannot alter official reference config")
		cfg.Gateway.OpenAIWS.ForceHTTP = true
		require.Error(t, verifyGatewayModeResolution(cfg, account, mode))
		cfg.Gateway.OpenAIWS.ForceHTTP = false
		account.Extra["openai_oauth_responses_websockets_v2_mode"] = "off"
		require.Error(t, verifyGatewayModeResolution(cfg, account, mode))
	}
	require.NotEqual(t, hashes["passthrough"], hashes["ctx_pool"])
	require.Equal(t, "passthrough", gatewayScenarioConfig().Gateway.OpenAIWS.IngressModeDefault)
}
func TestGatewayModeEvidenceRequiresActualPoolPath(t *testing.T) {
	hash := strings.Repeat("a", 64)
	for _, mode := range []string{"passthrough", "ctx_pool"} {
		meta := captureMetadata{GatewayMode: mode, GatewayConfigHash: hash}
		evidence := gatewayModeEvidence{Scenario: "ws_initial", Mode: mode, ConfigHash: hash, Observations: []gatewayIngressObservation{{Sequence: 1, ActualMode: mode, AccountMode: mode, ResolverTransport: "responses_websockets_v2", PoolAcquiresBefore: 2, PoolAcquiresAfter: 2}}}
		if mode == "ctx_pool" {
			evidence.Observations[0].PoolAcquiresAfter = 3
		}
		require.NoError(t, validateGatewayModeEvidence(meta, evidence, 1))
		for _, fault := range []string{"missing mode", "wrong mode", "missing hash", "different hash", "missing observation", "route count mismatch", "wrong resolver", "actual wrong account", "wrong execution", "negative counter"} {
			t.Run(mode+"/"+fault, func(t *testing.T) {
				got := evidence
				got.Observations = append([]gatewayIngressObservation(nil), evidence.Observations...)
				routes := 1
				switch fault {
				case "missing mode":
					got.Mode = ""
				case "wrong mode":
					got.Mode = "other"
				case "missing hash":
					got.ConfigHash = ""
				case "different hash":
					got.ConfigHash = strings.Repeat("b", 64)
				case "missing observation":
					got.Observations = nil
				case "route count mismatch":
					routes = 2
				case "wrong resolver":
					got.Observations[0].ResolverTransport = "http_sse"
				case "actual wrong account":
					got.Observations[0].AccountMode = "other"
				case "wrong execution":
					if mode == "ctx_pool" {
						got.Observations[0].PoolAcquiresAfter = 2
					} else {
						got.Observations[0].PoolAcquiresAfter = 3
					}
				case "negative counter":
					got.Observations[0].PoolAcquiresBefore = -1
				}
				require.Error(t, validateGatewayModeEvidence(meta, got, routes))
			})
		}
		dir := t.TempDir()
		writeJSON(filepath.Join(dir, "ws_initial-provenance.json"), map[string]any{"scenario": "ws_initial", "candidate_ingress_mode": mode, "gateway_config_sha256": hash, "ingress_observations": evidence.Observations, "service_routes": map[string]int{"WS /backend-api/codex/responses": 1}})
		writeJSONL(filepath.Join(dir, "ws_initial.jsonl"), []record{{Seq: 1, Conn: 1, ConnReq: 1, Hop: "gateway_upstream", Kind: "ws_handshake", Method: "GET", Target: "/backend-api/codex/responses", Proto: "HTTP/1.1", Headers: []headerKV{{Name: "Host", Value: "localhost"}}}})
		require.NoError(t, readGatewayModeEvidence(dir, "ws_initial", meta))
		require.Error(t, readGatewayModeEvidence(dir, "missing", meta))
	}
}
func TestGatewayMetadataMissingModeCannotPassAsGatewayCapture(t *testing.T) {
	dir := t.TempDir()
	comparatorFixture(t, dir)
	meta, err := readCaptureMetadata(dir)
	require.NoError(t, err)
	// These are explicitly synthetic verifier inputs, not candidate captures.
	trace, err := os.ReadFile(filepath.Join(dir, "traces", "tls.strace"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gateway-connect.strace"), trace, 0600))
	meta.Recorder = "backend/tools/codexgolden gateway"
	writeJSON(filepath.Join(dir, "meta.json"), meta)
	require.NoError(t, finalizeCaptureTraces(dir))
	_, err = readCaptureMetadata(dir)
	require.ErrorContains(t, err, "ingress mode")
	meta.GatewayMode = "ctx_pool"
	meta.GatewayConfigHash = strings.Repeat("a", 64)
	writeJSON(filepath.Join(dir, "meta.json"), meta)
	require.NoError(t, finalizeCaptureTraces(dir))
	_, err = readCaptureMetadata(dir)
	require.NoError(t, err)
	reference := meta
	reference.GatewayMode = ""
	reference.GatewayConfigHash = ""
	reference.Recorder = "backend/tools/codexgolden"
	require.NoError(t, compatibleCaptureMetadata(reference, meta), "candidate-only mode never changes common reference comparison")
}
