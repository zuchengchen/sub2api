package service

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

// cyberOrderingTestRepo records the sequence of repo calls to verify F7 ordering.
type cyberOrderingTestRepo struct {
	mu         sync.Mutex
	calls      []string
	emailSents []bool // EmailSent value captured at each CreateLog call
}

func (r *cyberOrderingTestRepo) CreateLog(ctx context.Context, log *ContentModerationLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "create")
	if log != nil {
		r.emailSents = append(r.emailSents, log.EmailSent)
		log.ID = 1 // simulate the database-assigned ID used by later delivery claims
	}
	return nil
}

func (r *cyberOrderingTestRepo) CreateLogWithArchive(ctx context.Context, log *ContentModerationLog, archive *ContentModerationEncryptedArchive) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "archive")
	if log != nil {
		r.emailSents = append(r.emailSents, log.EmailSent)
		log.ID = 1
	}
	return nil
}

func (r *cyberOrderingTestRepo) CreateContentLostLog(ctx context.Context, log *ContentModerationLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "content_lost")
	return nil
}

func (r *cyberOrderingTestRepo) GetArchive(context.Context, int64) (*ContentModerationLog, *ContentModerationEncryptedArchive, error) {
	return nil, nil, sql.ErrNoRows
}

func (r *cyberOrderingTestRepo) DeleteArchive(context.Context, ContentModerationArchiveAccess) (bool, error) {
	return false, nil
}

func (r *cyberOrderingTestRepo) RecordArchiveAccess(context.Context, ContentModerationArchiveAccess) error {
	return nil
}

func (r *cyberOrderingTestRepo) ReferencedArchiveKeyIDs(context.Context) ([]string, error) {
	return nil, nil
}

func (r *cyberOrderingTestRepo) ListLogs(ctx context.Context, filter ContentModerationLogFilter) ([]ContentModerationLog, *pagination.PaginationResult, error) {
	return nil, nil, nil
}

func (r *cyberOrderingTestRepo) CountFlaggedByUserSince(ctx context.Context, userID int64, since time.Time, excludeCyberPolicy bool) (int, error) {
	return 0, nil
}

func (r *cyberOrderingTestRepo) CleanupExpiredLogs(ctx context.Context, hitBefore time.Time, nonHitBefore time.Time) (*ContentModerationCleanupResult, error) {
	return &ContentModerationCleanupResult{}, nil
}

func (r *cyberOrderingTestRepo) DisableUserIfActive(context.Context, int64) (bool, error) {
	r.mu.Lock()
	r.calls = append(r.calls, "disable_user")
	r.mu.Unlock()
	return true, nil
}

func (r *cyberOrderingTestRepo) DisableAPIKeyIfActive(context.Context, int64) (string, bool, error) {
	r.mu.Lock()
	r.calls = append(r.calls, "disable_api_key")
	r.mu.Unlock()
	return "", false, nil
}

func (r *cyberOrderingTestRepo) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

func (r *cyberOrderingTestRepo) snapshotEmailSents() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]bool, len(r.emailSents))
	copy(out, r.emailSents)
	return out
}

type cyberDispositionTestRepo struct {
	contentModerationTestRepo
	dispositionMu    sync.Mutex
	userActive       bool
	keyActive        bool
	keyCredential    string
	disableUserCalls int
	disableKeyCalls  int
}

func (r *cyberDispositionTestRepo) DisableUserIfActive(context.Context, int64) (bool, error) {
	r.dispositionMu.Lock()
	defer r.dispositionMu.Unlock()
	r.disableUserCalls++
	if !r.userActive {
		return false, nil
	}
	r.userActive = false
	return true, nil
}

func (r *cyberDispositionTestRepo) DisableAPIKeyIfActive(context.Context, int64) (string, bool, error) {
	r.dispositionMu.Lock()
	defer r.dispositionMu.Unlock()
	r.disableKeyCalls++
	if !r.keyActive {
		return "", false, nil
	}
	r.keyActive = false
	return r.keyCredential, true, nil
}

func gptCyberScope() *ContentModerationScopeSnapshot {
	scope := NewContentModerationScopeSnapshot(nil, "  gPt-production")
	return &scope
}

func TestRecordCyberPolicyEvent_RiskControlOffStillForcesUserDisposition(t *testing.T) {
	repo := &cyberDispositionTestRepo{userActive: true}
	invalidator := &contentModerationTestAuthCacheInvalidator{}
	svc := NewContentModerationService(
		&contentModerationTestSettingRepo{values: map[string]string{
			SettingKeyRiskControlEnabled: "false",
		}},
		repo,
		nil,
		nil,
		nil,
		nil,
		invalidator,
		nil,
	)

	svc.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{
		UserID:          1,
		UserEmail:       "u@x.com",
		Model:           "gpt-5",
		Endpoint:        "/v1/responses",
		UpstreamMessage: "flagged",
		UpstreamBody:    `{"error":{"code":"cyber_policy"}}`,
		UpstreamStatus:  400,
		Scope:           gptCyberScope(),
		UserRole:        RoleUser,
	})

	logs := repo.snapshotLogs()
	require.Len(t, logs, 1)
	require.Equal(t, "disabled", logs[0].DispositionStatus)
	require.Equal(t, "user", logs[0].DispositionTarget)
	require.True(t, logs[0].DispositionTransitioned)
	require.True(t, logs[0].AutoBanned)
	require.Equal(t, []int64{1}, invalidator.userIDs)
}

func TestRecordCyberPolicyEvent_NonGPTSkipsAllSideEffects(t *testing.T) {
	repo := &cyberDispositionTestRepo{userActive: true}
	svc := NewContentModerationService(
		&contentModerationTestSettingRepo{values: map[string]string{
			SettingKeyRiskControlEnabled: "true",
		}},
		repo,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	scope := NewContentModerationScopeSnapshot(nil, "claude")

	svc.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{
		UserID:          1,
		UserEmail:       "u@x.com",
		Model:           "gpt-5",
		Endpoint:        "/v1/responses",
		UpstreamMessage: "flagged",
		UpstreamBody:    `{"error":{"code":"cyber_policy"}}`,
		UpstreamStatus:  400,
		Scope:           &scope,
		UserRole:        RoleUser,
	})
	require.Empty(t, repo.snapshotLogs())
	require.Equal(t, 0, repo.disableUserCalls)
}

func TestRecordCyberPolicyEvent_WritesLogAndDisablesUserOnce(t *testing.T) {
	repo := &cyberDispositionTestRepo{userActive: true}
	invalidator := &contentModerationTestAuthCacheInvalidator{}
	svc := NewContentModerationService(
		&contentModerationTestSettingRepo{values: map[string]string{SettingKeyRiskControlEnabled: "true"}},
		repo, nil, nil, nil, nil, invalidator, nil,
	)
	input := CyberPolicyRecordInput{
		UserID: 1, UserEmail: "u@x.com", Model: "gpt-5", Endpoint: "/v1/responses",
		UpstreamMessage: "flagged", UpstreamBody: `{"error":{"code":"cyber_policy"}}`,
		UpstreamStatus: 400, Scope: gptCyberScope(), UserRole: RoleUser,
	}
	svc.RecordCyberPolicyEvent(context.Background(), input)
	svc.RecordCyberPolicyEvent(context.Background(), input)

	logs := repo.snapshotLogs()
	require.Len(t, logs, 2)
	log := logs[0]

	require.Equal(t, "cyber_policy", log.Action)
	require.True(t, log.Flagged)
	require.Equal(t, "cyber_policy", log.HighestCategory)
	require.Contains(t, log.Error, "flagged")
	require.True(t, log.AutoBanned)
	// emailService is nil, so EmailSent must be false
	require.False(t, log.EmailSent)

	// UserID pointer must be set
	require.NotNil(t, log.UserID)
	require.Equal(t, int64(1), *log.UserID)

	// score for cyber_policy is always 1.0
	require.Equal(t, 1.0, log.HighestScore)

	// mode must be post_upstream
	require.Equal(t, "post_upstream", log.Mode)

	// provider
	require.Equal(t, "openai", log.Provider)

	// model
	require.Equal(t, "gpt-5", log.Model)

	// endpoint
	require.Equal(t, "/v1/responses", log.Endpoint)

	require.Equal(t, 0, log.ViolationCount, "upstream cyber disposition is independent of the local violation window")
	require.Equal(t, "disabled", log.DispositionStatus)
	require.Equal(t, "already_disabled", logs[1].DispositionStatus)
	require.Equal(t, []int64{1}, invalidator.userIDs, "auth cache is invalidated only for the winning transition")

	// Error field should also contain the upstream body JSON
	require.True(t, strings.Contains(log.Error, "cyber_policy") || strings.Contains(log.Error, "flagged"),
		"Error should mention flagged or cyber_policy")
}

func TestRecordCyberPolicyEvent_AdminDisablesOnlyTriggeringAPIKey(t *testing.T) {
	repo := &cyberDispositionTestRepo{userActive: true, keyActive: true, keyCredential: "sk-triggering"}
	invalidator := &contentModerationTestAuthCacheInvalidator{}
	svc := NewContentModerationService(
		&contentModerationTestSettingRepo{values: map[string]string{}},
		repo, nil, nil, nil, nil, invalidator, nil,
	)
	svc.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{
		UserID: 7, APIKeyID: 42, UserRole: RoleAdmin, Scope: gptCyberScope(),
		Model: "gpt-5", Endpoint: "/v1/responses", UpstreamMessage: "blocked",
	})

	logs := repo.snapshotLogs()
	require.Len(t, logs, 1)
	require.Equal(t, "api_key", logs[0].DispositionTarget)
	require.Equal(t, "disabled", logs[0].DispositionStatus)
	require.Equal(t, 0, repo.disableUserCalls)
	require.Equal(t, 1, repo.disableKeyCalls)
	require.Empty(t, invalidator.userIDs)
	require.Equal(t, []string{"sk-triggering"}, invalidator.keys)
}

func TestRecordCyberPolicyEvent_RuntimeSnapshotLoadFailureStillRecordsEvent(t *testing.T) {
	repo := &banCountArgsTestRepo{}
	settingRepo := &contentModerationRuntimeSettingRepo{values: map[string]string{
		SettingKeyRiskControlEnabled:      "true",
		SettingKeyContentModerationConfig: `{invalid`,
	}}
	svc := NewContentModerationService(settingRepo, repo, nil, nil, nil, nil, nil, nil)

	svc.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{
		UserID: 1,
		Model:  "gpt-5",
	})

	require.Len(t, repo.snapshotLogs(), 1)
	getValue, getMultiple := settingRepo.calls()
	require.Zero(t, getValue)
	require.GreaterOrEqual(t, getMultiple, 1)
}

func TestRecordCyberPolicyEvent_RuntimeSnapshotRefreshFailureKeepsStaleScope(t *testing.T) {
	repo := &banCountArgsTestRepo{}
	settingRepo := &contentModerationRuntimeSettingRepo{values: map[string]string{
		SettingKeyRiskControlEnabled:      "true",
		SettingKeyContentModerationConfig: `{"all_groups":true,"model_filter":{"type":"include","models":["gpt-5"]}}`,
	}}
	svc := NewContentModerationService(settingRepo, repo, nil, nil, nil, nil, nil, nil)
	svc.runtimeCacheTTL = time.Minute

	_, err := svc.loadRuntimeSnapshot(context.Background())
	require.NoError(t, err)
	current := svc.runtimeSnapshot.Load()
	require.NotNil(t, current)
	expired := *current
	expired.loadedAt = time.Now().Add(-2 * time.Minute)
	svc.runtimeSnapshot.Store(&expired)
	settingRepo.failMultiple(errors.New("database unavailable"))

	svc.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{
		UserID: 1,
		Model:  "gpt-5",
	})

	require.Len(t, repo.snapshotLogs(), 1)
	require.Eventually(t, func() bool {
		_, calls := settingRepo.calls()
		return calls == 2
	}, time.Second, time.Millisecond)
	getValue, getMultiple := settingRepo.calls()
	require.Zero(t, getValue)
	require.Equal(t, 2, getMultiple)
}

// TestRecordCyberPolicyEvent_DisablesBeforeLogAndEmail verifies the synchronous
// fallback path: disposition happens before audit persistence, and persistence
// happens before any email-delivery claim.
//
// Note on email ordering: EmailService is a concrete type with no injectable
// send interface, so SMTP-success cannot be simulated in unit tests.
// With emailService=nil the email block is skipped. The test therefore asserts
// the two invariants that are observable without real SMTP:
//  1. DisableUserIfActive runs before CreateLog.
//  2. The log is stored with EmailSent=false (not pre-set to true).
func TestRecordCyberPolicyEvent_DisablesBeforeLogAndEmail(t *testing.T) {
	repo := &cyberOrderingTestRepo{}
	svc := NewContentModerationService(
		&contentModerationTestSettingRepo{values: map[string]string{
			SettingKeyRiskControlEnabled: "true",
		}},
		repo,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil, // emailService=nil: email path safely skipped; see doc comment above
	)

	svc.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{
		RequestID:       "req-1",
		UserID:          7,
		UserEmail:       "u@example.com",
		Model:           "gpt-5",
		UpstreamMessage: "blocked",
		Scope:           gptCyberScope(),
		UserRole:        RoleUser,
	})

	calls := repo.snapshot()
	require.GreaterOrEqual(t, len(calls), 2, "disposition and CreateLog must both be called")
	require.Equal(t, []string{"disable_user", "create"}, calls[:2])

	// EmailSent must be false when the log is first persisted; a later atomic
	// claim owns the sole SMTP attempt.
	emailSents := repo.snapshotEmailSents()
	require.NotEmpty(t, emailSents, "CreateLog must have captured EmailSent value")
	require.False(t, emailSents[0], "log must be stored with EmailSent=false initially (F7)")

}

func TestRecordCyberPolicyEvent_DisablesBeforeArchive(t *testing.T) {
	repo := &cyberOrderingTestRepo{}
	svc := NewContentModerationService(
		&contentModerationTestSettingRepo{values: map[string]string{}},
		repo, nil, nil, nil, nil, nil, nil,
	)
	root := t.TempDir()
	keyRingPath := filepath.Join(root, "keyring.json")
	writeModerationArchiveTestKeyRing(t, keyRingPath, "k1", map[string][]byte{
		"k1": []byte("0123456789abcdef0123456789abcdef"),
	})
	runtime, err := newContentModerationArchiveRuntime(repo, moderationArchiveRuntimeOptions(root, keyRingPath))
	require.NoError(t, err)
	t.Cleanup(runtime.Close)
	svc.archiveRuntime = runtime

	svc.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{
		RequestID: "req-archive-order", UserID: 7, UserRole: RoleUser,
		Model: "gpt-5", Scope: gptCyberScope(),
		RawRequest: ContentModerationRawRequest{Body: []byte(`{"input":"blocked"}`)},
	})

	calls := repo.snapshot()
	require.GreaterOrEqual(t, len(calls), 2, "disposition and archive must both be called")
	require.Equal(t, []string{"disable_user", "archive"}, calls[:2])
}

// banCountArgsTestRepo 在 contentModerationTestRepo 基础上记录
// CountFlaggedByUserSince 收到的 excludeCyberPolicy 参数，供透传断言。
type banCountArgsTestRepo struct {
	contentModerationTestRepo
	argsMu     sync.Mutex
	countCalls []bool
}

func (r *banCountArgsTestRepo) CountFlaggedByUserSince(ctx context.Context, userID int64, since time.Time, excludeCyberPolicy bool) (int, error) {
	r.argsMu.Lock()
	r.countCalls = append(r.countCalls, excludeCyberPolicy)
	r.argsMu.Unlock()
	return r.contentModerationTestRepo.CountFlaggedByUserSince(ctx, userID, since, excludeCyberPolicy)
}

func (r *banCountArgsTestRepo) snapshotCountCalls() []bool {
	r.argsMu.Lock()
	defer r.argsMu.Unlock()
	out := make([]bool, len(r.countCalls))
	copy(out, r.countCalls)
	return out
}

func TestApplyFlaggedAccountSideEffects_PassesExcludeCyberFlag(t *testing.T) {
	repo := &banCountArgsTestRepo{}
	svc := NewContentModerationService(
		&contentModerationTestSettingRepo{values: map[string]string{}},
		repo, nil, nil, nil, nil, nil, nil,
	)
	userID := int64(42)

	cfgExclude := defaultContentModerationConfig()
	cfgExclude.CyberPolicyExcludeFromBanCount = true
	svc.applyFlaggedAccountSideEffects(context.Background(), cfgExclude, &ContentModerationLog{Flagged: true, UserID: &userID})

	cfgDefault := defaultContentModerationConfig() // 默认 false
	svc.applyFlaggedAccountSideEffects(context.Background(), cfgDefault, &ContentModerationLog{Flagged: true, UserID: &userID})

	require.Equal(t, []bool{true, false}, repo.snapshotCountCalls(),
		"applyFlaggedAccountSideEffects 必须把 cfg.CyberPolicyExcludeFromBanCount 透传给 COUNT 查询")
}

func TestRecordCyberPolicyEvent_AutoBanAndCountConfigDoNotControlCyberDisposition(t *testing.T) {
	repo := &cyberDispositionTestRepo{userActive: true}
	svc := NewContentModerationService(
		&contentModerationTestSettingRepo{values: map[string]string{
			SettingKeyRiskControlEnabled:      "true",
			SettingKeyContentModerationConfig: `{"cyber_policy_exclude_from_ban_count":true,"auto_ban_enabled":false}`,
		}},
		repo, nil, nil, nil, nil, nil, nil,
	)

	svc.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{
		UserID:          1,
		UserEmail:       "u@x.com",
		Model:           "gpt-5",
		Endpoint:        "/v1/responses",
		UpstreamMessage: "flagged",
		UpstreamStatus:  400,
		Scope:           gptCyberScope(),
		UserRole:        RoleUser,
	})

	logs := repo.snapshotLogs()
	require.Len(t, logs, 1)
	require.True(t, logs[0].Flagged)
	require.Equal(t, "cyber_policy", logs[0].Action)
	require.Equal(t, "disabled", logs[0].DispositionStatus)
	require.True(t, logs[0].DispositionTransitioned)
}
