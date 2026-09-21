package httputil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"testing"
)

func TestRequestBodyReadErrorKind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		kind string
	}{
		{name: "nil", err: nil, kind: BodyReadKindNone},
		{name: "max bytes", err: &http.MaxBytesError{Limit: 10}, kind: BodyReadKindMaxBytes},
		{name: "unsupported encoding", err: errors.New(`decode Content-Encoding "br": unsupported Content-Encoding`), kind: BodyReadKindUnsupportedContentEncoding},
		{name: "decode encoding", err: errors.New(`decode Content-Encoding "gzip": unexpected EOF`), kind: BodyReadKindDecodeContentEncoding},
		{name: "canceled", err: context.Canceled, kind: BodyReadKindClientDisconnect},
		{name: "reset", err: syscall.ECONNRESET, kind: BodyReadKindClientDisconnect},
		{name: "pipe", err: syscall.EPIPE, kind: BodyReadKindClientDisconnect},
		{name: "net closed", err: net.ErrClosed, kind: BodyReadKindClientDisconnect},
		{name: "unexpected eof", err: io.ErrUnexpectedEOF, kind: BodyReadKindTruncatedBody},
		{name: "wrapped reset", err: fmt.Errorf("read: %w", syscall.ECONNRESET), kind: BodyReadKindClientDisconnect},
		{name: "http2 stream closed", err: errors.New("http2: stream closed"), kind: BodyReadKindClientDisconnect},
		{name: "io timeout string", err: errors.New("read tcp 127.0.0.1:8080: i/o timeout"), kind: BodyReadKindTransport},
		{name: "generic io", err: errors.New("read failed"), kind: BodyReadKindIORead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RequestBodyReadErrorKind(tt.err); got != tt.kind {
				t.Fatalf("kind=%s want %s", got, tt.kind)
			}
		})
	}
}

func TestRequestBodyReadHTTPStatus(t *testing.T) {
	t.Parallel()
	if got := RequestBodyReadHTTPStatus(&http.MaxBytesError{Limit: 1}); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("max bytes status=%d", got)
	}
	if got := RequestBodyReadHTTPStatus(io.ErrUnexpectedEOF); got != http.StatusRequestTimeout {
		t.Fatalf("truncated status=%d", got)
	}
	if got := RequestBodyReadHTTPStatus(syscall.ECONNRESET); got != http.StatusRequestTimeout {
		t.Fatalf("disconnect status=%d", got)
	}
	if got := RequestBodyReadHTTPStatus(errors.New("read failed")); got != http.StatusBadRequest {
		t.Fatalf("io status=%d", got)
	}
}

func TestRequestContentEncodingCategory(t *testing.T) {
	t.Parallel()
	if got := RequestContentEncodingCategory("private-payload-marker"); got != "other" {
		t.Fatalf("got %q", got)
	}
	if got := RequestContentEncodingCategory("gzip"); got != "gzip" {
		t.Fatalf("got %q", got)
	}
}

type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "timeout" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return true }

func TestRequestBodyReadIncomplete(t *testing.T) {
	t.Parallel()
	if !RequestBodyReadIncomplete(timeoutNetError{}) {
		t.Fatal("transport timeout should be incomplete")
	}
	if RequestBodyReadIncomplete(errors.New("decode Content-Encoding \"gzip\": boom")) {
		t.Fatal("decode failure is a malformed body, not an incomplete upload")
	}
}
