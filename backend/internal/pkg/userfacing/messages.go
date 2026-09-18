package userfacing

// Common API/gateway messages. Website popups should prefer i18n by error code
// and not display these bilingual strings when a locale mapping exists.

var (
	InvalidAPIKey  = ZHEN("API Key 无效", "Invalid API key")
	APIKeyRequired = ZHEN(
		"请在 Authorization（Bearer）、x-api-key 或 x-goog-api-key 请求头中提供 API Key",
		"API key is required in Authorization header (Bearer scheme), x-api-key header, or x-goog-api-key header",
	)
	APIKeyQueryDeprecated = ZHEN(
		"请勿把 API Key 放在 URL 查询参数中，请改用请求头",
		"API key in query parameter is deprecated. Please use Authorization header instead.",
	)
	APIKeyDisabled                               = ZHEN("API Key 已禁用", "API key is disabled")
	APIKeyNotActive                              = ZHEN("API Key 未启用", "api key is not active")
	InvalidAuthRateLimited                       = ZHEN("认证失败次数过多，请稍后再试", "Too many invalid authentication attempts; retry later")
	APIKeyAuthOverloaded                         = ZHEN("API Key 认证暂时不可用", "API key authentication is temporarily unavailable")
	FailedToValidateAPIKey                       = ZHEN("校验 API Key 失败", "Failed to validate API key")
	UserNotFound                                 = ZHEN("用户不存在", "User not found")
	APIKeyUserNotFound                           = ZHEN("API Key 关联的用户不存在", "User associated with API key not found")
	UserInactive                                 = ZHEN("用户账号未启用", "User account is not active")
	InsufficientBalance                          = ZHEN("余额不足", "insufficient balance")
	InsufficientAccountBalance                   = ZHEN("账户余额不足", "Insufficient account balance")
	NoActiveSubscription                         = ZHEN("当前分组没有有效订阅", "No active subscription found for this group")
	SubscriptionInvalid                          = ZHEN("订阅无效或已过期", "subscription is invalid or expired")
	SubscriptionMaintenanceFailed                = ZHEN("订阅用量窗口维护失败", "Failed to maintain subscription usage windows")
	UserRPMExceeded                              = ZHEN("用户每分钟请求次数已达上限", "user requests-per-minute limit exceeded")
	GroupRPMExceeded                             = ZHEN("分组每分钟请求次数已达上限", "group requests-per-minute limit exceeded")
	DailyQuotaExhausted                          = ZHEN("该平台今日用量额度已用完", "Daily usage quota exhausted for this platform.")
	WeeklyQuotaExhausted                         = ZHEN("该平台本周用量额度已用完", "Weekly usage quota exhausted for this platform.")
	MonthlyQuotaExhausted                        = ZHEN("该平台本月用量额度已用完", "Monthly usage quota exhausted for this platform.")
	DailyLimitExceeded                           = ZHEN("日用量限额已用完", "daily usage limit exceeded")
	WeeklyLimitExceeded                          = ZHEN("周用量限额已用完", "weekly usage limit exceeded")
	MonthlyLimitExceeded                         = ZHEN("月用量限额已用完", "monthly usage limit exceeded")
	BillingUnavailable                           = ZHEN("计费服务暂时不可用，请稍后重试", "Billing service temporarily unavailable. Please retry later.")
	BillingError                                 = ZHEN("计费错误", "Billing error")
	NoAvailableAccounts                          = ZHEN("当前没有可用账号", "No available accounts")
	NoAvailableOpenAIAccounts                    = ZHEN("当前没有可用的 OpenAI 账号", "No available OpenAI accounts")
	NoAvailablePinnedOpenAIAccounts              = ZHEN("当前没有可用的固定 OpenAI 账号", "No available pinned OpenAI accounts")
	NoAvailableGrokAccounts                      = ZHEN("当前没有可用的 Grok 账号", "No available Grok accounts")
	NoAvailableCompatibleAccounts                = ZHEN("当前没有可用的兼容账号", "No available compatible accounts")
	NoAvailableCompactAccounts                   = ZHEN("当前没有支持 /responses/compact 的可用账号", "No available accounts support /responses/compact")
	NoAvailableOpenAIModelDiscoveryAccounts      = ZHEN("当前没有可用于发现模型的 OpenAI 账号", "No available OpenAI model discovery accounts")
	ClaudeCodeOnly                               = ZHEN("该分组仅允许 Claude Code 客户端", "this group only allows Claude Code clients")
	VipExclusiveModel                            = ZHEN("gpt-5.6-luna 仅限 VIP 用户使用", "The gpt-5.6-luna model is available to VIP users only")
	ImageGenerationDisabled                      = ZHEN("当前分组未开启图片生成", "Image generation is not enabled for this group")
	ModelRequired                                = ZHEN("必须指定模型", "model is required")
	ModelNotSupportedByComposite                 = ZHEN("该模型不受当前组合分组支持", "Model is not supported by composite groups")
	ModelNotSupportedByOpenAICompatibleComposite = ZHEN("该模型不受此 OpenAI 兼容接口的组合分组支持", "Model is not supported by this OpenAI-compatible endpoint for composite groups")
	ModelNotSupportedOnChatCompletions           = ZHEN("该模型不支持 Chat Completions 接口", "This model is not supported on the Chat Completions endpoint")
	FailedToReadBody                             = ZHEN("读取请求体失败", "Failed to read request body")
	RequestBodyEmpty                             = ZHEN("请求体为空", "Request body is empty")
	FailedToParseBody                            = ZHEN("解析请求体失败", "Failed to parse request body")
	UserContextNotFound                          = ZHEN("用户上下文不存在", "User context not found")
	UpstreamRequestFailed                        = ZHEN("上游请求失败", "Upstream request failed")
	TooManyPendingRequests                       = ZHEN("排队请求过多，请稍后重试", "Too many pending requests, please retry later")
	TooManyConcurrentRequests                    = ZHEN("并发请求过多，请稍后重试", "too many concurrent requests, please retry later")
	ImageConcurrencyExceeded                     = ZHEN("生图并发已达上限，请稍后重试", "Image generation concurrency limit exceeded, please retry later")
	LiveConcurrencyReached                       = ZHEN("实时会话并发已达上限", "Live concurrency limit reached")
	FailedToAcquireUserSlot                      = ZHEN("获取用户并发名额失败", "failed to acquire user concurrency slot")
	APIKeyGroupRequired                          = ZHEN("API Key 必须绑定分组", "API key group is required")
	RedeemCodeNotFound                           = ZHEN("兑换码不存在", "redeem code not found")
	RedeemCodeUsed                               = ZHEN("兑换码已使用", "redeem code already used")
	RedeemCodeExpired                            = ZHEN("兑换码已过期", "redeem code expired")
	RedeemRateLimited                            = ZHEN("失败次数过多，请稍后再试", "too many failed attempts, please try again later")
	RedeemCodeLocked                             = ZHEN("兑换码正在处理，请稍后重试", "redeem code is being processed, please try again")
	PasswordIncorrect                            = ZHEN("当前密码不正确", "current password is incorrect")
	RegistrationDisabled                         = ZHEN("当前未开放注册", "registration is currently disabled")
	AuthorizationRequired                        = ZHEN("需要登录", "Authorization required")
	AuthorizationHeaderRequired                  = ZHEN("需要 Authorization 请求头", "Authorization header is required")
	InvalidAuthHeader                            = ZHEN("Authorization 格式必须为 Bearer {token}", "Authorization header format must be 'Bearer {token}'")
	EmptyToken                                   = ZHEN("令牌不能为空", "Token cannot be empty")
	TokenExpired                                 = ZHEN("登录已过期", "Token has expired")
	InvalidToken                                 = ZHEN("令牌无效", "Invalid token")
	TokenRevoked                                 = ZHEN("令牌已失效（密码已更改）", "Token has been revoked (password changed)")
	InternalError                                = ZHEN("服务器内部错误", "Internal server error")
	FailedToLoadUser                             = ZHEN("加载用户失败", "Failed to load user")
	TooManyRequests                              = ZHEN("请求过于频繁，请稍后再试", "Too many requests, please slow down and try again later")
	APIKeyRateLimited                            = ZHEN("失败次数过多，请稍后再试", "too many failed attempts, please try again later")
	CyberSessionBlocked                          = ZHEN("该会话已被网络安全策略屏蔽，请开启新会话", "This session is blocked by cyber-security policy, please start a new session")
)

func AccessDeniedIP(ip string) string {
	return ZHEN("当前 IP 无访问权限："+ip, "Access denied. Your IP is "+ip)
}

func NoAvailableAccountsDetail(detail string) string {
	if detail == "" {
		return NoAvailableAccounts
	}
	return NoAvailableAccounts + ": " + detail
}

func ModelNotSupportedByGroup(model string) string {
	return ZHEN(
		"当前分组没有任何已配置账号支持模型 "+model,
		`Model "`+model+`" is not supported by any configured account in this group`,
	)
}

func TimeoutWaitingConcurrency(slotType string) string {
	return ZHEN("等待"+slotLabelZH(slotType)+"并发名额超时", "timeout waiting for "+slotType+" concurrency slot")
}

func ConcurrencyLimitReached(slotType string) string {
	return ZHEN(slotLabelZH(slotType)+"并发已达上限", slotType+" concurrency limit reached")
}

func slotLabelZH(slotType string) string {
	switch slotType {
	case "user":
		return "用户"
	case "account":
		return "账号"
	default:
		return slotType
	}
}
