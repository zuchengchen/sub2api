package service

import (
	"crypto/sha256"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// openAICodexTurnStateHeader 是 Codex 的回合状态头。上游在响应头中铸造该
// 不透明 blob，客户端在同一回合的后续请求中原样回带（codex-rs 侧从
// /responses SSE、/responses/compact JSON 与 WS 握手三种响应中捕获，见
// codex-api/src/sse/responses.rs 与 endpoint/compact.rs）。
const openAICodexTurnStateHeader = "x-codex-turn-state"

// turn-state blob 是上游在"出站身份"（含 #5553 指纹收敛改写后的
// installation/session/thread 标识）下铸造的，同账号回放自洽；跨账号回放
// （failover 换号后客户端仍回带旧账号的 blob）是代理链独有、真实 Codex
// 永远不会产生的矛盾信号。溯源表按下游会话和实际 blob 的摘要记录账号，
// 并发线程或较晚提交的响应不得覆盖另一 blob 的来源。
type openAICodexTurnStateOrigin struct {
	accountID  int64
	oauthOwner openAIOAuthOwner
	expiresAt  time.Time
}

func openAICodexTurnStateOriginKey(seed, state string) [sha256.Size]byte {
	return sha256.Sum256([]byte(seed + "\x00" + state))
}

// openAICodexTurnStateSeed 返回溯源表键：API Key + 客户端原始会话标识。
// 客户端会话标识取自请求头（与指纹收敛的 thread 派生同源，见
// extractClientSessionID），确保同一下游会话的记录/守卫两侧使用同一键。
// 无会话标识时返回空串，表示不做跟踪（保持透传现状）。
func openAICodexTurnStateSeed(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	sessionID := extractClientSessionID(c.Request.Header)
	if sessionID == "" {
		return ""
	}
	return strconv.FormatInt(getAPIKeyIDFromContext(c), 10) + "\x00" + sessionID
}

// relayOpenAICodexTurnState 将上游响应中的 turn-state 显式写入下游响应头，
// 并记录铸造账号。必须在响应头提交点调用（WriteHeader 之前、且确认本次
// 上游响应就是将要写回客户端的响应之后）。上游无该头时主动清除 writer 上
// 可能残留的上一 failover attempt 的值——否则换号后旧账号的 blob 会粘到
// 新账号的响应上，这正是本文件要防止的跨账号矛盾。
func (s *OpenAIGatewayService) relayOpenAICodexTurnState(c *gin.Context, account *Account, upstream http.Header) {
	s.observeOpenAICodexTicketResponseHeader(c, upstream)
	if c == nil || c.Writer == nil {
		return
	}
	canonical := http.CanonicalHeaderKey(openAICodexTurnStateHeader)
	state := extractOpenAICodexTurnState(upstream)
	if state == "" {
		c.Writer.Header().Del(canonical)
		return
	}
	c.Writer.Header().Set(canonical, state)
	s.noteOpenAICodexTurnStateProvenance(c, account, state)
}

// stageOpenAICodexTurnState 将上游 turn-state 暂存到延迟提交的响应头集合
// （首输出守卫路径先缓存头、见到首个输出事件才提交）。此处**不**记录铸造
// 账号：该 attempt 仍可能在首输出超时后 failover，暂存头会被整体丢弃，
// 客户端从未收到该 blob。溯源必须在真正提交时记录，见
// noteStagedOpenAICodexTurnStateCommitted。
func stageOpenAICodexTurnState(dst *http.Header, upstream http.Header) {
	if dst == nil {
		return
	}
	canonical := http.CanonicalHeaderKey(openAICodexTurnStateHeader)
	state := extractOpenAICodexTurnState(upstream)
	if state == "" {
		if *dst != nil {
			dst.Del(canonical)
		}
		return
	}
	if *dst == nil {
		*dst = http.Header{}
	}
	dst.Set(canonical, state)
}

// noteStagedOpenAICodexTurnStateCommitted 在暂存响应头真正写入下游时记录
// 铸造账号——只有此刻客户端才确定收到了该 blob，溯源表才与客户端持有的
// 值一致（否则被 failover 丢弃的 attempt 会污染溯源，导致后续误剥离）。
func (s *OpenAIGatewayService) noteStagedOpenAICodexTurnStateCommitted(c *gin.Context, account *Account, staged http.Header) {
	if staged == nil || strings.TrimSpace(staged.Get(openAICodexTurnStateHeader)) == "" {
		return
	}
	s.noteOpenAICodexTurnStateProvenance(c, account, extractOpenAICodexTurnState(staged))
}

func extractOpenAICodexTurnState(upstream http.Header) string {
	if upstream == nil {
		return ""
	}
	return strings.TrimSpace(upstream.Get(openAICodexTurnStateHeader))
}

// noteOpenAICodexTurnStateProvenance retains no raw opaque state in its map.
func (s *OpenAIGatewayService) noteOpenAICodexTurnStateProvenance(c *gin.Context, account *Account, state string) {
	if s == nil || account == nil || account.ID <= 0 || state == "" {
		return
	}
	seed := openAICodexTurnStateSeed(c)
	if seed == "" {
		return
	}
	s.openaiCodexTurnStateOrigins.Store(openAICodexTurnStateOriginKey(seed, state), openAICodexTurnStateOrigin{
		accountID:  account.ID,
		oauthOwner: openAIOAuthOwnerForAccount(account),
		expiresAt:  time.Now().Add(s.openAIWSSessionStickyTTL()),
	})
	s.sweepOpenAICodexTurnStateOrigins()
}

// Codex learns WebSocket turn state from response.metadata.headers. Register
// it only after this event was successfully delivered to the downstream peer.
func (s *OpenAIGatewayService) noteOpenAICodexTurnStateEvent(c *gin.Context, account *Account, payload []byte) {
	if gjson.GetBytes(payload, "type").String() != "response.metadata" {
		return
	}
	headers := gjson.GetBytes(payload, "headers")
	if !headers.IsObject() {
		return
	}
	headers.ForEach(func(key, value gjson.Result) bool {
		if !strings.EqualFold(key.String(), openAICodexTurnStateHeader) {
			return true
		}
		for value.IsArray() {
			value = value.Get("0")
		}
		if value.Type == gjson.String {
			s.noteOpenAICodexTurnStateProvenance(c, account, strings.TrimSpace(value.Str))
		}
		return false
	})
}

// guardOpenAICodexTurnStateEcho 出站守卫：客户端回带的 turn-state 若已知由
// 其他账号铸造则剥离，同账号或无溯源记录时保持原样。292 门票注入已随
// 46998185d 移除，剥离后不再补发门票。
func (s *OpenAIGatewayService) guardOpenAICodexTurnStateEcho(c *gin.Context, account *Account, h http.Header) {
	if s == nil || h == nil || account == nil {
		return
	}
	state := strings.TrimSpace(h.Get(openAICodexTurnStateHeader))
	if s.openAICodexTurnStateHasDifferentOwner(c, account, state) {
		h.Del(openAICodexTurnStateHeader)
	}
}

// Native WebSocket frames carry state in client_metadata instead of the
// Upgrade header. Apply the same ownership check after filling header fallback.
func (s *OpenAIGatewayService) guardOpenAICodexTurnStatePayload(c *gin.Context, account *Account, payload []byte) ([]byte, error) {
	path := "client_metadata." + openAICodexTurnStateHeader
	state := gjson.GetBytes(payload, path)
	if state.Type == gjson.String && s.openAICodexTurnStateHasDifferentOwner(c, account, strings.TrimSpace(state.Str)) {
		return sjson.DeleteBytes(payload, path)
	}
	return payload, nil
}

func (s *OpenAIGatewayService) openAICodexTurnStateHasDifferentOwner(c *gin.Context, account *Account, state string) bool {
	if s == nil || account == nil {
		return false
	}
	if state == "" {
		return false
	}
	seed := openAICodexTurnStateSeed(c)
	if seed == "" {
		return false
	}
	key := openAICodexTurnStateOriginKey(seed, state)
	raw, ok := s.openaiCodexTurnStateOrigins.Load(key)
	if !ok {
		return false
	}
	origin, ok := raw.(openAICodexTurnStateOrigin)
	if !ok {
		s.openaiCodexTurnStateOrigins.Delete(key)
		return false
	}
	if !origin.expiresAt.IsZero() && time.Now().After(origin.expiresAt) {
		s.openaiCodexTurnStateOrigins.Delete(key)
		return false
	}
	return origin.accountID != account.ID || origin.oauthOwner != openAIOAuthOwnerForAccount(account)
}

// sweepOpenAICodexTurnStateOrigins 机会式清扫过期溯源记录：每 256 次写入
// 全量遍历一轮，防止仅靠读侧惰性删除导致的慢泄漏（会话键无上界）。
func (s *OpenAIGatewayService) sweepOpenAICodexTurnStateOrigins() {
	if s.openaiCodexTurnStateWrites.Add(1)%256 != 0 {
		return
	}
	now := time.Now()
	s.openaiCodexTurnStateOrigins.Range(func(key, value any) bool {
		origin, ok := value.(openAICodexTurnStateOrigin)
		if !ok || (!origin.expiresAt.IsZero() && now.After(origin.expiresAt)) {
			s.openaiCodexTurnStateOrigins.Delete(key)
		}
		return true
	})
}
