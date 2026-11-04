package tlsfingerprint

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProxyHandshakeCancellationClosesSocket(t *testing.T) {
	for _, scheme := range []string{"http", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer ln.Close()
			accepted := make(chan net.Conn, 1)
			ready := make(chan error, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					ready <- err
					return
				}
				accepted <- conn
				if scheme == "http" {
					_, err = http.ReadRequest(bufio.NewReader(conn))
				} else {
					_, err = io.ReadFull(conn, make([]byte, 3)) // SOCKS5 anonymous greeting
				}
				ready <- err
			}()
			proxyURL, err := url.Parse(scheme + "://" + ln.Addr().String())
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var conn net.Conn
				var err error
				if scheme == "http" {
					conn, err = NewHTTPProxyDialer(BuiltinProfile("openssl"), proxyURL).DialTLSContext(ctx, "tcp", "example.invalid:443")
				} else {
					conn, err = NewSOCKS5ProxyDialer(BuiltinProfile("rustls"), proxyURL).DialTLSContext(ctx, "tcp", "example.invalid:443")
				}
				if conn != nil {
					_ = conn.Close()
				}
				done <- err
			}()
			require.NoError(t, <-ready)
			peer := <-accepted
			defer peer.Close()
			cancel()
			select {
			case err := <-done:
				require.Error(t, err)
			case <-time.After(time.Second):
				_ = peer.Close()
				<-done
				t.Fatal("canceled proxy negotiation kept waiting for the peer")
			}
		})
	}
}

func TestHTTPSProxyAlwaysStartsWithTLS(t *testing.T) {
	for _, profile := range []*Profile{nil, {Name: "other-platform"}, BuiltinProfile("rustls")} {
		name := "nil"
		if profile != nil {
			name = profile.Name
		}
		t.Run(name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer ln.Close()
			firstBytes := make(chan []byte, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					firstBytes <- nil
					return
				}
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(time.Second))
				first := make([]byte, 3)
				_, _ = io.ReadFull(conn, first)
				firstBytes <- first
			}()
			proxyURL, err := url.Parse("https://synthetic:secret@" + ln.Addr().String())
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err = NewHTTPProxyDialer(profile, proxyURL).DialTLSContext(ctx, "tcp", "example.invalid:443")
			require.Error(t, err) // capture peer deliberately ends the TLS handshake
			first := <-firstBytes
			require.Len(t, first, 3)
			require.Equal(t, byte(22), first[0], "HTTPS proxy must never receive plaintext CONNECT or credentials")
			require.Equal(t, byte(3), first[1])
		})
	}
}

func TestShufflingProfileDoesNotMutateDefaultExtensions(t *testing.T) {
	original := slices.Clone(defaultExtensionOrder)
	for i := 0; i < 8; i++ {
		_ = buildClientHelloSpecFromProfile(&Profile{ShuffleExtensions: true})
	}
	require.Equal(t, original, defaultExtensionOrder, "a custom shuffle must not mutate shared defaults")
}
