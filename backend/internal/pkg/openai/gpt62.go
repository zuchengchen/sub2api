package openai

import (
	_ "embed"
	"fmt"
	"strings"
)

// GPT-6.2 descriptors are pinned independently so adding a new family does not
// change existing models' prompts or wire capabilities. Source: openai/codex
// 6c0cee502d215e5df2dd9a303cf2e252229c9e12, codex-rs/models-manager/models.json.
//
//go:embed codex_gpt62_astra.json
var codexGPT62AstraMetadata []byte

//go:embed codex_gpt62_sol.json
var codexGPT62SolMetadata []byte

func isGPT62ModelSpelling(model, base string) bool {
	canonical := CanonicalizeOpenAIModelAliasSpelling(model)
	if canonical == base {
		return true
	}
	suffix, ok := strings.CutPrefix(canonical, base+"-")
	if !ok {
		return false
	}
	switch suffix {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max", "openai-compact":
		return true
	default:
		return false
	}
}

func IsGPT62AstraModelSpelling(model string) bool {
	return isGPT62ModelSpelling(model, "gpt-6.2-astra")
}

func IsGPT62SolModelSpelling(model string) bool {
	return isGPT62ModelSpelling(model, "gpt-6.2-sol")
}

func IsGPT62ModelSpelling(model string) bool {
	return IsGPT62AstraModelSpelling(model) || IsGPT62SolModelSpelling(model)
}

// CodexGPT62Metadata returns the official descriptor for a recognized 6.2 model.
// The embedded bytes are immutable; callers must not modify them.
func CodexGPT62Metadata(model string) []byte {
	switch {
	case IsGPT62AstraModelSpelling(model):
		return codexGPT62AstraMetadata
	case IsGPT62SolModelSpelling(model):
		return codexGPT62SolMetadata
	default:
		return nil
	}
}

// ValidateGPT62ReasoningEffort rejects explicitly disabled reasoning. Ultra is
// a Codex workflow selection and remains available in the model manifest.
func ValidateGPT62ReasoningEffort(model, effort string) error {
	if !IsGPT62ModelSpelling(model) {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "none", "minimal":
		base := "gpt-6.2-sol"
		if IsGPT62AstraModelSpelling(model) {
			base = "gpt-6.2-astra"
		}
		return fmt.Errorf("%s does not support reasoning effort %q; use low, medium, high, xhigh or max, or an explicitly configured mapping", base, effort)
	default:
		return nil
	}
}
