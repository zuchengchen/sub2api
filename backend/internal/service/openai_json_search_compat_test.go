package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// The transport models the observed upstream rejection, rather than returning
// success for every request and checking the synthesis helper in isolation.
// JSON Schema is covered by the no-added-tools contract below; this fixture
// does not claim the provider rejects every JSON Schema/search combination.
type jsonSearchCompatUpstream struct {
	bodies   [][]byte
	headers  []http.Header
	statuses []int
}

func (u *jsonSearchCompatUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	if req.Header.Get("Content-Encoding") == "zstd" {
		decoder, decodeErr := zstd.NewReader(nil)
		if decodeErr != nil {
			return nil, decodeErr
		}
		body, err = decoder.DecodeAll(body, nil)
		decoder.Close()
		if err != nil {
			return nil, err
		}
	}
	u.bodies = append(u.bodies, append([]byte(nil), body...))
	u.headers = append(u.headers, req.Header.Clone())
	if gjson.GetBytes(body, "text.format.type").String() == "json_object" && jsonSearchCompatHasSearch(body) {
		u.statuses = append(u.statuses, http.StatusBadRequest)
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"json-search-rejected"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"Web Search cannot be used with JSON mode.","param":"tools"}}`)),
		}, nil
	}
	u.statuses = append(u.statuses, http.StatusOK)
	response := `{"id":"resp_json_search","object":"response","model":"gpt-6-luna","status":"completed","output":[{"type":"message","id":"msg_json_search","role":"assistant","status":"completed","content":[{"type":"output_text","text":"{\"ok\":true}","annotations":[]}]}],"usage":{"input_tokens":5,"output_tokens":4,"total_tokens":9}}`
	sse := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_json_search\",\"model\":\"gpt-6-luna\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_json_search\",\"output_index\":0,\"content_index\":0,\"delta\":\"{\\\"ok\\\":true}\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":" + response + "}\n\ndata: [DONE]\n\n"
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse))}, nil
}

func (u *jsonSearchCompatUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

// Inspect protocol declarations independently of the production synthesis
// predicate, including the additional_tools carrier used by Responses Lite.
func jsonSearchCompatDeclarations(body []byte) []gjson.Result {
	tools := append([]gjson.Result(nil), gjson.GetBytes(body, "tools").Array()...)
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("type").String() == "additional_tools" {
			tools = append(tools, item.Get("tools").Array()...)
		}
	}
	return tools
}

func jsonSearchCompatHasSearch(body []byte) bool {
	for _, tool := range jsonSearchCompatDeclarations(body) {
		switch tool.Get("type").String() {
		case "web_search", "web_search_preview", "web_search_preview_2025_03_11":
			return true
		}
	}
	return false
}

func jsonSearchCompatFormat(kind string) map[string]any {
	format := map[string]any{"type": kind}
	if kind == "json_schema" {
		format["name"] = "answer"
		format["strict"] = true
		format["schema"] = map[string]any{
			"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
			"required": []any{"ok"}, "additionalProperties": false,
		}
	}
	return format
}

func jsonSearchCompatRequest(route, format string, stream bool) map[string]any {
	request := map[string]any{"model": "gpt-6-luna", "stream": stream}
	if route == "chat" {
		request["messages"] = []any{
			map[string]any{"role": "system", "content": "Return valid JSON without losing the requested schema."},
			map[string]any{"role": "user", "content": "Return a JSON object with ok=true."},
		}
		responseFormat := jsonSearchCompatFormat(format)
		if format == "json_schema" {
			delete(responseFormat, "type")
			responseFormat = map[string]any{"type": "json_schema", "json_schema": responseFormat}
		}
		request["response_format"] = responseFormat
	} else {
		request["input"] = []any{map[string]any{"type": "message", "role": "user", "content": "Return a JSON object with ok=true."}}
		request["text"] = map[string]any{"format": jsonSearchCompatFormat(format)}
	}
	return request
}

func jsonSearchCompatForward(t *testing.T, route string, request map[string]any, account *Account) (*jsonSearchCompatUpstream, *httptest.ResponseRecorder, *OpenAIForwardResult, error) {
	t.Helper()
	body, err := json.Marshal(request)
	require.NoError(t, err)
	original := append([]byte(nil), body...)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	path := "/v1/responses"
	if route == "chat" {
		path = "/v1/chat/completions"
	}
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "AsyncOpenAI/Python 3.42.1")
	if route == "lite" {
		c.Request.Header.Set(responsesLiteHeader, "true")
	}
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	upstream := &jsonSearchCompatUpstream{}
	cfg := codexSynthEnabledConfig()
	cfg.Security.URLAllowlist.Enabled = false
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, tiboRouteDisabled: true}
	if account == nil {
		account = newCodexSynthTestAccount(77117, AccountTypeOAuth)
	}
	var result *OpenAIForwardResult
	if route == "chat" {
		result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-6-luna")
	} else {
		result, err = svc.Forward(context.Background(), c, account, body)
	}
	require.Equal(t, original, body, "forwarding must preserve the canonical request for retry/failover")
	return upstream, recorder, result, err
}

func requireJSONSearchCompatSuccess(t *testing.T, route string, stream bool, upstream *jsonSearchCompatUpstream, recorder *httptest.ResponseRecorder, result *OpenAIForwardResult, err error) []byte {
	t.Helper()
	require.NoError(t, err, "unexpected upstream statuses: %v; downstream: %s", upstream.statuses, recorder.Body.String())
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []int{http.StatusOK}, upstream.statuses)
	require.Len(t, upstream.bodies, 1)
	if stream {
		require.Contains(t, recorder.Header().Get("Content-Type"), "text/event-stream")
		require.Contains(t, recorder.Body.String(), `\"ok\":true`)
	} else if route == "chat" {
		require.JSONEq(t, `{"ok":true}`, gjson.Get(recorder.Body.String(), "choices.0.message.content").String())
	} else {
		require.JSONEq(t, `{"ok":true}`, gjson.Get(recorder.Body.String(), "output.0.content.0.text").String())
	}
	if route == "lite" {
		require.Equal(t, "true", upstream.headers[0].Get(responsesLiteHeader))
	}
	return upstream.bodies[0]
}

func TestJSONSearchCompatibility_ForwardWithoutDeclaredTools(t *testing.T) {
	pinCodexTestVersion(t, "0.166.0")
	for _, route := range []string{"chat", "responses", "lite"} {
		for _, format := range []string{"json_object", "json_schema"} {
			for _, emptyTools := range []bool{false, true} {
				for _, stream := range []bool{false, true} {
					t.Run(route+"/"+format+"/empty_tools_"+strconv.FormatBool(emptyTools)+"/stream_"+strconv.FormatBool(stream), func(t *testing.T) {
						request := jsonSearchCompatRequest(route, format, stream)
						if emptyTools {
							request["tools"] = []any{}
						}
						upstream, recorder, result, err := jsonSearchCompatForward(t, route, request, nil)
						sent := requireJSONSearchCompatSuccess(t, route, stream, upstream, recorder, result, err)
						require.Empty(t, jsonSearchCompatDeclarations(sent), "a structured-output request must not acquire implicit hosted tools")
						formatJSON, marshalErr := json.Marshal(jsonSearchCompatFormat(format))
						require.NoError(t, marshalErr)
						require.JSONEq(t, string(formatJSON), gjson.GetBytes(sent, "text.format").Raw)
						require.True(t, gjson.GetBytes(sent, "stream").Bool(), "Codex streams upstream for either client delivery mode")
					})
				}
			}
		}
	}
}

func TestJSONSearchCompatibility_TextAndNoneControls(t *testing.T) {
	pinCodexTestVersion(t, "0.166.0")
	for _, route := range []string{"chat", "responses", "lite"} {
		for _, format := range []string{"text", "json_object"} {
			for _, stream := range []bool{false, true} {
				t.Run(route+"/"+format+"/stream_"+strconv.FormatBool(stream), func(t *testing.T) {
					request := jsonSearchCompatRequest(route, format, stream)
					request["tool_choice"] = "none"
					upstream, recorder, result, err := jsonSearchCompatForward(t, route, request, nil)
					sent := requireJSONSearchCompatSuccess(t, route, stream, upstream, recorder, result, err)
					require.False(t, jsonSearchCompatHasSearch(sent))
					require.Equal(t, "none", gjson.GetBytes(sent, "tool_choice").String())
					if format == "text" {
						delete(request, "tool_choice")
						upstream, recorder, result, err = jsonSearchCompatForward(t, route, request, nil)
						sent = requireJSONSearchCompatSuccess(t, route, stream, upstream, recorder, result, err)
						require.Equal(t, route != "lite", jsonSearchCompatHasSearch(sent), "only non-Lite text requests synthesize hosted search")
					}
				})
			}
		}
	}
}

func TestJSONSearchCompatibility_FunctionHistoryPreservesContract(t *testing.T) {
	pinCodexTestVersion(t, "0.166.0")
	for _, route := range []string{"chat", "responses", "lite"} {
		for _, format := range []string{"json_object", "json_schema"} {
			for _, stream := range []bool{false, true} {
				t.Run(route+"/"+format+"/stream_"+strconv.FormatBool(stream), func(t *testing.T) {
					request := jsonSearchCompatRequest(route, format, stream)
					function := map[string]any{"name": "lookup", "description": "Look up the supplied key", "strict": true,
						"parameters": map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}}, "required": []any{"key"}, "additionalProperties": false}}
					if route == "chat" {
						request["tools"] = []any{map[string]any{"type": "function", "function": function}}
						request["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}
						request["messages"] = append(request["messages"].([]any),
							map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "fc_contract", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"key":"original"}`}}}},
							map[string]any{"role": "tool", "tool_call_id": "fc_contract", "content": "result-preserved"},
							map[string]any{"role": "user", "content": "Continue with the same function contract and return JSON."})
					} else {
						function["type"] = "function"
						request["tools"] = []any{function}
						request["tool_choice"] = map[string]any{"type": "function", "name": "lookup"}
						request["input"] = append(request["input"].([]any),
							map[string]any{"type": "function_call", "id": "fc_item", "call_id": "fc_contract", "name": "lookup", "arguments": `{"key":"original"}`},
							map[string]any{"type": "function_call_output", "call_id": "fc_contract", "output": "result-preserved"},
							map[string]any{"type": "message", "role": "user", "content": "Continue with the same function contract and return JSON."})
					}
					request["parallel_tool_calls"] = false
					upstream, recorder, result, err := jsonSearchCompatForward(t, route, request, nil)
					sent := requireJSONSearchCompatSuccess(t, route, stream, upstream, recorder, result, err)
					declarations := jsonSearchCompatDeclarations(sent)
					require.Len(t, declarations, 1)
					require.Equal(t, "lookup", declarations[0].Get("name").String())
					require.True(t, declarations[0].Get("strict").Bool())
					schema, marshalErr := json.Marshal(function["parameters"])
					require.NoError(t, marshalErr)
					require.JSONEq(t, string(schema), declarations[0].Get("parameters").Raw)
					require.JSONEq(t, `{"type":"function","name":"lookup"}`, gjson.GetBytes(sent, "tool_choice").Raw)
					require.False(t, gjson.GetBytes(sent, "parallel_tool_calls").Bool())
					call := gjson.GetBytes(sent, `input.#(type=="function_call")`)
					output := gjson.GetBytes(sent, `input.#(type=="function_call_output")`)
					require.Equal(t, "lookup", call.Get("name").String())
					require.Equal(t, "fc_contract", call.Get("call_id").String())
					require.Equal(t, call.Get("call_id").String(), output.Get("call_id").String())
					require.JSONEq(t, `{"key":"original"}`, call.Get("arguments").String())
					require.Equal(t, "result-preserved", output.Get("output").String())
					require.Equal(t, format, gjson.GetBytes(sent, "text.format.type").String())
				})
			}
		}
	}
}

func TestJSONSearchCompatibility_ExplicitSearchIsNotSilentlyRemoved(t *testing.T) {
	pinCodexTestVersion(t, "0.166.0")
	for _, route := range []string{"chat", "responses", "lite"} {
		for _, stream := range []bool{false, true} {
			t.Run(route+"/stream_"+strconv.FormatBool(stream), func(t *testing.T) {
				request := jsonSearchCompatRequest(route, "json_object", stream)
				search := map[string]any{"type": "web_search"}
				if route == "lite" {
					request["input"] = append([]any{map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{search}}}, request["input"].([]any)...)
				} else {
					request["tools"] = []any{search}
				}
				request["tool_choice"] = "auto"
				upstream, recorder, _, err := jsonSearchCompatForward(t, route, request, nil)
				require.Error(t, err, "an explicit incompatible contract must not be silently turned into success")
				if route == "lite" {
					require.Equal(t, http.StatusBadRequest, recorder.Code)
					require.Empty(t, upstream.bodies)
					require.Equal(t, "input[0].tools[0].type", gjson.Get(recorder.Body.String(), "error.param").String())
					return
				}
				require.Equal(t, []int{http.StatusBadRequest}, upstream.statuses)
				require.Len(t, upstream.bodies, 1, "the deterministic rejection must not be replayed with altered tools")
				require.True(t, jsonSearchCompatHasSearch(upstream.bodies[0]))
				require.Equal(t, "json_object", gjson.GetBytes(upstream.bodies[0], "text.format.type").String())
				require.Equal(t, "auto", gjson.GetBytes(upstream.bodies[0], "tool_choice").String())
				require.Contains(t, recorder.Body.String(), "Web Search cannot be used with JSON mode.")
			})
		}
	}
}

func TestJSONSearchCompatibility_SearchHistoryRetainsItsDeclaration(t *testing.T) {
	pinCodexTestVersion(t, "0.166.0")
	for _, route := range []string{"responses", "lite"} {
		for _, format := range []string{"text", "json_object"} {
			for _, stream := range []bool{false, true} {
				t.Run(route+"/"+format+"/stream_"+strconv.FormatBool(stream), func(t *testing.T) {
					request := jsonSearchCompatRequest(route, format, stream)
					history := map[string]any{"type": "web_search_call", "id": "ws_history", "status": "completed", "action": map[string]any{"type": "search", "query": "preserved history query"}}
					request["input"] = append([]any{history}, request["input"].([]any)...)
					request["tools"] = []any{}
					request["tool_choice"] = "none"
					account := newCodexSynthTestAccount(77118, AccountTypeOAuth)
					account.Extra[codexClientCapabilityExtraKey] = "compatible"
					upstream, recorder, result, err := jsonSearchCompatForward(t, route, request, account)
					if route == "lite" {
						require.Error(t, err)
						require.Nil(t, result)
						require.Empty(t, upstream.bodies)
						require.Equal(t, http.StatusBadRequest, recorder.Code)
						require.Equal(t, "input[0].type", gjson.Get(recorder.Body.String(), "error.param").String())
						return
					}
					if format == "text" {
						requireJSONSearchCompatSuccess(t, route, stream, upstream, recorder, result, err)
					} else {
						// History repair is a separate compatibility requirement. Do not
						// discard that history/declaration to promise JSON-mode success.
						require.Error(t, err)
						require.Equal(t, []int{http.StatusBadRequest}, upstream.statuses)
						require.Contains(t, recorder.Body.String(), "Web Search cannot be used with JSON mode.")
					}
					require.Len(t, upstream.bodies, 1)
					sent := upstream.bodies[0]
					declarations := jsonSearchCompatDeclarations(sent)
					require.Len(t, declarations, 1)
					require.Equal(t, "web_search", declarations[0].Get("type").String())
					require.True(t, declarations[0].Get("external_web_access").Exists())
					require.False(t, declarations[0].Get("external_web_access").Bool())
					historyJSON, marshalErr := json.Marshal(history)
					require.NoError(t, marshalErr)
					require.JSONEq(t, string(historyJSON), gjson.GetBytes(sent, `input.#(type=="web_search_call")`).Raw)
					require.Equal(t, "none", gjson.GetBytes(sent, "tool_choice").String())
					require.Equal(t, format, gjson.GetBytes(sent, "text.format.type").String())
				})
			}
		}
	}
}

func TestJSONSearchCompatibility_APIKeyContinuationPreservesAnchor(t *testing.T) {
	pinCodexTestVersion(t, "0.166.0")
	request := jsonSearchCompatRequest("responses", "json_object", false)
	request["previous_response_id"] = "resp_existing_owner"
	request["input"] = []any{map[string]any{"type": "function_call_output", "call_id": "call_previous_turn", "output": "continuation result"}}
	account := newCodexSynthTestAccount(77119, AccountTypeAPIKey)
	account.Credentials = map[string]any{"api_key": "synthetic-test-token", "base_url": "https://api.openai.com"}
	upstream, recorder, result, err := jsonSearchCompatForward(t, "responses", request, account)
	sent := requireJSONSearchCompatSuccess(t, "responses", false, upstream, recorder, result, err)
	require.Equal(t, "resp_existing_owner", gjson.GetBytes(sent, "previous_response_id").String())
	require.Equal(t, "call_previous_turn", gjson.GetBytes(sent, "input.0.call_id").String())
	require.Equal(t, "continuation result", gjson.GetBytes(sent, "input.0.output").String())
	require.False(t, jsonSearchCompatHasSearch(sent))
}
