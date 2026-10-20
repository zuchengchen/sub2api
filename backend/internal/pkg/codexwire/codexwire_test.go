package codexwire

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestBuildWSUpgradeRequest_ByteLayout(t *testing.T) {
	ordered := []HeaderKV{
		{Name: "chatgpt-account-id", Value: "acc"},
		{Name: "authorization", Value: "Bearer t"},
		{Name: "user-agent", Value: "codex/1.0"},
		{Name: "x-codex-turn-metadata", Value: "{}"},
		{Name: "cookie", Value: "c=1"},
	}
	raw, key, err := BuildWSUpgradeRequest("wss://chatgpt.com/backend-api/codex/responses?x=1", ordered, "AAAAAAAAAAAAAAAAAAAAAA==")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if key != "AAAAAAAAAAAAAAAAAAAAAA==" {
		t.Fatalf("sec key mismatch: %q", key)
	}
	want := "GET /backend-api/codex/responses?x=1 HTTP/1.1\r\n" +
		"Host: chatgpt.com\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: AAAAAAAAAAAAAAAAAAAAAA==\r\n" +
		"chatgpt-account-id: acc\r\n" +
		"authorization: Bearer t\r\n" +
		"user-agent: codex/1.0\r\n" +
		"x-codex-turn-metadata: {}\r\n" +
		"cookie: c=1\r\n" +
		"sec-websocket-extensions: permessage-deflate; client_max_window_bits\r\n" +
		"\r\n"
	if string(raw) != want {
		t.Fatalf("layout mismatch:\n--- got ---\n%s\n--- want ---\n%s", raw, want)
	}
}

func TestOrderCodexWSHeaders_TemplateOrder(t *testing.T) {
	h := http.Header{
		"User-Agent":            {"codex/1.0"},
		"Authorization":         {"Bearer t"},
		"X-Codex-Turn-Metadata": {"{}"},
		"Session-Id":            {"s1"},
		"X-Codex-Turn-State":    {"ts"},
		"Openai-Beta":           {"responses=v1"},
	}
	ordered, leftover := SplitCodexWSHeaders(h)
	var names []string
	for _, kv := range ordered {
		names = append(names, kv.Name)
	}
	want := []string{"authorization", "user-agent", "openai-beta", "session-id", "x-codex-turn-metadata"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("order mismatch: %v want %v", names, want)
	}
	// x-codex-turn-state 不在握手模板里 → 进入 leftover 由调用方挪到 client_metadata。
	if len(leftover) != 1 || !strings.EqualFold(leftover[0].Name, "x-codex-turn-state") {
		t.Fatalf("leftover should carry turn-state: %+v", leftover)
	}
}

func TestFrameRoundTrip_MaskedClientFrame(t *testing.T) {
	payload := []byte(`{"type":"response.create"}`)
	var buf bytes.Buffer
	if err := writeFrame(&buf, true, false, opcodeText, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(&buf)
	h, err := readFrameHeader(br)
	if err != nil {
		t.Fatalf("read header: %v", err)
	}
	if !h.fin || !h.masked || h.opcode != opcodeText || h.length != uint64(len(payload)) {
		t.Fatalf("header mismatch: %+v", h)
	}
	body := make([]byte, h.length)
	if _, err := io.ReadFull(br, body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	for i := range body {
		body[i] ^= h.maskKey[i%4]
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("payload mismatch: %q", body)
	}
}

func TestPMDRoundTrip_ContextTakeover(t *testing.T) {
	comp := newPMDCompressor(true)
	decomp := newPMDDecompressor(true)
	msgs := []string{
		`{"type":"response.create","input":[{"type":"message","content":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`,
		`{"type":"response.create","input":[{"type":"message","content":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`,
	}
	var firstLen int
	for i, m := range msgs {
		wire, err := comp.Compress([]byte(m))
		if err != nil {
			t.Fatalf("compress %d: %v", i, err)
		}
		if bytes.HasSuffix(wire, []byte{0x00, 0x00, 0xff, 0xff}) {
			t.Fatalf("tail not stripped msg %d", i)
		}
		out, err := decomp.Decompress(wire)
		if err != nil {
			t.Fatalf("decompress %d: %v", i, err)
		}
		if string(out) != m {
			t.Fatalf("roundtrip mismatch %d: %q", i, out)
		}
		if i == 0 {
			firstLen = len(wire)
		} else if len(wire) >= firstLen {
			t.Fatalf("context takeover should shrink repeat message: %d >= %d", len(wire), firstLen)
		}
	}
}

func TestPMDRoundTrip_NoContextTakeover(t *testing.T) {
	comp := newPMDCompressor(false)
	decomp := newPMDDecompressor(false)
	msg := `{"a":1,"b":"xyz"}`
	for i := 0; i < 2; i++ {
		wire, err := comp.Compress([]byte(msg))
		if err != nil {
			t.Fatalf("compress: %v", err)
		}
		out, err := decomp.Decompress(wire)
		if err != nil {
			t.Fatalf("decompress: %v", err)
		}
		if string(out) != msg {
			t.Fatalf("mismatch: %q", out)
		}
	}
}

// wsPipeServer 是一个最小的 WS 服务端：完成 101 握手（不带扩展），
// 回显一条 text 消息、发一条 ping、再发 close。
func wsPipeServer(t *testing.T, conn net.Conn, deflate bool) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	// 读握手请求到空行。
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		if line == "\r\n" {
			break
		}
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: dummy\r\n"
	if deflate {
		resp += "Sec-WebSocket-Extensions: permessage-deflate\r\n"
	}
	resp += "\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		return
	}
	// 读客户端一条帧（masked），回显 unmasked。
	h, err := readFrameHeader(br)
	if err != nil {
		return
	}
	payload := make([]byte, h.length)
	if _, err := io.ReadFull(br, payload); err != nil {
		return
	}
	if h.masked {
		for i := range payload {
			payload[i] ^= h.maskKey[i%4]
		}
	}
	if deflate && h.rsv1 {
		src := append(append([]byte(nil), payload...), pmdReadTail...)
		r := flate.NewReader(bytes.NewReader(src))
		payload, err = io.ReadAll(r)
		if err != nil {
			return
		}
	}
	// 回显 + ping + close（服务端帧不 mask）。
	writeServerFrame := func(opcode byte, p []byte, rsv1 bool) {
		var head [10]byte
		b0 := opcode | 0x80
		if rsv1 {
			b0 |= 0x40
		}
		head[0] = b0
		var n int
		switch l := len(p); {
		case l < 126:
			head[1] = byte(l)
			n = 2
		case l <= 0xffff:
			head[1] = 126
			head[2] = byte(l >> 8)
			head[3] = byte(l)
			n = 4
		default:
			return
		}
		conn.Write(head[:n])
		conn.Write(p)
	}
	echoPayload := payload
	if deflate {
		var cbuf bytes.Buffer
		w, _ := flate.NewWriter(&cbuf, flate.DefaultCompression)
		w.Write(payload)
		w.Flush()
		echoPayload = cbuf.Bytes()
		if len(echoPayload) >= 4 && bytes.Equal(echoPayload[len(echoPayload)-4:], pmdTail) {
			echoPayload = echoPayload[:len(echoPayload)-4]
		}
	}
	writeServerFrame(opcodeText, echoPayload, deflate)
	writeServerFrame(opcodePing, []byte("hb"), false)
	// 读掉客户端对 ping 的自动 pong（net.Pipe 无缓冲，必须消费客户端写）。
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if ph, err := readFrameHeader(br); err == nil && ph.opcode == opcodePong {
		io.CopyN(io.Discard, br, int64(ph.length))
	}
	writeServerFrame(opcodeClose, []byte{0x03, 0xe8}, false)
	// 客户端会回 close echo，读掉即可。
	if ch, err := readFrameHeader(br); err == nil {
		io.CopyN(io.Discard, br, int64(ch.length))
	}
}

func TestWSConnRoundTrip_Plain(t *testing.T) {
	client, server := net.Pipe()
	go wsPipeServer(t, server, false)

	c := wsClientHandshake(t, client, false)
	ctx := context.Background()
	msg := `{"hello":"ws"}`
	if err := c.WriteMessage(ctx, []byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}
	payload, err := c.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(payload) != msg {
		t.Fatalf("echo mismatch: %q", payload)
	}
	// 下一条读取：ping 自动应答后返回 close error。
	_, err = c.ReadMessage(ctx)
	var peerErr *ErrClosedByPeer
	if err == nil || !errors.As(err, &peerErr) {
		t.Fatalf("expected ErrClosedByPeer, got %v", err)
	}
	if peerErr.Code != 1000 {
		t.Fatalf("close code %d", peerErr.Code)
	}
	if c.UpstreamPingCount() != 1 {
		t.Fatalf("ping count %d", c.UpstreamPingCount())
	}
}

// wsClientHandshake 通过 net.Pipe 完成一次真实的握手交换后构造 WSConn。
func wsClientHandshake(t *testing.T, conn net.Conn, deflate bool) *WSConn {
	t.Helper()
	ordered := []HeaderKV{{Name: "authorization", Value: "Bearer t"}}
	reqBytes, _, err := BuildWSUpgradeRequest("ws://example.test/ws", ordered, "")
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if _, err := conn.Write(reqBytes); err != nil {
		t.Fatalf("write request: %v", err)
	}
	br := bufio.NewReader(conn)
	res, err := ReadUpgradeResponse(br, 0)
	if err != nil {
		t.Fatalf("read upgrade: %v", err)
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", res.StatusCode)
	}
	if deflate != res.PerMessageDeflate {
		t.Fatalf("deflate negotiation mismatch: %v", res.PerMessageDeflate)
	}
	return NewWSConn(conn, br, res)
}

func TestWSConnRoundTrip_Deflate(t *testing.T) {
	client, server := net.Pipe()
	go wsPipeServer(t, server, true)

	c := wsClientHandshake(t, client, true)
	ctx := context.Background()
	msg := `{"compressed":"ws","pad":"pppppppppppppppppppppppppp"}`
	if err := c.WriteMessage(ctx, []byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}
	payload, err := c.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(payload) != msg {
		t.Fatalf("echo mismatch: %q", payload)
	}
}

func TestWSConnClose_WritesCloseFrame(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go func() {
		br := bufio.NewReader(server)
		h, err := readFrameHeader(br)
		if err != nil {
			return
		}
		if h.opcode != opcodeClose {
			return
		}
	}()
	c := NewWSConn(client, bufio.NewReader(client), nil)
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := c.WriteMessage(context.Background(), []byte("x")); err == nil {
		t.Fatalf("write after close should fail")
	}
}

func TestReadUpgradeResponse_101AndError(t *testing.T) {
	// 101 with extension negotiation.
	raw101 := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Extensions: permessage-deflate; server_no_context_takeover\r\n" +
		"\r\n"
	res, err := ReadUpgradeResponse(bufio.NewReader(strings.NewReader(raw101)), 0)
	if err != nil {
		t.Fatalf("read 101: %v", err)
	}
	if !res.PerMessageDeflate || !res.ServerNoContextTakeover || res.ClientNoContextTakeover {
		t.Fatalf("negotiation parse wrong: %+v", res)
	}
	// 403 with body.
	raw403 := "HTTP/1.1 403 Forbidden\r\nContent-Length: 5\r\n\r\nnope!"
	res, err = ReadUpgradeResponse(bufio.NewReader(strings.NewReader(raw403)), 0)
	if err != nil {
		t.Fatalf("read 403: %v", err)
	}
	if res.StatusCode != 403 || string(res.Body) != "nope!" {
		t.Fatalf("error result wrong: %+v body=%q", res, res.Body)
	}
}
