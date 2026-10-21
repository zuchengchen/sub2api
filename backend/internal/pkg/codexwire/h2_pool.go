package codexwire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// H2Options configures the Codex HTTP/2 client pool.
type H2Options struct {
	// TLSProfile must advertise h2 ALPN (use the "rustls-h2" builtin profile).
	TLSProfile *tlsfingerprint.Profile
	// Timeout bounds connect + handshake per new connection.
	Timeout time.Duration
	// IdleTimeout closes pooled connections idle longer than this (default 90s,
	// matching the standard upstream idle timeout).
	IdleTimeout time.Duration
	// BaseDialer overrides TCP dialing (testing).
	BaseDialer func(ctx context.Context, network, addr string) (net.Conn, error)
}

// H2Pool keeps one multiplexed h2 connection per (key, proxy, target) triple
// and transparently falls back to raw HTTP/1.1 when ALPN doesn't yield h2.
type H2Pool struct {
	opts H2Options

	mu    sync.Mutex
	conns map[string]*h2Conn
}

func NewH2Pool(opts H2Options) *H2Pool {
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 90 * time.Second
	}
	return &H2Pool{opts: opts, conns: make(map[string]*h2Conn)}
}

// CloseAll tears down every pooled connection (process shutdown).
func (p *H2Pool) CloseAll() {
	p.mu.Lock()
	conns := p.conns
	p.conns = make(map[string]*h2Conn)
	p.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func (p *H2Pool) evict(key string, c *h2Conn) {
	p.mu.Lock()
	if p.conns[key] == c {
		delete(p.conns, key)
	}
	p.mu.Unlock()
}

// Do sends one request through the h2 pool. ordered carries the wire-ordered
// application headers (see OrderCodexHTTPHeaders). When the negotiated ALPN
// isn't h2 the same TCP+TLS connection is used for a raw HTTP/1.1 request.
func (p *H2Pool) Do(ctx context.Context, method, rawURL string, ordered []HeaderKV, body []byte, proxyURL, key string, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	host := u.Host
	if host == "" {
		return nil, errors.New("url missing host")
	}
	scheme := strings.ToLower(u.Scheme)
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

	// Plain-http targets cannot carry a TLS fingerprint; use the raw h1 path.
	if scheme != "https" {
		return doHTTP1OnConn(ctx, method, path, host, ordered, body, func(dctx context.Context) (net.Conn, error) {
			base := p.opts.BaseDialer
			if base == nil {
				base = (&net.Dialer{}).DialContext
			}
			return base(dctx, "tcp", addr)
		}, p.opts.Timeout)
	}

	poolKey := key + "|" + proxyURL + "|" + host

	for attempt := 0; attempt < 2; attempt++ {
		conn, err := p.acquire(ctx, poolKey, key, addr, proxyURL, profile)
		if err != nil {
			var h1c *h1FallbackConn
			if errors.As(err, &h1c) {
				slog.Info("codex_h2_alpn_fallback", "key", key, "host", host, "alpn", h1c.alpn)
				return doHTTP1OnConn(ctx, method, path, host, ordered, body, func(context.Context) (net.Conn, error) {
					return h1c.conn, nil
				}, 0)
			}
			return nil, err
		}
		resp, err := conn.RoundTrip(ctx, method, scheme, host, path, ordered, body)
		if err == nil {
			return resp, nil
		}
		// Retry once on a fresh connection for transport-level failures that
		// hit before any stream output.
		if errors.Is(err, errH2GoAway) || errors.Is(err, errH2StreamsFull) {
			continue
		}
		var netErr net.Error
		if errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			conn.Close()
			continue
		}
		return nil, err
	}
	return nil, errors.New("h2: request failed after connection retry")
}

func (p *H2Pool) acquire(ctx context.Context, poolKey, key, addr, proxyURL string, profile *tlsfingerprint.Profile) (*h2Conn, error) {
	p.mu.Lock()
	if c := p.conns[poolKey]; c != nil {
		if c.CanTake() {
			p.mu.Unlock()
			return c, nil
		}
		// Full or dying — dial a second conn rather than queueing; hyper does
		// the same when max streams is hit.
	}
	p.mu.Unlock()

	opts := p.opts
	opts.TLSProfile = profile
	c, err := dialH2(ctx, addr, proxyURL, opts)
	if err != nil {
		return nil, err
	}
	// Per-connection evidence of the negotiated transport; poolKey embeds the
	// proxy URL (possibly with credentials) so log key/host separately.
	slog.Info("codex_h2_dial", "key", key, "host", addr, "alpn", "h2")
	p.mu.Lock()
	// Someone may have won the race; use the existing conn if it has room.
	if existing := p.conns[poolKey]; existing != nil && existing.CanTake() {
		p.mu.Unlock()
		c.Close()
		return existing, nil
	}
	c.onDead = func(dead *h2Conn) { p.evict(poolKey, dead) }
	p.conns[poolKey] = c
	p.mu.Unlock()
	return c, nil
}
