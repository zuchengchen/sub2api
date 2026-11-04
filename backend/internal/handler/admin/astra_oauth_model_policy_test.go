package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAstraOAuthModelPolicyAdminCatalogAndListDTOAgree(t *testing.T) {
	svc := &availableModelsAdminService{stubAdminService: newStubAdminService(), account: service.Account{ID: 42, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Credentials: map[string]any{"model_mapping": map[string]any{"gpt-6-astra": "gpt-6-astra"}}}}
	router := setupAvailableModelsRouter(svc)
	for _, verdict := range []string{"unknown", "healthy", "degraded", "healthy"} {
		svc.account.Extra = map[string]any{service.OpenAIAstraPolicyExtraKey: map[string]any{"model": "gpt-6-astra", "verdict": "degraded", "recent_samples": []map[string]any{{"verdict": verdict, "at": "2020-01-01T00:00:00Z"}}}}
		full := dto.AccountFromService(&svc.account)
		lite := dto.AccountListItemFromAccount(full)
		require.Equal(t, verdict, full.OpenAIOAuthModelPolicy.Verdict)
		require.Equal(t, full.OpenAIOAuthModelPolicy, lite.OpenAIOAuthModelPolicy)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/42/models", nil))
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
