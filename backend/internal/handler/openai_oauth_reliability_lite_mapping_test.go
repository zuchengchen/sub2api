//go:build unit

package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type oauthReliabilityLiteMappingUpstream struct {
	service.HTTPUpstream
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	model   string
}

func (u *oauthReliabilityLiteMappingUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	u.mu.Lock()
	u.bodies = append(u.bodies, body)
	u.headers = append(u.headers, req.Header.Clone())
	u.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: " + fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_lite_channel_mapped","object":"response","status":"completed","model":%q,"output":[{"type":"message","id":"msg_lite_channel_mapped","role":"assistant","status":"completed","content":[{"type":"output_text","text":"mapped tool retained"}]}],"usage":{"input_tokens":7,"output_tokens":2}}}`, u.model) + "\n\n")),
		Request:    req,
	}, nil
}

func (u *oauthReliabilityLiteMappingUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

const oauthReliabilityLiteMappingHeader = "X-OpenAI-Internal-Codex-Responses-Lite"
const oauthReliabilityLiteMappingMetadata = "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite"

type oauthReliabilityLiteMappingResult struct {
	context  *gin.Context
	recorder *httptest.ResponseRecorder
	upstream *oauthReliabilityLiteMappingUpstream
	usage    *openAIWSUsageHandlerUsageLogRepoStub
	logs     *observer.ObservedLogs
}

func (r oauthReliabilityLiteMappingResult) diagnostic() string {
	return fmt.Sprintf("body=%s upstream_calls=%d upstream_error=%s transport=%s transport_reason=%s logs=%v",
		r.recorder.Body.String(), len(r.upstream.bodies), r.context.GetString(service.OpsUpstreamErrorMessageKey),
		r.context.GetString("openai_ws_transport_decision"), r.context.GetString("openai_ws_transport_reason"), r.logs.AllUntimed())
}

func oauthReliabilityRunLiteChannelMappedHandler(t *testing.T, route, source, target string, stream, lite, hosted bool) oauthReliabilityLiteMappingResult {
	t.Helper()
	c, recorder := newOpenAIResponsesFailoverTestContext(t, context.Background())
	key, ok := middleware2.GetAPIKeyFromContext(c)
	require.True(t, ok)
	key.UserID = key.User.ID
	key.Group.AllowMessagesDispatch = true
	mapping := map[string]string{}
	if target != "" {
		mapping[source] = target
	}
	channelSvc := service.NewChannelService(&openAIWSUsageHandlerChannelRepoStub{
		channels: []service.Channel{{
			ID: 7702, Name: "synthetic-lite-channel", Status: service.StatusActive,
			GroupIDs:     []int64{*key.GroupID},
			ModelMapping: map[string]map[string]string{service.PlatformOpenAI: mapping},
		}},
		groupPlatforms: map[int64]string{*key.GroupID: service.PlatformOpenAI},
	}, nil, nil, nil, nil)
	account := service.Account{
		ID: 1, Name: "synthetic-lite-oauth", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{"access_token": "synthetic-token"},
		Extra:       astraHandlerExtra("healthy"),
	}
	account.Extra["openai_passthrough"] = route == "passthrough"
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	// This fixture owns only a synthetic HTTP transport. Keep all WS
	// selection disabled even if constructor defaults change later.
	cfg.Gateway.OpenAIWS.ForceHTTP = true
	decision := service.NewOpenAIWSProtocolResolver(cfg).Resolve(&account)
	require.Equal(t, service.OpenAIUpstreamTransportHTTPSSE, decision.Transport)
	require.Equal(t, "global_force_http", decision.Reason)
	upstream := &oauthReliabilityLiteMappingUpstream{model: target}
	usage := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 1)}
	billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	// Include model-availability diagnosis: a persistently unavailable mapped
	// model must exercise the normal public error path, not a nil embedded stub.
	accountRepo := openAIImagesFailoverAccountRepo{accounts: []service.Account{account}}
	svc := service.NewOpenAIGatewayService(accountRepo, usage,
		nil, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil,
		billingCache, upstream, &service.DeferredService{}, nil, nil, nil, channelSvc, nil, nil, nil)
	svc.DisableOpenAITiboRouteForTest()
	svc.StopOpenAICodexTicketHarvester()
	t.Cleanup(svc.CloseOpenAIWSPool)
	h := NewOpenAIGatewayHandler(svc, service.NewConcurrencyService(nil), billingCache,
		&service.APIKeyService{}, nil, nil, nil, nil, cfg)
	path := "/v1/responses"
	tool := `{"type":"function","name":"synthetic_function","parameters":{"type":"object","properties":{},"additionalProperties":false}}`
	if hosted {
		tool = `{"type":"web_search","external_web_access":false}`
	}
	fields := `"input":"retain declared tool","tools":[` + tool + `]`
	switch route {
	case "chat":
		path = "/v1/chat/completions"
		tool = `{"type":"function","function":{"name":"synthetic_function","parameters":{"type":"object","properties":{},"additionalProperties":false}}}`
		if hosted {
			tool = `{"type":"web_search"}`
		}
		fields = `"messages":[{"role":"user","content":"retain declared tool"}],"tools":[` + tool + `]`
	case "messages":
		path = "/v1/messages"
		tool = `{"name":"synthetic_function","input_schema":{"type":"object","properties":{},"additionalProperties":false}}`
		if hosted {
			tool = `{"type":"web_search_20250305","name":"web_search"}`
		}
		fields = `"max_tokens":32,"messages":[{"role":"user","content":"retain declared tool"}],"tools":[` + tool + `]`
	}
	if hosted && route != "messages" {
		fields += `,"tool_choice":"required"`
	}
	if lite && (route == "responses" || route == "passthrough") {
		fields += `,"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}`
	}
	body := fmt.Sprintf(`{"model":%q,"stream":%t,%s}`, source, stream, fields)
	logCore, observedLogs := observer.New(zap.DebugLevel)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(logCore)))
	c.Request.Header.Set("Content-Type", "application/json")
	if lite {
		c.Request.Header.Set(oauthReliabilityLiteMappingHeader, "true")
	}
	switch route {
	case "chat":
		h.ChatCompletions(c)
	case "messages":
		h.Messages(c)
	default:
		h.Responses(c)
	}
	wantIngressLite := ""
	if lite {
		wantIngressLite = "true"
	}
	require.Equal(t, wantIngressLite, c.GetHeader(oauthReliabilityLiteMappingHeader), "final transport compatibility must preserve the ingress capability")
	return oauthReliabilityLiteMappingResult{context: c, recorder: recorder, upstream: upstream, usage: usage, logs: observedLogs}
}

func TestChatCacheDiagnosticChannelMappingUsesOriginalModel(t *testing.T) {
	const target = "gpt-6.2-sol"
	for _, tc := range []struct {
		name, source, filter string
		wantSample, wantTail bool
	}{
		{"target_filter_rejects_alias", "public-sol", target, false, false},
		{"alias_filter_keeps_general_usage", "public-sol", "public-sol", true, false},
		{"eligible_models_mapped_to_each_other", "gpt-6.2-astra", "", true, true},
		{"matching_model", target, target, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cache.jsonl")
			t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_USER_ID", "100")
			t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_PATH", path)
			t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_UNTIL", time.Now().Add(time.Minute).UTC().Format(time.RFC3339))
			t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MAX_REQUESTS", "1")
			t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MODEL", tc.filter)
			r := oauthReliabilityRunLiteChannelMappedHandler(t, "chat", tc.source, target, false, false, false)
			require.Equal(t, http.StatusOK, r.recorder.Code, r.diagnostic())
			require.Len(t, r.upstream.bodies, 1)
			require.Equal(t, target, gjson.GetBytes(r.upstream.bodies[0], "model").String())
			raw, err := os.ReadFile(path)
			if !tc.wantSample {
				require.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			require.NoError(t, err)
			events := map[string]gjson.Result{}
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				event := gjson.Parse(line)
				events[event.Get("stage").String()] = event
			}
			require.True(t, events["ingress"].Exists())
			require.True(t, events["upstream_response"].Exists())
			require.Equal(t, tc.wantTail, events["ingress"].Get("assistant_tail").Exists())
			require.Equal(t, tc.source == target, events["chat_delivery"].Exists())
			require.NotContains(t, string(raw), "retain declared tool")
		})
	}
}

// OAuth GPT-5.5 is already retired at account selection. A channel mapping must
// not bypass that policy through Messages. Direct-service GPT-5.5 tests retain
// coverage of the established non-Lite transport compatibility independently.
func TestOAuthReliabilityFinalLiteChannelMappedGPT55HandlerContract(t *testing.T) {
	for _, route := range []string{"responses", "passthrough", "chat", "messages"} {
		for _, mapped := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/mapped=%t/stream=%t", route, mapped, stream), func(t *testing.T) {
					target := ""
					if mapped {
						target = "gpt-5.5"
					}
					r := oauthReliabilityRunLiteChannelMappedHandler(t, route, "gpt-6-astra", target, stream, true, true)
					r.upstream.mu.Lock()
					defer r.upstream.mu.Unlock()
					if mapped {
						require.Equal(t, http.StatusNotFound, r.recorder.Code, r.diagnostic())
						require.Equal(t, "model_not_found", gjson.Get(r.recorder.Body.String(), "error.type").String())
						require.Contains(t, r.recorder.Body.String(), "2026-10-14")
					} else {
						require.Equal(t, http.StatusBadRequest, r.recorder.Code, r.diagnostic())
						require.Equal(t, "invalid_request_error", gjson.Get(r.recorder.Body.String(), "error.type").String())
						require.Equal(t, "unsupported_parameter", gjson.Get(r.recorder.Body.String(), "error.code").String())
						require.Equal(t, "tools[0].type", gjson.Get(r.recorder.Body.String(), "error.param").String())
					}
					require.Empty(t, r.upstream.bodies, "retired targets and invalid Lite tools must be rejected before upstream")
					require.Empty(t, r.usage.created)
				})
			}
		}
	}
}

// A supported native target still forwards through the public handler, using
// the final model and transport capability without deleting explicit tools.
func TestOAuthReliabilityFinalLiteChannelMappedNativeHandlerContract(t *testing.T) {
	for _, route := range []string{"responses", "passthrough", "chat", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				name   string
				source string
				lite   bool
				hosted bool
			}{
				{name: "lite_function", lite: true},
				{name: "lite_hosted", lite: true, hosted: true},
				{name: "non_lite_hosted", hosted: true},
				{name: "retired_public_name_replaced_with_native_function", source: "gpt-5.5", lite: true},
				{name: "retired_public_name_does_not_disable_final_lite", source: "gpt-5.5", lite: true, hosted: true},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", route, stream, tc.name), func(t *testing.T) {
					source := tc.source
					if source == "" {
						source = "gpt-6-astra"
					}
					r := oauthReliabilityRunLiteChannelMappedHandler(t, route, source, "gpt-5.6-sol", stream, tc.lite, tc.hosted)
					r.upstream.mu.Lock()
					defer r.upstream.mu.Unlock()
					if tc.lite && tc.hosted {
						require.Equal(t, http.StatusBadRequest, r.recorder.Code, r.diagnostic())
						require.Equal(t, "unsupported_parameter", gjson.Get(r.recorder.Body.String(), "error.code").String())
						require.Equal(t, "tools[0].type", gjson.Get(r.recorder.Body.String(), "error.param").String())
						require.Empty(t, r.upstream.bodies)
						require.Empty(t, r.usage.created)
						return
					}
					require.Equal(t, http.StatusOK, r.recorder.Code, r.diagnostic())
					require.Contains(t, r.recorder.Body.String(), "mapped tool retained")
					require.Len(t, r.upstream.bodies, 1)
					wire := r.upstream.bodies[0]
					require.Equal(t, "gpt-5.6-sol", gjson.GetBytes(wire, "model").String())
					wantLite := ""
					if tc.lite {
						wantLite = "true"
					}
					require.Equal(t, wantLite, r.upstream.headers[0].Get(oauthReliabilityLiteMappingHeader))
					require.False(t, gjson.GetBytes(wire, oauthReliabilityLiteMappingMetadata).Exists(), "HTTP projection consumes only the WS transport marker")
					wantTool := "function"
					if tc.hosted {
						wantTool = "web_search"
					}
					require.Equal(t, wantTool, gjson.GetBytes(wire, "tools.0.type").String())
					if tc.hosted && (route == "responses" || route == "passthrough") {
						external := gjson.GetBytes(wire, "tools.0.external_web_access")
						require.True(t, external.Exists())
						require.False(t, external.Bool(), "declared hosted-tool options must survive final mapping")
					}
					require.Len(t, r.usage.created, 1)
					usage := <-r.usage.created
					require.Equal(t, 7, usage.InputTokens+usage.CacheReadTokens)
					require.Equal(t, 2, usage.OutputTokens)
				})
			}
		}
	}
}
