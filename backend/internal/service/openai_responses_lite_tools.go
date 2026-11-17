package service

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/tidwall/gjson"
)

type openAIResponsesLiteValidationError struct {
	param   string
	message string
}

func (e *openAIResponsesLiteValidationError) Error() string { return e.message }

func newOpenAIResponsesLiteValidationError(param, format string, args ...any) error {
	return &openAIResponsesLiteValidationError{param: param, message: fmt.Sprintf(format, args...)}
}

// validateOpenAIResponsesLiteFinalRequest checks the actual attempt after model
// mapping, synthesis, history completion and header/metadata compatibility. It
// is intentionally separate from the earlier namespace normalization: ingress
// can request Lite while the mapped GPT-5.5 attempt is non-Lite. Callers render
// this typed error as an invalid_request_error/400 before any upstream send.
// It never deletes declarations, changes tool execution, or rewrites history.
func validateOpenAIResponsesLiteFinalRequest(account *Account, finalModel, liteHeader string, body []byte) error {
	if !isOpenAIResponsesLiteFinalRequest(account, finalModel, liteHeader, body) {
		return nil
	}
	if !gjson.ValidBytes(body) {
		return newOpenAIResponsesLiteValidationError("body", "responses Lite requires a valid JSON request")
	}
	root := gjson.ParseBytes(body)
	if err := validateOpenAIResponsesLiteToolList(root.Get("tools"), "tools"); err != nil {
		return err
	}
	var validationErr error
	index := 0
	input := root.Get("input")
	if !input.IsArray() {
		return nil
	}
	input.ForEach(func(_, item gjson.Result) bool {
		param := fmt.Sprintf("input[%d]", index)
		index++
		switch item.Get("type").String() {
		case openAIAdditionalToolsItemType, "tool_search_output":
			// Both carriers declare callable tools for later turns. The
			// contents of ordinary function/custom outputs remain opaque.
			validationErr = validateOpenAIResponsesLiteToolList(item.Get("tools"), param+".tools")
		case openAIWebSearchCallItemType:
			// This history requires a hosted search declaration. Neither
			// removing the history nor adding an unsupported Lite declaration
			// preserves the client's request.
			validationErr = newOpenAIResponsesLiteValidationError(param+".type", "responses Lite cannot replay hosted web_search_call history; use a compatible non-Lite request without deleting its history")
		}
		return validationErr == nil
	})
	return validationErr
}

// The traversal is limited to declaration carriers and namespace children. A
// function schema, tool argument or result may legitimately contain any of the
// same field names and is never inspected as a declaration. Use an explicit
// stack so nested namespaces cannot exhaust the Go call stack; diagnostic
// paths are bounded independently from the accepted request size.
func validateOpenAIResponsesLiteToolList(tools gjson.Result, param string) error {
	type declarationList struct {
		tools gjson.Result
		param string
	}
	pending := []declarationList{{tools: tools, param: param}}
	for len(pending) > 0 {
		list := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if !list.tools.Exists() || list.tools.Type == gjson.Null {
			continue
		}
		if !list.tools.IsArray() {
			return newOpenAIResponsesLiteValidationError(list.param, "responses Lite tool declarations must be an array")
		}
		var validationErr error
		index := 0
		list.tools.ForEach(func(_, tool gjson.Result) bool {
			toolParam := fmt.Sprintf("%s[%d]", list.param, index)
			index++
			if len(toolParam) > 240 {
				toolParam = param // retain the original carrier, never user names
			}
			switch tool.Get("type").String() {
			case "namespace":
				if len(tool.Get("tools").Array()) > 0 && len(tool.Get("children").Array()) > 0 {
					validationErr = newOpenAIResponsesLiteValidationError(toolParam, "A namespace must not supply both nonempty tools and children declarations")
					return false
				}
				// Both declaration spellings are supported by the existing
				// namespace bridge. Validate every supplied list without choosing
				// or deleting one when the other is empty or absent.
				pending = append(pending,
					declarationList{tools: tool.Get("tools"), param: toolParam + ".tools"},
					declarationList{tools: tool.Get("children"), param: toolParam + ".children"})
			case "tool_search":
				execution := tool.Get("execution")
				if execution.Type != gjson.String || execution.String() != "client" {
					validationErr = newOpenAIResponsesLiteValidationError(toolParam+".execution", "responses Lite requires tool_search execution to be client; hosted or unspecified execution cannot be converted automatically")
				}
			case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "image_generation", "file_search", "code_interpreter", "mcp", "computer", "computer_use_preview", "local_shell", "shell", "apply_patch":
				validationErr = newOpenAIResponsesLiteValidationError(toolParam+".type", "responses Lite does not support tool type %q; supported declarations are function tools, custom tools and client-executed tool search", tool.Get("type").String())
			}
			// Unknown private/future shapes are left for the upstream contract;
			// absence from the public client emitter is not proof of rejection.
			return validationErr == nil
		})
		if validationErr != nil {
			return validationErr
		}
	}
	return nil
}

// normalizeOpenAIResponsesLiteTools applies the Responses Lite request
// contract: reasoning must cover all turns, and private namespace declarations
// use the input.additional_tools carrier. Other top-level tools must belong to
// the small set accepted by the Lite endpoint; rejecting unsupported hosted
// tools is intentional because silently dropping them would change behavior.
func normalizeOpenAIResponsesLiteTools(reqBody map[string]any) (bool, error) {
	if reqBody == nil {
		return false, nil
	}
	if parallel, exists := reqBody["parallel_tool_calls"]; exists {
		if _, ok := parallel.(bool); !ok {
			return false, newOpenAIResponsesLiteValidationError("parallel_tool_calls", "responses Lite requires parallel_tool_calls to be a boolean")
		}
	}
	if rawReasoning, exists := reqBody["reasoning"]; exists && rawReasoning != nil {
		if _, ok := rawReasoning.(map[string]any); !ok {
			return false, newOpenAIResponsesLiteValidationError("reasoning", "responses Lite requires reasoning to be an object")
		}
	}
	rawTools, exists := reqBody["tools"]
	if !exists || rawTools == nil {
		changed, err := ensureOpenAIResponsesLiteReasoningContext(reqBody)
		if err != nil {
			return false, err
		}
		return ensureOpenAIResponsesLiteParallelToolCalls(reqBody, changed)
	}
	tools, ok := rawTools.([]any)
	if !ok {
		return false, newOpenAIResponsesLiteValidationError("tools", "responses Lite requires tools to be an array")
	}

	topLevelTools := make([]any, 0, len(tools))
	namespaceTools := make([]any, 0, len(tools))
	for index, rawTool := range tools {
		if customTool, ok := rawTool.(string); ok {
			if strings.TrimSpace(customTool) == "" {
				return false, fmt.Errorf("responses Lite custom tool at index %d must not be empty", index)
			}
			topLevelTools = append(topLevelTools, rawTool)
			continue
		}
		tool, ok := rawTool.(map[string]any)
		if !ok {
			return false, fmt.Errorf("responses Lite tool at index %d must be an object", index)
		}
		toolType := strings.TrimSpace(firstNonEmptyString(tool["type"]))
		switch toolType {
		case "function", "custom", "tool_search":
			topLevelTools = append(topLevelTools, rawTool)
		case "namespace":
			namespaceTools = append(namespaceTools, rawTool)
		case "":
			return false, fmt.Errorf("responses Lite tool at index %d is missing type", index)
		case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "image_generation", "file_search", "code_interpreter", "mcp", "computer", "computer_use_preview", "local_shell", "shell", "apply_patch":
			return false, newOpenAIResponsesLiteValidationError(fmt.Sprintf("tools[%d].type", index), "responses Lite does not support top-level tool type %q at index %d", toolType, index)
		default:
			// The private protocol may add tool declarations ahead of this
			// gateway. Preserve unknown extensions for upstream validation.
			topLevelTools = append(topLevelTools, rawTool)
		}
	}
	if len(namespaceTools) == 0 {
		changed, err := ensureOpenAIResponsesLiteReasoningContext(reqBody)
		if err != nil {
			return false, err
		}
		return ensureOpenAIResponsesLiteParallelToolCalls(reqBody, changed)
	}

	input, err := appendOpenAIResponsesLiteAdditionalTools(reqBody["input"], namespaceTools)
	if err != nil {
		return false, err
	}
	if _, err := ensureOpenAIResponsesLiteReasoningContext(reqBody); err != nil {
		return false, err
	}
	reqBody["input"] = input
	if len(topLevelTools) == 0 {
		delete(reqBody, "tools")
	} else {
		reqBody["tools"] = topLevelTools
	}
	return ensureOpenAIResponsesLiteParallelToolCalls(reqBody, true)
}

func ensureOpenAIResponsesLiteParallelToolCalls(reqBody map[string]any, changed bool) (bool, error) {
	parallel, exists := reqBody["parallel_tool_calls"]
	if exists {
		if _, ok := parallel.(bool); !ok {
			return false, newOpenAIResponsesLiteValidationError("parallel_tool_calls", "responses Lite requires parallel_tool_calls to be a boolean")
		}
	}
	if parallel == false {
		return changed, nil
	}
	reqBody["parallel_tool_calls"] = false
	return true, nil
}

func ensureOpenAIResponsesLiteReasoningContext(reqBody map[string]any) (bool, error) {
	rawReasoning, exists := reqBody["reasoning"]
	if !exists || rawReasoning == nil {
		reqBody["reasoning"] = map[string]any{"context": "all_turns"}
		return true, nil
	}
	reasoning, ok := rawReasoning.(map[string]any)
	if !ok {
		return false, newOpenAIResponsesLiteValidationError("reasoning", "responses Lite requires reasoning to be an object")
	}
	if context, ok := reasoning["context"].(string); ok && context == "all_turns" {
		return false, nil
	}
	reasoning["context"] = "all_turns"
	return true, nil
}

func appendOpenAIResponsesLiteAdditionalTools(input any, namespaceTools []any) ([]any, error) {
	var items []any
	switch typed := input.(type) {
	case nil:
		items = make([]any, 0, 1)
	case string:
		items = []any{map[string]any{
			"type":    "message",
			"role":    "user",
			"content": typed,
		}}
	case []any:
		items = typed
	default:
		return nil, fmt.Errorf("responses Lite namespace tools require input to be a string or array")
	}

	var target map[string]any
	var targetTools []any
	var allAdditionalTools []any
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyString(item["type"])) != "additional_tools" {
			continue
		}
		rawAdditionalTools, exists := item["tools"]
		additionalTools := []any(nil)
		toolsOK := true
		if exists && rawAdditionalTools != nil {
			additionalTools, toolsOK = rawAdditionalTools.([]any)
		}
		if !toolsOK {
			return nil, fmt.Errorf("responses Lite input.additional_tools tools must be an array")
		}
		if target == nil {
			target = item
			targetTools = additionalTools
		}
		allAdditionalTools = append(allAdditionalTools, additionalTools...)
	}

	merged, err := mergeOpenAIResponsesLiteAdditionalTools(allAdditionalTools, namespaceTools)
	if err != nil {
		return nil, err
	}
	newTools := merged[len(allAdditionalTools):]
	if target != nil {
		if len(newTools) > 0 {
			target["tools"] = append(append([]any(nil), targetTools...), newTools...)
		}
		return items, nil
	}

	items = append(items, map[string]any{
		"type":  "additional_tools",
		"role":  "developer",
		"tools": newTools,
	})
	return items, nil
}

func mergeOpenAIResponsesLiteAdditionalTools(existing []any, moved []any) ([]any, error) {
	merged := append([]any(nil), existing...)
	seen := make(map[string]any, len(existing)+len(moved))
	for _, rawTool := range existing {
		if identity := openAIResponsesLiteToolIdentity(rawTool); identity != "" {
			if previous, exists := seen[identity]; exists && !reflect.DeepEqual(previous, rawTool) {
				return nil, fmt.Errorf("responses Lite additional_tools contains conflicting definitions for %s", openAIResponsesLiteToolIdentityForError(rawTool))
			}
			seen[identity] = rawTool
		}
	}
	for _, rawTool := range moved {
		identity := openAIResponsesLiteToolIdentity(rawTool)
		if identity != "" {
			if previous, exists := seen[identity]; exists {
				if reflect.DeepEqual(previous, rawTool) {
					continue
				}
				return nil, fmt.Errorf("responses Lite additional_tools conflicts with migrated %s", openAIResponsesLiteToolIdentityForError(rawTool))
			}
			seen[identity] = rawTool
		}
		merged = append(merged, rawTool)
	}
	return merged, nil
}

func openAIResponsesLiteToolIdentity(rawTool any) string {
	tool, ok := rawTool.(map[string]any)
	if !ok {
		return ""
	}
	toolType := strings.TrimSpace(firstNonEmptyString(tool["type"]))
	name := strings.TrimSpace(firstNonEmptyString(tool["name"]))
	if toolType == "" || name == "" {
		return ""
	}
	return toolType + "\x00" + name
}

func openAIResponsesLiteToolIdentityForError(rawTool any) string {
	tool, _ := rawTool.(map[string]any)
	return fmt.Sprintf("tool type %q name %q", strings.TrimSpace(firstNonEmptyString(tool["type"])), strings.TrimSpace(firstNonEmptyString(tool["name"])))
}

func normalizeOpenAIResponsesLiteToolsPayload(body []byte) ([]byte, bool, error) {
	var requestBody map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &requestBody); err != nil {
		return body, false, fmt.Errorf("decode responses Lite request body: %w", err)
	}
	changed, err := normalizeOpenAIResponsesLiteTools(requestBody)
	if err != nil || !changed {
		return body, false, err
	}
	rebuilt, err := marshalOpenAIUpstreamJSON(requestBody)
	if err != nil {
		return body, false, fmt.Errorf("encode responses Lite request body: %w", err)
	}
	return rebuilt, true, nil
}

func normalizeOpenAIResponsesLiteParallelToolCallsPayload(body []byte) ([]byte, bool, error) {
	var requestBody map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &requestBody); err != nil {
		return body, false, fmt.Errorf("decode responses Lite request body: %w", err)
	}
	changed, err := ensureOpenAIResponsesLiteParallelToolCalls(requestBody, false)
	if err != nil || !changed {
		return body, false, err
	}
	rebuilt, err := marshalOpenAIUpstreamJSON(requestBody)
	if err != nil {
		return body, false, fmt.Errorf("encode responses Lite request body: %w", err)
	}
	return rebuilt, true, nil
}

func normalizeOpenAIResponsesLitePayloadForAccount(body []byte, account *Account) ([]byte, bool, error) {
	if account == nil || !account.IsOpenAI() {
		return body, false, nil
	}
	if account.IsOpenAIOAuthLike() {
		// The caller has already resolved that this attempt uses Lite. Validate
		// every declaration carrier before any normalization can rewrite it.
		if err := validateOpenAIResponsesLiteFinalRequest(account, "", "true", body); err != nil {
			return body, false, err
		}
		return normalizeOpenAIResponsesLiteToolsPayload(body)
	}
	return normalizeOpenAIResponsesLiteParallelToolCallsPayload(body)
}
