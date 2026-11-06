//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// The source-build catalog may expose exec through the code-mode namespace.
// Select only a capability actually declared in this request. This preserves
// the old direct-tool fixture while exercising the fixed source's real route.
func goldenDeclaredToolCall(body []byte, id string) (map[string]any, bool) {
	var request struct {
		Tools []map[string]any `json:"tools"`
		Input []struct {
			Type  string           `json:"type"`
			Tools []map[string]any `json:"tools"`
		} `json:"input"`
	}
	if json.Unmarshal(body, &request) != nil {
		return nil, false
	}
	tools := request.Tools
	for _, item := range request.Input {
		if item.Type == "additional_tools" {
			tools = append(tools, item.Tools...)
		}
	}
	type declared struct {
		name, namespace, kind, description string
		parameters                         map[string]any
	}
	var declarations []declared
	var collect func([]map[string]any, string)
	collect = func(tools []map[string]any, namespace string) {
		for _, tool := range tools {
			kind, _ := tool["type"].(string)
			name, _ := tool["name"].(string)
			if kind == "namespace" && namespace == "" && name != "" {
				var nested []map[string]any
				data, _ := json.Marshal(tool["tools"])
				if json.Unmarshal(data, &nested) == nil {
					collect(nested, name)
				}
				continue
			}
			description, _ := tool["description"].(string)
			parameters, _ := tool["parameters"].(map[string]any)
			declarations = append(declarations, declared{name, namespace, kind, description, parameters})
		}
	}
	collect(tools, "")
	for _, tool := range declarations {
		if tool.kind != "custom" || tool.name != "exec" || !strings.Contains(tool.description, "exec_command") {
			continue
		}
		call := map[string]any{"type": "custom_tool_call", "id": "ct_golden_" + id, "call_id": "call_golden_code_mode_" + id, "name": "exec", "input": `text(JSON.stringify(await tools.exec_command({cmd:"printf 'golden\\n'"})));`}
		if tool.namespace != "" {
			call["namespace"] = tool.namespace
		}
		return call, true
	}
	for _, tool := range declarations {
		if tool.kind != "function" || tool.name != "exec_command" {
			continue
		}
		properties, _ := tool.parameters["properties"].(map[string]any)
		cmd, _ := properties["cmd"].(map[string]any)
		if cmd["type"] != "string" {
			continue
		}
		call := map[string]any{"type": "function_call", "id": "fc_golden_" + id, "call_id": "call_golden_" + id, "name": "exec_command", "arguments": `{"cmd":"echo golden"}`, "status": "completed"}
		if tool.namespace != "" {
			call["namespace"] = tool.namespace
		}
		return call, true
	}
	return nil, false
}

var codeModeCompletion = regexp.MustCompile(`^Script completed\nWall time [0-9]+\.[0-9] seconds\nOutput:\n$`)
var codeModeChunk = regexp.MustCompile(`^[0-9a-f]+$`)

type goldenCodeModeOutput struct {
	Items  []map[string]any
	Result map[string]any
}

// Parse only the fixture-owned two-item exec wrapper. User tool text containing
// lookalike fields is never normalized or accepted as execution evidence.
func parseGoldenCodeModeOutput(value any) (goldenCodeModeOutput, bool) {
	var out goldenCodeModeOutput
	items, ok := value.([]any)
	if !ok || len(items) != 2 {
		return out, false
	}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok || m["type"] != "input_text" {
			return out, false
		}
		if _, ok := m["text"].(string); !ok {
			return out, false
		}
		out.Items = append(out.Items, m)
	}
	if !codeModeCompletion.MatchString(out.Items[0]["text"].(string)) {
		return out, false
	}
	if unmarshalReferenceJSON([]byte(out.Items[1]["text"].(string)), &out.Result) != nil || out.Result == nil {
		return out, false
	}
	chunk, _ := out.Result["chunk_id"].(string)
	if !codeModeChunk.MatchString(chunk) {
		return out, false
	}
	wall, ok := out.Result["wall_time_seconds"].(json.Number)
	if !ok {
		return out, false
	}
	elapsed, err := wall.Float64()
	if err != nil || elapsed < 0 {
		return out, false
	}
	return out, true
}

func successfulGoldenCodeModeOutput(raw json.RawMessage) bool {
	var value any
	if unmarshalReferenceJSON(raw, &value) != nil {
		return false
	}
	out, ok := parseGoldenCodeModeOutput(value)
	if !ok {
		return false
	}
	code, ok := out.Result["exit_code"].(json.Number)
	if !ok {
		return false
	}
	exit, err := code.Int64()
	_, hasSession := out.Result["session_id"]
	return err == nil && exit == 0 && !hasSession && out.Result["output"] == "golden\n"
}

func (n *referenceNormalizer) normalizeGoldenCodeModeOutput(value any) (any, bool) {
	out, ok := parseGoldenCodeModeOutput(value)
	if !ok {
		return nil, false
	}
	items := make([]any, len(out.Items))
	for i, item := range out.Items {
		copy := map[string]any{}
		for key, value := range item {
			copy[key] = value
		}
		items[i] = copy
	}
	items[0].(map[string]any)["text"] = "Script completed\nWall time <SECONDS> seconds\nOutput:\n"
	result := map[string]any{}
	for key, value := range out.Result {
		result[key] = value
	}
	result["chunk_id"] = n.chunkIdentity(out.Result["chunk_id"].(string))
	result["wall_time_seconds"] = json.Number("0")
	items[1].(map[string]any)["text"] = string(mustJSON(result))
	return items, true
}

func (n *referenceNormalizer) chunkIdentity(value string) string {
	if n.chunks == nil {
		n.chunks = map[string]string{}
	}
	if id, ok := n.chunks[value]; ok {
		return id
	}
	id := fmt.Sprintf("<CHUNK:%d:lower_hex:%d>", len(n.chunks)+1, len(value))
	n.chunks[value] = id
	return id
}
