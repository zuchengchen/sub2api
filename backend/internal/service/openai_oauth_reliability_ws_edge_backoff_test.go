//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func oauthReliabilityEdgeScope(account, owner, user, apiKey int64, egress, target, generation, transport string) openAIWSEdgeBackoffScope {
	return openAIWSEdgeBackoffScope{
		AccountID: account, CredentialOwnerID: owner, UserID: user, APIKeyID: apiKey,
		GroupID: 17, CredentialNamespace: "oauth-owner", Egress: egress, Target: target,
		CredentialGeneration: generation, Transport: transport,
	}
}

func oauthReliabilityEdge403(body string) error {
	return &openAIWSDialError{
		StatusCode: http.StatusForbidden,
		ResponseHeaders: http.Header{
			"Cf-Mitigated": {"challenge"},
			"Server":       {"cloudflare"},
		},
		ResponseBody: []byte(body),
		Err:          errors.New("websocket handshake rejected"),
	}
}

func oauthReliabilityStructured403() error {
	return &openAIWSDialError{
		StatusCode:   http.StatusForbidden,
		ResponseBody: []byte(`{"error":{"type":"permission_error","code":"account_forbidden","message":"forbidden"}}`),
		ResponseHeaders: http.Header{
			"Cf-Mitigated": {"challenge"},
		},
		Err: errors.New("websocket handshake rejected"),
	}
}

func oauthReliabilityEdgeKey(t *testing.T, scope openAIWSEdgeBackoffScope) openAIWSEdgeBackoffKey {
	t.Helper()
	key, ok := makeOpenAIWSEdgeBackoffKey(scope)
	require.True(t, ok)
	return key
}

func TestOAuthReliabilityConcurrentWSEdgeBackoffThresholdAndSingleProbe(t *testing.T) {
	base := 200 * time.Millisecond
	registry := newOpenAIWSEdgeBackoff(openAIWSEdgeBackoffSettings{
		FailureWindow: time.Minute, BaseDelay: base, MaxDelay: time.Second,
		JitterRatio: 0.2, ProbeTimeout: 100 * time.Millisecond, MaxEntries: 8,
	})
	key := oauthReliabilityEdgeKey(t, oauthReliabilityEdgeScope(1, 1, 101, 1001, "proxy:17", "chatgpt.example/v1/responses", "credential-v1", "codex-h2"))
	now := time.Unix(1_700_000_000, 0)

	first, bypass := registry.Acquire(context.Background(), key, now)
	require.False(t, bypass)
	require.NotNil(t, first)
	first.Finish(oauthReliabilityEdge403("<html>edge block one</html>"), now)
	second, bypass := registry.Acquire(context.Background(), key, now.Add(time.Second))
	require.False(t, bypass)
	require.NotNil(t, second)
	second.Finish(oauthReliabilityEdge403("<html>edge block two</html>"), now.Add(time.Second))

	blocked, bypass := registry.Acquire(context.Background(), key, now.Add(time.Second+time.Nanosecond))
	require.True(t, bypass)
	require.Nil(t, blocked)
	entry := registry.entries[key]
	require.NotNil(t, entry)
	require.True(t, entry.blockedUntil.After(now.Add(time.Second+time.Nanosecond)))
	delay := entry.blockedUntil.Sub(now.Add(time.Second))
	require.GreaterOrEqual(t, delay, 160*time.Millisecond)
	require.LessOrEqual(t, delay, 240*time.Millisecond)

	probeAt := entry.blockedUntil.Add(time.Nanosecond)
	probe, bypass := registry.Acquire(context.Background(), key, probeAt)
	require.False(t, bypass)
	require.NotNil(t, probe, "one half-open probe is admitted after TTL")
	secondProbe, bypass := registry.Acquire(context.Background(), key, probeAt)
	require.True(t, bypass, "concurrent callers share the existing probe decision")
	require.Nil(t, secondProbe)
	_, hasDeadline := probe.Context().Deadline()
	require.True(t, hasDeadline, "probe uses a bounded dial context")

	probe.Finish(nil, probeAt)
	fresh, bypass := registry.Acquire(context.Background(), key, probeAt.Add(time.Millisecond))
	require.False(t, bypass)
	require.NotNil(t, fresh, "probe success clears the short backoff")
	fresh.Finish(nil, probeAt.Add(time.Millisecond))
}

func TestOAuthReliabilityWSEdgeBackoffStructuredForbiddenAndCancellationDoNotCount(t *testing.T) {
	registry := newOpenAIWSEdgeBackoff(openAIWSEdgeBackoffSettings{BaseDelay: time.Second, MaxDelay: time.Second, ProbeTimeout: time.Second})
	key := oauthReliabilityEdgeKey(t, oauthReliabilityEdgeScope(2, 2, 202, 2002, "direct:egress-1", "chatgpt.example/v1/responses", "credential-v2", "codex-h2"))
	now := time.Unix(1_700_000_100, 0)

	for i := 0; i < 3; i++ {
		lease, bypass := registry.Acquire(context.Background(), key, now.Add(time.Duration(i)*time.Second))
		require.False(t, bypass)
		require.NotNil(t, lease)
		lease.Finish(oauthReliabilityStructured403(), now.Add(time.Duration(i)*time.Second))
	}
	lease, bypass := registry.Acquire(context.Background(), key, now.Add(4*time.Second))
	require.False(t, bypass)
	require.NotNil(t, lease, "structured account 403 must remain on existing policy path")
	lease.Finish(nil, now.Add(4*time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	canceled, bypass := registry.Acquire(ctx, key, now.Add(5*time.Second))
	require.False(t, bypass)
	require.NotNil(t, canceled)
	cancel()
	canceled.Finish(oauthReliabilityEdge403("<html>cancelled edge block</html>"), now.Add(5*time.Second))
	for i := 0; i < 2; i++ {
		lease, bypass := registry.Acquire(context.Background(), key, now.Add(time.Duration(6+i)*time.Second))
		require.False(t, bypass)
		require.NotNil(t, lease, "a canceled request must not consume a threshold strike")
		lease.Finish(oauthReliabilityEdge403("<html>edge block</html>"), now.Add(time.Duration(6+i)*time.Second))
	}
	blocked, bypass := registry.Acquire(context.Background(), key, now.Add(7*time.Second+time.Nanosecond))
	require.True(t, bypass)
	require.Nil(t, blocked)
}

func TestOAuthReliabilityWSEdgeBackoffScopesDoNotCrossInterfere(t *testing.T) {
	registry := newOpenAIWSEdgeBackoff(openAIWSEdgeBackoffSettings{BaseDelay: time.Second, MaxDelay: time.Second, ProbeTimeout: time.Second})
	base := oauthReliabilityEdgeScope(3, 3, 303, 3003, "proxy:17", "chatgpt.example/v1/responses", "credential-v3", "codex-h2")
	variants := []openAIWSEdgeBackoffScope{
		base,
		func() (v openAIWSEdgeBackoffScope) { v = base; v.AccountID++; return v }(),
		func() (v openAIWSEdgeBackoffScope) { v = base; v.UserID++; return v }(),
		func() (v openAIWSEdgeBackoffScope) { v = base; v.APIKeyID++; return v }(),
		func() (v openAIWSEdgeBackoffScope) { v = base; v.Egress = "proxy:18"; return v }(),
		func() (v openAIWSEdgeBackoffScope) { v = base; v.Target = "chatgpt.example/v1/compact"; return v }(),
		func() (v openAIWSEdgeBackoffScope) { v = base; v.CredentialGeneration = "credential-v4"; return v }(),
		func() (v openAIWSEdgeBackoffScope) { v = base; v.Transport = "codex-http1"; return v }(),
	}
	keys := make([]openAIWSEdgeBackoffKey, 0, len(variants))
	for _, variant := range variants {
		keys = append(keys, oauthReliabilityEdgeKey(t, variant))
	}
	for i := 1; i < len(keys); i++ {
		require.NotEqual(t, keys[0], keys[i])
	}
	for _, secret := range []string{"credential-v3", "proxy:17", "chatgpt.example/v1/responses", "oauth-owner"} {
		require.NotContains(t, fmt.Sprintf("%x", keys[0]), secret)
	}

	now := time.Unix(1_700_000_200, 0)
	for i := 0; i < 2; i++ {
		lease, bypass := registry.Acquire(context.Background(), keys[0], now.Add(time.Duration(i)*time.Second))
		require.False(t, bypass)
		lease.Finish(oauthReliabilityEdge403("<html>edge block</html>"), now.Add(time.Duration(i)*time.Second))
	}
	for _, key := range keys[1:] {
		lease, bypass := registry.Acquire(context.Background(), key, now.Add(time.Second+time.Nanosecond))
		require.False(t, bypass)
		require.NotNil(t, lease)
		lease.Finish(nil, now.Add(time.Second+time.Nanosecond))
	}
	blocked, bypass := registry.Acquire(context.Background(), keys[0], now.Add(time.Second+2*time.Nanosecond))
	require.True(t, bypass)
	require.Nil(t, blocked)
}

func TestOAuthReliabilityWSEdgeBackoffWindowJitterAndCapacityBound(t *testing.T) {
	registry := newOpenAIWSEdgeBackoff(openAIWSEdgeBackoffSettings{
		FailureWindow: 10 * time.Second, BaseDelay: 500 * time.Millisecond, MaxDelay: 700 * time.Millisecond,
		JitterRatio: 0.1, ProbeTimeout: time.Second, MaxEntries: 2,
	})
	now := time.Unix(1_700_000_300, 0)
	keys := make([]openAIWSEdgeBackoffKey, 3)
	for i := range keys {
		keys[i] = oauthReliabilityEdgeKey(t, oauthReliabilityEdgeScope(int64(10+i), int64(10+i), int64(400+i), int64(4000+i), "direct:egress", "chatgpt.example/v1/responses", "gen", "transport"))
	}
	for i := 0; i < 2; i++ {
		lease, bypass := registry.Acquire(context.Background(), keys[i], now.Add(time.Duration(i)*time.Second))
		require.False(t, bypass)
		lease.Finish(oauthReliabilityEdge403("<html>edge block</html>"), now.Add(time.Duration(i)*time.Second))
	}
	require.LessOrEqual(t, len(registry.entries), 2)
	lease, bypass := registry.Acquire(context.Background(), keys[2], now.Add(3*time.Second))
	require.False(t, bypass, "capacity pressure fails open for an untracked key")
	require.NotNil(t, lease)
	lease.Finish(nil, now.Add(3*time.Second))
	require.LessOrEqual(t, len(registry.entries), 2)

	// The first entry's failure window expires before its second edge event;
	// it must not inherit a strike from the old window.
	key := oauthReliabilityEdgeKey(t, oauthReliabilityEdgeScope(20, 20, 420, 4020, "direct:egress-2", "chatgpt.example/v1/responses", "gen", "transport"))
	first, bypass := registry.Acquire(context.Background(), key, now)
	require.False(t, bypass)
	first.Finish(oauthReliabilityEdge403("<html>edge block</html>"), now)
	second, bypass := registry.Acquire(context.Background(), key, now.Add(11*time.Second))
	require.False(t, bypass)
	require.NotNil(t, second)
	second.Finish(oauthReliabilityEdge403("<html>edge block</html>"), now.Add(11*time.Second))
	third, bypass := registry.Acquire(context.Background(), key, now.Add(12*time.Second))
	require.False(t, bypass, "one failure in the new window must not trip threshold two")
	require.NotNil(t, third)
	third.Finish(nil, now.Add(12*time.Second))
}

func TestOAuthReliabilityConcurrentWSEdgeBackoffProbeAdmissionIsAtomic(t *testing.T) {
	registry := newOpenAIWSEdgeBackoff(openAIWSEdgeBackoffSettings{BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, ProbeTimeout: time.Second})
	key := oauthReliabilityEdgeKey(t, oauthReliabilityEdgeScope(30, 30, 430, 4030, "proxy:30", "chatgpt.example/v1/responses", "gen", "transport"))
	now := time.Unix(1_700_000_400, 0)
	for i := 0; i < 2; i++ {
		lease, bypass := registry.Acquire(context.Background(), key, now.Add(time.Duration(i)*time.Second))
		require.False(t, bypass)
		lease.Finish(oauthReliabilityEdge403("<html>edge block</html>"), now.Add(time.Duration(i)*time.Second))
	}
	entry := registry.entries[key]
	probeAt := entry.blockedUntil.Add(time.Nanosecond)
	const callers = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	probes := 0
	bypasses := 0
	var probeLease *openAIWSEdgeDialLease
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, bypass := registry.Acquire(context.Background(), key, probeAt)
			mu.Lock()
			defer mu.Unlock()
			if bypass {
				bypasses++
			}
			if lease != nil {
				probes++
				probeLease = lease
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 1, probes)
	require.Equal(t, callers-1, bypasses)
	require.NotNil(t, probeLease)
	probeLease.Finish(nil, probeAt)
}

func TestOAuthReliabilityWSEdgeBackoffMissingAttributionFailsOpen(t *testing.T) {
	for _, scope := range []openAIWSEdgeBackoffScope{
		{AccountID: 1, CredentialOwnerID: 1, UserID: 1, APIKeyID: 1, GroupID: 1, Egress: "unknown", Target: "target", CredentialGeneration: "gen", Transport: "transport"},
		{AccountID: 1, CredentialOwnerID: 1, UserID: 1, APIKeyID: 1, GroupID: 1, Egress: "direct", Target: "target", CredentialGeneration: "", Transport: "transport"},
		{AccountID: 1, CredentialOwnerID: 1, UserID: 1, APIKeyID: 1, GroupID: 1, Egress: "direct", Target: "target", CredentialGeneration: "gen", Transport: ""},
		{AccountID: 1, CredentialOwnerID: 1, UserID: 1, APIKeyID: 1, GroupID: 1, Egress: "direct", Target: "target", CredentialGeneration: "gen", Transport: "transport"},
	} {
		_, ok := makeOpenAIWSEdgeBackoffKey(scope)
		if scope.Egress == "direct" && scope.CredentialGeneration == "gen" && scope.Transport == "transport" {
			require.True(t, ok)
		} else {
			require.False(t, ok)
		}
	}
}
