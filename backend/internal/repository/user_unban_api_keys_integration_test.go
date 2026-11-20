//go:build integration

package repository

import (
	"time"

	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (s *UserRepoSuite) TestUnbanAPIKeys() {
	keys := NewAPIKeyRepository(s.client, integrationDB)
	auth := service.NewAPIKeyService(keys, s.repo, nil, nil, nil, NewAPIKeyCache(testRedis(s.T())), &config.Config{
		APIKeyAuth: config.APIKeyAuthCacheConfig{L2TTLSeconds: 300},
	})
	moderationRepo := NewContentModerationRepository(integrationDB)
	moderation, err := service.ProvideContentModerationService(nil, moderationRepo, nil, nil, s.repo, nil, auth, nil, keys, nil)
	s.Require().NoError(err)
	admin := service.NewAdminService(nil, s.repo, nil, nil, nil, keys, nil, nil, nil, nil, nil, nil, nil, s.client, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	usagePolicy := NewUsagePolicyRepository(integrationDB)
	now := time.Now().UTC().Truncate(time.Second)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)

	bannedKey := func(userID int64) *service.APIKey {
		s.T().Helper()
		return mustCreateApiKey(s.T(), s.client, &service.APIKey{UserID: userID, Status: service.StatusAPIKeyDisabled})
	}
	assertKey := func(key *service.APIKey, want string) {
		s.T().Helper()
		got, err := s.client.APIKey.Get(mixins.SkipSoftDelete(s.ctx), key.ID)
		s.Require().NoError(err)
		s.Equal(want, got.Status, key.Name)
		s.Equal(key.Quota, got.Quota, key.Name)
		s.Equal(key.QuotaUsed, got.QuotaUsed, key.Name)
	}
	disableUntil := func(userID int64, until time.Time) {
		s.T().Helper()
		changed, err := usagePolicy.DisableUserForUsagePolicy(s.ctx, userID, until)
		s.Require().NoError(err)
		s.Require().True(changed)
	}
	assertUser := func(userID int64, wantStatus string, wantTimer bool) {
		s.T().Helper()
		var status string
		var hasTimer bool
		err := integrationDB.QueryRowContext(s.ctx, "SELECT status, usage_policy_unban_at IS NOT NULL FROM users WHERE id = $1", userID).Scan(&status, &hasTimer)
		s.Require().NoError(err)
		s.Equal(wantStatus, status)
		s.Equal(wantTimer, hasTimer)
	}

	s.Run("risk control unban", func() {
		user := s.mustCreateUser(&service.User{Status: service.StatusDisabled})
		other := s.mustCreateUser(&service.User{Status: service.StatusDisabled})
		otherKey := bannedKey(other.ID)
		deleted := bannedKey(user.ID)
		s.Require().NoError(s.client.APIKey.DeleteOneID(deleted.ID).Exec(s.ctx))
		cases := []struct {
			key  service.APIKey
			want string
		}{
			{service.APIKey{Name: "unlimited", Status: "disabled", QuotaUsed: 100}, "active"},
			{service.APIKey{Name: "room in quota", Status: "disabled", Quota: 10, QuotaUsed: 9, ExpiresAt: &future}, "active"},
			{service.APIKey{Name: "used final credit", Status: "disabled", Quota: 10, QuotaUsed: 10}, "quota_exhausted"},
			{service.APIKey{Name: "expired during ban", Status: "disabled", ExpiresAt: &past}, "expired"},
			{service.APIKey{Name: "expired and exhausted", Status: "disabled", Quota: 10, QuotaUsed: 11, ExpiresAt: &past}, "expired"},
			{service.APIKey{Name: "manually disabled", Status: "inactive", ExpiresAt: &past}, "inactive"},
			{service.APIKey{Name: "manually disabled without expiry", Status: "inactive"}, "inactive"},
			{service.APIKey{Name: "already expired", Status: "expired"}, "expired"},
			{service.APIKey{Name: "already exhausted", Status: "quota_exhausted"}, "quota_exhausted"},
			{service.APIKey{Name: "already active", Status: "active", ExpiresAt: &past}, "active"},
		}
		for i := range cases {
			key := &cases[i].key
			key.UserID = user.ID
			mustCreateApiKey(s.T(), s.client, key)

			cached, err := auth.GetByKey(s.ctx, key.Key)
			s.Require().NoError(err, key.Name)
			s.Equal(key.Status, cached.Status, key.Name)
			s.Require().NotNil(cached.User, key.Name)
			s.Equal(service.StatusDisabled, cached.User.Status, key.Name)
			// L1 is disabled, so another validation without a repository lookup must use Redis.
			lookups := auth.AuthLookupMetrics().Total
			_, _, err = auth.ValidateKey(s.ctx, key.Key)
			s.Require().Error(err, key.Name)
			s.Equal(lookups, auth.AuthLookupMetrics().Total, key.Name)
		}
		result, err := moderation.UnbanUser(s.ctx, user.ID)
		s.Require().NoError(err)
		s.Equal(user.ID, result.UserID)
		s.Equal(service.StatusActive, result.Status)
		assertUser(user.ID, service.StatusActive, false)
		for i := range cases {
			key, want := &cases[i].key, cases[i].want
			assertKey(key, want)
			if key.Status == service.StatusAPIKeyActive {
				continue
			}
			verified, owner, err := auth.ValidateKey(s.ctx, key.Key)
			if want != service.StatusAPIKeyActive {
				s.Equal("API_KEY_INACTIVE", infraerrors.Reason(err), key.Name)
				continue
			}
			s.Require().NoError(err, key.Name)
			s.Equal(key.ID, verified.ID, key.Name)
			s.Equal(user.ID, owner.ID, key.Name)
			s.Require().NotNil(verified.User, key.Name)
			s.Equal(service.StatusActive, verified.User.Status, key.Name)
		}
		assertKey(deleted, "disabled")
		assertKey(otherKey, "disabled")

		// Earlier releases could activate the user while leaving disabled keys behind.
		leftover := bannedKey(user.ID)
		for range 2 {
			_, err = moderation.UnbanUser(s.ctx, user.ID)
			s.Require().NoError(err)
			assertKey(leftover, "active")
			assertKey(deleted, "disabled")
			assertKey(otherKey, "disabled")
		}
	})

	s.Run("user management status change", func() {
		user := s.mustCreateUser(&service.User{})
		key := bannedKey(user.ID)
		_, err := admin.CreateUser(s.ctx, &service.CreateUserInput{Email: "create-during-unban-test@example.com", Password: "test-password"})
		s.Require().NoError(err)
		assertKey(key, "disabled")

		name := "updated"
		_, err = admin.UpdateUser(s.ctx, user.ID, &service.UpdateUserInput{Username: &name})
		s.Require().NoError(err)
		assertKey(key, "disabled")
		_, err = admin.UpdateUser(s.ctx, user.ID, &service.UpdateUserInput{Status: service.StatusActive})
		s.Require().NoError(err)
		assertKey(key, "disabled")
		_, err = admin.UpdateUser(s.ctx, user.ID, &service.UpdateUserInput{Status: service.StatusDisabled})
		s.Require().NoError(err)
		assertUser(user.ID, service.StatusDisabled, false)
		assertKey(key, "disabled")
		_, err = admin.UpdateUser(s.ctx, user.ID, &service.UpdateUserInput{Status: service.StatusActive})
		s.Require().NoError(err)
		assertUser(user.ID, service.StatusActive, false)
		assertKey(key, "active")
	})

	s.Run("usage policy cooldown", func() {
		due := s.mustCreateUser(&service.User{})
		withoutKeys := s.mustCreateUser(&service.User{})
		waiting := s.mustCreateUser(&service.User{})
		disableUntil(due.ID, past.Add(-time.Hour))
		disableUntil(withoutKeys.ID, past)
		disableUntil(waiting.ID, future)
		key := bannedKey(due.ID)
		exhausted := mustCreateApiKey(s.T(), s.client, &service.APIKey{UserID: due.ID, Status: "disabled", Quota: 5, QuotaUsed: 5})
		waitingKey := bannedKey(waiting.ID)

		ids, err := usagePolicy.UnbanDueUsers(s.ctx, 1)
		s.Require().NoError(err)
		s.Equal([]int64{due.ID}, ids)
		assertUser(due.ID, service.StatusActive, false)
		assertKey(key, "active")
		assertKey(exhausted, "quota_exhausted")
		assertUser(withoutKeys.ID, service.StatusDisabled, true)
		ids, err = usagePolicy.UnbanDueUsers(s.ctx, 10)
		s.Require().NoError(err)
		s.Equal([]int64{withoutKeys.ID}, ids)
		assertUser(withoutKeys.ID, service.StatusActive, false)
		ids, err = usagePolicy.UnbanDueUsers(s.ctx, 10)
		s.Require().NoError(err)
		s.Empty(ids)
		assertUser(waiting.ID, service.StatusDisabled, true)
		assertKey(waitingKey, "disabled")
	})

	s.Run("cooldown expiry also lifts an earlier individual key ban", func() {
		user := s.mustCreateUser(&service.User{Role: service.RoleAdmin})
		key := mustCreateApiKey(s.T(), s.client, &service.APIKey{UserID: user.ID})
		other := mustCreateApiKey(s.T(), s.client, &service.APIKey{UserID: user.ID})
		_, changed, err := moderationRepo.(service.ContentModerationDispositionRepository).DisableAPIKeyIfActive(s.ctx, key.ID)
		s.Require().NoError(err)
		s.Require().True(changed)
		assertKey(key, "disabled")
		disableUntil(user.ID, past)
		ids, err := usagePolicy.UnbanDueUsers(s.ctx, 10)
		s.Require().NoError(err)
		s.Contains(ids, user.ID)
		assertKey(key, "active")
		assertKey(other, "active")
	})

	s.Run("key update failure rolls back user and cooldown", func() {
		user := s.mustCreateUser(&service.User{})
		key := mustCreateApiKey(s.T(), s.client, &service.APIKey{UserID: user.ID, Name: "test unban rollback", Status: "disabled"})
		disableUntil(user.ID, past)
		_, err := integrationDB.ExecContext(s.ctx, `ALTER TABLE api_keys ADD CONSTRAINT test_unban_key_transition
			CHECK (name <> 'test unban rollback' OR status = 'disabled') NOT VALID`)
		s.Require().NoError(err)
		t := s.T()
		t.Cleanup(func() {
			if _, err := integrationDB.ExecContext(s.ctx, "ALTER TABLE api_keys DROP CONSTRAINT test_unban_key_transition"); err != nil {
				t.Errorf("remove test constraint: %v", err)
			}
		})
		assertRollback := func(err error, entryPoint string) {
			s.T().Helper()
			s.Require().ErrorContains(err, "test_unban_key_transition", entryPoint)
			assertUser(user.ID, service.StatusDisabled, true)
			assertKey(key, "disabled")
		}
		_, err = moderation.UnbanUser(s.ctx, user.ID)
		assertRollback(err, "risk control")
		_, err = admin.UpdateUser(s.ctx, user.ID, &service.UpdateUserInput{Status: service.StatusActive})
		assertRollback(err, "user management")
		_, err = usagePolicy.UnbanDueUsers(s.ctx, 1)
		assertRollback(err, "usage policy")
	})
}
