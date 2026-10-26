package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Regression (account 23156): a rate limit pushed the probe days out and
// lifting it never pulled the probe back. Holds are now re-synced from the
// database on every enrollment.
func TestTiboEnrollmentLiftsStaleHold(t *testing.T) {
	tc := newTiboRouteCase(t, false, false, cookieWSHTTPResponse("True"))
	now := time.Now()
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	state.mu.Lock()
	state.http = openAITiboRouteState{
		verdict: openAITiboHealthy, checkedAt: now.Add(-20 * time.Hour), flippedAt: now.Add(-48 * time.Hour),
		lastSampleAt: now.Add(-20 * time.Hour), nextProbeAt: now.Add(-19 * time.Hour),
		holdUntil: now.Add(5 * 24 * time.Hour),
	}
	state.mu.Unlock()

	tc.svc.probeOpenAITiboRoutes(context.Background())
	tc.svc.openaiTiboProbeWG.Wait()
	require.Len(t, tc.upstream.requests, 1, "the lifted limit resumes probing on the next enrollment")
	requireTiboProbe(t, tc.upstream.bodies[0])
	state.mu.Lock()
	defer state.mu.Unlock()
	require.True(t, state.http.holdUntil.IsZero())
	require.WithinDuration(t, time.Now(), state.http.checkedAt, 5*time.Second)
}

func TestTiboEnrollmentHoldsRateLimitedAccountThenResumes(t *testing.T) {
	tc := newTiboRouteCase(t, false, false, cookieWSHTTPResponse("True"))
	repo := mustTestValue[*tiboAccountRepo](t, tc.svc.accountRepo)
	now := time.Now()
	reset := now.Add(time.Hour)
	repo.setAccount(func(a *Account) { a.RateLimitResetAt = &reset })
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	state.mu.Lock()
	state.http = openAITiboRouteState{verdict: openAITiboHealthy, checkedAt: now.Add(-time.Minute), lastSampleAt: now.Add(-time.Minute), nextProbeAt: now.Add(-time.Second)}
	state.mu.Unlock()

	tc.svc.probeOpenAITiboRoutes(context.Background())
	tc.svc.openaiTiboProbeWG.Wait()
	require.Empty(t, tc.upstream.requests)
	status := tc.svc.openAITiboRouteStatuses(tc.account, time.Now())
	require.NotNil(t, status[0].HeldUntil)
	require.WithinDuration(t, reset, *status[0].HeldUntil, time.Second)
	require.WithinDuration(t, reset, *status[0].NextProbeAt, time.Second, "admin view shows the effective next probe")

	// The limit is cleared early (manual reset / recovery): next enrollment.
	repo.setAccount(func(a *Account) { a.RateLimitResetAt = nil })
	tc.svc.openaiTiboEnrolledAt.Store(0)
	tc.svc.probeOpenAITiboRoutes(context.Background())
	tc.svc.openaiTiboProbeWG.Wait()
	require.Len(t, tc.upstream.requests, 1)
}

// A request snapshot can be a stale scheduler copy; it must not set a hold.
func TestTiboRequestSnapshotDoesNotHold(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	now := time.Now()
	next := now.Add(5 * time.Minute)
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	state.mu.Lock()
	state.http = openAITiboRouteState{verdict: openAITiboHealthy, checkedAt: now, flippedAt: now, lastSampleAt: now, nextProbeAt: next}
	state.mu.Unlock()
	reset := now.Add(5 * 24 * time.Hour)
	snapshot := *tc.account
	snapshot.RateLimitResetAt = &reset

	require.Equal(t, openAITiboHealthy, tc.svc.openAITiboRequestVerdicts(context.Background(), &snapshot))
	state.mu.Lock()
	defer state.mu.Unlock()
	require.True(t, state.http.holdUntil.IsZero())
	require.Equal(t, next, state.http.nextProbeAt)
	require.Empty(t, tc.upstream.requests)
}

func TestTiboEnrollmentDropsUnlistedAccounts(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	now := time.Now()
	gone := tc.svc.openAITiboAccountState(999)
	gone.mu.Lock()
	gone.http.nextProbeAt = now.Add(time.Hour)
	gone.mu.Unlock()
	busy := tc.svc.openAITiboAccountState(998)
	busy.mu.Lock()
	busy.http.inflight = true
	busy.mu.Unlock()
	listed := tc.svc.openAITiboAccountState(tc.account.ID)
	listed.mu.Lock()
	listed.http.nextProbeAt = now.Add(time.Hour)
	listed.mu.Unlock()

	tc.svc.enrollOpenAITiboAccounts(context.Background())
	require.Nil(t, tc.svc.loadOpenAITiboAccountState(999), "deleted/disabled accounts are dropped")
	require.NotNil(t, tc.svc.loadOpenAITiboAccountState(998), "a probe in flight is kept for the next pass")
	require.NotNil(t, tc.svc.loadOpenAITiboAccountState(tc.account.ID))
}

func TestTiboClientErrorBackoff(t *testing.T) {
	cfg := tiboTestSettings()
	now := time.Now()
	var r openAITiboRouteState
	step := func(status int) time.Duration {
		r.observe(openAITiboProbeSample{verdict: openAITiboUnknown, status: status}, now, cfg)
		r.scheduleNext(openAITiboRouteHTTP, now, cfg)
		return r.nextProbeAt.Sub(now)
	}
	for i, want := range []time.Duration{10, 20, 40, 80, 120, 120} {
		require.Equal(t, want*time.Minute, step(http.StatusBadRequest), "rejection %d", i+1)
	}
	require.Equal(t, 6, r.clientErrorStreak)
	require.Equal(t, cfg.interval, step(http.StatusTooManyRequests), "429 is transient: back to cadence")
	require.Equal(t, cfg.interval, step(http.StatusForbidden))
	require.Equal(t, 2*cfg.interval, step(http.StatusForbidden))
	require.Equal(t, cfg.interval, step(0), "network errors are transient")

	r.observe(tiboSample(openAITiboHealthy), now, cfg)
	r.scheduleNext(openAITiboRouteHTTP, now, cfg)
	require.Zero(t, r.clientErrorStreak)
	require.Equal(t, cfg.interval, r.nextProbeAt.Sub(now))
}

func TestTiboProbeRetryAfterIsCapped(t *testing.T) {
	cfg := tiboTestSettings()
	now := time.Now()
	var r openAITiboRouteState
	retryAt := now.Add(5 * 24 * time.Hour)
	r.observe(openAITiboProbeSample{verdict: openAITiboUnknown, status: http.StatusTooManyRequests, retryAt: &retryAt}, now, cfg)
	r.scheduleNext(openAITiboRouteHTTP, now, cfg)
	require.Equal(t, now.Add(openAITiboProbeRetryAfterCap), r.nextProbeAt)
}

func TestTiboUnknownSampleSkipsPersist(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	repo := mustTestValue[*tiboAccountRepo](t, tc.svc.accountRepo)
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	state.mu.Lock()
	state.http.recent = []openAITiboRecentSample{{Verdict: openAITiboHealthy, At: time.Now()}}
	state.mu.Unlock()

	tc.svc.recordOpenAITiboSample(state, openAITiboRouteHTTP, tc.account, openAITiboProbeSample{verdict: openAITiboUnknown, status: http.StatusBadRequest})
	_, wrote := repo.updateFor(tc.account.ID)
	require.False(t, wrote, "unknown samples change nothing worth writing")
	tc.svc.recordOpenAITiboSample(state, openAITiboRouteHTTP, tc.account, tiboSample(openAITiboHealthy))
	_, wrote = repo.updateFor(tc.account.ID)
	require.True(t, wrote)
}

// After the degraded dwell, a disagreeing sample keeps confirm_spacing
// instead of re-probing on the next tick.
func TestTiboDwellEndKeepsConfirmSpacing(t *testing.T) {
	cfg := tiboTestSettings()
	t0 := time.Now()
	r := openAITiboRouteState{verdict: openAITiboDegraded, checkedAt: t0, flippedAt: t0}
	at := t0.Add(time.Minute)
	for _, v := range []openAITiboVerdict{openAITiboHealthy, openAITiboHealthy} {
		r.observe(tiboSample(v), at, cfg)
		r.scheduleNext(openAITiboRouteHTTP, at, cfg)
		at = r.nextProbeAt
	}
	require.True(t, r.dwellDeferred)
	require.Equal(t, t0.Add(cfg.minDegradedDwell), at, "waits for the dwell end")
	r.observe(tiboSample(openAITiboDegraded), at, cfg)
	r.scheduleNext(openAITiboRouteHTTP, at, cfg)
	require.Equal(t, at.Add(cfg.confirmSpacing), r.nextProbeAt)
}

func TestTiboProbeModelMatchIsCaseInsensitive(t *testing.T) {
	body := "data: " + string(cookieWSCompletion("GPT-6-Astra", "True")) + "\n\n"
	sample := openAITiboSampleFromObservation(http.StatusOK, observeOpenAITiboHTTPProbe([]byte(body)))
	require.Equal(t, openAITiboHealthy, sample.verdict)
}

func TestTiboAccountHold(t *testing.T) {
	now := time.Now()
	base := func() *Account {
		return &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
	}
	require.Equal(t, openAITiboHold{}, openAITiboAccountHold(base(), now))

	paused := base()
	paused.Schedulable = false
	require.True(t, openAITiboAccountHold(paused, now).parked)
	disabled := base()
	disabled.Status = StatusDisabled
	require.True(t, openAITiboAccountHold(disabled, now).parked)

	limited := base()
	rl, overload, past := now.Add(time.Hour), now.Add(2*time.Hour), now.Add(-time.Hour)
	limited.RateLimitResetAt, limited.OverloadUntil, limited.TempUnschedulableUntil = &rl, &overload, &past
	hold := openAITiboAccountHold(limited, now)
	require.False(t, hold.parked)
	require.Equal(t, overload, hold.until, "the latest active window")
}
