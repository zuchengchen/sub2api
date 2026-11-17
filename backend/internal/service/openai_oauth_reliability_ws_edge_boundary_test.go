//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const oauthReliabilityEdgeBoundaryBody = `{"model":"gpt-5.5","stream":false,"input":[{"role":"user","content":"synthetic boundary request"}]}`
const oauthReliabilityEdgeBoundaryCompleted = `{"type":"response.completed","response":{"id":"resp_boundary_done","model":"gpt-5.5","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":1}}}`

type oauthReliabilityEdgeBoundaryOutcome struct {
	result             *OpenAIForwardResult
	err                error
	fallback, disabled string
	body               string
	status             int
}

// Use the built-in websocket dialer, so this exercises the actual fingerprint,
// HeadersFactory, handshake parser, pool admission and Forward fallback path.
func oauthReliabilityEdgeBoundaryFixture(t *testing.T, upstreamURL string) (*OpenAIGatewayService, *Account, *httpUpstreamSequenceRecorder) {
	t.Helper()
	svc, account, upstream := httpBridgeFixture(t, upstreamURL)
	target, err := url.Parse(upstreamURL)
	require.NoError(t, err)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	t.Cleanup(transport.CloseIdleConnections)
	dialer := newDefaultOpenAIWSClientDialer().(*coderOpenAIWSClientDialer)
	dialer.directOnce.Do(func() {
		dialer.directClient = &http.Client{Transport: &oauthReliabilityEdgeLocalTransport{target, transport}}
	})
	svc.openaiWSPool.setClientDialerForTest(dialer)
	svc.cfg.Gateway.OpenAIWS.DeltaContextEnabled = false
	return svc, account, upstream
}

func oauthReliabilityEdgeBoundaryForward(ctx context.Context, svc *OpenAIGatewayService, account *Account, session string, body []byte) oauthReliabilityEdgeBoundaryOutcome {
	c, rec := httpBridgeContext(session)
	c.Set("api_key", &APIKey{ID: 901, UserID: 902})
	c.Request = c.Request.WithContext(ctx)
	result, err := svc.Forward(ctx, c, account, body)
	return oauthReliabilityEdgeBoundaryOutcome{
		result: result, err: err, fallback: c.GetString("openai_ws_session_fallback_reason"),
		disabled: svc.openAIWSSessionDisabledReason(resolveOpenAIWSSessionScope(c, account, body)),
		body:     rec.Body.String(), status: rec.Code,
	}
}

func oauthReliabilityEdgeBoundaryChallenge(w http.ResponseWriter) {
	w.Header().Set("Cf-Mitigated", "challenge")
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte("<html>synthetic edge challenge</html>"))
}

func oauthReliabilityEdgeBoundaryPrime(t *testing.T, ctx context.Context, svc *OpenAIGatewayService, account *Account) openAIWSEdgeBackoffKey {
	t.Helper()
	for i := 0; i < 2; i++ {
		out := oauthReliabilityEdgeBoundaryForward(ctx, svc, account, "boundary-prime", []byte(oauthReliabilityEdgeBoundaryBody))
		require.NoError(t, out.err)
		require.NotNil(t, out.result)
		require.Equal(t, openAIWSUpstreamForbiddenReason, out.fallback)
		require.Empty(t, out.disabled)
	}
	registry := svc.getOpenAIWSEdgeBackoff()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	require.Len(t, registry.entries, 1)
	for key, entry := range registry.entries {
		require.True(t, entry.blockedUntil.After(time.Now()))
		return key
	}
	t.Fatal("two real edge handshakes did not produce a cooldown")
	return openAIWSEdgeBackoffKey{}
}

func oauthReliabilityEdgeBoundaryExpire(t *testing.T, registry *openAIWSEdgeBackoff, key openAIWSEdgeBackoffKey) {
	t.Helper()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	entry := registry.entries[key]
	require.NotNil(t, entry)
	// Advance only the test's cooldown clock; no timing sleep or upstream call.
	entry.blockedUntil = time.Now().Add(-time.Second)
}

func oauthReliabilityEdgeBoundaryEntryCount(registry *openAIWSEdgeBackoff) int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return len(registry.entries)
}

func TestOAuthReliabilityWSEdgeForwardStructured403DoesNotAcquireCooldown(t *testing.T) {
	for _, body := range []string{
		`{"error":{"type":"permission_error","code":"account_forbidden","message":"forbidden"}}`,
		`{"error":{"type":"permission_error","code":"account_forbidden","message":"cloudflare denied this account"}}`,
	} {
		t.Run(fmt.Sprintf("mentions_edge_%v", gjson.Get(body, "error.message").String() != "forbidden"), func(t *testing.T) {
			var handshakes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				handshakes.Add(1)
				w.Header().Set("Cf-Mitigated", "challenge")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			svc, account, upstream := oauthReliabilityEdgeBoundaryFixture(t, server.URL)
			defer svc.openaiWSPool.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for i := 0; i < 3; i++ {
				out := oauthReliabilityEdgeBoundaryForward(ctx, svc, account, "structured-403", []byte(oauthReliabilityEdgeBoundaryBody))
				require.NoError(t, out.err, "the existing per-request HTTP fallback is unchanged")
				require.NotNil(t, out.result)
				require.Equal(t, openAIWSUpstreamForbiddenReason, out.fallback)
				require.Empty(t, out.disabled)
			}
			require.Equal(t, int32(3), handshakes.Load(), "structured permission failures cannot suppress a later dial")
			require.Equal(t, 3, upstream.callCount)
			require.Zero(t, oauthReliabilityEdgeBoundaryEntryCount(svc.getOpenAIWSEdgeBackoff()))
		})
	}
}

func TestOAuthReliabilityWSEdgeForwardProgressNeverFallsBackEvenWithActiveCooldown(t *testing.T) {
	for _, progress := range []struct {
		name, event           string
		input, output, cached int
	}{
		{"usage_only", `{"type":"response.in_progress","response":{"id":"resp_boundary_partial","model":"gpt-5.5","usage":{"input_tokens":11,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}}`, 11, 3, 2},
		{"hosted_tool", `{"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","id":"ws_boundary_partial","status":"completed","action":{"type":"search","query":"synthetic lookup"}}}`, 0, 0, 0},
	} {
		for _, failure := range []string{"disconnect", "capacity_error"} {
			t.Run(progress.name+"/"+failure, func(t *testing.T) {
				var healthy atomic.Bool
				var handshakes, frames atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					handshakes.Add(1)
					if !healthy.Load() {
						oauthReliabilityEdgeBoundaryChallenge(w)
						return
					}
					conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					for {
						if _, _, err := conn.ReadMessage(); err != nil {
							return
						}
						if frames.Add(1) != 2 {
							if conn.WriteMessage(websocket.TextMessage, []byte(oauthReliabilityEdgeBoundaryCompleted)) != nil {
								return
							}
							continue
						}
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_boundary_partial","model":"gpt-5.5"}}`))
						_ = conn.WriteMessage(websocket.TextMessage, []byte(progress.event))
						if failure == "capacity_error" {
							_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"Our servers are currently overloaded. Please try again later."}}`))
						}
						return
					}
				}))
				defer server.Close()
				svc, account, upstream := oauthReliabilityEdgeBoundaryFixture(t, server.URL)
				defer svc.openaiWSPool.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				key := oauthReliabilityEdgeBoundaryPrime(t, ctx, svc, account)
				registry := svc.getOpenAIWSEdgeBackoff()
				oauthReliabilityEdgeBoundaryExpire(t, registry, key)
				healthy.Store(true)
				seed := oauthReliabilityEdgeBoundaryForward(ctx, svc, account, "boundary-progress", []byte(oauthReliabilityEdgeBoundaryBody))
				require.NoError(t, seed.err)
				require.NotNil(t, seed.result)
				require.Empty(t, seed.fallback)
				require.Zero(t, oauthReliabilityEdgeBoundaryEntryCount(registry))
				// Other requests may trip this exact scope while our existing
				// healthy socket stays reusable. Re-arm that state without asking
				// this logical inference to perform extra model executions.
				for i := 0; i < 2; i++ {
					lease, bypass := registry.Acquire(ctx, key, time.Now())
					require.False(t, bypass)
					lease.Finish(oauthReliabilityEdge403("<html>synthetic challenge</html>"), time.Now())
				}
				out := oauthReliabilityEdgeBoundaryForward(ctx, svc, account, "boundary-progress", []byte(oauthReliabilityEdgeBoundaryBody))
				require.Error(t, out.err)
				var failover *UpstreamFailoverError
				require.False(t, errors.As(out.err, &failover), "partial inference cannot authorize another account attempt: %v", out.err)
				require.Equal(t, int32(3), handshakes.Load(), "the second model turn reused the healthy connection")
				require.Equal(t, int32(2), frames.Load(), "the already-started model turn must not be sent again")
				require.Equal(t, 2, upstream.callCount, "active cooldown cannot turn post-progress failure into HTTP replay")
				require.Empty(t, out.fallback)
				require.Empty(t, out.disabled)
				require.NotNil(t, out.result)
				require.Equal(t, "resp_boundary_partial", out.result.ResponseID)
				require.Equal(t, progress.input, out.result.Usage.InputTokens)
				require.Equal(t, progress.output, out.result.Usage.OutputTokens)
				require.Equal(t, progress.cached, out.result.Usage.CacheReadInputTokens)
				require.False(t, out.result.SucceededForScheduling())
				require.NotContains(t, out.body, "resp_http")
				require.Equal(t, 1, oauthReliabilityEdgeBoundaryEntryCount(registry), "reusing a connection neither consumes nor clears fresh-dial backoff")
			})
		}
	}
}

func TestOAuthReliabilityWSEdgeForwardExplicitAnchorOptsOutOfCooldown(t *testing.T) {
	var healthy atomic.Bool
	var handshakes, frames atomic.Int32
	requests := make(chan []byte, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes.Add(1)
		if !healthy.Load() {
			oauthReliabilityEdgeBoundaryChallenge(w)
			return
		}
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			frames.Add(1)
			requests <- raw
			if conn.WriteMessage(websocket.TextMessage, []byte(oauthReliabilityEdgeBoundaryCompleted)) != nil {
				return
			}
		}
	}))
	defer server.Close()
	svc, account, upstream := oauthReliabilityEdgeBoundaryFixture(t, server.URL)
	defer svc.openaiWSPool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	oauthReliabilityEdgeBoundaryPrime(t, ctx, svc, account)
	healthy.Store(true)
	body := []byte(`{"model":"gpt-5.5","stream":false,"previous_response_id":"resp_explicit_anchor","input":[{"role":"user","content":"continue this exact response"}]}`)
	out := oauthReliabilityEdgeBoundaryForward(ctx, svc, account, "boundary-anchor", body)
	require.NoError(t, out.err)
	require.NotNil(t, out.result)
	require.Empty(t, out.fallback)
	require.Empty(t, out.disabled)
	require.Equal(t, int32(3), handshakes.Load(), "connection-bound continuation still reaches the existing WS path")
	require.Equal(t, int32(1), frames.Load())
	require.Equal(t, 2, upstream.callCount, "anchor must not be transparently converted into a new HTTP request")
	select {
	case raw := <-requests:
		require.Equal(t, "resp_explicit_anchor", gjson.GetBytes(raw, "previous_response_id").String())
		require.Contains(t, string(raw), "continue this exact response")
	case <-ctx.Done():
		t.Fatal("upstream did not receive the anchored model frame")
	}
	require.Equal(t, 1, oauthReliabilityEdgeBoundaryEntryCount(svc.getOpenAIWSEdgeBackoff()), "an opted-out handshake cannot clear another eligible path's cooldown")
	// The new healthy connection remains reusable even while the scope's
	// fresh-dial policy is still blocked.
	out = oauthReliabilityEdgeBoundaryForward(ctx, svc, account, "boundary-anchor", []byte(oauthReliabilityEdgeBoundaryBody))
	require.NoError(t, out.err)
	require.Empty(t, out.fallback)
	require.Equal(t, int32(3), handshakes.Load())
	require.Equal(t, int32(2), frames.Load())
	require.Equal(t, 2, upstream.callCount)
}

func TestOAuthReliabilityWSEdgeForwardCancellationNeverStartsHTTPFallback(t *testing.T) {
	for _, phase := range []string{"before_acquire", "during_probe"} {
		t.Run(phase, func(t *testing.T) {
			var handshakes, frames atomic.Int32
			var healthy atomic.Bool
			probeEntered := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := handshakes.Add(1)
				if n <= 2 {
					oauthReliabilityEdgeBoundaryChallenge(w)
					return
				}
				if !healthy.Load() {
					probeEntered <- struct{}{}
					<-r.Context().Done()
					return
				}
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
				frames.Add(1)
				_ = conn.WriteMessage(websocket.TextMessage, []byte(oauthReliabilityEdgeBoundaryCompleted))
			}))
			defer server.Close()
			svc, account, upstream := oauthReliabilityEdgeBoundaryFixture(t, server.URL)
			defer svc.openaiWSPool.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			key := oauthReliabilityEdgeBoundaryPrime(t, ctx, svc, account)
			registry := svc.getOpenAIWSEdgeBackoff()
			requestCtx, cancelRequest := context.WithCancel(ctx)
			defer cancelRequest()
			if phase == "before_acquire" {
				cancelRequest()
			} else {
				oauthReliabilityEdgeBoundaryExpire(t, registry, key)
			}
			done := make(chan oauthReliabilityEdgeBoundaryOutcome, 1)
			go func() {
				done <- oauthReliabilityEdgeBoundaryForward(requestCtx, svc, account, "boundary-canceled", []byte(oauthReliabilityEdgeBoundaryBody))
			}()
			if phase == "during_probe" {
				select {
				case <-probeEntered:
				case <-ctx.Done():
					t.Fatal("half-open handshake did not start")
				}
				cancelRequest()
			}
			var out oauthReliabilityEdgeBoundaryOutcome
			select {
			case out = <-done:
			case <-ctx.Done():
				t.Fatal("canceled request did not return")
			}
			require.Error(t, out.err)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(out.err, &failover))
			require.Empty(t, out.fallback)
			require.Empty(t, out.disabled)
			require.Equal(t, 2, upstream.callCount, "a canceled request cannot gain an HTTP execution")
			require.Zero(t, frames.Load())
			if phase == "before_acquire" {
				require.Equal(t, int32(2), handshakes.Load())
				require.Equal(t, 1, oauthReliabilityEdgeBoundaryEntryCount(registry))
				return
			}
			require.Equal(t, int32(3), handshakes.Load())
			require.Zero(t, oauthReliabilityEdgeBoundaryEntryCount(registry), "canceled probe must release its lease without extending the edge failure")
			healthy.Store(true)
			out = oauthReliabilityEdgeBoundaryForward(ctx, svc, account, "boundary-canceled", []byte(oauthReliabilityEdgeBoundaryBody))
			require.NoError(t, out.err)
			require.NotNil(t, out.result)
			require.Empty(t, out.fallback)
			require.Equal(t, int32(4), handshakes.Load(), "a later live request can make a fresh handshake immediately")
			require.Equal(t, int32(1), frames.Load())
			require.Equal(t, 2, upstream.callCount)
		})
	}
}

func TestOAuthReliabilityConcurrentWSEdgeForwardAdmitsOneHalfOpenProbe(t *testing.T) {
	var handshakes, frames atomic.Int32
	probeEntered := make(chan struct{}, 1)
	releaseProbe := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(releaseProbe) }) }
	defer unblock()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := handshakes.Add(1)
		if n <= 2 {
			oauthReliabilityEdgeBoundaryChallenge(w)
			return
		}
		if n == 3 {
			probeEntered <- struct{}{}
			select {
			case <-releaseProbe:
			case <-r.Context().Done():
				return
			}
		}
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			frames.Add(1)
			if conn.WriteMessage(websocket.TextMessage, []byte(oauthReliabilityEdgeBoundaryCompleted)) != nil {
				return
			}
		}
	}))
	defer server.Close()
	svc, account, upstream := oauthReliabilityEdgeBoundaryFixture(t, server.URL)
	defer svc.openaiWSPool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	key := oauthReliabilityEdgeBoundaryPrime(t, ctx, svc, account)
	registry := svc.getOpenAIWSEdgeBackoff()
	oauthReliabilityEdgeBoundaryExpire(t, registry, key)
	done := make(chan oauthReliabilityEdgeBoundaryOutcome, 1)
	go func() {
		done <- oauthReliabilityEdgeBoundaryForward(ctx, svc, account, "boundary-half-open-probe", []byte(oauthReliabilityEdgeBoundaryBody))
	}()
	select {
	case <-probeEntered:
	case <-ctx.Done():
		t.Fatal("half-open probe did not reach the real handshake")
	}
	// MaxConnsPerAccount=2 leaves room for this independent acquisition. It
	// must consult the shared edge scope, not queue behind a capacity limit.
	follower := oauthReliabilityEdgeBoundaryForward(ctx, svc, account, "boundary-half-open-follower", []byte(oauthReliabilityEdgeBoundaryBody))
	require.NoError(t, follower.err)
	require.NotNil(t, follower.result)
	require.Equal(t, openAIWSEdgeBackoffReason, follower.fallback)
	require.Empty(t, follower.disabled)
	require.Equal(t, int32(3), handshakes.Load(), "only the admitted probe may enter the network dial")
	require.Zero(t, frames.Load(), "neither caller has sent a WS model frame while the probe is pending")
	require.Equal(t, 3, upstream.callCount, "the other caller falls back exactly once")
	unblock()
	var probe oauthReliabilityEdgeBoundaryOutcome
	select {
	case probe = <-done:
	case <-ctx.Done():
		t.Fatal("admitted probe did not complete")
	}
	require.NoError(t, probe.err)
	require.NotNil(t, probe.result)
	require.Empty(t, probe.fallback)
	require.Empty(t, probe.disabled)
	require.Equal(t, int32(1), frames.Load())
	require.Zero(t, oauthReliabilityEdgeBoundaryEntryCount(registry))
	next := oauthReliabilityEdgeBoundaryForward(ctx, svc, account, "boundary-half-open-probe", []byte(oauthReliabilityEdgeBoundaryBody))
	require.NoError(t, next.err)
	require.Empty(t, next.fallback)
	require.Equal(t, int32(3), handshakes.Load(), "successful probe restores normal socket reuse")
	require.Equal(t, int32(2), frames.Load())
	require.Equal(t, 3, upstream.callCount)
}

// Cookie WS business routing is currently disabled by openai_codex_ticket.go.
// Exercise its live admission boundary without fabricating an enabled public
// route: these connection-bound callers must not consume an ordinary cooldown.
func TestOAuthReliabilityWSEdgeCookieAndConnectionScopedRequestsOptOut(t *testing.T) {
	for _, scoped := range []string{"cookie_generation", "cookie_warmup", "preferred_connection"} {
		t.Run(scoped, func(t *testing.T) {
			svc := &OpenAIGatewayService{}
			account := &Account{ID: 9230, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
			hook := svc.openAIWSEdgeDialHook(&APIKey{ID: 901, UserID: 902}, "gpt-5.5", true)
			require.NotNil(t, hook)
			info := openAIWSFreshDialInfo{
				Request: openAIWSAcquireRequest{Account: account, WSURL: "wss://fixture.invalid/responses", Headers: make(http.Header)},
				Headers: http.Header{"Authorization": {"Bearer synthetic-cookie-scope"}, "Cookie": {"synthetic-cookie=value"}},
				Dialer:  newDefaultOpenAIWSClientDialer(),
			}
			for i := 0; i < 2; i++ {
				lease, err := hook(context.Background(), info)
				require.NoError(t, err)
				require.NotNil(t, lease)
				lease.Finish(oauthReliabilityEdge403("<html>synthetic challenge</html>"), time.Now())
			}
			lease, err := hook(context.Background(), info)
			require.ErrorIs(t, err, errOpenAIWSEdgeBackoff)
			require.Nil(t, lease, "positive control proves that this exact scope is blocked")
			switch scoped {
			case "cookie_generation":
				info.Request.Headers.Set(openAICookieWSGenerationHeader, "synthetic-generation")
			case "cookie_warmup":
				info.Request.CookieWarmup = true
			case "preferred_connection":
				info.Request.ForcePreferredConn = true
			}
			lease, err = hook(context.Background(), info)
			require.NoError(t, err)
			require.Nil(t, lease, "connection-bound work must continue through its original policy")
			require.Equal(t, 1, oauthReliabilityEdgeBoundaryEntryCount(svc.getOpenAIWSEdgeBackoff()))
		})
	}
}
