package repository

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// This driver intentionally uses only APIs present on the original baseline.
// One operation is two complete real loopback quota GETs on one private req
// client. conn/op exposes changed keep-alive behavior independently of cost;
// this is a local HTTP transport measurement, not a WAN/TLS latency claim.
func newCodexAuxiliaryQuotaHTTP1Pair(t testing.TB) (func(), *atomic.Int64, *atomic.Int64, func()) {
	t.Setenv(codexDeviceClientDisabledEnv, "")
	t.Setenv("CODEX_CA_CERTIFICATE", "")
	t.Setenv("SSL_CERT_FILE", "")
	var requests, connections atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || (r.URL.Path != "/backend-api/wham/usage" && r.URL.Path != "/backend-api/wham/rate-limit-reset-credits") {
			http.Error(w, "wrong request", http.StatusBadRequest)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"available_count":1,"complete":true}`)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	srv.Start()
	client, err := newCodexDeviceReqClient("", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	requestPair := func() {
		for _, path := range []string{"/backend-api/wham/usage", "/backend-api/wham/rate-limit-reset-credits"} {
			response, err := client.R().SetHeader("Authorization", "Bearer synthetic-benchmark").SetHeader("Chatgpt-Account-Id", "account-benchmark").SetHeader("User-Agent", "codex-benchmark").Get(srv.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK || response.String() != `{"available_count":1,"complete":true}` {
				t.Fatal(fmt.Errorf("incomplete quota response: status=%d body_bytes=%d", response.StatusCode, len(response.Bytes())))
			}
		}
	}
	return requestPair, &requests, &connections, srv.Close
}

func BenchmarkCodexAuxiliaryQuotaHTTP1Pair(b *testing.B) {
	requestPair, requests, connections, cleanup := newCodexAuxiliaryQuotaHTTP1Pair(b)
	defer cleanup()
	requestPair()
	beforeRequests, beforeConnections := requests.Load(), connections.Load()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		requestPair()
	}
	b.StopTimer()
	if got := requests.Load() - beforeRequests; got != int64(2*b.N) {
		b.Fatalf("requests=%d, want %d", got, 2*b.N)
	}
	b.ReportMetric(float64(connections.Load()-beforeConnections)/float64(b.N), "conn/op")
}

func TestCodexAuxiliaryQuotaHTTP1PairFixture(t *testing.T) {
	pair, requests, connections, cleanup := newCodexAuxiliaryQuotaHTTP1Pair(t)
	defer cleanup()
	pair()
	if requests.Load() != 2 || connections.Load() == 0 {
		t.Fatal("fixture did not perform two real loopback GETs")
	}
}
