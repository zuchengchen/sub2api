package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// topLevelJSONKeys lists top-level object keys in wire order.
func topLevelJSONKeys(t *testing.T, body []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	first, err := dec.Token()
	require.NoError(t, err)
	require.Equal(t, json.Delim('{'), first)
	var keys []string
	for dec.More() {
		token, err := dec.Token()
		require.NoError(t, err)
		key, ok := token.(string)
		require.True(t, ok)
		keys = append(keys, key)
		var value json.RawMessage
		require.NoError(t, dec.Decode(&value))
	}
	return keys
}

func TestRestoreOpenAIUpstreamJSONTopLevelOrder(t *testing.T) {
	t.Run("keeps original order and places new known keys officially", func(t *testing.T) {
		original := []byte(`{"model":"gpt-5.4","stream":true,"prompt_cache_key":"pc","input":"hi"}`)
		encoded := []byte(`{"input":"hi","model":"gpt-5.4","prompt_cache_key":"pc","stream":true,"client_metadata":{"a":1}}`)
		got := restoreOpenAIUpstreamJSONTopLevelOrder(original, encoded)
		require.Equal(t,
			[]string{"model", "stream", "prompt_cache_key", "input", "client_metadata"},
			topLevelJSONKeys(t, got))
		require.Equal(t, "gpt-5.4", gjson.GetBytes(got, "model").String())
		require.Equal(t, float64(1), gjson.GetBytes(got, "client_metadata.a").Float())
	})
	t.Run("unknown new keys append after original", func(t *testing.T) {
		original := []byte(`{"stream":true,"model":"m"}`)
		encoded := []byte(`{"gateway_extra":"x","model":"m","stream":true}`)
		got := restoreOpenAIUpstreamJSONTopLevelOrder(original, encoded)
		require.Equal(t, []string{"stream", "model", "gateway_extra"}, topLevelJSONKeys(t, got))
	})
	t.Run("nested order and raw value bytes preserved", func(t *testing.T) {
		original := []byte(`{"model":"m","stream":true}`)
		encoded := []byte(`{"model":{"z":1,"a":2},"stream":true}`)
		got := restoreOpenAIUpstreamJSONTopLevelOrder(original, encoded)
		require.Equal(t, []string{"model", "stream"}, topLevelJSONKeys(t, got))
		require.Contains(t, string(got), `{"z":1,"a":2}`)
	})
	t.Run("duplicate original keys use first occurrence", func(t *testing.T) {
		original := []byte(`{"stream":true,"model":"a","stream":false}`)
		encoded := []byte(`{"model":"b","stream":true}`)
		got := restoreOpenAIUpstreamJSONTopLevelOrder(original, encoded)
		require.Equal(t, []string{"stream", "model"}, topLevelJSONKeys(t, got))
	})
	t.Run("invalid input returns encoded", func(t *testing.T) {
		encoded := []byte(`{"model":"m"}`)
		require.Equal(t, encoded, restoreOpenAIUpstreamJSONTopLevelOrder([]byte(`not json`), encoded))
		require.Equal(t, encoded, restoreOpenAIUpstreamJSONTopLevelOrder([]byte(`[1,2]`), encoded))
		require.Equal(t, encoded, restoreOpenAIUpstreamJSONTopLevelOrder([]byte(`"scalar"`), encoded))
	})
}

func TestOpenAIForwardCodexLanePreservesTopLevelKeyOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Official serde order: model first, routing prefix ahead of input.
	body := []byte(`{"model":"gpt-5.4","stream":false,"instructions":"keep me","input":[{"type":"function_call_output","call_id":"call_orphan","output":"stale"}]}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid-order"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_1","status":"completed","model":"gpt-5.4","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := openAIFailoverCachedBodyTestAccount(91, "codex-order", nil)

	// Orphan tool output forces the decoded-map re-encode path
	// (markDecodedModified disables byte patches); without restoration the
	// wire body would serialize top-level keys alphabetically.
	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.bodies, 1)

	keys := topLevelJSONKeys(t, upstream.bodies[0])
	// The client keys keep their original positions and every gateway-added
	// key lands in the official serde declaration order (ResponsesApiRequest:
	// model, stream, service_tier, instructions, input, tools, tool_choice,
	// parallel_tool_calls, reasoning, store, stream_options, include,
	// prompt_cache_key, text, client_metadata, access_programs).
	require.Equal(t,
		[]string{"model", "stream", "instructions", "input", "tool_choice",
			"parallel_tool_calls", "reasoning", "store", "include", "client_metadata"},
		keys)
	require.JSONEq(t, `[]`, string(gjson.GetBytes(upstream.bodies[0], "input").Raw))
}
