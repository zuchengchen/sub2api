package service_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func nativeAssistantRecordFixture(t *testing.T, h *nativeTrafficHarness) {
	t.Helper()
	if dir := os.Getenv("NATIVE_REASONING_ASSISTANT_CAPTURE_DIR"); dir != "" {
		t.Cleanup(func() {
			require.NoError(t, os.MkdirAll(dir, 0700))
			rows, _, _, _ := h.snapshot()
			recordNativeTrafficScenario(t, dir, strings.ReplaceAll(t.Name(), "/", "__"), rows, h.clientEvidence())
		})
	}
}

func nativeTrafficAssistantOutput(round int, reasoning bool) []json.RawMessage {
	var output []json.RawMessage
	if reasoning {
		output = append(output, nativeTrafficJSON(map[string]any{"type": "reasoning", "id": fmt.Sprintf("rs_answer_%d", round), "status": "completed", "encrypted_content": fmt.Sprintf("opaque_final_reasoning_%d", round), "summary": []any{map[string]any{"type": "summary_text", "text": "The synthetic result is ready."}}}))
	}
	output = append(output, json.RawMessage(fmt.Sprintf(`{"type":"message","id":"msg_answer_%d","role":"assistant","status":"completed","phase":"final_answer","content":[{"type":"output_text","text":"Synthetic final answer %d.","annotations":[]}],"future_message_state":{"sequence":9007199254740993,"retained":null}}`, round, round)))
	return output
}

func nativeAssistantRequireNewTurnStablePrefix(t *testing.T, first, follow nativeTrafficRow) {
	t.Helper()
	initial := gjson.GetBytes(first.Body, "input").Array()
	next := gjson.GetBytes(follow.Body, "input").Array()
	require.Greater(t, len(next), len(initial))
	for i, item := range initial {
		require.Equal(t, item.Raw, next[i].Raw, "the complete previously sent native prefix must stay byte-stable")
	}
	firstTurn := gjson.GetBytes(first.Body, "client_metadata.turn_id").String()
	nextTurn := gjson.GetBytes(follow.Body, "client_metadata.turn_id").String()
	require.NotEmpty(t, firstTurn)
	require.NotEmpty(t, nextTurn)
	require.NotEqual(t, firstTurn, nextTurn, "a new real user turn must keep its own actual turn identity")
	require.NotContains(t, initial[1].Raw, `"turn_id"`, "the static gateway capability guard is not re-stamped as a new turn")
}

func TestNativeReasoningReplayAssistantMessageHTTPAfterTools(t *testing.T) {
	for _, shape := range []string{"full", "empty", "absent"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%v", shape, stream), func(t *testing.T) {
				h := newNativeTrafficHarness(t, "replay")
				nativeAssistantRecordFixture(t, h)
				h.mu.Lock()
				h.sparseTerminalOutput = shape
				h.outputOverrides = map[int][]json.RawMessage{3: nativeTrafficAssistantOutput(3, true), 4: nativeTrafficAssistantOutput(4, true)}
				h.mu.Unlock()
				history := nativeTrafficInitialHistory()
				for round := 0; round < 3; round++ {
					assistant, err := h.call(history, stream, "")
					require.NoError(t, err)
					history = nativeTrafficContinue(history, assistant)
				}
				history = append(history, json.RawMessage(`{"role":"user","content":"Explain the completed synthetic answer."}`))
				_, err := h.call(history, stream, "")
				require.NoError(t, err)
				rows, _, _, puts := h.snapshot()
				require.Len(t, rows, 4)
				nativeAssistantRequireNewTurnStablePrefix(t, rows[0], rows[3])
				require.Equal(t, gjson.GetBytes(rows[0].Body, "client_metadata.turn_id").String(), gjson.GetBytes(rows[2].Body, "client_metadata.turn_id").String())
				for prior := 0; prior < 3; prior++ {
					require.Equal(t, 0, nativeTrafficCompareOutputBlock(rows[3].Body, rows[prior].Output, true, prior < 2), "the next user turn must preserve complete prior native block %d, including the final assistant", prior)
				}
				require.NotContains(t, string(rows[3].Body), `\u003cthinking\u003e`)
				require.Equal(t, 4, puts)
			})
		}
	}
}

func TestNativeReasoningReplayAssistantMessageHTTPWithoutToolsOrOpaque(t *testing.T) {
	for _, shape := range []string{"full", "empty", "absent"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%v", shape, stream), func(t *testing.T) {
				h := newNativeTrafficHarness(t, "replay")
				nativeAssistantRecordFixture(t, h)
				h.mu.Lock()
				h.sparseTerminalOutput = shape
				h.outputOverrides = map[int][]json.RawMessage{1: nativeTrafficAssistantOutput(1, false), 2: nativeTrafficAssistantOutput(2, false)}
				h.mu.Unlock()
				history := nativeTrafficInitialHistory()
				assistant, err := h.call(history, stream, "")
				require.NoError(t, err)
				history = append(nativeTrafficContinue(history, assistant), json.RawMessage(`{"role":"user","content":"Continue the ordinary assistant conversation."}`))
				_, err = h.call(history, stream, "")
				require.NoError(t, err)
				rows, _, _, puts := h.snapshot()
				require.Len(t, rows, 2)
				nativeAssistantRequireNewTurnStablePrefix(t, rows[0], rows[1])
				require.Equal(t, 0, nativeTrafficCompareOutputBlock(rows[1].Body, rows[0].Output, false, false), "ordinary assistant native IDs, phase and extensions must survive the next user turn")
				require.Equal(t, 2, puts)
			})
		}
	}
}
