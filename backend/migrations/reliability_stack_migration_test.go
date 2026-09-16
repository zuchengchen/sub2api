package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBillingAttemptOutboxMigrationDefinesDurableContract(t *testing.T) {
	content, err := FS.ReadFile("244_billing_attempt_outbox.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(content))
	require.Contains(t, sql, "create table if not exists billing_attempt_outbox")
	require.Contains(t, sql, "unique (attempt_id, api_key_id)")
	require.Contains(t, sql, "finalization_pending")
	require.Contains(t, sql, "idx_billing_attempt_outbox_claim")
}

func TestSchedulerDirtyWorkMigrationKeepsSourcePromotionContracts(t *testing.T) {
	content, err := FS.ReadFile("249_scheduler_dirty_work.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(content))
	require.Contains(t, sql, "create table if not exists scheduler_dirty_work")
	require.Contains(t, sql, "kind in (1, 2, 3)")
	require.Contains(t, sql, "scheduler_dirty_account_sources")
	require.Contains(t, sql, "scheduler_dirty_group_sources")
	require.Contains(t, sql, "scheduler_dirty_membership_sources")
}

func TestSchedulerOwnershipEpochMigrationIsDurableSingleton(t *testing.T) {
	content, err := FS.ReadFile("251_scheduler_ownership_epoch.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(content))
	require.Contains(t, sql, "create table if not exists scheduler_ownership_epoch")
	require.Contains(t, sql, "singleton")
}

func TestAPIKeyConcurrencyMigrationDefaultsUnlimited(t *testing.T) {
	content, err := FS.ReadFile("243_api_key_concurrency.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(content))
	require.Contains(t, sql, "add column if not exists concurrency")
	require.Contains(t, sql, "default 0")
}
