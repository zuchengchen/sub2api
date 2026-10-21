package codexwire

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// h2TestPair returns a client-side h2Conn plus the accepted server conn over
// TCP loopback (kernel buffers avoid the write/write deadlock net.Pipe has).
func h2TestPair(t *testing.T) (*h2Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	acceptCh := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		acceptCh <- res{c, err}
	}()
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	srv := <-acceptCh
	if srv.err != nil {
		t.Fatal(srv.err)
	}
	t.Cleanup(func() {
		cli.Close()
		srv.c.Close()
	})
	return newH2ConnForTest(cli), srv.c
}

// readClientStartup reads preface + SETTINGS + WINDOW_UPDATE, asserting h2's
// exact startup bytes/order. Returns the server-side framer.
func readClientStartup(t *testing.T, srv net.Conn) *http2.Framer {
	t.Helper()
	_ = srv.SetReadDeadline(time.Now().Add(5 * time.Second))
	preface := make([]byte, len(h2ClientPreface))
	if _, err := io.ReadFull(srv, preface); err != nil {
		t.Fatalf("read preface: %v", err)
	}
	if string(preface) != h2ClientPreface {
		t.Fatalf("preface = %q, want %q", preface, h2ClientPreface)
	}
	fr := http2.NewFramer(srv, srv)
	got, order := readSettings(t, fr)
	want := map[http2.SettingID]uint32{
		http2.SettingEnablePush:        0,
		http2.SettingInitialWindowSize: h2InitStreamWindow,
		http2.SettingMaxFrameSize:      16384,
		http2.SettingMaxHeaderListSize: 16384,
	}
	for id, v := range want {
		if got[id] != v {
			t.Fatalf("SETTINGS[%v] = %d, want %d", id, got[id], v)
		}
	}
	wantOrder := []http2.SettingID{
		http2.SettingEnablePush, http2.SettingInitialWindowSize,
		http2.SettingMaxFrameSize, http2.SettingMaxHeaderListSize,
	}
	if len(order) != len(wantOrder) {
		t.Fatalf("settings count = %d, want %d", len(order), len(wantOrder))
	}
	for i := range wantOrder {
		if order[i] != wantOrder[i] {
			t.Fatalf("settings order = %v, want %v", order, wantOrder)
		}
	}
	wf, err := fr.ReadFrame()
	if err != nil {
		t.Fatalf("read WINDOW_UPDATE: %v", err)
	}
	wu, ok := wf.(*http2.WindowUpdateFrame)
	if !ok {
		t.Fatalf("frame after SETTINGS = %T, want *http2.WindowUpdateFrame", wf)
	}
	if wu.StreamID != 0 || wu.Increment != h2ConnWindowIncr {
		t.Fatalf("window_update stream=%d incr=%d, want 0/%d", wu.StreamID, wu.Increment, h2ConnWindowIncr)
	}
	_ = srv.SetReadDeadline(time.Time{})
	return fr
}

// readSettings extracts settings + order while the frame is still owned.
func readSettings(t *testing.T, fr *http2.Framer) (map[http2.SettingID]uint32, []http2.SettingID) {
	t.Helper()
	f, err := fr.ReadFrame()
	if err != nil {
		t.Fatalf("read SETTINGS: %v", err)
	}
	sf, ok := f.(*http2.SettingsFrame)
	if !ok {
		t.Fatalf("frame = %T, want *http2.SettingsFrame", f)
	}
	got := map[http2.SettingID]uint32{}
	var order []http2.SettingID
	_ = sf.ForeachSetting(func(s http2.Setting) error {
		got[s.ID] = s.Val
		order = append(order, s.ID)
		return nil
	})
	return got, order
}

func TestH2PrefaceSettingsWindowUpdate(t *testing.T) {
	c, srv := h2TestPair(t)
	errCh := make(chan error, 1)
	go func() { errCh <- c.startHandshakeForTest() }()
	readClientStartup(t, srv)
	if err := <-errCh; err != nil {
		t.Fatalf("handshake: %v", err)
	}
}

// serverConsume is a background loop reading all client frames, replying to
// PING with nothing (client must ACK server pings, not vice versa) and
// collecting interesting frames.
type serverConsume struct {
	t       *testing.T
	fr      *http2.Framer
	wrMu    sync.Mutex // serializes response frame writes
	mu      sync.Mutex
	headers map[uint32][]hpack.HeaderField
	data    map[uint32]int // data frames per stream
	done    chan struct{}
}

func newServerConsume(t *testing.T, fr *http2.Framer) *serverConsume {
	s := &serverConsume{
		t: t, fr: fr,
		headers: make(map[uint32][]hpack.HeaderField),
		data:    make(map[uint32]int),
		done:    make(chan struct{}),
	}
	go s.loop()
	return s
}

func (s *serverConsume) loop() {
	dec := hpack.NewDecoder(4096, nil)
	for {
		f, err := s.fr.ReadFrame()
		if err != nil {
			close(s.done)
			return
		}
		switch fr := f.(type) {
		case *http2.HeadersFrame:
			var block []byte
			block = append(block, fr.HeaderBlockFragment()...)
			for !fr.HeadersEnded() {
				cf, err := s.fr.ReadFrame()
				if err != nil {
					close(s.done)
					return
				}
				cont, ok := cf.(*http2.ContinuationFrame)
				if !ok {
					continue
				}
				block = append(block, cont.HeaderBlockFragment()...)
				if cont.HeadersEnded() {
					break
				}
			}
			var fields []hpack.HeaderField
			dec.SetEmitFunc(func(f hpack.HeaderField) { fields = append(fields, f) })
			if _, err := dec.Write(block); err == nil {
				s.mu.Lock()
				s.headers[fr.StreamID] = fields
				s.mu.Unlock()
			}
		case *http2.DataFrame:
			s.mu.Lock()
			s.data[fr.StreamID]++
			s.mu.Unlock()
		}
	}
}

func (s *serverConsume) waitStream(t *testing.T, id uint32) []hpack.HeaderField {
	t.Helper()
	for i := 0; i < 100; i++ {
		s.mu.Lock()
		if h := s.headers[id]; h != nil {
			s.mu.Unlock()
			return h
		}
		s.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no headers for stream %d", id)
	return nil
}

func (s *serverConsume) respondOK(t *testing.T, streamID uint32, chunks [][]byte) {
	t.Helper()
	s.wrMu.Lock()
	defer s.wrMu.Unlock()
	var buf encBuffer
	e := hpack.NewEncoder(&buf)
	_ = e.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
	_ = e.WriteField(hpack.HeaderField{Name: "content-type", Value: "text/event-stream"})
	if err := s.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: streamID, BlockFragment: buf.Bytes(), EndHeaders: true}); err != nil {
		t.Fatalf("write response headers: %v", err)
	}
	for i, ch := range chunks {
		if err := s.fr.WriteData(streamID, i == len(chunks)-1, ch); err != nil {
			t.Fatalf("write data: %v", err)
		}
	}
	if len(chunks) == 0 {
		_ = s.fr.WriteData(streamID, true, nil)
	}
}

func TestH2RoundTripOverTCP(t *testing.T) {
	c, srv := h2TestPair(t)
	if err := c.startHandshakeForTest(); err != nil {
		t.Fatal(err)
	}
	fr := readClientStartup(t, srv)
	consume := newServerConsume(t, fr)
	_ = fr.WriteSettings() // server settings → client will ACK

	body := []byte(`{"model":"gpt-5.2-codex"}`)
	ordered := []HeaderKV{
		{Name: "content-type", Value: "application/json"},
		{Name: "authorization", Value: "Bearer sk-test"},
		{Name: "content-length", Value: "26"},
	}
	go func() {
		fields := consume.waitStream(t, 1)
		// Pseudo order + values verified.
		if fields[0].Name != ":method" || fields[0].Value != "POST" ||
			fields[1].Name != ":scheme" || fields[1].Value != "https" ||
			fields[2].Name != ":authority" || fields[2].Value != "chatgpt.com" ||
			fields[3].Name != ":path" || fields[3].Value != "/v1/responses" {
			t.Errorf("pseudo headers = %v", fields[:4])
		}
		consume.respondOK(t, 1, [][]byte{[]byte("data: {}\n\n")})
	}()

	resp, err := c.RoundTrip(context.Background(), "POST", "https", "chatgpt.com", "/v1/responses", ordered, body)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	if string(data) != "data: {}\n\n" {
		t.Fatalf("body = %q", data)
	}
}

func TestH2DataFrameSplitAndMultiplex(t *testing.T) {
	c, srv := h2TestPair(t)
	if err := c.startHandshakeForTest(); err != nil {
		t.Fatal(err)
	}
	fr := readClientStartup(t, srv)
	consume := newServerConsume(t, fr)
	_ = fr.WriteSettings(http2.Setting{ID: http2.SettingMaxFrameSize, Val: 16384})

	const bigBody = 40000 // > 2×16KiB → must split

	go func() {
		consume.waitStream(t, 1)
		// Wait for ≥3 data frames then respond.
		for i := 0; i < 200; i++ {
			consume.mu.Lock()
			n := consume.data[1]
			consume.mu.Unlock()
			if n >= 3 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		consume.mu.Lock()
		n := consume.data[1]
		consume.mu.Unlock()
		if n < 3 {
			t.Errorf("data frames = %d, want ≥3 for %d-byte body", n, bigBody)
		}
		consume.respondOK(t, 1, [][]byte{[]byte("ok")})
	}()

	resp, err := c.RoundTrip(context.Background(), "POST", "https", "chatgpt.com", "/responses", nil, make([]byte, bigBody))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	// Drain the body so the server goroutine finishes before cleanup closes
	// the socket (otherwise its final DATA races conn teardown).
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
}

// TestH2TwoStreamsMultiplexed verifies two concurrent requests share one conn.
func TestH2TwoStreamsMultiplexed(t *testing.T) {
	c, srv := h2TestPair(t)
	if err := c.startHandshakeForTest(); err != nil {
		t.Fatal(err)
	}
	fr := readClientStartup(t, srv)
	consume := newServerConsume(t, fr)
	_ = fr.WriteSettings()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		fields := consume.waitStream(t, 1)
		if fields[0].Value != "POST" {
			t.Errorf("stream1 method = %v", fields[0].Value)
		}
		consume.respondOK(t, 1, [][]byte{[]byte("s1")})
	}()
	go func() {
		defer wg.Done()
		consume.waitStream(t, 3)
		consume.respondOK(t, 3, [][]byte{[]byte("s3")})
	}()

	type result struct {
		body string
		err  error
	}
	ch := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			resp, err := c.RoundTrip(context.Background(), "POST", "https", "chatgpt.com", "/r", nil, nil)
			if err != nil {
				ch <- result{err: err}
				return
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			ch <- result{body: string(b)}
		}()
	}
	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		r := <-ch
		if r.err != nil {
			t.Fatalf("stream failed: %v", r.err)
		}
		got[r.body] = true
	}
	wg.Wait()
	if !got["s1"] || !got["s3"] {
		t.Fatalf("got %v, want both s1 and s3 bodies", got)
	}
}

func TestH2H1FallbackOnALPN(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	acceptCh := make(chan res, 1)
	go func() {
		conn, err := ln.Accept()
		acceptCh <- res{conn, err}
	}()
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	srv := <-acceptCh
	if srv.err != nil {
		t.Fatal(srv.err)
	}
	defer srv.c.Close()

	go func() {
		br := bufio.NewReader(srv.c)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		_, _ = io.ReadAll(req.Body)
		_, _ = srv.c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello"))
	}()

	resp, err := doHTTP1OnConn(context.Background(), "POST", "/responses", "chatgpt.com",
		[]HeaderKV{{Name: "authorization", Value: "Bearer x"}},
		[]byte("{}"),
		func(context.Context) (net.Conn, error) { return cli, nil }, 0)
	if err != nil {
		t.Fatalf("h1 fallback: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(data) != "hello" {
		t.Fatalf("body = %q", data)
	}
}

// --- test helpers ---

type encBuffer struct{ b []byte }

func (e *encBuffer) Write(p []byte) (int, error) { e.b = append(e.b, p...); return len(p), nil }
func (e *encBuffer) Bytes() []byte               { return e.b }

// newH2ConnForTest builds an h2Conn over a raw conn (no TLS) for frame tests.
func newH2ConnForTest(nc net.Conn) *h2Conn {
	c := &h2Conn{
		nc:        nc,
		framer:    http2.NewFramer(nc, nc),
		streams:   make(map[uint32]*h2Stream),
		nextID:    1,
		connSendW: h2DefaultPeerMax,
		connRecvW: h2InitConnWindow,
		done:      make(chan struct{}),
		enc:       newHpackEncoder(),
		peer: h2SettingsUpdate{
			initWindow: h2DefaultPeerMax,
			maxFrame:   16384,
		},
	}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// startHandshakeForTest performs only the startup-frame write + read loop
// (TLS already established by the transport in tests).
func (c *h2Conn) startHandshakeForTest() error {
	var startup []byte
	startup = append(startup, h2ClientPreface...)
	buf := &sliceWriter{b: &startup}
	fr := http2.NewFramer(buf, nil)
	_ = fr.WriteSettings(
		http2.Setting{ID: http2.SettingEnablePush, Val: 0},
		http2.Setting{ID: http2.SettingInitialWindowSize, Val: h2InitStreamWindow},
		http2.Setting{ID: http2.SettingMaxFrameSize, Val: 16384},
		http2.Setting{ID: http2.SettingMaxHeaderListSize, Val: 16384},
	)
	_ = fr.WriteWindowUpdate(0, h2ConnWindowIncr)
	if _, err := c.nc.Write(startup); err != nil {
		return err
	}
	go c.readLoop()
	return nil
}
