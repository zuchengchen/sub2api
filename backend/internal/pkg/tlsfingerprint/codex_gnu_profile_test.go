package tlsfingerprint

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	utls "github.com/refraction-networking/utls"
	"github.com/stretchr/testify/require"
	"io"
	"net"
	"slices"
	"testing"
	"time"
)

func captureGNUProfileRecord(t *testing.T, p *Profile) []byte {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	got := make(chan []byte, 1)
	serverErr := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		raw, err := readRecord(bufio.NewReader(c))
		got <- raw
		serverErr <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	base := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, ln.Addr().String())
	}
	go func() {
		c, err := NewDialer(p, base).DialTLSContext(ctx, "tcp", "localhost:443")
		if c != nil {
			_ = c.Close()
		}
		done <- err
	}()
	raw := <-got
	require.NoError(t, <-serverErr)
	require.Error(t, <-done, "capture-only peer ends after hello")
	return raw
}

// Complete record framing, not substring matching, establishes extension evidence.
func gnuProfileExtensionPayloads(raw []byte) (map[uint16][]byte, error) {
	if len(raw) < 9 || raw[0] != 22 || raw[5] != 1 || int(binary.BigEndian.Uint16(raw[3:5])) != len(raw)-5 || (int(raw[6])<<16|int(raw[7])<<8|int(raw[8])) != len(raw)-9 {
		return nil, fmt.Errorf("invalid ClientHello")
	}
	body := raw[9:]
	take := func(n int) ([]byte, error) {
		if n < 0 || n > len(body) {
			return nil, io.ErrUnexpectedEOF
		}
		v := body[:n]
		body = body[n:]
		return v, nil
	}
	if _, err := take(34); err != nil {
		return nil, err
	}
	var extensions []byte
	for i, width := range []int{1, 2, 1, 2} {
		v, err := take(width)
		if err != nil {
			return nil, err
		}
		n := int(v[0])
		if width == 2 {
			n = int(binary.BigEndian.Uint16(v))
		}
		v, err = take(n)
		if err != nil {
			return nil, err
		}
		if i == 3 {
			extensions = v
		}
	}
	if len(body) != 0 {
		return nil, fmt.Errorf("trailing ClientHello")
	}
	out := map[uint16][]byte{}
	for len(extensions) > 0 {
		if len(extensions) < 4 {
			return nil, io.ErrUnexpectedEOF
		}
		kind, n := binary.BigEndian.Uint16(extensions), int(binary.BigEndian.Uint16(extensions[2:]))
		if n > len(extensions)-4 {
			return nil, io.ErrUnexpectedEOF
		}
		if _, ok := out[kind]; ok {
			return nil, fmt.Errorf("duplicate extension")
		}
		out[kind] = append([]byte(nil), extensions[4:4+n]...)
		extensions = extensions[4+n:]
	}
	return out, nil
}

func TestCodexOpenSSL35GNUActualClientHello(t *testing.T) {
	// Fixed bfdbf01 host-GNU OpenSSL3.5.5 zlib+zstd capture: extension11
	// payload03000102 and extension27 payload0400010003 in seven native hellos.
	// Other shape fields match the separately pinned static-native profile.
	p := BuiltinProfile("openssl35-gnu")
	require.NotNil(t, p)
	require.False(t, p.InsecureSkipVerify)
	require.Nil(t, p.RootCAs)
	require.True(t, p.H1)
	require.False(t, p.H2)
	raw := captureGNUProfileRecord(t, p)
	extensions, err := gnuProfileExtensionPayloads(raw)
	require.NoError(t, err)
	require.Equal(t, []byte{3, 0, 1, 2}, extensions[11])
	require.Equal(t, []byte{4, 0, 1, 0, 3}, extensions[27])
	ours, err := parseHelloForTest(raw)
	require.NoError(t, err)
	expected := pickGolden(t, loadGoldenHellos(t, "0.164.0", "tls_clienthellos.json"), "openssl", false)
	expected.PointFormats = []uint8{0, 1, 2}
	expected.Extensions = append(expected.Extensions, 27)
	assertHelloMatchesGolden(t, ours, expected, true)
	var typed *utls.UtlsCompressCertExtension
	for _, extension := range buildClientHelloSpecFromProfile(p).Extensions {
		if value, ok := extension.(*utls.UtlsCompressCertExtension); ok {
			typed = value
		}
	}
	require.NotNil(t, typed, "must register real uTLS certificate decoders")
	require.Equal(t, []utls.CertCompressionAlgo{utls.CertCompressionZlib, utls.CertCompressionZstd}, typed.Algorithms)
	for _, name := range []string{"openssl36", "rustls", "rustls-h2"} {
		t.Run("unchanged_"+name, func(t *testing.T) {
			old := BuiltinProfile(name)
			require.Empty(t, old.CertCompressionAlgorithms)
			raw := captureGNUProfileRecord(t, old)
			ext, err := gnuProfileExtensionPayloads(raw)
			require.NoError(t, err)
			require.Equal(t, []byte{1, 0}, ext[11])
			require.NotContains(t, ext, uint16(27))
			require.NotEqual(t, p.Name, old.Name)
		})
	}
}

func TestCodexOpenSSL35GNUCaptureRejectsMalformedEvidence(t *testing.T) {
	raw := captureGNUProfileRecord(t, BuiltinProfile("openssl35-gnu"))
	for _, size := range []int{0, 4, 8, 43, len(raw) - 1} {
		_, err := gnuProfileExtensionPayloads(raw[:size])
		require.Error(t, err)
	}
	_, err := gnuProfileExtensionPayloads(append(append([]byte(nil), raw...), 0))
	require.Error(t, err)
	collision := append([]byte(nil), raw...)
	copy(collision[11:20], []byte{0, 27, 0, 5, 4, 0, 1, 0, 3})
	ext, err := gnuProfileExtensionPayloads(collision)
	require.NoError(t, err)
	require.Equal(t, []byte{4, 0, 1, 0, 3}, ext[27], "lookalike random bytes are not another extension")
	duplicate := append([]byte(nil), raw...)
	index := bytes.Index(duplicate[50:], []byte{0, 27, 0, 5, 4, 0, 1, 0, 3})
	require.NotEqual(t, -1, index)
	binary.BigEndian.PutUint16(duplicate[50+index:], 11)
	_, err = gnuProfileExtensionPayloads(duplicate)
	require.Error(t, err)
}

func TestCodexOpenSSL35GNURejectsUnsupportedCompressionBeforeHello(t *testing.T) {
	for _, fault := range []string{"unknown", "duplicate algorithm", "missing algorithm", "missing extension", "duplicate extension"} {
		t.Run(fault, func(t *testing.T) {
			p := BuiltinProfile("openssl35-gnu")
			switch fault {
			case "unknown":
				p.CertCompressionAlgorithms = []uint16{4}
			case "duplicate algorithm":
				p.CertCompressionAlgorithms = []uint16{1, 1}
			case "missing algorithm":
				p.CertCompressionAlgorithms = nil
			case "missing extension":
				p.Extensions = slices.DeleteFunc(p.Extensions, func(v uint16) bool { return v == 27 })
			case "duplicate extension":
				p.Extensions = append(p.Extensions, 27)
			}
			client, server := net.Pipe()
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			c, err := performTLSHandshake(ctx, client, p, "localhost:443")
			require.Error(t, err)
			require.Nil(t, c)
			raw, err := io.ReadAll(server)
			require.NoError(t, err)
			require.Empty(t, raw)
		})
	}
}

func TestCodexOpenSSL35GNUValidatesCertificatesAndNegotiation(t *testing.T) {
	cert := selfSignedLocalhost(t)
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	require.NoError(t, err)
	for _, test := range []struct {
		name    string
		version uint16
		group   tls.CurveID
		trusted bool
		host    string
	}{
		{"tls13 hybrid", tls.VersionTLS13, tls.X25519MLKEM768, true, "localhost"}, {"tls13 x25519", tls.VersionTLS13, tls.X25519, true, "localhost"}, {"tls13 retry p256", tls.VersionTLS13, tls.CurveP256, true, "localhost"}, {"tls12 p256", tls.VersionTLS12, tls.CurveP256, true, "localhost"}, {"unknown root", tls.VersionTLS13, tls.X25519, false, "localhost"}, {"wrong hostname", tls.VersionTLS13, tls.X25519, true, "wrong.invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: test.version, MaxVersion: test.version, CurvePreferences: []tls.CurveID{test.group}})
			require.NoError(t, err)
			defer ln.Close()
			done := make(chan error, 1)
			go func() {
				c, err := ln.Accept()
				if err != nil {
					done <- err
					return
				}
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(3 * time.Second))
				_, err = c.Write([]byte("ok"))
				done <- err
			}()
			p := BuiltinProfile("openssl35-gnu")
			p.RootCAs = x509.NewCertPool()
			if test.trusted {
				p.RootCAs.AddCert(parsed)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			base := func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, ln.Addr().String())
			}
			c, err := NewDialer(p, base).DialTLSContext(ctx, "tcp", net.JoinHostPort(test.host, "443"))
			if !test.trusted || test.host != "localhost" {
				require.Error(t, err)
				require.Nil(t, c)
				require.Error(t, <-done)
				return
			}
			require.NoError(t, err)
			defer c.Close()
			raw := make([]byte, 2)
			_, err = io.ReadFull(c, raw)
			require.NoError(t, err)
			require.Equal(t, "ok", string(raw))
			require.NoError(t, <-done)
			state := c.(*utls.UConn).ConnectionState()
			require.Equal(t, test.version, state.Version)
			require.Empty(t, state.NegotiatedProtocol)
		})
	}
}
