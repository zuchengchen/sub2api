package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Keep the real HTTP transport on both sides of the gateway: a recorder cannot
// prove that comments reached a proxy before upstream headers or semantic output.
type responsesWireUpstream struct {
	client *http.Client
	target *url.URL
}

func (u *responsesWireUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	forward := req.Clone(req.Context())
	target := *u.target
	target.Path = req.URL.Path
	forward.URL = &target
	forward.Host = target.Host
	return u.client.Do(forward)
}

func (u *responsesWireUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

type responsesWireOutcome struct {
	result      *OpenAIForwardResult
	err         error
	beforeRetry int
}

func readResponsesWireComment(t *testing.T, reader *bufio.Reader) {
	t.Helper()
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(line, ":"), "expected only a transport comment, got %q", line)
	line, err = reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "\n", line)
}

func TestOpenAIResponsesPreoutputWire(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		lane := "native"
		if passthrough {
			lane = "passthrough"
		}
		for _, scenario := range []string{"headers", "partial_event", "early_error", "late_error", "failover", "cancel"} {
			t.Run(lane+"/"+scenario, func(t *testing.T) {
				release := make(chan struct{})
				releaseRetry := make(chan struct{})
				shutdown := make(chan struct{})
				defer close(shutdown)
				var attempts atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					attempt := attempts.Add(1)
					if scenario == "partial_event" {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "event: response.created\n")
						w.(http.Flusher).Flush()
					}
					if scenario != "early_error" {
						gate := release
						if attempt > 1 {
							gate = releaseRetry
						}
						select {
						case <-gate:
						case <-r.Context().Done():
							return
						case <-shutdown:
							return
						}
					}
					if scenario == "early_error" || scenario == "late_error" || (scenario == "failover" && attempt <= 2) {
						w.Header().Set("Content-Type", "application/json")
						status := http.StatusBadRequest
						if scenario == "failover" {
							status = http.StatusServiceUnavailable
						}
						w.WriteHeader(status)
						_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"invalid_value","param":"input","message":"Invalid input value"}}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					if scenario == "partial_event" {
						_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_wire\"}}\n\n")
					}
					_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"+
						"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_wire\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n")
					w.(http.Flusher).Flush()
					// Completion must not depend on upstream EOF.
					select {
					case <-r.Context().Done():
					case <-shutdown:
					}
				}))
				t.Cleanup(upstream.Close)
				target, err := url.Parse(upstream.URL)
				require.NoError(t, err)
				svc := &OpenAIGatewayService{
					cfg:               &config.Config{Gateway: config.GatewayConfig{StreamKeepaliveInterval: 1, MaxLineSize: defaultMaxLineSize}},
					httpUpstream:      &responsesWireUpstream{client: upstream.Client(), target: target},
					tiboRouteDisabled: true,
				}
				if scenario == "early_error" {
					svc.cfg.Gateway.StreamKeepaliveInterval = 3600
				}
				account := &Account{
					ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
					Status: StatusActive, Schedulable: true,
					Credentials: map[string]any{"api_key": "test-key", "base_url": "https://example.com"},
					Extra:       map[string]any{"openai_passthrough": passthrough, "cancel_upstream_on_client_disconnect": true},
				}
				outcomes := make(chan responsesWireOutcome, 1)
				retryReady := make(chan int, 1)
				router := gin.New()
				router.POST("/v1/responses", func(c *gin.Context) {
					body, readErr := io.ReadAll(c.Request.Body)
					if readErr != nil {
						outcomes <- responsesWireOutcome{err: readErr}
						return
					}
					result, forwardErr := svc.Forward(c.Request.Context(), c, account, body)
					beforeRetry := 0
					for retry := 0; scenario == "failover" && retry < 2; retry++ {
						var failoverErr *UpstreamFailoverError
						if !errors.As(forwardErr, &failoverErr) {
							outcomes <- responsesWireOutcome{result: result, err: forwardErr}
							return
						}
						beforeRetry = OpenAICompactKeepaliveAdjustedWrittenSize(c)
						retryReady <- beforeRetry
						result, forwardErr = svc.Forward(c.Request.Context(), c, account, body)
					}
					outcomes <- responsesWireOutcome{result: result, err: forwardErr, beforeRetry: beforeRetry}
				})
				gateway := httptest.NewServer(router)
				t.Cleanup(gateway.Close)
				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.2","stream":true,"input":"hello"}`))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")
				resp, err := gateway.Client().Do(req)
				require.NoError(t, err, "downstream headers must not wait for upstream semantic output")
				defer resp.Body.Close()
				reader := bufio.NewReader(resp.Body)
				if scenario != "early_error" {
					require.Equal(t, http.StatusOK, resp.StatusCode)
					require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
					readResponsesWireComment(t, reader)
					readResponsesWireComment(t, reader)
					if scenario == "cancel" {
						cancel()
						if passthrough {
							// Preserve the existing detached-upstream policy: stop
							// downstream heartbeats but drain the finite response.
							close(release)
						}
					} else {
						close(release)
					}
				}
				if scenario == "failover" {
					select {
					case size := <-retryReady:
						require.Equal(t, -1, size, "header-wait comments must not block failover")
					case <-ctx.Done():
						t.Fatal("first attempt did not remain eligible for failover")
					}
					readResponsesWireComment(t, reader)
					readResponsesWireComment(t, reader)
					close(releaseRetry)
				}
				var outcome responsesWireOutcome
				select {
				case outcome = <-outcomes:
				case <-time.After(3 * time.Second):
					t.Fatal("forward did not terminate after completion, failure, or cancellation")
				}
				if scenario == "cancel" {
					if passthrough {
						require.NoError(t, outcome.err)
						require.NotNil(t, outcome.result)
						require.Equal(t, 3, outcome.result.Usage.InputTokens)
						require.Equal(t, 1, outcome.result.Usage.OutputTokens)
					} else {
						require.ErrorIs(t, outcome.err, context.Canceled)
					}
					return
				}
				wire, err := io.ReadAll(reader)
				require.NoError(t, err)
				body := stripKeepaliveComments(string(wire))
				if scenario == "early_error" {
					require.Error(t, outcome.err)
					require.Equal(t, http.StatusBadRequest, resp.StatusCode)
					require.True(t, gjson.Valid(body))
					require.False(t, strings.Contains(string(wire), ": keepalive"))
					return
				}
				if scenario == "late_error" {
					require.Error(t, outcome.err)
					events := parseCompactBridgeSSE(t, body)
					require.Len(t, events, 1)
					payload := events[0][1]
					require.Equal(t, "response.failed", gjson.Get(payload, "type").String())
					require.Equal(t, "failed", gjson.Get(payload, "response.status").String())
					require.False(t, gjson.Get(payload, "response.usage").Exists())
					if !passthrough {
						require.Equal(t, "invalid_value", gjson.Get(payload, "response.error.code").String())
						require.Equal(t, "input", gjson.Get(payload, "response.error.param").String())
					}
					return
				}
				require.NoError(t, outcome.err)
				require.NotNil(t, outcome.result)
				require.Equal(t, 3, outcome.result.Usage.InputTokens)
				require.Equal(t, 1, outcome.result.Usage.OutputTokens)
				require.Contains(t, body, `"delta":"hello"`)
				require.Equal(t, 1, strings.Count(body, "event: response.completed"))
				require.NotContains(t, body, "response.failed")
				if scenario == "partial_event" {
					require.Contains(t, body, "event: response.created\ndata:")
				}
				if scenario == "failover" {
					require.Equal(t, int32(3), attempts.Load())
					require.Equal(t, -1, outcome.beforeRetry)
				}
			})
		}
	}
}
