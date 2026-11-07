package service_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNativeReasoningReplayPureToolHTTPKeepsBothPriorNativeTurns(t *testing.T) {
	for _, shape := range []string{"full", "empty", "absent"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%v", shape, stream), func(t *testing.T) {
				h := newNativeTrafficHarness(t, "replay")
				h.mu.Lock()
				h.sparseTerminalOutput, h.pureToolSecondRound = shape, true
				h.mu.Unlock()
				history := nativeTrafficInitialHistory()
				for round := 0; round < 3; round++ {
					assistant, err := h.call(history, stream, "")
					require.NoError(t, err)
					history = nativeTrafficContinue(history, assistant)
				}
				rows, _, _, puts := h.snapshot()
				require.Len(t, rows, 3)
				require.Equal(t, 0, nativeTrafficComparisonExit(rows[1].Body, rows[0].Output), "B must restore A's complete opaque-bearing native output")
				require.Equal(t, 0, nativeTrafficComparisonExit(rows[2].Body, rows[0].Output), "C must still retain A's complete opaque-bearing native output")
				for _, item := range rows[1].Output {
					require.NotContains(t, string(item), "encrypted_content")
					require.NotContains(t, string(item), "encrypted_function_args")
					require.Contains(t, string(item), `"namespace":"functions"`)
					require.Contains(t, string(item), `"future_tool_state":{"sequence":9007199254740993,"retained":null}`)
				}
				require.Equal(t, 0, nativeTrafficCompareBlock(rows[2].Body, rows[1].Output, false), "C must also contain B's entire original tool-only block, including native IDs, namespace and unknown fields")
				require.Equal(t, 3, puts, "the complete pure tool turn must be committed as its own strict history record")
			})
		}
	}
}
