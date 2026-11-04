package codexwire

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// DialOptions controls the raw transport dial.
type DialOptions struct {
	// DisableCompression is for protocols such as Codex Live that do not offer
	// permessage-deflate. The default keeps the Responses offer unchanged.
	DisableCompression bool
	// TLSProfile selects a uTLS ClientHello profile; nil falls back to stock
	// crypto/tls.
	TLSProfile *tlsfingerprint.Profile
	// HandshakeTimeout bounds TCP+TLS+101 handshake.
	HandshakeTimeout time.Duration
	// BaseDialer overrides TCP dialing (testing).
	BaseDialer func(ctx context.Context, network, addr string) (net.Conn, error)
}

// tlsConn establishes a TLS connection toward addr honoring proxyURL and the
// optional fingerprint profile. proxyURL supports http(s) and socks5(h).
func tlsConn(ctx context.Context, addr string, proxyURL string, profile *tlsfingerprint.Profile, baseDialer func(ctx context.Context, network, addr string) (net.Conn, error)) (net.Conn, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		if profile != nil {
			return tlsfingerprint.NewDialer(profile, baseDialer).DialTLSContext(ctx, "tcp", addr)
		}
		if baseDialer == nil {
			baseDialer = (&net.Dialer{}).DialContext
		}
		raw, err := baseDialer(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		host, _, _ := net.SplitHostPort(addr)
		tlsConn := tls.Client(raw, &tls.Config{ServerName: host})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, err
		}
		return tlsConn, nil
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy url: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "socks5", "socks5h":
		return tlsfingerprint.NewSOCKS5ProxyDialer(profile, parsed).DialTLSContext(ctx, "tcp", addr)
	case "http", "https":
		return tlsfingerprint.NewHTTPProxyDialer(profile, parsed).DialTLSContext(ctx, "tcp", addr)
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", parsed.Scheme)
	}
}

// DialWS establishes the transport, writes the byte-exact upgrade request and
// validates the 101 response. headers are application headers in wire order.
func DialWS(ctx context.Context, wsURL string, ordered []HeaderKV, proxyURL string, opts DialOptions) (*WSConn, *UpgradeResult, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, nil, fmt.Errorf("parse ws url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	host := u.Host
	if host == "" {
		return nil, nil, errors.New("ws url missing host")
	}
	addr := host
	if u.Port() == "" {
		switch scheme {
		case "wss", "https":
			addr = net.JoinHostPort(host, "443")
		case "ws", "http":
			addr = net.JoinHostPort(host, "80")
		default:
			return nil, nil, fmt.Errorf("unsupported ws scheme %q", scheme)
		}
	}
	dialCtx := ctx
	cancel := func() {}
	if opts.HandshakeTimeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, opts.HandshakeTimeout)
	}
	defer cancel()

	var conn net.Conn
	if scheme == "ws" || scheme == "http" {
		if strings.TrimSpace(proxyURL) != "" {
			return nil, nil, errors.New("raw websocket plaintext proxy is unsupported")
		}
		base := opts.BaseDialer
		if base == nil {
			base = (&net.Dialer{}).DialContext
		}
		conn, err = base(dialCtx, "tcp", addr)
	} else {
		conn, err = tlsConn(dialCtx, addr, proxyURL, opts.TLSProfile, opts.BaseDialer)
	}
	if err != nil {
		return nil, nil, err
	}

	// This watcher owns only the handshake, not the established connection.
	stop := context.AfterFunc(dialCtx, func() { _ = conn.Close() })
	defer stop()
	reqBytes, secKey, err := buildWSUpgradeRequest(wsURL, ordered, "", !opts.DisableCompression)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if deadline, ok := dialCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := conn.Write(reqBytes); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("write upgrade request: %w", err)
	}
	br := bufio.NewReader(conn)
	res, err := ReadUpgradeResponse(br, 1<<20)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	if res.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, res, fmt.Errorf("websocket upgrade failed: status %d", res.StatusCode)
	}
	accept := sha1.Sum([]byte(secKey + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if res.Headers.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(accept[:]) ||
		!strings.EqualFold(res.Headers.Get("Upgrade"), "websocket") ||
		!headerHasToken(res.Headers.Get("Connection"), "upgrade") {
		_ = conn.Close()
		return nil, res, errors.New("invalid websocket upgrade response")
	}
	if opts.DisableCompression && res.PerMessageDeflate {
		_ = conn.Close()
		return nil, res, errors.New("websocket server selected compression without an offer")
	}
	if !stop() && dialCtx.Err() != nil {
		_ = conn.Close()
		return nil, res, dialCtx.Err()
	}
	return NewWSConn(conn, br, res), res, nil
}

func headerHasToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}
