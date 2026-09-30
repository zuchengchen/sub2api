//go:build integration

package repository

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Runs the real CTE on PostgreSQL: RETURNING must expose the pre-update
// deadline, and only an active pause may produce a "cleared" audit event.
func (s *AccountRepoSuite) TestClearTempUnschedulableAuditsOnlyActivePause() {
	sink := &tempUnschedAuditSink{}
	logger.SetSink(sink)
	s.T().Cleanup(func() { logger.SetSink(nil) })

	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-temp-audit-clear"})
	clearedEvents := func() int {
		n := 0
		for _, event := range sink.auditEvents() {
			if event.Fields["account_id"] == account.ID && event.Fields["action"] == accountTempUnschedAuditActionCleared {
				n++
			}
		}
		return n
	}

	// Never paused (the ClearRateLimit path): no event, row still cleared.
	s.Require().NoError(s.repo.ClearTempUnschedulable(s.ctx, account.ID))
	s.Require().Zero(clearedEvents())

	// Active pause lifted early: one event carrying the lifted deadline.
	until := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	s.Require().NoError(s.repo.SetTempUnschedulable(s.ctx, account.ID, until, "upstream transport error (proxy/network): connection refused"))
	s.Require().NoError(s.repo.ClearTempUnschedulable(s.ctx, account.ID))
	s.Require().Equal(1, clearedEvents())
	var lifted map[string]any
	for _, event := range sink.auditEvents() {
		if event.Fields["account_id"] == account.ID && event.Fields["action"] == accountTempUnschedAuditActionCleared {
			lifted = event.Fields
		}
	}
	s.Require().Equal(until.Format(time.RFC3339), lifted["previous_until"])

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Nil(got.TempUnschedulableUntil)
	s.Require().Empty(got.TempUnschedulableReason)

	// Clearing again, and clearing an already-expired pause, add nothing.
	s.Require().NoError(s.repo.ClearTempUnschedulable(s.ctx, account.ID))
	_, err = s.repo.sql.ExecContext(s.ctx,
		`UPDATE accounts SET temp_unschedulable_until = NOW() - INTERVAL '1 minute', temp_unschedulable_reason = 'expired' WHERE id = $1`, account.ID)
	s.Require().NoError(err)
	s.Require().NoError(s.repo.ClearTempUnschedulable(s.ctx, account.ID))
	s.Require().Equal(1, clearedEvents())

	got, err = s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Nil(got.TempUnschedulableUntil, "expired pause columns must still be reset")

	// A missing account keeps the old contract: no error, no event.
	s.Require().NoError(s.repo.ClearTempUnschedulable(s.ctx, account.ID+1_000_000))
	s.Require().Equal(1, clearedEvents())
}
