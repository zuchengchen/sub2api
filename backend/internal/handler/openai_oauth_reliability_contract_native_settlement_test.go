//go:build unit

package handler

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// The success classifier has a public, meaningful side effect: success clears
// the existing consecutive-429 counter, while a failed metered turn must not.
// Prime 29 soft rejections only after the account has actually been selected;
// one final rejection then reaches formal cooldown iff no success was recorded.
type oauthReliabilityNativeSettlementRateRepo struct {
	service.AccountRepository
	formal atomic.Int32
}

func (r *oauthReliabilityNativeSettlementRateRepo) UpdateExtra(context.Context, int64, map[string]any) error {
	return nil
}
func (r *oauthReliabilityNativeSettlementRateRepo) BulkUpdate(_ context.Context, ids []int64, _ service.AccountBulkUpdate) (int64, error) {
	return int64(len(ids)), nil
}
func (r *oauthReliabilityNativeSettlementRateRepo) SetRateLimited(context.Context, int64, time.Time) error {
	r.formal.Add(1)
	return nil
}

func TestOAuthReliabilityContractNativeWSHandlerSettlesMeteredFailureOnce(t *testing.T) {
	for _, completed := range []bool{false, true} {
		name := "disconnect"
		if completed {
			name = "completed_control"
		}
		t.Run(name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			cert := wsPartialSettlementTLS(t)
			groupID := int64(9710)
			group := &service.Group{ID: groupID, Hydrated: true, Name: "synthetic-native-settlement", Platform: service.PlatformOpenAI, Status: service.StatusActive, RateMultiplier: 1}
			var frames atomic.Int32
			var accountIDsMu sync.Mutex
			var selectedTokens []string
			accounts := make([]service.Account, 2)
			rateRepo := &oauthReliabilityNativeSettlementRateRepo{}
			rateService := service.NewRateLimitService(rateRepo, nil, nil, nil)
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/backend-api/codex/responses" {
					http.Error(w, "unexpected endpoint", 404)
					return
				}
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_, body, err := conn.ReadMessage()
				if err != nil {
					return
				}
				if gjson.GetBytes(body, "type").String() != "response.create" {
					t.Errorf("expected response.create on upstream wire")
					return
				}
				frames.Add(1)
				accountIDsMu.Lock()
				selectedTokens = append(selectedTokens, r.Header.Get("Authorization"))
				accountIDsMu.Unlock()
				for i := 0; i < 29; i++ {
					rateService.HandleUpstreamError(context.Background(), &accounts[0], http.StatusTooManyRequests, nil, nil)
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.in_progress","response":{"id":"resp_native_partial","model":"gpt-6-astra","usage":{"input_tokens":11,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}}`))
				if completed {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_native_partial","model":"gpt-6-astra","status":"completed","output":[],"usage":{"input_tokens":11,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}}`))
				}
			}))
			upstream.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}
			upstream.StartTLS()
			t.Cleanup(upstream.Close)
			proxy, connects := wsPartialSettlementProxy(t, upstream.Listener.Addr().String())
			for i := range accounts {
				accounts[i] = service.Account{ID: 9711 + int64(i), Name: fmt.Sprintf("native-synthetic-%d", i), Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 2, Priority: 1 + i*100, ProxyID: &proxy.ID, Proxy: proxy,
					Credentials: map[string]any{"access_token": fmt.Sprintf("native-synthetic-token-%d", i), "chatgpt_account_id": fmt.Sprintf("native-synthetic-account-%d", i)},
					Extra: map[string]any{"responses_websockets_v2_enabled": true, "openai_oauth_responses_websockets_v2_mode": service.OpenAIWSIngressModeCtxPool,
						service.OpenAIAstraPolicyExtraKey: map[string]any{"model": "gpt-6-astra", "recent_samples": []any{map[string]any{"verdict": "healthy", "at": "2020-01-01T00:00:00Z"}}}},
					AccountGroups: []service.AccountGroup{{AccountID: 9711 + int64(i), GroupID: groupID}}}
				require.True(t, accounts[i].AllowsOpenAIOAuthModelPolicy("gpt-6-astra"))
				service.ResetOpenAI429Counter(accounts[i].ID)
				accountID := accounts[i].ID
				t.Cleanup(func() { service.ResetOpenAI429Counter(accountID) })
			}
			accountRepo := &openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}
			usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 8)}
			billingRepo := &compatStreamUsageBillingRepo{}
			tripwire := &wsPartialSettlementHTTPTripwire{}
			cfg := &config.Config{}
			disabled := false
			cfg.Gateway.OpenAITiboRoute.Enabled = &disabled
			cfg.Default.RateMultiplier = 1
			cfg.Gateway.MaxAccountSwitches = 2
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			cfg.Gateway.OpenAIWS.Enabled = true
			cfg.Gateway.OpenAIWS.OAuthEnabled = true
			cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			cfg.Gateway.OpenAIWS.ExactFidelityEnabled = true
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
			cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
			cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
			billingCache := service.NewBillingCacheService(newHandlerInflightCache(100), nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billingCache.Stop)
			svc := service.NewOpenAIGatewayService(accountRepo, usageRepo, billingRepo, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, nil, tripwire, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
			svc.StopOpenAICodexTicketHarvester()
			t.Cleanup(svc.CloseOpenAIWSPool)
			h := NewOpenAIGatewayHandler(svc, service.NewConcurrencyService(&fakeConcurrencyCache{}), billingCache, &service.APIKeyService{}, nil, nil, nil, nil, cfg)
			apiKey := &service.APIKey{ID: 9713, UserID: 9714, Key: "synthetic-native-client", GroupID: &groupID, Group: group, User: &service.User{ID: 9714, Status: service.StatusActive, Role: service.RoleUser, Balance: 100}}
			handlerDone := make(chan struct{})
			logCore, observedLogs := observer.New(zap.DebugLevel)
			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(logCore)))
				c.Set(string(middleware.ContextKeyAPIKey), apiKey)
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 9714, Concurrency: 2})
				c.Next()
			})
			router.GET("/v1/responses", func(c *gin.Context) { h.ResponsesWebSocket(c); close(handlerDone) })
			server := httptest.NewServer(router)
			t.Cleanup(server.Close)
			client, _, err := (&websocket.Dialer{HandshakeTimeout: 5 * time.Second}).Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close() })
			require.NoError(t, client.SetReadDeadline(time.Now().Add(8*time.Second)))
			require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra","stream":true,"input":[{"type":"message","role":"user","content":"synthetic native settlement request"}],"reasoning":{"effort":"high"}}`)))
			var seen []string
			for {
				_, event, readErr := client.ReadMessage()
				if readErr != nil {
					break
				}
				seen = append(seen, string(event))
				if gjson.GetBytes(event, "type").String() == "response.completed" {
					_ = client.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"))
					_ = client.Close()
					break
				}
			}
			select {
			case <-handlerDone:
			case <-time.After(3 * time.Second):
				t.Fatal("native WS handler did not return")
			}
			require.Equal(t, int32(1), frames.Load(), "one upstream inference only; logs=%v", observedLogs.AllUntimed())
			require.Equal(t, int32(1), connects.Load())
			require.Zero(t, tripwire.calls.Load(), "native partial must not fall back to HTTP")
			accountIDsMu.Lock()
			tokens := append([]string(nil), selectedTokens...)
			accountIDsMu.Unlock()
			require.Equal(t, []string{"Bearer native-synthetic-token-0"}, tokens)
			require.Contains(t, strings.Join(seen, "\n"), "response.in_progress")
			if !completed {
				require.NotContains(t, strings.Join(seen, "\n"), "response.completed")
			}
			var usage *service.UsageLog
			select {
			case usage = <-usageRepo.created:
			case <-time.After(3 * time.Second):
				t.Fatalf("metered turn did not settle; logs=%v", observedLogs.AllUntimed())
			}
			require.Empty(t, usageRepo.created, "one WS turn must write one usage row")
			billingRepo.mu.Lock()
			commands := append([]*service.UsageBillingCommand(nil), billingRepo.commands...)
			billingRepo.mu.Unlock()
			require.Len(t, commands, 1, "one WS turn must create one billing command")
			require.Equal(t, "resp_native_partial", usage.RequestID)
			require.Equal(t, int64(9711), usage.AccountID)
			require.Equal(t, 11, usage.InputTokens+usage.CacheReadTokens)
			require.Equal(t, 3, usage.OutputTokens)
			require.Equal(t, 2, usage.CacheReadTokens)
			require.True(t, usage.OpenAIWSMode)
			require.True(t, usage.Stream)
			require.Equal(t, usage.RequestID, commands[0].RequestID)
			require.Equal(t, usage.InputTokens, commands[0].InputTokens)
			require.Equal(t, usage.OutputTokens, commands[0].OutputTokens)
			require.Greater(t, commands[0].BalanceCost, 0.0)
			quotaHeaders := http.Header{"X-Codex-Secondary-Used-Percent": []string{"100"}, "X-Codex-Secondary-Reset-After-Seconds": []string{"18000"}, "X-Codex-Secondary-Window-Minutes": []string{"300"}}
			rateService.HandleUpstreamError(context.Background(), &accounts[0], http.StatusTooManyRequests, quotaHeaders, nil)
			wantFormal := int32(1)
			if completed {
				wantFormal = 0
			}
			require.Equal(t, wantFormal, rateRepo.formal.Load(), "failed metered turn must not be classified as success and clear recovery counters; success control must clear them")
		})
	}
}
