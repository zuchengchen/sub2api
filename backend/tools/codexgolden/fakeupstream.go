//go:build linux

package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
)

// scenario controls how the fake upstream answers.
type scenario struct {
	Name string
	// RejectWebSocket answers every upgrade with 426 so Codex falls back to
	// HTTP SSE for the rest of the session.
	RejectWebSocket bool
	// ToolLoop makes the first real turn return an exec_command function call
	// so the recording includes the in-turn follow-up request.
	ToolLoop bool
	// UnauthorizedUntilRefresh answers 401 to Responses requests until Codex
	// has refreshed its token against POST /oauth/token, so the recording
	// includes the unauthorized-recovery refresh request.
	UnauthorizedUntilRefresh bool
}

// headerKV preserves the exact wire name casing and order.
type headerKV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// record is one client→server unit observed on the wire.
type record struct {
	Seq          int             `json:"seq"`
	Conn         int             `json:"conn"`
	ConnReq      int             `json:"conn_request_index"`
	Kind         string          `json:"kind"` // http_request | ws_handshake | ws_message
	Method       string          `json:"method,omitempty"`
	Target       string          `json:"target,omitempty"`
	Proto        string          `json:"proto,omitempty"`
	Headers      []headerKV      `json:"headers,omitempty"`
	BodyEncoding string          `json:"body_encoding,omitempty"`
	BodyWireLen  int             `json:"body_wire_len,omitempty"`
	BodyJSON     json.RawMessage `json:"body_json,omitempty"`
	BodyText     string          `json:"body_text,omitempty"`
	Frame        *frameInfo      `json:"frame,omitempty"`
}

type frameInfo struct {
	Opcode     int  `json:"opcode"`
	Fragments  int  `json:"fragments"`
	Masked     bool `json:"masked"`
	Compressed bool `json:"compressed"`
	WireLen    int  `json:"wire_len"`
}

// upstreamOptions configures what the fake ChatGPT backend answers beyond the
// scripted Responses turns.
type upstreamOptions struct {
	// WorkspaceOrigin is returned as workspace_backend_origin from
	// /wham/accounts/check. Codex rewrites the Responses provider origin to it
	// (must be https).
	WorkspaceOrigin string
	// ModelsJSON is served for GET /codex/models (404 when empty, so Codex
	// falls back to its bundled catalog).
	ModelsJSON []byte
	// TLS terminates TLS on the listener when set.
	TLS *tls.Config
	// OnHello observes every TLS ClientHello before the handshake.
	OnHello func(*clientHello)
}

type fakeUpstream struct {
	ln       net.Listener
	scenario scenario
	opts     upstreamOptions
	mu       sync.Mutex
	records  []record
	seq      int
	connSeq  atomic.Int64
	respSeq  atomic.Int64
	wg       sync.WaitGroup
	// refreshedAccess is the access token issued by /oauth/token.
	refreshedAccess atomic.Value
}

func newFakeUpstream(sc scenario, opts upstreamOptions) (*fakeUpstream, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &fakeUpstream{ln: ln, scenario: sc, opts: opts}
	go f.serve()
	return f, nil
}

func (f *fakeUpstream) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeUpstream) close() []record {
	_ = f.ln.Close()
	done := make(chan struct{})
	go func() { f.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]record(nil), f.records...)
}

func (f *fakeUpstream) add(r record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	r.Seq = f.seq
	f.records = append(f.records, r)
}

func (f *fakeUpstream) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer func() { _ = c.Close() }()
			conn := net.Conn(c)
			if f.opts.TLS != nil {
				pc, hello, err := peekClientHello(c)
				if err != nil {
					return
				}
				if hello != nil && f.opts.OnHello != nil {
					f.opts.OnHello(hello)
				}
				tc := tls.Server(pc, f.opts.TLS)
				_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
				if err := tc.Handshake(); err != nil {
					return
				}
				_ = tc.SetDeadline(time.Time{})
				conn = tc
			}
			f.handleConn(conn, int(f.connSeq.Add(1)))
		}()
	}
}

// prefixConn replays already-read bytes before reading from the socket.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (p *prefixConn) Read(b []byte) (int, error) { return p.r.Read(b) }

// peekClientHello reads the first TLS record, parses it, and returns a conn
// that replays it so the real handshake can proceed.
func peekClientHello(c net.Conn) (net.Conn, *clientHello, error) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	rec, err := readTLSRecord(br)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, nil, err
	}
	hello, _ := parseClientHello(rec)
	// br may hold bytes past the record; keep reading through it.
	return &prefixConn{Conn: c, r: io.MultiReader(bytes.NewReader(rec), br)}, hello, nil
}

func (f *fakeUpstream) handleConn(c net.Conn, connID int) {
	br := bufio.NewReader(c)
	for reqIdx := 1; ; reqIdx++ {
		_ = c.SetReadDeadline(time.Now().Add(120 * time.Second))
		method, target, proto, headers, err := readRequestHead(br)
		if err != nil {
			return
		}
		get := func(name string) string {
			for _, h := range headers {
				if strings.EqualFold(h.Name, name) {
					return h.Value
				}
			}
			return ""
		}
		if strings.EqualFold(get("upgrade"), "websocket") {
			f.add(record{Conn: connID, ConnReq: reqIdx, Kind: "ws_handshake", Method: method, Target: target, Proto: proto, Headers: headers})
			if f.scenario.RejectWebSocket {
				_, _ = io.WriteString(c, "HTTP/1.1 426 Upgrade Required\r\ncontent-length: 0\r\n\r\n")
				continue
			}
			f.serveWebSocket(c, br, connID, get("sec-websocket-key"), get("sec-websocket-extensions"))
			return
		}
		body, wireLen, err := readBody(br, get("content-length"), get("transfer-encoding"))
		if err != nil {
			return
		}
		rec := record{Conn: connID, ConnReq: reqIdx, Kind: "http_request", Method: method, Target: target, Proto: proto, Headers: headers, BodyWireLen: wireLen}
		enc := strings.ToLower(strings.TrimSpace(get("content-encoding")))
		rec.BodyEncoding = enc
		if enc == "zstd" && len(body) > 0 {
			if dec, derr := zstdDecode(body); derr == nil {
				body = dec
			}
		}
		setBody(&rec, body)
		f.add(rec)

		path := target
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path = path[:i]
		}
		switch {
		case method == "POST" && strings.HasSuffix(path, "/oauth/token"):
			access := fakeJWT(map[string]any{"exp": 4102444800, "golden_refreshed": true, "https://api.openai.com/auth": goldenAuthClaims()})
			f.refreshedAccess.Store(access)
			tok, _ := json.Marshal(map[string]any{
				"id_token":      fakeJWT(map[string]any{"email": "golden@example.com", "exp": 4102444800, "https://api.openai.com/auth": goldenAuthClaims()}),
				"access_token":  access,
				"refresh_token": "rt-golden-2",
			})
			writeJSONResponse(c, 200, tok)
		case method == "POST" && strings.HasSuffix(path, "/responses") && f.scenario.UnauthorizedUntilRefresh && !f.authorizedAfterRefresh(get("authorization")):
			writeJSONResponse(c, 401, []byte(`{"error":{"message":"token expired","type":"invalid_request_error","code":"token_expired"}}`))
		case method == "POST" && strings.HasSuffix(path, "/responses"):
			f.writeSSE(c, f.scriptedEvents(body))
		case method == "GET" && strings.HasSuffix(path, "/wham/accounts/check"):
			writeJSONResponse(c, 200, f.accountsCheck())
		case method == "GET" && strings.HasSuffix(path, "/codex/models") && len(f.opts.ModelsJSON) > 0:
			writeJSONResponse(c, 200, f.opts.ModelsJSON)
		default:
			writeJSONResponse(c, 404, []byte("{}"))
		}
	}
}

func (f *fakeUpstream) authorizedAfterRefresh(authorization string) bool {
	access, _ := f.refreshedAccess.Load().(string)
	return access != "" && authorization == "Bearer "+access
}

func writeJSONResponse(c net.Conn, status int, body []byte) {
	text := map[int]string{200: "OK", 401: "Unauthorized", 404: "Not Found"}[status]
	_, _ = fmt.Fprintf(c, "HTTP/1.1 %d %s\r\ncontent-type: application/json\r\ncontent-length: %d\r\n\r\n", status, text, len(body))
	_, _ = c.Write(body)
}

// accountsCheck answers workspace discovery for the synthetic account so the
// Codex turn can start (see app-server workspace_routing.rs).
func (f *fakeUpstream) accountsCheck() []byte {
	origin := f.opts.WorkspaceOrigin
	if origin == "" {
		origin = "NO_CONSTRAINT"
	}
	body, _ := json.Marshal(map[string]any{
		"accounts": []any{map[string]any{
			"id":                       "acct-golden",
			"plan_type":                "plus",
			"workspace_backend_origin": origin,
			"account_routing_override": "NO_CONSTRAINT",
			"name":                     nil,
			"structure":                "personal",
		}},
		"account_ordering":   []string{"acct-golden"},
		"default_account_id": "acct-golden",
	})
	return body
}

func setBody(rec *record, body []byte) {
	if len(body) == 0 {
		return
	}
	if json.Valid(body) {
		rec.BodyJSON = append(json.RawMessage(nil), body...)
		return
	}
	rec.BodyText = string(body)
}

func readRequestHead(br *bufio.Reader) (method, target, proto string, headers []headerKV, err error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", "", "", nil, err
	}
	parts := strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 3)
	if len(parts) != 3 {
		return "", "", "", nil, fmt.Errorf("bad request line %q", line)
	}
	method, target, proto = parts[0], parts[1], parts[2]
	for {
		l, rerr := br.ReadString('\n')
		if rerr != nil {
			return "", "", "", nil, rerr
		}
		l = strings.TrimRight(l, "\r\n")
		if l == "" {
			break
		}
		name, value, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		headers = append(headers, headerKV{Name: name, Value: strings.TrimSpace(value)})
	}
	return method, target, proto, headers, nil
}

func readBody(br *bufio.Reader, contentLength, transferEncoding string) ([]byte, int, error) {
	if strings.Contains(strings.ToLower(transferEncoding), "chunked") {
		var out bytes.Buffer
		wire := 0
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return nil, 0, err
			}
			wire += len(line)
			sizeStr, _, _ := strings.Cut(strings.TrimRight(line, "\r\n"), ";")
			size, err := strconv.ParseInt(strings.TrimSpace(sizeStr), 16, 64)
			if err != nil {
				return nil, 0, err
			}
			if size == 0 {
				// trailers until blank line
				for {
					t, terr := br.ReadString('\n')
					if terr != nil {
						return nil, 0, terr
					}
					wire += len(t)
					if strings.TrimRight(t, "\r\n") == "" {
						break
					}
				}
				return out.Bytes(), wire, nil
			}
			if _, err := io.CopyN(&out, br, size); err != nil {
				return nil, 0, err
			}
			crlf := make([]byte, 2)
			if _, err := io.ReadFull(br, crlf); err != nil {
				return nil, 0, err
			}
			wire += int(size) + 2
		}
	}
	n, _ := strconv.Atoi(strings.TrimSpace(contentLength))
	if n <= 0 {
		return nil, 0, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(br, buf); err != nil {
		return nil, 0, err
	}
	return buf, n, nil
}

func zstdDecode(b []byte) ([]byte, error) {
	d, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.DecodeAll(b, nil)
}

// scriptedEvents returns the Responses events for one request body.
func (f *fakeUpstream) scriptedEvents(body []byte) []map[string]any {
	var req struct {
		Generate *bool            `json:"generate"`
		Input    []map[string]any `json:"input"`
	}
	_ = json.Unmarshal(body, &req)
	id := fmt.Sprintf("resp_golden_%03d", f.respSeq.Add(1))
	created := map[string]any{"type": "response.created", "response": map[string]any{"id": id}}
	usage := map[string]any{
		"input_tokens": 10, "input_tokens_details": map[string]any{"cached_tokens": 0},
		"output_tokens": 2, "output_tokens_details": map[string]any{"reasoning_tokens": 0},
		"total_tokens": 12,
	}
	completed := map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "usage": usage}}
	if req.Generate != nil && !*req.Generate {
		return []map[string]any{created, completed}
	}
	hasToolOutput := false
	for _, item := range req.Input {
		if t, _ := item["type"].(string); t == "function_call_output" {
			hasToolOutput = true
		}
	}
	if f.scenario.ToolLoop && !hasToolOutput {
		call := map[string]any{
			"type": "function_call", "id": "fc_golden_" + id, "call_id": "call_golden_" + id,
			"name": "exec_command", "arguments": `{"cmd":"echo golden"}`, "status": "completed",
		}
		return []map[string]any{created,
			{"type": "response.output_item.added", "output_index": 0, "item": call},
			{"type": "response.output_item.done", "output_index": 0, "item": call},
			completed}
	}
	msg := map[string]any{
		"type": "message", "id": "msg_golden_" + id, "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": "ok", "annotations": []any{}}},
	}
	return []map[string]any{created,
		{"type": "response.output_item.added", "output_index": 0, "item": msg},
		{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": "ok"},
		{"type": "response.output_item.done", "output_index": 0, "item": msg},
		completed}
}

func (f *fakeUpstream) writeSSE(c net.Conn, events []map[string]any) {
	var head strings.Builder
	head.WriteString("HTTP/1.1 200 OK\r\ncontent-type: text/event-stream\r\ntransfer-encoding: chunked\r\nx-codex-turn-state: golden-turn-state\r\n\r\n")
	_, _ = io.WriteString(c, head.String())
	for _, ev := range events {
		data, _ := json.Marshal(ev)
		chunk := fmt.Sprintf("event: %s\ndata: %s\n\n", ev["type"], data)
		_, _ = fmt.Fprintf(c, "%x\r\n%s\r\n", len(chunk), chunk)
	}
	_, _ = io.WriteString(c, "0\r\n\r\n")
}

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func (f *fakeUpstream) serveWebSocket(c net.Conn, br *bufio.Reader, connID int, key, offeredExt string) {
	sum := sha1.Sum([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	deflate := strings.Contains(strings.ToLower(offeredExt), "permessage-deflate")
	resp := "HTTP/1.1 101 Switching Protocols\r\nupgrade: websocket\r\nconnection: Upgrade\r\nsec-websocket-accept: " + accept + "\r\nx-codex-turn-state: golden-turn-state\r\n"
	if deflate {
		resp += "sec-websocket-extensions: permessage-deflate\r\n"
	}
	resp += "\r\n"
	if _, err := io.WriteString(c, resp); err != nil {
		return
	}
	inflate := &inflater{}
	for {
		_ = c.SetReadDeadline(time.Now().Add(120 * time.Second))
		msg, info, err := readClientMessage(br)
		if err != nil {
			return
		}
		switch info.Opcode {
		case 0x9: // ping
			_ = writeServerFrame(c, 0xA, msg)
			continue
		case 0x8:
			_ = writeServerFrame(c, 0x8, nil)
			return
		case 0xA:
			continue
		}
		if info.Compressed {
			out, derr := inflate.inflate(msg)
			if derr != nil {
				return
			}
			msg = out
		}
		rec := record{Conn: connID, Kind: "ws_message", Frame: info}
		setBody(&rec, msg)
		f.add(rec)
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(msg, &probe) == nil && probe.Type == "response.create" {
			for _, ev := range f.scriptedEvents(msg) {
				data, _ := json.Marshal(ev)
				if err := writeServerFrame(c, 0x1, data); err != nil {
					return
				}
			}
		}
	}
}

func readClientMessage(br *bufio.Reader) ([]byte, *frameInfo, error) {
	var payload []byte
	info := &frameInfo{}
	for {
		var hdr [2]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return nil, nil, err
		}
		fin := hdr[0]&0x80 != 0
		opcode := int(hdr[0] & 0x0f)
		masked := hdr[1]&0x80 != 0
		n := uint64(hdr[1] & 0x7f)
		wire := 2
		switch n {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(br, ext[:]); err != nil {
				return nil, nil, err
			}
			n = uint64(binary.BigEndian.Uint16(ext[:]))
			wire += 2
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(br, ext[:]); err != nil {
				return nil, nil, err
			}
			n = binary.BigEndian.Uint64(ext[:])
			wire += 8
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(br, mask[:]); err != nil {
				return nil, nil, err
			}
			wire += 4
		}
		if n > 64<<20 {
			return nil, nil, fmt.Errorf("frame too large: %d", n)
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(br, data); err != nil {
			return nil, nil, err
		}
		wire += int(n)
		if masked {
			for i := range data {
				data[i] ^= mask[i%4]
			}
		}
		if opcode >= 0x8 { // control frames are never fragmented
			return data, &frameInfo{Opcode: opcode, Fragments: 1, Masked: masked, WireLen: wire}, nil
		}
		if info.Fragments == 0 {
			info.Opcode = opcode
			info.Masked = masked
			info.Compressed = hdr[0]&0x40 != 0
		}
		info.Fragments++
		info.WireLen += wire
		payload = append(payload, data...)
		if fin {
			return payload, info, nil
		}
	}
}

func writeServerFrame(c net.Conn, opcode byte, payload []byte) error {
	var hdr []byte
	hdr = append(hdr, 0x80|opcode)
	switch n := len(payload); {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xffff:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	_, err := c.Write(append(hdr, payload...))
	return err
}

// inflater decodes permessage-deflate messages with client context takeover.
type inflater struct{ window []byte }

func (in *inflater) inflate(payload []byte) ([]byte, error) {
	src := append(append([]byte(nil), payload...), 0x00, 0x00, 0xff, 0xff, 0x01, 0x00, 0x00, 0xff, 0xff)
	r := flate.NewReaderDict(bytes.NewReader(src), in.window)
	out, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		return nil, err
	}
	in.window = append(in.window, out...)
	if len(in.window) > 32<<10 {
		in.window = append([]byte(nil), in.window[len(in.window)-32<<10:]...)
	}
	return out, nil
}
