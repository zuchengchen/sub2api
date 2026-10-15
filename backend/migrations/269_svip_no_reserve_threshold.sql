-- SVIP 无保证金；自动升级改为总余额 >= 100。
-- 历史 231 的部分索引用 balance > 100，会漏掉刚好 100 元的待升级用户。
DROP INDEX IF EXISTS idx_users_vip_pending_upgrade;
CREATE INDEX IF NOT EXISTS idx_users_vip_pending_upgrade
    ON users (balance)
    WHERE deleted_at IS NULL AND vip = false AND balance >= 100;

COMMENT ON COLUMN users.vip IS 'VIP 用户标记；总余额达到阈值自动升级且永久生效；无保证金，享受风控/倍率特权。';
