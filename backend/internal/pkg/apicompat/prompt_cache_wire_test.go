package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesPromptCacheFieldsRoundTrip(t *testing.T) {
	payload := []byte(`{
		"model":"gpt-5.6-luna",
		"input":[{
			"type":"message",
			"role":"developer",
			"prompt_cache_breakpoint":{"mode":"explicit"},
			"content":[{
				"type":"input_text",
				"text":"stable instructions",
				"prompt_cache_breakpoint":{"mode":"explicit"}
			}]
		}],
		"prompt_cache_options":{"mode":"explicit","ttl":"30m"}
	}`)

	var req ResponsesRequest
	require.NoError(t, json.Unmarshal(payload, &req))
	require.NotNil(t, req.PromptCacheOptions)
	require.Equal(t, "explicit", req.PromptCacheOptions.Mode)
	require.Equal(t, "30m", req.PromptCacheOptions.TTL)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(req.Input, &items))
	require.Len(t, items, 1)
	require.NotNil(t, items[0].PromptCacheBreakpoint)
	require.Equal(t, "explicit", items[0].PromptCacheBreakpoint.Mode)

	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))
	require.Len(t, parts, 1)
	require.NotNil(t, parts[0].PromptCacheBreakpoint)
	require.Equal(t, "explicit", parts[0].PromptCacheBreakpoint.Mode)

	roundTrip, err := json.Marshal(req)
	require.NoError(t, err)
	require.JSONEq(t, string(payload), string(roundTrip))
}

func TestChatCompletionsToResponsesPreservesExplicitPromptCacheFields(t *testing.T) {
	var req ChatCompletionsRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"model":"gpt-5.6-luna",
		"messages":[{
			"role":"developer",
			"content":[{
				"type":"text",
				"text":"stable instructions",
				"prompt_cache_breakpoint":{"mode":"explicit"}
			}]
		}],
		"prompt_cache_options":{"mode":"explicit","ttl":"30m"}
	}`), &req))

	resp, err := ChatCompletionsToResponses(&req)
	require.NoError(t, err)
	require.NotNil(t, resp.PromptCacheOptions)
	require.Equal(t, "explicit", resp.PromptCacheOptions.Mode)
	require.Equal(t, "30m", resp.PromptCacheOptions.TTL)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	require.Len(t, items, 1)
	require.Equal(t, "developer", items[0].Role)

	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))
	require.Len(t, parts, 1)
	require.Equal(t, "input_text", parts[0].Type)
	require.NotNil(t, parts[0].PromptCacheBreakpoint)
	require.Equal(t, "explicit", parts[0].PromptCacheBreakpoint.Mode)
}

func TestChatCompletionsToResponsesPreservesAssistantPromptCacheBreakpoint(t *testing.T) {
	var req ChatCompletionsRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"model":"gpt-5.6-luna",
		"messages":[{
			"role":"assistant",
			"reasoning_content":"reasoning",
			"content":[
				{"type":"text","text":"stable answer","prompt_cache_breakpoint":{"mode":"explicit"}},
				{"type":"text","text":"dynamic suffix"}
			]
		}]
	}`), &req))

	resp, err := ChatCompletionsToResponses(&req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	require.Len(t, items, 1)
	require.Equal(t, "assistant", items[0].Role)

	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))
	require.Len(t, parts, 2)
	require.Equal(t, "<thinking>reasoning</thinking>\nstable answer", parts[0].Text)
	require.NotNil(t, parts[0].PromptCacheBreakpoint)
	require.Equal(t, "explicit", parts[0].PromptCacheBreakpoint.Mode)
	require.Equal(t, "dynamic suffix", parts[1].Text)
	require.Nil(t, parts[1].PromptCacheBreakpoint)
}

func TestChatCompletionsToResponsesDoesNotInjectPromptCacheFields(t *testing.T) {
	req := &ChatCompletionsRequest{
		Model:    "gpt-5.6-luna",
		Messages: []ChatMessage{{Role: "user", Content: json.RawMessage(`"hello"`)}},
	}

	resp, err := ChatCompletionsToResponses(req)
	require.NoError(t, err)
	require.Nil(t, resp.PromptCacheOptions)

	payload, err := json.Marshal(resp)
	require.NoError(t, err)
	var wire map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(payload, &wire))
	_, hasOptions := wire["prompt_cache_options"]
	require.False(t, hasOptions)

	var items []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	require.Len(t, items, 1)
	_, hasBreakpoint := items[0]["prompt_cache_breakpoint"]
	require.False(t, hasBreakpoint)
}
