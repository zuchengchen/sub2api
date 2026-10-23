package tlsfingerprint

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// goldenHello mirrors backend/tools/codexgolden clientHello JSON.
type goldenHello struct {
	Lane              string   `json:"lane"`
	RecordVersion     uint16   `json:"record_version"`
	LegacyVersion     uint16   `json:"legacy_version"`
	SessionIDLen      int      `json:"session_id_len"`
	CipherSuites      []uint16 `json:"cipher_suites"`
	Compression       []uint8  `json:"compression_methods"`
	Extensions        []uint16 `json:"extensions"`
	ServerName        string   `json:"server_name"`
	ALPN              []string `json:"alpn"`
	SupportedGroups   []uint16 `json:"supported_groups"`
	SignatureSchemes  []uint16 `json:"signature_algorithms"`
	SupportedVersions []uint16 `json:"supported_versions"`
	PSKModes          []uint8  `json:"psk_key_exchange_modes"`
	PointFormats      []uint8  `json:"ec_point_formats"`
	KeyShares         []struct {
		Group  uint16 `json:"group"`
		KeyLen int    `json:"key_len"`
	} `json:"key_shares"`
	HybridReusesX25519 *bool `json:"hybrid_reuses_x25519"`
	StatusRequest      bool  `json:"status_request"`
}

const goldenDir = "../codexwire/testdata/golden/0.163.0"

func loadGoldenHellos(t *testing.T, file string) []goldenHello {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(goldenDir, file))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var out []goldenHello
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	return out
}

// pickGolden returns the golden hello for lane whose ALPN presence matches.
func pickGolden(t *testing.T, hellos []goldenHello, lane string, alpn bool) goldenHello {
	t.Helper()
	for _, h := range hellos {
		if h.Lane == lane && (len(h.ALPN) > 0) == alpn {
			return h
		}
	}
	t.Fatalf("golden has no %s hello with alpn=%v", lane, alpn)
	return goldenHello{}
}

// captureOurHello dials a local listener with profile and parses the first
// TLS record our dialer sends.
func captureOurHello(t *testing.T, profile *Profile) goldenHello {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	got := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- nil
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		rec, _ := readRecord(bufio.NewReader(c))
		got <- rec
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	base := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, ln.Addr().String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		_, _ = NewDialer(profile, base).DialTLSContext(ctx, "tcp", net.JoinHostPort("localhost", port))
	}()
	rec := <-got
	if rec == nil {
		t.Fatal("no ClientHello captured")
	}
	h, err := parseHelloForTest(rec)
	if err != nil {
		t.Fatalf("parse our hello: %v", err)
	}
	return h
}

func assertHelloMatchesGolden(t *testing.T, ours, golden goldenHello, orderedExtensions bool) {
	t.Helper()
	eq := func(name string, a, b any) {
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		if string(ja) != string(jb) {
			t.Errorf("%s mismatch:\n ours   %s\n golden %s", name, ja, jb)
		}
	}
	eq("record_version", ours.RecordVersion, golden.RecordVersion)
	eq("legacy_version", ours.LegacyVersion, golden.LegacyVersion)
	eq("session_id_len", ours.SessionIDLen, golden.SessionIDLen)
	eq("cipher_suites", ours.CipherSuites, golden.CipherSuites)
	eq("compression_methods", ours.Compression, golden.Compression)
	if orderedExtensions {
		eq("extensions (ordered)", ours.Extensions, golden.Extensions)
	} else {
		a, b := slices.Clone(ours.Extensions), slices.Clone(golden.Extensions)
		slices.Sort(a)
		slices.Sort(b)
		eq("extensions (set)", a, b)
	}
	eq("server_name", ours.ServerName, golden.ServerName)
	eq("alpn", ours.ALPN, golden.ALPN)
	eq("supported_groups", ours.SupportedGroups, golden.SupportedGroups)
	eq("signature_algorithms", ours.SignatureSchemes, golden.SignatureSchemes)
	eq("supported_versions", ours.SupportedVersions, golden.SupportedVersions)
	eq("psk_modes", ours.PSKModes, golden.PSKModes)
	eq("ec_point_formats", ours.PointFormats, golden.PointFormats)
	eq("key_shares", ours.KeyShares, golden.KeyShares)
	eq("status_request", ours.StatusRequest, golden.StatusRequest)
	eq("hybrid_reuses_x25519", ours.HybridReusesX25519, golden.HybridReusesX25519)
}

func TestCodexGolden_OpenSSLHTTPLane(t *testing.T) {
	golden := pickGolden(t, loadGoldenHellos(t, "tls_clienthellos.json"), "openssl", false)
	ours := captureOurHello(t, BuiltinProfile("openssl36"))
	assertHelloMatchesGolden(t, ours, golden, true)
}

func TestCodexGolden_RustlsWSLane(t *testing.T) {
	golden := pickGolden(t, loadGoldenHellos(t, "tls_clienthellos.json"), "rustls", false)
	for i := 0; i < 5; i++ { // shuffled order: check several handshakes
		ours := captureOurHello(t, BuiltinProfile("rustls"))
		assertHelloMatchesGolden(t, ours, golden, false)
	}
}

func TestCodexGolden_RustlsCustomCAHTTPLane(t *testing.T) {
	golden := pickGolden(t, loadGoldenHellos(t, "tls_clienthellos_custom_ca.json"), "rustls", true)
	// The custom-CA capture's first connection carries session_ticket(35);
	// later resumption attempts add pre_shared_key(41). Compare to a fresh one.
	if !slices.Contains(golden.Extensions, 35) || slices.Contains(golden.Extensions, 41) {
		t.Fatalf("unexpected golden extension set %v", golden.Extensions)
	}
	ours := captureOurHello(t, BuiltinProfile("rustls-h2"))
	assertHelloMatchesGolden(t, ours, golden, false)
}

// TestHybridKeyShareHandshake checks that the prefilled hybrid/X25519 shares
// complete real handshakes whichever group the server picks, including an HRR.
func TestHybridKeyShareHandshake(t *testing.T) {
	cert := selfSignedLocalhost(t)
	for name, curves := range map[string][]tls.CurveID{
		"server_prefers_hybrid": {tls.X25519MLKEM768, tls.X25519},
		"server_x25519_only":    {tls.X25519},
		"server_p256_only_hrr":  {tls.CurveP256},
	} {
		t.Run(name, func(t *testing.T) {
			for _, profileName := range []string{"rustls", "openssl36"} {
				ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, CurvePreferences: curves})
				if err != nil {
					t.Fatal(err)
				}
				errc := make(chan error, 1)
				go func() {
					c, err := ln.Accept()
					if err != nil {
						errc <- err
						return
					}
					defer func() { _ = c.Close() }()
					tlsConn, ok := c.(*tls.Conn)
					if !ok {
						errc <- fmt.Errorf("accepted conn is %T, want *tls.Conn", c)
						return
					}
					if err := tlsConn.Handshake(); err != nil {
						errc <- err
						return
					}
					_, err = c.Write([]byte("ok"))
					errc <- err
				}()
				profile := *BuiltinProfile(profileName)
				profile.InsecureSkipVerify = true
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				conn, err := NewDialer(&profile, nil).DialTLSContext(ctx, "tcp", ln.Addr().String())
				cancel()
				if err != nil {
					_ = ln.Close()
					t.Fatalf("%s handshake: %v", profileName, err)
				}
				buf := make([]byte, 2)
				if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ok" {
					t.Fatalf("%s read: %q %v", profileName, buf, err)
				}
				_ = conn.Close()
				if err := <-errc; err != nil {
					t.Fatalf("%s server: %v", profileName, err)
				}
				_ = ln.Close()
			}
		})
	}
}

func selfSignedLocalhost(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func readRecord(br *bufio.Reader) ([]byte, error) {
	head := make([]byte, 5)
	if _, err := io.ReadFull(br, head); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(head[3:5]))
	body := make([]byte, n)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, err
	}
	return append(head, body...), nil
}

// parseHelloForTest is a compact ClientHello parser (kept in sync with
// tools/codexgolden/clienthello.go).
func parseHelloForTest(rec []byte) (goldenHello, error) {
	var h goldenHello
	if len(rec) < 9 || rec[0] != 0x16 || rec[5] != 0x01 {
		return h, errors.New("not a ClientHello record")
	}
	h.RecordVersion = binary.BigEndian.Uint16(rec[1:3])
	b := rec[9:]
	take := func(n int) []byte {
		if len(b) < n {
			b = nil
			return nil
		}
		v := b[:n]
		b = b[n:]
		return v
	}
	u16 := func(p []byte) uint16 { return binary.BigEndian.Uint16(p) }
	h.LegacyVersion = u16(take(2))
	take(32)
	h.SessionIDLen = int(take(1)[0])
	take(h.SessionIDLen)
	cs := take(int(u16(take(2))))
	for len(cs) >= 2 {
		h.CipherSuites = append(h.CipherSuites, u16(cs))
		cs = cs[2:]
	}
	h.Compression = append([]uint8(nil), take(int(take(1)[0]))...)
	exts := take(int(u16(take(2))))
	var hybridTail, plain []byte
	for len(exts) >= 4 {
		typ, n := u16(exts), int(u16(exts[2:]))
		data := exts[4 : 4+n]
		exts = exts[4+n:]
		h.Extensions = append(h.Extensions, typ)
		switch typ {
		case 0:
			h.ServerName = string(data[5:])
		case 5:
			h.StatusRequest = true
		case 10:
			for l := data[2:]; len(l) >= 2; l = l[2:] {
				h.SupportedGroups = append(h.SupportedGroups, u16(l))
			}
		case 11:
			h.PointFormats = append([]uint8(nil), data[1:]...)
		case 13:
			for l := data[2:]; len(l) >= 2; l = l[2:] {
				h.SignatureSchemes = append(h.SignatureSchemes, u16(l))
			}
		case 16:
			for l := data[2:]; len(l) > 0; {
				n := int(l[0])
				h.ALPN = append(h.ALPN, string(l[1:1+n]))
				l = l[1+n:]
			}
		case 43:
			for l := data[1:]; len(l) >= 2; l = l[2:] {
				h.SupportedVersions = append(h.SupportedVersions, u16(l))
			}
		case 45:
			h.PSKModes = append([]uint8(nil), data[1:]...)
		case 51:
			for l := data[2:]; len(l) >= 4; {
				g, n := u16(l), int(u16(l[2:]))
				key := l[4 : 4+n]
				h.KeyShares = append(h.KeyShares, struct {
					Group  uint16 `json:"group"`
					KeyLen int    `json:"key_len"`
				}{g, n})
				if g == 0x11ec && n == 1184+32 {
					hybridTail = key[1184:]
				}
				if g == 0x001d {
					plain = key
				}
				l = l[4+n:]
			}
		}
	}
	if hybridTail != nil && plain != nil {
		reused := string(hybridTail) == string(plain)
		h.HybridReusesX25519 = &reused
	}
	return h, nil
}

// The CONNECT request must match hyper-util's tunnel connector: no
// User-Agent (Go's http.Request.Write adds Go-http-client/1.1).
func TestBuildProxyConnectRequest_NoUserAgent(t *testing.T) {
	u, _ := url.Parse("http://user:pass@proxy.example:8080")
	got := buildProxyConnectRequest("chatgpt.com:443", u, false)
	want := "CONNECT chatgpt.com:443 HTTP/1.1\r\nHost: chatgpt.com:443\r\nProxy-Authorization: Basic dXNlcjpwYXNz\r\n\r\n"
	if got != want {
		t.Fatalf("CONNECT bytes:\n got %q\nwant %q", got, want)
	}
	if got := buildProxyConnectRequest("chatgpt.com:443", &url.URL{Scheme: "http", Host: "p:1"}, false); strings.Contains(got, "User-Agent") || strings.Contains(got, "Proxy-Authorization") {
		t.Fatalf("unexpected headers: %q", got)
	}
}

func TestBuildProxyConnectRequest_WSLaneKeepAlive(t *testing.T) {
	u, _ := url.Parse("http://user:pass@proxy.example:8080")
	want := "CONNECT chatgpt.com:443 HTTP/1.1\r\nHost: chatgpt.com:443\r\nProxy-Connection: Keep-Alive\r\nProxy-Authorization: Basic dXNlcjpwYXNz\r\n\r\n"
	if got := buildProxyConnectRequest("chatgpt.com:443", u, BuiltinProfile("rustls").ProxyConnectKeepAlive); got != want {
		t.Fatalf("ws CONNECT:\n got %q\nwant %q", got, want)
	}
	if BuiltinProfile("openssl36").ProxyConnectKeepAlive || BuiltinProfile("rustls-h2").ProxyConnectKeepAlive {
		t.Fatal("HTTP lanes (hyper-util) send no Proxy-Connection")
	}
}

// End to end: the proxy receives exactly the CONNECT head, without Go's UA.
func TestHTTPProxyDialer_SendsExactConnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- ""
			return
		}
		defer func() { _ = c.Close() }()
		br := bufio.NewReader(c)
		var head strings.Builder
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				break
			}
			_, _ = head.WriteString(line)
			if line == "\r\n" {
				break
			}
		}
		got <- head.String()
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\ncontent-length: 0\r\n\r\n")
	}()
	proxyURL, _ := url.Parse("http://u:p@" + ln.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = NewHTTPProxyDialer(BuiltinProfile("openssl36"), proxyURL).DialTLSContext(ctx, "tcp", "chatgpt.com:443")
	if err == nil {
		t.Fatal("expected CONNECT failure from 403 proxy")
	}
	want := "CONNECT chatgpt.com:443 HTTP/1.1\r\nHost: chatgpt.com:443\r\nProxy-Authorization: Basic dTpw\r\n\r\n"
	if head := <-got; head != want {
		t.Fatalf("proxy saw:\n%q\nwant:\n%q", head, want)
	}
}
