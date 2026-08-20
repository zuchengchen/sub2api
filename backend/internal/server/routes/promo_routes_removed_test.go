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

func TestPromoCodeRoutesAreNotRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{
		Auth:    &handler.AuthHandler{},
		Passkey: &handler.PasskeyHandler{},
		Setting: &handler.SettingHandler{},
		Admin:   &handler.AdminHandlers{},
	}
	jwtAuth := servermiddleware.JWTAuthMiddleware(func(c *gin.Context) { c.Next() })
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) { c.Next() })
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	v1 := router.Group("/api/v1")
	RegisterAuthRoutes(v1, handlers, jwtAuth, auditLog, nil, nil, nil)
	RegisterAdminRoutes(v1, handlers, adminAuth, auditLog, stepUp, nil, nil)

	for _, route := range router.Routes() {
		require.NotEqual(t, "/api/v1/auth/validate-promo-code", route.Path)
		require.False(t, strings.HasPrefix(route.Path, "/api/v1/admin/promo-codes"), route.Method+" "+route.Path)
	}

	for _, request := range []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/api/v1/auth/validate-promo-code"},
		{method: http.MethodGet, path: "/api/v1/admin/promo-codes"},
		{method: http.MethodGet, path: "/api/v1/admin/promo-codes/1/usages"},
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(request.method, request.path, nil))
		require.Equal(t, http.StatusNotFound, recorder.Code, request.method+" "+request.path)
	}
}
