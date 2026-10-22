package service

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexFingerprintIDsContextKey 是暂存在 gin context 的收敛 ID 集合键。
// 由 Forward（非透传）或 forwardOpenAIPassthrough（透传）解析后写入，请求
// 构造器读取用于出站头改写——请求体与出站头必须共享同一份 IDs，保证
// turn_id 等随机字段一致。
const codexFingerprintIDsContextKey = "codex_fingerprint_ids"

// stageCodexFingerprintIDs 将本 attempt 解析出的收敛 ID 暂存到 gin context。
// 必须无条件覆写（含 nil）：failover 从收敛账号切到 off 账号时，上一账号的
// IDs 不得残留并被误应用到新账号的出站头（typed-nil 由应用侧 nil 守卫吸收）。
func stageCodexFingerprintIDs(c *gin.Context, ids *codexFingerprintIDs) {
	if c != nil {
		c.Set(codexFingerprintIDsContextKey, ids)
	}
}

func stagedCodexFingerprintIDs(c *gin.Context, account *Account) *codexFingerprintIDs {
	if c == nil || account == nil || !account.UsesOpenAICodexProtocol() {
		return nil
	}
	value, ok := c.Get(codexFingerprintIDsContextKey)
	if !ok {
		return nil
	}
	ids, ok := value.(*codexFingerprintIDs)
	if !ok || ids == nil || ids.accountID != account.ID {
		return nil
	}
	return ids
}

// applyStagedCodexFingerprintHeaders 读取 context 暂存的收敛 ID 并改写出站头。
// 非透传与透传两个请求构造器共用本函数，防止应用语义漂移。仅解析该
// snapshot 的 OAuth 账号可读取，避免 stale context 跨账号 failover 泄漏。
func applyStagedCodexFingerprintHeaders(c *gin.Context, account *Account, h http.Header) {
	applyCodexFingerprintHeaders(h, stagedCodexFingerprintIDs(c, account))
}

func applyStagedCodexFingerprintClientMetadata(c *gin.Context, account *Account, reqBody map[string]any) bool {
	return applyCodexFingerprintClientMetadata(reqBody, stagedCodexFingerprintIDs(c, account))
}

// codexFingerprintMode 控制 OAuth 账号出站请求的设备指纹收敛强度。
// 多人共享同一 OAuth 账号时，每个用户的 Codex 客户端会携带各自不同的
// installation_id / session_id / thread_id，上游据此判定设备数和会话数。
// 收敛模式将这些标识改写为账号级恒定值，减少上游可见的设备/会话指纹。
type codexFingerprintMode string

const (
	// codexFingerprintOff 不做任何收敛，原样透传客户端标识。
	// 必须在 extra 里显式写成 "off"；缺省/空/非法值按 full 处理。
	codexFingerprintOff codexFingerprintMode = "off"
	// codexFingerprintDevice 仅收敛 installation_id 为账号级恒定值。
	// 上游看到 1 台设备 + 多会话（每用户各自的 session）。
	codexFingerprintDevice codexFingerprintMode = "device"
	// codexFingerprintSession 收敛 installation_id + session_id，
	// thread_id 按客户端原始 session-id 确定性派生（每个真实 Codex 会话一个独立线程）。
	// 上游看到 1 台设备 + 1 会话 + N 线程，最接近正常用户 spawn 子代理的模式。
	codexFingerprintSession codexFingerprintMode = "session"
	// codexFingerprintFull 收敛所有标识：installation_id + session_id + thread_id。
	// 上游看到 1 台设备 + 1 会话 + 1 线程。这是缺省值（见 GetCodexFingerprintMode）。
	codexFingerprintFull codexFingerprintMode = "full"
)

const (
	codexFingerprintModeExtraKey = "codex_fingerprint_mode"
	codexFingerprintSeedExtraKey = "codex_fingerprint_seed"
)

func canonicalCodexFingerprintSeed(value any) (string, bool) {
	raw, ok := value.(string)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(raw)
	parsed, err := uuid.Parse(trimmed)
	if err != nil || parsed == uuid.Nil || trimmed != parsed.String() {
		return "", false
	}
	return trimmed, true
}

func newCodexFingerprintSeed() string {
	return uuid.NewString()
}

func stripCodexFingerprintSeed(extra map[string]any) map[string]any {
	if extra == nil {
		return nil
	}
	stripped := maps.Clone(extra)
	delete(stripped, codexFingerprintSeedExtraKey)
	return stripped
}

func codexFingerprintModeFromExtra(extra map[string]any) codexFingerprintMode {
	if extra == nil {
		return codexFingerprintFull
	}
	raw, _ := extra[codexFingerprintModeExtraKey].(string)
	switch mode := codexFingerprintMode(strings.TrimSpace(raw)); mode {
	case codexFingerprintOff, codexFingerprintDevice, codexFingerprintSession, codexFingerprintFull:
		return mode
	default:
		return codexFingerprintFull
	}
}

func codexFingerprintModeRequiresSeed(mode codexFingerprintMode) bool {
	switch mode {
	case codexFingerprintDevice, codexFingerprintSession, codexFingerprintFull:
		return true
	default:
		return false
	}
}

func codexFingerprintSeed(extra map[string]any) (string, bool) {
	if extra == nil {
		return "", false
	}
	return canonicalCodexFingerprintSeed(extra[codexFingerprintSeedExtraKey])
}

func prepareCodexFingerprintExtraForCreate(platform, accountType string, extra map[string]any) map[string]any {
	prepared := stripCodexFingerprintSeed(extra)
	if platform != PlatformOpenAI || (accountType != AccountTypeOAuth && accountType != AccountTypeSetupToken) || !codexFingerprintModeRequiresSeed(codexFingerprintModeFromExtra(prepared)) {
		return prepared
	}
	if prepared == nil {
		prepared = make(map[string]any, 1)
	}
	prepared[codexFingerprintSeedExtraKey] = newCodexFingerprintSeed()
	return prepared
}

func prepareCodexFingerprintExtraForUpdate(account *Account, extra map[string]any) map[string]any {
	prepared := stripCodexFingerprintSeed(extra)
	if account == nil || !account.IsOpenAIOAuthLike() {
		return prepared
	}
	if seed, ok := codexFingerprintSeed(account.Extra); ok {
		if prepared == nil {
			prepared = make(map[string]any, 1)
		}
		prepared[codexFingerprintSeedExtraKey] = seed
		return prepared
	}
	if codexFingerprintModeRequiresSeed(codexFingerprintModeFromExtra(prepared)) {
		if prepared == nil {
			prepared = make(map[string]any, 1)
		}
		prepared[codexFingerprintSeedExtraKey] = newCodexFingerprintSeed()
	}
	return prepared
}

func sanitizedCodexFingerprintExtraUpdates(updates map[string]any) map[string]any {
	if updates == nil {
		return nil
	}
	sanitized := maps.Clone(updates)
	delete(sanitized, codexFingerprintSeedExtraKey)
	return sanitized
}

// ShouldEnsureCodexFingerprintSeedForExtraUpdates reports whether a JSONB key-level
// extra update should atomically preserve or create the system-managed per-account
// seed. Missing mode is treated as full, so any extra patch except an explicit off
// mints or keeps a seed.
func ShouldEnsureCodexFingerprintSeedForExtraUpdates(updates map[string]any) bool {
	if updates == nil {
		return false
	}
	return codexFingerprintModeRequiresSeed(codexFingerprintModeFromExtra(updates))
}

// GetCodexFingerprintMode 从账号 extra JSON 读取指纹收敛模式。
//
// 缺省是完全收敛：未设置、空值或非法值一律按 full 处理。管理员必须把
// extra["codex_fingerprint_mode"] 显式写成 "off" 才会透传客户端标识。
//
// 历史：上游 v0.1.175（#5553）曾把缺省当作 session，#5610 又改成 opt-in off。
// 本 fork 默认打开完全收敛，并配合种子回填，让存量 OAuth / setup-token
// 账号在缺键时也走 full。
func (a *Account) GetCodexFingerprintMode() codexFingerprintMode {
	if a == nil || !a.IsOpenAIOAuthLike() {
		return codexFingerprintOff
	}
	return codexFingerprintModeFromExtra(a.Extra)
}

// deriveStableUUIDv4 从种子确定性派生一个 UUIDv4 格式的字符串。
// 同一种子永远返回同一值。
func deriveStableUUIDv4(seed string) string {
	h := sha256.Sum256([]byte(seed))
	b := h[:16]
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 1
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		binary.BigEndian.Uint32(b[0:4]),
		binary.BigEndian.Uint16(b[4:6]),
		binary.BigEndian.Uint16(b[6:8]),
		binary.BigEndian.Uint16(b[8:10]),
		b[10:16])
}

// resolveConvergedInstallationID 返回账号级恒定的 installation_id。
// 优先使用管理员配置的真实 device_id，无则从系统管理的账号随机种子确定性派生。
func resolveConvergedInstallationID(account *Account, seed string) string {
	if account == nil {
		return ""
	}
	if deviceID := account.GetOpenAIDeviceID(); deviceID != "" {
		return deviceID
	}
	if seed == "" {
		return ""
	}
	return deriveStableUUIDv4("sub2api:codex-install-id:v2:" + seed)
}

// resolveConvergedSessionID 返回账号级恒定的 session_id（session 模式的根会话，
// 以及 full 模式下完全没有会话信号时的兜底）。官方 session/thread id 是
// Uuid::now_v7()（protocol/src/session_id.rs、thread_id.rs），因此派生 v7，
// 时间戳取由种子确定的过去时刻。
func resolveConvergedSessionID(seed string) string {
	if seed == "" {
		return ""
	}
	key := "sub2api:codex-session-id:v3:" + seed
	return deriveStableCodexUUIDv7(key, deriveCodexStableTimestampMs(key))
}

// resolveConvergedThreadID 按客户端会话键确定性派生 thread_id（v7）。
// 每个下游会话获得一个独立线程，模拟正常用户开多窗口的模式。
func resolveConvergedThreadID(seed, clientSessionID string) string {
	if seed == "" || clientSessionID == "" {
		return ""
	}
	return resolveCodexSessionIdentity(defaultCodexTurnClock, seed+":thread", clientSessionID)
}

// codexFingerprintIDs 收敛后的完整 ID 集合。
// 由 resolveCodexFingerprintIDs 一次性生成，同一个实例在头改写和体改写之间共享，
// 确保所有载体中的 turn_id 等随机字段一致。体改写时还会补记原始
// client_metadata.session_id，用于识别 root prompt_cache_key 的默认值。
type codexFingerprintIDs struct {
	accountID                     int64
	mode                          codexFingerprintMode
	installationID                string
	sessionID                     string
	threadID                      string
	turnID                        string
	windowID                      string
	windowNumber                  uint64
	turnStartedAtUnixMs           int64
	originalBodySessionID         string
	originalBodySessionIDCaptured bool
	convergence                   bool
	scopedClientSessionID         string
	// sessionPromptCacheKey is the body prompt_cache_key the session id was
	// derived from (full mode, no client session id). Official Codex sends
	// prompt_cache_key == session_id, so that key is rewritten to sessionID.
	sessionPromptCacheKey string
}

// resolveCodexFingerprintIDs 按收敛模式计算出站 ID 集合。
// clientSessionID 是客户端原始的 session-id 头值（连字符形式），用于 session 模式下
// 的 thread_id 派生——每个真实 Codex 会话得到一个独立线程。
// 返回 nil 表示 off 模式，不需要改写。
// 注意：包含随机生成的 turn_id，调用方必须只调用一次并共享结果给头改写和体改写。
func resolveCodexFingerprintIDs(account *Account, clientSessionID string, mode codexFingerprintMode) *codexFingerprintIDs {
	return resolveCodexFingerprintIDsWithWindow(account, clientSessionID, mode, "", codexTurnEvidence{})
}

// resolveCodexFingerprintIDsWithWindow 计算一次请求的出站身份。
//
// 多用户中转的映射：一个 OAuth 账号 = 一台设备（installation_id 恒定），一个下游
// 会话 = 该设备上的一个 Codex 线程（session_id = thread_id，v7，按会话稳定），一个
// 用户回合 = 一个 turn（turn_id / turn_started_at 在工具循环内不变）。旧 full 模式把
// 账号下所有用户压成同一个 session/thread 并给每个请求新 turn_id——多条并发连接
// 带同一线程 id、同一回合里 turn_id 变化，都是官方客户端不可能产生的形态。
func resolveCodexFingerprintIDsWithWindow(account *Account, clientSessionID string, mode codexFingerprintMode, clientWindowNumber string, ev codexTurnEvidence) *codexFingerprintIDs {
	if account == nil || mode == codexFingerprintOff {
		return nil
	}
	seed, ok := codexFingerprintSeed(account.Extra)
	if !ok {
		return nil
	}

	ids := &codexFingerprintIDs{
		accountID:   account.ID,
		mode:        mode,
		convergence: codexFingerprintConvergenceEnabled(account),
	}

	ids.installationID = resolveConvergedInstallationID(account, seed)
	if ids.installationID == "" {
		return nil
	}
	ids.windowNumber, _ = strconv.ParseUint(codexWindowNumberOrDefault(clientWindowNumber), 10, 64)
	windowNumber := strconv.FormatUint(ids.windowNumber, 10)

	// Conversation key: the client's own session id, else its prompt_cache_key.
	sessionKey := strings.TrimSpace(clientSessionID)
	fromPromptCacheKey := false
	if sessionKey == "" && ev.PromptCacheKey != "" && !codexConvergencePromptCacheKeyPattern.MatchString(ev.PromptCacheKey) {
		sessionKey = ev.PromptCacheKey
		fromPromptCacheKey = true
	}

	switch mode {
	case codexFingerprintDevice:
		return ids

	case codexFingerprintSession:
		ids.sessionID = resolveConvergedSessionID(seed)
		ids.threadID = resolveConvergedThreadID(seed, sessionKey)
		if ids.threadID == "" {
			ids.threadID = ids.sessionID
		}
		ids.turnID, ids.turnStartedAtUnixMs = resolveCodexTurnIdentity(defaultCodexTurnClock, seed, ids.threadID, ev)
		ids.windowID = ids.threadID + ":" + windowNumber
		return ids

	case codexFingerprintFull:
		if sessionKey != "" {
			ids.sessionID = resolveCodexSessionIdentity(defaultCodexTurnClock, seed, sessionKey)
			if fromPromptCacheKey {
				ids.sessionPromptCacheKey = sessionKey
			}
		} else {
			ids.sessionID = resolveConvergedSessionID(seed)
		}
		// Root threads: thread_id == session_id (golden 0.163.0).
		ids.threadID = ids.sessionID
		ids.turnID, ids.turnStartedAtUnixMs = resolveCodexTurnIdentity(defaultCodexTurnClock, seed, ids.threadID, ev)
		ids.windowID = ids.threadID + ":" + windowNumber
		return ids
	}

	return nil
}

// extractClientSessionID 从请求头中提取客户端原始的会话标识。
// 优先取 session-id（连字符形式，Codex CLI 标准），回退到 session_id（下划线形式）。
// 返回的值尚未被 isolateOpenAISessionID 改写，是客户端的真实标识。
func extractClientSessionID(h http.Header) string {
	if v := strings.TrimSpace(h.Get("session-id")); v != "" {
		return v
	}
	return strings.TrimSpace(h.Get("session_id"))
}

// resolveCodexFingerprintIDsFromRequest 从客户端原始请求头中提取 session-id，
// 结合账号配置一次性解析收敛 ID 集合。调用方应将返回的 ids 同时传给
// applyCodexFingerprintHeaders 和 applyCodexFingerprintClientMetadata。
func resolveCodexFingerprintIDsFromRequest(account *Account, clientHeaders http.Header) *codexFingerprintIDs {
	return resolveCodexFingerprintIDsWithBody(nil, account, clientHeaders, nil)
}

func resolveCodexFingerprintIDsWithBody(c *gin.Context, account *Account, clientHeaders http.Header, rawClientMetadata any) *codexFingerprintIDs {
	if clientHeaders == nil && c != nil && c.Request != nil {
		clientHeaders = c.Request.Header
	}
	return resolveCodexFingerprintIDsWithEvidence(c, account, clientHeaders, rawClientMetadata, collectCodexTurnEvidence(rawClientMetadata, clientHeaders, gjson.Result{}))
}

// resolveCodexFingerprintIDsForRawBody resolves IDs for a raw JSON body
// (passthrough / WS frames), with turn evidence from client_metadata,
// prompt_cache_key and input.
func resolveCodexFingerprintIDsForRawBody(c *gin.Context, account *Account, clientHeaders http.Header, body []byte) *codexFingerprintIDs {
	if clientHeaders == nil && c != nil && c.Request != nil {
		clientHeaders = c.Request.Header
	}
	parsed := gjson.ParseBytes(body)
	cm := parsed.Get("client_metadata")
	return resolveCodexFingerprintIDsWithEvidence(c, account, clientHeaders, cm, collectCodexTurnEvidence(cm, clientHeaders, parsed))
}

// resolveCodexFingerprintIDsForDecodedBody resolves IDs for a decoded body.
func resolveCodexFingerprintIDsForDecodedBody(c *gin.Context, account *Account, clientHeaders http.Header, decoded map[string]any) *codexFingerprintIDs {
	if clientHeaders == nil && c != nil && c.Request != nil {
		clientHeaders = c.Request.Header
	}
	var cm any
	if decoded != nil {
		cm = decoded["client_metadata"]
	}
	return resolveCodexFingerprintIDsWithEvidence(c, account, clientHeaders, cm, collectCodexTurnEvidenceFromMap(decoded, clientHeaders))
}

func resolveCodexFingerprintIDsWithEvidence(c *gin.Context, account *Account, clientHeaders http.Header, rawClientMetadata any, ev codexTurnEvidence) *codexFingerprintIDs {
	if account == nil {
		return nil
	}
	mode := account.GetCodexFingerprintMode()
	if mode == codexFingerprintOff {
		return nil
	}
	if clientHeaders == nil && c != nil && c.Request != nil {
		clientHeaders = c.Request.Header
	}
	clientSessionID := ""
	source := codexAccountIdentitySource(c, account)
	convergence := codexFingerprintConvergenceEnabled(source)
	if clientHeaders != nil {
		clientSessionID = extractClientSessionID(clientHeaders)
	}
	if clientSessionID == "" && convergence {
		clientSessionID = codexFingerprintSessionEvidence(rawClientMetadata, true)
	}
	if clientSessionID == "" && convergence && clientHeaders != nil {
		clientSessionID = codexConvergenceMetadataString(gjson.Parse(clientHeaders.Get(openAIWSTurnMetadataHeader)), "session_id")
	}
	clientWindowNumber := codexFingerprintWindowNumberEvidence(rawClientMetadata)
	if clientWindowNumber == "" && clientHeaders != nil {
		clientWindowNumber = extractClientWindowNumber(clientHeaders)
	}
	ids := resolveCodexFingerprintIDsWithWindow(account, clientSessionID, mode, clientWindowNumber, ev)
	if ids == nil {
		return nil
	}
	ids.convergence = convergence
	if convergence && clientSessionID != "" && c != nil {
		ids.scopedClientSessionID = scopeCodexAccountIdentityValue(
			source, getAPIKeyIDFromContext(c), "session", clientSessionID)
	}
	return ids
}

func codexWindowNumberOrDefault(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !codexWindowNumberPattern.MatchString(raw) {
		return "0"
	}
	return raw
}

func extractClientWindowNumber(h http.Header) string {
	for _, name := range [...]string{"x-codex-window-id", "window_id"} {
		if m := codexConvergenceWindowIDPattern.FindStringSubmatch(strings.TrimSpace(h.Get(name))); m != nil {
			return m[2]
		}
	}
	meta := gjson.Parse(h.Get(openAIWSTurnMetadataHeader))
	if number := codexWindowNumberFromJSON(meta.Get("window_number")); number != "" {
		return number
	}
	if m := codexConvergenceWindowIDPattern.FindStringSubmatch(meta.Get("window_id").String()); m != nil {
		return m[2]
	}
	return ""
}

// applyCodexFingerprintHeaders 按预计算的收敛 ID 改写出站 HTTP 头中的设备指纹。
// 在 buildUpstreamRequest 的白名单透传之后、enforceCodexIdentityHeaders 之前调用。
func applyCodexFingerprintHeaders(h http.Header, ids *codexFingerprintIDs) {
	if h == nil || ids == nil {
		return
	}

	// 所有非 off 模式都收敛 installation_id
	h.Set("x-codex-installation-id", ids.installationID)

	if ids.mode == codexFingerprintDevice {
		rewriteCodexTurnMetadataFields(h, map[string]any{
			"installation_id": ids.installationID,
		}, ids)
		return
	}

	// session / full 模式：改写所有相关头
	h.Set("x-codex-window-id", ids.windowID)
	h.Set("x-client-request-id", ids.threadID)
	// 连字符形式和下划线形式都改写，保证一致
	h.Set("session-id", ids.sessionID)
	h.Set("session_id", ids.sessionID)
	h.Set("thread-id", ids.threadID)

	rewriteCodexTurnMetadataFields(h, map[string]any{
		"installation_id":         ids.installationID,
		"session_id":              ids.sessionID,
		"thread_id":               ids.threadID,
		"turn_id":                 ids.turnID,
		"window_id":               ids.windowID,
		"window_number":           ids.windowNumber,
		"turn_started_at_unix_ms": ids.turnStartedAtUnixMs,
	}, ids)
}

// rewriteCodexTurnMetadataFields 解析 x-codex-turn-metadata 头中的 JSON，
// 替换指定字段后回写。合法对象保留未指定字段（如 sandbox、thread_source）；
// 非法/非对象值重建为最小合法 metadata，避免 flat 与 embedded identity 分裂。
func rewriteCodexTurnMetadataFields(h http.Header, fields map[string]any, ids *codexFingerprintIDs) {
	raw := h.Get(openAIWSTurnMetadataHeader)
	if strings.TrimSpace(raw) == "" {
		return
	}
	h.Set(openAIWSTurnMetadataHeader, rewriteCodexFingerprintTurnMetadata(raw, fields, ids))
}

// codexSandboxTagForOutboundPlatform maps a client sandbox tag to the one the
// outbound identity's platform would report (codex-rs protocol/src/sandbox.rs:
// seatbelt = macOS, seccomp = Linux, windows_* = Windows). Converged accounts
// present one device; a macOS client's "seatbelt" next to a Linux User-Agent
// is a combination no real client sends. "none"/"external" are kept.
func codexSandboxTagForOutboundPlatform(tag string) (string, bool) {
	tag = strings.TrimSpace(tag)
	if tag == "" || tag == "none" || tag == "external" {
		return "", false
	}
	ua := strings.ToLower(resolveCodexOutboundIdentity("").userAgent)
	want := ""
	switch {
	case strings.Contains(ua, "mac os"):
		want = "seatbelt"
	case strings.Contains(ua, "windows"):
		return "", false // several Windows tags exist; keep the client's
	default:
		want = "seccomp"
	}
	if tag == want {
		return "", false
	}
	return want, true
}

func rewriteCodexFingerprintTurnMetadata(raw string, fields map[string]any, ids *codexFingerprintIDs) string {
	return rewriteCodexTurnMetadataJSON(raw, true, func(metadata map[string]any) map[string]any {
		updates := maps.Clone(fields)
		if tag, ok := metadata["sandbox"].(string); ok {
			if next, changed := codexSandboxTagForOutboundPlatform(tag); changed {
				updates["sandbox"] = next
			}
		}
		root, _ := metadata["root_turn_id"].(string)
		preserveCodexConvergenceRootTurn(metadata, ids)
		if next, _ := metadata["root_turn_id"].(string); next != root {
			updates["root_turn_id"] = next
		}
		return updates
	})
}

// applyCodexFingerprintClientMetadata 按预计算的收敛 ID 改写请求体中的 client_metadata。
// 使用与头改写相同的 ids 实例，确保 turn_id 等随机字段一致。
func applyCodexFingerprintClientMetadata(reqBody map[string]any, ids *codexFingerprintIDs) bool {
	if reqBody == nil || ids == nil {
		return false
	}

	captureCodexFingerprintOriginalBodySessionID(ids, reqBody["client_metadata"])
	existing, _ := reqBody["client_metadata"].(map[string]any)
	if existing == nil {
		existing = make(map[string]any)
	}

	modified := false
	if applyCodexFingerprintToClientMetadataMap(existing, ids) {
		reqBody["client_metadata"] = existing
		modified = true
	}
	if applyCodexFingerprintPromptCacheKey(reqBody, ids) {
		modified = true
	}
	return modified
}

// applyCodexFingerprintToClientMetadataMap 是 client_metadata 改写的共享核心，
// map 版（非透传，body 已解码）与 raw 字节版（透传热路径）都经由它，保证两条
// 路径的收敛语义永不漂移。
func applyCodexFingerprintToClientMetadataMap(existing map[string]any, ids *codexFingerprintIDs) bool {
	if existing == nil || ids == nil {
		return false
	}

	modified := false

	if ids.installationID != "" {
		existing["x-codex-installation-id"] = ids.installationID
		modified = true
	}

	if ids.mode == codexFingerprintDevice {
		rewriteClientMetadataEmbeddedTurnMetadata(existing, map[string]any{
			"installation_id": ids.installationID,
		}, ids)
		return modified
	}

	// session / full 模式
	preserveCodexConvergenceRootTurn(existing, ids)
	existing["session_id"] = ids.sessionID
	existing["thread_id"] = ids.threadID
	existing["turn_id"] = ids.turnID
	existing["x-codex-window-id"] = ids.windowID

	rewriteClientMetadataEmbeddedTurnMetadata(existing, map[string]any{
		"installation_id":         ids.installationID,
		"session_id":              ids.sessionID,
		"thread_id":               ids.threadID,
		"turn_id":                 ids.turnID,
		"window_id":               ids.windowID,
		"window_number":           ids.windowNumber,
		"turn_started_at_unix_ms": ids.turnStartedAtUnixMs,
	}, ids)
	return true
}

func captureCodexFingerprintOriginalBodySessionID(ids *codexFingerprintIDs, clientMetadata any) {
	if ids == nil || ids.originalBodySessionIDCaptured {
		return
	}
	ids.originalBodySessionIDCaptured = true
	ids.originalBodySessionID = codexFingerprintSessionEvidence(clientMetadata, ids.convergence)
}

func codexFingerprintSessionEvidence(clientMetadata any, allowEmbedded bool) string {
	var session, embedded string
	switch metadata := clientMetadata.(type) {
	case map[string]any:
		session, _ = metadata["session_id"].(string)
		embedded, _ = metadata[openAIWSTurnMetadataHeader].(string)
	case map[string]string:
		session = metadata["session_id"]
		embedded = metadata[openAIWSTurnMetadataHeader]
	case gjson.Result:
		session = codexConvergenceMetadataString(metadata, "session_id")
		embedded = codexConvergenceMetadataString(metadata, openAIWSTurnMetadataHeader)
	}
	if session = strings.TrimSpace(session); session != "" {
		return session
	}
	if allowEmbedded {
		return codexConvergenceMetadataString(gjson.Parse(embedded), "session_id")
	}
	return ""
}

var codexWindowNumberPattern = regexp.MustCompile(`^[0-9]{1,19}$`)

func codexWindowNumberFromJSON(value gjson.Result) string {
	var raw string
	switch value.Type {
	case gjson.Number:
		raw = value.Raw
	case gjson.String:
		raw = strings.TrimSpace(value.Str)
	}
	if codexWindowNumberPattern.MatchString(raw) {
		return raw
	}
	return ""
}

func codexFingerprintWindowNumberEvidence(clientMetadata any) string {
	var window, embedded string
	switch metadata := clientMetadata.(type) {
	case map[string]any:
		window, _ = metadata["x-codex-window-id"].(string)
		embedded, _ = metadata[openAIWSTurnMetadataHeader].(string)
	case map[string]string:
		window = metadata["x-codex-window-id"]
		embedded = metadata[openAIWSTurnMetadataHeader]
	case gjson.Result:
		window = codexConvergenceMetadataString(metadata, "x-codex-window-id")
		embedded = codexConvergenceMetadataString(metadata, openAIWSTurnMetadataHeader)
	}
	if m := codexConvergenceWindowIDPattern.FindStringSubmatch(strings.TrimSpace(window)); m != nil {
		return m[2]
	}
	meta := gjson.Parse(embedded)
	if number := codexWindowNumberFromJSON(meta.Get("window_number")); number != "" {
		return number
	}
	if m := codexConvergenceWindowIDPattern.FindStringSubmatch(meta.Get("window_id").String()); m != nil {
		return m[2]
	}
	return ""
}

func captureCodexFingerprintOriginalBodySessionIDRaw(ids *codexFingerprintIDs, value gjson.Result) {
	if ids == nil || ids.originalBodySessionIDCaptured {
		return
	}
	ids.originalBodySessionIDCaptured = true
	if value.Exists() && value.Type == gjson.String {
		ids.originalBodySessionID = strings.TrimSpace(value.String())
	}
}

func shouldRewriteCodexFingerprintPromptCacheKey(ids *codexFingerprintIDs, promptCacheKey string) bool {
	if ids == nil || ids.sessionID == "" {
		return false
	}
	if ids.mode != codexFingerprintSession && ids.mode != codexFingerprintFull {
		return false
	}
	if ids.convergence && codexConvergencePromptCacheKeyPattern.MatchString(strings.TrimSpace(promptCacheKey)) {
		return false
	}
	// The session id was derived from this very key: official Codex sends
	// prompt_cache_key == session_id.
	if ids.sessionPromptCacheKey != "" && strings.TrimSpace(promptCacheKey) == ids.sessionPromptCacheKey {
		return true
	}
	if ids.originalBodySessionIDCaptured && ids.originalBodySessionID != "" {
		return promptCacheKey == ids.originalBodySessionID
	}
	if !ids.convergence {
		return false
	}
	if ids.scopedClientSessionID != "" {
		return promptCacheKey == ids.scopedClientSessionID
	}
	return false
}

func applyCodexFingerprintPromptCacheKey(reqBody map[string]any, ids *codexFingerprintIDs) bool {
	if reqBody == nil {
		return false
	}
	promptCacheKey, ok := reqBody["prompt_cache_key"].(string)
	if !ok || strings.TrimSpace(promptCacheKey) == "" || !shouldRewriteCodexFingerprintPromptCacheKey(ids, promptCacheKey) {
		return false
	}
	if promptCacheKey == ids.sessionID {
		return false
	}
	reqBody["prompt_cache_key"] = ids.sessionID
	return true
}

// applyCodexFingerprintClientMetadataRaw 在原始 JSON 字节上改写 client_metadata，
// 供透传路径使用——透传是热路径，禁止对可能高达数十 MB 的 body 做全量
// Unmarshal（见 forwardOpenAIPassthrough 的轻量提取注释）。实现为：gjson 提取
// client_metadata 小对象单独解码，经共享核心改写后 sjson 一次性拼回，body
// 其余字节原样保留；root prompt_cache_key 仅在可证明是 body session 默认值时
// 做标量改写。语义与 applyCodexFingerprintClientMetadata 逐点一致（含
// "非对象值整体替换为收敛集合"的行为）。
func applyCodexFingerprintClientMetadataRaw(body []byte, ids *codexFingerprintIDs) ([]byte, bool, error) {
	if len(body) == 0 || ids == nil {
		return body, false, nil
	}
	// 非 JSON 对象的 body（数组/标量/畸形）没有 client_metadata 语义，
	// sjson 在这类根上写字段会改写整体结构，直接放行保持原样。
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		captureCodexFingerprintOriginalBodySessionIDRaw(ids, gjson.Result{})
		return body, false, nil
	}

	existing := map[string]any{}
	if cm := gjson.GetBytes(body, "client_metadata"); cm.IsObject() {
		captureCodexFingerprintOriginalBodySessionIDRaw(ids, gjson.GetBytes(body, "client_metadata.session_id"))
		if err := json.Unmarshal([]byte(cm.Raw), &existing); err != nil {
			return body, false, fmt.Errorf("decode client_metadata for fingerprint: %w", err)
		}
	} else {
		captureCodexFingerprintOriginalBodySessionIDRaw(ids, gjson.Result{})
	}

	next := body
	modified := false
	if applyCodexFingerprintToClientMetadataMap(existing, ids) {
		raw, err := json.Marshal(existing)
		if err != nil {
			return body, false, fmt.Errorf("encode converged client_metadata: %w", err)
		}
		var setErr error
		next, setErr = sjson.SetRawBytes(body, "client_metadata", raw)
		if setErr != nil {
			return body, false, fmt.Errorf("splice converged client_metadata: %w", setErr)
		}
		modified = true
	}
	promptCacheKey := gjson.GetBytes(body, "prompt_cache_key")
	if promptCacheKey.Exists() && promptCacheKey.Type == gjson.String && strings.TrimSpace(promptCacheKey.String()) != "" && shouldRewriteCodexFingerprintPromptCacheKey(ids, promptCacheKey.String()) {
		rewritten, err := sjson.SetBytes(next, "prompt_cache_key", ids.sessionID)
		if err != nil {
			return body, false, fmt.Errorf("splice converged prompt_cache_key: %w", err)
		}
		next = rewritten
		modified = true
	}
	return next, modified, nil
}

// rewriteClientMetadataEmbeddedTurnMetadata 改写 client_metadata 中内嵌的
// x-codex-turn-metadata JSON 字符串里的指定字段。非法/非对象值会重建，
// 避免 flat client_metadata 与 embedded metadata 暴露两套身份。
func rewriteClientMetadataEmbeddedTurnMetadata(clientMetadata map[string]any, fields map[string]any, ids *codexFingerprintIDs) {
	raw, ok := clientMetadata["x-codex-turn-metadata"].(string)
	if !ok || raw == "" {
		return
	}
	clientMetadata["x-codex-turn-metadata"] = rewriteCodexFingerprintTurnMetadata(raw, fields, ids)
}

// codexStableUUIDv7BaseMs 是确定性 v7 时间戳的锚点（2026-06-01T00:00:00Z）。
// 官方 thread_id/session_id/window_id 都是 Uuid::now_v7()（codex-rs
// protocol/src/thread_id.rs:30、core/src/state/auto_compact_window.rs:13），
// 高 48 位是创建时刻的毫秒时间戳。收敛 ID 必须跨重启稳定，不能用当前时间，
// 故以固定锚点为上界、由种子哈希向前散列 200 天——时间戳恒为过去的合法
// 「创建时刻」。官方 Uuid::now_v7() 永不产生未来时间戳，落在未来的 v7 与
// turn_started_at / 请求到达时间交叉验证即可被识别为合成 ID。
const codexStableUUIDv7BaseMs = 1780272000000 // 2026-06-01T00:00:00Z
const codexStableUUIDv7SpanMs = 200 * 24 * 60 * 60 * 1000

// deriveStableCodexUUIDv7 从种子确定性派生一个 UUIDv7 格式的字符串，
// 时间戳位精确取 anchorMs（调用方保证是合理的过去创建时刻）。唯一性由
// 种子哈希的低 62 位随机字段保证，不靠散列时间戳——散列时间戳会让
// turn_id 的时刻偏离同一请求里的 turn_started_at_unix_ms，反而造出官方
// 不存在的「turn 创建时刻早于开始时刻数月」的矛盾。
func deriveStableCodexUUIDv7(seed string, anchorMs int64) string {
	h := sha256.Sum256([]byte(seed))
	ms := uint64(anchorMs)
	b := make([]byte, 16)
	binary.BigEndian.PutUint64(b[0:8], ms<<16)
	copy(b[6:], h[6:8])
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	copy(b[8:], h[8:16])
	b[8] = (b[8] & 0x3f) | 0x80 // variant 1
	return formatCodexUUID(b)
}

// deriveCodexStableTimestampMs 由种子确定一个稳定的毫秒时间戳，
// 供 session/thread/window 这类跨重启恒定的 v7 ID 使用。散列方向朝锚点
// 之前（base − [0,200d)），结果恒在过去。
func deriveCodexStableTimestampMs(seed string) int64 {
	h := sha256.Sum256([]byte("sub2api:codex-v7-ts:v1:" + seed))
	return codexStableUUIDv7BaseMs - int64(binary.BigEndian.Uint16(h[0:2]))*codexStableUUIDv7SpanMs/65536
}

func formatCodexUUID(b []byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		binary.BigEndian.Uint32(b[0:4]),
		binary.BigEndian.Uint16(b[4:6]),
		binary.BigEndian.Uint16(b[6:8]),
		binary.BigEndian.Uint16(b[8:10]),
		b[10:16])
}
