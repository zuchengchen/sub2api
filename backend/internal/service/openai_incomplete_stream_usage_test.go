package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplyEstimatedOpenAIUsageIfMissingEstimatesChatPromptAndOutput(t *testing.T) {
	t.Parallel()

	usage := OpenAIUsage{}
	applyEstimatedOpenAIUsageIfMissing(
		&usage,
		"gpt-5.6-luna",
		[]byte(`{"model":"gpt-5.6-luna","messages":[{"role":"user","content":"hello there"}]}`),
		"partial output text",
	)
	require.Greater(t, usage.InputTokens, 0)
	require.Greater(t, usage.OutputTokens, 0)
}

func TestApplyEstimatedOpenAIUsageIfMissingDoesNotOverrideRealUsage(t *testing.T) {
	t.Parallel()

	usage := OpenAIUsage{InputTokens: 12, OutputTokens: 34}
	applyEstimatedOpenAIUsageIfMissing(
		&usage,
		"gpt-5.6-luna",
		[]byte(`{"model":"gpt-5.6-luna","messages":[{"role":"user","content":"hello there"}]}`),
		"ignored",
	)
	require.Equal(t, 12, usage.InputTokens)
	require.Equal(t, 34, usage.OutputTokens)
}

func TestApplyEstimatedOpenAIUsageIfMissingSkipsEmptyStreamAndBody(t *testing.T) {
	t.Parallel()

	usage := OpenAIUsage{}
	applyEstimatedOpenAIUsageIfMissing(&usage, "gpt-5.6-luna", nil, "")
	require.False(t, openAIUsageHasTokens(&usage))
}

func TestIsOpenAIStreamedBillingDelta(t *testing.T) {
	t.Parallel()

	require.True(t, isOpenAIStreamedBillingDelta("response.output_text.delta"))
	require.True(t, isOpenAIStreamedBillingDelta("response.reasoning_text.delta"))
	require.False(t, isOpenAIStreamedBillingDelta("response.created"))
	require.False(t, isOpenAIStreamedBillingDelta("response.completed"))
}
