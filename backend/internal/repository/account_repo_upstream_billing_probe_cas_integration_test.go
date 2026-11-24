//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newUpstreamBillingGroupSyncFixture(t *testing.T, ctx context.Context) (*accountRepository, *service.Account) {
	t.Helper()
	client := testEntClient(t)
	created := mustCreateAccount(t, client, &service.Account{
		Name:        "local name",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "test-upstream-key"},
		Extra: map[string]any{
			service.UpstreamBillingProbeEnabledExtraKey:     true,
			service.UpstreamBillingRateSyncEnabledExtraKey:  true,
			service.UpstreamBillingGroupSyncEnabledExtraKey: true,
			"sample_account_option":                         "preserved",
		},
	})
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := integrationDB.ExecContext(cleanupCtx, "DELETE FROM scheduler_outbox WHERE account_id = $1", created.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(cleanupCtx, "DELETE FROM accounts WHERE id = $1", created.ID)
		require.NoError(t, err)
	})
	_, err := client.Account.UpdateOneID(created.ID).SetNotes("local notes").SetRateMultiplier(0.75).Save(ctx)
	require.NoError(t, err)
	repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	account, err := repo.GetByID(ctx, created.ID)
	require.NoError(t, err)
	return repo, account
}

func upstreamBillingGroupSyncEvents(t *testing.T, ctx context.Context, accountID int64) int {
	t.Helper()
	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		"SELECT count(*) FROM scheduler_outbox WHERE account_id = $1 AND event_type = $2",
		accountID, service.SchedulerOutboxEventAccountChanged).Scan(&count))
	return count
}

func TestSyncUpstreamBillingGroupAppliesOnlyChangedNonemptyText(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	repo, account := newUpstreamBillingGroupSyncFixture(t, ctx)

	group := service.BillingGroupInfo{Name: "upstream group"}
	saved, err := repo.SyncUpstreamBillingGroup(ctx, account, group)
	require.NoError(t, err)
	require.Equal(t, group.Name, saved.Name)
	require.NotNil(t, saved.Notes)
	require.Equal(t, "local notes", *saved.Notes)
	require.Equal(t, 0.75, saved.BillingRateMultiplier())
	require.Equal(t, account.Extra, saved.Extra)
	require.Equal(t, 1, upstreamBillingGroupSyncEvents(t, ctx, account.ID))

	// Remove the first event so a new no-op event could not be hidden by deduplication.
	_, err = integrationDB.ExecContext(ctx, "DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
	require.NoError(t, err)
	unchanged, err := repo.SyncUpstreamBillingGroup(ctx, saved, group)
	require.NoError(t, err)
	require.True(t, saved.UpdatedAt.Equal(unchanged.UpdatedAt))
	require.Zero(t, upstreamBillingGroupSyncEvents(t, ctx, account.ID))
}

func TestSyncUpstreamBillingGroupRejectsStaleEditAndConnection(t *testing.T) {
	for _, test := range []struct {
		name string
		edit string
	}{
		{
			name: "newer account edit",
			edit: "UPDATE accounts SET notes = 'new local notes', updated_at = updated_at + interval '1 second' WHERE id = $1",
		},
		{
			name: "changed connection with same version",
			edit: `UPDATE accounts SET credentials = '{"api_key":"new-test-key"}'::jsonb WHERE id = $1`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			repo, account := newUpstreamBillingGroupSyncFixture(t, ctx)
			_, err := integrationDB.ExecContext(ctx, test.edit, account.ID)
			require.NoError(t, err)
			current, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)

			result, err := repo.SyncUpstreamBillingGroup(ctx, account, service.BillingGroupInfo{
				Name: "upstream group", Description: "upstream notes",
			})
			require.ErrorIs(t, err, service.ErrUpstreamBillingProbeIdentityChanged)
			require.Nil(t, result)
			after, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			require.Equal(t, current.Name, after.Name)
			require.Equal(t, current.Notes, after.Notes)
			require.Equal(t, current.Credentials, after.Credentials)
			require.True(t, current.UpdatedAt.Equal(after.UpdatedAt))
			require.Zero(t, upstreamBillingGroupSyncEvents(t, ctx, account.ID))
		})
	}
}

func TestUpdateUpstreamBillingProbeSnapshotChecksDisplayFieldsIndependently(t *testing.T) {
	for _, test := range []struct {
		name      string
		edit      string
		wantName  string
		wantNotes string
	}{
		{
			name:     "edit during probe keeps changed name",
			edit:     "UPDATE accounts SET name = 'new local name' WHERE id = $1",
			wantName: "new local name", wantNotes: "upstream notes",
		},
		{
			name:     "edit during probe keeps changed notes",
			edit:     "UPDATE accounts SET notes = NULL WHERE id = $1",
			wantName: "upstream group",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			repo, account := newUpstreamBillingGroupSyncFixture(t, ctx)
			_, err := integrationDB.ExecContext(ctx, test.edit, account.ID)
			require.NoError(t, err)

			rate := 0.25
			require.NoError(t, repo.UpdateUpstreamBillingProbeSnapshot(ctx, account,
				&service.UpstreamBillingProbeSnapshot{Status: service.UpstreamBillingProbeStatusOK}, &rate,
				service.BillingGroupInfo{Name: "upstream group", Description: "upstream notes"}))
			saved, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			require.Equal(t, test.wantName, saved.Name)
			if test.wantNotes == "" {
				require.Nil(t, saved.Notes)
			} else {
				require.NotNil(t, saved.Notes)
				require.Equal(t, test.wantNotes, *saved.Notes)
			}
			require.Equal(t, "preserved", saved.Extra["sample_account_option"])
			require.NotNil(t, saved.Extra[service.UpstreamBillingProbeExtraKey])
			require.Equal(t, rate, saved.BillingRateMultiplier())
			require.Equal(t, 1, upstreamBillingGroupSyncEvents(t, ctx, account.ID))
		})
	}
}
