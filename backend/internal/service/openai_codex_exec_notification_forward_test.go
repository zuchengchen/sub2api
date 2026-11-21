package service

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const httpExecNotificationItems = `[
	{"type":"custom_tool_call","id":"ctc_exec","call_id":"call_exec","name":"exec","input":"fixture"},
	{"type":"custom_tool_call_output","id":"ctco_primary","call_id":"call_exec","output":[{"type":"input_text","text":"running"}]},
	{"type":"custom_tool_call_output","id":"ctco_one","call_id":"call_exec","name":"exec","output":"same text"},
	{"type":"custom_tool_call_output","id":"ctco_two","call_id":"call_exec","name":"exec","output":"same text"}
]`

func httpExecNotificationRequest() []byte {
	return []byte(`{"model":"gpt-5.5","instructions":"Test history","stream":false,"store":false,
		"tools":[{"type":"custom","name":"exec"}],"input":` + httpExecNotificationItems + `}`)
}

func TestCodexExecNotificationHTTPForwardsOriginalItems(t *testing.T) {
	for _, tc := range []struct {
		name        string
		account     *Account
		passthrough bool
		chat        bool
		omitTools   bool
	}{
		{name: "codex_oauth", account: newOpenAIOAuthNamespaceTestAccount()},
		{name: "oauth_passthrough", account: newOpenAIOAuthNamespaceTestAccount(), passthrough: true},
		{name: "native_api_key", account: newOpenAIRejectedFieldTestAccount()},
		{name: "api_key_passthrough_without_tool_declarations", account: newOpenAIRejectedFieldTestAccount(), passthrough: true, omitTools: true},
		{name: "responses_shaped_chat_on_oauth", account: newOpenAIOAuthNamespaceTestAccount(), chat: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: diagnosticContinuationSSE("resp_note", "[]")}
			svc := newOpenAIRejectedFieldTestService(upstream)
			tc.account.Extra["openai_passthrough"] = tc.passthrough
			body := httpExecNotificationRequest()
			var err error
			if tc.omitTools {
				body, err = sjson.DeleteBytes(body, "tools")
				require.NoError(t, err)
			}
			c := newOpenAIRejectedFieldTestContext(body)
			SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
			if tc.chat {
				c.Request.URL.Path = "/v1/chat/completions"
				_, err = svc.ForwardAsChatCompletions(context.Background(), c, tc.account, body, "", "")
			} else {
				_, err = svc.Forward(context.Background(), c, tc.account, body)
			}
			require.NoError(t, err)
			require.Len(t, upstream.bodies, 1)
			// A provider can inject its own instructions. Compare every tool
			// item, including its ID, call_id, contents and relative order.
			var toolItems []gjson.Result
			for _, item := range gjson.GetBytes(upstream.lastBody, "input").Array() {
				if strings.HasPrefix(item.Get("type").Str, "custom_tool_") {
					toolItems = append(toolItems, item)
				}
			}
			want := gjson.Parse(httpExecNotificationItems).Array()
			require.Len(t, toolItems, len(want))
			for i := range want {
				require.JSONEq(t, want[i].Raw, toolItems[i].Raw)
			}
		})
	}
}

func TestCodexExecNotificationRejectsLossyHTTPConversion(t *testing.T) {
	for _, route := range []string{"chat", "anthropic", "function_tools", "grok", "deepseek", "responses_shaped_chat", "responses_fallback", "anthropic_gateway"} {
		t.Run(route, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{}
			svc := newOpenAIRejectedFieldTestService(upstream)
			account := newOpenAIRejectedFieldTestAccount()
			body := httpExecNotificationRequest()
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
			var err error
			switch route {
			case "chat", "responses_shaped_chat":
				account.Extra[openai_compat.ExtraKeyResponsesMode] = string(openai_compat.ResponsesSupportModeForceChatCompletions)
			case "anthropic":
				account.Platform = PlatformKimi
				account.Credentials["api_protocol"] = APIProtocolAnthropic
			case "function_tools":
				account.Extra["openai_passthrough"] = true
			case "deepseek":
				account.Platform = PlatformDeepseek
				account.Credentials["api_protocol"] = APIProtocolResponses
			case "grok":
				account.Platform = PlatformGrok
			case "responses_fallback":
				delete(account.Extra, openai_compat.ExtraKeyResponsesSupported)
				upstream.resp = newOpenAIRejectedFieldTestResponse(http.StatusNotFound, `{"error":{"message":"Responses unavailable"}}`)
			}
			switch route {
			case "responses_shaped_chat", "responses_fallback":
				c.Request.URL.Path = "/v1/chat/completions"
				_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
			case "anthropic_gateway":
				_, err = (&GatewayService{}).forwardAsResponsesWithoutHostedWebSearch(context.Background(), c, account, body, nil)
			default:
				_, err = svc.Forward(context.Background(), c, account, body)
			}
			require.ErrorContains(t, err, "cannot preserve Codex exec notifications")
			if route == "responses_fallback" {
				require.Len(t, upstream.requests, 1)
				require.Equal(t, "/v1/responses", upstream.lastReq.URL.Path)
			} else {
				require.Empty(t, upstream.requests)
			}
			require.Equal(t, http.StatusBadRequest, c.Writer.Status())
			require.True(t, IsResponseCommitted(c))
			require.Equal(t, "unsupported_parameter", gjson.GetBytes(recorder.Body.Bytes(), "error.code").Str)
			require.Equal(t, "input", gjson.GetBytes(recorder.Body.Bytes(), "error.param").Str)
		})
	}
}

func TestCodexExecNotificationConversionScope(t *testing.T) {
	t.Run("oauth_legacy_ids_only_preserve_the_notification_call", func(t *testing.T) {
		input := gjson.Parse(`[{"type":"custom_tool_call","call_id":"call_x","name":"exec","input":"fixture"},
			{"type":"custom_tool_call_output","call_id":"call_x","name":"exec","output":"notify"},
			{"type":"function_call","call_id":"call_y","name":"lookup","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_y","output":"done"},
			{"type":"item_reference","id":"call_x"},{"type":"item_reference","id":"call_y"}]`)
		opts := codexInputFilterOptions{PreserveReferences: true, NativeItemIDs: true, ExecNotificationIDs: codexExecNotificationIDs(input)}
		items := filterCodexInputWithOptions(input.Value().([]any), opts)
		require.Len(t, items, 6)
		for i, expected := range []string{"call_x", "call_x", "fc_y", "fc_y"} {
			require.Equal(t, expected, items[i].(map[string]any)["call_id"])
		}
		require.Equal(t, "call_x", items[4].(map[string]any)["id"])
		require.Equal(t, "fc_y", items[5].(map[string]any)["id"])
	})
	t.Run("delta_on_explicit_anchor", func(t *testing.T) {
		body := []byte(`{"previous_response_id":"resp_x","input":{"type":"custom_tool_call_output","name":"exec","call_id":"call_x","output":"notify"}}`)
		require.Error(t, codexExecNotificationConversionError(body))
	})
	t.Run("different_tool_namespace", func(t *testing.T) {
		body := []byte(`{"input":[{"type":"custom_tool_call","namespace":"other","name":"exec","call_id":"x","input":"1"},{"type":"custom_tool_call_output","call_id":"x","name":"exec","output":"ordinary"}]}`)
		require.NoError(t, codexExecNotificationConversionError(body))
	})
	t.Run("nested_user_data_is_opaque", func(t *testing.T) {
		body := []byte(`{"input":[{"role":"user","content":[{"type":"input_text","text":"some data","extra":{"type":"custom_tool_call_output","name":"exec","call_id":"x","output":"payload"}}]}]}`)
		require.NoError(t, codexExecNotificationConversionError(body))
	})
	t.Run("conflicting_local_id_with_explicit_anchor", func(t *testing.T) {
		body := []byte(`{"previous_response_id":"resp_x","input":[{"type":"custom_tool_call","name":"exec","call_id":"x","input":"1"},{"type":"custom_tool_call","namespace":"other","name":"exec","call_id":"x","input":"2"},{"type":"custom_tool_call_output","name":"exec","call_id":"x","output":"notify"}]}`)
		require.Error(t, codexExecNotificationConversionError(body))
	})
}
