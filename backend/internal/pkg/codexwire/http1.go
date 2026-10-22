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
// POST /backend-api/codex/responses (HTTP/1.1 fallback lane), captured from
// codex-cli 0.163.0 by backend/tools/codexgolden
// (testdata/golden/0.163.0/http_fallback_tool_loop.jsonl):
//
//	version, x-codex-beta-features, [x-codex-turn-state], x-codex-window-id,
//	x-codex-turn-metadata, x-codex-routing-hint, x-client-request-id,
//	session-id, thread-id, accept, content-encoding, content-type,
//	authorization, chatgpt-account-id, originator, user-agent, host,
//	content-length
//
// Headers absent from the capture are slotted next to the keys codex-rs
// inserts them with: subagent/parent-thread/memgen/lite/timing-metrics ride in
// the per-turn block after x-codex-turn-metadata, attestation follows the
// routing hint, and reqwest's cookie store appends cookie after the default
// headers. hyper writes host and content-length after all of them.
var codexHTTPHeaderOrder = []string{
	"version",
	"x-codex-beta-features",
	"x-codex-turn-state",
	"x-codex-window-id",
	"x-codex-turn-metadata",
	"x-codex-parent-thread-id",
	"x-openai-subagent",
	"x-openai-memgen-request",
	"x-openai-internal-codex-responses-lite",
	"x-responsesapi-include-timing-metrics",
	"x-codex-routing-hint",
	"x-oai-attestation",
	"x-client-request-id",
	"session-id",
	"thread-id",
	"accept",
	"accept-encoding",
	"content-encoding",
	"content-type",
	"authorization",
	"chatgpt-account-id",
	"x-openai-fedramp",
	"originator",
	"user-agent",
	"cookie",
}

// codexModelsHeaderOrder is GET {base}/codex/models (golden 0.163.0): provider
// default headers only, reqwest's default accept last.
var codexModelsHeaderOrder = []string{
	"version", "authorization", "chatgpt-account-id", "x-openai-fedramp", "originator", "user-agent", "accept",
}

// OrderCodexHTTPHeaders flattens h into the official Codex HTTP request order
// for POST /responses.
func OrderCodexHTTPHeaders(h http.Header) []HeaderKV {
	return orderByTemplate(h, codexHTTPHeaderOrder)
}

// OrderCodexHTTPHeadersForPath picks the captured template for the request
// path (models manifest vs Responses). reqwest's default accept (*/*) is
// added for the models GET when the caller did not set one.
func OrderCodexHTTPHeadersForPath(path string, h http.Header) []HeaderKV {
	if strings.HasSuffix(strings.TrimRight(path, "/"), "/codex/models") {
		if h.Get("Accept") == "" {
			h = h.Clone()
			h.Set("Accept", "*/*")
		}
		return orderByTemplate(h, codexModelsHeaderOrder)
	}
	return orderByTemplate(h, codexHTTPHeaderOrder)
}

func orderByTemplate(h http.Header, template []string) []HeaderKV {
	if len(h) == 0 {
		return nil
	}
	out := make([]HeaderKV, 0, len(h))
	matched := make(map[string]bool, len(h))
	for _, name := range template {
		for key, values := range h {
			if !strings.EqualFold(key, name) {
				continue
			}
			matched[key] = true
			for _, value := range values {
				if strings.TrimSpace(value) == "" {
					continue
				}
				out = append(out, HeaderKV{Name: name, Value: value})
			}
		}
	}
	// 模板外头原样透传（reqwest 也发送 HeaderMap 里全部头）；http crate 线序
	// 恒定小写名，Go 的 Canonical-Case 键需要降写。
	for key, values := range h {
		if matched[key] {
			continue
		}
		name := strings.ToLower(key)
		for _, value := range values {
			if strings.TrimSpace(value) == "" {
				continue
			}
			out = append(out, HeaderKV{Name: name, Value: value})
		}
	}
	return out
}

// HTTP1Options tunes the raw HTTP/1.1 client.
type HTTP1Options struct {
	TLSProfile *tlsfingerprint.Profile
	// DialTimeout bounds TCP connect + proxy CONNECT + TLS handshake.
	DialTimeout time.Duration
	// ResponseHeaderTimeout bounds request write + response header read
	// (0 = bounded only by ctx; Codex turns can take minutes before the first
	// byte under load).
	ResponseHeaderTimeout time.Duration
	// BaseDialer overrides TCP dialing (testing).
	BaseDialer func(ctx context.Context, network, addr string) (net.Conn, error)
}

// DoHTTP1 performs one raw HTTP/1.1 request with byte-exact request-line and
// ordered application headers on a fresh connection, returning the streaming
// *http.Response. The caller owns resp.Body; closing it closes the socket.
//
// One connection per request matches the official Codex CLI: core/src/client.rs
// builds a new reqwest Client (and therefore a new connection pool) for every
// Responses request, so each HTTP turn opens a new TCP+TLS connection
// (confirmed by testdata/golden/0.163.0/http_fallback_tool_loop.jsonl).
// Cancelling ctx closes the socket at any point, including mid-body.
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
	conn, err := dialHTTP1(ctx, scheme, addr, proxyURL, opts)
	if err != nil {
		return nil, err
	}
	return writeHTTP1Request(ctx, conn, method, path, host, ordered, body, opts.ResponseHeaderTimeout)
}

func dialHTTP1(ctx context.Context, scheme, addr, proxyURL string, opts HTTP1Options) (net.Conn, error) {
	dialCtx := ctx
	if opts.DialTimeout > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, opts.DialTimeout)
		defer cancel()
	}
	switch scheme {
	case "http":
		base := opts.BaseDialer
		if base == nil {
			base = (&net.Dialer{}).DialContext
		}
		return base(dialCtx, "tcp", addr)
	case "https":
		return tlsConn(dialCtx, addr, proxyURL, opts.TLSProfile, opts.BaseDialer)
	default:
		return nil, fmt.Errorf("unsupported scheme %q", scheme)
	}
}

// doHTTP1OnConn runs a raw HTTP/1.1 request over a connection produced by
// dial (used when the h2 dialer negotiated a non-h2 ALPN). timeout bounds the
// dial only.
func doHTTP1OnConn(ctx context.Context, method, path, host string, ordered []HeaderKV, body []byte, dial func(context.Context) (net.Conn, error), timeout time.Duration) (*http.Response, error) {
	dialCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	conn, err := dial(dialCtx)
	if err != nil {
		return nil, err
	}
	return writeHTTP1Request(ctx, conn, method, path, host, ordered, body, 0)
}

// writeHTTP1Request writes the byte-exact request on conn and reads the
// streaming response. The socket is closed when ctx is cancelled or the
// response body is closed, whichever comes first.
func writeHTTP1Request(ctx context.Context, conn net.Conn, method, path, host string, ordered []HeaderKV, body []byte, headerTimeout time.Duration) (*http.Response, error) {
	var req strings.Builder
	fmt.Fprintf(&req, "%s %s HTTP/1.1\r\n", method, path)
	hasCL := false
	hasHost := false
	for _, kv := range ordered {
		if strings.ContainsAny(kv.Name, "\r\n:") || strings.ContainsAny(kv.Value, "\r\n") {
			_ = conn.Close()
			return nil, fmt.Errorf("invalid header %q", kv.Name)
		}
		if strings.EqualFold(kv.Name, "content-length") || strings.EqualFold(kv.Name, "transfer-encoding") {
			hasCL = true
		}
		if strings.EqualFold(kv.Name, "host") {
			hasHost = true
		}
		fmt.Fprintf(&req, "%s: %s\r\n", kv.Name, kv.Value)
	}
	// hyper emits transport framing headers AFTER user headers, lowercase:
	// host then content-length (golden http_fallback_tool_loop.jsonl). An
	// empty reqwest body is end-of-stream, so hyper's set_length writes no
	// content-length at all (golden GET wham/accounts/check, codex/models).
	if !hasHost {
		fmt.Fprintf(&req, "host: %s\r\n", host)
	}
	if !hasCL && len(body) > 0 {
		fmt.Fprintf(&req, "content-length: %d\r\n", len(body))
	}
	fmt.Fprint(&req, "\r\n")

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	fail := func(err error) (*http.Response, error) {
		stop()
		_ = conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	if headerTimeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(headerTimeout))
	}
	// One write for head+body, like hyper's buffered writer.
	wire := make([]byte, 0, req.Len()+len(body))
	wire = append(wire, req.String()...)
	wire = append(wire, body...)
	if _, err := conn.Write(wire); err != nil {
		return fail(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: method})
	if err != nil {
		return fail(fmt.Errorf("read response: %w", err))
	}
	_ = conn.SetDeadline(time.Time{})
	resp.Body = &connBoundBody{ReadCloser: resp.Body, conn: conn, stop: stop}
	return resp, nil
}

// connBoundBody ties the socket's lifetime to the response body.
type connBoundBody struct {
	ReadCloser interface {
		Read([]byte) (int, error)
		Close() error
	}
	conn net.Conn
	stop func() bool
}

func (b *connBoundBody) Read(p []byte) (int, error) { return b.ReadCloser.Read(p) }
func (b *connBoundBody) Close() error {
	if b.stop != nil {
		b.stop()
	}
	_ = b.ReadCloser.Close()
	return b.conn.Close()
}
