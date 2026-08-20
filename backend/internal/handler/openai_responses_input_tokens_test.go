package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestResponsesInputTokensIsClassifiedAsTokenCountRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", nil)

	require.True(t, isCountTokensRequest(c))
	require.True(t, isTokenCountRequestPath("/responses/input_tokens"))
	require.False(t, isTokenCountRequestPath("/v1/responses"))
}

func TestResponsesInputTokensContentModerationBlocksBeforeBillingAndForward(t *testing.T) {
	gin.SetMode(gin.TestMode)
	moderationRepo := &contentModerationHandlerTestRepo{}
	moderationService := service.NewContentModerationService(
		&contentModerationHandlerSettingRepo{values: map[string]string{
			service.SettingKeyRiskControlEnabled: "true",
			service.SettingKeyContentModerationConfig: `{
				"enabled": true,
				"mode": "pre_block",
				"all_groups": true,
				"hard_block_patterns": ["bad prompt"],
				"keyword_blocking_mode": "keyword_only",
				"first_layer_stage": "enforce",
				"second_layer_enabled": false,
				"block_message": "content moderation test block"
			}`,
		}},
		moderationRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)

	groupID := int64(7)
	apiKey := &service.APIKey{
		ID:      11,
		GroupID: &groupID,
		Group:   &service.Group{ID: groupID, Name: "GPT-test", Platform: service.PlatformOpenAI},
		User:    &service.User{ID: 42, Email: "user@example.com"},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", strings.NewReader(
		`{"model":"gpt-5.5","input":"bad prompt"}`,
	))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware2.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: apiKey.User.ID})

	h := &OpenAIGatewayHandler{
		gatewayService:           &service.OpenAIGatewayService{},
		billingCacheService:      &service.BillingCacheService{},
		apiKeyService:            &service.APIKeyService{},
		contentModerationService: moderationService,
		concurrencyHelper:        &ConcurrencyHelper{concurrencyService: &service.ConcurrencyService{}},
	}
	h.ResponsesInputTokens(c)

	require.Equal(t, http.StatusForbidden, recorder.Code)
	var response struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Equal(t, "content_policy_violation", response.Error.Type)
	require.Eventually(t, func() bool {
		return len(moderationRepo.logSnapshot()) == 1
	}, time.Second, 10*time.Millisecond)
}
