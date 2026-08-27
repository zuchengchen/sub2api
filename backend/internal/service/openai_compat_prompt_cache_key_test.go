package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func mustRawJSON(t *testing.T, s string) json.RawMessage {
	t.Helper()
	return json.RawMessage(s)
}

func TestShouldAutoInjectPromptCacheKeyForCompat(t *testing.T) {
	require.True(t, shouldAutoInjectPromptCacheKeyForCompat("gpt-5.5"))
	require.True(t, shouldAutoInjectPromptCacheKeyForCompat("gpt-5.5-pro"))
	require.True(t, shouldAutoInjectPromptCacheKeyForCompat("gpt-5.4"))
	require.True(t, shouldAutoInjectPromptCacheKeyForCompat("gpt-5.4-mini"))
	require.True(t, shouldAutoInjectPromptCacheKeyForCompat("gpt-5.2"))
	require.True(t, shouldAutoInjectPromptCacheKeyForCompat("gpt-5.3"))
	require.True(t, shouldAutoInjectPromptCacheKeyForCompat("gpt-5.3-codex"))
	require.True(t, shouldAutoInjectPromptCacheKeyForCompat("gpt-5.3-codex-spark"))
	require.False(t, shouldAutoInjectPromptCacheKeyForCompat("gpt-4o"))
}

func TestDeriveCompatPromptCacheKey_StableAcrossLaterTurns(t *testing.T) {
	base := &apicompat.ChatCompletionsRequest{
		Model: "gpt-5.4",
		Messages: []apicompat.ChatMessage{
			{Role: "system", Content: mustRawJSON(t, `"You are helpful."`)},
			{Role: "user", Content: mustRawJSON(t, `"Hello"`)},
		},
	}
	extended := &apicompat.ChatCompletionsRequest{
		Model: "gpt-5.4",
		Messages: []apicompat.ChatMessage{
			{Role: "system", Content: mustRawJSON(t, `"You are helpful."`)},
			{Role: "user", Content: mustRawJSON(t, `"Hello"`)},
			{Role: "assistant", Content: mustRawJSON(t, `"Hi there!"`)},
			{Role: "developer", Content: mustRawJSON(t, `"Turn-local formatting hint"`)},
			{Role: "user", Content: mustRawJSON(t, `"How are you?"`)},
		},
	}

	k1 := deriveCompatPromptCacheKey(base, "gpt-5.4")
	k2 := deriveCompatPromptCacheKey(extended, "gpt-5.4")
	require.Equal(t, k1, k2, "cache key should be stable across later turns")
	require.NotEmpty(t, k1)
}

func TestDeriveCompatPromptCacheKey_ReusablePrefixIgnoresFirstUser(t *testing.T) {
	tests := []struct {
		name  string
		apply func(*apicompat.ChatCompletionsRequest)
	}{
		{
			name: "system",
			apply: func(req *apicompat.ChatCompletionsRequest) {
				req.Messages = append([]apicompat.ChatMessage{{Role: "system", Content: mustRawJSON(t, `"Shared system prompt"`)}}, req.Messages...)
			},
		},
		{
			name: "developer",
			apply: func(req *apicompat.ChatCompletionsRequest) {
				req.Messages = append([]apicompat.ChatMessage{{Role: "developer", Content: mustRawJSON(t, `"Shared developer prompt"`)}}, req.Messages...)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := &apicompat.ChatCompletionsRequest{
				Model:    "gpt-5.6-luna",
				Messages: []apicompat.ChatMessage{{Role: "user", Content: mustRawJSON(t, `"Question A"`)}},
			}
			second := &apicompat.ChatCompletionsRequest{
				Model:    "gpt-5.6-luna",
				Messages: []apicompat.ChatMessage{{Role: "user", Content: mustRawJSON(t, `"Unrelated question B"`)}},
			}
			tt.apply(first)
			tt.apply(second)

			require.Equal(t,
				deriveCompatPromptCacheKey(first, "gpt-5.6-luna"),
				deriveCompatPromptCacheKey(second, "gpt-5.6-luna"),
				"a reusable prefix should route independent user prompts to the same cache group",
			)
		})
	}
}

func TestDeriveCompatPromptCacheKey_NonMessagePrefixKeepsFirstUserShard(t *testing.T) {
	tests := []struct {
		name  string
		apply func(*apicompat.ChatCompletionsRequest)
	}{
		{name: "instructions", apply: func(req *apicompat.ChatCompletionsRequest) {
			req.Instructions = "Shared top-level instructions"
		}},
		{name: "tools", apply: func(req *apicompat.ChatCompletionsRequest) {
			req.Tools = []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{Name: "lookup"}}}
		}},
		{name: "legacy functions", apply: func(req *apicompat.ChatCompletionsRequest) {
			req.Functions = []apicompat.ChatFunction{{Name: "lookup"}}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := &apicompat.ChatCompletionsRequest{
				Model:    "gpt-5.6-luna",
				Messages: []apicompat.ChatMessage{{Role: "user", Content: mustRawJSON(t, `"Question A"`)}},
			}
			second := &apicompat.ChatCompletionsRequest{
				Model:    "gpt-5.6-luna",
				Messages: []apicompat.ChatMessage{{Role: "user", Content: mustRawJSON(t, `"Question B"`)}},
			}
			tt.apply(first)
			tt.apply(second)

			require.NotEqual(t,
				deriveCompatPromptCacheKey(first, first.Model),
				deriveCompatPromptCacheKey(second, second.Model),
				"a prefix without a content breakpoint must retain user-level sharding",
			)
		})
	}
}

func TestDeriveCompatPromptCacheKey_DiffersAcrossSessions(t *testing.T) {
	req1 := &apicompat.ChatCompletionsRequest{
		Model: "gpt-5.4",
		Messages: []apicompat.ChatMessage{
			{Role: "user", Content: mustRawJSON(t, `"Question A"`)},
		},
	}
	req2 := &apicompat.ChatCompletionsRequest{
		Model: "gpt-5.4",
		Messages: []apicompat.ChatMessage{
			{Role: "user", Content: mustRawJSON(t, `"Question B"`)},
		},
	}

	k1 := deriveCompatPromptCacheKey(req1, "gpt-5.4")
	k2 := deriveCompatPromptCacheKey(req2, "gpt-5.4")
	require.NotEqual(t, k1, k2, "different first user messages should yield different keys")
}

func TestDeriveCompatPromptCacheKey_UserFallbackStableAcrossLaterTurns(t *testing.T) {
	base := &apicompat.ChatCompletionsRequest{
		Model:    "gpt-5.6-luna",
		Messages: []apicompat.ChatMessage{{Role: "user", Content: mustRawJSON(t, `"Question A"`)}},
	}
	extended := &apicompat.ChatCompletionsRequest{
		Model: "gpt-5.6-luna",
		Messages: []apicompat.ChatMessage{
			{Role: "user", Content: mustRawJSON(t, `"Question A"`)},
			{Role: "assistant", Content: mustRawJSON(t, `"Answer A"`)},
			{Role: "user", Content: mustRawJSON(t, `"Follow-up question"`)},
		},
	}

	require.Equal(t,
		deriveCompatPromptCacheKey(base, base.Model),
		deriveCompatPromptCacheKey(extended, extended.Model),
	)
}

func TestDeriveCompatPromptCacheKey_EmptyPrefixFallsBackToFirstUser(t *testing.T) {
	first := &apicompat.ChatCompletionsRequest{
		Model: "gpt-5.6-luna",
		Messages: []apicompat.ChatMessage{
			{Role: "system", Content: mustRawJSON(t, `"  "`)},
			{Role: "user", Content: mustRawJSON(t, `"Question A"`)},
		},
	}
	second := &apicompat.ChatCompletionsRequest{
		Model: "gpt-5.6-luna",
		Messages: []apicompat.ChatMessage{
			{Role: "system", Content: mustRawJSON(t, `"  "`)},
			{Role: "user", Content: mustRawJSON(t, `"Question B"`)},
		},
	}

	require.NotEqual(t,
		deriveCompatPromptCacheKey(first, "gpt-5.6-luna"),
		deriveCompatPromptCacheKey(second, "gpt-5.6-luna"),
	)

	withoutEmptyPrefix := &apicompat.ChatCompletionsRequest{
		Model:    "gpt-5.6-luna",
		Messages: []apicompat.ChatMessage{{Role: "user", Content: mustRawJSON(t, `"Question A"`)}},
	}
	require.Equal(t,
		deriveCompatPromptCacheKey(first, "gpt-5.6-luna"),
		deriveCompatPromptCacheKey(withoutEmptyPrefix, "gpt-5.6-luna"),
		"an empty system message must not fragment the user fallback group",
	)
}

func TestDeriveCompatPromptCacheKey_UncacheableSystemFallsBackToFirstUser(t *testing.T) {
	first := &apicompat.ChatCompletionsRequest{
		Model: "gpt-5.6-luna",
		Messages: []apicompat.ChatMessage{
			{Role: "system", Content: mustRawJSON(t, `[{"type":"text","text":"stable"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]`)},
			{Role: "user", Content: mustRawJSON(t, `"Question A"`)},
		},
	}
	second := &apicompat.ChatCompletionsRequest{
		Model: "gpt-5.6-luna",
		Messages: []apicompat.ChatMessage{
			{Role: "system", Content: first.Messages[0].Content},
			{Role: "user", Content: mustRawJSON(t, `"Question B"`)},
		},
	}

	require.NotEqual(t,
		deriveCompatPromptCacheKey(first, first.Model),
		deriveCompatPromptCacheKey(second, second.Model),
		"a system message that cannot be canonicalized must not create a shared routing key",
	)
}

func TestDeriveCompatPromptCacheKey_UnanchoredRequestReturnsEmpty(t *testing.T) {
	require.Empty(t, deriveCompatPromptCacheKey(&apicompat.ChatCompletionsRequest{
		Model: "gpt-5.6-luna",
	}, "gpt-5.6-luna"))
	require.Empty(t, deriveCompatPromptCacheKey(&apicompat.ChatCompletionsRequest{
		Model:    "gpt-5.6-luna",
		Messages: []apicompat.ChatMessage{{Role: "user", Content: mustRawJSON(t, `"  "`)}},
	}, "gpt-5.6-luna"))
}

func TestDeriveCompatPromptCacheKey_ChangesWithStablePrefixAndCacheSettings(t *testing.T) {
	boolPtr := func(value bool) *bool { return &value }
	baseRequest := func() *apicompat.ChatCompletionsRequest {
		return &apicompat.ChatCompletionsRequest{
			Model:             "gpt-5.6-luna",
			Instructions:      "Shared instructions",
			ReasoningEffort:   "medium",
			ToolChoice:        mustRawJSON(t, `{"type":"function","function":{"name":"lookup"}}`),
			ResponseFormat:    mustRawJSON(t, `{"type":"json_schema","json_schema":{"name":"result","schema":{"type":"object"}}}`),
			ParallelToolCalls: boolPtr(true),
			ServiceTier:       "default",
			Tools: []apicompat.ChatTool{{
				Type: "function",
				Function: &apicompat.ChatFunction{
					Name:       "lookup",
					Parameters: mustRawJSON(t, `{"type":"object","properties":{"key":{"type":"string"}}}`),
				},
			}},
			Functions: []apicompat.ChatFunction{{Name: "legacy_lookup"}},
			Messages: []apicompat.ChatMessage{
				{Role: "system", Content: mustRawJSON(t, `"System prompt"`)},
				{Role: "developer", Content: mustRawJSON(t, `"Developer prompt"`)},
				{Role: "user", Content: mustRawJSON(t, `"Question A"`)},
			},
		}
	}

	tests := []struct {
		name   string
		mutate func(*apicompat.ChatCompletionsRequest)
	}{
		{name: "reasoning effort", mutate: func(req *apicompat.ChatCompletionsRequest) { req.ReasoningEffort = "high" }},
		{name: "tool choice", mutate: func(req *apicompat.ChatCompletionsRequest) { req.ToolChoice = mustRawJSON(t, `"none"`) }},
		{name: "tools", mutate: func(req *apicompat.ChatCompletionsRequest) { req.Tools[0].Function.Name = "search" }},
		{name: "legacy functions", mutate: func(req *apicompat.ChatCompletionsRequest) { req.Functions[0].Name = "legacy_search" }},
		{name: "response format", mutate: func(req *apicompat.ChatCompletionsRequest) {
			req.ResponseFormat = mustRawJSON(t, `{"type":"json_object"}`)
		}},
		{name: "parallel tool calls", mutate: func(req *apicompat.ChatCompletionsRequest) { req.ParallelToolCalls = boolPtr(false) }},
		{name: "service tier", mutate: func(req *apicompat.ChatCompletionsRequest) { req.ServiceTier = "flex" }},
		{name: "instructions", mutate: func(req *apicompat.ChatCompletionsRequest) { req.Instructions = "Different instructions" }},
		{name: "system prompt", mutate: func(req *apicompat.ChatCompletionsRequest) {
			req.Messages[0].Content = mustRawJSON(t, `"Different system prompt"`)
		}},
		{name: "developer prompt", mutate: func(req *apicompat.ChatCompletionsRequest) {
			req.Messages[1].Content = mustRawJSON(t, `"Different developer prompt"`)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := baseRequest()
			second := baseRequest()
			tt.mutate(second)
			require.NotEqual(t,
				deriveCompatPromptCacheKey(first, "gpt-5.6-luna"),
				deriveCompatPromptCacheKey(second, "gpt-5.6-luna"),
			)
		})
	}

	require.NotEqual(t,
		deriveCompatPromptCacheKey(baseRequest(), "gpt-5.6-luna"),
		deriveCompatPromptCacheKey(baseRequest(), "gpt-5.6-terra"),
		"resolved upstream model must affect the cache key",
	)
}

func TestDeriveCompatPromptCacheKey_UsesEffectiveLegacyFunctionCall(t *testing.T) {
	first := &apicompat.ChatCompletionsRequest{
		Model:        "gpt-5.6-luna",
		Instructions: "Shared instructions",
		FunctionCall: mustRawJSON(t, `{"name":"lookup"}`),
		Messages: []apicompat.ChatMessage{
			{Role: "developer", Content: mustRawJSON(t, `"Shared developer prompt"`)},
			{Role: "user", Content: mustRawJSON(t, `"Question"`)},
		},
	}
	second := &apicompat.ChatCompletionsRequest{
		Model:        first.Model,
		Instructions: first.Instructions,
		FunctionCall: mustRawJSON(t, `{"name":"search"}`),
		Messages:     first.Messages,
	}
	require.NotEqual(t,
		deriveCompatPromptCacheKey(first, first.Model),
		deriveCompatPromptCacheKey(second, second.Model),
	)

	// Explicit tool_choice wins during Chat Completions -> Responses conversion,
	// so an ignored legacy function_call must not fragment the cache group.
	first.ToolChoice = mustRawJSON(t, `"auto"`)
	second.ToolChoice = first.ToolChoice
	require.Equal(t,
		deriveCompatPromptCacheKey(first, first.Model),
		deriveCompatPromptCacheKey(second, second.Model),
	)
}

func TestDeriveCompatPromptCacheKey_CanonicalizesJSONSettings(t *testing.T) {
	first := &apicompat.ChatCompletionsRequest{
		Model:          "gpt-5.6-luna",
		Instructions:   "Shared instructions",
		ToolChoice:     mustRawJSON(t, `{"type":"function","function":{"name":"lookup"}}`),
		ResponseFormat: mustRawJSON(t, `{"type":"json_schema","json_schema":{"name":"result","schema":{"type":"object"}}}`),
		Messages: []apicompat.ChatMessage{
			{Role: "developer", Content: mustRawJSON(t, `"Shared developer prompt"`)},
			{Role: "user", Content: mustRawJSON(t, `"Question A"`)},
		},
	}
	second := &apicompat.ChatCompletionsRequest{
		Model:          first.Model,
		Instructions:   first.Instructions,
		ToolChoice:     mustRawJSON(t, `{ "function": { "name": "lookup" }, "type": "function" }`),
		ResponseFormat: mustRawJSON(t, `{ "json_schema": { "schema": { "type": "object" }, "name": "result" }, "type": "json_schema" }`),
		Messages: []apicompat.ChatMessage{
			{Role: "developer", Content: mustRawJSON(t, `"Shared developer prompt"`)},
			{Role: "user", Content: mustRawJSON(t, `"Question B"`)},
		},
	}

	require.Equal(t,
		deriveCompatPromptCacheKey(first, first.Model),
		deriveCompatPromptCacheKey(second, second.Model),
	)
}

func TestDeriveCompatPromptCacheKey_NormalizesRoleAndServiceTierAliases(t *testing.T) {
	first := &apicompat.ChatCompletionsRequest{
		Model:       "gpt-5.6-luna",
		ServiceTier: "fast",
		Messages: []apicompat.ChatMessage{
			{Role: "SYSTEM", Content: mustRawJSON(t, `"Shared prompt"`)},
			{Role: "user", Content: mustRawJSON(t, `"Question A"`)},
		},
	}
	second := &apicompat.ChatCompletionsRequest{
		Model:       "gpt-5.6-luna",
		ServiceTier: "priority",
		Messages: []apicompat.ChatMessage{
			{Role: "system", Content: mustRawJSON(t, `"Shared prompt"`)},
			{Role: "user", Content: mustRawJSON(t, `"Question B"`)},
		},
	}

	require.Equal(t,
		deriveCompatPromptCacheKey(first, first.Model),
		deriveCompatPromptCacheKey(second, second.Model),
	)
}

func TestDeriveCompatPromptCacheKey_UsesResolvedSparkFamily(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{
		Model: "gpt-5.3-codex-spark",
		Messages: []apicompat.ChatMessage{
			{Role: "user", Content: mustRawJSON(t, `"Question A"`)},
		},
	}

	k1 := deriveCompatPromptCacheKey(req, "gpt-5.3-codex-spark")
	k2 := deriveCompatPromptCacheKey(req, " openai/gpt-5.3-codex-spark ")
	require.NotEmpty(t, k1)
	require.Equal(t, k1, k2, "resolved spark family should derive a stable compat cache key")
}

func TestDeriveAnthropicCompatPromptCacheKey_StableAcrossLaterTurns(t *testing.T) {
	base := &apicompat.AnthropicRequest{
		Model:  "claude-sonnet-4-5",
		System: mustRawJSON(t, `"You are helpful."`),
		Messages: []apicompat.AnthropicMessage{
			{Role: "user", Content: mustRawJSON(t, `"Open repo"`)},
		},
	}
	extended := &apicompat.AnthropicRequest{
		Model:  "claude-sonnet-4-5",
		System: mustRawJSON(t, `"You are helpful."`),
		Messages: []apicompat.AnthropicMessage{
			{Role: "user", Content: mustRawJSON(t, `"Open repo"`)},
			{Role: "assistant", Content: mustRawJSON(t, `"Opened."`)},
			{Role: "user", Content: mustRawJSON(t, `"Run tests"`)},
		},
	}

	k1 := deriveAnthropicCompatPromptCacheKey(base, "gpt-5.3-codex")
	k2 := deriveAnthropicCompatPromptCacheKey(extended, "gpt-5.3-codex")
	require.NotEmpty(t, k1)
	require.Equal(t, k1, k2, "cache key should stay stable as later Claude Code turns append history")
}

func TestDeriveAnthropicCompatPromptCacheKey_UsesCacheControlAnchors(t *testing.T) {
	base := &apicompat.AnthropicRequest{
		Model: "claude-sonnet-4-5",
		System: mustRawJSON(t, `[
			{"type":"text","text":"project instructions","cache_control":{"type":"ephemeral"}}
		]`),
		Messages: []apicompat.AnthropicMessage{
			{Role: "user", Content: mustRawJSON(t, `[
				{"type":"text","text":"repo anchor","cache_control":{"type":"ephemeral"}}
			]`)},
		},
	}
	extended := &apicompat.AnthropicRequest{
		Model:  base.Model,
		System: base.System,
		Messages: []apicompat.AnthropicMessage{
			base.Messages[0],
			{Role: "assistant", Content: mustRawJSON(t, `[{"type":"text","text":"Opened."}]`)},
			{Role: "user", Content: mustRawJSON(t, `[{"type":"text","text":"Run tests"}]`)},
		},
	}

	k1 := deriveAnthropicCompatPromptCacheKey(base, "gpt-5.4")
	k2 := deriveAnthropicCompatPromptCacheKey(extended, "gpt-5.4")
	require.NotEmpty(t, k1)
	require.Equal(t, k1, k2)
	require.True(t, strings.HasPrefix(k1, "anthropic-cache-"))
	require.False(t, strings.HasPrefix(k1, compatPromptCacheKeyPrefix))
}
