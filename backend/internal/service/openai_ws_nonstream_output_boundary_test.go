package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Non-streaming delivery has not written a client byte when these upstream
// events arrive. It is still unsafe to submit that logical inference twice.
func TestHTTPToWSNonstreamUpstreamOutputStopsReplay(t *testing.T) {
	prefixes := []struct{ name, event string }{
		{"text_done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_partial","role":"assistant","content":[{"type":"output_text","text":"already generated"}]}}`},
		{"text_delta", `{"type":"response.output_text.delta","output_index":0,"item_id":"msg_partial","delta":"already generated"}`},
		{"hosted_tool", `{"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","id":"ws_partial","status":"completed","action":{"type":"search","query":"synthetic lookup"}}}`},
		{"image_done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"image_generation_call","id":"ig_partial","status":"completed","result":"c3ludGhldGljLWltYWdl","size":"1024x1024"}}`},
	}
	for _, inferred := range []bool{false, true} {
		for _, failure := range []string{"disconnect", "capacity_error"} {
			for _, prefix := range prefixes {
				t.Run(fmt.Sprintf("inferred_%v/%s/%s", inferred, failure, prefix.name), func(t *testing.T) {
					var calls atomic.Int32
					target := int32(1)
					if inferred {
						target = 2
					}
					prefixSent := make(chan struct{})
					releaseFailure := make(chan struct{})
					var once sync.Once
					unblock := func() { once.Do(func() { close(releaseFailure) }) }
					defer unblock()
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
						if err != nil {
							return
						}
						defer conn.Close()
						for {
							if _, _, err := conn.ReadMessage(); err != nil {
								return
							}
							n := calls.Add(1)
							if n < target {
								_ = conn.WriteMessage(websocket.TextMessage, autoContinuationEvent("resp_seed", autoContinuationCall))
								continue
							}
							if n > target {
								_ = conn.WriteMessage(websocket.TextMessage, autoContinuationEvent("resp_replayed", autoContinuationAnswer))
								continue
							}
							_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_partial","model":"gpt-6-astra"}}`))
							_ = conn.WriteMessage(websocket.TextMessage, []byte(prefix.event))
							_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.in_progress","response":{"id":"resp_partial","model":"gpt-6-astra","usage":{"input_tokens":11,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}}`))
							close(prefixSent)
							<-releaseFailure
							if failure == "capacity_error" {
								_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"Our servers are currently overloaded. Please try again later."}}`))
							}
							return
						}
					}))
					defer server.Close()
					svc, account, httpUpstream := httpBridgeFixture(t, server.URL)
					defer svc.openaiWSPool.Close()
					ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
					defer cancel()
					body := autoContinuationBody(false)
					if inferred {
						seed, _ := httpBridgeContext("nonstream-boundary")
						_, err := svc.Forward(ctx, seed, account, body)
						require.NoError(t, err)
						body = autoContinuationBody(false, autoContinuationCall, autoContinuationTool)
					}
					rawRecorder := httptest.NewRecorder()
					writer := &lockedTestResponseWriter{ResponseWriter: rawRecorder}
					c, _ := gin.CreateTestContext(writer)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					c.Request.Header.Set("session-id", "nonstream-boundary")
					c.Set("api_key", &APIKey{ID: 123})
					SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
					type outcome struct {
						result *OpenAIForwardResult
						err    error
					}
					done := make(chan outcome, 1)
					go func() { result, err := svc.Forward(ctx, c, account, body); done <- outcome{result, err} }()
					select {
					case <-prefixSent:
					case <-ctx.Done():
						t.Fatal("synthetic upstream did not produce prefix")
					}
					require.Empty(t, writer.snapshotBody(), "client delivery has not started even though upstream produced semantic output")
					unblock()
					var out outcome
					select {
					case out = <-done:
					case <-ctx.Done():
						t.Fatal("forward did not finish")
					}
					assert.Equal(t, target, calls.Load(), "the already-started inference must not be resubmitted")
					var failover *UpstreamFailoverError
					assert.False(t, errors.As(out.err, &failover), "an account-failover sentinel would authorize a duplicate inference: %v", out.err)
					assert.Error(t, out.err)
					assert.Equal(t, 0, httpUpstream.callCount, "HTTP fallback would also replay the turn")
					require.NotNil(t, out.result, "known usage and response attribution must survive the failure")
					assert.Equal(t, "resp_partial", out.result.RequestID)
					assert.Equal(t, "resp_partial", out.result.ResponseID)
					assert.Equal(t, "gpt-6-astra", out.result.UpstreamModel)
					assert.Equal(t, 11, out.result.Usage.InputTokens)
					assert.Equal(t, 3, out.result.Usage.OutputTokens)
					assert.Equal(t, 2, out.result.Usage.CacheReadInputTokens)
					if prefix.name == "image_done" {
						assert.Equal(t, 1, out.result.ImageCount)
					}
					assert.False(t, out.result.ClientDisconnect, "the downstream client is still connected")
					assert.False(t, out.result.SucceededForScheduling(), "partial output is not successful completion")
					assert.NotContains(t, writer.snapshotBody(), "resp_replayed")
					assert.False(t, c.GetBool("openai_ws_auto_incremental_succeeded"))
				})
			}
		}
	}
}

func TestHTTPToWSNonstreamWithoutUpstreamProgressRetainsRecovery(t *testing.T) {
	for _, failure := range []string{"disconnect", "capacity_error"} {
		t.Run(failure, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, _, err = conn.ReadMessage(); err != nil {
					return
				}
				n := calls.Add(1)
				if n > 1 {
					_ = conn.WriteMessage(websocket.TextMessage, autoContinuationEvent("resp_safe_retry", autoContinuationAnswer))
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_preamble","model":"gpt-6-astra"}}`))
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":""}`))
				if failure == "capacity_error" {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"Our servers are currently overloaded. Please try again later."}}`))
				}
			}))
			defer server.Close()
			svc, account, upstream := httpBridgeFixture(t, server.URL)
			defer svc.openaiWSPool.Close()
			c, recorder := httpBridgeContext("no-output-boundary")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := svc.Forward(ctx, c, account, autoContinuationBody(false))
			if failure == "disconnect" {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, int32(2), calls.Load())
				require.Equal(t, "resp_safe_retry", result.ResponseID)
			} else {
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.Equal(t, http.StatusServiceUnavailable, failover.StatusCode)
				require.Equal(t, int32(1), calls.Load())
				require.Nil(t, result)
				require.Empty(t, recorder.Body.String())
			}
			require.Equal(t, 0, upstream.callCount)
		})
	}
}
