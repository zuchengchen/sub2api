## 检查 API Key 能不能用

如果你只打算使用 Codex，可以直接跳到下一节。这里的命令用于检查 API Key 和网址是否填写正确。

下面用 `key66.cc.cd` 举例。命令中的 `<你的 API Key>` 要换成你刚创建的 Key，尖括号也要一起删掉。

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

成功时，窗口会显示一段模型列表。失败时通常会看到一个三位数：

- `200`：连接成功。
- `401`：API Key 没填、填错、已经停用或已经过期。
- `403`：当前账户或分组不能使用这个模型。
- `404`：网址可能填错，重点检查结尾有没有 `/v1`。
- `429`：短时间使用次数太多，或者额度受到限制。稍等后再试，并检查余额和 Key 额度。

这些数字叫“状态码”，只是帮助判断问题的位置，不需要背下来。完整的对照表和处理方案见[错误码含义与处理方案](#error-codes)。
