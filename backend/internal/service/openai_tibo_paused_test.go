package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newPausedTiboCase(t *testing.T) (*tiboRouteCase, *tiboAccountRepo) {
	t.Helper()
	tc := newTiboRouteCase(t, false, false, cookieWSHTTPResponse("True"))
	tc.account.Schedulable = false
	tc.account.Extra["codex_5h_used_percent"] = 20.0
	tc.account.Extra["codex_7d_used_percent"] = 30.0
	tc.account.Extra["codex_usage_updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	repo := mustTestValue[*tiboAccountRepo](t, tc.svc.accountRepo)
	repo.setAccount(func(a *Account) { *a = *cloneTiboAccount(tc.account) })
	// Make first enrollment deterministic without waiting for startup jitter.
	tc.svc.cfg.Gateway.OpenAITiboRoute.Interval = time.Nanosecond
	tc.svc.settingService = &SettingService{}
	tc.svc.settingService.SetOpenAIQuotaAutoPauseSettings(OpsOpenAIAccountQuotaAutoPauseSettings{})
	return tc, repo
}

func TestTiboPausedOAuthEligibility(t *testing.T) {
	now := time.Now()
	future, past := now.Add(time.Hour), now.Add(-time.Hour)
	for _, tt := range []struct {
		name     string
		mutate   func(*Account)
		settings OpsOpenAIAccountQuotaAutoPauseSettings
		allowed  bool
	}{
		{name: "available quota", allowed: true},
		{name: "disabled", mutate: func(a *Account) { a.Status = StatusDisabled }},
		{name: "error", mutate: func(a *Account) { a.Status = StatusError }},
		{name: "expired", mutate: func(a *Account) { a.AutoPauseOnExpired, a.ExpiresAt = true, &past }},
		{name: "expiry pause disabled", allowed: true, mutate: func(a *Account) { a.ExpiresAt = &past }},
		{name: "account rate limit", mutate: func(a *Account) { a.RateLimitResetAt = &future }},
		{name: "overloaded", mutate: func(a *Account) { a.OverloadUntil = &future }},
		{name: "temporarily unavailable", mutate: func(a *Account) { a.TempUnschedulableUntil = &future }},
		{name: "model rate limit", mutate: func(a *Account) {
			a.Extra["model_rate_limits"] = map[string]any{openAICodexTicketDefaultModel: map[string]any{"rate_limit_reset_at": future.Format(time.RFC3339Nano)}}
		}},
		{name: "5h exhausted", mutate: func(a *Account) { a.Extra["codex_5h_used_percent"] = 100.0 }},
		{name: "7d exhausted despite disabled threshold", mutate: func(a *Account) {
			a.Extra["codex_7d_used_percent"], a.Extra["auto_pause_7d_disabled"] = 100.0, true
		}},
		{name: "account 5h threshold", mutate: func(a *Account) { a.Extra["auto_pause_5h_threshold"] = 0.2 }},
		{name: "account 7d threshold", mutate: func(a *Account) { a.Extra["auto_pause_7d_threshold"] = 0.3 }},
		{name: "global 5h threshold", settings: OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold5h: 0.2}},
		{name: "global 7d threshold", settings: OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold7d: 0.3}},
		{name: "account overrides global threshold", allowed: true, settings: OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold5h: 0.1}, mutate: func(a *Account) {
			a.Extra["auto_pause_5h_threshold"] = 0.5
		}},
		{name: "account disables global threshold", allowed: true, settings: OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold7d: 0.1}, mutate: func(a *Account) {
			a.Extra["auto_pause_7d_disabled"] = true
		}},
		{name: "credits explicitly enabled", allowed: true, mutate: func(a *Account) {
			a.Extra["codex_5h_used_percent"], a.Extra[OpenAICreditsEnabledExtraKey] = 100.0, true
		}},
		{name: "credits still obey threshold", mutate: func(a *Account) {
			a.Extra["codex_5h_used_percent"], a.Extra[OpenAICreditsEnabledExtraKey], a.Extra["auto_pause_5h_threshold"] = 100.0, true, 0.9
		}},
		{name: "quota window reset", allowed: true, mutate: func(a *Account) {
			a.Extra["codex_5h_used_percent"], a.Extra["codex_5h_reset_at"] = 100.0, past.Format(time.RFC3339Nano)
		}},
		{name: "relative quota window reset", allowed: true, mutate: func(a *Account) {
			a.Extra["codex_5h_used_percent"], a.Extra["codex_5h_reset_after_seconds"] = 100.0, 60
			a.Extra["codex_usage_updated_at"] = now.Add(-2 * time.Minute).Format(time.RFC3339Nano)
		}},
		{name: "stale quota without pending reset", allowed: true, mutate: func(a *Account) {
			a.Extra["codex_5h_used_percent"] = 100.0
			a.Extra["codex_usage_updated_at"] = now.Add(-openAICodexAutoPauseStaleAfter - time.Hour).Format(time.RFC3339Nano)
		}},
		{name: "stale quota with pending reset", mutate: func(a *Account) {
			a.Extra["codex_5h_used_percent"], a.Extra["codex_5h_reset_at"] = 100.0, future.Format(time.RFC3339Nano)
			a.Extra["codex_usage_updated_at"] = now.Add(-openAICodexAutoPauseStaleAfter - time.Hour).Format(time.RFC3339Nano)
		}},
		{name: "unknown quota keeps scheduler policy", allowed: true, mutate: func(a *Account) { a.Extra = nil }},
		{name: "paused setup token", mutate: func(a *Account) { a.Type = AccountTypeSetupToken }},
		{name: "paused API key", mutate: func(a *Account) { a.Type = AccountTypeAPIKey }},
		{name: "other platform", mutate: func(a *Account) { a.Platform = PlatformAnthropic }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tc, repo := newPausedTiboCase(t)
			if tt.mutate != nil {
				repo.setAccount(tt.mutate)
			}
			tc.svc.settingService.SetOpenAIQuotaAutoPauseSettings(tt.settings)
			tc.svc.enrollOpenAITiboAccounts(context.Background())
			if tt.allowed {
				require.NotNil(t, tc.svc.loadOpenAITiboAccountState(tc.account.ID), "paused OAuth was enrolled")
			}
			tc.svc.probeOpenAITiboRoutes(context.Background())
			tc.svc.openaiTiboProbeWG.Wait()
			latest, err := repo.GetByID(context.Background(), tc.account.ID)
			require.NoError(t, err)
			require.False(t, latest.Schedulable)
			require.False(t, latest.IsSchedulable(), "a probe must not enable business requests")
			if !tt.allowed {
				require.Empty(t, tc.upstream.requests)
				_, wrote := repo.updateFor(tc.account.ID)
				require.False(t, wrote, "a skipped probe does not change history")
				return
			}
			require.Len(t, tc.upstream.requests, 1)
			requireTiboProbe(t, tc.upstream.bodies[0])
			status := tc.svc.openAITiboRouteStatuses(latest, time.Now())
			require.Len(t, status, 1)
			require.Len(t, status[0].RecentSamples, 1)
			require.Equal(t, "healthy", status[0].RecentSamples[0].Verdict)
			_, wrote := repo.updateFor(tc.account.ID)
			require.True(t, wrote, "the probe result was persisted")
			// The request snapshot stays ineligible even though background probing works.
			require.Equal(t, "manual_unschedulable", openAITiboAccountSkipReason(latest, time.Now()))
			tc.svc.openAITiboRequestVerdicts(context.Background(), latest)
			tc.svc.openaiTiboProbeWG.Wait()
			require.Len(t, tc.upstream.requests, 1)
		})
	}
}

func TestTiboPausedOAuthRechecksBeforeProbe(t *testing.T) {
	for _, change := range []string{"quota", "global threshold", "rate limit", "disabled", "runtime model cooldown", "feature disabled"} {
		t.Run(change, func(t *testing.T) {
			tc, repo := newPausedTiboCase(t)
			tc.svc.enrollOpenAITiboAccounts(context.Background())
			state := tc.svc.loadOpenAITiboAccountState(tc.account.ID)
			require.NotNil(t, state)
			switch change {
			case "quota":
				repo.setAccount(func(a *Account) { a.Extra["codex_7d_used_percent"] = 100.0 })
			case "global threshold":
				tc.svc.settingService.SetOpenAIQuotaAutoPauseSettings(OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold5h: 0.1})
			case "rate limit":
				reset := time.Now().Add(time.Hour)
				repo.setAccount(func(a *Account) { a.RateLimitResetAt = &reset })
			case "disabled":
				repo.setAccount(func(a *Account) { a.Status = StatusDisabled })
			case "runtime model cooldown":
				for i := 0; i < 2; i++ {
					tc.svc.recordOpenAIAccountModelTransientFailure(tc.account, openAICodexTicketDefaultModel, time.Now())
				}
			case "feature disabled":
				enabled := false
				tc.svc.cfg.Gateway.OpenAITiboRoute.Enabled = &enabled
			}
			// Enrollment is throttled: the run must re-read the row/settings itself.
			tc.svc.probeOpenAITiboRoutes(context.Background())
			tc.svc.openaiTiboProbeWG.Wait()
			require.Empty(t, tc.upstream.requests)
			require.Positive(t, repo.gets.Load(), "authoritative account was reloaded")
			state.mu.Lock()
			defer state.mu.Unlock()
			require.Empty(t, state.http.probes, "aborted probe refunded its budget")
			require.Empty(t, state.http.recent)
			require.False(t, state.http.inflight)
		})
	}
}

func TestTiboPausedOAuthResumesAfterHoldClears(t *testing.T) {
	for _, reason := range []string{"quota", "global threshold", "rate limit", "runtime model cooldown"} {
		t.Run(reason, func(t *testing.T) {
			tc, repo := newPausedTiboCase(t)
			var clear func()
			switch reason {
			case "quota":
				repo.setAccount(func(a *Account) { a.Extra["codex_7d_used_percent"] = 100.0 })
				clear = func() {
					repo.setAccount(func(a *Account) { a.Extra["codex_7d_reset_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339Nano) })
				}
			case "global threshold":
				tc.svc.settingService.SetOpenAIQuotaAutoPauseSettings(OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold7d: 0.1})
				clear = func() {
					tc.svc.settingService.SetOpenAIQuotaAutoPauseSettings(OpsOpenAIAccountQuotaAutoPauseSettings{})
				}
			case "rate limit":
				reset := time.Now().Add(time.Hour)
				repo.setAccount(func(a *Account) { a.RateLimitResetAt = &reset })
				clear = func() { repo.setAccount(func(a *Account) { a.RateLimitResetAt = nil }) }
			case "runtime model cooldown":
				// Business mappings must not redirect the fixed Astra probe's cooldown check.
				repo.setAccount(func(a *Account) {
					a.Credentials["model_mapping"] = map[string]any{openAICodexTicketDefaultModel: "gpt-5.6-sol"}
				})
				for i := 0; i < 2; i++ {
					tc.svc.recordOpenAIAccountModelTransientFailure(tc.account, openAICodexTicketDefaultModel, time.Now())
				}
				clear = func() { tc.svc.clearOpenAIAccountModelTransientState(tc.account.ID, openAICodexTicketDefaultModel) }
			}
			// Previously tracked accounts must also be unparked after the hold clears.
			state := tc.svc.openAITiboAccountState(tc.account.ID)
			tc.svc.probeOpenAITiboRoutes(context.Background())
			tc.svc.openaiTiboProbeWG.Wait()
			require.Empty(t, tc.upstream.requests)
			clear()
			tc.svc.openaiTiboEnrolledAt.Store(0)
			tc.svc.probeOpenAITiboRoutes(context.Background())
			tc.svc.openaiTiboProbeWG.Wait()
			require.Len(t, tc.upstream.requests, 1)
			state.mu.Lock()
			require.False(t, state.http.parked)
			require.True(t, state.http.holdUntil.IsZero())
			state.mu.Unlock()
			latest, err := repo.GetByID(context.Background(), tc.account.ID)
			require.NoError(t, err)
			require.False(t, latest.Schedulable)
		})
	}
}

func TestTiboPausedOAuthKeepsProbeBudgetAndSingleFlight(t *testing.T) {
	tc, _ := newPausedTiboCase(t)
	gate := make(chan struct{})
	safe := &tiboSafeUpstream{probeGate: gate}
	tc.svc.httpUpstream = safe
	// Always release a blocked probe even if an assertion fails.
	defer func() { close(gate); tc.svc.openaiTiboProbeWG.Wait() }()
	tc.svc.probeOpenAITiboRoutes(context.Background())
	state := tc.svc.loadOpenAITiboAccountState(tc.account.ID)
	require.NotNil(t, state)
	for i := 0; i < 10; i++ {
		tc.svc.probeOpenAITiboRoutes(context.Background())
	}
	state.mu.Lock()
	inflight, probes := state.http.inflight, len(state.http.probes)
	state.mu.Unlock()
	require.True(t, inflight)
	require.Equal(t, 1, probes, "ticks share one in-flight probe")

	budget, _ := newPausedTiboCase(t)
	budgetState := budget.svc.openAITiboAccountState(budget.account.ID)
	for i := 0; i < budget.svc.openAITiboRouteConfig().maxProbesPerHour; i++ {
		budgetState.http.probes = append(budgetState.http.probes, time.Now().Add(-time.Minute))
	}
	budget.svc.probeOpenAITiboRoutes(context.Background())
	budget.svc.openaiTiboProbeWG.Wait()
	require.Empty(t, budget.upstream.requests, "paused accounts keep the hourly cap")
}
