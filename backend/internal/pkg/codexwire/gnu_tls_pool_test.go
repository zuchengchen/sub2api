package codexwire

import (
	"context"
	"crypto/x509"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGNUProfileH1PoolSeparatesNativePresetsAndTrust(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.RemoteAddr) }))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	old, next := tlsfingerprint.BuiltinProfile("openssl36"), tlsfingerprint.BuiltinProfile("openssl35-gnu")
	old.RootCAs, old.RootCAID = roots, "synthetic-root"
	next.RootCAs, next.RootCAID = roots, "synthetic-root"
	pool := NewH1Pool(H1PoolOptions{})
	defer pool.CloseAll()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	call := func(profile *tlsfingerprint.Profile) (string, error) {
		response, err := pool.Do(ctx, "GET", server.URL, nil, nil, "", "same-principal", HTTP1Options{TLSProfile: profile})
		if err != nil {
			return "", err
		}
		raw, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		return string(raw), err
	}
	a, err := call(old)
	require.NoError(t, err)
	b, err := call(next)
	require.NoError(t, err)
	require.NotEqual(t, a, b, "native preset switch cannot reuse the old TLS connection")
	again, err := call(next)
	require.NoError(t, err)
	require.Equal(t, b, again, "same profile and roots reuse one connection")
	changed := *next
	changed.RootCAID = "rotated-untrusted-root"
	changed.RootCAs = x509.NewCertPool()
	_, err = call(&changed)
	require.Error(t, err, "new trust identity must not borrow the previously trusted connection")
	again, err = call(next)
	require.NoError(t, err)
	require.Equal(t, b, again)
}
