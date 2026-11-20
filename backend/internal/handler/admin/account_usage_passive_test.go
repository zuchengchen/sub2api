package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type passiveUsageHandlerAccounts struct{ service.AccountRepository }

func (*passiveUsageHandlerAccounts) GetByIDs(context.Context, []int64) ([]*service.Account, error) {
	return []*service.Account{{
		ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Extra: map[string]any{"codex_5h_used_percent": 48.0},
	}}, nil
}

func TestGetBatchUsagePassiveDoesNotForceAnActiveQuery(t *testing.T) {
	usage := service.NewAccountUsageService(&passiveUsageHandlerAccounts{}, nil, nil, nil, nil, nil, service.NewUsageCache(), nil, nil)
	handler := AccountHandler{accountUsageService: usage}
	router := gin.New()
	router.POST("/accounts/usage/batch", handler.GetBatchUsage)
	req := httptest.NewRequest(http.MethodPost, "/accounts/usage/batch", strings.NewReader(`{"account_ids":[1],"source":"passive","force":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var result struct {
		Data struct {
			Usage map[string]service.UsageInfo `json:"usage"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	require.Equal(t, "passive", result.Data.Usage["1"].Source)
	require.Equal(t, 48.0, result.Data.Usage["1"].FiveHour.Utilization)
}
