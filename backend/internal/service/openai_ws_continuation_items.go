package service

import (
	"crypto/sha256"
	"encoding/json"
)

// Canonicalize replay-only fields, without changing the outgoing request.
// In particular tool call_id, reasoning ciphertext, tools and all message
// content remain part of the comparison. Unresolved references are ineligible.
func openAIWSContinuationItemDigest(raw []byte) ([sha256.Size]byte, bool) {
	var item map[string]any
	if err := decodeOpenAIJSONUseNumber(raw, &item); err != nil || item == nil {
		return [sha256.Size]byte{}, false
	}
	kind, _ := item["type"].(string)
	switch kind {
	case "", "message", "reasoning", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output", "compaction",
		"additional_tools", "configuration_update":
		// Native Codex keeps tool declarations and configuration updates in
		// input history. Their complete contents must participate in the hash;
		// dropping them would incorrectly reuse a changed context.
	case "agent_message", "local_shell_call", "tool_search_call", "tool_search_output",
		"web_search_call", "image_generation_call", "context_compaction":
		// Native Codex durable items. Compare their complete payload, including
		// IDs/status; never infer hosted state from an ID-only reference.
		if !openAIWSNativeContinuationItemComplete(item) {
			return [sha256.Size]byte{}, false
		}
	case "compaction_summary":
		// Native remote compaction v2 replays its encrypted summary as the
		// new context window. Require the actual self-contained ciphertext and
		// compare the entire item, including its ID and opaque extensions.
		ciphertext, ok := item["encrypted_content"].(string)
		if !ok || ciphertext == "" {
			return [sha256.Size]byte{}, false
		}
		normalized, err := json.Marshal(item)
		return sha256.Sum256(normalized), err == nil
	default:
		// Unknown/hosted item types may use id semantically. Full replay is
		// safer than guessing their serialization and continuation contract.
		return [sha256.Size]byte{}, false
	}
	if kind == "" && item["role"] != nil {
		item["type"] = "message"
	}
	// Only nullable fields in the pinned Codex protocol are equivalent to
	// omission. Unknown extensions, non-empty values and ciphertext still count.
	for _, key := range openAIWSContinuationNullableFields(kind) {
		if value, exists := item[key]; exists && value == nil {
			delete(item, key)
		}
	}
	if kind == "reasoning" {
		if content, ok := item["content"].([]any); item["content"] == nil || (ok && len(content) == 0) {
			delete(item, "content")
		}
	}
	// The native protocol models this envelope as Option<...>. Codex drops
	// null on replay (and can add only the tracing fields removed below).
	// Empty and absent metadata must therefore compare equally.
	if item["internal_chat_message_metadata_passthrough"] == nil {
		delete(item, "internal_chat_message_metadata_passthrough")
	}
	if metadata, ok := item["internal_chat_message_metadata_passthrough"].(map[string]any); ok {
		delete(metadata, "turn_id")
		delete(metadata, "create_time")
		delete(metadata, "content_item_kinds")
		delete(metadata, "content_item_metadata")
		// Native Codex omits this upstream output token annotation on replay.
		delete(metadata, "content_token_count")
		for _, key := range []string{"cell_id", "executed_tool_calls", "tool_calls_complete"} {
			if metadata[key] == nil {
				delete(metadata, key)
			}
		}
		// Keep cell_id/executed_tool_calls/tool_calls_complete and unknown
		// metadata: late tool-result updates require a full replay.
		if len(metadata) == 0 {
			delete(item, "internal_chat_message_metadata_passthrough")
		}
	}
	if content, ok := item["content"].([]any); ok {
		for _, part := range content {
			if block, ok := part.(map[string]any); ok {
				for _, field := range []string{"annotations", "logprobs"} {
					if values, ok := block[field].([]any); ok && len(values) == 0 {
						delete(block, field)
					}
				}
			}
		}
	}
	normalized, err := json.Marshal(item)
	return sha256.Sum256(normalized), err == nil
}
