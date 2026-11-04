package repository

import (
	"context"
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
)

func TestCodexRawHTTPPreservesSuccessfulRequestSnapshot(t *testing.T) {
	for _, lane := range []string{"openssl36", "rustls-h2"} {
		t.Run(lane, func(t *testing.T) {
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Location", "/backend-api/codex/live/rtc_test")
				_, _ = io.WriteString(w, "synthetic-sdp-answer")
			}))
			srv.EnableHTTP2 = true
			srv.StartTLS()
			defer srv.Close()
			profile := *tlsfingerprint.BuiltinProfile(lane)
			profile.RootCAs = x509.NewCertPool()
			profile.RootCAs.AddCert(srv.Certificate())
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			original, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/backend-api/codex/realtime/calls", strings.NewReader(`{"sdp":"synthetic"}`))
			original.Header.Set("Authorization", "Bearer original-synthetic")
			attempt := original.Clone(service.WithCodexBusinessHTTP(ctx, true))
			attempt.Header.Set("Authorization", "Bearer reloaded-synthetic")
			attempt.Header.Set("X-Oai-Attestation", `{"v":1,"s":0,"t":"synthetic"}`)
			attempt.Header.Set("Session-Id", "mapped-session")
			upstream := &httpUpstreamService{}
			defer upstream.codexH2().CloseAll()
			var response *http.Response
			var err error
			if profile.H2 {
				response, err = upstream.doCodexH2Uncontrolled(attempt, "", 71, &profile)
			} else {
				response, err = upstream.doCodexH1Uncontrolled(attempt, "", &profile, service.HTTPUpstreamProfileOpenAI)
			}
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, "synthetic-sdp-answer", string(body))
			require.NoError(t, response.Body.Close())
			require.Same(t, attempt, response.Request, "raw transport must preserve the successful request, not its method-only parser stub")
			require.Equal(t, "Bearer reloaded-synthetic", response.Request.Header.Get("Authorization"))
			require.Equal(t, attempt.Header.Get("X-Oai-Attestation"), response.Request.Header.Get("X-Oai-Attestation"))
			require.Equal(t, "Bearer original-synthetic", original.Header.Get("Authorization"))
		})
	}
}
