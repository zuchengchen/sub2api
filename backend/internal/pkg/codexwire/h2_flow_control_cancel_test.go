package codexwire

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
)

func TestH2QueuedDATAWindowAccounting(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "sent"
		if canceled {
			name = "canceled_before_DATA"
		}
		t.Run(name, func(t *testing.T) {
			client, peer := net.Pipe()
			defer peer.Close()
			conn := newH2ConnForTest(client)
			defer conn.Close()
			var wire bytes.Buffer
			conn.framer = http2.NewFramer(&wire, nil)
			stream, err := conn.openStream()
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			observed := &signaledContext{Context: ctx, seen: make(chan struct{})}
			stream.ctx = observed
			beforeConn, beforeStream := conn.connSendW, stream.sendWin
			payload := bytes.Repeat([]byte("x"), 4096)
			conn.writeMu.Lock()
			locked := true
			defer func() {
				if locked {
					conn.writeMu.Unlock()
				}
			}()
			done := make(chan error, 1)
			go func() { done <- conn.writeBody(stream, payload) }()
			select {
			case <-observed.seen:
			case <-time.After(time.Second):
				t.Fatal("upload did not reach queued frame write")
			}
			if canceled {
				cancel()
			} else {
				conn.writeMu.Unlock()
				locked = false
			}
			select {
			case err = <-done:
			case <-time.After(time.Second):
				conn.writeMu.Unlock()
				locked = false
				cancel()
				<-done
				t.Fatal("upload did not leave the write queue")
			}
			wantConn, wantStream := beforeConn-int64(len(payload)), beforeStream-int64(len(payload))
			if canceled {
				require.ErrorIs(t, err, context.Canceled)
				require.Zero(t, wire.Len(), "queued cancellation must send no DATA")
				wantConn, wantStream = beforeConn, beforeStream
				conn.writeMu.Unlock()
				locked = false
			} else {
				require.NoError(t, err)
				require.Greater(t, wire.Len(), len(payload), "positive control sent a DATA frame")
			}
			conn.mu.Lock()
			afterConn, afterStream := conn.connSendW, stream.sendWin
			conn.mu.Unlock()
			require.Equal(t, wantConn, afterConn, "unsent reservations must not consume connection credit")
			require.Equal(t, wantStream, afterStream, "unsent reservations must not consume stream credit")
			require.True(t, conn.CanTake())
		})
	}
}

func TestH2RepeatedQueuedCancellationPreservesOtherStreamCapacity(t *testing.T) {
	client, peer := net.Pipe()
	defer peer.Close()
	conn := newH2ConnForTest(client)
	defer conn.Close()
	var wire bytes.Buffer
	conn.framer = http2.NewFramer(&wire, nil)
	for i := 0; i < 8; i++ {
		stream, err := conn.openStream()
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		observed := &signaledContext{Context: ctx, seen: make(chan struct{})}
		stream.ctx = observed
		conn.writeMu.Lock()
		done := make(chan error, 1)
		go func() { done <- conn.writeBody(stream, bytes.Repeat([]byte("x"), 4096)) }()
		select {
		case <-observed.seen:
		case <-time.After(time.Second):
			cancel()
			conn.writeMu.Unlock()
			conn.Close()
			<-done
			t.Fatal("canceled uploads exhausted the connection window before sending data")
		}
		cancel()
		err = <-done
		conn.writeMu.Unlock()
		require.ErrorIs(t, err, context.Canceled)
		conn.dropStream(stream)
	}
	require.Zero(t, wire.Len())
	other, err := conn.openStream()
	require.NoError(t, err)
	payload := bytes.Repeat([]byte("y"), h2DefaultPeerMax)
	done := make(chan error, 1)
	go func() { done <- conn.writeBody(other, payload) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		conn.mu.Lock()
		conn.failStreamLocked(other, context.Canceled)
		conn.mu.Unlock()
		<-done
		t.Fatal("a later stream stalled although no peer flow-control credit was consumed")
	}
	reader := http2.NewFramer(nil, bytes.NewReader(wire.Bytes()))
	var received []byte
	for {
		frame, err := reader.ReadFrame()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		data, ok := frame.(*http2.DataFrame)
		require.True(t, ok)
		require.Equal(t, other.id, data.StreamID)
		received = append(received, data.Data()...)
	}
	require.Equal(t, payload, received)
}

func TestH2UncertainDATAWriteStillRetiresConnection(t *testing.T) {
	client, peer := net.Pipe()
	defer peer.Close()
	conn := newH2ConnForTest(client)
	defer conn.Close()
	conn.framer = http2.NewFramer(h2ResponseWriteFunc(func([]byte) (int, error) { return 4, io.ErrUnexpectedEOF }), nil)
	stream, err := conn.openStream()
	require.NoError(t, err)
	require.ErrorIs(t, conn.writeBody(stream, bytes.Repeat([]byte("x"), 4096)), io.ErrUnexpectedEOF)
	select {
	case <-conn.done:
	case <-time.After(time.Second):
		t.Fatal("an uncertain DATA prefix left the connection reusable")
	}
	require.False(t, conn.CanTake())
	require.Equal(t, int64(h2DefaultPeerMax-4096), conn.connSendW, "do not refund uncertain on-wire DATA")
}
