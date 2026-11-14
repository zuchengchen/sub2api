# 大请求上传空闲超时

Responses 和 Messages 的长会话、图片请求可能在上传过程中暂停超过 60 秒。
`sub2api-client-body-timeout.conf` 将本站的请求体读取空闲超时设置为 180 秒。
它限制两次读取之间的间隔，不限制请求的总上传时间。继续使用
`proxy_request_buffering off`、原有请求体大小限制和请求头超时。

在 Sub2API 的 `server` 块中包含：

```nginx
include /etc/nginx/snippets/sub2api-client-body-timeout.conf;
```

安装时先备份原文件，再将本目录的同名文件复制到该路径，执行 `nginx -t`。
配置验证成功后以 `systemctl reload nginx` 平滑重载。此文件仅供 Sub2API
站点包含；无需调整 nginx 的全局超时。回滚时恢复备份、再次验证并重载。

可运行隔离的真实 nginx 测试，监听随机的本机端口，使用合成数据和本地接收端：

```bash
python3 deploy/tests/nginx-upload-idle-test.py
```

测试约需 65 秒，同时验证旧 60 秒配置记录 408、新配置可完成同样暂停的上传，
收到的数据字节数和摘要一致，并确认超过 256 MiB 的请求仍返回 413。
该调整可以容忍较长的上传停顿；客户端主动取消或连接实际断开仍按中断处理。
