package service

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/codexwire"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// These are real loopback TCP sockets after the HTTP Upgrade boundary. The
// peer records every outgoing byte, so an extra Close frame cannot disappear
// behind a mock Close implementation or an event normalizer.
func nativeResponsesClosePair(t *testing.T) (*codexWireClientConn, net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 1)
	failed := make(chan error, 1)
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			failed <- e
			return
		}
		accepted <- conn
	}()
	client, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	var peer net.Conn
	select {
	case peer = <-accepted:
	case e := <-failed:
		t.Fatal(e)
	case <-time.After(time.Second):
		t.Fatal("no loopback TCP peer")
	}
	t.Cleanup(func() { _ = peer.Close() })
	require.NoError(t, peer.SetDeadline(time.Now().Add(3*time.Second)))
	return &codexWireClientConn{conn: codexwire.NewWSConn(client, nil, nil)}, peer
}
func nativeResponsesCloseBytes(t *testing.T, peer net.Conn) []byte {
	t.Helper()
	raw, err := io.ReadAll(peer)
	require.NoError(t, err, "socket must reach EOF before deadline")
	return raw
}
func nativeResponsesDecodeClose(t *testing.T, raw []byte) []byte {
	t.Helper()
	require.GreaterOrEqual(t, len(raw), 6)
	require.Equal(t, byte(0x88), raw[0])
	require.NotZero(t, raw[1]&0x80, "client controls must be masked")
	n := int(raw[1] & 0x7f)
	require.Equal(t, 6+n, len(raw), "exactly one close frame")
	payload := append([]byte(nil), raw[6:]...)
	for i := range payload {
		payload[i] ^= raw[2+i%4]
	}
	return payload
}
func TestCodexResponsesNativeCloseDropsSocket(t *testing.T) {
	conn, peer := nativeResponsesClosePair(t)
	require.NoError(t, conn.Close())
	require.NoError(t, conn.Close())
	require.Empty(t, nativeResponsesCloseBytes(t, peer), "Responses owner shutdown must be direct EOF, without a Close frame")
}
func TestCodexResponsesNativeClosePreservesOtherProtocols(t *testing.T) {
	t.Run("generic_empty_close", func(t *testing.T) {
		conn, peer := nativeResponsesClosePair(t)
		require.NoError(t, conn.conn.Close())
		require.Empty(t, nativeResponsesDecodeClose(t, nativeResponsesCloseBytes(t, peer)))
	})
	t.Run("live_code_1000", func(t *testing.T) {
		conn, peer := nativeResponsesClosePair(t)
		live := &liveCodexWireConn{conn}
		require.NoError(t, live.Close())
		require.NoError(t, live.Close())
		require.Equal(t, []byte{0x03, 0xe8}, nativeResponsesDecodeClose(t, nativeResponsesCloseBytes(t, peer)))
	})
	t.Run("peer_close_still_echoes", func(t *testing.T) {
		conn, peer := nativeResponsesClosePair(t)
		_, err := peer.Write([]byte{0x88, 0x02, 0x03, 0xe8})
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err = conn.ReadMessage(ctx)
		require.Equal(t, coderws.StatusNormalClosure, coderws.CloseStatus(err))
		require.NoError(t, conn.Close())
		require.Equal(t, []byte{0x03, 0xe8}, nativeResponsesDecodeClose(t, nativeResponsesCloseBytes(t, peer)))
	})
}

type nativeResponsesCloseDialer struct {
	conn  openAIWSClientConn
	calls atomic.Int64
}

func (d *nativeResponsesCloseDialer) Dial(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error) {
	d.calls.Add(1)
	return d.conn, 101, http.Header{}, nil
}
func TestCodexResponsesNativePoolRetirementDropsSocket(t *testing.T) {
	conn, peer := nativeResponsesClosePair(t)
	cfg := newOpenAIWSExecutionScopeTestConfig()
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	pool := newOpenAIWSConnPool(cfg)
	defer pool.Close()
	dialer := &nativeResponsesCloseDialer{conn: conn}
	pool.setClientDialerForTest(dialer)
	account := &Account{ID: 100, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"access_token": "synthetic-only", "chatgpt_account_id": "acct-golden", "chatgpt_user_id": "user-golden"}}
	req := openAIWSAcquireRequest{Account: account, WSURL: "ws://127.0.0.1/responses", Headers: http.Header{"Session-Id": {"native-close-session"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	first, err := pool.Acquire(ctx, req)
	require.NoError(t, err)
	id := first.ConnID()
	first.Release()
	second, err := pool.Acquire(ctx, req)
	require.NoError(t, err)
	require.Equal(t, id, second.ConnID(), "release must preserve pooled connection reuse")
	require.True(t, second.Reused())
	second.Release()
	require.EqualValues(t, 1, dialer.calls.Load())
	// The explicit pool retirement must reach the adapter Close path. Release
	// alone has already been proven to retain the same physical connection.
	pool.Close()
	require.Empty(t, nativeResponsesCloseBytes(t, peer), "native pool retirement must not append empty-payload Close")
}
