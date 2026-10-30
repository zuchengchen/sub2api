package codexwire

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"golang.org/x/net/http2"
)

type h2ResponseWriteFunc func([]byte) (int, error)

func (f h2ResponseWriteFunc) Write(p []byte) (int, error) { return f(p) }

// Complete the response synchronously while writing the request. Both headCh
// and done are ready before RoundTrip waits, without sleeps or scheduler timing.
func h2CompletedResponseConn(head *h2ResponseHead, body string, endErr error) *h2Conn {
	c := newH2ConnForTest(nil)
	c.framer = http2.NewFramer(h2ResponseWriteFunc(func(p []byte) (int, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, st := range c.streams {
			if st.closed {
				continue
			}
			if head != nil {
				st.gotHead = true
				st.headCh <- head
			}
			if body != "" {
				st.bodyCh <- []byte(body)
			}
			c.failStreamLocked(st, endErr)
		}
		return len(p), nil
	}), nil)
	return c
}

func TestH2RoundTripCompletedResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		endErr error
	}{
		{name: "headers_end_stream", endErr: io.EOF},
		{name: "data_end_stream", body: "data: {}\n\n", endErr: io.EOF},
		{name: "reset_after_headers", body: "partial", endErr: errH2StreamReset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Exercise both choices of a select with two ready channels. Every
			// iteration must return the queued head, even if done wins first.
			for i := 0; i < 128; i++ {
				c := h2CompletedResponseConn(&h2ResponseHead{
					status:  http.StatusOK,
					headers: []HeaderKV{{Name: "content-type", Value: "text/event-stream"}},
				}, tc.body, tc.endErr)
				resp, err := c.RoundTrip(context.Background(), "GET", "https", "example.test", "/", nil, nil)
				if err != nil {
					t.Fatalf("iteration %d: completed response lost its headers: %v", i, err)
				}
				if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
					t.Fatalf("unexpected response head: status=%d headers=%v", resp.StatusCode, resp.Header)
				}
				if len(c.streams) != 1 {
					t.Fatal("response stream removed before its body was closed")
				}
				body, err := io.ReadAll(resp.Body)
				if string(body) != tc.body {
					t.Fatalf("body = %q, want %q", body, tc.body)
				}
				if errors.Is(tc.endErr, io.EOF) {
					if err != nil {
						t.Fatalf("completed body: %v", err)
					}
				} else if !errors.Is(err, tc.endErr) {
					t.Fatalf("body error = %v, want %v", err, tc.endErr)
				}
				if err := resp.Body.Close(); err != nil {
					t.Fatal(err)
				}
				if len(c.streams) != 0 {
					t.Fatal("closed response leaked its stream")
				}
			}
		})
	}
}

func TestH2RoundTripClosedBeforeHeaders(t *testing.T) {
	for _, tc := range []struct {
		name   string
		endErr error
	}{
		{name: "eof", endErr: io.EOF},
		{name: "reset", endErr: errH2StreamReset},
		{name: "canceled", endErr: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := h2CompletedResponseConn(nil, "", tc.endErr)
			resp, err := c.RoundTrip(context.Background(), "GET", "https", "example.test", "/", nil, nil)
			if resp != nil || err == nil {
				t.Fatalf("response=%v error=%v, want an error without a response", resp, err)
			}
			if errors.Is(tc.endErr, io.EOF) {
				if err.Error() != "h2: stream closed before response headers" {
					t.Fatalf("error = %v", err)
				}
			} else if !errors.Is(err, tc.endErr) {
				t.Fatalf("error = %v, want %v", err, tc.endErr)
			}
			if len(c.streams) != 0 {
				t.Fatal("failed response leaked its stream")
			}
		})
	}
}
