package service

import (
	"context"
	"testing"
)

type defaultSignupAPIKeyRepoStub struct {
	APIKeyRepository
	created             *APIKey
	grantExclusiveGroup bool
}

func (s *defaultSignupAPIKeyRepoStub) Create(_ context.Context, key *APIKey) error {
	copy := *key
	s.created = &copy
	return nil
}

func (s *defaultSignupAPIKeyRepoStub) CreateSignupAPIKey(_ context.Context, key *APIKey, grantExclusiveGroup bool) error {
	s.grantExclusiveGroup = grantExclusiveGroup
	return s.Create(context.Background(), key)
}

type defaultSignupGroupRepoStub struct {
	GroupRepository
	groups []Group
}

func (s *defaultSignupGroupRepoStub) ListActive(context.Context) ([]Group, error) {
	return s.groups, nil
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

func TestProvisionDefaultSignupAPIKeyUsesGPTProGroup(t *testing.T) {
	groupRepo := &defaultSignupGroupRepoStub{groups: []Group{
		{ID: 7, Name: "other", Status: StatusActive},
		{ID: 42, Name: " GPT-PRO ", Status: StatusActive, IsExclusive: true},
	}}
	keyRepo := &defaultSignupAPIKeyRepoStub{}
	apiKeys := &APIKeyService{apiKeyRepo: keyRepo, groupRepo: groupRepo}

	if err := apiKeys.ProvisionDefaultSignupAPIKey(context.Background(), 99); err != nil {
		t.Fatalf("ProvisionDefaultSignupAPIKey returned error: %v", err)
	}
	if keyRepo.created == nil {
		t.Fatal("expected a default API key to be created")
	}
	if keyRepo.created.UserID != 99 {
		t.Fatalf("created key user ID = %d, want 99", keyRepo.created.UserID)
	}
	if keyRepo.created.Name != defaultSignupAPIKeyName {
		t.Fatalf("created key name = %q, want %q", keyRepo.created.Name, defaultSignupAPIKeyName)
	}
	if keyRepo.created.GroupID == nil || *keyRepo.created.GroupID != 42 {
		t.Fatalf("created key group ID = %v, want 42", keyRepo.created.GroupID)
	}
	if keyRepo.created.Status != StatusActive {
		t.Fatalf("created key status = %q, want %q", keyRepo.created.Status, StatusActive)
	}
	if !keyRepo.grantExclusiveGroup {
		t.Fatal("expected exclusive GPT-PRO access to be granted atomically")
	}
	if len(keyRepo.created.Key) < len("sk-")+64 || keyRepo.created.Key[:3] != "sk-" {
		t.Fatalf("created key has unexpected format: %q", keyRepo.created.Key)
	}
}
