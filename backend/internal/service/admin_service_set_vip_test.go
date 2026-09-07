//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type vipToggleUserRepoStub struct {
	*userRepoStub
	setVIPCalls int
	lastVIP     bool
}

func (s *vipToggleUserRepoStub) SetVIP(_ context.Context, id int64, vip bool) (bool, error) {
	s.setVIPCalls++
	s.lastVIP = vip
	if s.user == nil || s.user.ID != id {
		return false, ErrUserNotFound
	}
	if s.user.IsVIP == vip {
		return false, nil
	}
	s.user.IsVIP = vip
	return true, nil
}

func TestAdminService_SetUserVIP_GrantsAndInvalidatesCache(t *testing.T) {
	repo := &vipToggleUserRepoStub{userRepoStub: &userRepoStub{user: &User{ID: 429, Email: "u@example.com", IsVIP: false}}}
	invalidator := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{userRepo: repo, authCacheInvalidator: invalidator}

	updated, err := svc.SetUserVIP(context.Background(), 429, true)
	require.NoError(t, err)
	require.True(t, updated.IsVIP)
	require.Equal(t, 1, repo.setVIPCalls)
	require.Equal(t, []int64{429}, invalidator.userIDs)
}

func TestAdminService_SetUserVIP_CancelsAndInvalidatesCache(t *testing.T) {
	repo := &vipToggleUserRepoStub{userRepoStub: &userRepoStub{user: &User{ID: 429, Email: "u@example.com", IsVIP: true}}}
	invalidator := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{userRepo: repo, authCacheInvalidator: invalidator}

	updated, err := svc.SetUserVIP(context.Background(), 429, false)
	require.NoError(t, err)
	require.False(t, updated.IsVIP)
	require.Equal(t, 1, repo.setVIPCalls)
	require.Equal(t, []int64{429}, invalidator.userIDs)
}

func TestAdminService_SetUserVIP_NoopWhenUnchanged(t *testing.T) {
	repo := &vipToggleUserRepoStub{userRepoStub: &userRepoStub{user: &User{ID: 429, Email: "u@example.com", IsVIP: true}}}
	invalidator := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{userRepo: repo, authCacheInvalidator: invalidator}

	updated, err := svc.SetUserVIP(context.Background(), 429, true)
	require.NoError(t, err)
	require.True(t, updated.IsVIP)
	require.Zero(t, repo.setVIPCalls)
	require.Empty(t, invalidator.userIDs)
}
