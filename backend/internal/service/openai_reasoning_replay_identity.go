package service

import "strings"

// Fixed Codex bfdbf01 protocol/response_item_id.rs::is_prefixed and
// core/client.rs::prepare_response_items_for_request preserve received IDs with
// nonempty prefix and suffix. A complete opaque reasoning item carries its own
// state under store=false; it is not a bare server-side ID lookup. Missing or
// incomplete opaque state deliberately retains the legacy compatibility policy.
// The encrypted string stays opaque: this predicate does not try to decrypt it.
func keepOpenAIReasoningReplayID(id, encrypted, status any) bool {
	itemID, ok := id.(string)
	if !ok {
		return false
	}
	prefix, suffix, found := strings.Cut(itemID, "_")
	if !found || prefix == "" || suffix == "" {
		return false
	}
	opaque, ok := encrypted.(string)
	if !ok || strings.TrimSpace(opaque) == "" {
		return false
	}
	if status == nil {
		return true
	}
	state, ok := status.(string)
	return ok && (state == "" || state == "completed")
}
