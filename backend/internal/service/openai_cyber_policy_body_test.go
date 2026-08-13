package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBuildCyberPolicyMarkBodyRedactsInstructionsBeforeTruncation(t *testing.T) {
	longInstructions := strings.Repeat("agent system prompt. ", 400)
	payload, err := json.Marshal(map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"error":        map[string]any{"code": "cyber_policy", "message": "flagged"},
			"instructions": longInstructions,
			"model":        "gpt-5.6-sol",
		},
	})
	require.NoError(t, err)
	require.Greater(t, len(payload), cyberPolicyMarkBodyMaxBytes)

	got := buildCyberPolicyMarkBody(payload)
	require.LessOrEqual(t, len(got), cyberPolicyMarkBodyMaxBytes)
	require.NotContains(t, got, strings.Repeat("agent system prompt. ", 2))
	require.Equal(t, cyberPolicyBodyDroppedInstructions, gjson.Get(got, "response.instructions").String())
	require.Equal(t, "cyber_policy", gjson.Get(got, "response.error.code").String())
	require.Equal(t, "gpt-5.6-sol", gjson.Get(got, "response.model").String())
	require.True(t, gjson.Valid(got))
}

func TestBuildCyberPolicyMarkBodyTopLevelAndFallback(t *testing.T) {
	payload := []byte(`{"error":{"code":"cyber_policy"},"instructions":"secret agent prompt","model":"gpt-5"}`)
	got := buildCyberPolicyMarkBody(payload)
	require.Equal(t, cyberPolicyBodyDroppedInstructions, gjson.Get(got, "instructions").String())
	require.Equal(t, "gpt-5", gjson.Get(got, "model").String())

	invalid := []byte(strings.Repeat("x", cyberPolicyMarkBodyMaxBytes+50))
	require.Equal(t, string(invalid[:cyberPolicyMarkBodyMaxBytes]), buildCyberPolicyMarkBody(invalid))
}
