package service

import (
	"net/http"
	"strings"
)

// isUpstreamContentPolicy403 识别上游以 403 返回的内容审核拦截，例如经 CLIProxyAPI
// 转发的 Devin "Your request was blocked by our content policy"。
// 这是请求级拒绝而非账号失效：换号重放只会被同样拦截，按 403 处罚账号会把整组账号打下线，
// 客户端（Claude Code）还会把 403 误判为认证失败并提示重新登录。
func isUpstreamContentPolicy403(statusCode int, responseBody []byte) bool {
	if statusCode != http.StatusForbidden || len(responseBody) == 0 {
		return false
	}
	msg := strings.ToLower(extractUpstreamErrorMessage(responseBody))
	if msg == "" {
		msg = strings.ToLower(string(responseBody))
	}
	return strings.Contains(msg, "content policy") && !grokAccountAccessMessage(msg)
}
