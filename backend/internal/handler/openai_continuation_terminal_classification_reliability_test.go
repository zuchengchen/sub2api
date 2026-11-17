//go:build unit

package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type oauthContinuationClassificationUpstream struct {
	service.HTTPUpstream
	mu            sync.Mutex
	status        int
	body          string
	succeedSecond bool
	accountIDs    []int64
	bodies        [][]byte
}

func (u *oauthContinuationClassificationUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	u.bodies = append(u.bodies, append([]byte(nil), body...))
	u.mu.Unlock()
	status, response := u.status, u.body
	if u.succeedSecond && accountID == 9911 {
		status = http.StatusOK
		response = `{"id":"resp_classification_success","object":"response","status":"completed","model":"gpt-5.6-sol","output":[],"usage":{"input_tokens":7,"output_tokens":2}}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"synthetic-continuation-classification"}, "Retry-After": {"3"}}, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
}

func (u *oauthContinuationClassificationUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

func oauthContinuationClassificationHandler(t *testing.T, upstream service.HTTPUpstream, passthrough bool, websocketIngress ...bool) *OpenAIGatewayHandler {
	t.Helper()
	ws := len(websocketIngress) > 0 && websocketIngress[0]
	accounts := []service.Account{
		{ID: 9910, Name: "synthetic-continuation-first", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 1},
		{ID: 9911, Name: "synthetic-continuation-second", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 2},
	}
	for i := range accounts {
		accounts[i].Credentials = map[string]any{"api_key": "synthetic-key", "base_url": "https://upstream.example", "pool_mode": true, "pool_mode_retry_count": 0, "pool_mode_retry_status_codes": []any{403, 502, 503}}
		accounts[i].Extra = astraHandlerExtra("healthy")
		accounts[i].Extra["openai_passthrough"] = passthrough
		accounts[i].Extra[openai_compat.ExtraKeyResponsesMode] = string(openai_compat.ResponsesSupportModeForceResponses)
		if ws {
			accounts[i].Extra["responses_websockets_v2_enabled"] = true
			accounts[i].Extra["openai_apikey_responses_websockets_v2_mode"] = service.OpenAIWSIngressModeHTTPBridge
		}
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Gateway.MaxAccountSwitches = 1
	cfg.Gateway.OpenAIWS.ForceHTTP = !ws
	cfg.Gateway.OpenAIWS.Enabled = ws
	cfg.Gateway.OpenAIWS.APIKeyEnabled = ws
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = ws
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = ws
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	usage := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 8)}
	svc := service.NewOpenAIGatewayService(&openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}, usage, nil, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	svc.DisableOpenAITiboRouteForTest()
	svc.StopOpenAICodexTicketHarvester()
	t.Cleanup(svc.CloseOpenAIWSPool)
	// A real tenant owner is established before the native-anchor tests enter
	// the public handler. No synthetic cross-owner bypass is used.
	require.NoError(t, svc.BindOpenAIHTTPResponseOwner(context.Background(), 3131, "resp_synthetic_owned", 100, 99))
	h := NewOpenAIGatewayHandler(svc, service.NewConcurrencyService(nil), billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	h.maxAccountSwitches = 1
	return h
}

func oauthContinuationClassificationRequest(t *testing.T, route, fields string, stream bool) (*gin.Context, *httptest.ResponseRecorder, []byte) {
	t.Helper()
	c, rec := newOpenAIResponsesFailoverTestContext(t, context.Background())
	key, ok := middleware2.GetAPIKeyFromContext(c)
	require.True(t, ok)
	key.UserID = key.User.ID
	path := "/v1/responses"
	if route == "chat_responses_shape" {
		path = "/v1/chat/completions"
	}
	body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":%t,%s}`, stream, fields))
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, rec, body
}

func TestOAuthReliabilityContinuationReplayDeniedKeepsUpstreamFailure(t *testing.T) {
	for _, route := range []string{"responses", "passthrough", "chat_responses_shape"} {
		for _, input := range []struct{ name, fields string }{
			{"opaque_no_tool_output", `"input":[{"type":"reasoning","id":"rs_synthetic","summary":[],"encrypted_content":"synthetic-ciphertext"},{"role":"user","content":"continue synthetic complete history"}]`},
			{"owned_native_anchor", `"previous_response_id":"resp_synthetic_owned","input":[{"role":"user","content":"synthetic native delta"}]`},
		} {
			for _, failure := range []struct {
				name       string
				status     int
				body       string
				wantClient int
			}{
				{"rate_limit_429", 429, `{"error":{"type":"rate_limit_error","message":"synthetic rate limit"}}`, 429},
				{"upstream_502", 502, `{"error":{"type":"server_error","message":"synthetic upstream temporary failure"}}`, 502},
				{"capacity_503", 503, `{"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`, 503},
				{"credential_403", 403, `{"error":{"code":"workspace_suspended","message":"synthetic workspace is suspended"}}`, 502},
			} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/stream=%t", route, input.name, failure.name, stream), func(t *testing.T) {
						upstream := &oauthContinuationClassificationUpstream{status: failure.status, body: failure.body, succeedSecond: true}
						h := oauthContinuationClassificationHandler(t, upstream, route == "passthrough")
						c, rec, body := oauthContinuationClassificationRequest(t, route, input.fields, stream)
						proof := service.AnalyzeOpenAIContinuationReplayProof(body)
						require.False(t, proof.HasToolOutput, "the exact regression must not fabricate missing calls from an ordinary native history")
						require.NoError(t, service.ValidateOpenAIContinuationInput(body))
						require.False(t, service.OpenAIContinuationMaySwitchAccount(body), "the safety boundary remains closed")
						if route == "chat_responses_shape" {
							h.ChatCompletions(c)
						} else {
							h.Responses(c)
						}
						require.Equal(t, []int64{9910}, upstream.accountIDs, "no replay or cross-credential migration, even with a healthy second account")
						require.Len(t, upstream.bodies, 1)
						require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(upstream.bodies[0], "input").Raw, "preserve native content on the first attempt")
						require.Equal(t, gjson.GetBytes(body, "previous_response_id").String(), gjson.GetBytes(upstream.bodies[0], "previous_response_id").String())
						events, ok := c.MustGet(service.OpsUpstreamErrorsKey).([]*service.OpsUpstreamErrorEvent)
						require.True(t, ok)
						require.Len(t, events, 1, "one actual failed attempt owns the Ops status")
						require.Equal(t, int64(9910), events[0].AccountID)
						require.Equal(t, "failover", events[0].Kind)
						require.Equal(t, failure.status, events[0].UpstreamStatusCode)
						require.Equal(t, failure.wantClient, rec.Code, rec.Body.String())
						require.NotEqual(t, "continuation_context_missing", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
						require.NotEqual(t, "invalid_request_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
						require.NotContains(t, rec.Body.String(), "matching preceding call")
						require.Equal(t, "3", rec.Header().Get("Retry-After"))
					})
				}
			}
		}
	}
}

func TestOAuthReliabilityContinuationFailureClassificationControls(t *testing.T) {
	for _, scenario := range []string{"true_orphan", "opaque_zero_switch_budget", "portable_next_account", "portable_zero_switch_budget"} {
		t.Run(scenario, func(t *testing.T) {
			upstream := &oauthContinuationClassificationUpstream{status: 502, body: `{"error":{"message":"synthetic upstream failure"}}`, succeedSecond: true}
			h := oauthContinuationClassificationHandler(t, upstream, true)
			fields := `"input":"synthetic portable root"`
			if scenario == "true_orphan" {
				fields = `"input":[{"type":"function_call_output","call_id":"call_missing","output":"synthetic result"}]`
			}
			if scenario == "opaque_zero_switch_budget" {
				fields = `"input":[{"type":"reasoning","summary":[],"encrypted_content":"synthetic-ciphertext"},{"role":"user","content":"continue"}]`
			}
			if strings.HasSuffix(scenario, "zero_switch_budget") {
				h.maxAccountSwitches = 0
			}
			c, rec, _ := oauthContinuationClassificationRequest(t, "passthrough", fields, false)
			h.Responses(c)
			switch scenario {
			case "true_orphan":
				require.Empty(t, upstream.accountIDs)
				require.Equal(t, 400, rec.Code)
				require.Equal(t, "continuation_context_missing", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
			case "portable_next_account":
				require.Equal(t, []int64{9910, 9911}, upstream.accountIDs)
				require.Equal(t, 200, rec.Code)
			default:
				require.Equal(t, []int64{9910}, upstream.accountIDs)
				require.Equal(t, 502, rec.Code)
				require.NotContains(t, rec.Body.String(), "continuation_context_missing")
			}
		})
	}
}

func TestOAuthReliabilityContinuationWSReplayDeniedKeepsUpstreamFailure(t *testing.T) {
	for _, status := range []int{429, 502} {
		t.Run(fmt.Sprintf("status=%d", status), func(t *testing.T) {
			upstream := &oauthContinuationClassificationUpstream{status: status, body: `{"error":{"message":"synthetic upstream failure"}}`, succeedSecond: true}
			h := oauthContinuationClassificationHandler(t, upstream, false, true)
			seed, _, _ := oauthContinuationClassificationRequest(t, "responses", `"input":"seed"`, true)
			done := make(chan *gin.Context, 1)
			router := gin.New()
			router.GET("/v1/responses", func(c *gin.Context) {
				for key, value := range seed.Keys {
					c.Set(key, value)
				}
				h.ResponsesWebSocket(c)
				done <- c
			})
			server := httptest.NewServer(router)
			t.Cleanup(server.Close)
			client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close() })
			require.NoError(t, client.SetReadDeadline(time.Now().Add(8*time.Second)))
			payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":[{"type":"reasoning","summary":[],"encrypted_content":"synthetic-ciphertext"},{"role":"user","content":"continue synthetic native history"}]}`)
			require.NoError(t, client.WriteMessage(websocket.TextMessage, payload))
			var terminalErr error
			for {
				_, frame, readErr := client.ReadMessage()
				if readErr != nil {
					terminalErr = readErr
					break
				}
				require.NotContains(t, string(frame), "continuation_context_missing")
				require.NotContains(t, string(frame), "matching preceding call")
			}
			require.True(t, websocket.IsCloseError(terminalErr, websocket.CloseTryAgainLater), "error=%v", terminalErr)
			var c *gin.Context
			select {
			case c = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("WS handler did not terminate")
			}
			require.Equal(t, []int64{9910}, upstream.accountIDs)
			require.Len(t, upstream.bodies, 1)
			require.JSONEq(t, gjson.GetBytes(payload, "input").Raw, gjson.GetBytes(upstream.bodies[0], "input").Raw)
			streamErr, ok := service.GetOpsStreamError(c)
			require.True(t, ok)
			require.Equal(t, status, streamErr.IntendedStatus)
			require.Len(t, streamErr.UpstreamErrors, 1)
			require.Equal(t, status, streamErr.UpstreamErrors[0].UpstreamStatusCode)
			require.Equal(t, int64(9910), streamErr.UpstreamErrors[0].AccountID)
			require.NotEqual(t, "invalid_request_error", streamErr.ErrType)
			if status == 429 {
				require.Equal(t, "rate_limit_error", streamErr.ErrType)
			}
		})
	}
}
