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

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type oauthReliabilityMessagesMappingUpstream struct {
	service.HTTPUpstream
	mu     sync.Mutex
	bodies [][]byte
	paths  []string
}

func (u *oauthReliabilityMessagesMappingUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	u.bodies = append(u.bodies, append([]byte(nil), body...))
	u.paths = append(u.paths, req.URL.Path)
	u.mu.Unlock()
	model := gjson.GetBytes(body, "model").String()
	stream := gjson.GetBytes(body, "stream").Bool()
	contentType := "application/json"
	var response string
	switch req.URL.Path {
	case "/v1/responses":
		contentType = "text/event-stream"
		response = "data: " + fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_mapping_contract","object":"response","status":"completed","model":%q,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"mapping contract answer"}]}],"usage":{"input_tokens":7,"output_tokens":2}}}`, model) + "\n\n"
	case "/v1/chat/completions":
		if stream {
			contentType = "text/event-stream"
			response = "data: " + fmt.Sprintf(`{"id":"chatcmpl_mapping_contract","object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{"role":"assistant","content":"mapping contract answer"},"finish_reason":null}]}`, model) + "\n\n" +
				"data: " + fmt.Sprintf(`{"id":"chatcmpl_mapping_contract","object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`, model) + "\n\ndata: [DONE]\n\n"
		} else {
			response = fmt.Sprintf(`{"id":"chatcmpl_mapping_contract","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"mapping contract answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`, model)
		}
	case "/v1/messages":
		if stream {
			contentType = "text/event-stream"
			response = "event: message_start\ndata: " + fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_mapping_contract","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"usage":{"input_tokens":7,"output_tokens":0}}}`, model) + "\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"mapping contract answer\"}}\n\n" +
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		} else {
			response = fmt.Sprintf(`{"id":"msg_mapping_contract","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"mapping contract answer"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":2}}`, model)
		}
	default:
		return nil, fmt.Errorf("unexpected synthetic upstream path %q", req.URL.Path)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}, "X-Request-Id": {"req_mapping_contract"}}, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
}

func (u *oauthReliabilityMessagesMappingUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

func TestOAuthReliabilityContractMessagesMappedEffortEligibility(t *testing.T) {
	const source = "gpt-5.6-sol"
	const channelTarget = "gpt-5.6-sol-high"
	for _, mode := range []string{"responses", "chat", "native_anthropic", "responses_wrong_mapping", "chat_wrong_mapping"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", mode, stream), func(t *testing.T) {
				c, recorder := newOpenAIResponsesFailoverTestContext(t, context.Background())
				key, ok := middleware2.GetAPIKeyFromContext(c)
				require.True(t, ok)
				key.Group.AllowMessagesDispatch = true
				platform := service.PlatformOpenAI
				mappedModel, expectedPath := source, "/v1/responses"
				mapping := map[string]any{source: source}
				extra := map[string]any{openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceResponses)}
				credentials := map[string]any{"api_key": "synthetic-token", "base_url": "https://mapping.example", "model_mapping": mapping}
				if strings.HasPrefix(mode, "chat") {
					extra[openai_compat.ExtraKeyResponsesMode] = string(openai_compat.ResponsesSupportModeForceChatCompletions)
					expectedPath = "/v1/chat/completions"
				}
				if mode == "native_anthropic" {
					// Native Anthropic does not split GPT-looking aliases. Its raw
					// channel target is the explicit key for this provider mapping.
					platform, mappedModel, expectedPath = service.PlatformKimi, "k3", "/v1/messages"
					credentials["api_protocol"] = service.APIProtocolAnthropic
					credentials["model_mapping"] = map[string]any{channelTarget: mappedModel}
				}
				wrongMapping := strings.HasSuffix(mode, "wrong_mapping")
				if wrongMapping {
					// Conversion actually uses the base name. A mapping that only
					// advertises the unsplit spelling cannot authorize that model.
					credentials["model_mapping"] = map[string]any{channelTarget: "unrelated-model"}
				}
				key.Group.Platform = platform
				account := service.Account{ID: 1, Name: "synthetic-messages-mapping", Platform: platform, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Credentials: credentials, Extra: extra}
				channelSvc := service.NewChannelService(&openAIWSUsageHandlerChannelRepoStub{
					channels:       []service.Channel{{ID: 7713, Name: "synthetic-messages-channel", Status: service.StatusActive, GroupIDs: []int64{*key.GroupID}, ModelMapping: map[string]map[string]string{platform: {source: channelTarget}}}},
					groupPlatforms: map[int64]string{*key.GroupID: platform},
				}, nil, nil, nil, nil)
				cfg := &config.Config{RunMode: config.RunModeSimple}
				cfg.Default.RateMultiplier = 1
				cfg.Gateway.OpenAIWS.ForceHTTP = true
				cfg.Security.URLAllowlist.Enabled = false
				upstream := &oauthReliabilityMessagesMappingUpstream{}
				usage := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 2)}
				billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
				t.Cleanup(billingCache.Stop)
				svc := service.NewOpenAIGatewayService(openAIImagesFailoverAccountRepo{accounts: []service.Account{account}}, usage,
					nil, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil,
					billingCache, upstream, &service.DeferredService{}, nil, nil, nil, channelSvc, nil, nil, nil)
				svc.DisableOpenAITiboRouteForTest()
				svc.StopOpenAICodexTicketHarvester()
				t.Cleanup(svc.CloseOpenAIWSPool)
				h := NewOpenAIGatewayHandler(svc, service.NewConcurrencyService(nil), billingCache, &service.APIKeyService{}, nil, nil, nil, nil, cfg)
				body := fmt.Sprintf(`{"model":%q,"stream":%t,"max_tokens":32,"messages":[{"role":"user","content":"retain model routing and intent"}]}`, source, stream)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Messages(c)
				upstream.mu.Lock()
				defer upstream.mu.Unlock()
				if wrongMapping {
					require.Equal(t, http.StatusNotFound, recorder.Code, recorder.Body.String())
					require.Equal(t, "model_not_found", gjson.Get(recorder.Body.String(), "error.type").String())
					require.Empty(t, upstream.bodies, "a raw alias match cannot bypass actual normalized-model eligibility")
					require.Empty(t, usage.created)
					return
				}
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.Contains(t, recorder.Body.String(), "mapping contract answer")
				require.Equal(t, []string{expectedPath}, upstream.paths)
				require.Len(t, upstream.bodies, 1)
				wire := upstream.bodies[0]
				require.Equal(t, mappedModel, gjson.GetBytes(wire, "model").String())
				if mode == "native_anthropic" {
					require.False(t, gjson.GetBytes(wire, "output_config").Exists(), "native aliases must not create an effort directive")
					require.JSONEq(t, gjson.Get(body, "messages").Raw, gjson.GetBytes(wire, "messages").Raw)
				} else {
					effortPath := "reasoning.effort"
					if mode == "chat" {
						effortPath = "reasoning_effort"
					}
					require.Equal(t, "high", gjson.GetBytes(wire, effortPath).String(), "normalize only the routing identity; retain configured effort")
				}
				require.Len(t, usage.created, 1)
				log := <-usage.created
				require.Equal(t, source, log.RequestedModel)
				require.Equal(t, channelTarget, log.Model, "keep the existing channel-mapped Messages response identity")
				require.NotNil(t, log.UpstreamModel)
				require.Equal(t, mappedModel, *log.UpstreamModel)
				require.NotNil(t, log.ModelMappingChain)
				require.Equal(t, source+"→"+channelTarget+"→"+mappedModel, *log.ModelMappingChain)
				require.Equal(t, 7, log.InputTokens+log.CacheReadTokens)
				require.Equal(t, 2, log.OutputTokens)
			})
		}
	}
}
