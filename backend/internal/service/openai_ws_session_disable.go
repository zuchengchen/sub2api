package service

import (
	"strings"
	"time"
)

// 官方 Codex CLI 的传输回退语义（codex-rs core/src/client.rs）：
// provider.supports_websockets 默认开启、每请求优先 WS；两类触发会置位
// 会话级 disable_websockets，会话余生全部走 HTTP——
//   1. WS 握手收到 426 Upgrade Required（能力信号）
//   2. 流式重试预算耗尽（stream_max_retries=5）
//
// 网关中「会话」没有直接的进程对应物：本文件把它映射为账号级内存标记 +
// 分档 TTL 的自动重探。标记到期后下一个请求重新尝试 WS（正常 acquire +
// prewarm 即探测）；探测失败按新原因重新标记，成功则清除。
//
// 分类原则与官方一致：能力信号与「重试预算耗尽的可重试错误」说明该账号
// 的 WS 传输当前不可用，标记禁用；本地连接池争用（acquire/queue）不是
// 上游健康问题，只让本请求透明回退、不污染账号状态；请求内容级与凭据级
// 错误（policy/auth/dead-anchor/invalid-encrypted）在 HTTP 上会以同形失败，
// 不触发回退也不标记。

// openAIWSSessionDisableState 记录一次账号级 WS 禁用。
type openAIWSSessionDisableState struct {
	reason string
	since  time.Time
	until  time.Time
}

// openAIWSSessionFallbackAction 描述一次 WS 失败应采取的会话级动作。
type openAIWSSessionFallbackAction struct {
	// fallbackHTTP: 本请求可在未写下游的前提下透明改走 HTTP 重试。
	fallbackHTTP bool
	// markDisable: 该失败说明账号 WS 传输当前不健康，标记会话级禁用。
	markDisable bool
	// capability: 能力信号（426/ws_unsupported），使用更长的禁用 TTL。
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

// openAIWSSessionDisableTTL 按禁用类别返回 TTL；<=0 的配置回落到内置默认。
func (s *OpenAIGatewayService) openAIWSSessionDisableTTL(capability bool) time.Duration {
	ms := 0
	if s != nil && s.cfg != nil {
		if capability {
			ms = s.cfg.Gateway.OpenAIWS.SessionDisableCapabilityTTLMS
		} else {
			ms = s.cfg.Gateway.OpenAIWS.SessionDisableRetryTTLMS
		}
	}
	if ms <= 0 {
		if capability {
			ms = 24 * 60 * 60 * 1000 // 24h
		} else {
			ms = 15 * 60 * 1000 // 15min
		}
	}
	return time.Duration(ms) * time.Millisecond
}

// openAIWSSessionDisabledReason 返回账号当前的 WS 禁用原因（"" = 未禁用）。
// 到期条目惰性清除：到期即视为「自动重探窗口已开」，下一个请求可正常尝试 WS。
func (s *OpenAIGatewayService) openAIWSSessionDisabledReason(accountID int64) string {
	if s == nil || accountID <= 0 || !s.openAIWSSessionDisableEnabled() {
		return ""
	}
	raw, ok := s.openaiWSSessionDisabled.Load(accountID)
	if !ok || raw == nil {
		return ""
	}
	state, ok := raw.(openAIWSSessionDisableState)
	if !ok || state.until.IsZero() {
		s.openaiWSSessionDisabled.Delete(accountID)
		return ""
	}
	if time.Now().Before(state.until) {
		return state.reason
	}
	s.openaiWSSessionDisabled.Delete(accountID)
	return ""
}

// markOpenAIWSSessionDisabled 标记账号 WS 会话级禁用；禁用时长按原因分档。
func (s *OpenAIGatewayService) markOpenAIWSSessionDisabled(accountID int64, reason string, capability bool) {
	if s == nil || accountID <= 0 || !s.openAIWSSessionDisableEnabled() {
		return
	}
	s.openaiWSSessionDisabled.Store(accountID, openAIWSSessionDisableState{
		reason: reason,
		since:  time.Now(),
		until:  time.Now().Add(s.openAIWSSessionDisableTTL(capability)),
	})
}

// clearOpenAIWSSessionDisabled 在 WS 成功后清除账号禁用标记。
func (s *OpenAIGatewayService) clearOpenAIWSSessionDisabled(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	s.openaiWSSessionDisabled.Delete(accountID)
}
