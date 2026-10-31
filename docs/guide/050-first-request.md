## 检查 API Key 能不能用

下面的命令请求模型列表，用于检查服务地址、Key 验证和当前列表访问条件。**返回模型列表不等于已经完成实际模型调用，也不保证列表中的每个模型此刻都能成功使用。**

以 `key66.cc.cd` 为例，把 `<你的 API Key>` 替换为自己的 Key，尖括号也要一起删掉。不要把带真实 Key 的命令或配置发给别人。

### macOS、Linux 或 WSL

```bash
export SUB2API_API_KEY="<你的 API Key>"
curl --silent --show-error --include https://key66.cc.cd/v1/models \
  -H "Authorization: Bearer $SUB2API_API_KEY"
```

### Windows PowerShell

```powershell
$env:SUB2API_API_KEY = "<你的 API Key>"
curl.exe --silent --show-error --include https://key66.cc.cd/v1/models `
  -H "Authorization: Bearer $env:SUB2API_API_KEY"
```

`--include` 会显示 HTTP 状态。请同时检查响应正文：

- `200` 且正文为模型列表：这次列表请求成功。
- `401`：通常是 Key 缺失、无效或被停用。
- `403`：可能是 Key 过期、余额不足、用户/IP/订阅等条件不允许；以具体提示为准。
- `404`：检查服务地址和请求路径，本例的完整路径是 `/v1/models`。
- `429`：可能是额度耗尽或请求过于频繁，不能只靠一直重试解决。

模型列表请求也可能检查余额、有效期或额度。完整处理方式见[错误码含义与处理方案](#error-codes)。需要验证实际模型调用时，选择这个 Key 可用的模型发送一个短问题，再查看[用量记录](/usage)；实际调用可能产生费用。
