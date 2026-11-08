package service

import (
	"bytes"
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

// sparseReplayOutput verifies the declared output set of one complete Responses
// event stream. Codex consumes completed items in arrival order; contiguous
// indices, added/done closure and exact reference agreement are additional
// gateway restrictions. Neither this nor an event sequence can prove that a
// provider generated an item it never declared on this stream.
type sparseReplayOutput struct {
	invalid       bool
	started       bool
	responseID    string
	sequenceMode  int // 0 unknown, 1 absent everywhere, 2 numbered everywhere
	lastSequence  int64
	lastEvent     []byte
	lastKind      string
	items         map[int]*sparseReplayItem
	ids           map[string]int
	calls         map[string]int
	doneOrder     []int
	retainedBytes int
}

type sparseReplayItem struct {
	id, kind, call, name string
	identity             map[string]json.RawMessage
	added                []byte
	doneCanonical        []byte
	doneRaw              json.RawMessage
}

func sparseReplayInteger(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	return n, err == nil && n >= 0
}

func sparseReplayString(object map[string]json.RawMessage, name string, required bool) (string, bool) {
	raw, present := object[name]
	if !present {
		return "", !required
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || bytes.Equal(raw, []byte("null")) || value == "" {
		return "", false
	}
	return value, true
}

func (s *sparseReplayOutput) fail() {
	s.invalid = true
	s.items, s.ids, s.calls, s.doneOrder, s.lastEvent = nil, nil, nil, nil, nil
}

func (s *sparseReplayOutput) observe(raw, canonical []byte, kind string, limits NativeReasoningReplayLimits) {
	if s.invalid {
		return
	}
	if !utf8.Valid(raw) {
		s.fail()
		return
	}
	var event map[string]json.RawMessage
	if json.Unmarshal(canonical, &event) != nil {
		s.fail()
		return
	}
	if value, present := event["type"]; present {
		var declared string
		if json.Unmarshal(value, &declared) != nil || declared == "" {
			s.fail()
			return
		}
	}
	sequence, numbered := event["sequence_number"]
	if s.sequenceMode == 0 {
		if numbered {
			s.sequenceMode = 2
		} else {
			s.sequenceMode = 1
		}
	} else if numbered != (s.sequenceMode == 2) {
		s.fail()
		return
	}
	if numbered {
		n, ok := sparseReplayInteger(sequence)
		if !ok {
			s.fail()
			return
		}
		if s.lastEvent != nil {
			if n == s.lastSequence && kind == s.lastKind && bytes.Equal(canonical, s.lastEvent) {
				return // An exact event retransmission does not declare another item.
			}
			if s.lastSequence == int64(^uint64(0)>>1) || n != s.lastSequence+1 {
				s.fail()
				return
			}
		}
		s.lastSequence, s.lastEvent, s.lastKind = n, bytes.Clone(canonical), kind
	}
	var response map[string]json.RawMessage
	if value, exists := event["response"]; exists && json.Unmarshal(value, &response) != nil {
		s.fail()
		return
	}
	if nativeReplayNonNull(event["error"]) || nativeReplayNonNull(event["incomplete_details"]) || nativeReplayNonNull(response["error"]) || nativeReplayNonNull(response["incomplete_details"]) {
		s.fail()
		return
	}
	for _, name := range []string{"output", "output_items", "items"} {
		if _, declared := event[name]; declared {
			s.fail() // No known lifecycle declares extra items at the event root.
			return
		}
	}
	if kind == "response.created" || kind == "response.in_progress" {
		id, ok := sparseReplayString(response, "id", true)
		if !ok || s.started && id != s.responseID || kind == "response.created" && len(s.doneOrder) > 0 {
			s.fail()
			return
		}
		if status, present := response["status"]; present && string(status) != `"in_progress"` && string(status) != `"queued"` {
			s.fail()
			return
		}
		if output, present := response["output"]; present && string(output) != "[]" {
			s.fail()
			return
		}
		s.started, s.responseID = true, id
	} else if !s.started {
		s.fail()
		return
	}
	if id, present := event["response_id"]; present {
		var value string
		if json.Unmarshal(id, &value) != nil || value != s.responseID {
			s.fail()
			return
		}
	}
	if id, present := response["id"]; present {
		var value string
		if json.Unmarshal(id, &value) != nil || value != s.responseID {
			s.fail()
			return
		}
	}
	switch kind {
	case "response.created", "response.in_progress":
		if _, exists := event["item"]; exists || s.hasReference(event) {
			s.fail()
		}
	case "response.completed", "response.done":
		if _, exists := event["item"]; exists || s.hasReference(event) {
			s.fail()
		}
	case "response.output_item.added", "response.output_item.done":
		s.observeItem(raw, event, kind, limits)
	case "response.metadata", "codex.response.metadata", "responsesapi.websocket_timing":
		// These known notifications do not declare output items. They still
		// participate in the sequence, response identity and error checks above.
		// A response envelope may carry headers; it must not declare output or
		// item references. Metadata/header payloads are never used as output.
		for _, fields := range []map[string]json.RawMessage{event, response} {
			if status, present := fields["status"]; present && string(status) != `"queued"` && string(status) != `"in_progress"` && string(status) != `"completed"` {
				s.fail()
				return
			}
			if s.hasReference(fields) {
				s.fail()
				return
			}
			for _, name := range []string{"item", "output", "output_items", "items", "content_index", "summary_index"} {
				if _, declared := fields[name]; declared {
					s.fail()
					return
				}
			}
		}
	default:
		expected := sparseReplayEventItemType(kind)
		if expected == "" || response != nil {
			s.fail()
			return
		}
		// The fixed consumer treats these optional payloads as absent when
		// omitted/null. Validate present values, but never reconstruct content
		// from them or impose a new mandatory delta/text field.
		for _, name := range []string{"delta", "text", "arguments", "input"} {
			if value, present := event[name]; present && nativeReplayNonNull(value) {
				var text string
				if json.Unmarshal(value, &text) != nil {
					s.fail()
					return
				}
			}
		}
		for _, name := range []string{"part", "annotation"} {
			if value, present := event[name]; present && nativeReplayNonNull(value) {
				var object map[string]json.RawMessage
				if json.Unmarshal(value, &object) != nil || object == nil {
					s.fail()
					return
				}
			}
		}
		item := s.reference(event, limits)
		if item == nil || item.kind != expected || item.doneRaw != nil {
			s.fail()
			return
		}
		for _, name := range []string{"content_index", "summary_index"} {
			if value, exists := event[name]; exists {
				if _, ok := sparseReplayInteger(value); !ok {
					s.fail()
					return
				}
			}
		}
		// Embedded output items in a non-item event would be a new declaration
		// outside the known lifecycle. Do not infer their relation from text.
		if _, present := event["item"]; present {
			s.fail()
		}
	}
}

func sparseReplayEventItemType(kind string) string {
	switch kind {
	case "response.content_part.added", "response.content_part.done", "response.output_text.delta", "response.output_text.done", "response.output_text.annotation.added", "response.refusal.delta", "response.refusal.done":
		return "message"
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		return "function_call"
	case "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		return "custom_tool_call"
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_text.delta", "response.reasoning_text.done":
		return "reasoning"
	default:
		return ""
	}
}

func (s *sparseReplayOutput) hasReference(event map[string]json.RawMessage) bool {
	for _, name := range []string{"output_index", "item_id", "call_id"} {
		if _, present := event[name]; present {
			return true
		}
	}
	return false
}

// Every reference on one event must resolve to the same item, not merely to
// some member of the final set. A pair of independently valid foreign IDs is
// still an invalid association.
func (s *sparseReplayOutput) reference(event map[string]json.RawMessage, limits NativeReasoningReplayLimits) *sparseReplayItem {
	index := -1
	if raw, present := event["output_index"]; present {
		n, ok := sparseReplayInteger(raw)
		if !ok || n >= int64(limits.MaxOutputItems) {
			return nil
		}
		index = int(n)
	}
	for _, field := range []string{"item_id", "call_id"} {
		if raw, present := event[field]; present {
			var id string
			if json.Unmarshal(raw, &id) != nil || id == "" {
				return nil
			}
			lookup := s.ids
			if field == "call_id" {
				lookup = s.calls
			}
			i, ok := lookup[id]
			if !ok || index >= 0 && i != index {
				return nil
			}
			index = i
		}
	}
	if index < 0 {
		return nil
	}
	return s.items[index]
}

func (s *sparseReplayOutput) observeItem(raw []byte, event map[string]json.RawMessage, kind string, limits NativeReasoningReplayLimits) {
	n, ok := sparseReplayInteger(event["output_index"])
	if !ok || n >= int64(limits.MaxOutputItems) {
		s.fail()
		return
	}
	index := int(n)
	var object map[string]json.RawMessage
	if json.Unmarshal(event["item"], &object) != nil || object == nil {
		s.fail()
		return
	}
	if nativeReplayNonNull(object["error"]) || nativeReplayNonNull(object["incomplete_details"]) {
		s.fail()
		return
	}
	id, idOK := sparseReplayString(object, "id", true)
	typeName, typeOK := sparseReplayString(object, "type", true)
	call, name := "", ""
	callOK, nameOK := true, true
	if typeName == "function_call" || typeName == "custom_tool_call" {
		call, callOK = sparseReplayString(object, "call_id", true)
		name, nameOK = sparseReplayString(object, "name", true)
	}
	if !idOK || !typeOK || !callOK || !nameOK || typeName != "reasoning" && typeName != "message" && typeName != "function_call" && typeName != "custom_tool_call" {
		s.fail()
		return
	}
	if typeName == "message" && string(object["role"]) != `"assistant"` {
		s.fail()
		return
	}
	if kind == "response.output_item.added" {
		if status, present := object["status"]; present && string(status) != `"in_progress"` && string(status) != `"completed"` {
			s.fail()
			return
		}
		if previous := s.items[index]; previous != nil {
			if !bytes.Equal(previous.added, event["item"]) || s.reference(event, limits) != previous {
				s.fail()
			}
			return
		}
		if index != len(s.items) {
			s.fail()
			return
		}
		if s.items == nil {
			s.items, s.ids, s.calls = make(map[int]*sparseReplayItem), make(map[string]int), make(map[string]int)
		}
		if _, duplicate := s.ids[id]; duplicate {
			s.fail()
			return
		}
		if call != "" {
			if _, duplicate := s.calls[call]; duplicate {
				s.fail()
				return
			}
			s.calls[call] = index
		}
		s.ids[id] = index
		s.retainedBytes += len(event["item"])
		if s.retainedBytes > limits.MaxEntryBytes {
			s.fail()
			return
		}
		s.items[index] = &sparseReplayItem{id: id, kind: typeName, call: call, name: name, identity: object, added: bytes.Clone(event["item"])}
	} else {
		item := s.items[index]
		if item == nil || item.id != id || item.kind != typeName || item.call != call || item.name != name {
			s.fail()
			return
		}
		if item.doneRaw != nil {
			if !bytes.Equal(item.doneCanonical, event["item"]) || s.reference(event, limits) != item {
				s.fail()
			}
			return
		}
		if index != len(s.doneOrder) {
			s.fail()
			return
		}
		for _, field := range []string{"role", "namespace", "phase"} {
			if value, present := item.identity[field]; present && !bytes.Equal(value, object[field]) {
				s.fail()
				return
			}
		}
		if status, present := object["status"]; present && string(status) != `"completed"` {
			s.fail()
			return
		}
		var original map[string]json.RawMessage
		if json.Unmarshal(raw, &original) != nil {
			s.fail()
			return
		}
		s.retainedBytes += len(original["item"]) + len(event["item"])
		if s.retainedBytes > limits.MaxEntryBytes {
			s.fail()
			return
		}
		item.doneRaw, item.doneCanonical = bytes.Clone(original["item"]), bytes.Clone(event["item"])
		s.doneOrder = append(s.doneOrder, index)
	}
	if s.reference(event, limits) != s.items[index] {
		s.fail()
	}
}

func (s *sparseReplayOutput) response(raw json.RawMessage, canonical []byte, limits NativeReasoningReplayLimits) (json.RawMessage, bool) {
	if s.invalid || !s.started || len(s.items) == 0 || len(s.items) != len(s.doneOrder) {
		return nil, false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(canonical, &object) != nil || string(object["status"]) != `"completed"` {
		return nil, false
	}
	id, ok := sparseReplayString(object, "id", true)
	if !ok || id != s.responseID || nativeReplayNonNull(object["error"]) || nativeReplayNonNull(object["incomplete_details"]) {
		return nil, false
	}
	var usage map[string]json.RawMessage
	if json.Unmarshal(object["usage"], &usage) != nil {
		return nil, false
	}
	for _, name := range []string{"input_tokens", "output_tokens"} {
		if _, ok := sparseReplayInteger(usage[name]); !ok {
			return nil, false
		}
	}
	if value, present := usage["total_tokens"]; present {
		if _, ok := sparseReplayInteger(value); !ok {
			return nil, false
		}
	}
	for name, keys := range map[string][]string{"input_tokens_details": {"cached_tokens", "cache_write_tokens"}, "output_tokens_details": {"reasoning_tokens"}} {
		if value, present := usage[name]; present && nativeReplayNonNull(value) {
			var details map[string]json.RawMessage
			if json.Unmarshal(value, &details) != nil || details == nil {
				return nil, false
			}
			for _, key := range keys {
				if raw, exists := details[key]; exists {
					n, ok := sparseReplayInteger(raw)
					input, _ := sparseReplayInteger(usage["input_tokens"])
					if !ok || key == "cached_tokens" && n > input {
						return nil, false
					}
				}
			}
		}
	}
	var output bytes.Buffer
	output.WriteByte('[')
	visible := false
	for index := range s.doneOrder {
		item := s.items[index]
		if item == nil || len(item.doneRaw) == 0 {
			return nil, false
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(item.doneRaw, &fields) != nil {
			return nil, false
		}
		if encrypted, present := fields["encrypted_content"]; present {
			var text string
			if json.Unmarshal(encrypted, &text) != nil {
				return nil, false
			}
		}
		visible = visible || item.kind == "message" || item.kind == "function_call" || item.kind == "custom_tool_call"
		if index > 0 {
			output.WriteByte(',')
		}
		output.Write(item.doneRaw)
	}
	output.WriteByte(']')
	if !visible {
		return nil, false
	}
	var original map[string]json.RawMessage
	if json.Unmarshal(raw, &original) != nil {
		return nil, false
	}
	// Marshal envelope fields as raw values, then insert the original item block
	// verbatim. No typed ResponseItem serialization may discard extension data.
	delete(original, "output")
	var rebuilt bytes.Buffer
	rebuilt.WriteByte('{')
	for key, value := range original {
		encoded, _ := json.Marshal(key)
		rebuilt.Write(encoded)
		rebuilt.WriteByte(':')
		rebuilt.Write(value)
		rebuilt.WriteByte(',')
	}
	rebuilt.WriteString(`"output":`)
	rebuilt.Write(output.Bytes())
	rebuilt.WriteByte('}')
	if rebuilt.Len() > limits.MaxEntryBytes {
		return nil, false
	}
	return rebuilt.Bytes(), true
}
