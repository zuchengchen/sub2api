package config

import (
	"fmt"
	"time"
)

// OpenAITiboRouteConfig tunes Tibo route selection for Cookie WS accounts:
// background probing, vote-confirmed verdict flips and route tiering.
// Zero durations/counts fall back to the built-in defaults; see
// OpenAIGatewayService.openAITiboRouteConfig.
type OpenAITiboRouteConfig struct {
	// Probe cadence per confirmed verdict. Each interval gets ±Jitter.
	HealthyInterval  time.Duration `mapstructure:"healthy_interval"`
	DegradedInterval time.Duration `mapstructure:"degraded_interval"`
	// UnknownBackoff is the retry ladder after unknown samples (non-200,
	// timeout, 429, token failure). The last step repeats; Retry-After wins
	// when it is later.
	UnknownBackoff []time.Duration `mapstructure:"unknown_backoff"`
	// Jitter is the ± fraction applied to every probe delay (0 disables).
	Jitter float64 `mapstructure:"jitter"`
	// ConfirmSamples is the vote window for a verdict flip; a majority of it
	// must agree. ConfirmSpacing separates the confirmation probes.
	ConfirmSamples int           `mapstructure:"confirm_samples"`
	ConfirmSpacing time.Duration `mapstructure:"confirm_spacing"`
	// MinDegradedDwell delays degraded -> healthy after a flip to degraded.
	// Negative disables the dwell.
	MinDegradedDwell time.Duration `mapstructure:"min_degraded_dwell"`
	// MaxStale keeps the last confirmed verdict while samples are unknown or
	// missing; after it the effective state is unknown.
	MaxStale time.Duration `mapstructure:"max_stale"`
	// ActiveWindow limits background probes to accounts that served a request
	// within this window, so idle accounts cost nothing.
	ActiveWindow time.Duration `mapstructure:"active_window"`
	// MaxProbesPerHour caps probes per account and route (including
	// confirmation probes). ProbeConcurrency caps probes process-wide.
	MaxProbesPerHour int `mapstructure:"max_probes_per_hour"`
	ProbeConcurrency int `mapstructure:"probe_concurrency"`
	// SchedulerPreferHealthyRoute soft-orders scheduler candidates by route
	// health tier (healthy > unknown > degraded) and releases degraded sticky
	// sessions when a healthy account exists.
	SchedulerPreferHealthyRoute bool `mapstructure:"scheduler_prefer_healthy_route"`
	// DegradedAlertRatio logs a warning when the share of Tibo-routed requests
	// served on the degraded HTTP fallback within the current hour exceeds it
	// (0 disables). DegradedAlertMinRequests avoids alerts on tiny samples.
	DegradedAlertRatio       float64 `mapstructure:"degraded_alert_ratio"`
	DegradedAlertMinRequests int     `mapstructure:"degraded_alert_min_requests"`
}

func validateOpenAITiboRoute(cfg OpenAITiboRouteConfig) error {
	for name, value := range map[string]time.Duration{
		"healthy_interval": cfg.HealthyInterval, "degraded_interval": cfg.DegradedInterval,
		"confirm_spacing": cfg.ConfirmSpacing, "max_stale": cfg.MaxStale,
		"active_window": cfg.ActiveWindow,
	} {
		if value < 0 {
			return fmt.Errorf("gateway.openai_tibo_route.%s must be non-negative", name)
		}
	}
	for _, step := range cfg.UnknownBackoff {
		if step <= 0 {
			return fmt.Errorf("gateway.openai_tibo_route.unknown_backoff steps must be positive")
		}
	}
	if cfg.Jitter < 0 || cfg.Jitter >= 1 {
		return fmt.Errorf("gateway.openai_tibo_route.jitter must be within [0,1)")
	}
	if cfg.ConfirmSamples < 0 || cfg.ConfirmSamples > 9 {
		return fmt.Errorf("gateway.openai_tibo_route.confirm_samples must be within [0,9]")
	}
	if cfg.MaxProbesPerHour < 0 || cfg.ProbeConcurrency < 0 || cfg.DegradedAlertMinRequests < 0 {
		return fmt.Errorf("gateway.openai_tibo_route probe limits must be non-negative")
	}
	if cfg.DegradedAlertRatio < 0 || cfg.DegradedAlertRatio > 1 {
		return fmt.Errorf("gateway.openai_tibo_route.degraded_alert_ratio must be within [0,1]")
	}
	return nil
}
