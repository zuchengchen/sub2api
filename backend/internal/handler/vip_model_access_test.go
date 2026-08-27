package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRequireUserModelAccess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name          string
		user          *service.User
		model         string
		resolvedModel string
		wantAllowed   bool
	}{
		{name: "vip can use luna", user: &service.User{IsVIP: true}, model: "gpt-5.6-luna", wantAllowed: true},
		{name: "ordinary user cannot use luna", user: &service.User{}, model: "gpt-5.6-luna", wantAllowed: false},
		{name: "ordinary user cannot use compact luna spelling", user: &service.User{}, model: "gpt5.6luna", wantAllowed: false},
		{name: "ordinary user cannot use dated luna", user: &service.User{}, model: "gpt-5.6-luna-2026-07-09", wantAllowed: false},
		{name: "missing user fails closed for luna", model: "gpt-5.6-luna", wantAllowed: false},
		{name: "ordinary user can use other model", user: &service.User{}, model: "gpt-5.6-sol", wantAllowed: true},
		{name: "composite alias cannot expose luna", user: &service.User{}, model: "vip-alias", resolvedModel: "gpt-5.6-luna", wantAllowed: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if tt.resolvedModel != "" {
				c.Request = c.Request.WithContext(service.WithCompositeRouteDecision(c.Request.Context(), service.CompositeRouteDecision{
					Matched:        true,
					TargetPlatform: service.PlatformOpenAI,
					UpstreamModel:  tt.resolvedModel,
				}))
			}

			called := false
			allowed := requireUserModelAccess(c, &service.APIKey{User: tt.user}, func(c *gin.Context, status int, errType, message string) {
				called = true
				c.JSON(status, gin.H{"error": gin.H{"type": errType, "message": message}})
			}, tt.model)

			require.Equal(t, tt.wantAllowed, allowed)
			require.Equal(t, !tt.wantAllowed, called)
			if !tt.wantAllowed {
				require.Equal(t, http.StatusForbidden, rec.Code)
				require.Contains(t, rec.Body.String(), service.VipExclusiveModelAccessMessage)
			}
		})
	}
}

func TestRequireGoogleModelAccessUsesGoogleErrorEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gpt-5.6-luna:generateContent", nil)

	require.False(t, requireGoogleModelAccess(c, &service.APIKey{User: &service.User{}}, service.VipExclusiveModelName))
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":403`)
	require.Contains(t, rec.Body.String(), service.VipExclusiveModelAccessMessage)
}

func TestRequireUserAccountModelAccess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name           string
		user           *service.User
		account        *service.Account
		model          string
		requireCompact bool
		wantAllowed    bool
	}{
		{
			name: "ordinary user cannot use account alias to luna",
			user: &service.User{},
			account: &service.Account{Platform: service.PlatformOpenAI, Credentials: map[string]any{
				"model_mapping": map[string]any{"luna-alias": service.VipExclusiveModelName},
			}},
			model:       "luna-alias",
			wantAllowed: false,
		},
		{
			name: "ordinary user cannot use wildcard account alias to luna",
			user: &service.User{},
			account: &service.Account{Platform: service.PlatformOpenAI, Credentials: map[string]any{
				"model_mapping": map[string]any{"vip-*": service.VipExclusiveModelName},
			}},
			model:       "vip-alias",
			wantAllowed: false,
		},
		{
			name: "ordinary user cannot use compact account alias to luna",
			user: &service.User{},
			account: &service.Account{Platform: service.PlatformOpenAI, Credentials: map[string]any{
				"compact_model_mapping": map[string]any{"compact-alias": service.VipExclusiveModelName},
			}},
			model:          "compact-alias",
			requireCompact: true,
			wantAllowed:    false,
		},
		{
			name: "ordinary user can use compact alias on normal responses",
			user: &service.User{},
			account: &service.Account{Platform: service.PlatformOpenAI, Credentials: map[string]any{
				"compact_model_mapping": map[string]any{"compact-alias": service.VipExclusiveModelName},
			}},
			model:       "compact-alias",
			wantAllowed: true,
		},
		{
			name: "openai passthrough fails closed for stale ordinary mapping",
			user: &service.User{},
			account: &service.Account{
				Platform: service.PlatformOpenAI,
				Type:     service.AccountTypeAPIKey,
				Credentials: map[string]any{
					"model_mapping": map[string]any{"public-alias": service.VipExclusiveModelName},
				},
				Extra: map[string]any{"openai_passthrough": true},
			},
			model:       "public-alias",
			wantAllowed: false,
		},
		{
			name: "ordinary user cannot use non-openai account alias to luna",
			user: &service.User{},
			account: &service.Account{Platform: service.PlatformAnthropic, Credentials: map[string]any{
				"model_mapping": map[string]any{"luna-alias": service.VipExclusiveModelName},
			}},
			model:       "luna-alias",
			wantAllowed: false,
		},
		{
			name: "vip can use account alias to luna",
			user: &service.User{IsVIP: true},
			account: &service.Account{Platform: service.PlatformOpenAI, Credentials: map[string]any{
				"model_mapping": map[string]any{"luna-alias": service.VipExclusiveModelName},
			}},
			model:       "luna-alias",
			wantAllowed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

			called := false
			allowed := requireUserAccountModelAccess(
				c,
				&service.APIKey{User: tt.user},
				tt.account,
				func(c *gin.Context, status int, errType, message string) {
					called = true
					c.JSON(status, gin.H{"error": gin.H{"type": errType, "message": message}})
				},
				tt.requireCompact,
				tt.model,
			)

			require.Equal(t, tt.wantAllowed, allowed)
			require.Equal(t, !tt.wantAllowed, called)
			if !tt.wantAllowed {
				require.Equal(t, http.StatusForbidden, rec.Code)
				require.Contains(t, rec.Body.String(), service.VipExclusiveModelAccessMessage)
			}
		})
	}
}
