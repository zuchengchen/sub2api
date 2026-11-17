//go:build unit

package handler

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
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
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// These are public synthetic constraints, never a captured customer request.
const oauthContractWSStrictSchema = `{"type":"object","properties":{"values":{"type":"array","items":{"type":"string","pattern":"^[a-z]+$"},"uniqueItems":true,"minItems":1},"n":{"type":"integer","minimum":9007199254740993}},"required":["values","n"],"additionalProperties":false}`
const oauthContractWSSchemaMessage = `Invalid schema for response_format 'synthetic_contract': In context=('properties', 'values'), uniqueItems is not permitted when strict is true.`

func oauthContractWSSchemaError(code, typ string) string {
	body, _ := json.Marshal(map[string]any{"code": code, "type": typ, "param": "text.format.schema", "message": oauthContractWSSchemaMessage})
	return string(body)
}

func oauthContractWSSchemaFailed(object, usage string) string {
	if usage == "" {
		usage = "null"
	}
	return `{"type":"response.failed","response":{"id":"resp_schema_contract","object":"response","status":"failed","output":[],"error":` + object + `,"usage":` + usage + `}}`
}

// The public HTTP handler, scheduler, OAuth request builder, actual TLS WS
// connection, reader, error writer and Ops middleware all execute. The account
// proxy pins chatgpt.com to loopback; any HTTP inference is a tripwire.
func TestOAuthReliabilityContractWSStrictSchemaHTTPBoundary(t *testing.T) {
	schemaObject := oauthContractWSSchemaError("invalid_json_schema", "")
	bare := `{"type":"error","code":"invalid_json_schema","param":"text.format.schema","message":` + oauthContractWSSchemaQuote(oauthContractWSSchemaMessage) + `}`
	metered := `{"input_tokens":5,"output_tokens":1}`
	usageFrame := `{"type":"response.in_progress","response":{"id":"resp_schema_contract","usage":` + metered + `}}`
	success := `{"type":"response.completed","response":{"id":"resp_schema_contract","object":"response","model":"gpt-6-astra","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"values\":[\"first\",\"second\"],\"n\":9007199254740993}"}]}],"usage":` + metered + `}}`
	cases := []struct {
		name                          string
		frames                        []string
		code                          string
		want400, actualUsage, success bool
	}{
		{"bare_top_code_eof", []string{bare}, "invalid_json_schema", true, false, false},
		{"bare_nested_code_eof", []string{`{"type":"error","error":` + schemaObject + `}`}, "invalid_json_schema", true, false, false},
		{"bare_nested_schema_type_eof", []string{`{"type":"error","error":` + oauthContractWSSchemaError("", "invalid_json_schema") + `}`}, "", true, false, false},
		{"existing_invalid_request_type", []string{`{"type":"error","error":` + oauthContractWSSchemaError("invalid_function_parameters", "invalid_request_error") + `}`}, "invalid_function_parameters", true, false, false},
		{"authoritative_failed", []string{oauthContractWSSchemaFailed(schemaObject, "")}, "invalid_json_schema", true, false, false},
		{"bare_then_metered_failed", []string{bare, oauthContractWSSchemaFailed(schemaObject, metered)}, "invalid_json_schema", true, true, false},
		{"prior_actual_usage", []string{usageFrame, oauthContractWSSchemaFailed(schemaObject, "")}, "invalid_json_schema", true, true, false},
		{"tool_progress_and_usage", []string{`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_schema","call_id":"call_schema","name":"lookup","arguments":"{}"}}`, usageFrame, bare, oauthContractWSSchemaFailed(schemaObject, "")}, "invalid_json_schema", true, true, false},
		{"visible_text_and_usage", []string{`{"type":"response.output_text.delta","item_id":"msg_schema","delta":"synthetic partial"}`, usageFrame, bare, oauthContractWSSchemaFailed(schemaObject, "")}, "invalid_json_schema", true, true, false},
		{"unknown_bare_control", []string{`{"type":"error","code":"opaque_provider_failure","param":"text.format.schema","message":"Synthetic opaque rejection"}`}, "", false, false, false},
		{"quoted_schema_control", []string{`{"type":"error","code":"opaque_provider_failure","message":` + oauthContractWSSchemaQuote(oauthContractWSSchemaMessage) + `}`}, "", false, false, false},
		{"ambiguous_duplicate_code", []string{`{"type":"error","code":"invalid_json_schema","code":"opaque_provider_failure","param":"text.format.schema","message":"Synthetic ambiguous error"}`}, "", false, false, false},
		{"ambiguous_duplicate_code_reverse", []string{`{"type":"error","code":"opaque_provider_failure","code":"invalid_json_schema","param":"text.format.schema","message":"Synthetic ambiguous error"}`}, "", false, false, false},
		{"ambiguous_duplicate_type", []string{`{"type":"error","error":{"type":"invalid_json_schema","type":"opaque_provider_failure","param":"text.format.schema","message":"Synthetic ambiguous error"}}`}, "", false, false, false},
		{"ambiguous_duplicate_type_reverse", []string{`{"type":"error","error":{"type":"opaque_provider_failure","type":"invalid_json_schema","param":"text.format.schema","message":"Synthetic ambiguous error"}}`}, "", false, false, false},
		{"type_only_quoted_capacity", []string{oauthContractWSSchemaFailed(strings.Replace(oauthContractWSSchemaError("", "invalid_json_schema"), "synthetic_contract", "server is overloaded", 1), "")}, "", true, false, false},
		{"pending_then_authoritative_server_metered", []string{bare, oauthContractWSSchemaFailed(`{"type":"server_error","code":"server_error","message":"Synthetic server failure"}`, metered)}, "", false, true, false},
		{"prewarm_cyber_then_schema_metered", []string{bare, `{"type":"error","error":{"type":"rate_limit_error","code":"cyber_policy","message":"Synthetic superseded cyber signal"}}`, oauthContractWSSchemaFailed(schemaObject, metered)}, "invalid_json_schema", true, true, false},
		{"prewarm_done_with_failed_status", []string{bare, strings.Replace(oauthContractWSSchemaFailed(`{"type":"server_error","code":"server_error","message":"Synthetic server failure"}`, metered), "response.failed", "response.done", 1)}, "", false, true, false},
		{"prewarm_completed_with_error", []string{bare, strings.Replace(oauthContractWSSchemaFailed(`{"type":"server_error","code":"server_error","message":"Synthetic server failure"}`, metered), "response.failed", "response.completed", 1)}, "", false, true, false},
		{"real_capacity_control", []string{`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Server is overloaded"}}`}, "", false, true, false},
		{"successful_same_schema", []string{success}, "", false, true, true},
	}
	for _, prewarm := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, tc := range cases {
				if !prewarm && strings.HasPrefix(tc.name, "prewarm_") {
					continue
				}
				if prewarm && !tc.want400 && tc.name != "pending_then_authoritative_server_metered" && !strings.HasPrefix(tc.name, "prewarm_") {
					continue
				}
				t.Run(fmt.Sprintf("prewarm=%t/stream=%t/%s", prewarm, stream, tc.name), func(t *testing.T) {
					gin.SetMode(gin.TestMode)
					setupOpsErrorLogTestQueue(t, 8)
					cert := wsPartialSettlementTLS(t)
					var calls atomic.Int32
					requests := make(chan []byte, 8)
					upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != "/backend-api/codex/responses" {
							http.Error(w, "wrong path", 404)
							return
						}
						conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
						if err != nil {
							return
						}
						defer conn.Close()
						_, frame, err := conn.ReadMessage()
						if err != nil {
							return
						}
						requests <- append([]byte(nil), frame...)
						n := calls.Add(1)
						frames := tc.frames
						if n > 1 {
							frames = []string{success}
						}
						for _, frame := range frames {
							if conn.WriteMessage(websocket.TextMessage, []byte(frame)) != nil {
								return
							}
						}
						// Close immediately: this tests actual terminal/EOF authority,
						// never a sleep hoping a usage frame arrives later.
					}))
					upstream.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}
					upstream.StartTLS()
					t.Cleanup(upstream.Close)
					proxy, _ := wsPartialSettlementProxy(t, upstream.Listener.Addr().String())
					groupID := int64(9810)
					group := &service.Group{ID: groupID, Hydrated: true, Name: "synthetic-schema", Platform: service.PlatformOpenAI, Status: service.StatusActive, RateMultiplier: 1}
					accounts := make([]service.Account, 2)
					for i := range accounts {
						accounts[i] = service.Account{ID: 9811 + int64(i), Name: fmt.Sprintf("schema-synthetic-%d", i), Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 2, Priority: 1 + i*100, ProxyID: &proxy.ID, Proxy: proxy,
							Credentials:   map[string]any{"access_token": fmt.Sprintf("schema-synthetic-token-%d", i), "chatgpt_account_id": fmt.Sprintf("schema-synthetic-account-%d", i)},
							Extra:         map[string]any{"responses_websockets_v2_enabled": true, service.OpenAIAstraPolicyExtraKey: map[string]any{"model": "gpt-6-astra", "recent_samples": []any{map[string]any{"verdict": "healthy", "at": "2020-01-01T00:00:00Z"}}}},
							AccountGroups: []service.AccountGroup{{AccountID: 9811 + int64(i), GroupID: groupID}}}
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
					cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled = prewarm
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
					key := &service.APIKey{ID: 9813, UserID: 9814, Key: "synthetic-schema-client", GroupID: &groupID, Group: group, User: &service.User{ID: 9814, Status: service.StatusActive, Role: service.RoleUser, Balance: 100}}
					ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil)
					core, _ := observer.New(zap.ErrorLevel)
					router := gin.New()
					router.Use(OpsErrorLoggerMiddleware(ops))
					router.Use(func(c *gin.Context) {
						c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
						c.Set(string(middleware.ContextKeyAPIKey), key)
						c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 9814, Concurrency: 2})
						c.Next()
					})
					router.POST("/v1/responses", handler.Responses)
					body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":[{"type":"message","role":"user","content":"synthetic schema request"}],"text":{"format":{"type":"json_schema","name":"synthetic_contract","strict":true,"schema":%s}}}`, stream, oauthContractWSStrictSchema)
					request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
					ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
					defer cancel()
					request = request.WithContext(ctx)
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("User-Agent", "synthetic-schema-fixture")
					request.Header.Set("session-id", "synthetic-schema-session")
					recorder := httptest.NewRecorder()
					router.ServeHTTP(recorder, request)
					wantCalls := int32(1)
					if tc.name == "real_capacity_control" {
						wantCalls = 2
					}
					require.Equal(t, wantCalls, calls.Load(), "parameter errors are terminal; real capacity retains its bounded policy; response=%s", recorder.Body.String())
					require.Zero(t, tripwire.calls.Load(), "a rejected schema cannot be repaired by HTTP replay")
					var sent []byte
					select {
					case sent = <-requests:
					default:
						t.Fatal("actual upstream WS request was not captured")
					}
					require.Equal(t, "response.create", gjson.GetBytes(sent, "type").String())
					if prewarm {
						require.True(t, gjson.GetBytes(sent, "generate").Exists())
						require.False(t, gjson.GetBytes(sent, "generate").Bool())
					}
					require.True(t, gjson.GetBytes(sent, "text.format.strict").Bool())
					require.Equal(t, oauthContractWSSchemaValue(t, oauthContractWSStrictSchema), oauthContractWSSchemaValue(t, gjson.GetBytes(sent, "text.format.schema").Raw), "map key order may change, constraint values and large integers may not")
					response := recorder.Body.String()
					if prewarm && !tc.want400 && (tc.name == "pending_then_authoritative_server_metered" || strings.HasPrefix(tc.name, "prewarm_")) {
						require.Equal(t, http.StatusBadGateway, recorder.Code, "authoritative failed terminal cannot become success/400")
						if tc.name == "pending_then_authoritative_server_metered" {
							require.Equal(t, "server_error", gjson.Get(response, "error.code").String())
						}
					}
					if tc.want400 {
						if stream && !prewarm {
							require.Equal(t, http.StatusOK, recorder.Code, "already-streaming HTTP status is immutable")
							var failed []gjson.Result
							for _, line := range strings.Split(response, "\n") {
								if strings.HasPrefix(line, "data: ") {
									event := gjson.Parse(strings.TrimPrefix(line, "data: "))
									if event.Get("type").String() == "response.failed" {
										failed = append(failed, event)
									}
								}
							}
							require.Len(t, failed, 1, "one authoritative failure; no handler generic502 suffix: %s", response)
							require.Equal(t, "text.format.schema", failed[0].Get("response.error.param").String())
							if tc.code != "" {
								require.Equal(t, tc.code, failed[0].Get("response.error.code").String())
							}
							require.Contains(t, failed[0].Get("response.error.message").String(), "uniqueItems")
						} else {
							require.Equal(t, http.StatusBadRequest, recorder.Code, response)
							require.True(t, json.Valid(recorder.Body.Bytes()), "exactly one error JSON")
							require.Equal(t, "text.format.schema", gjson.Get(response, "error.param").String())
							if tc.code != "" {
								require.Equal(t, tc.code, gjson.Get(response, "error.code").String())
							}
							require.Contains(t, gjson.Get(response, "error.message").String(), "uniqueItems")
						}
						require.Equal(t, int64(1), OpsErrorLogQueueLength(), response)
						job := <-opsErrorLogQueue
						require.Equal(t, http.StatusBadRequest, job.entry.StatusCode, "logical final status")
						require.NotNil(t, job.entry.UpstreamErrorsJSON)
						events := gjson.Parse(*job.entry.UpstreamErrorsJSON).Array()
						require.Len(t, events, 1, "one upstream error attempt")
						require.Equal(t, int64(http.StatusBadRequest), events[0].Get("upstream_status_code").Int())
						require.Equal(t, int64(9811), events[0].Get("account_id").Int())
					} else if tc.success {
						require.Equal(t, http.StatusOK, recorder.Code)
						require.Contains(t, response, "9007199254740993")
						require.Zero(t, OpsErrorLogQueueLength())
					} else {
						require.NotEqual(t, http.StatusBadRequest, recorder.Code, "opaque/prose/duplicate errors cannot gain parameter classification")
						for len(opsErrorLogQueue) > 0 {
							job := <-opsErrorLogQueue
							require.NotEqual(t, http.StatusBadRequest, job.entry.StatusCode)
						}
					}
					billing.mu.Lock()
					commands := append([]*service.UsageBillingCommand(nil), billing.commands...)
					billing.mu.Unlock()
					if tc.actualUsage {
						require.Len(t, usage.created, 1, "actual usage is settled once even when rejection follows progress")
						row := <-usage.created
						require.Equal(t, 5, row.InputTokens+row.CacheReadTokens)
						require.Equal(t, 1, row.OutputTokens)
						if tc.name != "real_capacity_control" {
							require.Equal(t, int64(9811), row.AccountID)
						}
						require.Len(t, commands, 1)
					} else {
						require.Empty(t, usage.created, "unmetered schema failure must not invent usage")
						require.Empty(t, commands)
					}
				})
			}
		}
	}
}

func oauthContractWSSchemaQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func oauthContractWSSchemaValue(t *testing.T, raw string) any {
	t.Helper()
	require.True(t, json.Valid([]byte(raw)), "actual schema must remain JSON")
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	require.NoError(t, decoder.Decode(&value))
	return value
}
