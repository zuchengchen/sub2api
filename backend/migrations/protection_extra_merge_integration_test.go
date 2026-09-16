//go:build integration

package migrations

import (
	"context"
	"database/sql"
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestOpenAILegacyProtectionBackfillPreserves429Keys(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if exec.CommandContext(ctx, "docker", "info").Run() != nil {
		t.Fatal("docker is required to apply protection backfill against PostgreSQL")
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

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS accounts (
			id bigserial PRIMARY KEY,
			name text, platform text, type text, credentials jsonb, extra jsonb,
			concurrency integer, status text, parent_account_id bigint, deleted_at timestamptz
		);`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO accounts (name, platform, type, extra, concurrency, status)
		VALUES
		('openai-main', 'openai', 'oauth', '{"auto_pause_5h_threshold":0.95,"auto_pause_7d_disabled":true,"keep":"yes"}'::jsonb, 8, 'active'),
		('grok-acc', 'grok', 'oauth', '{"auto_pause_5h_threshold":0.8}'::jsonb, 4, 'active');
		INSERT INTO accounts (name, platform, type, extra, concurrency, status, parent_account_id)
		VALUES ('openai-shadow', 'openai', 'oauth', '{"auto_pause_5h_threshold":0.7}'::jsonb, 2, 'active', 1);
	`)
	require.NoError(t, err)

	body, err := FS.ReadFile("253_openai_legacy_protection_backfill.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(body))
	require.NoError(t, err)

	assertExtra := func(name string, wantProtection bool, wantPause float64) map[string]any {
		t.Helper()
		var raw []byte
		require.NoError(t, db.QueryRowContext(ctx, `SELECT extra FROM accounts WHERE name = $1`, name).Scan(&raw))
		var extra map[string]any
		require.NoError(t, json.Unmarshal(raw, &extra))
		require.Equal(t, wantPause, extra["auto_pause_5h_threshold"])
		if wantProtection {
			require.Equal(t, true, extra["anti_degradation"])
			require.Equal(t, "legacy", extra["protection_scope"])
		} else {
			require.NotEqual(t, true, extra["anti_degradation"])
			require.NotEqual(t, "legacy", extra["protection_scope"])
		}
		return extra
	}

	first := assertExtra("openai-main", true, 0.95)
	require.Equal(t, true, first["auto_pause_7d_disabled"])
	require.Equal(t, "yes", first["keep"])
	assertExtra("grok-acc", false, 0.8)
	assertExtra("openai-shadow", false, 0.7)

	_, err = db.ExecContext(ctx, string(body))
	require.NoError(t, err)
	second := assertExtra("openai-main", true, 0.95)
	require.Equal(t, first["anti_degrade"], second["anti_degrade"])
}
