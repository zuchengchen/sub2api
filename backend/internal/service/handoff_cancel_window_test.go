package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Deterministically model cancellation after the original AfterFunc has been
// stopped but before handoff's second context check. A pre-Flush caller cannot
// rely on its eventual finish defer to release this failed transfer.
func TestGPT61SolChatCachePublicationCancelAfterStopReleases(t *testing.T) {
	for _, clientOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "forward_context", true: "client_context"}[clientOnly], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client, cancelClient := context.WithCancel(context.Background())
			defer cancelClient()
			var registry nativeReasoningPublications
			lease := registry.acquire(ctx, "synthetic-owner", "synthetic-key", DefaultNativeReasoningReplayLimits())
			require.NotNil(t, lease)
			defer lease.release()
			lease.mu.Lock()
			stopped := lease.stop()
			lease.stop = func() bool {
				if clientOnly {
					cancelClient()
				} else {
					cancel()
				}
				return true
			}
			lease.mu.Unlock()
			require.True(t, stopped)
			bounded, finish, transferred := lease.handoff(ctx, client, 100*time.Millisecond)
			require.False(t, transferred)
			require.Nil(t, bounded)
			require.Nil(t, finish)
			select {
			case <-lease.entry.done:
			default:
				t.Fatal("failed post-stop handoff left a lease with no cancellation callback or deadline; a blocked Flush retains it")
			}
		})
	}
}
