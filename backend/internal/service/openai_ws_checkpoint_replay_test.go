package service

import (
	"context"
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
	for _, mode := range []string{"custom", "failed_custom", "partial_output", "repeat_error", "repeat_failed", "missing_result", "unrelated_call"} {
		t.Run(mode, func(t *testing.T) {
			toolCall, toolResult, toolKind := autoContinuationCall, autoContinuationTool, "function"
			if strings.Contains(mode, "custom") {
				toolCall = `{"type":"custom_tool_call","id":"ctc_test","call_id":"call_lookup","name":"exec","input":"lookup()"}`
				toolResult = `{"type":"custom_tool_call_output","call_id":"call_lookup","output":"result"}`
				toolKind = "custom tool"
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
						callID := "call_lookup"
						if mode == "unrelated_call" {
							callID = "call_unknown"
						}
						errorJSON := fmt.Sprintf(`{"type":"invalid_request_error","param":"input","message":"No tool output found for %s call %s."}`, toolKind, callID)
						event := `{"type":"error","error":` + errorJSON + `}`
						if mode == "failed_custom" || mode == "repeat_failed" {
							event = `{"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":` + errorJSON + `}}`
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
			_, err := s.Forward(ctx, first, a, autoContinuationBody(true))
			require.NoError(t, err)
			<-requests
			second, recorder := httpBridgeContext("missing-tool-safety")
			body := autoContinuationBody(true, toolCall, toolResult)
			if mode == "missing_result" {
				body = autoContinuationBody(true, toolCall, autoContinuationUser)
			}
			result, err := s.Forward(ctx, second, a, body)
			replayed := mode == "custom" || mode == "failed_custom" || strings.HasPrefix(mode, "repeat_")
			if mode == "custom" || mode == "failed_custom" {
				require.NoError(t, err)
				require.Contains(t, recorder.Body.String(), "resp_replayed")
			} else if mode == "repeat_failed" {
				require.Contains(t, recorder.Body.String(), "response.failed")
				require.NotNil(t, result)
				require.False(t, result.SucceededForScheduling())
			} else {
				require.Error(t, err)
				require.Contains(t, recorder.Body.String(), `"type":"error"`)
				require.NotContains(t, recorder.Body.String(), "resp_replayed")
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
			if replayed {
				replay := <-requests
				require.False(t, gjson.GetBytes(replay, "previous_response_id").Exists())
				require.Len(t, gjson.GetBytes(replay, "input").Array(), 4)
				require.Equal(t, "reasoning.encrypted_content", gjson.GetBytes(replay, "include.0").String())
			}
		})
	}
}
