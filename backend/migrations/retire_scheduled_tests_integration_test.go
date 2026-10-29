//go:build integration

package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestMigration272RetiresScheduledTestsWithoutLosingHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Match migration 271's isolated-cluster mode; never migrate the URL's DB.
	dsn := os.Getenv("MIGRATION_272_TEST_ADMIN_DSN")
	if dsn == "" {
		if exec.CommandContext(ctx, "docker", "info").Run() != nil {
			t.Fatal("Docker or MIGRATION_272_TEST_ADMIN_DSN for an isolated PostgreSQL test cluster is required")
		}
		container, err := tcpostgres.Run(ctx, "postgres:18.1-alpine3.23",
			tcpostgres.WithDatabase("postgres"),
			tcpostgres.WithUsername("postgres"),
			tcpostgres.WithPassword("postgres"),
			tcpostgres.BasicWaitStrategies(),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = container.Terminate(context.Background()) })
		dsn, err = container.ConnectionString(ctx, "sslmode=disable", "TimeZone=UTC")
		require.NoError(t, err)
	}
	adminURL, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"postgres", "postgresql"}, adminURL.Scheme)
	require.Equal(t, "/postgres", adminURL.Path, "use the isolated cluster's maintenance database")
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	var maintenanceDB string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT current_database()`).Scan(&maintenanceDB))
	require.Equal(t, "postgres", maintenanceDB)
	databaseName := fmt.Sprintf("sub2api_migration272_%d", time.Now().UnixNano())
	_, err = admin.ExecContext(ctx, `CREATE DATABASE `+databaseName+` TEMPLATE template0`)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, err := admin.ExecContext(cleanupCtx, `DROP DATABASE `+databaseName)
		require.NoError(t, err, "remove isolated migration database")
	})
	adminURL.Path = "/" + databaseName
	db, err := sql.Open("postgres", adminURL.String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	var actualDB, postgresVersion string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT current_database(), version()`).Scan(&actualDB, &postgresVersion))
	require.Equal(t, databaseName, actualDB)
	t.Logf("Verified isolated database %s: %s", actualDB, postgresVersion)

	apply := func(name string) int64 {
		t.Helper()
		body, err := FS.ReadFile(name)
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		result, err := tx.ExecContext(ctx, string(body))
		require.NoError(t, err, name)
		changed, err := result.RowsAffected()
		require.NoError(t, err)
		require.NoError(t, tx.Commit(), name)
		return changed
	}
	// Use the real plan/result foreign keys and account scheduler triggers.
	for _, name := range []string{
		"001_init.sql", "005_schema_parity.sql", "020_add_temp_unschedulable.sql",
		"030_add_account_expires_at.sql", "036_scheduler_outbox.sql",
		"037_add_account_rate_multiplier.sql", "066_add_scheduled_test_tables.sql",
		"067_add_account_load_factor.sql", "070_add_scheduled_test_auto_recover.sql",
		"125_add_channel_monitors.sql", "249_scheduler_dirty_work.sql",
		"255_intelligent_tests.sql",
	} {
		apply(name)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO accounts (id, name, platform, type, credentials, extra, status, schedulable, updated_at, deleted_at)
		VALUES
		(1, 'ordinary-oauth', 'openai', 'oauth', '{"access_token":"test-only"}', '{"custom":true}', 'active', true, '2026-01-02', NULL),
		(2, 'ordinary-apikey', 'anthropic', 'apikey', '{"api_key":"test-only"}', '{}', 'error', false, '2026-01-02', NULL),
		(3, 'deleted-account', 'openai', 'oauth', '{}', '{}', 'active', false, '2026-01-02', '2026-01-02');
		INSERT INTO groups (id, name) VALUES (1, 'ordinary-group');
		INSERT INTO account_groups (account_id, group_id) VALUES (1, 1), (2, 1);
		INSERT INTO scheduled_test_plans
		    (id, account_id, model_id, cron_expression, max_results, enabled, auto_recover, next_run_at, last_run_at, created_at, updated_at)
		VALUES
		(1, 1, 'gpt-5', '*/7 * * * *', 1, true, true, '2026-12-01', '2026-01-01', '2025-12-01', '2026-01-02'),
		(2, 2, 'claude-custom', '0 3 * * 1', 10, true, false, NULL, NULL, '2025-12-01', '2026-01-02'),
		(3, 1, 'custom-model', '0 */2 * * *', 20, false, true, NULL, '2026-01-01', '2025-12-01', '2026-01-02'),
		(4, 2, 'another-model', '15 * * * *', 30, false, false, '2026-01-01', '2026-01-01', '2025-12-01', '2026-01-02'),
		(5, 1, 'gpt-5.3-codex-spark', '*/30 * * * *', 40, false, false, NULL, '2026-01-01', '2025-12-01', '2026-01-02'),
		(6, 3, 'deleted-account-model', '0 12 * * *', 50, true, true, '2026-12-01', '2026-01-01', '2025-12-01', '2026-01-02');
		INSERT INTO scheduled_test_results (plan_id, status, response_text, error_message, latency_ms, started_at, finished_at, created_at)
		SELECT id, 'success', 'historical response ' || id, '', 123, '2026-01-01', '2026-01-01 00:00:01', '2026-01-01 00:00:01'
		FROM scheduled_test_plans;
		INSERT INTO scheduled_test_results (plan_id, status, error_message, latency_ms)
		VALUES (1, 'error', 'historical failure beyond max_results', 456);
		INSERT INTO scheduler_outbox (event_type, account_id, payload)
		VALUES ('account_changed', 1, '{"group_ids":[1]}');
		INSERT INTO scheduler_dirty_work (kind, entity_id, generation) VALUES (1, 1, 7);
		INSERT INTO channel_monitors (id, name, provider, endpoint, api_key_encrypted, primary_model, enabled, interval_seconds, last_checked_at, created_by)
		VALUES (1, 'active-monitor', 'openai', 'https://example.test', 'test-only', 'gpt-5', true, 60, '2026-01-01', 1);
		INSERT INTO channel_monitor_histories (monitor_id, model, status, latency_ms)
		VALUES (1, 'gpt-5', 'operational', 100);
		INSERT INTO account_tests (account_id, test_type, input, anti_degradation, config_snapshot, requested_by)
		VALUES (1, 'pelican', 'test input', true, '{"keep":true}', 1);
	`)
	require.NoError(t, err)
	snapshot := func(query string) string {
		t.Helper()
		var result string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(rows)), '[]'::jsonb)::text FROM (`+query+`) AS rows`).Scan(&result))
		return result
	}
	preserved := map[string]string{}
	for _, table := range []string{
		"accounts", "groups", "account_groups", "scheduled_test_results",
		"scheduler_outbox", "scheduler_dirty_work", "scheduler_dirty_account_sources",
		"scheduler_dirty_group_sources", "scheduler_dirty_membership_sources",
		"channel_monitors", "channel_monitor_histories", "test_settings", "account_tests",
	} {
		query := `SELECT * FROM ` + table + ` ORDER BY 1, 2`
		preserved[query] = snapshot(query)
	}
	for _, query := range []string{
		`SELECT id, account_id, model_id, cron_expression, max_results, last_run_at, created_at FROM scheduled_test_plans ORDER BY id`,
		`SELECT * FROM scheduled_test_plans WHERE id = 5`,
	} {
		preserved[query] = snapshot(query)
	}

	const migration = "272_retire_scheduled_tests.sql"
	require.EqualValues(t, 5, apply(migration), "retire enabled, auto-recover-only, next-run-only and deleted-account plans")
	var allRetired, changedTimestamps bool
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT bool_and(NOT enabled AND NOT auto_recover AND next_run_at IS NULL),
		       bool_and(updated_at > '2026-01-02'::timestamptz) FILTER (WHERE id <> 5)
		FROM scheduled_test_plans`).Scan(&allRetired, &changedTimestamps))
	require.True(t, allRetired)
	require.True(t, changedTimestamps)
	for query, before := range preserved {
		require.JSONEq(t, before, snapshot(query), query)
	}
	t.Log("All six plans retired; five changed timestamps, already-retired plan and seven historical results preserved")

	// A fresh transaction must perform no writes, including timestamp updates.
	const allPlans = `SELECT * FROM scheduled_test_plans ORDER BY id`
	preserved[allPlans] = snapshot(allPlans)
	require.Zero(t, apply(migration), "idempotent rerun must not update any row")
	for query, before := range preserved {
		require.JSONEq(t, before, snapshot(query), "idempotent rerun: %s", query)
	}
	t.Log("Rerun updated zero rows; plan timestamps, accounts, scheduler work, channel monitors and intelligent tests unchanged")
}
