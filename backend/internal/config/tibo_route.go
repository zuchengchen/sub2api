package config

import (
	"fmt"
	"time"
)

// OpenAITiboRouteConfig tunes Tibo HTTP probing, vote-confirmed verdict
// flips and scheduler tiering. Zero durations/counts fall back to the
// built-in defaults; see OpenAIGatewayService.openAITiboRouteConfig.
type OpenAITiboRouteConfig struct {
	// Interval is the base delay between regular probes. Spread is the
	// absolute ± window (uniform). Regular probes land in
	// [Interval-Spread, Interval+Spread]. ConfirmSpacing is not spread.
	Interval time.Duration `mapstructure:"interval"`
	Spread   time.Duration `mapstructure:"spread"`
	// HealthyInterval / DegradedInterval / UnknownBackoff / Jitter are
	// accepted for older configs; regular cadence uses Interval and Spread.
	HealthyInterval  time.Duration   `mapstructure:"healthy_interval"`
	DegradedInterval time.Duration   `mapstructure:"degraded_interval"`
	UnknownBackoff   []time.Duration `mapstructure:"unknown_backoff"`
	Jitter           float64         `mapstructure:"jitter"`
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
	// ActiveWindow is accepted for older configs. Background probes cover
	// every eligible ChatGPT OAuth account, including idle ones.
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
		"interval": cfg.Interval, "spread": cfg.Spread,
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
