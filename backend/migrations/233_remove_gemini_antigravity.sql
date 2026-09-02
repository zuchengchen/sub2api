-- Migration: 233_remove_gemini_antigravity
-- Gemini / Antigravity 平台已从代码中下线（无网关、无 OAuth、无渠道监控 adapter）。
-- 本迁移只做两件事：
--   1. 处理存量行：让指向这两个平台的数据不再参与调度/路由/监控；
--   2. 收紧 CHECK 约束，保证新部署不会再写入这两个平台。
--
-- 原则：
--   - 账号 / 分组只停用不删除（保留审计与用量归属）；
--   - 渠道监控 / 模板 / 复合路由 / 用户平台配额删除：ent enum 已不含这两个值，
--     留下的行会让列表接口反序列化失败，而这些行本身也已无法工作；
--   - usage_logs 等历史流水不动。
-- 先处理存量再收紧 CHECK，否则孤儿行会让 ADD CONSTRAINT 失败。

-- ---------- 1. 存量数据 ----------

-- 账号：停用并摘出调度池。
UPDATE accounts
   SET status = 'disabled',
       schedulable = FALSE,
       updated_at = NOW()
 WHERE platform IN ('gemini', 'antigravity')
   AND (status <> 'disabled' OR schedulable);

-- 分组：停用。
UPDATE groups
   SET status = 'disabled',
       updated_at = NOW()
 WHERE platform IN ('gemini', 'antigravity')
   AND status <> 'disabled';

-- 渠道监控：删除（histories / daily_rollups 通过 FK ON DELETE CASCADE 一并清理）。
DELETE FROM channel_monitors
 WHERE provider IN ('gemini', 'antigravity');

-- 渠道监控请求模板：删除（关联监控的 template_id 由 FK ON DELETE SET NULL 处理）。
DELETE FROM channel_monitor_request_templates
 WHERE provider IN ('gemini', 'antigravity');

-- 被动监控 v2：从 platforms 维度里移除这两个平台。
UPDATE channel_monitor_v2_config
   SET platforms = COALESCE(
           (SELECT jsonb_agg(elem)
              FROM jsonb_array_elements(platforms) AS elem
             WHERE elem->>'platform' NOT IN ('gemini', 'antigravity')),
           '[]'::jsonb),
       version = version + 1,
       updated_at = NOW()
 WHERE id = 1
   AND EXISTS (
       SELECT 1
         FROM jsonb_array_elements(platforms) AS elem
        WHERE elem->>'platform' IN ('gemini', 'antigravity'));

-- 复合路由：目标平台或端点为 gemini/antigravity 的路由已无转发器可承接，硬删。
DELETE FROM composite_model_routes
 WHERE target_platform IN ('gemini', 'antigravity')
    OR endpoint = 'gemini';

-- 用户平台配额：这两个平台不再产生用量，硬删（CHECK 对软删行同样生效）。
DELETE FROM user_platform_quotas
 WHERE platform IN ('gemini', 'antigravity');

-- 已下线功能的孤儿设置项。
DELETE FROM settings
 WHERE key IN (
     'gemini_quota_policy',
     'fallback_model_gemini',
     'fallback_model_antigravity',
     'enable_identity_patch',
     'identity_patch_prompt',
     'antigravity_user_agent_version'
 );

-- ---------- 2. 收紧 CHECK ----------

ALTER TABLE channel_monitors
    DROP CONSTRAINT IF EXISTS channel_monitors_provider_check;
ALTER TABLE channel_monitors
    ADD CONSTRAINT channel_monitors_provider_check
    CHECK (provider IN ('openai', 'anthropic', 'grok', 'kimi', 'zhipu', 'deepseek'));

ALTER TABLE channel_monitor_request_templates
    DROP CONSTRAINT IF EXISTS channel_monitor_request_templates_provider_check;
ALTER TABLE channel_monitor_request_templates
    ADD CONSTRAINT channel_monitor_request_templates_provider_check
    CHECK (provider IN ('openai', 'anthropic', 'grok', 'kimi', 'zhipu', 'deepseek'));

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;
ALTER TABLE composite_model_routes
    ADD CONSTRAINT composite_model_routes_target_platform_check
    CHECK (target_platform IN ('anthropic', 'openai', 'grok', 'kimi', 'zhipu', 'deepseek'));

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_endpoint_check;
ALTER TABLE composite_model_routes
    ADD CONSTRAINT composite_model_routes_endpoint_check
    CHECK (endpoint IN ('any', 'messages', 'count_tokens', 'responses', 'chat_completions', 'embeddings', 'images'));

ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;
ALTER TABLE user_platform_quotas
    ADD CONSTRAINT user_platform_quotas_platform_check
    CHECK (platform IN ('anthropic', 'openai', 'grok', 'kimi', 'zhipu', 'deepseek'));
