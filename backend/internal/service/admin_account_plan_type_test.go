//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdminServiceListAccountsPassesPlanType(t *testing.T) {
	repo := &accountRepoStubForAdminList{listWithFiltersAccounts: []Account{{ID: 9}}}
	svc := &adminServiceImpl{accountRepo: repo}
	accounts, total, err := svc.ListAccounts(context.Background(), 2, 10, "", AccountTypeOAuth, StatusActive, "account", 0, PrivacyModeTrainingOff, "name", "asc", "prolite")
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Equal(t, int64(9), accounts[0].ID)
	require.Equal(t, "prolite", repo.listWithFiltersPlanType)
	require.Equal(t, "account", repo.listWithFiltersSearch)
	require.Equal(t, 2, repo.listWithFiltersParams.Page)

	pool, err := svc.ListAccountsForSchedulerScoreFilter(context.Background(), PlatformOpenAI, AccountTypeOAuth, "", "", 0, "", "prolite")
	require.NoError(t, err)
	require.Equal(t, accounts, pool)
	require.Equal(t, "prolite", repo.listAllWithFiltersPlanType)
}

func TestAdminServiceBulkUpdateResolvesOnlyPlanFilteredIDs(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{listData: []Account{{ID: 11}, {ID: 19}}}
	svc := &adminServiceImpl{accountRepo: repo}
	ids, err := svc.resolveBulkUpdateTargetIDs(context.Background(), &BulkUpdateAccountFilters{
		Platform: PlatformOpenAI, Type: AccountTypeOAuth, Group: "ungrouped", PlanType: "promax",
	})
	require.NoError(t, err)
	require.Equal(t, []int64{11, 19}, ids)
	require.Equal(t, "promax", repo.lastListFilters.planType)
	require.Equal(t, AccountListGroupUngrouped, repo.lastListFilters.groupID)
}
