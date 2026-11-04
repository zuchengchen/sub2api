package service

import (
	"crypto/sha256"
	"strings"

	"github.com/tidwall/gjson"
)

type openAIWSContinuationFingerprint struct {
	Digest [sha256.Size]byte
	Kind   string
}

func openAIWSContinuationNullableFields(kind string) []string {
	switch kind {
	case "", "message":
		return []string{"id", "status", "encrypted_content", "phase"}
	case "function_call":
		return []string{"id", "status", "encrypted_content", "namespace", "encrypted_function_args"}
	case "custom_tool_call":
		return []string{"id", "status", "encrypted_content", "namespace"}
	case "function_call_output":
		return []string{"id", "name", "namespace"}
	case "custom_tool_call_output":
		return []string{"id", "name"}
	case "reasoning", "local_shell_call", "tool_search_call", "web_search_call", "image_generation_call", "context_compaction":
		return []string{"encrypted_content"}
	default:
		return nil
	}
}

// Fixed vocabulary and an index only. Never log raw types supplied by clients,
// hashes, item IDs, content, tool arguments or ciphertext.
type openAIWSContinuationDiagnostic struct {
	Index        int
	Kind         string
	PreviousKind string
}

func (d *openAIWSContinuationDiagnostic) set(index int, kind, previousKind string) {
	if d != nil && d.Index < 0 {
		d.Index, d.Kind, d.PreviousKind = index, kind, previousKind
	}
}

func openAIWSContinuationDiagnosticType(item gjson.Result) string {
	switch item.Get("type").String() {
	case "":
		return "message"
	case "message":
		return "message"
	case "reasoning":
		return "reasoning"
	case "function_call":
		return "function_call"
	case "function_call_output":
		return "function_call_output"
	case "custom_tool_call":
		return "custom_tool_call"
	case "custom_tool_call_output":
		return "custom_tool_call_output"
	case "agent_message":
		return "agent_message"
	case "local_shell_call":
		return "local_shell_call"
	case "tool_search_call":
		return "tool_search_call"
	case "tool_search_output":
		return "tool_search_output"
	case "web_search_call":
		return "web_search_call"
	case "image_generation_call":
		return "image_generation_call"
	case "compaction":
		return "compaction"
	case "compaction_summary":
		return "compaction_summary"
	case "context_compaction":
		return "context_compaction"
	case "additional_tools":
		return "additional_tools"
	case "configuration_update":
		return "configuration_update"
	case "item_reference":
		return "item_reference"
	default:
		return "unknown"
	}
}

// Be conservative about newly recognized protocol items. Their fields are
// hashed without rewriting the outgoing request; an unresolved reference or
// incomplete item cannot establish a reusable full-history checkpoint.
func openAIWSNativeContinuationItemComplete(item map[string]any) bool {
	text := func(key string) string { value, _ := item[key].(string); return value }
	hasText := func(key string) bool { return strings.TrimSpace(text(key)) != "" }
	object := func(key string) bool { _, ok := item[key].(map[string]any); return ok }
	array := func(key string) bool { _, ok := item[key].([]any); return ok }
	switch text("type") {
	case "agent_message":
		return hasText("author") && hasText("recipient") && array("content")
	case "local_shell_call":
		return hasText("call_id") && object("action")
	case "tool_search_call":
		return (text("execution") == "server" || (text("execution") == "client" && hasText("call_id"))) && object("arguments")
	case "tool_search_output":
		return (text("execution") == "server" || (text("execution") == "client" && hasText("call_id"))) && hasText("status") && array("tools")
	case "web_search_call":
		return hasText("encrypted_content") || object("action")
	case "image_generation_call":
		return hasText("result")
	case "context_compaction":
		return hasText("encrypted_content")
	default:
		return false
	}
}
