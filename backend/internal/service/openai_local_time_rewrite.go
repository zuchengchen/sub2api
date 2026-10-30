package service

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 出站给 OpenAI 的默认本地日期/时区是美西太平洋时区。
// 决定 4 之后它只是回退值：实际请求按账号出口 IP（代理/直连）自动确定，
// 仅在拿不到出口时区时才回到这里，与引入动态时区之前的硬编码行为保持一致。
const openAIOutboundTimezoneName = DefaultOpenAIRequestTimezone

var (
	openAILocalTimeEnvContextRe     = regexp.MustCompile(`(?s)<environment_context>.*?</environment_context>`)
	openAILocalTimeCurrentDateTagRe = regexp.MustCompile(`(?s)<current_date>\s*[^<]*?</current_date>`)
	openAILocalTimeTimezoneTagRe    = regexp.MustCompile(`(?s)<timezone>\s*[^<]*?</timezone>`)
	openAILocalTimeCurrentTimeTagRe = regexp.MustCompile(`(?s)<current_time>([^<]*)</current_time>`)
	openAILocalTimeLocalDateTagRe   = regexp.MustCompile(`(?s)<local_date>\s*[^<]*?</local_date>`)
	openAILocalTimeTimeZoneTagRe    = regexp.MustCompile(`(?s)<time_zone>\s*[^<]*?</time_zone>`)
	openAILocalTimeHHMMSSRe         = regexp.MustCompile(`^\d{2}:\d{2}:\d{2}$`)

	// 仅匹配未转义的 JSON 键，避免改写用户正文里作为字符串粘贴的 JSON。
	openAILocalTimeJSONCurrentDateRe = regexp.MustCompile(`"current_date"\s*:\s*"[0-9]{4}-[0-9]{2}-[0-9]{2}"`)
	openAILocalTimeJSONCurrentTimeRe = regexp.MustCompile(`"current_time"\s*:\s*"[^"]*"`)
	openAILocalTimeJSONTimezoneRe    = regexp.MustCompile(`"timezone"\s*:\s*"(UTC|GMT|Local|[A-Za-z]+(?:[_-][A-Za-z]+)*(?:/[A-Za-z0-9_+\-]+)+)"`)
)

type openAILocalTimeContext struct {
	Timezone    string
	CurrentDate string
	CurrentTime string
}

func (s *OpenAIGatewayService) openaiClientLocalTimeRewriteEnabled() bool {
	if s == nil || s.cfg == nil {
		return true
	}
	return !s.cfg.Gateway.DisableOpenAIClientLocalTimeRewrite
}

func (s *OpenAIGatewayService) rewriteOpenAIClientLocalTimeForRequest(c *gin.Context, account *Account, body []byte) []byte {
	// A native WS session sends base instructions only in its prewarm. Later
	// deltas and tool outputs still describe that same client's environment.
	// Identify the client from the request, rather than requiring every frame
	// to repeat the base-instructions item. This also protects quoted XML in
	// user text and tool output from the legacy timezone substitution.
	if isCodexOAuthAccount(account) && c != nil && openai.IsCodexOfficialClientByHeaders(c.GetHeader("User-Agent"), c.GetHeader("originator")) {
		return body
	}
	return s.rewriteOpenAIClientLocalTimeIfEnabled(account, body)
}

func (s *OpenAIGatewayService) rewriteOpenAIClientLocalTimeIfEnabled(account *Account, body []byte) []byte {
	if len(body) == 0 || account == nil || !account.IsOpenAI() || !s.openaiClientLocalTimeRewriteEnabled() {
		return body
	}
	if codexInputCarriesBaseInstructions(body) {
		// Native CLI environment facts describe the user's machine, not the
		// gateway or its exit proxy. Keep them intact across retries and turns.
		return body
	}
	resolution := s.resolveOpenAIRequestTimezone(context.Background(), account)
	next, _ := rewriteOpenAIClientLocalTimeContextWith(body, openAILocalTimeContextFor(resolution.Timezone))
	return next
}

// resolveOpenAIRequestTimezone 返回账号本次请求应使用的时区。
// 解析器未注入（测试、未连线）时退回历史默认美西，保证零值安全。
func (s *OpenAIGatewayService) resolveOpenAIRequestTimezone(ctx context.Context, account *Account) OpenAIRequestTimezoneResolution {
	if s != nil && s.requestTimezoneResolver != nil {
		return s.requestTimezoneResolver.Resolve(ctx, account)
	}
	return OpenAIRequestTimezoneResolution{Timezone: openAIOutboundTimezoneName, Source: "fallback", Fallback: true}
}

func rewriteOpenAIClientLocalTimeContextWith(body []byte, ctx openAILocalTimeContext) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	next := rewriteOpenAIClientLocalTimeTagsAndJSONKeys(body, ctx)
	changed := !bytes.Equal(next, body)
	if gjson.ValidBytes(next) {
		located, locChanged := rewriteOpenAIUserLocationTimezones(next, ctx.Timezone)
		if locChanged {
			next = located
			changed = true
		}
	}
	return next, changed
}

func currentOpenAIOutboundLocalTimeContext() openAILocalTimeContext {
	return openAILocalTimeContextFor(openAIOutboundTimezoneName)
}

// openAILocalTimeContextFor 按目标时区名生成改写上下文（日期/时间取该时区的当前值）。
// 时区非法或加载失败时回落到 UTC，并保留原名（调用方负责把合法名传进来）。
func openAILocalTimeContextFor(tzName string) openAILocalTimeContext {
	loc := openAIRequestLocation(tzName)
	now := time.Now().In(loc)
	return openAILocalTimeContext{
		Timezone:    tzName,
		CurrentDate: now.Format("2006-01-02"),
		CurrentTime: now.Format("15:04:05"),
	}
}

func openAIRequestLocation(name string) *time.Location {
	if name == "" {
		name = openAIOutboundTimezoneName
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	// 固定兜底：默认名加载失败时用固定偏移构造一个同名区，避免落到 UTC
	// 与美西相差 7/8 小时而悄悄改变「当前日期」。
	if name == openAIOutboundTimezoneName {
		return time.FixedZone(openAIOutboundTimezoneName, -8*3600)
	}
	return time.UTC
}

func rewriteOpenAIClientLocalTimeTagsAndJSONKeys(body []byte, ctx openAILocalTimeContext) []byte {
	out := openAILocalTimeEnvContextRe.ReplaceAllFunc(body, func(block []byte) []byte {
		return rewriteOpenAILocalTimeXMLTagsInEnvContext(block, ctx)
	})
	out = openAILocalTimeJSONCurrentDateRe.ReplaceAll(out, []byte(`"current_date":"`+ctx.CurrentDate+`"`))
	out = openAILocalTimeJSONCurrentTimeRe.ReplaceAll(out, []byte(`"current_time":"`+ctx.CurrentTime+`"`))
	out = openAILocalTimeJSONTimezoneRe.ReplaceAll(out, []byte(`"timezone":"`+ctx.Timezone+`"`))
	return out
}

func rewriteOpenAILocalTimeXMLTagsInEnvContext(block []byte, ctx openAILocalTimeContext) []byte {
	out := block
	out = openAILocalTimeCurrentDateTagRe.ReplaceAll(out, []byte("<current_date>"+ctx.CurrentDate+"</current_date>"))
	out = openAILocalTimeLocalDateTagRe.ReplaceAll(out, []byte("<local_date>"+ctx.CurrentDate+"</local_date>"))
	out = openAILocalTimeTimezoneTagRe.ReplaceAll(out, []byte("<timezone>"+ctx.Timezone+"</timezone>"))
	out = openAILocalTimeTimeZoneTagRe.ReplaceAll(out, []byte("<time_zone>"+ctx.Timezone+"</time_zone>"))
	out = openAILocalTimeCurrentTimeTagRe.ReplaceAllFunc(out, func(tag []byte) []byte {
		return rewriteOpenAILocalTimeCurrentTimeTag(tag, ctx.CurrentTime)
	})
	return out
}

func rewriteOpenAILocalTimeCurrentTimeTag(tag []byte, currentTime string) []byte {
	m := openAILocalTimeCurrentTimeTagRe.FindSubmatch(tag)
	if len(m) != 2 {
		return tag
	}
	inner := m[1]
	trimmed := bytes.TrimSpace(inner)
	if !openAILocalTimeHHMMSSRe.Match(trimmed) {
		return tag
	}
	innerStart := bytes.Index(tag, inner)
	trimmedOff := bytes.Index(inner, trimmed)
	if innerStart < 0 || trimmedOff < 0 {
		return tag
	}
	start := innerStart + trimmedOff
	end := start + len(trimmed)
	out := make([]byte, 0, start+len(currentTime)+len(tag)-end)
	out = append(out, tag[:start]...)
	out = append(out, currentTime...)
	out = append(out, tag[end:]...)
	return out
}

func rewriteOpenAIUserLocationTimezones(body []byte, tzName string) ([]byte, bool) {
	if tzName == "" || !gjson.ValidBytes(body) {
		return body, false
	}
	next := body
	changed := false

	rewritePath := func(path string) {
		node := gjson.GetBytes(next, path)
		if !node.Exists() || node.Type != gjson.String {
			return
		}
		if strings.TrimSpace(node.String()) == tzName {
			return
		}
		updated, err := sjson.SetBytes(next, path, tzName)
		if err != nil {
			return
		}
		next = updated
		changed = true
	}

	rewritePath("web_search_options.user_location.approximate.timezone")
	rewritePath("web_search_options.user_location.timezone")
	rewritePath("user_location.approximate.timezone")
	rewritePath("user_location.timezone")

	tools := gjson.GetBytes(next, "tools")
	if tools.IsArray() {
		for i := range tools.Array() {
			prefix := fmt.Sprintf("tools.%d", i)
			rewritePath(prefix + ".user_location.approximate.timezone")
			rewritePath(prefix + ".user_location.timezone")
		}
	}
	return next, changed
}
