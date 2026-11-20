# 单用户 GPT 缓存诊断

## 原生 Responses 的试点和验收

对使用 `/v1/responses` 的 GPT 文本客户端，在服务环境中设置
`SUB2_RESPONSES_CACHE_AFFINITY_SCOPE=key:<本站密钥的数字 ID>` 可为一把密钥启用增强的会话路由。
先在同一把密钥、同一模型和相近任务上记录基线。验收通过后可设为 `all`，移除此变量可还原原有路由。
环境变量在服务进程启动时生效，请按生产发布流程安排重启。采样和路由开关可以独立启停。

试点也会在原生 Responses 转 Codex OAuth 时保留可验证的完整函数调用配对。客户端回送上游的
`function_call.id=fc_…`、`call_id=call_…` 以及对应工具结果时，网关可以将它们与上游连接保存的历史逐项比较。
只有当前凭据、执行作用域和完整历史均匹配时才会使用增量续接。内容变动时继续发送完整历史。

能配置客户端或中转时，优先透传真实会话的 `session-id`，同一对话在 HTTP 各轮及每次 WebSocket 握手上使用相同值。
并行子线程各有自己的 `thread-id`。完整历史按原顺序发送，只追加新项。
也可在同一密钥的「使用密钥 → Claude Code」复制 Messages 配置，用一条新对话作对照。

原生 Responses 采样使用以下独立配置，和下面的 Chat 采样各有自己的限额和输出文件。
目录由服务用户独占（`0700`），每个窗口选用新文件。模型填实际路由到的 GPT 文本模型。

```text
SUB2_RESPONSES_CACHE_DIAGNOSTIC_USER_ID=<本站用户数字 ID>
SUB2_RESPONSES_CACHE_DIAGNOSTIC_API_KEY_ID=<该用户的密钥数字 ID>
SUB2_RESPONSES_CACHE_DIAGNOSTIC_MODELS=gpt-6.1-sol
SUB2_RESPONSES_CACHE_DIAGNOSTIC_PATH=/opt/sub2api/data/cache-diagnostics/responses-<UTC时间>.jsonl
SUB2_RESPONSES_CACHE_DIAGNOSTIC_UNTIL=<未来一小时内的 UTC RFC3339 时间>
SUB2_RESPONSES_CACHE_DIAGNOSTIC_MAX_REQUESTS=100
```

按窗口和经过 HMAC 的会话、线程标识比较相邻轮次，检查账号是否切换，以及客户端原始请求中的 instructions、tools 与历史 input 项的摘要是否变化。WebSocket 样本还记录本窗口的连接编号和该连接收到的轮次。同一连接可以服务多个线程。匿名 HTTP 请求和无法关联的重连单列。片段或带 `previous_response_id` 的增量输入与全量历史分别比较。对于 HTTP 转上游 WebSocket 的请求，`http_ws_incremental.reason` 记录网关检查点的判定，例如历史项不匹配的 `input_mismatch`、请求字段变化的 `request_property_changed`，或成功选中增量的 `incremental`。实际缓存读取仍以返回用量为准。窗口结束后诊断会停止，跨窗口的摘要不能关联。

每次入站请求最多 32 MiB，最多记录 1024 个历史项，摘要输入预算为 4 MiB。单个值超过 256 KiB 时仅提供原始字节的 HMAC。遇到截断或无法生成规范化摘要的标记时，只比较有完整证据的部分。每个文件最多 16 MiB。采样名额按入场计数，最终路由到其他模型或产生媒体结果的请求会占用名额，但不会写入样本。

用可关联的生产记录比较改动前后至少 100 个同类续轮，首轮与轮次未知的请求单列。按实际模型、入站接口、输入 token 长度档分组，汇总缓存读取 token，除以普通输入、缓存写入和缓存读取 token 的总和。上游诊断的 `usage.input_tokens` 已含缓存部分，该口径直接用缓存读取除以总输入。`zero_or_unreported` 单列。同步核对错误率、流式完成、工具调用配对和重复扣费。满足试点目标后扩大范围，再观察一个完整的 24 小时业务周期。

## Chat Completions 转 Responses

这项诊断观察现有请求，不产生额外上游调用，不调整缓存标识、协议转换、调度或计费。
适用于 `/v1/chat/completions` 转 Responses 的路径，覆盖流式和非流式响应。
默认关闭，不支持通过客户端请求头开启。

## 开启范围

在安装包含本功能的正式 `czc-*` 发布后，为服务设置以下环境变量；时间值在实际采样前生成：

```text
SUB2_USER_CACHE_DIAGNOSTIC_USER_ID=428
SUB2_USER_CACHE_DIAGNOSTIC_PATH=/opt/sub2api/data/cache-diagnostics/428-<UTC时间>.jsonl
SUB2_USER_CACHE_DIAGNOSTIC_UNTIL=<采样开始后20分钟的UTC RFC3339时间>
SUB2_USER_CACHE_DIAGNOSTIC_MAX_REQUESTS=30
```

创建仅服务用户可访问的目录（`0700`）。输出文件由服务以 `0600` 创建，使用每轮独立的新文件名。
父目录必须由服务用户独占且位于本地文件系统；诊断同步追加小量摘要，不适合网络盘。
已有符号链接、设备、目录、组/其他用户可读文件均拒绝写入；写前核对已打开文件的身份、权限和大小。
配置需要同时满足：正整数用户 ID、绝对文件路径、未来一小时以内的到期时间和 1–100 次请求上限。
省略请求上限时默认 30 次；任何其他必填配置缺失或非法都不采样。
仅依据服务端已经认证的 API Key 用户 ID 决定是否允许采样，其他用户不会产生诊断记录。
可用 `SUB2_USER_CACHE_DIAGNOSTIC_MODEL` 指定本轮采样的入站模型，例如 `gpt-6.1-sol`、`gpt-6.2-sol` 或 `gpt-6.2-astra`。筛选值与客户端入站 `model` 字符串逐字匹配，包括大小写和空白。即使分组推理策略、复合路由或渠道随后改写请求，`ingress` 仍描述改写前收到的请求。省略筛选或留空时，该用户所有模型的请求都可以进入通用诊断。
不匹配请求不写文件、不占采样名额；该拒绝在同一入站请求内固定，重试更换 body 也不能绕过。
启用模型筛选时，入站须为不超过 32 MiB 的合法 JSON 对象，顶层只允许一个精确命名为 `model` 的字段；
重复字段（包括转义后的同名键）及 `Model`/`MODEL` 等大小写别名均拒绝采样，嵌套对象中的同名字段不影响筛选。

每个入站请求最多观察 4 次上游尝试、最多写 13 条记录（1 条入站，以及每次尝试的请求、响应和目标模型下游交付记录各 1 条）。交付记录绑定到对应的已采样尝试，失败后的重试也使用同一个总上限。
重试各自携带独立的尝试/上游账号/用量关联信息，迟到的前次响应不会归到下一次尝试。到期后停止新增样本和所有写入，包含未结束请求；
因此没有响应事件的样本不能解释为“上游没有返回用量或推理项”。输出文件达到 16 MiB 后停止写入。
采样结束后移除临时环境配置，避免日后运维误用；到期时间不更新时，重启也不会重新采样。

## 记录内容与关联

- `ingress`：认证用户、API Key、请求关联哈希、会话哈希，以及转换前的字段和消息摘要。
- `upstream_request`：上游账号、尝试序号、实际发送请求的字段/消息摘要和会话相关请求头的进程内 HMAC。
- `upstream_response`：HTTP 状态、终态类型、上游原始数值用量、输出项类型，以及加密推理项的长度和哈希。
- `chat_delivery` 记录实际消费者的返回状态、用量、成功写出的响应 ID 与 assistant 哈希，以及原生续接模式、资格和恢复组数。上游为 `gpt-6.1-sol` 的兼容转换路径沿用既有采样范围。其他 GPT 对话模型要求入站含 Chat `messages` 数组，且入站与出站精确匹配同一模型。原生续接关闭时也可在已开启的有界诊断中观察交付。

GPT 对话模型的 `ingress.assistant_tail` 包含最多 4 条最近的入站 assistant 消息的位置和字段摘要。`gpt-6.1-sol` 以外的 GPT 模型仅为带 `messages` 数组的 Chat 输入采集这些字段。请求成功、下游完整写出且该次尝试的出站模型与入站精确模型一致时，`chat_delivery.assistant_comparison` 使用相同规则记录已交付的 assistant。
二者直接调用原生续接使用的消息归一化函数，同时保留完整 JSON 摘要，区分引擎已经允许的空字段表示差异。
固定字段只记录存在性、JSON 类型、长度及 HMAC；未知字段只记录数量和整体 HMAC，不记录未知字段名。
HMAC 使用不落盘的进程随机密钥，并绑定用户、API Key 和用途；只允许在同一进程及既有会话、账号、模型条件下比较。
随机源失败、重复 JSON 成员、解析或资源上限失败均显式标为不可用，不回退到可预测密钥或部分匹配。
每个 assistant 最多处理 256 KiB，入站最后 4 项总计最多 1 MiB，新增摘要每事件最多 16 KiB；
字段诊断额外要求完整入站 JSON 不超过 4 MiB，超出时只将 `assistant_tail` 标为不可用。
原有采样次数、期限、文件和请求大小限制保持不变。更早的 assistant 未被字段采样，不代表内容相同。

字段摘要是定位差异的证据，不会参与恢复判定或修改请求。原始 JSON 哈希不同但原生归一化 HMAC 相同，
只能证明该消息满足已有的归一化规则；仍须核对完整历史、参数、身份、实际出站前缀、完整原生项和用量。
非空 `reasoning_content` 会进入转换后的上游输入，不能将字段缺失或变化当作可忽略信息。
`restored_groups=0` 本身也无法区分保存未成功、历史不匹配、容量限制与投影失败。

每个消息摘要含位置、结构哈希和 JSON 值字节长度，不包含提示词、模型回答、工具参数或工具结果原文。
不记录 Authorization、Cookie、API Key、错误正文、原始会话 ID、原始响应 ID 或加密推理内容。
只记录固定白名单中的协议枚举、字段名和数值用量；未知内容只参与哈希。
用量数字保留上游 JSON 的原始数字表示（包括大整数、小数及指数），不先转成浮点数或整数；
未知用量字段、字符串用量和超过 128 字符的异常数字不记录。
`usage_request_id_sha256` 是最终用量请求 ID 的 SHA-256，优先采用中间件的 `client:`/`local:` 标识，
缺失时采用上游响应头的请求 ID。没有可关联标识时留空，不创建假的用量关联。

可在只读 SQL 中计算 `encode(sha256(convert_to(request_id, 'UTF8')), 'hex')`，
与同一用户、同一时间窗口的 `usage_logs` 对齐。对齐后比较 `cache_read_tokens`、
普通输入加缓存读写的总输入、输出、账号，以及原始 `response.usage` 的相应字段。

请求最多解析 32 MiB、展示前 1024 个消息摘要；超限会带标记。
响应单行及完整 SSE 事件的解析缓冲分别不超过 4 MiB，非 SSE JSON 文档不超过 4 MiB；
最多汇总前 128 个输出位置。每次上游尝试最多观察 32 MiB 或 8192 个事件；
超过限制时停止解析，继续原样传递后续响应，并在最终事件记录 `observation_limited`。
SSE 多行 `data:` 按协议合并，缺少空行分隔的尾部不会被当作完整事件。
成功的 2xx 响应还依据最终出站请求的 `stream: true` 选择 SSE 观察，覆盖上游缺少或误标
Content-Type 的情况。HTTP 错误和非流式 JSON 响应沿用 JSON 观察规则。
超长行/事件会带 `truncated_events`；读取错误和不完整事件分别带 `read_error`、
`partial_event`，这些标记表示证据不完整。`eof=false` 仅说明没有读到传输 EOF；
消费者在完整 SSE 终态后正常关闭时，不必再读取 EOF。
响应不会被提前读出、截断、改写或重放，观察器只处理正常消费者已读出的字节。
`invalid_json_events`、缺少终态、到期和上限都需要作为采样不完整处理。

## 判断顺序

1. 按认证用户、会话哈希和模型字段哈希分组，确认上游账号及缓存 key 是否稳定。
2. 比较连续请求的消息哈希，分别检查客户端入站和实际出站的共享前缀。
3. 对比同一请求上游数值用量与数据库落库值。分别汇总首轮和连续轮次，按缓存读取 token 总和除以普通输入、缓存写入和缓存读取 token 总和计算加权命中率，并单独统计具备有效输入的请求中有缓存读取的笔数。
4. 检查输出中是否返回 `reasoning`/加密状态，以及下一轮是否携带相应状态；检查是否出现托管搜索等额外输出项。
5. 根据实际结果再决定是否修改兼容层。缺少加密状态承载能力本身不证明它解释了全部缓存下降，不能据此调整或伪造缓存 token。

## 发布与退出

这是应用代码变更，必须遵守仓库 AGENTS.md 的完整 VERSION、发布说明、不可变 `czc-*` 标签、GitHub Release 和标签构建部署流程。
启用后先确认一个目标用户样本，再核对其他用户没有被记录；限额或到期后确认日志不再增长。
退出采样不需要更改数据库、用户 Key 或上游账号。保留受限访问的哈希证据用于核对，按运维保留策略清理。

### Terminal candidates and read errors

The observer preserves `read_error` as an I/O fact. New `read_error_phase`
values distinguish `none`, `before_terminal_candidate`,
`same_read_after_terminal_candidate`, and `after_terminal_candidate`.
`read_error_class` is a fixed category (`none`, `eof`, `closed`, `canceled`,
`deadline`, `unexpected_eof`, or `other`); error text is never recorded.
`local_close_started` is latched before delegating Close, and
`read_error_observed_after_close_started` describes the order in which the
observer records the read result. It does not establish the physical cause or
exact return time of an error. Close does not wait for an arbitrary active
reader: that reader publishes its result and the single final record when it
returns. No additional read is issued and original bytes/errors are unchanged.

A `terminal_candidate` is a fully framed, strict JSON/UTF-8 observation of the
first completed/done event with a response object and ID. It is **not protocol
success or replay eligibility**. It freezes response-ID hash, status class,
current-terminal numeric usage, item summaries and sticky prefix error,
identity-conflict, invalid, truncated and limited flags. Missing terminal usage
never inherits earlier usage. Bytes after this boundary are counted separately
by `post_terminal_bytes` and `post_terminal_observation_limited`; they cannot
rewrite the prefix's item/usage evidence. Numeric values keep their original
JSON-number representation.

A consumer may accept a post-terminal close/cancellation read error only with
independent successful protocol evidence for the same actual request, matching
response-ID and usage, and a clean candidate prefix. Missing/other status,
earlier errors (including response.error/auth/quota), malformed/duplicate JSON,
invalid UTF-8, identity conflicts, limits, partial frames and unknown/deadline
errors do not gain an exception. All original native item/opaque/full-history
checks remain required. Historical boolean-only read errors cannot be relabeled
as post-terminal evidence, and production rollout analysis without an
independent consumer proof must keep such cases uncertain.

For the target Chat bridge, `chat_delivery` adds independent evidence from the
actual response consumer and successful writes. A complete streaming record
requires an assistant role, a supported finish reason and `[DONE]`; buffered
records require one complete assistant response. Short writes, errors, partial
frames and capture limits prevent `downstream_delivered_complete`. The capture
uses the native replay limits (256 KiB logical content by default) and never
writes content to the diagnostic log. `forward_returned_success`,
`context_canceled` and `downstream_delivered_complete` remain separate facts:
no single boolean establishes completion. The Chat forward result does not
reliably populate its disconnect flag, so this diagnostic omits that flag.
Join the same trace/attempt and require a
matching `consumer_response_id_sha256` and `consumer_usage` before applying the
post-terminal exception above. Successful server writes do not prove a remote
client read the bytes. The additional evidence does not change the forwarding
result, replay admission or billing, and cannot upgrade old samples that lack it.
