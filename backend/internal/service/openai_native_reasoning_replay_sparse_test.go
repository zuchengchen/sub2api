package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func sparseReplayFrame(kind string, index int, item json.RawMessage) []byte {
	if item == nil {
		return []byte(fmt.Sprintf(`{"type":%q}`, kind))
	}
	return append(append([]byte(fmt.Sprintf(`{"type":%q,"output_index":%d,"item":`, kind, index)), item...), '}')
}

func sparseReplayFixture(t *testing.T) (*NativeReasoningReplay, NativeReasoningReplayRequest, NativeReasoningReplayCompletion, [][]byte, []json.RawMessage) {
	t.Helper()
	engine, _, request, completion := nativeReplayFixture(t)
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(completion.Response, &envelope))
	var items []json.RawMessage
	require.NoError(t, json.Unmarshal(envelope["output"], &items))
	envelope["output"] = json.RawMessage(`[]`)
	envelope["usage"] = json.RawMessage(`{"input_tokens":17,"output_tokens":5,"extra":{"n":9007199254740993}}`)
	envelope["future_terminal"] = json.RawMessage(`{"sequence":9007199254740993,"preserve":null}`)
	terminal, err := json.Marshal(envelope)
	require.NoError(t, err)
	frames := [][]byte{[]byte(`{"type":"response.created","response":{"id":"resp_synthetic","status":"in_progress","output":[]}}`)}
	for i, item := range items {
		frames = append(frames, sparseReplayFrame("response.output_item.added", i, item), sparseReplayFrame("response.output_item.done", i, item))
	}
	frames = append(frames, append(append([]byte(`{"type":"response.completed","response":`), terminal...), '}'))
	return engine, request, completion, frames, items
}

func sparseReplayEdit(t *testing.T, raw []byte, edit func(map[string]json.RawMessage)) []byte {
	t.Helper()
	var value map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &value))
	edit(value)
	result, err := json.Marshal(value)
	require.NoError(t, err)
	return result
}

func sparseReplayCollect(frames [][]byte, limits NativeReasoningReplayLimits) (json.RawMessage, bool) {
	c := NewNativeReasoningReplayCollector(limits)
	for _, frame := range frames {
		c.Observe(frame, "")
	}
	return c.CompletedResponse()
}

func TestNativeReasoningReplaySparseTerminalRestoresExactNativeBlock(t *testing.T) {
	for _, shape := range []string{"empty", "absent", "numbered", "in_progress_anchor", "identical_duplicates"} {
		t.Run(shape, func(t *testing.T) {
			engine, request, completion, frames, items := sparseReplayFixture(t)
			switch shape {
			case "absent":
				frames[len(frames)-1] = sparseReplayEdit(t, frames[len(frames)-1], func(event map[string]json.RawMessage) {
					event["response"] = sparseReplayEdit(t, event["response"], func(response map[string]json.RawMessage) { delete(response, "output") })
				})
			case "numbered":
				for i := range frames {
					index := i
					frames[i] = sparseReplayEdit(t, frames[i], func(event map[string]json.RawMessage) { event["sequence_number"] = json.RawMessage(fmt.Sprint(index)) })
				}
			case "in_progress_anchor":
				frames[0] = bytes.Replace(frames[0], []byte("response.created"), []byte("response.in_progress"), 1)
			case "identical_duplicates":
				frames = append(frames[:3], append([][]byte{frames[1], frames[2]}, frames[3:]...)...)
			}
			rebuilt, ok := sparseReplayCollect(frames, engine.limits)
			require.True(t, ok)
			var response map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(rebuilt, &response))
			require.JSONEq(t, `{"sequence":9007199254740993,"preserve":null}`, string(response["future_terminal"]))
			var output []json.RawMessage
			require.NoError(t, json.Unmarshal(response["output"], &output))
			require.Equal(t, items, output)
			completion.Response = rebuilt
			require.NoError(t, engine.Commit(context.Background(), request, completion))
			restored, err := engine.Restore(context.Background(), nativeReplayContinuation(request, completion))
			require.NoError(t, err)
			require.Len(t, restored, 1)
			for i := range items {
				require.JSONEq(t, string(items[i]), string(restored[0].Output[i]))
			}
		})
	}
}

func TestNativeReasoningReplaySparseRawDoneAndTerminalFieldsSurviveCapture(t *testing.T) {
	engine, _, _, frames, items := sparseReplayFixture(t)
	// Preserve exact raw item representation through the consumer; fields and
	// opaque strings remain untouched even if storage later encodes its envelope.
	items[0] = json.RawMessage(strings.ReplaceAll(string(items[0]), `"type":"reasoning"`, `"type" : "reasoning"`))
	frames[1], frames[2] = sparseReplayFrame("response.output_item.added", 0, items[0]), sparseReplayFrame("response.output_item.done", 0, items[0])
	var wire bytes.Buffer
	for _, frame := range frames {
		wire.WriteString("data: ")
		wire.Write(frame)
		wire.WriteString("\n\n")
	}
	for _, size := range []int{1, 19, wire.Len()} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			capture := NewNativeReasoningResponseCapture(&nativeCaptureFragmentReader{reader: bytes.NewReader(wire.Bytes()), size: size}, engine.limits)
			gotWire, err := io.ReadAll(capture)
			require.NoError(t, err)
			require.Equal(t, wire.Bytes(), gotWire)
			require.NoError(t, capture.Close())
			response, ok := capture.CompletedResponse()
			require.True(t, ok)
			var decoded struct {
				Output []json.RawMessage `json:"output"`
			}
			require.NoError(t, json.Unmarshal(response, &decoded))
			require.Equal(t, items[0], decoded.Output[0])
			require.Contains(t, string(decoded.Output[1]), `"phase":"commentary"`)
			require.Contains(t, string(decoded.Output[2]), `"encrypted_function_args":["opaque-synthetic-args"]`)
		})
	}
}

func TestNativeReasoningReplaySparseClosureFaultsStayIneligible(t *testing.T) {
	for _, fault := range []string{"no_anchor", "wrong_response", "foreign_response_id", "missing_done", "done_only", "gap", "done_reordered", "duplicate_id", "call_changed", "name_changed", "namespace_changed", "phase_changed", "failed_added", "failed_done", "unknown_item", "unknown_event", "dangling_delta", "cross_reference", "foreign_call", "duplicate_added_reference", "duplicate_done_reference", "null_output", "missing_status", "error_envelope", "incomplete", "missing_usage", "fractional_usage", "reasoning_only", "conflicting_done", "bad_utf8", "duplicate_json"} {
		t.Run(fault, func(t *testing.T) {
			engine, _, _, frames, items := sparseReplayFixture(t)
			editItem := func(position int, field string, value json.RawMessage) {
				frames[position] = sparseReplayEdit(t, frames[position], func(event map[string]json.RawMessage) {
					event["item"] = sparseReplayEdit(t, event["item"], func(item map[string]json.RawMessage) { item[field] = value })
				})
			}
			terminal := func(edit func(map[string]json.RawMessage)) {
				frames[len(frames)-1] = sparseReplayEdit(t, frames[len(frames)-1], func(event map[string]json.RawMessage) {
					event["response"] = sparseReplayEdit(t, event["response"], edit)
				})
			}
			switch fault {
			case "no_anchor":
				frames = frames[1:]
			case "wrong_response":
				terminal(func(m map[string]json.RawMessage) { m["id"] = json.RawMessage(`"resp_other"`) })
			case "foreign_response_id":
				frames[2] = sparseReplayEdit(t, frames[2], func(m map[string]json.RawMessage) { m["response_id"] = json.RawMessage(`"resp_other"`) })
			case "missing_done":
				frames = append(frames[:6], frames[7:]...)
			case "done_only":
				frames = append(frames[:1], frames[2:]...)
			case "gap":
				frames[5] = bytes.Replace(frames[5], []byte(`"output_index":2`), []byte(`"output_index":3`), 1)
			case "done_reordered":
				frames = [][]byte{frames[0], frames[1], frames[3], frames[5], frames[4], frames[2], frames[6], frames[7]}
			case "duplicate_id":
				editItem(3, "id", json.RawMessage(`"rs_synthetic"`))
			case "call_changed":
				editItem(6, "call_id", json.RawMessage(`"call_other"`))
			case "name_changed":
				editItem(6, "name", json.RawMessage(`"other"`))
			case "namespace_changed":
				editItem(6, "namespace", json.RawMessage(`"other"`))
			case "phase_changed":
				editItem(4, "phase", json.RawMessage(`"final_answer"`))
			case "failed_added":
				editItem(1, "status", json.RawMessage(`"failed"`))
			case "failed_done":
				editItem(2, "status", json.RawMessage(`"incomplete"`))
			case "unknown_item":
				editItem(5, "type", json.RawMessage(`"future_tool"`))
			case "unknown_event":
				frames = append(frames[:3], append([][]byte{[]byte(`{"type":"response.future_item_event","output_index":1}`)}, frames[3:]...)...)
			case "dangling_delta":
				frames = append(frames[:3], append([][]byte{[]byte(`{"type":"response.output_text.delta","output_index":3,"delta":"missing"}`)}, frames[3:]...)...)
			case "cross_reference":
				frames = append(frames[:4], append([][]byte{[]byte(`{"type":"response.output_text.delta","output_index":1,"item_id":"rs_synthetic","delta":"mismatch"}`)}, frames[4:]...)...)
			case "foreign_call":
				frames = append(frames[:6], append([][]byte{[]byte(`{"type":"response.function_call_arguments.delta","output_index":2,"call_id":"call_other","delta":"{}"}`)}, frames[6:]...)...)
			case "duplicate_added_reference", "duplicate_done_reference":
				position := 3
				if fault == "duplicate_done_reference" {
					position = 4
				}
				duplicate := sparseReplayEdit(t, frames[position], func(m map[string]json.RawMessage) { m["item_id"] = json.RawMessage(`"rs_synthetic"`) })
				frames = append(frames[:5], append([][]byte{duplicate}, frames[5:]...)...)
			case "null_output":
				terminal(func(m map[string]json.RawMessage) { m["output"] = json.RawMessage(`null`) })
			case "missing_status":
				terminal(func(m map[string]json.RawMessage) { delete(m, "status") })
			case "error_envelope":
				frames[2] = sparseReplayEdit(t, frames[2], func(m map[string]json.RawMessage) { m["error"] = json.RawMessage(`{"code":"unknown"}`) })
			case "incomplete":
				frames[len(frames)-1] = bytes.Replace(frames[len(frames)-1], []byte("response.completed"), []byte("response.incomplete"), 1)
			case "missing_usage":
				terminal(func(m map[string]json.RawMessage) { delete(m, "usage") })
			case "fractional_usage":
				terminal(func(m map[string]json.RawMessage) {
					m["usage"] = json.RawMessage(`{"input_tokens":1.5,"output_tokens":1}`)
				})
			case "reasoning_only":
				frames = append(frames[:3], frames[7:]...)
			case "conflicting_done":
				changed := json.RawMessage(strings.ReplaceAll(string(items[0]), "opaque-synthetic-reasoning", "changed"))
				frames = append(frames[:3], append([][]byte{sparseReplayFrame("response.output_item.done", 0, changed)}, frames[3:]...)...)
			case "bad_utf8":
				frames[2] = bytes.Replace(frames[2], []byte("opaque-synthetic-reasoning"), []byte{0xff}, 1)
			case "duplicate_json":
				frames[2] = bytes.Replace(frames[2], []byte(`"output_index":0`), []byte(`"output_index":0,"output_index":1`), 1)
			}
			_, ok := sparseReplayCollect(frames, engine.limits)
			require.False(t, ok)
		})
	}
}

func TestNativeReasoningReplaySparseSequenceChecksDoNotAffectFullTerminal(t *testing.T) {
	for _, fault := range []string{"gap", "missing", "boolean", "fractional", "negative", "overflow", "conflicting_repeat"} {
		t.Run(fault, func(t *testing.T) {
			engine, _, completion, frames, _ := sparseReplayFixture(t)
			for i := range frames {
				index := i
				frames[i] = sparseReplayEdit(t, frames[i], func(m map[string]json.RawMessage) { m["sequence_number"] = json.RawMessage(fmt.Sprint(index)) })
			}
			frames[2] = sparseReplayEdit(t, frames[2], func(m map[string]json.RawMessage) {
				switch fault {
				case "gap":
					m["sequence_number"] = json.RawMessage(`99`)
				case "missing":
					delete(m, "sequence_number")
				case "boolean":
					m["sequence_number"] = json.RawMessage(`true`)
				case "fractional":
					m["sequence_number"] = json.RawMessage(`2.5`)
				case "negative":
					m["sequence_number"] = json.RawMessage(`-1`)
				case "overflow":
					m["sequence_number"] = json.RawMessage(`9223372036854775808`)
				case "conflicting_repeat":
					m["sequence_number"] = json.RawMessage(`1`)
				}
			})
			_, ok := sparseReplayCollect(frames, engine.limits)
			require.False(t, ok)
			frames[len(frames)-1] = append(append([]byte(`{"type":"response.completed","response":`), completion.Response...), '}')
			_, ok = sparseReplayCollect(frames, engine.limits)
			require.True(t, ok, "a full authoritative output remains governed by the existing exact matching rules")
		})
	}
}

func TestNativeReasoningReplaySparseCaptureRequiresFullTerminalAndLimits(t *testing.T) {
	engine, _, _, frames, _ := sparseReplayFixture(t)
	var wire []byte
	for _, frame := range frames {
		wire = append(wire, append(append([]byte("data: "), frame...), '\n', '\n')...)
	}
	for _, cut := range []int{0, len(wire) - 2, len(wire) - 15} {
		capture := NewNativeReasoningResponseCapture(io.NopCloser(bytes.NewReader(wire[:cut])), engine.limits)
		_, _ = io.ReadAll(capture)
		_ = capture.Close()
		_, ok := capture.CompletedResponse()
		require.False(t, ok)
	}
	_, ok := sparseReplayCollect(frames, NativeReasoningReplayLimits{MaxEntryBytes: 512})
	require.False(t, ok)
	_, ok = sparseReplayCollect(frames, NativeReasoningReplayLimits{MaxOutputItems: 2})
	require.False(t, ok)
}

func TestNativeReasoningReplaySparseOptionalFieldsProgressAndReferenceOnlyEvents(t *testing.T) {
	engine, _, _, frames, _ := sparseReplayFixture(t)
	for _, position := range []int{1, 2} {
		frames[position] = sparseReplayEdit(t, frames[position], func(event map[string]json.RawMessage) {
			event["item"] = sparseReplayEdit(t, event["item"], func(item map[string]json.RawMessage) {
				item["name"], item["call_id"], item["encrypted_content"] = json.RawMessage(`null`), json.RawMessage(`""`), json.RawMessage(`null`)
			})
		})
	}
	progress := []byte(`{"type":"response.in_progress","response":{"id":"resp_synthetic","status":"in_progress","output":[]}}`)
	frames = append(frames[:3], append([][]byte{progress}, frames[3:]...)...)
	// index is optional only when an exact unique item/call reference gives the
	// same association; sub-indices never select an output item themselves.
	ref := []byte(`{"type":"response.function_call_arguments.done","item_id":"fc_synthetic","call_id":"call_synthetic","arguments":"{\"city\":\"fixture\"}"}`)
	frames = append(frames[:7], append([][]byte{ref}, frames[7:]...)...)
	result, ok := sparseReplayCollect(frames, engine.limits)
	require.True(t, ok, "message item still has opaque state; nullable reasoning extensions must survive")
	require.Contains(t, string(result), `"encrypted_content":null`)
	require.Contains(t, string(result), `"call_id":""`)
}

func TestNativeReasoningReplaySparseSequenceRetransmissionRequiresSameEffectiveType(t *testing.T) {
	engine, _, _, frames, _ := sparseReplayFixture(t)
	c := NewNativeReasoningReplayCollector(engine.limits)
	anchor := sparseReplayEdit(t, frames[0], func(event map[string]json.RawMessage) { event["sequence_number"] = json.RawMessage(`0`) })
	c.Observe(anchor, "")
	added := sparseReplayEdit(t, frames[1], func(event map[string]json.RawMessage) {
		delete(event, "type")
		event["sequence_number"] = json.RawMessage(`1`)
	})
	c.Observe(added, "response.output_item.added")
	c.Observe(added, "response.output_item.done")
	for i := 2; i < len(frames); i++ {
		sequence := i
		c.Observe(sparseReplayEdit(t, frames[i], func(event map[string]json.RawMessage) {
			event["sequence_number"] = json.RawMessage(fmt.Sprint(sequence))
		}), "")
	}
	_, ok := c.CompletedResponse()
	require.False(t, ok)
}

func TestNativeReasoningReplaySparseKnownUsageAndOutputReferenceFaults(t *testing.T) {
	for _, fault := range []string{"bad_total", "bad_cached", "cached_over_input", "bad_written", "bad_reasoning", "bad_details", "no_reference", "bad_subindex", "null_type", "extra_declaration", "item_error"} {
		t.Run(fault, func(t *testing.T) {
			engine, _, _, frames, _ := sparseReplayFixture(t)
			if strings.HasPrefix(fault, "bad_") && fault != "bad_subindex" || fault == "cached_over_input" {
				frames[len(frames)-1] = sparseReplayEdit(t, frames[len(frames)-1], func(event map[string]json.RawMessage) {
					event["response"] = sparseReplayEdit(t, event["response"], func(response map[string]json.RawMessage) {
						response["usage"] = sparseReplayEdit(t, response["usage"], func(usage map[string]json.RawMessage) {
							switch fault {
							case "bad_total":
								usage["total_tokens"] = json.RawMessage(`true`)
							case "bad_cached":
								usage["input_tokens_details"] = json.RawMessage(`{"cached_tokens":-1}`)
							case "cached_over_input":
								usage["input_tokens_details"] = json.RawMessage(`{"cached_tokens":18}`)
							case "bad_written":
								usage["input_tokens_details"] = json.RawMessage(`{"cache_write_tokens":"1"}`)
							case "bad_reasoning":
								usage["output_tokens_details"] = json.RawMessage(`{"reasoning_tokens":1.5}`)
							case "bad_details":
								usage["input_tokens_details"] = json.RawMessage(`[]`)
							}
						})
					})
				})
			} else {
				switch fault {
				case "null_type":
					frames[2] = sparseReplayEdit(t, frames[2], func(m map[string]json.RawMessage) { m["type"] = json.RawMessage(`null`) })
				case "extra_declaration":
					frames[2] = sparseReplayEdit(t, frames[2], func(m map[string]json.RawMessage) { m["output_items"] = json.RawMessage(`[{}]`) })
				case "item_error":
					frames[2] = sparseReplayEdit(t, frames[2], func(m map[string]json.RawMessage) {
						m["item"] = sparseReplayEdit(t, m["item"], func(it map[string]json.RawMessage) { it["error"] = json.RawMessage(`{"code":"unknown"}`) })
					})
				default:
					var delta []byte
					if fault == "no_reference" {
						delta = []byte(`{"type":"response.function_call_arguments.delta","delta":"{}"}`)
					} else {
						delta = []byte(`{"type":"response.function_call_arguments.delta","output_index":2,"content_index":-1,"delta":"{}"}`)
					}
					frames = append(frames[:6], append([][]byte{delta}, frames[6:]...)...)
				}
			}
			_, ok := sparseReplayCollect(frames, engine.limits)
			require.False(t, ok)
		})
	}
}

func TestNativeReasoningReplaySparseCaptureKeepsFullCanonicalSizeBoundary(t *testing.T) {
	_, _, completion, _, _ := sparseReplayFixture(t)
	response := bytes.Replace(completion.Response, []byte("Checking."), []byte(strings.Repeat("<", 2000)), 1)
	frame := append(append([]byte(`{"type":"response.completed","response":`), response...), '}')
	canonical, err := nativeReplayCanonicalObject(frame)
	require.NoError(t, err)
	require.Greater(t, len(canonical), len(frame)+100)
	limits := NativeReasoningReplayLimits{MaxEntryBytes: len(frame) + 100}
	wire := append(append([]byte("data: "), frame...), '\n', '\n')
	capture := NewNativeReasoningResponseCapture(io.NopCloser(bytes.NewReader(wire)), limits)
	_, err = io.ReadAll(capture)
	require.NoError(t, err)
	_, ok := capture.CompletedResponse()
	require.False(t, ok, "raw/canonical split must not widen existing full-output eligibility limits")
}

func TestNativeReasoningReplaySparseItemSubindicesDoNotUseOutputItemLimit(t *testing.T) {
	engine, _, _, frames, _ := sparseReplayFixture(t)
	parts := make([]map[string]string, 129)
	for i := range parts {
		parts[i] = map[string]string{"type": "output_text", "text": "x"}
	}
	rawParts, err := json.Marshal(parts)
	require.NoError(t, err)
	for _, position := range []int{3, 4} {
		frames[position] = sparseReplayEdit(t, frames[position], func(event map[string]json.RawMessage) {
			event["item"] = sparseReplayEdit(t, event["item"], func(item map[string]json.RawMessage) { item["content"] = rawParts })
		})
	}
	frames = append(frames[:4], append([][]byte{[]byte(`{"type":"response.output_text.done","item_id":"msg_synthetic","content_index":128,"text":"x"}`)}, frames[4:]...)...)
	_, ok := sparseReplayCollect(frames, engine.limits)
	require.True(t, ok, "129 parts within one item are not 129 output items")
}

func TestNativeReasoningReplaySparseOptionalPayloadTypesRemainChecked(t *testing.T) {
	for _, payload := range []struct {
		name   string
		fields string
		valid  bool
	}{
		{"absent", ``, true},
		{"null", `,"delta":null`, true},
		{"string", `,"delta":"synthetic"`, true},
		{"bad_delta", `,"delta":1`, false},
		{"bad_text", `,"text":[]`, false},
		{"bad_arguments", `,"arguments":{}`, false},
		{"bad_input", `,"input":true`, false},
		{"bad_part", `,"part":"text"`, false},
		{"bad_annotation", `,"annotation":[]`, false},
	} {
		t.Run(payload.name, func(t *testing.T) {
			engine, _, _, frames, _ := sparseReplayFixture(t)
			event := []byte(`{"type":"response.output_text.delta","output_index":1` + payload.fields + `}`)
			frames = append(frames[:4], append([][]byte{event}, frames[4:]...)...)
			_, ok := sparseReplayCollect(frames, engine.limits)
			require.Equal(t, payload.valid, ok)
		})
	}
}

func TestNativeReasoningReplayPureToolTurnNeedsNoNewOpaqueOrAncestor(t *testing.T) {
	for _, shape := range []string{"full", "empty", "absent"} {
		t.Run(shape, func(t *testing.T) {
			engine, request, completion, frames, items := sparseReplayFixture(t)
			tool := sparseReplayEdit(t, items[2], func(item map[string]json.RawMessage) {
				delete(item, "encrypted_content")
				delete(item, "encrypted_function_args")
				item["future_tool_state"] = json.RawMessage(`{"value":9007199254740993,"retained":null}`)
			})
			terminal := frames[len(frames)-1]
			if shape != "empty" {
				terminal = sparseReplayEdit(t, terminal, func(event map[string]json.RawMessage) {
					event["response"] = sparseReplayEdit(t, event["response"], func(response map[string]json.RawMessage) {
						if shape == "full" {
							response["output"] = append(append([]byte{'['}, tool...), ']')
						} else {
							delete(response, "output")
						}
					})
				})
			}
			got, ok := sparseReplayCollect([][]byte{frames[0], sparseReplayFrame("response.output_item.added", 0, tool), sparseReplayFrame("response.output_item.done", 0, tool), terminal}, engine.limits)
			require.True(t, ok)
			require.NotContains(t, string(got), "encrypted_content")
			require.NotContains(t, string(got), "encrypted_function_args")
			var visible apicompat.ResponsesResponse
			require.NoError(t, json.Unmarshal(got, &visible))
			visible.Output[0].Name = "python"
			completion.Response = got
			completion.DeliveredAssistant, _ = json.Marshal(apicompat.ResponsesToChatCompletions(&visible, request.Scope.Model).Choices[0].Message)
			require.NoError(t, engine.Commit(context.Background(), request, completion))
			restored, err := engine.Restore(context.Background(), nativeReplayContinuation(request, completion))
			require.NoError(t, err)
			require.Len(t, restored, 1)
			require.Len(t, restored[0].Output, 1)
			require.JSONEq(t, string(tool), string(restored[0].Output[0]))
			mismatched := completion
			mismatched.DeliveredAssistant = json.RawMessage(strings.ReplaceAll(string(completion.DeliveredAssistant), "call_synthetic", "call_other"))
			require.ErrorIs(t, engine.Commit(context.Background(), request, mismatched), ErrNativeReasoningReplayInvalid)
		})
	}
}
