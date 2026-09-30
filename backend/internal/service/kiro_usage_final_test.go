package service

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// kiro-rs reconciles a request-time cache_creation estimate into cache_read at
// stream end and marks the final message_delta usage with usage_final=true.
const (
	kiroStart       = `{"type":"message_start","message":{"usage":{"input_tokens":0,"output_tokens":1,"cache_creation_input_tokens":828840,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":828840}}}}`
	kiroFinalDelta  = `{"type":"message_delta","usage":{"input_tokens":0,"output_tokens":387,"cache_creation_input_tokens":0,"cache_read_input_tokens":828840,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"usage_final":true,"credit_usage":2.913}}`
	legacyZeroDelta = `{"type":"message_delta","usage":{"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`
)

func requireKiroReconciled(t *testing.T, u *ClaudeUsage) {
	t.Helper()
	require.Equal(t, 0, u.InputTokens)
	require.Equal(t, 387, u.OutputTokens)
	require.Equal(t, 0, u.CacheCreationInputTokens, "final delta creation=0 must override message_start")
	require.Equal(t, 0, u.CacheCreation1hTokens)
	require.Equal(t, 828840, u.CacheReadInputTokens)
}

func TestParseSSEUsage_UsageFinalOverridesStartWithZero(t *testing.T) {
	svc := &GatewayService{}
	u := &ClaudeUsage{}
	svc.parseSSEUsage(kiroStart, u)
	svc.parseSSEUsage(kiroFinalDelta, u)
	requireKiroReconciled(t, u)
}

func TestParseSSEUsagePassthrough_UsageFinalOverridesStartWithZero(t *testing.T) {
	u := &ClaudeUsage{}
	parseSSEUsagePassthrough(kiroStart, u)
	parseSSEUsagePassthrough(kiroFinalDelta, u)
	requireKiroReconciled(t, u)
}

func TestParseSSEUsage_ZeroDeltaWithoutMarkerKeepsStart(t *testing.T) {
	svc := &GatewayService{}
	u := &ClaudeUsage{}
	svc.parseSSEUsage(kiroStart, u)
	svc.parseSSEUsage(legacyZeroDelta, u)
	require.Equal(t, 828840, u.CacheCreationInputTokens)
	require.Equal(t, 50, u.OutputTokens)

	p := &ClaudeUsage{}
	parseSSEUsagePassthrough(kiroStart, p)
	parseSSEUsagePassthrough(legacyZeroDelta, p)
	require.Equal(t, 828840, p.CacheCreationInputTokens)
	require.Equal(t, 50, p.OutputTokens)
}

func TestMergeAnthropicUsage_UsageFinalOverridesStart(t *testing.T) {
	var start struct {
		Message struct {
			Usage apicompat.AnthropicUsage `json:"usage"`
		} `json:"message"`
	}
	var delta struct {
		Usage apicompat.AnthropicUsage `json:"usage"`
	}
	require.NoError(t, json.Unmarshal([]byte(kiroStart), &start))
	require.NoError(t, json.Unmarshal([]byte(kiroFinalDelta), &delta))
	require.True(t, delta.Usage.UsageFinal)

	u := &ClaudeUsage{}
	mergeAnthropicUsage(u, start.Message.Usage)
	require.Equal(t, 828840, u.CacheCreationInputTokens)
	require.Equal(t, 828840, u.CacheCreation1hTokens, "message_start 1h split must be kept for billing")
	require.Equal(t, 0, u.CacheCreation5mTokens)
	mergeAnthropicUsage(u, delta.Usage)
	require.Equal(t, 0, u.InputTokens)
	require.Equal(t, 0, u.CacheCreationInputTokens)
	require.Equal(t, 0, u.CacheCreation1hTokens)
	require.Equal(t, 828840, u.CacheReadInputTokens)
	require.Equal(t, 387, u.OutputTokens)
}

func TestMergeAnthropicUsage_CacheCreationBreakdown(t *testing.T) {
	var start struct {
		Message struct {
			Usage apicompat.AnthropicUsage `json:"usage"`
		} `json:"message"`
	}
	require.NoError(t, json.Unmarshal([]byte(kiroStart), &start))

	// A delta without cache_creation keeps the start split.
	u := &ClaudeUsage{}
	mergeAnthropicUsage(u, start.Message.Usage)
	mergeAnthropicUsage(u, apicompat.AnthropicUsage{OutputTokens: 9})
	require.Equal(t, 828840, u.CacheCreation1hTokens)
	require.Equal(t, 0, u.CacheCreation5mTokens)

	// A present sub-field overrides, including 0; an absent one is left alone.
	var partial apicompat.AnthropicUsage
	require.NoError(t, json.Unmarshal([]byte(`{"output_tokens":9,"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":100}}`), &partial))
	mergeAnthropicUsage(u, partial)
	require.Equal(t, 100, u.CacheCreation5mTokens)
	require.Equal(t, 828840, u.CacheCreation1hTokens)

	// Upstreams that never send the object leave both at 0 (billing falls back to 5m).
	legacy := &ClaudeUsage{}
	mergeAnthropicUsage(legacy, apicompat.AnthropicUsage{InputTokens: 10, CacheCreationInputTokens: 50})
	require.Equal(t, 50, legacy.CacheCreationInputTokens)
	require.Zero(t, legacy.CacheCreation5mTokens+legacy.CacheCreation1hTokens)

	// The field is not echoed when absent.
	raw, err := json.Marshal(apicompat.AnthropicUsage{InputTokens: 1})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "cache_creation\"")
}
