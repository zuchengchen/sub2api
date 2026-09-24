package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRedactAuditBodyHarvestProxyCredentials(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		SettingKeyOpenAICodexTicketHarvestProxyURL: "socks5h://customer-session-{session}:private-password@proxy.example:1080",
		"site_name": "unchanged",
	})
	require.NoError(t, err)
	redacted := RedactAuditBody(body, "application/json")
	require.NotContains(t, redacted, "private-password")
	require.NotContains(t, redacted, "customer")
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(redacted), &got))
	require.Equal(t, "***", got[SettingKeyOpenAICodexTicketHarvestProxyURL])
	require.Equal(t, "unchanged", got["site_name"])
}
