//go:build unit

package service

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestOAuthReliabilityContractChatSchemaEnvelopeRejectsAmbiguity(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct{ name, format, param string }{
			{"outer_schema", `{"type":"json_schema","schema":{"const":"PRIVATE_SCHEMA"},"json_schema":{"name":"fixture","schema":{}}}`, "response_format"},
			{"outer_strict", `{"type":"json_schema","strict":true,"json_schema":{"name":"fixture","strict":false,"schema":{}}}`, "response_format"},
			{"outer_extension", `{"type":"json_schema","x_future_contract":"PRIVATE_SCHEMA","json_schema":{"name":"fixture","schema":{}}}`, "response_format"},
			{"inner_type", `{"type":"json_schema","json_schema":{"type":"json_object","name":"fixture","schema":{}}}`, "response_format.json_schema"},
			{"duplicate_name", `{"type":"json_schema","json_schema":{"name":"PRIVATE_SCHEMA","name":"fixture","schema":{}}}`, "response_format.json_schema"},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tc.name), func(t *testing.T) {
				body := oauthReliabilityContractBody("chat", stream)
				body, err := sjson.SetRawBytes(body, "response_format", []byte(tc.format))
				require.NoError(t, err)
				original := bytes.Clone(body)
				upstream := &httpUpstreamRecorder{}
				c, recorder := oauthReliabilityContractContext("/v1/chat/completions", body)
				result, err := newOpenAIRejectedFieldTestService(upstream).ForwardAsChatCompletions(c.Request.Context(), c, newOpenAIOAuthNamespaceTestAccount(), body, "", "")
				require.Error(t, err)
				require.Nil(t, result)
				require.Empty(t, upstream.bodies)
				require.Equal(t, http.StatusBadRequest, recorder.Code)
				require.Equal(t, tc.param, gjson.Get(recorder.Body.String(), "error.param").String())
				require.Equal(t, "invalid_request_error", gjson.Get(recorder.Body.String(), "error.type").String())
				require.NotContains(t, recorder.Body.String(), "PRIVATE")
				require.NotContains(t, err.Error(), "PRIVATE")
				require.True(t, IsResponseCommitted(c))
				require.Equal(t, original, body)
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover))
			})
		}
	}
}

func TestOAuthReliabilityContractChatSchemaInnerExtensionsPreserved(t *testing.T) {
	const schema = `{"properties":{"items":{"uniqueItems":true,"minimum":9007199254740993}},"minProperties":1,"x_future_keyword":{"type":"PRIVATE_SCHEMA"}}`
	for _, strict := range []bool{false, true} {
		for _, rejected := range []bool{false, true} {
			t.Run(fmt.Sprintf("strict=%t/rejected=%t", strict, rejected), func(t *testing.T) {
				body := oauthReliabilityContractBody("chat", false)
				body, err := sjson.SetRawBytes(body, "response_format", []byte(fmt.Sprintf(`{"type":"json_schema","json_schema":{"name":"fixture","description":"retain","strict":%t,"schema":%s,"x_future_contract":{"value":9007199254740993}}}`, strict, schema)))
				require.NoError(t, err)
				response := oauthReliabilityContractSSE(oauthReliabilityContractCompleted)
				if rejected {
					response = newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, `{"error":{"type":"invalid_request_error","code":"invalid_json_schema","param":"text.format.schema","message":"Unsupported schema keyword"}}`)
				}
				upstream := &httpUpstreamRecorder{resp: response}
				c, recorder := oauthReliabilityContractContext("/v1/chat/completions", body)
				result, err := newOpenAIRejectedFieldTestService(upstream).ForwardAsChatCompletions(c.Request.Context(), c, newOpenAIOAuthNamespaceTestAccount(), body, "", "")
				if rejected {
					require.Error(t, err)
					require.Equal(t, http.StatusBadRequest, recorder.Code)
					var failover *UpstreamFailoverError
					require.False(t, errors.As(err, &failover))
				} else {
					require.NoError(t, err)
					require.NotNil(t, result)
				}
				require.Len(t, upstream.bodies, 1, "unsupported schema must not be weakened or retried")
				format := gjson.GetBytes(upstream.lastBody, "text.format")
				require.JSONEq(t, schema, format.Get("schema").Raw)
				require.Equal(t, strict, format.Get("strict").Bool())
				require.Equal(t, "9007199254740993", format.Get("schema.properties.items.minimum").Raw)
				require.Equal(t, "9007199254740993", format.Get("x_future_contract.value").Raw)
				require.False(t, format.Get("schema.type").Exists())
				require.False(t, jsonSearchCompatHasSearch(upstream.lastBody))
			})
		}
	}
}

func TestOAuthReliabilityContractMessagesSchemaRejectsMixedEnvelope(t *testing.T) {
	for _, target := range []string{"responses", "chat"} {
		for _, stream := range []bool{false, true} {
			for _, nested := range []string{`null`, `{"name":"PRIVATE_NAME","schema":{"const":"PRIVATE_SCHEMA"}}`} {
				t.Run(fmt.Sprintf("%s/stream=%t/null=%t", target, stream, nested == "null"), func(t *testing.T) {
					body := oauthReliabilityContractBody("messages", stream)
					body, err := sjson.SetRawBytes(body, "output_config.format", []byte(`{"type":"json_schema","schema":{"type":"object"},"json_schema":`+nested+`}`))
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
					require.Error(t, err)
					require.Nil(t, result)
					require.Empty(t, upstream.bodies)
					require.Equal(t, http.StatusBadRequest, recorder.Code)
					require.Equal(t, "output_config.format", gjson.Get(recorder.Body.String(), "error.param").String())
					require.NotContains(t, recorder.Body.String(), "PRIVATE")
					require.True(t, IsResponseCommitted(c))
				})
			}
		}
	}
}
