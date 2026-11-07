package handler

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestNativeReasoningReplayWSClientInterruptedSchedulingAttribution(t *testing.T) {
	confirmed := &service.OpenAIForwardResult{
		OpenAIWSMode: true, ClientInterrupted: true, UpstreamTerminalEvent: "response.incomplete",
		Usage: service.OpenAIUsage{InputTokens: 7, OutputTokens: 3},
	}
	require.False(t, openAIForwardHasSchedulingSample(confirmed))
	require.False(t, openAIForwardSucceededForScheduling(confirmed), "the real incomplete terminal must not be rewritten into upstream success")
	require.Equal(t, "response.incomplete", confirmed.UpstreamTerminalEvent)
	require.Equal(t, 7, confirmed.Usage.InputTokens)
	require.Equal(t, 3, confirmed.Usage.OutputTokens)
	for _, result := range []*service.OpenAIForwardResult{
		nil, {}, {ClientInterrupted: true},
		{OpenAIWSMode: true, UpstreamTerminalEvent: "response.completed"},
		{OpenAIWSMode: true, UpstreamTerminalEvent: "response.incomplete"},
		{OpenAIWSMode: true, UpstreamTerminalEvent: "response.failed"},
		{OpenAIWSMode: true, ClientDisconnect: true, UpstreamTerminalEvent: "response.incomplete"},
	} {
		require.True(t, openAIForwardHasSchedulingSample(result), "only the explicit verified WS interruption marker can suppress a quality sample")
	}
}

func TestNativeReasoningReplayWSClientInterruptedIsInternalOnly(t *testing.T) {
	var result service.OpenAIForwardResult
	require.NoError(t, json.Unmarshal([]byte(`{"ClientInterrupted":true,"client_interrupted":true}`), &result))
	require.False(t, result.ClientInterrupted)
	raw, err := json.Marshal(service.OpenAIForwardResult{ClientInterrupted: true})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "ClientInterrupted")
	require.NotContains(t, string(raw), "client_interrupted")
}
