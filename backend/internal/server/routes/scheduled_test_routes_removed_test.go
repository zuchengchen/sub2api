package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestScheduledTestRoutesRemovedWhileManualAndIntelligentTestsRemain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{}}
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) { c.Next() })
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil, nil)

	registered := make(map[string]bool)
	for _, route := range router.Routes() {
		require.False(t, strings.Contains(route.Path, "scheduled-test-plans"), route.Method+" "+route.Path)
		registered[route.Method+" "+route.Path] = true
	}
	for _, route := range []string{
		"POST /api/v1/admin/accounts/:id/test",
		"POST /api/v1/admin/accounts/:id/recover-state",
		"POST /api/v1/admin/intelligent-tests/run",
		"GET /api/v1/admin/intelligent-tests/records",
		"GET /api/v1/admin/ops/workers/status",
	} {
		require.True(t, registered[route], route)
	}

	for _, request := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/accounts/1/scheduled-test-plans"},
		{http.MethodPost, "/api/v1/admin/scheduled-test-plans"},
		{http.MethodPut, "/api/v1/admin/scheduled-test-plans/1"},
		{http.MethodDelete, "/api/v1/admin/scheduled-test-plans/1"},
		{http.MethodGet, "/api/v1/admin/scheduled-test-plans/1/results"},
	} {
		t.Run(request.method+" "+request.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(request.method, request.path, nil))
			require.Equal(t, http.StatusNotFound, recorder.Code)
		})
	}
}
