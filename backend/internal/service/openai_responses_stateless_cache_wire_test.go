//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestResponsesStatelessCacheFullGenerationUsesSingleWireRequest(t *testing.T) {
	const model = "gpt-6.1-sol"
	for _, mode := range []string{"", "key:538:full"} {
		t.Run("mode_"+mode, func(t *testing.T) {
			t.Setenv("SUB2_RESPONSES_STATELESS_CACHE_MODE", mode)
			t.Setenv(responsesCacheAffinityScopeEnv, "key:538")
			frames := make(chan []byte, 3)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				for {
					_, frame, err := conn.ReadMessage()
					if err != nil {
						return
					}
					frames <- frame
					resp := responsesHistoryCompleted(model, "resp_business", autoContinuationAnswer)
					if warm := gjson.GetBytes(frame, "generate"); warm.Exists() && !warm.Bool() {
						resp = responsesHistoryCompleted(model, "resp_warm", "")
					}
					if conn.WriteMessage(websocket.TextMessage, resp) != nil {
						return
					}
				}
			}))
			defer server.Close()
			svc, account, httpFallback := httpBridgeFixture(t, server.URL)
			defer svc.openaiWSPool.Close()
			account.Credentials["chatgpt_account_id"] = "local-wire-account"
			svc.cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled = true
			svc.cfg.Gateway.OpenAIWS.ExactFidelityEnabled = true
			// Keep the network destination pinned to the fixture when testing the
			// exact serializer. Its transport has a dedicated production dialer.
			local := *svc.cfg
			local.Gateway.OpenAIWS.ExactFidelityEnabled = false
			svc.openaiWSPool.cfg = &local
			// Anonymous native clients can resend reasoning alongside a matched
			// function_call and its tool result. Both arms must preserve all three.
			const reasoning = `{"type":"reasoning","id":"rs_wire","summary":[{"type":"summary_text","text":"Use lookup."}],"encrypted_content":"opaque-fixture"}`
			body := responsesSynthHistoryBody(t, model, reasoning, autoContinuationCall, autoContinuationTool)
			body, err := sjson.SetBytes(body, "prompt_cache_key", "same-public-hint")
			require.NoError(t, err)
			c, _ := httpBridgeContext("")
			c.Request.Header.Del("session-id")
			c.Request.Header.Set("User-Agent", "claude-cli/2.0.1 (external, cli)")
			c.Set("api_key", &APIKey{ID: 538, UserID: 935, Group: &Group{Platform: PlatformOpenAI}})
			svc.GenerateResponsesAccountSessionHash(c, body, model)
			result, err := svc.Forward(context.Background(), c, account, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.OpenAIWSMode)
			require.Empty(t, httpFallback.bodies)
			require.Greater(t, len(frames), 0)
			frame := <-frames
			require.True(t, json.Valid(frame))
			require.NotEmpty(t, gjson.GetBytes(frame, "prompt_cache_key").String())
			require.NotEmpty(t, gjson.GetBytes(frame, "input").Array())
			require.Contains(t, string(frame), "lookup result")
			require.Contains(t, string(frame), "opaque-fixture")
			assignment, action := responsesStatelessCacheTrialObservation(c)
			if mode == "" {
				require.Empty(t, assignment)
				require.True(t, gjson.GetBytes(frame, "generate").Exists())
				require.False(t, gjson.GetBytes(frame, "generate").Bool())
				require.Len(t, frames, 1)
				next := <-frames
				require.Equal(t, "resp_warm", gjson.GetBytes(next, "previous_response_id").String())
				require.Empty(t, gjson.GetBytes(next, "input").Array())
			} else {
				require.Equal(t, "fixed_full", assignment)
				require.Equal(t, "full_input_written", action)
				require.False(t, gjson.GetBytes(frame, "previous_response_id").Exists())
				require.False(t, gjson.GetBytes(frame, "generate").Exists())
				require.Empty(t, frames)
			}
		})
	}
}
