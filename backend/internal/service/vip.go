package service

import (
	"context"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// VIP 用户策略常量。
const (
	// VipBalanceThreshold 总余额（含赠送）超过该值时自动升级为 VIP。
	VipBalanceThreshold = 100.0
	// VipFrozenReserve VIP 冻结金额：可用余额 = balance - VipFrozenReserve。
	// 逻辑冻结，不占用 users.frozen_balance（该列被批量图片余额暂扣使用）。
	VipFrozenReserve = 100.0
	// VipRateDiscount VIP 在指定分组的计费倍率减免额（显示倍率 - 0.02）。
	VipRateDiscount = 0.02
	// VipDiscountedGroupName 享受倍率减免的分组名（大小写不敏感）。
	VipDiscountedGroupName = "gpt-pro"
	// vipSweepTimeout 启动扫描的超时上限。
	vipSweepTimeout = 30 * time.Second
)

// VipDiscountedGroup 判断分组名是否享受 VIP 倍率减免。
func VipDiscountedGroup(groupName string) bool {
	return strings.EqualFold(strings.TrimSpace(groupName), VipDiscountedGroupName)
}

// ApplyVipRateDiscount 对已解析的基础倍率应用 VIP 减免，结果不为负，
// 并消除浮点减法噪声（如 0.17-0.02=0.15000000000000002）。
func ApplyVipRateDiscount(multiplier float64) float64 {
	discounted := multiplier - VipRateDiscount
	if discounted < 0 {
		return 0
	}
	return math.Round(discounted*1e6) / 1e6
}

// applyVipGroupRateDiscount 仅当 VIP 用户命中减免分组时应用倍率减免。
func applyVipGroupRateDiscount(user *User, group *Group, multiplier float64) float64 {
	if user == nil || group == nil || !user.IsVIP || !VipDiscountedGroup(group.Name) {
		return multiplier
	}
	return ApplyVipRateDiscount(multiplier)
}

// vipUpgradeExecutor 提供 VIP 升级的公共执行逻辑，避免各触发点重复实现。
type vipUpgradeExecutor struct {
	userRepo    UserRepository
	invalidator APIKeyAuthCacheInvalidator
}

// NewVipUpgradeExecutor 创建 VIP 升级执行器。
func NewVipUpgradeExecutor(userRepo UserRepository, invalidator APIKeyAuthCacheInvalidator) *vipUpgradeExecutor {
	return &vipUpgradeExecutor{userRepo: userRepo, invalidator: invalidator}
}

// EnsureUpgrade 惰性升级：余额超过阈值且未升级时原子置位 vip 并失效认证缓存。
// 返回本次调用是否实际完成了升级。升级为永久生效，不提供降级路径。
func (e *vipUpgradeExecutor) EnsureUpgrade(ctx context.Context, userID int64) bool {
	if e == nil || e.userRepo == nil || userID <= 0 {
		return false
	}
	user, err := e.userRepo.GetByID(ctx, userID)
	if err != nil {
		return false
	}
	if user == nil || user.IsVIP || user.Balance <= VipBalanceThreshold {
		return false
	}
	upgraded, err := e.userRepo.SetVIPIfNotSet(ctx, userID)
	if err != nil {
		logger.LegacyPrintf("service.vip", "Warning: upgrade user %d to vip failed: %v", userID, err)
		return false
	}
	if !upgraded {
		return false
	}
	slog.Info("vip.user_upgraded", "user_id", userID, "balance", user.Balance)
	if e.invalidator != nil {
		e.invalidator.InvalidateAuthCacheByUserID(ctx, userID)
	}
	return true
}

// SweepExisting 全量扫描并升级所有满足余额条件的存量用户，返回升级的用户 ID。
func (e *vipUpgradeExecutor) SweepExisting(ctx context.Context) []int64 {
	if e == nil || e.userRepo == nil {
		return nil
	}
	userIDs, err := e.userRepo.UpgradeUsersAboveBalanceThreshold(ctx, VipBalanceThreshold)
	if err != nil {
		logger.LegacyPrintf("service.vip", "Warning: vip sweep failed: %v", err)
		return nil
	}
	for _, userID := range userIDs {
		slog.Info("vip.user_upgraded", "user_id", userID, "source", "sweep")
		if e.invalidator != nil {
			e.invalidator.InvalidateAuthCacheByUserID(ctx, userID)
		}
	}
	return userIDs
}

// StartSweep 异步执行一次启动扫描，兜底覆盖存量余额达标的用户（幂等）。
func (e *vipUpgradeExecutor) StartSweep() {
	if e == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), vipSweepTimeout)
		defer cancel()
		upgraded := e.SweepExisting(ctx)
		if len(upgraded) > 0 {
			logger.LegacyPrintf("service.vip", "startup sweep upgraded %d users to vip", len(upgraded))
		}
	}()
}
