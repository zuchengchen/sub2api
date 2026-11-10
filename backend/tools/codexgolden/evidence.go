//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

const recordingContract = 2

var fullScenarioNames = []string{"ws_initial", "ws_tool_loop", "http_fallback_tool_loop", "http_unauthorized_refresh", "ws_unauthorized_refresh", "ws_compaction"}
var traceOutputDir string

type captureMetadata struct {
	WSEvidenceHashes         map[string]string `json:"ws_evidence_sha256"`
	GatewayFingerprintHashes map[string]string `json:"gateway_fingerprint_evidence_sha256"`
	SourceBuildReceiptHash   string            `json:"source_build_receipt_sha256,omitempty"`
	GatewayMode              string            `json:"candidate_ingress_mode,omitempty"`
	GatewayConfigHash        string            `json:"gateway_config_sha256,omitempty"`
	Contract                 int               `json:"recording_contract"`
	Version                  string            `json:"codex_version"`
	Commit                   string            `json:"source_commit"`
	BinaryHash               string            `json:"binary_sha256"`
	ModelsHash               string            `json:"models_sha256"`
	TLSMode                  string            `json:"tls_mode"`
	Model                    string            `json:"model"`
	Reasoning                string            `json:"reasoning_effort"`
	ConfigHash               string            `json:"effective_config_sha256"`
	CAHash                   string            `json:"fixture_ca_sha256"`
	RecordedAt               string            `json:"recorded_at"`
	Recorder                 string            `json:"recorder"`
	Scenarios                []string          `json:"scenarios"`
	Strace                   bool              `json:"strace_guard"`
	Leaks                    []string          `json:"outbound_leaks"`
	Traces                   map[string]string `json:"trace_sha256"`
}

func recordingConfigHash() string {
	// Endpoints and filesystem roots are isolated per invocation. These are the
	// behavior-affecting configuration values shared by reference and candidate.
	config := map[string]any{"tls_mode": recorderTLSMode(), "model": "gpt-6-astra", "reasoning_effort": "xhigh", "sandbox": "read-only", "auth_store": "file", "updates": false, "compaction_limit": 1, "models_barrier": true, "scripted_native_output": 1, "native_response_evidence": 2, "ws_frame_evidence": 1, "ws_handshake_outcome_evidence": 1, "gateway_seeded_full_evidence": 1}
	return fmt.Sprintf("%x", sha256.Sum256(mustJSON(config)))
}

func addCaptureContract(meta map[string]any) error {
	ca, err := os.ReadFile(os.Getenv("CODEXGOLDEN_CA"))
	if err != nil {
		return fmt.Errorf("read fixture CA: %w", err)
	}
	meta["recording_contract"] = recordingContract
	meta["effective_config_sha256"] = recordingConfigHash()
	meta["fixture_ca_sha256"] = fmt.Sprintf("%x", sha256.Sum256(ca))
	return nil
}

func persistTrace(path, name string) error {
	if traceOutputDir == "" || path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("retain trace %s: %w", name, err)
	}
	if err := os.MkdirAll(traceOutputDir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(traceOutputDir, name+".strace"), data, 0600)
}

func expectedTracePaths(recorder string) []string {
	if recorder == "backend/tools/codexgolden gateway" {
		return []string{"gateway-connect.strace"}
	}
	result := []string{filepath.Join("traces", "tls.strace")}
	for _, name := range fullScenarioNames {
		result = append(result, filepath.Join("traces", name+".strace"))
	}
	sort.Strings(result)
	return result
}

func finalizeCaptureTraces(dir string) error {
	raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return err
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		return err
	}
	recorder, _ := meta["recorder"].(string)
	traces := map[string]string{}
	for _, path := range expectedTracePaths(recorder) {
		data, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			return err
		}
		if leaks := checkTrace(filepath.Join(dir, path)); len(leaks) > 0 {
			return fmt.Errorf("retained trace %s invalid: %v", path, leaks)
		}
		traces[path] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	if err := finalizeWSEvidenceHashes(dir, meta); err != nil {
		return err
	}
	meta["strace_guard"] = true
	meta["trace_sha256"] = traces
	if err := finalizeGatewayFingerprintHashes(dir, meta); err != nil {
		return err
	}
	writeJSON(filepath.Join(dir, "meta.json"), meta)
	return nil
}

func readCaptureMetadata(dir string) (captureMetadata, error) {
	var meta captureMetadata
	raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return meta, err
	}
	invalid := func(reason string) (captureMetadata, error) {
		return meta, fmt.Errorf("invalid recording metadata: %s", reason)
	}
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if meta.Contract != recordingContract || !regexp.MustCompile(`^codex-cli [0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`).MatchString(meta.Version) || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(meta.Commit) {
		return invalid("contract, version or commit")
	}
	for _, hash := range []string{meta.BinaryHash, meta.ModelsHash, meta.ConfigHash, meta.CAHash} {
		if !hex64.MatchString(hash) {
			return invalid("missing or malformed fingerprint")
		}
	}
	if meta.SourceBuildReceiptHash != "" {
		raw, err := os.ReadFile(filepath.Join(dir, sourceBuildReceiptFile))
		if err != nil || !hex64.MatchString(meta.SourceBuildReceiptHash) || fmt.Sprintf("%x", sha256.Sum256(raw)) != meta.SourceBuildReceiptHash {
			return invalid("missing or changed source build receipt")
		}
		if err := validateSourceBuildReceipt(raw, meta.Version, meta.Commit, meta.BinaryHash, meta.ModelsHash); err != nil {
			return invalid(err.Error())
		}
	} else if meta.Version == "codex-cli 0.0.0" || strings.Contains(strings.TrimPrefix(meta.Version, "codex-cli "), "-") {
		return invalid("source CLI without a verified build receipt")
	}
	if meta.TLSMode != "system_roots_namespace" && meta.TLSMode != "custom_ca_rustls_h2" {
		return invalid("unknown TLS mode")
	}
	if meta.Model != "gpt-6-astra" || meta.Reasoning != "xhigh" {
		return invalid("unexpected model configuration")
	}
	if _, err := time.Parse(time.RFC3339, meta.RecordedAt); err != nil {
		return invalid("missing or invalid capture time")
	}
	if meta.Recorder != "backend/tools/codexgolden" && meta.Recorder != "backend/tools/codexgolden gateway" {
		return invalid("unknown recorder")
	}
	if meta.Recorder == "backend/tools/codexgolden gateway" {
		if validateGatewayIngressMode(meta.GatewayMode) != nil || !hex64.MatchString(meta.GatewayConfigHash) {
			return invalid("missing candidate ingress mode/config fingerprint")
		}
	}
	if !reflect.DeepEqual(meta.Scenarios, fullScenarioNames) || !meta.Strace || len(meta.Leaks) != 0 {
		return invalid("incomplete scenarios or failed network guard")
	}
	paths := expectedTracePaths(meta.Recorder)
	if len(meta.Traces) != len(paths) {
		return invalid("missing trace evidence")
	}
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil || !hex64.MatchString(meta.Traces[path]) || fmt.Sprintf("%x", sha256.Sum256(data)) != meta.Traces[path] {
			return invalid("unreadable or changed trace: " + path)
		}
		if leaks := checkTrace(filepath.Join(dir, path)); len(leaks) != 0 {
			return invalid("failed retained trace: " + path)
		}
	}
	if err := validateWSEvidenceHashes(dir, meta.WSEvidenceHashes); err != nil {
		return invalid(err.Error())
	}
	return meta, nil
}

func compatibleCaptureMetadata(a, b captureMetadata) error {
	for _, field := range []struct{ name, a, b string }{
		{"source build receipt", a.SourceBuildReceiptHash, b.SourceBuildReceiptHash},
		{"version", a.Version, b.Version}, {"commit", a.Commit, b.Commit}, {"binary", a.BinaryHash, b.BinaryHash}, {"models", a.ModelsHash, b.ModelsHash},
		{"TLS mode", a.TLSMode, b.TLSMode}, {"model", a.Model, b.Model}, {"reasoning", a.Reasoning, b.Reasoning}, {"configuration", a.ConfigHash, b.ConfigHash}, {"CA fixture", a.CAHash, b.CAHash},
	} {
		if field.a != field.b {
			return fmt.Errorf("incompatible %s: reference and candidate must use the same fixed environment", field.name)
		}
	}
	return nil
}

func validateScenarioFiles(dir string) error {
	paths, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return err
	}
	names := make([]string, 0, len(paths))
	for _, path := range paths {
		names = append(names, strings.TrimSuffix(filepath.Base(path), ".jsonl"))
	}
	expected := append([]string(nil), fullScenarioNames...)
	sort.Strings(expected)
	sort.Strings(names)
	if !reflect.DeepEqual(names, expected) {
		return fmt.Errorf("scenario files do not match the complete recording contract")
	}
	return nil
}

func validateCoreTransports(meta captureMetadata, records map[recordKey]record) error {
	observed := 0
	streams := map[string]bool{}
	for _, row := range records {
		if !gatewayCoreTarget(row.Target) && row.Kind != "ws_message" {
			continue
		}
		observed++
		if row.TLS == nil {
			return fmt.Errorf("core request %d has no socket TLS association", row.Seq)
		}
		wantLane := "rustls"
		if row.Kind == "http_request" {
			wantProto := "HTTP/2.0"
			if meta.TLSMode == "system_roots_namespace" {
				wantProto, wantLane = "HTTP/1.1", "openssl"
			}
			if row.Proto != wantProto {
				return fmt.Errorf("core request %d uses %s in %s", row.Seq, row.Proto, meta.TLSMode)
			}
			if wantProto == "HTTP/2.0" {
				if row.H2 == nil {
					return fmt.Errorf("core request %d lacks HTTP/2 frame evidence", row.Seq)
				}
				key := fmt.Sprintf("%d/%d", row.Conn, row.H2.StreamID)
				if streams[key] {
					return fmt.Errorf("duplicate HTTP/2 stream association %s", key)
				}
				streams[key] = true
			}
		} else if row.Kind == "ws_handshake" && row.Proto != "HTTP/1.1" {
			return fmt.Errorf("WebSocket Upgrade must be HTTP/1.1")
		}
		if row.TLS.Lane != wantLane {
			return fmt.Errorf("core request %d TLS lane %s does not match %s", row.Seq, row.TLS.Lane, wantLane)
		}
	}
	if observed == 0 {
		return fmt.Errorf("no core transport observations")
	}
	return nil
}
