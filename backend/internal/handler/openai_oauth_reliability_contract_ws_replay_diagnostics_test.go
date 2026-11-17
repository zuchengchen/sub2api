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
	"github.com/Wei-Shaw/sub2api/internal/model"
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

type oauthWSReplayDiagnosticRuleRepo struct {
	service.ErrorPassthroughRepository
	rules []*model.ErrorPassthroughRule
}

func (r *oauthWSReplayDiagnosticRuleRepo) List(context.Context) ([]*model.ErrorPassthroughRule, error) {
	return r.rules, nil
}

// The public HTTP handler, real TLS WS transport, account proxy, Ops queue and
// repository boundary are exercised at ERROR log level. These assertions use
// only APIs present at czc-v2026.11.17.7, so this file alone is a runnable red
// baseline. No customer's request, connection or credentials are replayed.
func TestOAuthReliabilityContractWSHTTPIncompleteReplayDiagnostic(t *testing.T) {
	const zeroUsage = `{"input_tokens":0,"output_tokens":0}`
	cases := []struct {
		name, output, usage, source, ending string
		stream, clientOutput, terminal      bool
		firstZero, skipCurrent, newRule     bool
		noDiagnostic                        bool
	}{
		{name: "image_synthetic_terminal_is_not_upstream", output: "image", usage: oauthWSReaderUsage, source: "upstream", stream: true, clientOutput: true},
		{name: "text_usage_absent", output: "text", source: "absent", stream: true, clientOutput: true},
		{name: "buffered_tool_progress_without_client_output", output: "tool", usage: oauthWSReaderUsage, source: "upstream"},
		{name: "usage_progress_without_client_output", usage: oauthWSReaderUsage, source: "upstream", stream: true},
		{name: "parsed_zero_usage", output: "image", usage: zeroUsage, source: "upstream", stream: true, clientOutput: true},
		{name: "parsed_empty_usage_object", output: "image", usage: `{}`, source: "upstream", stream: true, clientOutput: true},
		{name: "null_usage_is_absent", output: "image", usage: `null`, source: "absent", stream: true, clientOutput: true},
		{name: "nonobject_usage_is_absent", output: "image", usage: `[]`, source: "absent", stream: true, clientOutput: true},
		{name: "actual_usage_then_zero_retains_metering", output: "image", usage: oauthWSReaderUsage, source: "upstream", ending: "usage_then_zero", stream: true, clientOutput: true},
		{name: "actual_terminal_missing_response_payload", output: "tool", usage: oauthWSReaderUsage, source: "upstream", ending: "missing_payload", terminal: true},
		{name: "second_attempt_does_not_inherit_zero_usage_observation", output: "image", source: "absent", stream: true, clientOutput: true, firstZero: true},
		{name: "existing_skip_monitoring_is_preserved", output: "image", usage: oauthWSReaderUsage, source: "upstream", stream: true, clientOutput: true, skipCurrent: true},
		{name: "diagnostic_does_not_apply_new_502_rule", output: "image", usage: oauthWSReaderUsage, source: "upstream", stream: true, clientOutput: true, newRule: true},
		{name: "terminal_short_write_keeps_prewrite_snapshot", output: "image", usage: oauthWSReaderUsage, source: "upstream", ending: "terminal_short_write", stream: true, clientOutput: true},
		{name: "completed_does_not_invent_read_diagnostic", output: "image", usage: oauthWSReaderUsage, ending: "completed", stream: true, noDiagnostic: true},
		{name: "graceful_close_keeps_existing_contract", output: "image", usage: oauthWSReaderUsage, ending: "graceful", stream: true, noDiagnostic: true},
		{name: "cancellation_does_not_invent_read_diagnostic", output: "image", usage: oauthWSReaderUsage, ending: "cancel", stream: true, noDiagnostic: true},
		{name: "first_semantic_write_failure_does_not_invent_read_diagnostic", output: "image", usage: oauthWSReaderUsage, ending: "write_failure", stream: true, noDiagnostic: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			setupOpsErrorLogTestQueue(t, 8)
			cert := wsPartialSettlementTLS(t)
			var calls atomic.Int32
			sent := make(chan []byte, 4)
			owners := make(chan string, 4)
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/backend-api/codex/responses" {
					http.Error(w, "unexpected synthetic endpoint", http.StatusNotFound)
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
				n := calls.Add(1)
				sent <- append([]byte(nil), request...)
				owners <- r.Header.Get("Authorization")
				write := func(frame string) bool { return conn.WriteMessage(websocket.TextMessage, []byte(frame)) == nil }
				if tc.firstZero && n == 1 {
					write(`{"type":"response.in_progress","response":{"id":"resp_diagnostic","usage":` + zeroUsage + `}}`)
					return // Safe old retry; its parsed-zero observation is not the next reader's.
				}
				if tc.ending != "missing_payload" {
					if !write(`{"type":"response.created","response":{"id":"resp_diagnostic","object":"response","model":"gpt-6-astra","status":"in_progress"}}`) {
						return
					}
					if tc.usage != "" && !write(`{"type":"response.in_progress","response":{"id":"resp_diagnostic","usage":`+tc.usage+`}}`) {
						return
					}
				}
				switch tc.output {
				case "image":
					if !write(`{"type":"response.output_item.done","output_index":0,"item":` + oauthWSReaderImage + `}`) {
						return
					}
				case "text":
					if !write(`{"type":"response.output_text.delta","item_id":"msg_diagnostic","output_index":0,"delta":"synthetic diagnostic text"}`) {
						return
					}
				case "tool":
					if !write(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_diagnostic","call_id":"call_diagnostic","name":"lookup","arguments":"{}"}}`) {
						return
					}
				}
				switch tc.ending {
				case "usage_then_zero":
					write(`{"type":"response.in_progress","response":{"id":"resp_diagnostic","usage":` + zeroUsage + `}}`)
				case "missing_payload":
					// A real terminal was read, but there is no response object to
					// return to a buffered client. This is different from no terminal.
					write(`{"type":"response.failed","usage":` + oauthWSReaderUsage + `}`)
				case "completed":
					write(`{"type":"response.completed","response":{"id":"resp_diagnostic","object":"response","model":"gpt-6-astra","status":"completed","output":[` + oauthWSReaderImage + `],"usage":` + oauthWSReaderUsage + `}}`)
				case "graceful":
					_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "synthetic restart"), time.Now().Add(time.Second))
				}
				// All other endings close the real socket without a WS Close frame.
			}))
			upstream.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}
			upstream.StartTLS()
			t.Cleanup(upstream.Close)
			proxy, connects := wsPartialSettlementProxy(t, upstream.Listener.Addr().String())
			proxy.Name = "synthetic-diagnostic-proxy"
			groupID := int64(9940)
			imagePrice := 0.25
			group := &service.Group{ID: groupID, Hydrated: true, Name: "synthetic-diagnostic", Platform: service.PlatformOpenAI, Status: service.StatusActive, RateMultiplier: 1, AllowImageGeneration: true, ImagePrice1K: &imagePrice}
			accounts := make([]service.Account, 2)
			for i := range accounts {
				accounts[i] = service.Account{ID: 9941 + int64(i), Name: fmt.Sprintf("diagnostic-synthetic-%d", i), Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 2, Priority: 1 + i*100, ProxyID: &proxy.ID, Proxy: proxy,
					Credentials:   map[string]any{"access_token": fmt.Sprintf("diagnostic-synthetic-token-%d", i), "chatgpt_account_id": fmt.Sprintf("diagnostic-synthetic-account-%d", i)},
					Extra:         map[string]any{"responses_websockets_v2_enabled": true, service.OpenAIAstraPolicyExtraKey: map[string]any{"model": "gpt-6-astra", "recent_samples": []any{map[string]any{"verdict": "healthy", "at": "2020-01-01T00:00:00Z"}}}},
					AccountGroups: []service.AccountGroup{{AccountID: 9941 + int64(i), GroupID: groupID}}}
			}
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
			svc := service.NewOpenAIGatewayService(&openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}, usage, billing, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, nil, tripwire, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
			svc.StopOpenAICodexTicketHarvester()
			t.Cleanup(svc.CloseOpenAIWSPool)
			var rules *service.ErrorPassthroughService
			if tc.newRule {
				rules = service.NewErrorPassthroughService(&oauthWSReplayDiagnosticRuleRepo{rules: []*model.ErrorPassthroughRule{{ID: 9945, Name: "synthetic all 502 hidden", Enabled: true, ErrorCodes: []int{502}, Platforms: []string{service.PlatformOpenAI}, MatchMode: model.MatchModeAny, PassthroughCode: true, PassthroughBody: true, SkipMonitoring: true}}}, nil)
				require.NotNil(t, rules.MatchRule(service.PlatformOpenAI, 502, []byte("EOF")), "negative control's rule is genuinely active")
			}
			h := NewOpenAIGatewayHandler(svc, service.NewConcurrencyService(&fakeConcurrencyCache{}), cache, &service.APIKeyService{}, nil, rules, nil, nil, cfg)
			key := &service.APIKey{ID: 9943, UserID: 9944, Key: "synthetic-diagnostic-client", GroupID: &groupID, Group: group, User: &service.User{ID: 9944, Status: service.StatusActive, Role: service.RoleUser, Balance: 100}}
			repo := &ingressRejectOpsRepo{}
			ops := service.NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil)
			core, _ := observer.New(zap.ErrorLevel)
			router := gin.New()
			router.Use(OpsErrorLoggerMiddleware(ops))
			router.Use(func(c *gin.Context) {
				c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
				c.Set(string(middleware.ContextKeyAPIKey), key)
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 9944, Concurrency: 2})
				c.Header("X-Request-Id", "diagnostic-boundary-request")
				if tc.skipCurrent {
					// This tests inheritance of an already decided request policy,
					// not a newly evaluated rule for the diagnostic event.
					c.Set(service.OpsSkipPassthroughKey, true)
				}
				c.Next()
			})
			var contextEvents []byte
			var committed bool
			router.POST("/v1/responses", func(c *gin.Context) {
				h.Responses(c)
				committed = service.IsResponseCommitted(c)
				events, _ := c.Get(service.OpsUpstreamErrorsKey)
				contextEvents, _ = json.Marshal(events)
			})
			body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":[{"type":"message","role":"user","content":"synthetic diagnostic request"}],"reasoning":{"effort":"high"}}`, tc.stream)
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
			defer cancel()
			request = request.WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("User-Agent", "synthetic-diagnostic-fixture")
			request.Header.Set("session-id", "synthetic-diagnostic-session")
			recorder := httptest.NewRecorder()
			writer := &oauthWSReaderBoundaryWriter{ResponseRecorder: recorder, failImage: tc.ending == "write_failure", failTerminal: tc.ending == "terminal_short_write"}
			if tc.ending == "cancel" {
				writer.cancel = cancel
			}
			router.ServeHTTP(writer, request)

			// Preserve .55 execution, wire and settlement before checking the new
			// observation. A red baseline must get past these existing contracts.
			wantCalls := int32(1)
			if tc.firstZero {
				wantCalls = 2
			}
			require.Equal(t, wantCalls, calls.Load())
			require.Equal(t, wantCalls, connects.Load())
			require.Zero(t, tripwire.calls.Load(), "no HTTP replay after progress")
			require.Len(t, sent, int(wantCalls))
			for len(sent) > 0 {
				frame := <-sent
				require.Equal(t, "response.create", gjson.GetBytes(frame, "type").String())
				require.Equal(t, "gpt-6-astra", gjson.GetBytes(frame, "model").String())
				require.False(t, gjson.GetBytes(frame, "generate").Exists())
			}
			require.Len(t, owners, int(wantCalls))
			for len(owners) > 0 {
				require.Equal(t, "Bearer diagnostic-synthetic-token-0", <-owners, "same selected credential")
			}
			require.Len(t, usage.created, 1)
			row := <-usage.created
			billing.mu.Lock()
			commands := append([]*service.UsageBillingCommand(nil), billing.commands...)
			billing.mu.Unlock()
			require.Len(t, commands, 1, "diagnostics do not estimate or bill an extra attempt")
			wantInput, wantOutput, wantCache := 0, 0, 0
			if tc.usage == oauthWSReaderUsage {
				wantInput, wantOutput, wantCache = 11, 3, 2
			}
			wantImages := 0
			if tc.output == "image" {
				wantImages = 1
			}
			require.Equal(t, int64(9941), row.AccountID)
			require.Equal(t, wantInput, row.InputTokens+row.CacheReadTokens)
			require.Equal(t, wantOutput, row.OutputTokens)
			require.Equal(t, wantCache, row.CacheReadTokens)
			require.Equal(t, wantImages, row.ImageCount)
			require.Equal(t, tc.stream, row.Stream)
			require.True(t, row.OpenAIWSMode)
			require.Equal(t, row.AccountID, commands[0].AccountID)
			require.Equal(t, row.RequestID, commands[0].RequestID)
			require.Equal(t, row.InputTokens, commands[0].InputTokens)
			require.Equal(t, row.OutputTokens, commands[0].OutputTokens)
			require.Equal(t, row.ImageCount, commands[0].ImageCount)
			if wantInput == 0 && wantImages == 0 {
				require.Zero(t, commands[0].BalanceCost, "missing usage is not newly estimated")
			} else {
				require.Positive(t, commands[0].BalanceCost)
			}
			wire := recorder.Body.String()
			var terminals []gjson.Result
			for _, line := range strings.Split(wire, "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				raw := strings.TrimPrefix(line, "data: ")
				if !gjson.Valid(raw) && tc.ending == "terminal_short_write" {
					require.True(t, strings.HasSuffix(wire, string(writer.partialTerminal)))
					continue
				}
				require.True(t, gjson.Valid(raw))
				event := gjson.Parse(raw)
				switch event.Get("type").String() {
				case "response.completed", "response.failed", "response.incomplete":
					terminals = append(terminals, event)
				}
			}
			switch {
			case tc.ending == "cancel" || tc.ending == "write_failure" || tc.ending == "terminal_short_write":
				require.True(t, writer.imageAttempted)
				require.Empty(t, terminals, "attempted partial bytes are not a delivered terminal")
				require.False(t, committed)
				if tc.ending == "terminal_short_write" {
					require.Equal(t, 1, writer.terminalAttempts)
					require.Less(t, len(writer.partialTerminal), writer.terminalFrameBytes)
				}
			case !tc.stream || tc.output == "":
				require.Equal(t, http.StatusBadGateway, recorder.Code)
				require.True(t, gjson.Valid(wire))
				require.Equal(t, "upstream_error", gjson.Get(wire, "error.type").String())
				if !tc.stream {
					require.Equal(t, "upstream websocket ended before completing the response", gjson.Get(wire, "error.message").String())
				} else {
					require.NotEmpty(t, gjson.Get(wire, "error.message").String())
				}
				require.Empty(t, terminals)
			default:
				require.Equal(t, http.StatusOK, recorder.Code)
				require.Len(t, terminals, 1)
				wantType := "response.failed"
				if tc.ending == "completed" {
					wantType = "response.completed"
				} else if tc.ending == "graceful" {
					wantType = "response.incomplete"
				}
				require.Equal(t, wantType, terminals[0].Get("type").String())
				if tc.output == "image" {
					require.Equal(t, "resp_diagnostic", terminals[0].Get("response.id").String())
				}
			}

			var jobs []*service.OpsInsertErrorLogInput
			for len(opsErrorLogQueue) > 0 {
				jobs = append(jobs, (<-opsErrorLogQueue).entry)
			}
			if tc.skipCurrent {
				require.Empty(t, jobs, "an observation cannot override existing skip_monitoring")
			} else if !tc.noDiagnostic {
				require.Len(t, jobs, 1, "exactly the pre-existing failure row, not a second diagnostic row")
				require.Equal(t, http.StatusBadGateway, jobs[0].StatusCode)
				require.Equal(t, "upstream", jobs[0].ErrorPhase)
				require.NotContains(t, jobs[0].ErrorMessage, "Recovered")
			} else {
				for _, job := range jobs {
					assert.NotEqual(t, http.StatusBadGateway, job.StatusCode, "no new failure for successful completion or client departure")
				}
			}
			findDiagnostic := func(raw string) []gjson.Result {
				var found []gjson.Result
				for _, event := range gjson.Parse(raw).Array() {
					if event.Get("kind").String() == "stream_read_diagnostic" {
						found = append(found, event)
					} else {
						require.False(t, event.Get("replay_boundary").Exists(), "no copying this observation onto other attempts")
					}
				}
				return found
			}
			require.Empty(t, findDiagnostic(string(contextEvents)), "diagnostics must not enter semantic attempt policy/classification state")
			if tc.noDiagnostic {
				for _, job := range jobs {
					if job.UpstreamErrorsJSON != nil {
						require.Empty(t, findDiagnostic(*job.UpstreamErrorsJSON))
					}
				}
				return
			}
			if tc.skipCurrent {
				return // Late serialization cannot create a row after monitoring was skipped.
			}
			require.NotNil(t, jobs[0].UpstreamErrorsJSON, "the existing failure row must contain its reader observation")
			queued := *jobs[0].UpstreamErrorsJSON
			diagnostics := findDiagnostic(queued)
			require.Len(t, diagnostics, 1, "the exact incomplete reader must expose its snapshot: %s", queued)
			diagnostic := diagnostics[0]
			require.Equal(t, int64(9941), diagnostic.Get("account_id").Int())
			require.Equal(t, proxy.ID, diagnostic.Get("proxy_id").Int())
			require.Equal(t, proxy.Name, diagnostic.Get("proxy_name").String())
			require.Equal(t, service.PlatformOpenAI, diagnostic.Get("platform").String())
			require.Equal(t, "ws_response", diagnostic.Get("stage").String())
			require.Equal(t, "incomplete_after_progress", diagnostic.Get("reason").String())
			require.NotEmpty(t, diagnostic.Get("message").String())
			require.LessOrEqual(t, len(diagnostic.Get("message").String()), 128)
			require.Zero(t, diagnostic.Get("upstream_status_code").Int(), "no fabricated HTTP status for a WS reader observation")
			for _, field := range []string{"detail", "upstream_response_body", "upstream_url", "upstream_request_id", "scope"} {
				require.Empty(t, diagnostic.Get(field).String(), field)
			}
			boundary := diagnostic.Get("replay_boundary")
			require.True(t, boundary.IsObject())
			require.Len(t, boundary.Map(), 5)
			for field, want := range map[string]bool{"upstream_progress_observed": true, "client_output_started": tc.clientOutput, "terminal_seen": tc.terminal, "retry_requested": false} {
				value := boundary.Get(field)
				require.True(t, value.Exists(), "observed false is distinct from unknown: %s", field)
				require.Contains(t, []gjson.Type{gjson.True, gjson.False}, value.Type)
				require.Equal(t, want, value.Bool(), field)
			}
			require.Equal(t, tc.source, boundary.Get("usage_source").String())
			for _, private := range []string{"diagnostic-synthetic-token", "synthetic diagnostic request", "synthetic diagnostic text", "c3ludGhldGljLWltYWdl", "fc_diagnostic", "call_diagnostic", "resp_diagnostic"} {
				require.NotContains(t, diagnostic.Raw, private, "fixed diagnostic fields contain no request/output content")
			}
			// Queue preparation and worker preparation are distinct boundaries;
			// the second pass must not turn the diagnostic into the final outcome.
			outcome := func(entry *service.OpsInsertErrorLogInput) []byte {
				copyOfEntry := *entry
				copyOfEntry.UpstreamErrors = nil
				copyOfEntry.UpstreamErrorsJSON = nil
				encoded, err := json.Marshal(copyOfEntry)
				require.NoError(t, err)
				return encoded
			}
			beforeOutcome := outcome(jobs[0])
			require.NoError(t, ops.RecordErrorBatch(context.Background(), jobs))
			require.Len(t, repo.entries, 1)
			require.Equal(t, string(beforeOutcome), string(outcome(repo.entries[0])), "classification, attribution and all existing row fields remain unchanged")
			require.NotNil(t, repo.entries[0].UpstreamErrorsJSON)
			require.Equal(t, queued, *repo.entries[0].UpstreamErrorsJSON, "repeated sanitization keeps exactly one snapshot and the existing serialized events")
			require.Len(t, findDiagnostic(*repo.entries[0].UpstreamErrorsJSON), 1)
		})
	}
}
