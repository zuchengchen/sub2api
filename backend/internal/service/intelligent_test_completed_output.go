package service

import (
	"sort"
	"strings"
)

const intelligentCompletedOutputItemLimit = 1024

type intelligentCompletedOutputItem struct {
	id       string
	text     string
	complete bool
}

// Some Responses streams put the final messages only in output_item.done and
// send an empty output array in response.completed. Keep just the bounded text
// of those messages, independently of the raw diagnostic capture limit.
type intelligentCompletedOutput struct {
	items    map[int]intelligentCompletedOutputItem
	ids      map[string]int
	bytes    int
	overflow bool
	invalid  bool
}

func (c *intelligentCompletedOutput) observe(event map[string]any) {
	kind, _ := event["type"].(string)
	if (kind != "response.output_item.added" && kind != "response.output_item.done") || c.overflow || c.invalid {
		return
	}
	item, _ := event["item"].(map[string]any)
	if item["type"] != "message" || item["role"] != "assistant" {
		return
	}
	index, ok := event["output_index"].(float64)
	if !ok || index < 0 || index > 1<<31-1 || index != float64(int(index)) {
		c.invalid = true
		return
	}
	position := int(index)
	id, _ := item["id"].(string)
	// Bound identifiers too, and reject one message reported in two positions.
	if len(id) > 1024 {
		c.invalid = true
		return
	}
	if previous, exists := c.ids[id]; id != "" && exists && previous != position {
		c.invalid = true
		return
	}
	previous, exists := c.items[position]
	if !exists && len(c.items) >= intelligentCompletedOutputItemLimit {
		c.overflow = true
		return
	}
	next := intelligentCompletedOutputItem{id: id, complete: kind == "response.output_item.done" && item["status"] == "completed"}
	if next.complete {
		next.text, _ = intelligentResponsesTerminalText(map[string]any{
			"response": map[string]any{"output": []any{item}},
		})
	}
	size := c.bytes - len(previous.text) + len(next.text)
	if size > intelligentCaptureTextLimit {
		c.overflow = true
		return
	}
	if c.items == nil {
		c.items = make(map[int]intelligentCompletedOutputItem)
		c.ids = make(map[string]int)
	}
	if previous.id != "" {
		delete(c.ids, previous.id)
	}
	if id != "" {
		c.ids[id] = position
	}
	c.items[position], c.bytes = next, size
}

func (c *intelligentCompletedOutput) terminalText(event map[string]any) (text string, present, truncated bool) {
	text, present = intelligentResponsesTerminalText(event)
	response, _ := event["response"].(map[string]any)
	output, isArray := response["output"].([]any)
	// Nonempty terminal output stays authoritative, even if its text is empty.
	// Missing output keeps the existing delta fallback. Only explicit [] needs
	// reconstruction, and a failed response can never become successful here.
	if !isArray || len(output) != 0 || c.invalid || !intelligentResponsesTerminalSuccessful(event) {
		return text, present, false
	}
	if c.overflow {
		return text, present, true
	}
	if len(c.items) == 0 {
		return text, present, false
	}
	positions := make([]int, 0, len(c.items))
	for position, item := range c.items {
		if !item.complete {
			return text, present, false
		}
		positions = append(positions, position)
	}
	sort.Ints(positions)
	var result strings.Builder
	result.Grow(c.bytes)
	for _, position := range positions {
		result.WriteString(c.items[position].text)
	}
	return result.String(), true, false
}
