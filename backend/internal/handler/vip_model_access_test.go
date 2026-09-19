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
		{name: "ordinary user can use luna", user: &service.User{}, model: "gpt-5.6-luna", wantAllowed: true},
		{name: "ordinary user can use compact luna spelling", user: &service.User{}, model: "gpt5.6luna", wantAllowed: true},
		{name: "ordinary user can use dated luna", user: &service.User{}, model: "gpt-5.6-luna-2026-07-09", wantAllowed: true},
		{name: "missing user can use luna", model: "gpt-5.6-luna", wantAllowed: true},
		{name: "ordinary user can use other model", user: &service.User{}, model: "gpt-5.6-sol", wantAllowed: true},
		{name: "composite alias can resolve to luna", user: &service.User{}, model: "vip-alias", resolvedModel: "gpt-5.6-luna", wantAllowed: true},
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
			name: "ordinary user can use account alias to luna",
			user: &service.User{},
			account: &service.Account{Platform: service.PlatformOpenAI, Credentials: map[string]any{
				"model_mapping": map[string]any{"luna-alias": service.VipExclusiveModelName},
			}},
			model:       "luna-alias",
			wantAllowed: true,
		},
		{
			name: "ordinary user can use wildcard account alias to luna",
			user: &service.User{},
			account: &service.Account{Platform: service.PlatformOpenAI, Credentials: map[string]any{
				"model_mapping": map[string]any{"vip-*": service.VipExclusiveModelName},
			}},
			model:       "vip-alias",
			wantAllowed: true,
		},
		{
			name: "ordinary user can use compact account alias to luna",
			user: &service.User{},
			account: &service.Account{Platform: service.PlatformOpenAI, Credentials: map[string]any{
				"compact_model_mapping": map[string]any{"compact-alias": service.VipExclusiveModelName},
			}},
			model:          "compact-alias",
			requireCompact: true,
			wantAllowed:    true,
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
			name: "openai passthrough allows ordinary mapping to luna",
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
			wantAllowed: true,
		},
		{
			name: "ordinary user can use non-openai account alias to luna",
			user: &service.User{},
			account: &service.Account{Platform: service.PlatformAnthropic, Credentials: map[string]any{
				"model_mapping": map[string]any{"luna-alias": service.VipExclusiveModelName},
			}},
			model:       "luna-alias",
			wantAllowed: true,
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
