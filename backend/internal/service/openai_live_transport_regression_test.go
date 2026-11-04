package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func liveTransportTestService(t *testing.T, exact bool) (*OpenAIGatewayService, *LiveCallRecord) {
	t.Helper()
	cfg := &config.Config{}
	cfg.JWT.Secret = "synthetic-live-transport-key"
	cfg.Gateway.OpenAIWS.ExactFidelityEnabled = exact
	account := &Account{ID: 41, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 2,
		Credentials: map[string]any{"access_token": "synthetic-live-token", "chatgpt_account_id": "acct-live"},
		Extra:       map[string]any{"codex_tls_profile": "none"},
	}
	svc := &OpenAIGatewayService{cfg: cfg, accountRepo: &liveTestAccountRepo{account: account}, liveAttestationCipher: newLiveAttestationCipher(cfg)}
	record := &LiveCallRecord{CallID: "rtc_synthetic", CallHash: hashLiveCallID("rtc_synthetic"), AccountID: account.ID,
		APIKeyID: 42, UserID: 43, LeaseID: "synthetic-lease", Model: "gpt-live-test", CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(1200 * time.Millisecond), Controller: LiveControllerPending}
	var err error
	record.AttestationCiphertext, err = svc.liveAttestationCipher.Encrypt(`{"v":1,"s":0,"t":"synthetic"}`)
	require.NoError(t, err)
	return svc, record
}

func TestLiveSidebandRealHandshakeDoesNotOfferCompression(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(fmt.Sprint(exact), func(t *testing.T) {
			svc, record := liveTransportTestService(t, exact)
			seen := make(chan http.Header, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Header.Clone()
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				_, _, _ = conn.Read(r.Context())
			}))
			defer srv.Close()
			previous := chatGPTLiveSidebandBaseURL
			chatGPTLiveSidebandBaseURL = "ws" + strings.TrimPrefix(srv.URL, "http") + "/backend-api/codex"
			defer func() { chatGPTLiveSidebandBaseURL = previous }()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, err := svc.dialLiveSideband(ctx, record)
			require.NoError(t, err)
			defer conn.Close()
			headers := <-seen
			require.Empty(t, headers.Get("Sec-WebSocket-Extensions"), "Realtime uses WebSocketConfig::default, without Responses PMD")
			require.Equal(t, "Bearer synthetic-live-token", headers.Get("Authorization"))
			require.NotEmpty(t, headers.Get(liveAttestationHeader))
		})
	}
}

type liveStatusDialer struct {
	status int
	calls  atomic.Int32
}

func (d *liveStatusDialer) Dial(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error) {
	d.calls.Add(1)
	return nil, d.status, nil, errors.New("synthetic handshake rejection")
}

func TestLiveObserverStopsOnTerminalHandshakeStatus(t *testing.T) {
	for _, status := range []int{401, 403, 404, 410} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			svc, record := liveTransportTestService(t, false)
			dialer := &liveStatusDialer{status: status}
			svc.openaiWSPassthroughDialer = dialer
			store := &liveTestStore{}
			require.NoError(t, store.SaveLiveCall(context.Background(), record, time.Hour))
			concurrency := &liveTestConcurrencyCache{}
			usage := &liveTestUsageRepo{}
			svc.cache, svc.concurrencyService, svc.usageLogRepo = store, NewConcurrencyService(concurrency), usage
			done := make(chan struct{})
			go func() { svc.observeLiveCall(record); close(done) }()
			select {
			case <-done:
			case <-time.After(500 * time.Millisecond):
				// The old loop retries until the fixed, short expiry. Wait for its
				// own cleanup so the regression itself leaks no goroutines.
				<-done
				t.Error("terminal handshake rejection was retried until session expiry")
			}
			require.Equal(t, int32(1), dialer.calls.Load())
			concurrency.mu.Lock()
			require.Equal(t, 1, concurrency.releases)
			concurrency.mu.Unlock()
			usage.mu.Lock()
			require.Len(t, usage.logs, 1)
			usage.mu.Unlock()
		})
	}
}

func TestLiveTransientHandshakeFailureRemainsRetryable(t *testing.T) {
	svc, record := liveTransportTestService(t, false)
	svc.openaiWSPassthroughDialer = &liveStatusDialer{status: 503}
	_, err := svc.dialLiveSideband(context.Background(), record)
	require.Error(t, err)
	require.False(t, liveSessionEnded(err))
}

type liveWireHTTPUpstream struct{}

func (*liveWireHTTPUpstream) Do(request *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return http.DefaultClient.Do(request)
}

func (s *liveWireHTTPUpstream) DoWithTLS(request *http.Request, proxy string, account int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(request, proxy, account, concurrency)
}

func TestLiveCreatedAndSidebandUseObservedWireContract(t *testing.T) {
	t.Setenv("CODEX_CA_CERTIFICATE", "")
	t.Setenv("SSL_CERT_FILE", "")
	svc, record := liveTransportTestService(t, true)
	svc.httpUpstream = &liveWireHTTPUpstream{}
	seen := make(chan *http.Request, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Clone(context.Background())
		if r.Method == http.MethodPost {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Location", "/backend-api/codex/live/rtc_synthetic")
			_, _ = io.WriteString(w, "v=0\r\n")
			return
		}
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, _, _ = conn.Read(r.Context())
	}))
	defer srv.Close()
	beforeCreate, beforeSideband := chatGPTLiveCallsURL, chatGPTLiveSidebandBaseURL
	chatGPTLiveCallsURL = srv.URL + "/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas"
	chatGPTLiveSidebandBaseURL = "ws" + strings.TrimPrefix(srv.URL, "http") + "/backend-api/codex"
	defer func() { chatGPTLiveCallsURL, chatGPTLiveSidebandBaseURL = beforeCreate, beforeSideband }()
	account := svc.accountRepo.(*liveTestAccountRepo).account
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	created, err := svc.createUpstreamLiveCall(ctx, account, &LiveCallRequest{SDP: "v=offer\r\n", Session: []byte(`{"model":"gpt-live-test"}`)}, `{"v":1,"s":0,"t":"synthetic"}`)
	require.NoError(t, err)
	createRequest := <-seen
	assert.Equal(t, "*/*", createRequest.Header.Get("Accept"))
	assert.NotEmpty(t, createRequest.Header.Get("User-Agent"))
	assert.NotEmpty(t, createRequest.Header.Get("originator"))
	assert.Equal(t, CodexCanonicalClientVersion(), createRequest.Header.Get("version"))
	assert.Empty(t, createRequest.Header.Get(openAIWSTurnMetadataHeader), "standalone API has no thread source")
	assert.Equal(t, createRequest.Header.Get("session-id"), createRequest.Header.Get("x-session-id"))
	require.NotEmpty(t, createRequest.Header.Get("session-id"))
	record.CallID = created.CallID
	record.UpstreamContextCiphertext, err = svc.encryptLiveUpstreamContext(account.ID, record.CallID, created.upstreamHeaders)
	require.NoError(t, err)
	conn, err := svc.dialLiveSideband(ctx, record)
	require.NoError(t, err)
	defer conn.Close()
	wsRequest := <-seen
	assert.Equal(t, "/backend-api/codex/live/rtc_synthetic", wsRequest.URL.Path)
	assert.Empty(t, wsRequest.Header.Get("Accept"))
	assert.Empty(t, wsRequest.Header.Get("Sec-WebSocket-Extensions"))
	assert.Equal(t, createRequest.Header.Get("x-session-id"), wsRequest.Header.Get("x-session-id"))
	assert.Equal(t, createRequest.Header.Get("session-id"), wsRequest.Header.Get("session-id"))
	assert.Equal(t, createRequest.Header.Get("x-oai-attestation"), wsRequest.Header.Get("x-oai-attestation"))
	for _, name := range []string{"User-Agent", "originator", "version"} {
		assert.Equal(t, createRequest.Header.Get(name), wsRequest.Header.Get(name), name)
	}
	assert.Empty(t, wsRequest.Header.Get(openAIWSTurnMetadataHeader))
}

type liveReloadedSnapshotUpstream struct{ liveHTTPUpstreamStub }

func (s *liveReloadedSnapshotUpstream) Do(request *http.Request, proxy string, account int64, concurrency int) (*http.Response, error) {
	response, err := s.liveHTTPUpstreamStub.Do(request, proxy, account, concurrency)
	if err != nil {
		return nil, err
	}
	successful := request.Clone(request.Context())
	successful.Header.Set("Authorization", "Bearer successful-reload-synthetic")
	successful.Header.Set(liveAttestationHeader, `{"v":1,"s":0,"t":"successful-snapshot"}`)
	response.Request = successful
	return response, nil
}

func (s *liveReloadedSnapshotUpstream) DoWithTLS(request *http.Request, proxy string, account int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(request, proxy, account, concurrency)
}

func TestLiveSnapshotKeepsSuccessfulReloadRequest(t *testing.T) {
	svc, record := liveTransportTestService(t, false)
	svc.httpUpstream = &liveReloadedSnapshotUpstream{}
	account := svc.accountRepo.(*liveTestAccountRepo).account
	created, err := svc.createUpstreamLiveCall(context.Background(), account, &LiveCallRequest{SDP: "v=offer", Session: []byte(`{"model":"gpt-live-test"}`)}, `{"v":1,"s":0,"t":"initial-snapshot"}`)
	require.NoError(t, err)
	record.CallID = created.CallID
	record.UpstreamContextCiphertext, err = svc.encryptLiveUpstreamContext(account.ID, record.CallID, created.upstreamHeaders)
	require.NoError(t, err)
	headers, err := svc.liveSidebandHeaders(context.Background(), account, record)
	require.NoError(t, err)
	require.Equal(t, "Bearer successful-reload-synthetic", headers.Get("Authorization"))
	require.Equal(t, `{"v":1,"s":0,"t":"successful-snapshot"}`, headers.Get(liveAttestationHeader))
	require.Equal(t, headers.Get("session-id"), headers.Get("x-session-id"))
	require.Equal(t, "synthetic-live-token", account.GetOpenAIAccessToken(), "snapshotting must not mutate the account")
}

func TestLiveIdentitySeedKeepsConfiguredPersonaAndExplicitMetadata(t *testing.T) {
	for _, enforcement := range []bool{false, true} {
		t.Run(fmt.Sprint(enforcement), func(t *testing.T) {
			SetCodexIdentityEnforcementEnabled(enforcement)
			t.Cleanup(func() { SetCodexIdentityEnforcementEnabled(true) })
			svc, _ := liveTransportTestService(t, true)
			account := svc.accountRepo.(*liveTestAccountRepo).account
			account.Extra["codex_client_family"] = "vscode"
			headers := http.Header{}
			metadata := `{"thread_source":"explicit-source","extension":"keep"}`
			headers.Set(openAIWSTurnMetadataHeader, metadata)
			svc.applyLiveUpstreamIdentityHeaders(headers, account)
			require.True(t, strings.HasPrefix(headers.Get("User-Agent"), "codex_vscode/"))
			require.Equal(t, "codex_vscode", headers.Get("originator"))
			require.Equal(t, CodexCanonicalClientVersion(), headers.Get("version"))
			require.Equal(t, metadata, headers.Get(openAIWSTurnMetadataHeader))
			require.Empty(t, headers.Get("OpenAI-Beta"))
		})
	}
}

func TestLiveSavedSnapshotIdentityAndMetadataRemainAuthoritative(t *testing.T) {
	svc, record := liveTransportTestService(t, true)
	account := svc.accountRepo.(*liveTestAccountRepo).account
	account.Extra["codex_client_family"] = "vscode"
	saved := http.Header{
		"User-Agent": {"codex_exec/0.166.0 (Linux; x86_64) terminal (codex_exec; 0.166.0)"},
		"Originator": {"codex_exec"}, "Version": {"0.166.0"},
		"Authorization": {"Bearer saved-synthetic"}, "X-Oai-Attestation": {`{"v":1,"s":0,"t":"saved-synthetic"}`},
		"Session-Id": {"saved-mapped-session"}, "Thread-Id": {"saved-mapped-thread"},
		"X-Codex-Turn-Metadata": {`{"thread_source":"saved-source"}`},
	}
	var err error
	record.UpstreamContextCiphertext, err = svc.encryptLiveUpstreamContext(account.ID, record.CallID, saved)
	require.NoError(t, err)
	actual, err := svc.liveSidebandHeaders(context.Background(), account, record)
	require.NoError(t, err)
	for name := range saved {
		require.Equal(t, saved.Get(name), actual.Get(name), name)
	}
	require.Equal(t, saved.Get("Session-Id"), actual.Get("x-session-id"))
	// Legacy snapshots with no persona get the configured account identity,
	// while their auth, attestation and explicitly saved metadata stay exact.
	for _, name := range []string{"User-Agent", "originator", "version"} {
		saved.Del(name)
	}
	record.UpstreamContextCiphertext, err = svc.encryptLiveUpstreamContext(account.ID, record.CallID, saved)
	require.NoError(t, err)
	actual, err = svc.liveSidebandHeaders(context.Background(), account, record)
	require.NoError(t, err)
	require.Equal(t, "codex_vscode", actual.Get("originator"))
	for name := range saved {
		require.Equal(t, saved.Get(name), actual.Get(name), name)
	}
}
