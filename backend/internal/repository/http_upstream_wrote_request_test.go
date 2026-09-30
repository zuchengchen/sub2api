package repository

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// These tests pin the transport behavior forwardExcelBPS relies on to decide
// whether a failed BPS request may already have executed: httptrace's
// WroteRequest must survive the real stack (account traffic wrapper,
// per-attempt context, per-account client, long-stream HTTP/1.1 and HTTP/2
// transports) and fire with a nil error only once the request was written.

type wroteRequestTrace struct {
	headers  atomic.Bool
	wrote    atomic.Int32 // WroteRequest calls
	wroteErr atomic.Int32 // WroteRequest calls with a non-nil error
}

func (tr *wroteRequestTrace) attach(ctx context.Context) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteHeaders: func() { tr.headers.Store(true) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			tr.wrote.Add(1)
			if info.Err != nil {
				tr.wroteErr.Add(1)
			}
		},
	})
}

// admitAllTrafficCache admits every attempt so DoHTTP takes its
// req.WithContext(permit context) branch.
type admitAllTrafficCache struct{ acquired atomic.Int32 }

func (c *admitAllTrafficCache) Acquire(context.Context, service.AccountTrafficPlan, string) (service.AccountTrafficAdmission, error) {
	c.acquired.Add(1)
	return service.AccountTrafficAdmission{Allowed: true}, nil
}
func (c *admitAllTrafficCache) Refresh(context.Context, int64, string) (bool, error) {
	return true, nil
}
func (c *admitAllTrafficCache) Finish(context.Context, service.AccountTrafficPlan, string, int, int64) error {
	return nil
}
func (c *admitAllTrafficCache) Snapshot(context.Context, service.AccountTrafficPlan) (service.AccountTrafficState, error) {
	return service.AccountTrafficState{}, nil
}
func (c *admitAllTrafficCache) Sync(context.Context, service.AccountTrafficPlan) error { return nil }

const wroteRequestTestBody = `{"model":"gpt-6-astra","stream":true,"input":"hello"}`

func wroteRequestUpstream(t *testing.T, controlled bool) (*httpUpstreamService, *admitAllTrafficCache) {
	t.Helper()
	cache := &admitAllTrafficCache{}
	upstream := NewHTTPUpstream(nil)
	if controlled {
		upstream = NewControlledHTTPUpstream(nil, service.NewAccountTrafficService(cache))
	}
	s, ok := upstream.(*httpUpstreamService)
	require.True(t, ok)
	return s, cache
}

// trustLongStreamClient pre-creates the account's long-stream client, as the
// first Do would, and trusts the test server's certificate on it.
func trustLongStreamClient(t *testing.T, s *httpUpstreamService, srv *httptest.Server) {
	t.Helper()
	entry, err := s.getClientEntry("", 1, 1, service.HTTPUpstreamProfileLongStream, false, false)
	require.NoError(t, err)
	transport, ok := entry.client.Transport.(*http.Transport)
	require.True(t, ok)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	transport.TLSClientConfig.RootCAs = roots
	t.Cleanup(transport.CloseIdleConnections)
}

// newWroteRequestRequest mirrors newExcelBPSRequest's context wrappers.
func newWroteRequestRequest(t *testing.T, ctx context.Context, target string, controlled bool) *http.Request {
	t.Helper()
	ctx = service.WithHTTPUpstreamRedirectsDisabled(service.WithHTTPUpstreamProfile(ctx, service.HTTPUpstreamProfileLongStream))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader([]byte(wroteRequestTestBody)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if controlled {
		account := &service.Account{ID: 1, Concurrency: 1, Extra: map[string]any{
			service.AccountTrafficPolicyKey: map[string]any{"strict_rpm_enabled": true, "rpm": 600, "burst": 10},
		}}
		req = service.WithAccountTrafficRequest(req, account)
	}
	return req
}

type readThenDropServer struct {
	*httptest.Server
	requests  atomic.Int32
	bodyBytes atomic.Int64
	proto     atomic.Int32
}

// newReadThenDropServer reads the whole request, then drops it without a
// response: HTTP/1.1 closes the hijacked connection, HTTP/2 resets the stream.
func newReadThenDropServer(t *testing.T, h2 bool) *readThenDropServer {
	t.Helper()
	s := &readThenDropServer{}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		s.proto.Store(int32(r.ProtoMajor))
		n, _ := io.Copy(io.Discard, r.Body)
		s.bodyBytes.Store(n)
		if r.ProtoMajor == 2 {
			panic(http.ErrAbortHandler) // RST_STREAM(INTERNAL_ERROR), no response
		}
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	if h2 {
		s.EnableHTTP2 = true
		s.StartTLS()
	} else {
		s.Start()
	}
	t.Cleanup(s.Close)
	return s
}

func closedLocalAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	return address
}

func TestHTTPUpstreamWroteRequestTrace(t *testing.T) {
	for _, tc := range []struct {
		name string
		h2   bool
	}{{"http1", false}, {"tls_h2", true}} {
		for _, controlled := range []bool{false, true} {
			name := tc.name + "/plain"
			if controlled {
				name = tc.name + "/traffic_controlled"
			}
			t.Run(name+"/written_then_dropped", func(t *testing.T) {
				srv := newReadThenDropServer(t, tc.h2)
				s, cache := wroteRequestUpstream(t, controlled)
				if tc.h2 {
					trustLongStreamClient(t, s, srv.Server)
				}
				var trace wroteRequestTrace
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				resp, err := s.Do(newWroteRequestRequest(t, trace.attach(ctx), srv.URL, controlled), "", 1, 1)
				require.Error(t, err)
				require.Nil(t, resp)
				require.NoError(t, ctx.Err())
				require.Equal(t, int32(1), srv.requests.Load(), "the transport must not replay a written POST")
				require.Equal(t, int64(len(wroteRequestTestBody)), srv.bodyBytes.Load())
				if tc.h2 {
					require.Equal(t, int32(2), srv.proto.Load())
				} else {
					require.Equal(t, int32(1), srv.proto.Load())
				}
				require.True(t, trace.headers.Load())
				require.Equal(t, int32(1), trace.wrote.Load(), "WroteRequest fires exactly once before Do returns")
				require.Zero(t, trace.wroteErr.Load(), "a written request reports WroteRequest without an error")
				if controlled {
					require.Equal(t, int32(1), cache.acquired.Load(), "the traffic wrapper's WithContext branch ran")
				}
				requireNoUpstreamInFlight(t, s)
			})
			t.Run(name+"/refused", func(t *testing.T) {
				s, cache := wroteRequestUpstream(t, controlled)
				scheme := "http://"
				if tc.h2 {
					scheme = "https://"
				}
				var trace wroteRequestTrace
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				resp, err := s.Do(newWroteRequestRequest(t, trace.attach(ctx), scheme+closedLocalAddress(t), controlled), "", 1, 1)
				require.ErrorIs(t, err, syscall.ECONNREFUSED)
				require.Nil(t, resp)
				require.False(t, trace.headers.Load())
				require.Zero(t, trace.wrote.Load(), "nothing was written, so WroteRequest never fires")
				if controlled {
					require.Equal(t, int32(1), cache.acquired.Load())
				}
				requireNoUpstreamInFlight(t, s)
			})
		}
	}
}

// HTTP/2's RoundTrip returns on context cancellation without waiting for its
// writer goroutine, so Do can report context.Canceled for a request the server
// has fully received before WroteRequest is delivered. HTTP/1.1 waits for its
// write loop first. The WroteRequest callback is held open here to make the
// delivery order observable; the cancellation happens after the full write.
func TestHTTPUpstreamHTTP2CancelCanPrecedeWroteRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		h2   bool
	}{{"http1", false}, {"tls_h2", true}} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			var bodyBytes atomic.Int64
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				n, _ := io.Copy(io.Discard, r.Body)
				bodyBytes.Store(n)
				<-r.Context().Done()
			}))
			if tc.h2 {
				srv.EnableHTTP2 = true
				srv.StartTLS()
			} else {
				srv.Start()
			}
			defer srv.Close()
			s, _ := wroteRequestUpstream(t, false)
			if tc.h2 {
				trustLongStreamClient(t, s, srv)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			reqCtx, cancelRequest := context.WithCancel(ctx)
			defer cancelRequest()
			gate := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(gate) }) }
			defer release()
			var headers, reported atomic.Bool
			traced := httptrace.WithClientTrace(reqCtx, &httptrace.ClientTrace{
				WroteHeaders: func() { headers.Store(true) },
				WroteRequest: func(httptrace.WroteRequestInfo) {
					cancelRequest() // an internal cancellation right after the full write
					<-gate
					reported.Store(true)
				},
			})
			type outcome struct {
				err      error
				reported bool
			}
			req := newWroteRequestRequest(t, traced, srv.URL, false)
			done := make(chan outcome, 1)
			go func() {
				resp, err := s.Do(req, "", 1, 1)
				if resp != nil {
					_ = resp.Body.Close()
				}
				done <- outcome{err: err, reported: reported.Load()}
			}()
			select {
			case got := <-done:
				require.True(t, tc.h2, "HTTP/1.1 Do must wait for its write loop")
				require.ErrorIs(t, got.err, context.Canceled)
				require.True(t, headers.Load())
				require.False(t, got.reported, "Do returned before WroteRequest was reported")
				require.Eventually(t, func() bool { return bodyBytes.Load() == int64(len(wroteRequestTestBody)) },
					5*time.Second, 10*time.Millisecond, "the server received the whole request")
				require.Equal(t, int32(1), requests.Load())
			case <-time.After(300 * time.Millisecond):
				require.False(t, tc.h2, "HTTP/2 Do should return on cancellation without its writer")
				release()
				got := <-done
				require.ErrorIs(t, got.err, context.Canceled)
				require.True(t, got.reported, "HTTP/1.1 reports WroteRequest before Do returns")
			}
			release()
			requireNoUpstreamInFlight(t, s)
		})
	}
}
