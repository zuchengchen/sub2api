//go:build unit

package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func gatewayAuditCyberTerminal(metered bool) string {
	usage := `{"input_tokens":0,"output_tokens":0}`
	if metered {
		usage = `{"input_tokens":8,"output_tokens":2}`
	}
	return fmt.Sprintf(`{"type":"response.failed","response":{"id":"resp_synthetic_cyber","status":"failed","error":{"code":"cyber_policy","message":"Synthetic policy failure"},"output":[],"usage":%s}}`, usage)
}

func TestGatewayAudit_BufferedCyberUsesExistingSettlement(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, metered := range []bool{false, true} {
			t.Run(fmt.Sprintf("passthrough=%t/metered=%t", passthrough, metered), func(t *testing.T) {
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + gatewayAuditCyberTerminal(metered) + "\n\n"))}}
				svc := newOpenAIImageGenerationControlTestService(upstream)
				c, _ := newOpenAIImageGenerationControlTestContext(true, "synthetic-audit")
				account := newOpenAIImageGenerationControlTestAccount()
				account.Extra = map[string]any{"openai_passthrough": passthrough}
				result, err := svc.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-5.5","stream":false,"input":"synthetic"}`))
				require.Error(t, err)
				var retry *UpstreamFailoverError
				require.False(t, errors.As(err, &retry))
				require.Len(t, upstream.requests, 1)
				require.Nil(t, result, "cyber failures retain their dedicated settlement path")
				mark := GetOpsCyberPolicy(c)
				require.NotNil(t, mark)
				require.Equal(t, "cyber_policy", mark.Code)
				if metered {
					require.Equal(t, 8, mark.UpstreamInTok)
					require.Equal(t, 2, mark.UpstreamOutTok)
				} else {
					require.Zero(t, mark.UpstreamInTok)
					require.Zero(t, mark.UpstreamOutTok)
				}
			})
		}
	}
}

func TestGatewayAudit_TimeoutDrainCyberUsesExistingSettlement(t *testing.T) {
	for _, timer := range []string{"first_output", "first_token"} {
		for _, metered := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/metered=%t", timer, metered), func(t *testing.T) {
				body := &gatewayAuditReleaseOnCloseBody{release: make(chan struct{}), reader: strings.NewReader("data: " + gatewayAuditCyberTerminal(metered) + "\n\n")}
				defer body.Close()
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}}
				svc := newOpenAIImageGenerationControlTestService(upstream)
				if timer == "first_output" {
					svc.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 1
				} else {
					svc.cfg.Gateway.StreamFirstTokenTimeout = 1
				}
				c, _ := newOpenAIImageGenerationControlTestContext(true, "synthetic-audit")
				result, err := svc.Forward(c.Request.Context(), c, newOpenAIImageGenerationControlTestAccount(), []byte(`{"model":"gpt-5.5","stream":true,"input":"synthetic"}`))
				require.Error(t, err)
				var retry *UpstreamFailoverError
				require.False(t, errors.As(err, &retry))
				require.Nil(t, result)
				mark := GetOpsCyberPolicy(c)
				require.NotNil(t, mark)
				if metered {
					require.Equal(t, 8, mark.UpstreamInTok)
					require.Equal(t, 2, mark.UpstreamOutTok)
				} else {
					require.Zero(t, mark.UpstreamInTok)
					require.Zero(t, mark.UpstreamOutTok)
				}
			})
		}
	}
}

func TestGatewayAudit_TimeoutDrainRetainsBillingObservation(t *testing.T) {
	for _, timer := range []string{"first_output", "first_token"} {
		t.Run(timer, func(t *testing.T) {
			payload := `{"type":"response.completed","response":{"id":"resp_synthetic_drained","model":"gpt-5.5","service_tier":"flex","status":"completed","output":[],"usage":{"input_tokens":8,"output_tokens":2}}}`
			body := &gatewayAuditReleaseOnCloseBody{release: make(chan struct{}), reader: strings.NewReader("data: " + payload + "\n\n")}
			defer body.Close()
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}}
			svc := newOpenAIImageGenerationControlTestService(upstream)
			svc.cache = nil
			if timer == "first_output" {
				svc.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 1
			} else {
				svc.cfg.Gateway.StreamFirstTokenTimeout = 1
			}
			c, rec := newOpenAIImageGenerationControlTestContext(true, "synthetic-audit")
			account := newOpenAIImageGenerationControlTestAccount()
			result, err := svc.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-5.5","stream":true,"service_tier":"priority","input":"synthetic"}`))
			require.Error(t, err, "a terminal seen only while draining a timed-out attempt does not publish success")
			var retry *UpstreamFailoverError
			require.False(t, errors.As(err, &retry))
			require.NotNil(t, result)
			require.Equal(t, 8, result.Usage.InputTokens)
			require.Equal(t, "gpt-5.5", result.UpstreamResponseModel)
			require.Equal(t, "flex", result.UpstreamResponseServiceTier)
			require.NotNil(t, result.ServiceTier)
			require.Equal(t, "priority", *result.ServiceTier)
			require.Equal(t, "flex", ApplyOpenAIServiceTierBillingResolution(account, result).Billing)
			require.NotContains(t, rec.Body.String(), "response.completed")
			bound, err := svc.getOpenAIWSStateStore().GetResponseAccount(c.Request.Context(), 4242, "resp_synthetic_drained")
			require.NoError(t, err)
			require.Zero(t, bound)
		})
	}
}

type gatewayAuditPrefixUntilCloseBody struct {
	prefix *strings.Reader
	closed chan struct{}
	once   sync.Once
}

func (b *gatewayAuditPrefixUntilCloseBody) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	<-b.closed
	return 0, io.EOF
}
func (b *gatewayAuditPrefixUntilCloseBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestGatewayAudit_FirstOutputTimeoutKeepsPartialFramePrivate(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		retry         bool
	}{
		{"valid_delta_without_blank_line", `{"type":"response.output_text.delta","delta":"synthetic pending output"}`, false},
		{"metadata_without_blank_line", `{"type":"response.created","response":{"id":"resp_synthetic"}}`, true},
		{"incomplete_json_without_blank_line", `{"type":"response.output_text.delta","delta":"synthetic pending`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := gatewayAuditNewContext(false)
			svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{OpenAIFirstOutputTimeoutSeconds: 1}}}
			body := &gatewayAuditPrefixUntilCloseBody{prefix: strings.NewReader("data: " + tc.payload + "\n"), closed: make(chan struct{})}
			defer body.Close()
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}
			_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now(), "model", "model")
			require.Error(t, err)
			var retry *UpstreamFailoverError
			require.Equal(t, tc.retry, errors.As(err, &retry))
			require.NotContains(t, rec.Body.String(), "synthetic pending", "undispatched event payload stays private")
			if tc.retry {
				require.Empty(t, rec.Body.String())
				return
			}
			frames := 0
			forEachOpenAISSEFrame(rec.Body.String(), func(eventType string, payload []byte) {
				frames++
				require.True(t, json.Valid(payload), "a partial data line must not be joined to the error frame")
				require.Equal(t, "error", gjson.GetBytes(payload, "type").String())
			})
			require.Equal(t, 1, frames)
		})
	}
}

func TestGatewayAudit_MeteredResponsesFailureKeepsPassthroughRule(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%t", passthrough), func(t *testing.T) {
			c, rec := gatewayAuditNewContext(false)
			bindPassthroughRule(c, PlatformOpenAI, []string{"context_length_exceeded"}, http.StatusBadRequest)
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"synthetic-rule"}}, Body: io.NopCloser(strings.NewReader(buildContextLengthFailedSSE()))}
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			account := &Account{ID: 1, Platform: PlatformOpenAI}
			var usage *OpenAIUsage
			var err error
			if passthrough {
				result, gotErr := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
				err = gotErr
				if result != nil {
					usage = result.usage
				}
			} else {
				result, gotErr := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
				err = gotErr
				if result != nil {
					usage = result.usage
				}
			}
			require.Error(t, err)
			var retry *UpstreamFailoverError
			require.False(t, errors.As(err, &retry))
			require.NotNil(t, usage)
			require.Equal(t, 100000, usage.InputTokens)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.True(t, json.Valid(rec.Body.Bytes()))
			require.Contains(t, gjson.GetBytes(rec.Body.Bytes(), "error.message").String(), "context window")
			raw, ok := c.Get(OpsUpstreamErrorsKey)
			require.True(t, ok)
			events, ok := raw.([]*OpsUpstreamErrorEvent)
			require.True(t, ok)
			require.Len(t, events, 1, "the terminal failure is recorded once when a non-retry rule matches")
		})
	}
}

func TestGatewayAudit_MeteredAnthropicRateLimitDoesNotReplay(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c := adaptiveProtocolTestContext("/v1/messages", body)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(buildResponsesFailedSSEStream("rate_limit_error", "Rate limit reached")))}}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	result, err := svc.ForwardAsAnthropic(c.Request.Context(), c, rawChatCompletionsTestAccount(), body, "", "")
	require.Error(t, err)
	var retry *UpstreamFailoverError
	require.False(t, errors.As(err, &retry))
	require.NotNil(t, result)
	require.Equal(t, 10, result.Usage.InputTokens)
	require.Len(t, upstream.requests, 1)
}
