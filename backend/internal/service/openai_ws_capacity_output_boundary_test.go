//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIWSV2CapacityReplayBoundary(t *testing.T) {
	for _, tc := range []struct {
		name         string
		prefix       []byte
		terminal     []byte
		wantFailover bool
	}{
		{"bare_error_before_output", nil, []byte(`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`), true},
		{"failed_before_output", nil, []byte(`{"type":"response.failed","response":{"id":"resp_capacity","status":"failed","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}}`), true},
		{"empty_delta_before_error", []byte(`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":""}`), []byte(`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`), true},
		{"nonempty_delta_before_error", []byte(`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello"}`), []byte(`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`), false},
		{"encrypted_reasoning_before_error", []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"synthetic"}}`), []byte(`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`), false},
		{"empty_reasoning_delta_before_error", []byte(`{"type":"response.reasoning_summary_text.delta","delta":""}`), []byte(`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`), true},
		{"empty_tool_delta_before_error", []byte(`{"type":"response.function_call_arguments.delta","delta":""}`), []byte(`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`), true},
		{"empty_text_done_before_error", []byte(`{"type":"response.output_text.done","text":""}`), []byte(`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`), true},
		{"tool_delta_before_error", []byte(`{"type":"response.function_call_arguments.delta","delta":"{}"}`), []byte(`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`), false},
		{"tool_item_before_error", []byte(`{"type":"response.output_item.added","item":{"type":"function_call","arguments":"{}"}}`), []byte(`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`), false},
		{"nonempty_delta_before_failed", []byte(`{"type":"response.output_text.delta","delta":"hello"}`), []byte(`{"type":"response.failed","response":{"id":"resp_capacity","status":"failed","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}}`), false},
		{"failed_with_usage_before_output", nil, []byte(`{"type":"response.failed","response":{"id":"resp_capacity","status":"failed","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."},"usage":{"input_tokens":8,"output_tokens":2}}}`), false},
		{"policy_failed_before_output", nil, []byte(`{"type":"response.failed","response":{"id":"resp_policy","status":"failed","error":{"code":"content_policy","message":"Request blocked by policy"}}}`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header.Set("User-Agent", "unit-test-agent/1.0")
			cfg := &config.Config{}
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			cfg.Gateway.OpenAIWS.Enabled = true
			cfg.Gateway.OpenAIWS.OAuthEnabled = true
			cfg.Gateway.OpenAIWS.APIKeyEnabled = true
			cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			events := [][]byte{[]byte(`{"type":"response.created","response":{"id":"resp_capacity","model":"gpt-5.1","status":"in_progress"}}`)}
			if tc.prefix != nil {
				events = append(events, tc.prefix)
			}
			events = append(events, tc.terminal)
			conn := &openAIWSCaptureConn{events: events}
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: conn})
			defer pool.Close()
			upstream := &httpUpstreamRecorder{}
			svc := &OpenAIGatewayService{tiboRouteDisabled: true, cfg: cfg, httpUpstream: upstream, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool}
			account := &Account{ID: 85, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"access_token": "test-token"}, Extra: astraHealthyTestExtra(map[string]any{"responses_websockets_v2_enabled": true})}
			result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.1","stream":true,"input":"hello"}`))
			var fe *UpstreamFailoverError
			require.Equal(t, tc.wantFailover, errors.As(err, &fe), "error=%v", err)
			if tc.wantFailover {
				require.Equal(t, http.StatusServiceUnavailable, fe.StatusCode)
				require.True(t, fe.RequestScopedTransient)
				require.Empty(t, rec.Body.String())
			} else {
				require.NotEmpty(t, rec.Body.String())
				if tc.prefix != nil {
					require.Equal(t, 1, strings.Count(rec.Body.String(), string(tc.prefix)), "already emitted semantic event must not be replayed")
				}
			}
			if tc.name == "failed_with_usage_before_output" {
				require.NotNil(t, result)
				require.Equal(t, 8, result.Usage.InputTokens)
				require.Equal(t, 2, result.Usage.OutputTokens)
				require.Equal(t, "response.failed", result.UpstreamTerminalEvent)
			}
			require.Empty(t, upstream.requests)
		})
	}
}
