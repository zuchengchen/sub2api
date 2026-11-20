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

const oauthLiteSearchHistoryItem = `{"type":"web_search_call","id":"ws_history","status":"completed","action":{"type":"search","query":"fixture","sources":[{"type":"url","url":"https://example.org"}]}}`

func TestOAuthLiteHostedSearchHistoryForwardedOnStandardHTTP(t *testing.T) {
	pinCodexTestVersion(t, "0.166.0")
	for _, tc := range []struct {
		route, path    string
		header, marker bool
		stream         bool
		historyIndex   int
	}{
		{"responses", "/v1/responses", true, true, true, 184},
		{"metadata_with_image_bridge", "/v1/responses", false, true, true, 1},
		{"passthrough", "/v1/responses", false, true, false, 1},
		{"chat", "/v1/chat/completions", true, false, false, 1},
	} {
		t.Run(tc.route, func(t *testing.T) {
			var input []json.RawMessage
			for i := 0; i < tc.historyIndex; i++ {
				input = append(input, json.RawMessage(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"turn %d"}]}`, i)))
			}
			input = append(input, json.RawMessage(oauthLiteSearchHistoryItem))
			if tc.route == "passthrough" {
				input = append(input,
					json.RawMessage(`{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"catalog","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}]}`),
					json.RawMessage(`{"type":"function_call","id":"fc_lookup","call_id":"call_lookup","namespace":"catalog","name":"lookup","arguments":"{}"}`),
					json.RawMessage(`{"type":"function_call_output","call_id":"call_lookup","namespace":"catalog","output":"retained"}`))
			}
			input = append(input, json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}`))
			request := map[string]any{
				"model": "gpt-6-astra", "instructions": "fixture", "stream": tc.stream,
				"input": input, "tools": []any{}, "parallel_tool_calls": false,
				"reasoning": map[string]string{"context": "all_turns"},
			}
			if tc.route != "responses" {
				request["tool_choice"] = "auto"
			}
			if tc.marker {
				request["client_metadata"] = map[string]string{responsesLiteWSMetadataKey: "true", "fixture_key": "keep"}
			}
			body, err := json.Marshal(request)
			require.NoError(t, err)
			original := bytes.Clone(body)
			c, recorder := oauthReliabilityContractContext(tc.path, body)
			if tc.header {
				c.Request.Header.Set(responsesLiteHeader, "true")
			}
			account := newCodexSynthTestAccount(9232, AccountTypeOAuth)
			account.Extra["openai_passthrough"] = tc.route == "passthrough"
			upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_history", "gpt-6-astra")}
			svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig(), httpUpstream: upstream, tiboRouteDisabled: true}
			if tc.route == "metadata_with_image_bridge" {
				svc.cfg.Gateway.ForceCodexCLI = true
				svc.cfg.Gateway.CodexImageGenerationBridgeEnabled = true
			}
			result, err := oauthReliabilityContractForward(svc, c, account, tc.route, body)
			require.NoError(t, err, recorder.Body.String())
			require.NotNil(t, result)
			require.Equal(t, http.StatusOK, recorder.Code)
			require.Len(t, upstream.bodies, 1)
			require.Equal(t, astraProCodexResponsesURL, upstream.lastReq.URL.String())
			require.Empty(t, upstream.lastReq.Header.Get(responsesLiteHeader))
			sent := gjson.ParseBytes(upstream.lastBody)
			require.False(t, sent.Get("client_metadata."+responsesLiteWSMetadataKey).Exists())
			if tc.marker {
				require.Equal(t, "keep", sent.Get("client_metadata.fixture_key").String())
			}
			require.Equal(t, "gpt-6-astra", sent.Get("model").String())
			require.Equal(t, "all_turns", sent.Get("reasoning.context").String())
			require.Equal(t, gjson.False, sent.Get("parallel_tool_calls").Type)
			require.JSONEq(t, `[{"type":"web_search","external_web_access":false}]`, sent.Get("tools").Raw)
			if tc.route == "passthrough" {
				require.Equal(t, "auto", sent.Get("tool_choice").String(), "caller-declared tools remain callable")
			} else {
				require.Equal(t, "none", sent.Get("tool_choice").String())
			}
			// Generic synthesis can prepend its Codex instructions. Every
			// caller item must still follow them in order, including input[184].
			sentInput := sent.Get("input").Array()
			require.GreaterOrEqual(t, len(sentInput), len(input))
			for i, item := range sentInput[len(sentInput)-len(input):] {
				require.JSONEq(t, string(input[i]), item.Raw)
			}
			require.Equal(t, original, body)
			require.Equal(t, tc.header, isOpenAIResponsesLiteHeader(c.GetHeader(responsesLiteHeader)))
			value, _ := c.Get(openAIFinalProtocolDiagnosticKey)
			require.False(t, value.(openAIFinalProtocolDiagnostic).FinalLite)
		})
	}
}

func TestOAuthLiteHostedSearchHistoryCompactHTTP(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, native := range []bool{false, true} {
			t.Run(fmt.Sprintf("passthrough=%t/native=%t", passthrough, native), func(t *testing.T) {
				path := "/v1/responses/compact"
				body := []byte(`{"model":"gpt-6-astra","instructions":"fixture","input":[` + oauthLiteSearchHistoryItem + `],"tools":[]}`)
				upstream := &httpUpstreamRecorder{resp: newOpenAIRejectedFieldTestResponse(http.StatusOK, `{"id":"resp_compact","status":"completed","output":[{"type":"compaction","encrypted_content":"fixture"}],"usage":{"input_tokens":1,"output_tokens":1}}`)}
				if native {
					path = "/v1/responses"
					var err error
					body, err = sjson.SetBytes(body, "stream", true)
					require.NoError(t, err)
					body, err = sjson.SetRawBytes(body, "input.-1", []byte(`{"type":"compaction_trigger"}`))
					require.NoError(t, err)
					upstream.responses = []*http.Response{
						newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, `{"error":{"code":"context_length_exceeded","message":"context window exceeded"}}`),
						nativeCompactionSSEResponse(true),
					}
				}
				c, recorder := oauthReliabilityContractContext(path, body)
				c.Request.Header.Set(responsesLiteHeader, "true")
				if native {
					MarkOpenAINativeCompactionV2(c)
				}
				account := newOpenAIOAuthNamespaceTestAccount()
				account.Extra["openai_passthrough"] = passthrough
				svc := newOpenAIRejectedFieldTestService(upstream)
				if native {
					svc.cfg.Gateway.OpenAICompactModel = "gpt-6-sol"
				}
				result, err := svc.Forward(c.Request.Context(), c, account, body)
				require.NoError(t, err, recorder.Body.String())
				require.NotNil(t, result)
				if native {
					require.Len(t, upstream.bodies, 2)
					require.Equal(t, "gpt-6-sol", gjson.GetBytes(upstream.bodies[1], "model").String())
				} else {
					require.Len(t, upstream.bodies, 1)
				}
				require.Equal(t, "gpt-6-astra", gjson.GetBytes(upstream.bodies[0], "model").String())
				for i, wire := range upstream.bodies {
					require.Empty(t, upstream.requests[i].Header.Get(responsesLiteHeader))
					require.JSONEq(t, oauthLiteSearchHistoryItem, gjson.GetBytes(wire, `input.#(type=="web_search_call")`).Raw)
					if native {
						require.Equal(t, astraProCodexResponsesURL, upstream.requests[i].URL.String())
						require.JSONEq(t, `[{"type":"web_search","external_web_access":false}]`, gjson.GetBytes(wire, "tools").Raw)
						require.Equal(t, "none", gjson.GetBytes(wire, "tool_choice").String())
						items := gjson.GetBytes(wire, "input").Array()
						require.Equal(t, "compaction_trigger", items[len(items)-1].Get("type").String())
					} else {
						require.Equal(t, astraProCodexResponsesURL+"/compact", upstream.requests[i].URL.String())
						require.Empty(t, gjson.GetBytes(wire, "tools").Array())
						require.False(t, gjson.GetBytes(wire, "tool_choice").Exists())
					}
				}
				require.Equal(t, "true", c.GetHeader(responsesLiteHeader))
			})
		}
	}
}

func TestOAuthLiteHostedSearchHistoryFailoverPreservesAPIKeyRequest(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","stream":false,"input":[` + oauthLiteSearchHistoryItem + `],"tools":[],"tool_choice":"auto","client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`)
	original := bytes.Clone(body)
	c := newOpenAIRejectedFieldTestContext(body)
	c.Request.Header.Set(responsesLiteHeader, "true")
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		newOpenAIRejectedFieldTestResponse(http.StatusServiceUnavailable, `{"error":{"code":"server_error","message":"temporarily unavailable"}}`),
		openAICompatSSECompletedResponse("resp_fallback", "gpt-6-astra"),
	}}
	svc := newOpenAIRejectedFieldTestService(upstream)
	_, err := svc.Forward(c.Request.Context(), c, newOpenAIOAuthNamespaceTestAccount(), body)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	result, err := svc.Forward(c.Request.Context(), c, newOpenAIRejectedFieldTestAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.bodies, 2)
	require.Empty(t, upstream.requests[0].Header.Get(responsesLiteHeader))
	require.Equal(t, "none", gjson.GetBytes(upstream.bodies[0], "tool_choice").String())
	require.Equal(t, "true", upstream.requests[1].Header.Get(responsesLiteHeader))
	require.True(t, isOpenAIResponsesLiteWebSocketPayload(upstream.bodies[1]))
	require.JSONEq(t, "[]", gjson.GetBytes(upstream.bodies[1], "tools").Raw)
	require.Equal(t, "auto", gjson.GetBytes(upstream.bodies[1], "tool_choice").String())
	require.JSONEq(t, oauthLiteSearchHistoryItem, gjson.GetBytes(upstream.bodies[1], "input.0").Raw)
	require.Equal(t, original, body)
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
