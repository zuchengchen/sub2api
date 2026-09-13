package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsUsagePolicyViolation(t *testing.T) {
	t.Parallel()

	msg := "Invalid prompt: your prompt was flagged as potentially violating our usage policy. Please try again"
	require.True(t, IsUsagePolicyViolation(&OpsInsertErrorLogInput{
		UpstreamErrorMessage: &msg,
		ErrorMessage:         "Upstream service temporarily unavailable",
	}))
	require.True(t, IsUsagePolicyViolation(&OpsInsertErrorLogInput{ErrorMessage: msg}))
	require.False(t, IsUsagePolicyViolation(&OpsInsertErrorLogInput{
		ErrorMessage:         "Upstream service temporarily unavailable",
		UpstreamErrorMessage: strPtr("Rate limit exceeded"),
	}))
	require.False(t, IsUsagePolicyViolation(nil))
}

type usagePolicyRepoStub struct {
	insertedID  int64
	inserted    bool
	count       int
	insertErr   error
	disabled    bool
	disableErr  error
	users       []UsagePolicyUserStat
	last        *UsagePolicyViolation
	disposition struct {
		id         int64
		autoBanned bool
		skipReason string
	}
}

func (r *usagePolicyRepoStub) InsertViolation(_ context.Context, v *UsagePolicyViolation) (int64, bool, int, error) {
	r.last = v
	return r.insertedID, r.inserted, r.count, r.insertErr
}

func (r *usagePolicyRepoStub) UpdateViolationDisposition(_ context.Context, id int64, autoBanned bool, skipReason string) error {
	r.disposition.id = id
	r.disposition.autoBanned = autoBanned
	r.disposition.skipReason = skipReason
	return nil
}

func (r *usagePolicyRepoStub) ListUserStats(context.Context) ([]UsagePolicyUserStat, error) {
	return r.users, nil
}

func (r *usagePolicyRepoStub) DisableUserIfActive(context.Context, int64) (bool, error) {
	return r.disabled, r.disableErr
}

type usagePolicySettingsStub struct {
	values map[string]string
}

func (s *usagePolicySettingsStub) Get(context.Context, string) (*Setting, error) {
	return nil, ErrSettingNotFound
}
func (s *usagePolicySettingsStub) GetValue(_ context.Context, key string) (string, error) {
	if s.values == nil {
		return "", ErrSettingNotFound
	}
	value, ok := s.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}
func (s *usagePolicySettingsStub) Set(_ context.Context, key, value string) error {
	if s.values == nil {
		s.values = map[string]string{}
	}
	s.values[key] = value
	return nil
}
func (s *usagePolicySettingsStub) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (s *usagePolicySettingsStub) SetMultiple(context.Context, map[string]string) error { return nil }
func (s *usagePolicySettingsStub) GetAll(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}
func (s *usagePolicySettingsStub) Delete(context.Context, string) error { return nil }

type usagePolicyUserRepoStub struct {
	user *User
	err  error
}

func (r *usagePolicyUserRepoStub) GetByID(context.Context, int64) (*User, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.user, nil
}

type usagePolicyAuthCacheStub struct {
	userID int64
}

func (s *usagePolicyAuthCacheStub) InvalidateAuthCacheByKey(context.Context, string) {}
func (s *usagePolicyAuthCacheStub) InvalidateAuthCacheByUserID(_ context.Context, userID int64) {
	s.userID = userID
}
func (s *usagePolicyAuthCacheStub) InvalidateAuthCacheByGroupID(context.Context, int64) {}

func usagePolicyEnabledSettings() *usagePolicySettingsStub {
	return &usagePolicySettingsStub{values: map[string]string{
		SettingKeyRiskControlEnabled: "true",
		SettingKeyUsagePolicyConfig:  `{"enabled":true,"auto_ban_enabled":true,"ban_threshold":1}`,
	}}
}

func TestUsagePolicyObserveDisablesRegularUser(t *testing.T) {
	userID := int64(42)
	repo := &usagePolicyRepoStub{insertedID: 9, inserted: true, count: 1, disabled: true}
	users := &usagePolicyUserRepoStub{user: &User{ID: userID, Role: RoleUser, Status: StatusActive}}
	cache := &usagePolicyAuthCacheStub{}
	svc := NewUsagePolicyService(repo, usagePolicyEnabledSettings(), users, cache)

	msg := "Invalid prompt: your prompt was flagged as potentially violating our usage policy."
	svc.ObserveErrorLogs(context.Background(), []*OpsInsertErrorLogInput{{
		UserID:               &userID,
		RequestID:            "req-1",
		ErrorMessage:         "Upstream service temporarily unavailable",
		UpstreamErrorMessage: &msg,
	}})

	require.True(t, repo.disposition.autoBanned)
	require.Equal(t, int64(42), cache.userID)
	require.Equal(t, "", repo.disposition.skipReason)
}

func TestUsagePolicyObserveSkipsAdmin(t *testing.T) {
	userID := int64(1)
	repo := &usagePolicyRepoStub{insertedID: 3, inserted: true, count: 8, disabled: true}
	users := &usagePolicyUserRepoStub{user: &User{ID: userID, Role: RoleAdmin, Status: StatusActive}}
	cache := &usagePolicyAuthCacheStub{}
	svc := NewUsagePolicyService(repo, usagePolicyEnabledSettings(), users, cache)

	msg := "your prompt was flagged as potentially violating our usage policy"
	svc.ObserveErrorLogs(context.Background(), []*OpsInsertErrorLogInput{{
		UserID:       &userID,
		RequestID:    "req-admin",
		ErrorMessage: msg,
	}})

	require.False(t, repo.disposition.autoBanned)
	require.Equal(t, usagePolicySkipReasonAdmin, repo.disposition.skipReason)
	require.Equal(t, int64(0), cache.userID)
}

func TestUsagePolicyObserveIgnoresUnrelatedErrors(t *testing.T) {
	userID := int64(7)
	repo := &usagePolicyRepoStub{insertedID: 1, inserted: true, count: 1, disabled: true}
	svc := NewUsagePolicyService(repo, usagePolicyEnabledSettings(), &usagePolicyUserRepoStub{
		user: &User{ID: userID, Role: RoleUser, Status: StatusActive},
	}, &usagePolicyAuthCacheStub{})

	svc.ObserveErrorLogs(context.Background(), []*OpsInsertErrorLogInput{{
		UserID:       &userID,
		ErrorMessage: "Rate limit exceeded",
	}})
	require.Nil(t, repo.last)
}

func TestUsagePolicyObserveRequiresRiskControl(t *testing.T) {
	userID := int64(7)
	repo := &usagePolicyRepoStub{insertedID: 1, inserted: true, count: 1, disabled: true}
	svc := NewUsagePolicyService(repo, &usagePolicySettingsStub{values: map[string]string{
		SettingKeyRiskControlEnabled: "false",
	}}, &usagePolicyUserRepoStub{user: &User{ID: userID, Role: RoleUser, Status: StatusActive}}, nil)

	msg := "flagged as potentially violating our usage policy"
	svc.ObserveErrorLogs(context.Background(), []*OpsInsertErrorLogInput{{
		UserID:       &userID,
		ErrorMessage: msg,
	}})
	require.Nil(t, repo.last)
}

func TestUsagePolicyGetStats(t *testing.T) {
	repo := &usagePolicyRepoStub{users: []UsagePolicyUserStat{
		{UserID: 2, Email: "a@example.com", Status: StatusDisabled, Count: 4, AutoBanned: true},
		{UserID: 3, Email: "b@example.com", Status: StatusActive, Count: 1, AutoBanned: false},
	}}
	svc := NewUsagePolicyService(repo, usagePolicyEnabledSettings(), nil, nil)
	stats, err := svc.GetStats(context.Background())
	require.NoError(t, err)
	require.Equal(t, 5, stats.Total)
	require.Equal(t, 2, stats.UniqueUsers)
	require.Equal(t, 1, stats.DisabledUsers)
	require.Equal(t, 1, stats.AutoBannedUsers)
}

func TestUsagePolicyUpdateConfig(t *testing.T) {
	settings := usagePolicyEnabledSettings()
	svc := NewUsagePolicyService(nil, settings, nil, nil)
	enabled := false
	threshold := 3
	cfg, err := svc.UpdateConfig(context.Background(), UpdateUsagePolicyConfigInput{
		Enabled:      &enabled,
		BanThreshold: &threshold,
	})
	require.NoError(t, err)
	require.False(t, cfg.Enabled)
	require.Equal(t, 3, cfg.BanThreshold)
	require.Contains(t, settings.values[SettingKeyUsagePolicyConfig], `"enabled":false`)
}

func TestUsagePolicyInsertFailureDoesNotBan(t *testing.T) {
	userID := int64(9)
	repo := &usagePolicyRepoStub{insertErr: errors.New("db down"), inserted: true, count: 1, disabled: true}
	svc := NewUsagePolicyService(repo, usagePolicyEnabledSettings(), &usagePolicyUserRepoStub{
		user: &User{ID: userID, Role: RoleUser, Status: StatusActive},
	}, &usagePolicyAuthCacheStub{})
	msg := "flagged as potentially violating our usage policy"
	svc.ObserveErrorLogs(context.Background(), []*OpsInsertErrorLogInput{{
		UserID:       &userID,
		ErrorMessage: msg,
	}})
	require.Equal(t, int64(0), repo.disposition.id)
}
