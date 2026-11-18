# KeyPouch 单账号导入

KeyPouch 在 OVH 本机向 sub2api 提交一个邮箱和任务 ID。sub2api 查询上游 OpenAI OAuth 账号，精确选取有效的 `GPT-不降智` 分组。已有唯一账号时只追加这个成员关系，保留禁用状态、其他组与原优先级。已有成员及重试都会从数据库读回。完成后若有人删除了成员关系，旧任务返回 `membership_changed`。

## 配置

| 配置项 | 用途 |
| --- | --- |
| `SUB2API_KEYPOUCH_IMPORT_SECRET` | 与 KeyPouch 服务共享的随机 Bearer 密钥。经服务管理器的受限环境文件注入，修改后由正常发布流程重载服务。 |
| `SUB2API_KEYPOUCH_OAUTH_CLIENT_ID` | 部署方有权用于此集成、支持授权码和 S256 的 OpenAI public client。 |
| `SUB2API_KEYPOUCH_OAUTH_REDIRECT_URI` | 提供方为同一个 client 登记的 Ant 回调完整地址，必须与 Ant 的接收器完全一致。 |
| `oidc_connect` | 专用 client 与 redirect 环境变量均未设置时，从已有启用的部署方 OIDC 注册读取。系统设置优先于配置文件或环境变量。 |

专用环境变量必须成对配置。已有 OIDC 注册仅在 issuer、授权及兑换端点与 OpenAI 官方发现结果一致、成功启用 PKCE、公有客户端鉴权方法为 `none`，并允许 `openid profile email offline_access` 时可用。已有注册内的回调必须已登记给这个客户端并符合 Ant 接收器的格式。使用部署方在提供方实际登记的端口和地址，IPv4 形如 `http://127.0.0.1:<port>/keypouch/oauth/callback`，IPv6 形如 `http://[::1]:<port>/keypouch/oauth/callback`。专用回调没有 query、片段或 userinfo。Codex 原生应用的 client ID 和默认回调不会用于此集成。服务固定授权及兑换端点，并在两阶段逐字传递同一回调。提供方有最终的回调注册记录。[OpenAI OIDC 发现](https://auth.openai.com/.well-known/openid-configuration)

未提供内部密钥时本地操作返回 `503 import_not_configured`。没有合格的注册时已有账号仍可追加分组，缺失账号返回 `503 oauth_not_configured`。取得注册、访问这些上游账号与路由的授权、真实用户在 Ant 当前实例中确认授权，都由部署方负责。回调由 Ant 的专用 loopback 接收器接收。

## 本机调用

短 DTO 使用 JSON。两个端点都要求 `Authorization: Bearer <shared-secret>` 和 `Content-Type: application/json`。KeyPouch 必须直连本机 sub2api，例如 `http://127.0.0.1:8080` 并保留本机 Host。请求含浏览器来源、转发代理头或公共 Host 时返回 404。反向代理应将 `/api/v1/internal/keypouch/` 排除在公共路由外。

| POST 路径 | 请求字段 | 成功响应 |
| --- | --- | --- |
| `/api/v1/internal/keypouch/imports/{job_id}/start` | `email` | `{"status":"member","account_id":1,"group_id":2}` 或 `{"status":"authorization_required","authorization_url":"…"}` |
| `/api/v1/internal/keypouch/imports/{job_id}/complete` | `email`、`code`、`state` | `{"status":"member","account_id":1,"group_id":2}` |

任务 ID 为 1 至 96 个 ASCII 字母、数字、下划线或连字符，第一个字符为字母或数字。一项任务绑定一个规范化邮箱。多个存量匹配或邮箱、用户、工作空间字段冲突时返回 409。旧记录仅带 JWT 时，服务仅比较其 payload 的身份字段以防重复。只有这些推断字段命中时返回身份冲突，供管理员补全资料。缺失账号只有在提供方兑换成功，且 ID token 的签名、issuer、audience、nonce、有效期、已验证邮箱与该任务一致，能确定工作空间和用户后才会创建。服务端通过已有账号服务写入凭据，并在任务事务内核实成员关系。

在 30 分钟内重复 `start` 返回原授权地址。兑换期间返回 `409 import_in_progress` 和两秒的 `Retry-After`，继续用同任务 `start` 查询即可。失败、过期或兑换进程中断后，`start` 在租约结束后生成新的授权请求。旧授权码不能再次消费。完成后的重试只返回账号及分组 ID。错误响应仅含固定短码，例如 `{"error":"identity_mismatch"}`。授权地址、回调参数、令牌和内部密钥不得进入业务日志或用户档案。

数据库增量迁移为 `273_keypouch_import_jobs.sql`。任务中待授权的 state、verifier、nonce 和注册信息使用现有 `SecretEncryptor` 的 AES-GCM 密钥封装，兑换开始时清除。账号凭据只进入既有账号存储。事务内追加目标组并写调度 outbox。回滚至上一正式版本时可保留该新增表。[仓库发布要求](../AGENTS.md)

## 独立验证

不访问生产数据的事务回归使用 PostgreSQL 18 的一次性 Unix socket 集群。预先建立专用控制库 `keypouch_import_test`，测试会创建并在结束时删除独立的临时数据库，执行完整迁移。这组检查覆盖组内回读、福利等其他分组与禁用状态保留、真实签名模拟兑换、错误身份、同邮箱并发、重试、断线和过期。运行位置为 `backend/`。

```bash
SUB2API_TEST_KEYPOUCH_SOCKET=/path/to/test/socket \
SUB2API_TEST_KEYPOUCH_PORT=54539 \
go test -count=1 ./internal/handler -run '^TestKeyPouch' -v
```
