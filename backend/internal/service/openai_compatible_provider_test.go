package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
)

func TestNormalizeOpenAICompatibleProviderConfig(t *testing.T) {
	tests := []struct {
		provider      openai_compat.CompatibleProviderID
		wantBaseURL   string
		wantModels    map[string]any
		wantResponses string
	}{
		{
			provider:      openai_compat.ProviderMiMo,
			wantBaseURL:   "https://api.xiaomimimo.com/v1",
			wantModels:    map[string]any{"mimo-v2.5": "mimo-v2.5"},
			wantResponses: "force_responses",
		},
		{
			provider:      openai_compat.ProviderZhipuGLM,
			wantBaseURL:   "https://open.bigmodel.cn/api/paas/v4",
			wantModels:    map[string]any{"glm-4.7-flash": "glm-4.7-flash", "glm-4.7-flashx": "glm-4.7-flashx"},
			wantResponses: "force_chat_completions",
		},
		{
			provider:      openai_compat.ProviderAlibabaQwen,
			wantBaseURL:   "https://dashscope.aliyuncs.com/compatible-mode/v1",
			wantModels:    map[string]any{"qwen3.7-flash": "qwen3.7-flash"},
			wantResponses: "force_responses",
		},
	}

	for _, tc := range tests {
		t.Run(string(tc.provider), func(t *testing.T) {
			credentials, extra, err := normalizeOpenAICompatibleProviderConfig(
				PlatformOpenAI,
				AccountTypeAPIKey,
				map[string]any{"api_key": "secret"},
				map[string]any{openai_compat.ExtraKeyCompatibleProvider: string(tc.provider)},
			)
			require.NoError(t, err)
			require.Equal(t, "secret", credentials["api_key"])
			require.Equal(t, tc.wantBaseURL, credentials["base_url"])
			require.Equal(t, tc.wantModels, credentials["model_mapping"])
			require.Equal(t, []string{"chat_completions"}, credentials[openAIEndpointCapabilitiesCredentialKey])
			require.Equal(t, string(tc.provider), extra[openai_compat.ExtraKeyCompatibleProvider])
			require.Equal(t, tc.wantResponses, extra[openai_compat.ExtraKeyResponsesMode])
			require.Equal(t, true, extra["openai_ws_force_http"])
			require.Equal(t, OpenAIWSIngressModeOff, extra["openai_apikey_responses_websockets_v2_mode"])
		})
	}
}

func TestNormalizeOpenAICompatibleProviderConfigRejectsInvalidConfiguration(t *testing.T) {
	_, _, err := normalizeOpenAICompatibleProviderConfig(
		PlatformOpenAI,
		AccountTypeAPIKey,
		map[string]any{"base_url": "https://example.invalid/v1"},
		map[string]any{openai_compat.ExtraKeyCompatibleProvider: string(openai_compat.ProviderMiMo)},
	)
	require.Error(t, err)

	_, _, err = normalizeOpenAICompatibleProviderConfig(
		PlatformAnthropic,
		AccountTypeAPIKey,
		nil,
		map[string]any{openai_compat.ExtraKeyCompatibleProvider: string(openai_compat.ProviderMiMo)},
	)
	require.Error(t, err)

	_, _, err = normalizeOpenAICompatibleProviderConfig(
		PlatformOpenAI,
		AccountTypeAPIKey,
		nil,
		map[string]any{openai_compat.ExtraKeyCompatibleProvider: "unknown"},
	)
	require.Error(t, err)
}
