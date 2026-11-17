package service

import (
	"strings"

	"github.com/tidwall/gjson"
)

// These bound local proof work. They are not upstream request limits: a request
// outside these bounds remains intact and must take a path that preserves state.
const (
	openAIContinuationProofMaxBytes = 64 << 20
	openAIContinuationProofMaxItems = 8192
	openAIContinuationProofMaxID    = 1024
)

// OpenAIContinuationReplayProof describes only the input's tool dependencies.
// It cannot prove that input includes the whole preceding conversation, that a
// response belongs to a credential, or that a retry has not already executed.
// Callers must independently prove those conditions before dropping an anchor,
// switching accounts or replaying. This helper never changes the request.
type OpenAIContinuationReplayProof struct {
	Verdict               string
	Reason                string
	HasToolOutput         bool
	HasItemReference      bool
	HasOpaqueState        bool
	HasPreviousResponseID bool
	HasConversation       bool
	// A local call/result contradiction is separate from opaque state, which
	// may be valid on its owning credential but cannot prove portability.
	ToolContextUnproven bool
	Calls               int
	Outputs             int
}

const (
	OpenAIContinuationProofInline       = "inline_tool_context"
	OpenAIContinuationProofNoDependency = "no_tool_dependency"
	OpenAIContinuationProofUnproven     = "unproven"
)

type openAIContinuationProofCall struct {
	kind      string
	name      string
	namespace string
	used      bool
}

// AnalyzeOpenAIContinuationReplayProof requires ordered, unique, inline calls
// and matching results. An item_reference proves neither the referenced item's
// contents nor its availability on another account. Unknown or ambiguous items
// decline the proof; they must never be silently removed to obtain a proof.
func AnalyzeOpenAIContinuationReplayProof(body []byte) OpenAIContinuationReplayProof {
	return analyzeOpenAIContinuationReplayProof(body, false)
}

func analyzeOpenAIContinuationReplayProof(body []byte, sameOwner bool) OpenAIContinuationReplayProof {
	proof := OpenAIContinuationReplayProof{Verdict: OpenAIContinuationProofUnproven}
	if len(body) > openAIContinuationProofMaxBytes {
		proof.Reason = "analysis_limit"
		return proof
	}
	if !openAIUnambiguousJSONObject(body) {
		proof.Reason = "invalid_ambiguous_or_bounded_json"
		return proof
	}
	root := parseRawJSONView(body)
	previous := root.Get("previous_response_id")
	if previous.Exists() && previous.Type != gjson.Null {
		if !openAIContinuationProofString(previous, true) {
			proof.Reason = "invalid_previous_response_id"
			return proof
		}
		proof.HasPreviousResponseID = true
	}
	if conversation := root.Get("conversation"); conversation.Exists() && conversation.Type != gjson.Null {
		// A native stored conversation supplies history which is absent from
		// input. It may resolve upstream, but never proves a local replay root.
		proof.HasOpaqueState = true
		proof.Reason = "native_conversation_context"
		proof.HasConversation = openAIContinuationProofString(conversation, true) ||
			(conversation.IsObject() && openAIContinuationProofString(conversation.Get("id"), true))
		if !proof.HasConversation {
			proof.Reason = "invalid_conversation"
		}
	}
	input := root.Get("input")
	if !input.Exists() || input.Type == gjson.String {
		if proof.Reason == "" {
			proof.Verdict, proof.Reason = OpenAIContinuationProofNoDependency, "no_tool_dependency"
		}
		return proof
	}
	if !input.IsArray() && !input.IsObject() {
		proof.Reason = "invalid_input_shape"
		return proof
	}
	// Do not expose IDs, tool names, data, or opaque values in the verdict.
	calls := make(map[string]openAIContinuationProofCall)
	itemIDs := make(map[string]struct{})
	items := 0
	unresolvedPrecedingContext := false
	decline := func(reason string) {
		switch reason {
		case "missing_preceding_call",
			"duplicate_item_id", "duplicate_call_id", "duplicate_output", "tool_family_mismatch", "tool_identity_mismatch":
			proof.ToolContextUnproven = true
		}
		if proof.Reason == "" {
			proof.Reason = reason
		}
	}
	analyze := func(item gjson.Result) bool {
		items++
		if items > openAIContinuationProofMaxItems {
			decline("analysis_limit")
			return false
		}
		if !item.IsObject() {
			decline("invalid_item_shape")
			return true
		}
		kindValue := item.Get("type")
		if kindValue.Exists() && kindValue.Type != gjson.String {
			decline("invalid_item_type")
			return true
		}
		kind := kindValue.String()
		if kind == "" && item.Get("role").Type == gjson.String {
			kind = "message"
		}
		// The public Responses ItemReference discriminator is optional.
		if kind == "item_reference" || (kind == "" && item.Get("id").Exists()) {
			proof.HasItemReference = true
			decline("unresolved_item_reference")
			return true
		}
		ownedReasoning := sameOwner && openAIContinuationProofOwnedOpaqueItem(item, kind)
		if openAIContinuationProofHasOpaqueState(item) {
			proof.HasOpaqueState = true
			if !ownedReasoning {
				decline("credential_or_hosted_state")
			}
		}
		if id := item.Get("id"); id.Exists() && id.Type != gjson.Null {
			if !openAIContinuationProofString(id, true) {
				decline("invalid_item_id")
			} else if _, duplicate := itemIDs[id.String()]; duplicate {
				decline("duplicate_item_id")
			} else {
				itemIDs[id.String()] = struct{}{}
			}
		}
		direction := openAIContinuationProofToolDirection(kind)
		if direction == 0 {
			if !ownedReasoning && !openAIContinuationProofPassiveItem(item, kind) {
				// Known passive items cannot stand in for a missing tool call,
				// even when their content is opaque or incomplete. Only an
				// unfamiliar wire family leaves the preceding call uncertain.
				switch kind {
				case "message", "reasoning", "compaction", "additional_tools", "configuration_update":
				default:
					unresolvedPrecedingContext = true
				}
				decline("unsupported_or_incomplete_item")
			}
			return true
		}
		if direction > 0 {
			proof.Calls++
		} else {
			proof.HasToolOutput = true
			proof.Outputs++
		}
		if kind == "program" || kind == "program_output" {
			unresolvedPrecedingContext = true
			proof.HasOpaqueState = true
			decline("credential_or_hosted_state")
			return true
		}
		// These are recognized dependencies, but their legacy/hosted identity
		// contracts do not establish portable inline context. Native MCP uses
		// function_call/function_call_output, not these legacy wire names.
		if kind == "tool_call" || kind == "mcp_tool_call" || kind == "mcp_tool_call_output" || kind == "local_shell_call_output" {
			unresolvedPrecedingContext = true
			decline("unproven_wire_family")
			return true
		}
		if kind == "tool_search_call" || kind == "tool_search_output" {
			if item.Get("execution").String() != "client" {
				proof.HasOpaqueState = true
				decline("credential_or_hosted_state")
				return true
			}
		}
		// Named function outputs without call_id are native Codex delegation
		// inputs. Preserve them as standalone data, not as invented call IDs.
		callID := item.Get("call_id")
		if kind == "function_call_output" && (!callID.Exists() || callID.Type == gjson.Null) &&
			openAIContinuationProofString(item.Get("name"), true) && openAIContinuationProofOutput(item, kind) {
			return true
		}
		if !openAIContinuationProofString(callID, true) {
			decline("missing_or_invalid_call_id")
			return true
		}
		id := callID.String()
		if direction > 0 {
			if _, duplicate := calls[id]; duplicate {
				decline("duplicate_call_id")
				return true
			}
			// Recognizable calls remain context when their schema is outside
			// this local replay proof; do not fabricate a missing-call error.
			calls[id] = openAIContinuationProofCall{kind: kind, name: item.Get("name").String(), namespace: item.Get("namespace").String()}
			if kind == "computer_call" && item.Get("action.type").String() != "screenshot" {
				// Other action variants remain native. Do not mistake a schema
				// we have not fully proved for an absent preceding call.
				decline("unsupported_typed_call")
				calls[id] = openAIContinuationProofCall{kind: kind}
				return true
			}
			if !openAIContinuationProofCallComplete(item, kind) {
				decline("incomplete_call")
				return true
			}
			return true
		}
		if !openAIContinuationProofOutput(item, kind) {
			decline("incomplete_output")
			return true
		}
		call, found := calls[id]
		if !found {
			if unresolvedPrecedingContext {
				decline("unresolved_preceding_context")
			} else {
				decline("missing_preceding_call")
			}
			return true
		}
		if call.used {
			decline("duplicate_output")
			return true
		}
		if !openAIContinuationProofMatchingKinds(call.kind, kind) {
			decline("tool_family_mismatch")
			return true
		}
		for key, expected := range map[string]string{"name": call.name, "namespace": call.namespace} {
			if actual := item.Get(key); actual.Exists() && actual.Type != gjson.Null &&
				(actual.Type != gjson.String || actual.String() != expected) {
				decline("tool_identity_mismatch")
				return true
			}
		}
		call.used = true
		calls[id] = call
		return true
	}
	if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool { return analyze(item) })
	} else {
		analyze(input)
	}
	for _, call := range calls {
		if !call.used {
			decline("missing_call_output")
		}
	}
	if proof.Reason != "" {
		return proof
	}
	proof.Verdict, proof.Reason = OpenAIContinuationProofNoDependency, "no_tool_dependency"
	if proof.Calls > 0 || proof.HasToolOutput {
		proof.Verdict, proof.Reason = OpenAIContinuationProofInline, "complete_inline_tool_context"
	}
	return proof
}

// Positive is a call, negative an output, zero a non-tool or unknown item.
func openAIContinuationProofToolDirection(kind string) int {
	switch kind {
	case "function_call", "custom_tool_call", "tool_search_call", "local_shell_call", "computer_call", "shell_call", "apply_patch_call", "tool_call", "mcp_tool_call", "program":
		return 1
	case "function_call_output", "custom_tool_call_output", "tool_search_output", "mcp_tool_call_output", "local_shell_call_output", "computer_call_output", "shell_call_output", "apply_patch_call_output", "program_output":
		return -1
	default:
		return 0
	}
}

func openAIContinuationProofMatchingKinds(call, output string) bool {
	switch call {
	case "local_shell_call":
		// codex-rs/protocol/src/models.rs and core/context_manager/normalize.rs
		// at e54a5713: native LocalShellCall returns FunctionCallOutput.
		return output == "function_call_output"
	case "tool_search_call":
		return output == "tool_search_output"
	default:
		return output == call+"_output"
	}
}

func openAIContinuationProofString(value gjson.Result, identity bool) bool {
	if value.Type != gjson.String {
		return false
	}
	if !identity {
		return true
	}
	text := value.String()
	if text == "" || len(text) > openAIContinuationProofMaxID || strings.TrimSpace(text) != text {
		return false
	}
	for _, ch := range text {
		if ch < 0x20 || ch == 0x7f {
			return false
		}
	}
	return true
}

func openAIContinuationProofStringArray(value gjson.Result) bool {
	if !value.IsArray() {
		return false
	}
	ok := true
	value.ForEach(func(_, item gjson.Result) bool {
		ok = item.Type == gjson.String
		return ok
	})
	return ok
}

func openAIContinuationProofCallComplete(item gjson.Result, kind string) bool {
	if status := item.Get("status"); status.Exists() && status.Type != gjson.Null &&
		(status.Type != gjson.String || status.String() != "completed") {
		return false
	}
	switch kind {
	case "function_call":
		return openAIContinuationProofString(item.Get("name"), true) && item.Get("arguments").Type == gjson.String
	case "custom_tool_call":
		return openAIContinuationProofString(item.Get("name"), true) && item.Get("input").Type == gjson.String
	case "tool_search_call":
		// Native Codex uses serde_json::Value, including JSON null.
		return item.Get("arguments").Exists()
	case "local_shell_call":
		return item.Get("action.type").String() == "exec" && openAIContinuationProofStringArray(item.Get("action.command"))
	case "computer_call":
		// Other computer actions have action-specific required fields. Keep
		// them native until a complete typed contract is available locally.
		return item.Get("action").IsObject() && item.Get("action.type").String() == "screenshot"
	case "shell_call":
		return openAIContinuationProofStringArray(item.Get("action.commands"))
	case "apply_patch_call":
		operation := item.Get("operation")
		if !openAIContinuationProofString(operation.Get("path"), true) {
			return false
		}
		switch operation.Get("type").String() {
		case "create_file", "update_file":
			return operation.Get("diff").Type == gjson.String
		case "delete_file":
			return true
		}
	}
	return false
}

func openAIContinuationProofOutput(item gjson.Result, kind string) bool {
	if status := item.Get("status"); status.Exists() && status.Type != gjson.Null &&
		(status.Type != gjson.String || (status.String() != "completed" && !(kind == "apply_patch_call_output" && status.String() == "failed"))) {
		return false
	}
	output := item.Get("output")
	switch kind {
	case "function_call_output", "custom_tool_call_output":
		return openAIContinuationProofContent(output)
	case "tool_search_output":
		return item.Get("tools").IsArray()
	case "computer_call_output":
		return output.IsObject() && output.Get("type").String() == "computer_screenshot" &&
			(openAIContinuationProofString(output.Get("image_url"), false) || openAIContinuationProofString(output.Get("file_id"), true))
	case "shell_call_output":
		if !output.IsArray() {
			return false
		}
		complete := true
		output.ForEach(func(_, entry gjson.Result) bool {
			if !entry.IsObject() || entry.Get("stdout").Type != gjson.String || entry.Get("stderr").Type != gjson.String {
				complete = false
				return false
			}
			switch entry.Get("outcome.type").String() {
			case "exit":
				code := entry.Get("outcome.exit_code")
				complete = code.Type == gjson.Number && !strings.ContainsAny(code.Raw, ".eE")
			case "timeout":
			default:
				complete = false
			}
			return complete
		})
		return complete
	case "apply_patch_call_output":
		status := item.Get("status").String()
		return (status == "completed" || status == "failed") && (!output.Exists() || output.Type == gjson.Null || output.Type == gjson.String)
	}
	return false
}

func openAIContinuationProofContent(content gjson.Result) bool {
	if openAIResponsesCompleteInlineMessageContent(content) {
		return true
	}
	if !content.IsArray() {
		return false
	}
	complete := true
	content.ForEach(func(_, part gjson.Result) bool {
		if !part.IsObject() {
			complete = false
			return false
		}
		switch part.Get("type").String() {
		case "input_text", "output_text":
			complete = part.Get("text").Type == gjson.String
		case "input_image":
			complete = part.Get("image_url").Type == gjson.String || part.Get("file_id").Type == gjson.String
		case "input_audio":
			// Native Codex FunctionCallOutputContentItem::InputAudio uses
			// audio_url; the public inline variant uses data and format.
			complete = part.Get("audio_url").Type == gjson.String ||
				(part.Get("input_audio.data").Type == gjson.String && part.Get("input_audio.format").Type == gjson.String)
		case "encrypted_content":
			complete = part.Get("encrypted_content").Type == gjson.String
		default:
			complete = false
		}
		return complete
	})
	return complete
}

func openAIContinuationProofPassiveItem(item gjson.Result, kind string) bool {
	switch kind {
	case "message":
		role := item.Get("role").String()
		content := item.Get("content")
		if status := item.Get("status"); status.Exists() && status.Type != gjson.Null &&
			(status.Type != gjson.String || status.String() != "completed") {
			return false
		}
		return (role == "user" || role == "assistant" || role == "system" || role == "developer") &&
			openAIResponsesCompleteInlineMessageContent(content)
	case "additional_tools":
		return item.Get("role").String() == "developer" && item.Get("tools").IsArray()
	case "configuration_update":
		return item.Get("reasoning").IsObject()
	}
	return false
}

func openAIContinuationProofOwnedOpaqueItem(item gjson.Result, kind string) bool {
	if (kind != "reasoning" && kind != "compaction") || item.Get("encrypted_content").Type != gjson.String || item.Get("encrypted_content").String() == "" {
		return false
	}
	valid := true
	item.ForEach(func(key, value gjson.Result) bool {
		if key.String() == "encrypted_content" {
			return true
		}
		switch key.String() {
		case "encrypted_function_args", "file_id", "container_id", "approval_request_id", "caller", "environment":
			valid = false
		default:
			valid = !openAIContinuationProofHasOpaqueState(value)
		}
		return valid
	})
	return valid
}

// Follow actual JSON objects, never JSON-looking strings in tool results or
// prompts. Opaque state stays on its original credential unless an independent
// ownership contract proves otherwise. This deliberately declines optimization
// for unknown state; it does not reject the underlying request.
func openAIContinuationProofHasOpaqueState(item gjson.Result) bool {
	if !item.IsObject() && !item.IsArray() {
		return false
	}
	found := false
	item.ForEach(func(key, value gjson.Result) bool {
		switch key.String() {
		case "encrypted_content", "encrypted_function_args", "file_id", "container_id", "approval_request_id":
			if value.Exists() && value.Type != gjson.Null && value.Raw != `""` {
				found = true
			}
		case "caller":
			if value.Exists() && value.Type != gjson.Null && value.Get("type").String() != "direct" {
				found = true
			}
		case "environment":
			if value.Exists() && value.Type != gjson.Null && value.Get("type").String() != "local" {
				found = true
			}
		}
		found = found || openAIContinuationProofHasOpaqueState(value)
		return !found
	})
	return found
}
