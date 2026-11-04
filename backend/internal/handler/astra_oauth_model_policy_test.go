package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func astraHandlerExtra(verdict string) map[string]any {
	return map[string]any{service.OpenAIAstraPolicyExtraKey: map[string]any{"model": "gpt-6-astra", "recent_samples": []map[string]any{{"verdict": verdict, "at": "2020-01-01T00:00:00Z"}}}}
}

func TestAstraOAuthModelPolicyPublicModelsRefreshWithoutAccountEdit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	group := &service.Group{ID: 28, Platform: service.PlatformOpenAI}
	account := service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Credentials: map[string]any{"model_mapping": map[string]any{"gpt-6-luna": "gpt-6-luna"}}}
	repo := &gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{group.ID: {account}}}
	handler := newGatewayModelsHandlerForTest(repo)
	for _, verdict := range []string{"unknown", "healthy", "degraded", "healthy"} {
		repo.byGroup[group.ID][0].Extra = astraHandlerExtra(verdict)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		c.Set("api_key", &service.APIKey{GroupID: &group.ID, Group: group})
		handler.Models(c)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		ids := map[string]bool{}
		for _, row := range gjson.GetBytes(rec.Body.Bytes(), "data").Array() {
			ids[row.Get("id").String()] = true
		}
		require.True(t, ids["gpt-6-astra"], verdict)
		require.True(t, ids["gpt-6-luna"], verdict)
		require.True(t, ids["gpt-image-2"], verdict)
	}
}
