//go:build unit

package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestOAuthReliabilityFinalLiteHTTPRejectsBeforeUpstream(t *testing.T) {
	for _, route := range []string{"responses", "passthrough", "chat", "messages"} {
		for _, stream := range []bool{false, true} {
			cases := []struct{ name, fields, param string }{
				{"explicit_hosted", `"tools":[{"type":"web_search"}]`, "tools[0].type"},
				{"additional_hosted", `"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"web_search"}]}]`, "input[0].tools[0].type"},
				{"nested_hosted", `"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"private_name","tools":[{"type":"image_generation"}]}]}]`, "input[0].tools[0].tools[0].type"},
				{"hosted_tool_search", `"tools":[{"type":"tool_search","execution":"server"}]`, "tools[0].execution"},
				{"implicit_tool_search", `"tools":[{"type":"tool_search"}]`, "tools[0].execution"},
				{"required_history", `"input":[{"type":"web_search_call","id":"ws_fixture","status":"completed","action":{"type":"search","query":"private query"}}]`, "input[0].type"},
			}
			if route == "messages" {
				cases = []struct{ name, fields, param string }{{"converted_hosted", `"messages":[{"role":"user","content":"fixture"}],"max_tokens":32,"tools":[{"type":"web_search_20250305","name":"web_search"}]`, "tools[0].type"}}
			}
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", route, stream, tc.name), func(t *testing.T) {
					body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","instructions":"fixture","stream":%t,%s}`, stream, tc.fields))
					if route != "messages" && !gjson.GetBytes(body, "input").Exists() {
						var setErr error
						body, setErr = sjson.SetBytes(body, "input", "fixture")
						require.NoError(t, setErr)
					}
					original := bytes.Clone(body)
					upstream := &httpUpstreamRecorder{}
					svc := newOpenAIRejectedFieldTestService(upstream)
					c, recorder := oauthReliabilityContractContext("/v1/"+route, body)
					c.Request.Header.Set(responsesLiteHeader, "true")
					account := newOpenAIOAuthNamespaceTestAccount()
					account.Extra["openai_passthrough"] = route == "passthrough"
					result, err := oauthReliabilityContractForward(svc, c, account, route, body)
					require.Error(t, err)
					require.Nil(t, result)
					require.Empty(t, upstream.bodies)
					require.Nil(t, upstream.lastReq)
					require.Equal(t, http.StatusBadRequest, recorder.Code)
					require.Equal(t, "invalid_request_error", gjson.Get(recorder.Body.String(), "error.type").String())
					require.Equal(t, tc.param, gjson.Get(recorder.Body.String(), "error.param").String())
					require.NotContains(t, recorder.Body.String(), "private query")
					require.Equal(t, original, body)
					var retry *UpstreamFailoverError
					require.False(t, errors.As(err, &retry))
				})
			}
		}
	}
}

func TestOAuthReliabilityFinalLiteSynthesisAcrossHTTPEntrypoints(t *testing.T) {
	pinCodexTestVersion(t, "0.163.0")
	for _, route := range []string{"responses", "chat", "messages"} {
		for _, lite := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/lite=%t", route, lite), func(t *testing.T) {
				body := oauthReliabilityContractBody(route, false)
				body, err := sjson.SetBytes(body, "model", "gpt-5.6-terra")
				require.NoError(t, err)
				account := newCodexSynthTestAccount(9231, AccountTypeOAuth)
				account.Extra[codexClientCapabilityExtraKey] = "generic"
				upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_lite_final", "gpt-5.6-terra")}
				svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig(), httpUpstream: upstream, tiboRouteDisabled: true}
				c, _ := oauthReliabilityContractContext("/v1/"+route, body)
				if lite {
					c.Request.Header.Set(responsesLiteHeader, "true")
				}
				result, err := oauthReliabilityContractForward(svc, c, account, route, body)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Len(t, upstream.bodies, 1)
				require.Equal(t, !lite, gjsonToolsContainWebSearch(gjson.GetBytes(upstream.lastBody, "tools")))
				if lite {
					require.Equal(t, "true", upstream.lastReq.Header.Get(responsesLiteHeader))
					require.Equal(t, "all_turns", gjson.GetBytes(upstream.lastBody, "reasoning.context").String())
				}
				value, found := c.Get(openAIFinalProtocolDiagnosticKey)
				require.True(t, found)
				diagnostic := value.(openAIFinalProtocolDiagnostic)
				require.Equal(t, lite, diagnostic.FinalLite)
				if lite {
					require.NotContains(t, diagnostic.ToolSources, "synthesis")
				} else {
					require.Contains(t, diagnostic.ToolSources, "synthesis")
				}
			})
		}
	}
}

func TestOAuthReliabilityFinalLiteGPT55PreservesHostedTools(t *testing.T) {
	for _, route := range []string{"responses", "passthrough", "chat", "messages"} {
		t.Run(route, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.5","stream":false,"input":[{"type":"message","role":"user","content":"retain"}],"tools":[{"type":"web_search","external_web_access":false}],"tool_choice":"required"}`)
			if route == "messages" {
				body = []byte(`{"model":"gpt-5.5","max_tokens":32,"stream":false,"messages":[{"role":"user","content":"retain"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
			}
			account := newOpenAIOAuthNamespaceTestAccount()
			account.Extra["openai_passthrough"] = route == "passthrough"
			upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_mapped_lite", "gpt-5.5")}
			svc := newOpenAIRejectedFieldTestService(upstream)
			c, _ := oauthReliabilityContractContext("/v1/"+route, body)
			c.Request.Header.Set(responsesLiteHeader, "true")
			result, err := oauthReliabilityContractForward(svc, c, account, route, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, upstream.bodies, 1)
			require.Equal(t, "gpt-5.5", gjson.GetBytes(upstream.lastBody, "model").String())
			require.Empty(t, upstream.lastReq.Header.Get(responsesLiteHeader))
			require.True(t, gjsonToolsContainWebSearch(gjson.GetBytes(upstream.lastBody, "tools")))
			require.Equal(t, "true", c.GetHeader(responsesLiteHeader), "failover retains ingress capability")
			value, _ := c.Get(openAIFinalProtocolDiagnosticKey)
			require.False(t, value.(openAIFinalProtocolDiagnostic).FinalLite)
		})
	}
}

func TestOAuthReliabilityFinalAstraEffortHTTPContract(t *testing.T) {
	for _, route := range []string{"responses", "passthrough", "chat", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, effort := range []string{"none", "minimal", "low"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", route, stream, effort), func(t *testing.T) {
					body := oauthReliabilityContractBody(route, stream)
					var err error
					body, err = sjson.SetBytes(body, "model", "gpt-6-astra")
					require.NoError(t, err)
					field := "reasoning.effort"
					if route == "chat" {
						field = "reasoning_effort"
					} else if route == "messages" {
						field = "output_config.effort"
					}
					body, err = sjson.SetBytes(body, field, effort)
					require.NoError(t, err)
					body, err = sjson.SetBytes(body, "instructions", "fixture")
					require.NoError(t, err)
					original := bytes.Clone(body)
					upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_astra_final", "gpt-6-astra")}
					svc := newOpenAIRejectedFieldTestService(upstream)
					account := newOpenAIOAuthNamespaceTestAccount()
					account.Extra["openai_passthrough"] = route == "passthrough"
					c, recorder := oauthReliabilityContractContext("/v1/"+route, body)
					result, err := oauthReliabilityContractForward(svc, c, account, route, body)
					require.Equal(t, original, body)
					if effort == "low" {
						require.NoError(t, err)
						require.NotNil(t, result)
						require.Len(t, upstream.bodies, 1)
						require.Equal(t, "low", gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
					} else {
						require.Error(t, err)
						require.Nil(t, result)
						require.Empty(t, upstream.bodies)
						require.Equal(t, http.StatusBadRequest, recorder.Code)
						require.Equal(t, "reasoning.effort", gjson.Get(recorder.Body.String(), "error.param").String())
						require.Contains(t, recorder.Body.String(), "does not support reasoning effort")
					}
				})
			}
		}
	}
}

func TestOAuthReliabilityFinalSchemaForwardingPreservesContract(t *testing.T) {
	const schema = `{"properties":{"values":{"items":{"type":"integer","minimum":9007199254740993},"uniqueItems":true}},"minProperties":1,"required":["values"],"additionalProperties":false}`
	for _, route := range []string{"responses", "passthrough", "chat"} {
		for _, strict := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/strict=%t", route, strict), func(t *testing.T) {
				body := oauthReliabilityContractBody(route, false)
				format := fmt.Sprintf(`{"type":"json_schema","name":"fixture","strict":%t,"schema":%s}`, strict, schema)
				field := "text.format"
				if route == "chat" {
					field = "response_format"
					format = fmt.Sprintf(`{"type":"json_schema","json_schema":{"name":"fixture","strict":%t,"schema":%s}}`, strict, schema)
				}
				var err error
				body, err = sjson.SetRawBytes(body, field, []byte(format))
				require.NoError(t, err)
				body, err = sjson.SetBytes(body, "instructions", "Return JSON.")
				require.NoError(t, err)
				upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_schema_final", "gpt-5.6-sol")}
				svc := newOpenAIRejectedFieldTestService(upstream)
				account := newOpenAIOAuthNamespaceTestAccount()
				account.Extra["openai_passthrough"] = route == "passthrough"
				c, _ := oauthReliabilityContractContext("/v1/"+route, body)
				_, err = oauthReliabilityContractForward(svc, c, account, route, body)
				require.NoError(t, err)
				require.Len(t, upstream.bodies, 1)
				require.JSONEq(t, schema, gjson.GetBytes(upstream.lastBody, "text.format.schema").Raw)
				require.Equal(t, "9007199254740993", gjson.GetBytes(upstream.lastBody, "text.format.schema.properties.values.items.minimum").Raw)
				require.Equal(t, strict, gjson.GetBytes(upstream.lastBody, "text.format.strict").Bool())
			})
		}
	}
}

func TestOAuthReliabilityFinalProtocolDiagnosticsAreBounded(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","tools":[{"type":"tool_search","execution":"client"}],"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"PRIVATE_TOOL_NAME","tools":[{"type":"custom","name":"PRIVATE_CHILD","format":{"definition":"PRIVATE_SCHEMA"}}]}]}]}`)
	c, _ := oauthReliabilityContractContext("/v1/responses", body)
	beginOpenAIResponsesValidation(c, body)
	d := recordOpenAIFinalProtocolDiagnostic(c, oauthReliabilityLiteAccount(), "gpt-6-astra", "true", body)
	encoded, err := json.Marshal(d)
	require.NoError(t, err)
	require.Less(t, len(encoded), 512)
	require.NotContains(t, string(encoded), "PRIVATE")
	require.Equal(t, []string{"client"}, d.ToolSources)
	require.Equal(t, []string{"top_level", "additional_tools", "namespace"}, d.ToolCarriers)
	require.Equal(t, []string{"client"}, d.ToolSearchExecution)
	beginOpenAIResponsesValidation(c, []byte(`{"input":[]}`))
	d = recordOpenAIFinalProtocolDiagnostic(c, oauthReliabilityLiteAccount(), "gpt-5.5", "true", []byte(`{"input":[]}`))
	require.False(t, d.FinalLite)
	require.Equal(t, []string{"none"}, d.ToolSources)
	// Header precedence matches the final HTTP metadata projection.
	wsBody := []byte(`{"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`)
	require.False(t, isOpenAIResponsesLiteFinalRequest(oauthReliabilityLiteAccount(), "gpt-6-astra", "false", wsBody))
	require.True(t, isOpenAIResponsesLiteFinalRequest(oauthReliabilityLiteAccount(), "gpt-6-astra", "", wsBody))
}
