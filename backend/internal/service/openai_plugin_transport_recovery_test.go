//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type openAIRecoveryUpstream struct {
	HTTPUpstream
	do func(*http.Request, string, int64, int) (*http.Response, error)
}

func (u *openAIRecoveryUpstream) Do(request *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	return u.do(request, proxy, id, concurrency)
}

type openAIRecoveryBody struct {
	io.Reader
	closed bool
}

func (b *openAIRecoveryBody) Close() error {
	b.closed = true
	return nil
}

func TestOpenAIUpstream_401ReloadThenRefreshWithoutMutatingRequest(t *testing.T) {
	account, repo, executor, provider := newOpenAIRecoveryFixture()
	firstBody := &openAIRecoveryBody{Reader: strings.NewReader(`{"error":"unauthorized"}`)}
	var calls int
	var requests []*http.Request
	upstream := &openAIRecoveryUpstream{do: func(request *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
		calls++
		requests = append(requests, request)
		require.Equal(t, account.ID, id)
		require.Equal(t, account.Concurrency, concurrency)
		require.Equal(t, "http://same-proxy", proxy)
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		require.JSONEq(t, `{"model":"gpt-5.4","input":"hello"}`, string(body))
		_ = request.Body.Close()
		if calls <= 2 {
			require.Equal(t, "Bearer rejected-access", request.Header.Get("Authorization"))
			return &http.Response{StatusCode: 401, Header: make(http.Header), Body: firstBody}, nil
		}
		require.True(t, firstBody.closed)
		require.Equal(t, "Bearer repaired-access", request.Header.Get("Authorization"))
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: done\n\n"))}, nil
	}}
	svc := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream, openAITokenProvider: provider}
	request, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hello"}`))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer rejected-access")
	response, err := svc.doOpenAIUpstream(request, "http://same-proxy", account)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, 200, response.StatusCode)
	require.Equal(t, 3, calls)
	require.NotSame(t, requests[0], requests[1])
	require.Equal(t, "Bearer rejected-access", request.Header.Get("Authorization"))
	require.Equal(t, int32(1), executor.calls.Load())
}

func TestOpenAIUpstream_ChangedReloadUsesSecondRejectedToken(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		account, repo, executor, provider := newOpenAIRecoveryFixture()
		credentials := shallowCopyMap(repo.account.Credentials)
		credentials["access_token"] = "reloaded-access"
		require.NoError(t, repo.UpdateCredentials(context.Background(), account.ID, credentials))
		executor.refresh = func(_ context.Context, latest *Account) (map[string]any, error) {
			require.Equal(t, "reloaded-access", latest.GetOpenAIAccessToken())
			updated := shallowCopyMap(latest.Credentials)
			updated["access_token"] = "repaired-access"
			return updated, nil
		}
		var sent []string
		upstream := &openAIRecoveryUpstream{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
			sent = append(sent, req.Header.Get("Authorization"))
			status := 200
			if len(sent) == 1 || (refresh && len(sent) == 2) {
				status = 401
			}
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("reply"))}, nil
		}}
		svc := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream, openAITokenProvider: provider}
		req, err := http.NewRequest(http.MethodPost, chatgptCodexURL, strings.NewReader(`{}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer rejected-access")
		resp, err := svc.doOpenAIUpstream(req, "", account)
		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode)
		resp.Body.Close()
		want := []string{"Bearer rejected-access", "Bearer reloaded-access"}
		if refresh {
			want = append(want, "Bearer repaired-access")
			require.Equal(t, int32(1), executor.calls.Load())
		} else {
			require.Zero(t, executor.calls.Load())
		}
		require.Equal(t, want, sent)
	}
}

func TestOpenAIUpstream_GuardedReloadFailureDoesNotReplay(t *testing.T) {
	for _, kind := range []string{"missing_identity", "changed_identity", "read_failure", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			account, repo, executor, provider := newOpenAIRecoveryFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "missing_identity":
				delete(account.Credentials, "chatgpt_account_id")
			case "changed_identity":
				repo.account.Credentials["chatgpt_account_id"] = "another-account"
			case "read_failure":
				repo.readErr = errors.New("synthetic read failure")
			}
			calls := 0
			upstream := &openAIRecoveryUpstream{do: func(*http.Request, string, int64, int) (*http.Response, error) {
				calls++
				if kind == "canceled" {
					cancel()
				}
				return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("original"))}, nil
			}}
			svc := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream, openAITokenProvider: provider}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexURL, strings.NewReader(`{}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer rejected-access")
			resp, err := svc.doOpenAIUpstream(req, "", account)
			require.NoError(t, err)
			require.Equal(t, 401, resp.StatusCode)
			resp.Body.Close()
			require.Equal(t, 1, calls)
			require.Zero(t, executor.calls.Load())
		})
	}
}

func TestOpenAIUpstream_Third401StopsRefreshAndPreservesResponse(t *testing.T) {
	account, repo, executor, provider := newOpenAIRecoveryFixture()
	calls := 0
	upstream := &openAIRecoveryUpstream{do: func(*http.Request, string, int64, int) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"still unauthorized"}`))}, nil
	}}
	svc := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream, openAITokenProvider: provider}
	request, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader(`{}`))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer rejected-access")
	response, err := svc.doOpenAIUpstream(request, "", account)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, 401, response.StatusCode)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "still unauthorized")
	require.Equal(t, 3, calls)
	_, err = provider.RefreshAfterUnauthorized(context.Background(), account, "repaired-access")
	require.Error(t, err)
	require.Equal(t, int32(1), executor.calls.Load())
	require.Equal(t, "rotated-refresh", repo.account.GetOpenAIRefreshToken())
}

func TestOpenAIUpstream_RefreshFailureRetainsReload401(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		t.Run(map[bool]string{false: "transient", true: "permanent"}[permanent], func(t *testing.T) {
			account, repo, executor, provider := newOpenAIRecoveryFixture()
			executor.refresh = func(context.Context, *Account) (map[string]any, error) {
				if permanent {
					return nil, openai.NewRefreshTokenError(401, []byte("secret"))
				}
				return nil, errors.New("network failure secret")
			}
			body := &openAIRecoveryBody{Reader: strings.NewReader("original-401")}
			calls := 0
			upstream := &openAIRecoveryUpstream{do: func(*http.Request, string, int64, int) (*http.Response, error) {
				calls++
				body = &openAIRecoveryBody{Reader: strings.NewReader("reload-401")}
				return &http.Response{StatusCode: 401, Header: make(http.Header), Body: body}, nil
			}}
			svc := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream, openAITokenProvider: provider}
			request, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader(`{}`))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer rejected-access")
			response, err := svc.doOpenAIUpstream(request, "", account)
			require.NoError(t, err)
			require.Equal(t, 401, response.StatusCode)
			require.False(t, body.closed)
			defer response.Body.Close()
			require.Equal(t, 2, calls)
			require.Zero(t, repo.setError)
		})
	}
}

func TestOpenAIUpstream_DoesNotReplaySuccessOrNonRewindableBodies(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized} {
		account, repo, executor, provider := newOpenAIRecoveryFixture()
		calls := 0
		body := &openAIRecoveryBody{Reader: strings.NewReader("stream-output")}
		upstream := &openAIRecoveryUpstream{do: func(*http.Request, string, int64, int) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
		}}
		svc := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream, openAITokenProvider: provider}
		request, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", io.NopCloser(strings.NewReader(`{}`)))
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer rejected-access")
		response, err := svc.doOpenAIUpstream(request, "", account)
		require.NoError(t, err)
		require.Equal(t, status, response.StatusCode)
		require.False(t, body.closed)
		_ = response.Body.Close()
		require.Equal(t, 1, calls)
		require.Zero(t, executor.calls.Load())
	}
}

func TestCodexProbeUnauthorizedDoesNotRecordBusinessAuthFailure(t *testing.T) {
	account, repo, executor, provider := newOpenAIRecoveryFixture()
	upstream := &codexProbeUpstream{httpUpstreamRecorder: httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"unauthorized"}`)),
	}}}
	svc := &OpenAIGatewayService{cfg: codexProbeConfig(nil), accountRepo: repo, httpUpstream: upstream, openAITokenProvider: provider}
	req, err := http.NewRequest(http.MethodPost, chatgptCodexURL, strings.NewReader(`{"model":"gpt-5.5","input":[]}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer rejected-access")
	resp, err := svc.doOpenAICodexProbe(req, "", account)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Len(t, upstream.requests, 1)
	require.Zero(t, executor.calls.Load())
	require.Zero(t, repo.setError)
	require.Nil(t, provider.refreshAPI.openAIRefreshFailure(account))
}
