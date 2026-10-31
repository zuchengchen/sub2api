//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type unifiedPlazaUserRepo struct {
	service.UserRepository
	vip bool
}

func (r unifiedPlazaUserRepo) GetByID(context.Context, int64) (*service.User, error) {
	return &service.User{ID: 1, IsVIP: r.vip}, nil
}

type unifiedPlazaSubscriptionRepo struct {
	service.UserSubscriptionRepository
}

func (unifiedPlazaSubscriptionRepo) ListActiveByUserID(context.Context, int64) ([]service.UserSubscription, error) {
	return nil, nil
}

type unifiedPlazaGroupRepo struct {
	service.GroupRepository
	name string
}

func (r unifiedPlazaGroupRepo) ListActive(context.Context) ([]service.Group, error) {
	return []service.Group{{ID: 7, Name: r.name, Platform: service.PlatformOpenAI, RateMultiplier: 0.1}}, nil
}

type unifiedPlazaChannelRepo struct{ service.ChannelRepository }

func (unifiedPlazaChannelRepo) ListAll(context.Context) ([]service.Channel, error) {
	return []service.Channel{{ID: 1, Status: service.StatusActive, GroupIDs: []int64{7},
		ModelPricing: []service.ChannelModelPricing{{Platform: service.PlatformOpenAI, BillingMode: service.BillingModeToken,
			Models: []string{"gpt-5.1", "gpt-5.6-luna", "gpt-6-luna"}}}}}, nil
}

type unifiedPlazaRateRepo struct {
	service.UserGroupRateRepository
	rate *float64
}

func (r unifiedPlazaRateRepo) GetByUserID(context.Context, int64) (map[int64]float64, error) {
	if r.rate == nil {
		return nil, nil
	}
	return map[int64]float64{7: *r.rate}, nil
}

func TestUnifiedUserRateModelPlazaHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	zero, low, high := 0.0, 0.07, 0.3
	for _, groupName := range []string{"gpt-pro", "GPT-PRO", "other"} {
		for _, rate := range []*float64{nil, &zero, &low, &high} {
			var plain modelPlazaResponse
			for _, vip := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/rate=%v/vip=%t", groupName, rate, vip), func(t *testing.T) {
					groups := unifiedPlazaGroupRepo{name: groupName}
					keys := service.NewAPIKeyService(nil, unifiedPlazaUserRepo{vip: vip}, groups, unifiedPlazaSubscriptionRepo{}, unifiedPlazaRateRepo{rate: rate}, nil, nil)
					h := NewModelPlazaHandler(service.NewModelPlazaService(unifiedPlazaChannelRepo{}, groups, nil, nil, nil), keys,
						service.NewSettingService(plazaVarySettingRepo{}, &config.Config{}))
					w := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(w)
					c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/model-plaza", nil)
					c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
					h.Get(c)
					require.Equal(t, http.StatusOK, w.Code, w.Body.String())
					var body struct {
						Code int                `json:"code"`
						Data modelPlazaResponse `json:"data"`
					}
					require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
					require.Zero(t, body.Code)
					require.Len(t, body.Data.Groups, 1)
					group := body.Data.Groups[0]
					require.Equal(t, 0.1, group.RateMultiplier)
					require.Equal(t, rate, group.UserRateMultiplier)
					require.Len(t, group.Models, 3)
					for _, model := range group.Models {
						if model.Name == "gpt-5.1" || (rate != nil && *rate >= 0.2) {
							require.Nil(t, model.RateMultiplier)
						} else {
							require.NotNil(t, model.RateMultiplier)
							require.Equal(t, 0.2, *model.RateMultiplier)
						}
					}
					if vip {
						require.Equal(t, plain, body.Data)
					} else {
						plain = body.Data
					}
				})
			}
		}
	}
}
