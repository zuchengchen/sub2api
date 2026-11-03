package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Exercise a real local upstream WebSocket, the service forwarding path, the
// handler's terminal fallback and OpsErrorLogger together. A capacity error
// after real output cannot be replayed, but must retain its retryable terminal
// code and semantic 503 rather than being replaced with a generic 502.
func TestOpenAIWSCapacityAfterOutputPreservesTerminalAndOps(t *testing.T) {
	for _, tc := range []struct {
		name, payload, wantCode string
		wantStatus              int
	}{
		{
			name:     "capacity_server_overloaded",
			payload:  `{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`,
			wantCode: "server_error", wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:     "capacity_slow_down",
			payload:  `{"type":"error","error":{"type":"service_unavailable_error","code":"slow_down","message":"synthetic capacity rejection"}}`,
			wantCode: "server_error", wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:     "ordinary_server_error_unchanged",
			payload:  `{"type":"error","error":{"type":"server_error","code":"server_error","message":"synthetic internal provider error"}}`,
			wantCode: "upstream_error", wantStatus: http.StatusBadGateway,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupOpsErrorLogTestQueue(t, 4)
			gin.SetMode(gin.TestMode)
			var dials atomic.Int32
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				dials.Add(1)
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade: %v", err)
					return
				}
				defer conn.Close()
				if _, _, err = conn.ReadMessage(); err != nil {
					t.Errorf("read request: %v", err)
					return
				}
				for _, frame := range []string{
					`{"type":"response.created","response":{"id":"resp_synthetic","model":"gpt-5.1","status":"in_progress"}}`,
					`{"type":"response.output_text.delta","delta":"synthetic already emitted text"}`,
					tc.payload,
				} {
					if err = conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
						t.Errorf("write event: %v", err)
						return
					}
				}
			}))
			defer upstream.Close()

			cfg := &config.Config{}
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			cfg.Gateway.OpenAIWS.Enabled = true
			cfg.Gateway.OpenAIWS.APIKeyEnabled = true
			cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			cfg.Gateway.LogUpstreamErrorBody = true
			cfg.Gateway.LogUpstreamErrorBodyMaxBytes = 2048
			svc := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			defer svc.CloseOpenAIWSPool()
			account := &service.Account{
				ID: 9082, Name: "synthetic", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
				Status: service.StatusActive, Schedulable: true, Concurrency: 1,
				Credentials: map[string]any{"api_key": "synthetic-key", "base_url": upstream.URL},
				Extra:       map[string]any{"responses_websockets_v2_enabled": true},
			}
			ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			router.Use(OpsErrorLoggerMiddleware(ops))
			router.POST("/v1/responses", func(c *gin.Context) {
				setOpsRequestContext(c, "gpt-5.1", true)
				c.Set(opsAccountIDKey, account.ID)
				service.MarkOpenAIResponsesClientStream(c, true)
				before := c.Writer.Size()
				_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.1","stream":true,"input":"synthetic"}`))
				require.Error(t, err)
				var failover *service.UpstreamFailoverError
				require.False(t, errors.As(err, &failover), "must not replay after semantic output")
				if !openAIForwardErrorAlreadyCommunicated(c, before, err) {
					(&OpenAIGatewayHandler{}).ensureForwardErrorResponse(c, false)
				}
			})
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
			request.Header.Set("User-Agent", "synthetic-client/1.0")
			router.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusOK, recorder.Code, "wire status remains committed")
			require.EqualValues(t, 1, dials.Load(), "never reconnect/replay committed output")
			assert.Equal(t, 1, strings.Count(recorder.Body.String(), "synthetic already emitted text"))
			assert.Equal(t, 1, strings.Count(recorder.Body.String(), `"type":"response.failed"`), "one protocol terminal")
			parsed := parseOpsErrorResponse(recorder.Body.Bytes())
			assert.True(t, parsed.StreamFailure)
			assert.Equal(t, tc.wantCode, parsed.Code, "terminal must preserve capacity retry semantics")
			assert.Equal(t, tc.wantStatus, inferStreamFailureStatus(nil, parsed))
			require.Equal(t, int64(1), OpsErrorLogQueueLength())
			job := <-opsErrorLogQueue
			assert.Equal(t, tc.wantStatus, job.entry.StatusCode, "persist semantic outcome")
			require.NotNil(t, job.entry.UpstreamStatusCode)
			assert.Equal(t, tc.wantStatus, *job.entry.UpstreamStatusCode, "display must not be overridden by stale 502")
			assert.Equal(t, "upstream", job.entry.ErrorPhase)
		})
	}
}
