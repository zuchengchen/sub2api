package service

import (
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/tidwall/sjson"
)

type openAICompatibleWireAPI string

const (
	openAICompatibleWireChat      openAICompatibleWireAPI = "chat_completions"
	openAICompatibleWireResponses openAICompatibleWireAPI = "responses"
)

// enforceOpenAICompatibleNonReasoning applies a server-owned provider policy
// after account/model routing. Client attempts to re-enable reasoning are
// removed or overwritten before the request reaches the upstream.
func enforceOpenAICompatibleNonReasoning(
	account *Account,
	upstreamModel string,
	body []byte,
	wireAPI openAICompatibleWireAPI,
) ([]byte, bool, error) {
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey {
		return body, false, nil
	}
	preset, ok := openai_compat.ResolveCompatibleProviderPreset(account.Extra)
	if !ok || !preset.ForceNonReasoning || !preset.SupportsModel(upstreamModel) {
		return body, false, nil
	}

	updated := body
	var err error
	for _, field := range []string{
		"thinking",
		"thinking_budget",
		"thinking_budget_tokens",
		"preserve_thinking",
		"reasoning",
		"reasoning_effort",
		"reasoningEffort",
		"enable_thinking",
		"output_config.effort",
	} {
		updated, err = sjson.DeleteBytes(updated, field)
		if err != nil {
			return nil, false, fmt.Errorf("remove managed reasoning field %q: %w", field, err)
		}
	}

	switch wireAPI {
	case openAICompatibleWireChat:
		switch preset.ChatReasoningStyle {
		case openai_compat.ChatReasoningStyleThinkingDisabled:
			updated, err = sjson.SetBytes(updated, "thinking.type", "disabled")
		case openai_compat.ChatReasoningStyleEnableFalse:
			updated, err = sjson.SetBytes(updated, "enable_thinking", false)
		default:
			return body, false, nil
		}
	case openAICompatibleWireResponses:
		if preset.ResponsesReasoning != openai_compat.ResponsesReasoningEffortNone {
			return body, false, nil
		}
		updated, err = sjson.SetBytes(updated, "reasoning.effort", "none")
		if err == nil && preset.ID == openai_compat.ProviderAlibabaQwen {
			updated, err = sjson.SetBytes(updated, "enable_thinking", false)
		}
	default:
		return nil, false, fmt.Errorf("unsupported OpenAI-compatible wire API %q", wireAPI)
	}
	if err != nil {
		return nil, false, fmt.Errorf("apply non-reasoning policy for %s: %w", preset.ID, err)
	}
	return updated, true, nil
}
