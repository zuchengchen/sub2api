//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type oauthReliabilityEdgePoolDialer struct {
	dial func(context.Context, http.Header) (openAIWSClientConn, int, http.Header, error)
}

func (d *oauthReliabilityEdgePoolDialer) Dial(ctx context.Context, _ string, headers http.Header, _ string) (openAIWSClientConn, int, http.Header, error) {
	return d.dial(ctx, headers)
}

func oauthReliabilityEdgePool(t *testing.T) *openAIWSConnPool {
	t.Helper()
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	pool := newOpenAIWSConnPool(cfg)
	t.Cleanup(pool.Close)
	return pool
}

func oauthReliabilityPoolEdgeHook(registry *openAIWSEdgeBackoff, key openAIWSEdgeBackoffKey) openAIWSFreshDialHook {
	return func(ctx context.Context, _ openAIWSFreshDialInfo) (*openAIWSEdgeDialLease, error) {
		lease, bypass := registry.Acquire(ctx, key, time.Now())
		if bypass {
			return nil, errOpenAIWSEdgeBackoff
		}
		return lease, nil
	}
}

func TestOAuthReliabilityWSEdgePoolHookRunsAfterAuthRefresh(t *testing.T) {
	pool := oauthReliabilityEdgePool(t)
	account := &Account{ID: 9201, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	type contextKey struct{}
	var order []string
	var dialCtx context.Context
	pool.clientDialer = &oauthReliabilityEdgePoolDialer{dial: func(ctx context.Context, headers http.Header) (openAIWSClientConn, int, http.Header, error) {
		order = append(order, "dial")
		require.Equal(t, "current-bearer", headers.Get("Authorization"))
		require.Equal(t, "lease-context", ctx.Value(contextKey{}))
		dialCtx = ctx
		return &openAIWSFakeConn{}, 0, nil, nil
	}}
	headers := http.Header{"Authorization": {"stale-bearer"}}
	req := openAIWSAcquireRequest{Account: account, WSURL: "wss://fixture.invalid/responses", Headers: headers,
		HeadersFactory: func(_ context.Context, current http.Header) (http.Header, *Account, error) {
			order = append(order, "auth")
			current.Set("Authorization", "current-bearer")
			return current, account, nil
		},
		BeforeFreshDial: func(ctx context.Context, info openAIWSFreshDialInfo) (*openAIWSEdgeDialLease, error) {
			order = append(order, "hook")
			require.Equal(t, "current-bearer", info.Headers.Get("Authorization"))
			require.Same(t, pool.cfg, info.Config)
			require.Same(t, pool.clientDialer, info.Dialer)
			next, cancel := context.WithCancel(context.WithValue(ctx, contextKey{}, "lease-context"))
			return &openAIWSEdgeDialLease{ctx: next, cancel: cancel}, nil
		},
	}
	conn, err := pool.dialConn(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, []string{"auth", "hook", "dial"}, order)
	require.Equal(t, "stale-bearer", headers.Get("Authorization"))
	require.ErrorIs(t, dialCtx.Err(), context.Canceled, "Finish releases the lease context after successful handshake")
	conn.close()
}

func TestOAuthReliabilityWSEdgePoolBypassDoesNotCountAsFailure(t *testing.T) {
	pool := oauthReliabilityEdgePool(t)
	account := &Account{ID: 9202, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	registry := newOpenAIWSEdgeBackoff(openAIWSEdgeBackoffSettings{BaseDelay: time.Second, MaxDelay: time.Second})
	key := oauthReliabilityEdgeKey(t, oauthReliabilityEdgeScope(account.ID, account.ID, 1, 1, "direct", "target", "generation", "transport"))
	var dials atomic.Int32
	pool.clientDialer = &oauthReliabilityEdgePoolDialer{dial: func(context.Context, http.Header) (openAIWSClientConn, int, http.Header, error) {
		dials.Add(1)
		return nil, http.StatusForbidden, http.Header{"Cf-Mitigated": {"challenge"}}, &openAIWSHandshakeError{Body: []byte("<html>edge fixture</html>"), Err: errors.New("handshake fixture")}
	}}
	req := openAIWSAcquireRequest{Account: account, WSURL: "wss://fixture.invalid/responses", BeforeFreshDial: oauthReliabilityPoolEdgeHook(registry, key)}
	for i := 0; i < 2; i++ {
		lease, err := pool.Acquire(context.Background(), req)
		require.Nil(t, lease)
		var dialErr *openAIWSDialError
		require.True(t, errors.As(err, &dialErr))
		require.Equal(t, http.StatusForbidden, dialErr.StatusCode)
	}
	ap := pool.getOrCreateAccountPool(account.ID)
	ap.mu.Lock()
	before := ap.prewarmFails
	ap.mu.Unlock()
	lease, err := pool.Acquire(context.Background(), req)
	require.Nil(t, lease)
	require.ErrorIs(t, err, errOpenAIWSEdgeBackoff)
	var dialErr *openAIWSDialError
	require.False(t, errors.As(err, &dialErr), "local cooldown must not manufacture an upstream403")
	require.Equal(t, int32(2), dials.Load())
	ap.mu.Lock()
	after, creating := ap.prewarmFails, ap.creating
	ap.mu.Unlock()
	require.Equal(t, before, after)
	require.Zero(t, creating)
}

func TestOAuthReliabilityWSEdgePoolHealthyReuseBypassesHook(t *testing.T) {
	pool := oauthReliabilityEdgePool(t)
	account := &Account{ID: 9203, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	var hooks atomic.Int32
	req := openAIWSAcquireRequest{Account: account, WSURL: "wss://fixture.invalid/responses", BeforeFreshDial: func(context.Context, openAIWSFreshDialInfo) (*openAIWSEdgeDialLease, error) {
		hooks.Add(1)
		return nil, errOpenAIWSEdgeBackoff
	}}
	conn := newOpenAIWSConn("healthy-fixture", account.ID, &openAIWSFakeConn{}, nil)
	conn.handshakeCompatibility = normalizeOpenAIWSHandshakeCompatibility(account, req.Headers)
	ap := pool.getOrCreateAccountPool(account.ID)
	ap.mu.Lock()
	ap.conns[conn.id] = conn
	ap.mu.Unlock()
	lease, err := pool.Acquire(context.Background(), req)
	require.NoError(t, err)
	require.True(t, lease.Reused())
	require.Zero(t, hooks.Load())
	lease.Release()
	require.False(t, conn.isClosed())
}

func TestOAuthReliabilityWSEdgePoolPersistentSnapshotsDropOnlyDialHook(t *testing.T) {
	pool := oauthReliabilityEdgePool(t)
	account := &Account{ID: 9204, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	req := openAIWSAcquireRequest{Account: account, WSURL: "wss://fixture.invalid/responses", Headers: make(http.Header),
		HeadersFactory: func(_ context.Context, h http.Header) (http.Header, *Account, error) { return h, nil, nil },
		BeforeFreshDial: func(context.Context, openAIWSFreshDialInfo) (*openAIWSEdgeDialLease, error) {
			return nil, errOpenAIWSEdgeBackoff
		},
	}
	require.NotNil(t, cloneOpenAIWSAcquireRequest(req).BeforeFreshDial, "ordinary retry clones retain the request scope")
	ap := pool.getOrCreateAccountPool(account.ID)
	pool.recordLastSuccessfulAcquire(account.ID, ap.generation, req)
	ap.mu.Lock()
	stored := cloneOpenAIWSAcquireRequestPtr(ap.lastAcquire)
	ap.mu.Unlock()
	require.NotNil(t, stored)
	require.Nil(t, stored.BeforeFreshDial)
	require.NotNil(t, stored.HeadersFactory)
	req.Headers.Set(openAICookieWSGenerationHeader, "fixture-candidate-generation")
	pool.recordLastSuccessfulAcquire(account.ID, ap.generation, req)
	ap.mu.Lock()
	storedCandidates := ap.candidateAcquires
	ap.mu.Unlock()
	candidates := 0
	for _, candidate := range storedCandidates {
		if candidate != nil {
			candidates++
			require.Nil(t, candidate.BeforeFreshDial)
			require.NotNil(t, candidate.HeadersFactory)
		}
	}
	require.Equal(t, 1, candidates)
	require.NotNil(t, req.BeforeFreshDial)
	require.Nil(t, cloneOpenAIWSPersistentAcquireRequest(nil))
}

func TestOAuthReliabilityWSEdgePoolProbeContextAndFinish(t *testing.T) {
	for _, outcome := range []string{"success", "timeout", "nil_connection"} {
		t.Run(outcome, func(t *testing.T) {
			pool := oauthReliabilityEdgePool(t)
			account := &Account{ID: 9205, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
			registry := newOpenAIWSEdgeBackoff(openAIWSEdgeBackoffSettings{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, ProbeTimeout: 20 * time.Millisecond})
			key := oauthReliabilityEdgeKey(t, oauthReliabilityEdgeScope(account.ID, account.ID, 1, 1, "direct", "target", "generation", "transport"))
			past := time.Now().Add(-time.Second)
			for i := 0; i < 2; i++ {
				lease, bypass := registry.Acquire(context.Background(), key, past)
				require.False(t, bypass)
				lease.Finish(oauthReliabilityEdge403("<html>fixture</html>"), past)
			}
			pool.clientDialer = &oauthReliabilityEdgePoolDialer{dial: func(ctx context.Context, _ http.Header) (openAIWSClientConn, int, http.Header, error) {
				deadline, exists := ctx.Deadline()
				require.True(t, exists)
				require.LessOrEqual(t, time.Until(deadline), 20*time.Millisecond)
				if outcome == "timeout" {
					<-ctx.Done()
					return nil, 0, nil, ctx.Err()
				}
				if outcome == "nil_connection" {
					return nil, 0, nil, nil
				}
				return &openAIWSFakeConn{}, 0, nil, nil
			}}
			conn, err := pool.dialConn(context.Background(), openAIWSAcquireRequest{Account: account, WSURL: "wss://fixture.invalid/responses", BeforeFreshDial: oauthReliabilityPoolEdgeHook(registry, key)})
			if outcome == "success" {
				require.NoError(t, err)
				conn.close()
			} else {
				require.Error(t, err)
			}
			registry.mu.Lock()
			remaining := len(registry.entries)
			registry.mu.Unlock()
			require.Zero(t, remaining, "success/non-edge terminal outcome releases probe state")
		})
	}
}

func TestOAuthReliabilityWSEdgeDialScopeUsesActualRouteAndCredentials(t *testing.T) {
	account := &Account{ID: 9206, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "workspace-fixture", "chatgpt_user_id": "upstream-user-fixture"}}
	info := openAIWSFreshDialInfo{Request: openAIWSAcquireRequest{Account: account, WSURL: "wss://fixture.invalid/responses"}, Headers: http.Header{"Authorization": {"Bearer credential-fixture"}}, Config: &config.Config{}, Dialer: newDefaultOpenAIWSClientDialer()}
	key, ok := openAIWSEdgeDialKey(context.Background(), info, account, 11, 22, 33, "model-fixture")
	require.True(t, ok)
	for _, tc := range []string{"user", "apikey", "group", "model", "target", "proxy", "credential", "transport", "owner"} {
		next := info
		next.Headers = info.Headers.Clone()
		user, apiKey, group, model, owner := int64(11), int64(22), int64(33), "model-fixture", account
		switch tc {
		case "user":
			user++
		case "apikey":
			apiKey++
		case "group":
			group++
		case "model":
			model = "other-model"
		case "target":
			next.Request.WSURL = "wss://fixture.invalid/other"
		case "proxy":
			next.Request.ProxyURL = "http://proxy-user:proxy-password@proxy.invalid:8080"
		case "credential":
			next.Headers.Set("Authorization", "Bearer rotated-fixture")
		case "transport":
			next.Headers.Set("User-Agent", "changed-wire-profile")
		case "owner":
			copyOwner := *account
			copyOwner.ID++
			owner = &copyOwner
		}
		changed, ok := openAIWSEdgeDialKey(context.Background(), next, owner, user, apiKey, group, model)
		require.True(t, ok, tc)
		require.NotEqual(t, key, changed, tc)
	}
	unknown := info
	unknown.Dialer = &openAIWSFakeDialer{}
	_, ok = openAIWSEdgeDialKey(context.Background(), unknown, account, 11, 22, 33, "model-fixture")
	require.False(t, ok, "unknown custom dialer must not be inferred direct")
	proxyID := int64(7)
	account.ProxyID = &proxyID
	_, ok = openAIWSEdgeDialKey(context.Background(), info, account, 11, 22, 33, "model-fixture")
	require.False(t, ok, "configured but unresolved proxy must not become direct")
	account.ProxyID = nil
	info.Headers.Del("Authorization")
	_, ok = openAIWSEdgeDialKey(context.Background(), info, account, 11, 22, 33, "model-fixture")
	require.False(t, ok)
	for _, secret := range []string{"credential-fixture", "workspace-fixture", "fixture.invalid", "proxy-password"} {
		require.NotContains(t, fmt.Sprintf("%x", key), secret)
	}
}

func TestOAuthReliabilityConcurrentWSEdgeRegistryIsServiceScoped(t *testing.T) {
	svc := &OpenAIGatewayService{}
	const callers = 32
	results := make(chan *openAIWSEdgeBackoff, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- svc.getOpenAIWSEdgeBackoff() }()
	}
	wg.Wait()
	close(results)
	want := svc.getOpenAIWSEdgeBackoff()
	for got := range results {
		require.Same(t, want, got)
	}
	other := &OpenAIGatewayService{}
	require.NotSame(t, want, other.getOpenAIWSEdgeBackoff())
	require.Nil(t, svc.openAIWSEdgeDialHook(nil, "model", true))
	require.Nil(t, svc.openAIWSEdgeDialHook(&APIKey{ID: 1, UserID: 2}, "model", false))
	require.Nil(t, svc.openAIWSEdgeDialHook(&APIKey{ID: 1, UserID: 2}, "", true))
}

func TestOAuthReliabilityWSEdgeHookUsesRefreshedCredentialGeneration(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 9207, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	hook := svc.openAIWSEdgeDialHook(&APIKey{ID: 22, UserID: 11}, "model-fixture", true)
	require.NotNil(t, hook)
	info := openAIWSFreshDialInfo{Request: openAIWSAcquireRequest{Account: account, WSURL: "wss://fixture.invalid/responses"}, Headers: http.Header{"Authorization": {"Bearer initial-fixture"}}, Config: &config.Config{}, Dialer: newDefaultOpenAIWSClientDialer()}
	for i := 0; i < 2; i++ {
		lease, err := hook(context.Background(), info)
		require.NoError(t, err)
		require.NotNil(t, lease)
		lease.Finish(oauthReliabilityEdge403("<html>edge fixture</html>"), time.Now())
	}
	lease, err := hook(context.Background(), info)
	require.Nil(t, lease)
	require.ErrorIs(t, err, errOpenAIWSEdgeBackoff)
	info.Headers.Set("Authorization", "Bearer refreshed-fixture")
	lease, err = hook(context.Background(), info)
	require.NoError(t, err)
	require.NotNil(t, lease)
	lease.Finish(nil, time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lease, err = hook(ctx, info)
	require.Nil(t, lease)
	require.ErrorIs(t, err, context.Canceled)
	info.Request.ForcePreferredConn = true
	lease, err = hook(context.Background(), info)
	require.NoError(t, err)
	require.Nil(t, lease, "connection-bound continuation must not take a fresh-dial bypass")
}
