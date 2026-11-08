package service_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNativeReasoningReplayMetadataHTTPThreeTurnNativeRestoration(t *testing.T) {
	for _, shape := range []string{"full", "empty", "absent"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%v", shape, stream), func(t *testing.T) {
				h := newNativeTrafficHarness(t, "replay")
				h.mu.Lock()
				h.sparseTerminalOutput, h.metadataEvents = shape, true
				h.mu.Unlock()
				history := nativeTrafficInitialHistory()
				for turn := 0; turn < 3; turn++ {
					assistant, err := h.call(history, stream, "")
					require.NoError(t, err)
					history = nativeTrafficContinue(history, assistant)
				}
				h.mu.Lock()
				rows := append([]nativeTrafficRow(nil), h.rows...)
				errors := append([]string(nil), h.errors...)
				h.mu.Unlock()
				require.Empty(t, errors)
				require.Len(t, rows, 3)
				for turn := 1; turn < len(rows); turn++ {
					for prior := 1; prior <= turn; prior++ {
						require.Equal(t, 0, nativeTrafficComparisonExit(rows[turn].Body, nativeTrafficOutput(prior)), "turn %d must retain complete native turn %d across metadata", turn+1, prior)
					}
				}
				h.store.mu.Lock()
				require.Equal(t, 3, h.store.puts)
				h.store.mu.Unlock()
			})
		}
	}
}
