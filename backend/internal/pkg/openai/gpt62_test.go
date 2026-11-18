package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGPT62ModelIdentityAndCatalog(t *testing.T) {
	for _, model := range []string{"gpt-6.2-astra", "gpt-6.2-sol"} {
		t.Run(model, func(t *testing.T) {
			require.Contains(t, DefaultModelIDs(), model)
			for _, suffix := range []string{"", "-low", "-max", "-openai-compact"} {
				alias := "openai/" + model + suffix
				require.True(t, IsGPT62ModelSpelling(alias))
				require.False(t, IsGPT61SolModelSpelling(alias))
				require.False(t, IsGPT6SolOrLunaModelSpelling(alias))
				require.True(t, RequiresResponsesForToolCalls(alias))
				catalog, ok := CodexCatalogModelForSlug(alias)
				require.True(t, ok)
				require.Equal(t, model, catalog.Slug)
				require.Equal(t, strings.TrimSpace(catalog.ModelMessages.InstructionsTemplate), CodexBaseInstructionsForModel(alias))
				require.Contains(t, catalog.ModelMessages.InstructionsTemplate, "based on GPT-6")
			}
			for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
				require.NoError(t, ValidateGPT62ReasoningEffort(model, effort))
			}
			for _, effort := range []string{"none", "minimal"} {
				require.ErrorContains(t, ValidateGPT62ReasoningEffort(model, effort), model)
			}
			var descriptor struct {
				Slug   string `json:"slug"`
				Levels []struct {
					Effort string `json:"effort"`
				} `json:"supported_reasoning_levels"`
			}
			require.NoError(t, json.Unmarshal(CodexGPT62Metadata(model), &descriptor))
			require.Equal(t, model, descriptor.Slug)
			var efforts []string
			for _, level := range descriptor.Levels {
				efforts = append(efforts, level.Effort)
			}
			require.Equal(t, []string{"low", "medium", "high", "xhigh", "max", "ultra"}, efforts)
		})
	}
	for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol", "gpt-6.2", "gpt-6.2-astra-law", "gpt-6.2-astra-preview", "gpt-6.2-solitude", "gpt-6.2-sol-preview", "gpt-6.2-astra-ultra", "gpt-6.2-sol-ultra"} {
		require.False(t, IsGPT62ModelSpelling(model), model)
		require.Empty(t, CodexGPT62Metadata(model))
		require.NoError(t, ValidateGPT62ReasoningEffort(model, "none"))
	}
}
