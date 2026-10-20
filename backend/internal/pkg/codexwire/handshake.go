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
	"net/textproto"
	"net/url"
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
	buf.WriteString("GET ")
	buf.WriteString(path)
	buf.WriteString(" HTTP/1.1\r\nHost: ")
	buf.WriteString(host)
	buf.WriteString("\r\n")
	for _, kv := range wsUpgradeGeneratedNames {
		buf.WriteString(kv.Name)
		buf.WriteString(": ")
		buf.WriteString(kv.Value)
		buf.WriteString("\r\n")
	}
	buf.WriteString("Sec-WebSocket-Key: ")
	buf.WriteString(secKey)
	buf.WriteString("\r\n")
	for _, kv := range ordered {
		if strings.ContainsAny(kv.Name, "\r\n:") || strings.ContainsAny(kv.Value, "\r\n") {
			return nil, "", fmt.Errorf("invalid header %q", kv.Name)
		}
		buf.WriteString(kv.Name)
		buf.WriteString(": ")
		buf.WriteString(kv.Value)
		buf.WriteString("\r\n")
	}
	buf.WriteString("sec-websocket-extensions: ")
	buf.WriteString(WSUpgradeOffer)
	buf.WriteString("\r\n\r\n")
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
	tp := textproto.NewReader(br)
	statusLine, err := tp.ReadLine()
	if err != nil {
		return nil, fmt.Errorf("read status line: %w", err)
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 ") && !strings.HasPrefix(statusLine, "HTTP/1.0 ") {
		return nil, fmt.Errorf("malformed status line %q", statusLine)
	}
	var statusCode int
	if _, err := fmt.Sscanf(statusLine[len("HTTP/1.1 "):], "%d", &statusCode); err != nil {
		return nil, fmt.Errorf("parse status line %q: %w", statusLine, err)
	}
	mimeHeaders, err := tp.ReadMIMEHeader()
	if err != nil {
		return nil, fmt.Errorf("read response headers: %w", err)
	}
	res := &UpgradeResult{StatusCode: statusCode, Headers: http.Header(mimeHeaders)}
	if statusCode == http.StatusSwitchingProtocols {
		res.PerMessageDeflate, res.ServerNoContextTakeover, res.ClientNoContextTakeover =
			parseExtensionNegotiation(res.Headers.Get("Sec-WebSocket-Extensions"))
		return res, nil
	}
	body, _ := io.ReadAll(io.LimitReader(br, maxBodyBytes))
	res.Body = body
	return res, nil
}

// parseExtensionNegotiation interprets the server's Sec-WebSocket-Extensions
// accept value for permessage-deflate parameters.
func parseExtensionNegotiation(value string) (pmd, serverNoTakeover, clientNoTakeover bool) {
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
			switch strings.ToLower(strings.TrimSpace(strings.SplitN(p, "=", 2)[0])) {
			case "server_no_context_takeover":
				serverNoTakeover = true
			case "client_no_context_takeover":
				clientNoTakeover = true
			}
		}
	}
	return pmd, serverNoTakeover, clientNoTakeover
}
