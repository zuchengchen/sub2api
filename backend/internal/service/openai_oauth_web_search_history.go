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
// Standard Responses can replay that history with a cached-only web_search
// declaration. Lite rejects hosted declarations in every carrier. Callers
// resolve the outbound mode before applying this repair and keep the legacy
// /responses/compact schema separate from /responses compaction turns.

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

// Only direct input items name a replayed call. Most requests omit this type
// entirely. JSON unicode escapes can spell a field or its value, so candidate
// bodies use a read-only JSON view without copying the history.
func openAIResponsesHasWebSearchHistory(body []byte) bool {
	if !bytes.Contains(body, []byte(openAIWebSearchCallItemType)) && !bytes.Contains(body, []byte(`\u`)) {
		return false
	}
	input := parseRawJSONView(body).Get("input")
	if !input.IsArray() {
		return false
	}
	found := false
	input.ForEach(func(_, item gjson.Result) bool {
		found = item.IsObject() && item.Get("type").Str == openAIWebSearchCallItemType
		return !found
	})
	// GJSON can extract fields from incomplete documents. A malformed request
	// still needs its normal JSON validation error.
	return found && gjson.ValidBytes(body)
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
	tools, toolsAreArray := reqBody["tools"].([]any)
	if (reqBody["tools"] != nil && !toolsAreArray) || openAIToolsContainWebSearch(tools) {
		return false
	}
	hasWebSearchCall := false
	callerDeclaredTools := len(tools) > 0
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		switch item["type"] {
		case openAIWebSearchCallItemType:
			hasWebSearchCall = true
		case openAIAdditionalToolsItemType, "tool_search_output":
			declared, isArray := item["tools"].([]any)
			if (item["tools"] != nil && !isArray) || openAIToolsContainWebSearch(declared) {
				return false
			}
			callerDeclaredTools = callerDeclaredTools || len(declared) > 0
		}
	}
	if !hasWebSearchCall {
		return false
	}
	if !callerDeclaredTools {
		// With an empty tool set, a required/forced choice cannot be
		// satisfied by the caller's tools. Adding search would turn it into
		// an executable action which the caller never declared.
		if !shouldPinOpenAIWebSearchHistoryToolChoice(reqBody["tool_choice"]) {
			return false
		}
		reqBody["tool_choice"] = "none"
	}
	reqBody["tools"] = append(tools, cloneOpenAIWebSearchHistoryTool())
	return true
}

// ensureOpenAIOAuthWebSearchToolForHistoryBody is the raw-body variant used by
// the passthrough and WebSocket paths; it avoids decoding the whole body.
func ensureOpenAIOAuthWebSearchToolForHistoryBody(body []byte, responsesLite bool) ([]byte, bool, error) {
	if responsesLite || !openAIResponsesHasWebSearchHistory(body) {
		return body, false, nil
	}
	root := parseRawJSONView(body)
	tools := root.Get("tools")
	if (tools.Type != gjson.Null && !tools.IsArray()) || gjsonToolsContainWebSearch(tools) {
		return body, false, nil
	}
	callerDeclaredTools := tools.Get("0").Exists()
	skipRepair := false
	root.Get("input").ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").Str {
		case openAIAdditionalToolsItemType, "tool_search_output":
			itemTools := item.Get("tools")
			if (itemTools.Type != gjson.Null && !itemTools.IsArray()) || gjsonToolsContainWebSearch(itemTools) {
				skipRepair = true
				return false
			}
			callerDeclaredTools = callerDeclaredTools || itemTools.Get("0").Exists()
		}
		return true
	})
	if skipRepair {
		return body, false, nil
	}
	choice := root.Get("tool_choice")
	if !callerDeclaredTools && !shouldPinOpenAIWebSearchHistoryToolChoice(choice.String()) {
		return body, false, nil
	}

	var (
		next []byte
		err  error
	)
	if tools.IsArray() {
		next, err = sjson.SetBytes(body, "tools.-1", cloneOpenAIWebSearchHistoryTool())
	} else {
		next, err = sjson.SetBytes(body, "tools", []any{cloneOpenAIWebSearchHistoryTool()})
	}
	if err != nil {
		return body, false, fmt.Errorf("declare web_search tool for web_search_call history: %w", err)
	}
	if !callerDeclaredTools && choice.Str != "none" {
		next, err = sjson.SetBytes(next, "tool_choice", "none")
		if err != nil {
			return body, false, fmt.Errorf("pin tool_choice for web_search_call history: %w", err)
		}
	}
	return next, true, nil
}

func gjsonToolsContainWebSearch(tools gjson.Result) bool {
	if !tools.IsArray() {
		return false
	}
	found := false
	tools.ForEach(func(_, tool gjson.Result) bool {
		found = isOpenAIWebSearchToolType(tool.Get("type").Str)
		return !found
	})
	return found
}

func cloneOpenAIWebSearchHistoryTool() map[string]any {
	tool := make(map[string]any, len(openAIWebSearchHistoryTool))
	for key, value := range openAIWebSearchHistoryTool {
		tool[key] = value
	}
	return tool
}
