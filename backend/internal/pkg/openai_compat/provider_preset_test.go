package openai_compat

import "testing"

func TestCompatibleProviderPresetByID(t *testing.T) {
	tests := []struct {
		id            CompatibleProviderID
		baseURL       string
		models        []string
		responsesMode ResponsesSupportMode
		chatStyle     CompatibleProviderChatReasoningStyle
	}{
		{ProviderMiMo, "https://api.xiaomimimo.com/v1", []string{"mimo-v2.5"}, ResponsesSupportModeForceResponses, ChatReasoningStyleThinkingDisabled},
		{ProviderZhipuGLM, "https://open.bigmodel.cn/api/paas/v4", []string{"glm-4.7-flash", "glm-4.7-flashx"}, ResponsesSupportModeForceChatCompletions, ChatReasoningStyleThinkingDisabled},
		{ProviderAlibabaQwen, "https://dashscope.aliyuncs.com/compatible-mode/v1", []string{"qwen3.7-flash"}, ResponsesSupportModeForceResponses, ChatReasoningStyleEnableFalse},
	}

	for _, tc := range tests {
		t.Run(string(tc.id), func(t *testing.T) {
			preset, ok := CompatibleProviderPresetByID(string(tc.id))
			if !ok {
				t.Fatalf("preset %q not found", tc.id)
			}
			if preset.BaseURL != tc.baseURL || preset.ResponsesMode != tc.responsesMode || preset.ChatReasoningStyle != tc.chatStyle {
				t.Fatalf("unexpected preset: %+v", preset)
			}
			if !preset.ForceNonReasoning || len(preset.Models) != len(tc.models) {
				t.Fatalf("unexpected policy/models: %+v", preset)
			}
			for i := range tc.models {
				if preset.Models[i] != tc.models[i] || !preset.SupportsModel(tc.models[i]) {
					t.Fatalf("unexpected model list: %#v", preset.Models)
				}
			}
		})
	}
}

func TestCompatibleProviderPresetByIDRejectsUnknownAndCopiesModels(t *testing.T) {
	if _, ok := CompatibleProviderPresetByID("unknown"); ok {
		t.Fatal("unknown preset must be rejected")
	}
	preset, ok := CompatibleProviderPresetByID(string(ProviderMiMo))
	if !ok {
		t.Fatal("MiMo preset not found")
	}
	preset.Models[0] = "mutated"
	again, _ := CompatibleProviderPresetByID(string(ProviderMiMo))
	if again.Models[0] != "mimo-v2.5" {
		t.Fatalf("registry models were mutated: %#v", again.Models)
	}
}

func TestResolveCompatibleProviderPreset(t *testing.T) {
	preset, ok := ResolveCompatibleProviderPreset(map[string]any{ExtraKeyCompatibleProvider: " ALIBABA_QWEN "})
	if !ok || preset.ID != ProviderAlibabaQwen {
		t.Fatalf("unexpected preset: %+v ok=%v", preset, ok)
	}
	if _, ok := ResolveCompatibleProviderPreset(map[string]any{ExtraKeyCompatibleProvider: 1}); ok {
		t.Fatal("non-string preset must be rejected")
	}
}
