//go:build linux

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizePreservesExecutedToolSemanticMetadata(t *testing.T) {
	const beforeID = "01900000-1234-7123-8123-123456789abc"
	const afterID = "01900001-5678-7456-a456-abcdef123456"
	for _, tc := range []struct {
		name, container, key string
		before, after        any
	}{
		{"arguments duration", "arguments", "duration_ms", float64(12), float64(99)},
		{"arguments thread id", "arguments", "thread_id", beforeID, afterID},
		{"result metadata clock", "tool_result_metadata", "created_at", float64(12), float64(99)},
		{"result metadata turn id", "tool_result_metadata", "turn_id", beforeID, afterID},
		{"arguments nested envelope", "arguments", "internal_chat_message_metadata_passthrough", map[string]any{"create_time": 12}, map[string]any{"create_time": 99}},
		{"result dotted key", "tool_result_metadata", "trace.turn_id", beforeID, afterID},
		{"arguments arrays", "arguments", "items", []any{map[string]any{"duration_ms": 12}}, []any{map[string]any{"duration_ms": 99}}},
		{"result raw structured values", "tool_result_metadata", "x-codex-turn-metadata", `{"turn_id":"` + beforeID + `","turn_started_at_unix_ms":12}`, `{"turn_id":"` + afterID + `","turn_started_at_unix_ms":99}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := func(value any) string {
				b, err := json.Marshal(map[string]any{"input": []any{map[string]any{"type": "function_call_output", "call_id": "call_exact", "output": "same", "internal_chat_message_metadata_passthrough": map[string]any{"executed_tool_calls": []any{map[string]any{"name": "lookup", tc.container: map[string]any{tc.key: value}}}}}}})
				require.NoError(t, err)
				return string(b)
			}
			require.Positive(t, compareRecords(tc.name, comparisonRecord(body(tc.before)), comparisonRecord(body(tc.after)), false))
		})
	}
}

func TestNormalizeDoesNotInferTraceOwnershipFromNames(t *testing.T) {
	for _, body := range []string{
		`{"client_metadata":{"duration_ms":12}}`,
		`{"client_metadata":{"custom":{"duration_ms":12}}}`,
		`{"input":[{"internal_chat_message_metadata_passthrough":{"created_at":12}}]}`,
		`{"input":[{"internal_chat_message_metadata_passthrough":{"content_item_metadata":[{"create_time":12}]}}]}`,
		`{"other":{"internal_chat_message_metadata_passthrough":{"create_time":12}}}`,
		`{"input[].internal_chat_message_metadata_passthrough.create_time":12}`,
		`{"input":{"[]":{"internal_chat_message_metadata_passthrough":{"create_time":12}}}}`,
		`{"events":[{"event_params":{"custom":{"duration_ms":12}}}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			require.Positive(t, compareRecords("unowned clock", comparisonRecord(body), comparisonRecord(strings.ReplaceAll(body, "12", "99")), false))
		})
	}
}

func TestNormalizeShallowTraceClocksAndIDsRemainDynamic(t *testing.T) {
	for _, pair := range [][2]string{
		{`{"input":[{"internal_chat_message_metadata_passthrough":{"create_time":12}}]}`, `{"input":[{"internal_chat_message_metadata_passthrough":{"create_time":99}}]}`},
		{`{"client_metadata":{"x-codex-ws-stream-request-start-ms":"12"}}`, `{"client_metadata":{"x-codex-ws-stream-request-start-ms":"99"}}`},
		{`{"events":[{"event_params":{"duration_ms":12}}]}`, `{"events":[{"event_params":{"duration_ms":99}}]}`},
		{`{"input":[{"internal_chat_message_metadata_passthrough":{"turn_id":"01900000-1234-7123-8123-123456789abc"}}]}`, `{"input":[{"internal_chat_message_metadata_passthrough":{"turn_id":"01900001-5678-7456-a456-abcdef123456"}}]}`},
	} {
		require.Zero(t, compareRecords("known shallow tracing", comparisonRecord(pair[0]), comparisonRecord(pair[1]), false))
	}
}

func TestNormalizeEmbeddedTurnMetadataPreservesNestedSemanticValues(t *testing.T) {
	body := func(id string, clock int, semantic int) string {
		// Legal multi-line JSON, with escaping, encoded inside an outer JSON string.
		embedded := "{\n\"turn_id\": " + string(mustJSON(id)) + ",\n\"turn_started_at_unix_ms\": " + string(mustJSON(clock)) + ",\n\"custom\": {\"duration_ms\": " + string(mustJSON(semantic)) + ", \"literal\": \"quoted \\\"turn_id\\\"\"}\n}"
		return string(mustJSON(map[string]any{"client_metadata": map[string]any{"x-codex-turn-metadata": embedded}}))
	}
	first := body("01900000-1234-7123-8123-123456789abc", 12, 12)
	normal := body("01900001-5678-7456-a456-abcdef123456", 99, 12)
	require.Zero(t, compareRecords("valid multiline metadata", comparisonRecord(first), comparisonRecord(normal), false))
	require.Positive(t, compareRecords("nested semantic metadata", comparisonRecord(first), comparisonRecord(body("01900001-5678-7456-a456-abcdef123456", 99, 99)), false))
}
