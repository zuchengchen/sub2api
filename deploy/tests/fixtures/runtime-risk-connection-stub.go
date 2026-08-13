package main

import (
	"bufio"
	"crypto/sha1" // #nosec G505 -- WebSocket RFC 6455 requires SHA-1 for Sec-WebSocket-Accept.
	"encoding/base64"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const websocketMagic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func main() {
	address := flag.String("listen", "127.0.0.1:0", "listen address")
	connect := flag.String("connect", "", "connect to a WebSocket endpoint instead of serving")
	flag.Parse()
	if *connect != "" {
		if err := runWebSocketProbe(*connect); err != nil {
			panic(err)
		}
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"runtime-risk-stub"}`))
	})
	mux.HandleFunc("/http", streamHTTP)
	mux.HandleFunc("/sse", streamSSE)
	mux.HandleFunc("/ws", streamWebSocket)

	server := &http.Server{
		Addr:              *address,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		panic(err)
	}
}

func runWebSocketProbe(address string) error {
	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial WebSocket endpoint: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return fmt.Errorf("set WebSocket read deadline: %w", err)
	}

	request := fmt.Sprintf(
		"GET /ws HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n",
		address,
	)
	if _, err := io.WriteString(conn, request); err != nil {
		return fmt.Errorf("write WebSocket handshake: %w", err)
	}

	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		return fmt.Errorf("read WebSocket handshake: %w", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("unexpected WebSocket status: %s", response.Status)
	}
	if !strings.EqualFold(strings.TrimSpace(response.Header.Get("Upgrade")), "websocket") {
		return fmt.Errorf("WebSocket Upgrade response header is missing")
	}
	fmt.Println("101 Switching Protocols")

	buffer := make([]byte, 4096)
	for {
		if _, err := reader.Read(buffer); err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Println("connection closed")
				return nil
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return fmt.Errorf("WebSocket connection did not close before the probe deadline: %w", err)
			}
			fmt.Println("connection closed")
			return nil
		}
	}
}

func streamHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	streamTicks(w, r, func(index int) []byte { return []byte(fmt.Sprintf("tick-%d\n", index)) })
}

func streamSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	streamTicks(w, r, func(index int) []byte { return []byte(fmt.Sprintf("data: tick-%d\n\n", index)) })
}

func streamTicks(w http.ResponseWriter, r *http.Request, payload func(int) []byte) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", http.StatusInternalServerError)
		return
	}
	for index := 0; ; index++ {
		if _, err := w.Write(payload(index)); err != nil {
			return
		}
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func streamWebSocket(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") ||
		!headerHasToken(r.Header.Get("Connection"), "upgrade") {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		http.Error(w, "websocket key required", http.StatusBadRequest)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unavailable", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	digest := sha1.Sum([]byte(key + websocketMagic)) // #nosec G401 -- mandated by RFC 6455.
	accept := base64.StdEncoding.EncodeToString(digest[:])
	_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
	if err := rw.Flush(); err != nil {
		return
	}

	for index := 0; ; index++ {
		if err := writeWebSocketText(rw, fmt.Sprintf("tick-%d", index)); err != nil {
			return
		}
		if err := rw.Flush(); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func headerHasToken(value, token string) bool {
	for _, item := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(item), token) {
			return true
		}
	}
	return false
}

func writeWebSocketText(writer *bufio.ReadWriter, value string) error {
	payload := []byte(value)
	if err := writer.WriteByte(0x81); err != nil {
		return err
	}
	switch {
	case len(payload) < 126:
		if err := writer.WriteByte(byte(len(payload))); err != nil {
			return err
		}
	case len(payload) <= 65535:
		if err := writer.WriteByte(126); err != nil {
			return err
		}
		var size [2]byte
		binary.BigEndian.PutUint16(size[:], uint16(len(payload)))
		if _, err := writer.Write(size[:]); err != nil {
			return err
		}
	default:
		return fmt.Errorf("test payload is too large")
	}
	_, err := writer.Write(payload)
	return err
}
