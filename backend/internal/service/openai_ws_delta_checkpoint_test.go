package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIWSDeltaRecorderTerminalOutputBaseline(t *testing.T) {
	recorder := &openAIWSDeltaRecorder{}
	recorder.observe([]byte(`{"type":"response.completed","response":{"id":"resp_terminal","status":"completed","output":[{"type":"function_call","id":"fc_one","call_id":"call_one","name":"lookup","arguments":"{}"}]}}`))
	input := []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":"lookup"}`)}
	chain, changed := recorder.finalize(1, "fp", openAIWSDeltaItemsHash(input), nil)
	require.True(t, changed)
	require.NotNil(t, chain)
	require.Len(t, chain.BaselineHashes, 2, "a terminal-only tool call is part of the anchored context")
	current := append(input,
		json.RawMessage(`{"type":"function_call","id":"fc_one","call_id":"call_one","name":"lookup","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_one","output":"found"}`))
	decision := resolveOpenAIWSDelta(chain, 1, "fp", current)
	require.True(t, decision.Delta, decision.Reason)
	require.Len(t, decision.TailItems, 1, "do not resend the anchored tool call as new input")
}

func TestOpenAIWSDeltaRecorderRejectsConflictingOrFailedCompletion(t *testing.T) {
	for _, terminal := range []string{
		`{"type":"response.completed","response":{"id":"resp_bad","status":"failed","output":[]}}`,
		`{"type":"response.completed","response":{"id":"resp_bad","error":{"code":"bad"},"output":[]}}`,
		`{"type":"response.completed","response":{"id":"resp_bad","output":[{"type":"message","id":"msg_one","role":"assistant","content":"changed"}]}}`,
	} {
		t.Run(terminal, func(t *testing.T) {
			recorder := &openAIWSDeltaRecorder{}
			recorder.observe([]byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_one","role":"assistant","content":"first"}}`))
			recorder.observe([]byte(terminal))
			chain, _ := recorder.finalize(1, "fp", nil, nil)
			require.Nil(t, chain, "ambiguous/failed output must not establish a continuation checkpoint")
		})
	}
}

func TestOpenAIWSDeltaWireItemsKeepPayloadMutation(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":"original"}]}`)
	current := []any{map[string]any{"type": "message", "role": "user", "content": "changed"}}
	items := openAIWSDeltaWireItems(body, current)
	require.Len(t, items, 1)
	require.JSONEq(t, `{"type":"message","role":"user","content":"changed"}`, string(items[0]), "equal lengths do not imply equivalent logical input")
}

func TestOpenAIWSDeltaUnknownParameterChangeUsesFullInput(t *testing.T) {
	payload := map[string]any{"model": "gpt-test", "truncation": "disabled"}
	before := openAIWSDeltaFieldsFingerprint(payload)
	payload["truncation"] = "auto"
	require.NotEqual(t, before, openAIWSDeltaFieldsFingerprint(payload), "unknown request properties may alter context")
}

func TestOpenAIWSDeltaPreservesNativeIdentityAndLateToolMetadata(t *testing.T) {
	base := json.RawMessage(`{"type":"function_call_output","id":"fco_one","call_id":"call_one","output":"ok","name":null,"namespace":null,"internal_chat_message_metadata_passthrough":{"turn_id":"first","create_time":1}}`)
	replay := json.RawMessage(`{"type":"function_call_output","id":"fco_one","call_id":"call_one","output":"ok","internal_chat_message_metadata_passthrough":{"turn_id":"next","create_time":2}}`)
	require.Equal(t, openAIWSDeltaItemHash(base), openAIWSDeltaItemHash(replay), "nullable and tracing fields do not change native model history")
	for _, changed := range []string{
		`{"type":"function_call_output","id":"fco_other","call_id":"call_one","output":"ok"}`,
		`{"type":"function_call_output","id":"fco_one","call_id":"call_other","output":"ok"}`,
		`{"type":"function_call_output","id":"fco_one","call_id":"call_one","output":"ok","internal_chat_message_metadata_passthrough":{"executed_tool_calls":[{"name":"lookup","arguments":{},"tool_result_metadata":{"late":true}}]}}`,
	} {
		require.NotEqual(t, openAIWSDeltaItemHash(replay), openAIWSDeltaItemHash(json.RawMessage(changed)))
	}
}

func TestOpenAIWSDeltaEmptyTailIsAnArray(t *testing.T) {
	input := []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":"hello"}`)}
	chain := &openAIWSDeltaChainState{ResponseID: "resp_same", AccountID: 1, FieldsFP: "fp", BaselineHashes: openAIWSDeltaItemsHash(input)}
	decision := resolveOpenAIWSDelta(chain, 1, "fp", input)
	require.True(t, decision.Delta, decision.Reason)
	tail, err := json.Marshal(decision.TailItems)
	require.NoError(t, err)
	require.Equal(t, "[]", string(tail), "Codex permits equality with the baseline, including prewarm")
}

func TestOpenAIWSDeltaToolPairsAndCompactionBoundaries(t *testing.T) {
	user := json.RawMessage(`{"type":"message","role":"user","content":"hello"}`)
	call := json.RawMessage(`{"type":"function_call","id":"fc_one","call_id":"call_one","name":"lookup","arguments":"{}"}`)
	base := []json.RawMessage{user, call}
	checkpoint := &openAIWSDeltaChainState{ResponseID: "resp_tool", AccountID: 1, FieldsFP: "fp", BaselineHashes: openAIWSDeltaItemsHash(base)}
	for _, tail := range []string{
		`{"type":"message","role":"user","content":"missing result"}`,
		`{"type":"custom_tool_call_output","call_id":"call_one","output":"wrong kind"}`,
		`{"type":"function_call_output","call_id":"other_call","output":"wrong call"}`,
		`{"type":"function_call_output","call_id":"call_one","output":null}`,
		`{"type":"compaction_trigger"}`,
		`{"type":"item_reference","id":"fc_one"}`,
	} {
		items := append(append([]json.RawMessage{}, base...), json.RawMessage(tail))
		require.False(t, resolveOpenAIWSDelta(checkpoint, 1, "fp", items).Delta, tail)
	}
}

func TestOpenAIWSDeltaCheckpointInvalidatedByEveryWrite(t *testing.T) {
	for _, failed := range []bool{false, true} {
		captured := &openAIWSCaptureConn{closed: failed}
		conn := newOpenAIWSConn("checkpoint", 1, captured, nil)
		conn.deltaCheckpoint.Store(&openAIWSDeltaChainState{ResponseID: "resp_old"})
		err := conn.writeJSON(map[string]any{"type": "response.create", "generate": false}, context.Background())
		if failed {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
		require.Nil(t, conn.deltaCheckpoint.Load(), "prewarm/native/failed writes all supersede the old socket checkpoint")
		_ = captured.Close()
	}
}
