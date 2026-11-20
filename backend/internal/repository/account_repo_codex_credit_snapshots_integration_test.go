//go:build integration

package repository

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (s *AccountRepoSuite) TestCodexCreditSnapshotsKeepNewerObservationsIndependently() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: s.T().Name(), Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Extra: map[string]any{codexCreditsSnapshotKey: map[string]any{"fetched_at": 10}},
	})
	points := func(query int64, balance string) map[string]any {
		return map[string]any{"query_started_at_us": query, "credits": map[string]any{"balance": balance}}
	}
	cards := func(query int64, count int) map[string]any {
		return map[string]any{"query_started_at_us": query, "available_count": count}
	}
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{
		codexCreditsSnapshotKey: points(30, "42"), codexResetCreditSnapshotKey: cards(10, 3),
	}))
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{
		codexCreditsSnapshotKey: points(20, "100"), codexResetCreditSnapshotKey: cards(20, 2),
		"codex_usage_updated_at": "example",
	}))
	stored, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Equal("example", stored.Extra["codex_usage_updated_at"])
	s.Equal("42", stored.Extra[codexCreditsSnapshotKey].(map[string]any)["credits"].(map[string]any)["balance"])
	s.EqualValues(2, stored.Extra[codexResetCreditSnapshotKey].(map[string]any)["available_count"])

	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{
		codexCreditsSnapshotKey: points(40, "0"), codexResetCreditSnapshotKey: map[string]any{"available_count": 9},
	}))
	stored, err = s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Equal("0", stored.Extra[codexCreditsSnapshotKey].(map[string]any)["credits"].(map[string]any)["balance"])
	s.EqualValues(2, stored.Extra[codexResetCreditSnapshotKey].(map[string]any)["available_count"])
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{codexCreditsSnapshotKey: nil}))
	stored, err = s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Nil(stored.Extra[codexCreditsSnapshotKey])
	s.Contains(stored.Extra, codexResetCreditSnapshotKey)
}

func (s *AccountRepoSuite) TestCodexCreditSnapshotsFollowTheEditedOAuthOwner() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: s.T().Name(), Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "first", "chatgpt_account_id": "shared-workspace", "chatgpt_user_id": "first-user"},
	})
	staleForm, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	startedAt := time.Now().Add(-time.Second).UnixMicro()
	snapshots := map[string]any{
		codexCreditsSnapshotKey:      map[string]any{"query_started_at_us": startedAt, "credits": map[string]any{"balance": "42"}},
		codexResetCreditSnapshotKey:  map[string]any{"query_started_at_us": startedAt, "available_count": 2},
		codexUsageObservedAtKey:      startedAt,
		"codex_primary_used_percent": 76,
		"codex_5h_used_percent":      76,
		"codex_7d_used_percent":      95,
	}
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, snapshots))
	staleForm.Credentials["access_token"] = "rotated"
	s.Require().NoError(s.repo.Update(s.ctx, staleForm))
	stored, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Contains(stored.Extra, codexCreditsSnapshotKey)
	s.Contains(stored.Extra, codexResetCreditSnapshotKey)
	s.EqualValues(76, stored.Extra["codex_primary_used_percent"])
	s.EqualValues(95, stored.Extra["codex_7d_used_percent"])
	s.NotContains(stored.Extra, codexCreditOwnerChangedKey)

	stored.Credentials["chatgpt_user_id"] = "second-user"
	s.Require().NoError(s.repo.Update(s.ctx, stored))
	// A periodic query for the first user finishes after the edit.
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, snapshots))
	stored, err = s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.NotContains(stored.Extra, codexCreditsSnapshotKey)
	s.NotContains(stored.Extra, codexResetCreditSnapshotKey)
	s.NotContains(stored.Extra, "codex_primary_used_percent")
	s.NotContains(stored.Extra, "codex_5h_used_percent")
	s.NotContains(stored.Extra, "codex_7d_used_percent")
	s.NotContains(stored.Extra, codexUsageObservedAtKey)
	reboundAt := int64(stored.Extra[codexCreditOwnerChangedKey].(float64))
	s.Greater(reboundAt, startedAt)

	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{
		codexCreditsSnapshotKey: map[string]any{"query_started_at_us": reboundAt + 1, "credits": map[string]any{"balance": "18"}},
	}))
	stored, err = s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Contains(stored.Extra, codexCreditsSnapshotKey)
	s.NotContains(stored.Extra, codexResetCreditSnapshotKey)
}

func (s *AccountRepoSuite) TestCodexWindowSnapshotsRejectLateReadsAndKeepMissingWindows() {
	const legacyAt = "2026-03-07T12:00:00.123456789+08:00"
	base, err := time.Parse(time.RFC3339Nano, legacyAt)
	s.Require().NoError(err)
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: s.T().Name(), Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Extra: map[string]any{"codex_usage_updated_at": legacyAt, "codex_5h_used_percent": 51, "codex_7d_used_percent": 72},
	})
	// One poll started before the persisted header sample and finished after it.
	// Its points and reset counts still advance independently of the windows.
	before := base.Add(-time.Microsecond).UnixMicro()
	readback := map[string]any{
		codexUsageObservedAtKey: before, "codex_usage_updated_at": base.Add(time.Second).Format(time.RFC3339Nano),
		"codex_5h_used_percent": 19, "codex_7d_used_percent": 20,
		codexCreditsSnapshotKey:     map[string]any{"query_started_at_us": before, "credits": map[string]any{"balance": "27"}},
		codexResetCreditSnapshotKey: map[string]any{"query_started_at_us": before, "available_count": 2},
	}
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, readback))
	stored, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Equal(legacyAt, stored.Extra["codex_usage_updated_at"])
	s.EqualValues(51, stored.Extra["codex_5h_used_percent"])
	s.EqualValues(72, stored.Extra["codex_7d_used_percent"])
	s.Contains(stored.Extra, codexCreditsSnapshotKey)
	s.Contains(stored.Extra, codexResetCreditSnapshotKey)

	later := base.Add(10 * time.Microsecond)
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{
		"codex_usage_updated_at": later.Format(time.RFC3339Nano), "codex_5h_used_percent": 85,
	}))
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, readback))
	stored, err = s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.EqualValues(later.UnixMicro(), stored.Extra[codexUsageObservedAtKey])
	s.EqualValues(85, stored.Extra["codex_5h_used_percent"])
	s.EqualValues(72, stored.Extra["codex_7d_used_percent"])
}
