package repository

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type codexQuotaLogBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *codexQuotaLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *codexQuotaLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func TestOAuthExchangeCodePreservesOfficialRawFormOrder(t *testing.T) {
	t.Setenv(codexDeviceClientDisabledEnv, "")
	t.Setenv("CODEX_CA_CERTIFICATE", "")
	t.Setenv("SSL_CERT_FILE", "")
	seen := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Error("wrong form content type")
		}
		seen <- string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"synthetic","refresh_token":"synthetic-next","expires_in":3600}`)
	}))
	defer srv.Close()
	client := &openaiOAuthService{tokenURL: srv.URL + "/oauth/token"}
	_, err := client.ExchangeCode(context.Background(), "code +/你好", "verifier+ ~*", "http://localhost:1455/auth/callback?next=a b", "", "client/id")
	require.NoError(t, err)
	// url::form_urlencoded preserves parameter insertion order and escapes '~'
	// while leaving '*' unescaped (the WHATWG form percent-encode set).
	require.Equal(t, "grant_type=authorization_code&client_id=client%2Fid&code=code+%2B%2F%E4%BD%A0%E5%A5%BD&redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback%3Fnext%3Da+b&code_verifier=verifier%2B+%7E*", <-seen)
}

func TestCodexQuotaCustomCASeparatesCredentialsAndPreservesWebTrust(t *testing.T) {
	t.Setenv(codexDeviceClientDisabledEnv, "")
	t.Setenv("SSL_CERT_FILE", "")
	type capture struct {
		account, token, remote string
		proto                  int
	}
	seen := make(chan capture, 16)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- capture{r.Header.Get("Chatgpt-Account-Id"), r.Header.Get("Authorization"), r.RemoteAddr, r.ProtoMajor}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != strings.TrimPrefix(srv.URL, "https://") {
			http.Error(w, "unexpected proxy target", http.StatusForbidden)
			return
		}
		remote, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, "local dial failed", 502)
			return
		}
		defer remote.Close()
		conn, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() { _, _ = io.Copy(remote, buffer); _ = remote.Close(); close(done) }()
		_, _ = io.Copy(conn, remote)
		_ = conn.Close()
		<-done
	}))
	defer proxy.Close()
	ca := filepath.Join(t.TempDir(), "quota-ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600))
	t.Setenv("CODEX_CA_CERTIFICATE", ca)
	var logs codexQuotaLogBuffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	client, err := newCodexDeviceReqClient(proxy.URL, 3*time.Second)
	require.NoError(t, err)
	get := func(account, token, path string) capture {
		t.Helper()
		_, err := client.R().SetHeader("Authorization", "Bearer "+token).SetHeader("Chatgpt-Account-Id", account).SetHeader("User-Agent", "codex-synthetic").Get(srv.URL + path)
		require.NoError(t, err)
		return <-seen
	}
	a1 := get("acct-a", "synthetic-a", "/backend-api/wham/usage")
	a2 := get("acct-a", "synthetic-a", "/backend-api/wham/rate-limit-reset-credits")
	b := get("acct-b", "synthetic-b", "/backend-api/wham/usage")
	a3 := get("acct-a", "synthetic-a", "/backend-api/wham/usage")
	require.Equal(t, 2, a1.proto)
	require.Equal(t, a1.remote, a2.remote, "one backend client reuses its current auth snapshot")
	require.Equal(t, a1.remote, a3.remote)
	require.NotEqual(t, a1.remote, b.remote, "two credentials on the same factory must not share a connection")
	rotated := get("acct-a", "synthetic-rotated", "/backend-api/wham/usage")
	require.NotEqual(t, a1.remote, rotated.remote, "credential rotation changes the connection partition")
	_, err = client.R().Get(srv.URL + "/backend-api/accounts/check")
	require.Error(t, err, "web account queries must retain their original trust lane")
	key := codexQuotaPoolKey(context.Background(), http.Header{"Authorization": {"Bearer synthetic-a"}, "Chatgpt-Account-Id": {"acct-a"}, "User-Agent": {"codex-synthetic"}})
	require.NotContains(t, logs.String(), key, "private credential partition fingerprints must not be logged")
	require.NotContains(t, logs.String(), "synthetic-a")
	require.Contains(t, logs.String(), "codex-quota")

	keyPair, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(2), IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	otherRoot, err := x509.CreateCertificate(rand.Reader, template, template, &keyPair.PublicKey, keyPair)
	require.NoError(t, err)
	otherCA := filepath.Join(t.TempDir(), "other-valid-ca.pem")
	require.NoError(t, os.WriteFile(otherCA, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: otherRoot}), 0600))
	t.Setenv("CODEX_CA_CERTIFICATE", otherCA)
	_, err = client.R().SetHeader("Authorization", "Bearer synthetic-a").SetHeader("Chatgpt-Account-Id", "acct-a").SetHeader("User-Agent", "codex-synthetic").Get(srv.URL + "/backend-api/wham/usage")
	require.Error(t, err, "a valid but different trust snapshot must not reuse the prior socket")

	// A malformed/removed custom root cannot reuse a socket authenticated under
	// the old trust snapshot. Reinstalling the same valid root restores access.
	badCA := filepath.Join(t.TempDir(), "bad-ca.pem")
	require.NoError(t, os.WriteFile(badCA, []byte("not a certificate"), 0600))
	t.Setenv("CODEX_CA_CERTIFICATE", badCA)
	_, err = client.R().SetHeader("Authorization", "Bearer synthetic-a").SetHeader("Chatgpt-Account-Id", "acct-a").Get(srv.URL + "/backend-api/wham/usage")
	require.Error(t, err)
	t.Setenv("CODEX_CA_CERTIFICATE", "")
	_, err = client.R().SetHeader("Authorization", "Bearer synthetic-a").SetHeader("Chatgpt-Account-Id", "acct-a").Get(srv.URL + "/backend-api/wham/usage")
	require.Error(t, err)
	t.Setenv("CODEX_CA_CERTIFICATE", ca)
	require.Equal(t, 2, get("acct-a", "synthetic-a", "/backend-api/wham/usage").proto)
}

func TestCodexQuotaCustomCACancellation(t *testing.T) {
	t.Setenv(codexDeviceClientDisabledEnv, "")
	t.Setenv("SSL_CERT_FILE", "")
	arrived := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(arrived)
		<-r.Context().Done()
	}))
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "quota-ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600))
	t.Setenv("CODEX_CA_CERTIFICATE", ca)
	client, err := newCodexDeviceReqClient("", 3*time.Second)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.R().SetContext(ctx).SetHeader("Authorization", "Bearer synthetic").Get(srv.URL + "/backend-api/wham/usage")
		done <- err
	}()
	select {
	case <-arrived:
		cancel()
	case err := <-done:
		t.Fatalf("quota request failed before reaching the scoped-CA endpoint: %v", err)
	case <-time.After(4 * time.Second):
		t.Fatal("quota request did not reach local endpoint")
	}
	select {
	case err := <-done:
		require.Error(t, err)
		require.Contains(t, strings.ToLower(err.Error()), "canceled")
	case <-time.After(time.Second):
		t.Fatal("quota response did not unblock after cancellation")
	}
}
