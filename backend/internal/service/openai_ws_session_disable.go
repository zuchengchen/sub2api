package service

import (
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// 官方 Codex CLI 的传输回退语义（codex-rs core/src/client.rs）：
// provider.supports_websockets 默认开启、每请求优先 WS；两类触发会置位
// 会话级 disable_websockets，会话余生全部走 HTTP——
//   1. WS 握手收到 426 Upgrade Required（能力信号）
//   2. 流式重试预算耗尽（stream_max_retries=5）
//
// 网关按凭据归属、账号、分组和隔离后的客户端执行会话保存标记。
// 活跃会话不会因故障 TTL 到期重新探测；24h 空闲或容量淘汰结束本地会话。
// 匿名请求仅共享本次请求的状态。
//
// 分类原则与官方一致：能力信号与「重试预算耗尽的可重试错误」禁用本会话
// 的 WS 传输；本地连接池争用（acquire/queue）只让本请求透明回退；请求内容级与凭据级
// 错误（policy/auth/dead-anchor/invalid-encrypted）在 HTTP 上会以同形失败，
// 不触发回退也不标记。

type openAIWSSessionScope struct {
	AccountID           int64
	CredentialOwnerID   int64
	CredentialNamespace string
	GroupID             int64
	Session             string
	Ephemeral           bool
}

// Resolve from the original client identity, before account namespace projection.
// Access tokens are deliberately excluded: OAuth refresh must not reset fallback.
func resolveOpenAIWSSessionScope(c *gin.Context, account *Account, body []byte) openAIWSSessionScope {
	if account == nil {
		return openAIWSSessionScope{}
	}
	scope := openAIWSSessionScope{AccountID: account.ID, CredentialOwnerID: account.ID, GroupID: getOpenAIGroupIDFromContext(c)}
	if owner := codexAccountIdentitySource(c, account); owner != nil {
		scope.CredentialOwnerID = owner.ID
		scope.CredentialNamespace = codexAccountIdentityNamespace(owner)
	}
	const scopeKey = "openai_ws_session_scope"
	if c != nil {
		if cached, ok := c.Get(scopeKey); ok {
			if frozen, ok := cached.(openAIWSSessionScope); ok && frozen.AccountID == scope.AccountID && frozen.CredentialOwnerID == scope.CredentialOwnerID && frozen.CredentialNamespace == scope.CredentialNamespace && frozen.GroupID == scope.GroupID {
				return frozen
			}
		}
	}
	apiKeyID := getAPIKeyIDFromContext(c)
	scope.Session, _ = resolveOpenAIWSExecutionScope(c, body, apiKeyID)
	if scope.Session == "" {
		scope.Ephemeral = true
		scope.Session = openAIWSLocalStateScope(c, apiKeyID)
	}
	if c != nil {
		c.Set(scopeKey, scope)
	}
	return scope
}

// openAIWSSessionDisableState lasts for the logical session, not a retry TTL.
type openAIWSSessionDisableState struct {
	reason         string
	since          time.Time
	prewarmStarted bool
	lastUsed       time.Time
}

const openAIWSSessionRegistryLimit = 8192
const openAIWSSessionIdleLifetime = 24 * time.Hour

// Serializes insertions and eviction, without changing the service's shared map.
var openAIWSSessionRegistryMu sync.Mutex

func (s *OpenAIGatewayService) openAIWSSessionStateLocked(scope openAIWSSessionScope) openAIWSSessionDisableState {
	if raw, ok := s.openaiWSSessionDisabled.Load(scope); ok {
		if state, ok := raw.(openAIWSSessionDisableState); ok && (state.lastUsed.IsZero() || time.Since(state.lastUsed) < openAIWSSessionIdleLifetime) {
			return state
		}
		s.openaiWSSessionDisabled.Delete(scope)
	}
	return openAIWSSessionDisableState{}
}

func (s *OpenAIGatewayService) storeOpenAIWSSessionStateLocked(scope openAIWSSessionScope, state openAIWSSessionDisableState) {
	now := time.Now()
	if _, exists := s.openaiWSSessionDisabled.Load(scope); !exists {
		count := 0
		var oldest any
		oldestAt := now
		s.openaiWSSessionDisabled.Range(func(key, value any) bool {
			entry, ok := value.(openAIWSSessionDisableState)
			if !ok || (!entry.lastUsed.IsZero() && now.Sub(entry.lastUsed) >= openAIWSSessionIdleLifetime) {
				s.openaiWSSessionDisabled.Delete(key)
				return true
			}
			count++
			if oldest == nil || entry.lastUsed.Before(oldestAt) {
				oldest, oldestAt = key, entry.lastUsed
			}
			return true
		})
		if count >= openAIWSSessionRegistryLimit && oldest != nil {
			s.openaiWSSessionDisabled.Delete(oldest)
		}
	}
	state.lastUsed = now
	s.openaiWSSessionDisabled.Store(scope, state)
}

// Reserve before writing so a reconnect or a concurrent turn cannot prewarm twice.
func (s *OpenAIGatewayService) openAIWSSessionPrewarmPending(scope openAIWSSessionScope) bool {
	openAIWSSessionRegistryMu.Lock()
	defer openAIWSSessionRegistryMu.Unlock()
	state := s.openAIWSSessionStateLocked(scope)
	return !state.prewarmStarted && state.reason == ""
}

func (s *OpenAIGatewayService) beginOpenAIWSSessionPrewarm(scope openAIWSSessionScope) bool {
	if s == nil || scope.Session == "" {
		return false
	}
	openAIWSSessionRegistryMu.Lock()
	defer openAIWSSessionRegistryMu.Unlock()
	state := s.openAIWSSessionStateLocked(scope)
	allowed := !state.prewarmStarted && state.reason == ""
	state.prewarmStarted = true
	s.storeOpenAIWSSessionStateLocked(scope, state)
	return allowed
}

// openAIWSSessionFallbackAction 描述一次 WS 失败应采取的会话级动作。
type openAIWSSessionFallbackAction struct {
	// fallbackHTTP: 本请求可在未写下游的前提下透明改走 HTTP 重试。
	fallbackHTTP bool
	// markDisable: 标记当前逻辑会话，账号健康由独立策略维护。
	markDisable bool
	// capability: 能力信号（426/ws_unsupported）无需耗尽传输重试。
	capability bool
	reason     string
}

// openAIWSSessionFallbackActionFor 按 WS 失败原因分类会话级动作。
// reason 取 openAIWSFallbackError.Reason（prewarm_ 前缀已剥离后传入）。
func openAIWSSessionFallbackActionFor(reason string) openAIWSSessionFallbackAction {
	switch reason {
	case "upgrade_required", "ws_unsupported":
		return openAIWSSessionFallbackAction{fallbackHTTP: true, markDisable: true, capability: true, reason: reason}
	case "message_too_big":
		// WS 帧上限问题；HTTP 无帧上限可重试，但非账号健康问题。
		return openAIWSSessionFallbackAction{fallbackHTTP: true, reason: reason}
	case "acquire_timeout", "acquire_conn", "conn_queue_full", "preferred_conn_unavailable":
		// 本地连接池争用：本请求可回退，不标记账号。
		return openAIWSSessionFallbackAction{fallbackHTTP: true, reason: reason}
	case "dial_failed",
		"upstream_5xx",
		"read_event",
		"write_request",
		"write",
		"event_error",
		"error_event",
		"upstream_error_event",
		"ws_connection_limit_reached",
		"missing_final_response":
		return openAIWSSessionFallbackAction{fallbackHTTP: true, markDisable: true, reason: reason}
	default:
		// auth_failed / policy_violation / previous_response_not_found /
		// invalid_encrypted_content / retry_backoff_canceled / 未知原因：
		// HTTP 会给出同形失败或语义不对，不回退不标记。
		return openAIWSSessionFallbackAction{reason: reason}
	}
}

// openAIWSSessionFallbackReason 从转发错误中取分类用的 reason 串（剥离 prewarm_ 前缀）。
func openAIWSSessionFallbackReason(err error) string {
	reason, _ := classifyOpenAIWSReconnectReason(err)
	return strings.TrimPrefix(reason, "prewarm_")
}

func (s *OpenAIGatewayService) openAIWSSessionDisableEnabled() bool {
	if s == nil || s.cfg == nil {
		return true
	}
	return s.cfg.Gateway.OpenAIWS.SessionDisableEnabled
}

func (s *OpenAIGatewayService) openAIWSSessionDisabledReason(scope openAIWSSessionScope) string {
	if s == nil || scope.AccountID <= 0 || scope.Session == "" || !s.openAIWSSessionDisableEnabled() {
		return ""
	}
	openAIWSSessionRegistryMu.Lock()
	defer openAIWSSessionRegistryMu.Unlock()
	state := s.openAIWSSessionStateLocked(scope)
	if state.reason != "" || state.prewarmStarted {
		s.storeOpenAIWSSessionStateLocked(scope, state)
	}
	return state.reason
}

func (s *OpenAIGatewayService) markOpenAIWSSessionDisabled(scope openAIWSSessionScope, reason string) {
	if s == nil || scope.AccountID <= 0 || scope.Session == "" || !s.openAIWSSessionDisableEnabled() {
		return
	}
	openAIWSSessionRegistryMu.Lock()
	defer openAIWSSessionRegistryMu.Unlock()
	state := s.openAIWSSessionStateLocked(scope)
	if state.reason == "" {
		state.reason, state.since = reason, time.Now()
	}
	s.storeOpenAIWSSessionStateLocked(scope, state)
}

// Anonymous sessions end with the request/connection that created their scope.
func (s *OpenAIGatewayService) clearOpenAIWSSessionDisabled(scope openAIWSSessionScope) {
	if s == nil || scope.AccountID <= 0 {
		return
	}
	openAIWSSessionRegistryMu.Lock()
	defer openAIWSSessionRegistryMu.Unlock()
	s.openaiWSSessionDisabled.Delete(scope)
}
