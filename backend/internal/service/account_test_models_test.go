package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

func TestFetchOpenAIAccountModelsOAuthPopulatesPickerFields(t *testing.T) {
	_, calls := newCodexModelsOAuthCacheServer(t, `{"models":[{"slug":"new-oauth-model"},{"slug":"gpt-6-astra"}]}`)
	gateway := &OpenAIGatewayService{}
	svc := &AccountTestService{openaiGatewayService: gateway}
	account := newCodexModelsTestAccount()
	ctx := context.Background()

	before, err := gateway.FetchOpenAIModelsList(ctx, account)
	require.NoError(t, err)
	models, err := svc.FetchOpenAIAccountModels(ctx, account)
	require.NoError(t, err)
	require.Len(t, models, 2)
	require.Equal(t, "new-oauth-model", models[0].ID)
	require.Equal(t, "new-oauth-model", models[0].DisplayName)
	require.Equal(t, "model", models[0].Type)
	require.Equal(t, "gpt-6-astra", models[1].ID)
	require.Equal(t, "GPT-6 Astra", models[1].DisplayName)
	require.Equal(t, "model", models[1].Type)
	after, err := gateway.FetchOpenAIModelsList(ctx, account)
	require.NoError(t, err)
	require.Equal(t, before.Body, after.Body, "picker fields must not change the shared catalog")
	require.NotContains(t, string(after.Body), "display_name")
	require.EqualValues(t, 1, calls.Load(), "picker must reuse the shared discovery cache")
}

func TestFetchOpenAIAccountModelsAPIKeyPopulatesPickerFields(t *testing.T) {
	gateway := newCodexModelsAPIKeyTestService(&codexModelsHTTPUpstreamStub{do: func(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		return ordinaryModelsUpstreamResponse(`{"data":[
			{"id":"new-api-model","owned_by":"provider","created":123},
			{"id":"blank-label","display_name":"  ","type":""},
			{"id":"named-model","display_name":"Provider Model","type":"model"}
		]}`), nil
	}})
	svc := &AccountTestService{openaiGatewayService: gateway}
	models, err := svc.FetchOpenAIAccountModels(context.Background(), newCodexModelsAPIKeyTestAccount("https://models.example/v1"))
	require.NoError(t, err)
	require.Len(t, models, 3)
	for i, name := range []string{"new-api-model", "blank-label", "Provider Model"} {
		require.Equal(t, name, models[i].DisplayName)
		require.Equal(t, "model", models[i].Type)
	}
	require.Equal(t, "provider", models[0].OwnedBy)
	require.EqualValues(t, 123, models[0].Created)
	require.Equal(t, "named-model", models[2].ID)
}

func TestMergeOpenAIAccountTestModelsAppendsMappedImageModels(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"gpt-6-astra":   "gpt-6-astra",
				"gpt-image-2":   "gpt-image-2",
				"gpt-image-2.5": "gpt-image-2.5",
				"custom-alias":  "gpt-5.5",
				"gpt-image-*":   "gpt-image-2",
			},
		},
	}

	merged := MergeOpenAIAccountTestModels([]openai.Model{{ID: "gpt-6-astra"}}, account)
	ids := make([]string, 0, len(merged))
	display := map[string]string{}
	for _, model := range merged {
		ids = append(ids, model.ID)
		display[model.ID] = model.DisplayName
	}
	require.Equal(t, "gpt-6-astra", ids[0], "live catalog order must be preserved")
	require.Contains(t, ids, "gpt-image-2")
	require.Contains(t, ids, "gpt-image-2.5")
	require.Contains(t, ids, "custom-alias")
	require.NotContains(t, ids, "gpt-image-*")
	require.Equal(t, "GPT Image 2.5", display["gpt-image-2.5"])
	require.Equal(t, "GPT Image 2", display["gpt-image-2"])
}

func TestFetchOpenAIAccountModelsOAuthIncludesMappedImageModels(t *testing.T) {
	_, _ = newCodexModelsOAuthCacheServer(t, `{"models":[{"slug":"gpt-6-astra"}]}`)
	gateway := &OpenAIGatewayService{}
	svc := &AccountTestService{openaiGatewayService: gateway}
	account := newCodexModelsTestAccount()
	account.Credentials["model_mapping"] = map[string]any{
		"gpt-6-astra":   "gpt-6-astra",
		"gpt-image-2":   "gpt-image-2",
		"gpt-image-2.5": "gpt-image-2.5",
	}

	models, err := svc.FetchOpenAIAccountModels(context.Background(), account)
	require.NoError(t, err)
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	require.Contains(t, ids, "gpt-6-astra")
	require.Contains(t, ids, "gpt-image-2")
	require.Contains(t, ids, "gpt-image-2.5")
}

func TestFetchOpenAIAccountModelsPreservesEmptyCatalog(t *testing.T) {
	gateway := newCodexModelsAPIKeyTestService(&codexModelsHTTPUpstreamStub{do: func(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		return ordinaryModelsUpstreamResponse(`{"data":[]}`), nil
	}})
	svc := &AccountTestService{openaiGatewayService: gateway}
	models, err := svc.FetchOpenAIAccountModels(context.Background(), newCodexModelsAPIKeyTestAccount("https://models.example/v1"))
	require.NoError(t, err)
	require.Empty(t, models, "an empty upstream catalog must not become a static model list")
}
