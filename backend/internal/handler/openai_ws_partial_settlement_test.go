//go:build unit

package handler

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// This transport is only a tripwire. Responses HTTP fallback must not silently
// hide a repeated inference after the real upstream WS produced partial output.
type wsPartialSettlementHTTPTripwire struct{ calls atomic.Int32 }

func (t *wsPartialSettlementHTTPTripwire) Do(*http.Request, string, int64, int) (*http.Response, error) {
	t.calls.Add(1)
	return nil, errors.New("unexpected HTTP fallback in partial-settlement fixture")
}
func (t *wsPartialSettlementHTTPTripwire) DoWithTLS(r *http.Request, p string, a int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return t.Do(r, p, a, n)
}

func wsPartialSettlementTLS(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic settlement loopback"}, DNSNames: []string{"chatgpt.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	cert, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	require.NoError(t, err)
	caPath := filepath.Join(t.TempDir(), "settlement-ca.pem")
	require.NoError(t, os.WriteFile(caPath, certPEM, 0600))
	t.Setenv("CODEX_CA_CERTIFICATE", caPath)
	t.Setenv("SSL_CERT_FILE", "")
	return cert
}

// The production OAuth URL is unchanged. The explicit account proxy accepts
// only that CONNECT target and routes it to the test's loopback TLS server.
func wsPartialSettlementProxy(t *testing.T, target string) (*service.Proxy, *atomic.Int32) {
	t.Helper()
	connects := &atomic.Int32{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "chatgpt.com:443" {
			http.Error(w, "unexpected synthetic proxy destination", http.StatusForbidden)
			return
		}
		connects.Add(1)
		remote, err := net.DialTimeout("tcp", target, 2*time.Second)
		if err != nil {
			http.Error(w, "loopback unavailable", 502)
			return
		}
		client, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = remote.Close()
			return
		}
		defer client.Close()
		defer remote.Close()
		if _, err = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		done := make(chan struct{})
		go func() { _, _ = io.Copy(remote, buffer); _ = remote.Close(); close(done) }()
		_, _ = io.Copy(client, remote)
		_ = client.Close()
		<-done
	}))
	t.Cleanup(proxy.Close)
	u, err := url.Parse(proxy.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	return &service.Proxy{ID: 991, Protocol: "http", Host: u.Hostname(), Port: port, Status: service.StatusActive}, connects
}

func TestOpenAIResponsesNonstreamWSPartialSettlementOnce(t *testing.T) {
	cases := []struct {
		name, prefix, failure     string
		image, cyber, recoverable bool
	}{
		{name: "text_then_disconnect", prefix: `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_partial","role":"assistant","content":[{"type":"output_text","text":"synthetic partial"}]}}`, failure: "disconnect"},
		{name: "image_then_disconnect", prefix: `{"type":"response.output_item.done","output_index":0,"item":{"type":"image_generation_call","id":"ig_partial","status":"completed","result":"c3ludGhldGljLWltYWdl","size":"1024x1024"}}`, failure: "disconnect", image: true},
		{name: "metered_preamble_then_disconnect", failure: "disconnect"},
		{name: "text_then_capacity", prefix: `{"type":"response.output_text.delta","delta":"synthetic partial"}`, failure: "capacity"},
		{name: "cyber_before_text", failure: "cyber", cyber: true},
		{name: "cyber_after_text", prefix: `{"type":"response.output_text.delta","delta":"synthetic partial"}`, failure: "cyber", cyber: true},
		{name: "before_output_capacity_control", failure: "capacity", recoverable: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			cert := wsPartialSettlementTLS(t)
			var frames atomic.Int32
			var ownersMu sync.Mutex
			var owners []string
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/backend-api/codex/responses" {
					http.Error(w, "unexpected endpoint", 404)
					return
				}
				owner := r.Header.Get("Authorization")
				ownersMu.Lock()
				owners = append(owners, owner)
				ownersMu.Unlock()
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_, request, err := conn.ReadMessage()
				if err != nil {
					return
				}
				if gjson.GetBytes(request, "model").String() != "gpt-6-astra" {
					t.Errorf("mapped model changed on wire")
					return
				}
				n := frames.Add(1)
				if n > 1 {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_recovered","model":"gpt-6-astra","status":"completed","output":[],"usage":{"input_tokens":11,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}}`))
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_partial","model":"gpt-6-astra","status":"in_progress"}}`))
				if tc.prefix != "" {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(tc.prefix))
				}
				if !tc.recoverable {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.in_progress","response":{"id":"resp_partial","model":"gpt-6-astra","usage":{"input_tokens":11,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}}`))
				}
				switch tc.failure {
				case "capacity":
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"synthetic capacity rejection"}}`))
				case "cyber":
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"code":"cyber_policy","type":"rate_limit_error","message":"blocked by cyber policy"},"usage":{"input_tokens":11,"output_tokens":3}}`))
				}
			}))
			upstream.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}
			upstream.StartTLS()
			t.Cleanup(upstream.Close)
			proxy, connects := wsPartialSettlementProxy(t, upstream.Listener.Addr().String())
			groupID := int64(9610)
			imagePrice := 0.25
			group := &service.Group{ID: groupID, Hydrated: true, Name: "synthetic-settlement", Platform: service.PlatformOpenAI, Status: service.StatusActive, RateMultiplier: 1, AllowImageGeneration: true, ImagePrice1K: &imagePrice}
			accounts := []service.Account{}
			for i := 0; i < 2; i++ {
				accounts = append(accounts, service.Account{ID: 9611 + int64(i), Name: fmt.Sprintf("synthetic-%d", i), Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 2, Priority: 1 + i*100, ProxyID: &proxy.ID, Proxy: proxy,
					Credentials: map[string]any{"access_token": fmt.Sprintf("synthetic-token-%d", i), "chatgpt_account_id": fmt.Sprintf("synthetic-account-%d", i)},
					Extra: map[string]any{"responses_websockets_v2_enabled": true,
						service.OpenAIAstraPolicyExtraKey: map[string]any{
							"model":          "gpt-6-astra",
							"recent_samples": []any{map[string]any{"verdict": "healthy", "at": "2020-01-01T00:00:00Z"}},
						},
					}, AccountGroups: []service.AccountGroup{{AccountID: 9611 + int64(i), GroupID: groupID}}})
			}
			for i := range accounts {
				require.True(t, accounts[i].AllowsOpenAIOAuthModelPolicy("gpt-6-astra"), "synthetic account must pass the real model-policy gate")
			}
			accountRepo := &openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}
			usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 8)}
			billingRepo := &compatStreamUsageBillingRepo{}
			tripwire := &wsPartialSettlementHTTPTripwire{}
			cfg := &config.Config{}
			disableBackgroundRoute := false
			cfg.Gateway.OpenAITiboRoute.Enabled = &disableBackgroundRoute
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
			billingCache := service.NewBillingCacheService(newHandlerInflightCache(100), nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billingCache.Stop)
			// Handler preflight uses the balance stub. The billing transaction itself
			// records Apply directly; no background cache writer or external DB is used.
			svc := service.NewOpenAIGatewayService(accountRepo, usageRepo, billingRepo, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, nil, tripwire, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
			svc.StopOpenAICodexTicketHarvester()
			t.Cleanup(svc.CloseOpenAIWSPool)
			h := NewOpenAIGatewayHandler(svc, service.NewConcurrencyService(&fakeConcurrencyCache{}), billingCache, &service.APIKeyService{}, nil, nil, nil, nil, cfg)
			apiKey := &service.APIKey{ID: 9613, UserID: 9614, Key: "synthetic-client-key", GroupID: &groupID, Group: group, User: &service.User{ID: 9614, Status: service.StatusActive, Role: service.RoleUser, Balance: 100}}
			type finished struct {
				cyberMarked, cyberRecorded       bool
				transport, reason, upstreamError string
			}
			handlerDone := make(chan finished, 1)
			router := gin.New()
			logCore, observedLogs := observer.New(zap.DebugLevel)
			router.Use(func(c *gin.Context) {
				c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(logCore)))
				c.Set(string(middleware.ContextKeyAPIKey), apiKey)
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 9614, Concurrency: 2})
				c.Header("X-Request-Id", "handler-settlement-request")
				c.Next()
			})
			router.POST("/v1/responses", func(c *gin.Context) {
				h.Responses(c)
				handlerDone <- finished{service.GetOpsCyberPolicy(c) != nil, c.GetBool(cyberPolicyRecordedKey), c.GetString("openai_ws_transport_decision"), c.GetString("openai_ws_transport_reason"), c.GetString(service.OpsUpstreamErrorMessageKey)}
			})
			server := httptest.NewServer(router)
			t.Cleanup(server.Close)
			request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-6-astra","stream":false,"input":[{"type":"message","role":"user","content":"synthetic settlement request"}],"reasoning":{"effort":"high"}}`))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("User-Agent", "synthetic-handler-fixture")
			client := &http.Client{Timeout: 10 * time.Second}
			response, err := client.Do(request)
			require.NoError(t, err)
			responseBody, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			var finishedState finished
			select {
			case finishedState = <-handlerDone:
			case <-time.After(3 * time.Second):
				t.Fatal("actual Responses handler did not return")
			}
			require.Greater(t, frames.Load(), int32(0), "fixture must execute a real upstream response.create: transport=%s reason=%s error=%s proxy_connects=%d HTTP_calls=%d body=%s logs=%v", finishedState.transport, finishedState.reason, finishedState.upstreamError, connects.Load(), tripwire.calls.Load(), responseBody, observedLogs.AllUntimed())
			var log *service.UsageLog
			var observedUsage []*service.UsageLog
			usageDeadline := time.NewTimer(3 * time.Second)
			defer usageDeadline.Stop()
		usageWait:
			for {
				select {
				case row := <-usageRepo.created:
					observedUsage = append(observedUsage, row)
					// Normal partial-result settlement is synchronous with handler
					// return. Cyber's Ops-owned task is asynchronous and creates a
					// non-WS usage row. Wait for that exact owner before counting;
					// otherwise an early duplicate normal row could hide a later
					// second charge by winning the first channel receive.
					if !tc.cyber || (row.RequestType == service.RequestTypeCyberBlocked && !row.OpenAIWSMode) {
						log = row
						break usageWait
					}
				case <-usageDeadline.C:
					t.Fatalf("missing expected settlement owner after handler: HTTP=%d rows=%d body=%s", response.StatusCode, len(observedUsage), responseBody)
				}
			}
			require.Len(t, observedUsage, 1, "cyber's asynchronous owner must not coexist with normal partial-result settlement")
			billingRepo.mu.Lock()
			commands := append([]*service.UsageBillingCommand(nil), billingRepo.commands...)
			billingRepo.mu.Unlock()
			require.Len(t, commands, 1, "one real handler request must settle exactly one billing command")
			require.Empty(t, usageRepo.created, "one usage row only, including cyber's independent asynchronous path")
			require.Equal(t, int32(0), tripwire.calls.Load(), "no HTTP replay")
			ownersMu.Lock()
			seenOwners := append([]string(nil), owners...)
			ownersMu.Unlock()
			require.NotEmpty(t, seenOwners)
			require.Equal(t, "Bearer synthetic-token-0", seenOwners[0], "the primary account is used before any possible failover")
			if tc.recoverable {
				require.GreaterOrEqual(t, frames.Load(), int32(2), "the fixture permits pre-output recovery")
				require.Equal(t, http.StatusOK, response.StatusCode)
				require.Equal(t, "resp_recovered", log.RequestID)
			} else {
				require.Equal(t, int32(1), frames.Load(), "neither same-account retry nor account switch may replay a started inference")
				require.Equal(t, int32(1), connects.Load())
				require.Equal(t, []string{"Bearer synthetic-token-0"}, seenOwners)
				require.GreaterOrEqual(t, response.StatusCode, 400)
				require.NotContains(t, string(responseBody), "resp_recovered")
				require.Equal(t, int64(9611), log.AccountID)
			}
			require.Equal(t, "gpt-6-astra", log.Model)
			require.Equal(t, 11, log.InputTokens+log.CacheReadTokens)
			require.Equal(t, 3, log.OutputTokens)
			require.False(t, log.Stream)
			require.Equal(t, log.RequestID, commands[0].RequestID)
			require.Equal(t, log.AccountID, commands[0].AccountID)
			require.Equal(t, log.InputTokens, commands[0].InputTokens)
			require.Equal(t, log.OutputTokens, commands[0].OutputTokens)
			require.Greater(t, commands[0].BalanceCost, 0.0, "this must exercise billable settlement, not Simple Mode")
			require.Equal(t, tc.cyber, finishedState.cyberMarked)
			require.Equal(t, tc.cyber, finishedState.cyberRecorded)
			if tc.cyber {
				require.Equal(t, service.RequestTypeCyberBlocked, log.RequestType)
				require.False(t, log.OpenAIWSMode, "cyber is settled by the existing Ops-owned path")
				// Existing OpsCyberPolicy settlement owns the HTTP request identity;
				// there must be no second normal WS usage row for resp_partial.
				require.Equal(t, "handler-settlement-request", log.RequestID)
			} else {
				require.True(t, log.OpenAIWSMode)
				require.NotEqual(t, service.RequestTypeCyberBlocked, log.RequestType)
				if !tc.recoverable {
					require.Equal(t, "resp_partial", log.RequestID)
				}
				require.NotNil(t, log.UpstreamModel)
				require.Equal(t, "gpt-6-astra", *log.UpstreamModel)
				require.NotNil(t, log.ReasoningEffort)
				require.Equal(t, "high", *log.ReasoningEffort)
				require.Equal(t, 2, log.CacheReadTokens)
			}
			wantImages := 0
			if tc.image {
				wantImages = 1
			}
			require.Equal(t, wantImages, log.ImageCount)
			require.Equal(t, wantImages, commands[0].ImageCount)
		})
	}
}
