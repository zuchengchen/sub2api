//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// /v1/chat/completions over an Anthropic upstream must keep the
// cache_creation 5m/1h split, otherwise billing prices 1h writes at 5m.
func TestAnthropicChatCompletionsKeepsCacheCreationTTLBreakdown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const start = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5.5","content":[],"usage":{"input_tokens":6,"output_tokens":1,"cache_creation_input_tokens":46756,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":46756}}}}`
	for _, tc := range []struct {
		name                 string
		delta                string
		input, created, read int
		created5m, created1h int
		output               int
	}{
		{
			// kiro-rs: final delta replaces the request-time estimate.
			name:  "kiro_final_delta",
			delta: `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":12,"output_tokens":9,"cache_creation_input_tokens":93194,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":93194},"usage_final":true}}`,
			input: 12, created: 93194, read: 0, created5m: 0, created1h: 93194, output: 9,
		},
		{
			// kiro-rs reconciled: creation moved to read, 1h must drop to 0.
			name:  "kiro_reconciled_delta",
			delta: `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":6,"output_tokens":9,"cache_creation_input_tokens":0,"cache_read_input_tokens":46756,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"usage_final":true}}`,
			input: 6, created: 0, read: 46756, created5m: 0, created1h: 0, output: 9,
		},
		{
			// Official Anthropic shape: delta carries only output; start's split stays.
			name:  "output_only_delta",
			delta: `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`,
			input: 6, created: 46756, read: 0, created5m: 0, created1h: 46756, output: 9,
		},
	} {
		for _, stream := range []bool{true, false} {
			name := tc.name + "/buffered"
			if stream {
				name = tc.name + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				sse := "event: message_start\ndata: " + start + "\n\n" +
					"event: message_delta\ndata: " + tc.delta + "\n\n" +
					"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				upstream := &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(sse)),
				}}
				streamField := "false"
				if stream {
					streamField = "true"
				}
				body := []byte(`{"model":"claude-opus-5.5","messages":[{"role":"user","content":"hi"}],"stream":` + streamField + `}`)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))

				svc := &GatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
				account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "sk-test"}}
				result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
				require.NoError(t, err)
				u := result.Usage
				require.Equal(t, tc.input, u.InputTokens)
				require.Equal(t, tc.output, u.OutputTokens)
				require.Equal(t, tc.created, u.CacheCreationInputTokens)
				require.Equal(t, tc.read, u.CacheReadInputTokens)
				require.Equal(t, tc.created5m, u.CacheCreation5mTokens)
				require.Equal(t, tc.created1h, u.CacheCreation1hTokens)
			})
		}
	}
}
