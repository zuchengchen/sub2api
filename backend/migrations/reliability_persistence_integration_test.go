//go:build integration

package migrations

import (
	"context"
	"database/sql"
	"os/exec"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestReliabilityMigrationsPersistOutboxAndDirtyWork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if exec.CommandContext(ctx, "docker", "info").Run() != nil {
		t.Fatal("docker is required to apply reliability migrations against PostgreSQL")
	}

	container, err := tcpostgres.Run(ctx, "postgres:18.1-alpine3.23",
		tcpostgres.WithDatabase("sub2api_test"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable", "TimeZone=UTC")
	require.NoError(t, err)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.PingContext(ctx))

	for _, name := range []string{
		"244_billing_attempt_outbox.sql",
		"249_scheduler_dirty_work.sql",
		"251_scheduler_ownership_epoch.sql",
	} {
		body, err := FS.ReadFile(name)
		require.NoError(t, err, name)
		_, err = db.ExecContext(ctx, string(body))
		require.NoError(t, err, name)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO billing_attempt_outbox (attempt_id, api_key_id, request_fingerprint, command)
		VALUES ('attempt-1', 9, 'fp-1', '{"attempt_id":"attempt-1"}'::jsonb)`)
	require.NoError(t, err)
	var status string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT status FROM billing_attempt_outbox WHERE attempt_id = 'attempt-1'`).Scan(&status))
	require.Equal(t, "pending", status)

	_, err = db.ExecContext(ctx, `
		INSERT INTO scheduler_dirty_work (kind, entity_id, generation)
		VALUES (1, 42, 7)`)
	require.NoError(t, err)
	var generation int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT generation FROM scheduler_dirty_work WHERE kind = 1 AND entity_id = 42`).Scan(&generation))
	require.Equal(t, int64(7), generation)

	_, err = db.ExecContext(ctx, `INSERT INTO scheduler_ownership_epoch (singleton, epoch) VALUES (TRUE, 1)`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		CREATE TABLE promo_codes (id BIGSERIAL PRIMARY KEY, code TEXT NOT NULL);
		CREATE TABLE promo_code_usages (id BIGSERIAL PRIMARY KEY);
		INSERT INTO promo_codes (code) VALUES ('KEEP-ME')`)
	require.NoError(t, err)
	promoSQL, err := FS.ReadFile("248_drop_empty_promo_tables.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(promoSQL))
	require.Error(t, err)
	require.Contains(t, err.Error(), "promo_codes has business rows")
}
