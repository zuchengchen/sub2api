package repository

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// Codex 0.166's captured GET /codex/models sends reqwest's Accept default
// without a Content-Length. The Responses template must not be reused for it.
func TestCodexH2ModelsAndResponsesHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body, accept, wantAccept string
		wantLength                                   bool
	}{
		{"models default", http.MethodGet, "/backend-api/codex/models", "", "", "*/*", false},
		{"models explicit accept", http.MethodGet, "/backend-api/codex/models", "", "application/json", "application/json", false},
		{"responses body", http.MethodPost, "/backend-api/codex/responses", `{"model":"gpt-5.5","stream":true}`, "text/event-stream", "text/event-stream", true},
		{"responses empty body", http.MethodPost, "/backend-api/codex/responses", "", "text/event-stream", "text/event-stream", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type observedRequest struct {
				proto  int
				header http.Header
				body   string
				err    error
			}
			observed := make(chan observedRequest, 1)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				observed <- observedRequest{r.ProtoMajor, r.Header.Clone(), string(body), err}
				_, _ = io.WriteString(w, "ok")
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			t.Cleanup(server.Close)

			profile := *tlsfingerprint.BuiltinProfile("rustls-h2")
			profile.InsecureSkipVerify = true
			upstream := &httpUpstreamService{}
			t.Cleanup(func() { upstream.codexH2().CloseAll() })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, tc.method, server.URL+tc.path, strings.NewReader(tc.body))
			require.NoError(t, err)
			if tc.accept != "" {
				request.Header.Set("Accept", tc.accept)
			}
			request.Header.Set("Authorization", "Bearer fake-token")
			request.Header.Set("User-Agent", "codex_exec/0.166.0")
			response, err := upstream.doCodexH2Uncontrolled(request, "", 31, &profile)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, "ok", string(body))
			got := <-observed
			require.NoError(t, got.err)
			require.Equal(t, 2, got.proto, "the real HTTP/2 transport must run")
			require.Equal(t, tc.body, got.body)
			require.Equal(t, tc.wantAccept, got.header.Get("Accept"))
			if tc.wantLength {
				require.Equal(t, strconv.Itoa(len(tc.body)), got.header.Get("Content-Length"))
			} else {
				require.NotContains(t, got.header, "Content-Length")
			}
		})
	}
}
