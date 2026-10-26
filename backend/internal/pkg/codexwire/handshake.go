package codexwire

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// WSUpgradeOffer is the Sec-WebSocket-Extensions offer emitted by the official
// Codex client (tokio-tungstenite DeflateConfig::default()).
const WSUpgradeOffer = "permessage-deflate; client_max_window_bits"

// wsUpgradeGeneratedOrder is the tungstenite-generated header block that always
// precedes application headers on the wire.
var wsUpgradeGeneratedNames = []HeaderKV{
	{Name: "Connection", Value: "Upgrade"},
	{Name: "Upgrade", Value: "websocket"},
	{Name: "Sec-WebSocket-Version", Value: "13"},
}

// BuildWSUpgradeRequest renders the full HTTP/1.1 upgrade request exactly as
// the official client emits it: request line, generated headers, application
// headers in codex order, then the extension offer last (tungstenite appends it
// at serialization time). secKey may be empty to generate a fresh nonce.
func BuildWSUpgradeRequest(wsURL string, ordered []HeaderKV, secKey string) ([]byte, string, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, "", fmt.Errorf("parse ws url: %w", err)
	}
	host := u.Host
	if host == "" {
		return nil, "", errors.New("ws url missing host")
	}
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	if secKey == "" {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, "", err
		}
		secKey = base64.StdEncoding.EncodeToString(nonce[:])
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "GET %s HTTP/1.1\r\nHost: %s\r\n", path, host)
	for _, kv := range wsUpgradeGeneratedNames {
		fmt.Fprintf(&buf, "%s: %s\r\n", kv.Name, kv.Value)
	}
	fmt.Fprintf(&buf, "Sec-WebSocket-Key: %s\r\n", secKey)
	for _, kv := range ordered {
		if strings.ContainsAny(kv.Name, "\r\n:") || strings.ContainsAny(kv.Value, "\r\n") {
			return nil, "", fmt.Errorf("invalid header %q", kv.Name)
		}
		fmt.Fprintf(&buf, "%s: %s\r\n", kv.Name, kv.Value)
	}
	fmt.Fprintf(&buf, "sec-websocket-extensions: %s\r\n\r\n", WSUpgradeOffer)
	return buf.Bytes(), secKey, nil
}

// UpgradeResult carries the parsed 101 (or failure) response.
type UpgradeResult struct {
	StatusCode int
	Headers    http.Header
	// PerMessageDeflate reports whether the server accepted the extension.
	PerMessageDeflate bool
	// ServerNoContextTakeover disables our decompressor's sliding window.
	ServerNoContextTakeover bool
	// ClientNoContextTakeover disables our compressor's sliding window.
	ClientNoContextTakeover bool
	// ClientMaxWindowBits is the negotiated compressor LZ77 window (8-15,
	// 0 = negotiated default 15). RFC 7692 client_max_window_bits.
	ClientMaxWindowBits int
	// ServerMaxWindowBits is the negotiated decompressor window hint.
	ServerMaxWindowBits int
	// Body carries the non-101 response body (bounded).
	Body []byte
}

// ReadUpgradeResponse reads the server's handshake reply from br. On a 101 the
// extension negotiation is parsed; on any other status the error body is
// captured (bounded) for diagnostics.
func ReadUpgradeResponse(br *bufio.Reader, maxBodyBytes int64) (*UpgradeResult, error) {
	if maxBodyBytes <= 0 {
		maxBodyBytes = 1 << 20
	}
	response, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		return nil, fmt.Errorf("read upgrade response: %w", err)
	}
	res := &UpgradeResult{StatusCode: response.StatusCode, Headers: response.Header}
	if response.StatusCode == http.StatusSwitchingProtocols {
		res.PerMessageDeflate, res.ServerNoContextTakeover, res.ClientNoContextTakeover,
			res.ClientMaxWindowBits, res.ServerMaxWindowBits =
			parseExtensionNegotiation(res.Headers.Get("Sec-WebSocket-Extensions"))
		return res, nil
	}
	// Honor Content-Length/chunked framing. Waiting for socket EOF on a rejected
	// persistent HTTP connection would consume the entire handshake timeout.
	body, _ := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes))
	res.Body = body
	return res, nil
}

// parseExtensionNegotiation interprets the server's Sec-WebSocket-Extensions
// accept value for permessage-deflate parameters.
func parseExtensionNegotiation(value string) (pmd, serverNoTakeover, clientNoTakeover bool, clientBits, serverBits int) {
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		params := strings.Split(part, ";")
		if strings.ToLower(strings.TrimSpace(params[0])) != "permessage-deflate" {
			continue
		}
		pmd = true
		for _, p := range params[1:] {
			kv := strings.SplitN(p, "=", 2)
			key := strings.ToLower(strings.TrimSpace(kv[0]))
			switch key {
			case "server_no_context_takeover":
				serverNoTakeover = true
			case "client_no_context_takeover":
				clientNoTakeover = true
			case "client_max_window_bits":
				if len(kv) == 2 {
					clientBits = parsePMDWindowBits(kv[1])
				}
			case "server_max_window_bits":
				if len(kv) == 2 {
					serverBits = parsePMDWindowBits(kv[1])
				}
			}
		}
	}
	return pmd, serverNoTakeover, clientNoTakeover, clientBits, serverBits
}

// parsePMDWindowBits parses an RFC 7692 *_max_window_bits value; out-of-range
// values yield 0 (treated as the 15-bit default).
func parsePMDWindowBits(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 8 || n > 15 {
		return 0
	}
	return n
}
