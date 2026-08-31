## 使用 goal-workflow 小助手

`goal-workflow` 是 Codex 的一个小助手（Codex 中称为 `skill`）。你只要说出一个大概需求，它会一次问一个问题，帮你把任务范围、风险和验收方法整理清楚。

### 安装

在 Codex 中完整输入下面这句话，也可以点击页面上的复制按钮：

<!-- copy-command:skill-install -->
```text
安装 skill https://github.com/zuchengchen/goal-workflow/tree/master/skills/goal-workflow
```

地址比较长，不要删掉后面的 `tree/master/skills/goal-workflow`。安装完成后：

1. 完全退出 Codex。
2. 重新打开 Codex。
3. 输入 `$goal-workflow`，检查 Codex 能否识别。

### 更新

更新前先备份旧版本。直接在 Codex 中输入：

<!-- copy-command:skill-update -->
```text
安全更新 skill https://github.com/zuchengchen/goal-workflow/tree/master/skills/goal-workflow：先备份现有 goal-workflow，再重新安装并验证；失败时恢复备份
```

更新完成后，退出并重新打开 Codex。这样做的好处是：如果新版本安装失败，还能恢复旧版本。

### 如果提示 Goal 功能未开启

大多数用户不需要手工修改。只有 Codex 明确提示 Goal 功能不可用时，才检查 `.codex/config.toml` 中是否有下面内容：

```toml
[features]
goals = true
```

如果文件中已经有 `[features]`，只需在它下面增加 `goals = true`，不要再写第二个 `[features]`。保存后重启 Codex。

### 使用例子

<!-- copy-command:goal-example -->
```text
$goal-workflow 清理内存
```

接下来只要一次回答一个问题。它会先让你确认完整任务，再询问是否保存和开始执行。没有得到最后确认前，它不会直接开始修改。
