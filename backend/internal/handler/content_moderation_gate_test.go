package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type contentModerationWhitelistSettingRepo struct {
	values map[string]string
}

func (r *contentModerationWhitelistSettingRepo) Get(_ context.Context, key string) (*service.Setting, error) {
	value, ok := r.values[key]
	if !ok {
		return nil, service.ErrSettingNotFound
	}
	return &service.Setting{Key: key, Value: value}, nil
}

func (r *contentModerationWhitelistSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	value, ok := r.values[key]
	if !ok {
		return "", service.ErrSettingNotFound
	}
	return value, nil
}

func (r *contentModerationWhitelistSettingRepo) Set(_ context.Context, key, value string) error {
	r.values[key] = value
	return nil
}

func (r *contentModerationWhitelistSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			values[key] = value
		}
	}
	return values, nil
}

func (r *contentModerationWhitelistSettingRepo) SetMultiple(_ context.Context, values map[string]string) error {
	for key, value := range values {
		r.values[key] = value
	}
	return nil
}

func (r *contentModerationWhitelistSettingRepo) GetAll(_ context.Context) (map[string]string, error) {
	values := make(map[string]string, len(r.values))
	for key, value := range r.values {
		values[key] = value
	}
	return values, nil
}

func (r *contentModerationWhitelistSettingRepo) Delete(_ context.Context, key string) error {
	delete(r.values, key)
	return nil
}

func TestContentModerationErrorReturnsMatchedKeyword(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	h := &OpenAIGatewayHandler{}
	h.openAIContentModerationError(c, &service.ContentModerationDecision{
		Blocked:         true,
		Flagged:         true,
		Message:         "内容审计命中风险规则，请调整输入后重试",
		StatusCode:      http.StatusForbidden,
		MatchedKeyword:  "secret-token",
		HighestCategory: "keyword",
		Action:          service.ContentModerationActionKeywordBlock,
	})

	require.Equal(t, http.StatusForbidden, recorder.Code)
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.Equal(t, "content_policy_violation", payload.Error.Type)
	require.Equal(t, "内容审计命中风险规则，请调整输入后重试（命中敏感词：secret-token）", payload.Error.Message)
}

func TestContentModerationDecisionMessageReturnsNonKeywordReason(t *testing.T) {
	require.Equal(t,
		"内容审计命中风险规则，请调整输入后重试（违规类型：jailbreak）",
		contentModerationDecisionMessage(&service.ContentModerationDecision{
			Blocked: true, Message: "内容审计命中风险规则，请调整输入后重试",
			HighestCategory: "jailbreak", Action: service.ContentModerationActionSecondLayerBlock,
		}),
	)
	require.Equal(t,
		"内容审计命中风险规则，请调整输入后重试（命中历史风险内容）",
		contentModerationDecisionMessage(&service.ContentModerationDecision{
			Blocked: true, Message: "内容审计命中风险规则，请调整输入后重试",
			Action: service.ContentModerationActionCacheBlock,
		}),
	)
}

func TestContentModerationWSCloseReasonTruncatesAtUTF8Boundary(t *testing.T) {
	reason := contentModerationWSCloseReason(&service.ContentModerationDecision{
		Blocked:        true,
		Message:        "内容审计命中风险规则，请调整输入后重试",
		MatchedKeyword: strings.Repeat("敏感词", 100),
		Action:         service.ContentModerationActionKeywordBlock,
	})

	require.LessOrEqual(t, len(reason), 120)
	require.True(t, utf8.ValidString(reason))
	require.Contains(t, reason, "命中敏感词")
}

// Ported from upstream's websocket security-audit logging fix: dedupe-cache
// hits must still emit an audit log entry (cached=true) instead of returning
// silently.
func TestRunUnifiedContentModerationLogsWebSocketChecksAndCacheHits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := service.NewContentModerationService(nil, nil, nil, nil, nil, nil, nil, nil)
	core, logs := observer.New(zap.InfoLevel)
	reqLog := zap.New(core)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(contentModerationWSTurnContextKey, 2)
	payload := []byte(`{"type":"response.create","response":{"input":"same turn"}}`)

	first := runUnifiedContentModeration(c, reqLog, svc, nil, middleware2.AuthSubject{UserID: 7}, "openai_responses", "gpt-test", payload, "subsequent_turn")
	second := runUnifiedContentModeration(c, reqLog, svc, nil, middleware2.AuthSubject{UserID: 7}, "openai_responses", "gpt-test", payload, "subsequent_turn")
	require.NotNil(t, first)
	require.NotNil(t, second)

	startLogs := logs.FilterMessage("content_moderation.gateway_check_start").All()
	require.Len(t, startLogs, 1)
	require.Equal(t, false, startLogs[0].ContextMap()["cached"])

	doneLogs := logs.FilterMessage("content_moderation.gateway_check_done").All()
	require.Len(t, doneLogs, 2)
	require.Equal(t, false, doneLogs[0].ContextMap()["cached"])
	require.Equal(t, true, doneLogs[1].ContextMap()["cached"])
	require.Equal(t, true, doneLogs[1].ContextMap()["allowed"])
	require.Equal(t, "subsequent_turn", doneLogs[1].ContextMap()["stage"])
}

func TestRunContentModerationStage_UserEmailWhitelistRemainsInScopeAndUsesBodyBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := map[string]any{
		"enabled":              true,
		"mode":                 "pre_block",
		"user_email_whitelist": []string{"allowed@example.com"},
	}
	rawCfg, err := json.Marshal(cfg)
	require.NoError(t, err)
	svc := service.NewContentModerationService(
		&contentModerationWhitelistSettingRepo{values: map[string]string{
			service.SettingKeyRiskControlEnabled:      "true",
			service.SettingKeyContentModerationConfig: string(rawCfg),
		}},
		nil, nil, nil, nil, nil, nil, nil,
	)
	svc.SetPendingRequestBodyBudgetForTest(1)
	groupID := int64(7)
	apiKey := &service.APIKey{
		ID:      11,
		Name:    "whitelisted-key",
		GroupID: &groupID,
		Group:   &service.Group{ID: groupID, Name: "GPT Production"},
		User:    &service.User{ID: 42, Email: "Allowed@Example.COM"},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	decision := runContentModerationStage(
		c, zap.NewNop(), svc, apiKey, middleware2.AuthSubject{UserID: 42},
		service.ContentModerationProtocolOpenAIResponses, "gpt-5.5", []byte(`{"input":"larger than budget"}`), "http",
	)

	require.NotNil(t, decision)
	require.False(t, decision.Allowed)
	require.False(t, decision.Blocked)
	require.Equal(t, http.StatusServiceUnavailable, decision.StatusCode)
	require.Equal(t, service.ContentModerationActionBudgetRejected, decision.Action)
	require.Equal(t, int64(0), svc.PendingRequestBodyBytes())
	require.True(t, contentModerationScopeSnapshot(c, apiKey).InScope)
}
