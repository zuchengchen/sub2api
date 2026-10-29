package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

func isRetiredOpenAIModel(account *Account, model string) bool {
	return openai.IsRetiredSparkModel(model) || isRetiredOpenAISubscriptionModel(account, model)
}

// Spark is removed for every upstream, including explicit replacements of its
// public name. Check before normalization, passthrough and protocol fallbacks.
func validateRetiredOpenAIRequestModel(account *Account, requestedModel string, requireCompact bool, mappedModel string) error {
	models := []string{requestedModel, resolveOpenAIAccountUpstreamModelForRequest(account, requestedModel, requireCompact)}
	if account != nil && (!account.IsOpenAIPassthroughEnabled() || shouldForwardOpenAIResponsesViaRawChatCompletions(account)) {
		models = append(models, account.GetMappedModel(requestedModel))
	}
	if mappedModel != "" {
		models = append(models, mappedModel)
	}
	for _, model := range models {
		if openai.IsRetiredSparkModel(model) {
			return fmt.Errorf("model %q has been retired", strings.TrimSpace(model))
		}
	}
	return nil
}

// GPT-5.5 retired from Codex with ChatGPT sign-in on 2026-10-14 at
// 19:00 UTC. API-key upstreams remain supported. Do not infer upstream
// eligibility from the API key a client uses to authenticate to this gateway.
// https://learn.chatgpt.com/docs/models#gpt-55-retirement
func isRetiredOpenAISubscriptionModel(account *Account, upstreamModel string) bool {
	if account == nil || !account.IsOpenAI() || !account.IsOpenAIOAuthLike() {
		return false
	}
	model := canonicalizeOpenAIModelAliasSpelling(upstreamModel)
	return model == "gpt-5.5" || strings.HasPrefix(model, "gpt-5.5-")
}

// Only remove entries blocked by retirement. Unknown models and other platforms
// retain their existing listing policy. An explicitly mapped replacement is
// allowed, but a public alias targeting retired GPT-5.5 is not.
func openAIModelRetiredForAccounts(accounts []Account, modelID string) bool {
	if openai.IsRetiredSparkModel(modelID) {
		return true
	}
	retired := false
	for i := range accounts {
		account := &accounts[i]
		target := resolveOpenAIAccountUpstreamModelForRequest(account, modelID, false)
		if !account.IsOpenAI() && !openai.IsRetiredSparkModel(target) {
			continue
		}
		if isRetiredOpenAIModel(account, target) {
			retired = true
		} else if account.IsModelSupported(modelID) {
			return false
		}
	}
	return retired
}

func filterRetiredOpenAIModelIDs(accounts []Account, models []string) []string {
	filtered := make([]string, 0, len(models))
	for _, model := range models {
		if !openAIModelRetiredForAccounts(accounts, model) {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

// Entries must already use client-visible names, not raw upstream IDs. Resolve
// each public name exactly as routing does, after projection and before the
// final client ETag is calculated. Preserve unknown fields.
func filterRetiredOpenAIPublicModelsBody(body []byte, accounts []Account, codex bool) ([]byte, bool, error) {
	field, idField := "data", "id"
	if codex {
		field, idField = "models", "slug"
	}
	envelope, entries, err := modelCatalogEntries(body, field)
	if err != nil {
		return nil, false, err
	}
	filtered := make([]json.RawMessage, 0, len(entries))
	for _, raw := range entries {
		var entry map[string]json.RawMessage
		var id string
		if json.Unmarshal(raw, &entry) == nil {
			_ = json.Unmarshal(entry[idField], &id)
		}
		if !openAIModelRetiredForAccounts(accounts, id) {
			filtered = append(filtered, raw)
		}
	}
	if len(filtered) == len(entries) {
		return body, false, nil
	}
	envelope[field], err = json.Marshal(filtered)
	if err != nil {
		return nil, false, err
	}
	encoded, err := json.Marshal(envelope)
	return encoded, true, err
}

// Compact-only administrator mappings are evaluated against their actual
// target, without treating an old client-visible name as a retired target.
func supportsOpenAIRequestModel(account *Account, requestedModel string, requireCompact bool) bool {
	if validateRetiredOpenAIRequestModel(account, requestedModel, requireCompact, "") != nil {
		return false
	}
	if isRetiredOpenAISubscriptionModel(account, resolveOpenAIAccountUpstreamModelForRequest(account, requestedModel, requireCompact)) {
		return false
	}
	return account.isModelMappingSupported(requestedModel)
}
