package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCodexTicketModeValidation(t *testing.T) {
	for _, mode := range []string{"", "turn_state", "cookie_ws", " COOKIE_WS "} {
		require.NoError(t, validateOpenAICodexTicketMode(OpenAICodexTicketConfig{
			Mode: mode, CookieWSAccountIDs: []int64{23141},
		}))
	}
	require.Error(t, validateOpenAICodexTicketMode(OpenAICodexTicketConfig{Mode: "cookie"}))
	require.Error(t, validateOpenAICodexTicketMode(OpenAICodexTicketConfig{Mode: "cookie_ws", CookieWSAccountIDs: []int64{0}}))
	require.Error(t, validateOpenAICodexTicketMode(OpenAICodexTicketConfig{Mode: "cookie_ws", CookieWSAccountIDs: []int64{-1}}))
}
