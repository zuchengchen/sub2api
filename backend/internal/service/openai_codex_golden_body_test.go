package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/codexwire"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// goldenCodexResponsesBody returns the first HTTP POST /responses body
// captured from codex-cli 0.163.0 (backend/tools/codexgolden).
func goldenCodexResponsesBody(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "pkg", "codexwire", "testdata", "golden", "0.163.0", "http_fallback_tool_loop.jsonl"))
	require.NoError(t, err)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rec struct {
			Kind     string          `json:"kind"`
			Target   string          `json:"target"`
			BodyJSON json.RawMessage `json:"body_json"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &rec))
		if rec.Kind == "http_request" && strings.HasSuffix(rec.Target, "/codex/responses") {
			return rec.BodyJSON
		}
	}
	t.Fatal("golden has no /responses body")
	return nil
}

func TestCodexGoldenBody_HasNoTopLevelInstructions(t *testing.T) {
	body := goldenCodexResponsesBody(t)
	require.False(t, gjson.GetBytes(body, "instructions").Exists(), "codex 0.163 sends no top-level instructions")
	require.True(t, codexInputCarriesBaseInstructions(body))
	require.False(t, codexInputCarriesBaseInstructions([]byte(`{"input":[{"role":"developer","content":"x"}]}`)))
}

// A real Codex 0.163 request must not get a synthetic top-level
// instructions: its base prompt is already input[0].
func TestForward_CodexGoldenBodyGetsNoSyntheticInstructions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := goldenCodexResponsesBody(t)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "codex_exec/0.163.0 (Ubuntu 26.4.0; x86_64) xterm-256color (codex_exec; 0.163.0)")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"stop"}}`)),
	}}
	svc := &OpenAIGatewayService{tiboRouteDisabled: true, cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID: 321, Name: "acc", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{"access_token": "oauth-token", "chatgpt_account_id": "chatgpt-acc"},
		Status:      StatusActive, Schedulable: true,
	}
	_, _ = svc.Forward(context.Background(), c, account, body)
	require.NotNil(t, upstream.lastReq)
	require.False(t, gjson.GetBytes(upstream.lastBody, "instructions").Exists(),
		"synthetic instructions must not be added when input carries model.base_instructions")
	// The base prompt is sent exactly once, as the first developer input item.
	require.Equal(t, "developer", gjson.GetBytes(upstream.lastBody, "input.0.role").String())
	require.Equal(t, gjson.GetBytes(body, "input.0.content.0.text").String(), gjson.GetBytes(upstream.lastBody, "input.0.content.0.text").String())
}

func TestCodexOfficial164HTTPReplayThroughGateway(t *testing.T) {
	codexCanonicalUAMu.RLock()
	previous := codexCanonicalUAResolver
	codexCanonicalUAMu.RUnlock()
	t.Cleanup(func() { SetCodexCanonicalUserAgentResolver(previous) })
	SetCodexCanonicalUserAgentResolver(func() string { return buildCodexCLIUserAgent("0.164.0") })
	for _, scenario := range []string{"http_fallback_tool_loop", "http_unauthorized_refresh"} {
		data, err := os.ReadFile(filepath.Join("..", "pkg", "codexwire", "testdata", "golden", "0.164.0", scenario+".jsonl"))
		require.NoError(t, err)
		for _, passthrough := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/passthrough=%t", scenario, passthrough), func(t *testing.T) {
				checked := 0
				for _, line := range bytes.Split(data, []byte("\n")) {
					if len(line) == 0 {
						continue
					}
					var record struct {
						Kind    string                         `json:"kind"`
						Method  string                         `json:"method"`
						Target  string                         `json:"target"`
						Headers []struct{ Name, Value string } `json:"headers"`
						Body    json.RawMessage                `json:"body_json"`
					}
					require.NoError(t, json.Unmarshal(line, &record))
					if record.Kind != "http_request" || record.Method != http.MethodPost || !strings.HasSuffix(record.Target, "/codex/responses") {
						continue
					}
					checked++
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(record.Body))
					var expectedNames []string
					for _, h := range record.Headers {
						expectedNames = append(expectedNames, h.Name)
						if h.Name != "content-length" && h.Name != "content-encoding" && h.Name != "host" {
							c.Request.Header.Set(h.Name, h.Value)
						}
					}
					upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}}
					cfg := codexProbeConfig(nil)
					cfg.Gateway.OpenAIWS.ForceHTTP = true
					cfg.Gateway.CodexFullSynthesis = boolPtr(true)
					account := &Account{ID: 90164, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
						Credentials: map[string]any{"access_token": "fixture-test-token", "chatgpt_account_id": "acct-golden"},
						Extra:       map[string]any{"openai_passthrough": passthrough, codexClientFamilyExtraKey: "exec", codexFingerprintSeedExtraKey: testCodexFingerprintSeed}}
					svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, tiboRouteDisabled: true}
					_, err := svc.Forward(context.Background(), c, account, record.Body)
					require.NoError(t, err)
					request := upstream.lastReq
					require.NotNil(t, request)
					body := decodeCodexProbeTestBody(t, request)
					require.Equal(t, "0.164.0", request.Header.Get("version"))
					require.Equal(t, "codex_exec", request.Header.Get("originator"))
					require.Equal(t, "zstd", request.Header.Get("Content-Encoding"))
					require.False(t, gjson.GetBytes(body, "instructions").Exists())
					for _, name := range []string{"model", "reasoning", "tools", "tool_choice", "include", "parallel_tool_calls", "text"} {
						before, after := gjson.GetBytes(record.Body, name), gjson.GetBytes(body, name)
						if before.Exists() {
							require.JSONEq(t, before.Raw, after.Raw, name)
						}
					}
					beforeItems, afterItems := gjson.GetBytes(record.Body, "input").Array(), gjson.GetBytes(body, "input").Array()
					require.Len(t, afterItems, len(beforeItems), "native instructions must not be duplicated")
					for i, before := range beforeItems {
						for _, field := range []string{"content", "arguments", "output", "name", "encrypted_content"} {
							if value := before.Get(field); value.Exists() {
								require.JSONEq(t, value.Raw, afterItems[i].Get(field).Raw, field)
							}
						}
					}
					var names []string
					for _, h := range codexwire.OrderCodexHTTPHeaders(request.Header) {
						names = append(names, h.Name)
					}
					names = append(names, "host", "content-length")
					require.Equal(t, expectedNames, names)
					metadata := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String()
					original := gjson.GetBytes(record.Body, "client_metadata.x-codex-turn-metadata").String()
					for _, field := range []string{"sandbox", "sandbox_mode", "thread_source", "turn_trigger", "analytics_enabled"} {
						require.Equal(t, gjson.Get(original, field).Raw, gjson.Get(metadata, field).Raw, field)
					}
					require.Equal(t, request.Header.Get("session-id"), gjson.Get(metadata, "session_id").String())
					require.Equal(t, request.Header.Get("thread-id"), gjson.Get(metadata, "thread_id").String())
				}
				require.Positive(t, checked)
			})
		}
	}
}
