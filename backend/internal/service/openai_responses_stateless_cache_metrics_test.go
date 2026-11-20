//go:build unit

package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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

func TestResponsesStatelessCacheTrialSnapshotAtConfiguredLogLevel(t *testing.T) {
	buckets := map[string]responsesCacheTrialTotals{
		"control_prewarm/under_131072": {Requests: 7, Metered: 7, Input: 9000, Cached: 2800},
		"trial_full/131072_262143":     {Requests: 3, Failures: 1, FirstTokenN: 2, FirstTokenMS: 256},
	}
	for _, level := range []zapcore.Level{zap.DebugLevel, zap.InfoLevel, zap.WarnLevel, zap.ErrorLevel} {
		t.Run(level.String(), func(t *testing.T) {
			core, captured := observer.New(level)
			var fallback bytes.Buffer
			writeResponsesCacheTrialSnapshot(zap.New(core), &fallback, 10, buckets)
			if level <= zap.InfoLevel {
				require.Empty(t, fallback.String())
				require.Equal(t, 1, captured.Len())
				entry := captured.All()[0]
				require.Equal(t, zap.InfoLevel, entry.Level)
				require.Equal(t, "responses_stateless_cache_trial_totals", entry.Message)
				require.EqualValues(t, 10, entry.ContextMap()["events"])
				require.Equal(t, buckets, entry.ContextMap()["buckets"])
				return
			}
			require.Zero(t, captured.Len())
			require.Equal(t, 1, bytes.Count(fallback.Bytes(), []byte{'\n'}))
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(fallback.Bytes(), &fields))
			require.Len(t, fields, 3)
			require.JSONEq(t, `"responses_stateless_cache_trial_totals"`, string(fields["msg"]))
			require.JSONEq(t, "10", string(fields["events"]))
			var cells map[string]map[string]int64
			require.NoError(t, json.Unmarshal(fields["buckets"], &cells))
			require.Len(t, cells, len(buckets))
			for name, expected := range buckets {
				require.Equal(t, map[string]int64{
					"requests": expected.Requests, "failures": expected.Failures,
					"prewarm_written": expected.PrewarmWritten, "full_written": expected.FullWritten,
					"final_ws": expected.FinalWS, "metered": expected.Metered,
					"input_tokens": expected.Input, "cached_tokens": expected.Cached,
					"first_token_samples": expected.FirstTokenN, "first_token_ms_sum": expected.FirstTokenMS,
				}, cells[name])
			}
		})
	}
}

func TestResponsesStatelessCacheTrialMetersOnlyRandomizedArms(t *testing.T) {
	statelessTrialMeters.Lock()
	before := statelessTrialMeters.events
	statelessTrialMeters.Unlock()
	for _, assignment := range []string{"", "fixed_full", "caller-controlled marker"} {
		ResponsesStatelessCacheOutcome{assignment: assignment}.Publish()
	}
	statelessTrialMeters.Lock()
	after := statelessTrialMeters.events
	statelessTrialMeters.Unlock()
	require.Equal(t, before, after)
}
