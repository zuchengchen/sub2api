package service

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

type liveRouteScheduler struct {
	OpenAIAccountScheduler
	account  *Account
	released atomic.Int32
}

func (s *liveRouteScheduler) Select(_ context.Context, req OpenAIAccountScheduleRequest) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	if req.RequiredCapability != OpenAIEndpointCapabilityLive {
		return nil, OpenAIAccountScheduleDecision{}, ErrNoAvailableAccounts
	}
	return &AccountSelectionResult{Account: s.account, Acquired: true, ReleaseFunc: func() { s.released.Add(1) }}, OpenAIAccountScheduleDecision{}, nil
}

type liveRouteStore struct {
	*liveTestStore
	observerDone chan struct{}
}

func (s *liveRouteStore) GetSessionAccountID(context.Context, int64, string) (int64, error) {
	return 0, ErrStickySessionNotFound
}

func (s *liveRouteStore) ClaimLiveController(context.Context, string, string, string) (bool, error) {
	close(s.observerDone)
	return false, nil // Do not start a detached observer; the test dials the saved record.
}

func liveRouteTLSFixture(t *testing.T, seen chan<- *http.Request) *httptest.Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic-live-route"},
		DNSNames: []string{"api.openai.com", "chatgpt.com"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	caPath := filepath.Join(t.TempDir(), "synthetic-live-ca.pem")
	require.NoError(t, os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600))
	t.Setenv("CODEX_CA_CERTIFICATE", caPath)
	t.Setenv("SSL_CERT_FILE", "")
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Clone(context.Background())
		conn, acceptErr := coderws.Accept(w, r, nil)
		if acceptErr != nil {
			return
		}
		defer conn.CloseNow()
		_ = conn.Write(r.Context(), coderws.MessageText, []byte(`{"type":"session.started","session":{"id":"synthetic-route"}}`))
		_, _, _ = conn.Read(r.Context())
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, NextProtos: []string{"http/1.1"}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func liveRouteCONNECTFixture(t *testing.T, target string, seen chan<- string) *Proxy {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || (r.Host != "api.openai.com:443" && r.Host != "chatgpt.com:443") {
			http.Error(w, "unexpected synthetic target", http.StatusBadRequest)
			return
		}
		seen <- r.Host
		upstream, err := net.DialTimeout("tcp", target, 3*time.Second)
		if err != nil {
			http.Error(w, "synthetic target unavailable", http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		downstream, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer downstream.Close()
		_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		if buffer.Flush() != nil {
			return
		}
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(upstream, buffer); done <- struct{}{} }()
		go func() { _, _ = io.Copy(downstream, upstream); done <- struct{}{} }()
		<-done
		_ = downstream.Close()
		_ = upstream.Close()
		<-done
	}))
	t.Cleanup(srv.Close)
	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)
	parsedPort, err := strconv.Atoi(port)
	require.NoError(t, err)
	return &Proxy{ID: 91, Protocol: "http", Host: host, Port: parsedPort, Status: StatusActive}
}

// This executes CreateLiveCall itself, then reads its stored record and dials
// both the new and legacy routes through real CONNECT/TLS/WS handshakes. Only
// account selection, slots and record storage are fakes in this focused test;
// the separate repository test covers the actual Redis read/write mapping.
func TestLiveCreatePersistsDefaultRouteAndLegacyDialRemainsCompatible(t *testing.T) {
	svc, _ := liveTransportTestService(t, true)
	account := svc.accountRepo.(*liveTestAccountRepo).account
	account.Extra["codex_tls_profile"] = "rustls"
	wsRequests := make(chan *http.Request, 2)
	connects := make(chan string, 2)
	tlsFixture := liveRouteTLSFixture(t, wsRequests)
	account.Proxy = liveRouteCONNECTFixture(t, tlsFixture.Listener.Addr().String(), connects)
	account.ProxyID = &account.Proxy.ID
	store := &liveRouteStore{liveTestStore: &liveTestStore{}, observerDone: make(chan struct{})}
	scheduler := &liveRouteScheduler{account: account}
	concurrency := &liveTestConcurrencyCache{}
	usage := &liveTestUsageRepo{}
	svc.cache = store
	svc.openaiScheduler = scheduler
	svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true")
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	svc.concurrencyService = NewConcurrencyService(concurrency)
	svc.usageLogRepo = usage
	svc.liveAttestation = liveAttestationStub{header: `{"v":1,"s":0,"t":"synthetic-route"}`}
	svc.httpUpstream = &liveWireHTTPUpstream{}
	createRequests := make(chan *http.Request, 1)
	createServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		createRequests <- r.Clone(context.Background())
		w.Header().Set("Location", "/backend-api/codex/realtime/calls/rtc_route_new")
		_, _ = io.WriteString(w, "v=0\r\n")
	}))
	defer createServer.Close()
	before := chatGPTLiveCallsURL
	chatGPTLiveCallsURL = createServer.URL + "/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas"
	defer func() { chatGPTLiveCallsURL = before }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	created, err := svc.CreateLiveCall(ctx, &LiveCallRequest{SDP: "v=offer\r\n", Session: []byte(`{"model":"gpt-live-1-codex","delegation":{"type":"client"}}`)},
		LiveCallIdentity{UserID: 42, APIKeyID: 43, InboundEndpoint: "/v1/live"}, 2)
	require.NoError(t, err)
	select {
	case <-store.observerDone:
	case <-ctx.Done():
		t.Fatal("observer did not return")
	}
	record, err := store.GetLiveCall(ctx, hashLiveCallID(created.CallID))
	require.NoError(t, err)
	require.Equal(t, liveSidebandRouteOpenAIV1, record.SidebandRoute, "must be written by CreateLiveCall, not the test")
	require.Equal(t, "/backend-api/codex/realtime/calls/rtc_route_new", created.Location)
	require.Equal(t, "/v1/live", record.InboundEndpoint)
	require.Equal(t, int64(42), record.UserID)
	require.Equal(t, int64(43), record.APIKeyID)
	require.Equal(t, int32(1), scheduler.released.Load())
	createRequest := <-createRequests
	account.Credentials["access_token"] = "different-after-create"
	for _, tc := range []struct{ name, route, host, path string }{
		{"new", record.SidebandRoute, "api.openai.com", "/v1/live/rtc_route_new"},
		{"legacy", "", "chatgpt.com", "/backend-api/codex/live/rtc_route_new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := *record
			copy.SidebandRoute = tc.route
			conn, dialErr := svc.dialLiveSideband(ctx, &copy)
			require.NoError(t, dialErr)
			defer conn.Close()
			messageType, payload, readErr := conn.ReadFrame(ctx)
			require.NoError(t, readErr)
			require.Equal(t, coderws.MessageText, messageType)
			require.Contains(t, string(payload), "session.started")
			request := <-wsRequests
			require.Equal(t, tc.host+":443", <-connects)
			require.Equal(t, tc.host, request.Host)
			require.Equal(t, tc.host, request.TLS.ServerName)
			require.Equal(t, tc.path, request.URL.Path)
			for _, name := range []string{"Authorization", "Chatgpt-Account-Id", "session-id", "thread-id", "x-session-id", "User-Agent", "originator", "version", "OpenAI-Alpha", liveAttestationHeader} {
				require.Equal(t, createRequest.Header.Get(name), request.Header.Get(name), name)
			}
			require.Empty(t, request.Header.Get("Sec-WebSocket-Extensions"))
		})
	}
	// Unknown values fail before account access or another network connection.
	svc.accountRepo = nil
	invalid := *record
	invalid.SidebandRoute = "https://unexpected.invalid"
	_, err = svc.dialLiveSideband(ctx, &invalid)
	require.ErrorIs(t, err, ErrLiveUnavailable)
	require.Empty(t, connects)
	require.Empty(t, wsRequests)
	svc.finalizeLiveCall(record)
	svc.finalizeLiveCall(record)
	concurrency.mu.Lock()
	require.Equal(t, 1, concurrency.releases)
	concurrency.mu.Unlock()
	usage.mu.Lock()
	require.Len(t, usage.logs, 1)
	usage.mu.Unlock()
}
