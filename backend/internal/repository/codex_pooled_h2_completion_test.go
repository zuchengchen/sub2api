package repository

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/codexwire"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// This socket writes the real encrypted TLS record before delaying only its
// return to the H2 sender. TLS 1.3 AEAD adds 16 tag bytes and one inner type byte;
// the DATA frame adds nine bytes. The unique payload fits the first TLS record
// even with dynamic record sizing, and avoids matching any
// handshake, HPACK or control frame. No response or transport error is invented.
type pooledH2DelayedWrite struct {
	net.Conn
	bodyBytes int
	heldOnce  *atomic.Bool
	written   chan struct{}
	release   chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *pooledH2DelayedWrite) Write(raw []byte) (int, error) {
	n, err := c.Conn.Write(raw)
	if err != nil || n != len(raw) || len(raw) != c.bodyBytes+31 || raw[0] != 23 ||
		int(binary.BigEndian.Uint16(raw[3:5])) != len(raw)-5 || !c.heldOnce.CompareAndSwap(false, true) {
		return n, err
	}
	close(c.written)
	select {
	case <-c.release:
	case <-c.closed:
	}
	return n, err
}

func (c *pooledH2DelayedWrite) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func pooledH2CompletionFixture(t *testing.T, waitForCancellation bool) (*http.Client, string, string, <-chan string, <-chan struct{}, func(), *atomic.Int32, *atomic.Int32) {
	t.Helper()
	t.Setenv("SSL_CERT_FILE", "")
	payload := "grant_type=" + strings.Repeat("x", 512)
	peers := make(chan string, 4)
	var requests, dials atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/warmup" {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != payload || r.ProtoMajor != 2 {
			http.Error(w, "synthetic request mismatch", http.StatusBadRequest)
			return
		}
		requests.Add(1)
		peers <- r.RemoteAddr
		if waitForCancellation {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "synthetic-ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600))
	t.Setenv("CODEX_CA_CERTIFICATE", ca)
	profile, err := codexwire.WithCodexCustomCA(tlsfingerprint.BuiltinProfile("rustls-h2"))
	require.NoError(t, err)
	written, release := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	pool := codexwire.NewH2Pool(codexwire.H2Options{Timeout: 10 * time.Second, LogKey: "synthetic-login-settlement",
		BaseDialer: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			dials.Add(1)
			return &pooledH2DelayedWrite{Conn: conn, bodyBytes: len(payload), heldOnce: &held, written: written, release: release, closed: make(chan struct{})}, nil
		}})
	t.Cleanup(pool.CloseAll)
	transport := &codexDeviceRoundTripper{profile: profile, authCode: true, authH2: true, codeH2: pool, timeout: 10 * time.Second}
	client := &http.Client{Transport: transport}
	// Complete the initial SETTINGS/ACK before deliberately holding writeMu on
	// request DATA. Otherwise the read loop legitimately waits to ACK SETTINGS
	// before it can deliver response frames, so no response-close race exists.
	warm, err := client.Get(srv.URL + "/warmup")
	require.NoError(t, err)
	_, err = io.ReadAll(warm.Body)
	require.NoError(t, err)
	require.NoError(t, warm.Body.Close())
	return client, srv.URL + "/oauth/token", payload, peers, written, unblock, &requests, &dials
}

func TestCodexLoginPooledH2CompleteBodyWaitsForWriterAndReuses(t *testing.T) {
	client, endpoint, payload, peers, written, release, requests, dials := pooledH2CompletionFixture(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	post := func() *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(payload))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, `{}`, string(body))
		return resp
	}
	first := post()
	select {
	case <-written:
	case <-ctx.Done():
		t.Fatal("real final DATA write was not observed")
	}
	closed := make(chan error, 1)
	go func() { closed <- first.Body.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("normal body Close canceled an unfinished successful DATA write: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("normal body Close did not join the released writer")
	}
	second := post()
	require.NoError(t, second.Body.Close())
	firstPeer, secondPeer := <-peers, <-peers
	require.Equal(t, firstPeer, secondPeer, "one login client must keep its actual H2 socket")
	require.EqualValues(t, 1, dials.Load())
	require.EqualValues(t, 2, requests.Load(), "each intended POST must execute exactly once")
}

func TestCodexLoginPooledH2ExplicitCancelAbortsWriterWithoutReplay(t *testing.T) {
	client, endpoint, payload, peers, written, release, requests, dials := pooledH2CompletionFixture(t, true)
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(payload))
		if err != nil {
			done <- err
			return
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-written:
	case <-time.After(5 * time.Second):
		t.Fatal("real final DATA write was not observed")
	}
	select {
	case <-peers:
	case <-time.After(5 * time.Second):
		t.Fatal("synthetic upstream did not receive the one request")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("explicit send cancellation did not unblock the request")
	}
	require.EqualValues(t, 1, requests.Load(), "a submitted code POST must not be replayed")
	require.EqualValues(t, 1, dials.Load(), "explicit cancellation cannot reconnect and replay")
}
