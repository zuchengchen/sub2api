//go:build unit

package service

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func scopedStatisticsServiceContext(action string) context.Context {
	return adminaccess.WithPrincipal(context.Background(), &adminaccess.Principal{Policy: adminaccess.Policy{Rules: []adminaccess.Rule{{Permissions: []string{action}, GroupIDs: []int64{1}, AccountTypes: []string{"oauth"}, Platforms: []string{"openai"}}}}})
}
func TestScopedAdminQuotaCacheWriteRechecksScope(t *testing.T) {
	a := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, GroupIDs: []int64{2}}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{1: a}}
	svc := &OpenAIQuotaService{accountRepo: repo}
	err := svc.CacheCreditsSnapshot(scopedStatisticsServiceContext("openai.quota.read"), 1, &OpenAIQuotaUsage{})
	require.Equal(t, 404, infraerrors.Code(err))
	require.Zero(t, repo.extraUpdateCalls)
}

func TestScopedAdminSnapshotWriteDoesNotNotifyAutomaticReset(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, GroupIDs: []int64{1}}
		repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{1: a}}
		svc := &OpenAIQuotaService{accountRepo: repo}
		notifier := NewOpenAIQuotaAutoResetService(nil, nil, nil, nil, nil, nil, nil)
		openAIAutoResetNotifierRegistry.Lock()
		previous := openAIAutoResetNotifierRegistry.service
		openAIAutoResetNotifierRegistry.service = notifier
		openAIAutoResetNotifierRegistry.Unlock()
		defer func() {
			openAIAutoResetNotifierRegistry.Lock()
			openAIAutoResetNotifierRegistry.service = previous
			openAIAutoResetNotifierRegistry.Unlock()
			notifier.cancel()
		}()
		require.NoError(t, svc.CacheCreditsSnapshot(scopedStatisticsServiceContext("openai.quota.read"), 1, &OpenAIQuotaUsage{}))
		synctest.Wait()
		require.Equal(t, 1, repo.extraUpdateCalls)
		require.Empty(t, notifier.queue)
	})
}

func TestScopedAdminQuotaHeaderReloadRechecksScope(t *testing.T) {
	a := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, GroupIDs: []int64{2}}
	svc := &OpenAIQuotaService{accountRepo: &stubQuotaAccountRepo{accounts: map[int64]*Account{1: a}}}
	headers, _, err := svc.buildCodexQuotaHeaders(scopedStatisticsServiceContext("openai.quota.read"), 1, "token", "upstream-id", false)
	require.Nil(t, headers)
	require.Equal(t, 404, infraerrors.Code(err))
}
