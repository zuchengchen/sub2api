package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func tiboTestSettings() openAITiboSettings {
	return (&OpenAIGatewayService{cfg: &config.Config{}}).openAITiboRouteConfig()
}

func tiboSample(v openAITiboVerdict) openAITiboProbeSample {
	return openAITiboProbeSample{verdict: v, status: 200}
}

func TestTiboVoteSingleDisagreementDoesNotFlip(t *testing.T) {
	cfg := tiboTestSettings()
	now := time.Now()
	var r openAITiboRouteState
	require.True(t, r.observe(tiboSample(openAITiboHealthy), now, cfg).flipped, "first definitive sample adopts")
	require.Equal(t, openAITiboHealthy, r.verdict)

	require.False(t, r.observe(tiboSample(openAITiboDegraded), now.Add(time.Second), cfg).flipped)
	require.Equal(t, openAITiboHealthy, r.effective(now.Add(time.Second), cfg), "one degraded sample keeps healthy")
	require.Len(t, r.pending, 1)
	require.False(t, r.observe(tiboSample(openAITiboHealthy), now.Add(2*time.Second), cfg).flipped)
	require.Len(t, r.pending, 2, "window stays open until it is decided")

	transition := r.observe(tiboSample(openAITiboDegraded), now.Add(3*time.Second), cfg)
	require.True(t, transition.flipped, "2 of 3 agree")
	require.Equal(t, openAITiboHealthy, transition.from)
	require.Equal(t, openAITiboDegraded, transition.to)
	require.Equal(t, []openAITiboVerdict{openAITiboDegraded, openAITiboHealthy, openAITiboDegraded}, transition.votes)
	require.Empty(t, r.pending)
	require.Equal(t, now.Add(3*time.Second), r.flippedAt)
}

func TestTiboVoteWindowClosesWhenChallengerCannotWin(t *testing.T) {
	cfg := tiboTestSettings()
	now := time.Now()
	r := openAITiboRouteState{verdict: openAITiboHealthy, checkedAt: now, flippedAt: now}
	r.observe(tiboSample(openAITiboDegraded), now.Add(time.Second), cfg)
	r.observe(tiboSample(openAITiboHealthy), now.Add(2*time.Second), cfg)
	r.observe(tiboSample(openAITiboHealthy), now.Add(3*time.Second), cfg)
	require.Empty(t, r.pending, "1 of 3 cannot win")
	require.Equal(t, openAITiboHealthy, r.verdict)
	require.Equal(t, now.Add(3*time.Second), r.checkedAt, "agreeing samples refresh the verdict")
}

func TestTiboVoteUnknownNeverVotes(t *testing.T) {
	cfg := tiboTestSettings()
	now := time.Now()
	r := openAITiboRouteState{verdict: openAITiboHealthy, checkedAt: now, flippedAt: now}
	r.observe(tiboSample(openAITiboDegraded), now.Add(time.Second), cfg)
	for i := 0; i < 5; i++ {
		require.False(t, r.observe(openAITiboProbeSample{verdict: openAITiboUnknown, status: 429}, now.Add(time.Duration(2+i)*time.Second), cfg).flipped)
	}
	require.Equal(t, openAITiboHealthy, r.verdict)
	require.Equal(t, []openAITiboVerdict{openAITiboDegraded}, r.pending, "unknown samples do not fill the window")
	require.Equal(t, 5, r.unknownStreak)
	require.Equal(t, now, r.checkedAt, "unknown samples do not refresh the verdict")
	r.observe(tiboSample(openAITiboDegraded), now.Add(10*time.Second), cfg)
	require.Equal(t, openAITiboDegraded, r.verdict)
	require.Zero(t, r.unknownStreak)
}

func TestTiboVoteMaxStale(t *testing.T) {
	cfg := tiboTestSettings()
	now := time.Now()
	r := openAITiboRouteState{verdict: openAITiboDegraded, checkedAt: now}
	require.Equal(t, openAITiboDegraded, r.effective(now.Add(cfg.maxStale), cfg))
	require.Equal(t, openAITiboUnknown, r.effective(now.Add(cfg.maxStale+time.Second), cfg))
	require.Equal(t, openAITiboDegraded, r.verdict, "the confirmed verdict is kept")
	require.Equal(t, openAITiboUnknown, (&openAITiboRouteState{}).effective(now, cfg))
}

func TestTiboVoteMinDegradedDwell(t *testing.T) {
	cfg := tiboTestSettings()
	start := time.Now()
	r := openAITiboRouteState{verdict: openAITiboDegraded, checkedAt: start, flippedAt: start}
	r.observe(tiboSample(openAITiboHealthy), start.Add(time.Minute), cfg)
	require.False(t, r.observe(tiboSample(openAITiboHealthy), start.Add(2*time.Minute), cfg).flipped, "confirmed but inside the dwell")
	require.True(t, r.dwellDeferred)
	r.scheduleNext(openAITiboRouteHTTP, start.Add(2*time.Minute), cfg)
	require.False(t, r.nextProbeAt.Before(start.Add(cfg.minDegradedDwell)), "recheck waits for the dwell end")

	transition := r.observe(tiboSample(openAITiboHealthy), start.Add(cfg.minDegradedDwell+time.Second), cfg)
	require.True(t, transition.flipped)
	require.Equal(t, openAITiboHealthy, r.verdict)
	require.False(t, r.dwellDeferred)

	// healthy -> degraded is never delayed.
	r.observe(tiboSample(openAITiboDegraded), start.Add(cfg.minDegradedDwell+2*time.Second), cfg)
	require.True(t, r.observe(tiboSample(openAITiboDegraded), start.Add(cfg.minDegradedDwell+3*time.Second), cfg).flipped)

	// Negative config disables the dwell.
	cfg.minDegradedDwell = 0
	r.observe(tiboSample(openAITiboHealthy), start.Add(cfg.minDegradedDwell+4*time.Second), cfg)
	require.True(t, r.observe(tiboSample(openAITiboHealthy), start.Add(cfg.minDegradedDwell+5*time.Second), cfg).flipped)
}

func TestTiboForceDegraded(t *testing.T) {
	now := time.Now()
	r := openAITiboRouteState{verdict: openAITiboHealthy, checkedAt: now.Add(-time.Minute), pending: []openAITiboVerdict{openAITiboDegraded}}
	require.True(t, r.forceDegraded(now))
	require.Equal(t, openAITiboDegraded, r.verdict)
	require.Equal(t, now, r.flippedAt)
	require.Empty(t, r.pending)
	require.False(t, r.forceDegraded(now.Add(time.Second)), "already degraded")
	require.Equal(t, now, r.flippedAt)
}

func TestTiboScheduleCadenceBackoffAndRetryAfter(t *testing.T) {
	cfg := tiboTestSettings()
	now := time.Now()
	r := openAITiboRouteState{}
	r.observe(tiboSample(openAITiboHealthy), now, cfg)
	r.scheduleNext(openAITiboRouteHTTP, now, cfg)
	require.Equal(t, now.Add(cfg.healthyInterval), r.nextProbeAt, "zero jitter in tests")

	r.observe(tiboSample(openAITiboDegraded), now, cfg)
	r.scheduleNext(openAITiboRouteHTTP, now, cfg)
	require.Equal(t, now.Add(cfg.confirmSpacing), r.nextProbeAt, "confirmation probes are spaced")
	r.observe(tiboSample(openAITiboDegraded), now, cfg)
	r.scheduleNext(openAITiboRouteHTTP, now, cfg)
	require.Equal(t, now.Add(cfg.degradedInterval), r.nextProbeAt)

	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		r.observe(openAITiboProbeSample{verdict: openAITiboUnknown}, now, cfg)
		r.scheduleNext(openAITiboRouteHTTP, now, cfg)
		require.Equal(t, now.Add(want), r.nextProbeAt, "unknown step %d", i)
	}
	retryAt := now.Add(time.Hour)
	r.observe(openAITiboProbeSample{verdict: openAITiboUnknown, status: 429, retryAt: &retryAt}, now, cfg)
	r.scheduleNext(openAITiboRouteHTTP, now, cfg)
	require.Equal(t, retryAt, r.nextProbeAt, "Retry-After wins when later")

	var bps openAITiboRouteState
	bps.observe(tiboSample(openAITiboDegraded), now, cfg)
	bps.scheduleNext(openAITiboRouteBPS, now, cfg)
	require.Equal(t, now.Add(cfg.bpsProbeInterval), bps.nextProbeAt)
}

func TestTiboJitterBounds(t *testing.T) {
	for i := 0; i < 200; i++ {
		got := openAITiboJitter(10*time.Minute, 0.2)
		require.GreaterOrEqual(t, got, 8*time.Minute)
		require.LessOrEqual(t, got, 12*time.Minute)
	}
	require.Equal(t, time.Minute, openAITiboJitter(time.Minute, 0))
}

func TestTiboProbeBudget(t *testing.T) {
	cfg := tiboTestSettings()
	now := time.Now()
	var r openAITiboRouteState
	for i := 0; i < cfg.maxProbesPerHour; i++ {
		require.True(t, r.takeBudget(now.Add(time.Duration(i)*time.Second), cfg))
	}
	require.False(t, r.takeBudget(now.Add(time.Minute), cfg))
	require.Equal(t, now.Add(time.Hour), r.nextProbeAt, "deferred to the window end")
	require.True(t, r.takeBudget(now.Add(time.Hour), cfg), "the oldest probe left the window")
}

// Reading hour counts must not corrupt the stored windows (regression: an
// unassigned in-place prune duplicated entries and inflated the budget).
func TestTiboWindowCountDoesNotMutate(t *testing.T) {
	now := time.Now()
	times := []time.Time{now.Add(-2 * time.Hour), now.Add(-90 * time.Minute), now.Add(-time.Minute), now}
	require.Equal(t, 2, countOpenAITiboWindow(times, now))
	require.Equal(t, 2, countOpenAITiboWindow(times, now))
	require.Len(t, times, 4)
	require.Equal(t, now.Add(-2*time.Hour), times[0], "unchanged")

	tc := newTiboRouteCase(t, false, false)
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	state.mu.Lock()
	state.http.probes = append([]time.Time(nil), times...)
	state.http.flips = append([]time.Time(nil), times...)
	state.mu.Unlock()
	status := tc.svc.openAITiboRouteStatuses(tc.account, false, now)
	require.Equal(t, 2, status[0].ProbesHour)
	require.Equal(t, 2, status[0].FlipsHour)
	state.mu.Lock()
	defer state.mu.Unlock()
	require.Equal(t, times, state.http.probes, "the admin view does not rewrite the window")
	require.Equal(t, times, state.http.flips)
}

func TestTiboCalibrationCounters(t *testing.T) {
	cfg := tiboTestSettings()
	now := time.Now()
	r := openAITiboRouteState{verdict: openAITiboHealthy, checkedAt: now, flippedAt: now.Add(-time.Hour)}
	// Noise: one False outvoted by two True.
	r.observe(tiboSample(openAITiboDegraded), now.Add(time.Second), cfg)
	r.observe(tiboSample(openAITiboHealthy), now.Add(2*time.Second), cfg)
	r.observe(tiboSample(openAITiboHealthy), now.Add(3*time.Second), cfg)
	// Real change: two False of three.
	r.observe(tiboSample(openAITiboDegraded), now.Add(4*time.Second), cfg)
	r.observe(tiboSample(openAITiboDegraded), now.Add(5*time.Second), cfg)
	require.Equal(t, 2, r.voteWindows)
	require.Equal(t, 1, r.voteConfirmed)
	require.Equal(t, 1, r.voteRejected)

	tc := newTiboRouteCase(t, true, false)
	tc.svc.cfg.Gateway.OpenAITiboRoute.BPSProbeMode = "shadow"
	tc.seed(openAITiboRouteHTTP, openAITiboDegraded)
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	tc.svc.recordOpenAITiboSample(state, openAITiboRouteBPS, tc.account, tiboSample(openAITiboDegraded))
	tc.svc.recordOpenAITiboSample(state, openAITiboRouteBPS, tc.account, tiboSample(openAITiboHealthy))
	tc.svc.recordOpenAITiboSample(state, openAITiboRouteBPS, tc.account, openAITiboProbeSample{verdict: openAITiboUnknown})
	status := tc.svc.openAITiboRouteStatuses(tc.account, false, time.Now())
	require.Equal(t, "bps", status[1].Route)
	require.Equal(t, 1, status[1].ShadowAgree)
	require.Equal(t, 1, status[1].ShadowDisagree, "unknown samples are not compared")
}

func TestTiboRouteOrderByTier(t *testing.T) {
	H, U, D := openAITiboHealthy, openAITiboUnknown, openAITiboDegraded
	type tiers = map[openAITiboRoute]openAITiboVerdict
	for _, tc := range []struct {
		name   string
		tiers  tiers
		pinned openAITiboRoute
		want   []openAITiboRoute
	}{
		{"healthy http wins", tiers{"http": H, "bps": H, "cookie_ws": H}, "", []openAITiboRoute{"http"}},
		{"degraded http", tiers{"http": D, "bps": H, "cookie_ws": H}, "", []openAITiboRoute{"bps", "cookie_ws", "http"}},
		{"unknown http below healthy", tiers{"http": U, "cookie_ws": H}, "", []openAITiboRoute{"cookie_ws", "http"}},
		{"enforce bps unknown", tiers{"http": D, "bps": U, "cookie_ws": H}, "", []openAITiboRoute{"cookie_ws", "bps", "http"}},
		{"everything degraded", tiers{"http": D, "bps": D}, "", []openAITiboRoute{"http"}},
		{"pin keeps healthy route", tiers{"http": H, "cookie_ws": H}, "cookie_ws", []openAITiboRoute{"cookie_ws", "http"}},
		{"pin ignored when not healthy", tiers{"http": D, "bps": D, "cookie_ws": H}, "bps", []openAITiboRoute{"cookie_ws", "http"}},
		{"http only", tiers{"http": U}, "", []openAITiboRoute{"http"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, openAITiboOrderRoutes(tc.tiers, tc.pinned))
		})
	}
}

// A cold account waits for its first probe once; the verdict is persisted
// and a restarted process uses it without waiting.
func TestTiboVerdictPersistsAndWarmStarts(t *testing.T) {
	tc := newTiboRouteCase(t, false, false, cookieWSHTTPResponse("True"), cookieWSHTTPResponse("http ok"))
	tc.forward(t)
	repo := mustTestValue[*cookieWSLifecycleRepo](t, tc.svc.accountRepo)
	repo.mu.Lock()
	persisted := repo.updates[tc.account.ID][openAITiboVerdictExtraKey(openAITiboRouteHTTP)]
	repo.mu.Unlock()
	record, ok := parseOpenAITiboVerdictRecord(persisted)
	require.True(t, ok, "flip persisted to Extra")
	require.Equal(t, openAITiboHealthy, record.Verdict)
	require.Equal(t, openAICodexTicketDefaultModel, record.Model)
	require.True(t, IsOpenAICodexTicketExtraKey(openAITiboVerdictExtraKey(openAITiboRouteHTTP)), "admin edits keep it, exports redact it")
	merged := MergeOpenAICodexTicketExtra(map[string]any{"note": "admin", openAITiboVerdictExtraKey(openAITiboRouteHTTP): "spoofed"}, map[string]any{openAITiboVerdictExtraKey(openAITiboRouteHTTP): persisted})
	require.Equal(t, persisted, merged[openAITiboVerdictExtraKey(openAITiboRouteHTTP)], "an admin edit cannot overwrite the verdict")
	require.NotContains(t, RedactOpenAICodexTicketExtra(merged), openAITiboVerdictExtraKey(openAITiboRouteHTTP))

	warm := newTiboRouteCase(t, false, false, cookieWSHTTPResponse("http ok"))
	warm.account.Extra[openAITiboVerdictExtraKey(openAITiboRouteHTTP)] = persisted
	result, _, _ := warm.forward(t)
	require.False(t, result.OpenAIWSMode)
	require.Len(t, warm.upstream.requests, 1, "no probe: the persisted verdict is fresh")
	require.NotContains(t, string(warm.upstream.bodies[0]), openAICookieWSProbePrompt)

	stale := newTiboRouteCase(t, false, false, cookieWSHTTPResponse("True"), cookieWSHTTPResponse("http ok"))
	stale.account.Extra[openAITiboVerdictExtraKey(openAITiboRouteHTTP)] = map[string]any{
		"verdict": "healthy", "model": openAICodexTicketDefaultModel,
		"checked_at": time.Now().Add(-time.Hour).Format(time.RFC3339Nano), "flipped_at": time.Now().Add(-2 * time.Hour).Format(time.RFC3339Nano),
	}
	stale.forward(t)
	require.Len(t, stale.upstream.requests, 2, "beyond max_stale the account is cold again")
	requireTiboProbe(t, stale.upstream.bodies[0])
}

func TestTiboStartupLoadSeedsFromFullRows(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	repo := mustTestValue[*cookieWSLifecycleRepo](t, tc.svc.accountRepo)
	require.NoError(t, repo.UpdateExtra(context.Background(), tc.account.ID, map[string]any{
		openAITiboVerdictExtraKey(openAITiboRouteHTTP): map[string]any{"verdict": "degraded", "model": openAICodexTicketDefaultModel,
			"checked_at": time.Now().Add(-time.Minute).Format(time.RFC3339Nano), "flipped_at": time.Now().Add(-time.Minute).Format(time.RFC3339Nano)},
	}))
	tc.svc.probeOpenAITiboRoutes(context.Background())
	tc.svc.openaiTiboProbeWG.Wait()
	require.Equal(t, openAITiboDegraded, tc.svc.openAITiboEffective(tc.account.ID, openAITiboRouteHTTP, time.Now()))
	require.Empty(t, tc.upstream.requests, "loaded accounts are idle until they serve a request")
}

func TestTiboTickProbesOnlyActiveDueAccounts(t *testing.T) {
	tc := newTiboRouteCase(t, false, false, cookieWSHTTPResponse("False"))
	tc.svc.openaiTiboLoaded.Store(true)
	now := time.Now()
	active := tc.svc.openAITiboAccountState(tc.account.ID)
	active.mu.Lock()
	active.lastUsedAt = now.Add(-time.Minute)
	active.http = openAITiboRouteState{verdict: openAITiboHealthy, checkedAt: now.Add(-11 * time.Minute), flippedAt: now.Add(-time.Hour), nextProbeAt: now.Add(-time.Second), lastSampleAt: now.Add(-11 * time.Minute)}
	active.mu.Unlock()
	idle := tc.svc.openAITiboAccountState(99)
	idle.mu.Lock()
	idle.lastUsedAt = now.Add(-31 * time.Minute)
	idle.http = openAITiboRouteState{verdict: openAITiboHealthy, checkedAt: now.Add(-40 * time.Minute), nextProbeAt: now.Add(-time.Minute), lastSampleAt: now.Add(-40 * time.Minute)}
	idle.mu.Unlock()

	tc.svc.probeOpenAITiboRoutes(context.Background())
	tc.svc.openaiTiboProbeWG.Wait()
	require.Len(t, tc.upstream.requests, 1, "only the recently active account is probed")
	requireTiboProbe(t, tc.upstream.bodies[0])
	active.mu.Lock()
	defer active.mu.Unlock()
	require.Equal(t, openAITiboHealthy, active.http.verdict, "one False opens a vote, no flip")
	require.Equal(t, []openAITiboVerdict{openAITiboDegraded}, active.http.pending)
	require.WithinDuration(t, time.Now().Add(20*time.Second), active.http.nextProbeAt, 5*time.Second, "confirmation spacing")
	require.Len(t, active.http.probes, 1)
}

func TestTiboTickRespectsBudgetAndIneligibleAccounts(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	tc.svc.openaiTiboLoaded.Store(true)
	cfg := tc.svc.openAITiboRouteConfig()
	now := time.Now()
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	state.mu.Lock()
	state.lastUsedAt = now
	state.http.lastSampleAt = now.Add(-time.Minute)
	for i := 0; i < cfg.maxProbesPerHour; i++ {
		state.http.probes = append(state.http.probes, now.Add(-time.Duration(i+1)*time.Minute))
	}
	state.mu.Unlock()
	tc.svc.probeOpenAITiboRoutes(context.Background())
	tc.svc.openaiTiboProbeWG.Wait()
	require.Empty(t, tc.upstream.requests, "hourly budget exhausted")

	// A paused account is not sampled and its budget is refunded.
	repo := mustTestValue[*cookieWSLifecycleRepo](t, tc.svc.accountRepo)
	repo.mu.Lock()
	repo.accounts[0].Schedulable = false
	repo.mu.Unlock()
	state.mu.Lock()
	state.http.probes = nil
	state.http.nextProbeAt = time.Time{}
	state.mu.Unlock()
	tc.svc.probeOpenAITiboRoutes(context.Background())
	tc.svc.openaiTiboProbeWG.Wait()
	require.Empty(t, tc.upstream.requests)
	state.mu.Lock()
	defer state.mu.Unlock()
	require.Empty(t, state.http.probes)
	require.True(t, state.http.nextProbeAt.After(time.Now()), "retried later")
	require.False(t, state.http.inflight)
}

func TestTiboHTTPDegradedTriggersBPSProbeInShadow(t *testing.T) {
	tc := newTiboRouteCase(t, true, false, cookieWSHTTPResponse("False"))
	tc.svc.cfg.Gateway.OpenAITiboRoute.BPSProbeMode = config.OpenAITiboBPSProbeShadow
	tc.svc.openaiTiboLoaded.Store(true)
	now := time.Now()
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	state.mu.Lock()
	state.lastUsedAt, state.bpsEnabled = now, true
	state.http = openAITiboRouteState{verdict: openAITiboHealthy, checkedAt: now, flippedAt: now.Add(-time.Hour),
		pending: []openAITiboVerdict{openAITiboDegraded}, pendingSince: now, lastSampleAt: now}
	state.bps.nextProbeAt = now.Add(time.Hour)
	state.mu.Unlock()
	tc.svc.recordOpenAITiboSample(state, openAITiboRouteHTTP, tc.account, tiboSample(openAITiboDegraded))
	tc.svc.openaiTiboProbeWG.Wait()
	state.mu.Lock()
	defer state.mu.Unlock()
	require.Equal(t, openAITiboDegraded, state.http.verdict)
	require.Len(t, state.bps.probes, 1, "HTTP turning degraded samples BPS immediately")
	require.False(t, state.bps.lastSampleAt.IsZero())
}
