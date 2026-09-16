package handler

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

var (
	securityPolicyServiceMu sync.RWMutex
	securityPolicyService   *service.SecurityPolicyService
)

func SetSecurityPolicyService(svc *service.SecurityPolicyService) {
	securityPolicyServiceMu.Lock()
	securityPolicyService = svc
	securityPolicyServiceMu.Unlock()
}

func currentSecurityPolicyService() *service.SecurityPolicyService {
	securityPolicyServiceMu.RLock()
	defer securityPolicyServiceMu.RUnlock()
	return securityPolicyService
}

// checkGroupSecurityPolicy is the extra gate before unified content audit.
// Default-off groups are a no-op. Hits short-circuit and never auto-ban.
func checkGroupSecurityPolicy(c *gin.Context, securityPolicy *service.SecurityPolicyService, apiKey *service.APIKey, protocol, model string, body []byte) *service.ContentModerationDecision {
	if securityPolicy == nil || apiKey == nil {
		return nil
	}
	group := apiKey.Group
	if group == nil || !group.SecurityPolicyEnabled {
		return nil
	}
	mode := service.NormalizeSecurityPolicyMode(group.SecurityPolicyMode)
	verdict := securityPolicy.EvaluateRequest(c.Request.Context(), c, service.SecurityPolicyRequest{
		APIKey:    apiKey,
		Protocol:  protocol,
		Model:     model,
		Endpoint:  GetInboundEndpoint(c),
		Body:      body,
		RequestID: c.Writer.Header().Get("X-Request-Id"),
		ClientIP:  strings.TrimSpace(ip.GetClientIP(c)),
		UserAgent: c.GetHeader("User-Agent"),
	})
	if verdict == nil || verdict.Allowed {
		return nil
	}
	record := service.SecurityPolicyHitRecord{
		RequestID:   c.Writer.Header().Get("X-Request-Id"),
		APIKey:      apiKey,
		Protocol:    protocol,
		Model:       model,
		Endpoint:    GetInboundEndpoint(c),
		Verdict:     verdict,
		SessionMode: mode,
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		securityPolicy.RecordHit(ctx, record)
	}()
	action := service.SecurityPolicyActionBlock
	if verdict.SessionTerminated {
		action = service.SecurityPolicyActionSessionBlock
	}
	return &service.ContentModerationDecision{
		Allowed:    false,
		Blocked:    true,
		Flagged:    true,
		Message:    verdict.ClientMessage,
		StatusCode: http.StatusForbidden,
		Action:     action,
	}
}
