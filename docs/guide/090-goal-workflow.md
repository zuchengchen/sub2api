## 使用 goal-workflow 小助手

`goal-workflow` 是 Codex 的一个小助手（称为 `skill`）。它先读取任务上下文，通过简短问题整理目标、范围、风险与验收方法，再让你决定是否保存并执行。

### 安装

在 **Codex 对话中**输入下面的请求；它不是 PowerShell 命令。也可以点击页面的复制按钮：

<!-- copy-command:skill-install -->
```text
安装 skill https://github.com/zuchengchen/goal-workflow/tree/master/skills/goal-workflow
```

保留完整仓库路径。安装后检查 Codex 报告的安装位置和验证结果，再输入 `$goal-workflow` 确认识别成功；未识别时重新打开 Codex，并检查是否有重复安装。

### 更新

同样在 Codex 对话中输入：

<!-- copy-command:skill-update -->
```text
安全更新 skill https://github.com/zuchengchen/goal-workflow/tree/master/skills/goal-workflow：先备份现有 goal-workflow，再重新安装并验证；失败时恢复备份
```

这是一项带备份要求的更新请求，不代表 skill 自带保证成功的自动更新器。检查实际安装位置、备份位置及验证结果后再使用新版本。

### 如果提示 Goal 功能未开启

先确认当前客户端支持 Goal，可以查看其 `/goal` 帮助或[官方 Goals 说明](https://developers.openai.com/cookbook/examples/codex/using_goals_in_codex)。配置开关不能为不支持此功能的旧客户端增加功能。

对于已支持、但功能被关闭的客户端，在实际生效的用户配置中合并：

```toml
[features]
goals = true
```

已有 `[features]` 时只调整它下面的 `goals`，不要重复建表。按客户端要求重新加载配置。字段说明见 [Codex 配置参考](https://learn.chatgpt.com/docs/config-file/config-reference)。

### 使用例子

<!-- copy-command:goal-example -->
```text
$goal-workflow 清理内存
```

按它的问题说明需求。规划阶段可以只读检查环境，但不会实施目标任务。最后展示完整目标和绝对保存路径，由你一次选择：

- **保存并开始**：保存、精确回读验证后启动；不需要再重复批准开始。
- **仅保存**：保存目标文件，暂不执行。
- **修改或取消**：先调整草案或结束流程。

没有创建 Goal 的工具时，skill 可能提供 `/goal ...` 交接命令；只有确认 Goal 真正启动后才执行任务，不能把显示一条命令当作已经启动。具体行为以[维护者的 skill 文件](https://github.com/zuchengchen/goal-workflow/tree/master/skills/goal-workflow)为准。
