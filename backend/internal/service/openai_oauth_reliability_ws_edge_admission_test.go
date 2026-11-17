//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func oauthReliabilityEdgeReplacementFixture(t *testing.T, mode string) (*openAIWSConnPool, *openAIWSConn, openAIWSAcquireRequest) {
	t.Helper()
	pool := oauthReliabilityEdgePool(t)
	account := &Account{ID: 9301, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	req := openAIWSAcquireRequest{Account: account, WSURL: "wss://fixture.invalid/responses", Headers: make(http.Header)}
	healthy := newOpenAIWSConn("healthy-existing", account.ID, &openAIWSFakeConn{}, nil)
	healthy.handshakeCompatibility = normalizeOpenAIWSHandshakeCompatibility(account, req.Headers)
	if mode == "force_new" {
		req.ForceNewConn = true
	} else {
		req.Headers.Set("X-Codex-Beta-Features", "synthetic-new-transport")
		require.False(t, healthy.matchesHandshakeCompatibility(normalizeOpenAIWSHandshakeCompatibility(account, req.Headers)))
	}
	ap := pool.getOrCreateAccountPool(account.ID)
	ap.mu.Lock()
	ap.conns[healthy.id] = healthy
	ap.mu.Unlock()
	return pool, healthy, req
}

func oauthReliabilityRequireHealthyReplacementRetained(t *testing.T, pool *openAIWSConnPool, healthy *openAIWSConn, req openAIWSAcquireRequest) {
	t.Helper()
	require.False(t, healthy.isClosed())
	require.False(t, healthy.isLeased(), "temporary replacement reservation must be released on every denied exit")
	ap := pool.getOrCreateAccountPool(req.Account.ID)
	ap.mu.Lock()
	kept, creating, count := ap.conns[healthy.id], ap.creating, len(ap.conns)
	ap.mu.Unlock()
	require.Same(t, healthy, kept)
	require.Equal(t, 1, count)
	require.Zero(t, creating)
	require.Zero(t, pool.metrics.scaleDownTotal.Load())

	// Prove that the original client can actually acquire the surviving socket.
	req.ForceNewConn = false
	req.Headers.Del("X-Codex-Beta-Features")
	req.HeadersFactory = nil
	req.BeforeFreshDial = nil
	lease, err := pool.Acquire(context.Background(), req)
	require.NoError(t, err)
	require.True(t, lease.Reused())
	require.Equal(t, healthy.id, lease.ConnID())
	lease.Release()
}

func TestOAuthReliabilityWSEdgeAdmissionRetainsHealthyCapacity(t *testing.T) {
	for _, mode := range []string{"force_new", "incompatible"} {
		t.Run(mode, func(t *testing.T) {
			pool, healthy, req := oauthReliabilityEdgeReplacementFixture(t, mode)
			registry := newOpenAIWSEdgeBackoff(openAIWSEdgeBackoffSettings{})
			key := oauthReliabilityEdgeKey(t, oauthReliabilityEdgeScope(req.Account.ID, req.Account.ID, 1, 1, "direct", "target", "generation", "transport"))
			for i := 0; i < 2; i++ {
				lease, bypass := registry.Acquire(context.Background(), key, time.Now())
				require.False(t, bypass)
				lease.Finish(oauthReliabilityEdge403("<html>Cloudflare challenge</html>"), time.Now())
			}
			var dials atomic.Int32
			pool.clientDialer = &oauthReliabilityEdgePoolDialer{dial: func(context.Context, http.Header) (openAIWSClientConn, int, http.Header, error) {
				dials.Add(1)
				return &openAIWSFakeConn{}, 0, nil, nil
			}}
			req.BeforeFreshDial = oauthReliabilityPoolEdgeHook(registry, key)
			lease, err := pool.Acquire(context.Background(), req)
			require.Nil(t, lease)
			require.ErrorIs(t, err, errOpenAIWSEdgeBackoff)
			require.Zero(t, dials.Load())
			oauthReliabilityRequireHealthyReplacementRetained(t, pool, healthy, req)
			ap := pool.getOrCreateAccountPool(req.Account.ID)
			ap.mu.Lock()
			failures := ap.prewarmFails
			ap.mu.Unlock()
			require.Zero(t, failures, "a local bypass is not a failed upstream handshake")
		})
	}
}

func TestOAuthReliabilityWSEdgeAdmissionFailureReleasesReplacement(t *testing.T) {
	for _, mode := range []string{"force_new", "incompatible"} {
		for _, outcome := range []string{"factory_error", "hook_error_with_lease", "cancel_after_hook", "pinned_during_hook"} {
			t.Run(mode+"/"+outcome, func(t *testing.T) {
				pool, healthy, req := oauthReliabilityEdgeReplacementFixture(t, mode)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				failure := errors.New("synthetic admission failure")
				registry := newOpenAIWSEdgeBackoff(openAIWSEdgeBackoffSettings{})
				key := oauthReliabilityEdgeKey(t, oauthReliabilityEdgeScope(req.Account.ID, req.Account.ID, 1, 1, "direct", "target", "generation", "transport"))
				var dials atomic.Int32
				pool.clientDialer = &oauthReliabilityEdgePoolDialer{dial: func(context.Context, http.Header) (openAIWSClientConn, int, http.Header, error) {
					dials.Add(1)
					return &openAIWSFakeConn{}, 0, nil, nil
				}}
				req.BeforeFreshDial = func(ctx context.Context, _ openAIWSFreshDialInfo) (*openAIWSEdgeDialLease, error) {
					lease, bypass := registry.Acquire(ctx, key, time.Now())
					require.False(t, bypass)
					switch outcome {
					case "hook_error_with_lease":
						return lease, failure
					case "cancel_after_hook":
						cancel()
					case "pinned_during_hook":
						require.True(t, pool.PinConn(req.Account.ID, healthy.id))
					}
					return lease, nil
				}
				if outcome == "factory_error" {
					req.HeadersFactory = func(context.Context, http.Header) (http.Header, error) { return nil, failure }
				}
				lease, err := pool.Acquire(ctx, req)
				require.Nil(t, lease)
				if outcome == "cancel_after_hook" {
					require.ErrorIs(t, err, context.Canceled)
				} else if outcome == "pinned_during_hook" {
					require.ErrorIs(t, err, errOpenAIWSConnQueueFull)
					pool.UnpinConn(req.Account.ID, healthy.id)
				} else {
					require.ErrorIs(t, err, failure)
				}
				require.Zero(t, dials.Load())
				registry.mu.Lock()
				remaining := len(registry.entries)
				registry.mu.Unlock()
				require.Zero(t, remaining, "an acquired admission lease must finish even if dialing never begins")
				oauthReliabilityRequireHealthyReplacementRetained(t, pool, healthy, req)
			})
		}
	}
}

func TestOAuthReliabilityWSEdgeAdmissionAllowsHealthyReplacement(t *testing.T) {
	for _, mode := range []string{"force_new", "incompatible"} {
		t.Run(mode, func(t *testing.T) {
			pool, healthy, req := oauthReliabilityEdgeReplacementFixture(t, mode)
			registry := newOpenAIWSEdgeBackoff(openAIWSEdgeBackoffSettings{})
			key := oauthReliabilityEdgeKey(t, oauthReliabilityEdgeScope(req.Account.ID, req.Account.ID, 1, 1, "direct", "target", "generation", "transport"))
			var dials atomic.Int32
			pool.clientDialer = &oauthReliabilityEdgePoolDialer{dial: func(context.Context, http.Header) (openAIWSClientConn, int, http.Header, error) {
				dials.Add(1)
				require.True(t, healthy.isClosed(), "the admitted replacement must retire its predecessor before dialing")
				return &openAIWSFakeConn{}, 0, nil, nil
			}}
			req.BeforeFreshDial = func(ctx context.Context, info openAIWSFreshDialInfo) (*openAIWSEdgeDialLease, error) {
				require.False(t, healthy.isClosed(), "admission must precede healthy connection eviction")
				return oauthReliabilityPoolEdgeHook(registry, key)(ctx, info)
			}
			lease, err := pool.Acquire(context.Background(), req)
			require.NoError(t, err)
			require.False(t, lease.Reused())
			require.NotEqual(t, healthy.id, lease.ConnID())
			require.Equal(t, int32(1), dials.Load())
			lease.Release()
			ap := pool.getOrCreateAccountPool(req.Account.ID)
			ap.mu.Lock()
			creating, count := ap.creating, len(ap.conns)
			ap.mu.Unlock()
			require.Zero(t, creating)
			require.Equal(t, 1, count)
			require.EqualValues(t, 1, pool.metrics.scaleDownTotal.Load())
		})
	}
}

func TestOAuthReliabilityConcurrentWSEdgeAdmissionReservation(t *testing.T) {
	pool, healthy, req := oauthReliabilityEdgeReplacementFixture(t, "force_new")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entered, decide := make(chan struct{}), make(chan struct{})
	req.BeforeFreshDial = func(ctx context.Context, _ openAIWSFreshDialInfo) (*openAIWSEdgeDialLease, error) {
		close(entered)
		select {
		case <-decide:
			return nil, errOpenAIWSEdgeBackoff
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var dials atomic.Int32
	pool.clientDialer = &oauthReliabilityEdgePoolDialer{dial: func(context.Context, http.Header) (openAIWSClientConn, int, http.Header, error) {
		dials.Add(1)
		return &openAIWSFakeConn{}, 0, nil, nil
	}}
	type outcome struct {
		lease *openAIWSConnLease
		err   error
	}
	first := make(chan outcome, 1)
	go func() {
		lease, err := pool.Acquire(ctx, req)
		first <- outcome{lease, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("fresh admission was not reached")
	}
	other := req
	other.ForceNewConn, other.BeforeFreshDial = false, nil
	second := make(chan outcome, 1)
	go func() {
		lease, err := pool.Acquire(ctx, other)
		second <- outcome{lease, err}
	}()
	require.Eventually(t, func() bool { return healthy.waiters.Load() > 0 }, time.Second, time.Millisecond, "the existing client must queue on the still-live reserved connection")
	require.False(t, healthy.isClosed())
	close(decide)
	result := <-first
	require.Nil(t, result.lease)
	require.ErrorIs(t, result.err, errOpenAIWSEdgeBackoff)
	result = <-second
	require.NoError(t, result.err)
	require.NotNil(t, result.lease)
	require.True(t, result.lease.Reused())
	require.Equal(t, healthy.id, result.lease.ConnID())
	result.lease.Release()
	require.Zero(t, dials.Load())
	oauthReliabilityRequireHealthyReplacementRetained(t, pool, healthy, req)
}
