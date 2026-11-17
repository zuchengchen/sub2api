//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// Keep the production websocket client and handshake parser. Only the network
// destination is redirected to an isolated fixture; credentials are synthetic.
type oauthReliabilityEdgeLocalTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (r *oauthReliabilityEdgeLocalTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := req.Clone(req.Context())
	target := *req.URL
	target.Scheme = r.target.Scheme
	target.Host = r.target.Host
	next.URL = &target
	return r.base.RoundTrip(next)
}

func TestOAuthReliabilityWSEdgeForwardBypassesOnlyFreshDialAndRecovers(t *testing.T) {
	var handshakes, frames atomic.Int32
	var healthy atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes.Add(1)
		if !healthy.Load() {
			w.Header().Set("Cf-Mitigated", "challenge")
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<html>synthetic edge challenge</html>"))
			return
		}
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, _, err = conn.ReadMessage()
			if err != nil {
				return
			}
			frames.Add(1)
			if conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_edge_recovered","model":"gpt-5.5","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":1}}}`)) != nil {
				return
			}
		}
	}))
	defer server.Close()
	svc, account, httpUpstream := httpBridgeFixture(t, server.URL)
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	dialer := newDefaultOpenAIWSClientDialer().(*coderOpenAIWSClientDialer)
	dialer.directOnce.Do(func() {
		dialer.directClient = &http.Client{Transport: &oauthReliabilityEdgeLocalTransport{target, transport}}
	})
	svc.openaiWSPool.setClientDialerForTest(dialer)
	svc.cfg.Gateway.OpenAIWS.DeltaContextEnabled = false
	body := []byte(`{"model":"gpt-5.5","stream":false,"input":[{"role":"user","content":"synthetic backoff request"}]}`)
	call := func() string {
		c, rec := httpBridgeContext("edge-same-session")
		c.Set("api_key", &APIKey{ID: 901, UserID: 902})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		result, err := svc.Forward(ctx, c, account, body)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Empty(t, svc.openAIWSSessionDisabledReason(resolveOpenAIWSSessionScope(c, account, body)), "edge backoff must not disable the logical session")
		return c.GetString("openai_ws_session_fallback_reason")
	}
	require.Equal(t, openAIWSUpstreamForbiddenReason, call())
	require.Equal(t, openAIWSUpstreamForbiddenReason, call())
	require.Equal(t, openAIWSEdgeBackoffReason, call())
	require.Equal(t, int32(2), handshakes.Load(), "third request must skip the fresh handshake")
	require.Zero(t, frames.Load(), "failed handshakes never submitted model requests")
	require.Equal(t, 3, httpUpstream.callCount, "each HTTP fallback is exactly one attempt")
	registry := svc.getOpenAIWSEdgeBackoff()
	registry.mu.Lock()
	entriesBeforeProbe := len(registry.entries)
	for _, entry := range registry.entries {
		entry.blockedUntil = time.Now().Add(-time.Second)
	}
	registry.mu.Unlock()
	require.Equal(t, 1, entriesBeforeProbe)
	healthy.Store(true)
	require.Empty(t, call())
	require.Equal(t, int32(3), handshakes.Load())
	require.Equal(t, int32(1), frames.Load())
	registry.mu.Lock()
	remaining := len(registry.entries)
	registry.mu.Unlock()
	require.Zero(t, remaining, "successful probe clears only its key")
	require.Empty(t, call())
	require.Equal(t, int32(3), handshakes.Load(), "healthy socket must be reused")
	require.Equal(t, int32(2), frames.Load())
	require.Equal(t, 3, httpUpstream.callCount)
}

func TestOAuthReliabilityWSEdgeLocalReasonNeverBecomesSessionPenalty(t *testing.T) {
	err := wrapOpenAIWSFallback(classifyOpenAIWSAcquireError(errOpenAIWSEdgeBackoff), errOpenAIWSEdgeBackoff)
	reason, reconnect := classifyOpenAIWSReconnectReason(err)
	require.Equal(t, openAIWSEdgeBackoffReason, reason)
	require.False(t, reconnect)
	action := openAIWSSessionFallbackActionFor(reason)
	require.True(t, action.fallbackHTTP)
	require.False(t, action.markDisable)
	require.False(t, action.capability)
}

func TestOAuthReliabilityWSEdgeForwardUnknown403StillDials(t *testing.T) {
	for _, tc := range []struct {
		name, body, challenge string
	}{
		{"empty", "", ""},
		{"plain", "Forbidden", ""},
		{"ordinary_html", "<html>Forbidden</html>", ""},
		{"plain_with_challenge_header", "Forbidden", "challenge"},
		{"truncated_json", `{"error":{"message":"Forbidden`, "challenge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var handshakes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handshakes.Add(1)
				w.Header().Set("Cf-Mitigated", tc.challenge)
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			svc, account, upstream := httpBridgeFixture(t, server.URL)
			target, err := url.Parse(server.URL)
			require.NoError(t, err)
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = nil
			defer transport.CloseIdleConnections()
			dialer := newDefaultOpenAIWSClientDialer().(*coderOpenAIWSClientDialer)
			dialer.directOnce.Do(func() {
				dialer.directClient = &http.Client{Transport: &oauthReliabilityEdgeLocalTransport{target, transport}}
			})
			svc.openaiWSPool.setClientDialerForTest(dialer)
			svc.cfg.Gateway.OpenAIWS.DeltaContextEnabled = false
			body := []byte(`{"model":"gpt-5.5","stream":false,"input":"synthetic unknown forbidden"}`)
			for i := 0; i < 3; i++ {
				c, _ := httpBridgeContext("unknown-forbidden-session")
				c.Set("api_key", &APIKey{ID: 901, UserID: 902})
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				c.Request = c.Request.WithContext(ctx)
				result, err := svc.Forward(ctx, c, account, body)
				cancel()
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, openAIWSUpstreamForbiddenReason, c.GetString("openai_ws_session_fallback_reason"))
				require.Empty(t, svc.openAIWSSessionDisabledReason(resolveOpenAIWSSessionScope(c, account, body)))
			}
			require.Equal(t, int32(3), handshakes.Load(), "unknown 403 evidence must not suppress a later fresh handshake")
			require.Equal(t, 3, upstream.callCount)
		})
	}
}
