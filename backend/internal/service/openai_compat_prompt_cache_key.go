package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

const compatPromptCacheKeyPrefix = "compat_cc_"

func shouldAutoInjectPromptCacheKeyForCompat(model string) bool {
	trimmed := strings.TrimSpace(strings.ToLower(model))
	// 仅对 Codex OAuth 路径支持的 GPT-5 族开启自动注入，避免 normalizeCodexModel
	// 的默认兜底把任意模型（如 gpt-4o、claude-*）误判为 gpt-5.4。
	if !strings.Contains(trimmed, "gpt-5") && !strings.Contains(trimmed, "codex") {
		return false
	}
	normalized := strings.TrimSpace(strings.ToLower(normalizeCodexModel(trimmed)))
	return strings.HasPrefix(normalized, "gpt-5") || strings.Contains(normalized, "codex")
}

func deriveCompatPromptCacheKey(req *apicompat.ChatCompletionsRequest, mappedModel string) string {
	if req == nil {
		return ""
	}

	normalizedModel := normalizeCodexModel(strings.TrimSpace(mappedModel))
	if normalizedModel == "" {
		normalizedModel = normalizeCodexModel(strings.TrimSpace(req.Model))
	}
	if normalizedModel == "" {
		normalizedModel = strings.TrimSpace(req.Model)
	}

	seedParts := make([]string, 0, 12)
	appendCompatPromptCacheSeedPart(&seedParts, "model", normalizedModel)

	// These settings affect the rendered prompt or the upstream cache-routing
	// pool. Keep them in the key so requests with incompatible cache prefixes do
	// not compete for the same routing identity.
	if effort := strings.TrimSpace(req.ReasoningEffort); effort != "" {
		appendCompatPromptCacheSeedPart(&seedParts, "reasoning_effort", effort)
	}
	if len(req.ToolChoice) > 0 {
		appendCompatPromptCacheSeedPart(&seedParts, "tool_choice", normalizeCompatSeedJSON(req.ToolChoice))
	} else if len(req.FunctionCall) > 0 {
		// Legacy function_call is converted to tool_choice only when an explicit
		// tool_choice is absent.
		appendCompatPromptCacheSeedPart(&seedParts, "function_call", normalizeCompatSeedJSON(req.FunctionCall))
	}
	if len(req.ResponseFormat) > 0 {
		appendCompatPromptCacheSeedPart(&seedParts, "response_format", normalizeCompatSeedJSON(req.ResponseFormat))
	}
	if req.ParallelToolCalls != nil {
		appendCompatPromptCacheSeedPart(&seedParts, "parallel_tool_calls", fmt.Sprintf("%t", *req.ParallelToolCalls))
	}
	if serviceTier := normalizedOpenAIServiceTierValue(req.ServiceTier); serviceTier != "" {
		appendCompatPromptCacheSeedPart(&seedParts, "service_tier", serviceTier)
	}

	hasStablePrefix := false
	if len(req.Tools) > 0 {
		if raw, err := json.Marshal(req.Tools); err == nil {
			appendCompatPromptCacheSeedPart(&seedParts, "tools", normalizeCompatSeedJSON(raw))
		}
	}
	if len(req.Functions) > 0 {
		if raw, err := json.Marshal(req.Functions); err == nil {
			appendCompatPromptCacheSeedPart(&seedParts, "functions", normalizeCompatSeedJSON(raw))
		}
	}
	if strings.TrimSpace(req.Instructions) != "" {
		appendCompatPromptCacheSeedPart(&seedParts, "instructions", req.Instructions)
	}

	// Only leading system/developer messages form a reusable prompt prefix.
	// System-like messages appended after a user/assistant turn are conversation
	// history and must not make the routing key change on later turns.
	prefixOpen := true
	firstUserCaptured := false
	firstUserContent := ""
	firstUserMeaningful := false
	for _, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		if prefixOpen {
			switch role {
			case "system", "developer":
				cacheable := hasMeaningfulCompatPromptCacheContent(msg.Content)
				if role == "system" {
					cacheable = hasLosslessCompatPromptCacheSystemText(msg.Content)
				}
				if cacheable {
					content := normalizeCompatSeedJSON(msg.Content)
					appendCompatPromptCacheSeedPart(&seedParts, role, content)
					hasStablePrefix = true
				}
				continue
			default:
				prefixOpen = false
			}
		}

		if role == "user" && !firstUserCaptured {
			firstUserContent = normalizeCompatSeedJSON(msg.Content)
			firstUserMeaningful = hasMeaningfulCompatPromptCacheContent(msg.Content)
			firstUserCaptured = true
		}
	}

	if !hasStablePrefix {
		// Without a reusable static prefix, retain the first user message as a
		// narrow session anchor. Returning no key for an unanchored request is
		// safer than grouping every request by model alone.
		if !firstUserCaptured || !firstUserMeaningful {
			return ""
		}
		appendCompatPromptCacheSeedPart(&seedParts, "first_user", firstUserContent)
	}

	return compatPromptCacheKeyPrefix + hashSensitiveValueForLog(strings.Join(seedParts, "|"))
}

func appendCompatPromptCacheSeedPart(parts *[]string, label, value string) {
	// Length-prefix values so prompt text containing separators cannot create an
	// ambiguous seed before it is hashed.
	*parts = append(*parts, fmt.Sprintf("%s=%d:%s", label, len(value), value))
}

func hasMeaningfulCompatPromptCacheContent(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return strings.TrimSpace(string(raw)) != ""
	}
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		for _, rawPart := range typed {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			typeName, _ := part["type"].(string)
			text, _ := part["text"].(string)
			typeName = strings.ToLower(strings.TrimSpace(typeName))
			if (typeName == "text" || typeName == "input_text") && strings.TrimSpace(text) != "" {
				return true
			}
		}
		return false
	case map[string]any:
		return false
	default:
		return true
	}
}

func hasLosslessCompatPromptCacheSystemText(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		hasText := false
		for _, rawPart := range typed {
			part, ok := rawPart.(map[string]any)
			if !ok {
				return false
			}
			typeName, _ := part["type"].(string)
			typeName = strings.ToLower(strings.TrimSpace(typeName))
			if typeName != "text" && typeName != "input_text" {
				return false
			}
			text, _ := part["text"].(string)
			if strings.TrimSpace(text) != "" {
				hasText = true
			}
		}
		return hasText
	default:
		return false
	}
}

func deriveAnthropicCompatPromptCacheKey(req *apicompat.AnthropicRequest, mappedModel string) string {
	if req == nil {
		return ""
	}
	if anchorKey := deriveAnthropicCacheControlPromptCacheKey(req); anchorKey != "" {
		return anchorKey
	}

	normalizedModel := normalizeCodexModel(strings.TrimSpace(mappedModel))
	if normalizedModel == "" {
		normalizedModel = normalizeCodexModel(strings.TrimSpace(req.Model))
	}
	if normalizedModel == "" {
		normalizedModel = strings.TrimSpace(req.Model)
	}

	seedParts := []string{"model=" + normalizedModel}
	if req.OutputConfig != nil && strings.TrimSpace(req.OutputConfig.Effort) != "" {
		seedParts = append(seedParts, "effort="+strings.TrimSpace(req.OutputConfig.Effort))
	}
	if len(req.ToolChoice) > 0 {
		seedParts = append(seedParts, "tool_choice="+normalizeCompatSeedJSON(req.ToolChoice))
	}
	if len(req.Tools) > 0 {
		if raw, err := json.Marshal(req.Tools); err == nil {
			seedParts = append(seedParts, "tools="+normalizeCompatSeedJSON(raw))
		}
	}
	if len(req.System) > 0 {
		seedParts = append(seedParts, "system="+normalizeCompatSeedJSON(req.System))
	}

	firstUserCaptured := false
	for _, msg := range req.Messages {
		if strings.TrimSpace(msg.Role) != "user" || firstUserCaptured {
			continue
		}
		seedParts = append(seedParts, "first_user="+normalizeCompatSeedJSON(msg.Content))
		firstUserCaptured = true
	}

	return compatPromptCacheKeyPrefix + hashSensitiveValueForLog(strings.Join(seedParts, "|"))
}

func deriveAnthropicCacheControlPromptCacheKey(req *apicompat.AnthropicRequest) string {
	if req == nil {
		return ""
	}

	var parts []string
	var systemBlocks []apicompat.AnthropicContentBlock
	if len(req.System) > 0 && json.Unmarshal(req.System, &systemBlocks) == nil {
		for _, block := range systemBlocks {
			if block.Type == "text" &&
				block.CacheControl != nil &&
				strings.TrimSpace(block.CacheControl.Type) == "ephemeral" &&
				strings.TrimSpace(block.Text) != "" {
				parts = append(parts, "system:"+strings.TrimSpace(block.Text))
			}
		}
	}

	firstUserAnchor := ""
	for _, msg := range req.Messages {
		var blocks []apicompat.AnthropicContentBlock
		if len(msg.Content) == 0 || json.Unmarshal(msg.Content, &blocks) != nil {
			continue
		}
		role := strings.TrimSpace(msg.Role)
		for _, block := range blocks {
			if block.Type != "text" ||
				block.CacheControl == nil ||
				strings.TrimSpace(block.CacheControl.Type) != "ephemeral" ||
				strings.TrimSpace(block.Text) == "" {
				continue
			}
			switch role {
			case "user":
				if firstUserAnchor == "" {
					firstUserAnchor = strings.TrimSpace(block.Text)
				}
			case "assistant":
				parts = append(parts, "assistant:"+strings.TrimSpace(block.Text))
			}
		}
	}
	if firstUserAnchor != "" {
		parts = append(parts, "user_anchor:"+firstUserAnchor)
	}
	if len(parts) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte("anthropic-cache:" + strings.Join(parts, "\n")))
	return fmt.Sprintf("anthropic-cache-%x", sum[:16])
}

func normalizeCompatSeedJSON(v json.RawMessage) string {
	if len(v) == 0 {
		return ""
	}
	var tmp any
	if err := json.Unmarshal(v, &tmp); err != nil {
		return string(v)
	}
	out, err := json.Marshal(tmp)
	if err != nil {
		return string(v)
	}
	return string(out)
}
