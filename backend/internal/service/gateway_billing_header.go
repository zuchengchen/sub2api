package service

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ccVersionInBillingRe matches the semver part of cc_version (X.Y.Z).
var ccVersionInBillingRe = regexp.MustCompile(`cc_version=\d+\.\d+\.\d+`)

var ccVersionWithFingerprintInBillingRe = regexp.MustCompile(`cc_version=\d+\.\d+\.\d+\.[0-9a-fA-F]{3}\b`)

const claudeBillingSourceTextKey = "claude_billing_source_text"

// captureClaudeBillingSource 在 system 注入/改写之前保存指纹采样的源文本，
// 供后续 billing version sync 复用。不要从文本前缀推断来源。
func captureClaudeBillingSource(c *gin.Context, body []byte) {
	if c != nil {
		c.Set(claudeBillingSourceTextKey, extractFirstUserText(body))
	}
}

// syncBillingHeaderVersion rewrites cc_version in x-anthropic-billing-header
// system text blocks to match the version extracted from userAgent.
// Recompute any recognized fingerprint suffix because its input includes the version.
// Only touches system array blocks whose text starts with "x-anthropic-billing-header".

// effectiveRequestBillingUserAgent 选择写进 x-anthropic-billing-header 的
// User-Agent 版本来源。真实 Claude Code 请求用 OAuth 账号指纹 UA（上游账号
// 而非客户端订阅持有出站 billing attribution）；mimicry 用代理兼容 UA。
// 调用方必须在同一次请求内只取一次 mimic UA 并复用，保证 cc_version 与出站
// User-Agent 头严格一致。
func effectiveRequestBillingUserAgent(mimicUserAgent, clientUserAgent, tokenType string, mimicClaudeCode bool, fingerprint *Fingerprint) string {
	if tokenType != "oauth" {
		return ""
	}
	if mimicClaudeCode {
		return mimicUserAgent
	}
	if fingerprint != nil && ExtractCLIVersion(fingerprint.UserAgent) != "" {
		return fingerprint.UserAgent
	}
	if ExtractCLIVersion(clientUserAgent) != "" {
		return clientUserAgent
	}
	return ""
}

func syncBillingHeaderVersion(body []byte, userAgent string, c *gin.Context) []byte {
	version := ExtractCLIVersion(userAgent)
	if version == "" {
		return body
	}

	systemResult := gjson.GetBytes(body, "system")
	if !systemResult.Exists() || !systemResult.IsArray() {
		return body
	}

	// 指纹采样以 system 注入前保存的源文本为准；缺失时退回当前 body。
	firstText := extractFirstUserText(body)
	if c != nil {
		if saved, ok := c.Get(claudeBillingSourceTextKey); ok {
			if savedText, ok := saved.(string); ok {
				firstText = savedText
			}
		}
	}
	fingerprint := computeClaudeCodeFingerprintFromText(firstText, version)
	replacement := "cc_version=" + version
	idx := 0
	systemResult.ForEach(func(_, item gjson.Result) bool {
		text := item.Get("text")
		if text.Exists() && text.Type == gjson.String &&
			strings.HasPrefix(text.String(), "x-anthropic-billing-header") {
			fingerprintedReplacement := replacement + "." + fingerprint
			newText := ccVersionWithFingerprintInBillingRe.ReplaceAllString(text.String(), fingerprintedReplacement)
			newText = ccVersionInBillingRe.ReplaceAllString(newText, replacement)
			if newText != text.String() {
				if updated, err := sjson.SetBytes(body, fmt.Sprintf("system.%d.text", idx), newText); err == nil {
					body = updated
				}
			}
		}
		idx++
		return true
	})

	return body
}

// billingSourceText 返回 billing 指纹采样文本：优先取 system 注入前保存的源文本。
func billingSourceText(body []byte, c *gin.Context) string {
	if c != nil {
		if saved, ok := c.Get(claudeBillingSourceTextKey); ok {
			if savedText, ok := saved.(string); ok {
				return savedText
			}
		}
	}
	return extractFirstUserText(body)
}

// ensureClaudeCodeBillingBlock supplies the upstream-account attribution when
// a real Claude Code client reached the gateway without its own block.
func ensureClaudeCodeBillingBlock(body []byte, userAgent string, c *gin.Context) []byte {
	version := ExtractCLIVersion(userAgent)
	if version == "" {
		return body
	}
	system := gjson.GetBytes(body, "system")
	if system.IsArray() {
		for _, item := range system.Array() {
			if strings.HasPrefix(item.Get("text").String(), "x-anthropic-billing-header") {
				return syncBillingHeaderVersion(body, userAgent, c)
			}
		}
	}
	text := fmt.Sprintf("x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=cli; cch=00000;", version, computeClaudeCodeFingerprintFromText(billingSourceText(body, c), version))
	billingBlock, err := marshalAnthropicSystemTextBlockWithCacheControl(text, nil)
	if err != nil {
		return body
	}
	items := [][]byte{billingBlock}
	if system.IsArray() {
		for _, item := range system.Array() {
			items = append(items, []byte(item.Raw))
		}
	} else if system.Exists() && system.Type == gjson.String {
		if original, err := marshalAnthropicSystemTextBlockWithCacheControl(system.String(), nil); err == nil {
			items = append(items, original)
		}
	}
	updated, ok := setJSONRawBytes(body, "system", buildJSONArrayRaw(items))
	if !ok {
		return body
	}
	return updated
}
