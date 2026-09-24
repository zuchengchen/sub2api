package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func assertCookieRecoverySafe(t *testing.T, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	for _, secret := range []string{"private-user", "private-password", "proxy.invalid", "secret-token", "secret-cookie"} {
		require.NotContains(t, string(encoded), secret)
		require.NotContains(t, fmt.Sprintf("%+v", value), secret)
	}
}

func TestOpenAICookieWSRecoveryTransportClassification(t *testing.T) {
	for _, tc := range []struct {
		name, stage, code string
		err               error
		status            int
	}{
		{"timeout", "http_request", "cookie_ws_http_request_timeout", fmt.Errorf("%s: %w", cookieTestSecret, context.DeadlineExceeded), 0},
		{"DNS", "http_request", "cookie_ws_http_request_dns", &url.Error{Op: "Post", URL: cookieTestSecret, Err: &net.DNSError{Err: cookieTestSecret, Name: "proxy.invalid", IsNotFound: true}}, 0},
		{"proxy auth", "http_request", "cookie_ws_proxy_authentication", errors.New("socks connect: username/password authentication failed " + cookieTestSecret), 0},
		{"proxy connect", "http_request", "cookie_ws_proxy_connection", errors.New("proxyconnect tcp: connection refused " + cookieTestSecret), 0},
		{"TLS", "http_request", "cookie_ws_http_request_tls", errors.New("tls: certificate verification failed " + cookieTestSecret), 0},
		{"connect", "http_request", "cookie_ws_http_request_failed", errors.New(cookieTestSecret), 0},
		{"WS EOF", "ws_read", "cookie_ws_ws_read_closed", fmt.Errorf("%s: %w", cookieTestSecret, io.EOF), 0},
		{"WS truncated", "ws_read", "cookie_ws_ws_read_closed", fmt.Errorf("%s: %w", cookieTestSecret, io.ErrUnexpectedEOF), 0},
		{"WS reset", "ws_read", "cookie_ws_ws_read_connection_reset", fmt.Errorf("%s: %w", cookieTestSecret, syscall.ECONNRESET), 0},
		{"WS refused", "ws_acquire", "cookie_ws_ws_acquire_connection_refused", fmt.Errorf("%s: %w", cookieTestSecret, syscall.ECONNREFUSED), 0},
		{"stream", "http_stream", "cookie_ws_http_stream_failed", errors.New(cookieTestSecret), 503},
		{"HTTP407", "http_request", "cookie_ws_proxy_authentication", errors.New(cookieTestSecret), 407},
		{"handshake", "ws_acquire", "cookie_ws_ws_handshake_http_403", &openAIWSDialError{StatusCode: 403, Err: errors.New(cookieTestSecret), ResponseBody: []byte(cookieTestSecret), ResponseHeaders: http.Header{"Set-Cookie": {cookieTestSecret}}}, 403},
		{"WS timeout", "ws_acquire", "cookie_ws_ws_handshake_timeout", &openAIWSDialError{Err: fmt.Errorf("%s: %w", cookieTestSecret, context.DeadlineExceeded)}, 0},
		{"capacity", "ws_acquire", "cookie_ws_capacity_busy", errOpenAIWSConnQueueFull, 0},
		{"no cookie", "ws_acquire", "cookie_ws_cookie_unavailable", ErrOpenAICodexTicketUnavailable, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := cookieWSRecoveryOperationError(tc.stage, tc.err, tc.status, http.Header{"Set-Cookie": {cookieTestSecret}})
			require.Equal(t, tc.code, failure.detail.Code)
			require.Equal(t, tc.status, failure.detail.HTTPStatus)
			assertCookieRecoverySafe(t, failure.detail)
			assertCookieRecoverySafe(t, failure)
			assertCookieRecoverySafe(t, errors.Unwrap(failure))
		})
	}
}

func TestOpenAICookieWSRecoveryHTTPStreamRetainsStatusAndRetryAfter(t *testing.T) {
	for _, status := range []int{200, 401, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			resp := &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"120"}, "Set-Cookie": {cookieTestSecret}}, Body: passthroughErrReadCloser{err: errors.New(cookieTestSecret)}}
			u := &httpUpstreamRecorder{resp: resp}
			s, d := cookieWSTestService(t, u)
			before := time.Now()
			result, err := s.doOpenAICookieWSHTTPProbe(context.Background(), ticketTestAccount(41), "unused", "socks5://test", newOpenAICookieWSIdentity())
			require.Error(t, err)
			require.NotNil(t, result)
			require.Equal(t, status, result.status)
			require.Equal(t, "120", result.headers.Get("Retry-After"))
			assertCookieRecoverySafe(t, err)
			s.refreshOpenAICookieWSSlot(context.Background(), ticketTestAccount(41), 1)
			diagnostic := s.openAICookieWSRecoverySnapshot(41, 1, "refresh")
			require.NotNil(t, diagnostic)
			require.Equal(t, "backoff", diagnostic.Phase)
			require.Equal(t, 1, diagnostic.Attempts)
			require.Equal(t, "http_stream", diagnostic.LastError.Stage)
			require.Equal(t, "cookie_ws_http_stream_failed", diagnostic.LastError.Code)
			require.Equal(t, status, diagnostic.LastError.HTTPStatus)
			require.NotNil(t, diagnostic.LastFailureAt)
			require.WithinDuration(t, before.Add(120*time.Second), *diagnostic.NextAttemptAt, 2*time.Second)
			raw, _ := s.openaiCookieWSRetry.Load(openAICookieWSKeySlot(41, openAICodexTicketDefaultModel, 1))
			require.True(t, raw.(*openAICookieWSRetryState).nextAttemptAt.Equal(*diagnostic.NextAttemptAt))
			assertCookieRecoverySafe(t, diagnostic)
			require.Zero(t, d.dials)
		})
	}
}

func TestOpenAICookieWSRecoveryHTTPValidationReasons(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		body       []byte
		noCookie   bool
		status     int
	}{
		{"False", "cookie_ws_http_validation_not_true", cookieWSCompletion(openAICodexTicketDefaultModel, "False"), false, 200},
		{"wrong model", "cookie_ws_http_validation_model_mismatch", cookieWSCompletion("gpt-6-sol", "True"), false, 200},
		{"unfinished", "cookie_ws_http_validation_incomplete", []byte(`{"type":"response.output_text.delta","delta":"True"}`), false, 200},
		{"stream status", "cookie_ws_http_validation_http_401", []byte(`{"type":"response.failed","response":{"error":{"status":401,"message":"secret-token"}}}`), false, 200},
		{"missing cookie", "cookie_ws_http_cookie_missing", cookieWSCompletion(openAICodexTicketDefaultModel, "True"), true, 200},
		{"HTTP status", "cookie_ws_http_request_http_503", []byte(cookieTestSecret), false, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := cookieWSHTTPResponse("True")
			resp.StatusCode = tc.status
			resp.Body = io.NopCloser(strings.NewReader("data: " + string(tc.body) + "\n\n"))
			if tc.noCookie {
				resp.Header.Del("Set-Cookie")
			}
			s, d := cookieWSTestService(t, &httpUpstreamRecorder{resp: resp})
			before := time.Now()
			s.refreshOpenAICookieWSSlot(context.Background(), ticketTestAccount(41), 0)
			diagnostic := s.openAICookieWSRecoverySnapshot(41, 0, "refresh")
			require.Equal(t, tc.code, diagnostic.LastError.Code)
			assertCookieRecoverySafe(t, diagnostic)
			require.Zero(t, d.dials)
			require.WithinDuration(t, before.Add(5*time.Second), *diagnostic.NextAttemptAt, time.Second, "a status inside the stream must not change existing retry policy")
		})
	}
}

func TestOpenAICookieWSRecoveryCandidateReasonsCloseSocket(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		message    []byte
		status     int
	}{
		{"False", "cookie_ws_ws_validation_not_true", cookieWSCompletion(openAICodexTicketDefaultModel, "False"), 0},
		{"wrong model", "cookie_ws_ws_validation_model_mismatch", cookieWSCompletion("gpt-6-sol", "True"), 0},
		{"unfinished", "cookie_ws_ws_validation_incomplete", []byte(`{"type":"response.incomplete","response":{"error":{"message":"secret-token"}}}`), 0},
		{"rate limit", "cookie_ws_ws_validation_http_429", []byte(`{"type":"error","error":{"status":429,"message":"secret-token"}}`), 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, d := cookieWSTestService(t, nil)
			d.conn.answers = [][]byte{tc.message}
			lease, err := s.validateOpenAICookieWSCandidate(context.Background(), ticketTestAccount(41), "unused", cookieWSTestTicket(41, time.Now()))
			require.Nil(t, lease)
			var failure *openAICookieWSRecoveryFailure
			require.ErrorAs(t, err, &failure)
			require.Equal(t, tc.code, failure.detail.Code)
			require.Equal(t, tc.status, failure.detail.HTTPStatus)
			require.True(t, d.conn.closed.Load())
			require.Len(t, d.conn.writes, 1)
			assertCookieRecoverySafe(t, failure.detail)
		})
	}
}

func TestOpenAICookieWSRecoverySuccessClearsFailureAndDoesNotAddRequests(t *testing.T) {
	u := &httpUpstreamRecorder{responses: []*http.Response{cookieWSHTTPResponse("False"), cookieWSHTTPResponse("True")}}
	s, d := cookieWSTestService(t, u, "True", "True")
	a := ticketTestAccount(41)
	s.refreshOpenAICookieWSSlot(context.Background(), a, 0)
	failed := s.openAICookieWSRecoverySnapshot(41, 0, "refresh")
	require.Equal(t, "backoff", failed.Phase)
	s.refreshOpenAICookieWSSlot(context.Background(), a, 0)
	require.Len(t, u.requests, 1)
	require.Equal(t, 1, s.openAICookieWSRecoverySnapshot(41, 0, "refresh").Attempts)
	s.openaiCookieWSRetry.Store(openAICookieWSKeySlot(41, openAICodexTicketDefaultModel, 0), &openAICookieWSRetryState{attempts: 1, nextAttemptAt: time.Now().Add(-time.Second)})
	s.refreshOpenAICookieWSSlot(context.Background(), a, 0)
	succeeded := s.openAICookieWSRecoverySnapshot(41, 0, "refresh")
	require.Equal(t, "idle", succeeded.Phase)
	require.Equal(t, 2, succeeded.Attempts)
	require.NotNil(t, succeeded.LastSuccessAt)
	require.NotNil(t, succeeded.LastFailureAt)
	require.Nil(t, succeeded.LastError)
	require.Nil(t, succeeded.NextAttemptAt)
	require.Len(t, u.requests, 2)
	require.Len(t, d.conn.writes, 2)
	s.refreshOpenAICookieWSSlot(context.Background(), a, 0)
	require.Len(t, u.requests, 2)
	require.Equal(t, 2, s.openAICookieWSRecoverySnapshot(41, 0, "refresh").Attempts)
}

func TestOpenAICookieWSRecoveryAccountTokenPersistence(t *testing.T) {
	for _, tc := range []struct {
		name, code, stage string
		attempts          int
		setup             func(*OpenAIGatewayService)
	}{
		{"account", "cookie_ws_account_unavailable", "account", 0, func(s *OpenAIGatewayService) {
			s.accountRepo.(*cookieWSLifecycleRepo).getErr = errors.New(cookieTestSecret)
		}},
		{"token", "cookie_ws_token_unavailable", "authentication", 1, func(s *OpenAIGatewayService) {
			s.accountRepo.(*cookieWSLifecycleRepo).accounts[0].Credentials = map[string]any{}
		}},
		{"persistence", "cookie_ws_persist_failed", "persistence", 1, func(s *OpenAIGatewayService) {
			s.accountRepo.(*cookieWSLifecycleRepo).err = errors.New(cookieTestSecret)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, d := cookieWSTestService(t, &httpUpstreamRecorder{resp: cookieWSHTTPResponse("True")}, "True", "True")
			tc.setup(s)
			s.refreshOpenAICookieWSSlot(context.Background(), ticketTestAccount(41), 0)
			diagnostic := s.openAICookieWSRecoverySnapshot(41, 0, "refresh")
			require.Equal(t, tc.code, diagnostic.LastError.Code)
			require.Equal(t, tc.stage, diagnostic.LastError.Stage)
			require.Equal(t, tc.attempts, diagnostic.Attempts)
			assertCookieRecoverySafe(t, diagnostic)
			if tc.name == "persistence" {
				require.True(t, d.conn.closed.Load())
			}
		})
	}
}

func TestOpenAICookieWSRecoveryWarmupFailureAndRetry(t *testing.T) {
	s, a, d := cookieWarmupFixture(t, 3, "False")
	s.maintainOpenAICookieWSMinimum(context.Background(), a.ID)
	raw, _ := s.openaiCookieWSWarmupRetry.Load(a.ID)
	for slot := 0; slot < 3; slot++ {
		diagnostic := s.openAICookieWSRecoverySnapshot(a.ID, slot, "warmup")
		require.Equal(t, "backoff", diagnostic.Phase)
		require.Equal(t, 1, diagnostic.Attempts)
		require.Equal(t, "cookie_ws_validation_not_true", diagnostic.LastError.Code)
		require.Equal(t, "ws_validation", diagnostic.LastError.Stage)
		require.True(t, raw.(time.Time).Equal(*diagnostic.NextAttemptAt))
		require.Nil(t, s.openAICookieWSRecoverySnapshot(a.ID, slot, "refresh"))
	}
	s.maintainOpenAICookieWSMinimum(context.Background(), a.ID)
	require.Len(t, d.conns, 3)
	s.openaiCookieWSWarmupRetry.Store(a.ID, time.Now().Add(-time.Second))
	d.answer = "True"
	s.maintainOpenAICookieWSMinimum(context.Background(), a.ID)
	require.Len(t, d.conns, 6)
	for slot := 0; slot < 3; slot++ {
		diagnostic := s.openAICookieWSRecoverySnapshot(a.ID, slot, "warmup")
		require.Equal(t, "idle", diagnostic.Phase)
		require.Equal(t, 2, diagnostic.Attempts)
		require.Nil(t, diagnostic.LastError)
		require.Nil(t, diagnostic.NextAttemptAt)
		require.NotNil(t, diagnostic.LastSuccessAt)
	}
}

func TestOpenAICookieWSRecoverySnapshotsAreIndependentAndDetached(t *testing.T) {
	s := &OpenAIGatewayService{}
	past := time.Now().Add(-time.Second)
	s.beginOpenAICookieWSRecovery(41, 0, "refresh", "harvesting")
	s.failOpenAICookieWSRecovery(41, 0, "refresh", "backoff", cookieWSRecoveryStatusFailure("http_request", 503), &past)
	snapshot := s.openAICookieWSRecoverySnapshot(41, 0, "refresh")
	require.Equal(t, "waiting", snapshot.Phase)
	require.NotNil(t, snapshot.NextAttemptAt)
	snapshot.LastError.Code = "mutated"
	*snapshot.LastAttemptAt = time.Time{}
	*snapshot.NextAttemptAt = time.Time{}
	again := s.openAICookieWSRecoverySnapshot(41, 0, "refresh")
	require.Equal(t, "cookie_ws_http_request_http_503", again.LastError.Code)
	require.False(t, again.LastAttemptAt.IsZero())
	require.True(t, again.NextAttemptAt.Equal(past))
	var wg sync.WaitGroup
	for slot := 0; slot < 3; slot++ {
		for _, operation := range []string{"refresh", "warmup"} {
			wg.Add(1)
			go func(slot int, operation string) {
				defer wg.Done()
				for i := 0; i < 20; i++ {
					s.beginOpenAICookieWSRecovery(41, slot, operation, "warming")
					s.openAICookieWSRecoverySnapshot(41, slot, operation)
				}
			}(slot, operation)
		}
	}
	wg.Wait()
	require.Equal(t, 21, s.openAICookieWSRecoverySnapshot(41, 0, "refresh").Attempts)
	require.Equal(t, 20, s.openAICookieWSRecoverySnapshot(41, 0, "warmup").Attempts)
	require.Equal(t, 20, s.openAICookieWSRecoverySnapshot(41, 1, "refresh").Attempts)
	require.Nil(t, s.openAICookieWSRecoverySnapshot(42, 0, "refresh"))
	require.Nil(t, s.openAICookieWSRecoverySnapshot(41, 0, "invalid"))
	require.Nil(t, s.openAICookieWSRecoverySnapshot(41, 0, "refresh").NextAttemptAt)
}

func TestOpenAICookieWSRecoveryWarmupObservesExistingRecoveryOnce(t *testing.T) {
	s, a, d := cookieWarmupFixture(t, 3, "True")
	s.maintainOpenAICookieWSMinimum(context.Background(), a.ID)
	next := time.Now().Add(time.Minute)
	s.openaiCookieWSWarmupRetry.Store(a.ID, next)
	s.failOpenAICookieWSRecovery(a.ID, 1, "warmup", "backoff", cookieWSRecoveryOperationError("ws_acquire", errOpenAIWSConnQueueFull, 0, nil), &next)
	s.maintainOpenAICookieWSMinimum(context.Background(), a.ID)
	diagnostic := s.openAICookieWSRecoverySnapshot(a.ID, 1, "warmup")
	require.Equal(t, "idle", diagnostic.Phase)
	require.Nil(t, diagnostic.LastError)
	require.Nil(t, diagnostic.NextAttemptAt)
	require.NotNil(t, diagnostic.LastSuccessAt)
	require.NotNil(t, diagnostic.LastFailureAt)
	s.maintainOpenAICookieWSMinimum(context.Background(), a.ID)
	require.Equal(t, diagnostic.LastSuccessAt, s.openAICookieWSRecoverySnapshot(a.ID, 1, "warmup").LastSuccessAt)
	require.Len(t, d.conns, 3, "observing recovered capacity sends no probes")
}
