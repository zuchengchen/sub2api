package repository

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// tempUnschedAuditSink captures sink events. Tests using it must not call
// t.Parallel: logger.SetSink is process-global.
type tempUnschedAuditSink struct {
	mu     sync.Mutex
	events []*logger.LogEvent
}

func (s *tempUnschedAuditSink) WriteLogEvent(event *logger.LogEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *tempUnschedAuditSink) auditEvents() []*logger.LogEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*logger.LogEvent, 0, len(s.events))
	for _, event := range s.events {
		if event.Component == accountTempUnschedAuditComponent {
			out = append(out, event)
		}
	}
	return out
}

func installTempUnschedAuditSink(t *testing.T) *tempUnschedAuditSink {
	t.Helper()
	sink := &tempUnschedAuditSink{}
	logger.SetSink(sink)
	t.Cleanup(func() { logger.SetSink(nil) })
	return sink
}

func TestAccountTempUnschedAudit_SetTempUnschedulableAppliedRecordsEvent(t *testing.T) {
	sink := installTempUnschedAuditSink(t)
	repo := newAccountRepositoryWithSQL(nil, &recordingSQLExecutor{result: rowsAffectedResult(1)}, nil)
	until := time.Now().Add(10 * time.Minute)

	require.NoError(t, repo.SetTempUnschedulable(context.Background(), 22173, until,
		"upstream transport error (proxy/network): dial tcp 127.0.0.1:8992: connect: connection refused"))

	events := sink.auditEvents()
	require.Len(t, events, 1)
	event := events[0]
	require.Equal(t, "info", event.Level)
	require.Equal(t, "account temp-unschedulable set", event.Message)
	require.Equal(t, int64(22173), event.Fields["account_id"])
	require.Equal(t, accountTempUnschedAuditActionSet, event.Fields["action"])
	require.Equal(t, "SetTempUnschedulable", event.Fields["writer"])
	require.Equal(t, until.UTC().Format(time.RFC3339), event.Fields["until"])
	require.InDelta(t, 600, event.Fields["duration_seconds"], 2)
	require.Contains(t, event.Fields["reason"], "connection refused")
	caller, _ := event.Fields["caller"].(string)
	require.Contains(t, caller, "TestAccountTempUnschedAudit_SetTempUnschedulableAppliedRecordsEvent")
	require.NotContains(t, caller, "(*accountRepository)", "writer frames must be skipped so the caller shows the triggering subsystem")
}

func TestAccountTempUnschedAudit_NotAppliedOrFailedWritesRecordNothing(t *testing.T) {
	sink := installTempUnschedAuditSink(t)
	until := time.Now().Add(time.Minute)
	snapshot := service.GrokCredentialMutationSnapshot{CredentialsJSON: `{"refresh_token":"r"}`}

	unchanged := newAccountRepositoryWithSQL(nil, &recordingSQLExecutor{result: rowsAffectedResult(0)}, nil)
	require.NoError(t, unchanged.SetTempUnschedulable(context.Background(), 1, until, "already paused longer"))
	applied, err := unchanged.SetGrokCredentialTempUnschedulableIfMatch(context.Background(), 1, snapshot, until, "transient")
	require.NoError(t, err)
	require.False(t, applied)
	applied, err = unchanged.SetGrokOAuthRefreshTempUnschedulableIfCredentialsUnchanged(context.Background(), 1, map[string]any{"refresh_token": "r"}, nil, until, "transient")
	require.NoError(t, err)
	require.False(t, applied)

	failing := newAccountRepositoryWithSQL(nil, &recordingSQLExecutor{err: errors.New("db down")}, nil)
	require.Error(t, failing.SetTempUnschedulable(context.Background(), 1, until, "x"))

	require.Empty(t, sink.auditEvents())
}

func TestAccountTempUnschedAudit_EveryWriterRecordsWhenApplied(t *testing.T) {
	sink := installTempUnschedAuditSink(t)
	until := time.Now().Add(time.Minute)
	repo := newAccountRepositoryWithSQL(nil, &recordingSQLExecutor{result: rowsAffectedResult(1)}, nil)

	applied, err := repo.SetGrokCredentialTempUnschedulableIfMatch(context.Background(), 7,
		service.GrokCredentialMutationSnapshot{CredentialsJSON: `{"refresh_token":"r"}`}, until, "grok transient")
	require.NoError(t, err)
	require.True(t, applied)
	applied, err = repo.SetGrokOAuthRefreshTempUnschedulableIfCredentialsUnchanged(context.Background(), 8,
		map[string]any{"refresh_token": "r"}, nil, until, "token refresh retry exhausted: timeout")
	require.NoError(t, err)
	require.True(t, applied)

	events := sink.auditEvents()
	require.Len(t, events, 2)
	require.Equal(t, "SetGrokCredentialTempUnschedulableIfMatch", events[0].Fields["writer"])
	require.Equal(t, int64(7), events[0].Fields["account_id"])
	require.Equal(t, "SetGrokOAuthRefreshTempUnschedulableIfCredentialsUnchanged", events[1].Fields["writer"])
	require.Equal(t, int64(8), events[1].Fields["account_id"])
}

const clearTempUnschedQueryPattern = `(?s)WITH prev AS \(.*FOR UPDATE.*UPDATE accounts AS a.*RETURNING prev\.temp_unschedulable_until`

// ClearTempUnschedulable must keep its scheduler side effects (outbox + snapshot)
// in every non-error case, but only an active pause produces an audit event.
func TestAccountTempUnschedAudit_ClearRecordsOnlyActivePause(t *testing.T) {
	activeUntil := time.Now().Add(7 * time.Minute).UTC().Truncate(time.Second)
	tests := []struct {
		name      string
		rows      *sqlmock.Rows
		wantEvent bool
	}{
		{name: "active pause lifted early", rows: sqlmock.NewRows([]string{"until", "active"}).AddRow(activeUntil, true), wantEvent: true},
		{name: "rate-limit-only account, never paused", rows: sqlmock.NewRows([]string{"until", "active"}).AddRow(nil, false)},
		{name: "pause already expired", rows: sqlmock.NewRows([]string{"until", "active"}).AddRow(time.Now().Add(-time.Minute), false)},
		{name: "account missing or deleted", rows: sqlmock.NewRows([]string{"until", "active"})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sink := installTempUnschedAuditSink(t)
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			mock.ExpectQuery(clearTempUnschedQueryPattern).WithArgs(int64(22173)).WillReturnRows(tc.rows)
			mock.ExpectExec(`INSERT INTO scheduler_outbox`).WillReturnResult(sqlmock.NewResult(1, 1))

			repo := newAccountRepositoryWithSQL(nil, db, nil)
			require.NoError(t, repo.ClearTempUnschedulable(context.Background(), 22173))
			require.NoError(t, mock.ExpectationsWereMet(), "outbox must still be enqueued")

			events := sink.auditEvents()
			if !tc.wantEvent {
				require.Empty(t, events)
				return
			}
			require.Len(t, events, 1)
			cleared := events[0]
			require.Equal(t, "account temp-unschedulable cleared", cleared.Message)
			require.Equal(t, accountTempUnschedAuditActionCleared, cleared.Fields["action"])
			require.Equal(t, "ClearTempUnschedulable", cleared.Fields["writer"])
			require.Equal(t, int64(22173), cleared.Fields["account_id"])
			require.Equal(t, activeUntil.Format(time.RFC3339), cleared.Fields["previous_until"])
			require.InDelta(t, 420, cleared.Fields["remaining_before_seconds"], 2)
			require.NotContains(t, cleared.Fields, "until")
		})
	}
}

func TestAccountTempUnschedAudit_ClearQueryErrorReturnsWithoutSideEffects(t *testing.T) {
	sink := installTempUnschedAuditSink(t)
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectQuery(clearTempUnschedQueryPattern).WillReturnError(errors.New("db down"))

	repo := newAccountRepositoryWithSQL(nil, db, nil)
	require.Error(t, repo.ClearTempUnschedulable(context.Background(), 22173))
	require.NoError(t, mock.ExpectationsWereMet(), "no outbox write after a failed clear")
	require.Empty(t, sink.auditEvents())
}

func TestAccountTempUnschedAuditReason_RedactsAndBounds(t *testing.T) {
	require.NotContains(t, accountTempUnschedAuditReason("refresh failed access_token=abc123 status=401"), "abc123")

	long := accountTempUnschedAuditReason(strings.Repeat("界", accountTempUnschedAuditReasonMaxRunes+50))
	require.Equal(t, accountTempUnschedAuditReasonMaxRunes+1, len([]rune(long)))
	require.True(t, strings.HasSuffix(long, "…"))
}

type tempUnschedAuditOpsRepo struct {
	service.OpsRepository // only BatchInsertSystemLogs is exercised
	mu                    sync.Mutex
	inputs                []*service.OpsInsertSystemLogInput
}

func (r *tempUnschedAuditOpsRepo) BatchInsertSystemLogs(_ context.Context, inputs []*service.OpsInsertSystemLogInput) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inputs = append(r.inputs, inputs...)
	return int64(len(inputs)), nil
}

// The production sink only indexes warn+ levels unless the component contains
// "audit"; this pins that the info-level event reaches ops_system_logs with
// its account_id column populated. WriteSinkEvent bypasses the global level,
// so production's log.level=error does not suppress it.
func TestAccountTempUnschedAudit_ReachesOpsSystemLogSink(t *testing.T) {
	opsRepo := &tempUnschedAuditOpsRepo{}
	sink := service.NewOpsSystemLogSink(opsRepo)
	sink.Start()
	logger.SetSink(sink)
	t.Cleanup(func() { logger.SetSink(nil) })

	repo := newAccountRepositoryWithSQL(nil, &recordingSQLExecutor{result: rowsAffectedResult(1)}, nil)
	require.NoError(t, repo.SetTempUnschedulable(context.Background(), 22173, time.Now().Add(5*time.Minute), "health:auto err_rate=0.75"))
	sink.Stop() // drains and flushes the queue synchronously

	opsRepo.mu.Lock()
	defer opsRepo.mu.Unlock()
	var row *service.OpsInsertSystemLogInput
	for _, input := range opsRepo.inputs {
		if input.Component == accountTempUnschedAuditComponent {
			row = input
		}
	}
	require.NotNil(t, row, "audit event must be indexed by the ops system-log sink")
	require.Equal(t, "info", row.Level)
	require.NotNil(t, row.AccountID)
	require.Equal(t, int64(22173), *row.AccountID)

	var extra map[string]any
	require.NoError(t, json.Unmarshal([]byte(row.ExtraJSON), &extra))
	require.Equal(t, "health:auto err_rate=0.75", extra["reason"])
	require.Equal(t, accountTempUnschedAuditActionSet, extra["action"])
}
