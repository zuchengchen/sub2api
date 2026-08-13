package middleware

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDeriveAuditAction(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   string
	}{
		{"PUT", "/api/v1/admin/accounts/:id", "admin.accounts.update"},
		{"POST", "/api/v1/admin/accounts", "admin.accounts.create"},
		{"DELETE", "/api/v1/admin/backups/:id", "admin.backups.delete"},
		{"GET", "/api/v1/admin/users/:id/api-keys", "admin.users.api_keys.read"},
		{"POST", "/api/v1/admin/redeem-codes/batch", "admin.redeem_codes.batch.create"},
	}
	for _, tc := range cases {
		if got := deriveAuditAction(tc.method, tc.path); got != tc.want {
			t.Fatalf("deriveAuditAction(%q, %q) = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

type auditCaptureRepository struct {
	mu   sync.Mutex
	logs []*service.AuditLog
}

func (r *auditCaptureRepository) BatchInsert(_ context.Context, logs []*service.AuditLog) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, logs...)
	return int64(len(logs)), nil
}
func (r *auditCaptureRepository) Insert(_ context.Context, log *service.AuditLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, log)
	return nil
}
func (r *auditCaptureRepository) List(context.Context, *service.AuditLogFilter) (*service.AuditLogList, error) {
	return &service.AuditLogList{}, nil
}
func (r *auditCaptureRepository) GetByID(context.Context, int64) (*service.AuditLog, error) {
	return nil, service.ErrAuditLogNotFound
}
func (r *auditCaptureRepository) Count(context.Context) (int64, error) { return 0, nil }
func (r *auditCaptureRepository) TruncateAll(context.Context) error    { return nil }
func (r *auditCaptureRepository) DeleteBefore(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func TestRiskControlArchiveOperationsUseStableAuditedActions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repository := &auditCaptureRepository{}
	auditService := service.NewAuditLogService(repository, nil)
	auditService.Start()

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyUser), AuthSubject{UserID: 77})
		c.Set(string(ContextKeyUserRole), "admin")
		c.Next()
	})
	router.Use(gin.HandlerFunc(NewAuditLogMiddleware(auditService)))
	router.GET("/api/v1/admin/risk-control/logs/:id/archive/preview", func(c *gin.Context) {
		SetAuditExtra(c, map[string]any{
			"result": "success", "token": "audit-canary-secret", "raw_prompt": "audit-canary-prompt",
		})
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	router.GET("/api/v1/admin/risk-control/logs/:id/archive/download", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json", []byte(`{"ok":true}`))
	})
	router.DELETE("/api/v1/admin/risk-control/logs/:id/archive", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"deleted": true})
	})

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/risk-control/logs/41/archive/preview", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/risk-control/logs/41/archive/download", nil),
		httptest.NewRequest(http.MethodDelete, "/api/v1/admin/risk-control/logs/41/archive", nil),
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusOK, recorder.Code)
	}
	auditService.Stop()

	repository.mu.Lock()
	logs := append([]*service.AuditLog(nil), repository.logs...)
	repository.mu.Unlock()
	require.Len(t, logs, 3)

	byAction := make(map[string]*service.AuditLog, len(logs))
	for _, entry := range logs {
		byAction[entry.Action] = entry
		require.Empty(t, entry.RequestBody)
		require.NotContains(t, entry.RequestBody, "audit-canary")
		require.NotContains(t, entry.Extra, "token")
		require.NotContains(t, entry.Extra, "raw_prompt")
		require.Equal(t, map[string]string{"id": "41"}, entry.Extra["params"])
	}

	require.Equal(t, "success", byAction["admin.risk_control.archive.preview"].Extra["result"])
	require.NotNil(t, byAction["admin.risk_control.archive.download"])
	require.NotNil(t, byAction["admin.risk_control.archive.delete"])
}

func TestRiskControlArchiveAuditRoutesHaveStableActions(t *testing.T) {
	expected := map[string]string{
		"GET /api/v1/admin/risk-control/logs/:id/archive/preview":  "admin.risk_control.archive.preview",
		"GET /api/v1/admin/risk-control/logs/:id/archive/download": "admin.risk_control.archive.download",
		"DELETE /api/v1/admin/risk-control/logs/:id/archive":       "admin.risk_control.archive.delete",
	}
	for route, action := range expected {
		if strings.HasPrefix(route, "GET ") {
			require.Equal(t, action, auditSensitiveReads[route])
		} else {
			require.Equal(t, action, auditActionOverrides[route])
		}
	}
}

func TestPasskeyLoginAuditUsesCanonicalLoginActionAndOmitsCredentialBody(t *testing.T) {
	route := "POST /api/v1/auth/passkey/login/finish"
	require.Equal(t, service.AuditActionLogin, auditActionOverrides[route])
	require.Contains(t, auditBodyOmittedRoutes, route)
}

// Ollama 会话保存的请求体整体就是浏览器 Cookie 明文，键级脱敏清单曾漏掉裸键
// "session"，必须走整体不入库路径，防止会话凭证长期留存在 audit_logs。
func TestOllamaCloudUsageSessionRouteOmitsAuditBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	require.Contains(t, auditBodyOmittedRoutes, "PUT /api/v1/admin/accounts/:id/ollama-cloud-usage/session")

	repository := &auditCaptureRepository{}
	auditService := service.NewAuditLogService(repository, nil)
	auditService.Start()

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyUser), AuthSubject{UserID: 77})
		c.Set(string(ContextKeyUserRole), "admin")
		c.Next()
	})
	router.Use(gin.HandlerFunc(NewAuditLogMiddleware(auditService)))
	router.PUT("/api/v1/admin/accounts/:id/ollama-cloud-usage/session", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	request := httptest.NewRequest(http.MethodPut, "/api/v1/admin/accounts/7/ollama-cloud-usage/session",
		bytes.NewBufferString(`{"session":"wos-session=audit-canary-cookie; __Secure-authjs.session-token.0=audit-canary-shard"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	auditService.Stop()

	repository.mu.Lock()
	logs := append([]*service.AuditLog(nil), repository.logs...)
	repository.mu.Unlock()
	require.Len(t, logs, 1)
	require.Equal(t, "<credential-bearing body omitted>", logs[0].RequestBody)
	require.NotContains(t, logs[0].RequestBody, "audit-canary")
}
