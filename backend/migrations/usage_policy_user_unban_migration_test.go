package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration240UsagePolicyUserUnban(t *testing.T) {
	content, err := FS.ReadFile("240_usage_policy_user_unban.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "usage_policy_unban_at")
	require.Contains(t, sql, "idx_users_usage_policy_unban_at")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS")
}
