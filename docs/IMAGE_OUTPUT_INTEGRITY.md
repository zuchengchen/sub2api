# 图片输出的尺寸与模型信息

Images API 和 Responses 的图片结果会保留上游模型声明，并通过图片对象内的 `sub2api_image` 说明实际输出。该对象位于 Images 的 `data[]`、图片完成事件，或 Responses 的 `image_generation_call` 内。客户端应允许额外 JSON 字段。

```json
{
  "requested_model": "gpt-image-2.6-flare",
  "reported_model": "gpt-image-2-codex",
  "model_status": "different",
  "requested_size": "1024x1024",
  "upstream_size": "1254x1254",
  "output_size": "1024x1024",
  "postprocessed": true,
  "resize_mode": "contain",
  "warnings": ["upstream_image_model_differs"]
}
```

`requested_model` 是客户端选择；`reported_model` 是上游响应的声明。`model_status` 为 `reported`（存在声明且未发现差异）、`different`（声明与请求不同）或 `unreported`（没有声明）。没有声明时不再把请求模型填入响应的 `model`。上游可能使用内部别名，因此 `different` 表示无法从响应确认请求模型，不据此断言上游实际使用了另一套权重。

当客户端显式请求 `WIDTHxHEIGHT`，且返回的内嵌 PNG / JPEG 尺寸不同，服务会对已生成图片等比缩放到请求画布；比例不同时居中补边，保留完整内容。PNG 默认透明补边，`background=opaque` 或 JPEG 使用白边。`postprocessed=true` 明确表示本地处理，不能称为上游原生输出该尺寸，也不能把放大后的图片称为增加了原生细节。`size=auto` 保留原图。

缩放目标最多单边 3840、总像素 8,294,400；源图最多单边 8192、总像素 16,777,216，编码输入最多 48 MiB。超出处理范围、无法解码或缩放失败时保留原始图片，并在 `warnings` 中说明限制。GIF、WebP 与远程图片 URL 不进行本地缩放；GIF 动画帧和时间信息保持原样。图片格式和可读尺寸按文件内容标注。流式中间预览保留上游结果，尺寸修正应用于最终完成的图片。

用量记录仍使用处理前的上游尺寸及 token。读取图片头补齐缺失的上游尺寸不需要下载远程 URL。HTTP、SSE、Responses WebSocket（连接池、透传、HTTP 桥接）均按每次请求或每轮请求处理，下一轮不会继承上一轮的图片选项。

GIF preview 由上游账号和模型权限决定。`GIF output is not enabled` 保留为明确的上游拒绝，服务不会通过重试、模型改名或把静态图片本地拼接成动画来声称已生成原生 GIF。公开模型文档支持某种格式，并不代表当前 OAuth 账号或 provider 已开通。
