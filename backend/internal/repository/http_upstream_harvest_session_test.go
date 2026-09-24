package repository

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestHTTPUpstreamHarvestRotatedSessionsRetireCompletedClients(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(server.Close)
	svc := NewHTTPUpstream(nil).(*httpUpstreamService)
	request := func(harvest bool, proxy string) *http.Response {
		ctx := t.Context()
		if harvest {
			ctx = service.WithHTTPUpstreamProfile(ctx, service.HTTPUpstreamProfileOpenAIHarvest)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		require.NoError(t, err)
		resp, err := svc.Do(req, proxy, 41, 20)
		require.NoError(t, err)
		return resp
	}

	// The ordinary traffic client stays cached while short-lived harvest sessions
	// are retired as soon as their response bodies are closed.
	ordinary := request(false, "")
	require.NoError(t, ordinary.Body.Close())
	require.Len(t, svc.clients, 1)
	var ordinaryEntry *upstreamClientEntry
	for _, entry := range svc.clients {
		ordinaryEntry = entry
	}
	proxy, err := url.Parse(server.URL)
	require.NoError(t, err)
	for i := range 4 {
		proxy.User = url.UserPassword(fmt.Sprintf("user-session-%08x", i), "secret")
		resp := request(true, proxy.String())
		require.Len(t, svc.clients, 2)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, resp.Body.Close())
		require.Len(t, svc.clients, 1)
		require.Same(t, ordinaryEntry, svc.clients[ordinaryEntry.cacheKey])
	}

	// Two in-flight requests sharing a fixed session retain their client until
	// both are closed; one completed request must not retire the other's entry.
	first := request(true, proxy.String())
	second := request(true, proxy.String())
	require.NoError(t, first.Body.Close())
	require.Len(t, svc.clients, 2)
	require.NoError(t, second.Body.Close())
	require.Len(t, svc.clients, 1)
}

func TestHTTPUpstreamHarvestFailureRetiresClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(server.Close)
	svc := NewHTTPUpstream(nil).(*httpUpstreamService)
	req, err := http.NewRequestWithContext(
		service.WithHTTPUpstreamProfile(t.Context(), service.HTTPUpstreamProfileOpenAIHarvest),
		http.MethodGet, server.URL, nil,
	)
	require.NoError(t, err)
	_, err = svc.Do(req, "", 41, 20)
	require.Error(t, err)
	require.Empty(t, svc.clients)
}
