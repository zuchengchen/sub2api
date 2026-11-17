//go:build unit

package handler

import (
	"bytes"
	"context"
	"encoding/json"
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

// Use a native model that is unchanged by Codex alias normalization so the
// assertions isolate account failover from the existing model-mapping policy.
const oauthReliabilityHandlerModelError = `{"error":{"type":"invalid_request_error","code":"model_not_found","param":"model","message":"The requested model is unavailable on this credential"}}`
const oauthReliabilityHandlerModelDetail = `{"detail":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}`
const oauthReliabilityHandlerModelSuccess = `{"type":"response.completed","response":{"id":"resp_model_available","object":"response","model":"gpt-5.6-sol","status":"completed","output":[{"type":"message","id":"msg_model_available","role":"assistant","status":"completed","content":[{"type":"output_text","text":"model available on the next credential"}]}],"usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}}`

type oauthReliabilityModelHandlerUpstream struct {
	service.HTTPUpstream
	mu            sync.Mutex
	status        int
	body          string
	succeedSecond bool
	accountIDs    []int64
	models        []string
	onFirst       func()
}

func (u *oauthReliabilityModelHandlerUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	wire, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	u.models = append(u.models, gjson.GetBytes(wire, "model").String())
	first := len(u.accountIDs) == 1
	u.mu.Unlock()
	if first && u.onFirst != nil {
		u.onFirst()
	}
	status, body, contentType := u.status, u.body, "application/json"
	if u.succeedSecond && accountID == 2 {
		status, body = http.StatusOK, "data: "+oauthReliabilityHandlerModelSuccess+"\n\n"
	}
	if status == http.StatusOK {
		contentType = "text/event-stream"
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}, "X-Request-Id": {fmt.Sprintf("synthetic-model-%d", accountID)}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func (u *oauthReliabilityModelHandlerUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

func (u *oauthReliabilityModelHandlerUpstream) calls() ([]int64, []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...), append([]string(nil), u.models...)
}

func oauthReliabilityModelHandler(t *testing.T, upstream service.HTTPUpstream, passthrough, ws bool) (*OpenAIGatewayHandler, *openAIWSUsageHandlerUsageLogRepoStub) {
	t.Helper()
	accounts := []service.Account{
		{ID: 1, Name: "synthetic-model-oauth", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Priority: 0,
			Credentials: map[string]any{"access_token": "synthetic-token"}, Extra: astraHandlerExtra("healthy")},
		{ID: 2, Name: "synthetic-model-api-key", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 10,
			Credentials: map[string]any{"api_key": "synthetic-key", "base_url": "https://upstream.example", "pool_mode": true, "pool_mode_retry_status_codes": []any{400, 502}}, Extra: astraHandlerExtra("healthy")},
	}
	for i := range accounts {
		accounts[i].Extra["openai_passthrough"] = passthrough
		accounts[i].Extra[openai_compat.ExtraKeyResponsesMode] = string(openai_compat.ResponsesSupportModeForceResponses)
		if ws {
			accounts[i].Extra["responses_websockets_v2_enabled"] = true
			accounts[i].Extra["openai_oauth_responses_websockets_v2_mode"] = service.OpenAIWSIngressModeHTTPBridge
			accounts[i].Extra["openai_apikey_responses_websockets_v2_mode"] = service.OpenAIWSIngressModeHTTPBridge
		}
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Gateway.MaxAccountSwitches = 1
	cfg.Gateway.OpenAIWS.Enabled = ws
	cfg.Gateway.OpenAIWS.OAuthEnabled = ws
	cfg.Gateway.OpenAIWS.APIKeyEnabled = ws
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = ws
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = ws
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	usage := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 8)}
	billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	svc := service.NewOpenAIGatewayService(openAIImagesFailoverAccountRepo{accounts: accounts}, usage, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billingCache, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	svc.DisableOpenAITiboRouteForTest()
	svc.StopOpenAICodexTicketHarvester()
	t.Cleanup(svc.CloseOpenAIWSPool)
	h := NewOpenAIGatewayHandler(svc, service.NewConcurrencyService(nil), billingCache, &service.APIKeyService{}, nil, nil, nil, nil, cfg)
	h.maxAccountSwitches = 1
	return h, usage
}

func oauthReliabilityModelRequest(t *testing.T, route string, stream bool, ctx context.Context) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	c, recorder := newOpenAIResponsesFailoverTestContext(t, ctx)
	key, _ := middleware2.GetAPIKeyFromContext(c)
	key.Group.AllowMessagesDispatch = true
	path, input := "/v1/responses", `"input":"synthetic request"`
	if route == "chat" {
		path, input = "/v1/chat/completions", `"messages":[{"role":"user","content":"synthetic request"}]`
	} else if route == "messages" {
		path, input = "/v1/messages", `"max_tokens":32,"messages":[{"role":"user","content":"synthetic request"}]`
	}
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":%t,%s}`, stream, input))).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	return c, recorder
}

func oauthReliabilityRunModelHandler(h *OpenAIGatewayHandler, c *gin.Context, route string) {
	switch route {
	case "chat":
		h.ChatCompletions(c)
	case "messages":
		h.Messages(c)
	default:
		h.Responses(c)
	}
}

func TestOAuthReliabilityModelUnavailableHandlerRoutesAndExhaustion(t *testing.T) {
	for _, route := range []string{"responses", "passthrough", "chat", "messages"} {
		for _, status := range []int{400, 502} {
			for _, stream := range []bool{false, true} {
				for _, scenario := range []string{"next_account_success", "all_accounts_reject", "zero_switch_budget"} {
					t.Run(fmt.Sprintf("%s/%d/stream=%t/%s", route, status, stream, scenario), func(t *testing.T) {
						upstream := &oauthReliabilityModelHandlerUpstream{status: status, body: oauthReliabilityHandlerModelDetail, succeedSecond: scenario == "next_account_success"}
						h, usage := oauthReliabilityModelHandler(t, upstream, route == "passthrough", false)
						if scenario == "zero_switch_budget" {
							h.maxAccountSwitches = 0
						}
						c, recorder := oauthReliabilityModelRequest(t, route, stream, context.Background())
						oauthReliabilityRunModelHandler(h, c, route)
						ids, models := upstream.calls()
						if scenario == "zero_switch_budget" {
							require.Equal(t, []int64{1}, ids)
						} else {
							require.Equal(t, []int64{1, 2}, ids, "one attempt per credential, including pool-mode API key")
						}
						for _, model := range models {
							require.Equal(t, "gpt-5.6-sol", model, "account failover never substitutes the requested model")
						}
						if scenario == "next_account_success" {
							require.Equal(t, http.StatusOK, recorder.Code)
							require.Contains(t, recorder.Body.String(), "model available on the next credential")
							require.NotContains(t, recorder.Body.String(), "model_not_found")
							require.Len(t, usage.created, 1, "only the successful attempt records usage")
							log := <-usage.created
							require.Equal(t, int64(2), log.AccountID)
							require.Equal(t, 7, log.InputTokens+log.CacheReadTokens)
							require.Equal(t, 2, log.OutputTokens)
						} else {
							require.Equal(t, http.StatusBadRequest, recorder.Code)
							require.True(t, json.Valid(recorder.Body.Bytes()))
							require.Equal(t, "model_not_found", gjson.Get(recorder.Body.String(), "error.code").String())
							require.Equal(t, "model", gjson.Get(recorder.Body.String(), "error.param").String())
							require.Equal(t, "invalid_request_error", gjson.Get(recorder.Body.String(), "error.type").String())
							require.Equal(t, status, c.GetInt(service.OpsUpstreamStatusCodeKey))
							require.Empty(t, usage.created, "rejected attempts without usage do not create bills")
						}
					})
				}
			}
		}
	}
}

func TestOAuthReliabilityModelUnavailableHandlerUnknownAndCanceledControls(t *testing.T) {
	for _, route := range []string{"responses", "passthrough", "chat", "messages"} {
		for _, cancelRequest := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cancel=%t", route, cancelRequest), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				upstream := &oauthReliabilityModelHandlerUpstream{status: 400, body: `{"error":{"type":"invalid_request_error","code":"invalid_value","param":"input","message":"Bad Request"},"request":{"text":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}}`}
				if cancelRequest {
					upstream.body = oauthReliabilityHandlerModelError
					upstream.onFirst = cancel
				}
				h, usage := oauthReliabilityModelHandler(t, upstream, route == "passthrough", false)
				c, recorder := oauthReliabilityModelRequest(t, route, false, ctx)
				oauthReliabilityRunModelHandler(h, c, route)
				ids, _ := upstream.calls()
				require.Equal(t, []int64{1}, ids)
				require.Empty(t, usage.created)
				if !cancelRequest {
					require.Equal(t, http.StatusBadRequest, recorder.Code)
					require.NotContains(t, recorder.Body.String(), `"code":"model_not_found"`)
				}
			})
		}
	}
}

func oauthReliabilityModelFailover(status int) *service.UpstreamFailoverError {
	return &service.UpstreamFailoverError{StatusCode: status, ResponseBody: []byte(`{"error":{"message":"PRIVATE_TOKEN"}}`), Reason: service.OpenAIModelUnavailableReason,
		ClientStatusCode: http.StatusBadRequest, ClientMessage: "PRIVATE_MODEL", Scope: service.GatewayFailureScopeAccount, NextAccountAction: service.NextAccountRetry}
}

func TestOAuthReliabilityModelUnavailableCommittedHTTPKeepsParameterAndOneTerminal(t *testing.T) {
	for _, route := range []string{"responses", "chat", "messages"} {
		for _, status := range []int{400, 502} {
			for _, prior := range []string{"heartbeat", "semantic_output", "terminal"} {
				t.Run(fmt.Sprintf("%s/%d/%s", route, status, prior), func(t *testing.T) {
					c, recorder := oauthReliabilityModelRequest(t, route, true, context.Background())
					prefix := ": keepalive\n\n"
					if prior == "semantic_output" {
						prefix = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"existing output\"}\n\n"
					} else if prior == "terminal" {
						prefix = "event: error\ndata: {\"error\":{\"code\":\"existing_failure\"}}\n\n"
					}
					c.Header("Content-Type", "text/event-stream")
					_, err := c.Writer.WriteString(prefix)
					require.NoError(t, err)
					c.Writer.Flush()
					if prior == "terminal" {
						service.MarkResponseCommitted(c)
						service.MarkOpsStreamFailure(c, "server_error", "existing_failure", "existing terminal", 503)
					}
					h := &OpenAIGatewayHandler{}
					for range 2 {
						if route == "messages" {
							h.handleAnthropicFailoverExhausted(c, oauthReliabilityModelFailover(status), false)
						} else {
							h.handleFailoverExhausted(c, oauthReliabilityModelFailover(status), false)
						}
					}
					require.Equal(t, http.StatusOK, recorder.Code, "committed wire status cannot become 400")
					if prior == "terminal" {
						require.Equal(t, prefix, recorder.Body.String())
						return
					}
					require.Equal(t, 1, strings.Count(recorder.Body.String(), `"param":"model"`))
					require.Equal(t, 1, strings.Count(recorder.Body.String(), `"code":"model_not_found"`))
					require.NotContains(t, recorder.Body.String(), "PRIVATE")
					if route == "responses" {
						require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: response.failed"))
					} else {
						require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: error"))
					}
					streamErr, ok := service.GetOpsStreamError(c)
					require.True(t, ok)
					require.Equal(t, 400, streamErr.IntendedStatus)
					require.Equal(t, status, streamErr.UpstreamStatus)
					require.Equal(t, "model_not_found", streamErr.Code)
				})
			}
		}
	}
}

func TestOAuthReliabilityModelUnavailableFullHandlerAfterHeadersDoesNotReplay(t *testing.T) {
	for _, route := range []string{"responses", "passthrough", "chat", "messages"} {
		for _, status := range []int{400, 502} {
			t.Run(fmt.Sprintf("%s/%d", route, status), func(t *testing.T) {
				c, recorder := oauthReliabilityModelRequest(t, route, true, context.Background())
				upstream := &oauthReliabilityModelHandlerUpstream{status: status, body: oauthReliabilityHandlerModelError, succeedSecond: true}
				upstream.onFirst = func() {
					c.Header("Content-Type", "text/event-stream")
					_, err := c.Writer.WriteString(": keepalive\n\n")
					require.NoError(t, err)
					c.Writer.Flush()
				}
				h, usage := oauthReliabilityModelHandler(t, upstream, route == "passthrough", false)
				oauthReliabilityRunModelHandler(h, c, route)
				ids, _ := upstream.calls()
				require.Equal(t, []int64{1}, ids, "a committed downstream response cannot switch credentials")
				require.Equal(t, http.StatusOK, recorder.Code)
				require.Equal(t, 1, strings.Count(recorder.Body.String(), `"param":"model"`))
				require.Equal(t, 1, strings.Count(recorder.Body.String(), `"code":"model_not_found"`))
				streamErr, ok := service.GetOpsStreamError(c)
				require.True(t, ok)
				require.Equal(t, 400, streamErr.IntendedStatus)
				require.Equal(t, status, streamErr.UpstreamStatus)
				require.Empty(t, usage.created)
			})
		}
	}
}

func TestOAuthReliabilityModelUnavailableHTTPHandlerNeverReplaysMeteredFailure(t *testing.T) {
	for _, route := range []string{"responses", "passthrough", "chat", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", route, stream), func(t *testing.T) {
				upstream := &oauthReliabilityModelHandlerUpstream{status: 200, body: "data: " + `{"type":"response.failed","response":{"id":"resp_model_metered","model":"gpt-5.6-sol","status":"failed","output":[],"error":{"type":"invalid_request_error","code":"model_not_found","param":"model","message":"model unavailable"},"usage":{"input_tokens":11,"output_tokens":3}}}` + "\n\n"}
				h, usage := oauthReliabilityModelHandler(t, upstream, route == "passthrough", false)
				c, recorder := oauthReliabilityModelRequest(t, route, stream, context.Background())
				oauthReliabilityRunModelHandler(h, c, route)
				ids, _ := upstream.calls()
				require.Equal(t, []int64{1}, ids)
				require.Equal(t, 1, strings.Count(recorder.Body.String(), "model_not_found"), "one authoritative terminal")
				require.Len(t, usage.created, 1, "metered failure settles once")
				log := <-usage.created
				require.Equal(t, int64(1), log.AccountID)
				require.Equal(t, 11, log.InputTokens+log.CacheReadTokens)
				require.Equal(t, 3, log.OutputTokens)
			})
		}
	}
}

func TestOAuthReliabilityModelUnavailableNativeWSHandlerExhaustion(t *testing.T) {
	for _, status := range []int{400, 502} {
		for _, succeeds := range []bool{false, true} {
			t.Run(fmt.Sprintf("status=%d/second_succeeds=%t", status, succeeds), func(t *testing.T) {
				upstream := &oauthReliabilityModelHandlerUpstream{status: status, body: oauthReliabilityHandlerModelError, succeedSecond: succeeds}
				h, usage := oauthReliabilityModelHandler(t, upstream, false, true)
				seed, _ := oauthReliabilityModelRequest(t, "responses", true, context.Background())
				key, _ := middleware2.GetAPIKeyFromContext(seed)
				done := make(chan *gin.Context, 1)
				router := gin.New()
				router.GET("/v1/responses", func(c *gin.Context) {
					c.Set(string(middleware2.ContextKeyAPIKey), key)
					c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: key.User.ID, Concurrency: 0})
					h.ResponsesWebSocket(c)
					done <- c
				})
				server := httptest.NewServer(router)
				t.Cleanup(server.Close)
				client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
				require.NoError(t, err)
				t.Cleanup(func() { _ = client.Close() })
				require.NoError(t, client.SetReadDeadline(time.Now().Add(8*time.Second)))
				require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"synthetic request"}`)))
				var frames [][]byte
				var terminalErr error
				for {
					_, frame, readErr := client.ReadMessage()
					if readErr != nil {
						terminalErr = readErr
						break
					}
					frames = append(frames, bytes.Clone(frame))
					if gjson.GetBytes(frame, "type").String() == "response.completed" {
						_ = client.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"))
						_ = client.Close()
						break
					}
				}
				var c *gin.Context
				select {
				case c = <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("native WS handler did not stop")
				}
				ids, models := upstream.calls()
				require.Equal(t, []int64{1, 2}, ids)
				require.Equal(t, []string{"gpt-5.6-sol", "gpt-5.6-sol"}, models)
				require.Len(t, frames, 1)
				if succeeds {
					require.Equal(t, "response.completed", gjson.GetBytes(frames[0], "type").String())
					require.Len(t, usage.created, 1)
				} else {
					require.True(t, websocket.IsCloseError(terminalErr, websocket.ClosePolicyViolation), "error=%v", terminalErr)
					require.Equal(t, 400, int(gjson.GetBytes(frames[0], "status").Int()))
					require.Equal(t, "model_not_found", gjson.GetBytes(frames[0], "error.code").String())
					require.Equal(t, "model", gjson.GetBytes(frames[0], "error.param").String())
					streamErr, ok := service.GetOpsStreamError(c)
					require.True(t, ok)
					require.Equal(t, 400, streamErr.IntendedStatus)
					require.Equal(t, status, streamErr.UpstreamStatus)
					require.Empty(t, usage.created)
				}
			})
		}
	}
}

func TestOAuthReliabilityModelUnavailableWSDeduplicatesCurrentTurnOnly(t *testing.T) {
	for _, priorTurn := range []int{1, 2} {
		t.Run(fmt.Sprintf("prior_turn=%d", priorTurn), func(t *testing.T) {
			c, _ := oauthReliabilityModelRequest(t, "responses", true, context.Background())
			setOpenAIClientTransportWS(c)
			service.BeginOpsStreamTurn(c, priorTurn)
			service.MarkOpsStreamFailure(c, "server_error", "prior_failure", "prior turn", 503)
			service.BeginOpsStreamTurn(c, 2)
			closeOpenAIWSFailoverExhausted(c, nil, oauthReliabilityModelFailover(502))
			err, ok := service.GetOpsStreamError(c)
			require.True(t, ok)
			if priorTurn == 2 {
				require.Equal(t, "prior_failure", err.Code)
			} else {
				require.Equal(t, "model_not_found", err.Code)
				require.Equal(t, 400, err.IntendedStatus)
				require.Equal(t, 502, err.UpstreamStatus)
			}
		})
	}
	// The typed hint does not alter an unrelated close classification.
	c, _ := oauthReliabilityModelRequest(t, "responses", true, context.Background())
	closeOpenAIWSFailoverExhausted(c, nil, &service.UpstreamFailoverError{StatusCode: 502})
	err, ok := service.GetOpsStreamError(c)
	require.True(t, ok)
	require.Equal(t, 502, err.IntendedStatus)
	require.NotEqual(t, "model_not_found", err.Code)
}

func TestOAuthReliabilityModelUnavailableHTTPStatusUsageSettlesOnce(t *testing.T) {
	for _, route := range []string{"responses", "passthrough", "chat", "messages"} {
		for _, status := range []int{400, 502} {
			for _, stream := range []bool{false, true} {
				for _, nested := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/status=%d/stream=%t/nested=%t", route, status, stream, nested), func(t *testing.T) {
						body := strings.TrimSuffix(oauthReliabilityHandlerModelError, "}") + `,"usage":{"input_tokens":11,"input_tokens_details":{"cached_tokens":4},"output_tokens":3}}`
						if nested {
							body = `{"type":"response.failed","response":` + body + `}`
						}
						upstream := &oauthReliabilityModelHandlerUpstream{status: status, body: body, succeedSecond: true}
						h, usage := oauthReliabilityModelHandler(t, upstream, route == "passthrough", false)
						c, recorder := oauthReliabilityModelRequest(t, route, stream, context.Background())
						oauthReliabilityRunModelHandler(h, c, route)
						ids, _ := upstream.calls()
						require.Equal(t, []int64{1}, ids, "provider-metered model rejection cannot authorize a second execution")
						require.Equal(t, http.StatusBadRequest, recorder.Code)
						require.Equal(t, "model_not_found", gjson.Get(recorder.Body.String(), "error.code").String())
						require.Equal(t, "model", gjson.Get(recorder.Body.String(), "error.param").String())
						require.Equal(t, status, c.GetInt(service.OpsUpstreamStatusCodeKey))
						require.Len(t, usage.created, 1, "actual upstream usage settles exactly once despite client400")
						log := <-usage.created
						require.Equal(t, int64(1), log.AccountID)
						require.Equal(t, 11, log.InputTokens+log.CacheReadTokens)
						require.Equal(t, 4, log.CacheReadTokens)
						require.Equal(t, 3, log.OutputTokens)
					})
				}
			}
		}
	}
}
