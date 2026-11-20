//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestContentModerationRevokesOnlyActiveKeysOfPermanentlyDisabledUser(t *testing.T) {
	ctx := context.Background()
	repo := NewContentModerationRepository(integrationDB).(service.ContentModerationBulkDispositionRepository)
	usage := NewUsagePolicyRepository(integrationDB)
	user := mustCreateUser(t, integrationEntClient, &service.User{})
	other := mustCreateUser(t, integrationEntClient, &service.User{})
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id IN ($1, $2)", user.ID, other.ID)
	})
	active := mustCreateApiKey(t, integrationEntClient, &service.APIKey{UserID: user.ID})
	mustCreateApiKey(t, integrationEntClient, &service.APIKey{UserID: user.ID, Status: "inactive"})
	deleted := mustCreateApiKey(t, integrationEntClient, &service.APIKey{UserID: user.ID})
	require.NoError(t, integrationEntClient.APIKey.DeleteOneID(deleted.ID).Exec(ctx))
	mustCreateApiKey(t, integrationEntClient, &service.APIKey{UserID: other.ID})

	transitioned, err := usage.DisableUserForUsagePolicy(ctx, user.ID, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, transitioned)
	keys, err := repo.DisableActiveAPIKeysForDisabledUser(ctx, user.ID)
	require.NoError(t, err)
	require.Empty(t, keys, "a delayed revocation must also skip a later, temporary ban")

	transitioned, err = repo.DisableUserIfActive(ctx, user.ID)
	require.NoError(t, err)
	require.False(t, transitioned, "replacing the cooldown must not count a second active-to-disabled transition")
	ids, err := usage.UnbanDueUsers(ctx, 100)
	require.NoError(t, err)
	require.NotContains(t, ids, user.ID)
	var status string
	var scheduled bool
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		"SELECT status, usage_policy_unban_at IS NOT NULL FROM users WHERE id = $1", user.ID).Scan(&status, &scheduled))
	require.Equal(t, service.StatusDisabled, status)
	require.False(t, scheduled)

	keys, err = repo.DisableActiveAPIKeysForDisabledUser(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, []string{active.Key}, keys)
	keys, err = repo.DisableActiveAPIKeysForDisabledUser(ctx, user.ID)
	require.NoError(t, err)
	require.Empty(t, keys)

	transitioned, err = repo.DisableUserIfActive(ctx, other.ID)
	require.NoError(t, err)
	require.True(t, transitioned, "a first permanent ban on an active user must still count")
}

func TestContentModerationKeyRevocationWaitsForConcurrentUnban(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	user := mustCreateUser(t, integrationEntClient, &service.User{Status: service.StatusDisabled})
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM users WHERE id = $1", user.ID)
	})
	key := mustCreateApiKey(t, integrationEntClient, &service.APIKey{UserID: user.ID})
	repo := NewContentModerationRepository(integrationDB).(service.ContentModerationBulkDispositionRepository)

	unbanTx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = unbanTx.Rollback() }()
	var unbanPID int
	require.NoError(t, unbanTx.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&unbanPID))
	_, err = unbanTx.ExecContext(ctx, "UPDATE users SET status = 'active' WHERE id = $1", user.ID)
	require.NoError(t, err)

	type result struct {
		keys []string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		keys, err := repo.DisableActiveAPIKeysForDisabledUser(ctx, user.ID)
		done <- result{keys: keys, err: err}
	}()
	require.Eventually(t, func() bool {
		var blocked bool
		err := integrationDB.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))
		)`, unbanPID).Scan(&blocked)
		return err == nil && blocked
	}, 5*time.Second, 10*time.Millisecond, "revocation must wait for the user row")
	require.NoError(t, unbanTx.Commit())

	out := <-done
	require.NoError(t, out.err)
	require.Empty(t, out.keys)
	current, err := integrationEntClient.APIKey.Get(ctx, key.ID)
	require.NoError(t, err)
	require.Equal(t, service.StatusActive, current.Status)
}
