package codexwire

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// codexHTTPHeaderOrder is the official Codex HTTP request header order for
// POST /backend-api/codex/responses, derived from codex-rs
// (endpoint/responses.rs stream_request + session layer base headers). The
// wire order is the HeaderMap iteration order for this key set; it follows the
// same hash-derived ordering observed on the WS handshake for the shared keys.
// Transport-written headers (Host, Content-Length, Accept-Encoding,
// Content-Type, Connection) are excluded — the request writer emits them at
// their reqwest-style positions.
var codexHTTPHeaderOrder = []string{
	"chatgpt-account-id",
	"authorization",
	"user-agent",
	"originator",
	"version",
	"x-codex-beta-features",
	"x-codex-turn-state",
	"x-client-request-id",
	"session-id",
	"thread-id",
	"x-codex-window-id",
	"x-codex-turn-metadata",
	"x-codex-parent-thread-id",
	"x-openai-subagent",
	"x-openai-memgen-request",
	"x-openai-internal-codex-responses-lite",
	"x-responsesapi-include-timing-metrics",
	"x-codex-routing-hint",
	"x-oai-attestation",
	"cookie",
	// Body/negotiation headers are user-set in reqwest's HeaderMap; their exact
	// hash positions are pending a real HTTP capture for calibration.
	"content-type",
	"content-encoding",
	"accept",
	"accept-encoding",
}

// OrderCodexHTTPHeaders flattens h into the official Codex HTTP request order.
func OrderCodexHTTPHeaders(h http.Header) []HeaderKV {
	return orderByTemplate(h, codexHTTPHeaderOrder)
}

func orderByTemplate(h http.Header, template []string) []HeaderKV {
	if len(h) == 0 {
		return nil
	}
	out := make([]HeaderKV, 0, len(h))
	for _, name := range template {
		for key, values := range h {
			if !strings.EqualFold(key, name) {
				continue
			}
			for _, value := range values {
				if strings.TrimSpace(value) == "" {
					continue
				}
				out = append(out, HeaderKV{Name: name, Value: value})
			}
		}
	}
	return out
}

// HTTP1Options tunes the raw HTTP/1.1 client.
type HTTP1Options struct {
	TLSProfile *tlsfingerprint.Profile
	// Timeout bounds connect + request write + response header read.
	Timeout time.Duration
	// BaseDialer overrides TCP dialing (testing).
	BaseDialer func(ctx context.Context, network, addr string) (net.Conn, error)
}

// DoHTTP1 performs one raw HTTP/1.1 request with byte-exact request-line and
// ordered application headers, returning the streaming *http.Response.
// The caller owns resp.Body (closing it closes the connection — the response
// body reader is bound to the socket, no keep-alive pooling by design: the
// Codex backend lane is one request per turn on a fresh stream, matching the
// official client's reqwest pool semantics at the HTTP level).
func DoHTTP1(ctx context.Context, method, rawURL string, ordered []HeaderKV, body []byte, proxyURL string, opts HTTP1Options) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	host := u.Host
	if host == "" {
		return nil, errors.New("url missing host")
	}
	addr := host
	if u.Port() == "" {
		if scheme == "https" {
			addr = net.JoinHostPort(host, "443")
		} else {
			addr = net.JoinHostPort(host, "80")
		}
	}
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	dialCtx := ctx
	cancel := func() {}
	if opts.Timeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
	}
	defer cancel()

	var conn net.Conn
	switch scheme {
	case "http":
		base := opts.BaseDialer
		if base == nil {
			base = (&net.Dialer{}).DialContext
		}
		conn, err = base(dialCtx, "tcp", addr)
	case "https":
		conn, err = tlsConn(dialCtx, addr, proxyURL, opts.TLSProfile, opts.BaseDialer)
	default:
		return nil, fmt.Errorf("unsupported scheme %q", scheme)
	}
	if err != nil {
		return nil, err
	}
	return writeHTTP1Request(dialCtx, conn, method, path, host, ordered, body)
}

// doHTTP1OnConn runs a raw HTTP/1.1 request over an already-established
// connection (used when the h2 dialer negotiated a non-h2 ALPN).
func doHTTP1OnConn(ctx context.Context, method, path, host string, ordered []HeaderKV, body []byte, dial func(context.Context) (net.Conn, error), timeout time.Duration) (*http.Response, error) {
	dialCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	conn, err := dial(dialCtx)
	if err != nil {
		return nil, err
	}
	return writeHTTP1Request(dialCtx, conn, method, path, host, ordered, body)
}

// writeHTTP1Request writes the byte-exact request on conn and reads the
// streaming response.
func writeHTTP1Request(ctx context.Context, conn net.Conn, method, path, host string, ordered []HeaderKV, body []byte) (*http.Response, error) {
	var req strings.Builder
	fmt.Fprintf(&req, "%s %s HTTP/1.1\r\nHost: %s\r\n", method, path, host)
	hasCL := false
	for _, kv := range ordered {
		if strings.ContainsAny(kv.Name, "\r\n:") || strings.ContainsAny(kv.Value, "\r\n") {
			_ = conn.Close()
			return nil, fmt.Errorf("invalid header %q", kv.Name)
		}
		if strings.EqualFold(kv.Name, "content-length") || strings.EqualFold(kv.Name, "transfer-encoding") {
			hasCL = true
		}
		fmt.Fprintf(&req, "%s: %s\r\n", kv.Name, kv.Value)
	}
	// hyper appends the framing header after user headers.
	if !hasCL {
		fmt.Fprintf(&req, "content-length: %d\r\n", len(body))
	}
	fmt.Fprint(&req, "\r\n")
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := conn.Write([]byte(req.String())); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if len(body) > 0 {
		if _, err := conn.Write(body); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	br := bufio.NewReader(conn)
	reqProto := &http.Request{Method: method}
	resp, err := http.ReadResponse(br, reqProto)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read response: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	resp.Body = &connBoundBody{ReadCloser: resp.Body, conn: conn}
	return resp, nil
}

// connBoundBody ties the socket's lifetime to the response body.
type connBoundBody struct {
	ReadCloser interface {
		Read([]byte) (int, error)
		Close() error
	}
	conn net.Conn
}

func (b *connBoundBody) Read(p []byte) (int, error) { return b.ReadCloser.Read(p) }
func (b *connBoundBody) Close() error {
	_ = b.ReadCloser.Close()
	return b.conn.Close()
}
