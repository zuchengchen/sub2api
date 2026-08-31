-- 删除 ops_alert_rules.last_triggered_at。
--
-- 该列自 033_ops_monitoring_vnext.sql 引入后从未被写入：告警冷却实际由
-- ops_alert_evaluator_service.go 读取 GetLatestAlertEvent().FiredAt 判定，
-- 与本列无关。因此 8 条规则在产生 83 次告警后该列仍全为 NULL，
-- 任何据此排查"规则上次何时触发"的查询都会得到"从未触发"的错误结论。
--
-- 功能上无读取方，删除不影响冷却行为；保留反而持续误导排障。
ALTER TABLE ops_alert_rules DROP COLUMN IF EXISTS last_triggered_at;
