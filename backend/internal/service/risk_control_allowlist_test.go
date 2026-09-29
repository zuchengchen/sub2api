package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type failingAllowlistRepo struct {
	fakeSettingRepo
	failKey string
}

func (r *failingAllowlistRepo) GetValue(ctx context.Context, key string) (string, error) {
	if key == r.failKey || r.failKey == "all" {
		return "", errors.New("database unavailable")
	}
	return r.fakeSettingRepo.GetValue(ctx, key)
}

func TestRiskControlAllowlistRetainsLastGoodValueAndRetries(t *testing.T) {
	for _, failKey := range []string{SettingKeyCyberPolicyUserAllowlist, "all"} {
		t.Run(failKey, func(t *testing.T) {
			repo := &failingAllowlistRepo{fakeSettingRepo: fakeSettingRepo{vals: map[string]string{SettingKeyCyberPolicyUserAllowlist: "12"}}}
			svc := &SettingService{settingRepo: repo}
			ctx := context.Background()
			require.True(t, svc.IsCyberPolicyUserAllowlisted(ctx, 12))
			expire := func() {
				cached, ok := svc.cyberSessionBlockRuntimeCache.Load().(*cachedCyberSessionBlockRuntime)
				require.True(t, ok)
				old := *cached
				old.expiresAt = 0
				svc.cyberSessionBlockRuntimeCache.Store(&old)
			}
			expire()
			repo.failKey = failKey
			require.True(t, svc.IsCyberPolicyUserAllowlisted(ctx, 12))
			cached, ok := svc.cyberSessionBlockRuntimeCache.Load().(*cachedCyberSessionBlockRuntime)
			require.True(t, ok)
			require.LessOrEqual(t, time.Until(time.Unix(0, cached.expiresAt)), cyberSessionBlockRuntimeErrorTTL)
			repo.failKey = ""
			repo.vals[SettingKeyCyberPolicyUserAllowlist] = ""
			expire()
			require.False(t, svc.IsCyberPolicyUserAllowlisted(ctx, 12), "successful removal must replace stale membership")
		})
	}
}

func TestRiskControlAllowlistAuditsWithoutLocalPenalties(t *testing.T) {
	cfg := defaultContentModerationConfig()
	cfg.Enabled, cfg.AutoBanEnabled, cfg.EmailOnHit = true, true, true
	cfg.BanThreshold = 1
	cfg.Mode = ContentModerationModePreBlock
	cfg.FirstLayerStage = ContentModerationFirstLayerStageEnforce
	cfg.SecondLayerEnabled = false
	cfg.HardBlockPatterns = []string{"definitely dangerous operation"}
	cfg.normalize()
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	for _, tc := range []struct {
		name        string
		allowlist   string
		wantLogOnly bool
	}{
		{name: "allowlisted", allowlist: "91", wantLogOnly: true},
		{name: "not_allowlisted", allowlist: "34", wantLogOnly: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &contentModerationReplayRepo{}
			userRepo := &contentModerationTestUserRepo{user: &User{ID: 91, Role: RoleUser, Status: StatusActive}}
			svc := NewContentModerationService(&contentModerationTestSettingRepo{values: map[string]string{
				SettingKeyRiskControlEnabled: "true", SettingKeyContentModerationConfig: string(raw), SettingKeyCyberPolicyUserAllowlist: tc.allowlist,
			}}, repo, &contentModerationReplayCache{}, nil, userRepo, nil, nil, nil)
			scope := NewContentModerationScopeSnapshot(nil, "gpt-allowlist")
			decision, err := svc.Check(context.Background(), ContentModerationCheckInput{
				RequestID: "allowlist", UserID: 91, UserEmail: "trusted@example.com", UserRole: RoleUser, Scope: &scope,
				Protocol: ContentModerationProtocolOpenAIResponses,
				Body:     []byte(`{"input":"definitely dangerous operation"}`),
			})
			require.NoError(t, err)
			logs := repo.snapshotLogs()
			require.Len(t, logs, 1)
			require.True(t, logs[0].Flagged, "evidence must still be recorded")
			if !tc.wantLogOnly {
				require.True(t, decision.Blocked)
				require.NotEqual(t, ContentModerationModeRiskControlLogOnly, logs[0].Mode)
				return
			}
			require.True(t, decision.Allowed)
			require.False(t, decision.Blocked)
			require.Equal(t, ContentModerationActionAllow, decision.Action)
			require.Equal(t, ContentModerationModeRiskControlLogOnly, logs[0].Mode)
			require.False(t, logs[0].AutoBanned)
			require.False(t, logs[0].EmailSent)
			require.Equal(t, StatusActive, userRepo.user.Status)
		})
	}
}

type failingModerationAllowlistRepo struct {
	contentModerationTestSettingRepo
	fail bool
}

func (r *failingModerationAllowlistRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	if r.fail {
		return nil, errors.New("database unavailable")
	}
	return r.contentModerationTestSettingRepo.GetMultiple(ctx, keys)
}

func TestRiskControlAllowlistSnapshotRetainsMembershipOnFailure(t *testing.T) {
	repo := &failingModerationAllowlistRepo{contentModerationTestSettingRepo: contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyRiskControlEnabled: "true", SettingKeyCyberPolicyUserAllowlist: "12",
	}}}
	svc := &ContentModerationService{settingRepo: repo}
	ctx := context.Background()
	_, err := svc.refreshRuntimeSnapshot(ctx)
	require.NoError(t, err)
	repo.fail = true
	_, err = svc.refreshRuntimeSnapshot(ctx)
	require.Error(t, err)
	require.Contains(t, svc.runtimeSnapshot.Load().allowlistedUsers, int64(12))
	repo.fail = false
	repo.values[SettingKeyCyberPolicyUserAllowlist] = "34"
	_, err = svc.refreshRuntimeSnapshot(ctx)
	require.NoError(t, err)
	require.NotContains(t, svc.runtimeSnapshot.Load().allowlistedUsers, int64(12))
	require.Contains(t, svc.runtimeSnapshot.Load().allowlistedUsers, int64(34))
}
