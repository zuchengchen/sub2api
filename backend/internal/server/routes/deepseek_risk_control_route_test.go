package routes

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDeepSeekRiskControlAdminRouteReplacesLegacyAPIKeyTest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		ContentModeration: admin.NewContentModerationHandler((*service.ContentModerationService)(nil)),
	}}
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) { c.Next() })
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil, nil)

	routes := make(map[string]struct{})
	for _, route := range router.Routes() {
		routes[route.Method+" "+route.Path] = struct{}{}
	}
	_, exists := routes["POST /api/v1/admin/risk-control/deepseek/channels/:id/test"]
	require.True(t, exists)
	_, exists = routes["POST /api/v1/admin/risk-control/deepseek/channels/:id/test-api"]
	require.True(t, exists)
	_, exists = routes["POST /api/v1/admin/risk-control/api-keys/test"]
	require.False(t, exists)
}
