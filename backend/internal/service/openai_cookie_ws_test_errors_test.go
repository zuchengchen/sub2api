package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const cookieTestSecret = "socks5://private-user:private-password@proxy.invalid; Bearer secret-token; Cookie=secret-cookie"

type cookieTestErrorDialer struct {
	status int
	err    error
	calls  int
}

func (d *cookieTestErrorDialer) Dial(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error) {
	d.calls++
	return nil, d.status, http.Header{"Set-Cookie": {cookieTestSecret}}, d.err
}

func TestCookieWSTestErrorClassificationDoesNotExposeUpstreamSecrets(t *testing.T) {
	for _, tc := range []struct {
		stage          string
		err            error
		code, category string
	}{
		{"acquire", &openAIWSDialError{StatusCode: 403, ResponseBody: []byte(cookieTestSecret), Err: errors.New(cookieTestSecret)}, "cookie_ws_handshake_http_403", "account_error"},
		{"acquire", &openAIWSDialError{StatusCode: 429, Err: errors.New(cookieTestSecret)}, "cookie_ws_handshake_http_429", "rate_limited"},
		{"acquire", &openAIWSDialError{StatusCode: 404, Err: errors.New(cookieTestSecret)}, "cookie_ws_handshake_http_404", "model_error"},
		{"acquire", &openAIWSDialError{StatusCode: 502, Err: errors.New(cookieTestSecret)}, "cookie_ws_handshake_http_502", "network_error"},
		{"acquire", &openAIWSDialError{Err: fmt.Errorf("%s: %w", cookieTestSecret, context.DeadlineExceeded)}, "cookie_ws_acquire_timeout", "network_error"},
		{"reserve", context.DeadlineExceeded, "cookie_ws_capacity_timeout", "rate_limited"},
		{"acquire", context.Canceled, "cookie_ws_cancelled", "failed"},
		{"acquire", errOpenAIWSConnQueueFull, "cookie_ws_capacity_busy", "rate_limited"},
		{"acquire", errOpenAICookieWSAccountUnavailable, "cookie_ws_account_unavailable", "account_error"},
		{"acquire", errOpenAIWSCookieExpired, "cookie_ws_cookie_expired", "account_error"},
		{"acquire", errOpenAIWSCookieRetired, "cookie_ws_cookie_retired", "account_error"},
		{"acquire", ErrOpenAICodexTicketUnavailable, "cookie_ws_cookie_unavailable", "account_error"},
		{"acquire", openAICookieWSUnavailableFailover(ErrOpenAICodexTicketUnavailable), "cookie_ws_cookie_unavailable", "account_error"},
		{"acquire", errors.New(cookieTestSecret), "cookie_ws_acquire_failed", "network_error"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			e := cookieWSTestOperationError(tc.stage, tc.err)
			require.Equal(t, tc.code, e.Code)
			require.Equal(t, tc.category, e.Category)
			require.NotContains(t, fmt.Sprintf("%+v", e), cookieTestSecret)
			require.NotContains(t, e.Error(), "secret-token")
			if errors.Is(tc.err, context.DeadlineExceeded) {
				require.ErrorIs(t, e, context.DeadlineExceeded)
			}
		})
	}
}

func TestCookieWSTestAdapterPreservesSafeFailureThroughFinalize(t *testing.T) {
	for _, tc := range []struct {
		name, code, category string
		events               [][]byte
		setup                func(*OpenAIGatewayService, *Account)
	}{
		{"not_true", "cookie_ws_validation_not_true", "failed", [][]byte{cookieWSCompletion("gpt-6-astra", "False")}, nil},
		{"model_mismatch", "cookie_ws_validation_model_mismatch", "model_error", [][]byte{cookieWSCompletion("gpt-6-sol", "True")}, nil},
		{"probe_429", "cookie_ws_validation_http_429", "rate_limited", [][]byte{[]byte(`{"type":"error","error":{"status":429,"type":"rate_limit_exceeded","message":"secret-token"}}`)}, nil},
		{"probe_401", "cookie_ws_validation_http_401", "account_error", [][]byte{[]byte(`{"type":"error","error":{"status":401,"type":"authentication_error","message":"secret-token"}}`)}, nil},
		{"probe_503", "cookie_ws_validation_http_503", "network_error", [][]byte{[]byte(`{"type":"response.failed","response":{"error":{"status":503,"type":"server_error","message":"secret-token"}}}`)}, nil},
		{"malformed", "cookie_ws_validation_malformed", "failed", [][]byte{[]byte(cookieTestSecret)}, nil},
		{"probe_closed", "cookie_ws_validation_closed", "network_error", nil, nil},
		{"account_disabled", "cookie_ws_account_unavailable", "account_error", nil, func(s *OpenAIGatewayService, a *Account) {
			s.accountRepo.(*cookieWSLifecycleRepo).accounts[0].Status = StatusDisabled
		}},
		{"account_limit", "cookie_ws_account_rate_limited", "rate_limited", nil, func(s *OpenAIGatewayService, a *Account) {
			until := time.Now().Add(time.Hour)
			s.accountRepo.(*cookieWSLifecycleRepo).accounts[0].RateLimitResetAt = &until
		}},
		{"no_cookie", "cookie_ws_cookie_unavailable", "account_error", nil, func(s *OpenAIGatewayService, a *Account) {
			s.openaiCookieWSTickets.Delete(openAICodexTicketKey(a.ID, "gpt-6-astra"))
		}},
		{"handshake", "cookie_ws_handshake_http_403", "account_error", nil, func(s *OpenAIGatewayService, a *Account) {
			s.getOpenAIWSConnPool().setClientDialerForTest(&cookieTestErrorDialer{status: 403, err: errors.New(cookieTestSecret)})
		}},
		{"dial_timeout", "cookie_ws_acquire_timeout", "network_error", nil, func(s *OpenAIGatewayService, a *Account) {
			s.getOpenAIWSConnPool().setClientDialerForTest(&cookieTestErrorDialer{err: fmt.Errorf("%s: %w", cookieTestSecret, context.DeadlineExceeded)})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &openAIWSCaptureConn{events: tc.events}
			gateway, account, _, dialer := newCookieForwardFixture(t, conn)
			gateway.getOpenAIWSConnPool().SetCookieValidator(gateway.validateOpenAICookieWSBusinessConn)
			if tc.setup != nil {
				tc.setup(gateway, account)
			}
			svc := &AccountTestService{openaiGatewayService: gateway, httpUpstream: gateway.httpUpstream}
			capture := &intelligentCapture{}
			writer := &intelligentSSEWriter{header: http.Header{}}
			c, _ := gin.CreateTestContext(writer)
			ctx := context.WithValue(context.Background(), intelligentRunKey{}, &intelligentRunContext{prompt: "Draw HTML", testType: "pelican", capture: capture})
			c.Request = httptest.NewRequest(http.MethodPost, "/test", nil).WithContext(ctx)
			err := svc.testOpenAICookieWSAccountConnection(c, account, "gpt-6-astra", "hi")
			var safe *openAICookieWSTestError
			require.ErrorAs(t, err, &safe)
			require.Equal(t, tc.code, safe.Code)
			record := &IntelligentTestRecord{ConfigSnapshot: &IntelligentTestConfig{}}
			require.Error(t, finalizeIntelligentTestRun(record, capture, writer, "", "", "", err))
			require.Equal(t, tc.category, record.Status)
			require.Equal(t, safe.Message, record.ErrorMessage)
			require.Contains(t, writer.body.String(), tc.code)
			for _, private := range []string{"secret-token", "secret-cookie", "private-password"} {
				require.NotContains(t, writer.body.String()+record.ErrorMessage+record.RawResponse, private)
			}
			require.Empty(t, record.Result)
			require.False(t, writer.sawComplete)
			require.LessOrEqual(t, len(conn.writes), 1, "no business request or automatic replay after failed validation")
			require.LessOrEqual(t, dialer.DialCount(), 1, "test never switches accounts or retries")
			if len(conn.writes) > 0 {
				require.True(t, openAICookieWSIsProbePayload(conn.writes[0]))
				require.True(t, conn.closed)
			}
		})
	}
}

func TestCookieWSTestAdapterBusinessHTTPStatusIsSafeAndNeverReplayed(t *testing.T) {
	for _, tc := range []struct {
		status   int
		category string
	}{{400, "request_error"}, {401, "account_error"}, {403, "account_error"}, {404, "model_error"}, {422, "request_error"}, {429, "rate_limited"}, {500, "network_error"}, {503, "network_error"}} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			conn := &openAIWSCaptureConn{events: [][]byte{[]byte(fmt.Sprintf(`{"type":"response.failed","response":{"error":{"status_code":%d,"message":"secret-token"}}}`, tc.status))}}
			gateway, account, _, dialer := newCookieForwardFixture(t, conn)
			svc := &AccountTestService{openaiGatewayService: gateway, httpUpstream: gateway.httpUpstream}
			capture := &intelligentCapture{}
			writer := &intelligentSSEWriter{header: http.Header{}}
			c, _ := gin.CreateTestContext(writer)
			ctx := context.WithValue(context.Background(), intelligentRunKey{}, &intelligentRunContext{prompt: "Draw HTML", capture: capture})
			c.Request = httptest.NewRequest(http.MethodPost, "/test", nil).WithContext(ctx)
			err := svc.testOpenAICookieWSAccountConnection(c, account, "gpt-6-astra", "hi")
			var safe *openAICookieWSTestError
			require.ErrorAs(t, err, &safe)
			require.Equal(t, fmt.Sprintf("cookie_ws_response_http_%d", tc.status), safe.Code)
			record := &IntelligentTestRecord{ConfigSnapshot: &IntelligentTestConfig{}}
			require.Error(t, finalizeIntelligentTestRun(record, capture, writer, "", "", "", err))
			require.Equal(t, tc.category, record.Status)
			require.NotContains(t, record.ErrorMessage+writer.body.String(), "secret-token")
			require.True(t, conn.closed)
			require.Len(t, conn.writes, 1)
			require.Equal(t, 1, dialer.DialCount())
		})
	}
}

func TestCookieWSTestAdapterCapacityWaitIsNotNetworkFailure(t *testing.T) {
	gateway, account, ticket, _ := newCookieForwardFixture(t, &openAIWSCaptureConn{})
	_, release, err := gateway.reserveOpenAICookieWSSlot(context.Background(), account, ticket.Model, "")
	require.NoError(t, err)
	defer release()
	svc := &AccountTestService{openaiGatewayService: gateway, httpUpstream: gateway.httpUpstream}
	capture := &intelligentCapture{}
	writer := &intelligentSSEWriter{header: http.Header{}}
	c, _ := gin.CreateTestContext(writer)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	ctx = context.WithValue(ctx, intelligentRunKey{}, &intelligentRunContext{prompt: "Draw HTML", capture: capture})
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil).WithContext(ctx)
	err = svc.testOpenAICookieWSAccountConnection(c, account, "gpt-6-astra", "hi")
	var safe *openAICookieWSTestError
	require.ErrorAs(t, err, &safe)
	require.Equal(t, "cookie_ws_capacity_timeout", safe.Code)
	record := &IntelligentTestRecord{ConfigSnapshot: &IntelligentTestConfig{}}
	require.Error(t, finalizeIntelligentTestRun(record, capture, writer, "", "", "", err))
	require.Equal(t, "rate_limited", record.Status)
	require.False(t, strings.Contains(record.ErrorMessage, "connection failed"))
}
