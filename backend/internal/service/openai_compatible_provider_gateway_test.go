//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func managedOpenAICompatibleTestAccount(provider, baseURL, model string) *Account {
	return &Account{
		ID:          91,
		Name:        provider,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":       "sk-managed-test",
			"base_url":      baseURL,
			"model_mapping": map[string]any{model: model},
		},
		Extra: map[string]any{
			openai_compat.ExtraKeyCompatibleProvider: provider,
		},
	}
}

func managedProviderRejectingUpstream() *httpUpstreamRecorder {
	return &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"stop after request capture"}}`)),
	}}
}

func TestManagedProviderChatRequestsReachCorrectWireAPIWithReasoningDisabled(t *testing.T) {
	tests := []struct {
		name          string
		provider      string
		baseURL       string
		model         string
		wantURL       string
		wantField     string
		wantFieldText string
	}{
		{
			name:          "MiMo uses Responses",
			provider:      string(openai_compat.ProviderMiMo),
			baseURL:       "https://api.xiaomimimo.com/v1",
			model:         "mimo-v2.5",
			wantURL:       "https://api.xiaomimimo.com/v1/responses",
			wantField:     "reasoning.effort",
			wantFieldText: "none",
		},
		{
			name:          "GLM uses Chat Completions",
			provider:      string(openai_compat.ProviderZhipuGLM),
			baseURL:       "https://open.bigmodel.cn/api/paas/v4",
			model:         "glm-4.7-flashx",
			wantURL:       "https://open.bigmodel.cn/api/paas/v4/chat/completions",
			wantField:     "thinking.type",
			wantFieldText: "disabled",
		},
		{
			name:          "Qwen uses Responses",
			provider:      string(openai_compat.ProviderAlibabaQwen),
			baseURL:       "https://dashscope.aliyuncs.com/compatible-mode/v1",
			model:         "qwen3.7-flash",
			wantURL:       "https://dashscope.aliyuncs.com/compatible-mode/v1/responses",
			wantField:     "reasoning.effort",
			wantFieldText: "none",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"` + tc.model + `","messages":[{"role":"user","content":"hello"}],"stream":false,"reasoning":{"effort":"high"},"enable_thinking":true,"thinking":{"type":"enabled"}}`)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")

			upstream := managedProviderRejectingUpstream()
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := managedOpenAICompatibleTestAccount(tc.provider, tc.baseURL, tc.model)

			result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
			require.Error(t, err)
			require.Nil(t, result)
			require.NotNil(t, upstream.lastReq)
			require.Equal(t, tc.wantURL, upstream.lastReq.URL.String())
			require.Equal(t, tc.wantFieldText, gjson.GetBytes(upstream.lastBody, tc.wantField).String())
			require.NotEqual(t, "high", gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
			require.NotEqual(t, "enabled", gjson.GetBytes(upstream.lastBody, "thinking.type").String())
			if tc.provider == string(openai_compat.ProviderAlibabaQwen) {
				require.True(t, gjson.GetBytes(upstream.lastBody, "enable_thinking").Exists())
				require.False(t, gjson.GetBytes(upstream.lastBody, "enable_thinking").Bool())
			}
		})
	}
}

func TestManagedProviderNativeResponsesCannotReenableReasoning(t *testing.T) {
	for _, tc := range []struct {
		provider string
		baseURL  string
		model    string
	}{
		{string(openai_compat.ProviderMiMo), "https://api.xiaomimimo.com/v1", "mimo-v2.5"},
		{string(openai_compat.ProviderAlibabaQwen), "https://dashscope.aliyuncs.com/compatible-mode/v1", "qwen3.7-flash"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			body := []byte(`{"model":"` + tc.model + `","input":"hello","stream":false,"reasoning":{"effort":"high","summary":"detailed"},"enable_thinking":true}`)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")

			upstream := managedProviderRejectingUpstream()
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := managedOpenAICompatibleTestAccount(tc.provider, tc.baseURL, tc.model)

			result, err := svc.Forward(context.Background(), c, account, body)
			require.Error(t, err)
			require.Nil(t, result)
			require.Equal(t, tc.baseURL+"/responses", upstream.lastReq.URL.String())
			require.Equal(t, "none", gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
			require.False(t, gjson.GetBytes(upstream.lastBody, "reasoning.summary").Exists())
			if tc.provider == string(openai_compat.ProviderAlibabaQwen) {
				require.False(t, gjson.GetBytes(upstream.lastBody, "enable_thinking").Bool())
			} else {
				require.False(t, gjson.GetBytes(upstream.lastBody, "enable_thinking").Exists())
			}
		})
	}
}
