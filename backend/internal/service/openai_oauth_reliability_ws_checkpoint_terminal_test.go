//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOAuthReliabilityHTTPToWSContinuationSyntheticFailureDoesNotReplay(t *testing.T) {
	for _, test := range []struct {
		name, code   string
		realTerminal bool
	}{
		{name: "bare_eof_without_code"},
		{name: "bare_eof_with_invalid_request_code", code: "invalid_request_error"},
		{name: "real_failed_terminal", code: "invalid_request_error", realTerminal: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := make(chan []byte, 4)
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
					switch calls.Add(1) {
					case 1:
						_ = conn.WriteMessage(websocket.TextMessage, autoContinuationEvent("resp_checkpoint_provenance", autoContinuationCall))
					case 2:
						errorObject := map[string]any{
							"type": "invalid_request_error", "param": "input",
							"message": "No tool output found for function call call_lookup.",
						}
						if test.code != "" {
							errorObject["code"] = test.code
						}
						errorRaw, _ := json.Marshal(errorObject)
						event := fmt.Sprintf(`{"type":"error","error":%s}`, errorRaw)
						if test.realTerminal {
							event = fmt.Sprintf(`{"type":"response.failed","response":{"id":"resp_checkpoint_rejected","status":"failed","error":%s}}`, errorRaw)
						}
						_ = conn.WriteMessage(websocket.TextMessage, []byte(event))
						return
					default:
						_ = conn.WriteMessage(websocket.TextMessage, autoContinuationEvent("resp_checkpoint_replayed", autoContinuationAnswer))
					}
				}
			}))
			defer server.Close()
			svc, account, httpUpstream := httpBridgeFixture(t, server.URL)
			defer svc.openaiWSPool.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			first, _ := httpBridgeContext("checkpoint-terminal-provenance")
			_, err := svc.Forward(ctx, first, account, autoContinuationBody(true))
			require.NoError(t, err)
			<-requests
			second, recorder := httpBridgeContext("checkpoint-terminal-provenance")
			body := autoContinuationBody(true, autoContinuationCall, autoContinuationTool)
			result, err := svc.Forward(ctx, second, account, body)
			delta := <-requests
			require.Equal(t, "resp_checkpoint_provenance", gjson.GetBytes(delta, "previous_response_id").String())
			require.JSONEq(t, "["+autoContinuationTool+"]", gjson.GetBytes(delta, "input").Raw)
			require.Zero(t, httpUpstream.callCount)
			if test.realTerminal {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.True(t, result.SucceededForScheduling())
				require.Equal(t, int32(3), calls.Load(), "an authoritative rejection allows one full-history recovery")
				require.Equal(t, int32(2), dials.Load())
				require.True(t, second.GetBool("openai_ws_auto_incremental_replay"))
				replay := <-requests
				require.False(t, gjson.GetBytes(replay, "previous_response_id").Exists())
				for _, field := range []string{"input", "tools", "reasoning", "include"} {
					require.JSONEq(t, gjson.GetBytes(body, field).Raw, gjson.GetBytes(replay, field).Raw, field)
				}
				require.Contains(t, recorder.Body.String(), "resp_checkpoint_replayed")
				return
			}
			require.Error(t, err, "a gateway-synthesized terminal is not evidence that upstream stopped before execution")
			require.Nil(t, result, "no upstream usage or success may be invented")
			require.Equal(t, int32(2), calls.Load(), "bare error plus EOF must not replay a model request")
			require.Equal(t, int32(1), dials.Load())
			require.False(t, second.GetBool("openai_ws_auto_incremental_replay"))
			require.False(t, second.GetBool("openai_ws_auto_incremental_succeeded"))
			var terminal []byte
			forEachOpenAISSEFrame(recorder.Body.String(), func(eventType string, payload []byte) {
				if eventType == "response.failed" {
					terminal = append([]byte(nil), payload...)
				}
			})
			require.NotEmpty(t, terminal)
			require.Equal(t, "invalid_request_error", gjson.GetBytes(terminal, "response.error.type").String())
			require.Equal(t, "input", gjson.GetBytes(terminal, "response.error.param").String())
			require.Equal(t, "No tool output found for function call call_lookup.", gjson.GetBytes(terminal, "response.error.message").String())
			require.NotContains(t, recorder.Body.String(), "resp_checkpoint_replayed")
		})
	}
}
