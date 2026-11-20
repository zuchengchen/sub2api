package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// cyberKillDispositionRepo 记录处置调用，供终止对话 + 吊销 Key 断言。
type cyberKillDispositionRepo struct {
	cyberDispositionTestRepo
	disabledUserID int64
	revokeUserKeys func(int64) []string
}

func (r *cyberKillDispositionRepo) DisableUserIfActive(_ context.Context, userID int64) (bool, error) {
	r.disabledUserID = userID
	return true, nil
}

func (r *cyberKillDispositionRepo) DisableActiveAPIKeysForDisabledUser(_ context.Context, userID int64) ([]string, error) {
	return r.revokeUserKeys(userID), nil
}

func TestRecordCyberPolicyEvent_KillsConversationsAndRevokesAllKeys(t *testing.T) {
	repo := &cyberKillDispositionRepo{}
	invalidator := &contentModerationTestAuthCacheInvalidator{}

	svc := NewContentModerationService(
		&contentModerationTestSettingRepo{values: map[string]string{
			SettingKeyRiskControlEnabled: "true",
		}},
		repo, nil, nil, nil, nil, invalidator, nil,
	)
	registry := NewGatewayKillSwitchRegistry()
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	registry.Register(7, "req-a", cancelA)
	registry.Register(7, "req-b", cancelB)
	registry.Register(8, "req-other", cancelB) // 用户 8 的登记复用 cancelB 仅作占位
	svc.conversationTerminator = registry
	repo.revokeUserKeys = func(userID int64) []string {
		require.Equal(t, int64(7), userID)
		require.Equal(t, userID, repo.disabledUserID)
		require.ErrorIs(t, ctxA.Err(), context.Canceled)
		require.ErrorIs(t, ctxB.Err(), context.Canceled)
		return []string{"sk-key-12", "sk-key-13"}
	}

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
	require.Zero(t, repo.disableKeyCalls, "普通用户使用带用户状态检查的批量吊销")
	require.Equal(t, []string{"sk-key-12", "sk-key-13"}, invalidator.keys, "逐 Key 失效认证缓存")
	require.Equal(t, []int64{7}, invalidator.userIDs)

	logs := repo.snapshotLogs()
	require.Len(t, logs, 1)
	require.Equal(t, "disabled", logs[0].DispositionStatus)
	require.True(t, logs[0].AutoBanned)
}

func TestCyberPolicyKeyRevocationFailureInvalidatesAlreadyDisabledUser(t *testing.T) {
	for _, tc := range []struct {
		name string
		repo ContentModerationRepository
	}{
		{"missing disposition repository", &contentModerationTestRepo{}},
		{"key write failed", &cyberDispositionTestRepo{revokeUserKeysErr: errors.New("write failed")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalidator := &contentModerationTestAuthCacheInvalidator{}
			users := &contentModerationTestUserRepo{user: &User{ID: 7, Role: RoleUser, Status: StatusDisabled}}
			svc := NewContentModerationService(nil, tc.repo, nil, nil, users, nil, invalidator, nil)
			log := &ContentModerationLog{}
			transitioned, err := svc.applyCyberPolicyDisposition(context.Background(), CyberPolicyRecordInput{UserID: 7}, log)
			require.NoError(t, err, "a key failure must not schedule a replay of the successful user ban")
			require.False(t, transitioned)
			require.Equal(t, "already_disabled", log.DispositionStatus)
			require.Equal(t, []int64{7}, invalidator.userIDs)
			require.Empty(t, invalidator.keys)
		})
	}
}
