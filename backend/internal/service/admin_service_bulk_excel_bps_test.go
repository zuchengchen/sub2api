package service

import (
	"context"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type excelBPSBulkAccountRepo struct {
	AccountRepository
	bulkUpdateCalls  int
	bulkUpdateIDs    []int64
	lastBulkUpdate   AccountBulkUpdate
	getByIDsAccounts []*Account
	getByIDsIDs      []int64
	listData         []Account
	listResult       *pagination.PaginationResult
}

func (r *excelBPSBulkAccountRepo) BulkUpdate(_ context.Context, ids []int64, updates AccountBulkUpdate) (int64, error) {
	r.bulkUpdateCalls++
	r.bulkUpdateIDs = append([]int64{}, ids...)
	r.lastBulkUpdate = updates
	return int64(len(ids)), nil
}

func (r *excelBPSBulkAccountRepo) GetByIDs(_ context.Context, ids []int64) ([]*Account, error) {
	r.getByIDsIDs = append([]int64{}, ids...)
	return r.getByIDsAccounts, nil
}

func (r *excelBPSBulkAccountRepo) ListWithFilters(context.Context, pagination.PaginationParams, string, string, string, string, int64, string) ([]Account, *pagination.PaginationResult, error) {
	if r.listResult != nil {
		return r.listData, r.listResult, nil
	}
	return r.listData, &pagination.PaginationResult{Total: int64(len(r.listData))}, nil
}

func requireExcelBPSApplicationError(t *testing.T, err error, reason string) {
	t.Helper()
	var appErr *infraerrors.ApplicationError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, reason, appErr.Reason)
}

func TestAdminServiceBulkUpdateAccounts_ExcelBPSSettings(t *testing.T) {
	tests := []struct {
		name  string
		extra map[string]any
		want  map[string]any
	}{
		{
			name: "selected models and billing",
			extra: map[string]any{
				"openai_excel_bps":                         true,
				"openai_excel_bps_models":                  []any{" gpt-6-astra ", "gpt-6-sol", "gpt-6-astra", " "},
				"openai_excel_bps_cache_creation_as_input": true,
				"unrelated":                                "preserved",
			},
			want: map[string]any{
				"openai_excel_bps":                         true,
				"openai_excel_bps_models":                  []string{"gpt-6-astra", "gpt-6-sol"},
				"openai_excel_bps_cache_creation_as_input": true,
				"unrelated":                                "preserved",
			},
		},
		{
			name:  "all models",
			extra: map[string]any{"openai_excel_bps": true, "openai_excel_bps_models": nil},
			want:  map[string]any{"openai_excel_bps": true, "openai_excel_bps_models": nil},
		},
		{
			name:  "empty model list",
			extra: map[string]any{"openai_excel_bps_models": []any{}},
			want:  map[string]any{"openai_excel_bps_models": []string{}},
		},
		{
			name:  "typed model list",
			extra: map[string]any{"openai_excel_bps_models": []string{" gpt-6-astra ", "gpt-6-astra"}},
			want:  map[string]any{"openai_excel_bps_models": []string{"gpt-6-astra"}},
		},
		{
			name:  "disable clears subordinate settings",
			extra: map[string]any{"openai_excel_bps": false},
			want:  map[string]any{"openai_excel_bps": false, "openai_excel_bps_models": nil, "openai_excel_bps_cache_creation_as_input": false},
		},
		{
			name:  "disabled overrides subordinate settings",
			extra: map[string]any{"openai_excel_bps": false, "openai_excel_bps_models": []string{"gpt-6-astra"}, "openai_excel_bps_cache_creation_as_input": true},
			want:  map[string]any{"openai_excel_bps": false, "openai_excel_bps_models": nil, "openai_excel_bps_cache_creation_as_input": false},
		},
		{
			name:  "billing only",
			extra: map[string]any{"openai_excel_bps_cache_creation_as_input": false},
			want:  map[string]any{"openai_excel_bps_cache_creation_as_input": false},
		},
		{
			name:  "auto disable only",
			extra: map[string]any{"openai_excel_bps_auto_disable_on_403": true},
			want:  map[string]any{"openai_excel_bps_auto_disable_on_403": true},
		},
		{
			name:  "disabled clears auto disable",
			extra: map[string]any{"openai_excel_bps": false, "openai_excel_bps_auto_disable_on_403": true},
			want:  map[string]any{"openai_excel_bps": false, "openai_excel_bps_models": nil, "openai_excel_bps_cache_creation_as_input": false, "openai_excel_bps_auto_disable_on_403": false},
		},
		{
			name:  "omitted BPS settings",
			extra: map[string]any{"unrelated": true},
			want:  map[string]any{"unrelated": true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &excelBPSBulkAccountRepo{getByIDsAccounts: []*Account{
				{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
				{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			}}
			svc := &adminServiceImpl{accountRepo: repo}
			result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
				AccountIDs: []int64{1, 2}, Extra: tt.extra,
			})
			require.NoError(t, err)
			require.Equal(t, 2, result.Success)
			require.Equal(t, 1, repo.bulkUpdateCalls)
			require.Equal(t, []int64{1, 2}, repo.bulkUpdateIDs)
			require.Equal(t, tt.want, repo.lastBulkUpdate.Extra)
			require.Empty(t, repo.lastBulkUpdate.Credentials)
		})
	}
}

func TestAdminServiceBulkUpdateAccounts_RejectsInvalidExcelBPSValues(t *testing.T) {
	for _, extra := range []map[string]any{
		{"openai_excel_bps": "true"},
		{"openai_excel_bps": nil},
		{"openai_excel_bps": 1},
		{"openai_excel_bps_cache_creation_as_input": "false"},
		{"openai_excel_bps_cache_creation_as_input": nil},
		{"openai_excel_bps_auto_disable_on_403": "true"},
		{"openai_excel_bps_auto_disable_on_403": nil},
		{"openai_excel_bps_models": "gpt-6-astra"},
		{"openai_excel_bps_models": map[string]any{}},
		{"openai_excel_bps_models": []any{"gpt-6-astra", 1}},
		{"openai_excel_bps_models": []any{nil}},
		{"openai_excel_bps": false, "openai_excel_bps_models": true},
	} {
		repo := &excelBPSBulkAccountRepo{}
		svc := &adminServiceImpl{accountRepo: repo}
		result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
			AccountIDs: []int64{1}, Extra: extra,
		})
		require.Nil(t, result)
		requireExcelBPSApplicationError(t, err, "OPENAI_EXCEL_BPS_INVALID")
		require.Zero(t, repo.bulkUpdateCalls)
	}
}

func TestAdminServiceBulkUpdateAccounts_RejectsInvalidExcelBPSTargets(t *testing.T) {
	parentID := int64(99)
	targets := []struct {
		name    string
		account *Account
	}{
		{"missing", nil},
		{"other platform", &Account{ID: 2, Platform: PlatformAnthropic, Type: AccountTypeOAuth}},
		{"API key", &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}},
		{"setup token", &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeSetupToken}},
		{"shadow", &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parentID}},
		{"agent identity", &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"auth_mode": OpenAIAuthModeAgentIdentity}}},
		{"personal access token", &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"auth_mode": OpenAIAuthModePersonalAccessToken}}},
		{"legacy personal access token", &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{openAIAuthModeLegacyCredentialKey: OpenAIAuthModePersonalAccessToken}}},
	}
	for _, target := range targets {
		t.Run(target.name, func(t *testing.T) {
			for _, extra := range []map[string]any{
				{"openai_excel_bps": true},
				{"openai_excel_bps": false},
				{"openai_excel_bps_models": nil},
				{"openai_excel_bps_cache_creation_as_input": true},
				{"openai_excel_bps_auto_disable_on_403": true},
			} {
				repo := &excelBPSBulkAccountRepo{getByIDsAccounts: []*Account{
					{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
				}}
				if target.account != nil {
					repo.getByIDsAccounts = append(repo.getByIDsAccounts, target.account)
				}
				svc := &adminServiceImpl{accountRepo: repo}
				result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
					AccountIDs: []int64{1, 2}, Extra: extra,
				})
				require.Nil(t, result)
				requireExcelBPSApplicationError(t, err, "OPENAI_BULK_TARGET_INVALID")
				require.Zero(t, repo.bulkUpdateCalls, "eligible accounts must not be partially updated")
			}
		})
	}
}

func TestAdminServiceBulkUpdateAccounts_ExcelBPSFilteredTargets(t *testing.T) {
	for _, valid := range []bool{true, false} {
		account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
		if !valid {
			account.Type = AccountTypeAPIKey
		}
		repo := &excelBPSBulkAccountRepo{
			listData:         []Account{{ID: 7}},
			listResult:       &pagination.PaginationResult{Total: 1},
			getByIDsAccounts: []*Account{account},
		}
		svc := &adminServiceImpl{accountRepo: repo}
		result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
			Filters: &BulkUpdateAccountFilters{Platform: PlatformOpenAI},
			Extra:   map[string]any{"openai_excel_bps": true, "openai_excel_bps_models": []string{"gpt-6-astra"}},
		})
		require.Equal(t, []int64{7}, repo.getByIDsIDs)
		if valid {
			require.NoError(t, err)
			require.Equal(t, 1, result.Success)
			require.Equal(t, []int64{7}, repo.bulkUpdateIDs)
		} else {
			require.Nil(t, result)
			requireExcelBPSApplicationError(t, err, "OPENAI_BULK_TARGET_INVALID")
			require.Zero(t, repo.bulkUpdateCalls)
		}
	}
}
