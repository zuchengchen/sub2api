package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoadOpenAITiboRouteDefaults(t *testing.T) {
	resetViperWithJWTSecret(t)
	cfg, err := Load()
	require.NoError(t, err)
	got := cfg.Gateway.OpenAITiboRoute
	require.Equal(t, 10*time.Minute, got.HealthyInterval)
	require.Equal(t, 5*time.Minute, got.DegradedInterval)
	require.Equal(t, []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute}, got.UnknownBackoff)
	require.InDelta(t, 0.2, got.Jitter, 1e-9)
	require.Equal(t, 3, got.ConfirmSamples)
	require.Equal(t, 20*time.Second, got.ConfirmSpacing)
	require.Equal(t, 10*time.Minute, got.MinDegradedDwell)
	require.Equal(t, 30*time.Minute, got.MaxStale)
	require.Equal(t, 30*time.Minute, got.ActiveWindow)
	require.Equal(t, 20, got.MaxProbesPerHour)
	require.Equal(t, 4, got.ProbeConcurrency)
	require.Equal(t, OpenAITiboBPSProbeOff, got.BPSProbeMode)
	require.Equal(t, 10*time.Minute, got.BPSProbeInterval)
	require.False(t, got.SchedulerPreferHealthyRoute)
}

func TestLoadOpenAITiboRouteFromFileAndEnv(t *testing.T) {
	resetViperWithJWTSecret(t)
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(configFile, []byte("gateway:\n  openai_tibo_route:\n    confirm_spacing: 45s\n    unknown_backoff: [30s, 90s]\n    bps_probe_mode: shadow\n"), 0o600))
	t.Setenv("CONFIG_FILE", configFile)
	t.Setenv("GATEWAY_OPENAI_TIBO_ROUTE_SCHEDULER_PREFER_HEALTHY_ROUTE", "true")
	t.Setenv("GATEWAY_OPENAI_TIBO_ROUTE_MAX_STALE", "20m")
	cfg, err := Load()
	require.NoError(t, err)
	got := cfg.Gateway.OpenAITiboRoute
	require.Equal(t, 45*time.Second, got.ConfirmSpacing)
	require.Equal(t, []time.Duration{30 * time.Second, 90 * time.Second}, got.UnknownBackoff)
	require.Equal(t, OpenAITiboBPSProbeShadow, got.BPSProbeMode)
	require.True(t, got.SchedulerPreferHealthyRoute)
	require.Equal(t, 20*time.Minute, got.MaxStale)
}

func TestValidateOpenAITiboRoute(t *testing.T) {
	require.NoError(t, validateOpenAITiboRoute(OpenAITiboRouteConfig{}))
	require.NoError(t, validateOpenAITiboRoute(OpenAITiboRouteConfig{BPSProbeMode: "Enforce", Jitter: 0.2, ConfirmSamples: 3, DegradedAlertRatio: 0.5, MinDegradedDwell: -time.Second}))
	for name, cfg := range map[string]OpenAITiboRouteConfig{
		"mode":        {BPSProbeMode: "always"},
		"interval":    {HealthyInterval: -time.Second},
		"backoff":     {UnknownBackoff: []time.Duration{time.Minute, 0}},
		"jitter":      {Jitter: 1},
		"samples":     {ConfirmSamples: 10},
		"budget":      {MaxProbesPerHour: -1},
		"alert_ratio": {DegradedAlertRatio: 1.5},
	} {
		require.Error(t, validateOpenAITiboRoute(cfg), name)
	}
}
