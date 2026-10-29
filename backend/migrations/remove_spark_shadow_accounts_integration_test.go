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

func TestMigration271RetiresSparkWithoutLosingHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// An explicit test-cluster maintenance URL allows local PostgreSQL without
	// Docker. Always create and drop our own database; never migrate the URL's DB.
	dsn := os.Getenv("MIGRATION_271_TEST_ADMIN_DSN")
	if dsn == "" {
		if exec.CommandContext(ctx, "docker", "info").Run() != nil {
			t.Fatal("Docker or MIGRATION_271_TEST_ADMIN_DSN for an isolated PostgreSQL test cluster is required")
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
	databaseName := fmt.Sprintf("sub2api_migration271_%d", time.Now().UnixNano())
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

	apply := func(t *testing.T, name string) {
		t.Helper()
		body, err := FS.ReadFile(name)
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx, string(body))
		require.NoError(t, err, name)
		require.NoError(t, tx.Commit(), name)
	}
	// Exercise the real foreign keys, parent constraints and scheduler triggers.
	for _, name := range []string{
		"001_init.sql", "003_subscription.sql", "005_schema_parity.sql",
		"020_add_temp_unschedulable.sql", "027_usage_billing_consistency.sql",
		"030_add_account_expires_at.sql", "036_scheduler_outbox.sql",
		"037_add_account_rate_multiplier.sql", "066_add_scheduled_test_tables.sql",
		"067_add_account_load_factor.sql", "070_add_scheduled_test_auto_recover.sql",
		"071_add_usage_billing_dedup.sql", "152_scheduler_outbox_dedup_key.sql",
		"154_account_spark_shadow.sql", "194_channel_monitor_v2.sql",
		"197_channel_monitor_v2_seed_popular_models.sql", "244_billing_attempt_outbox.sql",
		"249_scheduler_dirty_work.sql",
	} {
		apply(t, name)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO accounts (id, name, platform, type, credentials, extra, updated_at, deleted_at)
		VALUES
		(1, 'parent', 'openai', 'oauth', '{"access_token":"parent-test-token","model_mapping":{"gpt-5.3-codex-spark":"custom"}}', '{"custom":true}', '2026-01-01', NULL),
		(2, 'ordinary', 'anthropic', 'apikey', '{"api_key":"ordinary-test-key"}', '{}', '2026-01-01', NULL),
		(3, 'already-deleted-ordinary', 'openai', 'oauth', '{}', '{}', '2026-01-01', '2026-01-01');
		INSERT INTO accounts (id, name, platform, type, credentials, parent_account_id, quota_dimension, updated_at, deleted_at)
		VALUES
		(10, 'linked-spark', 'openai', 'oauth', '{"historical":"keep"}', 1, 'spark', '2026-01-01', NULL),
		(11, 'already-deleted-spark', 'openai', 'oauth', '{}', 1, 'spark', '2026-01-01', '2026-01-01'),
		(12, 'ungrouped-spark', 'openai', 'oauth', '{}', 3, 'spark', '2026-01-01', NULL);
		INSERT INTO groups (id, name) VALUES (1, 'shared'), (2, 'spark-only');
		INSERT INTO account_groups (account_id, group_id) VALUES (1, 1), (2, 1), (10, 1), (10, 2), (11, 2);
		INSERT INTO scheduled_test_plans (id, account_id, model_id, auto_recover, next_run_at, last_run_at, updated_at)
		VALUES
		(1, 1, 'gpt-5', true, '2026-12-01', '2026-01-01', '2026-01-01'),
		(2, 10, 'gpt-5.3-codex-spark', true, '2026-12-01', '2026-01-01', '2026-01-01'),
		(3, 11, 'custom-model', true, '2026-12-01', '2026-01-01', '2026-01-01'),
		(4, 1, 'gpt-5.3-codex-spark', true, '2026-12-01', '2026-01-01', '2026-01-01'),
		(5, 2, 'gpt-5.3-codex-spark-custom', true, '2026-12-01', '2026-01-01', '2026-01-01');
		INSERT INTO scheduled_test_results (plan_id, response_text) VALUES (2, 'historical Spark response'), (4, 'historical parent response');
		INSERT INTO users (id, email, password_hash, balance) VALUES (1, 'migration271@example.test', 'test-only', 123.45);
		INSERT INTO api_keys (id, user_id, key, name, group_id) VALUES (1, 1, 'test-only', 'history', 2);
		INSERT INTO usage_logs (id, user_id, api_key_id, account_id, group_id, model, actual_cost)
		VALUES (1, 1, 1, 10, 2, 'gpt-5.3-codex-spark', 1.25), (2, 1, 1, 1, 1, 'gpt-5', 2.50);
		INSERT INTO billing_usage_entries (usage_log_id, user_id, api_key_id, billing_type, delta_usd) VALUES (1, 1, 1, 0, -1.25);
		INSERT INTO usage_billing_dedup (request_id, api_key_id, request_fingerprint) VALUES ('historical-spark', 1, 'historical-fingerprint');
		INSERT INTO billing_attempt_outbox (attempt_id, api_key_id, request_fingerprint, command)
		VALUES ('historical-spark', 1, 'historical-fingerprint', '{"account_id":10,"model":"gpt-5.3-codex-spark","cost":1.25}');
		INSERT INTO channel_monitor_v2_metrics_1m (bucket_start, platform, group_id, model, success_requests)
		VALUES ('2026-01-01', 'openai', 2, 'gpt-5.3-codex-spark', 7);
		TRUNCATE scheduler_dirty_account_sources, scheduler_dirty_group_sources, scheduler_dirty_membership_sources;
	`)
	require.NoError(t, err)
	snapshot := func(t *testing.T, query string) string {
		t.Helper()
		var result string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(rows)), '[]'::jsonb)::text FROM (`+query+`) AS rows`).Scan(&result))
		return result
	}
	preserved := map[string]string{}
	for _, query := range []string{
		`SELECT * FROM accounts WHERE id < 10 ORDER BY id`,
		`SELECT id, credentials, extra FROM accounts ORDER BY id`,
		`SELECT * FROM groups ORDER BY id`,
		`SELECT * FROM users ORDER BY id`,
		`SELECT * FROM api_keys ORDER BY id`,
		`SELECT * FROM usage_logs ORDER BY id`,
		`SELECT * FROM billing_usage_entries ORDER BY id`,
		`SELECT * FROM usage_billing_dedup ORDER BY id`,
		`SELECT * FROM billing_attempt_outbox ORDER BY id`,
		`SELECT * FROM channel_monitor_v2_metrics_1m ORDER BY bucket_start`,
		`SELECT * FROM scheduled_test_results ORDER BY id`,
		`SELECT * FROM scheduled_test_plans WHERE id IN (1, 5) ORDER BY id`,
		`SELECT id, account_id, model_id, cron_expression, max_results, last_run_at, created_at FROM scheduled_test_plans ORDER BY id`,
		`SELECT * FROM account_groups WHERE account_id < 10 ORDER BY account_id, group_id`,
	} {
		preserved[query] = snapshot(t, query)
	}
	var seededPlatforms, expectedPlatforms string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT platforms::text FROM channel_monitor_v2_config`).Scan(&seededPlatforms))
	require.Contains(t, seededPlatforms, `gpt-5.3-codex-spark`)
	// Calculate the expected factory list independently of the migration's filter.
	require.NoError(t, db.QueryRowContext(ctx, `SELECT jsonb_set(platforms, '{1,models}', (platforms #> '{1,models}') - 'gpt-5.3-codex-spark')::text FROM channel_monitor_v2_config`).Scan(&expectedPlatforms))
	const migration = "271_remove_spark_shadow_accounts.sql"
	apply(t, migration)
	for query, before := range preserved {
		require.JSONEq(t, before, snapshot(t, query), query)
	}
	require.JSONEq(t, `[{"id":10,"deleted":true,"schedulable":false,"parent_account_id":null,"quota_dimension":"global"},{"id":11,"deleted":true,"schedulable":false,"parent_account_id":null,"quota_dimension":"global"},{"id":12,"deleted":true,"schedulable":false,"parent_account_id":null,"quota_dimension":"global"}]`,
		snapshot(t, `SELECT id, deleted_at IS NOT NULL AS deleted, schedulable, parent_account_id, quota_dimension FROM accounts WHERE id >= 10 ORDER BY id`))
	var deletionPreserved bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT deleted_at = '2026-01-01'::timestamptz FROM accounts WHERE id = 11`).Scan(&deletionPreserved))
	require.True(t, deletionPreserved)
	require.JSONEq(t, `[]`, snapshot(t, `SELECT * FROM account_groups WHERE account_id >= 10`))
	require.JSONEq(t, `[{"id":2,"enabled":false,"auto_recover":false,"next_run_at":null},{"id":3,"enabled":false,"auto_recover":false,"next_run_at":null},{"id":4,"enabled":false,"auto_recover":false,"next_run_at":null}]`,
		snapshot(t, `SELECT id, enabled, auto_recover, next_run_at FROM scheduled_test_plans WHERE id IN (2, 3, 4) ORDER BY id`))
	require.JSONEq(t, `[{"event_type":"account_changed","account_id":10,"group_id":null,"payload":{"group_ids":[1,2]}},{"event_type":"account_changed","account_id":11,"group_id":null,"payload":{"group_ids":[2]}},{"event_type":"account_changed","account_id":12,"group_id":null,"payload":{"group_ids":[]}}]`,
		snapshot(t, `SELECT event_type, account_id, group_id, payload FROM scheduler_outbox ORDER BY account_id`))
	require.JSONEq(t, `[{"account_id":10,"bucket_dirty":true},{"account_id":11,"bucket_dirty":true},{"account_id":12,"bucket_dirty":true}]`,
		snapshot(t, `SELECT account_id, bucket_dirty FROM scheduler_dirty_account_sources ORDER BY account_id`))
	require.JSONEq(t, `[{"account_id":10,"group_id":1},{"account_id":10,"group_id":2},{"account_id":11,"group_id":2}]`,
		snapshot(t, `SELECT account_id, group_id FROM scheduler_dirty_membership_sources ORDER BY account_id, group_id`))
	var actualPlatforms string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT platforms::text FROM channel_monitor_v2_config`).Scan(&actualPlatforms))
	require.JSONEq(t, expectedPlatforms, actualPlatforms)

	// Rerun in a new transaction: timestamps, outbox and dirty generations must
	// remain unchanged, not merely converge to the same visible account state.
	unchanged := map[string]string{}
	for _, table := range []string{"accounts", "account_groups", "groups", "scheduled_test_plans", "scheduled_test_results", "scheduler_outbox", "scheduler_dirty_account_sources", "scheduler_dirty_group_sources", "scheduler_dirty_membership_sources", "channel_monitor_v2_config", "usage_logs", "billing_usage_entries", "usage_billing_dedup", "billing_attempt_outbox"} {
		query := `SELECT * FROM ` + table + ` ORDER BY 1, 2`
		unchanged[query] = snapshot(t, query)
	}
	apply(t, migration)
	for query, before := range unchanged {
		require.JSONEq(t, before, snapshot(t, query), "idempotent rerun: %s", query)
	}

	for _, tc := range []struct {
		name    string
		before  string
		after   string
		changed bool
	}{
		{
			name:    "ordered custom platforms and duplicates",
			before:  `[{"platform":"custom","enabled":false,"models":["keep-first","gpt-5.3-codex-spark","keep-first","gpt-5.3-codex-spark","gpt-5.3-codex-spark-custom","keep-last"],"custom":{"keep":[3,1]}},{"platform":"anthropic","enabled":true,"models":[]},{"platform":"openai","enabled":true,"models":["gpt-5","gpt-5.3-codex-spark"],"label":"custom order"}]`,
			after:   `[{"platform":"custom","enabled":false,"models":["keep-first","keep-first","gpt-5.3-codex-spark-custom","keep-last"],"custom":{"keep":[3,1]}},{"platform":"anthropic","enabled":true,"models":[]},{"platform":"openai","enabled":true,"models":["gpt-5"],"label":"custom order"}]`,
			changed: true,
		},
		{
			name:    "only Spark stays collapsed",
			before:  `[{"platform":"openai","enabled":true,"models":["gpt-5.3-codex-spark","gpt-5.3-codex-spark"],"custom":7}]`,
			after:   `[{"platform":"openai","enabled":true,"models":["__other__"],"custom":7}]`,
			changed: true,
		},
		{name: "empty platforms", before: `[]`, after: `[]`},
		{
			name:   "empty missing and non-array lists remain intact",
			before: `[{"platform":"openai","models":[]},{"platform":"grok"},{"platform":"custom","models":null},{"platform":"legacy","models":"gpt-5.3-codex-spark"},null]`,
			after:  `[{"platform":"openai","models":[]},{"platform":"grok"},{"platform":"custom","models":null},{"platform":"legacy","models":"gpt-5.3-codex-spark"},null]`,
		},
		{name: "non-array platforms", before: `{"legacy":true}`, after: `{"legacy":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, `UPDATE channel_monitor_v2_config SET platforms = $1::jsonb, version = 20, enabled = false, group_ids = '{2,1}', updated_by = 99, updated_at = '2026-01-01'`, tc.before)
			require.NoError(t, err)
			apply(t, migration)
			var platforms string
			var version int
			var configPreserved, timestampPreserved bool
			require.NoError(t, db.QueryRowContext(ctx, `SELECT platforms::text, version, NOT enabled AND group_ids = '{2,1}'::bigint[] AND updated_by = 99, updated_at = '2026-01-01'::timestamptz FROM channel_monitor_v2_config`).Scan(&platforms, &version, &configPreserved, &timestampPreserved))
			require.JSONEq(t, tc.after, platforms)
			require.True(t, configPreserved)
			if tc.changed {
				require.Equal(t, 21, version)
				require.False(t, timestampPreserved)
			} else {
				require.Equal(t, 20, version)
				require.True(t, timestampPreserved)
			}
			beforeRerun := snapshot(t, `SELECT * FROM channel_monitor_v2_config`)
			apply(t, migration)
			require.JSONEq(t, beforeRerun, snapshot(t, `SELECT * FROM channel_monitor_v2_config`))
		})
	}
}
