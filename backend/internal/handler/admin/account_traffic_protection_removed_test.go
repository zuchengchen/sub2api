package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type removedPolicyAdminService struct {
	*stubAdminService
	input *service.UpdateAccountInput
}

func (s *removedPolicyAdminService) UpdateAccount(_ context.Context, id int64, input *service.UpdateAccountInput) (*service.Account, error) {
	s.input = input
	return &service.Account{ID: id, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Concurrency: *input.Concurrency, Extra: input.Extra}, nil
}
func TestAccountTrafficProtectionRemovedOrdinaryUpdateAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, old := range []any{nil, false, true, "malformed", map[string]any{"strict_rpm_enabled": true, "rpm": -1}} {
		svc := &removedPolicyAdminService{stubAdminService: newStubAdminService()}
		h := NewAccountHandler(svc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
		r := gin.New()
		r.PUT("/accounts/:id", h.Update)
		extra := map[string]any{"account_traffic_control": old, "anti_degradation": old, "codex_fingerprint_mode": "full", "codex_image_generation_bridge": true}
		body, err := json.Marshal(map[string]any{"name": "saved", "concurrency": 50, "extra": extra})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPut, "/accounts/7", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.NotNil(t, svc.input)
		require.Equal(t, 50, *svc.input.Concurrency)
		require.Equal(t, "full", svc.input.Extra["codex_fingerprint_mode"])
		require.Equal(t, true, svc.input.Extra["codex_image_generation_bridge"])
		var response struct {
			Data map[string]any `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
		require.NotContains(t, response.Data, "anti_degradation")
		require.NotContains(t, response.Data, "protection_scope")
	}
}
