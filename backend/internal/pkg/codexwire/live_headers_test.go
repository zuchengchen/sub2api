package codexwire

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func liveWireHeaders() http.Header {
	return http.Header{
		"User-Agent": {"codex_exec/0.166.0 (Linux; x86_64) terminal (codex_exec; 0.166.0)"},
		"Originator": {"codex_exec"}, "Version": {"0.166.0"}, "Authorization": {"Bearer synthetic-live"},
		"Chatgpt-Account-Id": {"acct-synthetic"}, "Openai-Alpha": {"quicksilver=v2"},
		"X-Session-Id": {"session-synthetic"}, "Session-Id": {"session-synthetic"}, "Thread-Id": {"session-synthetic"},
		"X-Oai-Attestation": {`{"v":1,"s":0,"t":"synthetic"}`},
	}
}

func liveRawCapture(t *testing.T, websocket bool) (string, <-chan []string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	seen := make(chan []string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(conn)
		if _, err := reader.ReadString('\n'); err != nil {
			return
		}
		var names []string
		length := 0
		key := ""
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" {
				break
			}
			name, value, _ := strings.Cut(strings.TrimSpace(line), ":")
			names = append(names, name)
			if strings.EqualFold(name, "Content-Length") {
				length, _ = strconv.Atoi(strings.TrimSpace(value))
			}
			if strings.EqualFold(name, "Sec-WebSocket-Key") {
				key = strings.TrimSpace(value)
			}
		}
		if _, err := io.CopyN(io.Discard, reader, int64(length)); err != nil {
			return
		}
		seen <- names
		if !websocket {
			_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
			return
		}
		accept := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(accept[:]))
		_, _ = io.Copy(io.Discard, conn)
	}()
	return ln.Addr().String(), seen
}

func TestLiveHTTPHeaderOrderOnActualSocket(t *testing.T) {
	addr, seen := liveRawCapture(t, false)
	headers := liveWireHeaders()
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "*/*")
	path := "/backend-api/codex/realtime/calls"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := DoHTTP1(ctx, "POST", "http://"+addr+path, OrderCodexHTTPHeadersForPath(path, headers), []byte(`{"sdp":"synthetic"}`), "", HTTP1Options{})
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, []string{"version", "openai-alpha", "x-session-id", "session-id", "thread-id", "x-oai-attestation", "authorization", "chatgpt-account-id", "content-type", "originator", "user-agent", "accept", "host", "content-length"}, <-seen)
}

func TestLiveWSHeaderOrderOnActualSocket(t *testing.T) {
	addr, seen := liveRawCapture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := DialWS(ctx, "ws://"+addr+"/backend-api/codex/live/rtc_synthetic", OrderCodexLiveWSHeaders(liveWireHeaders()), "", DialOptions{DisableCompression: true})
	require.NoError(t, err)
	defer conn.CloseNow()
	require.Equal(t, []string{"Host", "Connection", "Upgrade", "Sec-WebSocket-Version", "Sec-WebSocket-Key", "user-agent", "originator", "chatgpt-account-id", "authorization", "x-oai-attestation", "version", "openai-alpha", "x-session-id", "session-id", "thread-id"}, <-seen)
}

func TestLiveHeaderOrderingPreservesDeclaredMetadata(t *testing.T) {
	headers := liveWireHeaders()
	metadata := `{"thread_source":"explicit-source","extension":"keep"}`
	headers.Set("X-Codex-Turn-Metadata", metadata)
	for _, ordered := range [][]HeaderKV{OrderCodexHTTPHeadersForPath("/backend-api/codex/realtime/calls", headers), OrderCodexLiveWSHeaders(headers)} {
		found := false
		for _, h := range ordered {
			if h.Name == "x-codex-turn-metadata" {
				require.Equal(t, metadata, h.Value)
				found = true
			}
		}
		require.True(t, found)
	}
}
