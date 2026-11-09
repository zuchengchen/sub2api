package httputil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

type dataAndFailureReader struct {
	err error
}

func (r dataAndFailureReader) Read(p []byte) (int, error) {
	return copy(p, "partial"), r.err
}

func TestBodyReadDiagnosticsPreserveErrorsAndLastReadBytes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"truncated", io.ErrUnexpectedEOF, 408},
		{"canceled", context.Canceled, 408},
		{"timeout", timeoutNetError{}, 408},
		{"body_limit", &http.MaxBytesError{Limit: 9}, 413},
		{"unknown", errors.New("opaque reader failure"), 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &http.Request{Body: io.NopCloser(dataAndFailureReader{tc.err}), ContentLength: 25, Header: make(http.Header)}
			got, err := ReadRequestBodyWithPrealloc(req)
			if got != nil || !errors.Is(err, tc.err) || err.Error() != tc.err.Error() {
				t.Fatalf("reader failure contract changed: body=%v err=%v", got, err)
			}
			if RequestBodyReadHTTPStatus(err) != tc.want {
				t.Fatalf("status changed: %d", RequestBodyReadHTTPStatus(err))
			}
			measured, ok := RequestBodyReadDiagnostics(err)
			if !ok || measured.ReceivedBytes != 7 || measured.ReadElapsedMS < 0 || measured.ReadIdleMS < 0 || measured.ReadIdleMS > measured.ReadElapsedMS {
				t.Fatalf("invalid last-read measurements: %+v %v", measured, ok)
			}
			if tc.name == "body_limit" {
				var limitErr *http.MaxBytesError
				if !errors.As(err, &limitErr) || limitErr.Limit != 9 {
					t.Fatal("MaxBytesError no longer unwraps")
				}
			}
			if tc.name == "timeout" {
				var timeout net.Error
				if !errors.As(err, &timeout) || !timeout.Timeout() {
					t.Fatal("net.Error no longer unwraps")
				}
			}
		})
	}
}

func TestBodyReadDiagnosticsCountAcrossChunksAndUnknownLength(t *testing.T) {
	body := bytes.Repeat([]byte("x"), (2<<20)+17)
	req := &http.Request{
		Body:          io.NopCloser(io.MultiReader(bytes.NewReader(body), iotest.ErrReader(io.ErrUnexpectedEOF))),
		ContentLength: -1, Header: make(http.Header),
	}
	got, err := ReadRequestBodyWithPrealloc(req)
	measured, ok := RequestBodyReadDiagnostics(err)
	if got != nil || !ok || measured.ReceivedBytes != int64(len(body)) || req.ContentLength != -1 {
		t.Fatalf("unknown-length read lost measurements: body=%v measured=%+v ok=%v", got, measured, ok)
	}
}

func TestBodyReadDiagnosticsMeasureRawBytesForDecodeFailure(t *testing.T) {
	req := &http.Request{Body: io.NopCloser(strings.NewReader("broken-gzip")), ContentLength: 11, Header: http.Header{"Content-Encoding": []string{"gzip"}}}
	got, err := ReadRequestBodyWithPrealloc(req)
	measured, ok := RequestBodyReadDiagnostics(err)
	if got != nil || !ok || measured.ReceivedBytes != 11 || RequestBodyReadHTTPStatus(err) != 400 || RequestBodyReadErrorKind(err) != BodyReadKindDecodeContentEncoding {
		t.Fatalf("decode failure contract changed: measured=%+v ok=%v err=%v", measured, ok, err)
	}
	if req.ContentLength != 11 || req.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("failed decoder changed original request metadata")
	}
}

func TestBodyReadDiagnosticsUnknownAndSuccessfulReads(t *testing.T) {
	if _, ok := RequestBodyReadDiagnostics(errors.New("unknown reader")); ok {
		t.Fatal("invented measurements for another reader")
	}
	if _, ok := RequestBodyReadDiagnostics(nil); ok {
		t.Fatal("invented measurements for success")
	}
	body := []byte("complete")
	req := &http.Request{Body: NewPrereadBody(body), ContentLength: int64(len(body)), Header: make(http.Header)}
	got, err := ReadRequestBodyWithPrealloc(req)
	if err != nil || &got[0] != &body[0] {
		t.Fatal("preread zero-copy path changed")
	}
	// EOF returned together with the final data must remain a successful read.
	req.Body = io.NopCloser(dataAndFailureReader{io.EOF})
	got, err = ReadRequestBodyWithPrealloc(req)
	if err != nil || string(got) != "partial" {
		t.Fatalf("terminal data+EOF rejected: %q %v", got, err)
	}
}

func TestBodyReadDiagnosticsElapsedAndIdleHaveDistinctMeaning(t *testing.T) {
	start := time.Unix(1000, 0)
	progress := requestBodyReadProgress{started: start, lastProgress: start.Add(65 * time.Second), receivedBytes: 1024}
	err := progress.wrapError(io.ErrUnexpectedEOF, start.Add(125*time.Second))
	measured, ok := RequestBodyReadDiagnostics(fmt.Errorf("outer: %w", err))
	if !ok || measured.ReadElapsedMS != 125000 || measured.ReadIdleMS != 60000 || measured.ReceivedBytes != 1024 {
		t.Fatalf("elapsed/idle mismatch: %+v", measured)
	}
	progress.lastProgress = time.Time{}
	measured, _ = RequestBodyReadDiagnostics(progress.wrapError(io.ErrUnexpectedEOF, start.Add(time.Second)))
	if measured.ReadIdleMS != 1000 {
		t.Fatalf("no-progress interval must start at first read: %+v", measured)
	}
}

func BenchmarkOperationsUploadRead(b *testing.B) {
	for _, size := range []int{16 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("bytes_%d", size), func(b *testing.B) {
			body := bytes.Repeat([]byte("x"), size)
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req := &http.Request{Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(size)}
				got, err := ReadRequestBodyWithPrealloc(req)
				if err != nil || len(got) != size {
					b.Fatal("body read failed")
				}
			}
		})
	}
}
