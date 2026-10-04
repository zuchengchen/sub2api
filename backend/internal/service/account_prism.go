package service

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
)

const PrismBrowserModelsKey = "openai_prism_browser_models"

var prismBrowserModels = [...]string{"gpt-6.1-sol"}

func PrismBrowserSupportedModels() []string {
	return append([]string(nil), prismBrowserModels[:]...)
}

func isPrismBrowserModel(model string) bool {
	return prismBrowserCanonicalModel(model) == "gpt-6.1-sol"
}

func prismBrowserCanonicalModel(model string) string {
	canonical := canonicalizeOpenAIModelAliasSpelling(strings.TrimSpace(model))
	if openai.IsGPT61SolModelSpelling(canonical) && !prismBrowserCompactSpelling(canonical) {
		return "gpt-6.1-sol"
	}
	return ""
}

func prismBrowserCompactSpelling(model string) bool {
	canonical := canonicalizeOpenAIModelAliasSpelling(strings.TrimSpace(model))
	return strings.HasSuffix(canonical, "-openai-compact") && openai.IsGPT61SolModelSpelling(canonical)
}

func accountUsesPrismBrowser(account *Account, cfg *config.Config) bool {
	return accountHasPrismBrowser(account) && cfg != nil && cfg.Gateway.PrismBrowser.Enabled
}

func accountHasPrismBrowser(account *Account) bool {
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth ||
		account.IsShadow() || account.IsOpenAIAgentIdentity() || account.IsOpenAIPersonalAccessToken() {
		return false
	}
	if account.Extra == nil {
		return true
	}
	raw, configured := account.Extra["openai_prism_browser"]
	if !configured {
		return true
	}
	enabled, ok := raw.(bool)
	return ok && enabled
}

func shouldAttemptPrismBrowser(s *OpenAIGatewayService, c *gin.Context, account *Account, requestedModel string) bool {
	if s == nil || !accountUsesPrismBrowser(account, s.cfg) {
		return false
	}
	if GetOpenAIClientTransport(c) == OpenAIClientTransportWS {
		return false
	}
	if prismBrowserCompactSpelling(requestedModel) {
		return true
	}
	return account.IsPrismBrowserEnabledForModel(requestedModel)
}

func (a *Account) IsPrismBrowserEnabledForModel(requestedModel string) bool {
	if !accountHasPrismBrowser(a) {
		return false
	}
	if prismBrowserCompactSpelling(requestedModel) {
		return false
	}
	return a.isPrismBrowserUpstreamModelEnabled(a.GetMappedModel(strings.TrimSpace(requestedModel)))
}

func (a *Account) isPrismBrowserUpstreamModelEnabled(upstream string) bool {
	if !accountHasPrismBrowser(a) {
		return false
	}
	canonical := prismBrowserCanonicalModel(upstream)
	if canonical == "" || !isPrismBrowserModel(canonical) {
		return false
	}
	raw, configured := a.Extra[PrismBrowserModelsKey]
	if !configured {
		return true
	}
	switch models := raw.(type) {
	case []string:
		for _, model := range models {
			if prismBrowserCanonicalModel(model) == canonical {
				return true
			}
		}
	case []any:
		for _, value := range models {
			if model, ok := value.(string); ok && prismBrowserCanonicalModel(model) == canonical {
				return true
			}
		}
	}
	return false
}
