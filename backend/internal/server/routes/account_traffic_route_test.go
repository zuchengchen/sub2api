package routes

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountTrafficAdminRouteUsesTrafficNotTrafficControl(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		AccountTraffic: admin.NewAccountTrafficHandler(nil, nil),
	}}
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) { c.Next() })
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil, nil)

	routes := make(map[string]struct{})
	for _, route := range router.Routes() {
		routes[route.Method+" "+route.Path] = struct{}{}
	}
	_, getOK := routes["GET /api/v1/admin/accounts/:id/traffic"]
	_, putOK := routes["PUT /api/v1/admin/accounts/:id/traffic"]
	require.True(t, getOK)
	require.True(t, putOK)
	_, getLegacy := routes["GET /api/v1/admin/accounts/:id/traffic-control"]
	_, putLegacy := routes["PUT /api/v1/admin/accounts/:id/traffic-control"]
	require.False(t, getLegacy)
	require.False(t, putLegacy)
}
