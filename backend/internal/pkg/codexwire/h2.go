package codexwire

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// Codex-faithful HTTP/2 client replicating hyper + h2 0.4 wire behavior:
//   - client preface + SETTINGS (ENABLE_PUSH=0, INITIAL_WINDOW=2MiB,
//     MAX_FRAME_SIZE=16KiB, MAX_HEADER_LIST_SIZE=16KiB) in one write
//   - connection WINDOW_UPDATE +5177345 (65535 → 5242880)
//   - pseudo-header order :method :scheme :authority :path
//   - hpack encoding identical to the h2 crate (see hpack.go)
//   - DATA frames split at peer MAX_FRAME_SIZE, both-level flow control
//   - stream multiplexing honoring peer MAX_CONCURRENT_STREAMS

const (
	h2ClientPreface    = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
	h2InitStreamWindow = 2097152 // 2 MiB, h2 Stream::default_target_recv_window
	h2InitConnWindow   = 5242880 // 5 MiB total: 65535 + 5177345
	h2ConnWindowIncr   = 5177345
	h2DefaultPeerMax   = 65535
)

type h2SettingsUpdate struct {
	maxConcurrent uint32 // peer MAX_CONCURRENT_STREAMS (0 = unlimited)
	initWindow    int64  // peer INITIAL_WINDOW_SIZE for new streams
	maxFrame      int    // peer MAX_FRAME_SIZE for DATA splitting
	headerTable   int    // peer HEADER_TABLE_SIZE
}

type h2Stream struct {
	id      uint32
	headCh  chan *h2ResponseHead // buffered 1
	bodyCh  chan []byte          // buffered, carries DATA payloads
	done    chan struct{}        // closed on END_STREAM / RST / error / cancel
	err     error                // set before done closes on failure
	trail   []HeaderKV           // trailers (if any)
	gotHead bool                 // response HEADERS frame decoded
	recvWin int64                // bytes of recv window still available
	sendWin int64                // bytes of send window still available
	closed  bool                 // remote ended (END_STREAM seen)
}

type h2ResponseHead struct {
	status  int
	headers []HeaderKV
}

// h2Conn multiplexes request streams on one connection.
type h2Conn struct {
	nc     net.Conn
	framer *http2.Framer

	mu        sync.Mutex
	writeMu   sync.Mutex // serializes framer writes; never held across reads
	writeErr  error      // sticky write failure
	readErr   error      // sticky read-loop failure
	goaway    bool       // GOAWAY received — no new streams
	streams   map[uint32]*h2Stream
	nextID    uint32
	peer      h2SettingsUpdate
	connSendW int64      // connection-level send window
	connRecvW int64      // connection-level recv window
	cond      *sync.Cond // L == &mu; wakes body writers when windows open

	enc *hpackEncoder
	dec *hpackDecoderState

	done      chan struct{}
	closeOnce sync.Once

	onDead func(*h2Conn) // pool eviction hook
}

func (c *h2Conn) lock()   { c.mu.Lock() }
func (c *h2Conn) unlock() { c.mu.Unlock() }

// CanTake reports whether a new stream can be opened on this connection.
func (c *h2Conn) CanTake() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.goaway || c.readErr != nil || c.writeErr != nil {
		return false
	}
	if c.peer.maxConcurrent == 0 {
		return true
	}
	return uint32(len(c.streams)) < c.peer.maxConcurrent
}

func (c *h2Conn) Close() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.nc.Close()
		c.lock()
		for _, st := range c.streams {
			c.failStreamLocked(st, errors.New("connection closed"))
		}
		c.unlock()
		if c.onDead != nil {
			c.onDead(c)
		}
	})
}

func (c *h2Conn) failStreamLocked(st *h2Stream, err error) {
	if st.closed {
		return
	}
	st.closed = true
	st.err = err
	close(st.done)
	// Wake body writers blocked on flow control so they observe the failure.
	c.cond.Broadcast()
}

// openStream registers a new stream; caller holds no lock.
func (c *h2Conn) openStream() (*h2Stream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.goaway {
		return nil, errH2GoAway
	}
	if c.readErr != nil {
		return nil, c.readErr
	}
	if c.writeErr != nil {
		return nil, c.writeErr
	}
	if c.peer.maxConcurrent > 0 && uint32(len(c.streams)) >= c.peer.maxConcurrent {
		return nil, errH2StreamsFull
	}
	id := c.nextID
	c.nextID += 2
	st := &h2Stream{
		id:      id,
		headCh:  make(chan *h2ResponseHead, 1),
		bodyCh:  make(chan []byte, 64),
		done:    make(chan struct{}),
		recvWin: h2InitStreamWindow,
		sendWin: c.peer.initWindow,
	}
	c.streams[id] = st
	return st, nil
}

func (c *h2Conn) dropStream(st *h2Stream) {
	c.mu.Lock()
	delete(c.streams, st.id)
	c.mu.Unlock()
}

var (
	errH2GoAway      = errors.New("h2: goaway received")
	errH2StreamsFull = errors.New("h2: max concurrent streams reached")
	errH2StreamReset = errors.New("h2: stream reset by peer")
)

// writeFrame serializes frame writes through the framer. The write itself runs
// under writeMu only — c.mu must never be held across a socket write (a
// blocked write would starve the read loop waiting for the same mutex).
func (c *h2Conn) writeFrame(fn func(*http2.Framer) error) error {
	c.mu.Lock()
	if c.writeErr != nil {
		c.mu.Unlock()
		return c.writeErr
	}
	c.mu.Unlock()
	c.writeMu.Lock()
	err := fn(c.framer)
	c.writeMu.Unlock()
	if err != nil {
		c.mu.Lock()
		c.writeErr = err
		c.mu.Unlock()
		go c.Close()
		return err
	}
	return nil
}

// readLoop demultiplexes inbound frames into streams.
func (c *h2Conn) readLoop() {
	defer c.Close()
	c.dec = newHpackDecoder()
	for {
		fr, err := c.framer.ReadFrame()
		if err != nil {
			c.lock()
			c.readErr = err
			for _, st := range c.streams {
				c.failStreamLocked(st, err)
			}
			c.unlock()
			return
		}
		if err := c.handleFrame(fr); err != nil {
			c.lock()
			c.readErr = err
			for _, st := range c.streams {
				c.failStreamLocked(st, err)
			}
			c.unlock()
			return
		}
	}
}

func (c *h2Conn) handleFrame(fr http2.Frame) error {
	switch f := fr.(type) {
	case *http2.SettingsFrame:
		if f.IsAck() {
			return nil
		}
		var upd h2SettingsUpdate
		_ = f.ForeachSetting(func(s http2.Setting) error {
			switch s.ID {
			case http2.SettingMaxConcurrentStreams:
				upd.maxConcurrent = s.Val
			case http2.SettingInitialWindowSize:
				upd.initWindow = int64(s.Val)
			case http2.SettingMaxFrameSize:
				upd.maxFrame = int(s.Val)
			case http2.SettingHeaderTableSize:
				upd.headerTable = int(s.Val)
			}
			return nil
		})
		c.mu.Lock()
		// Delta-adjust every open stream's send window by the new initial.
		if upd.initWindow != 0 {
			delta := upd.initWindow - c.peer.initWindow
			c.peer.initWindow = upd.initWindow
			for _, st := range c.streams {
				st.sendWin += delta
			}
		}
		if upd.maxConcurrent != 0 {
			c.peer.maxConcurrent = upd.maxConcurrent
		}
		if upd.maxFrame != 0 {
			c.peer.maxFrame = upd.maxFrame
		}
		if upd.headerTable != 0 {
			c.enc.maxSize = upd.headerTable
		}
		c.mu.Unlock()
		c.cond.Broadcast()
		return c.writeFrame(func(fr *http2.Framer) error {
			return fr.WriteSettingsAck()
		})
	case *http2.PingFrame:
		if f.IsAck() {
			return nil
		}
		return c.writeFrame(func(fr *http2.Framer) error {
			return fr.WritePing(true, f.Data)
		})
	case *http2.WindowUpdateFrame:
		incr := int64(f.Increment)
		c.mu.Lock()
		if f.StreamID == 0 {
			c.connSendW += incr
		} else if st := c.streams[f.StreamID]; st != nil {
			st.sendWin += incr
		}
		c.mu.Unlock()
		c.cond.Broadcast()
		return nil
	case *http2.HeadersFrame:
		return c.handleHeaders(fr)
	case *http2.ContinuationFrame:
		return nil // buffered inside ReadFrame aggregation
	case *http2.DataFrame:
		return c.handleData(f)
	case *http2.RSTStreamFrame:
		c.mu.Lock()
		if st := c.streams[f.StreamID]; st != nil {
			c.failStreamLocked(st, fmt.Errorf("%w: code=%d", errH2StreamReset, f.ErrCode))
		}
		c.mu.Unlock()
		return nil
	case *http2.GoAwayFrame:
		c.mu.Lock()
		c.goaway = true
		for id, st := range c.streams {
			if id > f.LastStreamID {
				c.failStreamLocked(st, fmt.Errorf("%w: last_stream=%d", errH2GoAway, f.LastStreamID))
			}
		}
		c.mu.Unlock()
		return nil
	}
	return nil
}

// headersBuf accumulates HEADERS+CONTINUATION fragments per stream until
// END_HEADERS. x/net Framer already returns header fragments only from
// HeadersFrame; CONTINUATION frames arrive as separate frames, so buffer.
var _ = http2.ContinuationFrame{} // referenced for documentation

func (c *h2Conn) handleHeaders(fr http2.Frame) error {
	hf, ok := fr.(*http2.HeadersFrame)
	if !ok {
		return nil
	}
	id := hf.StreamID
	block := hf.HeaderBlockFragment()
	// Gather continuation fragments when headers span frames.
	for !hf.HeadersEnded() {
		next, err := c.framer.ReadFrame()
		if err != nil {
			return err
		}
		cf, ok := next.(*http2.ContinuationFrame)
		if !ok {
			return fmt.Errorf("h2: expected CONTINUATION, got %T", next)
		}
		block = append(block, cf.HeaderBlockFragment()...)
		if cf.HeadersEnded() {
			break
		}
	}
	fields, err := decodeHpackBlock(c.dec, block)
	if err != nil {
		return err
	}
	c.mu.Lock()
	st := c.streams[id]
	if st == nil {
		c.mu.Unlock()
		return nil
	}
	if hf.StreamEnded() {
		if st.gotHead {
			st.trail = fields
		} else {
			st.gotHead = true
			st.headCh <- fieldsToHead(fields)
		}
		c.failStreamLocked(st, io.EOF)
	} else if !st.gotHead {
		st.gotHead = true
		st.headCh <- fieldsToHead(fields)
	} else {
		st.trail = fields
	}
	c.mu.Unlock()
	return nil
}

func fieldsToHead(fields []HeaderKV) *h2ResponseHead {
	head := &h2ResponseHead{}
	for _, f := range fields {
		if f.Name == ":status" {
			head.status, _ = strconv.Atoi(f.Value)
			continue
		}
		head.headers = append(head.headers, f)
	}
	return head
}

func (c *h2Conn) handleData(f *http2.DataFrame) error {
	id := f.StreamID
	data := f.Data()
	c.mu.Lock()
	st := c.streams[id]
	if st != nil {
		n := int64(len(data))
		st.recvWin -= n
		c.connRecvW -= n
	}
	c.mu.Unlock()
	if st != nil && len(data) > 0 {
		// Copy — framer buffer is reused on the next ReadFrame.
		buf := make([]byte, len(data))
		copy(buf, data)
		select {
		case st.bodyCh <- buf:
		case <-st.done:
		}
	}
	if f.StreamEnded() && st != nil {
		c.mu.Lock()
		c.failStreamLocked(st, io.EOF)
		c.mu.Unlock()
	}
	// h2 releases capacity when the window drops below half target.
	if st != nil {
		var updStream, updConn int64
		c.mu.Lock()
		if st.recvWin < h2InitStreamWindow/2 {
			updStream = h2InitStreamWindow - st.recvWin
			st.recvWin = h2InitStreamWindow
		}
		if c.connRecvW < h2InitConnWindow/2 {
			updConn = h2InitConnWindow - c.connRecvW
			c.connRecvW = h2InitConnWindow
		}
		c.mu.Unlock()
		if updStream > 0 {
			_ = c.writeFrame(func(fr *http2.Framer) error {
				return fr.WriteWindowUpdate(id, uint32(updStream))
			})
		}
		if updConn > 0 {
			_ = c.writeFrame(func(fr *http2.Framer) error {
				return fr.WriteWindowUpdate(0, uint32(updConn))
			})
		}
	}
	return nil
}

// writeHeaders encodes and emits the HEADERS frame (with CONTINUATION split if
// the block exceeds peer MAX_FRAME_SIZE).
func (c *h2Conn) writeHeaders(st *h2Stream, method, scheme, authority, path string, fields []HeaderKV, endStream bool) error {
	block, err := c.enc.EncodeBlock(method, scheme, authority, path, fields)
	if err != nil {
		return err
	}
	maxFrame := c.peer.maxFrame
	if maxFrame <= 0 {
		maxFrame = 16384
	}
	return c.writeFrame(func(fr *http2.Framer) error {
		if len(block) <= maxFrame {
			return fr.WriteHeaders(http2.HeadersFrameParam{
				StreamID:      st.id,
				BlockFragment: block,
				EndStream:     endStream,
				EndHeaders:    true,
			})
		}
		if err := fr.WriteHeaders(http2.HeadersFrameParam{
			StreamID:      st.id,
			BlockFragment: block[:maxFrame],
			EndStream:     endStream,
			EndHeaders:    false,
		}); err != nil {
			return err
		}
		rest := block[maxFrame:]
		for len(rest) > maxFrame {
			if err := fr.WriteContinuation(st.id, false, rest[:maxFrame]); err != nil {
				return err
			}
			rest = rest[maxFrame:]
		}
		return fr.WriteContinuation(st.id, true, rest)
	})
}

// writeBody streams the request body through DATA frames under both-level flow
// control, splitting at peer MAX_FRAME_SIZE like h2.
func (c *h2Conn) writeBody(st *h2Stream, body []byte) error {
	off := 0
	for {
		c.mu.Lock()
		for (c.connSendW <= 0 || st.sendWin <= 0) && c.writeErr == nil && c.readErr == nil && !st.closed {
			c.cond.Wait() // L == &c.mu: released while waiting, re-acquired on wake
		}
		if err := c.writeErr; err != nil {
			c.mu.Unlock()
			return err
		}
		if err := c.readErr; err != nil {
			c.mu.Unlock()
			return err
		}
		if st.closed {
			err := st.err
			c.mu.Unlock()
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			return errors.New("h2: stream closed while sending body")
		}
		maxFrame := int64(c.peer.maxFrame)
		if maxFrame <= 0 {
			maxFrame = 16384
		}
		grant := int64(len(body) - off)
		if grant > maxFrame {
			grant = maxFrame
		}
		if grant > c.connSendW {
			grant = c.connSendW
		}
		if grant > st.sendWin {
			grant = st.sendWin
		}
		if grant <= 0 {
			c.mu.Unlock()
			continue
		}
		c.connSendW -= grant
		st.sendWin -= grant
		chunk := body[off : off+int(grant)]
		off += int(grant)
		last := off >= len(body)
		c.mu.Unlock()
		err := c.writeFrame(func(fr *http2.Framer) error {
			return fr.WriteData(st.id, last, chunk)
		})
		if err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}

// h2StreamBody adapts the DATA channel to io.ReadCloser; Close sends RST.
type h2StreamBody struct {
	conn *h2Conn
	st   *h2Stream
}

func (b *h2StreamBody) Read(p []byte) (int, error) {
	select {
	case data, ok := <-b.st.bodyCh:
		if !ok {
			return 0, io.EOF
		}
		return copy(p, data), nil
	case <-b.st.done:
		select {
		case data, ok := <-b.st.bodyCh:
			if !ok {
				break
			}
			return copy(p, data), nil
		default:
		}
		if err := b.st.err; err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		return 0, io.EOF
	}
}

func (b *h2StreamBody) Close() error {
	b.conn.mu.Lock()
	if !b.st.closed {
		b.st.closed = true
		b.st.err = errors.New("stream closed by client")
		close(b.st.done)
		b.conn.mu.Unlock()
		_ = b.conn.writeFrame(func(fr *http2.Framer) error {
			return fr.WriteRSTStream(b.st.id, http2.ErrCodeCancel)
		})
	} else {
		b.conn.mu.Unlock()
	}
	b.conn.dropStream(b.st)
	return nil
}

// RoundTrip sends one request on this connection and returns the response head
// with a streaming body.
func (c *h2Conn) RoundTrip(ctx context.Context, method, scheme, authority, path string, fields []HeaderKV, body []byte) (*http.Response, error) {
	st, err := c.openStream()
	if err != nil {
		return nil, err
	}
	if err := c.writeHeaders(st, method, scheme, authority, path, fields, len(body) == 0); err != nil {
		c.dropStream(st)
		return nil, err
	}
	if len(body) > 0 {
		// Body write waits on flow control; run it concurrently with head wait.
		bodyDone := make(chan error, 1)
		go func() { bodyDone <- c.writeBody(st, body) }()
		defer func() {
			select {
			case <-bodyDone:
			default:
			}
		}()
		go func() {
			select {
			case <-ctx.Done():
				c.mu.Lock()
				c.failStreamLocked(st, ctx.Err())
				c.mu.Unlock()
			case <-st.done:
			}
		}()
	}
	var head *h2ResponseHead
	select {
	case head = <-st.headCh:
	case <-st.done:
		// The response may finish before this goroutine receives its head.
		// Preserve queued headers and let the body expose any terminal error.
		select {
		case head = <-st.headCh:
		default:
			c.dropStream(st)
			if err := st.err; err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			return nil, errors.New("h2: stream closed before response headers")
		}
	case <-ctx.Done():
		c.dropStream(st)
		_ = c.writeFrame(func(fr *http2.Framer) error {
			return fr.WriteRSTStream(st.id, http2.ErrCodeCancel)
		})
		return nil, ctx.Err()
	}
	resp := &http.Response{
		StatusCode:    head.status,
		Status:        fmt.Sprintf("%d %s", head.status, http.StatusText(head.status)),
		Proto:         "HTTP/2.0",
		ProtoMajor:    2,
		Header:        make(http.Header),
		Body:          &h2StreamBody{conn: c, st: st},
		ContentLength: -1,
		Request:       &http.Request{Method: method},
	}
	for _, kv := range head.headers {
		if kv.Name == "content-length" {
			if n, err := strconv.ParseInt(kv.Value, 10, 64); err == nil {
				resp.ContentLength = n
			}
			continue
		}
		resp.Header.Add(kv.Name, kv.Value) // Add canonicalizes the key
	}
	return resp, nil
}

// --- connection establishment ---

// dialH2 performs TCP+TLS with the rustls-h2 profile, writes the client
// preface and startup frames, and starts the read loop.
func dialH2(ctx context.Context, addr, proxyURL string, opts H2Options) (*h2Conn, error) {
	dialCtx := ctx
	cancel := func() {}
	if opts.Timeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
	}
	defer cancel()

	conn, err := tlsConn(dialCtx, addr, proxyURL, opts.TLSProfile, opts.BaseDialer)
	if err != nil {
		return nil, err
	}
	// tlsConn already completed the TLS handshake; extract the negotiated ALPN.
	var negotiated string
	switch tc := conn.(type) {
	case interface{ ConnectionState() utls.ConnectionState }:
		negotiated = tc.ConnectionState().NegotiatedProtocol
	case *tls.Conn:
		negotiated = tc.ConnectionState().NegotiatedProtocol
	}
	if negotiated != "h2" {
		// Caller falls back to HTTP/1.1 on this same connection.
		if negotiated == "" {
			negotiated = "http/1.1"
		}
		return nil, &h1FallbackConn{conn: conn, alpn: negotiated}
	}
	c := &h2Conn{
		nc:        conn,
		framer:    http2.NewFramer(conn, conn),
		streams:   make(map[uint32]*h2Stream),
		nextID:    1,
		connSendW: h2DefaultPeerMax,
		done:      make(chan struct{}),
		enc:       newHpackEncoder(),
		peer: h2SettingsUpdate{
			initWindow: h2DefaultPeerMax,
			maxFrame:   16384,
		},
	}
	c.cond = sync.NewCond(&c.mu)

	// Single write: preface + SETTINGS + connection WINDOW_UPDATE — the same
	// coalesced startup hyper emits on connection open.
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
	c.connRecvW = h2InitConnWindow
	if deadline, ok := dialCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := conn.Write(startup); err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	go c.readLoop()
	return c, nil
}

// h1FallbackConn carries an established h1-ALPN connection back to the caller.
type h1FallbackConn struct {
	conn net.Conn
	alpn string
}

func (e *h1FallbackConn) Error() string {
	return "h2: server negotiated " + e.alpn + ", not h2"
}

type sliceWriter struct{ b *[]byte }

func (w *sliceWriter) Write(p []byte) (int, error) {
	*w.b = append(*w.b, p...)
	return len(p), nil
}
