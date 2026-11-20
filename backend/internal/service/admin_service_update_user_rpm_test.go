//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// rpmUserRepoStub 复用 admin_service_update_balance_test.go 的基础 stub 结构，
// 只在 Update 时把入参克隆一份，便于断言修改后的 RPMLimit。
type rpmUserRepoStub struct {
	*userRepoStub
	lastUpdated *User
}

func (s *rpmUserRepoStub) Update(_ context.Context, user *User, _ UserUpdateFields) error {
	if user == nil {
		return nil
	}
	clone := *user
	s.lastUpdated = &clone
	if s.userRepoStub != nil {
		s.userRepoStub.user = &clone
	}
	return nil
}

func TestAdminService_UpdateUser_InvalidatesAuthCacheOnRPMLimitChange(t *testing.T) {
	base := &userRepoStub{user: &User{ID: 42, Email: "u@example.com", RPMLimit: 10}}
	repo := &rpmUserRepoStub{userRepoStub: base}
	invalidator := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{
		userRepo:             repo,
		redeemCodeRepo:       &redeemRepoStub{},
		authCacheInvalidator: invalidator,
	}

	newRPM := 60
	updated, err := svc.UpdateUser(context.Background(), 42, &UpdateUserInput{
		RPMLimit: &newRPM,
	})
	require.NoError(t, err)
	require.NotNil(t, updated)
	require.Equal(t, 60, updated.RPMLimit)
	require.Equal(t, []int64{42}, invalidator.userIDs, "仅修改 RPMLimit 也应失效 API Key 认证缓存")
}

func TestAdminService_UpdateUser_NoInvalidateWhenRPMLimitUnchanged(t *testing.T) {
	base := &userRepoStub{user: &User{ID: 42, Email: "u@example.com", RPMLimit: 10, Username: "old"}}
	repo := &rpmUserRepoStub{userRepoStub: base}
	invalidator := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{
		userRepo:             repo,
		redeemCodeRepo:       &redeemRepoStub{},
		authCacheInvalidator: invalidator,
	}

	newName := "new"
	sameRPM := 10
	_, err := svc.UpdateUser(context.Background(), 42, &UpdateUserInput{
		Username: &newName,
		RPMLimit: &sameRPM,
	})
	require.NoError(t, err)
	require.Empty(t, invalidator.userIDs, "只改 username 不应触发认证缓存失效")
}

func TestAdminService_UpdateUser_InvalidatesAuthCacheAfterConcurrentStatusChange(t *testing.T) {
	for _, tc := range []struct {
		name          string
		requested     string
		statusAtWrite string
	}{
		{"enable after concurrent ban", StatusActive, StatusDisabled},
		{"disable after concurrent unban", StatusDisabled, StatusActive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The snapshot already matches the request; the stored row changed before Update.
			const userID int64 = 42
			storedStatus := tc.statusAtWrite
			invalidator := &authCacheInvalidatorStub{}
			repo := &mockUserRepo{
				getByIDUser: &User{ID: userID, Status: tc.requested},
				updateFn: func(_ context.Context, user *User) error {
					require.Empty(t, invalidator.userIDs)
					require.NotEqual(t, storedStatus, user.Status)
					storedStatus = user.Status
					return nil
				},
			}
			svc := &adminServiceImpl{userRepo: repo, authCacheInvalidator: invalidator}

			_, err := svc.UpdateUser(context.Background(), userID, &UpdateUserInput{Status: tc.requested})
			require.NoError(t, err)
			require.Len(t, repo.updateFields, 1)
			require.True(t, repo.updateFields[0].Status)
			require.Equal(t, tc.requested, storedStatus)
			require.Equal(t, []int64{userID}, invalidator.userIDs)
		})
	}
}

func TestAdminService_UpdateUser_FailedStatusWritePreservesAuthCache(t *testing.T) {
	const userID int64 = 42
	repo := &mockUserRepo{
		getByIDUser: &User{ID: userID, Status: StatusDisabled},
		updateFn:    func(context.Context, *User) error { return ErrUserNotFound },
	}
	invalidator := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{userRepo: repo, authCacheInvalidator: invalidator}

	_, err := svc.UpdateUser(context.Background(), userID, &UpdateUserInput{Status: StatusActive})
	require.ErrorIs(t, err, ErrUserNotFound)
	require.Empty(t, invalidator.userIDs)
}
