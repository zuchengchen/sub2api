package domain

// Status constants
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
	StatusError    = "error"
	StatusUnused   = "unused"
	StatusUsed     = "used"
	StatusExpired  = "expired"
)

// Role constants
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// Platform constants
const (
	PlatformAnthropic = "anthropic"
	PlatformOpenAI    = "openai"
	PlatformGrok      = "grok"
	// 国产 OpenAI 兼容供应商（经 OpenAI 网关转发，按 Chat Completions 协议）。
	PlatformKimi      = "kimi"     // Kimi (月之暗面 / Moonshot)
	PlatformZhipu     = "zhipu"    // 智谱 GLM (bigmodel)
	PlatformDeepseek  = "deepseek" // DeepSeek
	PlatformComposite = "composite"
)

// Account mode constants 区分国产供应商的「按量付费（余额）」与「Coding Plan」两种接入方式。
// 存储于 credentials["account_mode"]，决定 base_url 预设与额度监控方式。
const (
	AccountModePayG   = "payg"   // 按量付费：消耗余额，做余额检测冷却
	AccountModeCoding = "coding" // Coding Plan：滚动用量窗口冷却（5h / weekly）
)

// API protocol constants 国产供应商的上游 API 协议维度。存储于
// credentials["api_protocol"]，与 account_mode 正交：协议决定转发端点与格式，
// 模式决定额度监控方式。同协议请求零转换直通；跨协议组合才走转换链。
const (
	APIProtocolChatCompletions = "chat_completions" // OpenAI Chat Completions（默认）
	APIProtocolAnthropic       = "anthropic"        // 原生 Anthropic /v1/messages（适配 Claude Code）
	APIProtocolResponses       = "responses"        // OpenAI Responses（deepseek / kimi 原生端点，适配 Codex）
	APIProtocolAdaptive        = "adaptive"         // 按入站协议优先选择供应商原生端点
)

// Account type constants
const (
	AccountTypeOAuth          = "oauth"           // OAuth类型账号（full scope: profile + inference）
	AccountTypeSetupToken     = "setup-token"     // Setup Token类型账号（inference only scope）
	AccountTypeAPIKey         = "apikey"          // API Key类型账号
	AccountTypeUpstream       = "upstream"        // 上游透传类型账号（通过 Base URL + API Key 连接上游）
	AccountTypeBedrock        = "bedrock"         // AWS Bedrock 类型账号（通过 SigV4 签名或 API Key 连接 Bedrock，由 credentials.auth_mode 区分）
	AccountTypeServiceAccount = "service_account" // Google Service Account 类型账号（用于 Vertex AI）
)

// Redeem type constants
const (
	RedeemTypeBalance      = "balance"
	RedeemTypeConcurrency  = "concurrency"
	RedeemTypeSubscription = "subscription"
	RedeemTypeInvitation   = "invitation"
)

// PromoCode status constants
const (
	PromoCodeStatusActive   = "active"
	PromoCodeStatusDisabled = "disabled"
)

// Admin adjustment type constants
const (
	AdjustmentTypeAdminBalance     = "admin_balance"     // 管理员调整余额
	AdjustmentTypeAdminConcurrency = "admin_concurrency" // 管理员调整并发数
)

// Group subscription type constants
const (
	SubscriptionTypeStandard     = "standard"     // 标准计费模式（按余额扣费）
	SubscriptionTypeSubscription = "subscription" // 订阅模式（按限额控制）
)

// Subscription status constants
const (
	SubscriptionStatusActive    = "active"
	SubscriptionStatusExpired   = "expired"
	SubscriptionStatusSuspended = "suspended"
)

// DefaultBedrockModelMapping 是 AWS Bedrock 平台的默认模型映射
// 将 Anthropic 标准模型名映射到 Bedrock 模型 ID
// 注意：此处的 "us." 前缀仅为默认值，ResolveBedrockModelID 会根据账号配置的
// aws_region 自动调整为匹配的区域前缀（如 eu.、apac.、jp. 等）
var DefaultBedrockModelMapping = map[string]string{
	// Claude Fable
	"claude-fable-5-1": "anthropic.claude-fable-5-1",
	"claude-fable-5":   "anthropic.claude-fable-5",
	// Claude Opus
	"claude-opus-5":            "us.anthropic.claude-opus-5-v1",
	"claude-opus-4-8":          "us.anthropic.claude-opus-4-8-v1",
	"claude-opus-4-7":          "us.anthropic.claude-opus-4-7-v1",
	"claude-opus-4-6-thinking": "us.anthropic.claude-opus-4-6-v1",
	"claude-opus-4-6":          "us.anthropic.claude-opus-4-6-v1",
	"claude-opus-4-5-thinking": "us.anthropic.claude-opus-4-5-20251101-v1:0",
	"claude-opus-4-5-20251101": "us.anthropic.claude-opus-4-5-20251101-v1:0",
	"claude-opus-4-1":          "us.anthropic.claude-opus-4-1-20250805-v1:0",
	"claude-opus-4-20250514":   "us.anthropic.claude-opus-4-20250514-v1:0",
	// Claude Sonnet
	"claude-sonnet-5":            "us.anthropic.claude-sonnet-5-v1",
	"claude-sonnet-4-6-thinking": "us.anthropic.claude-sonnet-4-6",
	"claude-sonnet-4-6":          "us.anthropic.claude-sonnet-4-6",
	"claude-sonnet-4-5":          "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
	"claude-sonnet-4-5-thinking": "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
	"claude-sonnet-4-5-20250929": "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
	"claude-sonnet-4-20250514":   "us.anthropic.claude-sonnet-4-20250514-v1:0",
	// Claude Haiku
	"claude-haiku-4-5":          "us.anthropic.claude-haiku-4-5-20251001-v1:0",
	"claude-haiku-4-5-20251001": "us.anthropic.claude-haiku-4-5-20251001-v1:0",
}
