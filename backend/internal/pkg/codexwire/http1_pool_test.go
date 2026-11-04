package codexwire

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func readH1PooledResponse(t *testing.T, p *H1Pool, ctx context.Context, origin, key string) string {
	t.Helper()
	resp, err := p.Do(ctx, "GET", origin, []HeaderKV{{Name: "accept", Value: "*/*"}}, nil, "", key, HTTP1Options{})
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return string(body)
}

func TestH1AuxiliaryPoolReuseAndIdentityIsolation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.RemoteAddr) }))
	defer srv.Close()
	p := NewH1Pool(H1PoolOptions{})
	defer p.CloseAll()
	a := readH1PooledResponse(t, p, context.Background(), srv.URL, "account-a")
	require.Equal(t, a, readH1PooledResponse(t, p, context.Background(), srv.URL, "account-a"))
	b := readH1PooledResponse(t, p, context.Background(), srv.URL, "account-b")
	require.NotEqual(t, a, b)
	require.Equal(t, a, readH1PooledResponse(t, p, context.Background(), srv.URL, "account-a"))
}

func TestH1AuxiliaryPoolParallelRequestsUseIndependentLeases(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Peer", r.RemoteAddr)
		if r.URL.Path == "/hold" {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, r.RemoteAddr)
	}))
	defer srv.Close()
	p := NewH1Pool(H1PoolOptions{})
	defer p.CloseAll()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first, err := p.Do(ctx, "GET", srv.URL+"/hold", nil, nil, "", "same", HTTP1Options{})
	require.NoError(t, err)
	other := readH1PooledResponse(t, p, ctx, srv.URL+"/next", "same")
	require.NotEqual(t, first.Header.Get("X-Peer"), other, "unread response owns its socket")
	require.NoError(t, first.Body.Close())
}

func TestH1AuxiliaryPoolRetiresPartialCanceledAndRemoteClose(t *testing.T) {
	for _, mode := range []string{"partial", "cancel", "remote-close"} {
		t.Run(mode, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if mode == "remote-close" {
					conn, _, _ := w.(http.Hijacker).Hijack()
					_ = conn.Close()
					return
				}
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				_, _ = io.WriteString(w, "partial")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer srv.Close()
			p := NewH1Pool(H1PoolOptions{})
			defer p.CloseAll()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			resp, err := p.Do(ctx, "POST", srv.URL, nil, []byte("one-time-body"), "", "owner", HTTP1Options{})
			if mode == "remote-close" {
				require.Error(t, err)
				require.Nil(t, resp)
			} else {
				require.NoError(t, err)
				one := make([]byte, 1)
				_, err = io.ReadFull(resp.Body, one)
				require.NoError(t, err)
				if mode == "cancel" {
					cancel()
					_, err = io.ReadAll(resp.Body)
					require.Error(t, err)
				}
				done := make(chan error, 1)
				go func() { done <- resp.Body.Close() }()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("closing abandoned body tried to drain")
				}
			}
			require.Equal(t, int32(1), requests.Load(), "never auto replay an uncertain POST")
			p.mu.Lock()
			idle, all := p.idleCount, len(p.all)
			p.mu.Unlock()
			require.Zero(t, idle)
			require.Zero(t, all)
		})
	}
}

func TestH1AuxiliaryPoolPreservesExactHeaders(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	seen := make(chan string, 2)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		br := bufio.NewReader(conn)
		for i := 0; i < 2; i++ {
			var header strings.Builder
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				header.WriteString(line)
				if line == "\r\n" {
					break
				}
			}
			seen <- header.String()
			_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\ncontent-length: 2\r\n\r\nok")
		}
	}()
	p := NewH1Pool(H1PoolOptions{})
	defer p.CloseAll()
	for i := 0; i < 2; i++ {
		got := readH1PooledResponse(t, p, context.Background(), "http://"+ln.Addr().String()+"/oauth/token", "login")
		require.Equal(t, "ok", got)
		require.Equal(t, fmt.Sprintf("GET /oauth/token HTTP/1.1\r\naccept: */*\r\nhost: %s\r\n\r\n", ln.Addr()), <-seen)
	}
}

func TestH1AuxiliaryPoolBoundsAndExpiresIdleConnections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.RemoteAddr) }))
	defer srv.Close()
	p := NewH1Pool(H1PoolOptions{IdleTimeout: 25 * time.Millisecond, MaxIdleConns: 2, MaxIdlePerKey: 1})
	defer p.CloseAll()
	for i := 0; i < 4; i++ {
		readH1PooledResponse(t, p, context.Background(), srv.URL, fmt.Sprint(i))
	}
	p.mu.Lock()
	idle := p.idleCount
	p.mu.Unlock()
	require.LessOrEqual(t, idle, 2)
	require.Eventually(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return len(p.all) == 0 }, time.Second, 5*time.Millisecond)
}

func TestH1AuxiliaryPoolFullReadWithoutCloseDoesNotRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.RemoteAddr) }))
	defer srv.Close()
	p := NewH1Pool(H1PoolOptions{})
	defer p.CloseAll()
	first, err := p.Do(context.Background(), "GET", srv.URL, nil, nil, "", "same", HTTP1Options{})
	require.NoError(t, err)
	data, err := io.ReadAll(first.Body)
	require.NoError(t, err)
	second := readH1PooledResponse(t, p, context.Background(), srv.URL, "same")
	require.NotEqual(t, string(data), second)
	require.NoError(t, first.Body.Close())
}

func TestH1AuxiliaryPoolCanceledOldContextCannotCloseNextRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(w, bytes.NewBufferString(r.RemoteAddr)) }))
	defer srv.Close()
	p := NewH1Pool(H1PoolOptions{})
	defer p.CloseAll()
	ctx, cancel := context.WithCancel(context.Background())
	first := readH1PooledResponse(t, p, ctx, srv.URL, "same")
	cancel()
	require.Equal(t, first, readH1PooledResponse(t, p, context.Background(), srv.URL, "same"))
}

func TestH1AuxiliaryPoolCleanupCancellationAfterEOFStillRequiresClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.RemoteAddr) }))
	defer srv.Close()
	p := NewH1Pool(H1PoolOptions{})
	defer p.CloseAll()
	ctx, cancel := context.WithCancel(context.Background())
	first, err := p.Do(ctx, "GET", srv.URL, nil, nil, "", "same", HTTP1Options{})
	require.NoError(t, err)
	peer, err := io.ReadAll(first.Body)
	require.NoError(t, err)
	cancel() // repository cancelOnCloseBody cancels immediately before Close
	p.mu.Lock()
	idle := p.idleCount
	p.mu.Unlock()
	require.Zero(t, idle, "EOF does not transfer body ownership before Close")
	require.NoError(t, first.Body.Close())
	require.Equal(t, string(peer), readH1PooledResponse(t, p, context.Background(), srv.URL, "same"))
}
