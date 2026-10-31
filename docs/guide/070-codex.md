## 在 Codex 中使用本站

### 推荐方法：直接复制配置

1. 打开[API Key 管理](/keys)，找到要使用的 Key，点击“使用密钥”。
2. 选择 Codex、自己的操作系统和页面提供的认证模式。
3. 先备份已有配置，再按页面说明处理**所有列出的文件和终端设置**。兼容模式可能同时提供 `config.toml` 与 `auth.json`；其他认证模式不同，不要混用。
4. 如果选择本地模型目录，按页面说明下载目录文件并放到配置指定的位置；远程目录则使用页面提供的目录地址。
5. 检查模型名属于这个 Key 当前可用的模型目录。页面生成的推荐模型随分组和目录改变，没有对所有用户通用的固定模型。
6. 按页面要求重新启动 Codex。使用 API Key Mode 时，需要完全退出并重启，再新建任务。发送一个短问题后，核对[用量记录](/usage)。

复制内容中可能包含完整 Key 或认证信息，只保存在自己的电脑上。合并配置时保留适合自己的权限设置，不需要把关闭沙箱或关闭审批作为接入本站的前提。

### 手工填写配置

下面是**从终端启动 Codex CLI**的最小接入示例。图形客户端的配置位置及环境变量读取方式应以其说明为准。

用户级配置默认位置：

- Windows：`%USERPROFILE%\.codex\config.toml`。
- macOS/Linux：`~/.codex/config.toml`。
- 设置过 `CODEX_HOME`、使用 profile 或命令行覆盖时，先确认真正生效的用户级配置；不要把凭据放进项目仓库的配置。

先备份再合并，不要覆盖整份现有配置。下面的顶层 `model_provider`、`model` 放在第一个 `[表]` 之前，已有同名项和表不要重复添加。把模型占位符替换为**当前 Key 可用的真实模型 ID**：

```toml
model_provider = "sub2api"
model = "<当前 API Key 可用的模型 ID>"

[model_providers.sub2api]
name = "Sub2API"
base_url = "https://key66.cc.cd/v1"
env_key = "SUB2API_API_KEY"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = false
```

- `model_provider` 必须与下面 `model_providers.sub2api` 的名称对应，它不是本站 API Key 的分组名称。
- 此例的 `base_url` 用于 Responses 接口，结尾包含 `/v1`；其他客户端按“使用密钥”对应页签填写。
- `env_key` 填环境变量的名称，不填 Key 本身。`SUB2API_API_KEY` 是本示例自定义名称，并非 Codex 内置必填名称。
- 此例使用 HTTP/SSE；页面生成的其他接入模式可能不同，不要只替换个别认证字段后混用。

在准备启动 Codex 的 PowerShell 窗口中：

```powershell
$env:SUB2API_API_KEY = "<你的 API Key>"
codex
```

macOS/Linux 终端中：

```bash
export SUB2API_API_KEY="<你的 API Key>"
codex
```

环境变量对当前终端及从它启动的 Codex 生效，新开终端需要重新设置。终端历史和配置备份可能留下敏感内容，不要公开分享。

配置字段与覆盖规则参见 [Codex 配置参考](https://learn.chatgpt.com/docs/config-file/config-reference)和[高级配置说明](https://learn.chatgpt.com/docs/config-file/config-advanced)。
