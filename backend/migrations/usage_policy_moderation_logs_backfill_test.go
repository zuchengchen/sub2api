package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration239BackfillUsagePolicyModerationLogs(t *testing.T) {
	content, err := FS.ReadFile("239_backfill_usage_policy_moderation_logs.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "INSERT INTO content_moderation_logs")
	require.Contains(t, sql, "'usage_policy'")
	require.Contains(t, sql, "FROM usage_policy_violations")
	require.Contains(t, sql, "archive_status")
	require.Contains(t, sql, "'none'")
}
