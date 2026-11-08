package codexwire

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type h2SettlementContext struct {
	context.Context
	armed   atomic.Bool
	once    sync.Once
	waiting chan struct{}
}

func (c *h2SettlementContext) Done() <-chan struct{} {
	if c.armed.Load() {
		c.once.Do(func() { close(c.waiting) })
	}
	return c.Context.Done()
}

type h2SettlementConn struct {
	net.Conn
	mode                             string
	release                          chan struct{}
	written                          chan struct{}
	closed                           chan struct{}
	holdOnce, releaseOnce, closeOnce sync.Once
}

func (c *h2SettlementConn) Write(raw []byte) (int, error) {
	n, err := c.Conn.Write(raw)
	if len(raw) < 9 || raw[3] != byte(http2.FrameData) || err != nil {
		return n, err
	}
	held := false
	c.holdOnce.Do(func() { held = true; close(c.written) })
	if !held {
		return n, err
	}
	select {
	case <-c.release:
	case <-c.closed:
		return n, net.ErrClosed
	}
	switch c.mode {
	case "error":
		return n, io.ErrUnexpectedEOF
	case "short":
		return n - 1, io.ErrShortWrite
	}
	return n, err
}
func (c *h2SettlementConn) releaseWrite() { c.releaseOnce.Do(func() { close(c.release) }) }
func (c *h2SettlementConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

type h2SettlementPeer struct {
	requestEnd bool
	goaways    []*http2.GoAwayFrame
}

// All state comes from real framed writes/reads. The socket wrapper delays only
// the return of an already transmitted DATA frame; it never changes a success
// into a fabricated pending=false state or signals completion before the write.
func h2SettlementFixture(t *testing.T, mode string) (*h2Conn, *h2StreamBody, *h2SettlementConn, *h2SettlementContext, context.CancelFunc, <-chan error, <-chan h2SettlementPeer) {
	t.Helper()
	client, peer := net.Pipe()
	wire := &h2SettlementConn{Conn: client, mode: mode, release: make(chan struct{}), written: make(chan struct{}), closed: make(chan struct{})}
	conn := newH2ConnForTest(wire)
	base, cancel := context.WithCancel(context.Background())
	ctx := &h2SettlementContext{Context: base, waiting: make(chan struct{})}
	stream, err := conn.openStream()
	require.NoError(t, err)
	stream.ctx, stream.sendPending, conn.pendingBodies = ctx, true, 1
	readerDone := make(chan struct{})
	go func() { defer close(readerDone); conn.readLoop() }()
	ready, rows := make(chan struct{}), make(chan h2SettlementPeer, 1)
	go func() {
		result := h2SettlementPeer{}
		defer func() { _ = peer.Close(); rows <- result; close(rows) }()
		_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
		fr := http2.NewFramer(peer, peer)
		fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
		if fr.WriteSettings() != nil {
			return
		}
		responded := false
		respond := func(id uint32) bool {
			if responded {
				return true
			}
			responded = true
			return fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: []byte{0x88}, EndHeaders: true}) == nil && fr.WriteData(id, true, []byte("complete")) == nil
		}
		for {
			frame, err := fr.ReadFrame()
			if err != nil {
				return
			}
			switch f := frame.(type) {
			case *http2.SettingsFrame:
				if f.IsAck() {
					close(ready)
				}
			case *http2.MetaHeadersFrame:
				if mode == "prewrite" && !respond(f.StreamID) {
					return
				}
			case *http2.DataFrame:
				result.requestEnd = result.requestEnd || f.StreamEnded()
				if !respond(f.StreamID) {
					return
				}
			case *http2.GoAwayFrame:
				result.goaways = append(result.goaways, f)
			}
		}
	}()
	writerDone := make(chan error, 1)
	t.Cleanup(func() {
		wire.releaseWrite()
		cancel()
		conn.Close()
		_ = peer.Close()
		select {
		case <-writerDone:
		case <-time.After(time.Second):
			t.Error("request writer did not join")
		}
		select {
		case <-readerDone:
		case <-time.After(time.Second):
			t.Error("response reader did not join")
		}
	})
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("initial SETTINGS ACK did not complete")
	}
	require.NoError(t, conn.writeHeaders(stream, "POST", "https", "synthetic.test", "/responses", nil, false))
	if mode == "prewrite" {
		close(writerDone)
	} else {
		payload := bytes.Repeat([]byte("x"), 16)
		if mode == "nonfinal" {
			payload = bytes.Repeat([]byte("x"), 16385)
		}
		go func() { defer close(writerDone); writerDone <- conn.writeBody(stream, payload) }()
	}
	body := &h2StreamBody{conn: conn, st: stream}
	got, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "complete", string(got))
	conn.mu.Lock()
	require.True(t, stream.closed)
	require.ErrorIs(t, stream.err, io.EOF)
	require.True(t, stream.sendPending)
	require.Equal(t, 1, conn.pendingBodies)
	conn.mu.Unlock()
	return conn, body, wire, ctx, cancel, writerDone, rows
}

func TestH2ResponseCompleteSettlesInFlightWriteBeforeRetirement(t *testing.T) {
	for _, mode := range []string{"full", "short", "error", "nonfinal", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			conn, body, wire, ctx, cancel, writerDone, rows := h2SettlementFixture(t, mode)
			ctx.armed.Store(true)
			checked := make(chan bool, 1)
			go func() { checked <- body.ResponseComplete() }()
			select {
			case <-ctx.waiting:
			case result := <-checked:
				t.Fatalf("completion returned %v before joining the held DATA writer", result)
			case <-time.After(time.Second):
				t.Fatal("completion did not begin bounded settlement")
			}
			if mode == "cancel" {
				cancel()
			} else {
				wire.releaseWrite()
			}
			var complete bool
			select {
			case complete = <-checked:
			case <-time.After(time.Second):
				t.Fatal("completion did not leave settlement")
			}
			var writeErr error
			select {
			case writeErr = <-writerDone:
			case <-time.After(time.Second):
				t.Fatal("DATA writer did not complete")
			}
			if mode == "full" {
				require.NoError(t, writeErr)
				require.True(t, complete, "the response EOF preceded local publication, but the final DATA write succeeded")
			} else {
				require.Error(t, writeErr)
				require.False(t, complete)
			}
			require.NoError(t, body.Close())
			if complete {
				conn.retire()
			} else {
				conn.Close()
			}
			var observed h2SettlementPeer
			select {
			case observed = <-rows:
			case <-time.After(time.Second):
				t.Fatal("peer did not observe connection close")
			}
			require.Equal(t, mode != "nonfinal", observed.requestEnd)
			if mode == "full" {
				require.Len(t, observed.goaways, 1)
				require.Equal(t, http2.ErrCodeNo, observed.goaways[0].ErrCode)
				require.Zero(t, observed.goaways[0].LastStreamID)
				require.EqualValues(t, 8, observed.goaways[0].Length)
			} else {
				require.Empty(t, observed.goaways)
			}
		})
	}
}

func TestH2ResponseCompleteRejectsUnstartedRequest(t *testing.T) {
	conn, body, _, _, _, _, rows := h2SettlementFixture(t, "prewrite")
	require.False(t, body.ResponseComplete(), "an available writer lock cannot prove any request DATA was sent")
	require.NoError(t, body.Close())
	conn.Close()
	observed := <-rows
	require.False(t, observed.requestEnd)
	require.Empty(t, observed.goaways)
}

func TestH2ResponseCompleteSettlementIsBoundedAndCancelable(t *testing.T) {
	for _, fault := range []string{"deadline", "cancel_waiting", "not_remote_eof"} {
		t.Run(fault, func(t *testing.T) {
			client, peer := net.Pipe()
			defer peer.Close()
			conn := newH2ConnForTest(client)
			defer conn.Close()
			stream, err := conn.openStream()
			require.NoError(t, err)
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &h2SettlementContext{Context: base, waiting: make(chan struct{})}
			stream.ctx, stream.sendPending = ctx, true
			conn.pendingBodies = 1
			if fault != "not_remote_eof" {
				conn.mu.Lock()
				conn.failStreamLocked(stream, io.EOF)
				conn.mu.Unlock()
			}
			body := &h2StreamBody{conn: conn, st: stream}
			conn.writeMu.Lock()
			defer conn.writeMu.Unlock()
			ctx.armed.Store(true)
			start := time.Now()
			done := make(chan bool, 1)
			go func() { done <- body.ResponseComplete() }()
			if fault == "cancel_waiting" {
				select {
				case <-ctx.waiting:
				case <-time.After(time.Second):
					t.Fatal("wait did not start")
				}
				cancel()
			}
			select {
			case result := <-done:
				require.False(t, result)
			case <-time.After(time.Second):
				t.Fatal("settlement waited without a bound")
			}
			if fault == "deadline" {
				require.GreaterOrEqual(t, time.Since(start), 200*time.Millisecond)
			}
		})
	}
}

func TestH2ResponseCompleteFastPathAndFailures(t *testing.T) {
	for _, fault := range []string{"none", "read_error", "write_error", "no_graceful", "connection_closed", "canceled"} {
		t.Run(fault, func(t *testing.T) {
			client, peer := net.Pipe()
			defer peer.Close()
			conn := newH2ConnForTest(client)
			defer conn.Close()
			stream, err := conn.openStream()
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream.ctx = ctx
			conn.mu.Lock()
			conn.failStreamLocked(stream, io.EOF)
			conn.mu.Unlock()
			switch fault {
			case "read_error":
				conn.readErr = io.ErrUnexpectedEOF
			case "write_error":
				conn.writeErr = io.ErrShortWrite
			case "no_graceful":
				conn.noGraceful = true
			case "connection_closed":
				conn.Close()
			case "canceled":
				cancel()
			}
			body := &h2StreamBody{conn: conn, st: stream}
			want := fault == "none"
			require.Equal(t, want, body.ResponseComplete())
			allocations := testing.AllocsPerRun(100, func() {
				if body.ResponseComplete() != want {
					panic(errors.New("completion changed"))
				}
			})
			require.Zero(t, allocations, "the non-pending fast path must not allocate a timer or channel")
		})
	}
}
