//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Only the final transport is redirected: the rate-limit service builds the
// request and uses the real gateway probe preparation/compression path.
type recoveryProbeLoopbackTransport struct {
	target    *url.URL
	client    *http.Client
	calls     atomic.Int32
	last      *http.Request
	profile   *tlsfingerprint.Profile
	proxy     string
	accountID int64
	closed    atomic.Bool
}

type recoveryProbeObservedBody struct {
	io.ReadCloser
	closed *atomic.Bool
}

func (b *recoveryProbeObservedBody) Close() error {
	b.closed.Store(true)
	return b.ReadCloser.Close()
}

func (u *recoveryProbeLoopbackTransport) Do(r *http.Request, proxy string, accountID int64, _ int) (*http.Response, error) {
	u.calls.Add(1)
	u.last, u.proxy, u.accountID = r.Clone(r.Context()), proxy, accountID
	if r.URL.String() != chatgptCodexURL {
		return nil, fmt.Errorf("unexpected probe destination")
	}
	copy := r.Clone(r.Context())
	copy.URL.Scheme, copy.URL.Host = u.target.Scheme, u.target.Host
	response, err := u.client.Do(copy)
	if response != nil && response.Body != nil {
		response.Body = &recoveryProbeObservedBody{ReadCloser: response.Body, closed: &u.closed}
	}
	return response, err
}

func (u *recoveryProbeLoopbackTransport) DoWithTLS(r *http.Request, proxy string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.profile = profile
	return u.Do(r, proxy, accountID, concurrency)
}

func TestOpenAI429RecoveryHTTPUsesRealProbeSenderWithoutAuthReplay(t *testing.T) {
	t.Setenv("CODEX_CA_CERTIFICATE", "")
	t.Setenv("SSL_CERT_FILE", "")
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			account, repo, executor, provider := newOpenAIRecoveryFixture()
			account.Extra = map[string]any{codexFingerprintSeedExtraKey: testCodexFingerprintSeed}
			proxyID := int64(45)
			account.ProxyID, account.Proxy = &proxyID, &Proxy{Protocol: "http", Host: "127.0.0.1", Port: 19000}
			type observed struct {
				method, path, host string
				headers            http.Header
				body               []byte
				err                error
			}
			seen := make(chan observed, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err == nil && r.Header.Get("Content-Encoding") == "zstd" {
					var decoder *zstd.Decoder
					decoder, err = zstd.NewReader(nil)
					if err == nil {
						raw, err = decoder.DecodeAll(raw, nil)
						decoder.Close()
					}
				}
				seen <- observed{r.Method, r.URL.Path, r.Host, r.Header.Clone(), raw, err}
				w.Header().Set("X-Request-Id", "synthetic-recovery-probe")
				w.Header().Set("X-Codex-Primary-Used-Percent", "2")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
			}))
			defer server.Close()
			target, err := url.Parse(server.URL)
			require.NoError(t, err)
			upstream := &recoveryProbeLoopbackTransport{target: target, client: server.Client()}
			gateway := &OpenAIGatewayService{cfg: codexProbeConfig(nil), accountRepo: repo, httpUpstream: upstream, openAITokenProvider: provider}
			svc := &RateLimitService{codexProbeSender: gateway}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result := svc.probeOpenAI429RecoveryHTTP(ctx, account, "gpt-5.5")
			require.NoError(t, result.Err)
			require.Equal(t, status, result.Status)
			require.Equal(t, "synthetic-recovery-probe", result.Headers.Get("X-Request-Id"))
			require.EqualValues(t, 1, upstream.calls.Load(), "diagnostics must not replay rejected credentials")
			require.True(t, upstream.closed.Load(), "probe drains and closes its response")
			require.NotNil(t, upstream.profile, "production Codex transport profile must be selected")
			require.Equal(t, account.ID, upstream.accountID)
			require.Equal(t, "http://127.0.0.1:19000", upstream.proxy)
			row := <-seen
			require.NoError(t, row.err)
			require.Equal(t, http.MethodPost, row.method)
			require.Equal(t, "/backend-api/codex/responses", row.path)
			require.Equal(t, "chatgpt.com", row.host)
			require.Equal(t, "Bearer "+account.GetOpenAIAccessToken(), row.headers.Get("Authorization"))
			require.Equal(t, account.GetChatGPTAccountID(), row.headers.Get("ChatGPT-Account-ID"))
			require.Equal(t, "zstd", row.headers.Get("Content-Encoding"))
			require.Equal(t, "text/event-stream", row.headers.Get("Accept"))
			require.NotEmpty(t, row.headers.Get("session-id"))
			require.NotEmpty(t, row.headers.Get("User-Agent"))
			require.Equal(t, "gpt-5.5", gjson.GetBytes(row.body, "model").String())
			require.True(t, gjson.GetBytes(row.body, "stream").Bool())
			require.False(t, gjson.GetBytes(row.body, "store").Bool())
			require.NotEmpty(t, gjson.GetBytes(row.body, "input").Array())
			require.Zero(t, executor.calls.Load())
			require.Zero(t, repo.setError)
			require.Nil(t, provider.refreshAPI.openAIRefreshFailure(account))
		})
	}
}

func TestOpenAI429RecoveryHTTPCancellationAndAdmission(t *testing.T) {
	t.Setenv("CODEX_CA_CERTIFICATE", "")
	t.Setenv("SSL_CERT_FILE", "")
	arrived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		arrived <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	upstream := &recoveryProbeLoopbackTransport{target: target, client: server.Client()}
	svc := &RateLimitService{codexProbeSender: &OpenAIGatewayService{cfg: codexProbeConfig(nil), httpUpstream: upstream}}
	account := newCodexProbeTestAccount()
	for _, rejected := range []*Account{nil, {Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, {Platform: PlatformOpenAI, Type: AccountTypeOAuth}} {
		result := svc.probeOpenAI429RecoveryHTTP(context.Background(), rejected, "gpt-5.5")
		require.True(t, result.Stop)
		require.Error(t, result.Err)
	}
	require.Zero(t, upstream.calls.Load())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan openai429RecoveryProbeResult, 1)
	go func() { done <- svc.probeOpenAI429RecoveryHTTP(ctx, account, "gpt-5.5") }()
	select {
	case <-arrived:
	case <-ctx.Done():
		t.Fatal("probe did not reach local server")
	}
	cancel()
	select {
	case result := <-done:
		require.Error(t, result.Err)
		require.True(t, strings.Contains(result.Err.Error(), "context canceled"), result.Err.Error())
	case <-time.After(time.Second):
		t.Fatal("cancelled probe did not return")
	}
	require.EqualValues(t, 1, upstream.calls.Load(), "cancellation must not create a replacement request")
}
