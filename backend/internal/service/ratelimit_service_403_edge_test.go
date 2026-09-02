//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// productionEdgeChallenge403Body is the ChatGPT HTML holding page observed in
// production. Treating it as an account 403 unschedules every failover hop.
const productionEdgeChallenge403Body = `<html>
 <head>
 <meta name="viewport" content="width=device-width, initial-scale=1" />
 <style global>body{font-family:Arial,Helvetica,sans-serif}.container{align-items:center;display:flex;flex-direction:column;gap:2rem;height:100%;justify-content:center;width:100%}</style>
 <meta http-equiv="refresh" content="360"></head>
 <body>
 <div class="container"><div class="logo"><svg width="41" height="41"></svg></div></div>
 </body>
</html>`

func newEdge403Service(counts ...int64) (*RateLimitService, *rateLimitAccountRepoStub, *openAI403CounterCacheStub, *runtimeBlockRecorder) {
	repo := &rateLimitAccountRepoStub{}
	counter := &openAI403CounterCacheStub{counts: counts}
	blocker := &runtimeBlockRecorder{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil)
	svc.SetOpenAI403CounterCache(counter)
	svc.SetAccountRuntimeBlocker(blocker)
	return svc, repo, counter, blocker
}

func edge403Account(id int64) *Account {
	return &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
}

func TestHandleUpstreamError_OpenAI403EdgeChallengeLeavesAccountSchedulable(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"production_html_holding_page", productionEdgeChallenge403Body},
		{"cloudflare_just_a_moment", `<!DOCTYPE html><html><head><title>Just a moment...</title></head><body>Checking your browser</body></html>`},
		{"cloudflare_challenge_platform", `<html><body><script src="/cdn-cgi/challenge-platform/h/b/orchestrate/chl_page/v1"></script></body></html>`},
		{"bare_text_challenge", `Attention Required! Enable JavaScript and cookies to continue`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo, counter, blocker := newEdge403Service(99)
			account := edge403Account(401)

			shouldDisable := svc.HandleUpstreamError(
				context.Background(),
				account,
				http.StatusForbidden,
				http.Header{},
				[]byte(tc.body),
			)

			require.True(t, shouldDisable)
			require.Equal(t, 0, repo.tempCalls, "must not persist a temp-unschedulable block")
			require.Equal(t, 0, repo.setErrorCalls, "must not permanently disable the account")
			require.Empty(t, blocker.accounts, "must not block scheduling in memory")
			require.Len(t, counter.counts, 1, "must not consume the 403 counter")
		})
	}
}

func TestHandleUpstreamError_OpenAI403AccountLevelStillTempUnschedules(t *testing.T) {
	svc, repo, _, blocker := newEdge403Service(1)
	account := edge403Account(402)

	shouldDisable := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusForbidden,
		http.Header{},
		[]byte(`{"error":{"message":"workspace forbidden by policy"}}`),
	)

	require.True(t, shouldDisable)
	require.Equal(t, 1, repo.tempCalls)
	require.Equal(t, 0, repo.setErrorCalls)
	require.Contains(t, repo.lastTempReason, "workspace forbidden by policy")
	require.Contains(t, repo.lastTempReason, "(1/3)")
	require.Len(t, blocker.accounts, 1)
}

func TestHandleUpstreamError_OpenAI403AccountLevelStillDisablesAtThreshold(t *testing.T) {
	svc, repo, _, _ := newEdge403Service(openAI403DisableThreshold)
	account := edge403Account(403)

	shouldDisable := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusForbidden,
		http.Header{},
		[]byte(`{"error":{"message":"workspace forbidden by policy"}}`),
	)

	require.True(t, shouldDisable)
	require.Equal(t, 1, repo.setErrorCalls)
	require.Equal(t, 0, repo.tempCalls)
	require.Contains(t, repo.lastErrorMsg, "consecutive_403=3/3")
}
