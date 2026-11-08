# 单用户 Chat Completions 缓存诊断

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
仅依据服务端已经认证的 API Key 用户 ID 筛选，其他用户不进入解析或记录路径。

每个入站请求最多观察 4 次上游尝试、最多写 9 条记录（1 条入站、每次尝试最多 2 条）。
重试各自携带独立的尝试/上游账号/用量关联信息，迟到的前次响应不会归到下一次尝试。到期后停止新增样本和所有写入，包含未结束请求；
因此没有响应事件的样本不能解释为“上游没有返回用量或推理项”。输出文件达到 16 MiB 后停止写入。
采样结束后移除临时环境配置，避免日后运维误用；到期时间不更新时，重启也不会重新采样。

## 记录内容与关联

- `ingress`：认证用户、API Key、请求关联哈希、会话哈希，以及转换前的字段和消息摘要。
- `upstream_request`：上游账号、尝试序号、实际发送请求的字段/消息摘要和会话相关请求头的进程内 HMAC。
- `upstream_response`：HTTP 状态、终态类型、上游原始数值用量、输出项类型，以及加密推理项的长度和哈希。

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

请求最多解析 32 MiB、展示前 512 个消息摘要；超限会带标记。
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
3. 对比同一请求上游数值用量与数据库落库值，确认统计口径。
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
