package admin

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountResponseCodexTicketsUsesConfiguredPolicy(t *testing.T) {
	account := &service.Account{ID: 41, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}
	h := &AccountHandler{cfg: &config.Config{}}
	require.Empty(t, h.accountResponseFromService(account).CodexTurnTickets)
	require.Empty(t, h.accountListResponseFromService(account).CodexTurnTickets)
	h.cfg.Gateway.OpenAICodexTicket = config.OpenAICodexTicketConfig{Enabled: true, Models: []string{"configured-model"}, FailClosed: false}
	status := h.accountListResponseFromService(account).CodexTurnTickets
	require.Len(t, status, 1)
	require.Equal(t, "configured-model", status[0].Model)
	require.False(t, status[0].Blocked)
	h.cfg.Gateway.OpenAICodexTicket.FailClosed = true
	require.True(t, h.accountResponseFromService(account).CodexTurnTickets[0].Blocked)
}

func TestAccountResponseCookieStatusDoesNotTrustPersistedVerification(t *testing.T) {
	now := time.Now().Add(-time.Minute)
	identity := map[string]any{}
	for _, key := range []string{"version", "user_agent", "originator", "installation_id", "window_id", "session_id", "thread_id", "client_request_id", "routing_hint"} {
		identity[key] = "private-profile"
	}
	account := &service.Account{
		ID: 41, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Schedulable: true,
		Extra: map[string]any{"codex_cookie_ws:gpt-6-astra": map[string]any{
			"account_id": 41, "slot": 0, "model": "gpt-6-astra", "generation": "private-generation",
			"cookies": "private-cookie", "identity": identity,
			"captured_at": now, "refresh_at": now.Add(50 * time.Minute), "expires_at": now.Add(time.Hour),
			"http_verified": true, "ws_verified": true,
		}},
	}
	h := &AccountHandler{cfg: &config.Config{}}
	h.cfg.Gateway.OpenAICodexTicket = config.OpenAICodexTicketConfig{Enabled: true, Mode: "cookie_ws"}
	for _, statuses := range [][]service.OpenAICodexTicketStatus{
		h.accountListResponseFromService(account).CodexTurnTickets,
		h.accountResponseFromService(account).CodexTurnTickets,
	} {
		require.Len(t, statuses, 1)
		status := statuses[0]
		require.False(t, status.Ready)
		require.True(t, status.Blocked)
		require.Zero(t, status.VerifiedWS)
		require.Zero(t, status.CookieGroupsReady)
		require.Equal(t, 1, status.CookieGroupsValid)
		require.Equal(t, "unavailable", status.RecoveryState)
		require.Len(t, status.CookieSlots, 3)
		data, err := json.Marshal(status)
		require.NoError(t, err)
		require.Contains(t, string(data), `"verified_ws":0`)
		require.NotContains(t, string(data), "private-")
	}
}

func TestAccountResponseCodexTicketsReadsLiveSettingsAfterRestart(t *testing.T) {
	cfg := &config.Config{}
	repo := &settingHandlerRepoStub{values: map[string]string{service.SettingKeyOpenAICodexTicketEnabled: "true"}}
	settings := service.NewSettingService(repo, cfg)
	h := &AccountHandler{cfg: cfg}
	h.SetCodexTicketSettings(settings)
	account := &service.Account{ID: 41, Platform: service.PlatformOpenAI, Type: service.AccountTypeSetupToken}
	require.Len(t, h.accountListResponseFromService(account).CodexTurnTickets, 2)
	require.False(t, cfg.Gateway.OpenAICodexTicket.Enabled)
	repo.values[service.SettingKeyOpenAICodexTicketEnabled] = "false"
	settings.InvalidateOpenAICodexTicketEnabledCache()
	require.Empty(t, h.accountResponseFromService(account).CodexTurnTickets)
}
