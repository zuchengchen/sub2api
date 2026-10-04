package repository

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestFilterSchedulerExtraKeepsPrismBrowserKeys(t *testing.T) {
	account := service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Extra: map[string]any{
		"openai_prism_browser": false, service.PrismBrowserModelsKey: []string{"gpt-6.1-sol"}, "unrelated": "drop",
	}}
	filtered := filterSchedulerExtra(account.Extra)
	require.Equal(t, false, filtered["openai_prism_browser"])
	require.Equal(t, []string{"gpt-6.1-sol"}, filtered[service.PrismBrowserModelsKey])
	require.NotContains(t, filtered, "unrelated")
}

func TestBuildSchedulerMetadataAccountKeepsPrismForceOff(t *testing.T) {
	account := service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Extra: map[string]any{
		"openai_prism_browser": false, "unrelated": "drop",
	}}
	require.False(t, serviceAccountHasPrism(account))
	payload, err := json.Marshal(buildSchedulerMetadataAccount(account))
	require.NoError(t, err)
	var restored service.Account
	require.NoError(t, json.Unmarshal(payload, &restored))
	require.False(t, serviceAccountHasPrism(restored))
	require.NotContains(t, restored.Extra, "unrelated")
}

func serviceAccountHasPrism(account service.Account) bool {
	if account.Extra == nil {
		return true
	}
	raw, ok := account.Extra["openai_prism_browser"]
	if !ok {
		return true
	}
	enabled, isBool := raw.(bool)
	return isBool && enabled
}
