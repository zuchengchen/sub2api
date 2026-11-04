package codexwire

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// H1Pool is a client-owned pool for auxiliary Codex clients. Responses and web
// callers continue to use DoHTTP1's fresh-connection contract. Keys must identify
// one effective credential snapshot; no key or credential is logged here.
type H1Pool struct {
	opts      H1PoolOptions
	mu        sync.Mutex
	closed    bool
	idle      map[string][]*h1PooledConn
	all       map[*h1PooledConn]struct{}
	idleCount int
}

type H1PoolOptions struct {
	IdleTimeout   time.Duration
	MaxLifetime   time.Duration
	MaxIdleConns  int
	MaxIdlePerKey int
}

type h1PooledConn struct {
	nc         net.Conn
	br         *bufio.Reader
	key        string
	created    time.Time
	idle       bool // protected by pool.mu
	timer      *time.Timer
	generation uint64
}

func NewH1Pool(opts H1PoolOptions) *H1Pool {
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 90 * time.Second
	}
	if opts.MaxLifetime <= 0 {
		opts.MaxLifetime = 10 * time.Minute
	}
	if opts.MaxIdleConns <= 0 {
		opts.MaxIdleConns = 16
	}
	if opts.MaxIdlePerKey <= 0 {
		opts.MaxIdlePerKey = 2
	}
	return &H1Pool{opts: opts, idle: make(map[string][]*h1PooledConn), all: make(map[*h1PooledConn]struct{})}
}

func (p *H1Pool) CloseAll() {
	p.mu.Lock()
	p.closed = true
	conns := make([]*h1PooledConn, 0, len(p.all))
	for c := range p.all {
		if c.timer != nil {
			c.timer.Stop()
		}
		conns = append(conns, c)
	}
	p.all = make(map[*h1PooledConn]struct{})
	p.idle = make(map[string][]*h1PooledConn)
	p.idleCount = 0
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.nc.Close()
	}
}

func (p *H1Pool) removeLocked(c *h1PooledConn) {
	c.generation++
	delete(p.all, c)
	if c.timer != nil {
		c.timer.Stop()
	}
	if !c.idle {
		return
	}
	c.idle = false
	items := p.idle[c.key]
	for i, item := range items {
		if item == c {
			items = append(items[:i], items[i+1:]...)
			p.idleCount--
			break
		}
	}
	if len(items) == 0 {
		delete(p.idle, c.key)
	} else {
		p.idle[c.key] = items
	}
}

func (p *H1Pool) discard(c *h1PooledConn) {
	p.mu.Lock()
	p.removeLocked(c)
	p.mu.Unlock()
	_ = c.nc.Close()
}

func (p *H1Pool) release(c *h1PooledConn) {
	p.mu.Lock()
	_, known := p.all[c]
	if !known || p.closed || time.Since(c.created) >= p.opts.MaxLifetime || p.idleCount >= p.opts.MaxIdleConns || len(p.idle[c.key]) >= p.opts.MaxIdlePerKey {
		p.removeLocked(c)
		p.mu.Unlock()
		_ = c.nc.Close()
		return
	}
	c.idle = true
	c.generation++
	generation := c.generation
	p.idle[c.key] = append(p.idle[c.key], c)
	p.idleCount++
	ttl := p.opts.IdleTimeout
	if remaining := p.opts.MaxLifetime - time.Since(c.created); remaining < ttl {
		ttl = remaining
	}
	c.timer = time.AfterFunc(ttl, func() {
		p.mu.Lock()
		if !c.idle || c.generation != generation {
			p.mu.Unlock()
			return
		}
		p.removeLocked(c)
		p.mu.Unlock()
		_ = c.nc.Close()
	})
	p.mu.Unlock()
}

func (p *H1Pool) acquire(ctx context.Context, key, scheme, addr, proxyURL string, opts HTTP1Options) (*h1PooledConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrClosed
	}
	items := p.idle[key]
	if len(items) > 0 {
		c := items[len(items)-1]
		p.removeLocked(c)
		p.all[c] = struct{}{}
		p.mu.Unlock()
		if time.Since(c.created) >= p.opts.MaxLifetime {
			p.discard(c)
		} else {
			return c, nil
		}
	} else {
		p.mu.Unlock()
	}
	nc, err := dialHTTP1(ctx, scheme, addr, proxyURL, opts)
	if err != nil {
		return nil, err
	}
	c := &h1PooledConn{nc: nc, br: bufio.NewReader(nc), key: key, created: time.Now()}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = nc.Close()
		return nil, ErrClosed
	}
	p.all[c] = struct{}{}
	p.mu.Unlock()
	return c, nil
}

// Do sends exactly once. A stale socket or uncertain write is reported to the
// caller, never automatically replayed (including OAuth codes and consume POSTs).
func (p *H1Pool) Do(ctx context.Context, method, rawURL string, ordered []HeaderKV, body []byte, proxyURL, key string, opts HTTP1Options) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("invalid auxiliary HTTP URL")
	}
	addr := u.Host
	if u.Port() == "" {
		port := "443"
		if u.Scheme == "http" {
			port = "80"
		}
		addr = net.JoinHostPort(u.Hostname(), port)
	}
	partition := key + "|" + proxyURL + "|" + u.Scheme + "|" + u.Host
	if opts.TLSProfile != nil {
		partition += "|" + opts.TLSProfile.Name + "|" + opts.TLSProfile.RootCAID
	}
	wire, err := buildHTTP1RequestBytes(method, u.RequestURI(), u.Host, ordered, body)
	if err != nil {
		return nil, err
	}
	c, err := p.acquire(ctx, partition, u.Scheme, addr, proxyURL, opts)
	if err != nil {
		return nil, err
	}
	cancelDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { p.discard(c); close(cancelDone) })
	var stopOnce sync.Once
	stopCancel := func() {
		stopOnce.Do(func() {
			if !stop() {
				<-cancelDone
			}
		})
	}
	fail := func(err error) (*http.Response, error) {
		stopCancel()
		p.discard(c)
		if cause := ctx.Err(); cause != nil {
			return nil, cause
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if opts.ResponseHeaderTimeout > 0 {
		_ = c.nc.SetDeadline(time.Now().Add(opts.ResponseHeaderTimeout))
	}
	if n, err := c.nc.Write(wire); err != nil {
		return fail(err)
	} else if n != len(wire) {
		return fail(io.ErrShortWrite)
	}
	resp, err := http.ReadResponse(c.br, &http.Request{Method: method})
	if err != nil {
		return fail(fmt.Errorf("read response: %w", err))
	}
	_ = c.nc.SetDeadline(time.Time{})
	reusable := !resp.Close && (resp.ContentLength >= 0 || (len(resp.TransferEncoding) == 1 && strings.EqualFold(resp.TransferEncoding[0], "chunked")))
	b := &h1PooledBody{body: resp.Body, pool: p, conn: c, stopCancel: stopCancel, reusable: reusable}
	if resp.Body == http.NoBody || resp.ContentLength == 0 {
		b.stopCancel()
		b.eof.Store(true)
	}
	resp.Body = b
	return resp, nil
}

type h1PooledBody struct {
	body       io.ReadCloser
	pool       *H1Pool
	conn       *h1PooledConn
	stopCancel func()
	reusable   bool
	eof        atomic.Bool
	failed     atomic.Bool
	once       sync.Once
	err        error
}

func (b *h1PooledBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if err == io.EOF {
		// The wire response is complete. Request cleanup may now cancel its
		// context without invalidating an idle-capable transport, like net/http.
		// The socket remains leased until Close, so another request cannot start
		// while the caller still owns this body.
		b.stopCancel()
		b.eof.Store(true)
	} else if err != nil {
		b.failed.Store(true)
	}
	return n, err
}

func (b *h1PooledBody) Close() error {
	b.once.Do(func() {
		b.stopCancel()
		reuse := b.reusable && b.eof.Load() && !b.failed.Load() && b.conn.br.Buffered() == 0
		if !reuse {
			b.pool.discard(b.conn)
			// Closing the socket intentionally interrupts any remaining body.
			// Its drain error is not a new failure of the caller's Close.
			_ = b.body.Close()
			return
		}
		b.err = b.body.Close()
		if reuse && b.err == nil {
			b.pool.release(b.conn)
		} else {
			b.pool.discard(b.conn)
		}
	})
	return b.err
}
