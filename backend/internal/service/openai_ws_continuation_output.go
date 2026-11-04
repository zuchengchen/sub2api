package service

import (
	"crypto/sha256"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
)

const openAIWSContinuationOutputLimit = 2048

// Only bounded fingerprints survive a stream event. Do not retain tool text,
// images or encrypted reasoning in the connection's continuation checkpoint.
type openAIWSContinuationOutputItem struct {
	digest   [sha256.Size]byte
	id       [sha256.Size]byte
	callID   [sha256.Size]byte
	toolType string
	kind     string
	done     bool
}

type openAIWSContinuationOutputCollector struct {
	items   []openAIWSContinuationOutputItem
	invalid bool
}

func openAIWSContinuationOutputIdentity(item gjson.Result) [sha256.Size]byte {
	if id := item.Get("id").String(); id != "" {
		return sha256.Sum256([]byte(id))
	}
	return [sha256.Size]byte{}
}

func openAIWSContinuationOutputFingerprint(item gjson.Result) (openAIWSContinuationOutputItem, bool) {
	digest, ok := openAIWSContinuationItemDigest([]byte(item.Raw))
	if !ok {
		return openAIWSContinuationOutputItem{}, false
	}
	result := openAIWSContinuationOutputItem{digest: digest, id: openAIWSContinuationOutputIdentity(item), kind: openAIWSContinuationDiagnosticType(item), done: true}
	switch kind := item.Get("type").String(); kind {
	case "function_call", "custom_tool_call", "local_shell_call", "tool_search_call":
		if kind == "tool_search_call" && item.Get("execution").String() == "server" {
			break // Server-owned search does not require a client tool result.
		}
		id := item.Get("call_id").String()
		if strings.TrimSpace(id) == "" {
			return result, false
		}
		result.callID, result.toolType = sha256.Sum256([]byte(id)), kind+"_output"
		if kind == "local_shell_call" {
			result.toolType = "function_call_output"
		}
		if kind == "tool_search_call" {
			result.toolType = "tool_search_output"
		}
	case "tool_search_output":
		if item.Get("execution").String() != "server" {
			return result, false
		}
	case "function_call_output", "custom_tool_call_output":
		// Tool results belong to input; do not infer a continuation contract
		// for unexpected output-side results.
		return result, false
	}
	return result, true
}

func (c *openAIWSContinuationOutputCollector) observe(event []byte, eventType string) {
	if c.invalid || (eventType != "response.output_item.added" && eventType != "response.output_item.done") {
		return
	}
	index := gjson.GetBytes(event, "output_index")
	if index.Type != gjson.Number || index.Float() != float64(index.Int()) || index.Int() < 0 || index.Int() >= openAIWSContinuationOutputLimit {
		c.invalid = true
		return
	}
	i := int(index.Int())
	for len(c.items) <= i {
		c.items = append(c.items, openAIWSContinuationOutputItem{})
	}
	item := gjson.GetBytes(event, "item")
	id := openAIWSContinuationOutputIdentity(item)
	previous := c.items[i]
	if !item.IsObject() || (previous.id != [sha256.Size]byte{} && previous.id != id) {
		c.invalid = true
		return
	}
	if eventType == "response.output_item.added" {
		c.items[i].id = id
		return
	}
	completed, ok := openAIWSContinuationOutputFingerprint(item)
	if !ok || (previous.done && previous != completed) {
		c.invalid = true
		return
	}
	c.items[i] = completed
}

// A terminal output may omit items already sent as output_item.done. Merge by
// identity, then require a contiguous, conflict-free sequence. Ambiguous or
// unfinished output disables the checkpoint without changing the client stream.
func (c *openAIWSContinuationOutputCollector) finish(output gjson.Result) ([]openAIWSContinuationOutputItem, bool) {
	if c.invalid || (!output.IsArray() && (output.Exists() || len(c.items) == 0)) {
		return nil, false
	}
	terminal := output.Array()
	if len(terminal) > openAIWSContinuationOutputLimit {
		return nil, false
	}
	items := append([]openAIWSContinuationOutputItem(nil), c.items...)
	lastIndex := -1
	for i, raw := range terminal {
		item, ok := openAIWSContinuationOutputFingerprint(raw)
		if !ok {
			return nil, false
		}
		index := -1
		for j, existing := range items {
			if item.id != [sha256.Size]byte{} && existing.id == item.id {
				index = j
				break
			}
		}
		if index < 0 {
			// Without an identity, positional matching needs a complete final
			// array, or an exact match at the already observed position.
			if len(terminal) < len(items) && (i >= len(items) || !items[i].done || items[i] != item) {
				return nil, false
			}
			index = i
		}
		if index <= lastIndex {
			return nil, false
		}
		lastIndex = index
		for len(items) <= index {
			items = append(items, openAIWSContinuationOutputItem{})
		}
		existing := items[index]
		if (existing.done && existing != item) || (existing.id != [sha256.Size]byte{} && existing.id != item.id) {
			return nil, false
		}
		items[index] = item
	}
	ids := make(map[[sha256.Size]byte]bool)
	for _, item := range items {
		if !item.done || (item.id != [sha256.Size]byte{} && ids[item.id]) {
			return nil, false
		}
		ids[item.id] = true
	}
	return items, true
}

// Check both directions, including parallel calls and custom tools. Never
// fabricate missing results or attach results to a different kind of call.
func openAIWSContinuationToolsComplete(items []gjson.Result, pending map[[sha256.Size]byte]string) bool {
	expected := make(map[[sha256.Size]byte]string, len(pending))
	seen := make(map[[sha256.Size]byte]bool, len(pending))
	for id, kind := range pending {
		expected[id], seen[id] = kind, true
	}
	for _, item := range items {
		kind := item.Get("type").String()
		switch kind {
		case "function_call", "custom_tool_call", "local_shell_call", "tool_search_call", "tool_search_output", "function_call_output", "custom_tool_call_output":
			if (kind == "tool_search_call" || kind == "tool_search_output") && item.Get("execution").String() == "server" {
				continue
			}
			id := item.Get("call_id").String()
			if item.Get("call_id").Type != gjson.String || strings.TrimSpace(id) == "" {
				return false
			}
			key := sha256.Sum256([]byte(id))
			if strings.HasSuffix(kind, "_output") {
				output := item.Get("output")
				validOutput := output.Type == gjson.String || output.IsArray()
				if kind == "tool_search_output" {
					validOutput = item.Get("execution").String() == "client" && item.Get("tools").IsArray()
				}
				if expected[key] != kind || !validOutput {
					return false
				}
				delete(expected, key)
			} else {
				if seen[key] {
					return false
				}
				resultKind := kind + "_output"
				if kind == "local_shell_call" {
					resultKind = "function_call_output"
				}
				if kind == "tool_search_call" {
					resultKind = "tool_search_output"
				}
				expected[key], seen[key] = resultKind, true
			}
		}
	}
	return len(expected) == 0
}

// This is a targeted repair for an inferred anchor, not a generic 400 retry.
// Verify the exact rejected call exists with a real result in the full input.
func openAIWSCanReplayMissingToolOutput(responseError gjson.Result, request map[string]any) bool {
	if openAIWSPayloadString(request, "previous_response_id") != "" {
		return false
	}
	if kind := responseError.Get("type").String(); kind != "" && kind != "invalid_request_error" {
		return false
	}
	if code := responseError.Get("code").String(); code != "" && code != "invalid_request_error" {
		return false
	}
	if param := responseError.Get("param").String(); param != "" && param != "input" {
		return false
	}
	message := strings.TrimSpace(responseError.Get("message").String())
	for _, kind := range []string{"function", "custom_tool"} {
		prefix := "No tool output found for " + strings.ReplaceAll(kind, "_", " ") + " call "
		if !strings.HasPrefix(message, prefix) {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(message, prefix), ".")
		if id == "" || strings.ContainsAny(id, " \r\n\t") {
			return false
		}
		raw, err := json.Marshal(request["input"])
		if err != nil {
			return false
		}
		input := gjson.ParseBytes(raw)
		if !input.IsArray() || !openAIWSContinuationToolsComplete(input.Array(), nil) {
			return false
		}
		for _, item := range input.Array() {
			if item.Get("type").String() == kind+"_call" && item.Get("call_id").String() == id {
				return true
			}
		}
	}
	return false
}
