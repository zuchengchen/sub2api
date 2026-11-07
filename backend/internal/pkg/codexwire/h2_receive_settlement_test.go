package codexwire

import (
	"bytes"
	"context"
	"io"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
)

// A full queue holds the parsed last DATA frame before EOF publication. The
// existing receive-window accounting signals parser entry without referring to
// the new marker, so these regression tests also compile against the old code.
func h2QueueFinalDATA(t *testing.T, conn *h2Conn, stream *h2Stream, end bool) ([]byte, <-chan error) {
	t.Helper()
	var expected bytes.Buffer
	for i := 0; i < cap(stream.bodyCh); i++ {
		chunk := []byte{byte(i), 'x'}
		stream.bodyCh <- chunk
		expected.Write(chunk)
	}
	tail := []byte("final payload must not disappear")
	expected.Write(tail)
	var wire bytes.Buffer
	require.NoError(t, http2.NewFramer(&wire, nil).WriteData(stream.id, end, tail))
	frame, err := http2.NewFramer(nil, &wire).ReadFrame()
	require.NoError(t, err)
	before := stream.recvWin
	processed := make(chan error, 1)
	go func() { defer close(processed); processed <- conn.handleData(frame.(*http2.DataFrame)) }()
	t.Cleanup(func() {
		conn.Close()
		select {
		case <-processed:
		case <-time.After(time.Second):
			t.Error("DATA publisher did not join")
		}
	})
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		conn.mu.Lock()
		parsed, closed := stream.recvWin != before, stream.closed
		conn.mu.Unlock()
		if parsed {
			require.False(t, closed, "full queue must precede EOF publication")
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("final frame was not parsed")
		default:
			runtime.Gosched()
		}
	}
	return expected.Bytes(), processed
}

func TestH2ResponseCompleteWaitsForKnownEndPublication(t *testing.T) {
	for _, fault := range []string{"full", "writer_pending", "read_error", "cancel", "deadline", "no_marker"} {
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
			stream.ctx = ctx
			body := &h2StreamBody{conn: conn, st: stream}
			writerLocked := fault == "writer_pending"
			if writerLocked {
				stream.sendPending, conn.pendingBodies = true, 1
				conn.writeMu.Lock()
				defer func() {
					if writerLocked {
						conn.writeMu.Unlock()
					}
				}()
			}
			expected, processed := h2QueueFinalDATA(t, conn, stream, fault != "no_marker")
			if fault == "no_marker" {
				require.False(t, body.ResponseComplete(), "the same bytes without END_STREAM are incomplete")
				return
			}
			ctx.armed.Store(true)
			start := time.Now()
			checked := make(chan bool, 1)
			go func() { checked <- body.ResponseComplete() }()
			select {
			case <-ctx.waiting:
			case result := <-checked:
				t.Fatalf("completion returned %v before joining END_STREAM publication", result)
			case <-time.After(time.Second):
				t.Fatal("receive settlement did not start")
			}
			switch fault {
			case "cancel":
				cancel()
			case "read_error":
				conn.mu.Lock()
				conn.failStreamLocked(stream, io.ErrUnexpectedEOF)
				conn.mu.Unlock()
			case "deadline":
			default:
				// Draining permits the actual handler to enqueue the last payload
				// and only then publish EOF; no test writes completion flags.
				payload, err := io.ReadAll(body)
				require.NoError(t, err)
				require.Equal(t, expected, payload)
				if writerLocked {
					select {
					case result := <-checked:
						t.Fatalf("returned %v before request writer settled", result)
					default:
					}
					conn.mu.Lock()
					stream.sendPending, conn.pendingBodies = false, 0
					conn.mu.Unlock()
					conn.writeMu.Unlock()
					writerLocked = false
				}
			}
			select {
			case result := <-checked:
				require.Equal(t, fault == "full" || fault == "writer_pending", result)
			case <-time.After(time.Second):
				t.Fatal("receive settlement did not remain bounded")
			}
			if fault == "deadline" {
				require.GreaterOrEqual(t, time.Since(start), 200*time.Millisecond)
			}
			conn.Close()
			select {
			case err := <-processed:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("DATA delivery did not join")
			}
		})
	}
}

func TestH2FinalDATAFullQueuePreservesPayloadAndAllowsAbort(t *testing.T) {
	for _, action := range []string{"drain", "early_close", "cancel"} {
		t.Run(action, func(t *testing.T) {
			client, peer := net.Pipe()
			defer peer.Close()
			conn := newH2ConnForTest(client)
			defer conn.Close()
			stream, err := conn.openStream()
			require.NoError(t, err)
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &h2SettlementContext{Context: base, waiting: make(chan struct{})}
			stream.ctx = ctx
			expected, processed := h2QueueFinalDATA(t, conn, stream, true)
			body := &h2StreamBody{conn: conn, st: stream}
			if action == "drain" {
				got, err := io.ReadAll(body)
				require.NoError(t, err)
				require.Equal(t, expected, got)
				require.True(t, body.ResponseComplete())
			} else {
				ctx.armed.Store(true)
				checked := make(chan bool, 1)
				go func() { checked <- body.ResponseComplete() }()
				if action == "cancel" {
					select {
					case <-ctx.waiting:
					case <-time.After(time.Second):
						t.Fatal("receive wait did not start")
					}
					cancel()
				}
				select {
				case result := <-checked:
					require.False(t, result)
				case <-time.After(time.Second):
					t.Fatal("a full queue blocked Close indefinitely")
				}
				// The owner aborts before closing the reader, as in production.
				cancel()
				conn.Close()
			}
			require.NoError(t, body.Close())
			select {
			case err := <-processed:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("DATA delivery did not join after drain or cancellation")
			}
		})
	}
}
