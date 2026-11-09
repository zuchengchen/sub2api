package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGatewayAudit_PriorUsageSurvivesZeroFailure(t *testing.T) {
	body := "data: " + `{"type":"response.in_progress","response":{"id":"resp_synthetic","usage":{"input_tokens":8,"output_tokens":2}}}` + "\n\n" +
		"data: " + `{"type":"response.failed","response":{"id":"resp_synthetic","status":"failed","output":[],"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."},"usage":{"input_tokens":0,"output_tokens":0}}}` + "\n\n"
	for _, route := range []string{"responses_stream", "responses_passthrough_stream", "responses_buffered", "responses_passthrough_buffered", "chat_stream", "chat_buffered", "messages_stream", "messages_buffered"} {
		t.Run(route, func(t *testing.T) {
			usage, err := gatewayAuditRunRoute(t, route, body)
			require.Error(t, err)
			var retry *UpstreamFailoverError
			require.False(t, errors.As(err, &retry))
			require.NotNil(t, usage)
			require.Equal(t, 8, usage.InputTokens)
			require.Equal(t, 2, usage.OutputTokens)
		})
	}
}

func TestGatewayAudit_CompletedResponseStillSucceeds(t *testing.T) {
	body := "data: " + `{"type":"response.completed","response":{"id":"resp_synthetic","object":"response","status":"completed","output":[{"id":"msg_synthetic","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"synthetic answer","annotations":[]}]}],"usage":{"input_tokens":8,"output_tokens":2}}}` + "\n\n"
	for _, route := range []string{"responses_stream", "responses_passthrough_stream", "responses_buffered", "responses_passthrough_buffered", "chat_stream", "chat_buffered", "messages_stream", "messages_buffered"} {
		t.Run(route, func(t *testing.T) {
			usage, err := gatewayAuditRunRoute(t, route, body)
			require.NoError(t, err)
			require.NotNil(t, usage)
			require.Equal(t, 8, usage.InputTokens)
			require.Equal(t, 2, usage.OutputTokens)
		})
	}
}

// A transport can already have received a frame when Close unblocks its read.
// Release only on Close so the timeout branch, rather than select scheduling,
// deterministically drains these bytes after the deadline has won.
type gatewayAuditReleaseOnCloseBody struct {
	release chan struct{}
	once    sync.Once
	reader  *strings.Reader
}

func (b *gatewayAuditReleaseOnCloseBody) Read(p []byte) (int, error) {
	<-b.release
	return b.reader.Read(p)
}
func (b *gatewayAuditReleaseOnCloseBody) Close() error {
	b.once.Do(func() { close(b.release) })
	return nil
}

func TestGatewayAudit_TimeoutDrainObservesUpstreamWork(t *testing.T) {
	for _, timer := range []string{"first_output", "first_token"} {
		for _, tc := range []struct {
			name, payload string
			progressed    bool
			input         int
		}{
			{"preamble", `{"type":"response.created","response":{"id":"resp_synthetic"}}`, false, 0},
			{"usage", `{"type":"response.in_progress","response":{"id":"resp_synthetic","usage":{"input_tokens":8,"output_tokens":2}}}`, true, 8},
			{"hosted_tool", `{"type":"response.output_item.added","output_index":0,"item":{"id":"ws_synthetic","type":"web_search_call","status":"in_progress"}}`, true, 0},
		} {
			t.Run(timer+"/"+tc.name, func(t *testing.T) {
				c, _ := gatewayAuditNewContext(false)
				cfg := &config.Config{}
				if timer == "first_output" {
					cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 1
				} else {
					cfg.Gateway.StreamFirstTokenTimeout = 1
				}
				svc := &OpenAIGatewayService{cfg: cfg}
				body := &gatewayAuditReleaseOnCloseBody{release: make(chan struct{}), reader: strings.NewReader("data: " + tc.payload + "\n\n")}
				defer body.Close()
				resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}
				result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now().Add(-2*time.Second), "model", "model")
				require.Error(t, err)
				var retry *UpstreamFailoverError
				require.Equal(t, !tc.progressed, errors.As(err, &retry), "deadline drain must retain actual upstream work")
				require.NotNil(t, result)
				require.Equal(t, tc.input, result.usage.InputTokens)
			})
		}
	}
}

type gatewayAuditErrorReader struct{ err error }

func (r gatewayAuditErrorReader) Read([]byte) (int, error) { return 0, r.err }

type gatewayAuditTimeoutError struct{}

func (gatewayAuditTimeoutError) Error() string   { return "synthetic destination and sensitive marker" }
func (gatewayAuditTimeoutError) Timeout() bool   { return true }
func (gatewayAuditTimeoutError) Temporary() bool { return true }

func gatewayAuditNewContext(canceled bool) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if canceled {
		ctx, cancel := context.WithCancel(c.Request.Context())
		cancel()
		c.Request = c.Request.WithContext(ctx)
	}
	return c, rec
}

func TestGatewayAudit_BufferedReadErrorAfterWorkDoesNotReplay(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		input         int
	}{
		{"text", `{"type":"response.output_text.delta","delta":"synthetic output"}`, 0},
		{"hosted_tool", `{"type":"response.output_item.added","output_index":0,"item":{"id":"ws_synthetic","type":"web_search_call","status":"in_progress"}}`, 0},
		{"usage", `{"type":"response.in_progress","response":{"usage":{"input_tokens":8,"output_tokens":2}}}`, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gatewayAuditNewContext(false)
			resp := &http.Response{StatusCode: 200, Header: http.Header{"X-Request-Id": {"synthetic-upstream"}}, Body: io.NopCloser(io.MultiReader(strings.NewReader("data: "+tc.payload+"\n\n"), gatewayAuditErrorReader{io.ErrUnexpectedEOF}))}
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			result, err := svc.handleChatBufferedStreamingResponse(resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, "model", "model", "model", time.Now())
			require.Error(t, err)
			var retry *UpstreamFailoverError
			require.False(t, errors.As(err, &retry))
			if tc.input > 0 {
				require.NotNil(t, result)
				require.Equal(t, tc.input, result.Usage.InputTokens)
			}
		})
	}
}

func TestGatewayAudit_ReadFailureDiagnosticIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cause  error
		reason string
	}{
		{"unexpected_eof", io.ErrUnexpectedEOF, "upstream_stream_unexpected_eof"},
		{"closed", net.ErrClosed, "upstream_stream_connection_closed"},
		{"timeout", gatewayAuditTimeoutError{}, "upstream_stream_read_timeout"},
		{"http2", errors.New("http2: stream error synthetic sensitive marker"), OpenAIUpstreamHTTP2StreamErrorCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gatewayAuditNewContext(false)
			proxyID := int64(37)
			account := &Account{ID: 1, Platform: PlatformOpenAI, ProxyID: &proxyID, Proxy: &Proxy{ID: proxyID, Name: "synthetic-proxy"}}
			payload := "data: " + `{"type":"response.in_progress","response":{"usage":{"input_tokens":8,"output_tokens":2}}}` + "\n\n"
			resp := &http.Response{StatusCode: 200, Header: http.Header{"X-Request-Id": {"synthetic-real-upstream-id"}}, Body: io.NopCloser(io.MultiReader(strings.NewReader(payload), gatewayAuditErrorReader{tc.cause}))}
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			_, err := svc.handleChatStreamingResponse(resp, c, account, "model", "model", "model", time.Now(), nil)
			require.Error(t, err)
			raw, ok := c.Get(OpsUpstreamErrorsKey)
			require.True(t, ok)
			events, ok := raw.([]*OpsUpstreamErrorEvent)
			require.True(t, ok)
			require.Len(t, events, 1)
			ev := events[0]
			require.Equal(t, "stream_read_error", ev.Kind)
			require.Equal(t, "response_body", ev.Stage)
			require.Equal(t, tc.reason, ev.Reason)
			require.Equal(t, "synthetic-real-upstream-id", ev.UpstreamRequestID)
			require.NotNil(t, ev.ProxyID)
			require.Equal(t, proxyID, *ev.ProxyID)
			require.Equal(t, "synthetic-proxy", ev.ProxyName)
			require.Empty(t, ev.Detail)
			require.Empty(t, ev.UpstreamURL)
			require.Empty(t, ev.UpstreamResponseBody)
			encoded, err := json.Marshal(ev)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "sensitive marker")
		})
	}
}

func TestGatewayAudit_CancellationDoesNotReplayOrAddReadFailure(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "buffered"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			c, _ := gatewayAuditNewContext(true)
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(gatewayAuditErrorReader{context.Canceled})}
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			account := &Account{ID: 1, Platform: PlatformOpenAI}
			var err error
			if stream {
				_, err = svc.handleChatStreamingResponse(resp, c, account, "model", "model", "model", time.Now(), nil)
			} else {
				_, err = svc.handleChatBufferedStreamingResponse(resp, c, account, "model", "model", "model", time.Now())
			}
			require.Error(t, err)
			var retry *UpstreamFailoverError
			require.False(t, errors.As(err, &retry))
			_, exists := c.Get(OpsUpstreamErrorsKey)
			require.False(t, exists)
		})
	}
}

func TestGatewayAudit_PartialFailureDoesNotBindResponse(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, successful := range []bool{false, true} {
			name := "native"
			if passthrough {
				name = "passthrough"
			}
			if successful {
				name += "/success"
			} else {
				name += "/failure"
			}
			t.Run(name, func(t *testing.T) {
				terminal := `{"type":"response.failed","response":{"id":"resp_synthetic_binding","status":"failed","output":[],"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."},"usage":{"input_tokens":8,"output_tokens":2}}}`
				if successful {
					terminal = `{"type":"response.completed","response":{"id":"resp_synthetic_binding","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"synthetic"}]}],"usage":{"input_tokens":8,"output_tokens":2}}}`
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + terminal + "\n\n"))}}
				svc := newOpenAIImageGenerationControlTestService(upstream)
				svc.cache = nil
				c, _ := newOpenAIImageGenerationControlTestContext(true, "synthetic-audit")
				SetOpenAIHTTPResponseOwner(c, 100, 2424)
				account := newOpenAIImageGenerationControlTestAccount()
				account.Extra = map[string]any{"openai_passthrough": passthrough}
				result, err := svc.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-5.6-sol","stream":true,"input":"synthetic"}`))
				if successful {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
				require.NotNil(t, result)
				require.Equal(t, 8, result.Usage.InputTokens)
				bound, err := svc.getOpenAIWSStateStore().GetResponseAccount(context.Background(), 4242, "resp_synthetic_binding")
				require.NoError(t, err)
				if successful {
					require.Equal(t, account.ID, bound)
				} else {
					require.Zero(t, bound)
				}
			})
		}
	}
}

func TestGatewayAudit_MeteredCompactionMissingItemDoesNotReplay(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		name := "native"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: nativeCompactionSSEResponse(false)}
			svc := openAIClientToolsTestService(upstream)
			account := excelAccount()
			account.Extra = astraHealthyTestExtra(map[string]any{"openai_passthrough": passthrough})
			c, rec := newNativeCompactionContext("/v1/responses")
			result, err := svc.Forward(context.Background(), c, account, []byte(nativeCompactionBody))
			require.Error(t, err)
			var retry *UpstreamFailoverError
			require.False(t, errors.As(err, &retry), "the original 120/30 fixture has already billed this attempt")
			require.NotNil(t, result)
			require.Equal(t, 120, result.Usage.InputTokens)
			require.Equal(t, 30, result.Usage.OutputTokens)
			require.NotContains(t, rec.Body.String(), "response.completed")
		})
	}
}
