//go:build integration

package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// route_degraded is tri-state: NULL = Tibo route selection did not apply,
// false = healthy/unknown route, true = plain HTTP fallback (all routes degraded).
func TestUsageLog_RouteDegradedRoundTripFilterAndPartialIndex(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newUsageLogRepositoryWithSQL(client, tx)

	user := mustCreateUser(t, client, &service.User{Email: "route-degraded@test.com"})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-route-degraded", Name: "route-degraded"})
	account := mustCreateAccount(t, client, &service.Account{Name: "route-degraded-account"})
	now := time.Now().UTC()

	healthy, degraded := false, true
	idsByCase := make(map[string]int64, 3)
	for _, tc := range []struct {
		name  string
		value *bool
	}{
		{name: "null", value: nil},
		{name: "false", value: &healthy},
		{name: "true", value: &degraded},
	} {
		log := &service.UsageLog{
			UserID: user.ID, APIKeyID: apiKey.ID, AccountID: account.ID,
			RequestID: "req-route-degraded-" + tc.name,
			Model:     "gpt-5.4", InputTokens: 1, OutputTokens: 1,
			RouteDegraded: tc.value,
			CreatedAt:     now,
		}
		inserted, err := repo.Create(ctx, log)
		require.NoError(t, err)
		require.True(t, inserted)
		idsByCase[tc.name] = log.ID

		got, err := repo.GetByID(ctx, log.ID)
		require.NoError(t, err)
		require.Equal(t, tc.value, got.RouteDegraded, "round trip for %s", tc.name)
	}

	listIDs := func(filter *bool) []int64 {
		t.Helper()
		logs, _, err := repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 20},
			usagestats.UsageLogFilters{UserID: user.ID, RouteDegraded: filter})
		require.NoError(t, err)
		ids := make([]int64, 0, len(logs))
		for _, log := range logs {
			ids = append(ids, log.ID)
		}
		return ids
	}
	require.Equal(t, []int64{idsByCase["true"]}, listIDs(&degraded))
	require.Equal(t, []int64{idsByCase["false"]}, listIDs(&healthy))
	require.ElementsMatch(t, []int64{idsByCase["null"], idsByCase["false"], idsByCase["true"]}, listIDs(nil))

	_, err := tx.ExecContext(ctx, "SET LOCAL enable_seqscan = off")
	require.NoError(t, err)
	rows, err := tx.QueryContext(ctx, `
EXPLAIN (COSTS OFF)
SELECT id
FROM usage_logs
WHERE route_degraded IS TRUE
ORDER BY created_at DESC, id DESC
LIMIT 100
`)
	require.NoError(t, err)
	var planLines []string
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		planLines = append(planLines, line)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Contains(t, strings.Join(planLines, "\n"), usageLogsRouteDegradedIndex)
}
