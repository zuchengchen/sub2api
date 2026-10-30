package codexwire

import (
	"context"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestH2RetireKeepsActiveResponseAndOpensNewConnection(t *testing.T) {
	release := make(chan struct{})
	remotes := make(chan string, 2)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remotes <- r.RemoteAddr
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		if r.URL.Path == "/held" {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, "complete")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	profile := *tlsfingerprint.BuiltinProfile("rustls-h2")
	profile.RootCAs = x509.NewCertPool()
	profile.RootCAs.AddCert(srv.Certificate())
	pool := NewH2Pool(H2Options{Timeout: 5 * time.Second})
	defer pool.CloseAll()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, err := pool.Do(ctx, "GET", srv.URL+"/held", nil, nil, "", "auth-state", &profile)
	require.NoError(t, err)
	defer first.Body.Close()
	firstRemote := <-remotes
	pool.Retire("auth-state")
	second, err := pool.Do(ctx, "GET", srv.URL+"/next", nil, nil, "", "auth-state", &profile)
	require.NoError(t, err)
	require.NotEqual(t, firstRemote, <-remotes)
	_, err = io.ReadAll(second.Body)
	require.NoError(t, err)
	require.NoError(t, second.Body.Close())
	close(release)
	data, err := io.ReadAll(first.Body)
	require.NoError(t, err, "retiring auth must not truncate a concurrent response")
	require.Equal(t, "complete", string(data))
}

func TestH2RetireRejectsLateDialPublication(t *testing.T) {
	var dials atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "complete")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	profile := *tlsfingerprint.BuiltinProfile("rustls-h2")
	profile.RootCAs = x509.NewCertPool()
	profile.RootCAs.AddCert(srv.Certificate())
	pool := NewH2Pool(H2Options{Timeout: 5 * time.Second, BaseDialer: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if dials.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}})
	defer pool.CloseAll()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		resp, err := pool.Do(ctx, "GET", srv.URL, nil, nil, "", "owner", &profile)
		if err == nil {
			_, err = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
		result <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	pool.Retire("owner")
	close(release)
	require.NoError(t, <-result)
	require.Equal(t, int32(2), dials.Load(), "a dial begun before reload must not become the replacement auth client")
	pool.mu.Lock()
	require.Empty(t, pool.dials, "completed generation bookkeeping must be released")
	pool.mu.Unlock()
}
