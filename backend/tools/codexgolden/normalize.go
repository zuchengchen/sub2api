//go:build linux

package main

import (
	"bytes"
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
var toolExecutionPrefix = regexp.MustCompile(`^Chunk ID: [0-9a-f]+\nWall time: [0-9.]+ seconds\nProcess exited with code [0-9]+\n(?:Original token count: [0-9]+\n)?Output:\n`)

type referenceNormalizer struct{ ids map[string]string }

func (n *referenceNormalizer) value(path string, value any) any {
	key := path[strings.LastIndex(path, ".")+1:]
	metadata := strings.HasPrefix(path, "body.client_metadata.") || strings.Contains(path, ".internal_chat_message_metadata_passthrough.") || strings.HasPrefix(path, "body.events[].event_params.") || strings.HasPrefix(path, "header.x-codex-turn-metadata.")
	// Preserve field presence and type; only clocks and durations are volatile.
	volatile := false
	switch key {
	case "created_at", "started_at", "completed_at", "create_time", "turn_started_at_unix_ms",
		"x-codex-ws-stream-request-start-ms", "started_at_ms", "completed_at_ms", "duration_ms",
		"execution_duration_ms", "before_first_sampling_ms", "sampling_ms", "compaction_ms",
		"between_sampling_overhead_ms", "tool_blocking_ms", "after_last_sampling_ms":
		volatile = true
	}
	if value != nil && volatile && (metadata || path == "header.x-codex-ws-stream-request-start-ms") {
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
			out[k] = n.value(path+"."+k, v[k])
		}
		// Only the recorder's known exec_command wrapper contains clocks. The
		// tool's actual output, including UUIDs or lookalike text, stays exact.
		if path == "body.input[]" && v["type"] == "function_call_output" {
			callID, _ := v["call_id"].(string)
			output, _ := v["output"].(string)
			if strings.HasPrefix(callID, "call_golden_") {
				prefix := toolExecutionPrefix.FindString(output)
				if prefix != "" {
					normalized := dynamicChunk.ReplaceAllString(prefix, "Chunk ID: <CHUNK>")
					normalized = dynamicWallTime.ReplaceAllString(normalized, "Wall time: <SECONDS> seconds")
					out["output"] = normalized + output[len(prefix):]
				}
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = n.value(path+"[]", child)
		}
		return out
	case string:
		if path == "header.sec-websocket-key" || path == "header.content-length" || path == "header.date" {
			return "<DYNAMIC>"
		}
		// Turn metadata is a typed Rust struct encoded inside a string. Keep
		// its field order, unlike client_metadata's randomized HashMap order.
		if (metadata || strings.HasPrefix(path, "header.")) && key == "x-codex-turn-metadata" {
			var embedded map[string]any
			if json.Unmarshal([]byte(v), &embedded) == nil && embedded != nil {
				var out bytes.Buffer
				out.WriteByte('{')
				for i, k := range bodyTopLevelKeys([]byte(v)) {
					if i > 0 {
						out.WriteByte(',')
					}
					out.Write(mustJSON(k))
					out.WriteByte(':')
					out.Write(mustJSON(n.value(path+"."+k, embedded[k])))
				}
				out.WriteByte('}')
				return out.String()
			}
		}
		identity := path == "body.prompt_cache_key" || path == "body.input[].id"
		if metadata || strings.HasPrefix(path, "header.") {
			switch key {
			case "session-id", "thread-id", "x-client-request-id", "x-codex-window-id", "x-codex-installation-id",
				"session_id", "thread_id", "parent_thread_id", "turn_id", "root_turn_id", "installation_id", "window_id", "context_window_id":
				identity = true
			}
		}
		if !identity {
			return v
		}
		return dynamicUUID.ReplaceAllStringFunc(v, func(id string) string {
			if n.ids == nil {
				n.ids = make(map[string]string)
			}
			if n.ids[id] == "" {
				variant := "other"
				switch strings.ToLower(id[19:20]) {
				case "0", "1", "2", "3", "4", "5", "6", "7":
					variant = "ncs"
				case "8", "9", "a", "b":
					variant = "rfc4122"
				case "c", "d":
					variant = "microsoft"
				}
				letterCase := "mixed"
				if id == strings.ToLower(id) {
					letterCase = "lower"
				} else if id == strings.ToUpper(id) {
					letterCase = "upper"
				}
				n.ids[id] = fmt.Sprintf("<ID:%d:v%s:%s:%s>", len(n.ids)+1, id[14:15], variant, letterCase)
			}
			return n.ids[id]
		})
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
	connections := make(map[int]int)
	for _, key := range keys {
		recs := groups[key]
		sort.Slice(recs, func(i, j int) bool { return recs[i].Seq < recs[j].Seq })
		// The fixed CLI starts two independent model clients: bootstrap and
		// initialized. Each makes its first request on a separate connection;
		// scheduling can reverse their arrival even at the same endpoint. Sort
		// only that pair by its actual headers before assigning connection IDs.
		// Counts, header values/order, TLS and later connection reuse remain
		// observable. Retries and requests sharing a connection stay ordered.
		if len(recs) == 2 && recs[0].Kind == "http_request" && recs[0].Method == "GET" &&
			strings.HasPrefix(recs[0].Target, "/backend-api/codex/models?client_version=") &&
			recs[0].Conn != recs[1].Conn && recs[0].ConnReq == 1 && recs[1].ConnReq == 1 {
			sort.Slice(recs, func(i, j int) bool {
				return string(mustJSON(recs[i].Headers)) < string(mustJSON(recs[j].Headers))
			})
		}
		for _, rec := range recs {
			headers := make([]headerKV, 0, len(rec.Headers))
			for _, header := range rec.Headers {
				header.Value = normalizer.value("header."+strings.ToLower(header.Name), header.Value).(string)
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
				"body":           normalizer.value("body", body), "body_text": rec.BodyText,
			}
			if connections[rec.Conn] == 0 {
				connections[rec.Conn] = len(connections) + 1
			}
			value["connection"] = connections[rec.Conn]
			value["connection_request_index"] = rec.ConnReq
			if rec.TLS != nil {
				value["tls"] = rec.TLS.fingerprintKey()
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
	if validateTLSRecording(a) != nil || validateTLSRecording(b) != nil {
		return false
	}
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
					ordered := make([]float64, 0, len(ext))
					for _, value := range ext {
						number, ok := value.(float64)
						if !ok {
							return "invalid"
						}
						ordered = append(ordered, number)
					}
					sort.Float64s(ordered)
					fields["extensions"] = ordered
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
