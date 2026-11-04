package codexwire

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func TestH2BodySmallAndEmptyReadsPreserveDATA(t *testing.T) {
	want := strings.Repeat("response payload ", 1000)
	c := h2CompletedResponseConn(&h2ResponseHead{status: 200}, want, io.EOF)
	resp, err := c.RoundTrip(context.Background(), "GET", "https", "example.test", "/", nil, nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	n, err := resp.Body.Read(nil)
	require.Zero(t, n)
	require.NoError(t, err)
	var got bytes.Buffer
	buf := make([]byte, 7)
	for {
		n, err := resp.Body.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			require.ErrorIs(t, err, io.EOF)
			break
		}
	}
	require.Equal(t, len(want), got.Len(), "short reads must retain the remainder of each DATA frame")
	require.Equal(t, want, got.String())
}

func h2RegressionPool(t *testing.T, handler http.Handler, idle time.Duration) (*H2Pool, string, *tlsfingerprint.Profile) {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	profile := *tlsfingerprint.BuiltinProfile("rustls-h2")
	profile.RootCAs = x509.NewCertPool()
	profile.RootCAs.AddCert(srv.Certificate())
	pool := NewH2Pool(H2Options{Timeout: 3 * time.Second, IdleTimeout: idle})
	t.Cleanup(pool.CloseAll)
	return pool, srv.URL, &profile
}

func TestH2ConcurrentRequestsKeepHPACKAndStreamOrder(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdef123456"), 1000)
	pool, origin, profile := h2RegressionPool(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Sequence") != r.URL.Query().Get("id") {
			t.Errorf("HPACK state crossed streams: header=%q path=%q", r.Header.Get("X-Sequence"), r.URL.Query().Get("id"))
		}
		_, _ = w.Write(payload)
	}), time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Warm a single connection so workers exercise the same encoder and ID state.
	resp, err := pool.Do(ctx, "GET", origin, nil, nil, "", "shared", profile)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprint(i)
			resp, err := pool.Do(ctx, "GET", origin+"/?id="+id, []HeaderKV{{Name: "x-sequence", Value: id}}, nil, "", "shared", profile)
			if err != nil {
				t.Errorf("request %s: %v", id, err)
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil || !bytes.Equal(body, payload) {
				t.Errorf("request %s body: length=%d want=%d error=%v", id, len(body), len(payload), err)
			}
		}(i)
	}
	wg.Wait()
}

func TestH2CancelAfterGETResponseHeaders(t *testing.T) {
	pool, origin, profile := h2RegressionPool(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}), time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := pool.Do(ctx, "GET", origin, nil, nil, "", "cancel", profile)
	require.NoError(t, err)
	defer resp.Body.Close()
	cancel()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(resp.Body); done <- err }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		pool.CloseAll()
		<-done
		t.Fatal("GET body ignored cancellation after response headers")
	}
}

func TestH2IdleConnectionExpires(t *testing.T) {
	pool, origin, profile := h2RegressionPool(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}), 20*time.Millisecond)
	resp, err := pool.Do(context.Background(), "GET", origin, nil, nil, "", "idle", profile)
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Eventually(t, func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return len(pool.conns) == 0
	}, time.Second, 5*time.Millisecond, "idle sockets must be closed and evicted")
}

func TestH2DoesNotReplayPOSTAfterUncertainSubmission(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	c := newH2ConnForTest(client)
	defer c.Close()
	var submitted atomic.Int32
	c.framer = http2.NewFramer(h2ResponseWriteFunc(func(p []byte) (int, error) {
		// Lose the connection after the entire request body was submitted, but
		// before any response headers. This cannot prove the request was unbilled.
		if len(p) >= 9 && p[3] == byte(http2.FrameData) {
			submitted.Add(1)
			c.mu.Lock()
			for _, st := range c.streams {
				c.failStreamLocked(st, io.ErrUnexpectedEOF)
			}
			c.mu.Unlock()
		}
		return len(p), nil
	}), nil)
	var redials atomic.Int32
	pool := NewH2Pool(H2Options{BaseDialer: func(context.Context, string, string) (net.Conn, error) {
		redials.Add(1)
		return nil, errors.New("unexpected automatic replay dial")
	}})
	pool.conns["owner||example.invalid"] = c
	_, err := pool.Do(context.Background(), "POST", "https://example.invalid/responses", nil, []byte(`{"input":"bill once"}`), "", "owner", nil)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Equal(t, int32(1), submitted.Load())
	require.Zero(t, redials.Load(), "transport failures after submission must not replay POST")
}

func applyH2Settings(t *testing.T, c *h2Conn, settings ...http2.Setting) {
	t.Helper()
	var wire bytes.Buffer
	require.NoError(t, http2.NewFramer(&wire, nil).WriteSettings(settings...))
	frame, err := http2.NewFramer(nil, &wire).ReadFrame()
	require.NoError(t, err)
	require.NoError(t, c.handleFrame(frame))
}

func TestH2HonorsZeroPeerSettingsAndHPACKEviction(t *testing.T) {
	c := newH2ConnForTest(nil)
	c.framer = http2.NewFramer(io.Discard, nil)
	st, err := c.openStream()
	require.NoError(t, err)
	decoder := hpack.NewDecoder(4096, nil)
	fields := []HeaderKV{{Name: "x-sequence", Value: "retain"}}
	first, err := c.enc.EncodeBlock("GET", "https", "example.test", "/", fields)
	require.NoError(t, err)
	_, err = decoder.DecodeFull(first)
	require.NoError(t, err)
	applyH2Settings(t, c,
		http2.Setting{ID: http2.SettingMaxConcurrentStreams, Val: 0},
		http2.Setting{ID: http2.SettingInitialWindowSize, Val: 0},
		http2.Setting{ID: http2.SettingHeaderTableSize, Val: 0},
	)
	require.False(t, c.CanTake(), "MAX_CONCURRENT_STREAMS=0 means no new streams")
	require.Zero(t, st.sendWin, "INITIAL_WINDOW_SIZE=0 must reduce existing windows")
	decoder.SetAllowedMaxDynamicTableSize(0)
	second, err := c.enc.EncodeBlock("GET", "https", "example.test", "/", fields)
	require.NoError(t, err)
	require.NotEmpty(t, second)
	require.Equal(t, byte(0x20), second[0], "peer table shrink must be sent before the next header block")
	_, err = decoder.DecodeFull(second)
	require.NoError(t, err)
}

func TestH2ResponseContinuationOwnsHeaderFragments(t *testing.T) {
	c := newH2ConnForTest(nil)
	c.dec = newHpackDecoder()
	st, err := c.openStream()
	require.NoError(t, err)
	var block bytes.Buffer
	enc := hpack.NewEncoder(&block)
	require.NoError(t, enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"}))
	want := strings.Repeat("0123456789", 100)
	require.NoError(t, enc.WriteField(hpack.HeaderField{Name: "x-large", Value: want}))
	var wire bytes.Buffer
	writer := http2.NewFramer(&wire, nil)
	// Make the first fragment larger: the next ReadFrame reuses its buffer.
	split := block.Len() - 20
	require.NoError(t, writer.WriteHeaders(http2.HeadersFrameParam{StreamID: st.id, BlockFragment: block.Bytes()[:split], EndStream: true}))
	require.NoError(t, writer.WriteContinuation(st.id, true, block.Bytes()[split:]))
	c.framer = http2.NewFramer(io.Discard, &wire)
	first, err := c.framer.ReadFrame()
	require.NoError(t, err)
	require.NoError(t, c.handleHeaders(first))
	head := <-st.headCh
	require.Equal(t, 200, head.status)
	require.Contains(t, head.headers, HeaderKV{Name: "x-large", Value: want})
}

func TestH2PlaintextProxyNeverDialsDirect(t *testing.T) {
	called := false
	pool := NewH2Pool(H2Options{BaseDialer: func(context.Context, string, string) (net.Conn, error) {
		called = true
		return nil, errors.New("must not dial a direct socket")
	}})
	_, err := pool.Do(context.Background(), "POST", "http://example.invalid/responses", nil, nil, "http://proxy.invalid:8080", "owner", nil)
	require.Error(t, err)
	require.False(t, called, "an explicit proxy must never be silently bypassed")
}

type signalH2Wait struct {
	sync.Locker
	once  sync.Once
	ready chan struct{}
}

func (l *signalH2Wait) Unlock() {
	l.once.Do(func() { close(l.ready) })
	l.Locker.Unlock()
}

func TestH2BodyCloseWakesFlowControlWriter(t *testing.T) {
	c := newH2ConnForTest(nil)
	c.framer = http2.NewFramer(io.Discard, nil)
	st, err := c.openStream()
	require.NoError(t, err)
	st.sendWin = 0
	lock := &signalH2Wait{Locker: &c.mu, ready: make(chan struct{})}
	c.cond = sync.NewCond(lock)
	done := make(chan error, 1)
	go func() { done <- c.writeBody(st, []byte("pending upload")) }()
	<-lock.ready
	body := &h2StreamBody{conn: c, st: st}
	require.NoError(t, body.Close())
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		c.cond.Broadcast()
		<-done
		t.Fatal("closing the response left its upload goroutine asleep on flow control")
	}
}

func TestH2GOAWAYClosesAfterActiveBodiesFinish(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprint(active), func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			c := newH2ConnForTest(client)
			defer c.Close()
			c.framer = http2.NewFramer(io.Discard, nil)
			var st *h2Stream
			if active {
				var err error
				st, err = c.openStream()
				require.NoError(t, err)
			}
			var wire bytes.Buffer
			require.NoError(t, http2.NewFramer(&wire, nil).WriteGoAway(1, http2.ErrCodeNo, nil))
			frame, err := http2.NewFramer(nil, &wire).ReadFrame()
			require.NoError(t, err)
			require.NoError(t, c.handleFrame(frame))
			require.False(t, c.CanTake())
			if active {
				select {
				case <-c.done:
					t.Fatal("GOAWAY interrupted an accepted active stream")
				default:
				}
				require.NoError(t, (&h2StreamBody{conn: c, st: st}).Close())
			}
			select {
			case <-c.done:
			default:
				t.Fatal("drained GOAWAY connection remained open")
			}
		})
	}
}

func TestH2ClosedConnectionCannotTakeNewStreams(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	c := newH2ConnForTest(client)
	c.Close()
	require.False(t, c.CanTake())
	_, err := c.openStream()
	require.Error(t, err)
}
