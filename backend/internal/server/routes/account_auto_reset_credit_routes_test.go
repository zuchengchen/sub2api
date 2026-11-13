package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAutoResetCreditToggleRouteMatchesCreditsProtection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{Account: &adminhandler.AccountHandler{}}}
	chains := map[string][]string{}
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) {
		names := c.HandlerNames()
		chains[c.FullPath()] = names[:len(names)-1]
		if c.GetHeader("Authorization") == "" {
			servermiddleware.AbortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Authorization required")
			return
		}
		servermiddleware.AbortWithError(c, http.StatusForbidden, "FORBIDDEN", "Admin access required")
	})
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil, nil)

	for _, suffix := range []string{"openai-credits", "auto-reset-credit-enabled"} {
		for _, auth := range []string{"", "Bearer user-token"} {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/42/"+suffix, nil)
			req.Header.Set("Authorization", auth)
			router.ServeHTTP(rec, req)
			if auth == "" {
				require.Equal(t, http.StatusUnauthorized, rec.Code)
			} else {
				require.Equal(t, http.StatusForbidden, rec.Code)
			}
		}
	}
	creditsChain := chains["/api/v1/admin/accounts/:id/openai-credits"]
	require.NotEmpty(t, creditsChain)
	require.Equal(t, creditsChain, chains["/api/v1/admin/accounts/:id/auto-reset-credit-enabled"])

	// Account-scoped admin keys must not gain a new consumption permission
	// merely because this endpoint sits next to existing account routes.
	var allPermissions []string
	for _, permission := range adminaccess.PermissionCatalog() {
		allPermissions = append(allPermissions, permission.ID)
	}
	policy := adminaccess.Policy{
		GlobalPermissions: allPermissions,
		Rules: []adminaccess.Rule{{Permissions: allPermissions, AllGroups: true, IncludeUngrouped: true,
			AccountTypes: []string{"oauth", "setup-token", "apikey"}}},
	}
	require.False(t, adminaccess.RouteAllowed(policy, http.MethodPost, "/api/v1/admin/accounts/:id/auto-reset-credit-enabled"))
}
