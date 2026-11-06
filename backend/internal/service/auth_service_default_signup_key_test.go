package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type defaultSignupAPIKeyRepoStub struct {
	APIKeyRepository
	created []SignupAPIKeyCreate
	err     error
}

func (s *defaultSignupAPIKeyRepoStub) CreateSignupAPIKeys(_ context.Context, keys []SignupAPIKeyCreate) error {
	if s.err != nil {
		return s.err
	}
	for _, item := range keys {
		key := *item.Key
		s.created = append(s.created, SignupAPIKeyCreate{Key: &key, GrantExclusiveGroup: item.GrantExclusiveGroup})
	}
	return nil
}

type defaultSignupGroupRepoStub struct {
	GroupRepository
	groups []Group
	err    error
}

func (s *defaultSignupGroupRepoStub) ListActive(context.Context) ([]Group, error) {
	return s.groups, s.err
}

type signupAPIKeyProvisionerSpy struct {
	userIDs []int64
	err     error
}

func (s *signupAPIKeyProvisionerSpy) ProvisionDefaultSignupAPIKey(_ context.Context, userID int64) error {
	s.userIDs = append(s.userIDs, userID)
	return s.err
}

func TestPostAuthUserBootstrapProvisionsDefaultSignupAPIKey(t *testing.T) {
	spy := &signupAPIKeyProvisionerSpy{}
	auth := &AuthService{}
	auth.SetSignupAPIKeyProvisioner(spy)

	auth.postAuthUserBootstrap(context.Background(), &User{ID: 42}, "email", false)

	if len(spy.userIDs) != 1 || spy.userIDs[0] != 42 {
		t.Fatalf("provisioner user IDs = %v, want [42]", spy.userIDs)
	}
}

func TestPostAuthUserBootstrapSkipsInvalidUser(t *testing.T) {
	spy := &signupAPIKeyProvisionerSpy{}
	auth := &AuthService{signupAPIKeyProvisioner: spy}

	auth.postAuthUserBootstrap(context.Background(), &User{ID: 0}, "email", false)

	if len(spy.userIDs) != 0 {
		t.Fatalf("provisioner called for invalid user: %v", spy.userIDs)
	}
}

func signupGPTGroups() []Group {
	return []Group{
		{ID: 7, Name: "GPT-PRO", Platform: PlatformOpenAI, Status: StatusActive},
		{ID: 42, Name: TiboHealthyGroupName, Platform: PlatformOpenAI, Status: StatusActive, IsExclusive: true},
		{ID: 43, Name: TiboWelfareGroupName, Platform: PlatformOpenAI, Status: StatusActive},
	}
}

func TestProvisionDefaultSignupAPIKeyCreatesBothGPTGroups(t *testing.T) {
	groupRepo := &defaultSignupGroupRepoStub{groups: signupGPTGroups()}
	keyRepo := &defaultSignupAPIKeyRepoStub{}
	apiKeys := &APIKeyService{apiKeyRepo: keyRepo, groupRepo: groupRepo}

	require.NoError(t, apiKeys.ProvisionDefaultSignupAPIKey(context.Background(), 99))
	require.Len(t, keyRepo.created, 2)
	for i, want := range []struct {
		name      string
		groupID   int64
		exclusive bool
	}{
		{TiboHealthyGroupName, 42, true}, {TiboWelfareGroupName, 43, false},
	} {
		item := keyRepo.created[i]
		require.Equal(t, int64(99), item.Key.UserID)
		require.Equal(t, want.name, item.Key.Name)
		require.NotNil(t, item.Key.GroupID)
		require.Equal(t, want.groupID, *item.Key.GroupID)
		require.Equal(t, StatusActive, item.Key.Status)
		require.Equal(t, want.exclusive, item.GrantExclusiveGroup)
		require.True(t, strings.HasPrefix(item.Key.Key, "sk-"))
		require.Len(t, item.Key.Key, 67)
	}
	require.NotEqual(t, keyRepo.created[0].Key.Key, keyRepo.created[1].Key.Key)
}

func TestProvisionDefaultSignupAPIKeyRequiresBothActiveOpenAIGroups(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*defaultSignupGroupRepoStub)
	}{
		{"missing", func(r *defaultSignupGroupRepoStub) { r.groups = r.groups[:2] }},
		{"inactive", func(r *defaultSignupGroupRepoStub) { r.groups[2].Status = StatusDisabled }},
		{"wrong platform", func(r *defaultSignupGroupRepoStub) { r.groups[2].Platform = PlatformAnthropic }},
		{"different name", func(r *defaultSignupGroupRepoStub) { r.groups[1].Name = " GPT-不降智 " }},
		{"ambiguous", func(r *defaultSignupGroupRepoStub) { r.groups = append(r.groups, r.groups[1]) }},
		{"list error", func(r *defaultSignupGroupRepoStub) { r.err = errors.New("test group lookup failed") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groups := &defaultSignupGroupRepoStub{groups: signupGPTGroups()}
			tc.change(groups)
			keys := &defaultSignupAPIKeyRepoStub{}
			svc := &APIKeyService{apiKeyRepo: keys, groupRepo: groups}
			require.Error(t, svc.ProvisionDefaultSignupAPIKey(context.Background(), 99))
			require.Empty(t, keys.created, "do not provision only one key")
		})
	}
}

func TestProvisionDefaultSignupAPIKeySubscriptionDoesNotGrantExclusiveAccess(t *testing.T) {
	groups := &defaultSignupGroupRepoStub{groups: signupGPTGroups()}
	groups.groups[1].SubscriptionType = SubscriptionTypeSubscription
	keys := &defaultSignupAPIKeyRepoStub{}
	svc := &APIKeyService{apiKeyRepo: keys, groupRepo: groups}
	require.NoError(t, svc.ProvisionDefaultSignupAPIKey(context.Background(), 99))
	require.Len(t, keys.created, 2)
	require.False(t, keys.created[0].GrantExclusiveGroup)
}

func TestProvisionDefaultSignupAPIKeyPropagatesBatchFailure(t *testing.T) {
	keys := &defaultSignupAPIKeyRepoStub{err: errors.New("test key transaction failed")}
	svc := &APIKeyService{apiKeyRepo: keys, groupRepo: &defaultSignupGroupRepoStub{groups: signupGPTGroups()}}
	require.ErrorContains(t, svc.ProvisionDefaultSignupAPIKey(context.Background(), 99), "test key transaction failed")
	require.Empty(t, keys.created)
}

func TestPostAuthUserBootstrapCreatesTwoDefaultKeys(t *testing.T) {
	for _, source := range []string{"email", "linuxdo", "oidc", "wechat", "github", "google", "dingtalk"} {
		t.Run(source, func(t *testing.T) {
			keys := &defaultSignupAPIKeyRepoStub{}
			auth := &AuthService{signupAPIKeyProvisioner: &APIKeyService{apiKeyRepo: keys, groupRepo: &defaultSignupGroupRepoStub{groups: signupGPTGroups()}}}
			auth.postAuthUserBootstrap(context.Background(), &User{ID: 42}, source, false)
			require.Len(t, keys.created, 2)
			require.Equal(t, TiboHealthyGroupName, keys.created[0].Key.Name)
			require.Equal(t, TiboWelfareGroupName, keys.created[1].Key.Name)
		})
	}
}
