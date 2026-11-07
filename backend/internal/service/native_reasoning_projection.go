package service

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/tidwall/sjson"
)

// Bind both sides of a lossy protocol conversion, including unknown semantic
// fields. In particular, normalized tool aliases alone cannot distinguish two
// different client declarations. The leading native input also binds synthesized
// system/developer instructions that do not exist in the original Chat body.
func nativeReasoningProjectionParameters(chatBody, normalizedResponsesBody []byte, reverse map[string]string) (json.RawMessage, error) {
	limit := DefaultNativeReasoningReplayLimits().MaxHistoryBytes
	if len(chatBody) > limit || len(normalizedResponsesBody) > limit {
		return nil, ErrNativeReasoningReplayCapacity
	}
	chat, err := nativeReasoningProjectionObject(chatBody)
	if err != nil {
		return nil, err
	}
	upstream, err := nativeReasoningProjectionObject(normalizedResponsesBody)
	if err != nil {
		return nil, err
	}
	input, err := nativeReasoningProjectionItems(upstream["input"])
	if err != nil {
		return nil, err
	}
	boundary := len(input)
	for i, raw := range input {
		var item nativeReasoningProjectionItem
		if json.Unmarshal(raw, &item) != nil {
			return nil, ErrNativeReasoningReplayInvalid
		}
		if item.Role == "assistant" || isCodexToolCallInputType(item.Type) {
			boundary = i
			break
		}
	}
	for _, key := range []string{"messages", "stream", "stream_options", "client_metadata"} {
		delete(chat, key)
	}
	for _, key := range []string{"input", "stream", "stream_options", "client_metadata"} {
		delete(upstream, key)
	}
	prefix := nativeReasoningProjectionArray(input[:boundary])
	result, err := json.Marshal(struct {
		Client          map[string]json.RawMessage `json:"client"`
		Upstream        map[string]json.RawMessage `json:"upstream"`
		ToolNameReverse map[string]string          `json:"tool_name_reverse"`
		Prefix          json.RawMessage            `json:"input_prefix"`
	}{Client: chat, Upstream: upstream, ToolNameReverse: reverse, Prefix: prefix})
	if err != nil {
		return nil, ErrNativeReasoningReplayInvalid
	}
	if len(result) > limit {
		return nil, ErrNativeReasoningReplayCapacity
	}
	return nativeReplayCanonicalObject(result)
}

type nativeReasoningProjectionItem struct {
	Type      string `json:"type"`
	Role      string `json:"role"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Input     string `json:"input"`
}

type nativeReasoningProjectionFix struct {
	index      int
	callID     string
	outputType string
}

type nativeReasoningProjectionPlan struct {
	start, end  int
	replacement NativeReasoningReplayReplacement
	fixes       []nativeReasoningProjectionFix
}

// applyNativeReasoningProjection operates only on the final normalized input.
// Tool candidates are located by unique call IDs; message-only candidates by
// their whole structural fragment. Both need unique matching spans and both
// available adjacent anchors. Original native
// output never passes through an alias/filter transformation. Any failed proof
// retains the whole compatibility group and its original tool results.
func applyNativeReasoningProjection(body []byte, replacements []NativeReasoningReplayReplacement, reverse map[string]string, limits NativeReasoningReplayLimits) ([]byte, []NativeReasoningReplayReference, error) {
	if len(replacements) == 0 {
		return body, nil, nil
	}
	limits = limits.Normalize()
	if len(body) > limits.MaxHistoryBytes || len(replacements) > limits.MaxLookups {
		return body, nil, ErrNativeReasoningReplayCapacity
	}
	request, err := nativeReasoningProjectionObject(body)
	if err != nil {
		return body, nil, err
	}
	input, err := nativeReasoningProjectionItems(request["input"])
	if err != nil {
		return body, nil, err
	}
	forward, err := nativeReasoningProjectionForwardNames(reverse)
	if err != nil {
		return body, nil, err
	}
	canonical := make([][]byte, len(input))
	typed := make([]nativeReasoningProjectionItem, len(input))
	calls, results := map[string][]int{}, map[string][]int{}
	var messageStarts map[string][]int
	for i, raw := range input {
		canonical[i], err = nativeReplayCanonicalObject(raw)
		if err != nil {
			return body, nil, ErrNativeReasoningReplayInvalid
		}
		if json.Unmarshal(raw, &typed[i]) != nil {
			return body, nil, ErrNativeReasoningReplayInvalid
		}
		if isCodexToolCallInputType(typed[i].Type) {
			calls[typed[i].CallID] = append(calls[typed[i].CallID], i)
		}
		if nativeReasoningProjectionIsResult(typed[i].Type) {
			results[typed[i].CallID] = append(results[typed[i].CallID], i)
		}
	}
	var plans []nativeReasoningProjectionPlan
	seenMessages := map[int]bool{}
	for _, replacement := range replacements {
		if seenMessages[replacement.MessageIndex] {
			return body, nil, ErrNativeReasoningReplayAmbiguous
		}
		seenMessages[replacement.MessageIndex] = true
		if len(replacement.Output) > limits.MaxOutputItems {
			return body, nil, ErrNativeReasoningReplayCapacity
		}
		if len(replacement.Compatibility) == 0 || len(replacement.Output) == 0 {
			continue
		}
		if len(nativeReasoningProjectionArray(replacement.Output)) > limits.MaxEntryBytes {
			return body, nil, ErrNativeReasoningReplayCapacity
		}
		var found *nativeReasoningProjectionPlan
		ambiguous := false
		// Both are explicit production options: ordinary Codex normalization and
		// PreserveToolCallIDs. They must identify the same unique structural span
		// or exactly one span; no fuzzy fallback or nearby-message search exists.
		for _, preserve := range []bool{false, true} {
			fragment, before, after, nerr := nativeReasoningProjectionExpected(replacement, forward, preserve)
			if nerr != nil {
				continue
			}
			callOffset, firstID := -1, ""
			for i, raw := range fragment {
				var item nativeReasoningProjectionItem
				_ = json.Unmarshal(raw, &item)
				if item.Type == "function_call" {
					callOffset, firstID = i, item.CallID
					break
				}
			}
			var starts []int
			messageOnly := callOffset < 0
			if messageOnly {
				if !nativeReasoningProjectionMessageOnly(replacement, fragment) {
					continue
				}
				if messageStarts == nil {
					messageStarts = make(map[string][]int)
					for i, item := range typed {
						if item.Type == "message" && item.Role == "assistant" {
							key := nativeReplayDigest(canonical[i])
							messageStarts[key] = append(messageStarts[key], i)
						}
					}
				}
				starts = messageStarts[nativeReplayDigest(fragment[0])]
			} else {
				if firstID == "" || len(calls[firstID]) != 1 {
					continue
				}
				starts = []int{calls[firstID][0] - callOffset}
			}
			for _, start := range starts {
				end := start + len(fragment)
				if start < 0 || end > len(input) {
					continue
				}
				matched := true
				for i, raw := range fragment {
					if !bytes.Equal(raw, canonical[start+i]) {
						matched = false
						break
					}
				}
				if !matched || (len(before) > 0 && (start == 0 || !bytes.Equal(before, canonical[start-1]))) || (len(after) > 0 && (end == len(input) || !bytes.Equal(after, canonical[end]))) {
					continue
				}
				var fixes []nativeReasoningProjectionFix
				if messageOnly {
					if end < len(input) && nativeReasoningProjectionIsResult(typed[end].Type) {
						continue
					}
				} else {
					var ok bool
					fixes, ok = nativeReasoningProjectionPairs(replacement, fragment, typed, calls, results, forward, reverse, start, end)
					if !ok {
						continue
					}
				}
				plan := nativeReasoningProjectionPlan{start: start, end: end, replacement: replacement, fixes: fixes}
				if found != nil && (found.start != start || found.end != end) {
					ambiguous = true
					break
				}
				found = &plan
			}
			if ambiguous {
				break
			}
		}
		if found != nil && !ambiguous {
			plans = append(plans, *found)
		}
	}
	if len(plans) == 0 {
		return body, nil, nil
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].start < plans[j].start })
	nativeIDs := map[string]bool{}
	for i, plan := range plans {
		if i > 0 && (plans[i-1].end > plan.start || plans[i-1].replacement.MessageIndex >= plan.replacement.MessageIndex) {
			return body, nil, ErrNativeReasoningReplayAmbiguous
		}
		for _, fix := range plan.fixes {
			if nativeIDs[fix.callID] {
				return body, nil, ErrNativeReasoningReplayAmbiguous
			}
			nativeIDs[fix.callID] = true
		}
	}
	working := append([]json.RawMessage(nil), input...)
	out := make([]json.RawMessage, 0, len(input))
	cursor := 0
	var applied []NativeReasoningReplayReference
	for _, plan := range plans {
		out = append(out, working[cursor:plan.start]...)
		prefix := nativeReasoningProjectionArray(out)
		if len(prefix) > limits.MaxHistoryBytes {
			return body, nil, ErrNativeReasoningReplayCapacity
		}
		digest, err := NativeReasoningReplayInputDigest(prefix)
		if err != nil {
			return body, nil, err
		}
		if digest != plan.replacement.ExpectedSourceInputDigest {
			out = append(out, working[plan.start:plan.end]...)
			cursor = plan.end
			continue
		}
		// The following result edits are visible to the prefix check of the next
		// group, not deferred until all groups have already been admitted.
		for _, fix := range plan.fixes {
			updated, err := sjson.SetBytes(working[fix.index], "call_id", fix.callID)
			if err != nil {
				return body, nil, ErrNativeReasoningReplayInvalid
			}
			if typed[fix.index].Type != fix.outputType {
				updated, err = sjson.SetBytes(updated, "type", fix.outputType)
				if err != nil {
					return body, nil, ErrNativeReasoningReplayInvalid
				}
			}
			working[fix.index] = updated
		}
		out = append(out, plan.replacement.Output...)
		cursor = plan.end
		applied = append(applied, plan.replacement.Reference)
	}
	if len(applied) == 0 {
		return body, nil, nil
	}
	out = append(out, working[cursor:]...)
	encoded := nativeReasoningProjectionArray(out)
	if len(encoded) > limits.MaxHistoryBytes {
		return body, nil, ErrNativeReasoningReplayCapacity
	}
	updated, err := sjson.SetRawBytes(body, "input", encoded)
	if err != nil {
		return body, nil, ErrNativeReasoningReplayInvalid
	}
	return updated, applied, nil
}

// A message-looking compatibility fragment must never bypass tool pairing for
// an output that actually contains calls. Only already-supported output kinds
// are accepted, with at least one real assistant message.
func nativeReasoningProjectionMessageOnly(r NativeReasoningReplayReplacement, fragment []json.RawMessage) bool {
	for _, group := range [][]json.RawMessage{r.Compatibility, fragment} {
		for _, raw := range group {
			var item nativeReasoningProjectionItem
			if json.Unmarshal(raw, &item) != nil || item.Type != "message" || item.Role != "assistant" {
				return false
			}
		}
	}
	message := false
	for _, raw := range r.Output {
		var item nativeReasoningProjectionItem
		if _, err := nativeReplayCanonicalObject(raw); err != nil || json.Unmarshal(raw, &item) != nil {
			return false
		}
		switch item.Type {
		case "message":
			if item.Role != "assistant" {
				return false
			}
			message = true
		case "reasoning", "web_search_call":
		default:
			return false
		}
	}
	return message
}

func nativeReasoningProjectionExpected(r NativeReasoningReplayReplacement, forward map[string]string, preserve bool) ([]json.RawMessage, json.RawMessage, json.RawMessage, error) {
	transform := func(raw json.RawMessage) (json.RawMessage, error) {
		if len(raw) == 0 {
			return nil, nil
		}
		canonical, err := nativeReplayCanonicalObject(raw)
		if err != nil {
			return nil, err
		}
		var item map[string]any
		if decodeOpenAIJSONUseNumber(canonical, &item) != nil {
			return nil, ErrNativeReasoningReplayInvalid
		}
		if item["type"] == "function_call" || item["type"] == "custom_tool_call" {
			name, _ := item["name"].(string)
			item["name"] = nativeReasoningProjectionName(name, forward)
		}
		items, _ := normalizeCodexMessageContentText([]any{item})
		items = filterCodexInputWithOptions(items, codexInputFilterOptions{PreserveReferences: true, PreserveCallIDs: preserve})
		if len(items) != 1 {
			return nil, ErrNativeReasoningReplayInvalid
		}
		wrapper := map[string]any{"input": items}
		deleteOpenAIPromptCacheBreakpoints(wrapper)
		encoded, err := json.Marshal(items[0])
		if err != nil {
			return nil, ErrNativeReasoningReplayInvalid
		}
		return nativeReplayCanonicalObject(encoded)
	}
	fragment := make([]json.RawMessage, len(r.Compatibility))
	for i, raw := range r.Compatibility {
		value, err := transform(raw)
		if err != nil {
			return nil, nil, nil, err
		}
		fragment[i] = value
	}
	before, err := transform(r.Before)
	if err != nil {
		return nil, nil, nil, err
	}
	after, err := transform(r.After)
	if err != nil {
		return nil, nil, nil, err
	}
	return fragment, before, after, nil
}

func nativeReasoningProjectionPairs(r NativeReasoningReplayReplacement, fragment []json.RawMessage, input []nativeReasoningProjectionItem, calls, results map[string][]int, forward, reverse map[string]string, start, end int) ([]nativeReasoningProjectionFix, bool) {
	var original, normalized, native []nativeReasoningProjectionItem
	for _, raw := range r.Compatibility {
		var item nativeReasoningProjectionItem
		if json.Unmarshal(raw, &item) != nil {
			return nil, false
		}
		if item.Type == "function_call" {
			original = append(original, item)
		}
	}
	for _, raw := range fragment {
		var item nativeReasoningProjectionItem
		_ = json.Unmarshal(raw, &item)
		if item.Type == "function_call" {
			normalized = append(normalized, item)
		}
	}
	for _, raw := range r.Output {
		if _, err := nativeReplayCanonicalObject(raw); err != nil {
			return nil, false
		}
		var item nativeReasoningProjectionItem
		if json.Unmarshal(raw, &item) != nil {
			return nil, false
		}
		if item.Type == "function_call" || item.Type == "custom_tool_call" {
			native = append(native, item)
		}
	}
	if len(native) == 0 || len(native) != len(original) || len(native) != len(normalized) || end+len(native) > len(input) {
		return nil, false
	}
	var fixes []nativeReasoningProjectionFix
	expectedResults := map[int]bool{}
	for i, call := range native {
		old, projected := original[i], normalized[i]
		clientName := call.Name
		if name, ok := r.ToolNameReverse[call.Name]; ok {
			clientName = name
			if reverse[call.Name] != name {
				return nil, false
			}
		}
		if current, ok := reverse[call.Name]; ok && current != clientName {
			return nil, false
		}
		arguments := call.Arguments
		if call.Type == "custom_tool_call" {
			arguments = call.Input
		}
		if call.CallID == "" || old.CallID != call.CallID || old.Name != clientName || old.Arguments != arguments || nativeReasoningProjectionName(old.Name, forward) != call.Name {
			return nil, false
		}
		if len(calls[projected.CallID]) != 1 || calls[projected.CallID][0] < start || calls[projected.CallID][0] >= end || len(results[projected.CallID]) != 1 {
			return nil, false
		}
		resultIndex := results[projected.CallID][0]
		if resultIndex < end || resultIndex >= end+len(native) || input[resultIndex].Type != "function_call_output" || expectedResults[resultIndex] {
			return nil, false
		}
		// Restoring an original ID must not collide with a different surviving
		// normalized call/result elsewhere in the request.
		for _, index := range calls[call.CallID] {
			if index < start || index >= end {
				return nil, false
			}
		}
		for _, index := range results[call.CallID] {
			if index != resultIndex {
				return nil, false
			}
		}
		expectedResults[resultIndex] = true
		outputType := "function_call_output"
		if call.Type == "custom_tool_call" {
			outputType = "custom_tool_call_output"
		}
		fixes = append(fixes, nativeReasoningProjectionFix{index: resultIndex, callID: call.CallID, outputType: outputType})
	}
	if end+len(native) < len(input) && nativeReasoningProjectionIsResult(input[end+len(native)].Type) {
		return nil, false
	}
	return fixes, true
}

func nativeReasoningProjectionName(name string, forward map[string]string) string {
	if alias, ok := forward[name]; ok {
		return alias
	}
	if alias := aliasOpenAIOAuthReservedToolName(name); alias != name {
		return alias
	}
	return name
}

func nativeReasoningProjectionForwardNames(reverse map[string]string) (map[string]string, error) {
	forward := make(map[string]string, len(reverse))
	for alias, original := range reverse {
		if strings.TrimSpace(alias) == "" || strings.TrimSpace(original) == "" {
			return nil, ErrNativeReasoningReplayInvalid
		}
		if old, exists := forward[original]; exists && old != alias {
			return nil, ErrNativeReasoningReplayAmbiguous
		}
		forward[original] = alias
	}
	return forward, nil
}

func nativeReasoningProjectionObject(raw []byte) (map[string]json.RawMessage, error) {
	_, err := nativeReplayCanonicalObject(raw)
	if err != nil {
		return nil, ErrNativeReasoningReplayInvalid
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil, ErrNativeReasoningReplayInvalid
	}
	return object, nil
}

func nativeReasoningProjectionItems(raw json.RawMessage) ([]json.RawMessage, error) {
	// Parse the original bytes, not the canonical comparison representation, so
	// untouched items retain their original serialized content during splicing.
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, ErrNativeReasoningReplayInvalid
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, ErrNativeReasoningReplayInvalid
	}
	return items, nil
}

func nativeReasoningProjectionArray(items []json.RawMessage) json.RawMessage {
	var out bytes.Buffer
	out.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			out.WriteByte(',')
		}
		out.Write(item)
	}
	out.WriteByte(']')
	return out.Bytes()
}

func nativeReasoningProjectionIsResult(typ string) bool {
	switch typ {
	case "function_call_output", "custom_tool_call_output", "mcp_tool_call_output", "tool_search_output":
		return true
	default:
		return false
	}
}
