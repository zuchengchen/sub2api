package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUsagePolicyClientErrorExtractsSSEMessage(t *testing.T) {
	t.Parallel()
	body := []byte("event: response.failed\ndata: {\"response\":{\"error\":{\"code\":\"invalid_prompt\",\"message\":\"Invalid prompt: your prompt was flagged as potentially violating our usage policy. Please try again\"}}}\n\n")
	msg, ok := UsagePolicyClientError(body, "Upstream service temporarily unavailable")
	require.True(t, ok)
	require.Contains(t, msg, "flagged as potentially violating our usage policy")
	require.True(t, strings.HasSuffix(msg, UsagePolicyClientNotice))
	require.NotContains(t, msg, "Upstream service temporarily unavailable")

	_, ok = UsagePolicyClientError([]byte(`{"error":{"message":"Rate limit exceeded"}}`))
	require.False(t, ok)
}

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
	lastUserID  int64
	lastUntil   time.Time
	unbannedIDs []int64
	keys        []UsagePolicyKeyStat
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

func (r *usagePolicyRepoStub) ListKeyStats(context.Context) ([]UsagePolicyKeyStat, error) {
	return r.keys, nil
}

func (r *usagePolicyRepoStub) DisableUserForUsagePolicy(_ context.Context, userID int64, until time.Time) (bool, error) {
	r.lastUserID = userID
	r.lastUntil = until
	return r.disabled, r.disableErr
}

func (r *usagePolicyRepoStub) UnbanDueUsers(context.Context, int) ([]int64, error) {
	return r.unbannedIDs, nil
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

type usagePolicyAuthCacheStub struct {
	key    string
	userID int64
}

func (s *usagePolicyAuthCacheStub) InvalidateAuthCacheByKey(_ context.Context, key string) {
	s.key = key
}
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

func TestUsagePolicyObserveDisablesUser(t *testing.T) {
	userID := int64(42)
	keyID := int64(88)
	repo := &usagePolicyRepoStub{insertedID: 9, inserted: true, count: 1, disabled: true}
	cache := &usagePolicyAuthCacheStub{}
	archiver := &usagePolicyArchiverStub{}
	svc := NewUsagePolicyService(repo, usagePolicyEnabledSettings(), cache)
	svc.SetConversationArchiver(archiver)

	msg := "Invalid prompt: your prompt was flagged as potentially violating our usage policy."
	svc.ObserveErrorLogs(context.Background(), []*OpsInsertErrorLogInput{{
		UserID:               &userID,
		APIKeyID:             &keyID,
		RequestID:            "req-1",
		ErrorMessage:         "Upstream service temporarily unavailable",
		UpstreamErrorMessage: &msg,
		ConversationInput: &ContentModerationCheckInput{
			RawRequest: ContentModerationRawRequest{Body: []byte(`{"input":"hi"}`)},
		},
	}})

	require.True(t, repo.disposition.autoBanned)
	require.Equal(t, int64(42), repo.lastUserID)
	require.WithinDuration(t, time.Now().Add(time.Hour), repo.lastUntil, 5*time.Second)
	require.Equal(t, int64(42), cache.userID)
	require.Equal(t, "", repo.disposition.skipReason)
	require.Len(t, archiver.calls, 1)
	require.Equal(t, []byte(`{"input":"hi"}`), archiver.calls[0].RawRequest.Body)
}

func TestUsagePolicyObserveDisablesAdminUser(t *testing.T) {
	userID := int64(1)
	keyID := int64(3)
	repo := &usagePolicyRepoStub{insertedID: 3, inserted: true, count: 8, disabled: true}
	cache := &usagePolicyAuthCacheStub{}
	svc := NewUsagePolicyService(repo, usagePolicyEnabledSettings(), cache)

	msg := "your prompt was flagged as potentially violating our usage policy"
	svc.ObserveErrorLogs(context.Background(), []*OpsInsertErrorLogInput{{
		UserID:       &userID,
		APIKeyID:     &keyID,
		RequestID:    "req-admin",
		ErrorMessage: msg,
	}})

	require.True(t, repo.disposition.autoBanned)
	require.Equal(t, int64(1), repo.lastUserID)
	require.Equal(t, int64(1), cache.userID)
}

func TestUsagePolicyObserveIgnoresUnrelatedErrors(t *testing.T) {
	userID := int64(7)
	repo := &usagePolicyRepoStub{insertedID: 1, inserted: true, count: 1, disabled: true}
	svc := NewUsagePolicyService(repo, usagePolicyEnabledSettings(), &usagePolicyAuthCacheStub{})

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
	}}, nil)

	msg := "flagged as potentially violating our usage policy"
	svc.ObserveErrorLogs(context.Background(), []*OpsInsertErrorLogInput{{
		UserID:       &userID,
		ErrorMessage: msg,
	}})
	require.Nil(t, repo.last)
}

func TestUsagePolicyGetStats(t *testing.T) {
	keyA := int64(10)
	keyB := int64(11)
	repo := &usagePolicyRepoStub{keys: []UsagePolicyKeyStat{
		{UserID: 2, Email: "a@example.com", APIKeyID: &keyA, APIKeyStatus: StatusDisabled, Count: 4, AutoBanned: true},
		{UserID: 3, Email: "b@example.com", APIKeyID: &keyB, APIKeyStatus: StatusActive, Count: 1, AutoBanned: false},
	}}
	svc := NewUsagePolicyService(repo, usagePolicyEnabledSettings(), nil)
	stats, err := svc.GetStats(context.Background())
	require.NoError(t, err)
	require.Equal(t, 5, stats.Total)
	require.Equal(t, 2, stats.UniqueUsers)
	require.Equal(t, 2, stats.UniqueKeys)
	require.Equal(t, 1, stats.DisabledKeys)
	require.Equal(t, 1, stats.AutoBannedKeys)
}

func TestUsagePolicyUpdateConfig(t *testing.T) {
	settings := usagePolicyEnabledSettings()
	svc := NewUsagePolicyService(nil, settings, nil)
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

type usagePolicyArchiverStub struct {
	calls []CyberPolicyRecordInput
}

func (s *usagePolicyArchiverStub) RecordUsagePolicyConversation(_ context.Context, in CyberPolicyRecordInput) {
	s.calls = append(s.calls, in)
}

func TestUsagePolicyArchiveConversation(t *testing.T) {
	archiver := &usagePolicyArchiverStub{}
	svc := NewUsagePolicyService(nil, usagePolicyEnabledSettings(), nil)
	svc.SetConversationArchiver(archiver)
	msg := "flagged as potentially violating our usage policy"
	userID := int64(9)
	keyID := int64(4)
	svc.ArchiveConversation(context.Background(), &OpsInsertErrorLogInput{
		UserID:       &userID,
		APIKeyID:     &keyID,
		RequestID:    "req-archive",
		ErrorMessage: msg,
		Model:        "gpt-5.6-luna",
	}, ContentModerationCheckInput{
		UserEmail:  "a@example.com",
		RawRequest: ContentModerationRawRequest{Body: []byte(`{"input":"hi"}`)},
	})
	require.Len(t, archiver.calls, 1)
	require.Equal(t, int64(9), archiver.calls[0].UserID)
	require.Equal(t, int64(4), archiver.calls[0].APIKeyID)
	require.Equal(t, []byte(`{"input":"hi"}`), archiver.calls[0].RawRequest.Body)
}

func TestUsagePolicyInsertFailureDoesNotBan(t *testing.T) {
	userID := int64(9)
	repo := &usagePolicyRepoStub{insertErr: errors.New("db down"), inserted: true, count: 1, disabled: true}
	svc := NewUsagePolicyService(repo, usagePolicyEnabledSettings(), &usagePolicyAuthCacheStub{})
	msg := "flagged as potentially violating our usage policy"
	svc.ObserveErrorLogs(context.Background(), []*OpsInsertErrorLogInput{{
		UserID:       &userID,
		ErrorMessage: msg,
	}})
	require.Equal(t, int64(0), repo.disposition.id)
}
