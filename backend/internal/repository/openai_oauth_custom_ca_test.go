package repository

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/codexwire"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestOAuthCustomCARefreshUsesH2AndScopedTrust(t *testing.T) {
	t.Setenv(codexDeviceClientDisabledEnv, "")
	t.Setenv("SSL_CERT_FILE", "")
	type request struct {
		proto   int
		headers http.Header
		body    map[string]string
		remote  string
	}
	seen := make(chan request, 16)
	var mu sync.Mutex
	var hellos [][]string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad JSON", 400)
			return
		}
		seen <- request{r.ProtoMajor, r.Header.Clone(), body, r.RemoteAddr}
		if body["refresh_token"] == "synthetic-retry" {
			http.Error(w, "retry later", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"fake-access","refresh_token":"fake-next","expires_in":3600}`)
	}))
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		mu.Lock()
		hellos = append(hellos, append([]string(nil), hello.SupportedProtos...))
		mu.Unlock()
		return nil, nil
	}}
	server.StartTLS()
	t.Cleanup(server.Close)
	t.Cleanup(codexAuthH2Pool.CloseAll)
	caPath := filepath.Join(t.TempDir(), "auth-ca.pem")
	require.NoError(t, os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600))
	t.Setenv("CODEX_CA_CERTIFICATE", caPath)
	svc := &openaiOAuthService{tokenURL: server.URL + "/oauth/token"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = openai.WithCodexRefreshReason(ctx, "unauthorized_recovery")
	ctx = openai.WithCodexClientUserAgent(ctx, "codex_exec/0.166.0 (Ubuntu 26.4.0; x86_64) xterm-256color (codex_exec; 0.166.0)")
	var remotes []string
	for _, token := range []string{"synthetic-one", "synthetic-one", "synthetic-two"} {
		result, err := svc.RefreshToken(ctx, token, "")
		require.NoError(t, err)
		require.Equal(t, "fake-access", result.AccessToken)
		got := <-seen
		require.Equal(t, 2, got.proto)
		require.Equal(t, "*/*", got.headers.Get("Accept"))
		require.Equal(t, "codex_exec", got.headers.Get("originator"))
		require.Equal(t, "unauthorized_recovery", got.body["refresh_reason"])
		require.Equal(t, token, got.body["refresh_token"])
		remotes = append(remotes, got.remote)
		// The repository response is published only after durable auth storage
		// succeeds; callers then explicitly retire the previous auth state.
		svc.RetireAuthClient(ctx, token, "")
	}
	require.NotEqual(t, remotes[0], remotes[1], "successful auth reload creates a new client even with the same token")
	require.NotEqual(t, remotes[0], remotes[2], "distinct credentials must not share the auth connection")
	var failed []string
	for _, reason := range []string{"unauthorized_recovery", "forced"} {
		_, err := svc.RefreshToken(openai.WithCodexRefreshReason(ctx, reason), "synthetic-retry", "")
		require.Error(t, err)
		got := <-seen
		require.Equal(t, reason, got.body["refresh_reason"])
		failed = append(failed, got.remote)
	}
	require.Equal(t, failed[0], failed[1], "reason changes do not replace a failed auth state")
	_, err := svc.RefreshToken(openai.WithCodexClientUserAgent(ctx, "codex_cli_rs/0.166.0 (Windows 11; x86_64)"), "synthetic-retry", "")
	require.Error(t, err)
	require.NotEqual(t, failed[0], (<-seen).remote, "devices cannot share an auth connection")
	ownerA := openai.WithCodexAuthAccount(ctx, 1001)
	ownerB := openai.WithCodexAuthAccount(ctx, 1002)
	_, err = svc.RefreshToken(ownerA, "synthetic-retry", "")
	require.Error(t, err)
	remoteA := (<-seen).remote
	_, err = svc.RefreshToken(ownerB, "synthetic-retry", "")
	require.Error(t, err)
	remoteB := (<-seen).remote
	require.NotEqual(t, remoteA, remoteB, "even identical imported tokens belong to independent account auth states")
	svc.RetireAuthClient(ownerA, "synthetic-retry", "")
	_, err = svc.RefreshToken(ownerB, "synthetic-retry", "")
	require.Error(t, err)
	require.Equal(t, remoteB, (<-seen).remote, "reloading A must preserve B's auth client")
	_, err = svc.RefreshToken(ownerA, "synthetic-retry", "")
	require.Error(t, err)
	require.NotEqual(t, remoteA, (<-seen).remote, "unchanged credentials still get a new client after reload")
	mu.Lock()
	firstHello := hellos[0]
	mu.Unlock()
	require.Equal(t, []string{"h2", "http/1.1"}, firstHello)

	// The web/shared factory still ignores the Codex-only CA, uses its
	// original non-ALPN lane, and cannot trust this self-signed server.
	webClient, err := newCodexDeviceReqClient("", time.Second)
	require.NoError(t, err)
	_, err = webClient.R().SetContext(ctx).Get(server.URL + "/backend-api/accounts/check")
	require.Error(t, err)
	mu.Lock()
	lastHello := hellos[len(hellos)-1]
	mu.Unlock()
	require.Empty(t, lastHello)
	select {
	case <-seen:
		t.Fatal("untrusted web request reached application")
	default:
	}

	invalid := filepath.Join(t.TempDir(), "bad-ca.pem")
	require.NoError(t, os.WriteFile(invalid, []byte("not a certificate"), 0600))
	t.Setenv("SSL_CERT_FILE", caPath)
	t.Setenv("CODEX_CA_CERTIFICATE", invalid)
	_, err = codexwire.WithCodexCustomCA(tlsfingerprint.BuiltinProfile("rustls-h2"))
	require.ErrorContains(t, err, "no certificates", "Codex CA takes precedence")
	fallback, err := createOpenAIReqClient("")
	require.NoError(t, err, "default auth builder retries its original native builder on CA construction error")
	_, err = fallback.R().SetContext(ctx).Get(server.URL + "/oauth/token")
	require.Error(t, err, "native fallback keeps certificate verification enabled")
	mu.Lock()
	require.Empty(t, hellos[len(hellos)-1], "fallback must return to native non-ALPN lane")
	mu.Unlock()
}
