package codexwire

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWSCanceledWaiterDoesNotWaitForBlockedWriter(t *testing.T) {
	for _, operation := range []string{"message", "ping", "control"} {
		t.Run(operation, func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			observed := &observedWriteConn{Conn: client, started: make(chan struct{})}
			c := NewWSConn(observed, nil, nil)
			defer c.CloseNow()
			first := make(chan error, 1)
			go func() { first <- c.WriteMessage(context.Background(), []byte("blocked")) }()
			<-observed.started
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if operation != "control" {
				cancel()
			}
			waiter := make(chan error, 1)
			go func() {
				switch operation {
				case "message":
					waiter <- c.WriteMessage(ctx, []byte("must not be sent"))
				case "ping":
					waiter <- c.Ping(ctx)
				case "control":
					_, err := c.ReadMessage(ctx)
					waiter <- err
				}
			}()
			if operation == "control" {
				require.NoError(t, writeUnmaskedTestFrame(server, opcodePing, false, nil))
				cancel()
			}
			select {
			case err := <-waiter:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(500 * time.Millisecond):
				_ = c.CloseNow()
				<-waiter
				t.Error("canceled operation remained blocked behind the previous writer")
			}
			_ = c.CloseNow()
			require.Error(t, <-first)
		})
	}
}

func TestH2CancellationInterruptsBlockedHEADERS(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	observed := &observedWriteConn{Conn: client, started: make(chan struct{})}
	c := newH2ConnForTest(observed)
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.RoundTrip(ctx, "GET", "https", "example.test", "/", nil, nil)
		done <- err
	}()
	<-observed.started
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(500 * time.Millisecond):
		c.Close()
		<-done
		t.Fatal("RoundTrip ignored cancellation while HEADERS was blocked in the socket")
	}
}

type signaledContext struct {
	context.Context
	once sync.Once
	seen chan struct{}
}

func (c *signaledContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.seen) })
	return err
}

func TestH2CancellationWhileWaitingForHeaderOrder(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	observed := &observedWriteConn{Conn: client, started: make(chan struct{})}
	c := newH2ConnForTest(observed)
	defer c.Close()
	first := make(chan error, 1)
	go func() {
		_, err := c.RoundTrip(context.Background(), "GET", "https", "example.test", "/first", nil, nil)
		first <- err
	}()
	<-observed.started
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signaled := &signaledContext{Context: ctx, seen: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() {
		_, err := c.RoundTrip(signaled, "GET", "https", "example.test", "/second", nil, nil)
		waiter <- err
	}()
	select {
	case <-signaled.seen: // the initial Err check observed the still-live context
	case <-time.After(100 * time.Millisecond): // implementations without an initial check
	}
	cancel()
	select {
	case err := <-waiter:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(500 * time.Millisecond):
		c.Close()
		<-waiter
		t.Error("canceled request remained blocked on the stream/header ordering lock")
	}
	c.Close()
	require.Error(t, <-first)
}
