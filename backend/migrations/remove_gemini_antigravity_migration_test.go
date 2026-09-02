package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRemoveGeminiAntigravityMigration(t *testing.T) {
	content, err := FS.ReadFile("233_remove_gemini_antigravity.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")

	// 存量行必须在收紧 CHECK 之前处理，否则 ADD CONSTRAINT 会失败。
	accountsUpdate := strings.Index(sql, "UPDATE accounts SET status = 'disabled', schedulable = FALSE")
	firstConstraint := strings.Index(sql, "ADD CONSTRAINT channel_monitors_provider_check")
	require.Greater(t, accountsUpdate, -1)
	require.Greater(t, firstConstraint, accountsUpdate)

	// 账号 / 分组只停用不删。
	require.Contains(t, sql, "UPDATE groups SET status = 'disabled'")
	require.NotContains(t, sql, "DELETE FROM accounts")
	require.NotContains(t, sql, "DELETE FROM groups")
	require.NotContains(t, sql, "DELETE FROM usage_logs")
	require.NotContains(t, sql, "UPDATE usage_logs")

	// 无法再工作的行硬删。
	require.Contains(t, sql, "DELETE FROM channel_monitors WHERE provider IN ('gemini', 'antigravity')")
	require.Contains(t, sql, "DELETE FROM channel_monitor_request_templates WHERE provider IN ('gemini', 'antigravity')")
	require.Contains(t, sql, "DELETE FROM composite_model_routes WHERE target_platform IN ('gemini', 'antigravity') OR endpoint = 'gemini'")
	require.Contains(t, sql, "DELETE FROM user_platform_quotas WHERE platform IN ('gemini', 'antigravity')")
	require.Contains(t, sql, "'antigravity_user_agent_version'")

	// CHECK 与代码侧枚举一致（service.monitorProviders / AllowedQuotaPlatforms / composite 端点）。
	require.Contains(t, sql, "channel_monitors_provider_check CHECK (provider IN ('openai', 'anthropic', 'grok', 'kimi', 'zhipu', 'deepseek'))")
	require.Contains(t, sql, "channel_monitor_request_templates_provider_check CHECK (provider IN ('openai', 'anthropic', 'grok', 'kimi', 'zhipu', 'deepseek'))")
	require.Contains(t, sql, "composite_model_routes_target_platform_check CHECK (target_platform IN ('anthropic', 'openai', 'grok', 'kimi', 'zhipu', 'deepseek'))")
	require.Contains(t, sql, "composite_model_routes_endpoint_check CHECK (endpoint IN ('any', 'messages', 'count_tokens', 'responses', 'chat_completions', 'embeddings', 'images'))")
	require.Contains(t, sql, "user_platform_quotas_platform_check CHECK (platform IN ('anthropic', 'openai', 'grok', 'kimi', 'zhipu', 'deepseek'))")
	require.NotContains(t, sql, "CHECK (provider IN ('openai', 'anthropic', 'gemini'")
}
