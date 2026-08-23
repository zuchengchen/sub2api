package service

import (
	"context"
	"sync"
)

// CyberConversationTerminator 终止指定用户当前所有在途网关对话。
// 由 cyber 处置流程在禁用账号后立即调用，防止认证缓存 TTL 窗口内
// 已建立的 SSE/WebSocket/长请求继续消耗上游额度。
type CyberConversationTerminator interface {
	CancelAllForUser(userID int64) int
}

// GatewayKillSwitchRegistry 记录 userID -> requestID -> cancel 的在途
// 网关请求。进程本地状态：多实例部署时各实例独立生效，跨实例的
// 快速拒绝仍由账号/Key 禁用加认证缓存失效兜底。
type GatewayKillSwitchRegistry struct {
	mu     sync.RWMutex
	byUser map[int64]map[string]context.CancelFunc
}

// NewGatewayKillSwitchRegistry 创建空注册表。
func NewGatewayKillSwitchRegistry() *GatewayKillSwitchRegistry {
	return &GatewayKillSwitchRegistry{byUser: make(map[int64]map[string]context.CancelFunc)}
}

var defaultGatewayKillSwitch = NewGatewayKillSwitchRegistry()

// DefaultGatewayKillSwitchRegistry 返回进程级共享注册表。中间件注册、
// 风控处置终止都使用同一实例，避免为单一进程内组件贯穿整条 wire 注入链。
func DefaultGatewayKillSwitchRegistry() *GatewayKillSwitchRegistry {
	return defaultGatewayKillSwitch
}

// Register 登记一条在途请求的取消函数。
func (r *GatewayKillSwitchRegistry) Register(userID int64, requestID string, cancel context.CancelFunc) {
	if r == nil || userID <= 0 || requestID == "" || cancel == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byUser == nil {
		r.byUser = make(map[int64]map[string]context.CancelFunc)
	}
	byRequest, ok := r.byUser[userID]
	if !ok {
		byRequest = make(map[string]context.CancelFunc)
		r.byUser[userID] = byRequest
	}
	byRequest[requestID] = cancel
}

// Unregister 移除已结束的请求登记。
func (r *GatewayKillSwitchRegistry) Unregister(userID int64, requestID string) {
	if r == nil || userID <= 0 || requestID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	byRequest, ok := r.byUser[userID]
	if !ok {
		return
	}
	delete(byRequest, requestID)
	if len(byRequest) == 0 {
		delete(r.byUser, userID)
	}
}

// CancelAllForUser 取消该用户全部在途请求，返回取消数量。
func (r *GatewayKillSwitchRegistry) CancelAllForUser(userID int64) int {
	if r == nil || userID <= 0 {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	byRequest, ok := r.byUser[userID]
	if !ok {
		return 0
	}
	cancelled := 0
	for requestID, cancel := range byRequest {
		cancel()
		delete(byRequest, requestID)
		cancelled++
	}
	delete(r.byUser, userID)
	return cancelled
}

// CountForUser 返回该用户当前在途登记数量（测试与观测用）。
func (r *GatewayKillSwitchRegistry) CountForUser(userID int64) int {
	if r == nil || userID <= 0 {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byUser[userID])
}
