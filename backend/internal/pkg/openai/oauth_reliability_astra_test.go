//go:build unit

package openai

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOAuthReliabilityAstraReasoningEffortValidation(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-6", "openai/GPT_6_ASTRA", "gpt-6-astra-none", "gpt-6-astra-minimal", "gpt-6-astra-openai-compact"} {
		for _, effort := range []string{"none", "minimal", " NONE ", "MINIMAL"} {
			err := ValidateGPT6AstraReasoningEffort(model, effort)
			require.ErrorContains(t, err, "does not support reasoning effort", model)
			require.ErrorContains(t, err, "low, medium, high, xhigh or max")
		}
		for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
			require.NoError(t, ValidateGPT6AstraReasoningEffort(model, effort), model)
		}
	}
	for _, model := range []string{"gpt-6-luna", "gpt-6-sol", "gpt-6.1-sol", "gpt-5.5", "gpt-6-astra-preview", "gpt-6-astral", "unknown"} {
		require.NoError(t, ValidateGPT6AstraReasoningEffort(model, "none"), model)
		require.NoError(t, ValidateGPT6AstraReasoningEffort(model, "minimal"), model)
	}
}
