//go:build linux

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizePreservesLargeSemanticNumbers(t *testing.T) {
	for _, tc := range []struct{ name, before, after string }{
		{"config integer above float precision", `{"config":{"limit":9007199254740992}}`, `{"config":{"limit":9007199254740993}}`},
		{"negative large integer", `{"config":{"offset":-9007199254740992}}`, `{"config":{"offset":-9007199254740993}}`},
		{"raw tool arguments", `{"input":[{"type":"function_call_output","internal_chat_message_metadata_passthrough":{"executed_tool_calls":[{"name":"lookup","arguments":{"duration_ms":9007199254740992}}]}}]}`, `{"input":[{"type":"function_call_output","internal_chat_message_metadata_passthrough":{"executed_tool_calls":[{"name":"lookup","arguments":{"duration_ms":9007199254740993}}]}}]}`},
		{"raw result uint64 limit", `{"input":[{"type":"function_call_output","internal_chat_message_metadata_passthrough":{"executed_tool_calls":[{"name":"lookup","tool_result_metadata":{"created_at":18446744073709551614}}]}}]}`, `{"input":[{"type":"function_call_output","internal_chat_message_metadata_passthrough":{"executed_tool_calls":[{"name":"lookup","tool_result_metadata":{"created_at":18446744073709551615}}]}}]}`},
		{"tool schema enum", `{"tools":[{"parameters":{"enum":[9007199254740992]}}]}`, `{"tools":[{"parameters":{"enum":[9007199254740993]}}]}`},
		{"fraction exact precision", `{"config":{"threshold":0.12345678901234567890}}`, `{"config":{"threshold":0.12345678901234567891}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.True(t, json.Valid([]byte(tc.before)))
			require.True(t, json.Valid([]byte(tc.after)))
			require.Positive(t, compareRecords(tc.name, comparisonRecord(tc.before), comparisonRecord(tc.after), false))
			require.Zero(t, compareRecords("exact same numeric payload", comparisonRecord(tc.before), comparisonRecord(tc.before), false))
		})
	}
}
func TestNormalizeEmbeddedMetadataPreservesLargeSemanticNumbers(t *testing.T) {
	for _, header := range []bool{false, true} {
		t.Run(map[bool]string{false: "client_metadata_string", true: "header_metadata_string"}[header], func(t *testing.T) {
			records := func(value string) map[recordKey]record {
				embedded := "{\n\"turn_started_at_unix_ms\":9007199254741993,\n\"custom\":{\"limit\":" + value + ",\"literal\":\"escaped \\\"created_at\\\"\"}\n}"
				if !header {
					return comparisonRecord(string(mustJSON(map[string]any{"client_metadata": map[string]any{"x-codex-turn-metadata": embedded}})))
				}
				rows := comparisonRecord(`{}`)
				key := recordKey{Conn: 1, Seq: 1}
				row := rows[key]
				row.Headers = []headerKV{{Name: "x-codex-turn-metadata", Value: embedded}}
				rows[key] = row
				return rows
			}
			require.Positive(t, compareRecords("embedded large integer", records("9007199254740992"), records("9007199254740993"), false))
			require.Zero(t, compareRecords("exact embedded value", records("9007199254740993"), records("9007199254740993"), false))
		})
	}
}
func TestNormalizeKnownNumericClocksRemainDynamicWithoutChangingStringTypes(t *testing.T) {
	for _, body := range []string{
		`{"input":[{"internal_chat_message_metadata_passthrough":{"create_time":9007199254740992}}]}`,
		`{"events":[{"event_params":{"duration_ms":9007199254740992}}]}`,
		`{"client_metadata":{"x-codex-ws-stream-request-start-ms":9007199254740992}}`,
		string(mustJSON(map[string]any{"client_metadata": map[string]any{"x-codex-turn-metadata": `{"turn_started_at_unix_ms":9007199254740992}`}})),
	} {
		t.Run(body, func(t *testing.T) {
			require.Zero(t, compareRecords("known clock", comparisonRecord(body), comparisonRecord(strings.ReplaceAll(body, "9007199254740992", "9007199254740993")), false))
		})
	}
	require.Positive(t, compareRecords("number is not a numeric string", comparisonRecord(`{"config":{"limit":9007199254740993}}`), comparisonRecord(`{"config":{"limit":"9007199254740993"}}`), false))
	require.Positive(t, compareRecords("numeric strings remain semantic", comparisonRecord(`{"config":{"limit":"9007199254740992"}}`), comparisonRecord(`{"config":{"limit":"9007199254740993"}}`), false))
	require.Positive(t, compareRecords("clock preserves JSON scalar type", comparisonRecord(`{"input":[{"internal_chat_message_metadata_passthrough":{"create_time":9007199254740993}}]}`), comparisonRecord(`{"input":[{"internal_chat_message_metadata_passthrough":{"create_time":"9007199254740993"}}]}`), false))
}
func TestFirstJSONDifferenceReportsExactLargeNumbers(t *testing.T) {
	before := map[string]any{"limit": json.Number("9007199254740992")}
	after := map[string]any{"limit": json.Number("9007199254740993")}
	difference := firstJSONDifference(before, after)
	require.Contains(t, difference, "request.limit")
	require.Contains(t, difference, "9007199254740992")
	require.Contains(t, difference, "9007199254740993")
}
