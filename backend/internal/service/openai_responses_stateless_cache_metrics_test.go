//go:build unit

package service

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestResponsesStatelessCacheTrialMetersFinalAttempt(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(responsesStatelessCacheAssignmentKey, "trial_full")
	c.Set(responsesStatelessCacheActionKey, "full_input_written")
	var outcome ResponsesStatelessCacheOutcome
	outcome.Capture(c, nil, errors.New("failed attempt"))
	require.True(t, outcome.failed)
	ms := 75
	result := &OpenAIForwardResult{OpenAIWSMode: true, Usage: OpenAIUsage{InputTokens: 140000, CacheReadInputTokens: 100864},
		FirstTokenMs: &ms, UpstreamTerminalEvent: "response.completed"}
	outcome.Capture(c, result, nil)
	result.Usage.CacheReadInputTokens = 0 // The usage worker can own the result now.
	require.False(t, outcome.failed)
	d := &ResponsesCacheDiagnostic{event: make(map[string]any)}
	outcome.AttachDiagnostic(d)
	entry := d.event["stateless_cache_trial"].(map[string]any)
	require.Equal(t, "ws", entry["final_transport"])
	require.Equal(t, 75, entry["first_token_ms"])
	cell := "trial_full/131072_262143"
	statelessTrialMeters.Lock()
	before := statelessTrialMeters.buckets[cell]
	statelessTrialMeters.Unlock()
	outcome.Publish()
	statelessTrialMeters.Lock()
	after := statelessTrialMeters.buckets[cell]
	statelessTrialMeters.Unlock()
	require.EqualValues(t, 1, after.Requests-before.Requests)
	require.EqualValues(t, 1, after.FullWritten-before.FullWritten)
	require.EqualValues(t, 1, after.FinalWS-before.FinalWS)
	require.EqualValues(t, 0, after.Failures-before.Failures)
	require.EqualValues(t, 100864, after.Cached-before.Cached)
	require.EqualValues(t, 140000, after.Input-before.Input)
	require.EqualValues(t, 75, after.FirstTokenMS-before.FirstTokenMS)

	// A WS write followed by a successful HTTP fallback remains in the
	// assigned arm. Transport and missing first-token evidence are explicit.
	outcome.Capture(c, &OpenAIForwardResult{}, nil)
	require.Equal(t, "http", outcome.transport)
	require.False(t, outcome.hasFirst)
	outcome.Capture(c, nil, errors.New("no successful retry"))
	require.Equal(t, "unknown", outcome.transport)
	require.True(t, outcome.failed)
}
