package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProbeNewOpenAIOAuthAccountUsesCurrentAccountAndPersistsSample(t *testing.T) {
	tc := newTiboRouteCase(t, false, false, cookieWSHTTPResponse("True"))
	repo := &tiboAutoGroupRecorder{tiboAccountRepo: mustTestValue[*tiboAccountRepo](t, tc.svc.accountRepo)}
	tc.svc.accountRepo = repo
	proxy := &Proxy{ID: 7, Protocol: "http", Host: "proxy.example", Port: 8080}
	repo.setAccount(func(account *Account) {
		account.ProxyID, account.Proxy = &proxy.ID, proxy
		account.Credentials["access_token"] = "current-token"
	})
	tc.account.CreatedAt = time.Now()
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	state.http.lastSampleAt = tc.account.CreatedAt.Add(-time.Minute)
	state.http.nextProbeAt = tc.account.CreatedAt.Add(time.Minute)
	require.Zero(t, tc.svc.pruneOpenAITiboAccounts(nil, tc.account.CreatedAt.Add(-time.Second)), "a listing begun before import cannot drop the new state")

	tc.svc.probeNewOpenAIOAuthAccount(t.Context(), tc.account)
	// A second trigger finds the sample produced by the first probe.
	tc.svc.probeNewOpenAIOAuthAccount(t.Context(), tc.account)
	tc.svc.openaiTiboProbeWG.Wait()

	require.Len(t, tc.upstream.requests, 1)
	require.Equal(t, "Bearer current-token", tc.upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, proxy.URL(), tc.upstream.lastProxyURL)
	requireTiboProbe(t, tc.upstream.lastBody)
	require.Len(t, repo.samples, 1)
	require.Equal(t, string(openAITiboHealthy), repo.samples[0].Verdict)
	stored, ok := repo.updateFor(tc.account.ID)
	require.True(t, ok)
	require.Contains(t, stored, OpenAIAstraPolicyExtraKey)
}

func TestProbeNewOpenAIOAuthAccountSharesRunningProbe(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	upstream := &tiboAutoGroupGatedUpstream{
		answer: "True", entered: make(chan tiboAutoGroupProbeCall, 1), release: make(chan struct{}),
	}
	tc.svc.httpUpstream = upstream
	tc.svc.openaiTiboLoaded.Store(true)
	tc.svc.openAITiboAccountState(tc.account.ID)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer func() {
		cancel()
		tc.svc.openaiTiboProbeWG.Wait()
	}()
	tc.svc.probeOpenAITiboRoutes(ctx)
	select {
	case <-upstream.entered:
	case <-ctx.Done():
		t.Fatal("scheduled probe never reached the upstream")
	}

	waitCtx, stop := context.WithTimeout(ctx, 25*time.Millisecond)
	defer stop()
	tc.svc.probeNewOpenAIOAuthAccount(waitCtx, tc.account)
	require.ErrorIs(t, waitCtx.Err(), context.DeadlineExceeded)
	close(upstream.release)
	tc.svc.openaiTiboProbeWG.Wait()
	require.EqualValues(t, 1, upstream.calls.Load())
	require.Equal(t, openAITiboHealthy, tc.svc.openAITiboEffective(tc.account.ID, openAITiboRouteHTTP, time.Now()))
}

func TestProbeNewOpenAIOAuthAccountHonorsRateLimit(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	repo := mustTestValue[*tiboAccountRepo](t, tc.svc.accountRepo)
	resetAt := time.Now().Add(time.Minute)
	repo.setAccount(func(account *Account) { account.RateLimitResetAt = &resetAt })

	tc.svc.probeNewOpenAIOAuthAccount(t.Context(), tc.account)
	tc.svc.openaiTiboProbeWG.Wait()

	require.Empty(t, tc.upstream.requests)
	status := tc.svc.openAITiboRouteStatuses(tc.account, time.Now())
	require.Len(t, status, 1)
	require.Equal(t, &resetAt, status[0].HeldUntil)
}

func TestProbeNewOpenAIOAuthAccountUsesAgentIdentity(t *testing.T) {
	tc := newTiboRouteCase(t, false, false, cookieWSHTTPResponse("True"))
	repo := mustTestValue[*tiboAccountRepo](t, tc.svc.accountRepo)
	key, encoded := newTestAgentIdentityKey(t)
	repo.setAccount(func(account *Account) {
		delete(account.Credentials, "access_token")
		account.Credentials[openAIAuthModeCredentialKey] = OpenAIAuthModeAgentIdentity
		account.Credentials["agent_runtime_id"] = key.runtimeID
		account.Credentials["agent_private_key"] = encoded
		account.Credentials["task_id"] = key.taskID
	})

	tc.svc.probeNewOpenAIOAuthAccount(t.Context(), tc.account)
	tc.svc.openaiTiboProbeWG.Wait()

	require.Len(t, tc.upstream.requests, 1)
	require.True(t, strings.HasPrefix(tc.upstream.lastReq.Header.Get("Authorization"), "AgentAssertion "))
	require.Equal(t, "test-chatgpt", tc.upstream.lastReq.Header.Get("ChatGPT-Account-ID"))
	require.Equal(t, openAITiboHealthy, tc.svc.openAITiboEffective(tc.account.ID, openAITiboRouteHTTP, time.Now()))
}
