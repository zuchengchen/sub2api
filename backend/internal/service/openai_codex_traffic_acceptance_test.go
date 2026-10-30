//go:build linux

package service_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This opt-in socket suite requires a separately captured, verified official
// binary/reference. The Goal's focused and full regression commands supply all
// three inputs, so a skip cannot be counted as traffic acceptance.
func TestCodexOAuthTrafficAcceptance(t *testing.T) {
	bin, reference, candidate := os.Getenv("CODEX_TRAFFIC_REFERENCE_BIN"), os.Getenv("CODEX_TRAFFIC_REFERENCE_DIR"), os.Getenv("CODEX_TRAFFIC_CANDIDATE_DIR")
	if bin == "" && reference == "" && candidate == "" {
		t.Skip("set CODEX_TRAFFIC_REFERENCE_BIN, CODEX_TRAFFIC_REFERENCE_DIR and CODEX_TRAFFIC_CANDIDATE_DIR to execute real CLI traffic acceptance")
	}
	require.NotEmpty(t, bin)
	require.NotEmpty(t, reference)
	require.NotEmpty(t, candidate)
	var err error
	bin, err = filepath.Abs(bin)
	require.NoError(t, err)
	reference, err = filepath.Abs(reference)
	require.NoError(t, err)
	candidate, err = filepath.Abs(candidate)
	require.NoError(t, err)
	_, err = os.Stat(candidate)
	require.True(t, os.IsNotExist(err), "candidate output must be new for this invocation")
	require.NotEqual(t, reference, candidate)
	readJSON := func(t *testing.T, path string, value any) {
		t.Helper()
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NotEmpty(t, data)
		require.NoError(t, json.Unmarshal(data, value))
	}
	hashFile := func(path string) string {
		t.Helper()
		f, err := os.Open(path)
		require.NoError(t, err)
		defer f.Close()
		sum := sha256.New()
		_, err = io.Copy(sum, f)
		require.NoError(t, err)
		return hex.EncodeToString(sum.Sum(nil))
	}
	type metadata struct {
		Version       string   `json:"codex_version"`
		Commit        string   `json:"source_commit"`
		BinaryHash    string   `json:"binary_sha256"`
		ModelsHash    string   `json:"models_sha256"`
		Scenarios     []string `json:"scenarios"`
		Strace        bool     `json:"strace_guard"`
		Leaks         []string `json:"outbound_leaks"`
		Recorder      string   `json:"recorder"`
		NativePending bool     `json:"native_tls_pending"`
	}
	var ref metadata
	readJSON(t, filepath.Join(reference, "meta.json"), &ref)
	require.Equal(t, "backend/tools/codexgolden", ref.Recorder)
	require.True(t, ref.Strace)
	require.Empty(t, ref.Leaks)
	require.Regexp(t, `^[0-9a-f]{40}$`, ref.Commit)
	require.Equal(t, ref.BinaryHash, hashFile(bin))
	models := filepath.Join(reference, "models.json")
	require.Equal(t, ref.ModelsHash, hashFile(models))
	scenarios := []string{"ws_initial", "ws_tool_loop", "http_fallback_tool_loop", "http_unauthorized_refresh", "ws_unauthorized_refresh", "ws_compaction"}
	require.Equal(t, scenarios, ref.Scenarios)
	version := strings.TrimPrefix(ref.Version, "codex-cli ")
	require.NotEqual(t, version, ref.Version)
	require.NoError(t, os.MkdirAll(filepath.Dir(candidate), 0700))
	log, err := os.OpenFile(candidate+"-commands.log", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	require.NoError(t, err)
	defer log.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	backend, err := filepath.Abs("../..")
	require.NoError(t, err)
	run := func(name string, args ...string) {
		t.Helper()
		t.Logf("execute %s %q", name, args)
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = backend
		cmd.Stdout = log
		cmd.Stderr = log
		require.NoError(t, cmd.Run(), "full output: %s", log.Name())
	}
	recorder := candidate + "-recorder"
	run("go", "build", "-p", "2", "-o", recorder, "./tools/codexgolden")
	run(recorder, "gateway", "-codex", bin, "-out", candidate, "-models-json", models, "-expect-version", version, "-source-commit", ref.Commit)
	var actual metadata
	readJSON(t, filepath.Join(candidate, "meta.json"), &actual)
	require.Equal(t, ref.Version, actual.Version)
	require.Equal(t, ref.Commit, actual.Commit)
	require.Equal(t, ref.BinaryHash, actual.BinaryHash)
	require.Equal(t, ref.ModelsHash, actual.ModelsHash)
	require.Equal(t, scenarios, actual.Scenarios)
	require.False(t, actual.NativePending)
	require.Equal(t, "backend/tools/codexgolden gateway", actual.Recorder)
	var trace struct {
		Leaks  []string `json:"outbound_leaks"`
		Worker bool     `json:"worker_exit_success"`
	}
	readJSON(t, filepath.Join(candidate, "trace-validation.json"), &trace)
	require.True(t, trace.Worker)
	require.Empty(t, trace.Leaks)
	for _, scenario := range scenarios {
		t.Run(scenario, func(t *testing.T) {
			var provenance struct {
				Success bool           `json:"cli_exit_success"`
				Errors  []string       `json:"route_errors"`
				Denied  []string       `json:"proxy_denied"`
				Routes  map[string]int `json:"service_routes"`
				Writes  int            `json:"credential_writes"`
			}
			readJSON(t, filepath.Join(candidate, scenario+"-provenance.json"), &provenance)
			require.True(t, provenance.Success)
			require.Empty(t, provenance.Errors)
			require.Empty(t, provenance.Denied)
			require.Equal(t, 2, provenance.Routes["GET /backend-api/codex/models"])
			require.Positive(t, provenance.Routes["WS /backend-api/codex/responses"])
			if strings.Contains(scenario, "unauthorized_refresh") {
				require.Equal(t, 1, provenance.Writes)
			} else {
				require.Zero(t, provenance.Writes)
			}
			file, err := os.Open(filepath.Join(candidate, scenario+".jsonl"))
			require.NoError(t, err)
			defer file.Close()
			decoder := json.NewDecoder(file)
			core := 0
			for {
				var row struct {
					Hop string          `json:"hop"`
					TLS json.RawMessage `json:"tls"`
				}
				decodeErr := decoder.Decode(&row)
				if decodeErr == io.EOF {
					break
				}
				require.NoError(t, decodeErr)
				if row.Hop == "gateway_upstream" {
					core++
					require.NotEmpty(t, row.TLS, "every product request needs its socket TLS association")
				}
			}
			require.Positive(t, core)
		})
	}
	if t.Failed() {
		return
	}
	run(recorder, "compare", "-golden", reference, "-candidate", candidate)
	receipt, err := json.MarshalIndent(map[string]any{"reference": reference, "candidate": candidate, "binary_sha256": ref.BinaryHash, "recorder_sha256": hashFile(recorder), "scenarios": scenarios, "strict_comparison_passed": true, "completed_at": time.Now().UTC()}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(candidate, "acceptance.json"), receipt, 0600))
}
