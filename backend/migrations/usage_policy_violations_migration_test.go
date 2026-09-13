package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration238UsagePolicyViolations(t *testing.T) {
	content, err := FS.ReadFile("238_usage_policy_violations.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS usage_policy_violations")
	require.Contains(t, sql, "idx_usage_policy_violations_request_id")
	require.Contains(t, sql, "flagged as potentially violating our usage policy")
	require.Contains(t, sql, "ON CONFLICT (request_id) WHERE request_id <> '' DO NOTHING")
	require.Contains(t, sql, "skip_reason")
	require.NotContains(t, sql, "auto_banned = TRUE")
}
