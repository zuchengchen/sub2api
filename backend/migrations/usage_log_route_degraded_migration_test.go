package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUsageLogRouteDegradedMigrations(t *testing.T) {
	column, err := FS.ReadFile("261_add_usage_log_route_degraded.sql")
	require.NoError(t, err)
	columnSQL := strings.Join(strings.Fields(string(column)), " ")
	require.Contains(t, columnSQL, "ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS route_degraded BOOLEAN;")
	// Tri-state column: no NOT NULL and no DEFAULT, so historical rows stay NULL
	// ("Tibo route selection did not apply").
	require.NotContains(t, strings.ToUpper(columnSQL), "NOT NULL")
	require.NotContains(t, strings.ToUpper(columnSQL), "DEFAULT")
	require.NotContains(t, strings.ToUpper(columnSQL), "CONCURRENTLY")

	index, err := FS.ReadFile("262_add_usage_log_route_degraded_index_notx.sql")
	require.NoError(t, err)
	indexSQL := strings.Join(strings.Fields(string(index)), " ")
	require.Contains(t, indexSQL, "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_logs_route_degraded_created_at")
	require.Contains(t, indexSQL, "ON usage_logs (created_at DESC, id DESC)")
	require.Contains(t, indexSQL, "WHERE route_degraded IS TRUE")
}
