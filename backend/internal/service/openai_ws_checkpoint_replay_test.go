package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestHTTPToWSMissingToolReplaySafety(t *testing.T) {
	for _, mode := range []string{"custom", "failed_custom", "bare_then_failed_custom", "partial_output", "repeat_error", "repeat_failed", "missing_result", "unrelated_call", "metered_failed", "bare_then_metered_failed", "prior_usage", "reasoning_progress", "empty_tool_placeholder", "tool_progress"} {
		t.Run(mode, func(t *testing.T) {
			toolCall, toolResult, toolKind := autoContinuationCall, autoContinuationTool, "function"
			if strings.Contains(mode, "custom") {
				toolCall = `{"type":"custom_tool_call","id":"ctc_test","call_id":"call_lookup","name":"exec","input":"lookup()"}`
				toolResult = `{"type":"custom_tool_call_output","call_id":"call_lookup","output":"result"}`
				toolKind = "custom tool"
			}
			makeBody := func(items ...string) []byte {
				body := autoContinuationBody(true, items...)
				if strings.Contains(mode, "custom") {
					var request map[string]any
					require.NoError(t, json.Unmarshal(body, &request))
					request["tools"] = []any{map[string]any{"type": "custom", "name": "exec", "format": map[string]any{"type": "text"}}}
					body, _ = json.Marshal(request)
				}
				return body
			}
			requests := make(chan []byte, 8)
			var calls, dials atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				dials.Add(1)
				for {
					_, raw, err := conn.ReadMessage()
					if err != nil {
						return
					}
					requests <- raw
					n := calls.Add(1)
					if n == 1 {
						_ = conn.WriteMessage(websocket.TextMessage, autoContinuationEvent("resp_first", toolCall))
						continue
					}
					if n == 2 || strings.HasPrefix(mode, "repeat_") {
						if mode == "partial_output" {
							_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"already sent"}`))
						}
						switch mode {
						case "prior_usage":
							_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.in_progress","response":{"id":"resp_failed","usage":{"input_tokens":9,"output_tokens":1}}}`))
						case "reasoning_progress":
							_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.reasoning_text.delta","item_id":"rs_started","delta":"working"}`))
						case "empty_tool_placeholder", "tool_progress":
							_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_started","call_id":"call_started","name":"lookup","arguments":"","status":"in_progress"}}`))
							if mode == "tool_progress" {
								// The added item is an empty client-tool placeholder. A
								// nonempty argument delta proves model progress before failure.
								_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.function_call_arguments.delta","item_id":"fc_started","output_index":0,"delta":"{\"key\":"}`))
							}
						}
						callID := "call_lookup"
						if mode == "unrelated_call" {
							callID = "call_unknown"
						}
						errorJSON := fmt.Sprintf(`{"type":"invalid_request_error","param":"input","message":"No tool output found for %s call %s."}`, toolKind, callID)
						event := `{"type":"error","error":` + errorJSON + `}`
						if strings.HasPrefix(mode, "bare_then_") {
							_ = conn.WriteMessage(websocket.TextMessage, []byte(event))
						}
						if mode == "failed_custom" || mode == "bare_then_failed_custom" || mode == "repeat_failed" || strings.Contains(mode, "metered_failed") || mode == "prior_usage" || mode == "empty_tool_placeholder" || strings.HasSuffix(mode, "_progress") {
							usage := ""
							if strings.Contains(mode, "metered_failed") {
								usage = `,"usage":{"input_tokens":9,"output_tokens":1}`
							} else if mode == "prior_usage" {
								usage = `,"usage":{"input_tokens":0,"output_tokens":0}`
							}
							event = `{"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":` + errorJSON + usage + `}}`
						}
						_ = conn.WriteMessage(websocket.TextMessage, []byte(event))
						return
					}
					_ = conn.WriteMessage(websocket.TextMessage, autoContinuationEvent("resp_replayed", autoContinuationAnswer))
				}
			}))
			defer server.Close()
			s, a, httpUpstream := httpBridgeFixture(t, server.URL)
			defer s.openaiWSPool.Close()
			first, _ := httpBridgeContext("missing-tool-safety")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := s.Forward(ctx, first, a, makeBody())
			require.NoError(t, err)
			<-requests
			second, recorder := httpBridgeContext("missing-tool-safety")
			body := makeBody(toolCall, toolResult)
			if mode == "missing_result" {
				body = makeBody(toolCall, autoContinuationUser)
			}
			result, err := s.Forward(ctx, second, a, body)
			replayed := mode == "failed_custom" || mode == "bare_then_failed_custom" || mode == "repeat_failed" || mode == "empty_tool_placeholder"
			if mode == "failed_custom" || mode == "bare_then_failed_custom" || mode == "empty_tool_placeholder" {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.True(t, result.SucceededForScheduling())
				require.Contains(t, recorder.Body.String(), "resp_replayed")
			} else {
				require.Error(t, err)
				var failure []byte
				var terminals int
				forEachOpenAISSEFrame(recorder.Body.String(), func(eventType string, payload []byte) {
					if eventType == "response.failed" {
						terminals++
						failure = append([]byte(nil), payload...)
					}
				})
				require.Equal(t, 1, terminals, "deliver one terminal, including when a bare error precedes it")
				require.Equal(t, "failed", gjson.GetBytes(failure, "response.status").String())
				require.Equal(t, "invalid_request_error", gjson.GetBytes(failure, "response.error.type").String())
				require.Equal(t, "input", gjson.GetBytes(failure, "response.error.param").String())
				require.Contains(t, gjson.GetBytes(failure, "response.error.message").String(), "No tool output found")
				require.NotContains(t, recorder.Body.String(), "resp_replayed")
				if mode == "tool_progress" {
					var argumentDeltas int
					forEachOpenAISSEFrame(recorder.Body.String(), func(eventType string, payload []byte) {
						if eventType == "response.function_call_arguments.delta" {
							argumentDeltas++
							require.Equal(t, "fc_started", gjson.GetBytes(payload, "item_id").String())
							require.Equal(t, `{"key":`, gjson.GetBytes(payload, "delta").String())
						}
					})
					require.Equal(t, 1, argumentDeltas, "retain generated tool arguments exactly once")
				}
				progressed := mode == "partial_output" || mode == "prior_usage" || strings.Contains(mode, "metered_failed") || strings.HasSuffix(mode, "_progress")
				if progressed {
					require.NotNil(t, result, "retain observed progress and authoritative usage on terminal failure")
					require.False(t, result.SucceededForScheduling())
				} else {
					require.Nil(t, result, "a rejected turn with no observed work must not invent a usage result")
				}
				if mode == "prior_usage" || strings.Contains(mode, "metered_failed") {
					require.Equal(t, 9, result.Usage.InputTokens)
					require.Equal(t, 1, result.Usage.OutputTokens)
				}
			}
			wantCalls, wantDials := int32(2), int32(1)
			if replayed {
				wantCalls, wantDials = 3, 2
			}
			require.Equal(t, wantCalls, calls.Load(), "never loop on a second tool error")
			require.Equal(t, wantDials, dials.Load(), "replay uses a fresh upstream socket")
			require.Equal(t, replayed, second.GetBool("openai_ws_auto_incremental_replay"))
			require.False(t, second.GetBool("openai_ws_auto_incremental_succeeded"))
			require.Zero(t, httpUpstream.callCount)
			initial := <-requests
			if mode == "missing_result" {
				require.False(t, gjson.GetBytes(initial, "previous_response_id").Exists())
			}
			if mode != "missing_result" {
				require.Equal(t, "resp_first", gjson.GetBytes(initial, "previous_response_id").String())
				require.JSONEq(t, "["+toolResult+"]", gjson.GetBytes(initial, "input").Raw)
			}
			if replayed {
				replay := <-requests
				require.False(t, gjson.GetBytes(replay, "previous_response_id").Exists())
				require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(replay, "input").Raw)
				require.JSONEq(t, gjson.GetBytes(body, "tools").Raw, gjson.GetBytes(replay, "tools").Raw)
				require.Equal(t, "reasoning.encrypted_content", gjson.GetBytes(replay, "include.0").String())
			}
		})
	}
}
