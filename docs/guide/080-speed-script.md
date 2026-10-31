## 自动选择速度最快的网址

这个工具只适用于 **Windows 默认用户配置**。它比较本站入口的 `/health` 健康检查延迟，选择本次合格结果中延迟最低的域名。**它不测试模型生成速度、Token 吞吐或实际模型调用是否成功。**

下载：[自动测速 BAT](/downloads/select-fastest-codex-base-url.bat) · [SHA-256 校验文件](/downloads/select-fastest-codex-base-url.bat.sha256)

SHA-256（可以理解为文件的“指纹”）：`6cd9e48586f8b766cf3549d420b293bc0da7c875c3c50ac5b03ef2a0e6f7d4f4`

### 运行前先确认

1. 已成功配置过 Codex，且希望修改的是运行用户的 `%USERPROFILE%\.codex\config.toml`。
2. 文件有唯一的顶层 `model_provider`，对应服务配置表中有唯一的 `base_url`。
3. 没有依赖 `CODEX_HOME`、WSL 中的配置、独立 profile 或命令行参数来选择另一份生效配置。脚本不会识别这些覆盖，可能改到没有生效的基础配置。
4. 关闭 Codex 和可能修改配置的编辑器，再下载 BAT 与校验文件，放在同一文件夹。
5. 核对指纹后，右键 BAT，选择“以管理员身份运行”，并确认仍使用自己的 Windows 用户。

工具需要管理员权限，因为会修改 Windows 的 `hosts` 文件。可以把 hosts 理解成电脑自己的“网址通讯录”。如果实际配置不在上述路径，或脚本提示 TOML 格式不支持，请手工核对配置，不要删除已有设置来迁就脚本。

### 建议先核对文件指纹

在下载文件所在文件夹打开 PowerShell：

```powershell
(Get-FileHash .\select-fastest-codex-base-url.bat -Algorithm SHA256).Hash.ToLower()
Get-Content .\select-fastest-codex-base-url.bat.sha256
```

第一行输出的哈希应等于校验文件开头的哈希，也应与本节一致。不一致时不要运行。脚本不会自动读取这个校验文件，上述检查需要自己完成。

### 工具会做什么

1. 读取并备份完整的 Codex 配置和 Windows hosts 文件。
2. 写入或更新 `15.204.82.11 mofa.love.gd` 的 hosts 记录并刷新 DNS；成功后这条记录保留，即使最终选中了其他域名。
3. 测试下面五个网址：
   - `https://mofa.love.gd`
   - `https://key66.vip`
   - `https://mofayaoshipu.cc.cd`
   - `https://key66.cc.cd`
   - `https://mofa.duoduohuiying.cn`
4. 每个域名先预热一次，再正式测三次；正式三次均成功才参与选择，按三次耗时的中位数排序。
5. 只会更换目标 Codex 配置中 `base_url` 的网址域名，保留其协议、端口、路径和其他部分；不会同步修改单独的 `model_catalog_url`。
6. 检查写入后的文件和目标域名；这一步不会启动 Codex 或发送模型请求。

### 配置和密钥的处理

工具不要求你输入 Key，也不使用 Key 做健康检查，但它会读取并备份**整个配置文件**。若原配置含有 Key，备份也会含有 Key，必须像原配置一样保管。

工具不会创建或切换 provider，不会修改模型名称或主动删除旧备份。它支持的是有限的简单 TOML 结构，复杂转义、多行字符串或其他配置形式可能被拒绝。

### 怎么看运行结果

输出会列出每个域名的结果、最终选择和两个备份位置。备份位于原文件所在目录，文件名含 `.codex-speed.`、时间和 `.bak`。

如果五个域名都不合格，Codex 服务地址不会被改写，但脚本此前可能已经调整 hosts，随后会尝试恢复本次改动。**自动恢复可能失败**，尤其是文件已被其他程序修改时；看到 `Automatic rollback was incomplete` 要保留备份并核对结果。

### 想恢复原来的设置

1. 找到本次输出对应的配置和 hosts 两个备份，关闭 Codex。
2. 确认运行后没有需要保留的新修改；有疑问时先联系管理员，不要直接覆盖。
3. 使用管理员 PowerShell 将本次备份复制回对应原路径。
4. 执行 `ipconfig /flushdns`，再重新打开 Codex，核对实际生效的入口。

不要发送备份正文、完整配置或认证文件给别人；排查时只提供脱敏后的错误、文件名和操作时间。
