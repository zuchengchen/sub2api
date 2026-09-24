package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAICookieWSAccountSkipReasonAllSchedulingStops(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	future, past := now.Add(time.Hour), now.Add(-time.Second)
	tests := []struct {
		name, reason string
		change       func(*Account)
	}{
		{"disabled", "not_active", func(a *Account) { a.Status = StatusDisabled }},
		{"manual", "manual_unschedulable", func(a *Account) { a.Schedulable = false }},
		{"expired", "expired", func(a *Account) { a.AutoPauseOnExpired = true; a.ExpiresAt = &past }},
		{"rate limited", "rate_limited", func(a *Account) { a.RateLimitResetAt = &future }},
		{"overload", "overloaded", func(a *Account) { a.OverloadUntil = &future }},
		{"temporary", "temporarily_unschedulable", func(a *Account) { a.TempUnschedulableUntil = &future }},
		{"model", "model_rate_limited", func(a *Account) {
			setAccountModelRateLimitSnapshot(a, openAICodexTicketDefaultModel, future, "test", now)
		}},
		{"quota five hour", "quota_5h", func(a *Account) {
			a.Extra = map[string]any{"codex_5h_used_percent": 100.0, "codex_5h_reset_at": future.Format(time.RFC3339)}
		}},
		{"quota weekly", "quota_7d", func(a *Account) {
			a.Extra = map[string]any{"codex_7d_used_percent": 100.0, "codex_7d_reset_at": future.Format(time.RFC3339)}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := ticketTestAccount(41)
			tt.change(a)
			require.Equal(t, tt.reason, openAICookieWSAccountSkipReason(a, now))
		})
	}
	a := ticketTestAccount(41)
	a.RateLimitResetAt = &now
	a.OverloadUntil = &past
	a.TempUnschedulableUntil = &past
	setAccountModelRateLimitSnapshot(a, openAICodexTicketDefaultModel, past, "test", now)
	require.Empty(t, openAICookieWSAccountSkipReason(a, now), "expired cooldowns stop blocking exactly at reset")
	setAccountModelRateLimitSnapshot(a, "gpt-6-sol", future, "test", now)
	require.Empty(t, openAICookieWSAccountSkipReason(a, now), "unrelated model does not block Astra")
}

func TestOpenAICookieWSLatestAccountFailsClosedOnMissingAndRepositoryError(t *testing.T) {
	s, _ := cookieWSTestService(t, nil)
	s.accountRepo = nil
	_, err := s.latestOpenAICookieWSAccount(context.Background(), 41)
	require.ErrorIs(t, err, errOpenAICookieWSAccountUnavailable)
	s.accountRepo = &cookieWSLifecycleRepo{getErr: errors.New("db unavailable")}
	_, err = s.latestOpenAICookieWSAccount(context.Background(), 41)
	require.ErrorIs(t, err, errOpenAICookieWSAccountUnavailable)
	s.accountRepo = &cookieWSLifecycleRepo{}
	_, err = s.latestOpenAICookieWSAccount(context.Background(), 41)
	require.ErrorIs(t, err, errOpenAICookieWSAccountUnavailable)
}

type cookieWSEligibilityUpstream struct {
	HTTPUpstream
	requests  int
	onRequest func()
}

func (u *cookieWSEligibilityUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.requests++
	if u.onRequest != nil {
		u.onRequest()
	}
	return cookieWSHTTPResponse("True"), nil
}

func TestOpenAICookieWSHTTPUsesLatestSchedulingState(t *testing.T) {
	u := &cookieWSEligibilityUpstream{}
	s, dialer := cookieWSTestService(t, u, "True", "True")
	stale := ticketTestAccount(41)
	blocked := *stale
	future := time.Now().Add(time.Hour)
	blocked.RateLimitResetAt = &future
	s.accountRepo = &cookieWSLifecycleRepo{accounts: []Account{blocked}}
	s.refreshOpenAICookieWSSlot(context.Background(), stale, 0)
	require.Zero(t, u.requests)
	require.Zero(t, dialer.dials)
	_, err := s.doOpenAICookieWSHTTPProbe(context.Background(), stale, "stale-token", "socks5h://example", newOpenAICookieWSIdentity())
	require.ErrorIs(t, err, errOpenAICookieWSAccountUnavailable)
	require.Zero(t, u.requests)
}

func TestOpenAICookieWSStopsBeforeWSWhenAccountChangesDuringHTTP(t *testing.T) {
	u := &cookieWSEligibilityUpstream{}
	s, dialer := cookieWSTestService(t, u, "True", "True")
	repo := s.accountRepo.(*cookieWSLifecycleRepo)
	u.onRequest = func() {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		future := time.Now().Add(time.Hour)
		repo.accounts[0].TempUnschedulableUntil = &future
	}
	s.refreshOpenAICookieWSSlot(context.Background(), ticketTestAccount(41), 0)
	require.Equal(t, 1, u.requests)
	require.Zero(t, dialer.dials)
	require.Nil(t, s.lookupOpenAICookieWSTicketSlot(ticketTestAccount(41), openAICodexTicketDefaultModel, 0))
	s.refreshOpenAICookieWSSlot(context.Background(), ticketTestAccount(41), 1)
	s.refreshOpenAICookieWSSlot(context.Background(), ticketTestAccount(41), 2)
	require.Equal(t, 1, u.requests, "next slot must not reuse stale healthy snapshot")
}

func TestOpenAICookieWSBackgroundDoesNotProbeRateLimitedAccounts(t *testing.T) {
	u := &cookieWSEligibilityUpstream{}
	s, dialer := cookieWSTestService(t, u, "True", "True")
	repo := s.accountRepo.(*cookieWSLifecycleRepo)
	repo.accounts = repo.accounts[:1]
	future := time.Now().Add(time.Hour)
	repo.accounts[0].OverloadUntil = &future
	s.refreshOpenAICookieWSTickets(context.Background())
	require.Zero(t, u.requests)
	require.Zero(t, dialer.dials)
}

func TestOpenAICookieWSCandidateRechecksBeforeEveryValidationTurn(t *testing.T) {
	s, dialer := cookieWSTestService(t, nil, "True", "True")
	repo := s.accountRepo.(*cookieWSLifecycleRepo)
	dialer.conn.afterRead = func() { repo.mu.Lock(); defer repo.mu.Unlock(); repo.accounts[0].Schedulable = false }
	lease, err := s.validateOpenAICookieWSCandidate(context.Background(), ticketTestAccount(41), "stale-token", cookieWSTestTicket(41, time.Now()))
	require.ErrorIs(t, err, errOpenAICookieWSAccountUnavailable)
	require.Nil(t, lease)
	require.Len(t, dialer.conn.writes, 1, "a scheduling stop after first answer must prevent second probe")
	require.True(t, dialer.conn.closed.Load(), "stopped candidate cannot remain in the pool")
}
