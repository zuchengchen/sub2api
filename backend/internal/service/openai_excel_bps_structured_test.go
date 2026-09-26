package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func excelStructuredRequest(t *testing.T, model string, stream bool) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": model, "stream": stream, "input": "Return the answer.",
		"text": map[string]any{"format": map[string]any{
			"type": "json_schema", "name": "answer", "strict": true,
			"schema": map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "integer"}}, "required": []string{"answer"}, "additionalProperties": false},
		}},
	})
	require.NoError(t, err)
	return body
}

func TestExcelBPSStructuredOutputForwardContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, model := range []string{"gpt-6-astra", "gpt-6-luna", "gpt-6-sol"} {
		for _, stream := range []bool{false, true} {
			for _, valid := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/valid=%t", model, stream, valid), func(t *testing.T) {
					answer := "UNVALIDATED_TEXT"
					if valid {
						answer = "{\"answer\":17}"
					}
					event, err := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
						"id": "resp_json", "status": "completed", "model": model,
						"output": []any{map[string]any{"type": "message", "id": "msg_json", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": answer, "annotations": []any{}}}}},
						"usage":  map[string]any{"input_tokens": 10, "output_tokens": 5},
					}})
					require.NoError(t, err)
					wire := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"UNVALIDATED_TEXT\"}\n\n" + "event: response.completed\ndata: " + string(event) + "\n\n"
					upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}}
					svc := openAIClientToolsTestService(upstream)
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					account := excelAccount()
					result, err := svc.Forward(context.Background(), c, account, excelStructuredRequest(t, model, stream))
					require.Equal(t, "bps.openai.com", upstream.requests[0].URL.Host)
					require.Equal(t, model, gjson.GetBytes(upstream.bodies[0], "model").String())
					require.False(t, gjson.GetBytes(upstream.bodies[0], "text").Exists())
					require.Contains(t, string(upstream.bodies[0]), "structured final answer")
					require.True(t, account.Schedulable)
					if valid {
						require.NotNil(t, result)
						require.Len(t, upstream.requests, 1)
						require.NoError(t, err)
						require.Equal(t, "response.completed", result.UpstreamTerminalEvent)
						require.Equal(t, 10, result.Usage.InputTokens)
						require.NotContains(t, rec.Body.String(), "UNVALIDATED_TEXT")
						if stream {
							require.Contains(t, rec.Body.String(), "event: response.output_text.delta")
							require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.completed"))
						} else {
							require.Equal(t, 200, rec.Code)
							require.Equal(t, "json_schema", gjson.GetBytes(rec.Body.Bytes(), "text.format.type").String())
							require.JSONEq(t, answer, gjson.GetBytes(rec.Body.Bytes(), "output.0.content.0.text").String())
						}
					} else if stream {
						require.Len(t, upstream.requests, 1)
						require.Error(t, err)
						require.True(t, IsResponseCommitted(c))
						require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed"))
					} else {
						require.GreaterOrEqual(t, len(upstream.requests), 2)
						require.Equal(t, "chatgpt.com", upstream.requests[1].URL.Host)
						require.Error(t, err)
					}
				})
			}
		}
	}
}

func TestExcelBPSStructuredOutputDoesNotOverrideModelAccess(t *testing.T) {
	for _, model := range []string{"gpt-6-luna", "gpt-6-sol"} {
		t.Run(model, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{\"error\":{\"code\":\"basispoints_model_access_changed\"}}"))}}
			svc := openAIClientToolsTestService(upstream)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			account := excelAccount()
			_, err := svc.Forward(context.Background(), c, account, excelStructuredRequest(t, model, true))
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.NotErrorAs(t, err, &failover)
			require.GreaterOrEqual(t, len(upstream.requests), 2)
			require.Equal(t, "bps.openai.com", upstream.requests[0].URL.Host)
			require.Equal(t, "chatgpt.com", upstream.requests[1].URL.Host)
			require.True(t, account.Schedulable)
		})
	}
}
