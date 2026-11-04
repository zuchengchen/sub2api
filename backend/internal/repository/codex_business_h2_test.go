package repository

import (
	"context"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestCodexBusinessH2FreshClientsAndCompatibilityPool(t *testing.T) {
	type observed struct {
		path, peer, agent string
		status, proto     int
	}
	seen := make(chan observed, 8)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		status := http.StatusOK
		if r.Header.Get("Authorization") == "Bearer rejected" {
			status = http.StatusUnauthorized
		}
		seen <- observed{r.URL.Path, r.RemoteAddr, r.Header.Get("User-Agent"), status, r.ProtoMajor}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "complete body")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	profile := *tlsfingerprint.BuiltinProfile("rustls-h2")
	profile.RootCAs = x509.NewCertPool()
	profile.RootCAs.AddCert(srv.Certificate())
	profile.RootCAID = "business-fixture"
	upstream := &httpUpstreamService{}
	defer upstream.codexH2().CloseAll()
	var rows []observed
	for i, path := range []string{"/backend-api/codex/models", "/backend-api/codex/models", "/backend-api/codex/responses", "/backend-api/codex/responses"} {
		ctx := service.WithCodexBusinessHTTP(context.Background(), true)
		method, body := "GET", ""
		if i >= 2 {
			method, body = "POST", `{"input":"synthetic"}`
		}
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, strings.NewReader(body))
		require.NoError(t, err)
		agent := "codex_exec/0.166.0"
		if i > 0 {
			agent += " (codex_exec; 0.166.0)"
		}
		req.Header.Set("User-Agent", agent)
		req.Header.Set("Authorization", "Bearer current")
		if i == 2 {
			req.Header.Set("Authorization", "Bearer rejected")
		}
		resp, err := upstream.doCodexH2Uncontrolled(req, "", 7, &profile)
		require.NoError(t, err)
		got, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, "complete body", string(got))
		require.NoError(t, resp.Body.Close())
		row := <-seen
		require.Equal(t, 2, row.proto)
		require.Equal(t, agent, row.agent)
		rows = append(rows, row)
	}
	unique := map[string]bool{}
	for _, row := range rows {
		unique[row.peer] = true
	}
	require.Len(t, unique, 4, "each OAuth models/business/401 attempt owns a fresh HTTP client")
	require.Equal(t, http.StatusUnauthorized, rows[2].status)

	var compatibility []string
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequestWithContext(service.WithCodexBusinessHTTP(context.Background(), false), "GET", srv.URL+"/compatible", nil)
		resp, err := upstream.doCodexH2Uncontrolled(req, "", 8, &profile)
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		compatibility = append(compatibility, (<-seen).peer)
	}
	require.Equal(t, compatibility[0], compatibility[1], "explicit non-OAuth H2 keeps its existing pool")
}

func TestCodexBusinessH2DoesNotCloseBeforeBodyAndCancelsCleanly(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-finish:
			_, _ = io.WriteString(w, "tail")
		case <-r.Context().Done():
		}
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	profile := *tlsfingerprint.BuiltinProfile("rustls-h2")
	profile.RootCAs = x509.NewCertPool()
	profile.RootCAs.AddCert(srv.Certificate())
	upstream := &httpUpstreamService{}
	defer upstream.codexH2().CloseAll()
	ctx, cancel := context.WithCancel(service.WithCodexBusinessHTTP(context.Background(), true))
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/backend-api/codex/models", nil)
	resp, err := upstream.doCodexH2Uncontrolled(req, "", 9, &profile)
	require.NoError(t, err)
	<-started
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(resp.Body); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("body ended before server data or cancellation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("business cancellation did not unblock body")
	}
	_ = resp.Body.Close()
	close(finish)
}
