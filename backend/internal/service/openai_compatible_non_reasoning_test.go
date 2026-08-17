package service

import (
	"bytes"
	"context"
	"errors"
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

func compatibleProviderTestAccount(provider openai_compat.CompatibleProviderID) *Account {
	return &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{openai_compat.ExtraKeyCompatibleProvider: string(provider)},
	}
}

func TestEnforceOpenAICompatibleNonReasoningChat(t *testing.T) {
	tests := []struct {
		provider openai_compat.CompatibleProviderID
		model    string
		path     string
		want     string
	}{
		{openai_compat.ProviderMiMo, "mimo-v2.5", "thinking.type", "disabled"},
		{openai_compat.ProviderZhipuGLM, "glm-4.7-flash", "thinking.type", "disabled"},
		{openai_compat.ProviderZhipuGLM, "glm-4.7-flashx", "thinking.type", "disabled"},
		{openai_compat.ProviderAlibabaQwen, "qwen3.7-flash", "enable_thinking", "false"},
	}

	for _, tc := range tests {
		t.Run(string(tc.provider)+"/"+tc.model, func(t *testing.T) {
			body := []byte(`{"model":"` + tc.model + `","reasoning":{"effort":"high"},"reasoning_effort":"high","reasoningEffort":"high","thinking":{"type":"enabled"},"enable_thinking":true,"thinking_budget":8192,"thinking_budget_tokens":8192,"preserve_thinking":true,"output_config":{"effort":"high","format":{"type":"text"}}}`)
			got, changed, err := enforceOpenAICompatibleNonReasoning(compatibleProviderTestAccount(tc.provider), tc.model, body, openAICompatibleWireChat)
			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, tc.want, gjson.GetBytes(got, tc.path).String())
			require.False(t, gjson.GetBytes(got, "reasoning").Exists())
			require.False(t, gjson.GetBytes(got, "reasoning_effort").Exists())
			require.False(t, gjson.GetBytes(got, "thinking_budget").Exists())
			require.False(t, gjson.GetBytes(got, "thinking_budget_tokens").Exists())
			require.False(t, gjson.GetBytes(got, "preserve_thinking").Exists())
			require.False(t, gjson.GetBytes(got, "reasoningEffort").Exists())
			require.False(t, gjson.GetBytes(got, "output_config.effort").Exists())
			require.Equal(t, "text", gjson.GetBytes(got, "output_config.format.type").String())
		})
	}
}

func TestEnforceOpenAICompatibleNonReasoningResponses(t *testing.T) {
	for _, tc := range []struct {
		provider openai_compat.CompatibleProviderID
		model    string
	}{
		{openai_compat.ProviderMiMo, "mimo-v2.5"},
		{openai_compat.ProviderAlibabaQwen, "qwen3.7-flash"},
	} {
		t.Run(string(tc.provider), func(t *testing.T) {
			body := []byte(`{"model":"` + tc.model + `","reasoning":{"effort":"high","summary":"detailed"},"reasoningEffort":"high","enable_thinking":true,"output_config":{"effort":"high","format":{"type":"text"}}}`)
			got, changed, err := enforceOpenAICompatibleNonReasoning(compatibleProviderTestAccount(tc.provider), tc.model, body, openAICompatibleWireResponses)
			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, "none", gjson.GetBytes(got, "reasoning.effort").String())
			require.False(t, gjson.GetBytes(got, "reasoning.summary").Exists())
			require.False(t, gjson.GetBytes(got, "reasoningEffort").Exists())
			require.False(t, gjson.GetBytes(got, "output_config.effort").Exists())
			require.Equal(t, "text", gjson.GetBytes(got, "output_config.format.type").String())
			if tc.provider == openai_compat.ProviderAlibabaQwen {
				require.True(t, gjson.GetBytes(got, "enable_thinking").Exists())
				require.False(t, gjson.GetBytes(got, "enable_thinking").Bool())
			} else {
				require.False(t, gjson.GetBytes(got, "enable_thinking").Exists())
			}
		})
	}
}

func TestEnforceOpenAICompatibleNonReasoningDoesNotAffectOtherModels(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","reasoning":{"effort":"high"}}`)
	got, changed, err := enforceOpenAICompatibleNonReasoning(compatibleProviderTestAccount(openai_compat.ProviderMiMo), "gpt-5.4", body, openAICompatibleWireResponses)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, got)
}

func TestOpenAICompatibleNonReasoningFinalOutboundRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)

	type ingress string
	const (
		ingressChat      ingress = "chat"
		ingressResponses ingress = "responses"
		ingressMessages  ingress = "messages"
	)
	tests := []struct {
		provider openai_compat.CompatibleProviderID
		model    string
		wireAPI  openAICompatibleWireAPI
	}{
		{openai_compat.ProviderMiMo, "mimo-v2.5", openAICompatibleWireResponses},
		{openai_compat.ProviderZhipuGLM, "glm-4.7-flash", openAICompatibleWireChat},
		{openai_compat.ProviderZhipuGLM, "glm-4.7-flashx", openAICompatibleWireChat},
		{openai_compat.ProviderAlibabaQwen, "qwen3.7-flash", openAICompatibleWireResponses},
	}

	for _, tc := range tests {
		for _, requestIngress := range []ingress{ingressChat, ingressResponses, ingressMessages} {
			t.Run(string(tc.provider)+"/"+tc.model+"/"+string(requestIngress), func(t *testing.T) {
				account := compatibleProviderGatewayTestAccount(t, tc.provider)
				body, path := compatibleProviderIngressRequest(tc.model, string(requestIngress))
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")

				captureErr := errors.New("outbound request captured")
				upstream := &httpUpstreamRecorder{err: captureErr}
				svc := &OpenAIGatewayService{
					cfg:          compatibleProviderGatewayTestConfig(),
					httpUpstream: upstream,
				}

				var err error
				switch requestIngress {
				case ingressChat:
					_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
				case ingressResponses:
					_, err = svc.Forward(context.Background(), c, account, body)
				case ingressMessages:
					_, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
				}
				require.Error(t, err)
				require.NotNil(t, upstream.lastReq, "request did not reach the HTTP upstream")
				require.Equal(t, tc.model, gjson.GetBytes(upstream.lastBody, "model").String())

				switch tc.wireAPI {
				case openAICompatibleWireChat:
					require.True(t, strings.HasSuffix(upstream.lastReq.URL.Path, "/chat/completions"))
					require.Equal(t, "disabled", gjson.GetBytes(upstream.lastBody, "thinking.type").String())
					require.False(t, gjson.GetBytes(upstream.lastBody, "reasoning").Exists())
					require.False(t, gjson.GetBytes(upstream.lastBody, "reasoning_effort").Exists())
					require.False(t, gjson.GetBytes(upstream.lastBody, "reasoningEffort").Exists())
					require.False(t, gjson.GetBytes(upstream.lastBody, "output_config.effort").Exists())
					require.False(t, gjson.GetBytes(upstream.lastBody, "enable_thinking").Exists())
				case openAICompatibleWireResponses:
					require.True(t, strings.HasSuffix(upstream.lastReq.URL.Path, "/responses"))
					require.Equal(t, "none", gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
					require.False(t, gjson.GetBytes(upstream.lastBody, "reasoning.summary").Exists())
					require.False(t, gjson.GetBytes(upstream.lastBody, "reasoning_effort").Exists())
					require.False(t, gjson.GetBytes(upstream.lastBody, "reasoningEffort").Exists())
					require.False(t, gjson.GetBytes(upstream.lastBody, "output_config.effort").Exists())
					require.False(t, gjson.GetBytes(upstream.lastBody, "thinking").Exists())
					if tc.provider == openai_compat.ProviderAlibabaQwen {
						require.True(t, gjson.GetBytes(upstream.lastBody, "enable_thinking").Exists())
						require.False(t, gjson.GetBytes(upstream.lastBody, "enable_thinking").Bool())
					} else {
						require.False(t, gjson.GetBytes(upstream.lastBody, "enable_thinking").Exists())
					}
				}
			})
		}
	}
}

func TestOpenAICompatibleNonReasoningClearsResultMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		provider openai_compat.CompatibleProviderID
		model    string
		ingress  string
		wireAPI  openAICompatibleWireAPI
	}{
		{openai_compat.ProviderMiMo, "mimo-v2.5", "chat", openAICompatibleWireResponses},
		{openai_compat.ProviderMiMo, "mimo-v2.5", "messages", openAICompatibleWireResponses},
		{openai_compat.ProviderZhipuGLM, "glm-4.7-flash", "chat", openAICompatibleWireChat},
		{openai_compat.ProviderZhipuGLM, "glm-4.7-flash", "responses", openAICompatibleWireChat},
		{openai_compat.ProviderZhipuGLM, "glm-4.7-flashx", "messages", openAICompatibleWireChat},
		{openai_compat.ProviderAlibabaQwen, "qwen3.7-flash", "responses", openAICompatibleWireResponses},
	}

	for _, tc := range tests {
		t.Run(string(tc.provider)+"/"+tc.model+"/"+tc.ingress, func(t *testing.T) {
			account := compatibleProviderGatewayTestAccount(t, tc.provider)
			body, path := compatibleProviderIngressRequest(tc.model, tc.ingress)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")

			upstream := compatibleProviderSuccessfulUpstream(tc.wireAPI, tc.ingress, tc.model)
			svc := &OpenAIGatewayService{
				cfg:          compatibleProviderGatewayTestConfig(),
				httpUpstream: upstream,
			}

			var (
				result *OpenAIForwardResult
				err    error
			)
			switch tc.ingress {
			case "chat":
				result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
			case "responses":
				result, err = svc.Forward(context.Background(), c, account, body)
			case "messages":
				result, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
			}

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Nil(t, result.ReasoningEffort, "usage metadata must reflect the enforced non-reasoning request")
		})
	}
}

func compatibleProviderSuccessfulUpstream(wireAPI openAICompatibleWireAPI, ingress, model string) *httpUpstreamRecorder {
	contentType := "application/json"
	body := `{"id":"chatcmpl_managed","object":"chat.completion","model":"` + model + `","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	if wireAPI == openAICompatibleWireResponses {
		response := `{"id":"resp_managed","object":"response","model":"` + model + `","status":"completed","output":[{"type":"message","id":"msg_managed","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`
		body = response
		if ingress != "responses" {
			contentType = "text/event-stream"
			body = "data: {\"type\":\"response.completed\",\"response\":" + response + "}\n\ndata: [DONE]\n\n"
		}
	}
	return &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{contentType}, "x-request-id": []string{"rid-managed"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}}
}

func compatibleProviderGatewayTestAccount(t *testing.T, provider openai_compat.CompatibleProviderID) *Account {
	t.Helper()
	preset, ok := openai_compat.CompatibleProviderPresetByID(string(provider))
	require.True(t, ok)
	return &Account{
		ID:          901,
		Name:        "managed-" + string(provider),
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":       "sk-test",
			"base_url":      preset.BaseURL,
			"model_mapping": preset.ModelMapping(),
		},
		Extra: map[string]any{openai_compat.ExtraKeyCompatibleProvider: string(provider)},
	}
}

func compatibleProviderGatewayTestConfig() *config.Config {
	return &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
		Enabled:           false,
		AllowInsecureHTTP: true,
	}}}
}

func compatibleProviderIngressRequest(model, requestIngress string) ([]byte, string) {
	switch requestIngress {
	case "chat":
		return []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hello"}],"stream":false,"reasoning_effort":"high","reasoningEffort":"high","reasoning":{"effort":"high"},"thinking":{"type":"enabled"},"enable_thinking":true,"output_config":{"effort":"high"}}`), "/v1/chat/completions"
	case "responses":
		return []byte(`{"model":"` + model + `","input":"hello","stream":false,"reasoning":{"effort":"high","summary":"detailed"},"reasoning_effort":"high","reasoningEffort":"high","thinking":{"type":"enabled"},"enable_thinking":true,"output_config":{"effort":"high"}}`), "/v1/responses"
	default:
		return []byte(`{"model":"` + model + `","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":false,"thinking":{"type":"enabled","budget_tokens":1024},"output_config":{"effort":"high"}}`), "/v1/messages"
	}
}
