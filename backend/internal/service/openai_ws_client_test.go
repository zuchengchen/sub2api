package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestOpenAIWSAccountDialerRawAndFallback(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.ExactFidelityEnabled = true
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", "raw-handshake")
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.CloseNow()
		kind, frame, err := conn.Read(r.Context())
		if err == nil {
			err = conn.Write(r.Context(), kind, frame)
		}
		serverErr <- err
	}))
	defer server.Close()
	fallback := &openAIWSCaptureDialer{conn: &openAIWSCaptureConn{}}
	req := openAIWSAcquireRequest{Account: &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, WSURL: "ws" + strings.TrimPrefix(server.URL, "http")}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, headers, err := dialOpenAIWSForAccount(ctx, cfg, req, http.Header{"Authorization": {"Bearer fake"}}, fallback)
	require.NoError(t, err)
	defer conn.Close()
	require.IsType(t, &codexWireClientConn{}, conn)
	require.Zero(t, fallback.DialCount())
	require.Equal(t, "raw-handshake", headers.Get("X-Request-ID"))
	frameConn := conn.(openaiwsv2.FrameConn)
	require.NoError(t, frameConn.WriteFrame(ctx, coderws.MessageBinary, []byte(`{"type":"response.create"}`)))
	kind, frame, err := frameConn.ReadFrame(ctx)
	require.NoError(t, err)
	require.Equal(t, coderws.MessageBinary, kind)
	require.JSONEq(t, `{"type":"response.create"}`, string(frame))
	require.NoError(t, <-serverErr)
	req.Account.Type = AccountTypeAPIKey
	_, _, _, err = dialOpenAIWSForAccount(ctx, cfg, req, nil, fallback)
	require.NoError(t, err)
	require.Equal(t, 1, fallback.DialCount(), "API-key accounts retain their existing dialer")
}

func TestOpenAIWSAccountDialerHandshakeFailureAndProxy(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.ExactFidelityEnabled = true
	fallback := &openAIWSCaptureDialer{conn: &openAIWSCaptureConn{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", "failed-handshake")
		w.WriteHeader(http.StatusUpgradeRequired)
		_, _ = w.Write([]byte(`{"error":"upgrade"}`))
	}))
	defer server.Close()
	req := openAIWSAcquireRequest{Account: &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, WSURL: "ws" + strings.TrimPrefix(server.URL, "http")}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, status, headers, err := dialOpenAIWSForAccount(ctx, cfg, req, nil, fallback)
	require.Error(t, err)
	require.Nil(t, conn)
	require.Equal(t, http.StatusUpgradeRequired, status)
	require.Equal(t, "failed-handshake", headers.Get("X-Request-ID"))
	var handshakeErr *openAIWSHandshakeError
	require.ErrorAs(t, err, &handshakeErr)
	require.JSONEq(t, `{"error":"upgrade"}`, string(handshakeErr.Body))
	proxyRequests := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyRequests <- r.Method + " " + r.Host
		w.WriteHeader(http.StatusForbidden)
	}))
	defer proxy.Close()
	req.WSURL = "wss://codex-proxy-test.invalid/responses"
	req.ProxyURL = proxy.URL
	_, _, _, err = dialOpenAIWSForAccount(ctx, cfg, req, nil, fallback)
	require.Error(t, err)
	select {
	case request := <-proxyRequests:
		require.Equal(t, "CONNECT codex-proxy-test.invalid:443", request)
	case <-ctx.Done():
		t.Fatal("configured proxy was not used")
	}
	require.Zero(t, fallback.DialCount())
	// A plaintext proxy stays on the existing generic interface.
	req.WSURL = "ws://codex-proxy-test.invalid/responses"
	_, _, _, err = dialOpenAIWSForAccount(ctx, cfg, req, nil, fallback)
	require.NoError(t, err)
	require.Equal(t, 1, fallback.DialCount())
}

func TestCoderOpenAIWSClientDialer_ProxyHTTPClientReuse(t *testing.T) {
	dialer := newDefaultOpenAIWSClientDialer()
	impl, ok := dialer.(*coderOpenAIWSClientDialer)
	require.True(t, ok)

	c1, err := impl.proxyHTTPClient("http://127.0.0.1:8080")
	require.NoError(t, err)
	c2, err := impl.proxyHTTPClient("http://127.0.0.1:8080")
	require.NoError(t, err)
	require.Same(t, c1, c2, "同一代理地址应复用同一个 HTTP 客户端")

	c3, err := impl.proxyHTTPClient("http://127.0.0.1:8081")
	require.NoError(t, err)
	require.NotSame(t, c1, c3, "不同代理地址应分离客户端")
}

func TestCoderOpenAIWSClientDialer_ProxyHTTPClientInvalidURL(t *testing.T) {
	dialer := newDefaultOpenAIWSClientDialer()
	impl, ok := dialer.(*coderOpenAIWSClientDialer)
	require.True(t, ok)

	_, err := impl.proxyHTTPClient("://bad")
	require.Error(t, err)
}

func TestCoderOpenAIWSClientDialer_TransportMetricsSnapshot(t *testing.T) {
	dialer := newDefaultOpenAIWSClientDialer()
	impl, ok := dialer.(*coderOpenAIWSClientDialer)
	require.True(t, ok)

	_, err := impl.proxyHTTPClient("http://127.0.0.1:18080")
	require.NoError(t, err)
	_, err = impl.proxyHTTPClient("http://127.0.0.1:18080")
	require.NoError(t, err)
	_, err = impl.proxyHTTPClient("http://127.0.0.1:18081")
	require.NoError(t, err)

	snapshot := impl.SnapshotTransportMetrics()
	require.Equal(t, int64(1), snapshot.ProxyClientCacheHits)
	require.Equal(t, int64(2), snapshot.ProxyClientCacheMisses)
	require.InDelta(t, 1.0/3.0, snapshot.TransportReuseRatio, 0.0001)
}

func TestCoderOpenAIWSClientDialer_ProxyClientCacheCapacity(t *testing.T) {
	dialer := newDefaultOpenAIWSClientDialer()
	impl, ok := dialer.(*coderOpenAIWSClientDialer)
	require.True(t, ok)

	total := openAIWSProxyClientCacheMaxEntries + 32
	for i := 0; i < total; i++ {
		_, err := impl.proxyHTTPClient(fmt.Sprintf("http://127.0.0.1:%d", 20000+i))
		require.NoError(t, err)
	}

	impl.proxyMu.Lock()
	cacheSize := len(impl.proxyClients)
	impl.proxyMu.Unlock()

	require.LessOrEqual(t, cacheSize, openAIWSProxyClientCacheMaxEntries, "代理客户端缓存应受容量上限约束")
}

func TestCoderOpenAIWSClientDialer_ProxyClientCacheIdleTTL(t *testing.T) {
	dialer := newDefaultOpenAIWSClientDialer()
	impl, ok := dialer.(*coderOpenAIWSClientDialer)
	require.True(t, ok)

	oldProxy := "http://127.0.0.1:28080"
	_, err := impl.proxyHTTPClient(oldProxy)
	require.NoError(t, err)

	impl.proxyMu.Lock()
	oldEntry := impl.proxyClients[oldProxy]
	require.NotNil(t, oldEntry)
	oldEntry.lastUsedUnixNano = time.Now().Add(-openAIWSProxyClientCacheIdleTTL - time.Minute).UnixNano()
	impl.proxyMu.Unlock()

	// 触发一次新的代理获取，驱动 TTL 清理。
	_, err = impl.proxyHTTPClient("http://127.0.0.1:28081")
	require.NoError(t, err)

	impl.proxyMu.Lock()
	_, exists := impl.proxyClients[oldProxy]
	impl.proxyMu.Unlock()

	require.False(t, exists, "超过空闲 TTL 的代理客户端应被回收")
}

func TestCoderOpenAIWSClientDialer_ProxyTransportTLSHandshakeTimeout(t *testing.T) {
	dialer := newDefaultOpenAIWSClientDialer()
	impl, ok := dialer.(*coderOpenAIWSClientDialer)
	require.True(t, ok)

	client, err := impl.proxyHTTPClient("http://127.0.0.1:38080")
	require.NoError(t, err)
	require.NotNil(t, client)

	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, transport)
	require.Equal(t, 10*time.Second, transport.TLSHandshakeTimeout)
}

func TestCoderOpenAIWSClientConn_DoesNotSupportIdlePingWithoutReader(t *testing.T) {
	require.False(t, (&coderOpenAIWSClientConn{}).SupportsIdlePingWithoutReader())
}
