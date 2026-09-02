# Goal: 修复 sub2api 运维监控链路的 8 项缺陷

## Goal Directive

Follow the saved goal file at `/home/czc/projects/workging/sub2api/2026-08-31-ops-monitoring-fixes.md` and complete it only when all required verification passes; stop to ask if any listed stop condition occurs.

## Full Prompt

### Objective

修复本地生产环境 sub2api(0.1.184.1)运维监控链路的 8 项已定案缺陷,使告警邮件开关语义显式、队列深度指标稳定可用、去重标记具备清理机制、死开关与死字段被移除、指标采集间隔与评估语义一致、systemd 内存配置无矛盾、磁盘释放约 2.8GB,并通过全部验证项。

### Context

生产环境以 systemd 管理 sub2api,监听 127.0.0.1:8080,依赖 PostgreSQL 18 与 Redis。巡检确认服务本身健康:协程 207-415 波动、内存 11-19% 平稳、无泄漏,当日 3 次重启均为手动操作。缺陷集中在监控链路:

- 83 条告警(55 P0、28 P1)`email_sent` 全为 false,因 `recipients` 为空在 `ops_alert_evaluator_service.go:694` 短路。SMTP 可用但本次决定不发信。
- `ops_system_metrics.concurrency_queue_depth` 1126 样本仅 7 个有值且从无 0,证明 NULL 来自采集失败。外层预算 2s 与内层 3s 倒挂,对 4371 个可调度账号每账号 5 条命令共约 21800 条,失败后 `return nil` 且无日志。规则 7 因 `resetRuleState` 清零永不触发。
- 实测 `concurrency:account:active_index`(ZSET)仅 6 个成员,`wait:account:*` 键数为 0,当前真实队列深度即 0。走活跃索引可将命令量降至 1 条 ZRANGEBYSCORE + N 条 GET(实测 7 条)。
- `settings` 表 `notification_email_delivery:v2:*` 147 条(占 18% 行数),最老 14 天内,全代码库无删除逻辑。订阅到期提醒去重键不含日期,导致续费后永不再提醒。
- `aggregation_enabled` 有 UI toggle、i18n、前端类型,后端零读取点;预聚合实际由 `ops_monitoring_enabled` 控制。其默认值与存量值均为 false,若直接接上会立即停掉预聚合。
- `ops_alert_rules.last_triggered_at` 8 条规则全 NULL;冷却实际走 `GetLatestAlertEvent().FiredAt`,功能正确但该列误导排查。该列不由 ent 管理,仅定义在 `migrations/033_ops_monitoring_vnext.sql`。
- `ops_metrics_interval_seconds = 300`,而评估器每 60s 调 `GetLatestSystemMetrics`,同一行被连续读 5 次,`sustained_minutes` 语义失效:瞬时尖峰被放大成持续告警。无陈旧性防护。采集器 33ms、评估器 26ms。
- `GOMEMLIMIT` 在 `performance.conf`(2GiB)与 `runtime-risk.conf`(4GiB)矛盾,按字母序 4GiB 生效(正确),但 2GiB 低于实测峰值 3G,是未来新增 drop-in 时的隐患。`performance.conf` 含 `EnvironmentFile`,不可整体删除。宿主机 22GiB 内存。
- `/opt/sub2api` 下 29 个二进制占 3.2GB,磁盘已用 135G/193G(70%),无 swap。

工程约定:分支从 `dev-czc` 起(AGENTS.md);Go 1.27.0;前端用 pnpm;migration 幂等 DDL,最新编号 231,新增用 232,由 `setup.go:374` 在启动时自动应用并记入 `schema_migrations`。`backend/internal/service/zz_live_test.go` 为未跟踪的临时 live 测试,无跳过守卫且依赖文件存在,会调用远程 API,验证时必须排除。

### Approved Direction

逐条讨论后定案,每条均由用户明确选择:

1. 显式设 `alert.enabled = false`,消除"填入收件人即被刷爆"的陷阱;8 条规则 `notify_email` 保持不动(总开关在前置位短路)。
2. 队列深度采集改走活跃索引:读 `concurrency:account:active_index` 未过期成员,仅对这些账号批量取等待数求和。不在索引内的账号等待数必然为 0,对求和无贡献,故与遍历全部账号等价。同时外层预算 2s→5s 作为兜底,失败分支补 warn 日志。
3. 去重标记清理加入 `ops_cleanup`,保留期 30 天(最长去重窗口 7 天的 4 倍余量),**独立于 ops 数据现有的 3 天保留期** —— 若复用 3 天会在去重窗口内删标记导致重复发信。不做立即 SQL(30 天阈值下当前一条都删不到)。
4. 从 UI 摘掉 `aggregation_enabled`(toggle、i18n、前端类型),后端 DTO 字段保留以兼容旧数据。不接后端,因为接错即停预聚合,且预聚合仅耗 20ms,单独关闭无收益。
5. 删除 `last_triggered_at` 列,新增 migration 232。
6. 采集间隔 300s→60s 恢复持续时长语义,并加陈旧性防护(阈值取 2 倍采集间隔即 120s)应对采集器停摆时基于冻结数据告警的故障模式。
7. 仅删 `performance.conf` 中 `GOMEMLIMIT=2GiB` 一行并修正过期注释("8 GiB RAM budget" 与实际 22GiB 不符),保留 `EnvironmentFile`、`GOMAXPROCS`、`GOGC`。不改任何生效中的值。
8. 保留最近 3 个回滚点(`czc-v2026.08.31.2`、`czc-v2026.08.31.1`、`czc-v2026.08.30.4`)加当前二进制,删其余 25 个,释放约 2.8GB。执行前列出完整清单供确认,不用通配符批量删。

### Discovery Decisions

- 深度:exhaustive。涉及生产流量、SQL 数据、Go 代码部署、systemd 变更。
- SSRF 白名单(原第 9 条):用户明确选择不处理、不建风险记录。此为显式跳过区域,本次不做任何相关改动。
- 分支命名 `fix/ops-monitoring-*`,从 `dev-czc` 起(接受的默认值)。
- systemd 改动与二进制替换合并为同一次重启,减少中断次数(接受的默认值)。
- 陈旧性阈值取 2 倍采集间隔(120s)——明确假设,可在实施时复核。
- 不适用区域:发布沟通与文档(运维缺陷修复,无用户可见行为变更);告警邮件模板(已决定不发信)。
- Unresolved 项及处置:
  - 第 2+6 条叠加后的 Redis 实际负载 → 验证项 18 实测 + 停止条件。
  - 修复后队列深度是否稳定非 NULL → 验证项 12 + 停止条件。

### Scope

允许改动:

- `backend/internal/service/ops_metrics_collector.go`:队列深度采集改走活跃索引;外层预算 5s;失败补 warn 日志。
- `backend/internal/repository/concurrency_cache.go` 及 `backend/internal/service/concurrency_service.go`:新增"读活跃索引 + 批量取等待数"的只读方法及接口声明。
- `backend/internal/service/ops_alert_evaluator_service.go`:陈旧性防护。
- ops 清理逻辑所在文件:新增去重标记清理(30 天保留期)。
- `frontend/src/views/admin/ops/components/OpsSettingsDialog.vue`、`frontend/src/i18n/locales/zh/admin/ops.ts`(及其他语言同键)、`frontend/src/api/admin/ops.ts`:移除 `aggregation_enabled`。
- `backend/migrations/232_*.sql`:删除 `last_triggered_at` 列。
- 上述改动配套的单元测试。
- DB settings:`ops_email_notification_config` 的 `alert.enabled`;`ops_metrics_interval_seconds`。
- `/etc/systemd/system/sub2api.service.d/performance.conf`:删一行 + 改注释。
- `/opt/sub2api` 下 25 个备份二进制:删除。
- 构建、部署(备份当前二进制后替换)、重启、验证。

### Out Of Scope

- 启用 SSRF 白名单或任何 `security.url_allowlist` 改动。
- 配置告警邮件收件人或恢复邮件通知。
- 修改任何告警规则的阈值、窗口、冷却参数。
- 处理上游 429/限流问题本身(11593 条 upstream 错误中多数为重试成功,SLA 已正确排除)。
- 将去重标记迁移到 Redis。
- 将 `aggregation_enabled` 接入后端逻辑。
- 清理 `ops_error_logs`(90MB)或调整其保留期。
- 修改 `zz_live_test.go`(未跟踪文件,仅在验证时排除)。

### Verification

每项必须在当前会话实际执行并观察到结果。缺失、陈旧、不可读、零工作量或无法判定的证据一律视为失败。

构建与测试:

1. `make -C backend build` 退出码 0。
2. `cd backend && go test -skip 'TestZZLiveReviewBody234924' ./...` 退出码 0,且输出中受影响包(service、repository)显示 `ok` 而非 `no test files`——正面证明测试确实执行。
3. `cd backend && golangci-lint run ./...` 退出码 0。
4. `pnpm --dir frontend run lint:check` 与 `pnpm --dir frontend run typecheck` 退出码 0。
5. `pnpm --dir frontend run build` 退出码 0。

新增单元测试(必须先失败后通过,或至少覆盖两侧):

6. 去重标记清理:构造 31 天与 29 天两个标记,断言 31 天被删、29 天保留。两侧断言即校准。
7. 陈旧性防护:构造新鲜样本与超过 120s 的样本,断言前者被接受、后者被判定不可用。
8. 活跃索引求和:构造"索引内有等待数"、"索引内无等待数"、"已过期成员"三种成员,断言求和只计未过期且有等待数者。

数据库校准断言(对照列/对照值证明查询本身有效):

9. `alert.enabled` 精确读回为 `false`:查询返回恰好 1 行且值为 `false`。
10. `ops_metrics_interval_seconds` 精确读回为 `60`。
11. `last_triggered_at` 列不存在,**且**同表对照列(如 `cooldown_minutes`)存在——对照列证明 `information_schema` 查询有效,避免"查不到"被误当"已删除"。同时断言 `schema_migrations` 含新 migration 文件名。

部署后运行时验证:

12. 队列深度:重启后连续 3 个新样本 `concurrency_queue_depth` 均非 NULL(值可为 0——0 是有效观测,NULL 才是失败)。
13. 采集间隔:连续 3 个以上新样本的 `created_at` 间隔约 60s(容差 ±10s)。
14. 心跳:`ops_metrics_collector` 与 `ops_alert_evaluator` 的 `last_success_at` 晚于重启时刻,且 `last_error` 为空。
15. 服务状态:`systemctl is-active sub2api` 为 `active`,`curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/health` 为 200。
16. 网关未被破坏:重启后 `usage_logs` 有新的成功记录,且 `ops_error_logs` 无与本次改动相关的新 `internal` 类错误。
17. systemd:`systemctl show sub2api -p Environment` 中 `GOMEMLIMIT=4GiB` 恰好出现一次;`performance.conf` 不含 `GOMEMLIMIT`,**且**仍含 `EnvironmentFile`(对照项证明文件未被误删)。
18. Redis 负载实测:记录采集器 `last_duration_ms`。若 > 1000ms 触发停止条件。
19. 前端清理:逐文件统计 `aggregation_enabled` 出现次数为 0,**且**同文件内一个已知存在的标识(如 `auto_refresh_enabled`)计数 > 0——对照项证明搜索方法可靠。不使用裸 `! grep` 作否定断言,须区分"匹配"、"无匹配"、"读取错误"。
20. 磁盘:`/opt/sub2api/sub2api*` 文件数为 4;`df` 显示较执行前净释放约 2.8GB。

手工验收:

21. 打开管理后台运维监控页,确认页面正常加载、系统健康卡片显示新鲜数据、设置弹窗中已无预聚合开关。

收尾:

22. 删除 `/dev/shm/.ap.sh` 与 `/dev/shm/.ap_data`,并验证其已不存在。

### Risks And Rollout

- **删列不可通过二进制回滚**。执行前须 `pg_dump` 备份 `ops_alert_rules` 表(含数据),作为唯一回滚凭据。
- 回滚路径:恢复备份二进制 + 重启;DB 列需从备份单独恢复。
- systemd 改动与二进制替换合并为同一次重启,重启期间网关短暂不可用(历史停止耗时约 5s)。
- `ops_metrics_interval_seconds` 改 60s **必须在新二进制部署后**执行。若先改配置,旧二进制会以 60s 频次跑 21800 条命令的老逻辑,Redis 压力上升 5 倍。
- Redis 访问模式变化:从每 5 分钟 21800 条降至每分钟约 7 条,总量与频次双降。验证项 18 实测确认。
- 25 个二进制删除不可逆,但均可从对应 `czc-*` tag 重建。执行前列出完整清单确认。
- 磁盘当前 70% 且无 swap,本次净释放约 2.8GB。
- 活跃索引方案新增一处对索引维护正确性的依赖。若索引漏写,队列深度会被低估。该索引已被 `reconcileExpiredIndexCandidates` 当作权威数据源,可靠性与现有后台任务同级。

### Stop Conditions

出现以下任一情况,停止并询问,不得自行推断或调参:

- 任何验证项失败且原因不明确。
- 修复后队列深度仍为 NULL(验证项 12 失败)。
- 采集器 `last_duration_ms` > 1000ms(验证项 18)。
- 发现 `last_triggered_at` 或 `aggregation_enabled` 存在此前未识别的读取方。
- migration 232 需要涉及其他表或数据迁移(而非单纯删列)。
- 重启后网关出现新的错误类型,或 `usage_logs` 无新成功记录。
- 需要改动 Out Of Scope 列出的任何内容。
- 工具通道不可用导致无法验证。
- `dev-czc` 与远端状态不一致,或工作区存在预期外的改动。
- 待删除的 25 个二进制清单与预期不符。

## Completion Rule

Do not mark this goal complete until the objective is achieved and every required verification item passes, unless the user explicitly changes the completion standard.
