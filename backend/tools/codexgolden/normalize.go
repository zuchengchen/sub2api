//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

var dynamicUUID = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
var dynamicChunk = regexp.MustCompile(`Chunk ID: [0-9a-f]+`)
var dynamicWallTime = regexp.MustCompile(`Wall time: [0-9.]+ seconds`)
var dynamicDate = regexp.MustCompile(`<current_date>[^<]+</current_date>`)

type referenceNormalizer struct{ ids map[string]string }

func (n *referenceNormalizer) value(key string, value any) any {
	// Preserve field presence and type; only clocks and durations are volatile.
	volatile := false
	switch key {
	case "created_at", "started_at", "completed_at", "create_time", "turn_started_at_unix_ms",
		"x-codex-ws-stream-request-start-ms", "started_at_ms", "completed_at_ms", "duration_ms",
		"execution_duration_ms", "before_first_sampling_ms", "sampling_ms", "compaction_ms",
		"between_sampling_overhead_ms", "tool_blocking_ms", "after_last_sampling_ms",
		"active_context_tokens_before", "active_context_tokens_after":
		volatile = true
	}
	if value != nil && volatile {
		switch value.(type) {
		case string:
			return "<CLOCK>"
		case float64:
			return float64(0)
		}
	}
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out[k] = n.value(k, v[k])
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = n.value("", child)
		}
		return out
	case string:
		if key == "sec-websocket-key" || key == "content-length" || key == "date" {
			return "<DYNAMIC>"
		}
		// Metadata is JSON encoded inside a string, with its own random map order.
		var embedded any
		if (strings.HasPrefix(v, "{") || strings.HasPrefix(v, "[")) && json.Unmarshal([]byte(v), &embedded) == nil {
			return string(mustJSON(n.value("", embedded)))
		}
		v = dynamicUUID.ReplaceAllStringFunc(v, func(id string) string {
			if n.ids == nil {
				n.ids = make(map[string]string)
			}
			if n.ids[id] == "" {
				n.ids[id] = fmt.Sprintf("<ID:%d>", len(n.ids)+1)
			}
			return n.ids[id]
		})
		if key == "output" {
			v = dynamicChunk.ReplaceAllString(v, "Chunk ID: <CHUNK>")
			v = dynamicWallTime.ReplaceAllString(v, "Wall time: <SECONDS> seconds")
		}
		if key == "text" {
			v = dynamicDate.ReplaceAllString(v, "<current_date><DATE></current_date>")
		}
		return v
	default:
		return value
	}
}

// Concurrent bootstrap endpoints may race. Preserve order within each endpoint
// and every Responses frame, while comparing independent endpoint groups apart.
func normalizedRecordGroups(records map[recordKey]record) map[string][]any {
	groups := make(map[string][]record)
	for _, rec := range records {
		key := rec.Kind + " " + rec.Method + " " + rec.Target
		if rec.Kind == "ws_message" || strings.Contains(rec.Target, "/codex/responses") || strings.HasSuffix(rec.Target, "/oauth/token") {
			key = "responses_lifecycle"
		}
		groups[key] = append(groups[key], rec)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	normalizer := &referenceNormalizer{}
	out := make(map[string][]any)
	for _, key := range keys {
		recs := groups[key]
		sort.Slice(recs, func(i, j int) bool { return recs[i].Seq < recs[j].Seq })
		connections := make(map[int]int)
		for _, rec := range recs {
			headers := make([]headerKV, 0, len(rec.Headers))
			for _, header := range rec.Headers {
				header.Value, _ = normalizer.value(strings.ToLower(header.Name), header.Value).(string)
				headers = append(headers, header)
			}
			// The MCP client's product headers come from a randomized HashMap.
			// Responses/WS header order is intentionally still compared exactly.
			if strings.HasSuffix(rec.Target, "/ps/mcp") {
				sort.Slice(headers, func(i, j int) bool { return headers[i].Name < headers[j].Name })
			}
			var body any
			_ = json.Unmarshal(rec.BodyJSON, &body)
			value := map[string]any{
				"kind": rec.Kind, "method": rec.Method, "target": rec.Target,
				"proto": rec.Proto, "headers": headers, "body_encoding": rec.BodyEncoding,
				"body_key_order": bodyTopLevelKeys(rec.BodyJSON),
				"body":           normalizer.value("", body), "body_text": normalizer.value("", rec.BodyText),
			}
			if key == "responses_lifecycle" {
				if connections[rec.Conn] == 0 {
					connections[rec.Conn] = len(connections) + 1
				}
				value["connection"] = connections[rec.Conn]
			}
			if rec.Frame != nil {
				frame := *rec.Frame
				frame.WireLen = 0 // compression size changes with normalized IDs/clocks
				value["frame"] = frame
			}
			out[key] = append(out[key], value)
		}
	}
	return out
}

func tlsStructuresEqual(a, b any) bool {
	left, leftOK := a.([]any)
	right, rightOK := b.([]any)
	if !leftOK || !rightOK || len(left) == 0 || len(right) == 0 {
		return false
	}
	normalize := func(value any) string {
		rows, _ := value.([]any)
		unique := make(map[string]bool)
		for _, row := range rows {
			original, ok := row.(map[string]any)
			if !ok {
				return "invalid"
			}
			fields := make(map[string]any)
			for key, value := range original {
				if key != "raw_hex" && key != "observed_connections" && !strings.HasSuffix(key, "_hex") {
					fields[key] = value
				}
			}
			if fields["lane"] == "rustls" {
				if ext, ok := fields["extensions"].([]any); ok {
					ext = append([]any(nil), ext...)
					sort.Slice(ext, func(i, j int) bool { return ext[i].(float64) < ext[j].(float64) })
					fields["extensions"] = ext
				}
			}
			unique[string(mustJSON(fields))] = true
		}
		shapes := make([]string, 0, len(unique))
		for shape := range unique {
			shapes = append(shapes, shape)
		}
		sort.Strings(shapes)
		return strings.Join(shapes, "\n")
	}
	x, y := normalize(a), normalize(b)
	return x != "invalid" && y != "invalid" && x == y
}

func firstJSONDifference(a, b any) string {
	var left, right any
	_ = json.Unmarshal(mustJSON(a), &left)
	_ = json.Unmarshal(mustJSON(b), &right)
	var walk func(string, any, any) string
	walk = func(path string, a, b any) string {
		if reflect.DeepEqual(a, b) {
			return ""
		}
		switch av := a.(type) {
		case map[string]any:
			if bv, ok := b.(map[string]any); ok {
				keys := make([]string, 0, len(av)+len(bv))
				for k := range av {
					keys = append(keys, k)
				}
				for k := range bv {
					if _, exists := av[k]; !exists {
						keys = append(keys, k)
					}
				}
				sort.Strings(keys)
				for _, k := range keys {
					if diff := walk(path+"."+k, av[k], bv[k]); diff != "" {
						return diff
					}
				}
			}
		case []any:
			if bv, ok := b.([]any); ok && len(av) == len(bv) {
				for i := range av {
					if diff := walk(fmt.Sprintf("%s[%d]", path, i), av[i], bv[i]); diff != "" {
						return diff
					}
				}
			}
		}
		return fmt.Sprintf("%s: %.180s -> %.180s", path, mustJSON(a), mustJSON(b))
	}
	return walk("request", left, right)
}
