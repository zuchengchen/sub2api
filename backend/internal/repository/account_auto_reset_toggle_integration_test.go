//go:build integration

package repository

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (s *AccountRepoSuite) TestAutoResetCreditTogglePreservesExtra() {
	state := map[string]any{
		"status": "resetting", "attempt_credit_hash": "reserved-credit", "attempt_cycle_hash": "reserved-cycle",
		"checked_at": time.Now().UTC().Format(time.RFC3339),
	}
	extra := map[string]any{
		service.OpenAIAutoResetCreditEnabledExtraKey:     true,
		service.OpenAIAutoResetCredit5hThresholdExtraKey: 0.75,
		service.OpenAIAutoResetCredit7dThresholdExtraKey: "0.9",
		service.OpenAIAutoResetCreditStateExtraKey:       state,
		"model_rate_limits":                              map[string]any{"example": map[string]any{"reset_at": "2027-01-01T00:00:00Z"}},
		"codex_credits_snapshot":                         map[string]any{"balance": float64(10)},
	}
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: "automatic-reset-toggle", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Extra: extra,
	})
	s.False(service.ResolveOpenAIAutoResetCreditConfig(account).Enabled, "legacy enabled flag requires fresh opt-in")
	svc := service.NewAdminService(nil, nil, nil, s.repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, s.client, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	for _, enabled := range []bool{true, false, true} {
		updated, err := svc.SetOpenAIAutoResetCreditEnabled(s.ctx, account.ID, enabled)
		s.Require().NoError(err)
		config := service.ResolveOpenAIAutoResetCreditConfig(updated)
		s.Equal(enabled, config.Enabled)
		s.Equal(0.75, config.Threshold5h)
		s.Equal(0.9, config.Threshold7d)
		for key, value := range extra {
			if key != service.OpenAIAutoResetCreditEnabledExtraKey {
				s.Equal(value, updated.Extra[key], key)
			}
		}
		persisted, err := s.repo.GetByID(s.ctx, account.ID)
		s.Require().NoError(err)
		s.Equal(updated.Extra, persisted.Extra)
		if enabled {
			s.Equal(float64(1), persisted.Extra[service.OpenAIAutoResetCreditOptInVersionExtraKey])
		} else {
			s.Nil(persisted.Extra[service.OpenAIAutoResetCreditOptInVersionExtraKey])
		}
		var outboxCount int
		s.Require().NoError(scanSingleRow(s.ctx, s.repo.sql, "SELECT COUNT(*) FROM scheduler_outbox WHERE account_id = $1", []any{account.ID}, &outboxCount))
		s.Equal(1, outboxCount, "each toggle retains a durable notification, coalesced by the existing account outbox deduplication")
	}
}
