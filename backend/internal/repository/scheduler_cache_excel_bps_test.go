package repository

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestFilterSchedulerExtraKeepsExcelBPSKeys(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		account := service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Extra: map[string]any{
			"openai_excel_bps": true, "openai_excel_bps_auto_disable_on_403": enabled,
			"unrelated": "drop",
		}}
		filtered := filterSchedulerExtra(account.Extra)
		require.Equal(t, true, filtered["openai_excel_bps"])
		require.Equal(t, enabled, filtered["openai_excel_bps_auto_disable_on_403"])
		require.NotContains(t, filtered, "unrelated")
	}
}

func TestFilterSchedulerExtraKeepsExcelBPSModelScope(t *testing.T) {
	for _, tc := range []struct {
		name       string
		scoped     bool
		models     any
		astra, sol bool
	}{
		{"legacy", false, nil, true, true},
		{"astra only", true, []string{"gpt-6-astra"}, true, false},
		{"empty", true, []string{}, false, false},
		{"null stays scoped", true, nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Extra: map[string]any{"openai_excel_bps": true}}
			if tc.scoped {
				account.Extra["openai_excel_bps_models"] = tc.models
			}
			payload, err := json.Marshal(buildSchedulerMetadataAccount(account))
			require.NoError(t, err)
			var restored service.Account
			require.NoError(t, json.Unmarshal(payload, &restored))
			require.Equal(t, tc.astra, restored.IsExcelBPSEnabledForModel("gpt-6-astra"))
			require.Equal(t, tc.sol, restored.IsExcelBPSEnabledForModel("gpt-6-sol"))
		})
	}
}
