package service

import "maps"

const DefaultOpenAIOAuthConcurrency = 50

// ResolveAccountCreateConcurrency preserves an explicit zero (unlimited), while
// applying the OAuth default only when the caller omitted the field.
func ResolveAccountCreateConcurrency(platform, accountType string, provided *int) int {
	if provided != nil {
		return *provided
	}
	if platform == PlatformOpenAI && accountType == AccountTypeOAuth {
		return DefaultOpenAIOAuthConcurrency
	}
	return 0
}

func applyOpenAIOAuthCreateDefaults(account *Account, concurrencyProvided bool) {
	if !account.IsOpenAIOAuth() {
		return
	}
	if account.Concurrency == 0 && !concurrencyProvided {
		account.Concurrency = DefaultOpenAIOAuthConcurrency
	}
	// An explicit null means inherit. Presence, rather than truthiness, also
	// preserves old/nested false overrides and leaves their precedence intact.
	openaiExtra, _ := account.Extra[PlatformOpenAI].(map[string]any)
	for _, extra := range []map[string]any{account.Extra, openaiExtra} {
		for _, key := range []string{featureKeyCodexImageGenerationBridge, "codex_image_generation_bridge_enabled"} {
			if _, exists := extra[key]; exists {
				return
			}
		}
	}
	if account.CodexImageGenerationExplicitToolPolicy() == codexImageGenerationExplicitToolPolicyStrip {
		return
	}
	account.Extra = maps.Clone(account.Extra)
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	account.Extra[featureKeyCodexImageGenerationBridge] = true
}
