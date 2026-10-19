package service

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestEnsureConvertedAnthropicCacheBreakpoints_AddsToolsSystemAndMessages(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-5-5",
		"system":"You are a coding assistant.",
		"tools":[{"name":"read_file","input_schema":{"type":"object","properties":{}}},{"name":"edit","input_schema":{"type":"object","properties":{}}}],
		"messages":[
			{"role":"user","content":"q1"},
			{"role":"assistant","content":"a1"},
			{"role":"user","content":"q2"},
			{"role":"assistant","content":"a2"},
			{"role":"user","content":"q3"}
		]
	}`)

	out := ensureConvertedAnthropicCacheBreakpoints(body)
	require.Equal(t, "ephemeral", gjson.GetBytes(out, "system.0.cache_control.type").String())
	require.False(t, gjson.GetBytes(out, "system.0.cache_control.ttl").Exists())
	require.Equal(t, "You are a coding assistant.", gjson.GetBytes(out, "system.0.text").String())
	require.False(t, gjson.GetBytes(out, "tools.0.cache_control").Exists())
	require.Equal(t, "ephemeral", gjson.GetBytes(out, "tools.1.cache_control.type").String())
	require.Equal(t, "5m", gjson.GetBytes(out, "tools.1.cache_control.ttl").String())
	require.Equal(t, "ephemeral", gjson.GetBytes(out, "messages.4.content.0.cache_control.type").String())
	require.Equal(t, "ephemeral", gjson.GetBytes(out, "messages.2.content.0.cache_control.type").String())
	require.False(t, gjson.GetBytes(out, "messages.0.content.0.cache_control").Exists())
}

func TestEnsureConvertedAnthropicCacheBreakpoints_SingleUserMessage(t *testing.T) {
	body := []byte(`{
		"system":"You are a coding assistant.",
		"tools":[{"name":"read_file","input_schema":{"type":"object","properties":{}}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]
	}`)

	out := ensureConvertedAnthropicCacheBreakpoints(body)
	require.Equal(t, "ephemeral", gjson.GetBytes(out, "system.0.cache_control.type").String())
	require.Equal(t, "ephemeral", gjson.GetBytes(out, "tools.0.cache_control.type").String())
	require.Equal(t, "ephemeral", gjson.GetBytes(out, "messages.0.content.0.cache_control.type").String())
	require.Equal(t, 3, countCacheControl(out))
}

func TestEnsureConvertedAnthropicCacheBreakpoints_PreservesExistingBreakpoints(t *testing.T) {
	body := []byte(`{
		"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral","ttl":"1h"}}],
		"tools":[{"name":"bash","cache_control":{"type":"ephemeral","ttl":"1h"}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]
	}`)

	out := ensureConvertedAnthropicCacheBreakpoints(body)
	require.JSONEq(t, string(body), string(out))
}

func TestEnsureConvertedAnthropicCacheBreakpoints_AfterResponsesConversion(t *testing.T) {
	req := &apicompat.ResponsesRequest{
		Model:        "claude-sonnet-4-20250514",
		Instructions: "You are a coding assistant.",
		Input:        json.RawMessage(`[{"role":"user","content":"hi"}]`),
		Tools: []apicompat.ResponsesTool{{
			Type:       "function",
			Name:       "read_file",
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		}},
	}
	anth, err := apicompat.ResponsesToAnthropicRequest(req)
	require.NoError(t, err)
	body, err := json.Marshal(anth)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(body, "system.0.cache_control").Exists())
	require.False(t, gjson.GetBytes(body, "tools.0.cache_control").Exists())

	out := enforceCacheControlLimit(ensureConvertedAnthropicCacheBreakpoints(body))
	require.Equal(t, "You are a coding assistant.", gjson.GetBytes(out, "system.0.text").String())
	require.Equal(t, "ephemeral", gjson.GetBytes(out, "system.0.cache_control.type").String())
	require.Equal(t, "ephemeral", gjson.GetBytes(out, "tools.0.cache_control.type").String())
	require.Equal(t, "ephemeral", gjson.GetBytes(out, "messages.0.content.0.cache_control.type").String())
	require.LessOrEqual(t, countCacheControl(out), 4)
}

func TestEnsureConvertedAnthropicCacheBreakpoints_EmptyBodyNoop(t *testing.T) {
	require.Equal(t, []byte(nil), ensureConvertedAnthropicCacheBreakpoints(nil))
	require.Equal(t, `{}`, string(ensureConvertedAnthropicCacheBreakpoints([]byte(`{}`))))
}

func TestEnsureConvertedAnthropicCacheBreakpoints_ThenEnforceStaysAtMostFour(t *testing.T) {
	body := []byte(`{
		"system":"sys",
		"tools":[{"name":"a"},{"name":"b"}],
		"messages":[
			{"role":"user","content":"q1"},
			{"role":"assistant","content":"a1"},
			{"role":"user","content":"q2"},
			{"role":"assistant","content":"a2"},
			{"role":"user","content":"q3"}
		]
	}`)
	out := enforceCacheControlLimit(ensureConvertedAnthropicCacheBreakpoints(body))
	require.LessOrEqual(t, countCacheControl(out), 4)
	require.True(t, gjson.GetBytes(out, "messages.4.content.0.cache_control").Exists())
	require.True(t, gjson.GetBytes(out, "system.0.cache_control").Exists())
}

func countCacheControl(body []byte) int {
	n := 0
	var walk func(gjson.Result)
	walk = func(v gjson.Result) {
		if v.IsObject() {
			if v.Get("cache_control").Exists() {
				n++
			}
			v.ForEach(func(_, child gjson.Result) bool {
				walk(child)
				return true
			})
			return
		}
		if v.IsArray() {
			v.ForEach(func(_, child gjson.Result) bool {
				walk(child)
				return true
			})
		}
	}
	walk(gjson.ParseBytes(body))
	return n
}
