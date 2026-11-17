# OpenAI OAuth Bad Request 排查与恢复

OpenAI OAuth 的 Responses HTTP 请求可能在启用 zstd 请求压缩时收到只有
`Bad Request` 的 HTTP 400。当前默认发送原始 JSON；此设置同样适用于
Chat/Messages 转换为 Responses 和 WebSocket 回退 HTTP 的路径。
HTTP 400 也可能来自上下文超限、模型或参数不支持，因此不能仅凭错误码归因。

## 请求压缩

```yaml
gateway:
  openai_request_compression_enabled: false
```

对应环境变量为 `GATEWAY_OPENAI_REQUEST_COMPRESSION_ENABLED=false`。
默认值为 `false`；无需关闭 `gateway.openai_ws.exact_fidelity_enabled`。
身份头、TLS、WebSocket 和请求 JSON 内容仍按原有流程处理。配置需要运行包含
该开关的版本并重启服务后生效。将值改回 `true` 可恢复原有压缩行为；显式开启时
仍须满足原有 OpenAI OAuth/Responses/exact-fidelity 条件。

关闭压缩会增加上传流量，较大请求可能增加上传耗时。观察实际 HTTP 编码、
请求长度、最终失败率、首字节耗时和超时率。请求字节大小不等于模型 token 数。
不要直接删除会话历史，也不要将全部 400 设置为自动重试或切换账号。

通用 400 的结构诊断新增 `http_request_compression_policy`，分别记录
`disabled`（已关闭）、`zstd`（实际执行压缩）、`ineligible`（不属于压缩适用路径）、
`existing_encoding`（保留请求已有编码）。缺少此次传输标记时为 `unknown`，不会
依据历史请求推断。结合 `http_content_encoding`、`http_request_content_length`
和 `http_protocol` 查看当前失败尝试；诊断只记录固定分类和计数，不保存正文或凭据。

## `gpt-6-astra` 的 `none` 推理参数

上游可能拒绝 `gpt-6-astra` 的 `reasoning.effort=none`。Claude Code 使用的
Messages 协议中，显式 `thinking.type=disabled` 会转换为 `none`，并优先于
`output_config.effort`。修复后的兼容入口会对这种显式关闭请求应用已配置的分组
推理策略，同时保留请求强度 `none` 和实际发送强度 `low` 的分别记录。

只在已经确认受影响、允许该兼容行为的分组中追加以下规则：

```json
{
  "from": "none",
  "to": "low",
  "match_type": "exact",
  "model": "gpt-6-astra"
}
```

`low` 会启用推理，可能增加耗时和用量。确实要求关闭推理的调用，应选择支持该
能力的模型。该规则精确匹配转换后发给 OpenAI 的 `model`；若账号模型映射把别名
解析为 `gpt-6-astra`，也会命中。其他最终模型和已合法的强度保持原有策略；未指定
强度的 Messages 请求继续使用兼容层默认值。没有该规则时，不会自动将 `none`
升为 `low`。

管理界面可以编辑分组的推理强度映射，或使用以下管理 API：

1. 根据错误记录中的 `group_id` 确认实际分组和 API Key 当前绑定关系；确认平台
   是 OpenAI，或复合分组的此次请求实际路由为 OpenAI。不要对所有分组批量修改。
2. `GET /api/v1/admin/groups/:id`，保存在本机的原始 `reasoning_effort_mappings`，
   并核对已有精确匹配、前缀、后缀和全局规则。已有同一精确规则时不重复追加；
   存在不同目标的同一规则时先明确最终策略。
3. `PUT /api/v1/admin/groups/:id`，仅发送 `reasoning_effort_mappings` 字段。
   该字段是**整数组替换**，必须在原数组上追加规则；不要用单条规则覆盖其他映射。
   空数组会清空全部映射，省略该字段则保持原样。写入前再次读取并确认旧规则未被
   其他管理员更改；此接口没有提供条件更新保证。
4. 再次 GET 核对映射及其他配置。通过管理 API 更新会使该分组的认证缓存失效，
   不要直接修改数据库后假设缓存已同步。
5. 用合成请求验证 `thinking.type=disabled`、同带 `output_config.effort=high`、
   合法 `high`、未指定强度、其他模型等场景，核对请求与实际推理强度和最终状态。

对于仅带 `thinking.type=disabled` 的请求，需要包含入口绑定修复的新版本；旧版本
只在存在非空 `output_config.effort` 时绑定策略，单独保存映射不能完成这类修复。

回滚映射时，读取最新规则，仅移除本次新增且内容仍一致的精确规则，保留后来新增的
其他规则；如果期间没有其他更改，可以通过同一 PUT 接口恢复备份数组。再次 GET
验证，并使用受影响客户端的合成请求核对结果。

## 验证与发布

本地回归覆盖压缩开关及配置读取、完整转换转发、HTTP 重放长度、明确关闭推理的
策略绑定、原始强度记录、模型匹配边界和既有模型校验。真实上游对照必须使用合成
内容，比较同账号、同模型、同 JSON 在压缩与原文传输下的最终结果；只看到 HTTP
200 还需检查流式终态。

发布遵循仓库 `AGENTS.md`：测试通过后合入 `dev-czc`、`main-czc`，提升 VERSION，
创建不可变 `czc-*` 标签及 origin GitHub Release，从标签重建前端和 embed 服务端。
备份生产程序与修改的配置，验收首页、健康检查、版本及 `current_release_url`。
压缩开关、分组映射和二进制版本分别核对与回滚，避免把配置变更误报成已部署代码。
