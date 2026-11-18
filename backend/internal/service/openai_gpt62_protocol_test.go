//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestGPT62AliasesAndReasoningPreserveIdentity(t *testing.T) {
	for _, model := range []string{"gpt-6.2-astra", "gpt-6.2-sol"} {
		for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
			alias := "openai/" + model + "-" + effort
			require.Equal(t, model, normalizeCodexModel(alias))
			require.Equal(t, model, normalizeKnownOpenAICodexModel(alias))
			require.Equal(t, model, normalizeOpenAIModelForUpstream(&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, alias))
			require.True(t, shouldAutoInjectPromptCacheKeyForCompat(alias))
			require.Equal(t, effort, normalizeOpenAIReasoningEffortForModel(effort, model))
			request := &apicompat.AnthropicRequest{Model: alias}
			applyOpenAICompatModelNormalization(request)
			require.Equal(t, model, request.Model)
			require.Equal(t, effort, request.OutputConfig.Effort)
			converted, err := apicompat.AnthropicToResponses(request)
			require.NoError(t, err)
			require.Equal(t, model, converted.Model)
			require.Equal(t, effort, converted.Reasoning.Effort)
		}
		body := []byte(`{"reasoning":{"mode":"pro","effort":"max","context":"all_turns"},"temperature":0.7,"top_p":0.9}`)
		normalized, changed, err := normalizeOpenAIResponsesReasoningMode(body, model)
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, "pro", gjson.GetBytes(normalized, "reasoning.mode").String())
		require.Equal(t, "max", gjson.GetBytes(normalized, "reasoning.effort").String())
		require.Equal(t, "all_turns", gjson.GetBytes(normalized, "reasoning.context").String())
		require.False(t, gjson.GetBytes(normalized, "temperature").Exists())
		require.False(t, gjson.GetBytes(normalized, "top_p").Exists())
	}
	for _, model := range []string{"gpt-6.2-astra-preview", "gpt-6.2-sol-preview", "gpt-6.2-solitude"} {
		require.Equal(t, model, normalizeCodexModel(model))
		require.False(t, isOpenAIGPT6Model(model))
	}
}

func TestGPT62CodexCatalogUsesOfficialMetadata(t *testing.T) {
	for _, tc := range []struct {
		model   string
		context int64
	}{{"gpt-6.2-astra", 272000}, {"gpt-6.2-sol", 500000}} {
		body, err := BuildCodexModelsManifest([]string{tc.model})
		require.NoError(t, err)
		models := decodeCodexManifestModels(t, body)
		require.Len(t, models, 1)
		m := models[0]
		require.Equal(t, tc.model, m["slug"])
		require.Equal(t, "low", m["default_reasoning_level"])
		require.EqualValues(t, tc.context, m["context_window"])
		require.EqualValues(t, 872000, m["max_context_window"])
		require.Equal(t, []string{"low", "medium", "high", "xhigh", "max", "ultra"}, effortsFromManifestModel(t, m))
		require.Equal(t, "xhigh", m["multi_agent_reasoning_effort"])
		require.Equal(t, "v2", m["multi_agent_version"])
		require.Equal(t, true, m["use_responses_lite"])
		for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
			account := &Account{Platform: PlatformOpenAI, Type: accountType, Credentials: map[string]any{"base_url": "https://api.openai.com"}}
			caps := accountCodexToolCapabilities(account, tc.model)
			require.Equal(t, accountType == AccountTypeAPIKey, bytes.Contains(caps["service_tiers"], []byte("ultrafast")))
			if accountType == AccountTypeAPIKey {
				completed, err := completeAPIKeyCodexModelsManifestMetadata([]byte(fmt.Sprintf(`{"models":[{"slug":%q}]}`, tc.model)), true, account)
				require.NoError(t, err)
				require.False(t, gjson.GetBytes(completed, "models.0.use_responses_lite").Bool())
			}
		}
		account := newCodexModelsAPIKeyTestAccount("https://models.example/v1")
		native := []byte(fmt.Sprintf(`{"models":[{"slug":%q,"supported_reasoning_levels":[{"effort":"high"}],"context_window":100000,"service_tiers":[]}]}`, tc.model))
		completed, err := completeAPIKeyCodexModelsManifestMetadata(native, true, account)
		require.NoError(t, err)
		require.EqualValues(t, 100000, gjson.GetBytes(completed, "models.0.context_window").Int())
		require.Equal(t, "high", gjson.GetBytes(completed, "models.0.supported_reasoning_levels.0.effort").String())
		require.Equal(t, "[]", gjson.GetBytes(completed, "models.0.service_tiers").Raw)
	}
}

func TestGPT62ForwardAcrossHTTPEntrypoints(t *testing.T) {
	for _, model := range []string{"gpt-6.2-astra", "gpt-6.2-sol"} {
		for _, route := range []string{"responses", "chat", "messages"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", model, route, stream), func(t *testing.T) {
					body := oauthReliabilityContractBody(route, stream)
					body, err := sjson.SetBytes(body, "model", model)
					require.NoError(t, err)
					path := "reasoning.effort"
					if route == "chat" {
						path = "reasoning_effort"
					}
					if route == "messages" {
						path = "output_config.effort"
					}
					body, err = sjson.SetBytes(body, path, "max")
					require.NoError(t, err)
					original := bytes.Clone(body)
					upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_gpt62_fixture", model)}
					svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream, tiboRouteDisabled: true}
					c, recorder := oauthReliabilityContractContext("/v1/"+route, body)
					result, err := oauthReliabilityContractForward(svc, c, newOpenAIOAuthNamespaceTestAccount(), route, body)
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, http.StatusOK, recorder.Code)
					require.Len(t, upstream.bodies, 1)
					require.Equal(t, model, gjson.GetBytes(upstream.lastBody, "model").String())
					require.Equal(t, "max", gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
					require.Equal(t, original, body)
				})
			}
		}
	}
}

func TestGPT62APIKeyAliasesDisableResponsesLite(t *testing.T) {
	for _, target := range []string{"gpt-6.2-astra-max", "openai/gpt-6.2-sol", "GPT_6.2_ASTRA"} {
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
			"base_url": "https://api.openai.com", "model_mapping": map[string]any{"public-gpt62": target},
		}}
		account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Models: map[string]UpstreamModelMetadata{
			target: {ID: target, CodexToolCapabilities: map[string]json.RawMessage{"use_responses_lite": json.RawMessage("true")}},
		}})
		caps := accountCodexToolCapabilities(account, target)
		require.Equal(t, "false", string(caps["use_responses_lite"]))
		body, err := adjustAPIKeyCodexModelsManifest([]byte(`{"models":[{"slug":"public-gpt62","use_responses_lite":true}]}`), account)
		require.NoError(t, err)
		require.False(t, gjson.GetBytes(body, "models.0.use_responses_lite").Bool())
	}
}

func TestGPT62APIKeyChatToolsUseResponses(t *testing.T) {
	for _, model := range []string{"gpt-6.2-astra", "gpt-6.2-sol"} {
		body := []byte(fmt.Sprintf(`{"model":%q,"stream":false,"reasoning_effort":"max","messages":[{"role":"user","content":"Read fixture"},{"role":"assistant","tool_calls":[{"id":"call_gpt62","type":"function","function":{"name":"read","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_gpt62","content":"fixture content"}],"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object","properties":{}}}}]}`, model))
		upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_gpt62_apikey", model)}
		svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
		c, recorder := oauthReliabilityContractContext("/v1/chat/completions", body)
		result, err := svc.ForwardAsChatCompletions(c.Request.Context(), c, rawGPT56ResponsesAPIKeyAccount(model, model), body, "", "")
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, http.StatusOK, recorder.Code)
		require.Equal(t, "/v1/responses", upstream.lastReq.URL.Path)
		require.Equal(t, model, gjson.GetBytes(upstream.lastBody, "model").String())
		require.Equal(t, "max", gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
		require.Equal(t, "read", gjson.GetBytes(upstream.lastBody, "tools.0.name").String())
		require.Equal(t, "call_gpt62", gjson.GetBytes(upstream.lastBody, `input.#(type=="function_call").call_id`).String())
		require.Equal(t, "call_gpt62", gjson.GetBytes(upstream.lastBody, `input.#(type=="function_call_output").call_id`).String())
	}
}

func TestGPT62FinalReasoningHonorsConfiguredMapping(t *testing.T) {
	for _, model := range []string{"gpt-6.2-astra", "gpt-6.2-sol"} {
		for _, mapped := range []bool{false, true} {
			body := []byte(fmt.Sprintf(`{"model":%q,"stream":false,"max_tokens":32,"messages":[{"role":"user","content":"fixture"}],"thinking":{"type":"disabled"}}`, model))
			ctx := WithRequestedReasoningEffort(context.Background(), "none")
			if mapped {
				ctx = WithOpenAIReasoningEffortPolicy(ctx, "", []ReasoningEffortMapping{{From: "none", To: "low", MatchType: "exact", Model: model}}, "")
			}
			c, recorder := oauthReliabilityContractContext("/v1/messages", body)
			c.Request = c.Request.WithContext(ctx)
			upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_gpt62_mapping", model)}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream, tiboRouteDisabled: true}
			_, err := svc.ForwardAsAnthropic(ctx, c, newOpenAIOAuthNamespaceTestAccount(), body, "", "")
			if mapped {
				require.NoError(t, err)
				require.Equal(t, "low", gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
			} else {
				require.ErrorContains(t, err, "does not support reasoning effort")
				require.Equal(t, http.StatusBadRequest, recorder.Code)
				require.Empty(t, upstream.bodies)
				var response map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
			}
		}
	}
}
