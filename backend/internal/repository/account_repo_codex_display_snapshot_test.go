package repository

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUpdateExtraCodexDisplaySnapshotsAvoidSchedulerOutbox(t *testing.T) {
	for _, key := range []string{"codex_credits_snapshot", "codex_referral_snapshot"} {
		for _, tc := range []struct {
			name             string
			value            any
			schedulingChange bool
		}{
			{"refresh", map[string]any{"fetched_at": 1770000000}, false},
			{"invalidate", nil, false},
			{"mixed scheduling update", nil, true},
		} {
			t.Run(key+"/"+tc.name, func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
				t.Cleanup(func() { _ = client.Close() })
				repo := newAccountRepositoryWithSQL(client, db, nil)
				updates := map[string]any{key: tc.value}
				if tc.schedulingChange {
					updates["openai_compact_enabled"] = true
					mock.ExpectBegin()
				}
				payload, err := json.Marshal(updates)
				require.NoError(t, err)
				// 未显式关闭 codex_fingerprint_mode 的 extra 补丁都会原子保留/补铸
				// 指纹种子（缺失按 full 处理），UpdateExtra 因而走带 CASE 的 SQL。
				updateSQL := "UPDATE accounts SET extra = " +
					ensureCodexFingerprintSeedSQL("COALESCE(extra, '{}'::jsonb) || $1::jsonb") +
					", updated_at = NOW() WHERE id = $2 AND deleted_at IS NULL"
				mock.ExpectExec(regexp.QuoteMeta(updateSQL)).
					WithArgs(string(payload), int64(27)).WillReturnResult(sqlmock.NewResult(0, 1))
				if tc.schedulingChange {
					mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
						WithArgs(service.SchedulerOutboxEventAccountChanged, int64(27), nil, nil, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
					mock.ExpectCommit()
				}
				// Snapshot-only changes must perform just the UPDATE, without even a
				// transaction or account_changed event that would rebuild scheduler buckets.
				require.NoError(t, repo.UpdateExtra(context.Background(), 27, updates))
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}
