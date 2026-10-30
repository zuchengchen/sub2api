package repository

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Each iteration sends and fully consumes a real local TLS/HTTP2 request on
// the production Codex upstream transport; setup/handshake is excluded.
func BenchmarkCodexH2Transport(b *testing.B) {
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(previousLogger)
	var requests atomic.Int64
	var wrongProtocol atomic.Bool
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			wrongProtocol.Store(true)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	profile := *tlsfingerprint.BuiltinProfile("rustls-h2")
	// Benchmark-only local trust: both baseline and candidate use the same
	// existing flag. Certificate verification is covered by separate tests.
	profile.InsecureSkipVerify = true
	upstream := &httpUpstreamService{}
	defer upstream.codexH2().CloseAll()
	body := `{"model":"gpt-5.5","input":[{"role":"user","content":"benchmark real transport"}],"stream":true}`
	send := func() {
		req, err := http.NewRequestWithContext(context.Background(), "POST", server.URL+"/backend-api/codex/responses", strings.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer synthetic-benchmark")
		req.Header.Set("User-Agent", "codex_exec/0.166.0")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		resp, err := upstream.doCodexH2Uncontrolled(req, "", 987, &profile)
		if err != nil {
			b.Fatal(err)
		}
		n, err := io.Copy(io.Discard, resp.Body)
		closeErr := resp.Body.Close()
		if err != nil || closeErr != nil || resp.StatusCode != 200 || n == 0 {
			b.Fatalf("incomplete request: %v %v %d", err, closeErr, n)
		}
	}
	send()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		send()
	}
	b.StopTimer()
	if wrongProtocol.Load() || requests.Load() != int64(b.N+1) {
		b.Fatal("benchmark did not execute the expected HTTP2 work")
	}
}
