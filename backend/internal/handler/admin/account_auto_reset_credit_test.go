package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type autoResetCreditToggleAdminStub struct {
	*stubAdminService
	account *service.Account
	err     error
	calls   int
	id      int64
	enabled bool
}

func (s *autoResetCreditToggleAdminStub) SetOpenAIAutoResetCreditEnabled(_ context.Context, id int64, enabled bool) (*service.Account, error) {
	s.calls++
	s.id = id
	s.enabled = enabled
	return s.account, s.err
}

func TestAccountAutoResetCreditToggleValidatesRequest(t *testing.T) {
	for _, tc := range []struct{ id, body string }{
		{"42", `{}`}, {"42", `{"enabled":null}`}, {"42", `{"enabled":"true"}`},
		{"42", `{"enabled":1}`}, {"42", `{`}, {"invalid", `{"enabled":true}`},
		{"0", `{"enabled":true}`}, {"-1", `{"enabled":true}`},
	} {
		t.Run(tc.id+tc.body, func(t *testing.T) {
			stub := &autoResetCreditToggleAdminStub{stubAdminService: newStubAdminService()}
			rec := serveAutoResetCreditToggle(t, stub, tc.id, tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Zero(t, stub.calls)
		})
	}
}

func TestAccountAutoResetCreditToggleReturnsPersistedAccount(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		stub := &autoResetCreditToggleAdminStub{stubAdminService: newStubAdminService(), account: &service.Account{
			ID: 42, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			Extra: map[string]any{
				service.OpenAIAutoResetCreditEnabledExtraKey:      enabled,
				service.OpenAIAutoResetCreditOptInVersionExtraKey: nil,
				service.OpenAIAutoResetCredit5hThresholdExtraKey:  0.82,
				service.OpenAIAutoResetCreditStateExtraKey:        map[string]any{"status": "resetting"},
			},
		}}
		if enabled {
			stub.account.Extra[service.OpenAIAutoResetCreditOptInVersionExtraKey] = 1
		}
		body, err := json.Marshal(map[string]any{"enabled": enabled, "extra": map[string]any{"auto_reset_credit_5h_threshold": 0.1}})
		require.NoError(t, err)
		rec := serveAutoResetCreditToggle(t, stub, "42", string(body))
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, int64(42), stub.id)
		require.Equal(t, enabled, stub.enabled)
		require.Equal(t, 1, stub.calls)
		var response struct {
			Data struct {
				ID    int64          `json:"id"`
				Extra map[string]any `json:"extra"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
		require.Equal(t, int64(42), response.Data.ID)
		require.Equal(t, enabled, response.Data.Extra[service.OpenAIAutoResetCreditEnabledExtraKey])
		require.Equal(t, 0.82, response.Data.Extra[service.OpenAIAutoResetCredit5hThresholdExtraKey])
		require.Equal(t, map[string]any{"status": "resetting"}, response.Data.Extra[service.OpenAIAutoResetCreditStateExtraKey])
		if enabled {
			require.Equal(t, float64(1), response.Data.Extra[service.OpenAIAutoResetCreditOptInVersionExtraKey])
		} else {
			require.Nil(t, response.Data.Extra[service.OpenAIAutoResetCreditOptInVersionExtraKey])
		}
	}
}

func TestAccountAutoResetCreditTogglePropagatesServiceErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{service.ErrAccountNotFound, http.StatusNotFound},
		{infraerrors.BadRequest("OPENAI_AUTO_RESET_CREDIT_ACCOUNT_INVALID", "unsupported account"), http.StatusBadRequest},
		{infraerrors.BadRequest("OPENAI_AUTO_RESET_CREDIT_THRESHOLD_INVALID", "invalid threshold"), http.StatusBadRequest},
	} {
		stub := &autoResetCreditToggleAdminStub{stubAdminService: newStubAdminService(), err: tc.err}
		rec := serveAutoResetCreditToggle(t, stub, "42", `{"enabled":true}`)
		require.Equal(t, tc.status, rec.Code)
		require.Contains(t, rec.Body.String(), infraerrors.Reason(tc.err))
	}
}

func serveAutoResetCreditToggle(t *testing.T, stub *autoResetCreditToggleAdminStub, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewAccountHandler(stub, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router.POST("/accounts/:id/auto-reset-credit-enabled", handler.SetOpenAIAutoResetCreditEnabled)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/accounts/"+id+"/auto-reset-credit-enabled", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	return rec
}
