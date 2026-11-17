//go:build unit

package handler

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const oauthWSReaderImage = `{"type":"image_generation_call","id":"ig_reader","status":"completed","result":"c3ludGhldGljLWltYWdl","size":"1024x1024"}`
const oauthWSReaderUsage = `{"input_tokens":11,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}`

// Cancellation/write failure is triggered by actual downstream image delivery,
// after the preceding usage frame has been read. No sleep races the WS reader.
type oauthWSReaderBoundaryWriter struct {
	*httptest.ResponseRecorder
	cancel             context.CancelFunc
	failImage          bool
	imageAttempted     bool
	failTerminal       bool
	terminalAttempts   int
	partialTerminal    []byte
	terminalFrameBytes int
}

// A deterministic deadline signal is armed only after actual image delivery.
// It retains context cancellation propagation while exposing the distinct
// DeadlineExceeded state; no wall-clock sleep races TLS setup or the reader.
type oauthWSReaderDeadlineContext struct{ context.Context }

func (c oauthWSReaderDeadlineContext) Err() error {
	if context.Cause(c.Context) == context.DeadlineExceeded {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

func (w *oauthWSReaderBoundaryWriter) Write(p []byte) (int, error) {
	if w.failTerminal && (strings.Contains(string(p), `"type":"response.failed"`) || strings.Contains(string(p), `"type":"response.incomplete"`)) {
		w.terminalAttempts++
		w.terminalFrameBytes = len(p)
		w.partialTerminal = append([]byte(nil), p[:len(p)/2]...)
		n, _ := w.ResponseRecorder.Write(w.partialTerminal)
		return n, io.ErrClosedPipe // A legal partial write: n < len(p), err != nil.
	}
	if strings.Contains(string(p), `"type":"image_generation_call"`) {
		w.imageAttempted = true
		if w.failImage {
			return 0, io.ErrClosedPipe
		}
		n, err := w.ResponseRecorder.Write(p)
		if w.cancel != nil {
			w.cancel()
		}
		return n, err
	}
	return w.ResponseRecorder.Write(p)
}

// This executes the public Responses handler, real OAuth WS request builder,
// local TLS connection, account proxy, scheduler, Ops queue and billing path.
// It is not a reproduction of a customer's payload. In particular, the observed
// production EOF does not establish that its response contained an image.
func TestOAuthReliabilityContractWSHTTPReaderTerminationPublic(t *testing.T) {
	cases := []struct {
		name, output, ending string
		stream               bool
	}{
		{"stream_image_eof", "image", "eof", true},
		{"stream_text_eof", "text", "eof", true},
		{"stream_tool_eof", "tool", "eof", true},
		{"stream_usage_only_eof", "", "eof", true},
		{"buffered_image_eof", "image", "eof", false},
		{"buffered_text_eof", "text", "eof", false},
		{"buffered_tool_eof", "tool", "eof", false},
		{"stream_image_graceful", "image", "graceful", true},
		{"stream_text_graceful", "text", "graceful", true},
		{"stream_image_completed", "image", "completed", true},
		{"stream_image_failed", "image", "failed", true},
		{"stream_image_cancel", "image", "cancel", true},
		{"stream_image_write_failure", "image", "write_failure", true},
		{"stream_image_terminal_short_write", "image", "terminal_short_write", true},
		{"stream_image_deadline", "image", "deadline", true},
		{"stream_no_progress_retry", "", "retry", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			setupOpsErrorLogTestQueue(t, 8)
			cert := wsPartialSettlementTLS(t)
			var calls atomic.Int32
			sent := make(chan []byte, 8)
			owners := make(chan string, 8)
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/backend-api/codex/responses" {
					http.Error(w, "unexpected synthetic endpoint", 404)
					return
				}
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_, request, err := conn.ReadMessage()
				if err != nil {
					return
				}
				owners <- r.Header.Get("Authorization")
				sent <- append([]byte(nil), request...)
				n := calls.Add(1)
				write := func(frame string) bool { return conn.WriteMessage(websocket.TextMessage, []byte(frame)) == nil }
				if n > 1 {
					write(`{"type":"response.completed","response":{"id":"resp_reader_recovered","object":"response","model":"gpt-6-astra","status":"completed","output":[],"usage":` + oauthWSReaderUsage + `}}`)
					return
				}
				if !write(`{"type":"response.created","response":{"id":"resp_reader","object":"response","model":"gpt-6-astra","status":"in_progress"}}`) {
					return
				}
				if tc.ending == "retry" {
					write(`{"type":"response.output_text.delta","delta":""}`)
					return
				}
				if !write(`{"type":"response.in_progress","response":{"id":"resp_reader","usage":` + oauthWSReaderUsage + `}}`) {
					return
				}
				switch tc.output {
				case "image":
					if !write(`{"type":"response.output_item.done","output_index":0,"item":` + oauthWSReaderImage + `}`) {
						return
					}
				case "text":
					if !write(`{"type":"response.output_text.delta","item_id":"msg_reader","output_index":0,"delta":"synthetic reader text"}`) {
						return
					}
				case "tool":
					if !write(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_reader","call_id":"call_reader","name":"lookup","arguments":"{}"}}`) {
						return
					}
				}
				switch tc.ending {
				case "graceful":
					_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "synthetic restart"), time.Now().Add(time.Second))
				case "completed":
					write(`{"type":"response.completed","response":{"id":"resp_reader","object":"response","model":"gpt-6-astra","status":"completed","output":[` + oauthWSReaderImage + `],"usage":` + oauthWSReaderUsage + `}}`)
				case "failed":
					write(`{"type":"response.failed","response":{"id":"resp_reader","object":"response","model":"gpt-6-astra","status":"failed","output":[` + oauthWSReaderImage + `],"error":{"type":"server_error","code":"server_error","message":"synthetic authoritative failure"},"usage":` + oauthWSReaderUsage + `}}`)
				}
				// Returning closes the socket without a WS Close frame for EOF,
				// cancellation and downstream write-error cases. Nothing is replayed
				// or delayed to manufacture a terminal/usage frame.
			}))
			upstream.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}
			upstream.StartTLS()
			t.Cleanup(upstream.Close)
			proxy, connects := wsPartialSettlementProxy(t, upstream.Listener.Addr().String())
			groupID := int64(9930)
			imagePrice := 0.25
			group := &service.Group{ID: groupID, Hydrated: true, Name: "synthetic-reader", Platform: service.PlatformOpenAI, Status: service.StatusActive, RateMultiplier: 1, AllowImageGeneration: true, ImagePrice1K: &imagePrice}
			accounts := make([]service.Account, 2)
			for i := range accounts {
				accounts[i] = service.Account{ID: 9931 + int64(i), Name: fmt.Sprintf("reader-synthetic-%d", i), Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 2, Priority: 1 + i*100, ProxyID: &proxy.ID, Proxy: proxy,
					Credentials:   map[string]any{"access_token": fmt.Sprintf("reader-synthetic-token-%d", i), "chatgpt_account_id": fmt.Sprintf("reader-synthetic-account-%d", i)},
					Extra:         map[string]any{"responses_websockets_v2_enabled": true, service.OpenAIAstraPolicyExtraKey: map[string]any{"model": "gpt-6-astra", "recent_samples": []any{map[string]any{"verdict": "healthy", "at": "2020-01-01T00:00:00Z"}}}},
					AccountGroups: []service.AccountGroup{{AccountID: 9931 + int64(i), GroupID: groupID}}}
				require.True(t, accounts[i].AllowsOpenAIOAuthModelPolicy("gpt-6-astra"))
			}
			repo := &openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}
			usage := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 8)}
			billing := &compatStreamUsageBillingRepo{}
			tripwire := &wsPartialSettlementHTTPTripwire{}
			cfg := &config.Config{}
			disabled := false
			cfg.Gateway.OpenAITiboRoute.Enabled = &disabled
			cfg.Default.RateMultiplier = 1
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			cfg.Gateway.MaxAccountSwitches = 2
			cfg.Gateway.OpenAIWS.Enabled = true
			cfg.Gateway.OpenAIWS.OAuthEnabled = true
			cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			cfg.Gateway.OpenAIWS.ExactFidelityEnabled = true
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
			cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
			cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
			cache := service.NewBillingCacheService(newHandlerInflightCache(100), nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(cache.Stop)
			svc := service.NewOpenAIGatewayService(repo, usage, billing, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, nil, tripwire, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
			svc.StopOpenAICodexTicketHarvester()
			t.Cleanup(svc.CloseOpenAIWSPool)
			handler := NewOpenAIGatewayHandler(svc, service.NewConcurrencyService(&fakeConcurrencyCache{}), cache, &service.APIKeyService{}, nil, nil, nil, nil, cfg)
			key := &service.APIKey{ID: 9933, UserID: 9934, Key: "synthetic-reader-client", GroupID: &groupID, Group: group, User: &service.User{ID: 9934, Status: service.StatusActive, Role: service.RoleUser, Balance: 100}}
			ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil)
			core, _ := observer.New(zap.ErrorLevel)
			router := gin.New()
			router.Use(OpsErrorLoggerMiddleware(ops))
			router.Use(func(c *gin.Context) {
				c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
				c.Set(string(middleware.ContextKeyAPIKey), key)
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 9934, Concurrency: 2})
				c.Header("X-Request-Id", "reader-boundary-request")
				c.Next()
			})
			var committed bool
			var sourceStreamErrors []service.OpsStreamError
			router.POST("/v1/responses", func(c *gin.Context) {
				handler.Responses(c)
				committed = service.IsResponseCommitted(c)
				sourceStreamErrors = service.GetOpsStreamErrors(c)
			})
			body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":[{"type":"message","role":"user","content":"synthetic reader request"}],"reasoning":{"effort":"high"}}`, tc.stream)
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
			defer cancel()
			var expireDeadline func()
			if tc.ending == "deadline" {
				deadlineSignal, finish := context.WithCancelCause(ctx)
				defer finish(nil)
				ctx = oauthWSReaderDeadlineContext{deadlineSignal}
				expireDeadline = func() { finish(context.DeadlineExceeded) }
			}
			request = request.WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("User-Agent", "synthetic-reader-fixture")
			request.Header.Set("session-id", "synthetic-reader-session")
			recorder := httptest.NewRecorder()
			writer := &oauthWSReaderBoundaryWriter{ResponseRecorder: recorder, failImage: tc.ending == "write_failure", failTerminal: tc.ending == "terminal_short_write"}
			if tc.ending == "cancel" {
				writer.cancel = cancel
			} else if tc.ending == "deadline" {
				writer.cancel = expireDeadline
			}
			router.ServeHTTP(writer, request)

			wantCalls := int32(1)
			if tc.ending == "retry" {
				wantCalls = 2
			}
			assert.Equal(t, wantCalls, calls.Load(), "a started inference must not be replayed")
			assert.Equal(t, wantCalls, connects.Load(), "one fresh transport for each allowed logical attempt")
			assert.Zero(t, tripwire.calls.Load(), "HTTP fallback also constitutes a replay")
			require.Len(t, sent, int(wantCalls))
			for len(sent) > 0 {
				frame := <-sent
				assert.Equal(t, "response.create", gjson.GetBytes(frame, "type").String())
				assert.Equal(t, "gpt-6-astra", gjson.GetBytes(frame, "model").String())
				assert.False(t, gjson.GetBytes(frame, "generate").Exists(), "exercise the real inference, not generate=false prewarm")
			}
			require.Len(t, owners, int(wantCalls))
			for len(owners) > 0 {
				assert.Equal(t, "Bearer reader-synthetic-token-0", <-owners, "no account switch")
			}

			// Settlement is synchronous on this handler fixture. Assert it before
			// the missing-terminal oracle so a red image case still verifies usage.
			require.Len(t, usage.created, 1, "already observed usage must be recorded once")
			row := <-usage.created
			billing.mu.Lock()
			commands := append([]*service.UsageBillingCommand(nil), billing.commands...)
			billing.mu.Unlock()
			require.Len(t, commands, 1, "one and only one billing command")
			assert.Equal(t, int64(9931), row.AccountID)
			assert.Equal(t, 11, row.InputTokens+row.CacheReadTokens)
			assert.Equal(t, 3, row.OutputTokens)
			assert.Equal(t, 2, row.CacheReadTokens)
			assert.Equal(t, tc.stream, row.Stream)
			assert.True(t, row.OpenAIWSMode)
			assert.Equal(t, row.RequestID, commands[0].RequestID)
			assert.Equal(t, row.AccountID, commands[0].AccountID)
			assert.Equal(t, row.InputTokens, commands[0].InputTokens)
			assert.Equal(t, row.OutputTokens, commands[0].OutputTokens)
			wantImages := 0
			if tc.output == "image" {
				wantImages = 1
			}
			assert.Equal(t, wantImages, row.ImageCount)
			assert.Equal(t, wantImages, commands[0].ImageCount)
			assert.Positive(t, commands[0].BalanceCost)
			assert.Empty(t, usage.created)

			wire := recorder.Body.String()
			var terminals []gjson.Result
			imageItems := 0
			for _, line := range strings.Split(wire, "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				raw := strings.TrimPrefix(line, "data: ")
				if !json.Valid([]byte(raw)) && tc.ending == "terminal_short_write" {
					// Only this exact final, partially written transport frame is
					// expected to be incomplete. The Ops capture is not user wire.
					assert.True(t, strings.HasSuffix(wire, string(writer.partialTerminal)))
					continue
				}
				require.True(t, json.Valid([]byte(raw)), "valid complete SSE data frame")
				event := gjson.Parse(raw)
				switch event.Get("type").String() {
				case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
					terminals = append(terminals, event)
				case "response.output_item.done":
					if event.Get("item.type").String() == "image_generation_call" {
						imageItems++
					}
				}
			}
			if tc.ending == "cancel" || tc.ending == "write_failure" || tc.ending == "deadline" || tc.ending == "terminal_short_write" {
				require.True(t, writer.imageAttempted, "cancellation must happen after real semantic progress")
				assert.Empty(t, terminals, "a disconnected client did not receive a complete synthetic terminal")
				assert.False(t, committed, "failed/dropped terminal writes are not committed error responses")
				assert.Empty(t, sourceStreamErrors, "the forwarding owner must not mark an undelivered terminal successful")
				if tc.ending == "deadline" {
					assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
				}
				if tc.ending == "terminal_short_write" {
					assert.Equal(t, 1, writer.terminalAttempts, "exactly one terminal write attempt, no suffix retry")
					assert.NotEmpty(t, writer.partialTerminal)
					assert.Less(t, len(writer.partialTerminal), writer.terminalFrameBytes)
					assert.True(t, strings.HasSuffix(wire, string(writer.partialTerminal)), "only the returned partial bytes reached the client writer")
				}
			} else if !tc.stream || (tc.output == "" && tc.ending == "eof") {
				assert.Equal(t, http.StatusBadGateway, recorder.Code)
				assert.True(t, json.Valid([]byte(wire)), "no SSE suffix after a JSON error")
				assert.Equal(t, "upstream_error", gjson.Get(wire, "error.type").String())
				assert.NotEmpty(t, gjson.Get(wire, "error.message").String())
				assert.Empty(t, terminals)
			} else {
				assert.Equal(t, http.StatusOK, recorder.Code, "SSE status was already committed")
				assert.Len(t, terminals, 1, "exactly one Responses terminal after upstream output; wire=%s", wire)
				if len(terminals) == 1 {
					terminal := terminals[0]
					if tc.output == "image" {
						assert.Equal(t, "resp_reader", terminal.Get("response.id").String(), "the terminal belongs to the observed response")
					}
					if tc.ending == "completed" || tc.ending == "failed" {
						assert.Equal(t, "gpt-6-astra", terminal.Get("response.model").String())
						assert.Equal(t, 11, int(terminal.Get("response.usage.input_tokens").Int()))
						assert.Equal(t, 3, int(terminal.Get("response.usage.output_tokens").Int()))
					}
					if tc.ending == "failed" {
						assert.Equal(t, "server_error", terminal.Get("response.error.code").String())
						assert.Equal(t, "synthetic authoritative failure", terminal.Get("response.error.message").String())
					}
					switch tc.ending {
					case "completed", "retry":
						assert.Equal(t, "response.completed", terminal.Get("type").String())
					case "graceful":
						assert.Equal(t, "response.incomplete", terminal.Get("type").String())
						assert.Equal(t, "server_error", terminal.Get("response.incomplete_details.reason").String())
					default:
						assert.Contains(t, []string{"response.failed", "response.incomplete"}, terminal.Get("type").String())
						if terminal.Get("type").String() == "response.failed" {
							assert.Equal(t, "failed", terminal.Get("response.status").String())
							assert.NotEmpty(t, terminal.Get("response.error.message").String())
						} else {
							assert.Equal(t, "server_error", terminal.Get("response.incomplete_details.reason").String())
						}
					}
				}
			}
			if tc.stream && tc.output == "image" && tc.ending != "write_failure" {
				assert.Equal(t, 1, imageItems, "already delivered image result is not lost or duplicated")
			}
			if tc.ending == "eof" || tc.ending == "failed" {
				assert.Equal(t, int64(1), OpsErrorLogQueueLength(), "a terminated request is not a recovered 200")
				if len(opsErrorLogQueue) == 1 {
					job := <-opsErrorLogQueue
					wantStatus := http.StatusBadGateway
					if tc.ending == "failed" {
						// The existing terminal-scoped Ops classifier maps the
						// authoritative server_error code to 503, not transport EOF.
						wantStatus = http.StatusServiceUnavailable
					}
					assert.Equal(t, wantStatus, job.entry.StatusCode)
					assert.NotContains(t, job.entry.ErrorMessage, "Recovered upstream error")
				}
			} else {
				// OpsCaptureWriter snapshots attempted bytes before delegating
				// Write. Its recorded terminal is not proof of delivery when
				// the delegate short-writes; assert delivery above instead.
				for len(opsErrorLogQueue) > 0 {
					job := <-opsErrorLogQueue
					if tc.ending == "cancel" || tc.ending == "write_failure" {
						assert.NotEqual(t, http.StatusBadGateway, job.entry.StatusCode, "client departure is not a new upstream failure")
					}
				}
			}
		})
	}
}
