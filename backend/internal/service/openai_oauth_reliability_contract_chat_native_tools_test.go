//go:build unit

package service

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const oauthReliabilityCustomDeclaration = `{"type":"custom","custom":{"name":"run_code","description":"Run the supplied source","format":{"type":"grammar","grammar":{"syntax":"lark","definition":"start: /.+/"}},"x_contract":{"integer":9007199254740993}}}`

func oauthReliabilityCustomCompletion(output string) string {
	return `{"type":"response.completed","response":{"id":"resp_custom_contract","object":"response","model":"gpt-5.6-sol","status":"completed","output":` + output + `,"usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}}`
}

func oauthReliabilityAssertNativeCustomResult(t *testing.T, wire string, stream bool, wantInput string) {
	t.Helper()
	if !stream {
		call := gjson.Get(wire, "choices.0.message.tool_calls.0")
		require.Equal(t, "custom", call.Get("type").String())
		require.Equal(t, "call_native", call.Get("id").String())
		require.Equal(t, "run_code", call.Get("custom.name").String())
		require.True(t, call.Get("custom.input").Exists())
		require.Equal(t, wantInput, call.Get("custom.input").String())
		require.False(t, call.Get("function").Exists(), "native custom calls must not be disguised as JSON function arguments")
		require.Equal(t, "tool_calls", gjson.Get(wire, "choices.0.finish_reason").String())
		require.Equal(t, int64(9), gjson.Get(wire, "usage.total_tokens").Int())
		return
	}
	var input strings.Builder
	declarations, finishes, usage := 0, 0, 0
	for _, line := range strings.Split(wire, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			continue
		}
		require.True(t, gjson.Valid(data), data)
		for _, call := range gjson.Get(data, "choices.0.delta.tool_calls").Array() {
			require.Equal(t, int64(0), call.Get("index").Int())
			require.False(t, call.Get("function").Exists())
			if call.Get("type").String() != "" {
				declarations++
				require.Equal(t, "custom", call.Get("type").String())
				require.Equal(t, "call_native", call.Get("id").String())
				require.Equal(t, "run_code", call.Get("custom.name").String())
			}
			input.WriteString(call.Get("custom.input").String())
		}
		if gjson.Get(data, "choices.0.finish_reason").String() == "tool_calls" {
			finishes++
		}
		if gjson.Get(data, "usage.total_tokens").Int() == 9 {
			usage++
		}
	}
	require.Equal(t, wantInput, input.String(), "delta, item.done and terminal copies must not duplicate the custom input")
	require.Equal(t, 1, declarations)
	require.Equal(t, 1, finishes)
	require.Equal(t, 1, usage)
	require.Equal(t, 1, strings.Count(wire, "data: [DONE]"))
}

// The real Chat messages path is intentional: a Responses input body skips
// the typed Chat declaration converter and cannot reveal silently lost tools.
func TestOAuthReliabilityContractChatNativeCustomRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, lite := range []bool{false, true} {
			for _, shape := range []string{"terminal_only", "delta_empty_terminal", "delta_full_terminal", "item_done_only", "empty_input"} {
				t.Run(fmt.Sprintf("stream=%t/lite=%t/%s", stream, lite, shape), func(t *testing.T) {
					body := oauthReliabilityContractBody("chat", stream)
					body, err := sjson.SetRawBytes(body, "tools", []byte(`[`+oauthReliabilityCustomDeclaration+`]`))
					require.NoError(t, err)
					body, err = sjson.SetRawBytes(body, "tool_choice", []byte(`{"type":"custom","custom":{"name":"run_code"}}`))
					require.NoError(t, err)
					body, err = sjson.SetRawBytes(body, "stream_options", []byte(`{"include_usage":true}`))
					require.NoError(t, err)
					original := bytes.Clone(body)
					input := "print(1)"
					if shape == "empty_input" {
						input = ""
					}
					item := fmt.Sprintf(`{"type":"custom_tool_call","id":"ctc_native","call_id":"call_native","name":"run_code","input":%q,"status":"completed"}`, input)
					var frames []string
					terminal := "[" + item + "]"
					if strings.HasPrefix(shape, "delta_") {
						frames = append(frames,
							`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_native","call_id":"call_native","name":"run_code","input":""}}`,
							`{"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"print("}`,
							`{"type":"response.custom_tool_call_input.done","output_index":0,"input":"print(1)"}`,
							`{"type":"response.output_item.done","output_index":0,"item":`+item+`}`)
						if shape == "delta_empty_terminal" {
							terminal = "[]"
						}
					} else if shape == "item_done_only" {
						frames = append(frames, `{"type":"response.output_item.done","output_index":0,"item":`+item+`}`)
						terminal = "[]"
					}
					frames = append(frames, oauthReliabilityCustomCompletion(terminal))
					upstream := &httpUpstreamRecorder{resp: oauthReliabilityContractSSE(frames...)}
					svc := newOpenAIRejectedFieldTestService(upstream)
					c, recorder := oauthReliabilityContractContext("/v1/chat/completions", body)
					if lite {
						c.Request.Header.Set(responsesLiteHeader, "true")
					}
					result, err := svc.ForwardAsChatCompletions(c.Request.Context(), c, newOpenAIOAuthNamespaceTestAccount(), body, "", "")
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Len(t, upstream.bodies, 1)
					require.Equal(t, http.StatusOK, recorder.Code)
					tool := gjson.GetBytes(upstream.lastBody, `tools.#(type=="custom")`)
					require.Equal(t, "run_code", tool.Get("name").String())
					require.Equal(t, "grammar", tool.Get("format.type").String())
					require.Equal(t, "lark", tool.Get("format.syntax").String())
					require.Equal(t, "start: /.+/", tool.Get("format.definition").String())
					require.False(t, tool.Get("format.grammar").Exists())
					require.Equal(t, "9007199254740993", tool.Get("x_contract.integer").Raw)
					require.Equal(t, "custom", gjson.GetBytes(upstream.lastBody, "tool_choice.type").String())
					require.Equal(t, "run_code", gjson.GetBytes(upstream.lastBody, "tool_choice.name").String())
					require.Equal(t, original, body)
					require.Equal(t, 7, result.Usage.InputTokens)
					require.Equal(t, 2, result.Usage.OutputTokens)
					oauthReliabilityAssertNativeCustomResult(t, recorder.Body.String(), stream, input)
				})
			}
		}
	}
}

func TestOAuthReliabilityContractChatNativeCustomHistory(t *testing.T) {
	for _, output := range []string{"tool output", ""} {
		t.Run(fmt.Sprintf("empty=%t", output == ""), func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"run"},{"role":"assistant","tool_calls":[{"id":"call_native","type":"custom","custom":{"name":"run_code","input":""}}]},{"role":"tool","tool_call_id":"call_native","content":%q}],"tools":[%s]}`, output, oauthReliabilityCustomDeclaration))
			original := bytes.Clone(body)
			upstream := &httpUpstreamRecorder{resp: oauthReliabilityContractSSE(oauthReliabilityContractCompleted)}
			c, recorder := oauthReliabilityContractContext("/v1/chat/completions", body)
			result, err := newOpenAIRejectedFieldTestService(upstream).ForwardAsChatCompletions(c.Request.Context(), c, newOpenAIOAuthNamespaceTestAccount(), body, "", "")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, upstream.bodies, 1)
			call := gjson.GetBytes(upstream.lastBody, `input.#(type=="custom_tool_call")`)
			// Existing inline Codex replay may canonicalize call_* to ctc_*.
			// The external contract is a stable call/result pair; the separate
			// round-trip test distinguishes upstream item.id from call_id.
			require.NotEmpty(t, call.Get("call_id").String())
			require.Equal(t, "run_code", call.Get("name").String())
			require.Equal(t, gjson.String, call.Get("input").Type)
			require.Equal(t, "", call.Get("input").String())
			resultItem := gjson.GetBytes(upstream.lastBody, `input.#(type=="custom_tool_call_output")`)
			require.Equal(t, call.Get("call_id").String(), resultItem.Get("call_id").String())
			require.Equal(t, gjson.String, resultItem.Get("output").Type)
			require.Equal(t, output, resultItem.Get("output").String())
			require.False(t, gjson.GetBytes(upstream.lastBody, `input.#(type=="function_call")`).Exists())
			require.Contains(t, recorder.Body.String(), "contract answer")
			require.Equal(t, original, body)
		})
	}
}

func TestOAuthReliabilityContractChatUnsupportedToolRejectsBeforeExecution(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct{ name, tool, param string }{
			{"client_search", `{"type":"tool_search","execution":"client","parameters":{"type":"object"}}`, "tools[0].type"},
			{"server_search", `{"type":"tool_search","execution":"server"}`, "tools[0].type"},
			{"hosted_image", `{"type":"image_generation"}`, "tools[0].type"},
			{"unknown", `{"type":"PRIVATE_TOOL"}`, "tools[0].type"},
			{"ambiguous_custom", `{"type":"custom","name":"PRIVATE_VALUE","custom":{"name":"run_code"}}`, "tools[0].custom"},
			{"ambiguous_grammar", `{"type":"custom","custom":{"name":"run_code","format":{"type":"grammar","syntax":"regex","grammar":{"syntax":"lark","definition":"PRIVATE_GRAMMAR"}}}}`, "tools[0].custom.format"},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tc.name), func(t *testing.T) {
				body := oauthReliabilityContractBody("chat", stream)
				body, err := sjson.SetRawBytes(body, "tools", []byte(`[`+tc.tool+`]`))
				require.NoError(t, err)
				upstream := &httpUpstreamRecorder{}
				c, recorder := oauthReliabilityContractContext("/v1/chat/completions", body)
				result, err := newOpenAIRejectedFieldTestService(upstream).ForwardAsChatCompletions(c.Request.Context(), c, newOpenAIOAuthNamespaceTestAccount(), body, "", "")
				require.Error(t, err)
				require.Nil(t, result)
				require.Empty(t, upstream.bodies)
				require.Equal(t, http.StatusBadRequest, recorder.Code)
				require.Equal(t, "invalid_request_error", gjson.Get(recorder.Body.String(), "error.type").String())
				require.Equal(t, tc.param, gjson.Get(recorder.Body.String(), "error.param").String())
				require.True(t, IsResponseCommitted(c))
				require.NotContains(t, recorder.Body.String(), "PRIVATE")
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover))
			})
		}
	}
}

func TestOAuthReliabilityContractChatResponsesShapeSearchCapability(t *testing.T) {
	for _, route := range []string{"chat", "responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", route, stream), func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":%t,"input":[{"type":"message","role":"user","content":"fixture"}],"tools":[{"type":"tool_search","execution":"client","parameters":{"type":"object"}}]}`, stream))
				upstream := &httpUpstreamRecorder{resp: oauthReliabilityContractSSE(oauthReliabilityContractCompleted)}
				c, recorder := oauthReliabilityContractContext("/v1/"+route, body)
				c.Request.Header.Set(responsesLiteHeader, "true")
				result, err := oauthReliabilityContractForward(newOpenAIRejectedFieldTestService(upstream), c, newOpenAIOAuthNamespaceTestAccount(), route, body)
				if route == "chat" {
					require.Error(t, err)
					require.Nil(t, result)
					require.Empty(t, upstream.bodies)
					require.Equal(t, http.StatusBadRequest, recorder.Code)
					require.Equal(t, "tools[0].type", gjson.Get(recorder.Body.String(), "error.param").String())
					return
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Len(t, upstream.bodies, 1)
				require.Equal(t, "client", gjson.GetBytes(upstream.lastBody, `tools.#(type=="tool_search").execution`).String())
			})
		}
	}
}

func TestOAuthReliabilityContractChatCustomProgressFailureDoesNotReplay(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			body := oauthReliabilityContractBody("chat", stream)
			body, err := sjson.SetRawBytes(body, "tools", []byte(`[`+oauthReliabilityCustomDeclaration+`]`))
			require.NoError(t, err)
			upstream := &httpUpstreamRecorder{resp: oauthReliabilityContractSSE(
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","call_id":"call_native","name":"run_code","input":""}}`,
				`{"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"print("}`,
				oauthReliabilityContractFailed(oauthReliabilityContractSchemaError, `{"input_tokens":7,"output_tokens":2}`))}
			c, recorder := oauthReliabilityContractContext("/v1/chat/completions", body)
			result, err := newOpenAIRejectedFieldTestService(upstream).ForwardAsChatCompletions(c.Request.Context(), c, newOpenAIOAuthNamespaceTestAccount(), body, "", "")
			require.Error(t, err)
			require.NotNil(t, result)
			require.Equal(t, 7, result.Usage.InputTokens)
			require.Equal(t, 2, result.Usage.OutputTokens)
			require.Len(t, upstream.bodies, 1)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover))
			require.NotContains(t, recorder.Body.String(), `"type":"function"`)
			require.NotContains(t, recorder.Body.String(), `"finish_reason":"tool_calls"`)
		})
	}
}

func TestOAuthReliabilityContractChatExistingFunctionAndHostedToolsPreserved(t *testing.T) {
	for _, tc := range []struct{ name, tool, expectedType string }{
		{"function", `{"type":"function","function":{"name":"lookup","parameters":{"properties":{"value":{"minimum":9007199254740993}}},"strict":false}}`, "function"},
		{"hosted_search", `{"type":"web_search","external_web_access":false,"filters":{"allowed_domains":["example.invalid"]}}`, "web_search"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := oauthReliabilityContractBody("chat", false)
			body, err := sjson.SetRawBytes(body, "tools", []byte(`[`+tc.tool+`]`))
			require.NoError(t, err)
			upstream := &httpUpstreamRecorder{resp: oauthReliabilityContractSSE(oauthReliabilityContractCompleted)}
			c, recorder := oauthReliabilityContractContext("/v1/chat/completions", body)
			result, err := newOpenAIRejectedFieldTestService(upstream).ForwardAsChatCompletions(c.Request.Context(), c, newOpenAIOAuthNamespaceTestAccount(), body, "", "")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, upstream.bodies, 1)
			tool := gjson.GetBytes(upstream.lastBody, `tools.#(type=="`+tc.expectedType+`")`)
			if tc.expectedType == "function" {
				require.Equal(t, "lookup", tool.Get("name").String())
				require.Equal(t, "9007199254740993", tool.Get("parameters.properties.value.minimum").Raw)
				require.False(t, tool.Get("strict").Bool())
			} else {
				require.JSONEq(t, tc.tool, tool.Raw)
			}
			require.Contains(t, recorder.Body.String(), "contract answer")
		})
	}
}
