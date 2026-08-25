-- VIP 用户标记。
-- 总余额超过 VipBalanceThreshold 时自动升级，永久生效不降级：
--   1. 冻结 VipFrozenReserve 金额（可用余额 = balance - vip 冻结额）；
--   2. 内容风控仅执行第一层关键词拦截，跳过第二层审核；
--   3. 指定分组计费倍率减免。
-- 冻结为逻辑冻结：不写入 frozen_balance（该列被批量图片余额暂扣占用），
-- 由计费资格检查按 (balance - vip) 计算可用余额。
ALTER TABLE users ADD COLUMN IF NOT EXISTS vip boolean NOT NULL DEFAULT false;

CREATE INDEX IF NOT EXISTS idx_users_vip_pending_upgrade
    ON users (balance)
    WHERE deleted_at IS NULL AND vip = false AND balance > 100;

COMMENT ON COLUMN users.vip IS 'VIP 用户标记；总余额超过阈值自动升级且永久生效；冻结部分余额并享受风控/倍率特权。';
