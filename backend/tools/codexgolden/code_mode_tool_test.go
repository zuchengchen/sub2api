//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

func syntheticGoldenCodeModeOutput(chunk, clock, output string, exit int) []any {
	return []any{
		map[string]any{"type": "input_text", "text": "Script completed\nWall time " + clock + " seconds\nOutput:\n"},
		map[string]any{"type": "input_text", "text": string(mustJSON(map[string]any{"chunk_id": chunk, "wall_time_seconds": json.Number(clock), "exit_code": exit, "output": output}))},
	}
}

func TestGoldenDeclaredToolUsesActualNamespaceAndCapability(t *testing.T) {
	for _, tc := range []struct {
		name, body, kind, namespace string
		valid                       bool
	}{
		{"direct", `{"tools":[{"type":"function","name":"exec_command","parameters":{"properties":{"cmd":{"type":"string"}}}}]}`, "function_call", "", true},
		{"source code mode", `{"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec","description":"Calls tools.exec_command"}]}]}]}`, "custom_tool_call", "functions", true},
		{"missing declaration", `{"input":[]}`, "", "", false},
		{"lookalike user text", `{"input":[{"role":"user","tools":[{"type":"custom","name":"exec","description":"exec_command"}]}]}`, "", "", false},
		{"different nested capability", `{"tools":[{"type":"custom","name":"exec","description":"Only tools.image"}]}`, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call, ok := goldenDeclaredToolCall([]byte(tc.body), "resp_fixture")
			require.Equal(t, tc.valid, ok)
			if ok {
				require.Equal(t, tc.kind, call["type"])
				if tc.namespace != "" {
					require.Equal(t, tc.namespace, call["namespace"])
				}
			}
		})
	}
}

func TestGoldenCodeModeExecutionAndNormalizationCalibration(t *testing.T) {
	base := syntheticGoldenCodeModeOutput("abc123", "0.1", "golden\n", 0)
	require.True(t, successfulGoldenCodeModeOutput(mustJSON(base)))
	body := func(value any, id string) string {
		return string(mustJSON(map[string]any{"input": []any{map[string]any{"type": "custom_tool_call_output", "call_id": id, "output": value}}}))
	}
	baseBody := body(base, "call_golden_code_mode_1")
	fresh := syntheticGoldenCodeModeOutput("def456", "2.9", "golden\n", 0)
	require.Zero(t, compareRecords("fresh source wrapper clocks", comparisonRecord(baseBody), comparisonRecord(body(fresh, "call_golden_code_mode_1")), false))
	for _, fault := range []string{"exit", "output", "missing result", "session", "malformed", "spoofed success", "extra content", "clock type", "unrelated metadata"} {
		t.Run(fault, func(t *testing.T) {
			var value []any
			require.NoError(t, unmarshalReferenceJSON(mustJSON(base), &value))
			var result map[string]any
			require.NoError(t, unmarshalReferenceJSON([]byte(value[1].(map[string]any)["text"].(string)), &result))
			switch fault {
			case "exit":
				result["exit_code"] = 1
			case "output":
				result["output"] = "wrong\n"
			case "session":
				result["session_id"] = "still-running"
			case "clock type":
				result["wall_time_seconds"] = "0.1"
			case "unrelated metadata":
				result["custom"] = map[string]any{"chunk_id": "semantic"}
			}
			value[1].(map[string]any)["text"] = string(mustJSON(result))
			switch fault {
			case "missing result":
				value = value[:1]
			case "malformed":
				value[1].(map[string]any)["text"] = "{"
			case "spoofed success":
				value[0].(map[string]any)["text"] = "Script failed\nScript completed\nWall time 0.1 seconds\nOutput:\n"
			case "extra content":
				value = append(value, map[string]any{"type": "input_text", "text": "unexpected"})
			}
			if fault != "unrelated metadata" {
				require.False(t, successfulGoldenCodeModeOutput(mustJSON(value)))
			}
			require.Positive(t, compareRecords(fault, comparisonRecord(baseBody), comparisonRecord(body(value, "call_golden_code_mode_1")), false))
		})
	}
	// A real tool's text and a fixture-shaped user collision remain semantic.
	require.Positive(t, compareRecords("unowned collision", comparisonRecord(body(base, "user_call")), comparisonRecord(body(fresh, "user_call")), false))
	changed := body(syntheticGoldenCodeModeOutput("abc123", "0.1", "golden changed\n", 0), "call_golden_code_mode_1")
	require.NotEqual(t, baseBody, changed)
	require.Positive(t, compareRecords("real output changed", comparisonRecord(baseBody), comparisonRecord(changed), false))
	recs := []record{{Kind: "ws_message", BodyJSON: json.RawMessage(`{"generate":false}`)}, {Kind: "ws_message", BodyJSON: json.RawMessage(baseBody)}}
	require.NoError(t, validateScenario(scenario{ToolLoop: true}, recs))
	recs[1].BodyJSON = json.RawMessage(body(syntheticGoldenCodeModeOutput("def456", "0.1", "golden\n", 1), "call_golden_code_mode_1"))
	require.Error(t, validateScenario(scenario{ToolLoop: true}, recs))
}

func TestGoldenScriptIncludesNativeReasoningAndCompleteTerminal(t *testing.T) {
	upstream := &fakeUpstream{scenario: scenario{ToolLoop: true}}
	body := []byte(`{"tools":[{"type":"custom","name":"exec","description":"Use tools.exec_command"}],"input":[]}`)
	events := upstream.scriptedEvents(body)
	require.GreaterOrEqual(t, len(events), 6)
	terminal := events[len(events)-1]
	require.Equal(t, "response.completed", terminal["type"])
	response := terminal["response"].(map[string]any)
	require.Equal(t, "completed", response["status"])
	output := response["output"].([]any)
	require.Len(t, output, 2)
	require.Equal(t, "reasoning", output[0].(map[string]any)["type"])
	require.NotEmpty(t, output[0].(map[string]any)["encrypted_content"])
	require.Equal(t, "custom_tool_call", output[1].(map[string]any)["type"])
	seen := 0
	for _, event := range events {
		if event["type"] == "response.output_item.done" {
			require.Equal(t, output[event["output_index"].(int)], event["item"])
			seen++
		}
	}
	require.Equal(t, len(output), seen)
	upstream = &fakeUpstream{}
	events = upstream.scriptedEvents([]byte(`{"input":[]}`))
	output = events[len(events)-1]["response"].(map[string]any)["output"].([]any)
	require.Equal(t, "final_answer", output[1].(map[string]any)["phase"])
	require.NotEmpty(t, output[1].(map[string]any)["encrypted_content"])
}

func TestGoldenSourceNativeStateRequiresActualContinuation(t *testing.T) {
	rows := nativeTestLifecycle(false)
	require.NoError(t, validateScenario(scenario{ToolLoop: true, NativeState: true}, rows))
	rows[1].NativeResponse.Complete = false
	require.Error(t, validateScenario(scenario{ToolLoop: true, NativeState: true}, rows))
}

func TestGoldenCodeModeFullComparisonExitCalibration(t *testing.T) {
	base, candidate := t.TempDir(), t.TempDir()
	comparatorFixture(t, base)
	comparatorFixture(t, candidate)
	inject := func(dir string, value []any) {
		path := filepath.Join(dir, "ws_initial.jsonl")
		rows, err := readRecordMap(path)
		require.NoError(t, err)
		ordered := make([]record, 0, len(rows))
		for _, row := range rows {
			ordered = append(ordered, row)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].Seq < ordered[j].Seq })
		injected := false
		for i := range ordered {
			if ordered[i].Kind != "ws_message" || injected {
				continue
			}
			var body map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(ordered[i].BodyJSON, &body))
			var input []json.RawMessage
			require.NoError(t, json.Unmarshal(body["input"], &input))
			// Replace only our prior fixture injection if this is a mutation run.
			kept := input[:0]
			for _, item := range input {
				var header struct {
					CallID string `json:"call_id"`
				}
				require.NoError(t, json.Unmarshal(item, &header))
				if header.CallID != "call_golden_code_mode_calibration" {
					kept = append(kept, item)
				}
			}
			input = append(kept, mustJSON(map[string]any{"type": "custom_tool_call_output", "call_id": "call_golden_code_mode_calibration", "output": value}))
			ordered[i].BodyJSON = bytes.Replace(ordered[i].BodyJSON, body["input"], mustJSON(input), 1)
			injected = true
		}
		require.True(t, injected)
		writeJSONL(path, ordered)
	}
	inject(base, syntheticGoldenCodeModeOutput("aabbcc", "0.1", "golden\n", 0))
	inject(candidate, syntheticGoldenCodeModeOutput("ccddee", "0.7", "golden\n", 0))
	require.Equal(t, 0, compareMain([]string{"-golden", base, "-candidate", candidate}))
	inject(candidate, syntheticGoldenCodeModeOutput("ccddee", "0.7", "changed output\n", 0))
	require.Equal(t, 1, compareMain([]string{"-golden", base, "-candidate", candidate}), "a real command-output difference must not normalize away")
	require.NoError(t, os.WriteFile(filepath.Join(candidate, "ws_initial.jsonl"), []byte("{broken}\n"), 0600))
	require.Equal(t, 2, compareMain([]string{"-golden", base, "-candidate", candidate}), "malformed evidence must not become a product match")
}

func TestGoldenCodeModeChunkIdentityRelationshipsRemainObservable(t *testing.T) {
	body := func(ids ...string) string {
		items := []any{}
		for _, id := range ids {
			items = append(items, map[string]any{"type": "custom_tool_call_output", "call_id": "call_golden_code_mode_1", "output": syntheticGoldenCodeModeOutput(id, "0.1", "golden\n", 0)})
		}
		return string(mustJSON(map[string]any{"input": items}))
	}
	require.Zero(t, compareRecords("same identity relationship", comparisonRecord(body("aabbcc", "aabbcc")), comparisonRecord(body("ddeeff", "ddeeff")), false))
	require.Positive(t, compareRecords("lost shared identity", comparisonRecord(body("aabbcc", "aabbcc")), comparisonRecord(body("ddeeff", "112233")), false))
	require.Positive(t, compareRecords("collapsed distinct identities", comparisonRecord(body("aabbcc", "112233")), comparisonRecord(body("ddeeff", "ddeeff")), false))
	require.Positive(t, compareRecords("truncated identity format", comparisonRecord(body("aabbcc")), comparisonRecord(body("aabb")), false))
}
