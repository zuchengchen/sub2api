package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Retired endpoints still enforce the same access checks before returning their
// compatibility response. Exercise the real route registration, not just handlers.
func TestDataManagementRoutesPreserveAccessAndDeprecation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		DataManagement: adminhandler.NewDataManagementHandler(service.NewDataManagementService()),
	}}
	var calls []string
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) {
		calls = append(calls, "auth")
		switch c.GetHeader("Authorization") {
		case "":
			servermiddleware.AbortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Authorization required")
		case "Bearer admin-test":
			c.Next()
		default:
			servermiddleware.AbortWithError(c, http.StatusForbidden, "FORBIDDEN", "Admin access required")
		}
	})
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) {
		calls = append(calls, "audit")
		c.Next()
	})
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) {
		calls = append(calls, "step-up")
		if c.GetHeader("X-Test-Step-Up") != "verified" {
			servermiddleware.AbortWithError(c, http.StatusForbidden, "STEP_UP_REQUIRED", "Step-up required")
			return
		}
		c.Next()
	})
	RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil, nil)

	for _, endpoint := range []struct {
		method, path, body string
		stepUp             bool
	}{
		{http.MethodGet, "/agent/health", "", false},
		{http.MethodGet, "/config", "", false},
		{http.MethodPut, "/config", `{}`, false},
		{http.MethodGet, "/sources/postgres/profiles", "", false},
		{http.MethodPost, "/sources/postgres/profiles", `{"profile_id":"default","name":"Default","config":{"host":"db"}}`, false},
		{http.MethodPut, "/sources/redis/profiles/default", `{"name":"Default","config":{"addr":"redis:6379"}}`, false},
		{http.MethodDelete, "/sources/redis/profiles/default", "", false},
		{http.MethodPost, "/sources/postgres/profiles/default/activate", "", false},
		{http.MethodPost, "/s3/test", `{"region":"test-region","bucket":"test-bucket"}`, false},
		{http.MethodGet, "/s3/profiles", "", false},
		{http.MethodPost, "/s3/profiles", `{"profile_id":"default","name":"Default"}`, true},
		{http.MethodPut, "/s3/profiles/default", `{"name":"Default"}`, true},
		{http.MethodDelete, "/s3/profiles/default", "", false},
		{http.MethodPost, "/s3/profiles/default/activate", "", true},
		{http.MethodPost, "/backups", `{"backup_type":"full"}`, true},
		{http.MethodGet, "/backups?page_size=invalid", "", false},
		{http.MethodGet, "/backups/existing-job", "", false},
	} {
		t.Run(endpoint.method+" "+endpoint.path, func(t *testing.T) {
			for _, access := range []struct {
				name, auth string
				verified   bool
				denied     int
			}{
				{name: "unauthenticated", denied: http.StatusUnauthorized},
				{name: "non-admin", auth: "Bearer user-test", denied: http.StatusForbidden},
				{name: "admin", auth: "Bearer admin-test"},
				{name: "verified-admin", auth: "Bearer admin-test", verified: true},
			} {
				t.Run(access.name, func(t *testing.T) {
					calls = nil
					request := httptest.NewRequest(endpoint.method, "/api/v1/admin/data-management"+endpoint.path, strings.NewReader(endpoint.body))
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("Authorization", access.auth)
					if access.verified {
						request.Header.Set("X-Test-Step-Up", "verified")
					}
					recorder := httptest.NewRecorder()
					router.ServeHTTP(recorder, request)

					if access.denied != 0 {
						require.Equal(t, access.denied, recorder.Code)
						require.Equal(t, []string{"auth"}, calls)
						return
					}
					wantCalls := []string{"auth", "audit"}
					if endpoint.stepUp {
						wantCalls = append(wantCalls, "step-up")
					}
					require.Equal(t, wantCalls, calls)
					if endpoint.stepUp && !access.verified {
						require.Equal(t, http.StatusForbidden, recorder.Code)
						require.Contains(t, recorder.Body.String(), "STEP_UP_REQUIRED")
						return
					}

					var payload struct {
						Code     int               `json:"code"`
						Reason   string            `json:"reason"`
						Metadata map[string]string `json:"metadata"`
						Data     struct {
							Enabled    bool   `json:"enabled"`
							Reason     string `json:"reason"`
							SocketPath string `json:"socket_path"`
						} `json:"data"`
					}
					require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
					if endpoint.path == "/agent/health" {
						require.Equal(t, http.StatusOK, recorder.Code)
						require.Zero(t, payload.Code)
						require.False(t, payload.Data.Enabled)
						require.Equal(t, service.DataManagementDeprecatedReason, payload.Data.Reason)
						require.Equal(t, service.DefaultDataManagementAgentSocketPath, payload.Data.SocketPath)
						return
					}
					require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
					require.Equal(t, http.StatusServiceUnavailable, payload.Code)
					require.Equal(t, service.DataManagementDeprecatedReason, payload.Reason)
					require.Equal(t, service.DefaultDataManagementAgentSocketPath, payload.Metadata["socket_path"])
				})
			}
		})
	}
}
