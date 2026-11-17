//go:build unit

package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func oauthReliabilityMessagesSchemaChatResponse(stream bool) *http.Response {
	if stream {
		return oauthReliabilityContractSSE(
			`{"id":"chatcmpl_schema","object":"chat.completion.chunk","model":"gpt-5.6-sol","choices":[{"index":0,"delta":{"role":"assistant","content":"{}"},"finish_reason":null}]}`,
			`{"id":"chatcmpl_schema","object":"chat.completion.chunk","model":"gpt-5.6-sol","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
			`[DONE]`,
		)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"id":"chatcmpl_schema","object":"chat.completion","model":"gpt-5.6-sol","choices":[{"index":0,"message":{"role":"assistant","content":"{}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))}
}

func TestOAuthReliabilityMessagesOutputSchemaForwarding(t *testing.T) {
	// These constraints intentionally exercise lossless forwarding, including
	// constraints a particular upstream may reject. The mock is not capability
	// evidence for any model or schema keyword.
	const schema = `{"properties":{"values":{"items":{"minimum":9007199254740993},"uniqueItems":true}},"minProperties":1,"required":["values"],"additionalProperties":false,"x_future_keyword":{"enabled":true}}`
	for _, target := range []string{"responses", "synthesized_responses", "lite_responses", "chat"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", target, stream), func(t *testing.T) {
				body := oauthReliabilityContractBody("messages", stream)
				body, err := sjson.SetRawBytes(body, "output_config", []byte(`{"effort":"low","format":{"type":"json_schema","schema":`+schema+`}}`))
				require.NoError(t, err)
				original := bytes.Clone(body)
				upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_messages_schema", "gpt-5.6-sol")}
				svc := newOpenAIRejectedFieldTestService(upstream)
				account := newOpenAIOAuthNamespaceTestAccount()
				field := "text.format"
				if target == "chat" {
					account = forceChatMessagesFallbackAccount()
					svc.cfg = rawChatCompletionsTestConfig()
					upstream.resp = oauthReliabilityMessagesSchemaChatResponse(stream)
					field = "response_format.json_schema"
				} else if target == "synthesized_responses" || target == "lite_responses" {
					pinCodexTestVersion(t, "0.163.0")
					account = newCodexSynthTestAccount(9271, AccountTypeOAuth)
					account.Extra[codexClientCapabilityExtraKey] = "generic"
					svc.cfg = codexSynthEnabledConfig()
				}
				c, recorder := oauthReliabilityContractContext("/v1/messages", body)
				if target == "lite_responses" {
					c.Request.Header.Set(responsesLiteHeader, "true")
				}
				result, err := svc.ForwardAsAnthropic(c.Request.Context(), c, account, body, "", "")
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, http.StatusOK, recorder.Code)
				require.Len(t, upstream.bodies, 1)
				require.JSONEq(t, schema, gjson.GetBytes(upstream.lastBody, field+".schema").Raw)
				require.Equal(t, "9007199254740993", gjson.GetBytes(upstream.lastBody, field+".schema.properties.values.items.minimum").Raw)
				require.True(t, gjson.GetBytes(upstream.lastBody, field+".strict").Bool())
				require.Equal(t, "response", gjson.GetBytes(upstream.lastBody, field+".name").String())
				require.False(t, gjson.GetBytes(upstream.lastBody, field+".schema.type").Exists())
				require.False(t, jsonSearchCompatHasSearch(upstream.lastBody))
				require.NotNil(t, result.ReasoningEffort)
				require.Equal(t, "low", *result.ReasoningEffort)
				require.Equal(t, original, body)
			})
		}
	}
}

func TestOAuthReliabilityMessagesOutputFormatReturnsParameter400(t *testing.T) {
	for _, target := range []string{"responses", "chat"} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct{ format, param string }{
				{`{"type":"PRIVATE_FORMAT","schema":{"const":"PRIVATE_SCHEMA"}}`, "output_config.format.type"},
				{`{"type":"json_schema","schema":null}`, "output_config.format.schema"},
				{`{"type":"json_schema","schema":{},"strict":false}`, "output_config.format.strict"},
				{`{"type":"json_schema","schema":{"const":"PRIVATE_SCHEMA"},"schema":{}}`, "output_config.format"},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", target, stream, tc.param), func(t *testing.T) {
					body := oauthReliabilityContractBody("messages", stream)
					body, err := sjson.SetRawBytes(body, "output_config.format", []byte(tc.format))
					require.NoError(t, err)
					upstream := &httpUpstreamRecorder{}
					svc := newOpenAIRejectedFieldTestService(upstream)
					account := newOpenAIOAuthNamespaceTestAccount()
					if target == "chat" {
						account = forceChatMessagesFallbackAccount()
						svc.cfg = rawChatCompletionsTestConfig()
					}
					c, recorder := oauthReliabilityContractContext("/v1/messages", body)
					result, err := svc.ForwardAsAnthropic(c.Request.Context(), c, account, body, "", "")
					require.Nil(t, result)
					var formatErr *apicompat.AnthropicOutputFormatError
					require.ErrorAs(t, err, &formatErr)
					require.Nil(t, upstream.lastReq)
					require.Empty(t, upstream.bodies)
					require.Equal(t, http.StatusBadRequest, recorder.Code)
					require.Equal(t, "error", gjson.Get(recorder.Body.String(), "type").String())
					require.Equal(t, "invalid_request_error", gjson.Get(recorder.Body.String(), "error.type").String())
					require.Equal(t, tc.param, gjson.Get(recorder.Body.String(), "error.param").String())
					require.True(t, IsResponseCommitted(c))
					require.NotContains(t, recorder.Body.String(), "PRIVATE")
					require.NotContains(t, err.Error(), "PRIVATE")
					require.Less(t, recorder.Body.Len(), 512)
					var failover *UpstreamFailoverError
					require.False(t, errors.As(err, &failover))
				})
			}
		}
	}
}

func TestOAuthReliabilityMessagesOutputSchemaPreservesExplicitTools(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","max_tokens":32,"messages":[{"role":"user","content":"fixture"}],"tools":[{"type":"web_search_20250305","name":"web_search"}],"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{},"additionalProperties":false}}}}`)
	upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_messages_schema_tools", "gpt-5.6-sol")}
	svc := newOpenAIRejectedFieldTestService(upstream)
	c, _ := oauthReliabilityContractContext("/v1/messages", body)
	_, err := svc.ForwardAsAnthropic(c.Request.Context(), c, newOpenAIOAuthNamespaceTestAccount(), body, "", "")
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 1)
	require.True(t, jsonSearchCompatHasSearch(upstream.lastBody))
	require.Equal(t, "json_schema", gjson.GetBytes(upstream.lastBody, "text.format.type").String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "text.format.strict").Bool())
}

func TestOAuthReliabilityMessagesSchemaRejectionDoesNotRewriteOrRetry(t *testing.T) {
	const schema = `{"type":"object","properties":{},"required":[],"additionalProperties":false,"x_future_keyword":true}`
	body := []byte(`{"model":"gpt-5.6-sol","max_tokens":32,"messages":[{"role":"user","content":"fixture"}],"output_config":{"format":{"type":"json_schema","schema":` + schema + `}}}`)
	upstream := &httpUpstreamRecorder{resp: newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, `{"error":{"type":"invalid_request_error","code":"invalid_json_schema","param":"text.format.schema","message":"Unsupported schema keyword"}}`)}
	svc := newOpenAIRejectedFieldTestService(upstream)
	c, recorder := oauthReliabilityContractContext("/v1/messages", body)
	result, err := svc.ForwardAsAnthropic(c.Request.Context(), c, newOpenAIOAuthNamespaceTestAccount(), body, "", "")
	require.Error(t, err)
	require.Nil(t, result)
	require.Len(t, upstream.bodies, 1)
	require.JSONEq(t, schema, gjson.GetBytes(upstream.lastBody, "text.format.schema").Raw)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover))
}

func TestOAuthReliabilityMessagesNativeOutputFormatRemainsPassthrough(t *testing.T) {
	for _, format := range []string{
		`{"type":"json_schema","schema":{"properties":{"value":{"minimum":9007199254740993}},"additionalProperties":false}}`,
		`{"type":"future_native_format","native_config":{"value":9007199254740993}}`,
	} {
		t.Run(gjson.Get(format, "type").String(), func(t *testing.T) {
			body := []byte(`{"model":"k3","max_tokens":32,"messages":[{"role":"user","content":"fixture"}],"output_config":{"format":` + format + `}}`)
			upstream := &httpUpstreamRecorder{resp: nativeAnthropicBufferedResponse()}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			c, _ := oauthReliabilityContractContext("/v1/messages", body)
			_, err := svc.ForwardAsAnthropic(c.Request.Context(), c, nativeAnthropicTestAccount(), body, "", "")
			require.NoError(t, err)
			require.Len(t, upstream.bodies, 1)
			// Compact RawMessage comparison preserves numbers that JSONEq's
			// default float64 decoder could otherwise round.
			var want, got bytes.Buffer
			require.NoError(t, json.Compact(&want, []byte(format)))
			require.NoError(t, json.Compact(&got, []byte(gjson.GetBytes(upstream.lastBody, "output_config.format").Raw)))
			require.Equal(t, want.String(), got.String())
		})
	}
}
