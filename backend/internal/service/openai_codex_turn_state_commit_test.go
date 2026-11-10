package service

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCodexOAuthTurnStateTracksKeepaliveCommittedHeader(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c, rec := newTurnStateTestContext(t, 31, "heartbeat-state")
	c.Header(openAICodexTurnStateHeader, "actually-delivered")
	defer svc.watchOpenAICodexTurnStateCommit(c, &Account{ID: 311})()
	stop := startOpenAISSEKeepalive(c, time.Hour)
	k, ok := c.MustGet(openAICompactSSEKeepaliveKey).(*openAICompactSSEKeepalive)
	require.True(t, ok)
	require.True(t, k.beat(), "deterministically run one real keepalive commit")
	stop()
	require.Equal(t, "actually-delivered", rec.Result().Header.Get(openAICodexTurnStateHeader))
	// A later attempt can change the Header map, but cannot change what was sent.
	c.Header(openAICodexTurnStateHeader, "never-delivered")
	_, err := c.Writer.Write([]byte("data: done\n\n"))
	require.NoError(t, err)
	for _, state := range []string{"actually-delivered", "never-delivered"} {
		echo := http.Header{}
		echo.Set(openAICodexTurnStateHeader, state)
		svc.guardOpenAICodexTurnStateEcho(c, &Account{ID: 312}, echo)
		if state == "actually-delivered" {
			require.Empty(t, echo.Get(openAICodexTurnStateHeader))
		} else {
			require.Equal(t, state, echo.Get(openAICodexTurnStateHeader))
		}
	}
}
