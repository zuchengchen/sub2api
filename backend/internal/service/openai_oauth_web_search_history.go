package service

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The ChatGPT internal Codex endpoint rejects a request whose input replays a
// hosted web_search_call item unless the request also declares the web_search
// tool; the stream then fails with "response protection is unavailable".
// Codex's local context compaction sends the full history with tools:[], so
// every compaction after a web search fails (#7927).
//
// Declare a cached-only web_search tool for non-Lite requests. Lite rejects
// hosted declarations in additional_tools as well as at the top level. Leave
// its history untouched; final Lite validation reports the incompatible
// web_search_call instead of deleting history or fabricating a client tool.
// Callers must supply the final mode after model mapping, including the
// existing mapped-GPT55 exception. When a non-Lite caller declared no tools,
// pin tool_choice to "none" so the declaration cannot enable new searches.

const (
	openAIWebSearchCallItemType      = "web_search_call"
	openAIAdditionalToolsItemType    = "additional_tools"
	openAICompactionTriggerItemType  = "compaction_trigger"
	openAIAdditionalToolsDefaultRole = "developer"
)

var openAIWebSearchHistoryTool = map[string]any{
	"type":                "web_search",
	"external_web_access": false,
}

func isOpenAIWebSearchToolType(toolType string) bool {
	return strings.HasPrefix(strings.TrimSpace(toolType), "web_search")
}

func openAIToolsContainWebSearch(rawTools any) bool {
	tools, ok := rawTools.([]any)
	if !ok {
		return false
	}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if ok && isOpenAIWebSearchToolType(firstNonEmptyString(tool["type"])) {
			return true
		}
	}
	return false
}

// shouldPinOpenAIWebSearchHistoryToolChoice reports whether tool_choice may be
// forced to "none": only when the caller offered no tools, so the choice
// cannot meaningfully be anything else.
func shouldPinOpenAIWebSearchHistoryToolChoice(choice any) bool {
	switch typed := choice.(type) {
	case nil:
		return true
	case string:
		normalized := strings.ToLower(strings.TrimSpace(typed))
		return normalized == "" || normalized == "auto" || normalized == "none"
	default:
		return false
	}
}

// ensureOpenAIOAuthWebSearchToolForHistory is the map variant used by the
// Codex OAuth transform.
func ensureOpenAIOAuthWebSearchToolForHistory(reqBody map[string]any, responsesLite bool) bool {
	if reqBody == nil || responsesLite {
		return false
	}
	input, ok := reqBody["input"].([]any)
	if !ok {
		return false
	}
	hasWebSearchCall := false
	callerDeclaredTools := false
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		switch strings.TrimSpace(firstNonEmptyString(item["type"])) {
		case openAIWebSearchCallItemType:
			hasWebSearchCall = true
		case openAIAdditionalToolsItemType:
			if openAIToolsContainWebSearch(item["tools"]) {
				return false
			}
			if tools, _ := item["tools"].([]any); len(tools) > 0 {
				callerDeclaredTools = true
			}
		}
	}
	if !hasWebSearchCall || openAIToolsContainWebSearch(reqBody["tools"]) {
		return false
	}
	tools, _ := reqBody["tools"].([]any)
	callerDeclaredTools = callerDeclaredTools || len(tools) > 0

	reqBody["tools"] = append(tools, cloneOpenAIWebSearchHistoryTool())
	if !callerDeclaredTools {
		if choice, exists := reqBody["tool_choice"]; !exists || shouldPinOpenAIWebSearchHistoryToolChoice(choice) {
			reqBody["tool_choice"] = "none"
		}
	}
	return true
}

// ensureOpenAIOAuthWebSearchToolForHistoryBody is the raw-body variant used by
// the passthrough and WebSocket paths; it avoids decoding the whole body.
func ensureOpenAIOAuthWebSearchToolForHistoryBody(body []byte, responsesLite bool) ([]byte, bool, error) {
	if responsesLite || len(body) == 0 || !bytes.Contains(body, []byte(openAIWebSearchCallItemType)) {
		return body, false, nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, false, nil
	}
	items := input.Array()
	hasWebSearchCall := false
	callerDeclaredTools := false
	for _, item := range items {
		switch strings.TrimSpace(item.Get("type").String()) {
		case openAIWebSearchCallItemType:
			hasWebSearchCall = true
		case openAIAdditionalToolsItemType:
			itemTools := item.Get("tools")
			if gjsonToolsContainWebSearch(itemTools) {
				return body, false, nil
			}
			if itemTools.IsArray() && len(itemTools.Array()) > 0 {
				callerDeclaredTools = true
			}
		}
	}
	if !hasWebSearchCall {
		return body, false, nil
	}
	tools := gjson.GetBytes(body, "tools")
	if gjsonToolsContainWebSearch(tools) {
		return body, false, nil
	}
	topLevelTools := tools.IsArray() && len(tools.Array()) > 0
	callerDeclaredTools = callerDeclaredTools || topLevelTools

	var (
		next []byte
		err  error
	)
	if topLevelTools {
		next, err = sjson.SetBytes(body, "tools.-1", cloneOpenAIWebSearchHistoryTool())
	} else {
		next, err = sjson.SetBytes(body, "tools", []any{cloneOpenAIWebSearchHistoryTool()})
	}
	if err != nil {
		return body, false, fmt.Errorf("declare web_search tool for web_search_call history: %w", err)
	}
	if !callerDeclaredTools {
		choice := gjson.GetBytes(next, "tool_choice")
		if !choice.Exists() || choice.Type == gjson.Null || (choice.Type == gjson.String && shouldPinOpenAIWebSearchHistoryToolChoice(choice.String())) {
			next, err = sjson.SetBytes(next, "tool_choice", "none")
			if err != nil {
				return body, false, fmt.Errorf("pin tool_choice for web_search_call history: %w", err)
			}
		}
	}
	return next, true, nil
}

func gjsonToolsContainWebSearch(tools gjson.Result) bool {
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		if isOpenAIWebSearchToolType(tool.Get("type").String()) {
			return true
		}
	}
	return false
}

func cloneOpenAIWebSearchHistoryTool() map[string]any {
	tool := make(map[string]any, len(openAIWebSearchHistoryTool))
	for key, value := range openAIWebSearchHistoryTool {
		tool[key] = value
	}
	return tool
}
