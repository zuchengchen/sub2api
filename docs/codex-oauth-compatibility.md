# GPT OAuth 与 Codex 出站兼容性

本轮参考是官方 `openai/codex` 提交
`e54a5713caf331f42a6eef0166186df5f7563c29`，从该提交的锁定依赖构建。
开发构建的实际版本是 `codex-cli 0.0.0`；不得改标为已发布版本，或用其他
版本的 CLI、模型目录、伴随程序和旧采样代替这份参考。

多人共享网关具有多个下游用户、API key、客户端协议、调度和后台任务。
这些业务行为会产生与单个 CLI 进程不同的请求数量和时序。协议兼容、身份
隔离和真实请求成功都不能证明 OpenAI 的内部客户端分类，也不能证明它将
该网关视为“合法单个 Codex 用户”。本次保留既有共享、调度、并发与计费
能力；完全使用官方客户端或每个用户独立授权属于需要另行决定的架构方案。

## 身份与状态边界

- OAuth owner 同时包含选中的 ChatGPT 工作区和登录用户。工作区保留显式
  选择；托管 OAuth 的用户身份优先取当前 ID token 的
  `chatgpt_user_id`，缺少该字段时取 `user_id`。仅导入 access token 时可从
  access token 读取用户身份。陈旧的展示字段不能覆盖当前 token 的用户。
- 读取 token 中的 claims 只用于本地状态连续性，不是独立的认证证明。
  身份不完整时使用凭据摘要区分快照；未知身份的凭据变化不能自动继承旧
  会话状态。完整 owner 相同的正常 token 刷新可保留会话。
- token 缓存与刷新锁继续使用原来的账号行键，避免拆分跨进程刷新互斥。
  缓存中的不同 token 需要已知且匹配的工作区和用户；不匹配时按未命中
  处理，不由旧请求删除另一个 owner 的缓存。
- JWT 解析结果缓存仅保存不可变的身份提示，最多 1024 个、每个 token
  不超过 16 KiB；超限 token 仍解析但不缓存。每次使用仍与当前账号比较，
  解析缓存不充当授权、有效期或撤销状态缓存。
- 普通 OAuth 刷新统一使用账号行锁与完整凭据、代理配置的数据库 CAS。
  凭据写入和 scheduler outbox 同一 SQL 原子提交；并发重新授权或编辑
  获胜时，不用旧刷新结果覆盖它。管理员事务中的缓存发布和刷新锁释放
  等待提交或回滚。持有 SQL 行锁的管理员事务只尝试立即取得本地刷新锁，
  避免与先持刷新锁、后写账号行的后台任务互相等待；争用沿用 `LockHeld`
  返回。上游已消耗的 refresh token 无法随本地回滚恢复。
- 两个管理员刷新入口和 CRS 导入后的刷新使用同一协调器。直接接收粘贴
  refresh token、没有账号行 ID 的导入接口不在此行级互斥边界内；把同一
  refresh token 复制到多个账号行也不能获得跨行互斥保证。
- WS 连接的兼容性包含 OAuth owner，即使指纹配置为 `off` 或 `device`
  也不能复用另一个 owner 已认证的连接。改变 bearer 本身不会使完整且
  相同的 owner 被视为另一个用户。`passthrough` 与 `ctx_pool` 的首帧、
  后续帧及握手复用同一套 seed 身份处理，安装身份收敛仍保留 API key 的
  会话隔离。
- 实际返回的 HTTP 响应以及成功 WS 响应的 `previous_response_id`
  单独绑定 OAuth owner 和本站用户，
  不随调度粘连 TTL 续期改写来源。HTTP 透传、HTTP 转 WS 及原生 WS 每帧
  在最终账号处校验已知来源；同一身份正常刷新与同一用户的多个 key
  保持既有兼容行为。未知或过期来源仍走原有原生续接规则，不能声称已验证
  其来源。Redis 使用首次写入生效，确认结果后才发布本地来源；已知状态
  的缓存故障不会冒充未知来源放行。
- turn-state 按下游 API key、原始 session 和实际 blob 的 SHA-256 摘要
  记录来源，避免并发线程、后续响应和旧请求覆盖彼此。记录不保存原始 blob。
  来源记录有过期时间；未知或过期的状态仍遵守既有透传兼容行为。
- HTTP passthrough 在响应头实际提交时登记 turn-state，包括心跳提前提交
  响应头的情况；放弃的 attempt 不登记。WS 在 `response.metadata.headers`
  事件实际交付后登记，并对 `response.create.client_metadata` 中的回带值
  使用相同的 owner 检查。状态在 HTTP 与 WS 之间切换时也受此约束。

## 请求字段与传输

该参考的 `prompt_cache_options` 仅包含 `ttl`，可取 `30m` 或 `12h`。
Responses HTTP、WS 和 Chat Completions 转换路径保留这种原生形状；旧的
Platform `mode` 与显式 breakpoint 不因此获得 Codex 支持。删除不支持的
缓存字段时保留其他原始 JSON 值，包括大整数、用户内容和工具参数。

TTL 的出现、消失或改变会影响 WS 增量续传资格。不能只删除该参数再继续
使用旧的 `previous_response_id` 增量链。新的原生压缩使用普通 Responses
流；遗留 `/responses/compact` 接口是单独的兼容路径，不应混用两者的字段
契约或把新 CLI 的流式压缩采样当作遗留接口的实采。

`full` 指纹验收必须使用实际持久化的 seed 并观察到身份改写。只设置模式
名称、没有 seed，不能证明生产默认路径被执行。官方允许变化的 ID 仍需
保留格式、同异关系和作用域；不能通过把全部 ID 清空来取得比较成功。
启动预热尚未产生真实 turn 时，保留官方的空 `turn_id` 与省略开始时间形态，
避免把预热计成用户提交。已有 turn 的预热继续沿用该 turn。
长 WS 连接的 Upgrade 头可能仍描述启动预热，后续普通请求以当前正文的
request kind 为准；显式 `generate=false` 的旧客户端继续支持 header 预热。

Live 的 call create 与 sideband 使用不同的官方默认地址。新建 Live 记录
保存 `sideband_route=openai_v1`，其 sideband 连接到
`wss://api.openai.com/v1/live/{call_id}`；call create 继续使用 ChatGPT 的
Codex realtime/calls 入口及既有鉴权快照。缺少该标记的旧 Redis 记录仍走
原来的 ChatGPT sideband 地址，未知标记在拨号前拒绝。该字段不改变下游
接口、用户与账号绑定、租约或计费，也不需要 SQL 迁移。

TLS 环境也属于参考的一部分：系统 CA 与自定义 CA 的 HTTP/TLS 路径应分别
采样。WS Upgrade、分片、压缩、控制帧与连接终止都需要实际证据。
原生 Responses 连接回收按官方 owner drop 语义直接关闭传输，不额外发起
空 Close 帧；连接释放仍可复用，接收对端 Close 时仍回应，Live 仍使用
其独立的 code 1000 关闭握手。
HTTP/2 登录、刷新及其他复用客户端在正常响应结束时，先等待已发送请求的
最后 DATA 写入完成并停用写入取消回调，再释放响应。这样响应提前返回不会
因本次请求的清理而关闭整条连接、污染下一条流；显式取消和未完成响应仍
保留中止路径，不通过重试一次性登录请求掩盖失败。
`codexgolden` 的本轮 recording contract 为 2，包含哈希绑定的 WS sidecar。
缺失、截断或无法与实际消息对应的 sidecar 属于无效输入，而不是通过。
具体采样与比较命令见 [codexgolden 说明](../backend/tools/codexgolden/README.md)。

## 出站路径清单

下表用于防止主 Responses 接口通过后遗漏辅助和后台流量。列为“保留”的
路径仍然是可观察差异；它们没有被宣称为单个官方 CLI 的默认行为。

| 路径 | 项目入口 | 参考与处理边界 |
| --- | --- | --- |
| OAuth authorize、code exchange、refresh | `openai_oauth_service.go`、OAuth client、refresh service | 分别核对授权参数、form code exchange、JSON refresh、锁与持久化 |
| Responses HTTP/SSE | `openai_gateway_forward.go`、`openai_gateway_passthrough.go` | 核对请求、身份、流式终态、取消、用量及有限重试 |
| Responses WS | `openai_ws_forwarder_ingress.go`、`openai_ws_v2_passthrough_adapter.go`、`openai_ws_pool.go` | 分别验证 passthrough 与 ctx_pool；不能把 HTTP 回退记为 WS 成功 |
| 原生压缩与续传 | `openai_native_compaction_guard.go`、`openai_compaction_context.go` | 普通 Responses 流的压缩产物、状态与后续输入 |
| 遗留 unary compact | gateway 的 `/responses/compact` 分支 | 独立兼容路径，当前 CLI 不直接覆盖 |
| Chat Completions / Messages | `openai_gateway_chat_completions.go`、`openai_gateway_messages.go` | 保留系统/用户内容、工具关联、推理状态和计费；转换输入不是 CLI 原样输入 |
| Models | `openai_codex_models_service.go` | 普通 OAuth 在请求前检查 token freshness；保留缓存与请求数量证据 |
| Usage | `openai_quota_service.go` | 对照 `/backend-api/wham/usage` 与实际身份；项目管理查询顺序获取 usage/details，官方 app-server 并发获取，保留此时序差异 |
| Reset credits 详情与 consume | `openai_quota_service.go`、`openai_quota_auto_reset.go` | 详情计入请求量；consume 有副作用，仅做本地模拟 |
| Alpha search | `openai_alpha_search.go` | 对照 Codex alpha/search，区分其他凭据类型的兼容分支 |
| Live call 与 sideband | `openai_live.go` | 分开检查 call create 和 WS；保留调用、用户/key/账号绑定及租约 |
| 直接 Realtime WS | 官方 app-server 的 Realtime 入口 | 固定参考的该路径要求 API key；不能把 API-key 正向样本称为 OAuth 样本 |
| Count tokens | `openai_gateway_count_tokens.go` | 额外的 Platform input_tokens 请求及本地 fallback，保留既有兼容功能 |
| 图片兼容与直接图片路径 | `openai_images*.go` | 只以本地模拟验证媒体边界和用量；不进行真实媒体生成 |
| 图片资产下载 | `openai_images.go`、`openai_images_b64_backfill.go` | OAuth 的 file-service/sediment 两阶段下载 helper 没有当前生产调用者；可达 URL 回填只接在 APIKey 图片非流式路径，单列验证与权限边界 |
| 管理员测试与压缩探测 | `account_test_service.go`、`openai_codex_probe.go` | 诊断产生额外调用；不为制造相似度新增探测 |
| 429 恢复探测 | `openai_429_recovery_probe.go` | 既有调度恢复行为，不是单个 CLI 的默认后台循环 |
| Token enrichment | `openai_oauth_service.go`、`openai_privacy_service.go` | 网页账号/订阅查询属于额外保留路径 |
| Training privacy ensure | `openai_privacy_service.go` | 修改上游设置有副作用；本轮真实验收不触发该操作 |
| Referral 查询与邀请 | `openai_referral_client.go` | 独立网页功能，保留；不进行真实邀请 |
| 客户端版本同步 | `openai_codex_version_sync_service.go` | GitHub 额外出站；采样隔离并记录，不与 OAuth 模型请求混淆 |
| 官方独有 device-code、revoke、可选 telemetry 等 | 固定官方源码的登录与遥测路径 | 根据实际触发条件采样或列为非项目路径；不伪造无功能用途的流量 |

## 验证与后续维护

本轮证据保存在项目 `.codex/reports/codex-oauth-egress-20261109T133155Z-iw_im2x9/`。
其中官方源码、模型目录、工具链、构建 receipt、原始合成流量、Go JSON、
基线失败用例和候选结果分别绑定哈希。以该目录最终报告为准；本文不是
测试完成或部署成功的替代凭据。

验收包括两种 CA 环境与两种接入模式的六个核心 CLI 场景、辅助接口实采、
本地故障/隔离回归、race 检查、隔离 PostgreSQL/Redis 集成、前端与 embed
构建、性能对照及串行真实请求。真实请求使用最多两个健康 OAuth 账号，
调用次数不设固定上限；失败、重试、查询与刷新均计入实际调用记录，既有
上游配额和 429 处理不因此改变。

本轮在一个固定 OAuth 账号上完成了官方 CLI 短文本，以及 `ctx_pool` 和
`passthrough` 各六项真实场景：HTTP 工具调用/结果、WS 工具调用/结果、
WS 原生压缩及压缩后续传。压缩验收要求唯一的完成条目、完整终态、用量，
并把生成与实际续传的加密内容摘要关联起来。真实候选经过生产 service
转发；生产路由、鉴权、调度和用量落库由隔离集成覆盖。官方真实短文本
使用终止 TLS 的观测 relay，只证明功能与调用量；TLS/H2 对齐另由本地
四组严格对照证明，不能混用证据范围。

当前官方默认不发送 `prompt_cache_options`。上述六项场景在这一默认形状
下通过；本账号/模型的显式 `30m` HTTP 请求返回 400，错误参数明确为
`prompt_cache_options`。保留字段通过本地协议检查不代表上游允许该功能，
也不能把默认请求成功记为显式 TTL 成功。项目既有的参数拒绝后删字段
重试属于兼容扩展，与固定官方 CLI 的终止性 400 行为有差异。

本账号的遗留 `/responses/compact` 请求返回 404；这不足以断言端点全球
下线。只读配置投影调用同一冻结源码的模型解析函数，确认未采用全局
fallback，正常与 compact 均解析到请求模型。当前官方原生压缩返回
checkpoint，再由客户端保留和组装历史；旧 unary 输出用于替换历史。
不能仅将 checkpoint 包成旧 JSON 就声称无损兼容。显式 TTL 与遗留接口
仍是本轮两个未通过的真实项，用户已明确接受这两项具体限制作为本轮
验收例外；原始失败证据保留。原生六项验收明确省略遗留压缩与遗留压缩
后续传，不能记为全八项通过。批准记录位于证据目录的
`checks/approved-real-limitations.json`；它不代表上游能力已经修复，也不
改变其他账号、模型或未来版本的验收要求。

本轮性能复测中，用户已明确接受两项具体例外：刷新后不同 token 的缓存
命中中位数从 0.163 μs 增至 0.577 μs，分配仍为 40 B/op；64 KiB 请求
删除旧 Platform 缓存配置从 304 μs 增至 478 μs，B/op 增加约 2.6%。
两项分别对应身份校验和保留原始 JSON 的处理成本。

用户另行接受了四项带真实身份重写的 WS passthrough 基准例外：1 KiB 首帧
耗时增加 24%、B/op 增加 32%；256 KiB 首帧增加 19%、27%；1 KiB 后续帧
增加 31%、52%；256 KiB 后续帧增加 32%、44%。这些路径现在执行完整身份
派生与改写，仍有减少重复 metadata 解析和请求体复制的优化空间。
其他基准仍按原 10% 阈值验收。原始五次采样及五次复测、两次具体批准记录
在证据目录的 `checks/` 中；微基准不能解释为整条网络请求增加相同比例延迟。

升级参考时重新固定提交并构建，不仅更新 User-Agent。重点检查官方
`core/src/client.rs`、`login/src/auth`、`codex-api/src/common.rs`、Responses
HTTP/WS endpoint、Realtime endpoint 与模型目录；更新验证器时先证明它能
区分真实差异、无效证据和正常动态字段。

本轮修复的发布版本为 `czc-v2026.11.10.1` / `0.2.19.32`，完整升级、
验证和回滚说明见[发布说明](../.github/releases/czc-v2026.11.10.1.md)。
后续晋升、发布和部署继续遵守 `AGENTS.md`。源码回滚使用后续 revert，
不改写长期分支历史，不清空生产凭据、会话或用量数据。
Live 的旧二进制不认识新 route 标记，混合版本运行或回滚前必须让新版本
创建的 Live 会话结束，再切换整组服务；保留旧记录兼容不代表旧二进制能
正确接管新记录。
