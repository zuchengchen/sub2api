package codexwire

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func writeUnmaskedTestFrame(w io.Writer, opcode byte, compressed bool, payload []byte) error {
	first := byte(0x80) | opcode
	if compressed {
		first |= 0x40
	}
	frame := []byte{first}
	if len(payload) < 126 {
		frame = append(frame, byte(len(payload)))
	} else {
		frame = append(frame, 126, byte(len(payload)>>8), byte(len(payload)))
	}
	frame = append(frame, payload...)
	_, err := w.Write(frame)
	return err
}

func TestWSInflatedReadLimitAndTakeover(t *testing.T) {
	for _, size := range []int{4096, 4097} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			c := NewWSConn(client, nil, &UpgradeResult{PerMessageDeflate: true})
			defer c.CloseNow()
			c.SetReadLimit(4096)
			compress := newPMDCompressor(true, 0)
			payload := bytes.Repeat([]byte("x"), size)
			wire, err := compress.Compress(payload)
			require.NoError(t, err)
			require.Less(t, len(wire), 4096)
			writeDone := make(chan error, 1)
			go func() { writeDone <- writeUnmaskedTestFrame(server, opcodeText, true, wire) }()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			got, err := c.ReadMessage(ctx)
			require.NoError(t, <-writeDone)
			if size > 4096 {
				require.Error(t, err, "compressed bytes below the limit must not bypass the inflated limit")
				require.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, payload, got)
			wire, err = compress.Compress(payload[:1024])
			require.NoError(t, err)
			go func() { writeDone <- writeUnmaskedTestFrame(server, opcodeText, true, wire) }()
			got, err = c.ReadMessage(ctx)
			require.NoError(t, <-writeDone)
			require.NoError(t, err)
			require.Equal(t, payload[:1024], got, "accepted frames must still advance the takeover dictionary")
		})
	}
}

func TestWSConcurrentCompressedWritesRemainDecodable(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	c := NewWSConn(client, nil, &UpgradeResult{PerMessageDeflate: true})
	defer c.CloseNow()
	peer := NewWSConn(server, nil, &UpgradeResult{PerMessageDeflate: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const workers, iterations = 8, 5
	readDone := make(chan error, 1)
	go func() {
		seen := make(map[string]bool)
		for i := 0; i < workers*iterations; i++ {
			got, err := peer.ReadMessage(ctx)
			if err != nil {
				readDone <- err
				return
			}
			prefix, body, ok := strings.Cut(string(got), ":")
			if !ok || seen[prefix] || body != strings.Repeat(prefix+"-shared-content-", 400) {
				readDone <- fmt.Errorf("corrupted or duplicate compressed message %q", prefix)
				return
			}
			seen[prefix] = true
		}
		readDone <- nil
	}()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				prefix := fmt.Sprintf("%d/%d", i, j)
				if err := c.WriteMessage(ctx, []byte(prefix+":"+strings.Repeat(prefix+"-shared-content-", 400))); err != nil {
					t.Errorf("write %s: %v", prefix, err)
					return
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()
	require.NoError(t, <-readDone)
}

type observedWriteConn struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *observedWriteConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(p)
}

func TestWSCloseInterruptsBlockedWriter(t *testing.T) {
	for _, normal := range []bool{false, true} {
		t.Run(fmt.Sprint(normal), func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			observed := &observedWriteConn{Conn: client, started: make(chan struct{})}
			c := NewWSConn(observed, nil, nil)
			defer c.CloseNow()
			writeDone := make(chan error, 1)
			go func() { writeDone <- c.WriteMessage(context.Background(), []byte("blocked")) }()
			<-observed.started
			closeDone := make(chan error, 1)
			go func() {
				if normal {
					closeDone <- c.CloseNormal()
				} else {
					closeDone <- c.Close()
				}
			}()
			select {
			case <-closeDone:
			case <-time.After(2 * time.Second):
				_ = c.CloseNow()
				<-closeDone
				t.Error("close waited forever behind a blocked writer")
			}
			require.Error(t, <-writeDone)
		})
	}
}

func TestWSPeerCloseReleasesTransport(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	c := NewWSConn(client, nil, nil)
	defer c.CloseNow()
	peerDone := make(chan error, 1)
	go func() {
		if err := writeUnmaskedTestFrame(server, opcodeClose, false, []byte{0x03, 0xe8}); err != nil {
			peerDone <- err
			return
		}
		br := bufio.NewReader(server)
		h, err := readFrameHeader(br)
		if err != nil {
			peerDone <- err
			return
		}
		if _, err = io.CopyN(io.Discard, br, int64(h.length)); err != nil {
			peerDone <- err
			return
		}
		_ = server.SetReadDeadline(time.Now().Add(time.Second))
		_, err = br.ReadByte()
		peerDone <- err
	}()
	_, err := c.ReadMessage(context.Background())
	var closeErr *ErrClosedByPeer
	require.ErrorAs(t, err, &closeErr)
	require.Equal(t, uint16(1000), closeErr.Code)
	require.NoError(t, c.Close(), "idempotent close after peer close")
	require.ErrorIs(t, <-peerDone, io.EOF, "echoing close must also release the socket")
}

func TestWSReadHonorsEarlyCancelWithFutureDeadline(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	observed := &observedReadConn{Conn: client, started: make(chan struct{})}
	c := NewWSConn(observed, nil, nil)
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	done := make(chan error, 1)
	go func() { _, err := c.ReadMessage(ctx); done <- err }()
	<-observed.started
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		_ = c.CloseNow()
		<-done
		t.Fatal("deadline-bearing context ignored early cancellation")
	}
}

type observedReadConn struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *observedReadConn) Read(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Read(p)
}

func TestDialWSValidatesUpgradeAndCancelsHandshake(t *testing.T) {
	for _, mode := range []string{"invalid_accept", "missing_upgrade", "missing_connection", "canceled", "valid"} {
		t.Run(mode, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := make(chan struct{})
			go func() {
				br := bufio.NewReader(server)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				close(ready)
				if mode == "canceled" {
					cancel()
					return
				}
				accept := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
				headers := http.Header{"Upgrade": {"websocket"}, "Connection": {"keep-alive, Upgrade"}, "Sec-Websocket-Accept": {base64.StdEncoding.EncodeToString(accept[:])}}
				switch mode {
				case "invalid_accept":
					headers.Set("Sec-WebSocket-Accept", "invalid")
				case "missing_upgrade":
					headers.Del("Upgrade")
				case "missing_connection":
					headers.Del("Connection")
				}
				var response strings.Builder
				response.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
				_ = headers.Write(&response)
				response.WriteString("\r\n")
				_, _ = io.WriteString(server, response.String())
			}()
			type result struct {
				conn *WSConn
				err  error
			}
			done := make(chan result, 1)
			go func() {
				conn, _, err := DialWS(ctx, "ws://example.invalid/responses", nil, "", DialOptions{BaseDialer: func(context.Context, string, string) (net.Conn, error) { return client, nil }})
				done <- result{conn, err}
			}()
			<-ready
			select {
			case got := <-done:
				if mode == "valid" {
					require.NoError(t, got.err)
					require.NotNil(t, got.conn)
					cancel() // handshake watcher must not own the established socket
					go func() { _ = writeUnmaskedTestFrame(server, opcodeText, false, []byte("ok")) }()
					readCtx, stop := context.WithTimeout(context.Background(), time.Second)
					defer stop()
					body, err := got.conn.ReadMessage(readCtx)
					require.NoError(t, err)
					require.Equal(t, "ok", string(body))
				} else {
					require.Error(t, got.err)
					require.Nil(t, got.conn)
				}
			case <-time.After(time.Second):
				_ = client.Close()
				<-done
				t.Fatal("handshake ignored cancellation")
			}
		})
	}
}

func TestHTTP1BodyCloseInterruptsUndrainedStream(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	defer client.Close()
	go func() {
		_, _ = http.ReadRequest(bufio.NewReader(server))
		_, _ = io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
	}()
	resp, err := writeHTTP1Request(context.Background(), client, "GET", "/", "example.test", nil, nil, 0)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- resp.Body.Close() }()
	select {
	case <-done:
	case <-time.After(time.Second):
		_ = client.Close()
		<-done
		t.Fatal("Body.Close tried to drain an endless SSE response before closing its socket")
	}
}
