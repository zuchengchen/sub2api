package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// This synthetic trace follows the observed structure, not its private values:
// two items, five argument deltas, metadata at ordinal 11 / sequence 10, then
// arguments.done, item.done and completed. No captured text or opaque is used.
func nativeReplayMetadataFrames(t *testing.T, kind, shape string, numbered bool) (NativeReasoningReplayLimits, [][]byte, []json.RawMessage) {
	t.Helper()
	engine, _, _, original, output := sparseReplayFixture(t)
	items := []json.RawMessage{output[0], output[2]}
	var tool map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(items[1], &tool))
	frames := [][]byte{
		original[0],
		[]byte(`{"type":"response.in_progress","response":{"id":"resp_synthetic","status":"in_progress","output":[]}}`),
		sparseReplayFrame("response.output_item.added", 0, items[0]),
		sparseReplayFrame("response.output_item.done", 0, items[0]),
		sparseReplayFrame("response.output_item.added", 1, items[1]),
	}
	for i := 0; i < 5; i++ {
		frame, err := json.Marshal(map[string]json.RawMessage{"type": json.RawMessage(`"response.function_call_arguments.delta"`), "output_index": json.RawMessage(`1`), "item_id": tool["id"], "delta": json.RawMessage(`"synthetic fragment"`)})
		require.NoError(t, err)
		frames = append(frames, frame)
	}
	frames = append(frames, []byte(fmt.Sprintf(`{"type":%q,"response_id":"resp_synthetic","metadata":{"synthetic":true}}`, kind)))
	argsDone, err := json.Marshal(map[string]json.RawMessage{"type": json.RawMessage(`"response.function_call_arguments.done"`), "output_index": json.RawMessage(`1`), "item_id": tool["id"], "arguments": tool["arguments"]})
	require.NoError(t, err)
	frames = append(frames, argsDone, sparseReplayFrame("response.output_item.done", 1, items[1]))
	terminal := sparseReplayEdit(t, original[len(original)-1], func(event map[string]json.RawMessage) {
		event["response"] = sparseReplayEdit(t, event["response"], func(response map[string]json.RawMessage) {
			switch shape {
			case "full":
				response["output"], err = json.Marshal(items)
				require.NoError(t, err)
			case "absent":
				delete(response, "output")
			}
		})
	})
	frames = append(frames, terminal)
	if numbered {
		for i := range frames {
			index := i
			frames[i] = sparseReplayEdit(t, frames[i], func(event map[string]json.RawMessage) { event["sequence_number"] = json.RawMessage(fmt.Sprint(index)) })
		}
	}
	return engine.limits, frames, items
}

func TestNativeReasoningReplayMetadataKnownNotificationsKeepCompleteItems(t *testing.T) {
	for _, kind := range []string{"response.metadata", "codex.response.metadata", "responsesapi.websocket_timing"} {
		for _, shape := range []string{"full", "empty", "absent"} {
			for _, numbered := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/numbered_%v", kind, shape, numbered), func(t *testing.T) {
					limits, frames, items := nativeReplayMetadataFrames(t, kind, shape, numbered)
					require.Len(t, frames, 14)
					var wire bytes.Buffer
					for _, frame := range frames {
						wire.WriteString("data: ")
						wire.Write(frame)
						wire.WriteString("\n\n")
					}
					capture := NewNativeReasoningResponseCapture(&nativeCaptureFragmentReader{reader: bytes.NewReader(wire.Bytes()), size: 23}, limits)
					actual, err := io.ReadAll(capture)
					require.NoError(t, err)
					require.Equal(t, wire.Bytes(), actual)
					require.NoError(t, capture.Close())
					response, ok := capture.CompletedResponse()
					require.True(t, ok)
					var complete struct {
						Output []json.RawMessage `json:"output"`
					}
					require.NoError(t, json.Unmarshal(response, &complete))
					expected := append([]json.RawMessage(nil), items...)
					if shape == "full" {
						// The unchanged full-terminal path retains canonical items.
						for i := range expected {
							expected[i], err = nativeReplayCanonicalObject(expected[i])
							require.NoError(t, err)
						}
					}
					require.Equal(t, expected, complete.Output)
					require.Contains(t, string(response), `9007199254740993`)
				})
			}
		}
	}
}

func TestNativeReasoningReplayMetadataHeadersOnlyEnvelopeIsNotOutput(t *testing.T) {
	for _, kind := range []string{"response.metadata", "codex.response.metadata", "responsesapi.websocket_timing"} {
		t.Run(kind, func(t *testing.T) {
			limits, frames, items := nativeReplayMetadataFrames(t, kind, "empty", true)
			frames[10] = sparseReplayEdit(t, frames[10], func(event map[string]json.RawMessage) {
				event["response"] = json.RawMessage(`{"id":"resp_synthetic","headers":{"x-synthetic":"value"}}`)
				// Metadata payload keys are not output declarations. Their values
				// cannot replace, append or change any completed native item.
				event["metadata"] = json.RawMessage(`{"output":"not an item declaration","items":17}`)
			})
			response, ok := sparseReplayCollect(frames, limits)
			require.True(t, ok)
			var complete struct {
				Output []json.RawMessage `json:"output"`
			}
			require.NoError(t, json.Unmarshal(response, &complete))
			require.Equal(t, items, complete.Output)
			require.NotContains(t, string(response), "not an item declaration")
		})
	}
}

func TestNativeReasoningReplayMetadataDeclarationsErrorsAndConflictsStayIneligible(t *testing.T) {
	faults := []string{"foreign_response", "foreign_nested_response", "root_error", "nested_error", "root_incomplete", "nested_incomplete", "unknown_kind", "sequence_gap", "sequence_absent", "no_anchor", "after_terminal"}
	for _, scope := range []string{"root", "response"} {
		for _, field := range []string{"item", "output", "output_items", "items", "output_index", "item_id", "call_id", "content_index", "summary_index"} {
			faults = append(faults, scope+"_"+field)
		}
		for _, status := range []string{"failed", "incomplete", "cancelled", "canceled", "unknown", "null"} {
			faults = append(faults, scope+"_status_"+status)
		}
	}
	for _, fault := range faults {
		t.Run(fault, func(t *testing.T) {
			limits, frames, _ := nativeReplayMetadataFrames(t, "response.metadata", "empty", true)
			frames[10] = sparseReplayEdit(t, frames[10], func(event map[string]json.RawMessage) {
				switch fault {
				case "foreign_response":
					event["response_id"] = json.RawMessage(`"resp_other"`)
				case "foreign_nested_response":
					event["response"] = json.RawMessage(`{"id":"resp_other","headers":{}}`)
				case "root_error":
					event["error"] = json.RawMessage(`{"code":"permission_denied"}`)
				case "nested_error":
					event["response"] = json.RawMessage(`{"id":"resp_synthetic","error":{"code":"permission_denied"}}`)
				case "root_incomplete":
					event["incomplete_details"] = json.RawMessage(`{}`)
				case "nested_incomplete":
					event["response"] = json.RawMessage(`{"id":"resp_synthetic","incomplete_details":{}}`)
				case "unknown_kind":
					event["type"] = json.RawMessage(`"response.future_metadata"`)
				case "sequence_gap":
					event["sequence_number"] = json.RawMessage(`11`)
				case "sequence_absent":
					delete(event, "sequence_number")
				default:
					for _, field := range []string{"item", "output", "output_items", "items", "output_index", "item_id", "call_id", "content_index", "summary_index"} {
						if fault == "root_"+field {
							event[field] = json.RawMessage(`null`)
						} else if fault == "response_"+field {
							event["response"] = json.RawMessage(fmt.Sprintf(`{"id":"resp_synthetic",%q:null}`, field))
						}
					}
					for _, status := range []string{"failed", "incomplete", "cancelled", "canceled", "unknown", "null"} {
						value := json.RawMessage(fmt.Sprintf("%q", status))
						if status == "null" {
							value = json.RawMessage(`null`)
						}
						if fault == "root_status_"+status {
							event["status"] = value
						} else if fault == "response_status_"+status {
							event["response"] = json.RawMessage(fmt.Sprintf(`{"id":"resp_synthetic","status":%s}`, value))
						}
					}
				}
			})
			if fault == "no_anchor" {
				frames = append([][]byte{frames[10]}, frames...)
			} else if fault == "after_terminal" {
				frames = append(frames, frames[10])
			}
			_, ok := sparseReplayCollect(frames, limits)
			require.False(t, ok)
		})
	}
}
