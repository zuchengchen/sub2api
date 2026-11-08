package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func terminalDiagnosticWire() []byte {
	return []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_private\",\"status\":\"in_progress\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_private\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg_private\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"PRIVATE_ANSWER\"}]}],\"usage\":{\"input_tokens\":17,\"output_tokens\":5,\"total_tokens\":22,\"input_tokens_details\":{\"cached_tokens\":3}}}}\n\n")
}

type terminalDiagnosticBlockingBody struct {
	first           []byte
	reads           atomic.Int32
	started, closed chan struct{}
	once            sync.Once
	failure         error
}

func (b *terminalDiagnosticBlockingBody) Read(p []byte) (int, error) {
	if b.reads.Add(1) == 1 {
		return copy(p, b.first), nil
	}
	close(b.started)
	<-b.closed
	if b.failure != nil {
		return 0, b.failure
	}
	return 0, net.ErrClosed
}

func TestUserCacheDiagnosticTerminalReadErrorClasses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
	}{{"closed", net.ErrClosed}, {"canceled", context.Canceled}, {"deadline", context.DeadlineExceeded}, {"unexpected_eof", io.ErrUnexpectedEOF}, {"other", errors.New("SECRET_NETWORK_ERROR")}} {
		t.Run(tc.name, func(t *testing.T) {
			wire := terminalDiagnosticWire()
			body := &terminalDiagnosticBlockingBody{first: wire, started: make(chan struct{}), closed: make(chan struct{}), failure: tc.failure}
			o, path := terminalDiagnosticObserver(t, body)
			_, err := o.Read(make([]byte, len(wire)))
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() { _, e := o.Read(make([]byte, 1)); done <- e }()
			<-body.started
			require.NoError(t, o.Close())
			require.ErrorIs(t, <-done, tc.failure)
			row := userCacheDiagnosticReadEvents(t, path)[2]
			require.Equal(t, true, row["read_error"])
			require.Equal(t, tc.name, row["read_error_class"])
			require.Equal(t, "after_terminal_candidate", row["read_error_phase"])
			require.EqualValues(t, 2, body.reads.Load())
		})
	}
}
func (b *terminalDiagnosticBlockingBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func terminalDiagnosticObserver(t *testing.T, body io.ReadCloser) (*userCacheResponseObserver, string) {
	t.Helper()
	path := userCacheDiagnosticTestEnv(t)
	d := beginUserCacheDiagnostic(userCacheDiagnosticTestContext(428), []byte(`{}`))
	a := d.upstreamRequest(&Account{ID: 1}, httptest.NewRequest(http.MethodPost, "/responses", nil))
	r := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}
	a.upstreamResponse(r, nil)
	return r.Body.(*userCacheResponseObserver), path
}

func TestUserCacheDiagnosticTerminalReadErrorOrdering(t *testing.T) {
	wire := terminalDiagnosticWire()
	body := &terminalDiagnosticBlockingBody{first: wire, started: make(chan struct{}), closed: make(chan struct{})}
	o, path := terminalDiagnosticObserver(t, body)
	buf := make([]byte, len(wire)+64)
	n, err := o.Read(buf)
	require.NoError(t, err)
	require.Equal(t, wire, buf[:n])
	readDone := make(chan error, 1)
	go func() { _, e := o.Read(buf); readDone <- e }()
	<-body.started
	require.NoError(t, o.Close())
	select {
	case e := <-readDone:
		require.ErrorIs(t, e, net.ErrClosed)
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock real Read")
	}
	require.NoError(t, o.Close())
	require.EqualValues(t, 2, body.reads.Load())
	rows := userCacheDiagnosticReadEvents(t, path)
	require.Len(t, rows, 3)
	row := rows[2]
	require.Equal(t, true, row["read_error"])
	require.Equal(t, "after_terminal_candidate", row["read_error_phase"])
	require.Equal(t, "closed", row["read_error_class"])
	require.Equal(t, true, row["local_close_started"])
	require.Equal(t, true, row["read_error_observed_after_close_started"])
	candidate := row["terminal_candidate"].(map[string]any)
	require.Equal(t, diagnosticSHA256([]byte("resp_private")), candidate["response_id_sha256"])
	require.Equal(t, "completed", candidate["status_class"])
	require.Equal(t, false, candidate["prefix_error_event"])
}

type terminalDiagnosticDataError struct{ data []byte }

func (b *terminalDiagnosticDataError) Read(p []byte) (int, error) {
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, net.ErrClosed
}
func (*terminalDiagnosticDataError) Close() error { return nil }

func TestUserCacheDiagnosticSameReadTerminalBoundary(t *testing.T) {
	for _, complete := range []bool{true, false} {
		t.Run(map[bool]string{true: "complete", false: "partial"}[complete], func(t *testing.T) {
			wire := terminalDiagnosticWire()
			if !complete {
				wire = wire[:len(wire)-1]
			}
			o, path := terminalDiagnosticObserver(t, &terminalDiagnosticDataError{data: bytes.Clone(wire)})
			buf := make([]byte, len(wire)+1)
			n, err := o.Read(buf)
			require.ErrorIs(t, err, net.ErrClosed)
			require.Equal(t, wire, buf[:n])
			require.NoError(t, o.Close())
			row := userCacheDiagnosticReadEvents(t, path)[2]
			require.Equal(t, true, row["read_error"])
			if complete {
				require.Equal(t, "same_read_after_terminal_candidate", row["read_error_phase"])
				require.NotNil(t, row["terminal_candidate"])
			} else {
				require.Equal(t, "before_terminal_candidate", row["read_error_phase"])
				require.Nil(t, row["terminal_candidate"])
			}
		})
	}
}

type terminalDiagnosticUncooperativeBody struct{ entered, release chan struct{} }

func (b *terminalDiagnosticUncooperativeBody) Read([]byte) (int, error) {
	close(b.entered)
	<-b.release
	return 0, io.EOF
}
func (*terminalDiagnosticUncooperativeBody) Close() error { return nil }

func TestUserCacheDiagnosticCloseDoesNotWaitForArbitraryReader(t *testing.T) {
	body := &terminalDiagnosticUncooperativeBody{entered: make(chan struct{}), release: make(chan struct{})}
	o, path := terminalDiagnosticObserver(t, body)
	readDone := make(chan struct{})
	go func() { defer close(readDone); _, _ = o.Read(make([]byte, 1)) }()
	<-body.entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- o.Close() }()
	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		close(body.release)
		t.Fatal("diagnostics added a blocking wait to Close")
	}
	close(body.release)
	<-readDone
	rows := userCacheDiagnosticReadEvents(t, path)
	require.Len(t, rows, 3)
	require.Nil(t, rows[2]["terminal_candidate"])
}
