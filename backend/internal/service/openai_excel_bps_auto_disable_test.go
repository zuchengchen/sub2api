package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type excelBPSAutoDisableRepo struct {
	AccountRepository
	disable func(context.Context, *Account) (bool, error)
}

func (r *excelBPSAutoDisableRepo) DisableExcelBPSOn403(ctx context.Context, account *Account) (bool, error) {
	return r.disable(ctx, account)
}

func TestExcelBPSAutoDisableOn403(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		optIn      bool
		modelError bool
		changed    bool
		writeErr   error
		wantCalls  int
	}{
		{name: "default off", status: 403},
		{name: "enabled", status: 403, optIn: true, changed: true, wantCalls: 1},
		{name: "settings already changed", status: 403, optIn: true, wantCalls: 1},
		{name: "write failed", status: 403, optIn: true, writeErr: errors.New("write failed"), wantCalls: 1},
		{name: "model access denied", status: 403, optIn: true, modelError: true},
		{name: "bad request", status: 400, optIn: true},
		{name: "unauthorized", status: 401, optIn: true},
		{name: "rate limited", status: 429, optIn: true},
		{name: "server error", status: 500, optIn: true},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				account := excelAccount()
				if tc.optIn {
					account.Extra["openai_excel_bps_auto_disable_on_403"] = true
				}
				raw := `{"error":{"code":"permission_denied","message":"PRIVATE_UPSTREAM"}}`
				wantCode := "basispoints_upstream_error"
				if tc.modelError {
					raw = `{"error":{"code":"basispoints_model_access_changed"}}`
					wantCode = "basispoints_model_access_changed"
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(raw))}}
				svc := openAIClientToolsTestService(upstream)
				calls := 0
				svc.accountRepo = &excelBPSAutoDisableRepo{disable: func(ctx context.Context, got *Account) (bool, error) {
					calls++
					require.NoError(t, ctx.Err())
					require.Same(t, account, got)
					return tc.changed, tc.writeErr
				}}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				_, err := svc.Forward(context.Background(), c, account, []byte(fmt.Sprintf(`{"model":"gpt-6-astra","input":"test","stream":%v}`, stream)))
				require.EqualError(t, err, "excel BPS: "+wantCode)
				require.Equal(t, tc.status, rec.Code)
				require.Equal(t, wantCode, gjson.Get(rec.Body.String(), "error.code").String())
				require.Equal(t, tc.changed && tc.writeErr == nil, strings.Contains(rec.Body.String(), "automatically disabled"))
				require.NotContains(t, rec.Body.String(), "PRIVATE_UPSTREAM")
				require.Equal(t, tc.wantCalls, calls)
				require.Len(t, upstream.requests, 1, "do not replay the failed request")
				require.True(t, account.IsExcelBPSEnabled(), "do not mutate a shared scheduler snapshot")
				require.True(t, account.Schedulable)
				require.Equal(t, StatusActive, account.Status)
			})
		}
	}
}

func TestExcelBPSAutoDisableIgnoresTransportAndStreamErrors(t *testing.T) {
	for _, transport := range []bool{false, true} {
		t.Run(fmt.Sprint(transport), func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"basispoints_upstream_error\",\"message\":\"403 forbidden\"}}\n\n"))}}
			if transport {
				upstream.err = errors.New("connection failed")
			}
			account := excelAccount()
			account.Extra["openai_excel_bps_auto_disable_on_403"] = true
			svc := openAIClientToolsTestService(upstream)
			svc.accountRepo = &excelBPSAutoDisableRepo{disable: func(context.Context, *Account) (bool, error) {
				t.Fatal("only an upstream HTTP 403 may disable BPS")
				return false, nil
			}}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","input":"test","stream":true}`))
			require.Error(t, err)
			require.True(t, account.IsExcelBPSEnabled())
		})
	}
}

func TestExcelBPSAutoDisableUsesBoundedDetachedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	account := excelAccount()
	account.Extra["openai_excel_bps_auto_disable_on_403"] = true
	var writeContext context.Context
	svc := &OpenAIGatewayService{accountRepo: &excelBPSAutoDisableRepo{disable: func(ctx context.Context, _ *Account) (bool, error) {
		writeContext = ctx
		require.NoError(t, ctx.Err())
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.WithinDuration(t, time.Now().Add(openAIAccountStateUpdateTimeout), deadline, time.Second)
		return true, nil
	}}}
	require.True(t, svc.disableExcelBPSOn403(ctx, account))
	require.ErrorIs(t, writeContext.Err(), context.Canceled)
}

func TestAccount_IsExcelBPSAutoDisableOn403Enabled(t *testing.T) {
	require.False(t, (*Account)(nil).IsExcelBPSAutoDisableOn403Enabled())
	for _, mutate := range []func(*Account){
		func(a *Account) { delete(a.Extra, "openai_excel_bps_auto_disable_on_403") },
		func(a *Account) { a.Extra["openai_excel_bps_auto_disable_on_403"] = false },
		func(a *Account) { a.Extra["openai_excel_bps_auto_disable_on_403"] = "true" },
		func(a *Account) { a.Extra["openai_excel_bps"] = false },
		func(a *Account) { a.Type = AccountTypeAPIKey },
		func(a *Account) { a.Platform = PlatformAnthropic },
		func(a *Account) { parent := int64(1); a.ParentAccountID = &parent },
		func(a *Account) { a.Credentials["auth_mode"] = OpenAIAuthModeAgentIdentity },
		func(a *Account) { a.Credentials["auth_mode"] = "personal_access_token" },
	} {
		account := excelAccount()
		account.Extra["openai_excel_bps_auto_disable_on_403"] = true
		require.True(t, account.IsExcelBPSAutoDisableOn403Enabled())
		mutate(account)
		require.False(t, account.IsExcelBPSAutoDisableOn403Enabled())
	}
}
