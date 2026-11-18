package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestHTTPToWSNativeIncrementalImagesAndSizeFallback(t *testing.T) {
	// Each completed turn owns a distinct item ID. Reusing msg_answer would
	// make the fourth full-history request ambiguous before size selection.
	answer := func(turn int32) string {
		return strings.Replace(autoContinuationAnswer, "msg_answer", fmt.Sprintf("msg_native_%d", turn), 1)
	}
	requests := make(chan []byte, 8)
	var writes, dials atomic.Int32
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
			n := writes.Add(1)
			output := answer(n)
			if n == 1 {
				output = autoContinuationCall
			}
			if conn.WriteMessage(websocket.TextMessage, autoContinuationEvent(fmt.Sprintf("resp_native_%d", n), output)) != nil {
				return
			}
		}
	}))
	defer server.Close()
	s, a, upstream := httpBridgeFixture(t, server.URL)
	defer s.openaiWSPool.Close()
	for i := range upstream.responses {
		completed := autoContinuationEvent(fmt.Sprintf("resp_native_http_%d", i), answer(int32(i+4)))
		upstream.responses[i] = &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(gjson.GetBytes(completed, "response").Raw))}
	}
	s.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
	s.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 2 << 20
	payload := autoContinuationPayload(t)
	payload["stream"] = false
	image := map[string]any{"type": "message", "role": "user", "content": []any{
		map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + strings.Repeat("A", 128<<10), "detail": "original"},
	}}
	input := append(payload["input"].([]any), image)
	forward := func(items []any) ([]byte, bool, string) {
		t.Helper()
		payload["input"] = items
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		c, _ := httpBridgeContext("native-image-session")
		require.NoError(t, ValidateOpenAIContinuationInput(body), "fixture must preserve unambiguous call/result history before testing size routing")
		result, err := s.Forward(context.Background(), c, a, body)
		require.NoError(t, err)
		return body, result.OpenAIWSMode, c.GetString("openai_ws_auto_incremental_reason")
	}
	_, ws, reason := forward(input)
	require.True(t, ws, reason)
	first := <-requests
	require.Contains(t, string(first), "data:image/png;base64,")
	require.Equal(t, "no_checkpoint", reason)
	// A full history now exceeds the threshold, but its actual delta fits.
	s.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 32 << 10
	input = append(input, json.RawMessage(autoContinuationCall), json.RawMessage(autoContinuationTool))
	_, ws, reason = forward(input)
	require.True(t, ws, reason)
	require.Equal(t, "incremental", reason)
	delta := <-requests
	require.Equal(t, "resp_native_1", gjson.GetBytes(delta, "previous_response_id").String())
	require.JSONEq(t, "["+autoContinuationTool+"]", gjson.GetBytes(delta, "input").Raw)
	require.NotContains(t, string(delta), "data:image/")
	require.GreaterOrEqual(t, int64(len(first)), s.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes)
	require.Less(t, int64(len(delta)), s.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes, "measure the actual serialized delta, not full history")
	input = append(input, json.RawMessage(answer(2)), json.RawMessage(autoContinuationUser))
	_, ws, reason = forward(input)
	require.True(t, ws, reason)
	require.Equal(t, "incremental", reason)
	third := <-requests
	require.Equal(t, "resp_native_2", gjson.GetBytes(third, "previous_response_id").String())
	require.Len(t, gjson.GetBytes(third, "input").Array(), 1)
	require.Equal(t, "xhigh", gjson.GetBytes(third, "reasoning.effort").String())
	// The new delta itself is too big. No WS business write may occur; HTTP
	// must receive the entire original history, including tools and both images.
	full := append(append([]any{}, input...), json.RawMessage(answer(3)), image)
	fullBody, ws, reason := forward(full)
	require.False(t, ws, reason)
	require.Equal(t, 1, upstream.callCount)
	require.Equal(t, int32(3), writes.Load())
	replay := upstream.bodies[0]
	require.JSONEq(t, gjson.GetBytes(fullBody, "input").Raw, gjson.GetBytes(replay, "input").Raw, "size fallback keeps every tool, message and image")
	require.JSONEq(t, gjson.GetBytes(fullBody, "tools").Raw, gjson.GetBytes(replay, "tools").Raw)
	require.Equal(t, 2, strings.Count(string(replay), "data:image/png;base64,"))
	require.True(t, gjson.GetBytes(replay, "tools").IsArray())
	require.Equal(t, "call_lookup", gjson.GetBytes(replay, "input.3.call_id").String())
	require.Equal(t, "gpt-6-astra", gjson.GetBytes(replay, "model").String())
	require.Equal(t, "xhigh", gjson.GetBytes(replay, "reasoning.effort").String())
	require.False(t, gjson.GetBytes(replay, "previous_response_id").Exists())
	// HTTP output does not advance the WS checkpoint. On this same linear
	// history, the unsent image remains in the delta of later small turns, so
	// they must also use complete HTTP replay rather than dropping the image.
	followup := append([]any{}, full...)
	for turn := 0; turn < 2; turn++ {
		followup = append(followup, json.RawMessage(answer(int32(4+turn))), json.RawMessage(autoContinuationUser))
		followupBody, followupWS, followupReason := forward(followup)
		ws, reason = followupWS, followupReason
		require.False(t, ws, reason)
		require.Equal(t, turn+2, upstream.callCount)
		require.Equal(t, int32(3), writes.Load())
		require.Equal(t, 2, strings.Count(string(upstream.bodies[turn+1]), "data:image/png;base64,"))
		require.JSONEq(t, gjson.GetBytes(followupBody, "input").Raw, gjson.GetBytes(upstream.bodies[turn+1], "input").Raw)
	}
	// Returning to the prior WS branch proves a local size decision neither
	// closes the healthy socket nor disables WS for the entire session.
	branch := append(append([]any{}, input...), json.RawMessage(answer(3)), json.RawMessage(autoContinuationUser))
	_, ws, reason = forward(branch)
	require.True(t, ws, reason)
	require.Equal(t, "incremental", reason)
	<-requests
	require.Equal(t, int32(1), dials.Load())
	// A compacted input window establishes a fresh checkpoint. Only after
	// that full WS turn completes may its next turn use a small delta again.
	payload["client_metadata"] = map[string]any{"x-codex-window-id": "compacted-native-window"}
	compacted := []any{map[string]any{"type": "compaction_summary", "id": "cmp_native", "encrypted_content": "opaque-new-window"}, json.RawMessage(autoContinuationUser)}
	_, ws, reason = forward(compacted)
	require.True(t, ws, reason)
	require.Equal(t, "context_window_changed", reason)
	require.False(t, gjson.GetBytes(<-requests, "previous_response_id").Exists())
	compacted = append(compacted, json.RawMessage(answer(5)), json.RawMessage(autoContinuationUser))
	_, ws, reason = forward(compacted)
	require.True(t, ws, reason)
	require.Equal(t, "incremental", reason)
	require.Len(t, gjson.GetBytes(<-requests, "input").Array(), 1)
}
