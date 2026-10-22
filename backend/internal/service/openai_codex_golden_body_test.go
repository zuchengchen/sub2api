package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
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
