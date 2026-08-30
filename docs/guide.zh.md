## 快速开始

第一次使用本站时，按下面的顺序操作即可：

1. 打开[注册页面](/register)创建账户，然后登录。
2. 进入[购买兑换码](/redeem-store)，按购买页面提示取得兑换码。
3. 打开[兑换余额](/redeem)，粘贴兑换码并确认余额已经增加。
4. 进入[API Key 管理](/keys)创建密钥，选择需要使用的分组并设置合理的额度。
5. 在“使用密钥”中复制适合自己客户端的配置，或参考本文的 Codex 配置。
6. 发起一次模型列表或 Responses 请求，然后到[用量记录](/usage)核对调用结果和费用。

> 账户、余额、API Key 和用量数据在本站三个稳定入口之间共享。切换入口不会创建新账户，也不需要重复充值。

## 注册与登录

### 创建账户

1. 在[注册页面](/register)填写页面要求的信息。
2. 如果页面要求邮箱验证，请完成验证后再登录。
3. 登录成功后进入控制台，先确认页面右上角显示的是自己的账户。

如果注册入口暂时不可用，可能是站点关闭了新用户注册或需要邀请码。请以注册页当时显示的信息为准，不要反复提交相同表单。

### 保护账户

- 使用唯一且足够强的密码，不要和其他网站共用。
- 不要把登录凭据、验证码或完整 API Key 发到群聊、工单截图和公开仓库。
- 在共享电脑上使用后及时退出登录，并清理终端中的临时环境变量。
- 发现不明调用时，先撤销相关 API Key，再联系站点管理员核查。

## 充值：购买并兑换余额

本站余额充值统一按“购买兑换码，再兑换余额”的流程操作。

### 第一步：购买兑换码

1. 登录本站后进入[购买兑换码](/redeem-store)。
2. 选择符合需要的兑换码商品，并按购买页面提示完成订单。
3. 保存订单号、购买时间和交付的兑换码。不要在公开场所展示完整兑换码。

购买页面由外部商店提供，具体商品、价格、交付方式和界面可能变化。本站教程不承诺固定价格或固定到账时间。

### 第二步：兑换余额

1. 返回本站并打开[兑换余额](/redeem)。
2. 完整粘贴兑换码，检查前后没有多余空格。
3. 提交后等待页面显示成功结果。
4. 返回控制台或个人资料页，确认总余额和可用余额已经更新。

每个兑换码通常只能成功使用一次。不要连续重复提交已经成功的兑换码。

### 兑换失败怎么办

- **提示格式错误**：重新复制兑换码，去掉换行和首尾空格。
- **提示无效或已使用**：核对购买账户、订单和兑换记录，不要继续高频重试。
- **页面成功但余额未刷新**：刷新控制台或重新登录后再次查看。
- **仍无法解决**：通过私密渠道联系站点管理员，提供订单号、购买时间、错误提示和脱敏截图。不要公开发送密码、完整 API Key 或完整兑换码。

## 创建和保护 API Key

### 创建密钥

1. 打开[API Key 管理](/keys)，选择“创建 API Key”。
2. 使用能说明用途的名称，例如 `codex-windows` 或 `personal-cli`。
3. 选择需要的服务分组。不同分组的模型、倍率和可用性可能不同，应以创建页面和模型广场为准。
4. 根据使用场景设置额度或有效期，避免给测试密钥无限额度。
5. 创建后立即把密钥保存到安全的密码管理器或系统密钥存储中。

### 日常安全规则

- 每个设备或应用使用独立 API Key，出现问题时可以单独撤销。
- API Key 只能放在环境变量或受保护的本地配置中，不要写进 Git 仓库。
- 不要把一个人的 API Key 分享给其他人，也不要转售账户或密钥。
- 定期查看用量；发现未知模型、异常时间或费用突增时立即撤销密钥。
- 不再使用的密钥应直接删除或禁用，不要只修改名称。

## 完成第一次 API 调用

下面使用 `key66.cc.cd` 演示。也可以换成本文列出的另外两个稳定入口；API 路径必须保留 `/v1`。

### macOS、Linux 或 WSL

```bash
export SUB2API_API_KEY="<你的 API Key>"
curl https://key66.cc.cd/v1/models \
  -H "Authorization: Bearer $SUB2API_API_KEY"
```

### Windows PowerShell

```powershell
$env:SUB2API_API_KEY = "<你的 API Key>"
curl.exe https://key66.cc.cd/v1/models `
  -H "Authorization: Bearer $env:SUB2API_API_KEY"
```

成功时会返回模型列表。常见结果如下：

- HTTP `200`：认证和基础地址正常。
- HTTP `401`：API Key 缺失、拼写错误、已禁用或已过期。
- HTTP `403`：当前账户、分组或模型没有访问权限。
- HTTP `404`：通常是基础地址遗漏了 `/v1`，或客户端使用了错误接口。
- HTTP `429`：请求频率、并发或额度受到限制。

## 三个可用域名

| 入口 | API Base URL | 说明 |
| --- | --- | --- |
| `mofa.love.gd` | `https://mofa.love.gd/v1` | 稳定入口之一 |
| `mofayaoshipu.cc.cd` | `https://mofayaoshipu.cc.cd/v1` | 稳定入口之一 |
| `key66.cc.cd` | `https://key66.cc.cd/v1` | 稳定入口之一 |

三个入口连接同一个本站服务，共享账户、余额、API Key 和用量记录。选择当前网络下连接稳定的入口即可。

- 网页教程始终使用相对地址 `/guide`，不会自动跳转到其他域名。
- 切换 API 域名时只更改 Base URL，不需要重新创建 API Key。
- 某个入口暂时不可达时，可以手工切换，或使用本文的 Windows 自动测速脚本。

## 在 Codex 中使用本站

优先在[API Key 管理](/keys)中打开对应密钥的“使用密钥”窗口，复制 Codex 配置。手工配置时，编辑用户目录中的 `.codex/config.toml`：

```toml
model_provider = "sub2api"
model = "gpt-5.5"

[model_providers.sub2api]
name = "Sub2API"
base_url = "https://key66.cc.cd/v1"
env_key = "SUB2API_API_KEY"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = false
```

然后在启动 Codex 的同一个终端设置环境变量：

```powershell
$env:SUB2API_API_KEY = "<你的 API Key>"
codex
```

需要注意：

- `env_key` 填的是环境变量名称，不是 API Key 本身。
- `base_url` 必须包含 `/v1`。
- 不要同时在配置中保存明文密钥。
- 自动测速脚本不会创建或切换 `model_provider`，也不会管理 API Key。运行脚本前必须已经有可识别的当前 provider 和 `base_url`。
- Codex 配置字段可能随版本变化；遇到差异时以 [Codex 配置参考](https://learn.chatgpt.com/docs/config-file/config-reference)为准。

## 使用自动测速脚本

脚本下载地址：[select-fastest-codex-base-url.bat](/downloads/select-fastest-codex-base-url.bat)

SHA-256：`4bef37d6600d0d77101794cb9f0059554e60f751dc7e5b2cde3e9dc67eb9a43b`

### 运行前检查

1. 关闭所有正在运行的 Codex 窗口，避免配置在脚本运行期间被其他进程改写。
2. 下载 BAT 和对应的 `.sha256` 文件到同一个目录。
3. 在 PowerShell 中计算哈希：

```powershell
(Get-FileHash .\select-fastest-codex-base-url.bat -Algorithm SHA256).Hash.ToLower()
Get-Content .\select-fastest-codex-base-url.bat.sha256
```

两个结果必须一致。不一致时不要运行，重新下载并再次校验。

### 脚本会做什么

1. 请求管理员权限，并使用单实例锁防止两个脚本同时改文件。
2. 预检 `%USERPROFILE%\.codex\config.toml`，读取顶层 `model_provider`，定位对应的 `[model_providers.<名称>]` 和唯一 `base_url`。
3. 为 Codex 配置与 Windows `hosts` 分别建立不会覆盖旧文件的时间戳备份。
4. 把 `mofa.love.gd` 固定到 `15.204.82.11`，刷新 DNS 缓存。
5. 测试以下四个候选：
   - `https://mofa.love.gd`
   - `https://key66.vip`
   - `https://mofayaoshipu.cc.cd`
   - `https://key66.cc.cd`
6. 每个候选先请求一次 `/health` 预热，再正式请求三次。只有三次都返回 HTTP 200 且内容为 `{"status":"ok"}` 才参与排序。
7. 按三次耗时的中位数选择最快域名，只替换当前 provider 的 `base_url` 主机名。
8. 重新读取并验证配置；任何步骤失败都会尝试恢复本次改动。

`key66.vip` 只是按要求保留的测速候选，不作为当前稳定入口承诺。它失败时会显示不可用，不影响其他候选继续测速。

### 脚本不会做什么

- 不创建、删除或切换 `model_provider`。
- 不读取、写入或询问 API Key。
- 不修改 `wire_api`、`requires_openai_auth`、模型名或其他 provider。
- 不删除旧备份。
- 成功后不会自动撤销 `mofa.love.gd -> 15.204.82.11` 的 `hosts` 映射。

### 查看结果

脚本结束时会显示四个候选的正式测速结果、选中的域名、配置路径、`hosts` 路径和两个备份文件的位置。全部候选失败时，Codex 配置不会改变，并会恢复本次写入的 `hosts`。

### 手工恢复

如果脚本成功后需要撤销修改：

1. 从脚本输出中找到本次 `config.toml.codex-speed.<时间戳>.bak` 和 `hosts.codex-speed.<时间戳>.bak`。
2. 关闭 Codex。
3. 使用管理员 PowerShell 把两个备份分别复制回原路径。
4. 运行 `ipconfig /flushdns`。
5. 重新打开 Codex，并检查 `config.toml` 中原 provider 的 `base_url`。

备份可能包含原 Codex 配置信息，应保存在本机受保护目录，不要上传或发送给其他人。

## 使用 goal-workflow skill

`goal-workflow` 会把一句简略需求整理成经过确认、可保存并可启动的 Codex Goal。它会先访谈和展示完整 Goal，不会在确认前直接执行任务。

### 安装

在 Codex 中输入下面这句话：

<!-- copy-command:skill-install -->
```text
安装 skill https://github.com/zuchengchen/goal-workflow/tree/master/skills/goal-workflow
```

必须使用包含 `tree/master/skills/goal-workflow` 的完整地址。仓库根地址不是可靠的 skill 安装目标。

安装完成后：

1. 确认 Codex 的 skills 目录中存在 `goal-workflow/SKILL.md`。
2. 完全退出并重新启动 Codex，让新 skill 被重新发现。
3. 输入 `$goal-workflow`，确认 Codex 能识别它。

### 安全更新

不要直接覆盖正在使用的 skill。让 Codex 先备份旧目录，再重新安装并验证：

<!-- copy-command:skill-update -->
```text
安全更新 skill https://github.com/zuchengchen/goal-workflow/tree/master/skills/goal-workflow：先备份现有 goal-workflow，再重新安装并验证；失败时恢复备份
```

更新后重新启动 Codex。如果安装器提示同名目录已经存在，应先确认备份有效，再让安装器完成替换；不要用未验证的新版本删除唯一可用副本。

### 启用 Goal 功能

如果 Codex 明确提示 Goal mode 不可用，请先查看当前版本文档。需要配置开关的版本可在 `.codex/config.toml` 的现有 `[features]` 表中加入：

```toml
[features]
goals = true
```

不要重复创建多个 `[features]` 表。修改后重启 Codex，再使用 `/goal` 帮助确认功能状态。

### 使用示例

<!-- copy-command:goal-example -->
```text
$goal-workflow 清理内存
```

随后按一次一个问题的方式回答。你会先看到范围、风险和验证方案，再看到完整 Goal 文件内容。保存和启动是两次独立确认；只有第二次确认后才会开始执行。

更多说明见 [goal-workflow 安装文档](https://github.com/zuchengchen/goal-workflow/blob/master/INSTALL.md)与 [Codex Skills 文档](https://developers.openai.com/codex/skills)。

## SVIP 权利与义务

### 获得条件

- 总余额包含赠送余额，必须**严格大于 100 元**才会自动升级。
- 总余额恰好为 100 元时不会升级。
- 升级成功后永久有效，当前规则没有自动降级路径。
- 系统逻辑冻结 100 元作为 SVIP 准备金，因此可用余额按总余额扣除准备金后计算。

### 权利

| 权利 | 当前规则 |
| --- | --- |
| 专属模型 | 可使用 `gpt-5.6-luna` 及其同家族版本化名称 |
| 分组优惠 | 使用 `gpt-pro` 分组时，基础倍率减 0.05，最低不小于 0 |
| 资格期限 | 达标升级后永久保留 |
| 站内策略 | 享受站内标注的 SVIP 服务与风控策略，具体结果仍以系统执行为准 |

SVIP 不代表所有模型永远可用，也不保证任何请求一定成功。模型上游状态、账户余额、分组可用性和站点维护仍会影响调用。

### 义务

- 妥善保护账户、密码和 API Key，不得共享、出租或转售。
- 不得使用本站实施滥用、攻击、欺诈或其他违法违规行为。
- 遵守适用法律、站点规则和模型服务要求。
- 保持足够可用余额；冻结的 100 元准备金不能用于日常调用。
- 对自己账户发起的请求和产生的费用负责。

违反上述义务时，站点可以限制模型、API Key、账户或相关服务。具体处置以实际违规情况和站点规则为准。

## 查看余额、用量与请求记录

- **余额**：登录后可在控制台和页面右上角查看可用余额。SVIP 用户还会看到冻结准备金。
- **API Key 用量**：进入[API Key 用量查询](/key-usage)，输入自己的密钥查看额度与汇总。不要在不可信设备上查询。
- **账户用量**：进入[用量记录](/usage)，按日期、模型和密钥核对请求数、Token 与费用。
- **模型与分组**：进入[模型广场](/model-plaza)查看当前公开模型、分组和展示倍率；最终计费以实际请求记录为准。

建议在首次配置、切换域名、修改模型或运行测速脚本后进行一次小请求，并马上核对用量，避免长期使用错误配置。

## 常见问题

### 返回 401

检查环境变量是否在启动客户端的同一个终端中设置，确认 API Key 没有多余空格、没有被撤销，并确认请求头是 `Authorization: Bearer <API Key>`。

### 提示余额不足

先查看可用余额而不是只看总余额。SVIP 的 100 元准备金属于逻辑冻结金额，不能用于普通调用。需要余额时请购买新的兑换码并兑换。

### Codex 找不到 provider

确认顶层 `model_provider` 与 `[model_providers.<名称>]` 的名称完全一致，`base_url` 包含 `/v1`，并在修改配置后重新启动 Codex。

### 某个域名打不开

换用本文列出的另外一个稳定入口。若运行过测速脚本，还应检查 Windows `hosts` 中的 `mofa.love.gd` 映射是否仍然适用。

### goal-workflow 安装后无法识别

检查安装目标是否为完整 skill 子目录、`SKILL.md` 是否存在，然后完全重启 Codex。仍然失败时查看 Codex Skills 文档，不要反复安装到多个不同目录。

### 测速脚本全部失败

确认系统时间、网络、TLS 和 DNS 正常，并在浏览器中分别打开候选域名的 `/health`。全部失败时脚本不会修改 Codex 配置，并会尝试恢复本次 `hosts` 变更。

### 脚本提示配置结构不支持

脚本只处理能唯一识别的顶层 `model_provider`、对应 provider 表和现有 `base_url`。不要为了让脚本通过而删除其他配置；先使用“使用密钥”生成规范配置，再重新运行。

## 安全建议

- 所有示例中的 `<你的 API Key>` 都是占位符，不要把真实值写进教程、截图或 Git。
- 下载 BAT 后先核对 SHA-256，再允许运行管理员权限。
- 不要关闭 PowerShell 的 TLS 证书验证，也不要使用来源不明的镜像脚本。
- 配置和 `hosts` 备份只保存在本机；确认新配置稳定后再按自己的保留策略清理旧备份。
- 联系管理员时只提供必要的脱敏信息。密码、完整 API Key、Cookie 和验证码永远不应发送。

## 获取帮助

遇到无法自行解决的问题时，使用站点当前提供的联系渠道。建议准备以下信息：

- 问题发生时间和所用域名。
- 页面或客户端显示的完整错误类型，但先移除密钥、Cookie 和个人信息。
- 使用的模型、分组和客户端版本。
- 兑换问题对应的订单号与购买时间；不要在公开消息中发送完整兑换码。
- 测速脚本输出的候选状态和备份文件名；不要发送备份文件内容。

## 版本信息

- 教程版本：`1.0`
- 更新日期：`2026-08-30`
- 适用范围：本站当前 Sub2API 用户功能与 Codex 配置
- Windows 测速脚本：`select-fastest-codex-base-url.bat`
- 脚本 SHA-256：`4bef37d6600d0d77101794cb9f0059554e60f751dc7e5b2cde3e9dc67eb9a43b`

教程会随站点功能调整。页面内容与实际界面不一致时，以当前界面、用量记录和站点管理员确认的信息为准。
