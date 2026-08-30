//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// api_base_url is a single global value, so a deployment served under several
// domains advertised one hard-coded host everywhere. api_base_url_follow_host
// lets the frontend switch the displayed endpoint to the browsed domain, which
// only works if the flag survives parse → public settings → SSR injection.

func TestSettingService_ParseSettingsAPIBaseURLFollowHost(t *testing.T) {
	svc := NewSettingService(&settingUpdateRepoStub{}, &config.Config{})

	require.False(t, svc.parseSettings(map[string]string{}).APIBaseURLFollowHost,
		"must default to off so existing deployments keep the fixed endpoint")
	require.True(t, svc.parseSettings(map[string]string{
		SettingKeyAPIBaseURLFollowHost: "true",
	}).APIBaseURLFollowHost)
	require.False(t, svc.parseSettings(map[string]string{
		SettingKeyAPIBaseURLFollowHost: "false",
	}).APIBaseURLFollowHost)
}

// The flag must round-trip through UpdateSettings, otherwise saving the admin
// form would silently reset the toggle.
func TestSettingService_UpdateSettingsPersistsAPIBaseURLFollowHost(t *testing.T) {
	repo := &settingUpdateRepoStub{}
	svc := NewSettingService(repo, &config.Config{})

	updates, err := svc.buildSystemSettingsUpdates(context.Background(), &SystemSettings{
		APIBaseURL:           "https://key66.vip/v1",
		APIBaseURLFollowHost: true,
	})
	require.NoError(t, err)
	require.Equal(t, "true", updates[SettingKeyAPIBaseURLFollowHost])

	updates, err = svc.buildSystemSettingsUpdates(context.Background(), &SystemSettings{
		APIBaseURL: "https://key66.vip/v1",
	})
	require.NoError(t, err)
	require.Equal(t, "false", updates[SettingKeyAPIBaseURLFollowHost])
}

// The frontend reads api_base_url_follow_host off window.__APP_CONFIG__ during
// first paint; if injection drops it the endpoint flashes the wrong domain.
func TestPublicSettingsInjectionPayload_CarriesAPIBaseURLFollowHost(t *testing.T) {
	raw, err := json.Marshal(PublicSettingsInjectionPayload{
		APIBaseURL:           "https://key66.vip/v1",
		APIBaseURLFollowHost: true,
	})
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, true, decoded["api_base_url_follow_host"])
	require.Equal(t, "https://key66.vip/v1", decoded["api_base_url"])
}
