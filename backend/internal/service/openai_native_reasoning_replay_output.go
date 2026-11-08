package service

import (
	"bytes"
	"encoding/json"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

func (r *NativeReasoningReplay) validateCompletion(model string, response, assistant json.RawMessage, reverse map[string]string) ([]json.RawMessage, error) {
	canonical, err := nativeReplayCanonicalObject(response)
	if err != nil {
		return nil, ErrNativeReasoningReplayInvalid
	}
	var envelope struct {
		ID         string            `json:"id"`
		Model      string            `json:"model"`
		Status     string            `json:"status"`
		Error      json.RawMessage   `json:"error"`
		Incomplete json.RawMessage   `json:"incomplete_details"`
		Output     []json.RawMessage `json:"output"`
	}
	if json.Unmarshal(canonical, &envelope) != nil || envelope.ID == "" || envelope.Status != "completed" ||
		(envelope.Model != "" && envelope.Model != model) || nativeReplayNonNull(envelope.Error) || nativeReplayNonNull(envelope.Incomplete) ||
		len(envelope.Output) == 0 || len(envelope.Output) > r.limits.MaxOutputItems {
		return nil, ErrNativeReasoningReplayInvalid
	}
	var visible apicompat.ResponsesResponse
	if json.Unmarshal(canonical, &visible) != nil {
		return nil, ErrNativeReasoningReplayInvalid
	}
	ids, calls := map[string]bool{}, map[string]bool{}
	hasTool, hasMessage := false, false
	for i, raw := range envelope.Output {
		var item struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			Role      string `json:"role"`
			Status    string `json:"status"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Encrypted string `json:"encrypted_content"`
		}
		if json.Unmarshal(raw, &item) != nil || item.ID == "" || ids[item.ID] || (item.Status != "" && item.Status != "completed") {
			return nil, ErrNativeReasoningReplayInvalid
		}
		ids[item.ID] = true
		switch item.Type {
		case "reasoning", "web_search_call":
		case "message":
			if item.Role != "assistant" {
				return nil, ErrNativeReasoningReplayInvalid
			}
			for _, part := range visible.Output[i].Content {
				if part.Type != "output_text" && part.Type != "refusal" {
					return nil, ErrNativeReasoningReplayInvalid
				}
			}
			hasMessage = true
		case "function_call", "custom_tool_call":
			if item.CallID == "" || item.Name == "" || calls[item.CallID] {
				return nil, ErrNativeReasoningReplayInvalid
			}
			calls[item.CallID] = true
			hasTool = true
			if name, ok := reverse[item.Name]; ok {
				if name == "" {
					return nil, ErrNativeReasoningReplayInvalid
				}
				visible.Output[i].Name = name
			}
			if item.Type == "custom_tool_call" {
				// The streaming production converter exposes custom input as the
				// function arguments. This is projection only; raw output is kept.
				visible.Output[i].Type = "function_call"
				visible.Output[i].Arguments = visible.Output[i].Input
			}
		default:
			// A newly introduced visible output type needs an explicit projection
			// contract. Preserve availability through the normal conversion path.
			return nil, ErrNativeReasoningReplayInvalid
		}
	}
	// Both tool turns and final assistant messages belong to the native history.
	// A reasoning-only result has no supported visible-message mapping here.
	if !hasTool && !hasMessage {
		return nil, ErrNativeReasoningReplayInvalid
	}
	projected := apicompat.ResponsesToChatCompletions(&visible, model)
	if len(projected.Choices) != 1 {
		return nil, ErrNativeReasoningReplayInvalid
	}
	if !hasTool && !nativeReplayVisibleAssistant(projected.Choices[0].Message) {
		return nil, ErrNativeReasoningReplayInvalid
	}
	rawMessage, _ := json.Marshal(projected.Choices[0].Message)
	expected, err := nativeReplayCanonicalMessage(rawMessage)
	if err != nil || !bytes.Equal(expected, assistant) {
		return nil, ErrNativeReasoningReplayInvalid
	}
	// Preserve the original per-item bytes, including unknown fields and phase
	// presence. Canonicalization above was only for validation and projection.
	var original struct {
		Output []json.RawMessage `json:"output"`
	}
	if json.Unmarshal(response, &original) != nil {
		return nil, ErrNativeReasoningReplayInvalid
	}
	return original.Output, nil
}

func nativeReplayNonNull(b []byte) bool {
	return len(b) != 0 && !bytes.Equal(bytes.TrimSpace(b), []byte("null"))
}

// NativeReasoningReplayCollector consumes raw native frames before any tool-name
// restoration or lossy typed conversion. It is request-local, not goroutine safe.
// A nonempty terminal array remains the authoritative exact comparison. Sparse
// completion is usable only after the independently checked declared item set
// closes; isolated done events or a truncated stream remain insufficient.
type NativeReasoningReplayCollector struct {
	limits           NativeReasoningReplayLimits
	added            map[int]string
	done             map[int]json.RawMessage
	bytes            int
	responseID       string
	terminal         json.RawMessage
	terminalObserved json.RawMessage
	sparse           sparseReplayOutput
	invalid          bool
}

func NewNativeReasoningReplayCollector(limits NativeReasoningReplayLimits) *NativeReasoningReplayCollector {
	return &NativeReasoningReplayCollector{limits: limits.Normalize(), added: make(map[int]string), done: make(map[int]json.RawMessage)}
}

// Observe accepts an optional SSE event-line type for frames without a JSON
// type. Conflicting types, conflicting duplicate events, and events after the
// terminal poison this collection. Identical terminal/done repeats are harmless.
func (c *NativeReasoningReplayCollector) Observe(raw []byte, eventType string) {
	if c == nil || c.invalid {
		return
	}
	if len(raw) > c.limits.MaxEntryBytes {
		c.invalid = true
		return
	}
	canonical, err := nativeReplayCanonicalObject(raw)
	if err != nil || len(canonical) > c.limits.MaxEntryBytes {
		c.invalid = true
		return
	}
	var event struct {
		Type     string          `json:"type"`
		Index    *int            `json:"output_index"`
		Item     json.RawMessage `json:"item"`
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(canonical, &event) != nil {
		c.invalid = true
		return
	}
	if event.Type == "" {
		event.Type = eventType
	} else if eventType != "" && eventType != event.Type {
		c.invalid = true
		return
	}
	if event.Type == "" {
		c.invalid = true
		return
	}
	c.sparse.observe(raw, canonical, event.Type, c.limits)
	switch event.Type {
	case "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
		c.invalid = true
	case "response.created", "response.in_progress":
		if c.terminal != nil {
			c.invalid = true
			return
		}
		var response struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(event.Response, &response) != nil || response.ID == "" || (c.responseID != "" && c.responseID != response.ID) {
			c.invalid = true
			return
		}
		c.responseID = response.ID
	case "response.output_item.added", "response.output_item.done":
		if event.Index == nil || *event.Index < 0 || *event.Index >= c.limits.MaxOutputItems {
			c.invalid = true
			return
		}
		item, err := nativeReplayCanonicalObject(event.Item)
		var identity struct {
			ID string `json:"id"`
		}
		if err != nil || json.Unmarshal(item, &identity) != nil || identity.ID == "" {
			c.invalid = true
			return
		}
		index := *event.Index
		if old, exists := c.added[index]; exists && old != identity.ID {
			c.invalid = true
			return
		}
		if event.Type == "response.output_item.added" {
			if c.terminal != nil {
				c.invalid = true
				return
			}
			c.added[index] = identity.ID
			return
		}
		if old, exists := c.done[index]; exists {
			if !bytes.Equal(old, item) {
				c.invalid = true
			}
			return
		}
		if c.terminal != nil {
			c.invalid = true
			return
		}
		c.bytes += len(item)
		if c.bytes > c.limits.MaxEntryBytes {
			c.invalid = true
			return
		}
		c.done[index] = item
	case "response.completed", "response.done":
		response, err := nativeReplayCanonicalObject(event.Response)
		if err != nil {
			c.invalid = true
			return
		}
		if c.terminal != nil {
			if !bytes.Equal(c.terminalObserved, response) {
				c.invalid = true
			}
			return
		}
		var terminal struct {
			ID     string            `json:"id"`
			Status string            `json:"status"`
			Output []json.RawMessage `json:"output"`
		}
		if json.Unmarshal(response, &terminal) != nil || terminal.ID == "" || terminal.Status != "completed" ||
			(c.responseID != "" && terminal.ID != c.responseID) || len(terminal.Output) > c.limits.MaxOutputItems {
			c.invalid = true
			return
		}
		if len(terminal.Output) == 0 {
			var fields map[string]json.RawMessage
			var original map[string]json.RawMessage
			if json.Unmarshal(response, &fields) != nil || json.Unmarshal(raw, &original) != nil {
				c.invalid = true
				return
			}
			if output, exists := fields["output"]; exists && !bytes.Equal(output, []byte("[]")) {
				c.invalid = true // Explicit null/other empty shapes are not reconstructed.
				return
			}
			rebuilt, complete := c.sparse.response(original["response"], response, c.limits)
			if !complete {
				c.invalid = true
				return
			}
			c.terminal, c.terminalObserved = rebuilt, response
			return
		}
		for index, id := range c.added {
			if index >= len(terminal.Output) {
				c.invalid = true
				return
			}
			var item struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(terminal.Output[index], &item) != nil || item.ID != id {
				c.invalid = true
				return
			}
		}
		for index, done := range c.done {
			if index >= len(terminal.Output) || !bytes.Equal(done, terminal.Output[index]) {
				c.invalid = true
				return
			}
		}
		c.terminal, c.terminalObserved = response, response
	default:
		if c.terminal != nil {
			c.invalid = true
		}
	}
}

func (c *NativeReasoningReplayCollector) CompletedResponse() (json.RawMessage, bool) {
	if c == nil || c.invalid || len(c.terminal) == 0 {
		return nil, false
	}
	return append(json.RawMessage(nil), c.terminal...), true
}

// ApplyNativeReasoningReplayInput performs an exact anchored splice. The caller
// may update Compatibility and its anchors/positions while applying its own
// transforms, but must never modify Output. An anchor mismatch fails the whole
// splice. A causal-prefix mismatch keeps that complete compatibility group;
// later groups must independently prove their prefix before being restored.
func ApplyNativeReasoningReplayInput(input json.RawMessage, replacements []NativeReasoningReplayReplacement) (json.RawMessage, error) {
	var items []json.RawMessage
	if json.Unmarshal(input, &items) != nil {
		return nil, ErrNativeReasoningReplayInvalid
	}
	if len(replacements) == 0 {
		return input, nil
	}
	canonical := func(a, b []byte) bool {
		x, e1 := nativeReplayCanonical(a)
		y, e2 := nativeReplayCanonical(b)
		return e1 == nil && e2 == nil && bytes.Equal(x, y)
	}
	previous := 0
	var out []json.RawMessage
	for _, replacement := range replacements {
		start, end := replacement.CompatibilityStart, replacement.CompatibilityEnd
		if start < previous || end <= start || end > len(items) || end-start != len(replacement.Compatibility) || len(replacement.Output) == 0 {
			return nil, ErrNativeReasoningReplayInvalid
		}
		if start > 0 && !canonical(items[start-1], replacement.Before) {
			return nil, ErrNativeReasoningReplayInvalid
		}
		if end < len(items) && !canonical(items[end], replacement.After) {
			return nil, ErrNativeReasoningReplayInvalid
		}
		for i, item := range replacement.Compatibility {
			if !canonical(items[start+i], item) {
				return nil, ErrNativeReasoningReplayInvalid
			}
		}
		out = append(out, items[previous:start]...)
		prefix, err := json.Marshal(out)
		if err != nil {
			return nil, ErrNativeReasoningReplayInvalid
		}
		if out == nil {
			prefix = json.RawMessage(`[]`)
		}
		digest, err := NativeReasoningReplayInputDigest(prefix)
		if err != nil {
			return nil, err
		}
		if len(replacement.ExpectedSourceInputDigest) != 64 {
			return nil, ErrNativeReasoningReplayInvalid
		}
		if digest == replacement.ExpectedSourceInputDigest {
			out = append(out, replacement.Output...)
		} else {
			out = append(out, items[start:end]...)
		}
		previous = end
	}
	out = append(out, items[previous:]...)
	return json.Marshal(out)
}
