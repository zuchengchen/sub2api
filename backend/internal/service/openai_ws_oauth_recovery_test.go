//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIWSOAuthRecovery_LocalHandshakeRefreshOnce(t *testing.T) {
	for _, rejectsReplacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "refresh_then_upgrade", true: "refresh_then_401"}[rejectsReplacement], func(t *testing.T) {
			account, repo, executor, provider := newOpenAIRecoveryFixture()
			var calls atomic.Int32
			seen := make(chan string, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				seen <- r.Header.Get("Authorization")
				if r.Header.Get("Authorization") != "Bearer repaired-access" || rejectsReplacement {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				defer cancel()
				kind, payload, err := conn.Read(ctx)
				if err != nil {
					return
				}
				_ = conn.Write(ctx, kind, payload)
				_, _, _ = conn.Read(ctx)
			}))
			defer server.Close()
			cfg := &config.Config{}
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			pool := newOpenAIWSConnPool(cfg)
			defer pool.Close()
			svc := &OpenAIGatewayService{accountRepo: repo, openAITokenProvider: provider}
			headers := make(http.Header)
			headers.Set("Authorization", "Bearer rejected-access")
			request := openAIWSAcquireRequest{Account: account, WSURL: "ws" + strings.TrimPrefix(server.URL, "http") + "/responses", Headers: headers, HeadersFactory: func(ctx context.Context, headers http.Header) (http.Header, *Account, error) {
				return svc.refreshOpenAIWSAuthHeaders(ctx, account, headers)
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			lease, err := svc.acquireOpenAIWSWithAuthRecovery(ctx, pool, &request)
			require.Equal(t, int32(3), calls.Load(), "expected original, guarded reload and refreshed handshake: %v", err)
			require.Equal(t, "Bearer rejected-access", <-seen)
			require.Equal(t, "Bearer rejected-access", <-seen)
			require.Equal(t, "Bearer repaired-access", <-seen)
			require.Equal(t, "Bearer rejected-access", headers.Get("Authorization"), "original headers must remain immutable")
			require.Equal(t, "Bearer repaired-access", request.Headers.Get("Authorization"))
			require.Equal(t, int32(1), executor.calls.Load())
			if rejectsReplacement {
				var dialErr *openAIWSDialError
				require.ErrorAs(t, err, &dialErr)
				require.Equal(t, http.StatusUnauthorized, dialErr.StatusCode)
				require.Nil(t, lease)
				_, err = svc.acquireOpenAIWSWithAuthRecovery(ctx, pool, &request)
				require.Error(t, err)
				require.Equal(t, int32(3), calls.Load(), "rejected credential version must not redial")
				require.Equal(t, int32(1), executor.calls.Load())
				return
			}
			require.NoError(t, err)
			defer lease.Release()
			require.NoError(t, lease.WriteJSON(map[string]string{"type": "response.create", "input": "hello"}, time.Second))
			payload, err := lease.ReadMessage(time.Second)
			require.NoError(t, err)
			require.JSONEq(t, `{"type":"response.create","input":"hello"}`, string(payload))
			lease.MarkBroken()
			lease.Release()
			// A later dial with stale caller headers must use the durable replacement.
			fresh, _, err := svc.refreshOpenAIWSAuthHeaders(ctx, account, headers)
			require.NoError(t, err)
			require.Equal(t, "Bearer repaired-access", fresh.Get("Authorization"))
			require.Zero(t, repo.setError)
		})
	}
}

type openAIRecoveryLocalDialer struct {
	url string
}

func TestOpenAIWSOAuthRecovery_VerifiedAccess401UsesDialCredentials(t *testing.T) {
	for _, mode := range []string{"static_headers", "static_rotated_credentials", "factory_rotated_credentials"} {
		t.Run(mode, func(t *testing.T) {
			account, repo, executor, provider := newOpenAIRecoveryFixture()
			svc := &OpenAIGatewayService{accountRepo: repo, openAITokenProvider: provider}
			current := snapshotOAuthRefreshAccount(account)
			req := openAIWSAcquireRequest{Account: account, WSURL: "wss://fixture.invalid/responses", Headers: make(http.Header)}
			req.Headers.Set("Authorization", "Bearer "+account.GetOpenAIAccessToken())
			if mode != "static_headers" {
				current.Credentials["access_token"] = "fresh-dial-access"
				require.NoError(t, repo.UpdateCredentials(context.Background(), current.ID, current.Credentials))
			}
			if mode == "static_rotated_credentials" {
				req.Headers.Set("Authorization", "Bearer "+current.GetOpenAIAccessToken())
			}
			if mode == "factory_rotated_credentials" {
				req.HeadersFactory = func(ctx context.Context, h http.Header) (http.Header, *Account, error) {
					return svc.refreshOpenAIWSAuthHeaders(ctx, account, h)
				}
			}
			verifyOpenAITransportTestAccess(t, provider, current)
			calls := 0
			pool := oauthReliabilityEdgePool(t)
			pool.setClientDialerForTest(&oauthReliabilityEdgePoolDialer{dial: func(_ context.Context, h http.Header) (openAIWSClientConn, int, http.Header, error) {
				calls++
				require.Equal(t, "Bearer "+current.GetOpenAIAccessToken(), h.Get("Authorization"))
				return nil, http.StatusUnauthorized, nil, errors.New("unauthorized")
			}})
			lease, err := svc.acquireOpenAIWSWithAuthRecovery(context.Background(), pool, &req)
			require.Nil(t, lease)
			var dialErr *openAIWSDialError
			require.ErrorAs(t, err, &dialErr)
			require.Equal(t, http.StatusUnauthorized, dialErr.StatusCode)
			require.Equal(t, 1, calls)
			require.Zero(t, executor.calls.Load())
			require.False(t, provider.refreshAPI.openAIRejectionForAccount(current).accessVerifiedAt(current, time.Now()))
		})
	}
}

func TestOpenAIWSOAuthRecovery_PassthroughRevokesVerifiedAccessOnFirst401(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account, repo, executor, provider := newOpenAIRecoveryFixture()
	account.Extra = astraHealthyTestExtra(map[string]any{"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough})
	verifyOpenAITransportTestAccess(t, provider, account)
	cfg := passthroughLifecycleConfig()
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	svc := newPassthroughLifecycleService(cfg, nil)
	svc.accountRepo = repo
	svc.openAITokenProvider = provider
	var calls atomic.Int32
	svc.openaiWSPassthroughDialer = &oauthReliabilityEdgePoolDialer{dial: func(_ context.Context, h http.Header) (openAIWSClientConn, int, http.Header, error) {
		calls.Add(1)
		if h.Get("Authorization") != "Bearer "+account.GetOpenAIAccessToken() {
			return nil, 0, nil, errors.New("unexpected handshake authentication")
		}
		return nil, http.StatusUnauthorized, nil, errors.New("unauthorized")
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	downstream, serverErr := startPassthroughLifecycleServer(t, ctx, svc, account)
	defer downstream.Close()
	client := dialPassthroughLifecycleClient(t, downstream)
	defer client.CloseNow()
	select {
	case err := <-serverErr:
		require.Error(t, err)
	case <-ctx.Done():
		t.Fatal("passthrough auth rejection did not terminate")
	}
	require.Equal(t, int32(1), calls.Load())
	require.Zero(t, executor.calls.Load())
	require.False(t, provider.refreshAPI.openAIRejectionForAccount(account).accessVerifiedAt(account, time.Now()))
}

func (d *openAIRecoveryLocalDialer) Dial(ctx context.Context, _ string, headers http.Header, proxy string) (openAIWSClientConn, int, http.Header, error) {
	return newDefaultOpenAIWSClientDialer().Dial(ctx, d.url, headers, proxy)
}

func TestOpenAIWSOAuthRecovery_PassthroughLocalHandshake(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, rejectsReplacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "refresh_then_stream", true: "refresh_then_401"}[rejectsReplacement], func(t *testing.T) {
			account, repo, executor, provider := newOpenAIRecoveryFixture()
			account.Extra = astraHealthyTestExtra(map[string]any{"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough})
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer repaired-access" || rejectsReplacement {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				defer cancel()
				if _, _, err := conn.Read(ctx); err != nil {
					return
				}
				_ = conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_recovered","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`))
				_, _, _ = conn.Read(ctx)
			}))
			defer upstream.Close()
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.OpenAIWS.OAuthEnabled = true
			svc := newPassthroughLifecycleService(cfg, nil)
			svc.accountRepo = repo
			svc.openAITokenProvider = provider
			svc.openaiWSPassthroughDialer = &openAIRecoveryLocalDialer{url: "ws" + strings.TrimPrefix(upstream.URL, "http") + "/responses"}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			downstream, serverErr := startPassthroughLifecycleServer(t, ctx, svc, account)
			defer downstream.Close()
			client := dialPassthroughLifecycleClient(t, downstream)
			defer client.CloseNow()
			payload, err := readPassthroughLifecycleFrame(t, client, 4*time.Second)
			if rejectsReplacement {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Contains(t, string(payload), "resp_recovered")
			}
			_ = client.CloseNow()
			select {
			case <-serverErr:
			case <-ctx.Done():
				t.Fatal("passthrough auth recovery did not terminate")
			}
			require.Equal(t, int32(3), calls.Load())
			require.Equal(t, int32(1), executor.calls.Load())
			require.Zero(t, repo.setError)
			if rejectsReplacement {
				_, err := provider.RefreshAfterUnauthorized(context.Background(), account, "repaired-access")
				require.Error(t, err)
				require.Equal(t, int32(1), executor.calls.Load())
			}
		})
	}
}
