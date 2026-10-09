package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// These requests reproduce the stored single-turn HTTP conditions without
// contacting a live model. Assertions cover the actual core transport boundary.
func TestDiagnosticOAuthHTTPAcceptanceRequestPreservation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, fixture := range []string{"e14", "e25", "e14-tools", "e25-tools"} {
		for _, client := range []string{"generic", "codex"} {
			t.Run(fixture+"/"+client, func(t *testing.T) {
				body, err := os.ReadFile(filepath.Join("testdata", "diagnostic_http_responses", fixture+".request.json"))
				require.NoError(t, err)
				sent := diagnosticForwardHTTPFixture(t, body, client)
				var before, after map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(body, &before))
				require.NoError(t, json.Unmarshal(sent, &after))
				for key, value := range before {
					require.JSONEq(t, string(value), string(after[key]), "modified input field %s", key)
				}
				require.JSONEq(t, `["reasoning.encrypted_content"]`, string(after["include"]))
				var added []string
				for key := range after {
					if _, exists := before[key]; !exists {
						require.Equal(t, "include", key, "unexpected injected field")
						added = append(added, key)
					}
				}
				t.Logf("core boundary: %s; all input fields preserved; added fields=%v", fixture, added)
			})
		}
	}
}

func TestDiagnosticOAuthHTTPExplicitToolsAndIncludePreservation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-6-astra","instructions":"Preserve 中文 <tag> & literal input.","input":[{"role":"user","content":[{"type":"input_text","text":"Compute the fixture value."}]}],"reasoning":{"effort":"xhigh"},"tools":[{"type":"function","name":"fixture_math","description":"Compute a fixture expression.","parameters":{"type":"object","properties":{"expression":{"type":"string"}},"required":["expression"],"additionalProperties":false},"strict":true}],"tool_choice":"auto","parallel_tool_calls":false,"include":["message.output_text.logprobs","reasoning.encrypted_content"],"text":{"format":{"type":"text"},"verbosity":"medium"},"store":false,"stream":true}`)
	sent := diagnosticForwardHTTPFixture(t, body, "generic")
	var before, after map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &before))
	require.NoError(t, json.Unmarshal(sent, &after))
	for _, field := range []string{"input", "instructions", "reasoning", "tools", "tool_choice", "parallel_tool_calls", "include", "text", "model", "store", "stream"} {
		require.JSONEq(t, string(before[field]), string(after[field]), "modified input field %s", field)
	}
	t.Log("core boundary: explicit tools, tool policy, include, and text fields preserved")
}

func TestDiagnosticOAuthHTTPC01FullHistoryOnlyRemovesAssistantID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body, err := os.ReadFile(filepath.Join("testdata", "diagnostic_http_responses", "c01-full-history-redacted.request.json"))
	require.NoError(t, err)
	for _, client := range []string{"generic", "codex"} {
		t.Run(client, func(t *testing.T) {
			sent := diagnosticForwardHTTPFixture(t, body, client)
			var expected map[string]any
			require.NoError(t, decodeOpenAIJSONUseNumber(body, &expected))
			input := expected["input"].([]any)
			require.Len(t, input, 3)
			assistant := input[1].(map[string]any)
			require.Equal(t, "message", assistant["type"])
			require.Equal(t, "assistant", assistant["role"])
			require.Equal(t, "final_answer", assistant["phase"])
			require.Equal(t, "completed", assistant["status"])
			require.Contains(t, assistant, "id")
			delete(assistant, "id")
			expectedBody, err := json.Marshal(expected)
			require.NoError(t, err)
			// Whole-body equality after deleting this single key proves that all
			// messages, content, phase, status, and unrelated fields are intact.
			require.JSONEq(t, string(expectedBody), string(sent))
			t.Log("only changed JSON path: /input/1/id (removed); all 3 input items and complete contents preserved")
		})
	}
}

func diagnosticForwardHTTPFixture(t *testing.T, body []byte, client string) []byte {
	t.Helper()
	cfg := diagnosticContinuationConfig()
	upstream := &httpUpstreamRecorder{resp: diagnosticContinuationSSE("resp_http_fixture", `[]`)}
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), tiboRouteDisabled: true}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("User-Agent", "Python-urllib/3.12")
	if client == "codex" {
		c.Request.Header.Set("User-Agent", "codex-tui/0.160.1 (Ubuntu 22.4.0; x86_64) xterm-256color")
		c.Request.Header.Set("originator", "codex-tui")
	}
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	result, err := svc.Forward(context.Background(), c, diagnosticContinuationAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	decision, _ := c.Get("openai_ws_transport_decision")
	require.Equal(t, string(OpenAIUpstreamTransportHTTPSSE), decision)
	return upstream.lastBody
}
