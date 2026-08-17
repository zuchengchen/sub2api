//go:build unit

package service

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAccountTestManagedProviderUsesPresetWirePolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		provider openai_compat.CompatibleProviderID
		model    string
		wantPath string
		field    string
		value    string
	}{
		{openai_compat.ProviderMiMo, "mimo-v2.5", "/responses", "reasoning.effort", "none"},
		{openai_compat.ProviderZhipuGLM, "glm-4.7-flash", "/chat/completions", "thinking.type", "disabled"},
		{openai_compat.ProviderZhipuGLM, "glm-4.7-flashx", "/chat/completions", "thinking.type", "disabled"},
		{openai_compat.ProviderAlibabaQwen, "qwen3.7-flash", "/responses", "reasoning.effort", "none"},
	} {
		t.Run(string(tc.provider), func(t *testing.T) {
			account := accountTestManagedProviderAccount(t, tc.provider, compatibleProviderPresetBaseURL(t, tc.provider), tc.model)
			upstream := accountTestRejectingUpstream()
			svc := &AccountTestService{
				cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
				httpUpstream: upstream,
			}
			ctx, _ := newTestContext()

			err := svc.testOpenAIAccountConnection(ctx, account, tc.model, "", "")
			require.Error(t, err)
			require.NotNil(t, upstream.lastReq)
			require.Contains(t, upstream.lastReq.URL.Path, tc.wantPath)
			require.Equal(t, tc.value, gjson.GetBytes(upstream.lastBody, tc.field).String())
			require.False(t, gjson.GetBytes(upstream.lastBody, "reasoning_effort").Exists())
			if tc.provider == openai_compat.ProviderAlibabaQwen {
				require.False(t, gjson.GetBytes(upstream.lastBody, "enable_thinking").Bool())
			} else {
				require.False(t, gjson.GetBytes(upstream.lastBody, "enable_thinking").Exists())
			}
		})
	}
}

func TestAccountTestManagedGLMRejectsResponsesCompactionProbe(t *testing.T) {
	account := accountTestManagedProviderAccount(t, openai_compat.ProviderZhipuGLM, compatibleProviderPresetBaseURL(t, openai_compat.ProviderZhipuGLM), "glm-4.7-flash")
	upstream := accountTestRejectingUpstream()
	svc := &AccountTestService{
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream: upstream,
	}
	ctx, _ := newTestContext()

	err := svc.testOpenAICompactConnection(ctx, account, "glm-4.7-flash")
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not support Responses compaction")
	require.Nil(t, upstream.lastReq)
}

func compatibleProviderPresetBaseURL(t *testing.T, provider openai_compat.CompatibleProviderID) string {
	t.Helper()
	preset, ok := openai_compat.CompatibleProviderPresetByID(string(provider))
	require.True(t, ok)
	return preset.BaseURL
}

func accountTestManagedProviderAccount(t *testing.T, provider openai_compat.CompatibleProviderID, baseURL, model string) *Account {
	t.Helper()
	return &Account{
		ID:          902,
		Name:        "account-test-" + string(provider),
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":       "sk-account-test",
			"base_url":      baseURL,
			"model_mapping": map[string]any{model: model},
		},
		Extra: map[string]any{openai_compat.ExtraKeyCompatibleProvider: string(provider)},
	}
}

func accountTestRejectingUpstream() *httpUpstreamRecorder {
	return &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"stop after request capture"}}`)),
	}}
}
