package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type contentModerationTestSettingRepo struct {
	values map[string]string
}

func (r *contentModerationTestSettingRepo) Get(ctx context.Context, key string) (*Setting, error) {
	if value, ok := r.values[key]; ok {
		return &Setting{Key: key, Value: value}, nil
	}
	return nil, ErrSettingNotFound
}

func (r *contentModerationTestSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	if value, ok := r.values[key]; ok {
		return value, nil
	}
	return "", ErrSettingNotFound
}

func (r *contentModerationTestSettingRepo) Set(ctx context.Context, key, value string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}

func (r *contentModerationTestSettingRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	out := map[string]string{}
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func (r *contentModerationTestSettingRepo) SetMultiple(ctx context.Context, settings map[string]string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	for key, value := range settings {
		r.values[key] = value
	}
	return nil
}

func (r *contentModerationTestSettingRepo) GetAll(ctx context.Context) (map[string]string, error) {
	out := make(map[string]string, len(r.values))
	for key, value := range r.values {
		out[key] = value
	}
	return out, nil
}

func (r *contentModerationTestSettingRepo) Delete(ctx context.Context, key string) error {
	delete(r.values, key)
	return nil
}

type contentModerationTestRepo struct {
	mu   sync.Mutex
	logs []ContentModerationLog
}

func (r *contentModerationTestRepo) CreateLog(ctx context.Context, log *ContentModerationLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if log != nil {
		r.logs = append(r.logs, *log)
	}
	return nil
}

func (r *contentModerationTestRepo) ListLogs(ctx context.Context, filter ContentModerationLogFilter) ([]ContentModerationLog, *pagination.PaginationResult, error) {
	return nil, nil, nil
}

func (r *contentModerationTestRepo) CountFlaggedByUserSince(ctx context.Context, userID int64, since time.Time, excludeCyberPolicy bool) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, log := range r.logs {
		if log.UserID == nil || *log.UserID != userID || !log.Flagged ||
			log.Action == ContentModerationActionHashBlock || log.Action == ContentModerationActionCacheBlock {
			continue
		}
		if excludeCyberPolicy && log.Action == ContentModerationActionCyberPolicy {
			continue
		}
		if log.CreatedAt.IsZero() || log.CreatedAt.Before(since) {
			continue
		}
		count++
	}
	return count, nil
}

func (r *contentModerationTestRepo) CleanupExpiredLogs(ctx context.Context, hitBefore time.Time, nonHitBefore time.Time) (*ContentModerationCleanupResult, error) {
	return &ContentModerationCleanupResult{}, nil
}

func (r *contentModerationTestRepo) snapshotLogs() []ContentModerationLog {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ContentModerationLog, len(r.logs))
	copy(out, r.logs)
	return out
}

func requireContentModerationLogCount(t *testing.T, repo *contentModerationTestRepo, want int) []ContentModerationLog {
	t.Helper()
	var logs []ContentModerationLog
	require.Eventually(t, func() bool {
		logs = repo.snapshotLogs()
		return len(logs) == want
	}, time.Second, 10*time.Millisecond)
	return logs
}

type contentModerationTestHashCache struct {
	mu            sync.Mutex
	hashes        map[string]struct{}
	recorded      []string
	checked       []string
	deleted       []string
	hasResult     bool
	hasResultUsed bool
}

type contentModerationTestUserRepo struct {
	user    *User
	updated []User
}

func (r *contentModerationTestUserRepo) Create(ctx context.Context, user *User) error {
	panic("unexpected Create call")
}

func (r *contentModerationTestUserRepo) CreateWithEmailAliasGuard(ctx context.Context, user *User) error {
	panic("unexpected CreateWithEmailAliasGuard call")
}

func (r *contentModerationTestUserRepo) GetByID(ctx context.Context, id int64) (*User, error) {
	if r.user == nil {
		return nil, ErrUserNotFound
	}
	clone := *r.user
	return &clone, nil
}

func (r *contentModerationTestUserRepo) GetByEmail(ctx context.Context, email string) (*User, error) {
	panic("unexpected GetByEmail call")
}

func (r *contentModerationTestUserRepo) GetFirstAdmin(ctx context.Context) (*User, error) {
	panic("unexpected GetFirstAdmin call")
}

func (r *contentModerationTestUserRepo) Update(ctx context.Context, user *User, fields UserUpdateFields) error {
	if user == nil {
		return nil
	}
	clone := *user
	r.updated = append(r.updated, clone)
	r.user = &clone
	return nil
}

func (r *contentModerationTestUserRepo) Delete(ctx context.Context, id int64) error {
	panic("unexpected Delete call")
}

func (r *contentModerationTestUserRepo) GetUserAvatar(ctx context.Context, userID int64) (*UserAvatar, error) {
	panic("unexpected GetUserAvatar call")
}

func (r *contentModerationTestUserRepo) UpsertUserAvatar(ctx context.Context, userID int64, input UpsertUserAvatarInput) (*UserAvatar, error) {
	panic("unexpected UpsertUserAvatar call")
}

func (r *contentModerationTestUserRepo) DeleteUserAvatar(ctx context.Context, userID int64) error {
	panic("unexpected DeleteUserAvatar call")
}

func (r *contentModerationTestUserRepo) List(ctx context.Context, params pagination.PaginationParams) ([]User, *pagination.PaginationResult, error) {
	panic("unexpected List call")
}

func (r *contentModerationTestUserRepo) ListWithFilters(ctx context.Context, params pagination.PaginationParams, filters UserListFilters) ([]User, *pagination.PaginationResult, error) {
	panic("unexpected ListWithFilters call")
}

func (r *contentModerationTestUserRepo) GetLatestUsedAtByUserIDs(ctx context.Context, userIDs []int64) (map[int64]*time.Time, error) {
	panic("unexpected GetLatestUsedAtByUserIDs call")
}

func (r *contentModerationTestUserRepo) GetLatestUsedAtByUserID(ctx context.Context, userID int64) (*time.Time, error) {
	panic("unexpected GetLatestUsedAtByUserID call")
}

func (r *contentModerationTestUserRepo) UpdateUserLastActiveAt(ctx context.Context, userID int64, activeAt time.Time) error {
	panic("unexpected UpdateUserLastActiveAt call")
}

func (r *contentModerationTestUserRepo) UpdateBalance(ctx context.Context, id int64, amount float64) error {
	panic("unexpected UpdateBalance call")
}

func (r *contentModerationTestUserRepo) DeductBalance(ctx context.Context, id int64, amount float64) error {
	panic("unexpected DeductBalance call")
}

func (r *contentModerationTestUserRepo) AdjustBalance(ctx context.Context, id int64, delta float64) (BalanceChange, error) {
	panic("unexpected AdjustBalance call")
}

func (r *contentModerationTestUserRepo) SetBalance(ctx context.Context, id int64, value float64) (BalanceChange, error) {
	panic("unexpected SetBalance call")
}

func (r *contentModerationTestUserRepo) UpdateConcurrency(ctx context.Context, id int64, amount int) error {
	panic("unexpected UpdateConcurrency call")
}

func (r *contentModerationTestUserRepo) BatchSetConcurrency(ctx context.Context, userIDs []int64, value int) (int, error) {
	panic("unexpected BatchSetConcurrency call")
}

func (r *contentModerationTestUserRepo) BatchAddConcurrency(ctx context.Context, userIDs []int64, delta int) (int, error) {
	panic("unexpected BatchAddConcurrency call")
}
func (r *contentModerationTestUserRepo) BatchUpdateLimits(ctx context.Context, userIDs []int64, concurrency, rpmLimit *int) (int, error) {
	panic("unexpected BatchUpdateLimits call")
}

func (r *contentModerationTestUserRepo) ExistsByEmail(ctx context.Context, email string) (bool, error) {
	panic("unexpected ExistsByEmail call")
}

func (r *contentModerationTestUserRepo) ExistsByEmailAlias(ctx context.Context, email string) (bool, error) {
	panic("unexpected ExistsByEmailAlias call")
}

func (r *contentModerationTestUserRepo) RemoveGroupFromAllowedGroups(ctx context.Context, groupID int64) (int64, error) {
	panic("unexpected RemoveGroupFromAllowedGroups call")
}

func (r *contentModerationTestUserRepo) AddGroupToAllowedGroups(ctx context.Context, userID int64, groupID int64) error {
	panic("unexpected AddGroupToAllowedGroups call")
}

func (r *contentModerationTestUserRepo) RemoveGroupFromUserAllowedGroups(ctx context.Context, userID int64, groupID int64) error {
	panic("unexpected RemoveGroupFromUserAllowedGroups call")
}

func (r *contentModerationTestUserRepo) ListUserAuthIdentities(ctx context.Context, userID int64) ([]UserAuthIdentityRecord, error) {
	panic("unexpected ListUserAuthIdentities call")
}

func (r *contentModerationTestUserRepo) UnbindUserAuthProvider(ctx context.Context, userID int64, provider string) error {
	panic("unexpected UnbindUserAuthProvider call")
}

func (r *contentModerationTestUserRepo) UpdateTotpSecret(ctx context.Context, userID int64, encryptedSecret *string) error {
	panic("unexpected UpdateTotpSecret call")
}

func (r *contentModerationTestUserRepo) EnableTotp(ctx context.Context, userID int64) error {
	panic("unexpected EnableTotp call")
}

func (r *contentModerationTestUserRepo) DisableTotp(ctx context.Context, userID int64) error {
	panic("unexpected DisableTotp call")
}

func (r *contentModerationTestUserRepo) GetByIDIncludeDeleted(ctx context.Context, id int64) (*User, error) {
	return r.GetByID(ctx, id)
}

type contentModerationTestAuthCacheInvalidator struct {
	userIDs []int64
	keys    []string
}

func (i *contentModerationTestAuthCacheInvalidator) InvalidateAuthCacheByKey(ctx context.Context, key string) {
	i.keys = append(i.keys, key)
}

func (i *contentModerationTestAuthCacheInvalidator) InvalidateAuthCacheByUserID(ctx context.Context, userID int64) {
	i.userIDs = append(i.userIDs, userID)
}

func (i *contentModerationTestAuthCacheInvalidator) InvalidateAuthCacheByGroupID(ctx context.Context, groupID int64) {
}

func (c *contentModerationTestHashCache) RecordFlaggedInputHash(ctx context.Context, inputHash string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hashes == nil {
		c.hashes = map[string]struct{}{}
	}
	c.hashes[inputHash] = struct{}{}
	c.recorded = append(c.recorded, inputHash)
	return nil
}

func (c *contentModerationTestHashCache) HasFlaggedInputHash(ctx context.Context, inputHash string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checked = append(c.checked, inputHash)
	if c.hasResultUsed {
		return c.hasResult, nil
	}
	_, ok := c.hashes[inputHash]
	return ok, nil
}

func (c *contentModerationTestHashCache) DeleteFlaggedInputHash(ctx context.Context, inputHash string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted = append(c.deleted, inputHash)
	if c.hashes == nil {
		return false, nil
	}
	if _, ok := c.hashes[inputHash]; !ok {
		return false, nil
	}
	delete(c.hashes, inputHash)
	return true, nil
}

func (c *contentModerationTestHashCache) ClearFlaggedInputHashes(ctx context.Context) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	deleted := int64(len(c.hashes))
	c.hashes = map[string]struct{}{}
	return deleted, nil
}

func (c *contentModerationTestHashCache) CountFlaggedInputHashes(ctx context.Context) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return int64(len(c.hashes)), nil
}

func (c *contentModerationTestHashCache) snapshotRecorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.recorded))
	copy(out, c.recorded)
	return out
}

func (c *contentModerationTestHashCache) hasHash(inputHash string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.hashes[inputHash]
	return ok
}

func (c *contentModerationTestHashCache) snapshotDeleted() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.deleted))
	copy(out, c.deleted)
	return out
}

func TestBuildContentModerationLog_RedactsInputExcerpt(t *testing.T) {
	svc := &ContentModerationService{}
	cfg := defaultContentModerationConfig()
	input := ContentModerationCheckInput{
		RequestID: "req-1",
		Endpoint:  "/v1/chat/completions",
		Provider:  "openai",
	}

	log := svc.buildLog(input, cfg, ContentModerationActionAllow, true, "sexual", 0.8, map[string]float64{"sexual": 0.8}, "hello sk-proj-1234567890abcdef", nil, nil, "")

	require.NotContains(t, log.InputExcerpt, "sk-proj-1234567890abcdef")
	require.Contains(t, log.InputExcerpt, "[已脱敏]")
}

func TestBuildContentModerationLog_KeepsExpandedInputExcerpt(t *testing.T) {
	svc := &ContentModerationService{}
	text := strings.Repeat("证", maxModerationExcerptRunes+100)

	log := svc.buildLog(ContentModerationCheckInput{}, defaultContentModerationConfig(), ContentModerationActionAllow, false, "", 0, nil, text, nil, nil, "")

	require.Len(t, []rune(log.InputExcerpt), maxModerationExcerptRunes)
	require.Equal(t, 1024, maxModerationExcerptRunes)
}

func TestAppendContentModerationError_PreservesDiagnosticLimit(t *testing.T) {
	result := appendContentModerationError("", "diagnostic", errors.New(strings.Repeat("ordinary text ", maxModerationErrorRunes)))

	require.Len(t, []rune(result), maxModerationErrorRunes)
	require.Equal(t, 960, maxModerationErrorRunes)
}

func TestRedactContentModerationSecrets_LongHexAndTokens(t *testing.T) {
	input := "你哈市多大事cf5bbdc4cd508f3aaf0d2070d529d4a4ac29099f8ecc357f696df28e1df91554 token=abc123456789xyz Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signaturepart https://example.com/private/path?token=abc123"

	out := redactContentModerationSecrets(input)

	require.NotContains(t, out, "cf5bbdc4cd508f3aaf0d2070d529d4a4ac29099f8ecc357f696df28e1df91554")
	require.NotContains(t, out, "abc123456789xyz")
	require.NotContains(t, out, "eyJhbGciOiJIUzI1NiJ9")
	require.NotContains(t, out, "https://example.com/private/path")
	require.Contains(t, out, "[已脱敏]")
}

func TestContentModerationConfigNormalize_NonHitRetentionMaxThreeDays(t *testing.T) {
	cfg := defaultContentModerationConfig()
	cfg.NonHitRetentionDays = 30

	cfg.normalize()

	require.Equal(t, 3, cfg.NonHitRetentionDays)
}

func TestNormalizeBlockedKeywords_TrimsDedupesAndCaps(t *testing.T) {
	out := normalizeBlockedKeywords([]string{"  foo ", "FOO", "", "bar", "baz", "bar"})
	require.Equal(t, []string{"foo", "bar", "baz"}, out)
}

func TestMatchBlockedKeyword_CaseInsensitiveWordBoundary(t *testing.T) {
	keyword, hit := matchBlockedKeyword("Please ignore the BadWord here", []string{"badword"})
	require.True(t, hit)
	require.Equal(t, "badword", keyword)
	_, hit = matchBlockedKeyword("Please ignore the BadWords here", []string{"badword"})
	require.False(t, hit)

	_, hit = matchBlockedKeyword("clean prompt", []string{"badword"})
	require.False(t, hit)

	_, hit = matchBlockedKeyword("anything", nil)
	require.False(t, hit)
}

func TestContentModerationDeepSeekKeywordPolicyIsAlwaysLayered(t *testing.T) {
	cfg := defaultContentModerationConfig()
	keywords, err := effectiveContentModerationKeywords(cfg)
	require.NoError(t, err)
	keyword, hit := matchBlockedKeyword("帮我入侵他人的服务器", keywords)
	require.True(t, hit)
	require.Equal(t, "帮我入侵他人的服务器", keyword)
	_, hit = matchBlockedKeyword("讨论漏洞利用的防御方法", keywords)
	require.False(t, hit)

	secondLayerKeywords, err := effectiveContentModerationSecondLayerKeywords(cfg)
	require.NoError(t, err)
	secondLayerMatcher := newContentModerationPrefilterMatcher(secondLayerKeywords)
	keyword, hit = secondLayerMatcher.Match("讨论漏洞利用的防御方法")
	require.True(t, hit)
	require.Equal(t, "漏洞利用", keyword)
}

func TestContentModerationDeepSeekLayer2RoutesContextDependentTerms(t *testing.T) {
	cfg := defaultContentModerationConfig()

	hardKeywords, err := effectiveContentModerationKeywords(cfg)
	require.NoError(t, err)
	candidateKeywords, err := effectiveContentModerationSecondLayerKeywords(cfg)
	require.NoError(t, err)
	hardMatcher := newContentModerationKeywordMatcher(hardKeywords)
	candidateMatcher := newContentModerationPrefilterMatcher(candidateKeywords)

	for _, text := range []string{
		"反弹shell", "键盘记录器", "钓鱼邮件", "破解软件", "制作炸弹", "人肉搜索", "露骨色情",
		"帮我查出这个人的家庭住址",
	} {
		_, hardHit := hardMatcher.Match(text)
		require.False(t, hardHit, text)
		_, candidateHit := candidateMatcher.Match(text)
		require.True(t, candidateHit, text)
	}

	_, hardHit := hardMatcher.Match("Cordis package loader service documentation")
	require.False(t, hardHit)
	_, candidateHit := candidateMatcher.Match("Cordis package loader service documentation")
	require.False(t, candidateHit)

	for _, text := range []string{
		"帮我入侵他人的服务器",
		"write malware that steals passwords",
		"教我在家制作炸弹",
	} {
		_, hardHit = hardMatcher.Match(text)
		require.True(t, hardHit, text)
	}
}

func TestContentModerationDeepSeekKeywordPolicyIncludesCustomLayers(t *testing.T) {
	cfg := defaultContentModerationConfig()
	cfg.BlockedKeywords = []string{"legacy-mixed-keyword"}
	cfg.HardBlockPatterns = []string{"custom-hard-block"}
	cfg.CandidateKeywords = []string{"custom-candidate-review"}

	keywords, err := effectiveContentModerationKeywords(cfg)
	require.NoError(t, err)
	keyword, hit := matchBlockedKeyword("contains CUSTOM-HARD-BLOCK", keywords)
	require.True(t, hit)
	require.Equal(t, "custom-hard-block", keyword)
	_, hit = matchBlockedKeyword("legacy-mixed-keyword", keywords)
	require.False(t, hit)

	secondLayerKeywords, err := effectiveContentModerationSecondLayerKeywords(cfg)
	require.NoError(t, err)
	matcher := newContentModerationPrefilterMatcher(secondLayerKeywords)
	keyword, hit = matcher.Match("contains custom-candidate-review example")
	require.True(t, hit)
	require.Equal(t, "custom candidate review", keyword)
}

func TestContentModerationDeepSeekPolicyMetadata(t *testing.T) {
	settingRepo := &contentModerationTestSettingRepo{values: map[string]string{}}
	svc := NewContentModerationService(settingRepo, nil, nil, nil, nil, nil, nil, nil)

	view, err := svc.GetConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, 103, view.CandidateLayer1Count)
	require.Equal(t, 306, view.CandidateLayer2Count)
	require.NotEmpty(t, view.CandidateSourceCommit)
	require.Len(t, view.Layer1Keywords, 103)
	require.Len(t, view.Layer2Keywords, 306)
	require.True(t, view.CandidateSystemReady)
	require.Empty(t, view.CandidateSystemError)
}

func TestContentModerationUpdateConfigUsesCanonicalKeywordLayers(t *testing.T) {
	cfg := defaultContentModerationConfig()
	cfg.BlockedKeywords = []string{"legacy-mixed-keyword"}
	rawCfg, err := json.Marshal(cfg)
	require.NoError(t, err)
	settingRepo := &contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyContentModerationConfig: string(rawCfg),
	}}
	svc := NewContentModerationService(settingRepo, nil, nil, nil, nil, nil, nil, nil)
	layer1 := []string{" direct-block ", "DIRECT-BLOCK"}
	layer2 := []string{" candidate-review ", "CANDIDATE-REVIEW"}

	view, err := svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{
		Layer1Keywords: &layer1,
		Layer2Keywords: &layer2,
	})
	require.NoError(t, err)
	require.Len(t, view.Layer1Keywords, 104)
	require.Contains(t, view.Layer1Keywords, "direct-block")
	require.Len(t, view.Layer2Keywords, 307)
	require.Contains(t, view.Layer2Keywords, "candidate-review")
	require.True(t, view.CandidateSystemReady)
	require.Empty(t, view.CandidateSystemError)
	require.Empty(t, view.BlockedKeywords)

	var saved ContentModerationConfig
	require.NoError(t, json.Unmarshal([]byte(settingRepo.values[SettingKeyContentModerationConfig]), &saved))
	require.Equal(t, []string{"direct-block"}, saved.HardBlockPatterns)
	require.Equal(t, []string{"candidate-review"}, saved.CandidateKeywords)
	require.Empty(t, saved.BlockedKeywords)
}

func TestContentModerationUpdateConfigPersistsIndependentLayerStages(t *testing.T) {
	settingRepo := &contentModerationTestSettingRepo{values: map[string]string{}}
	svc := NewContentModerationService(settingRepo, nil, nil, nil, nil, nil, nil, nil)
	firstStage := ContentModerationFirstLayerStageEnforce
	secondStage := ContentModerationSecondLayerStageShadow
	secondLayerEnabled := false

	view, err := svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{
		FirstLayerStage:    &firstStage,
		SecondLayerEnabled: &secondLayerEnabled,
		SecondLayerStage:   &secondStage,
	})

	require.NoError(t, err)
	require.Equal(t, ContentModerationFirstLayerStageEnforce, view.FirstLayerStage)
	require.Equal(t, ContentModerationSecondLayerStageShadow, view.SecondLayerStage)
	var saved ContentModerationConfig
	require.NoError(t, json.Unmarshal([]byte(settingRepo.values[SettingKeyContentModerationConfig]), &saved))
	require.Equal(t, view.FirstLayerStage, saved.FirstLayerStage)
	require.Equal(t, view.SecondLayerStage, saved.SecondLayerStage)
}

func TestContentModerationStatusUsesDefaultPendingBodyBudgetForZeroValueService(t *testing.T) {
	settingRepo := &contentModerationTestSettingRepo{values: map[string]string{}}
	svc := &ContentModerationService{settingRepo: settingRepo}

	status, err := svc.GetStatus(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, DefaultContentModerationPendingBodyBudgetBytes, status.PendingBodyBudgetBytes)
	require.Zero(t, status.PendingBodyBytes)
	require.True(t, status.ArchiveRuntime.Degraded)
}

func TestParseContentModerationConfig_MigratesLegacyObserveToPreBlock(t *testing.T) {
	cfg, err := parseContentModerationConfig(`{"mode":"observe"}`)

	require.NoError(t, err)
	require.Equal(t, ContentModerationModePreBlock, cfg.Mode)
}

func TestContentModerationUpdateConfig_RejectsObserveMode(t *testing.T) {
	svc := NewContentModerationService(
		&contentModerationTestSettingRepo{values: map[string]string{}},
		&contentModerationTestRepo{}, nil, nil, nil, nil, nil, nil,
	)
	mode := "observe"

	_, err := svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{Mode: &mode})

	require.Error(t, err)
	require.Contains(t, err.Error(), "off 或 pre_block")
}

func TestNormalizeKeywordBlockingMode_UnknownFallsBackToDefault(t *testing.T) {
	require.Equal(t, ContentModerationKeywordModeKeywordAndAPI, normalizeKeywordBlockingMode(""))
	require.Equal(t, ContentModerationKeywordModeKeywordAndAPI, normalizeKeywordBlockingMode("bogus"))
	require.Equal(t, ContentModerationKeywordModeKeywordOnly, normalizeKeywordBlockingMode("keyword_only"))
	require.Equal(t, ContentModerationKeywordModeAPIOnly, normalizeKeywordBlockingMode("api_only"))
}

func TestContentModerationUpdateConfig_NormalizesAndValidatesUserEmailWhitelist(t *testing.T) {
	cfg := defaultContentModerationConfig()
	rawCfg, err := json.Marshal(cfg)
	require.NoError(t, err)
	repo := &contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyContentModerationConfig: string(rawCfg),
	}}
	svc := NewContentModerationService(repo, nil, nil, nil, nil, nil, nil, nil)
	emails := []string{" Allowed@Example.COM ", "allowed@example.com", "second@example.net"}

	view, err := svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{
		UserEmailWhitelist: &emails,
	})

	require.NoError(t, err)
	require.Equal(t, []string{"allowed@example.com", "second@example.net"}, view.UserEmailWhitelist)
	var saved ContentModerationConfig
	require.NoError(t, json.Unmarshal([]byte(repo.values[SettingKeyContentModerationConfig]), &saved))
	require.Equal(t, view.UserEmailWhitelist, saved.UserEmailWhitelist)

	invalid := []string{"not-an-email"}
	_, err = svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{
		UserEmailWhitelist: &invalid,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "用户邮箱白名单地址无效")
}

func TestUnifiedSevereBlockPersistsSynchronously(t *testing.T) {
	repo := &contentModerationTestRepo{}
	svc := NewContentModerationService(nil, repo, nil, nil, nil, nil, nil, nil)
	cfg := defaultContentModerationConfig()
	cfg.Enabled = true
	cfg.AutoBanEnabled = false
	fragment := ContentModerationFragment{Text: "blocked", Hash: strings.Repeat("a", 64)}

	decision := svc.unifiedBlockDecision(context.Background(), ContentModerationCheckInput{
		UserID: 9, UserRole: RoleUser, Body: []byte(`{"messages":[{"role":"user","content":"blocked"}]}`),
	}, cfg, fragment, ContentModerationActionKeywordBlock, contentModerationKeywordCategory, "blocked")

	require.True(t, decision.Blocked)
	logs := repo.snapshotLogs()
	require.Len(t, logs, 1)
	require.Equal(t, ContentModerationActionKeywordBlock, logs[0].Action)
}

func TestContentModerationAutoBanSkipsAdminAccount(t *testing.T) {
	var slogOutput bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&slogOutput, nil)))
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
	})

	cfg := defaultContentModerationConfig()
	cfg.BanThreshold = 2
	cfg.ViolationWindowHours = 24

	userID := int64(1001)
	repo := &contentModerationTestRepo{}
	require.NoError(t, repo.CreateLog(context.Background(), newContentModerationFlaggedLog(userID)))
	userRepo := &contentModerationTestUserRepo{user: &User{ID: userID, Role: RoleAdmin, Status: StatusActive}}
	invalidator := &contentModerationTestAuthCacheInvalidator{}
	svc := NewContentModerationService(nil, repo, nil, nil, userRepo, nil, invalidator, nil)

	svc.persistContentModerationLog(context.Background(), cfg, newContentModerationFlaggedLog(userID), "", false, true)

	logs := requireContentModerationLogCount(t, repo, 2)
	require.Equal(t, 2, logs[1].ViolationCount)
	require.False(t, logs[1].AutoBanned)
	require.Equal(t, StatusActive, userRepo.user.Status)
	require.Empty(t, userRepo.updated)
	require.Empty(t, invalidator.userIDs)
	require.Contains(t, slogOutput.String(), "content_moderation.autoban_skipped_admin")
	require.Contains(t, slogOutput.String(), "user_id=1001")
	require.Contains(t, slogOutput.String(), "role=admin")
	require.Contains(t, slogOutput.String(), "count=2")
	require.Contains(t, slogOutput.String(), "threshold=2")
}

func TestContentModerationAutoBanDisablesRegularUserAtThreshold(t *testing.T) {
	cfg := defaultContentModerationConfig()
	cfg.BanThreshold = 2
	cfg.ViolationWindowHours = 24

	userID := int64(1001)
	repo := &contentModerationTestRepo{}
	require.NoError(t, repo.CreateLog(context.Background(), newContentModerationFlaggedLog(userID)))
	userRepo := &contentModerationTestUserRepo{user: &User{ID: userID, Role: RoleUser, Status: StatusActive}}
	invalidator := &contentModerationTestAuthCacheInvalidator{}
	svc := NewContentModerationService(nil, repo, nil, nil, userRepo, nil, invalidator, nil)

	svc.persistContentModerationLog(context.Background(), cfg, newContentModerationFlaggedLog(userID), "", false, true)

	logs := requireContentModerationLogCount(t, repo, 2)
	require.Equal(t, 2, logs[1].ViolationCount)
	require.True(t, logs[1].AutoBanned)
	require.Len(t, userRepo.updated, 1)
	require.Equal(t, StatusDisabled, userRepo.user.Status)
	require.Equal(t, []int64{userID}, invalidator.userIDs)
}

func TestContentModerationAdminBelowBanThresholdRecordsViolationOnly(t *testing.T) {
	cfg := defaultContentModerationConfig()
	cfg.BanThreshold = 2
	cfg.ViolationWindowHours = 24

	userID := int64(1001)
	repo := &contentModerationTestRepo{}
	userRepo := &contentModerationTestUserRepo{user: &User{ID: userID, Role: RoleAdmin, Status: StatusActive}}
	invalidator := &contentModerationTestAuthCacheInvalidator{}
	svc := NewContentModerationService(nil, repo, nil, nil, userRepo, nil, invalidator, nil)

	svc.persistContentModerationLog(context.Background(), cfg, newContentModerationFlaggedLog(userID), "", false, true)

	logs := requireContentModerationLogCount(t, repo, 1)
	require.Equal(t, 1, logs[0].ViolationCount)
	require.False(t, logs[0].AutoBanned)
	require.Equal(t, StatusActive, userRepo.user.Status)
	require.Empty(t, userRepo.updated)
	require.Empty(t, invalidator.userIDs)
}

func newContentModerationFlaggedLog(userID int64) *ContentModerationLog {
	return &ContentModerationLog{
		UserID:          &userID,
		Action:          ContentModerationActionBlock,
		Flagged:         true,
		HighestCategory: "sexual",
		HighestScore:    0.9,
		CreatedAt:       time.Now(),
	}
}

func TestContentModerationDeleteFlaggedInputHash_NormalizesAndDeletes(t *testing.T) {
	existingHash := strings.Repeat("a", 64)
	hashCache := &contentModerationTestHashCache{hashes: map[string]struct{}{
		existingHash: {},
	}}
	svc := &ContentModerationService{hashCache: hashCache}

	result, err := svc.DeleteFlaggedInputHash(context.Background(), strings.ToUpper(existingHash))

	require.NoError(t, err)
	require.Equal(t, existingHash, result.InputHash)
	require.True(t, result.Deleted)
	require.False(t, hashCache.hasHash(existingHash))
	require.Equal(t, []string{existingHash}, hashCache.snapshotDeleted())

	result, err = svc.DeleteFlaggedInputHash(context.Background(), existingHash)

	require.NoError(t, err)
	require.Equal(t, existingHash, result.InputHash)
	require.False(t, result.Deleted)
}

func TestContentModerationClearFlaggedInputHashesAndStatusCount(t *testing.T) {
	cfg := defaultContentModerationConfig()
	cfg.Enabled = true
	rawCfg, err := json.Marshal(cfg)
	require.NoError(t, err)

	hashCache := &contentModerationTestHashCache{hashes: map[string]struct{}{
		strings.Repeat("a", 64): {},
		strings.Repeat("b", 64): {},
	}}
	svc := &ContentModerationService{
		settingRepo: &contentModerationTestSettingRepo{values: map[string]string{
			SettingKeyRiskControlEnabled:      "true",
			SettingKeyContentModerationConfig: string(rawCfg),
		}},
		hashCache: hashCache,
	}

	status, err := svc.GetStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(2), status.FlaggedHashCount)

	result, err := svc.ClearFlaggedInputHashes(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(2), result.Deleted)

	status, err = svc.GetStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(0), status.FlaggedHashCount)
}

func TestBuildContentModerationAccountDisabledEmailBody_ContainsBanDetails(t *testing.T) {
	userID := int64(1001)
	cfg := defaultContentModerationConfig()
	cfg.BanThreshold = 10
	body := buildContentModerationAccountDisabledEmailBody("Sub2API <Admin>", &ContentModerationLog{
		UserID:          &userID,
		UserEmail:       "user@example.com",
		GroupName:       "vip_2",
		HighestCategory: "sexual",
		HighestScore:    0.926,
		ViolationCount:  10,
	}, cfg)

	require.Contains(t, body, "账户已被自动禁用")
	require.Contains(t, body, "封禁详情")
	require.Contains(t, body, "账户当前处于封禁状态，所有 API 请求将被拒绝")
	require.Contains(t, body, "10 次（阈值 10）")
	require.Contains(t, body, "sexual / 0.926")
	require.Contains(t, body, "Sub2API &lt;Admin&gt;")
}

func TestContentModerationUnbanUser_ActivatesUserAndInvalidatesAuthCache(t *testing.T) {
	userRepo := &contentModerationTestUserRepo{user: &User{ID: 1001, Email: "user@example.com", Status: StatusDisabled}}
	invalidator := &contentModerationTestAuthCacheInvalidator{}
	repo := &contentModerationTestRepo{}
	svc := NewContentModerationService(nil, repo, nil, nil, userRepo, nil, invalidator, nil)

	result, err := svc.UnbanUser(context.Background(), 1001)

	require.NoError(t, err)
	require.Equal(t, int64(1001), result.UserID)
	require.Equal(t, StatusActive, result.Status)
	require.Len(t, userRepo.updated, 1)
	require.Equal(t, StatusActive, userRepo.updated[0].Status)
	require.Equal(t, []int64{1001}, invalidator.userIDs)
}

func TestContentModerationUnbanUser_ActiveUserOnlyInvalidatesAuthCache(t *testing.T) {
	userRepo := &contentModerationTestUserRepo{user: &User{ID: 1001, Email: "user@example.com", Status: StatusActive}}
	invalidator := &contentModerationTestAuthCacheInvalidator{}
	repo := &contentModerationTestRepo{}
	svc := NewContentModerationService(nil, repo, nil, nil, userRepo, nil, invalidator, nil)

	result, err := svc.UnbanUser(context.Background(), 1001)

	require.NoError(t, err)
	require.Equal(t, StatusActive, result.Status)
	require.Empty(t, userRepo.updated)
	require.Equal(t, []int64{1001}, invalidator.userIDs)
}

func TestContentModerationUpdateConfig_CyberPolicyExcludeFromBanCount(t *testing.T) {
	settingRepo := &contentModerationTestSettingRepo{values: map[string]string{}}
	svc := NewContentModerationService(settingRepo, nil, nil, nil, nil, nil, nil, nil)

	// 默认值必须是 false（计入，保持现状）
	view, err := svc.GetConfig(context.Background())
	require.NoError(t, err)
	require.False(t, view.CyberPolicyExcludeFromBanCount, "默认必须计入封号计数")

	// 指针式部分更新为 true
	exclude := true
	view, err = svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{
		CyberPolicyExcludeFromBanCount: &exclude,
	})
	require.NoError(t, err)
	require.True(t, view.CyberPolicyExcludeFromBanCount)

	// 持久化 JSON 含字段
	var saved ContentModerationConfig
	require.NoError(t, json.Unmarshal([]byte(settingRepo.values[SettingKeyContentModerationConfig]), &saved))
	require.True(t, saved.CyberPolicyExcludeFromBanCount)

	// 二次读取（从持久化 JSON 反序列化）roundtrip
	view, err = svc.GetConfig(context.Background())
	require.NoError(t, err)
	require.True(t, view.CyberPolicyExcludeFromBanCount)

	// 不传该字段的更新不得改动它（指针 nil = 保留）
	view, err = svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{})
	require.NoError(t, err)
	require.True(t, view.CyberPolicyExcludeFromBanCount)

	// 主动回拨 false 必须生效（防止未来误加 if val 保护逻辑）
	revert := false
	view, err = svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{
		CyberPolicyExcludeFromBanCount: &revert,
	})
	require.NoError(t, err)
	require.False(t, view.CyberPolicyExcludeFromBanCount)
}
