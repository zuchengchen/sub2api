package service_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGPT61SolChatCacheAssistantHTTPWithoutToolsOrOpaque(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
			h := newNativeTrafficHarness(t, "replay")
			h.modelOverride = "gpt-6.1-sol"
			h.outputOverrides = map[int][]json.RawMessage{1: nativeTrafficAssistantOutput(1, false), 2: nativeTrafficAssistantOutput(2, false)}
			history := nativeTrafficInitialHistory()
			assistant, err := h.call(history, stream, "")
			require.NoError(t, err)
			history = append(nativeTrafficContinue(history, assistant), json.RawMessage(`{"role":"user","content":"Continue the ordinary assistant conversation."}`))
			_, err = h.call(history, stream, "")
			require.NoError(t, err)
			rows, _, _, puts := h.snapshot()
			require.Len(t, rows, 2)
			for _, row := range rows {
				require.Equal(t, "gpt-6.1-sol", gjson.GetBytes(row.Body, "model").String())
			}
			nativeAssistantRequireNewTurnStablePrefix(t, rows[0], rows[1])
			require.Zero(t, nativeTrafficCompareOutputBlock(rows[1].Body, rows[0].Output, false, false), "an ordinary assistant's native IDs, phase, and extensions must survive the next user turn")
			require.Equal(t, 2, puts)
		})
	}
}
