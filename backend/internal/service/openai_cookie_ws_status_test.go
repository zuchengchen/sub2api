package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCookieWSRuntimeStatusPersistedVerificationDoesNotSurviveRestart(t *testing.T) {
	s, _ := cookieWSTestService(t, nil)
	a := ticketTestAccount(41)
	a.Extra = make(map[string]any)
	now := time.Now()
	for slot := 0; slot < openAICookieWSSlotCount; slot++ {
		ticket := cookieWSTestTicket(a.ID, now.Add(-time.Minute))
		ticket.Slot = slot
		a.Extra[openAICookieWSExtraKeySlot(ticket.Model, slot)] = ticket
	}
	service := &AccountTestService{openaiGatewayService: s}
	statuses := service.OpenAICodexTicketStatuses(a, s.openAICodexTicketConfig(), now)
	require.Len(t, statuses, 1)
	status := statuses[0]
	require.False(t, status.Ready)
	require.True(t, status.Blocked)
	require.Equal(t, "recovering", status.RecoveryState)
	require.Equal(t, 3, status.CookieGroupsValid)
	require.Zero(t, status.CookieGroupsReady)
	require.Zero(t, status.VerifiedWS)
	require.Len(t, status.CookieSlots, 3)
	for _, slot := range status.CookieSlots {
		require.False(t, slot.CookieReady)
		require.Equal(t, "waiting", slot.State)
	}
	data, err := json.Marshal(status)
	require.NoError(t, err)
	require.Contains(t, string(data), `"verified_ws":0`)
	for _, private := range []string{"cookies", "identity", "generation", "http_verified", "ws_verified", "__cf_bm"} {
		require.NotContains(t, string(data), private)
	}

	var unavailable *AccountTestService
	fallback := unavailable.OpenAICodexTicketStatuses(a, s.openAICodexTicketConfig(), now)[0]
	require.Equal(t, "unavailable", fallback.RecoveryState)
	require.False(t, fallback.Ready)
	require.Zero(t, fallback.CookieGroupsReady)
	require.Equal(t, 3, fallback.CookieGroupsValid)
}

func TestCookieWSRuntimeStatusTracksActualPoolAndPartialAvailability(t *testing.T) {
	for _, groups := range []int{1, 3} {
		s, a, dialer := cookieWarmupFixture(t, groups, "True")
		svc := &AccountTestService{openaiGatewayService: s}
		get := func() OpenAICodexTicketStatus {
			return svc.OpenAICodexTicketStatuses(a, s.openAICodexTicketConfig(), time.Now())[0]
		}
		before := get()
		require.Equal(t, groups, before.CookieGroupsReady)
		require.Zero(t, before.VerifiedWS)
		require.False(t, before.Ready, "verified cookies alone are not verified business sockets")
		require.Equal(t, "recovering", before.RecoveryState)
		require.Empty(t, dialer.conns, "an admin read must not open sockets or send probes")

		s.maintainOpenAICookieWSMinimum(context.Background(), a.ID)
		ready := get()
		require.Equal(t, groups, ready.VerifiedWS)
		require.True(t, ready.Ready)
		require.False(t, ready.Blocked)
		if groups == 3 {
			require.Equal(t, "ready", ready.RecoveryState)
		} else {
			require.Equal(t, "partial", ready.RecoveryState)
		}
		for _, slot := range ready.CookieSlots[:groups] {
			require.Equal(t, "ready", slot.State)
			require.Equal(t, 1, slot.VerifiedWS)
		}

		// The persisted Cookie remains valid when the last socket is closed.
		pool := s.getOpenAIWSConnPool()
		ap, ok := pool.getAccountPool(a.ID)
		require.True(t, ok)
		ap.mu.Lock()
		ids := make([]string, 0, len(ap.conns))
		for id := range ap.conns {
			ids = append(ids, id)
		}
		ap.mu.Unlock()
		for _, id := range ids {
			pool.evictConn(a.ID, id)
		}
		lost := get()
		require.Equal(t, groups, lost.CookieGroupsReady)
		require.Zero(t, lost.VerifiedWS)
		require.False(t, lost.Ready)
		require.Equal(t, "recovering", lost.RecoveryState)
		require.Len(t, dialer.conns, groups, "reading lost-connection state does not replenish it")
	}
}

func TestCookieWSRuntimeStatusUnschedulableAccountIsPausedDespiteLiveSockets(t *testing.T) {
	s, a, _ := cookieWarmupFixture(t, 3, "True")
	s.maintainOpenAICookieWSMinimum(context.Background(), a.ID)
	for _, tc := range []struct {
		name   string
		change func(*Account)
	}{
		{"manual_unschedulable", func(a *Account) { a.Schedulable = false }},
		{"rate_limited", func(a *Account) { until := time.Now().Add(time.Minute); a.RateLimitResetAt = &until }},
		{"runtime_blocked", func(a *Account) { s.openaiAccountRuntimeBlockUntil.Store(a.ID, time.Now().Add(time.Minute)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := *a
			tc.change(&copy)
			status := s.openAICookieWSRuntimeStatus(&copy, openAICodexTicketDefaultModel, time.Now())
			require.Equal(t, 3, status.VerifiedWS, "retain physical connection counts while pausing scheduling")
			require.False(t, status.Ready)
			require.True(t, status.Blocked)
			require.Equal(t, "paused", status.RecoveryState)
			require.Equal(t, tc.name, status.SkipReason)
			for _, slot := range status.CookieSlots {
				require.Equal(t, "paused", slot.State)
			}
		})
	}
}

func TestCookieWSRuntimeStatusIgnoresExpiredCookieAndRespectsPolicy(t *testing.T) {
	s, a, _ := cookieWarmupFixture(t, 3, "True")
	status := s.openAICookieWSRuntimeStatus(a, openAICodexTicketDefaultModel, time.Now().Add(61*time.Minute))
	require.Zero(t, status.CookieGroupsValid)
	require.Zero(t, status.CookieGroupsReady)
	require.Zero(t, status.RemainingSeconds)
	require.False(t, status.Ready)
	svc := &AccountTestService{openaiGatewayService: s}
	cfg := s.openAICodexTicketConfig()
	cfg.Enabled = false
	require.Empty(t, svc.OpenAICodexTicketStatuses(a, cfg, time.Now()))
	cfg.Enabled = true
	cfg.CookieWSAccountIDs = []int64{99}
	require.Empty(t, svc.OpenAICodexTicketStatuses(a, cfg, time.Now()))
}

func TestCookieWSRuntimeStatusSeparatesRefreshAndConnectionRecovery(t *testing.T) {
	s, a, _ := cookieWarmupFixture(t, 1, "True")
	now := time.Now()
	next := now.Add(time.Minute)
	s.beginOpenAICookieWSRecovery(a.ID, 0, "refresh", "harvesting")
	s.succeedOpenAICookieWSRecovery(a.ID, 0, "refresh")
	s.beginOpenAICookieWSRecovery(a.ID, 0, "warmup", "warming")
	s.failOpenAICookieWSRecovery(a.ID, 0, "warmup", "backoff",
		cookieWSRecoveryFailure("ws_handshake", "cookie_ws_ws_handshake_http_403", "Cookie recovery websocket handshake was rejected (HTTP 403)", 403, nil), &next)
	s.beginOpenAICookieWSRecovery(a.ID, 1, "refresh", "harvesting")
	s.beginOpenAICookieWSRecovery(a.ID, 2, "refresh", "restoring")
	status := s.openAICookieWSRuntimeStatus(a, openAICodexTicketDefaultModel, now)
	require.Equal(t, "recovering", status.RecoveryState)
	require.False(t, status.Ready)
	require.Equal(t, "backoff", status.CookieSlots[0].State)
	require.Equal(t, "idle", status.CookieSlots[0].Refresh.Phase)
	require.Nil(t, status.CookieSlots[0].Refresh.LastError)
	require.Equal(t, "ws_handshake", status.CookieSlots[0].Warmup.LastError.Stage)
	require.Equal(t, 403, status.CookieSlots[0].Warmup.LastError.HTTPStatus)
	require.WithinDuration(t, next, *status.CookieSlots[0].Warmup.NextAttemptAt, time.Millisecond)
	require.Equal(t, "harvesting", status.CookieSlots[1].State)
	require.Equal(t, "restoring", status.CookieSlots[2].State)
	// Returning a mutable DTO must not give callers ownership of the stored
	// diagnostic: serialization and later reads can run alongside recovery.
	status.CookieSlots[0].Warmup.LastError.Message = "caller mutation"
	*status.CookieSlots[0].Warmup.NextAttemptAt = time.Time{}
	fresh := s.openAICookieWSRuntimeStatus(a, openAICodexTicketDefaultModel, now)
	require.NotEqual(t, "caller mutation", fresh.CookieSlots[0].Warmup.LastError.Message)
	require.Equal(t, next, *fresh.CookieSlots[0].Warmup.NextAttemptAt)

	s.phaseOpenAICookieWSRecovery(a.ID, 0, "warmup", "backoff", nil)
	s.beginOpenAICookieWSRecovery(a.ID, 0, "warmup", "warming")
	active := s.openAICookieWSRuntimeStatus(a, openAICodexTicketDefaultModel, now)
	require.Equal(t, "warming", active.CookieSlots[0].State)
	require.Nil(t, active.CookieSlots[0].Warmup.NextAttemptAt)
}

func TestCookieWSRuntimeStatusLiveSocketSupersedesOldWarmupFailure(t *testing.T) {
	s, a, _ := cookieWarmupFixture(t, 1, "True")
	s.maintainOpenAICookieWSMinimum(context.Background(), a.ID)
	next := time.Now().Add(time.Minute)
	for _, operation := range []string{"refresh", "warmup"} {
		s.failOpenAICookieWSRecovery(a.ID, 0, operation, "backoff",
			cookieWSRecoveryStatusFailure("http_request", 503), &next)
	}
	status := s.openAICookieWSRuntimeStatus(a, openAICodexTicketDefaultModel, time.Now())
	require.Equal(t, "ready", status.CookieSlots[0].State)
	require.Equal(t, "idle", status.CookieSlots[0].Warmup.Phase)
	require.Nil(t, status.CookieSlots[0].Warmup.LastError)
	require.Nil(t, status.CookieSlots[0].Warmup.NextAttemptAt)
	require.NotNil(t, status.CookieSlots[0].Warmup.LastFailureAt)
	require.NotNil(t, status.CookieSlots[0].Refresh.LastError, "a pending Cookie refresh still matters while old connections work")
	require.NotNil(t, status.CookieSlots[0].Refresh.NextAttemptAt)
	require.NotNil(t, s.openAICookieWSRecoverySnapshot(a.ID, 0, "warmup").LastError, "admin reads never mutate the stored diagnostic")
}
