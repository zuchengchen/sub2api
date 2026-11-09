//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGatewayAudit_CompatMeteredResultRetainsBillingMetadata(t *testing.T) {
	for _, route := range []string{"chat", "messages"} {
		for _, tier := range []string{"priority", "flex"} {
			for _, stream := range []bool{false, true} {
				for _, success := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/success=%t", route, tier, stream, success), func(t *testing.T) {
						result, svc, account, c, upstream, err := gatewayAuditCompatForward(t, route, tier, stream, success, false)
						if success {
							require.NoError(t, err)
						} else {
							require.Error(t, err)
						}
						var retry *UpstreamFailoverError
						require.False(t, errors.As(err, &retry))
						require.Len(t, upstream.requests, 1, "a metered failure cannot replay")
						require.Equal(t, "/v1/responses", upstream.requests[0].URL.Path)
						require.NotNil(t, result)
						require.Equal(t, 8, result.Usage.InputTokens)
						require.Equal(t, 2, result.Usage.OutputTokens)
						requestedTier := tier
						if route == "messages" {
							requestedTier = "priority"
						}
						require.Equal(t, requestedTier, gjson.GetBytes(upstream.lastBody, "service_tier").String())
						require.NotNil(t, result.ServiceTier, "metered failures retain the final outbound billing tier")
						require.Equal(t, requestedTier, *result.ServiceTier)
						require.Equal(t, "high", gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
						require.NotNil(t, result.ReasoningEffort)
						require.Equal(t, "high", *result.ReasoningEffort)
						require.Equal(t, tier, result.UpstreamResponseServiceTier)
						// Messages fast uses the existing priority request contract. A
						// public upstream's flex response then lowers billing normally.
						resolution := ApplyOpenAIServiceTierBillingResolution(account, result)
						require.Equal(t, requestedTier, resolution.Requested)
						require.Equal(t, tier, resolution.Billing)
						require.Equal(t, tier, *result.ServiceTier)
						if route == "messages" {
							cacheKey := gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String()
							require.NotEmpty(t, cacheKey)
							bound := svc.getOpenAICompatSessionResponseID(c.Request.Context(), c, account, cacheKey)
							digestBindings := 0
							svc.openaiCompatAnthropicDigestSessions.Range(func(_, _ any) bool { digestBindings++; return true })
							if success {
								require.Equal(t, "resp_synthetic_followup", bound)
								require.Equal(t, 1, digestBindings)
							} else {
								require.Empty(t, bound)
								require.Zero(t, digestBindings)
							}
						}
					})
				}
			}
		}
	}
}

func TestGatewayAudit_CompatMeteredFailureUsesFilteredOutboundTier(t *testing.T) {
	for _, route := range []string{"chat", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", route, stream), func(t *testing.T) {
				result, _, account, _, upstream, err := gatewayAuditCompatForward(t, route, "priority", stream, false, true)
				require.Error(t, err)
				require.NotNil(t, result)
				require.False(t, gjson.GetBytes(upstream.lastBody, "service_tier").Exists(), "configured filter removes priority before forwarding")
				require.Nil(t, result.ServiceTier, "a filtered tier must not be recovered from the original request or observed response")
				require.Equal(t, "priority", result.UpstreamResponseServiceTier)
				require.NotNil(t, result.ReasoningEffort)
				require.Equal(t, "high", *result.ReasoningEffort)
				resolution := ApplyOpenAIServiceTierBillingResolution(account, result)
				require.Empty(t, resolution.Billing)
				require.Nil(t, result.ServiceTier)
			})
		}
	}
}

func gatewayAuditCompatForward(t *testing.T, route, tier string, stream, success, filterPriority bool) (*OpenAIForwardResult, *OpenAIGatewayService, *Account, *gin.Context, *httpUpstreamRecorder, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	status, event := "failed", "response.failed"
	output := `[]`
	errorField := `,"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}`
	if success {
		status, event = "completed", "response.completed"
		output = `[{"id":"msg_synthetic","type":"message","role":"assistant","content":[{"type":"output_text","text":"synthetic answer","annotations":[]}]}]`
		errorField = ""
	}
	payload := fmt.Sprintf(`{"type":%q,"response":{"id":"resp_synthetic_followup","object":"response","model":"gpt-5.5","status":%q,"service_tier":%q,"output":%s,"usage":{"input_tokens":8,"output_tokens":2}%s}}`, event, status, tier, output, errorField)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"synthetic-followup"}}, Body: io.NopCloser(strings.NewReader("data: " + payload + "\n\n"))}}
	settings := DefaultOpenAIFastPolicySettings()
	if filterPriority {
		settings = openAIFastFilterPriorityPolicy()
	}
	svc := newOpenAIGatewayServiceWithSettings(t, settings)
	svc.cfg = rawChatCompletionsTestConfig()
	svc.tiboRouteDisabled = true
	svc.httpUpstream = upstream
	account := rawChatCompletionsTestAccount()
	account.Extra = map[string]any{"openai_responses_supported": true}
	body := []byte(fmt.Sprintf(`{"model":"gpt-5.5","messages":[{"role":"user","content":"synthetic"}],"service_tier":%q,"reasoning_effort":"high","stream":%t}`, tier, stream))
	path := "/v1/chat/completions"
	if route == "messages" {
		path = "/v1/messages"
		body = []byte(fmt.Sprintf(`{"model":"gpt-5.5","max_tokens":32,"messages":[{"role":"user","content":"synthetic"}],"output_config":{"effort":"high"},"stream":%t}`, stream))
	}
	c := adaptiveProtocolTestContext(path, body)
	var result *OpenAIForwardResult
	var err error
	if route == "messages" {
		c.Request.Header.Set("anthropic-beta", claude.BetaFastMode)
		result, err = svc.ForwardAsAnthropic(c.Request.Context(), c, account, body, "", "")
	} else {
		result, err = svc.ForwardAsChatCompletions(c.Request.Context(), c, account, body, "", "")
	}
	return result, svc, account, c, upstream, err
}

func TestGatewayAudit_MessagesMissingTerminalReplayBoundary(t *testing.T) {
	for _, ending := range []string{"eof", "done"} {
		for _, tc := range []struct {
			name, payload   string
			canceled, retry bool
			input           int
		}{
			{"unstarted", `{"type":"response.in_progress","response":{"id":"resp_synthetic"}}`, false, true, 0},
			{"usage", `{"type":"response.in_progress","response":{"id":"resp_synthetic","usage":{"input_tokens":8,"output_tokens":2}}}`, false, false, 8},
			{"hosted_tool", `{"type":"response.output_item.added","output_index":0,"item":{"id":"ws_synthetic","type":"web_search_call","status":"in_progress"}}`, false, false, 0},
			{"canceled", `{"type":"response.in_progress","response":{"id":"resp_synthetic"}}`, true, false, 0},
		} {
			t.Run(ending+"/"+tc.name, func(t *testing.T) {
				body := []byte(`{"model":"gpt-5.5","max_tokens":32,"messages":[{"role":"user","content":"synthetic"}],"stream":true}`)
				c := adaptiveProtocolTestContext("/v1/messages", body)
				if tc.canceled {
					ctx, cancel := context.WithCancel(c.Request.Context())
					cancel()
					c.Request = c.Request.WithContext(ctx)
				}
				wire := "data: " + tc.payload + "\n\n"
				if ending == "done" {
					wire += "data: [DONE]\n\n"
				}
				resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
				svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
				result, err := svc.handleAnthropicStreamingResponse(resp, c, rawChatCompletionsTestAccount(), "gpt-5.5", "gpt-5.5", "gpt-5.5", time.Now())
				require.Error(t, err)
				var retry *UpstreamFailoverError
				require.Equal(t, tc.retry, errors.As(err, &retry), "missing terminal cannot erase prior usage/tool progress or cancellation")
				require.NotNil(t, result)
				require.Equal(t, tc.input, result.Usage.InputTokens)
				if tc.input > 0 {
					require.Equal(t, 2, result.Usage.OutputTokens)
				}
			})
		}
	}
}
