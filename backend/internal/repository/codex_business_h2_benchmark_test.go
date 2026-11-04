package repository

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// For baseline compilation overlay only the pure context helper alongside this
// file. Baseline ignores the marker; candidate consumes it at the real sender.
// The mock peer reads actual TLS/H2 HEADERS/DATA, allocates a fresh HPACK decoder
// on every connection, and checks the first stream is 1. This exposes changed
// connection/HPACK epochs instead of hiding fresh-client cost behind a warm pool.
func newCodexBusinessH2BenchmarkPair(t testing.TB) (func(), *atomic.Int64, *atomic.Int64, *atomic.Int64, func()) {
	template := httptest.NewTLSServer(nil)
	certificate, serverCertificate := template.TLS.Certificates[0], template.Certificate()
	template.Close()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{certificate}, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	var connections, epochs, requests atomic.Int64
	var invalid atomic.Bool
	var mu sync.Mutex
	conns := make(map[net.Conn]struct{})
	var workers sync.WaitGroup
	wantRequest := []byte(`{"model":"gpt-5.5","input":"local benchmark","stream":false}`)
	wantResponse := "complete benchmark response"
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			mu.Lock()
			conns[conn] = struct{}{}
			mu.Unlock()
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() { _ = conn.Close(); mu.Lock(); delete(conns, conn); mu.Unlock() }()
				preface := make([]byte, len(http2.ClientPreface))
				if _, err := io.ReadFull(conn, preface); err != nil || string(preface) != http2.ClientPreface {
					invalid.Store(true)
					return
				}
				fr := http2.NewFramer(conn, conn)
				fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
				if err := fr.WriteSettings(); err != nil {
					return
				}
				// Decode the real connection-local HPACK state, including dynamic
				// references on baseline's later streams, and count stream-1 epochs.
				first := true
				bodies := map[uint32][]byte{}
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
					case *http2.MetaHeadersFrame:
						if first {
							if f.StreamID != 1 {
								invalid.Store(true)
							}
							epochs.Add(1)
							first = false
						}
						bodies[f.StreamID] = nil
						if f.PseudoValue("method") != "POST" || f.PseudoValue("path") != "/backend-api/codex/responses" {
							invalid.Store(true)
						}
					case *http2.DataFrame:
						bodies[f.StreamID] = append(bodies[f.StreamID], f.Data()...)
						if len(f.Data()) > 0 {
							if fr.WriteWindowUpdate(0, uint32(len(f.Data()))) != nil {
								return
							}
						}
						if !f.StreamEnded() {
							continue
						}
						if !bytes.Equal(bodies[f.StreamID], wantRequest) {
							invalid.Store(true)
						}
						delete(bodies, f.StreamID)
						requests.Add(1)
						if fr.WriteHeaders(http2.HeadersFrameParam{StreamID: f.StreamID, BlockFragment: []byte{0x88}, EndHeaders: true}) != nil {
							return
						}
						if fr.WriteData(f.StreamID, true, []byte(wantResponse)) != nil {
							return
						}
					}
				}
			}()
		}
	}()
	profile := *tlsfingerprint.BuiltinProfile("rustls-h2")
	profile.RootCAs = x509.NewCertPool()
	profile.RootCAs.AddCert(serverCertificate)
	profile.RootCAID = "benchmark-local-root"
	upstream := &httpUpstreamService{}
	pair := func() {
		for i := 0; i < 2; i++ {
			ctx := service.WithCodexBusinessHTTP(context.Background(), true)
			req, err := http.NewRequestWithContext(ctx, "POST", "https://"+listener.Addr().String()+"/backend-api/codex/responses", strings.NewReader(string(wantRequest)))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer synthetic-benchmark")
			resp, err := upstream.doCodexH2Uncontrolled(req, "", 91, &profile)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if err != nil || closeErr != nil || resp.ProtoMajor != 2 || string(body) != wantResponse {
				t.Fatalf("incomplete H2 benchmark response: proto=%d body=%d error=%v close=%v", resp.ProtoMajor, len(body), err, closeErr)
			}
		}
		if invalid.Load() {
			t.Fatal("benchmark peer observed an invalid request or HPACK epoch")
		}
	}
	cleanup := func() {
		upstream.codexH2().CloseAll()
		_ = listener.Close()
		mu.Lock()
		for conn := range conns {
			_ = conn.Close()
		}
		mu.Unlock()
		workers.Wait()
	}
	return pair, &connections, &epochs, &requests, cleanup
}

func BenchmarkCodexBusinessH2ClientPair(b *testing.B) {
	pair, connections, epochs, requests, cleanup := newCodexBusinessH2BenchmarkPair(b)
	defer cleanup()
	pair()
	beforeConn, beforeEpoch, beforeRequests := connections.Load(), epochs.Load(), requests.Load()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pair()
	}
	b.StopTimer()
	if requests.Load()-beforeRequests != int64(2*b.N) {
		b.Fatal("benchmark did not complete two requests per operation")
	}
	b.ReportMetric(float64(connections.Load()-beforeConn)/float64(b.N), "conn/op")
	b.ReportMetric(float64(epochs.Load()-beforeEpoch)/float64(b.N), "hpack_epochs/op")
}

func TestCodexBusinessH2BenchmarkPairFixture(t *testing.T) {
	pair, connections, epochs, requests, cleanup := newCodexBusinessH2BenchmarkPair(t)
	defer cleanup()
	pair()
	if requests.Load() != 2 || connections.Load() == 0 || epochs.Load() != connections.Load() {
		t.Fatal("fixture did not complete two requests with consistent connection epochs")
	}
}
