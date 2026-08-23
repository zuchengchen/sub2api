package service

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// cyberKillDispositionRepo 记录处置调用，供终止对话 + 吊销 Key 断言。
type cyberKillDispositionRepo struct {
	contentModerationTestRepo
	mu             sync.Mutex
	disabledUserID int64
	disabledKeyIDs []int64
}

func (r *cyberKillDispositionRepo) DisableUserIfActive(_ context.Context, userID int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disabledUserID = userID
	return true, nil
}

func (r *cyberKillDispositionRepo) DisableAPIKeyIfActive(_ context.Context, apiKeyID int64) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disabledKeyIDs = append(r.disabledKeyIDs, apiKeyID)
	return fmt.Sprintf("sk-key-%d", apiKeyID), true, nil
}

// cyberKillAPIKeyRepo 只实现处置路径用到的 ListAllByUserID；嵌入 nil 接口
// 以满足 APIKeyRepository，意外方法调用会 panic 暴露问题。
type cyberKillAPIKeyRepo struct {
	APIKeyRepository
	keys []APIKey
}

func (r *cyberKillAPIKeyRepo) ListAllByUserID(_ context.Context, userID int64, filters APIKeyListFilters) ([]APIKey, error) {
	out := make([]APIKey, 0, len(r.keys))
	for _, key := range r.keys {
		if key.UserID != userID {
			continue
		}
		if filters.Status != "" && key.Status != filters.Status {
			continue
		}
		out = append(out, key)
	}
	return out, nil
}

func TestRecordCyberPolicyEvent_KillsConversationsAndRevokesAllKeys(t *testing.T) {
	repo := &cyberKillDispositionRepo{}
	apiKeys := &cyberKillAPIKeyRepo{keys: []APIKey{
		{ID: 11, UserID: 7, Status: StatusAPIKeyDisabled}, // 已停用：跳过
		{ID: 12, UserID: 7, Status: StatusActive},
		{ID: 13, UserID: 7, Status: StatusActive},
		{ID: 99, UserID: 8, Status: StatusActive}, // 其他用户：不得触碰
	}}
	invalidator := &contentModerationTestAuthCacheInvalidator{}

	svc := NewContentModerationService(
		&contentModerationTestSettingRepo{values: map[string]string{
			SettingKeyRiskControlEnabled: "true",
		}},
		repo, nil, nil, nil, nil, invalidator, nil,
	)
	svc.apiKeyRepo = apiKeys

	registry := NewGatewayKillSwitchRegistry()
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	registry.Register(7, "req-a", cancelA)
	registry.Register(7, "req-b", cancelB)
	registry.Register(8, "req-other", cancelB) // 用户 8 的登记复用 cancelB 仅作占位
	svc.conversationTerminator = registry

	svc.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{
		RequestID:       "req-kill",
		UserID:          7,
		UserEmail:       "u@example.com",
		Model:           "gpt-5",
		UpstreamMessage: "blocked",
		Scope:           cyberPolicyScope(),
		UserRole:        RoleUser,
	})

	require.Equal(t, context.Canceled, ctxA.Err(), "在途请求必须被立即取消")
	require.Equal(t, context.Canceled, ctxB.Err(), "同用户第二个在途请求必须被取消")
	require.Equal(t, 0, registry.CountForUser(7))
	require.Equal(t, 1, registry.CountForUser(8), "其他用户的登记不得被清除")

	require.Equal(t, int64(7), repo.disabledUserID, "必须禁用用户")
	require.Equal(t, []int64{12, 13}, repo.disabledKeyIDs, "必须吊销该用户全部活跃 Key")
	require.Equal(t, []string{"sk-key-12", "sk-key-13"}, invalidator.keys, "逐 Key 失效认证缓存")
	require.Equal(t, []int64{7}, invalidator.userIDs)

	logs := repo.snapshotLogs()
	require.Len(t, logs, 1)
	require.Equal(t, "disabled", logs[0].DispositionStatus)
	require.True(t, logs[0].AutoBanned)
}

func TestRevokeCyberPolicyUserAPIKeys_ListerUnavailableIsNoOp(t *testing.T) {
	repo := &cyberKillDispositionRepo{}
	svc := NewContentModerationService(
		&contentModerationTestSettingRepo{values: map[string]string{}},
		repo, nil, nil, nil, nil, nil, nil,
	)
	// svc.apiKeyRepo 为 nil：类型断言失败必须静默跳过，不影响主处置。
	require.NotPanics(t, func() {
		svc.revokeCyberPolicyUserAPIKeys(context.Background(), 7)
	})
	require.Empty(t, repo.disabledKeyIDs)
}
