package service

import (
	"encoding/json"
	"strings"
)

const openAIExplicitPromptCacheMode = "explicit"
const openAIImplicitPromptCacheMode = "implicit"

type openAIPromptCachePreparation struct {
	Enabled          bool
	EnsureBreakpoint bool
	AutoConfigured   bool
}

// prepareOpenAIGPT56PromptCaching validates client cache options and prepares
// Chat Completions compatibility requests for a reusable static prefix. The
// automatic path keeps implicit conversation caching enabled and adds one
// explicit boundary after the stable developer instructions.
func prepareOpenAIGPT56PromptCaching(body []byte, model string, auto bool) ([]byte, openAIPromptCachePreparation, error) {
	result := openAIPromptCachePreparation{}
	if !isOpenAIGPT56OrLaterModel(model) || len(body) == 0 {
		return body, result, nil
	}

	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, result, err
	}

	input, _ := request["input"].([]any)
	hasStablePrefix := openAIInputHasReusableStableTextPrefix(input)
	hasBreakpoint := openAIInputHasPromptCacheBreakpoint(input)
	hasSystem, systemCanBeCanonicalized := analyzeOpenAIPromptCacheSystemMessages(input)
	canEnsureStableBoundary := hasStablePrefix && (!hasSystem || systemCanBeCanonicalized)
	rawOptions, optionsPresent := request["prompt_cache_options"]
	mode, optionsValid := openAIPromptCacheOptionsMode(rawOptions)
	modified := false
	if optionsPresent && !optionsValid {
		delete(request, "prompt_cache_options")
		modified = true
		if deleteOpenAIPromptCacheBreakpoints(request) {
			modified = true
		}
		return marshalOpenAIPromptCachePreparation(body, request, modified, result)
	}

	if !optionsPresent && auto && canEnsureStableBoundary {
		request["prompt_cache_options"] = map[string]any{
			"mode": openAIImplicitPromptCacheMode,
			"ttl":  "30m",
		}
		mode = openAIImplicitPromptCacheMode
		optionsPresent = true
		result.AutoConfigured = true
		modified = true
	}
	if !optionsPresent && !hasBreakpoint {
		return body, result, nil
	}

	result.Enabled = true
	result.EnsureBreakpoint = (auto || mode == openAIExplicitPromptCacheMode) && canEnsureStableBoundary
	if hasSystem && !systemCanBeCanonicalized && (result.EnsureBreakpoint || hasBreakpoint) {
		delete(request, "prompt_cache_options")
		deleteOpenAIPromptCacheBreakpoints(request)
		return marshalOpenAIPromptCachePreparation(body, request, true, openAIPromptCachePreparation{})
	}
	// The Codex OAuth transform promotes role=system into top-level
	// instructions and may remove the original input item. Canonicalize those
	// instructions into one leading developer message so the text appears once,
	// retains system-before-instructions ordering, and can carry a breakpoint.
	if hasSystem && (result.EnsureBreakpoint || hasBreakpoint) && canonicalizeOpenAIPromptCacheSystemMessages(request, input) {
		modified = true
	}

	return marshalOpenAIPromptCachePreparation(body, request, modified, result)
}

func marshalOpenAIPromptCachePreparation(
	original []byte,
	request map[string]any,
	modified bool,
	result openAIPromptCachePreparation,
) ([]byte, openAIPromptCachePreparation, error) {
	if !modified {
		return original, result, nil
	}
	updated, err := json.Marshal(request)
	if err != nil {
		return nil, result, err
	}
	return updated, result, nil
}

func openAIPromptCacheOptionsMode(raw any) (mode string, valid bool) {
	options, ok := raw.(map[string]any)
	if !ok {
		return "", false
	}
	for name := range options {
		if name != "mode" && name != "ttl" {
			return "", false
		}
	}
	mode = openAIImplicitPromptCacheMode
	if rawMode, exists := options["mode"]; exists {
		var stringOK bool
		mode, stringOK = rawMode.(string)
		if !stringOK || (mode != openAIImplicitPromptCacheMode && mode != openAIExplicitPromptCacheMode) {
			return "", false
		}
	}
	if rawTTL, ok := options["ttl"]; ok {
		ttl, stringOK := rawTTL.(string)
		if !stringOK || ttl != "30m" {
			return "", false
		}
	}
	return mode, true
}

func openAIInputHasReusableStableTextPrefix(input []any) bool {
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			return false
		}
		role := strings.ToLower(strings.TrimSpace(firstNonEmptyString(item["role"])))
		if role == "system" || role == "developer" {
			if openAIMessageHasCacheableText(item) {
				return true
			}
			continue
		}
		if role == "" && strings.EqualFold(strings.TrimSpace(firstNonEmptyString(item["type"])), "additional_tools") {
			continue
		}
		return false
	}
	return false
}

func openAIMessageHasCacheableText(message map[string]any) bool {
	switch content := message["content"].(type) {
	case string:
		return strings.TrimSpace(content) != ""
	case []any:
		for _, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			typeName := strings.ToLower(strings.TrimSpace(firstNonEmptyString(part["type"])))
			if (typeName == "text" || typeName == "input_text") && strings.TrimSpace(firstNonEmptyString(part["text"])) != "" {
				return true
			}
		}
	}
	return false
}

func analyzeOpenAIPromptCacheSystemMessages(input []any) (hasSystem bool, canCanonicalize bool) {
	canCanonicalize = true
	prefixOpen := true
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			prefixOpen = false
			continue
		}
		role := strings.ToLower(strings.TrimSpace(firstNonEmptyString(item["role"])))
		if prefixOpen {
			switch role {
			case "system":
				hasSystem = true
				if _, lossless := extractLosslessTextFromContent(item["content"]); !lossless {
					canCanonicalize = false
				}
				continue
			case "developer":
				continue
			case "":
				if strings.EqualFold(strings.TrimSpace(firstNonEmptyString(item["type"])), "additional_tools") {
					continue
				}
			}
			prefixOpen = false
		}
		if role == "system" {
			hasSystem = true
			canCanonicalize = false
		}
	}
	return hasSystem, canCanonicalize
}

func canonicalizeOpenAIPromptCacheSystemMessages(request map[string]any, input []any) bool {
	var instructionParts []string
	remaining := make([]any, 0, len(input)+1)
	prefixOpen := true
	preserveBreakpoint := false

	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			prefixOpen = false
			remaining = append(remaining, rawItem)
			continue
		}
		role := strings.ToLower(strings.TrimSpace(firstNonEmptyString(item["role"])))
		if prefixOpen && role == "system" {
			text, lossless := extractLosslessTextFromContent(item["content"])
			if !lossless {
				return false
			}
			if text != "" {
				instructionParts = append(instructionParts, text)
			}
			if openAIInputHasPromptCacheBreakpoint([]any{item}) {
				preserveBreakpoint = true
			}
			continue
		}
		if prefixOpen && role != "developer" && !(role == "" && strings.EqualFold(strings.TrimSpace(firstNonEmptyString(item["type"])), "additional_tools")) {
			prefixOpen = false
		}
		remaining = append(remaining, rawItem)
	}

	if existing, ok := request["instructions"].(string); ok && strings.TrimSpace(existing) != "" {
		instructionParts = append(instructionParts, existing)
	}
	if len(instructionParts) == 0 {
		return false
	}

	part := map[string]any{
		"type": "input_text",
		"text": strings.Join(instructionParts, "\n\n"),
	}
	if preserveBreakpoint {
		part["prompt_cache_breakpoint"] = map[string]any{"mode": openAIExplicitPromptCacheMode}
	}
	canonical := map[string]any{
		"role":    "developer",
		"content": []any{part},
	}
	request["input"] = append([]any{canonical}, remaining...)
	delete(request, "instructions")
	return true
}

func removeOpenAIPromptCacheConfiguration(body []byte) ([]byte, bool, error) {
	if len(body) == 0 || !strings.Contains(string(body), "prompt_cache_") {
		return body, false, nil
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, false, err
	}
	modified := false
	if _, ok := request["prompt_cache_options"]; ok {
		delete(request, "prompt_cache_options")
		modified = true
	}
	if deleteOpenAIPromptCacheBreakpoints(request) {
		modified = true
	}
	if !modified {
		return body, false, nil
	}
	updated, err := json.Marshal(request)
	if err != nil {
		return nil, false, err
	}
	return updated, true, nil
}

func deleteOpenAIPromptCacheBreakpoints(request map[string]any) bool {
	modified := false
	if _, ok := request["prompt_cache_breakpoint"]; ok {
		delete(request, "prompt_cache_breakpoint")
		modified = true
	}
	input, _ := request["input"].([]any)
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := item["prompt_cache_breakpoint"]; ok {
			delete(item, "prompt_cache_breakpoint")
			modified = true
		}
		parts, _ := item["content"].([]any)
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if _, ok := part["prompt_cache_breakpoint"]; ok {
				delete(part, "prompt_cache_breakpoint")
				modified = true
			}
		}
	}
	return modified
}

// ensureOpenAIExplicitPromptCacheBreakpoint places an opt-in GPT-5.6 cache
// boundary after the leading stable system/developer prefix. It deliberately
// does nothing for user-only requests and never replaces a client breakpoint.
func ensureOpenAIExplicitPromptCacheBreakpoint(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}

	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, false, err
	}
	input, ok := request["input"].([]any)
	if !ok || len(input) == 0 || openAIInputHasPromptCacheBreakpoint(input) {
		return body, false, nil
	}

	var target map[string]any
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			break
		}
		role := strings.ToLower(strings.TrimSpace(firstNonEmptyString(item["role"])))
		if role == "system" || role == "developer" {
			if openAIMessageHasCacheableText(item) {
				target = item
			}
			continue
		}
		// Responses Lite may put stable tool declarations before messages.
		if role == "" && strings.EqualFold(strings.TrimSpace(firstNonEmptyString(item["type"])), "additional_tools") {
			continue
		}
		break
	}
	if target == nil || !addOpenAIExplicitBreakpointToMessage(target) {
		return body, false, nil
	}

	updated, err := json.Marshal(request)
	if err != nil {
		return nil, false, err
	}
	return updated, true, nil
}

func openAIInputHasPromptCacheBreakpoint(input []any) bool {
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := item["prompt_cache_breakpoint"]; ok {
			return true
		}
		parts, ok := item["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if _, ok := part["prompt_cache_breakpoint"]; ok {
				return true
			}
		}
	}
	return false
}

func addOpenAIExplicitBreakpointToMessage(message map[string]any) bool {
	breakpoint := map[string]any{"mode": openAIExplicitPromptCacheMode}
	switch content := message["content"].(type) {
	case string:
		if strings.TrimSpace(content) == "" {
			return false
		}
		message["content"] = []any{map[string]any{
			"type":                    "input_text",
			"text":                    content,
			"prompt_cache_breakpoint": breakpoint,
		}}
		return true
	case []any:
		for i := len(content) - 1; i >= 0; i-- {
			part, ok := content[i].(map[string]any)
			if !ok {
				continue
			}
			typeName := strings.ToLower(strings.TrimSpace(firstNonEmptyString(part["type"])))
			if typeName != "text" && typeName != "input_text" {
				continue
			}
			if strings.TrimSpace(firstNonEmptyString(part["text"])) == "" {
				continue
			}
			part["prompt_cache_breakpoint"] = breakpoint
			return true
		}
	}
	return false
}
