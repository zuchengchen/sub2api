//go:build unit

package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReliabilityMigrationsAreEmbedded(t *testing.T) {
	entries, err := FS.ReadDir(".")
	require.NoError(t, err)
	names := map[string]struct{}{}
	for _, entry := range entries {
		names[entry.Name()] = struct{}{}
	}
	for _, name := range []string{
		"243_api_key_concurrency.sql",
		"244_billing_attempt_outbox.sql",
		"249_scheduler_dirty_work.sql",
		"251_scheduler_ownership_epoch.sql",
		"252_scheduler_support_decision.sql",
		"248_drop_empty_promo_tables.sql",
	} {
		require.Contains(t, names, name)
		body, err := FS.ReadFile(name)
		require.NoError(t, err)
		require.NotEmpty(t, strings.TrimSpace(string(body)))
	}
}
