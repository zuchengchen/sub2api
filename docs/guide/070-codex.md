## 在 Codex 中使用本站

### 推荐方法：直接复制配置

1. 打开[API Key 管理](/keys)。
2. 找到刚创建的 API Key。
3. 点击“使用密钥”。
4. 找到 Codex 配置并复制。
5. 按页面提示放到 Codex 配置文件中。
6. 关闭并重新打开 Codex。

这种方法最省事，也最不容易填错。下面的手工方法只在无法直接复制时使用。

### 手工填写配置

Codex 的配置文件名是 `.codex/config.toml`：

- Windows 通常在 `C:\Users\你的 Windows 用户名\.codex\config.toml`。
- macOS 和 Linux 通常在自己的用户文件夹下，即 `~/.codex/config.toml`。

找到文件后，把下面内容放进去：

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

不用逐个理解所有英文，重点检查：

- `base_url` 是本站服务地址，结尾必须有 `/v1`。
- `env_key` 是存放 API Key 的名称，不是 API Key 本身。
- `model` 是要使用的模型名称。
- `model_provider` 是这组配置的名称。测速工具不会修改或切换它。

保存配置后，在准备启动 Codex 的 PowerShell 窗口输入：

```powershell
$env:SUB2API_API_KEY = "<你的 API Key>"
codex
```

第一行把 API Key 临时交给 Codex，第二行启动 Codex。关闭这个 PowerShell 窗口后，临时填写的 Key 会消失，不会被写进教程或代码。
