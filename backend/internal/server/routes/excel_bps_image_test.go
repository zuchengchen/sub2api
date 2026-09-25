package routes

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestExcelBPSImageRouteAllowsUnauthenticatedFetchOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	serviceGateway := &service.OpenAIGatewayService{}
	gateway := handler.NewOpenAIGatewayHandler(serviceGateway, nil, nil, nil, nil, nil, nil, nil, cfg)
	h := &handler.Handlers{OpenAIGateway: gateway, Gateway: &handler.GatewayHandler{}}
	router := gin.New()
	RegisterGatewayRoutes(router, h, func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) }, nil, nil, nil, nil, nil, cfg)
	path := "/api/bps-images/" + strings.Repeat("a", 43)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		require.Equal(t, http.StatusNotFound, w.Code)
		require.Equal(t, "private, no-store", w.Header().Get("Cache-Control"), "must reach the image handler without API authentication")
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
	require.Equal(t, http.StatusNotFound, w.Code, "no public upload route may exist")
}

type unreadBPSBody struct{ read bool }

func (b *unreadBPSBody) Read([]byte) (int, error) { b.read = true; return 0, io.EOF }
func (*unreadBPSBody) Close() error               { return nil }

func TestExcelBPSImageAdmissionDoesNotWrapGatewayHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxBodySize: 256 << 20, TextMaxBodySize: 32 << 20}}
	settings := service.NewSettingService(&bpsImageOffSettingsRepo{}, cfg)
	r := gin.New()
	RegisterGatewayRoutes(r, &handler.Handlers{OpenAIGateway: &handler.OpenAIGatewayHandler{}, Gateway: &handler.GatewayHandler{}}, func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI}})
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "auth"})
	}, nil, nil, nil, settings, nil, cfg)
	for _, path := range []string{"/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		body := &unreadBPSBody{}
		req.Body = body
		req.ContentLength = 65 << 20
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.NotContains(t, w.Body.String(), "basispoints_image_body_too_large", path)
		require.NotEqual(t, http.StatusRequestEntityTooLarge, w.Code, path)
	}
}

type bpsImageOffSettingsRepo struct{ service.SettingRepository }

func (*bpsImageOffSettingsRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	return map[string]string{service.SettingKeyExcelBPSImageRelayEnabled: "false"}, nil
}
