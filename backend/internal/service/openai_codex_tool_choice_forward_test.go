package service

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newCodexToolChoiceForwardResponse() *http.Response {
	return openAICompatSSECompletedResponse("resp_tool_choice", "gpt-5.4")
}

func newCodexToolChoiceOAuthAccount(passthrough bool) *Account {
	account := newOpenAIOAuthNamespaceTestAccount()
	account.ID = 6201
	account.Name = "oauth-tool-choice"
	account.RateMultiplier = f64p(1)
	if passthrough {
		account.Extra = map[string]any{"openai_passthrough": true}
	}
	return account
}

func requireOutboundFunctionToolChoice(t *testing.T, body []byte, name string) {
	t.Helper()
	require.NotEmpty(t, body)
	require.Equal(t, "function", gjson.GetBytes(body, "tool_choice.type").String(), string(body))
	require.Equal(t, name, gjson.GetBytes(body, "tool_choice.name").String(), string(body))
	require.NotEqual(t, "auto", gjson.GetBytes(body, "tool_choice").String(), string(body))
}

func TestOpenAIGatewayServiceForward_OAuthKeepsLegalFunctionToolChoice(t *testing.T) {
	gin.SetMode(gin.TestMode)

	additionalToolsBody := `{
		"model":"gpt-5.4","stream":false,"instructions":"test",
		"input":[
			{"type":"additional_tools","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]},
			{"type":"message","role":"user","content":"hi"}
		],
		"tool_choice":{"type":"function","name":"lookup"}
	}`
	topLevelBody := `{
		"model":"gpt-5.4","stream":false,"instructions":"test",
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
		"input":[{"type":"message","role":"user","content":"hi"}],
		"tool_choice":{"type":"function","name":"lookup"}
	}`
	unknownBody := `{
		"model":"gpt-5.4","stream":false,"instructions":"test",
		"input":[
			{"type":"additional_tools","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]},
			{"type":"message","role":"user","content":"hi"}
		],
		"tool_choice":{"type":"function","name":"missing"}
	}`
	allowedToolsBody := `{
		"model":"gpt-5.4","stream":false,"instructions":"test",
		"input":[
			{"type":"additional_tools","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]},
			{"type":"message","role":"user","content":"hi"}
		],
		"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"lookup"}]}
	}`
	namespaceBody := `{
		"model":"gpt-5.4","stream":false,"instructions":"test",
		"input":[
			{"type":"additional_tools","tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object"}}]}]},
			{"type":"message","role":"user","content":"hi"}
		],
		"tool_choice":{"type":"function","name":"spawn_agent"}
	}`
	aliasBody := `{
		"model":"gpt-5.4","stream":false,"instructions":"test",
		"input":[
			{"type":"additional_tools","tools":[{"type":"function","name":"python","parameters":{"type":"object"}}]},
			{"type":"message","role":"user","content":"hi"}
		],
		"tool_choice":{"type":"function","name":"python"}
	}`

	tests := []struct {
		name        string
		lite        bool
		body        string
		wantAuto    bool
		wantName    string
		wantAllowed bool
		wantAlias   bool
	}{
		{name: "additional_tools_legal_function", body: additionalToolsBody, wantName: "lookup"},
		{name: "top_level_legal_function", body: topLevelBody, wantName: "lookup"},
		{name: "additional_tools_unknown_function_auto", body: unknownBody, wantAuto: true},
		{name: "allowed_tools_untouched", body: allowedToolsBody, wantAllowed: true, wantName: "lookup"},
		{name: "namespace_child_function", body: namespaceBody, wantName: "spawn_agent"},
		{name: "reserved_python_alias", body: aliasBody, wantName: codexPythonToolAlias, wantAlias: true},
		{name: "lite_additional_tools_legal_function", lite: true, body: additionalToolsBody, wantName: "lookup"},
		{name: "lite_namespace_child_function", lite: true, body: namespaceBody, wantName: "spawn_agent"},
		{name: "lite_unknown_function_auto", lite: true, body: unknownBody, wantAuto: true},
		{name: "lite_allowed_tools_untouched", lite: true, body: allowedToolsBody, wantAllowed: true, wantName: "lookup"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			upstream := &httpUpstreamRecorder{resp: newCodexToolChoiceForwardResponse()}
			svc := newOpenAIRejectedFieldTestService(upstream)
			c := newOpenAIRejectedFieldTestContext(body)
			if tt.lite {
				c.Request.Header.Set(responsesLiteHeader, "true")
			}

			_, err := svc.Forward(context.Background(), c, newCodexToolChoiceOAuthAccount(false), body)

			require.NotNil(t, upstream.lastReq, "Forward must send an outbound request, err=%v", err)
			require.NotEmpty(t, upstream.lastBody, "outbound body missing, err=%v", err)
			forwarded := upstream.lastBody
			if tt.wantAuto {
				require.Equal(t, "auto", gjson.GetBytes(forwarded, "tool_choice").String(), string(forwarded))
				return
			}
			if tt.wantAllowed {
				require.Equal(t, "allowed_tools", gjson.GetBytes(forwarded, "tool_choice.type").String(), string(forwarded))
				require.Equal(t, "required", gjson.GetBytes(forwarded, "tool_choice.mode").String(), string(forwarded))
				require.Equal(t, tt.wantName, gjson.GetBytes(forwarded, "tool_choice.tools.0.name").String(), string(forwarded))
				require.NotEqual(t, "auto", gjson.GetBytes(forwarded, "tool_choice").String(), string(forwarded))
				return
			}
			requireOutboundFunctionToolChoice(t, forwarded, tt.wantName)
			if tt.wantAlias {
				require.Equal(t, codexPythonToolAlias, gjson.GetBytes(forwarded, `input.#(type=="additional_tools").tools.0.name`).String(), string(forwarded))
			}
		})
	}
}

func TestForwardAsChatCompletions_OAuthKeepsLegalFunctionToolChoice(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"lookup"}},
		"stream":false
	}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_chat_tool_choice", "gpt-5.4")}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream: upstream,
	}

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, newCodexToolChoiceOAuthAccount(false), body, "", "gpt-5.4")
	require.NotNil(t, upstream.lastReq, "chat completions must send an outbound request, err=%v", err)
	requireOutboundFunctionToolChoice(t, upstream.lastBody, "lookup")
}

func TestForwardAsChatCompletions_OAuthDowngradesUnknownFunctionToolChoice(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"missing"}},
		"stream":false
	}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_chat_tool_choice_missing", "gpt-5.4")}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream: upstream,
	}

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, newCodexToolChoiceOAuthAccount(false), body, "", "gpt-5.4")
	require.NotNil(t, upstream.lastReq, "chat completions must send an outbound request, err=%v", err)
	require.Equal(t, "auto", gjson.GetBytes(upstream.lastBody, "tool_choice").String(), string(upstream.lastBody))
}

func TestForwardAsAnthropic_OAuthKeepsLegalFunctionToolChoice(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{
		"model":"claude-sonnet-4-5","max_tokens":16,"stream":false,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"lookup","description":"lookup","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"lookup"}
	}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_messages_tool_choice", "gpt-5.4")}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream: upstream,
	}

	_, err := svc.ForwardAsAnthropic(context.Background(), c, newCodexToolChoiceOAuthAccount(false), body, "stable-cache-key", "gpt-5.4")
	require.NotNil(t, upstream.lastReq, "messages must send an outbound request, err=%v", err)
	requireOutboundFunctionToolChoice(t, upstream.lastBody, "lookup")
}

func TestForwardAsAnthropic_OAuthDowngradesUnknownFunctionToolChoice(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{
		"model":"claude-sonnet-4-5","max_tokens":16,"stream":false,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"lookup","description":"lookup","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"missing"}
	}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_messages_tool_choice_missing", "gpt-5.4")}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream: upstream,
	}

	_, err := svc.ForwardAsAnthropic(context.Background(), c, newCodexToolChoiceOAuthAccount(false), body, "stable-cache-key", "gpt-5.4")
	require.NotNil(t, upstream.lastReq, "messages must send an outbound request, err=%v", err)
	require.Equal(t, "auto", gjson.GetBytes(upstream.lastBody, "tool_choice").String(), string(upstream.lastBody))
}
