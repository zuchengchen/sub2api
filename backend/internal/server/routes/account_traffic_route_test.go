package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAccountTrafficProtectionRemovedRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	h := &handler.Handlers{Admin: &handler.AdminHandlers{}}
	auth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) { c.Next() })
	audit := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	step := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	RegisterAdminRoutes(router.Group("/api/v1"), h, auth, audit, step, nil, nil)
	registered := map[string]bool{}
	for _, r := range router.Routes() {
		registered[r.Method+" "+r.Path] = true
	}
	for _, route := range []string{"PUT /api/v1/admin/accounts/:id", "POST /api/v1/admin/accounts", "POST /api/v1/admin/accounts/:id/test", "POST /api/v1/admin/intelligent-tests/run", "GET /api/v1/admin/intelligent-tests/records"} {
		require.True(t, registered[route], route)
	}
	for _, r := range []struct{ method, path string }{
		{"GET", "/accounts/anti-degrade/strategies"}, {"GET", "/accounts/7/anti-degrade"}, {"POST", "/accounts/7/anti-degrade/apply"}, {"POST", "/accounts/7/anti-degrade/revert"}, {"POST", "/accounts/7/protection"}, {"GET", "/accounts/7/traffic"}, {"PUT", "/accounts/7/traffic"}, {"GET", "/accounts/7/traffic-control"},
	} {
		t.Run(r.method+r.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(r.method, "/api/v1/admin"+r.path, nil))
			require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		})
	}
}
