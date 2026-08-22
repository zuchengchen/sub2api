package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContentModerationActionIndexMigration(t *testing.T) {
	content, err := FS.ReadFile("229_content_moderation_action_index_notx.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_content_moderation_logs_action_created_at")
	require.Contains(t, sql, "ON content_moderation_logs(action, created_at DESC)")
}
