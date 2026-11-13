//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type autoResetToggleRepo struct {
	AccountRepository
	account     *Account
	getErr      error
	updateErr   error
	updates     map[string]any
	beforePatch func()
}

func (r *autoResetToggleRepo) GetByID(context.Context, int64) (*Account, error) {
	if r.getErr != nil || r.account == nil {
		return nil, r.getErr
	}
	copy := *r.account
	copy.Extra = cloneOpenAIAutoResetExtra(r.account.Extra)
	return &copy, nil
}

func (r *autoResetToggleRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.updates = updates
	if r.updateErr != nil {
		return r.updateErr
	}
	if r.beforePatch != nil {
		r.beforePatch()
	}
	if r.account.Extra == nil {
		r.account.Extra = map[string]any{}
	}
	for key, value := range updates {
		r.account.Extra[key] = value
	}
	return nil
}

func TestSetOpenAIAutoResetCreditEnabledDefaultsRequireExplicitOptIn(t *testing.T) {
	for _, extra := range []map[string]any{nil, {OpenAIAutoResetCreditEnabledExtraKey: true}} {
		repo := &autoResetToggleRepo{account: &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: extra}}
		svc := &adminServiceImpl{accountRepo: repo}
		require.False(t, ResolveOpenAIAutoResetCreditConfig(repo.account).Enabled)
		updated, err := svc.SetOpenAIAutoResetCreditEnabled(context.Background(), 42, true)
		require.NoError(t, err)
		require.Equal(t, OpenAIAutoResetCreditConfig{Enabled: true, Threshold5h: 1, Threshold7d: 1}, ResolveOpenAIAutoResetCreditConfig(updated))
		require.Equal(t, map[string]any{
			OpenAIAutoResetCreditEnabledExtraKey:      true,
			OpenAIAutoResetCreditOptInVersionExtraKey: openAIAutoResetCreditOptInVersion,
		}, repo.updates)

		updated, err = svc.SetOpenAIAutoResetCreditEnabled(context.Background(), 42, false)
		require.NoError(t, err)
		require.False(t, ResolveOpenAIAutoResetCreditConfig(updated).Enabled)
		require.Equal(t, false, updated.Extra[OpenAIAutoResetCreditEnabledExtraKey])
		require.Nil(t, updated.Extra[OpenAIAutoResetCreditOptInVersionExtraKey])
	}
}

func TestSetOpenAIAutoResetCreditEnabledPreservesConcurrentThresholdsAndAttemptState(t *testing.T) {
	now := time.Now().UTC()
	rateLimitUntil := now.Add(time.Hour)
	state := &OpenAIAutoResetCreditState{Status: OpenAIAutoResetStatusResetting, CheckedAt: now.Format(time.RFC3339), AttemptCreditHash: "reserved-credit", AttemptCycleHash: "reserved-cycle"}
	repo := &autoResetToggleRepo{account: &Account{
		ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Schedulable: false, RateLimitResetAt: &rateLimitUntil,
		Extra: map[string]any{
			OpenAIAutoResetCredit5hThresholdExtraKey: 0.75,
			OpenAIAutoResetCredit7dThresholdExtraKey: "0.9",
			OpenAIAutoResetCreditStateExtraKey:       state,
			"codex_credits_snapshot":                 map[string]any{"balance": 10},
		},
	}}
	// Simulate a concurrent threshold/runtime write after the initial read.
	repo.beforePatch = func() {
		repo.account.Extra[OpenAIAutoResetCredit5hThresholdExtraKey] = 0.82
		repo.account.Extra["codex_usage_updated_at"] = "newer-snapshot"
	}
	svc := &adminServiceImpl{accountRepo: repo}
	updated, err := svc.SetOpenAIAutoResetCreditEnabled(context.Background(), 42, true)
	require.NoError(t, err)
	require.Equal(t, OpenAIAutoResetCreditConfig{Enabled: true, Threshold5h: 0.82, Threshold7d: 0.9}, ResolveOpenAIAutoResetCreditConfig(updated))
	require.Equal(t, state, updated.Extra[OpenAIAutoResetCreditStateExtraKey])
	require.Equal(t, "newer-snapshot", updated.Extra["codex_usage_updated_at"])
	require.Equal(t, map[string]any{"balance": 10}, updated.Extra["codex_credits_snapshot"])
	require.False(t, updated.Schedulable, "the toggle must not override an administrator pause")
	require.Equal(t, &rateLimitUntil, updated.RateLimitResetAt, "quota recovery belongs to the worker")
	require.Equal(t, "OPENAI_QUOTA_AUTO_RESET_ENABLED", infraerrors.Reason(validateOpenAIManualReset(updated, now)))

	updated, err = svc.SetOpenAIAutoResetCreditEnabled(context.Background(), 42, false)
	require.NoError(t, err)
	require.Equal(t, state, updated.Extra[OpenAIAutoResetCreditStateExtraKey])
	require.Equal(t, 0.82, updated.Extra[OpenAIAutoResetCredit5hThresholdExtraKey])
	require.Equal(t, "OPENAI_QUOTA_AUTO_RESET_IN_PROGRESS", infraerrors.Reason(validateOpenAIManualReset(updated, now)))
	require.NoError(t, validateOpenAIManualReset(updated, now.Add(3*time.Minute)), "the existing stale-attempt guard still expires normally")
}

func TestSetOpenAIAutoResetCreditEnabledRejectsInvalidThresholdButAllowsDisable(t *testing.T) {
	for _, invalid := range []any{0.0, 1.01, "invalid", nil} {
		repo := &autoResetToggleRepo{account: &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Extra: map[string]any{OpenAIAutoResetCredit5hThresholdExtraKey: invalid, OpenAIAutoResetCreditEnabledExtraKey: true}}}
		svc := &adminServiceImpl{accountRepo: repo}
		_, err := svc.SetOpenAIAutoResetCreditEnabled(context.Background(), 42, true)
		require.Equal(t, "OPENAI_AUTO_RESET_CREDIT_THRESHOLD_INVALID", infraerrors.Reason(err))
		require.Nil(t, repo.updates)

		updated, err := svc.SetOpenAIAutoResetCreditEnabled(context.Background(), 42, false)
		require.NoError(t, err)
		require.False(t, ResolveOpenAIAutoResetCreditConfig(updated).Enabled)
		require.Equal(t, invalid, updated.Extra[OpenAIAutoResetCredit5hThresholdExtraKey])
	}
}

func TestSetOpenAIAutoResetCreditEnabledOnlySupportsOpenAIOAuth(t *testing.T) {
	for _, account := range []*Account{
		{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		{Platform: PlatformOpenAI, Type: AccountTypeSetupToken},
		{Platform: PlatformAnthropic, Type: AccountTypeOAuth},
		{Platform: PlatformGrok, Type: AccountTypeOAuth},
	} {
		for _, enabled := range []bool{true, false} {
			repo := &autoResetToggleRepo{account: account}
			svc := &adminServiceImpl{accountRepo: repo}
			_, err := svc.SetOpenAIAutoResetCreditEnabled(context.Background(), 42, enabled)
			require.Equal(t, "OPENAI_AUTO_RESET_CREDIT_ACCOUNT_INVALID", infraerrors.Reason(err))
			require.Nil(t, repo.updates)
		}
	}
}

func installAutoResetToggleTestNotifier(t *testing.T) *OpenAIQuotaAutoResetService {
	t.Helper()
	notifier := NewOpenAIQuotaAutoResetService(nil, nil, nil, nil, nil, nil, nil)
	openAIAutoResetNotifierRegistry.Lock()
	previous := openAIAutoResetNotifierRegistry.service
	openAIAutoResetNotifierRegistry.service = notifier
	openAIAutoResetNotifierRegistry.Unlock()
	t.Cleanup(func() {
		notifier.cancel()
		setOpenAIAutoResetNotifier(previous)
	})
	return notifier
}

func TestSetOpenAIAutoResetCreditEnabledNotifiesOnlyAfterCommit(t *testing.T) {
	for _, mode := range []string{"commit", "rollback", "commit_failure", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			notifier := installAutoResetToggleTestNotifier(t)
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			mock.ExpectBegin()
			tx, err := client.Tx(context.Background())
			require.NoError(t, err)
			repo := &autoResetToggleRepo{account: &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}}
			svc := &adminServiceImpl{accountRepo: repo}
			_, err = svc.SetOpenAIAutoResetCreditEnabled(dbent.NewTxContext(context.Background(), tx), 42, mode != "disabled")
			require.NoError(t, err)
			require.Empty(t, notifier.queue)
			switch mode {
			case "commit", "disabled":
				mock.ExpectCommit()
				require.NoError(t, tx.Commit())
			case "rollback":
				mock.ExpectRollback()
				require.NoError(t, tx.Rollback())
			case "commit_failure":
				mock.ExpectCommit().WillReturnError(errors.New("commit failed"))
				require.Error(t, tx.Commit())
			}
			if mode == "commit" {
				require.Len(t, notifier.queue, 1)
				require.Equal(t, int64(42), <-notifier.queue)
			} else {
				require.Empty(t, notifier.queue)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestSetOpenAIAutoResetCreditEnabledReadAndWriteErrors(t *testing.T) {
	notifier := installAutoResetToggleTestNotifier(t)
	for _, tc := range []struct {
		name      string
		account   *Account
		getErr    error
		updateErr error
		wantErr   error
	}{
		{"not found", nil, nil, nil, ErrAccountNotFound},
		{"read failure", nil, errors.New("read failed"), nil, nil},
		{"write failure", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, nil, errors.New("write failed"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &autoResetToggleRepo{account: tc.account, getErr: tc.getErr, updateErr: tc.updateErr}
			svc := &adminServiceImpl{accountRepo: repo}
			updated, err := svc.SetOpenAIAutoResetCreditEnabled(context.Background(), 42, true)
			require.Error(t, err)
			require.Nil(t, updated)
			require.Empty(t, notifier.queue)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			}
		})
	}
}
