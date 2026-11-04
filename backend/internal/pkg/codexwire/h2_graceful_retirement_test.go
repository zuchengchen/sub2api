package codexwire

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
)

func captureRetirementFrames(t *testing.T, peer net.Conn) <-chan []http2.Frame {
	t.Helper()
	frames := make(chan []http2.Frame, 1)
	go func() {
		defer peer.Close()
		_ = peer.SetReadDeadline(time.Now().Add(3 * time.Second))
		fr := http2.NewFramer(io.Discard, peer)
		var rows []http2.Frame
		for {
			row, err := fr.ReadFrame()
			if err != nil {
				break
			}
			rows = append(rows, row)
		}
		frames <- rows
	}()
	return frames
}
func assertNormalClientGOAWAY(t *testing.T, rows []http2.Frame) {
	t.Helper()
	require.Len(t, rows, 1)
	f, ok := rows[0].(*http2.GoAwayFrame)
	require.True(t, ok)
	require.EqualValues(t, 0, f.Flags)
	require.EqualValues(t, 0, f.StreamID)
	require.EqualValues(t, 0, f.LastStreamID)
	require.Equal(t, http2.ErrCodeNo, f.ErrCode)
	require.EqualValues(t, 8, f.Length)
	require.Empty(t, f.DebugData())
}
func TestH2NormalRetirementEmitsOneGOAWAY(t *testing.T) {
	for _, kind := range []string{"retire_concurrent", "idle"} {
		t.Run(kind, func(t *testing.T) {
			client, peer := net.Pipe()
			c := newH2ConnForTest(client)
			defer c.Close()
			rows := captureRetirementFrames(t, peer)
			if kind == "idle" {
				c.idleTimeout = time.Millisecond
				c.lastUsed = time.Now().Add(-time.Second)
				c.expireIdle()
			} else {
				var wg sync.WaitGroup
				for i := 0; i < 24; i++ {
					wg.Add(1)
					go func() { defer wg.Done(); c.retire() }()
				}
				wg.Wait()
			}
			assertNormalClientGOAWAY(t, <-rows)
		})
	}
}
func TestH2RetirementWaitsForActiveStreams(t *testing.T) {
	client, peer := net.Pipe()
	c := newH2ConnForTest(client)
	defer c.Close()
	rows := captureRetirementFrames(t, peer)
	first, err := c.openStream()
	require.NoError(t, err)
	second, err := c.openStream()
	require.NoError(t, err)
	c.retire()
	require.False(t, c.CanTake())
	select {
	case <-c.done:
		t.Fatal("retirement aborted active streams")
	default:
	}
	c.mu.Lock()
	c.failStreamLocked(first, io.EOF)
	c.mu.Unlock()
	require.NoError(t, (&h2StreamBody{conn: c, st: first}).Close())
	select {
	case <-c.done:
		t.Fatal("retirement aborted remaining stream")
	default:
	}
	c.mu.Lock()
	c.failStreamLocked(second, io.EOF)
	c.mu.Unlock()
	require.NoError(t, (&h2StreamBody{conn: c, st: second}).Close())
	assertNormalClientGOAWAY(t, <-rows)
}
func TestH2RetirementBlockedPeerAndWriteLockAreBounded(t *testing.T) {
	for _, hold := range []bool{false, true} {
		t.Run(map[bool]string{false: "peer_not_reading", true: "writer_locked"}[hold], func(t *testing.T) {
			client, peer := net.Pipe()
			defer peer.Close()
			c := newH2ConnForTest(client)
			defer c.Close()
			if hold {
				c.writeMu.Lock()
				defer c.writeMu.Unlock()
			}
			done := make(chan struct{})
			go func() { c.retire(); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("retirement blocked indefinitely")
			}
			select {
			case <-c.done:
			default:
				t.Fatal("retirement did not close")
			}
		})
	}
}

type retirementFailingConn struct {
	net.Conn
	writes atomic.Int64
}

func (c *retirementFailingConn) Write(p []byte) (int, error) {
	c.writes.Add(1)
	n, err := c.Conn.Write(p[:3])
	if err != nil {
		return n, err
	}
	return n, io.ErrUnexpectedEOF
}
func TestH2RetirementNeverWritesGOAWAYAfterPartialFailure(t *testing.T) {
	client, peer := net.Pipe()
	bad := &retirementFailingConn{Conn: client}
	c := newH2ConnForTest(bad)
	defer c.Close()
	raw := make(chan []byte, 1)
	go func() { defer peer.Close(); b, _ := io.ReadAll(peer); raw <- b }()
	err := c.writeFrameContext(context.Background(), func(fr *http2.Framer) error { return fr.WriteSettings() })
	require.Error(t, err)
	c.retire()
	require.Len(t, <-raw, 3)
	require.EqualValues(t, 1, bad.writes.Load())
}
func TestH2AbortCloseHasNoGracefulFrame(t *testing.T) {
	client, peer := net.Pipe()
	c := newH2ConnForTest(client)
	rows := captureRetirementFrames(t, peer)
	c.Close()
	require.Empty(t, <-rows)
}
