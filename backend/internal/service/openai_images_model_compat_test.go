package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIImageModelsKeepExplicitComplexityAndDefaults(t *testing.T) {
	for _, model := range []string{
		"gpt-image-1.5", "gpt-image-1-mini", "gpt-image-2", "gpt-image-2-2026-04-21",
		"gpt-image-2.5-flare", "gpt-image-2.5-sunburst",
		"gpt-image-2.5-flare-2026-09-08", "gpt-image-2.5-sunburst-2026-09-08",
		"gpt-image-2.6-flare", "gpt-image-2.6-sunburst",
	} {
		for _, complexity := range []string{"", "high"} {
			t.Run(model+"/"+complexity, func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"model":%q,"prompt":"draw","complexity":%q}`, model, complexity))
				c, _ := newOpenAIImagesTestContext(t, body)
				upstream := &httpUpstreamRecorder{resp: openAIImagesJSONResponse()}
				svc := newOpenAIImagesTestService(upstream)
				parsed, err := svc.ParseOpenAIImagesRequest(c, body)
				require.NoError(t, err)
				result, err := svc.ForwardImages(context.Background(), c, directImagesTestAccount(), body, parsed, "")
				require.NoError(t, err)
				require.Equal(t, "/backend-api/codex/images/generations", upstream.lastReq.URL.Path)
				require.Equal(t, model, result.UpstreamModel)
				require.Equal(t, model, gjson.GetBytes(upstream.lastBody, "model").String())
				require.Equal(t, complexity != "", gjson.GetBytes(upstream.lastBody, "complexity").Exists())
				require.Equal(t, complexity, gjson.GetBytes(upstream.lastBody, "complexity").String())
			})
		}
	}
}

func TestOpenAIImagesAliasesRespectExplicitMappings(t *testing.T) {
	for _, channelMapping := range []bool{false, true} {
		t.Run(fmt.Sprint(channelMapping), func(t *testing.T) {
			body := []byte(`{"model":"gpt-image-2.5","prompt":"draw"}`)
			c, _ := newOpenAIImagesTestContext(t, body)
			upstream := &httpUpstreamRecorder{resp: openAIImagesJSONResponse()}
			svc := newOpenAIImagesTestService(upstream)
			parsed, err := svc.ParseOpenAIImagesRequest(c, body)
			require.NoError(t, err)
			account := directImagesTestAccount()
			account.Type = AccountTypeAPIKey
			account.Credentials["api_key"] = "test-key"
			mapped := ""
			if channelMapping {
				mapped = "gpt-image-2.5-flare"
			} else {
				account.Credentials["model_mapping"] = map[string]any{"gpt-image-2.5": "gpt-image-2.5-flare"}
			}
			result, err := svc.ForwardImages(context.Background(), c, account, body, parsed, mapped)
			require.NoError(t, err)
			require.Equal(t, "gpt-image-2.5-flare", result.UpstreamModel)
			require.Equal(t, "gpt-image-2.5-flare", gjson.GetBytes(upstream.lastBody, "model").String())
		})
	}
}

func TestOpenAIResponsesImageAliasesAndComplexity(t *testing.T) {
	for _, model := range []string{"gpt-image-2.5", "gpt-image-2.6", "gpt-image-2.5-flare", "gpt-image-2.6-sunburst"} {
		for _, existingTool := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/existing=%t", model, existingTool), func(t *testing.T) {
				req := map[string]any{"model": model, "input": "draw", "complexity": "high", "quality": "low"}
				if existingTool {
					req["tools"] = []any{map[string]any{"type": "image_generation", "model": "gpt-image-2.5-flare", "complexity": "low"}}
				}
				require.True(t, normalizeOpenAIResponsesImageOnlyModel(req))
				require.NotContains(t, req, "complexity")
				tool := req["tools"].([]any)[0].(map[string]any)
				wantModel, wantComplexity := model, "high"
				if model == "gpt-image-2.5" || model == "gpt-image-2.6" {
					wantModel += "-sunburst"
				}
				if existingTool {
					wantModel, wantComplexity = "gpt-image-2.5-flare", "low"
				}
				require.Equal(t, wantModel, tool["model"])
				require.Equal(t, wantComplexity, tool["complexity"])
				require.Equal(t, "low", tool["quality"])
				require.False(t, normalizeOpenAIResponsesImageOnlyModel(req), "normalization must be idempotent")
			})
		}
		body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","tools":[{"type":"image_generation","model":%q,"complexity":"high"}]}`, model))
		wantNormalization := model == "gpt-image-2.5" || model == "gpt-image-2.6"
		require.Equal(t, wantNormalization, openAIRequestBodyImageGenerationToolNeedsNormalization(body))
		tool := map[string]any{"type": "image_generation", "model": model, "complexity": "high"}
		req := map[string]any{"model": "gpt-6-astra", "tools": []any{tool}}
		require.Equal(t, wantNormalization, normalizeOpenAIResponsesImageGenerationTools(req))
		if wantNormalization {
			require.Equal(t, model+"-sunburst", tool["model"])
		}
		require.Equal(t, "high", tool["complexity"])
		require.Equal(t, "gpt-6-astra", req["model"])
	}
}

func TestOpenAIImageAliasPricingUsesConcreteModel(t *testing.T) {
	for _, alias := range []string{"gpt-image-2.5", "gpt-image-2.6"} {
		t.Run(alias, func(t *testing.T) {
			old := &LiteLLMModelPricing{InputCostPerToken: 1}
			canonical := &LiteLLMModelPricing{InputCostPerToken: 2}
			svc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{"gpt-image-2": old, alias + "-sunburst": canonical}}
			require.Same(t, canonical, svc.GetModelPricing(alias))
			custom := &LiteLLMModelPricing{InputCostPerToken: 3}
			svc.pricingData[alias] = custom
			require.Same(t, custom, svc.GetModelPricing(alias), "an explicit alias price still takes precedence")
			delete(svc.pricingData, alias)
			delete(svc.pricingData, alias+"-sunburst")
			require.InDelta(t, 30e-6, svc.GetModelPricing(alias).OutputCostPerImageToken, 1e-12)
		})
	}
}

func TestCodexImagesUnknownModelsFailBeforeForwarding(t *testing.T) {
	for _, model := range []string{"gpt-image-6", "gpt-image-2.5-flash", "gpt-image-audit-nonexistent"} {
		t.Run(model, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":%q,"prompt":"draw"}`, model))
			c, rec := newOpenAIImagesTestContext(t, body)
			upstream := &httpUpstreamRecorder{resp: openAIImagesJSONResponse()}
			svc := newOpenAIImagesTestService(upstream)
			parsed, err := svc.ParseOpenAIImagesRequest(c, body)
			require.NoError(t, err)
			_, err = svc.ForwardImages(context.Background(), c, directImagesTestAccount(), body, parsed, "")
			require.Error(t, err)
			require.Nil(t, upstream.lastReq, "Codex native images can silently accept unknown model names")
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, "model", gjson.GetBytes(rec.Body.Bytes(), "error.param").String())
			require.Equal(t, "model_not_found", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
		})
	}
}

func TestOpenAIImagesAPIKeyKeepsProviderDefinedModels(t *testing.T) {
	body := []byte(`{"model":"gpt-image-custom","prompt":"draw"}`)
	c, _ := newOpenAIImagesTestContext(t, body)
	upstream := &httpUpstreamRecorder{resp: openAIImagesJSONResponse()}
	svc := newOpenAIImagesTestService(upstream)
	parsed, err := svc.ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	account := newOpenAIImageGenerationControlTestAccount()
	_, err = svc.ForwardImages(context.Background(), c, account, body, parsed, "")
	require.NoError(t, err)
	require.Equal(t, "gpt-image-custom", gjson.GetBytes(upstream.lastBody, "model").String())
}

func TestOpenAIResponsesImageAliasForwardingAndBilling(t *testing.T) {
	for _, alias := range []string{"gpt-image-2.5", "gpt-image-2.6"} {
		for _, mode := range []string{"image-only", "explicit-tool", "passthrough-tool"} {
			t.Run(alias+"/"+mode, func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","stream":false,"tools":[{"type":"image_generation","model":%q,"complexity":"high"}],"input":"draw"}`, alias))
				if mode == "image-only" {
					body = []byte(fmt.Sprintf(`{"model":%q,"stream":false,"complexity":"high","n":2,"input":"draw"}`, alias))
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"output":[{"type":"image_generation_call","result":"aGVsbG8="}],"usage":{"input_tokens":1,"output_tokens":2}}`)),
				}}
				svc := newOpenAIImageGenerationControlTestService(upstream)
				c, _ := newOpenAIImageGenerationControlTestContext(true, "curl/8.0")
				account := newOpenAIImageGenerationControlTestAccount()
				account.Extra = map[string]any{"use_responses_api": true, "openai_passthrough": mode == "passthrough-tool"}
				result, err := svc.Forward(context.Background(), c, account, body)
				require.NoError(t, err)
				require.Equal(t, alias+"-sunburst", gjson.GetBytes(upstream.lastBody, "tools.0.model").String())
				require.Equal(t, "high", gjson.GetBytes(upstream.lastBody, "tools.0.complexity").String())
				require.False(t, gjson.GetBytes(upstream.lastBody, "complexity").Exists())
				require.Equal(t, alias+"-sunburst", result.BillingModel)
				require.Equal(t, 1, result.ImageCount)
				if mode == "image-only" {
					require.Equal(t, alias, result.Model)
					require.Equal(t, openAIImagesResponsesMainModelValue(), result.UpstreamModel)
					require.Equal(t, int64(2), gjson.GetBytes(upstream.lastBody, "tools.0.n").Int())
				}
			})
		}
	}
}

func TestOpenAIResponsesWebSocketImageAliases(t *testing.T) {
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		for _, alias := range []string{"gpt-image-2.5", "gpt-image-2.6"} {
			body := []byte(fmt.Sprintf(`{"type":"response.create","model":"gpt-6-astra","input":[{"role":"user","content":[{"type":"input_text","text":"draw","nonce":1e1000000}]}],"tools":[{"type":"image_generation","model":%q,"complexity":"high"}]}`, alias))
			normalized, changed, err := normalizeOpenAIResponsesWebSocketCompatibilityBody(body, &Account{Platform: PlatformOpenAI, Type: accountType}, false)
			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, alias+"-sunburst", gjson.GetBytes(normalized, "tools.0.model").String())
			require.Equal(t, "high", gjson.GetBytes(normalized, "tools.0.complexity").String())
			require.Equal(t, "1e1000000", gjson.GetBytes(normalized, "input.0.content.0.nonce").Raw)
		}
	}
}

func TestOpenAIImagesResponsesInputFidelityAndNullDefaults(t *testing.T) {
	for _, model := range []string{"gpt-image-1.5", "gpt-image-1-mini", "gpt-image-2", "gpt-image-2.5", "gpt-image-2.6"} {
		t.Run(model, func(t *testing.T) {
			fidelity := "high"
			if model == "gpt-image-1-mini" {
				fidelity = "low"
			}
			wantFidelity := strings.HasPrefix(model, "gpt-image-1")
			parsed := &OpenAIImagesRequest{Model: model, Prompt: "draw", InputFidelity: fidelity}
			body, err := buildOpenAIImagesResponsesRequest(parsed, model)
			require.NoError(t, err)
			require.Equal(t, wantFidelity, gjson.GetBytes(body, "tools.0.input_fidelity").Exists())
			req := map[string]any{"model": model, "input": "draw", "n": 2, "input_fidelity": fidelity, "complexity": nil, "quality": nil}
			require.True(t, normalizeOpenAIResponsesImageOnlyModel(req))
			tool := req["tools"].([]any)[0].(map[string]any)
			require.Equal(t, 2, tool["n"])
			_, hasFidelity := tool["input_fidelity"]
			require.Equal(t, wantFidelity, hasFidelity)
			for _, key := range []string{"complexity", "quality"} {
				require.NotContains(t, req, key)
				require.NotContains(t, tool, key, "null must use the model default")
			}
			require.NotContains(t, req, "n")
			require.NotContains(t, req, "input_fidelity")
		})
	}
}

func TestAccountTestServiceImageAliasesMatchForwarding(t *testing.T) {
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		for _, alias := range []string{"gpt-image-2.5", "gpt-image-2.6"} {
			t.Run(accountType+"/"+alias, func(t *testing.T) {
				c, rec := newOpenAIImagesTestContext(t, nil)
				upstream := &httpUpstreamRecorder{resp: openAIImagesJSONResponse()}
				svc := &AccountTestService{httpUpstream: upstream, cfg: &config.Config{}}
				account := directImagesTestAccount()
				account.Type = accountType
				account.Credentials["api_key"] = "test-key"
				var err error
				if accountType == AccountTypeOAuth {
					err = svc.testOpenAIImageOAuth(c, context.Background(), account, alias, "draw")
				} else {
					err = svc.testOpenAIImageAPIKey(c, context.Background(), account, alias, "draw")
				}
				require.NoError(t, err)
				require.Equal(t, alias+"-sunburst", gjson.GetBytes(upstream.lastBody, "model").String())
				require.Contains(t, rec.Body.String(), `"success":true`)
			})
		}
	}
}
