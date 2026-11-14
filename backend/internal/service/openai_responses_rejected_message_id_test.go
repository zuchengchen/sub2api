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

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func rejectedMessageIDError(index int, id string) []byte {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"code": "invalid_value", "type": "invalid_request_error",
		"param":   fmt.Sprintf("input[%d].id", index),
		"message": fmt.Sprintf("Invalid 'input[%d].id': '%s'. Expected an ID that begins with 'msg'.", index, id),
	}})
	return body
}

func TestRejectedMessageIDRepairsOnlyExactOptionalID(t *testing.T) {
	for _, message := range []string{
		`{"type":"message","id":"item_BAD","role":"assistant","content":[{"type":"output_text","text":"keep","annotations":[]}]}`,
		`{"id":"item_BAD","role":"user","content":"keep"}`,
	} {
		body := []byte(`{"previous_response_id":"resp_keep","input":[` + message + `,
			{"type":"reasoning","id":"future_native","encrypted_content":"opaque-keep","summary":[],"future":9007199254740993},
			{"type":"function_call","id":"item_call","call_id":"call_keep","name":"run","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_keep","output":"opaque output"},
			{"type":"item_reference","id":"msg_other"},
			{"type":"message","id":"item_OTHER","role":"assistant","content":"preserve native ID"}]}`)
		next, reason, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest, body, rejectedMessageIDError(0, "item_BAD"))
		require.NoError(t, err)
		require.True(t, changed)
		require.Contains(t, reason, "input[0].id")
		require.Equal(t, strings.Replace(string(body), `"id":"item_BAD",`, "", 1), string(next))
		require.Equal(t, "future_native", gjson.GetBytes(next, "input.1.id").String())
		require.Equal(t, "9007199254740993", gjson.GetBytes(next, "input.1.future").Raw)
		require.Equal(t, "call_keep", gjson.GetBytes(next, "input.2.call_id").String())
		require.Equal(t, "call_keep", gjson.GetBytes(next, "input.3.call_id").String())
		require.Equal(t, "item_OTHER", gjson.GetBytes(next, "input.5.id").String())
		_, _, changed, err = normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest, next, rejectedMessageIDError(0, "item_BAD"))
		require.NoError(t, err)
		require.False(t, changed, "the same rejection cannot mutate an already repaired field")
	}
}

func TestRejectedMessageIDDoesNotGuessOrBreakReferences(t *testing.T) {
	valid := `{"input":[{"type":"message","id":"item_BAD","role":"assistant","content":"keep"}]}`
	rejection := string(rejectedMessageIDError(0, "item_BAD"))
	tests := []struct{ name, body, errorBody string }{
		{"generic bad request", valid, `{"error":{"type":"invalid_request_error","message":"Bad Request"}}`},
		{"wrong code", valid, strings.Replace(rejection, "invalid_value", "invalid_type", 1)},
		{"wrong error type", valid, strings.Replace(rejection, "invalid_request_error", "server_error", 1)},
		{"mismatched param", valid, strings.Replace(rejection, `"param":"input[0].id"`, `"param":"input[1].id"`, 1)},
		{"missing param", valid, strings.Replace(rejection, `"param":"input[0].id",`, "", 1)},
		{"message only mention", valid, strings.Replace(rejection, "Invalid 'input", "Suggestion: Invalid 'input", 1)},
		{"different expected prefix", valid, strings.Replace(rejection, "'msg'", "'rs'", 1)},
		{"different rejected id", valid, string(rejectedMessageIDError(0, "item_other"))},
		{"id is case sensitive", valid, string(rejectedMessageIDError(0, "item_bad"))},
		{"valid prefix", strings.Replace(valid, "item_BAD", "msg_valid", 1), string(rejectedMessageIDError(0, "msg_valid"))},
		{"index out of range", valid, string(rejectedMessageIDError(268, "item_BAD"))},
		{"non-array input", `{"input":{"0":{"type":"message","id":"item_BAD","role":"assistant","content":"keep"}}}`, rejection},
		{"reasoning", `{"input":[{"type":"reasoning","id":"item_BAD","role":"assistant","content":"keep","encrypted_content":"opaque"}]}`, rejection},
		{"tool call", `{"input":[{"type":"function_call","id":"item_BAD","call_id":"call_keep","role":"assistant","content":"keep"}]}`, rejection},
		{"reference only", `{"input":[{"type":"item_reference","id":"item_BAD"}]}`, rejection},
		{"dependent reference", strings.TrimSuffix(valid, "]}") + `,{"type":"item_reference","id":"item_BAD"}]}`, rejection},
		{"missing content", strings.Replace(valid, `,"content":"keep"`, "", 1), rejection},
		{"null content", strings.Replace(valid, `"content":"keep"`, `"content":null`, 1), rejection},
		{"object content", strings.Replace(valid, `"content":"keep"`, `"content":{}`, 1), rejection},
		{"missing role", strings.Replace(valid, `"role":"assistant",`, "", 1), rejection},
		{"unknown role", strings.Replace(valid, "assistant", "unknown", 1), rejection},
		{"invalid type", strings.Replace(valid, `"type":"message"`, `"type":null`, 1), rejection},
		{"duplicate id", strings.Replace(valid, `"id":"item_BAD"`, `"id":"item_BAD","id":"msg_keep"`, 1), rejection},
		{"duplicate input", `{"input":[],` + strings.TrimPrefix(valid, "{"), rejection},
		{"hidden reference", strings.TrimSuffix(valid, "]}") + `,{"type":"message","type":"item_reference","id":"item_BAD"}]}`, rejection},
		{"malformed request", valid[:len(valid)-1], rejection},
		{"malformed response", valid, rejection[:len(rejection)-1]},
		{"duplicate error param", valid, strings.Replace(rejection, `"param":"input[0].id"`, `"param":"input[0].id","param":"tools"`, 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next, _, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest, []byte(tt.body), []byte(tt.errorBody))
			require.NoError(t, err)
			require.False(t, changed)
			require.Nil(t, next)
		})
	}
	for _, status := range []int{200, 401, 403, 429, 502} {
		_, _, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(status, []byte(valid), []byte(rejection))
		require.NoError(t, err)
		require.False(t, changed)
	}
}

func rejectedMessageIDHistory(t *testing.T, index, padding int) []byte {
	t.Helper()
	items := make([]json.RawMessage, index+1)
	for i := range items {
		items[i] = json.RawMessage(fmt.Sprintf(`{"type":"message","id":"msg_%d","role":"user","content":"keep"}`, i))
	}
	items[0] = json.RawMessage(`{"type":"message","id":"msg_base","role":"developer","content":"Keep the conversation.","internal_chat_message_metadata_passthrough":{"content_item_kinds":["model.base_instructions"]}}`)
	items[index] = json.RawMessage(`{"type":"message","id":"item_BAD","role":"assistant","content":[{"type":"output_text","text":"keep ` + strings.Repeat("x", padding) + `","annotations":[]}]}`)
	body, err := json.Marshal(map[string]any{"model": "gpt-5.6-sol", "stream": false, "store": false, "instructions": "test", "input": items})
	require.NoError(t, err)
	return body
}

func TestRejectedMessageIDLargeHistoryUsesLocalJSONPatch(t *testing.T) {
	body := rejectedMessageIDHistory(t, 268, 16<<20)
	next, _, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest, body, rejectedMessageIDError(268, "item_BAD"))
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, strings.Replace(string(body), `"id":"item_BAD",`, "", 1), string(next))
	require.Len(t, gjson.GetBytes(next, "input").Array(), 269)
}

func TestRejectedMessageIDOAuthHTTPRecoversWithoutChangingOtherHistory(t *testing.T) {
	body := rejectedMessageIDHistory(t, 268, 0)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, string(rejectedMessageIDError(268, "item_BAD"))),
		newOpenAIRejectedFieldTestResponse(http.StatusOK, namespaceForwardOKResponse),
	}}
	c := newOpenAIRejectedFieldTestContext(body)
	result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), c, newOpenAIOAuthNamespaceTestAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.bodies, 2)
	require.Equal(t, "item_BAD", gjson.GetBytes(upstream.bodies[0], "input.268.id").String())
	require.False(t, gjson.GetBytes(upstream.bodies[1], "input.268.id").Exists())
	require.Equal(t, "msg_267", gjson.GetBytes(upstream.bodies[1], "input.267.id").String())
	require.Equal(t, gjson.GetBytes(upstream.bodies[0], "input.268.content").Raw, gjson.GetBytes(upstream.bodies[1], "input.268.content").Raw)
}

func TestRejectedMessageIDHTTPToWSSizeFallbackRecovers(t *testing.T) {
	var businessFrames atomic.Int32
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err == nil {
			businessFrames.Add(1)
		}
	}))
	defer wsServer.Close()
	svc, account, upstream := httpBridgeFixture(t, wsServer.URL)
	svc.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
	svc.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 8 << 10
	upstream.responses = []*http.Response{
		newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, string(rejectedMessageIDError(268, "item_BAD"))),
		newOpenAIRejectedFieldTestResponse(http.StatusOK, namespaceForwardOKResponse),
	}
	body := rejectedMessageIDHistory(t, 268, 0)
	c, _ := httpBridgeContext("message-id-fallback")
	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	require.Equal(t, "payload_too_large", c.GetString("openai_ws_session_fallback_reason"))
	require.Equal(t, int32(0), businessFrames.Load())
	require.Equal(t, 2, upstream.callCount)
	require.False(t, gjson.GetBytes(upstream.bodies[1], "input.268.id").Exists())
	events := c.MustGet(OpsUpstreamErrorsKey).([]*OpsUpstreamErrorEvent)
	require.Len(t, events, 2)
	require.Equal(t, "payload_too_large", events[0].Reason)
	require.Equal(t, "message_id_prefix", events[1].Reason)
	require.Equal(t, "payload_too_large", gjson.Get(events[1].Detail, "ws_fallback_reason").String())
}

func TestRejectedMessageIDWSHTTPBridgeRecoversBeforeOutput(t *testing.T) {
	body := rejectedMessageIDHistory(t, 268, 0)
	success := newOpenAIRejectedFieldTestResponse(http.StatusOK, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_id_retry\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	success.Header.Set("Content-Type", "text/event-stream")
	upstream := &httpUpstreamSequenceRecorder{responses: []*http.Response{
		newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, string(rejectedMessageIDError(268, "item_BAD"))), success,
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	c := newOpenAIRejectedFieldTestContext(body)
	var writes [][]byte
	result, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, newOpenAIOAuthNamespaceTestAccount(), "test-token", body, len(body),
		"gpt-5.6-sol", "", "", "", "", 1, func(message []byte) error { writes = append(writes, append([]byte(nil), message...)); return nil })
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, upstream.callCount)
	require.Equal(t, "item_BAD", gjson.GetBytes(upstream.bodies[0], "input.268.id").String())
	require.False(t, gjson.GetBytes(upstream.bodies[1], "input.268.id").Exists())
	require.Len(t, writes, 1)
	require.Equal(t, "response.completed", gjson.GetBytes(writes[0], "type").String())
}

func TestRejectedMessageIDHTTPStopsAfterOutputOrCancellation(t *testing.T) {
	for _, mode := range []string{"output", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			body := rejectedMessageIDHistory(t, 1, 0)
			upstream := &httpUpstreamRecorder{responses: []*http.Response{
				newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, string(rejectedMessageIDError(1, "item_BAD"))),
				newOpenAIRejectedFieldTestResponse(http.StatusOK, namespaceForwardOKResponse),
			}}
			c := newOpenAIRejectedFieldTestContext(body)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "output" {
				_, err := c.Writer.Write([]byte("model output already sent"))
				require.NoError(t, err)
			} else {
				cancel()
			}
			_, err := newOpenAIRejectedFieldTestService(upstream).Forward(ctx, c, newOpenAIOAuthNamespaceTestAccount(), body)
			require.Error(t, err)
			require.LessOrEqual(t, len(upstream.bodies), 1)
		})
	}
}
