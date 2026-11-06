//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAutoResetAccountEditPreservesLatestAttemptUnderRowLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := testEntClient(t)
	cache := NewSchedulerCache(testRedis(t))
	repo := newAccountRepositoryWithSQL(client, integrationDB, cache)
	account := mustCreateAccount(t, client, &service.Account{
		Name: t.Name(), Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "fixture-only"},
		Extra: map[string]any{
			service.OpenAIAutoResetCreditEnabledExtraKey:      true,
			service.OpenAIAutoResetCreditOptInVersionExtraKey: 1,
			service.OpenAIAutoResetCreditStateExtraKey:        map[string]any{"status": "available", "available_count": 2},
		},
	})
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id=$1", account.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id=$1", account.ID)
		require.NoError(t, err)
	})
	staleEdit, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	staleEdit.Name = "edited-with-stale-runtime"
	staleEdit.Extra[service.OpenAIAutoResetCreditEnabledExtraKey] = false

	// Simulate an automatic reset publishing a new reservation while an admin
	// request is still holding the old response snapshot.
	latest := map[string]any{"status": "resetting", "attempt_credit_hash": "latest-card", "attempt_cycle_hash": "latest-cycle"}
	update, err := json.Marshal(map[string]any{service.OpenAIAutoResetCreditStateExtraKey: latest})
	require.NoError(t, err)
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, "UPDATE accounts SET extra=extra || $1::jsonb WHERE id=$2", string(update), account.ID)
	require.NoError(t, err)
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(entered)
		done <- repo.UpdateWithAccountBillingSettings(ctx, staleEdit, nil, nil, nil)
	}()
	<-entered
	select {
	case err := <-done:
		t.Fatalf("account edit unexpectedly completed while worker held row lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, tx.Commit())
	require.NoError(t, <-done)
	stored, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, latest, stored.Extra[service.OpenAIAutoResetCreditStateExtraKey])
	require.False(t, service.ResolveOpenAIAutoResetCreditConfig(stored).Enabled)
	cached, err := cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.NotNil(t, cached)
	require.Equal(t, latest, cached.Extra[service.OpenAIAutoResetCreditStateExtraKey])
	require.False(t, service.ResolveOpenAIAutoResetCreditConfig(cached).Enabled)
}

func TestAutoResetConcurrentExtraUpdatesReachSchedulerCache(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := testEntClient(t)
	cache := NewSchedulerCache(testRedis(t))
	repo := newAccountRepositoryWithSQL(client, integrationDB, cache)
	account := mustCreateAccount(t, client, &service.Account{
		Name: t.Name(), Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "fixture-only"},
		Extra: map[string]any{
			service.OpenAIAutoResetCreditEnabledExtraKey:      true,
			service.OpenAIAutoResetCreditOptInVersionExtraKey: 1,
		},
	})
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id=$1", account.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id=$1", account.ID)
		require.NoError(t, err)
	})
	state := map[string]any{"status": "resetting", "attempt_credit_hash": "reserved-card"}
	updates := []map[string]any{
		{service.OpenAIAutoResetCreditStateExtraKey: state},
		{"codex_5h_used_percent": 98.0},
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errors := make(chan error, len(updates))
	for _, update := range updates {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; errors <- repo.UpdateExtra(ctx, account.ID, update) }()
	}
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	stored, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	cached, err := cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.NotNil(t, cached)
	for _, actual := range []*service.Account{stored, cached} {
		require.Equal(t, state, actual.Extra[service.OpenAIAutoResetCreditStateExtraKey])
		require.Equal(t, 98.0, actual.Extra["codex_5h_used_percent"])
		require.True(t, service.ResolveOpenAIAutoResetCreditConfig(actual).Enabled)
	}
}
