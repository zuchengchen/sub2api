# Goal: 使用教程改为按章节独立编辑与即时生效

## Goal Directive

Follow the saved goal file at `/home/czc/projects/workging/sub2api/2026-08-31-guide-editable-sections.md` and complete it only when all required verification passes; stop to ask if any listed stop condition occurs.

## Full Prompt

### Objective

把使用教程从「单文件 `docs/guide.zh.md` + 数据库整篇覆盖」改造为「仓库 `docs/guide/NNN-slug.md` 每章一文件 + 数据库按章节覆盖」：仓库侧删除 `docs/guide.zh.md`，拆成 16 个独立章节文件并由前端构建期聚合为内置默认版；后端新增按章节存取的设置键与 API；后台 `/admin/settings` 教程页改为「左侧章节列表（支持新增、删除、拖动排序）+ 右侧单章 Markdown 编辑与实时预览」，保存后仅刷新 `/guide` 即生效，无需重新构建或重启；数据库中已发布的整篇 Markdown 在读取时自动按 `##` 拆分迁移为章节结构且不丢内容。

### Context

- 教程正文当前是单文件 `docs/guide.zh.md`（504 行，16 个 `##` 章节，含 3 个 `<!-- copy-command:* -->` 标记与 2 处相同的 BAT SHA-256）。
- `frontend/src/views/public/GuideView.vue:156` 与 `frontend/src/views/admin/settings/GuideEditor.vue:191` 均以 `?raw` 在构建期导入该文件；`frontend/src/views/public/__tests__/GuideView.spec.ts:6` 同样导入。
- 数据库侧为 KV 设置表，现有键（`backend/internal/service/domain_constants.go:351-354`）：`guide_content`（整篇 Markdown）、`guide_version`、`guide_updated_at`、`guide_revisions`。**`setting` 是 KV 表，本次不需要 ent schema 变更或 SQL 迁移。**
- 现有服务层 `backend/internal/service/setting_guide.go`：`GetGuideSettings` / `SaveGuideSettings` / `ResetGuideSettings` / `RestoreGuideSettings`，带 `expected_version` 乐观锁（冲突返回 `GUIDE_VERSION_CONFLICT`）、`GuideMaxContentBytes = 256 * 1024`、`GuideRevisionLimit = 20`。
- 路由：`backend/internal/server/routes/auth.go:243` 公开 `GET /settings/guide`；`backend/internal/server/routes/admin.go:532-535` 管理端 `GET/PUT /admin/settings/guide`、`POST /admin/settings/guide/restore`、`POST /admin/settings/guide/reset`。
- 章节锚点来自 `frontend/src/utils/guideMarkdown.ts:19-36` 一张硬编码「中文标题 → 稳定 id」映射表，未登记标题退化为 `section-N`。
- 教程内已存在指向锚点的内部链接 `[错误码含义与处理方案](#error-codes)`；`#recharge`、`#goal-workflow`、`#svip` 被单测与 e2e 直接断言。
- 构建上下文限制：`Dockerfile:41-42` 只 COPY 了 `docs/legal/` 与 `docs/guide.zh.md`；`.dockerignore:13-20` 为 `docs/` 排除 + 单点放行；`.gitignore:138-139` 为 `docs/*` 排除 + `!docs/guide.zh.md` 放行。三处都必须改为放行 `docs/guide/`，否则章节文件不入 git、不进 Docker 构建上下文。
- 现有测试会断言教程内容：`GuideView.spec.ts`（锚点、命令 id 顺序、错误码三层、FAQ 内部链接、禁止出现支付竞态字样）、`guideBatAsset.spec.ts`（读取 `docs/guide.zh.md` 绝对路径、SHA-256 恰好出现 2 次、版本号 `1.2` 双向同步、SVIP 常量同步）、`GuideEditor.spec.ts`、`frontend/e2e/guide.spec.ts`、`frontend/e2e/guide-admin.spec.ts`。
- i18n 需同步两处：`frontend/src/i18n/locales/zh/admin/settings.ts:17-51` 与 `frontend/src/i18n/locales/en/admin/settings.ts:17-51`。
- 分支现状：`main-czc` 与 `dev-czc` 内容一致，`dev-czc` 多一个 merge 提交 `6fa3c282c`。
- 环境注意：未跟踪文件 `frontend/pnpm-workspace.yaml` 缺 `packages` 字段，所有 pnpm 命令必须带 `--ignore-workspace`。已实测 `cd frontend && corepack pnpm@9.15.9 --ignore-workspace exec vitest run src/views/public/__tests__/GuideView.spec.ts` 通过（5 passed）。

### Approved Direction

内容真相分两层：仓库 `docs/guide/` 是内置默认版（改章节集合要发版），数据库按 slug 存管理员覆盖版（保存即生效）。这样既满足「每章一个独立文件」，又不需要给容器挂可写卷、不引入路径穿越与并发写文件风险。

版本与并发沿用现有单一递增 `guide_version` + `expected_version` 乐观锁，历史存整份章节快照，因此「回退到某个时间点的整篇教程」仍是一个动作。

章节清单以数据库为准：仓库新增章节不会自动出现在已发布自定义教程的站点，由后台「引入内置教程中缺少的章节」按钮显式决定，避免管理员删掉的章节反复复活。

slug 稳定性优先：仓库章节 slug 取自文件名（`030-recharge.md` → `recharge`），保持 `#recharge`、`#error-codes` 等既有锚点与内部链接不变；后台新建章节时自动生成一次并锁定，改标题不改 slug。

### Discovery Decisions

- 存放方式：仓库拆文件 + 数据库按章节覆盖（用户选项 1）。
- 编辑能力：内容 + 新增 + 删除 + 拖动排序全支持（用户选项 2）。
- 旧内容迁移：读取时自动按 `##` 拆分为章节，保留原值与历史，不丢弃（用户选项 1）。
- 单文件去向：删除 `docs/guide.zh.md`，前端用 `import.meta.glob` 聚合 `docs/guide/*.md`（用户选项 1）。
- 版本与并发：全局递增版本号 + 整份快照历史（用户选项 1）。
- 章节文件格式：`NNN-slug.md`，首行即 `## 标题`，无 front matter（用户选项 1）。
- 新章节合并：数据库清单为准 + 后台手动引入按钮（用户选项 1）。
- slug 生成：后台新建章节按标题自动生成（用户选项 2），创建时算一次、之后固定（用户选项 1）。仓库既有章节 slug 取文件名以保住已分发锚点链接。
- 编辑器布局：左侧章节列表 + 右侧单章编辑与预览（用户选项 1）。
- 分支：从最新 `dev-czc` 新建 `feature/guide-sections`（用户选项 1）。合入、推送、打标签均需另行授权，不在本目标内。
- 假设：教程版本升到 `1.3`，`docs/guide/` 版本章节与 `GuideView.vue` 的 `bundledGuideVersion` 同步，`guideBatAsset.spec.ts` 的 `1.2` 断言一并更新为 `1.3`。
- 假设：拆分迁移遇到首个 `##` 之前的前言文本时，归入独立章节，slug 用 `preface`，不丢内容。
- 假设：容量限制为单章 64 KiB、全部章节合计沿用 256 KiB、章节数上限 64，超限返回明确错误码。
- 假设：e2e 扩展现有 `guide-admin.spec.ts` 覆盖「选章 → 编辑 → 保存 → 版本 +1」，不新增 spec 文件。
- 不适用：ent schema 变更与 SQL 迁移（`setting` 为 KV 表，仅新增设置键）。
- 不适用：权限模型改动（沿用现有 admin 路由鉴权）。
- 不适用：密钥与合规（不涉及任何凭据）。

### Scope

- 新建 `docs/guide/` 目录并拆分为 16 个 `NNN-slug.md` 文件，slug 沿用 `guideMarkdown.ts` 现有映射表的 id；删除 `docs/guide.zh.md`。
- 放行章节目录：`.gitignore`、`.dockerignore` 增加 `!docs/guide/` 相关规则，`Dockerfile` 的 COPY 改为 `docs/guide/`。
- `frontend/src/utils/guideMarkdown.ts`：新增章节文档模型（slug/title/order/content）、按 `##` 拆分整篇内容的迁移函数、后台新建章节的 slug 生成函数；`buildGuideDocument` 支持从章节数组构建，锚点 id 直接用 slug。
- 新增 `frontend/src/utils/guideSections.ts`（或等价模块）用 `import.meta.glob('../../../docs/guide/*.md', { query: '?raw', eager: true })` 聚合内置章节并按文件名序号排序。
- `frontend/src/views/public/GuideView.vue`：内置版改为读取聚合章节；已发布的数据库章节按 slug 覆盖对应章节；`bundledGuideVersion` 改为 `'1.3'`。
- `frontend/src/views/admin/settings/GuideEditor.vue`：改为左侧章节列表（新增/删除/拖动排序）+ 右侧单章编辑与预览；保存仅提交当前章节；保留版本历史、恢复内置、冲突重载；新增「引入内置教程中缺少的章节」入口。
- `frontend/src/api/guide.ts` 与 `frontend/src/api/admin/settings.ts`：新增章节形态的类型与请求。
- 后端：`domain_constants.go` 新增章节相关设置键；`setting_guide.go` 增加章节读写、拆分迁移、容量与数量校验；`internal/handler/setting_handler_guide.go` 与 `internal/handler/admin/setting_handler_guide.go` 暴露章节 API；`routes/admin.go` 与 `routes/auth.go` 按需登记路由。
- i18n：`zh/admin/settings.ts` 与 `en/admin/settings.ts` 同步新增文案键。
- 测试：更新 `GuideView.spec.ts`、`guideBatAsset.spec.ts`、`GuideEditor.spec.ts`、`e2e/guide.spec.ts`、`e2e/guide-admin.spec.ts`；新增 Go 侧章节拆分与校验单测（沿用 `setting_guide_test.go` 的 `guideSettingRepo` 假实现风格）。
- 从最新 `dev-czc` 创建并切换到 `feature/guide-sections` 后再改动。

### Out Of Scope

- 合入 `dev-czc` / `main-czc`、推送远端、打标签、写发版说明。
- 教程正文的内容改写（除拆分必需的机械切分与版本号行）。
- 后端运行时读写磁盘上的 `docs/guide/`（已明确否决）。
- 每章独立版本号与独立历史（已明确否决）。
- 富文本/WYSIWYG 编辑器、章节级权限、多语言教程正文。
- 未跟踪文件 `frontend/pnpm-workspace.yaml`、`backend/internal/service/zz_live_test.go` 的处理。

### Verification

全部命令在仓库根执行，前端命令统一带 `--ignore-workspace`。逐项均以命令退出码为判据，且必须观察到实际执行的用例数量而非「无错误输出」。

1. 章节文件完整性（正向证据）：`ls docs/guide/*.md | wc -l` 输出必须为 16；`for f in docs/guide/*.md; do head -1 "$f" | grep -q '^## ' || { echo "BAD:$f"; exit 1; }; done` 必须退出 0；`test ! -e docs/guide.zh.md`。
2. 内容无损：拆分后所有章节按序号拼接的结果，与 `git show dev-czc:docs/guide.zh.md` 的正文在忽略章节间空行差异后一致——用脚本比对并以退出码判定，禁止用 `|| true`，禁止用裸 `! grep` 做缺失断言。
3. 章节文件已被 git 跟踪（防 `.gitignore` 漏放行）：`git check-ignore -q docs/guide/010-quick-start.md` 必须返回非 0（即未被忽略），且 `git status --porcelain docs/guide/ | wc -l` 必须等于 16。
4. 前端类型检查：`cd frontend && corepack pnpm@9.15.9 --ignore-workspace exec vue-tsc --noEmit`，退出码 0。
5. 前端单测：`cd frontend && corepack pnpm@9.15.9 --ignore-workspace exec vitest run src/views/public/__tests__/GuideView.spec.ts src/views/admin/settings/__tests__/GuideEditor.spec.ts src/utils/__tests__/guideBatAsset.spec.ts src/router/__tests__/guide-route.spec.ts`，退出码 0，且输出的 `Test Files`/`Tests` 计数必须 ≥ 现有基线（GuideView 现为 5 个用例）；出现 `0 passed` 或 `no test files found` 视为失败。
6. 前端构建（证明 `import.meta.glob` 路径与 `docs/guide/` 在构建期可解析）：`cd frontend && corepack pnpm@9.15.9 --ignore-workspace run build`，退出码 0，且 `frontend/dist/index.html` 存在且非空。构建前先删除 `frontend/dist` 以保证证据来自本次运行。
7. 后端编译与单测：`cd backend && go build ./...`；`cd backend && go test ./internal/service/ -run 'Guide' -count=1 -v`，退出码 0，且输出中必须出现新增的章节拆分/校验用例名并为 `--- PASS`；`ok ... no test files` 或 0 个匹配用例视为失败。
8. 后端全量回归（受影响包）：`cd backend && go test ./internal/service/... ./internal/handler/... -count=1`，退出码 0。
9. e2e（浏览器可用时）：`cd frontend && corepack pnpm@9.15.9 --ignore-workspace exec playwright test e2e/guide.spec.ts e2e/guide-admin.spec.ts`，退出码 0，且 Playwright 报告的用例数 > 0。删除上次输出目录后再跑，确保证据属于本次运行。
10. 手动验收：管理员在 `/admin/settings` 教程页选中任一章节、修改正文、点击保存后，页面显示版本号 +1；刷新 `/guide` 后该章节内容已变化、其余章节不变、目录锚点仍为原 slug；`#recharge`、`#error-codes` 直接跳转有效。
11. 手动验收：对已存在整篇 `guide_content` 的库，首次打开教程页即看到按 `##` 拆好的章节列表，且拼接内容与原整篇一致。

任一验证项的证据缺失、过期、不可读或不确定，一律记为失败，不得改动断言使其通过。

### Risks And Rollout

- 主要风险：`import.meta.glob` 的相对路径在 vite 构建、vitest 与 Docker 三种上下文下解析行为不一致。缓解：验证项 4/5/6 三个上下文各跑一次，Docker 侧以 `Dockerfile` COPY 规则加 `.dockerignore` 放行规则的一致性作为静态保障。
- 主要风险：`.gitignore` 的 `docs/*` 规则导致章节文件静默不入库，本地正常但 CI 与镜像里教程为空。缓解：验证项 3 显式检查。
- 兼容性：已发布整篇内容的站点靠读取时自动拆分兼容；旧版 `guide_content` 键与历史记录保留不删，可回退。
- 回滚：`git checkout dev-czc -- docs/ frontend/ backend/` 或直接丢弃 `feature/guide-sections` 分支；数据库侧因保留旧键，回滚代码后整篇模式仍可读。
- 发布沟通：需要一条发版说明，但撰写与合入不在本目标内。

### Stop Conditions

- 需要新增 ent schema 或 SQL 迁移才能落地时，停下询问（当前判断为不需要）。
- 「内容无损」验证项无法在忽略空行差异下达成一致时，停下报告差异，不得删改教程正文来凑通过。
- Playwright 浏览器缺失、无法启动或超时：不修改断言、不跳过用例，把该项报告为未验证并说明原因。
- 需要改动教程正文实质内容、或需要改动测试里既有的锚点/措辞断言语义（而非仅版本号 `1.2`→`1.3`）时，停下询问。
- 需要触碰 `frontend/pnpm-workspace.yaml`、`backend/internal/service/zz_live_test.go` 或任何未跟踪文件时，停下询问。
- 发现锚点 slug 变更会破坏已分发链接（如 `#recharge`、`#error-codes`）时，停下询问。
- 需要合入、推送或打标签时，停下询问。

## Completion Rule

Do not mark this goal complete until the objective is achieved and every required verification item passes, unless the user explicitly changes the completion standard.
