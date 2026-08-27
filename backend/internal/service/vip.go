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
	// VipRateDiscount VIP 在指定分组的计费倍率减免额（显示倍率 - 0.05）。
	VipRateDiscount = 0.05
	// VipDiscountedGroupName 享受倍率减免的分组名（大小写不敏感）。
	VipDiscountedGroupName = "gpt-pro"
	// VipExclusiveModelName 仅允许 VIP 用户调用的模型家族基名。
	VipExclusiveModelName = "gpt-5.6-luna"
	// VipExclusiveModelAccessMessage 是网关拒绝普通用户调用 VIP 专属模型时的稳定提示。
	VipExclusiveModelAccessMessage = "The gpt-5.6-luna model is available to VIP users only"
	// vipSweepTimeout 启动扫描的超时上限。
	vipSweepTimeout = 30 * time.Second
)

// VipDiscountedGroup 判断分组名是否享受 VIP 倍率减免。
func VipDiscountedGroup(groupName string) bool {
	return strings.EqualFold(strings.TrimSpace(groupName), VipDiscountedGroupName)
}

// ApplyVipRateDiscount 对已解析的基础倍率应用 VIP 减免，结果不为负，
// 并消除浮点减法噪声（如 0.17-0.05=0.12000000000000001）。
func ApplyVipRateDiscount(multiplier float64) float64 {
	discounted := multiplier - VipRateDiscount
	if discounted < 0 {
		return 0
	}
	return math.Round(discounted*1e6) / 1e6
}

// IsVIPOnlyModel 判断模型是否属于 VIP 专属家族。日期版等带连字符后缀的
// Luna 变体与基名使用相同权限，避免通过版本化模型名绕过限制。
func IsVIPOnlyModel(model string) bool {
	normalized := normalizeVIPModelCandidate(model)
	return normalized == VipExclusiveModelName || strings.HasPrefix(normalized, VipExclusiveModelName+"-")
}

func normalizeVIPModelCandidate(model string) string {
	normalized := strings.ToLower(lastOpenAIModelSegment(model))
	normalized = strings.ReplaceAll(normalized, "_", "-")
	normalized = strings.Join(strings.Fields(normalized), "-")
	for strings.Contains(normalized, "--") {
		normalized = strings.ReplaceAll(normalized, "--", "-")
	}
	if strings.HasPrefix(normalized, "gpt5.6") {
		normalized = "gpt-5.6" + strings.TrimPrefix(normalized, "gpt5.6")
	}
	if strings.HasPrefix(normalized, "gpt-5.6luna") {
		normalized = VipExclusiveModelName + strings.TrimPrefix(normalized, "gpt-5.6luna")
	}
	return normalized
}

// UserCanAccessModel 判断用户是否可以调用指定模型。非 VIP 模型保持原有行为；
// VIP 专属模型在用户信息缺失时按普通用户处理并拒绝访问。
func UserCanAccessModel(user *User, model string) bool {
	return !IsVIPOnlyModel(model) || (user != nil && user.IsVIP)
}

// FilterUserAccessibleModels 从模型发现列表中移除当前用户无权调用的模型，
// 保持原顺序且不修改调用方传入的切片。
func FilterUserAccessibleModels(user *User, models []string) []string {
	if user != nil && user.IsVIP {
		return models
	}

	var filtered []string
	for i, model := range models {
		if IsVIPOnlyModel(model) {
			if filtered == nil {
				filtered = append(make([]string, 0, len(models)-1), models[:i]...)
			}
			continue
		}
		if filtered != nil {
			filtered = append(filtered, model)
		}
	}
	if filtered == nil {
		return models
	}
	return filtered
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
