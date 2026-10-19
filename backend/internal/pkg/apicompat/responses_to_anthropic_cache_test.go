package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesToAnthropicRequest_MapsPromptCacheBreakpoint(t *testing.T) {
	req := &ResponsesRequest{
		Model: "claude-sonnet-4-20250514",
		Input: json.RawMessage(`[
			{"role":"user","content":[{"type":"input_text","text":"stable prefix","prompt_cache_breakpoint":{"mode":"explicit"}},{"type":"input_text","text":"dynamic"}]}
		]`),
	}

	out, err := ResponsesToAnthropicRequest(req)
	require.NoError(t, err)
	require.Len(t, out.Messages, 1)

	var blocks []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(out.Messages[0].Content, &blocks))
	require.Len(t, blocks, 2)
	require.NotNil(t, blocks[0].CacheControl)
	require.Equal(t, "ephemeral", blocks[0].CacheControl.Type)
	require.Empty(t, blocks[0].CacheControl.TTL)
	require.Nil(t, blocks[1].CacheControl)
}

func TestResponsesToAnthropicRequest_MapsItemBreakpointAndSupportedTTL(t *testing.T) {
	req := &ResponsesRequest{
		Model: "claude-sonnet-4-20250514",
		Input: json.RawMessage(`[
			{"role":"user","prompt_cache_breakpoint":{"mode":"explicit"},"content":"hello"}
		]`),
		PromptCacheOptions: &PromptCacheOptions{Mode: "explicit", TTL: "1h"},
	}

	out, err := ResponsesToAnthropicRequest(req)
	require.NoError(t, err)
	require.Len(t, out.Messages, 1)

	var blocks []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(out.Messages[0].Content, &blocks))
	require.Len(t, blocks, 1)
	require.Equal(t, "hello", blocks[0].Text)
	require.NotNil(t, blocks[0].CacheControl)
	require.Equal(t, "ephemeral", blocks[0].CacheControl.Type)
	require.Equal(t, "1h", blocks[0].CacheControl.TTL)
}

func TestResponsesToAnthropicRequest_DropsUnsupportedPromptCacheTTL(t *testing.T) {
	req := &ResponsesRequest{
		Model: "claude-sonnet-4-20250514",
		Input: json.RawMessage(`[
			{"role":"user","content":[{"type":"input_text","text":"prefix","prompt_cache_breakpoint":{"mode":"explicit"}}]}
		]`),
		PromptCacheOptions: &PromptCacheOptions{Mode: "explicit", TTL: "30m"},
	}

	out, err := ResponsesToAnthropicRequest(req)
	require.NoError(t, err)

	var blocks []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(out.Messages[0].Content, &blocks))
	require.Len(t, blocks, 1)
	require.NotNil(t, blocks[0].CacheControl)
	require.Empty(t, blocks[0].CacheControl.TTL)
}

func TestResponsesToAnthropicRequest_DoesNotInjectBreakpointsWithoutClientMarker(t *testing.T) {
	req := &ResponsesRequest{
		Model:        "claude-sonnet-4-20250514",
		Instructions: "You are a coding assistant.",
		Input:        json.RawMessage(`[{"role":"user","content":"hi"}]`),
		Tools: []ResponsesTool{{
			Type:       "function",
			Name:       "read_file",
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		}},
	}

	out, err := ResponsesToAnthropicRequest(req)
	require.NoError(t, err)

	var system string
	require.NoError(t, json.Unmarshal(out.System, &system))
	require.Equal(t, "You are a coding assistant.", system)
	require.Nil(t, out.Tools[0].CacheControl)

	var content string
	require.NoError(t, json.Unmarshal(out.Messages[0].Content, &content))
	require.Equal(t, "hi", content)
}

func TestChatCompletionsChain_PreservesPromptCacheBreakpoint(t *testing.T) {
	ccReq := &ChatCompletionsRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []ChatMessage{{
			Role:    "user",
			Content: json.RawMessage(`[{"type":"text","text":"stable","prompt_cache_breakpoint":{"mode":"explicit"}},{"type":"text","text":"tail"}]`),
		}},
		PromptCacheOptions: &PromptCacheOptions{Mode: "explicit", TTL: "5m"},
	}

	respReq, err := ChatCompletionsToResponses(ccReq)
	require.NoError(t, err)
	out, err := ResponsesToAnthropicRequest(respReq)
	require.NoError(t, err)

	var blocks []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(out.Messages[0].Content, &blocks))
	require.Len(t, blocks, 2)
	require.NotNil(t, blocks[0].CacheControl)
	require.Equal(t, "5m", blocks[0].CacheControl.TTL)
	require.Nil(t, blocks[1].CacheControl)
}
