package openai_compat

import "strings"

// ExtraKeyCompatibleProvider identifies a managed OpenAI-compatible provider
// preset in accounts.extra. The transport platform remains "openai".
const ExtraKeyCompatibleProvider = "openai_compatible_provider"

// CompatibleProviderID is the stable persisted identifier for a managed
// OpenAI-compatible provider preset.
type CompatibleProviderID string

const (
	ProviderMiMo        = "mimo"
	ProviderZhipuGLM    = "zhipu_glm"
	ProviderAlibabaQwen = "alibaba_qwen"
)

// CompatibleProviderChatReasoningStyle describes the provider-native Chat
// Completions field used to disable reasoning.
type CompatibleProviderChatReasoningStyle string

// CompatibleProviderResponsesReasoning describes the provider-native
// Responses field used to disable reasoning.
type CompatibleProviderResponsesReasoning string

// CompatibleProviderPreset contains the server-owned account defaults and
// runtime policy for a supported OpenAI-compatible provider.
type CompatibleProviderPreset struct {
	ID                 CompatibleProviderID
	DisplayName        string
	BaseURL            string
	Models             []string
	ResponsesMode      ResponsesSupportMode
	ForceNonReasoning  bool
	ChatReasoningStyle CompatibleProviderChatReasoningStyle
	ResponsesReasoning CompatibleProviderResponsesReasoning
}

const (
	ChatReasoningStyleThinkingDisabled = "thinking_disabled"
	ChatReasoningStyleEnableFalse      = "enable_thinking_false"
	ResponsesReasoningEffortNone       = "reasoning_effort_none"
)

var compatibleProviderPresets = map[CompatibleProviderID]CompatibleProviderPreset{
	ProviderMiMo: {
		ID:                 ProviderMiMo,
		DisplayName:        "Xiaomi MiMo",
		BaseURL:            "https://api.xiaomimimo.com/v1",
		Models:             []string{"mimo-v2.5"},
		ResponsesMode:      ResponsesSupportModeForceResponses,
		ForceNonReasoning:  true,
		ChatReasoningStyle: ChatReasoningStyleThinkingDisabled,
		ResponsesReasoning: ResponsesReasoningEffortNone,
	},
	ProviderZhipuGLM: {
		ID:                 ProviderZhipuGLM,
		DisplayName:        "Zhipu GLM",
		BaseURL:            "https://open.bigmodel.cn/api/paas/v4",
		Models:             []string{"glm-4.7-flash", "glm-4.7-flashx"},
		ResponsesMode:      ResponsesSupportModeForceChatCompletions,
		ForceNonReasoning:  true,
		ChatReasoningStyle: ChatReasoningStyleThinkingDisabled,
	},
	ProviderAlibabaQwen: {
		ID:                 ProviderAlibabaQwen,
		DisplayName:        "Alibaba Cloud Qwen",
		BaseURL:            "https://dashscope.aliyuncs.com/compatible-mode/v1",
		Models:             []string{"qwen3.7-flash"},
		ResponsesMode:      ResponsesSupportModeForceResponses,
		ForceNonReasoning:  true,
		ChatReasoningStyle: ChatReasoningStyleEnableFalse,
		ResponsesReasoning: ResponsesReasoningEffortNone,
	},
}

// NormalizeCompatibleProviderID validates and canonicalizes a preset ID.
func NormalizeCompatibleProviderID(raw string) CompatibleProviderID {
	id := CompatibleProviderID(strings.ToLower(strings.TrimSpace(raw)))
	if _, ok := compatibleProviderPresets[id]; !ok {
		return ""
	}
	return id
}

// CompatibleProviderPresetByID returns a defensive copy of a provider preset.
func CompatibleProviderPresetByID(raw string) (CompatibleProviderPreset, bool) {
	id := NormalizeCompatibleProviderID(raw)
	if id == "" {
		return CompatibleProviderPreset{}, false
	}
	preset := compatibleProviderPresets[id]
	preset.Models = append([]string(nil), preset.Models...)
	return preset, true
}

// ResolveCompatibleProviderPreset reads a managed provider from account extra.
func ResolveCompatibleProviderPreset(extra map[string]any) (CompatibleProviderPreset, bool) {
	if extra == nil {
		return CompatibleProviderPreset{}, false
	}
	raw, ok := extra[ExtraKeyCompatibleProvider].(string)
	if !ok {
		return CompatibleProviderPreset{}, false
	}
	return CompatibleProviderPresetByID(raw)
}

func (p CompatibleProviderPreset) SupportsModel(model string) bool {
	model = strings.TrimSpace(model)
	for _, candidate := range p.Models {
		if model == candidate {
			return true
		}
	}
	return false
}

func (p CompatibleProviderPreset) ModelMapping() map[string]any {
	mapping := make(map[string]any, len(p.Models))
	for _, model := range p.Models {
		mapping[model] = model
	}
	return mapping
}
