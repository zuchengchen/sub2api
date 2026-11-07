package repository

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func TestCodexOwnedH2BodyRetiresNormallyOrAborts(t *testing.T) {
	for _, mode := range []string{"eof", "exact_body", "early_close", "cancel", "cancel_after_eof", "gzip_eof", "gzip_without_end_stream", "deflate_without_end_stream"} {
		t.Run(mode, func(t *testing.T) {
			template := httptest.NewTLSServer(nil)
			certificate, root := template.TLS.Certificates[0], template.Certificate()
			template.Close()
			listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{certificate}, NextProtos: []string{"h2"}})
			require.NoError(t, err)
			defer listener.Close()
			observed := make(chan []*http2.GoAwayFrame, 1)
			go func() {
				var goaways []*http2.GoAwayFrame
				defer func() { observed <- goaways }()
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
				preface := make([]byte, len(http2.ClientPreface))
				if _, err = io.ReadFull(conn, preface); err != nil {
					return
				}
				if string(preface) != http2.ClientPreface {
					return
				}
				fr := http2.NewFramer(conn, conn)
				fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
				if fr.WriteSettings() != nil {
					return
				}
				for {
					frame, err := fr.ReadFrame()
					if err != nil {
						return
					}
					switch f := frame.(type) {
					case *http2.SettingsFrame:
						if !f.IsAck() {
							if fr.WriteSettingsAck() != nil {
								return
							}
						}
					case *http2.DataFrame:
						if f.StreamEnded() {
							payload, headers := []byte("complete"), []byte{0x88}
							encoding := ""
							if strings.HasPrefix(mode, "gzip_") {
								encoding = "gzip"
								var compressed bytes.Buffer
								writer := gzip.NewWriter(&compressed)
								text := "complete"
								if mode == "gzip_without_end_stream" {
									text += "complete"
								}
								_, _ = writer.Write([]byte(text))
								_ = writer.Close()
								payload = compressed.Bytes()
							} else if mode == "deflate_without_end_stream" {
								encoding = "deflate"
								var compressed bytes.Buffer
								writer, _ := flate.NewWriter(&compressed, flate.DefaultCompression)
								_, _ = writer.Write(payload)
								_ = writer.Close()
								payload = compressed.Bytes()
							}
							if encoding != "" {
								var block bytes.Buffer
								encoder := hpack.NewEncoder(&block)
								_ = encoder.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
								_ = encoder.WriteField(hpack.HeaderField{Name: "content-encoding", Value: encoding})
								headers = block.Bytes()
							}
							if fr.WriteHeaders(http2.HeadersFrameParam{StreamID: f.StreamID, BlockFragment: headers, EndHeaders: true}) != nil {
								return
							}
							complete := mode == "eof" || mode == "exact_body" || mode == "cancel_after_eof" || mode == "gzip_eof"
							if fr.WriteData(f.StreamID, complete, payload) != nil {
								return
							}
						}
					case *http2.GoAwayFrame:
						goaways = append(goaways, f)
					}
				}
			}()
			profile := *tlsfingerprint.BuiltinProfile("rustls-h2")
			profile.RootCAs = x509.NewCertPool()
			profile.RootCAs.AddCert(root)
			ctx, cancel := context.WithCancel(service.WithCodexBusinessHTTP(context.Background(), true))
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "POST", "https://"+listener.Addr().String()+"/backend-api/codex/realtime/calls", strings.NewReader(`{"sdp":"synthetic"}`))
			require.NoError(t, err)
			upstream := &httpUpstreamService{}
			response, err := upstream.doCodexH2Uncontrolled(req, "", 91, &profile)
			require.NoError(t, err)
			switch mode {
			case "eof", "cancel_after_eof", "gzip_eof", "deflate_without_end_stream":
				body, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.Equal(t, "complete", string(body))
				if mode == "cancel_after_eof" {
					cancel()
				}
				if mode == "deflate_without_end_stream" {
					owned := response.Body.(*codexOwnedH2Body)
					require.True(t, owned.eof.Load(), "the decompressor can finish before HTTP/2 framing")
					require.False(t, owned.complete(), "decoded EOF must not replace remote END_STREAM")
				}
			case "exact_body", "gzip_without_end_stream":
				body := make([]byte, len("complete"))
				_, err := io.ReadFull(response.Body, body)
				require.NoError(t, err)
				require.Equal(t, "complete", string(body))
			case "cancel":
				done := make(chan error, 1)
				go func() { _, err := io.ReadAll(response.Body); done <- err }()
				cancel()
				select {
				case err := <-done:
					require.Error(t, err)
				case <-time.After(time.Second):
					t.Fatal("cancellation did not unblock read")
				}
			}
			require.NoError(t, response.Body.Close())
			require.NoError(t, response.Body.Close())
			select {
			case rows := <-observed:
				if mode == "eof" || mode == "exact_body" || mode == "gzip_eof" {
					require.Len(t, rows, 1)
					require.Equal(t, http2.ErrCodeNo, rows[0].ErrCode)
					require.Zero(t, rows[0].LastStreamID)
					require.EqualValues(t, 8, rows[0].Length)
				} else {
					require.Empty(t, rows, "error/early close must not claim normal shutdown")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("client retirement did not finish")
			}
		})
	}
}
