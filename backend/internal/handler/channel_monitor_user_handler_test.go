package handler

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUserMonitorViewToItemIncludesGroupRates(t *testing.T) {
	groupID := int64(84)
	rate := 1.0
	item := userMonitorViewToItem(&service.UserMonitorView{
		ID:                 9,
		Name:               "DeepSeek",
		Provider:           "openai",
		GroupName:          "DeepSeek",
		GroupID:            &groupID,
		GroupPlatform:      "deepseek",
		SubscriptionType:   "standard",
		RateMultiplier:     &rate,
		PeakRateEnabled:    true,
		PeakStart:          "18:00",
		PeakEnd:            "22:00",
		PeakRateMultiplier: 1.2,
		PrimaryModel:       "deepseek-chat",
		PrimaryStatus:      "operational",
	}, false)
	raw, err := json.Marshal(item)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"group_id":84`)
	require.Contains(t, string(raw), `"group_platform":"deepseek"`)
	require.Contains(t, string(raw), `"rate_multiplier":1`)
	require.Contains(t, string(raw), `"peak_rate_multiplier":1.2`)
	require.NotContains(t, string(raw), `"latest_quota"`)
}
