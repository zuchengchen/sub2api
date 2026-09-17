package service

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 出站给 OpenAI 的本地日期/时区固定为美西太平洋时区，不跟操作系统或应用 timezone 走。
const openAIOutboundTimezoneName = "America/Los_Angeles"

// openAIOutboundLocalTimeContextFn 可在测试中替换。
var openAIOutboundLocalTimeContextFn = currentOpenAIOutboundLocalTimeContext

var (
	openAIOutboundTimezoneOnce sync.Once
	openAIOutboundTimezoneLoc  *time.Location

	openAILocalTimeCurrentDateTagRe = regexp.MustCompile(`(?s)<current_date>\s*[^<]*?</current_date>`)
	openAILocalTimeTimezoneTagRe    = regexp.MustCompile(`(?s)<timezone>\s*[^<]*?</timezone>`)
	openAILocalTimeCurrentTimeTagRe = regexp.MustCompile(`(?s)<current_time>\s*[^<]*?</current_time>`)
	openAILocalTimeLocalDateTagRe   = regexp.MustCompile(`(?s)<local_date>\s*[^<]*?</local_date>`)
	openAILocalTimeTimeZoneTagRe    = regexp.MustCompile(`(?s)<time_zone>\s*[^<]*?</time_zone>`)

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

func (s *OpenAIGatewayService) rewriteOpenAIClientLocalTimeIfEnabled(account *Account, body []byte) []byte {
	if len(body) == 0 || account == nil || !account.IsOpenAI() || !s.openaiClientLocalTimeRewriteEnabled() {
		return body
	}
	next, _ := rewriteOpenAIClientLocalTimeContext(body)
	return next
}

func rewriteOpenAIClientLocalTimeContext(body []byte) ([]byte, bool) {
	return rewriteOpenAIClientLocalTimeContextWith(body, openAIOutboundLocalTimeContextFn())
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
	loc := openAIOutboundTimezone()
	now := time.Now().In(loc)
	return openAILocalTimeContext{
		Timezone:    openAIOutboundTimezoneName,
		CurrentDate: now.Format("2006-01-02"),
		CurrentTime: now.Format("15:04:05"),
	}
}

func openAIOutboundTimezone() *time.Location {
	openAIOutboundTimezoneOnce.Do(func() {
		loc, err := time.LoadLocation(openAIOutboundTimezoneName)
		if err != nil {
			loc = time.FixedZone(openAIOutboundTimezoneName, -8*3600)
		}
		openAIOutboundTimezoneLoc = loc
	})
	return openAIOutboundTimezoneLoc
}

func rewriteOpenAIClientLocalTimeTagsAndJSONKeys(body []byte, ctx openAILocalTimeContext) []byte {
	out := body
	out = openAILocalTimeCurrentDateTagRe.ReplaceAll(out, []byte("<current_date>"+ctx.CurrentDate+"</current_date>"))
	out = openAILocalTimeLocalDateTagRe.ReplaceAll(out, []byte("<local_date>"+ctx.CurrentDate+"</local_date>"))
	out = openAILocalTimeTimezoneTagRe.ReplaceAll(out, []byte("<timezone>"+ctx.Timezone+"</timezone>"))
	out = openAILocalTimeTimeZoneTagRe.ReplaceAll(out, []byte("<time_zone>"+ctx.Timezone+"</time_zone>"))
	out = openAILocalTimeCurrentTimeTagRe.ReplaceAll(out, []byte("<current_time>"+ctx.CurrentTime+"</current_time>"))
	out = openAILocalTimeJSONCurrentDateRe.ReplaceAll(out, []byte(`"current_date":"`+ctx.CurrentDate+`"`))
	out = openAILocalTimeJSONCurrentTimeRe.ReplaceAll(out, []byte(`"current_time":"`+ctx.CurrentTime+`"`))
	out = openAILocalTimeJSONTimezoneRe.ReplaceAll(out, []byte(`"timezone":"`+ctx.Timezone+`"`))
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
