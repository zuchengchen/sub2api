//go:build unit

package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const oauthReliabilityChatCreated = `{"type":"response.created","response":{"id":"resp_synthetic","model":"gpt-5.5","status":"in_progress","output":[]}}`
const oauthReliabilityChatUsage = `{"type":"response.in_progress","response":{"usage":{"input_tokens":19,"output_tokens":7,"input_tokens_details":{"cached_tokens":4}}}}`
const oauthReliabilityChatZeroUsage = `{"type":"response.in_progress","response":{"usage":{"input_tokens":0,"output_tokens":0}}}`
const oauthReliabilityChatText = `{"type":"response.output_text.delta","delta":"synthetic answer"}`
const oauthReliabilityChatRequest = `{"model":"gpt-5.5","messages":[{"role":"user","content":"synthetic request private marker"}],"stream":true}`

func oauthReliabilityChatSSE(payloads ...string) string {
	var out strings.Builder
	for _, payload := range payloads {
		out.WriteString("data: " + payload + "\n\n")
	}
	return out.String()
}

type oauthReliabilityChatErrorReader struct{ err error }

func (r oauthReliabilityChatErrorReader) Read([]byte) (int, error) { return 0, r.err }

type oauthReliabilityChatTimeoutError struct{}

func (oauthReliabilityChatTimeoutError) Error() string {
	return "private upstream destination timed out"
}
func (oauthReliabilityChatTimeoutError) Timeout() bool   { return true }
func (oauthReliabilityChatTimeoutError) Temporary() bool { return true }

type oauthReliabilityChatHarness struct {
	svc     *OpenAIGatewayService
	c       *gin.Context
	rec     *httptest.ResponseRecorder
	account *Account
	logs    *observer.ObservedLogs
}

func newOAuthReliabilityChatHarness(async bool) *oauthReliabilityChatHarness {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	core, logs := observer.New(zap.InfoLevel)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).
		WithContext(logger.IntoContext(context.Background(), zap.New(core)))
	cfg := &config.Config{}
	if async {
		// Exercise the channel path without waiting for a real timer tick.
		cfg.Gateway.StreamDataIntervalTimeout = 60
		cfg.Gateway.StreamKeepaliveInterval = 60
	}
	return &oauthReliabilityChatHarness{
		svc: &OpenAIGatewayService{cfg: cfg}, c: c, rec: rec, logs: logs,
		account: &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Name: "synthetic"},
	}
}

func (h *oauthReliabilityChatHarness) run(wire string, readErr error, body []byte) (*OpenAIForwardResult, error) {
	var reader io.Reader = strings.NewReader(wire)
	if readErr != nil {
		reader = io.MultiReader(reader, oauthReliabilityChatErrorReader{err: readErr})
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"synthetic-upstream-request"}},
		Body:       io.NopCloser(reader),
	}
	defer resp.Body.Close()
	return h.svc.handleChatStreamingResponse(resp, h.c, h.account, "gpt-5.5", "billing-model", "gpt-5.5", time.Now(), body)
}

func (h *oauthReliabilityChatHarness) boundary(t *testing.T) map[string]any {
	t.Helper()
	entries := h.logs.FilterMessage("openai chat_completions stream replay boundary").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Len(t, fields, 6, "diagnostics contain only fixed fields, with no request/response content")
	encoded, err := json.Marshal(fields)
	require.NoError(t, err)
	require.Less(t, len(encoded), 256)
	require.NotContains(t, string(encoded), "private")
	require.NotContains(t, string(encoded), "synthetic-upstream-request")
	return fields
}

func TestOAuthReliabilityChatReadFailureBeforeProgressReturnsFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, async := range []bool{false, true} {
		for _, prior := range []struct{ name, wire, usageSource string }{
			{"empty", "", "absent"},
			{"metadata", oauthReliabilityChatSSE(oauthReliabilityChatCreated, `{"type":"response.in_progress","response":{"id":"resp_synthetic"}}`), "absent"},
			{"heartbeat", ": synthetic heartbeat\n\nevent: ping\ndata: {}\n\n" + oauthReliabilityChatSSE(oauthReliabilityChatCreated), "absent"},
			{"zero_usage", oauthReliabilityChatSSE(oauthReliabilityChatCreated, oauthReliabilityChatZeroUsage), "upstream"},
		} {
			for _, ending := range []struct {
				name, suffix, code string
				err                error
			}{
				{name: "eof", code: OpenAIUpstreamStreamTruncatedCode},
				{name: "done_without_terminal", suffix: "data: [DONE]\n\n", code: OpenAIUpstreamStreamTruncatedCode},
				{name: "unexpected_eof", err: io.ErrUnexpectedEOF, code: OpenAIUpstreamStreamReadErrorCode},
				{name: "http2_reset", err: errors.New("stream error: stream ID 7; private destination"), code: OpenAIUpstreamHTTP2StreamErrorCode},
				{name: "read_timeout", err: oauthReliabilityChatTimeoutError{}, code: OpenAIUpstreamStreamReadErrorCode},
			} {
				t.Run(fmt.Sprintf("async=%t/%s/%s", async, prior.name, ending.name), func(t *testing.T) {
					h := newOAuthReliabilityChatHarness(async)
					result, err := h.run(prior.wire+ending.suffix, ending.err, []byte(oauthReliabilityChatRequest))
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.Nil(t, result, "an unstarted replay candidate must not create an estimated billing result")
					require.False(t, h.c.Writer.Written())
					require.Empty(t, h.rec.Body.String(), "metadata must stay private on an abandoned attempt")
					require.Equal(t, http.StatusBadGateway, failover.StatusCode)
					require.Equal(t, ending.code, gjson.GetBytes(failover.ResponseBody, "error.code").String())
					require.Equal(t, "synthetic-upstream-request", failover.ResponseHeaders.Get("x-request-id"))
					require.False(t, failover.SafeToFailoverAfterWrite, "outer commit checks remain authoritative")
					require.Zero(t, failover.SameAccountRetryMax, "do not add an independent retry budget")
					fields := h.boundary(t)
					require.Equal(t, false, fields["upstream_progress_observed"])
					require.Equal(t, false, fields["client_output_started"])
					require.Equal(t, false, fields["terminal_seen"])
					require.Equal(t, prior.usageSource, fields["usage_source"])
					require.Equal(t, true, fields["retry_requested"])
				})
			}
		}
	}
}

func TestOAuthReliabilityChatReadFailureDoesNotReplayProgress(t *testing.T) {
	for _, async := range []bool{false, true} {
		for _, prior := range []struct{ name, payload string }{
			{"text", oauthReliabilityChatText},
			{"reasoning", `{"type":"response.reasoning_text.delta","delta":"synthetic reasoning"}`},
			{"function_args", `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`},
			{"custom_tool_input", `{"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"synthetic input"}`},
			{"hosted_tool", `{"type":"response.output_item.added","item":{"type":"web_search_call","id":"ws_synthetic","status":"in_progress"}}`},
			{"tool_usage", `{"type":"response.in_progress","response":{"tool_usage":{"web_search":{"calls":1}}}}`},
			{"actual_usage", oauthReliabilityChatUsage},
			{"typed_decoder_failure", `{"type":"response.output_text.delta","delta":17}`},
		} {
			for _, ending := range []struct {
				name, suffix string
				err          error
			}{{name: "eof"}, {name: "done", suffix: "data: [DONE]\n\n"}, {name: "read_error", err: io.ErrUnexpectedEOF}} {
				t.Run(fmt.Sprintf("async=%t/%s/%s", async, prior.name, ending.name), func(t *testing.T) {
					h := newOAuthReliabilityChatHarness(async)
					wire := oauthReliabilityChatSSE(oauthReliabilityChatCreated, prior.payload, oauthReliabilityChatZeroUsage) + ending.suffix
					result, err := h.run(wire, ending.err, []byte(oauthReliabilityChatRequest))
					require.Error(t, err)
					var failover *UpstreamFailoverError
					require.NotErrorAs(t, err, &failover)
					require.NotNil(t, result)
					require.NotContains(t, h.rec.Body.String(), "[DONE]", "an incomplete turn must not be published as completed")
					if prior.name == "actual_usage" {
						require.Equal(t, 19, result.Usage.InputTokens)
						require.Equal(t, 7, result.Usage.OutputTokens)
						require.Equal(t, 4, result.Usage.CacheReadInputTokens)
					}
					fields := h.boundary(t)
					require.Equal(t, true, fields["upstream_progress_observed"], "later zero usage cannot clear progress")
					require.Equal(t, false, fields["retry_requested"])
				})
			}
		}
	}
}

func TestOAuthReliabilityChatPendingFrameReadFailurePreservesReplayBoundary(t *testing.T) {
	for _, async := range []bool{false, true} {
		for _, trailingNewline := range []string{"", "\n"} {
			for _, tc := range []struct {
				name, payload string
				retry         bool
				terminal      bool
			}{
				{name: "metadata", payload: oauthReliabilityChatCreated, retry: true},
				{name: "zero_usage", payload: oauthReliabilityChatZeroUsage, retry: true},
				{name: "text", payload: oauthReliabilityChatText},
				{name: "usage", payload: oauthReliabilityChatUsage},
				{name: "tool", payload: `{"type":"response.output_item.added","item":{"type":"web_search_call","status":"in_progress"}}`},
				{name: "completed", payload: `{"type":"response.completed","response":{"status":"completed","output":[]}}`, terminal: true},
			} {
				t.Run(fmt.Sprintf("async=%t/newline=%t/%s", async, trailingNewline != "", tc.name), func(t *testing.T) {
					h := newOAuthReliabilityChatHarness(async)
					result, err := h.run("data: "+tc.payload+trailingNewline, io.ErrUnexpectedEOF, nil)
					require.Error(t, err)
					var failover *UpstreamFailoverError
					require.Equal(t, tc.retry, errors.As(err, &failover))
					require.Empty(t, h.rec.Body.String(), "undelimited frames are observed privately")
					if tc.name == "usage" {
						require.NotNil(t, result)
						require.Equal(t, 19, result.Usage.InputTokens)
						require.Equal(t, 7, result.Usage.OutputTokens)
					}
					fields := h.boundary(t)
					require.Equal(t, tc.terminal, fields["terminal_seen"])
					require.Equal(t, tc.retry, fields["retry_requested"])
				})
			}
		}
	}
}

func TestOAuthReliabilityChatReadFailureDoesNotReplayCancellationOrCommit(t *testing.T) {
	for _, async := range []bool{false, true} {
		for _, tc := range []struct {
			name  string
			err   error
			setup func(*oauthReliabilityChatHarness)
		}{
			{name: "canceled_read", err: context.Canceled},
			{name: "deadline_read", err: context.DeadlineExceeded},
			{name: "line_limit", err: bufio.ErrTooLong},
			{name: "body_limit", err: ErrUpstreamResponseBodyTooLarge},
			{name: "canceled_request", setup: func(h *oauthReliabilityChatHarness) {
				ctx, cancel := context.WithCancel(h.c.Request.Context())
				cancel()
				h.c.Request = h.c.Request.WithContext(ctx)
			}},
			{name: "expired_request", setup: func(h *oauthReliabilityChatHarness) {
				ctx, cancel := context.WithDeadline(h.c.Request.Context(), time.Now().Add(-time.Second))
				cancel()
				h.c.Request = h.c.Request.WithContext(ctx)
			}},
			{name: "committed_marker", setup: func(h *oauthReliabilityChatHarness) { MarkResponseCommitted(h.c) }},
			{name: "committed_headers", setup: func(h *oauthReliabilityChatHarness) { h.c.Writer.WriteHeaderNow() }},
			{name: "client_keepalive", setup: func(h *oauthReliabilityChatHarness) { _, _ = h.c.Writer.Write([]byte(":\n\n")) }},
		} {
			t.Run(fmt.Sprintf("async=%t/%s", async, tc.name), func(t *testing.T) {
				h := newOAuthReliabilityChatHarness(async)
				if tc.setup != nil {
					tc.setup(h)
				}
				_, err := h.run(oauthReliabilityChatSSE(oauthReliabilityChatCreated), tc.err, nil)
				require.Error(t, err)
				var failover *UpstreamFailoverError
				require.NotErrorAs(t, err, &failover)
			})
		}
	}
}

func TestOAuthReliabilityChatCompletedStreamKeepsOutputAndUsage(t *testing.T) {
	for _, async := range []bool{false, true} {
		for _, terminal := range []string{"response.completed", "response.done", "response.incomplete"} {
			t.Run(fmt.Sprintf("async=%t/%s", async, terminal), func(t *testing.T) {
				h := newOAuthReliabilityChatHarness(async)
				completed := fmt.Sprintf(`{"type":%q,"response":{"id":"resp_synthetic","model":"gpt-5.5","status":"completed","output":[],"usage":{"input_tokens":19,"output_tokens":7,"input_tokens_details":{"cached_tokens":4}}}}`, terminal)
				wire := oauthReliabilityChatSSE(oauthReliabilityChatCreated, oauthReliabilityChatCreated, oauthReliabilityChatText, completed)
				result, err := h.run(wire, io.ErrUnexpectedEOF, []byte(oauthReliabilityChatRequest))
				require.NoError(t, err, "a completed turn ignores subsequent transport closure")
				require.NotNil(t, result)
				require.Equal(t, 19, result.Usage.InputTokens)
				require.Equal(t, 7, result.Usage.OutputTokens)
				require.Equal(t, 4, result.Usage.CacheReadInputTokens)
				require.Equal(t, "billing-model", result.BillingModel)
				require.Equal(t, 1, strings.Count(h.rec.Body.String(), `"role":"assistant"`))
				require.Equal(t, 1, strings.Count(h.rec.Body.String(), `"content":"synthetic answer"`))
				require.Equal(t, 1, strings.Count(h.rec.Body.String(), "data: [DONE]"))
				require.Empty(t, h.logs.FilterMessage("openai chat_completions stream replay boundary").All())
			})
		}
	}
}

func TestOAuthReliabilityChatReadFailureBillingAndDiagnostics(t *testing.T) {
	for _, tc := range []struct{ name, wire, source string }{
		{"absent", oauthReliabilityChatSSE(oauthReliabilityChatCreated), "absent"},
		{"actual", oauthReliabilityChatSSE(oauthReliabilityChatUsage, oauthReliabilityChatZeroUsage), "upstream"},
		{"estimated", oauthReliabilityChatSSE(oauthReliabilityChatCreated, oauthReliabilityChatText), "estimated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newOAuthReliabilityChatHarness(false)
			result, err := h.run(tc.wire, io.ErrUnexpectedEOF, []byte(oauthReliabilityChatRequest))
			require.Error(t, err)
			fields := h.boundary(t)
			require.Equal(t, tc.source, fields["usage_source"])
			if tc.source == "absent" {
				require.Nil(t, result)
				return
			}
			require.NotNil(t, result)
			if tc.source == "upstream" {
				require.Equal(t, OpenAIUsage{InputTokens: 19, OutputTokens: 7, CacheReadInputTokens: 4}, result.Usage)
			} else {
				require.Positive(t, result.Usage.InputTokens, "retain existing estimation after non-replayable output")
				require.Positive(t, result.Usage.OutputTokens)
			}
		})
	}
}

func TestOAuthReliabilityChatRecoveryLeavesNonOAuthBehavior(t *testing.T) {
	for _, account := range []*Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeSetupToken},
		{ID: 1, Platform: PlatformGrok, Type: AccountTypeOAuth},
	} {
		t.Run(account.Platform+"/"+account.Type, func(t *testing.T) {
			h := newOAuthReliabilityChatHarness(false)
			h.account = account
			result, err := h.run("", io.ErrUnexpectedEOF, nil)
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.NotErrorAs(t, err, &failover)
			require.NotNil(t, result)
		})
	}
}

func TestOAuthReliabilityConcurrentChatReplayBoundariesAreRequestLocal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sharedService := &OpenAIGatewayService{cfg: &config.Config{}}
	for i := 0; i < 12; i++ {
		t.Run(fmt.Sprintf("request_%02d", i), func(t *testing.T) {
			t.Parallel()
			h := newOAuthReliabilityChatHarness(false)
			h.svc = sharedService
			wire := oauthReliabilityChatSSE(oauthReliabilityChatCreated)
			progressed := i%2 != 0
			if progressed {
				wire += oauthReliabilityChatSSE(oauthReliabilityChatUsage)
			}
			result, err := h.run(wire, io.ErrUnexpectedEOF, nil)
			var failover *UpstreamFailoverError
			require.Equal(t, !progressed, errors.As(err, &failover))
			if progressed {
				require.NotNil(t, result)
				require.Equal(t, 19, result.Usage.InputTokens)
			} else {
				require.Nil(t, result)
			}
			require.Equal(t, progressed, h.boundary(t)["upstream_progress_observed"])
		})
	}
}
