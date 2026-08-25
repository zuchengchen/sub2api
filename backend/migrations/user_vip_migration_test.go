package migrations

import (
	"fmt"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestMigration231AddUserVip(t *testing.T) {
	content, err := FS.ReadFile("231_add_user_vip.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "ALTER TABLE users ADD COLUMN IF NOT EXISTS vip boolean NOT NULL DEFAULT false")
	require.Contains(t, sql, fmt.Sprintf("vip = false AND balance > %v", service.VipBalanceThreshold),
		"partial upgrade-sweep index must match the lazy-upgrade predicate")
	require.Contains(t, sql, "deleted_at IS NULL")
}
