# Goal: 使用教程增加错误码含义与处理方案

## Goal Directive

Follow the saved goal file at `/home/czc/projects/workging/sub2api/2026-08-30-guide-error-codes.md`; complete the task only when all required verification passes, and stop to ask if any listed stop condition occurs.

## Full Prompt

### Objective

在 `docs/guide.zh.md` 新增一个 `##` 级章节「错误码含义与处理方案」，用面向新手的中文解释用户实际可能遇到的 HTTP 状态码、错误体里的业务代号（reason）和 `error.type` 分类，每一项都给出具体处理步骤；同时改写「遇到问题先看这里」中与错误码重复的条目使其指向新章节，在 `frontend/src/utils/guideMarkdown.ts` 登记新章节的稳定锚点 id，并把教程版本从 `1.1` 升到 `1.2`（`docs/guide.zh.md` 底部与 `frontend/src/views/public/GuideView.vue` 的 `bundledGuideVersion` 两处同步）。

### Context

- 教程正文是单文件 `docs/guide.zh.md`（当前 414 行）。`frontend/src/views/public/GuideView.vue:156` 以 `?raw` 内置打包，`frontend/src/views/admin/settings/GuideEditor.vue:191` 也引用同一文件作为编辑器初始内容。
- 管理员可在数据库发布在线版本覆盖内置版本（`GuideView.vue:236-251`，仅当 `has_custom_content` 且内容非空时生效）。本次改动只动内置版本，不影响已发布的在线版本。
- 现状不足：只有「检查 API Key 能不能用」一节列了 200/401/403/404/429 五个码；「遇到问题先看这里」只单独讲了 401（`docs/guide.zh.md:356`）和余额不足（`:360`）。用户看到 `USER_RPM_EXCEEDED`、`billing_error` 这类字样时无处可查。
- `frontend/src/utils/guideMarkdown.ts:19-35` 有一张 `##` 标题 → 稳定锚点 id 的映射表。未登记的新标题会退化为 `section-N`，目录锚点不稳定。
- 已核实的后端映射（来源均为当前代码）：
  - 余额不足 → 403 + `billing_error`（`backend/internal/handler/gateway_handler.go:2395`）
  - 用户/分组 RPM 超限 → 429 + `rate_limit_exceeded` + `Retry-After`（`gateway_handler.go:2374-2377`）
  - 平台日/周/月限额耗尽 → 429 + `rate_limit_exceeded` + `Retry-After`（`gateway_handler.go:2379-2385`；`billing_cache_service.go:36-38` 说明选 429 而非 403 是为让 SDK 自动退避）
  - 并发排队已满 / 并发超限 → 429 + `rate_limit_error`（`concurrency_error_response.go:13-26`）
  - API Key 无效 → 401 + `authentication_error`（`gateway_handler_chat_completions.go:30`）
  - 模型/分组无权限 → 403 + `permission_error`（`gateway_handler_chat_completions.go:112`）
  - 无可用账号 → 503 + `api_error`；账号全部失败 → 502 + `server_error`（`gateway_handler_chat_completions.go:206,219`）
  - 上游请求失败 → 502 + `upstream_error`（`gateway_handler_chat_completions.go:284,293,416`）
  - 请求体过大 → 413 + `invalid_request_error`，消息形如 `Request body too large, limit is ...`（`openai_embeddings.go:52`、`request_body_limit.go:28`）
  - 客户端主动断开 → 499（`concurrency_error_response.go:10,29`）
  - 内容审核拦截 → 状态码由 decision 决定，可能带 `Retry-After`（`content_moderation_errors.go:72-84`）
- 常见业务 reason（`internal/service/`）：`INSUFFICIENT_BALANCE`、`USER_RPM_EXCEEDED`、`GROUP_RPM_EXCEEDED`、`USER_PLATFORM_DAILY_QUOTA_EXHAUSTED`（及 WEEKLY/MONTHLY）、`API_KEY_RATE_5H_EXCEEDED`（及 1D/7D）、`API_KEY_QUOTA_EXHAUSTED`、`API_KEY_EXPIRED`、`SUBSCRIPTION_INVALID`、`BILLING_SERVICE_ERROR`。
- 错误体结构为 `{"error":{"type":..., "message":...}}`（`gateway_handler_chat_completions.go:380-387`）。
- 现有测试会断言教程内容：`frontend/src/views/public/__tests__/GuideView.spec.ts`（锚点 `#recharge`/`#goal-workflow`/`#svip`、命令 id 顺序、SVIP 措辞、且断言教程**不得**出现支付竞态相关字样）、`frontend/src/utils/__tests__/guideBatAsset.spec.ts`（SHA-256 在教程中恰好出现 2 次、SVIP 常量与后端同步）、`frontend/e2e/guide.spec.ts` 与 `guide-admin.spec.ts`。
- 分支现状：当前在 `main-czc`；`dev-czc` 落后 `main-czc` 2 个提交，但 `docs/guide.zh.md` 与 `frontend/src/utils/guideMarkdown.ts` 在两个分支上无差异。
- 环境注意：未跟踪文件 `frontend/pnpm-workspace.yaml` 缺 `packages` 字段，直接跑 `corepack pnpm@9.15.9` 会报 `packages field missing or empty`；必须加 `--ignore-workspace`（`playwright.config.ts:29` 同样这么做）。

### Approved Direction

新增独立 `##` 章节承载完整对照表，并把 FAQ 里已重复的 401、余额不足两条改写为指向新章节，避免同一事实在两处各自维护而日后不一致。覆盖三层（HTTP 状态码 / 业务 reason / `error.type`），因为用户在不同客户端看到的字段不同：有的只显示三位数，有的原样打印 JSON 错误体。表格内容以已核实的代码映射为准，其余常见码（404/409/413/499/504）一并列入但描述保持在能确证的粒度。

### Discovery Decisions

- 结构：新增独立章节 + 改写 FAQ 重复条目（用户选项 1）。
- 覆盖层级：HTTP 状态码 + 业务 reason + `error.type` 三层全讲（用户选项 3）。
- 准确度：已核实映射 + 其余常见码一并列入（用户选项 2）。
- 分支：从最新 `dev-czc` 新建 `feature/guide-error-codes`（用户选项 1）。合入 `dev-czc`、推送、打标签均需另行授权，不在本目标内。
- 版本号：升到 `1.2`，`docs/guide.zh.md` 底部与 `GuideView.vue:159` 两处同步（用户选项 1）。更新日期保持 `2026-08-30`（即今日）。
- 假设：`error.type` 中 `rate_limit_error` 与 `rate_limit_exceeded` 两种写法并存是既有实现，教程同时列出并说明二者都表示被限流，不改后端去统一。
- 假设：新章节插入位置在「查看余额和使用记录」之后、「遇到问题先看这里」之前，使读者先懂错误码再看具体故障。
- 不适用：后端代码、数据库迁移、权限与密钥配置——本次为文档改动加一处前端锚点映射。
- 不适用：在线发布版本内容——由管理员自行发布，内置版本改动不覆盖它。

### Scope

- 修改 `docs/guide.zh.md`：新增「错误码含义与处理方案」章节；改写「遇到问题先看这里」中的 401 与余额不足条目为指向新章节；更新底部「教程版本」为 `1.2`。
- 修改 `frontend/src/utils/guideMarkdown.ts`：在 `sectionIds` 中为新标题登记稳定锚点 id `error-codes`。
- 修改 `frontend/src/views/public/GuideView.vue`：`bundledGuideVersion` 改为 `'1.2'`。
- 必要时在 `frontend/src/views/public/__tests__/GuideView.spec.ts` 增加对新章节锚点与关键措辞的断言。
- 从最新 `dev-czc` 创建并切换到 `feature/guide-error-codes` 后再改动。
- 只读地查阅 `backend/internal/` 以核对错误码映射。

### Out Of Scope

- 任何后端代码、错误码语义、状态码映射的改动。
- 统一 `rate_limit_error` 与 `rate_limit_exceeded` 两种写法。
- 英文版 / 日文版教程（`README.md`、`README_JA.md`）与其他 `docs/` 文档。
- 修复 `frontend/pnpm-workspace.yaml` 缺 `packages` 字段的问题（用 `--ignore-workspace` 规避即可）。
- 提交、合入 `dev-czc`、推送、打标签、发布 GitHub Release。
- 通过管理员界面发布在线版本教程。
- 教程页面的样式、布局、目录组件行为改动。

### Verification

全部命令在 `/home/czc/projects/workging/sub2api/frontend` 下执行，均须以退出码 0 判定成功，并确认输出中报告了实际执行的用例数（非 0 个用例）：

1. `corepack pnpm@9.15.9 --ignore-workspace exec vitest run src/views/public/__tests__/GuideView.spec.ts src/utils/__tests__/guideBatAsset.spec.ts src/router/__tests__/guide-route.spec.ts src/views/admin/settings/__tests__/GuideEditor.spec.ts`
   须显示 `Test Files` 全部 passed 且 `Tests` 计数大于 0；若出现 `no test files found` 视为失败。
2. `corepack pnpm@9.15.9 --ignore-workspace exec vue-tsc --noEmit`
   类型检查通过（改动涉及 `.ts` 与 `.vue`）。
3. `corepack pnpm@9.15.9 --ignore-workspace exec eslint src/utils/guideMarkdown.ts src/views/public/GuideView.vue`
   无 error 级问题。
4. `corepack pnpm@9.15.9 --ignore-workspace exec playwright test e2e/guide.spec.ts`
   两个 project（desktop-1440x900、mobile-390x844）均通过，输出须显示 2 个用例实际执行。Playwright 浏览器已确认安装于 `~/.cache/ms-playwright`。
5. 内容一致性断言（在新增的 vitest 断言中实现，不使用裸 `grep` 取反）：
   - `buildGuideDocument(guideMarkdown).sections` 中存在 `{ id: 'error-codes' }`，即新标题已登记稳定锚点而非退化为 `section-N`。
   - 教程正文包含 `INSUFFICIENT_BALANCE`、`rate_limit_exceeded`、`Retry-After` 等关键代号。
   - `docs/guide.zh.md` 中的教程版本与 `GuideView.vue` 的 `bundledGuideVersion` 一致，均为 `1.2`。
6. `git diff --stat` 确认改动文件仅限 Scope 所列，且未包含 `frontend/pnpm-workspace.yaml`、`backend/internal/service/zz_live_test.go` 等既有未跟踪/无关文件。

人工验收标准：

- 新章节对每个错误码都同时说明「大概意思」和「先做什么」，措辞与教程既有风格一致（面向零基础、不堆术语、术语首次出现时用通俗说明）。
- FAQ 中 401 与余额不足两条不再重复解释错误码本身，而是指向新章节。
- 教程中不出现任何真实 API Key、兑换码、密码或其他敏感值。

### Risks And Rollout

- 风险：`GuideView.spec.ts` 有一条断言教程**不得**匹配 `/A.{0,20}B.{0,80}(?:支付|订单)/s` 且不含「并发支付竞态」「没有支付但是到账」。新章节讲 400/403 与计费错误时措辞需避免触发该正则，否则测试失败。缓解：写完即跑验证项 1。
- 风险：`guideBatAsset.spec.ts` 断言 SHA-256 在教程中恰好出现 2 次。新章节不得引入第三处该摘要。
- 风险：教程写的错误码随后端演进而过期。缓解：表格只写已核实映射，措辞保留「以页面实际提示为准」的兜底。
- 兼容性：纯文档与前端常量改动，无 API 契约、无数据迁移、无配置变更。已发布在线版本教程的站点不受影响（内置版本仅在无在线版本时展示）。
- 发布：改动落在 `feature/guide-error-codes`，后续提升到 `dev-czc` 与 `main-czc` 需按 `AGENTS.md` 单独授权。
- 回滚：`git checkout -- docs/guide.zh.md frontend/src/utils/guideMarkdown.ts frontend/src/views/public/GuideView.vue`，或直接丢弃该 feature 分支。

### Stop Conditions

- 执行中发现某个错误码的实际映射与 Context 所载不符：以代码为准，停止并报告差异，不猜测语义。
- 需要改动后端代码、错误码语义或状态码映射才能把教程写对。
- `dev-czc` 与 `origin/dev-czc` 出现分叉，或创建分支需要 force-push、reset 等破坏性操作。
- 任一验证项失败且根因在既有代码或环境（而非本次改动）：停止并报告，不为让测试变绿而放宽断言或改动产品行为。
- 需要触碰 Out Of Scope 中的文件，或需要提交、推送、合入、打标签。
- 发现教程需要写入任何密钥、兑换码或凭据的真实值。

## Completion Rule

Do not mark this goal complete until the objective is achieved and every required verification item passes, unless the user explicitly changes the completion standard.
