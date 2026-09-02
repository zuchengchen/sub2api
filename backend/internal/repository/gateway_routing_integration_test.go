//go:build integration

package repository

import (
	"context"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/suite"
)

// GatewayRoutingSuite 测试网关路由相关的数据库查询
// 验证账户选择和分流逻辑在真实数据库环境下的行为
type GatewayRoutingSuite struct {
	suite.Suite
	ctx         context.Context
	client      *dbent.Client
	accountRepo *accountRepository
}

func (s *GatewayRoutingSuite) SetupTest() {
	s.ctx = context.Background()
	tx := testEntTx(s.T())
	s.client = tx.Client()
	s.accountRepo = newAccountRepositoryWithSQL(s.client, tx, nil)
}

func TestGatewayRoutingSuite(t *testing.T) {
	suite.Run(t, new(GatewayRoutingSuite))
}

// TestListSchedulableByPlatforms_GrokAndOpenAI 验证多平台账户查询
func (s *GatewayRoutingSuite) TestListSchedulableByPlatforms_GrokAndOpenAI() {
	// 创建各平台账户
	geminiAcc := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:        "grok-oauth",
		Platform:    service.PlatformGrok,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Schedulable: true,
		Priority:    1,
	})

	antigravityAcc := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:        "openai-oauth",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Schedulable: true,
		Priority:    2,
		Credentials: map[string]any{
			"access_token":  "test-token",
			"refresh_token": "test-refresh",
			"project_id":    "test-project",
		},
	})

	// 创建不应被选中的 anthropic 账户
	mustCreateAccount(s.T(), s.client, &service.Account{
		Name:        "anthropic-oauth",
		Platform:    service.PlatformAnthropic,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Schedulable: true,
		Priority:    0,
	})

	// 查询 gemini + antigravity 平台
	accounts, err := s.accountRepo.ListSchedulableByPlatforms(s.ctx, []string{
		service.PlatformGrok,
		service.PlatformOpenAI,
	})

	s.Require().NoError(err)
	s.Require().Len(accounts, 2, "应返回 gemini 和 antigravity 两个账户")

	// 验证返回的账户平台
	platforms := make(map[string]bool)
	for _, acc := range accounts {
		platforms[acc.Platform] = true
	}
	s.Require().True(platforms[service.PlatformGrok], "应包含 grok 账户")
	s.Require().True(platforms[service.PlatformOpenAI], "应包含 openai 账户")
	s.Require().False(platforms[service.PlatformAnthropic], "不应包含 anthropic 账户")

	// 验证账户 ID 匹配
	ids := make(map[int64]bool)
	for _, acc := range accounts {
		ids[acc.ID] = true
	}
	s.Require().True(ids[geminiAcc.ID])
	s.Require().True(ids[antigravityAcc.ID])
}

// TestListSchedulableByGroupIDAndPlatforms_WithGroupBinding 验证按分组过滤
func (s *GatewayRoutingSuite) TestListSchedulableByGroupIDAndPlatforms_WithGroupBinding() {
	// 创建 gemini 分组
	group := mustCreateGroup(s.T(), s.client, &service.Group{
		Name:     "grok-group",
		Platform: service.PlatformGrok,
		Status:   service.StatusActive,
	})

	// 创建账户
	boundAcc := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:        "bound-antigravity",
		Platform:    service.PlatformOpenAI,
		Status:      service.StatusActive,
		Schedulable: true,
	})
	unboundAcc := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:        "unbound-antigravity",
		Platform:    service.PlatformOpenAI,
		Status:      service.StatusActive,
		Schedulable: true,
	})

	// 只绑定一个账户到分组
	mustBindAccountToGroup(s.T(), s.client, boundAcc.ID, group.ID, 1)

	// 查询分组内的账户
	accounts, err := s.accountRepo.ListSchedulableByGroupIDAndPlatforms(s.ctx, group.ID, []string{
		service.PlatformGrok,
		service.PlatformOpenAI,
	})

	s.Require().NoError(err)
	s.Require().Len(accounts, 1, "应只返回绑定到分组的账户")
	s.Require().Equal(boundAcc.ID, accounts[0].ID)

	// 确认未绑定的账户不在结果中
	for _, acc := range accounts {
		s.Require().NotEqual(unboundAcc.ID, acc.ID, "不应包含未绑定的账户")
	}
}

// TestListSchedulableByPlatform_OpenAI 验证单平台查询
func (s *GatewayRoutingSuite) TestListSchedulableByPlatform_OpenAI() {
	// 创建多种平台账户
	mustCreateAccount(s.T(), s.client, &service.Account{
		Name:        "grok-1",
		Platform:    service.PlatformGrok,
		Status:      service.StatusActive,
		Schedulable: true,
	})

	antigravity := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:        "openai-1",
		Platform:    service.PlatformOpenAI,
		Status:      service.StatusActive,
		Schedulable: true,
	})

	// 只查询 antigravity 平台
	accounts, err := s.accountRepo.ListSchedulableByPlatform(s.ctx, service.PlatformOpenAI)

	s.Require().NoError(err)
	s.Require().Len(accounts, 1)
	s.Require().Equal(antigravity.ID, accounts[0].ID)
	s.Require().Equal(service.PlatformOpenAI, accounts[0].Platform)
}

// TestSchedulableFilter_ExcludesInactive 验证不可调度账户被过滤
func (s *GatewayRoutingSuite) TestSchedulableFilter_ExcludesInactive() {
	// 创建可调度账户
	activeAcc := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:        "active-antigravity",
		Platform:    service.PlatformOpenAI,
		Status:      service.StatusActive,
		Schedulable: true,
	})

	// 创建不可调度账户（需要先创建再更新，因为 fixture 默认设置 Schedulable=true）
	inactiveAcc := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:     "inactive-antigravity",
		Platform: service.PlatformOpenAI,
		Status:   service.StatusActive,
	})
	s.Require().NoError(s.client.Account.UpdateOneID(inactiveAcc.ID).SetSchedulable(false).Exec(s.ctx))

	// 创建错误状态账户
	mustCreateAccount(s.T(), s.client, &service.Account{
		Name:        "error-antigravity",
		Platform:    service.PlatformOpenAI,
		Status:      service.StatusError,
		Schedulable: true,
	})

	accounts, err := s.accountRepo.ListSchedulableByPlatform(s.ctx, service.PlatformOpenAI)

	s.Require().NoError(err)
	s.Require().Len(accounts, 1, "应只返回可调度的 active 账户")
	s.Require().Equal(activeAcc.ID, accounts[0].ID)
}
