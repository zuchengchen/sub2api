package codexwire

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// TestH2PoolEndToEndTLS runs the whole path — rustls-h2 ClientHello, h2
// negotiation, frames — against a real TLS+h2 server.
func TestH2PoolEndToEndTLS(t *testing.T) {
	var gotH2 bool
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotH2 = r.ProtoMajor == 2
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	profile := tlsfingerprint.BuiltinProfile("rustls-h2")
	p := *profile
	p.InsecureSkipVerify = true
	pool := NewH2Pool(H2Options{Timeout: 10 * time.Second})
	defer pool.CloseAll()

	ordered := []HeaderKV{
		{Name: "chatgpt-account-id", Value: "acct-1"},
		{Name: "authorization", Value: "Bearer tok"},
		{Name: "content-type", Value: "application/json"},
		{Name: "accept", Value: "text/event-stream"},
	}
	resp, err := pool.Do(context.Background(), "POST", srv.URL+"/backend-api/codex/responses",
		ordered, []byte(`{"model":"gpt-5.2-codex","stream":true}`), "", "acct-test", &p)
	if err != nil {
		t.Fatalf("pool.Do: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !gotH2 {
		t.Fatal("server saw non-h2 request — ALPN failed")
	}
	if string(data) != "data: {\"type\":\"response.completed\"}\n\n" {
		t.Fatalf("body = %q", data)
	}

	// Second request must reuse the same h2 connection (stream 3+).
	resp2, err := pool.Do(context.Background(), "POST", srv.URL+"/backend-api/codex/responses",
		ordered, []byte(`{}`), "", "acct-test", &p)
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	_ = resp2.Body.Close()
	pool.mu.Lock()
	n := len(pool.conns)
	pool.mu.Unlock()
	if n != 1 {
		t.Fatalf("conns = %d, want 1 (multiplexed)", n)
	}
}

// TestH2PoolH1Fallback verifies ALPN failure falls back to raw HTTP/1.1 on the
// same TLS connection.
func TestH2PoolH1Fallback(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Errorf("h1 fallback path but server saw h%d", r.ProtoMajor)
		}
		fmt.Fprint(w, "ok-h1")
	}))
	// h1-only TLS server: no h2 ALPN.
	srv.TLS = &tls.Config{NextProtos: []string{"http/1.1"}}
	srv.StartTLS()
	defer srv.Close()

	profile := tlsfingerprint.BuiltinProfile("rustls-h2")
	p := *profile
	p.InsecureSkipVerify = true
	pool := NewH2Pool(H2Options{Timeout: 10 * time.Second})
	defer pool.CloseAll()

	resp, err := pool.Do(context.Background(), "POST", srv.URL+"/responses",
		nil, []byte(`{}`), "", "acct-h1", &p)
	if err != nil {
		t.Fatalf("pool.Do h1 fallback: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(data) != "ok-h1" {
		t.Fatalf("body = %q", data)
	}
}
