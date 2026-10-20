package service

import (
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/google/uuid"
)

// codexUpstreamMinVersion 上游 /backend-api/codex 接受的最低 version 头：
// 若请求携带 version 且低于该值，上游直接 404（issue #3901，2026-07 实测）。
const codexUpstreamMinVersion = "0.144.0"

// codexClientVersionMaxLen 官方版本号均为短 ASCII 串，远低于此上限。
const codexClientVersionMaxLen = 64

// codexClientVersionPattern 允许 0.146.0 与 0.147.0-alpha.4 两类官方形态。
var codexClientVersionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}(-[0-9A-Za-z.]+)?$`)

// NormalizeCodexClientVersion 校验并归一化 Codex 客户端版本号，非法值返回空串。
// 该值会被拼进出站 User-Agent 与 version 头，必须拒绝任意字节，避免管理员误填或
// 自动同步拿到异常值时把不可控内容透给上游。
func NormalizeCodexClientVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" || len(version) > codexClientVersionMaxLen || !codexClientVersionPattern.MatchString(version) {
		return ""
	}
	return version
}

// buildCodexCLIUserAgent 按版本号拼出规范 Codex TUI User-Agent。
// UA 形态只在 codexCLIUserAgentSuffix 一处定义，避免多处拼装漂移。
func buildCodexCLIUserAgent(version string) string {
	if version = NormalizeCodexClientVersion(version); version == "" {
		return codexCLIUserAgent
	}
	return openai.CodexDefaultOriginator + "/" + version + codexCLIUserAgentSuffix
}

// codexIdentityEnforcement 控制 enforceCodexIdentityHeaders 是否强制统一出站身份，
// 由 gateway.disable_codex_identity_enforcement 在服务构造时取反发布。
// 默认开启：上游在容量紧张时按客户端身份分优先级降载，被降载的请求会拿到
// HTTP 200 + 流内 server_is_overloaded，本次请求即失败；强制统一出口可确保没有
// 请求带着第三方或陈旧身份出站。关闭后退回「仅按最终 UA 配对 originator」的收口语义。
var codexIdentityEnforcement = func() *atomic.Bool {
	v := &atomic.Bool{}
	v.Store(true)
	return v
}()

// SetCodexIdentityEnforcementEnabled 发布 Codex 出站身份强制统一开关。
// enforceCodexIdentityHeaders 是所有出站路径共用的纯函数收口点，无法在热路径注入配置，
// 故由持有配置的服务在构造时发布进程级快照。
func SetCodexIdentityEnforcementEnabled(enabled bool) {
	codexIdentityEnforcement.Store(enabled)
}

// codexCanonicalUserAgentResolver 返回当前生效的规范 Codex User-Agent（后台设置 / 自动同步版本号）。
// 由 SettingService 在装配时注入；解析器内部自带 TTL 缓存，热路径不触库。
type codexCanonicalUserAgentResolver func() string

var (
	codexCanonicalUAMu       sync.RWMutex
	codexCanonicalUAResolver codexCanonicalUserAgentResolver
)

// SetCodexCanonicalUserAgentResolver 注入规范 User-Agent 解析器。
// 未注入或解析结果非法时回退到编译期常量 codexCLIUserAgent。
func SetCodexCanonicalUserAgentResolver(resolver func() string) {
	codexCanonicalUAMu.Lock()
	defer codexCanonicalUAMu.Unlock()
	codexCanonicalUAResolver = resolver
}

// CodexCanonicalUserAgent 返回当前生效的规范 Codex User-Agent。
// 取值走与推理相同的解析链：面板 UA 指纹 + 面板/自动同步版本号 + 编译期兜底。
// 供无账号句柄的出站路径（OAuth 换 Token / 刷新）使用。
func CodexCanonicalUserAgent() string {
	return resolveCodexOutboundIdentity("").userAgent
}

// CodexCanonicalAuthIdentity 返回凭据面（auth.openai.com：换 Token / 刷新 / whoami）
// 出站请求的身份对：规范 User-Agent 与配套 originator，与推理解析链同源。
// 凭据面不发 version 头——真实 Codex 客户端在该面只携带 originator 与 User-Agent
// （codex-rs login/default_client.rs 的 default_headers()），version 门槛
// （issue #3901）只存在于 /backend-api/codex 推理面。
func CodexCanonicalAuthIdentity() (userAgent, originator string) {
	identity := resolveCodexOutboundIdentity("")
	return identity.userAgent, identity.originator
}

// ApplyCodexCanonicalAuthIdentity 为凭据面出站请求写入身份对（不含 version）。
func ApplyCodexCanonicalAuthIdentity(h http.Header) {
	if h == nil {
		return
	}
	userAgent, originator := CodexCanonicalAuthIdentity()
	h.Set("user-agent", userAgent)
	h.Set("originator", originator)
}

// CodexCanonicalClientVersion 返回当前生效的 Codex 客户端版本号。
func CodexCanonicalClientVersion() string {
	return resolveCodexOutboundIdentity("").version
}

// codexCanonicalUserAgent 返回出站规范 User-Agent。
func codexCanonicalUserAgent() string {
	codexCanonicalUAMu.RLock()
	resolver := codexCanonicalUAResolver
	codexCanonicalUAMu.RUnlock()
	if resolver != nil {
		if ua := resolver(); strings.TrimSpace(ua) != "" {
			return ua
		}
	}
	return codexCLIUserAgent
}

// codexOutboundIdentity 出站身份三元组，三者必须同源自洽：
// originator 与 User-Agent 首段配套（否则上游 404，issue #3901），
// version 等于 User-Agent 的版本段且不低于上游门槛。
type codexOutboundIdentity struct {
	userAgent  string
	originator string
	version    string
}

// resolveCodexOutboundIdentity 由候选 User-Agent 推导自洽的出站身份。
// candidateUA 为空时使用规范 User-Agent；推导不出官方身份时整体回退为规范 TUI 身份。
//
// 候选 UA（面板 / 账号级的管理员显式配置）只贡献客户端名与 OS / 架构 / 终端指纹，
// 其自带的版本段一律用当前生效版本重建：一条填写于某个历史版本的 UA 否则会把出站身份
// 永久钉死在陈旧版本上，绕过版本自动同步，落回上游优先降载的那一侧。
// 需要固定版本请填「Codex 客户端版本号」并关闭自动同步。
func resolveCodexOutboundIdentity(candidateUA string) codexOutboundIdentity {
	canonical := codexCanonicalUserAgent()
	if _, _, ok := openai.PairCodexClientIdentity(canonical); !ok {
		canonical = codexCLIUserAgent
	}
	ua := candidateUA
	if strings.TrimSpace(ua) == "" {
		ua = canonical
	}
	originator, pairedUA, ok := openai.PairCodexClientIdentity(ua)
	if !ok {
		if originator, pairedUA, ok = openai.PairCodexClientIdentity(canonical); !ok {
			originator, pairedUA = openai.CodexDefaultOriginator, codexCLIUserAgent
		}
	}
	// 生效版本只有一个来源：规范身份（面板版本号 → 自动同步值 → 内置常量，见
	// SettingService.GetOpenAICodexClientVersion）。UA 与 version 头由此同源派生。
	version := codexClientVersionFromUA(canonical)
	if rebuilt := openai.SetCodexUserAgentVersion(pairedUA, version); rebuilt != "" {
		pairedUA = rebuilt
	}
	return codexOutboundIdentity{userAgent: pairedUA, originator: originator, version: version}
}

// codexClientVersionFromUA 取 UA 的版本段作为生效版本；
// 非法或低于上游门槛（低于则上游 404，issue #3901）时回退编译期常量。
func codexClientVersionFromUA(ua string) string {
	version := NormalizeCodexClientVersion(openai.CodexUserAgentVersion(ua))
	if version == "" || CompareVersions(version, codexUpstreamMinVersion) < 0 {
		return codexCLIVersion
	}
	return version
}

// ensureCodexIdentityHeaders 补齐 OAuth（ChatGPT 内部接口）出站请求所需的 Codex 身份头。
// 已有 User-Agent 与 version 保持不变，交给紧随其后的 enforceCodexIdentityHeaders 收口。
func ensureCodexIdentityHeaders(h http.Header) {
	if h == nil {
		return
	}
	identity := resolveCodexOutboundIdentity("")
	if strings.TrimSpace(h.Get("user-agent")) == "" {
		h.Set("user-agent", identity.userAgent)
	}
	if strings.TrimSpace(h.Get("originator")) == "" {
		h.Set("originator", identity.originator)
	}
	if strings.TrimSpace(h.Get("version")) == "" {
		h.Set("version", identity.version)
	}
	h.Set("OpenAI-Beta", "responses=experimental")
}

// applyOpenAICodexProbeHeaders 为合成探测请求补齐 Codex 身份和引擎指纹。
func applyOpenAICodexProbeHeaders(h http.Header) {
	if h == nil {
		return
	}
	ensureCodexIdentityHeaders(h)
	h.Set("X-Codex-Window-ID", uuid.NewString())
}

// enforceCodexIdentityHeaders 收口 OAuth（ChatGPT 内部接口）出站请求的客户端身份头。
// 见 enforceCodexIdentityHeadersWithUA；无账号级自定义 User-Agent 时使用本函数。
func enforceCodexIdentityHeaders(h http.Header) {
	enforceCodexIdentityHeadersWithUA(h, "")
}

// enforceCodexIdentityHeadersWithUA 强制统一 OAuth 出站身份：User-Agent / originator / version
// 一律改写为网关的规范身份，客户端自报身份不参与构造。上游在容量紧张时按客户端身份分优先级
// 降载，被降载的请求会拿到 HTTP 200 + 流内 server_is_overloaded；统一出口可确保没有请求带着
// 第三方或陈旧身份出站，也天然满足 originator 与 UA 首段配套的上游校验（issue #3901）。
//
// overrideUA 是账号级自定义 User-Agent：管理员的显式配置仍然生效，但只贡献客户端名与
// OS / 架构 / 终端指纹——版本段与 originator 都由规范身份重建，不允许出现自相矛盾或陈旧的身份。
//
// 强制统一被 gateway.disable_codex_identity_enforcement 关闭时，退回「按最终 User-Agent 配对
// originator + version 门槛校正」的收口语义，供上游策略变动时回滚。
//
// 仅对携带 originator 的请求生效：compat 桥接等非 ChatGPT 内部接口路径会显式删除 originator，
// 不应被补回。需要从缺失身份头恢复的调用方应先调用 ensureCodexIdentityHeaders。
// 必须在所有 User-Agent 改写之后调用。
func enforceCodexIdentityHeadersWithUA(h http.Header, overrideUA string) {
	if h == nil || h.Get("originator") == "" {
		return
	}
	if !codexIdentityEnforcement.Load() {
		pairCodexIdentityHeaders(h)
		return
	}
	identity := resolveCodexOutboundIdentity(overrideUA)
	h.Set("user-agent", identity.userAgent)
	h.Set("originator", identity.originator)
	h.Set("version", identity.version)
}

// pairCodexIdentityHeaders 是关闭强制统一后的兜底收口：保留客户端真实身份，
// 仅保证 originator 与最终 User-Agent 首段配套、version 不低于上游门槛（issue #3901）。
func pairCodexIdentityHeaders(h http.Header) {
	originator, pairedUA, ok := openai.PairCodexClientIdentity(h.Get("user-agent"))
	if !ok {
		identity := resolveCodexOutboundIdentity("")
		originator, pairedUA = identity.originator, identity.userAgent
		h.Set("version", identity.version)
	}
	h.Set("user-agent", pairedUA)
	h.Set("originator", originator)
	if v := strings.TrimSpace(h.Get("version")); v != "" && CompareVersions(v, codexUpstreamMinVersion) < 0 {
		h.Set("version", resolveCodexOutboundIdentity("").version)
	}
}

// codexHTTPNonOfficialHeaders 是官方 Codex 客户端在 ChatGPT 业务 HTTP 路径
// （POST /backend-api/codex/responses）上**从不发送**的请求头。逐项依据
// codex-rs HEAD f4e18a95bb 源码：
//
//   - session_id、conversation_id：官方会话头只有连字符形式
//     session-id / thread-id（codex-api/src/requests/headers.rs:5-11）；下划线
//     形式在 7c7b4861d8（fix: drop underscored id headers #22193）中已删除。
//   - x-codex-installation-id：官方仅作为 client_metadata 键发送
//     （core/src/responses_metadata.rs:325-334），从不作为 HTTP 头。
//   - OpenAI-Beta：只在 client.rs:1314-1330 build_websocket_headers 写入，
//     属 WS 握手专有；HTTP 业务头集合仅由 client.rs:2325-2343
//     build_responses_headers 产生（x-codex-beta-features / x-codex-turn-state）。
//   - accept-language：reqwest 不写该头，官方业务请求里不存在。
//
// 反例（官方确实发送，不得删）：version 来自内置 openai provider 的
// http_headers（model-provider-info/src/lib.rs:533-535，经
// built_in_model_providers:656 进入所有出站请求）；x-client-request-id 由
// codex-api/src/endpoint/responses.rs:87-89 在 HTTP 上写为 thread_id。
//
// 这些名字「缺失」对上游完全无感（官方自己就不发），但「存在」是本代理独有的
// 可判别特征，因此出站前按官方线序削平，而不是继续按代理习惯补齐。
var codexHTTPNonOfficialHeaders = [...]string{
	"OpenAI-Beta",
	"session_id",
	"conversation_id",
	"x-codex-installation-id",
	"accept-language",
}

// codexWSNonOfficialHeaders 是 WS 握手上官方不发的同名子集。WS 与 HTTP 的差异
// 必须保留：OpenAI-Beta 与 x-client-request-id 是官方 WS 握手**确实发送**的
// （client.rs:1314-1330），故不在 WS 列表内。
var codexWSNonOfficialHeaders = [...]string{
	"session_id",
	"conversation_id",
	"x-codex-installation-id",
	"accept-language",
}

// projectCodexOutboundHeaders 把出站头收口成官方线序：删除官方在该传输上不发的
// 头，并保证连字符会话头确实存在（官方总是同时发 session-id 与 thread-id）。
//
// 必须在所有其它头改写（指纹收敛、账号 namespace、client_metadata 投影、账号级
// 覆写）之后调用——它是最后一道收口，任何更早的补齐都可能被后续步骤重新引入。
//
// 与强制统一身份共用 gateway.disable_codex_identity_enforcement：该开关的语义就是
// 「上游策略变动时退回旧线序」，因此关闭后这里保持原样不动。
func projectCodexOutboundHeaders(h http.Header, websocket bool) {
	if h == nil || !codexIdentityEnforcement.Load() {
		return
	}
	// HTTP 与 WS 名单长度不同，这里按切片遍历，保留各自完整集合。
	var nonOfficial []string
	if websocket {
		nonOfficial = codexWSNonOfficialHeaders[:]
	} else {
		nonOfficial = codexHTTPNonOfficialHeaders[:]
	}
	for _, name := range nonOfficial {
		deleteHeaderAllForms(h, name)
	}
	// 官方 build_session_headers 在 session_id 与 thread_id 均存在时必然同时写出
	// 两个连字符头；这里只把已有的值补成对，不凭空造 ID（造 ID 是上游派生层的职责）。
	if session := strings.TrimSpace(h.Get("session-id")); session != "" && strings.TrimSpace(h.Get("thread-id")) == "" {
		h.Set("thread-id", session)
	}
}
