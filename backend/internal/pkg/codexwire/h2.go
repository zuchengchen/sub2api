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
	maxConcurrent    uint32 // meaningful only when maxConcurrentSet
	maxConcurrentSet bool
	initWindow       int64 // peer INITIAL_WINDOW_SIZE for new streams
	maxFrame         int   // peer MAX_FRAME_SIZE for DATA splitting
	headerTable      int   // peer HEADER_TABLE_SIZE
}

type h2Stream struct {
	id          uint32
	ctx         context.Context      // request lifetime, immutable after opening
	headCh      chan *h2ResponseHead // buffered 1
	bodyCh      chan []byte          // buffered, carries DATA payloads
	done        chan struct{}        // closed on END_STREAM / RST / error / cancel
	err         error                // set before done closes on failure
	trail       []HeaderKV           // trailers (if any)
	gotHead     bool                 // response HEADERS frame decoded
	recvWin     int64                // bytes of recv window still available
	sendWin     int64                // bytes of send window still available
	closed      bool                 // remote ended (END_STREAM seen)
	sendPending bool                 // request body writer has not completed yet
	recvEnd     bool                 // DATA END_STREAM parsed; payload may still await delivery
}

type h2ResponseHead struct {
	status  int
	headers []HeaderKV
}

// h2Conn multiplexes request streams on one connection.
type h2Conn struct {
	nc     net.Conn
	framer *http2.Framer

	headersMu     contextMutex // stream IDs and HPACK blocks must be emitted in order
	mu            sync.Mutex
	writeMu       contextMutex // serializes framer writes; never held across reads
	writeErr      error        // sticky write failure
	readErr       error        // sticky read-loop failure
	goaway        bool         // GOAWAY received — no new streams
	draining      bool         // retired client; finish existing streams, then close
	noGraceful    bool         // failed/cancelled stream or uncertain request submission
	pendingBodies int
	streams       map[uint32]*h2Stream
	nextID        uint32
	peer          h2SettingsUpdate
	connSendW     int64      // connection-level send window
	connRecvW     int64      // connection-level recv window
	cond          *sync.Cond // L == &mu; wakes body writers when windows open

	enc *hpackEncoder
	dec *hpackDecoderState

	done         chan struct{}
	closeOnce    sync.Once
	gracefulOnce sync.Once

	idleTimer   *time.Timer
	idleTimeout time.Duration
	lastUsed    time.Time
	onDead      func(*h2Conn) // pool eviction hook
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
	if !c.peer.maxConcurrentSet {
		return true
	}
	return uint32(len(c.streams)) < c.peer.maxConcurrent
}

func (c *h2Conn) Close() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.nc.Close()
		c.lock()
		if c.idleTimer != nil {
			c.idleTimer.Stop()
		}
		c.goaway = true
		for _, st := range c.streams {
			c.failStreamLocked(st, errors.New("connection closed"))
		}
		onDead := c.onDead
		c.unlock()
		if onDead != nil {
			onDead(c)
		}
	})
}

func (c *h2Conn) failStreamLocked(st *h2Stream, err error) {
	if err != nil && !errors.Is(err, io.EOF) {
		c.noGraceful = true
	}
	if st.closed {
		return
	}
	st.closed = true
	st.err = err
	close(st.done)
	// Wake body writers blocked on flow control so they observe the failure.
	c.cond.Broadcast()
}

func (c *h2Conn) cancelStream(st *h2Stream, err error) {
	c.mu.Lock()
	if st.closed {
		c.mu.Unlock()
		return
	}
	c.failStreamLocked(st, err)
	c.mu.Unlock()
	// Releasing the request must not wait for a peer that stopped reading.
	// A reset that cannot be delivered retires the socket instead of leaving
	// either an upstream request or a reset goroutine alive indefinitely.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := c.writeFrameContext(ctx, func(fr *http2.Framer) error {
			return fr.WriteRSTStream(st.id, http2.ErrCodeCancel)
		}); err != nil {
			c.Close()
		}
	}()
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
	if c.peer.maxConcurrentSet && uint32(len(c.streams)) >= c.peer.maxConcurrent {
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
	c.lastUsed = time.Now()
	return st, nil
}

func (c *h2Conn) dropStream(st *h2Stream) {
	c.mu.Lock()
	if !st.closed || (st.err != nil && !errors.Is(st.err, io.EOF)) {
		c.noGraceful = true
	}
	delete(c.streams, st.id)
	c.lastUsed = time.Now()
	closeNow := c.draining && len(c.streams) == 0
	c.mu.Unlock()
	if closeNow {
		c.closeGracefully()
	}
}

func (c *h2Conn) retire() {
	c.mu.Lock()
	c.goaway, c.draining = true, true
	if c.idleTimer != nil {
		c.idleTimer.Stop()
	}
	closeNow := len(c.streams) == 0
	c.mu.Unlock()
	if closeNow {
		c.closeGracefully()
	}
}

// Close remains an abort operation. Only normal, fully drained retirement
// emits the observed client GOAWAY(NO_ERROR,last_stream_id=0), once and bounded.
func (c *h2Conn) closeGracefully() {
	c.gracefulOnce.Do(func() {
		c.mu.Lock()
		abort := c.noGraceful || c.readErr != nil || c.writeErr != nil
		c.mu.Unlock()
		if abort {
			c.Close()
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		_ = c.writeFrameContext(ctx, func(fr *http2.Framer) error {
			c.mu.Lock()
			unsafe := c.noGraceful || c.readErr != nil || c.writeErr != nil || len(c.streams) != 0 || c.pendingBodies != 0
			c.mu.Unlock()
			select {
			case <-c.done:
				unsafe = true
			default:
			}
			if unsafe {
				return errors.New("h2: retirement requires abort")
			}
			return fr.WriteGoAway(0, http2.ErrCodeNo, nil)
		})
		c.Close()
	})
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
	return c.writeFrameContext(context.Background(), fn)
}

func (c *h2Conn) writeFrameContext(ctx context.Context, fn func(*http2.Framer) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer c.writeMu.Unlock()
	c.mu.Lock()
	if c.writeErr != nil {
		err := c.writeErr
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()
	var stop func() bool
	if ctx.Done() != nil {
		// Cancellation during a socket write leaves an uncertain frame prefix.
		// Closing the connection is the only safe way to unblock that write;
		// resetting a deadline would allow later streams onto a corrupt frame.
		stop = context.AfterFunc(ctx, func() { _ = c.nc.Close() })
	}
	err := fn(c.framer)
	if stop != nil && !stop() && ctx.Err() != nil {
		err = ctx.Err()
	}
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
		var hasWindow, hasTable bool
		_ = f.ForeachSetting(func(s http2.Setting) error {
			switch s.ID {
			case http2.SettingMaxConcurrentStreams:
				upd.maxConcurrent = s.Val
				upd.maxConcurrentSet = true
			case http2.SettingInitialWindowSize:
				upd.initWindow = int64(s.Val)
				hasWindow = true
			case http2.SettingMaxFrameSize:
				upd.maxFrame = int(s.Val)
			case http2.SettingHeaderTableSize:
				upd.headerTable = int(s.Val)
				hasTable = true
			}
			return nil
		})
		c.mu.Lock()
		// Delta-adjust every open stream's send window by the new initial.
		if hasWindow {
			delta := upd.initWindow - c.peer.initWindow
			c.peer.initWindow = upd.initWindow
			for _, st := range c.streams {
				st.sendWin += delta
			}
		}
		if upd.maxConcurrentSet {
			c.peer.maxConcurrentSet = true
			c.peer.maxConcurrent = upd.maxConcurrent
		}
		if upd.maxFrame != 0 {
			c.peer.maxFrame = upd.maxFrame
		}
		if hasTable {
			c.enc.setMaxSize(upd.headerTable)
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
		c.goaway, c.draining = true, true
		for id, st := range c.streams {
			if id > f.LastStreamID {
				c.failStreamLocked(st, fmt.Errorf("%w: last_stream=%d", errH2GoAway, f.LastStreamID))
			}
		}
		closeNow := len(c.streams) == 0
		c.mu.Unlock()
		if closeNow {
			c.Close()
		}
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
	block := append([]byte(nil), hf.HeaderBlockFragment()...)
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
		if f.StreamEnded() {
			st.recvEnd = true
		}
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
	return c.writeFrameContext(st.ctx, func(fr *http2.Framer) error {
		c.mu.Lock()
		block, err := c.enc.EncodeBlock(method, scheme, authority, path, fields)
		maxFrame := c.peer.maxFrame
		c.mu.Unlock()
		if err != nil {
			return err
		}
		if maxFrame <= 0 {
			maxFrame = 16384
		}
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
		writeStarted := false
		err := c.writeFrameContext(st.ctx, func(fr *http2.Framer) error {
			writeStarted = true
			err := fr.WriteData(st.id, last, chunk)
			if err == nil && last {
				c.mu.Lock()
				if st.sendPending {
					st.sendPending = false
					c.pendingBodies--
				}
				c.mu.Unlock()
			}
			return err
		})
		if err != nil {
			if !writeStarted {
				// Credit was reserved before waiting for the shared writer. A
				// canceled waiter emitted no DATA, so the peer cannot return that
				// credit with WINDOW_UPDATE. Restore both windows for other streams.
				// Once WriteData starts, an uncertain prefix instead retires the
				// connection in writeFrameContext; never refund or replay it.
				c.mu.Lock()
				c.connSendW += grant
				st.sendWin += grant
				c.cond.Broadcast()
				c.mu.Unlock()
			}
			return err
		}
		if last {
			return nil
		}
	}
}

// h2StreamBody adapts the DATA channel to io.ReadCloser; Close sends RST.
type h2StreamBody struct {
	conn       *h2Conn
	st         *h2Stream
	pending    []byte
	stopCancel func() bool
}

func (b *h2StreamBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(b.pending) > 0 {
		n := copy(p, b.pending)
		b.pending = b.pending[n:]
		return n, nil
	}

	select {
	case data, ok := <-b.st.bodyCh:
		if !ok {
			return 0, io.EOF
		}
		n := copy(p, data)
		b.pending = data[n:]
		return n, nil
	case <-b.st.done:
		select {
		case data, ok := <-b.st.bodyCh:
			if !ok {
				break
			}
			n := copy(p, data)
			b.pending = data[n:]
			return n, nil
		default:
		}
		if err := b.st.err; err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		return 0, io.EOF
	}
}

// ResponseComplete reports successful framing completion, including a fully
// submitted request body. Consumers may stop after Content-Length bytes
// without making an extra Read that returns EOF. Last DATA delivery can precede
// local END_STREAM publication, and a fast peer can finish its response before
// the request writer publishes completion. Only these explicit framing windows
// wait, sharing one 250ms budget. Normal retirement has its separate 250ms budget
// (about 500ms total). Decoded EOF or Content-Length never proves END_STREAM.
func (b *h2StreamBody) ResponseComplete() bool {
	b.conn.mu.Lock()
	healthy := b.responseHealthyLocked()
	ended := b.st.closed && errors.Is(b.st.err, io.EOF)
	receiving, pending := b.st.recvEnd && !b.st.closed, b.st.sendPending
	b.conn.mu.Unlock()
	if !healthy || !ended && !receiving {
		return false
	}
	if ended && !pending {
		return true
	}
	ctx := b.st.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	bounded, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	if receiving {
		// Keep DATA enqueue before done publication: closing done first could
		// lose the final payload, while holding mu across a full queue deadlocks
		// cancellation. A parsed END_STREAM flag permits joining that local step.
		select {
		case <-b.st.done:
		case <-bounded.Done():
			return false
		}
		b.conn.mu.Lock()
		ended = b.responseHealthyLocked() && b.st.closed && errors.Is(b.st.err, io.EOF)
		pending = b.st.sendPending
		b.conn.mu.Unlock()
		if bounded.Err() != nil || !ended {
			return false
		}
		if !pending {
			return true
		}
	}
	// Writers take writeMu before mu. Never hold mu while joining a writer:
	// the successful final DATA callback needs mu to clear sendPending.
	if err := b.conn.writeMu.LockContext(bounded); err != nil {
		return false
	}
	defer b.conn.writeMu.Unlock()
	b.conn.mu.Lock()
	defer b.conn.mu.Unlock()
	return bounded.Err() == nil && b.responseHealthyLocked() && b.st.closed && errors.Is(b.st.err, io.EOF) && !b.st.sendPending
}

func (b *h2StreamBody) responseHealthyLocked() bool {
	if b.st.ctx != nil && b.st.ctx.Err() != nil {
		return false
	}
	select {
	case <-b.conn.done:
		return false
	default:
	}
	return (b.st.err == nil || errors.Is(b.st.err, io.EOF)) &&
		!b.conn.noGraceful && b.conn.readErr == nil && b.conn.writeErr == nil
}

func (b *h2StreamBody) Close() error {
	if b.stopCancel != nil {
		b.stopCancel()
	}
	b.conn.cancelStream(b.st, errors.New("stream closed by client"))
	b.conn.dropStream(b.st)
	return nil
}

// RoundTrip sends one request on this connection and returns the response head
// with a streaming body.
func (c *h2Conn) RoundTrip(ctx context.Context, method, scheme, authority, path string, fields []HeaderKV, body []byte) (*http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.headersMu.LockContext(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		c.headersMu.Unlock()
		return nil, err
	}
	st, err := c.openStream()
	if err != nil {
		c.headersMu.Unlock()
		return nil, err
	}
	st.ctx = ctx
	if err := c.writeHeaders(st, method, scheme, authority, path, fields, len(body) == 0); err != nil {
		c.dropStream(st)
		c.headersMu.Unlock()
		return nil, err
	}
	c.headersMu.Unlock()
	stopCancel := context.AfterFunc(ctx, func() {
		c.cancelStream(st, ctx.Err())
	})
	handedOff := false
	defer func() {
		if !handedOff {
			stopCancel()
		}
	}()
	if len(body) > 0 {
		c.mu.Lock()
		st.sendPending = true
		c.pendingBodies++
		c.mu.Unlock()
		// Body write waits on flow control; run it concurrently with head wait.
		go func() {
			if err := c.writeBody(st, body); err != nil {
				c.mu.Lock()
				c.noGraceful = true
				if st.sendPending {
					st.sendPending = false
					c.pendingBodies--
				}
				c.failStreamLocked(st, err)
				c.mu.Unlock()
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
		c.cancelStream(st, ctx.Err())
		c.dropStream(st)
		return nil, ctx.Err()
	}
	resp := &http.Response{
		StatusCode:    head.status,
		Status:        fmt.Sprintf("%d %s", head.status, http.StatusText(head.status)),
		Proto:         "HTTP/2.0",
		ProtoMajor:    2,
		Header:        make(http.Header),
		Body:          &h2StreamBody{conn: c, st: st, stopCancel: stopCancel},
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
	handedOff = true
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
	c.idleTimeout = opts.IdleTimeout
	if c.idleTimeout <= 0 {
		c.idleTimeout = 90 * time.Second
	}
	c.lastUsed = time.Now()

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
	c.idleTimer = time.AfterFunc(c.idleTimeout, c.expireIdle)
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

func (c *h2Conn) expireIdle() {
	c.mu.Lock()
	if c.goaway {
		c.mu.Unlock()
		return
	}
	if len(c.streams) == 0 && time.Since(c.lastUsed) >= c.idleTimeout {
		c.goaway = true
		c.draining = true
		c.mu.Unlock()
		c.closeGracefully()
		return
	}
	c.idleTimer.Reset(c.idleTimeout)
	c.mu.Unlock()
}
