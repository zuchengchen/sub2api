package service

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func pacificTestContext(t *testing.T, hour int) openAILocalTimeContext {
	t.Helper()
	loc, err := time.LoadLocation(openAIOutboundTimezoneName)
	require.NoError(t, err)
	now := time.Date(2026, 9, 17, hour, 15, 4, 0, loc)
	return openAILocalTimeContext{
		Timezone:    openAIOutboundTimezoneName,
		CurrentDate: now.Format("2006-01-02"),
		CurrentTime: now.Format("15:04:05"),
	}
}

func TestRewriteOpenAIClientLocalTimeContext_EnvironmentTags(t *testing.T) {
	ctx := pacificTestContext(t, 22)

	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"<environment_context>\n  <shell>zsh</shell>\n  <current_date>2026-02-26</current_date>\n  <timezone>Asia/Shanghai</timezone>\n</environment_context>"}]}`)
	got, changed := rewriteOpenAIClientLocalTimeContextWith(body, ctx)
	require.True(t, changed)
	text := gjson.GetBytes(got, "input.0.content").String()
	require.Contains(t, text, "<current_date>2026-09-17</current_date>")
	require.Contains(t, text, "<timezone>America/Los_Angeles</timezone>")
	require.NotContains(t, text, "2026-02-26")
	require.NotContains(t, text, "Asia/Shanghai")
}

func TestRewriteOpenAIClientLocalTimeContext_DoesNotTouchProseDates(t *testing.T) {
	ctx := pacificTestContext(t, 8)

	body := []byte(`{"input":[{"role":"user","content":"the bug from 2026-01-15 in Asia/Tokyo still happens"}]}`)
	got, changed := rewriteOpenAIClientLocalTimeContextWith(body, ctx)
	require.False(t, changed)
	require.JSONEq(t, string(body), string(got))
}

func TestRewriteOpenAIClientLocalTimeContext_JSONKeysAndUserLocation(t *testing.T) {
	ctx := pacificTestContext(t, 9)

	body := []byte(`{"current_date":"2026-06-04","timezone":"Europe/London","web_search_options":{"user_location":{"approximate":{"timezone":"UTC"}}},"tools":[{"type":"web_search","user_location":{"timezone":"UTC+8"}}]}`)
	got, changed := rewriteOpenAIClientLocalTimeContextWith(body, ctx)
	require.True(t, changed)
	require.Equal(t, "2026-09-17", gjson.GetBytes(got, "current_date").String())
	require.Equal(t, "America/Los_Angeles", gjson.GetBytes(got, "timezone").String())
	require.Equal(t, "America/Los_Angeles", gjson.GetBytes(got, "web_search_options.user_location.approximate.timezone").String())
	require.Equal(t, "America/Los_Angeles", gjson.GetBytes(got, "tools.0.user_location.timezone").String())
}

func TestRewriteOpenAIClientLocalTimeContext_CurrentTimeTag(t *testing.T) {
	ctx := pacificTestContext(t, 22)

	body := []byte(`{"input":"<environment_context><current_time>07:00:00</current_time></environment_context>"}`)
	got, changed := rewriteOpenAIClientLocalTimeContextWith(body, ctx)
	require.True(t, changed)
	require.Contains(t, string(got), "<current_time>22:15:04</current_time>")
}

func TestRewriteOpenAIClientLocalTimeContext_Idempotent(t *testing.T) {
	ctx := pacificTestContext(t, 10)

	body := []byte(`{"input":"<environment_context><current_date>2026-09-17</current_date><timezone>America/Los_Angeles</timezone></environment_context>"}`)
	got, changed := rewriteOpenAIClientLocalTimeContextWith(body, ctx)
	require.False(t, changed)
	require.Equal(t, string(body), string(got))
}

func TestRewriteOpenAIClientLocalTimeIfEnabled_UsesPacificTime(t *testing.T) {
	require.NoError(t, timezone.Init("Asia/Shanghai"))
	t.Cleanup(func() {
		openAIOutboundLocalTimeContextFn = currentOpenAIOutboundLocalTimeContext
	})
	openAIOutboundLocalTimeContextFn = func() openAILocalTimeContext {
		return openAILocalTimeContext{Timezone: openAIOutboundTimezoneName, CurrentDate: "2026-09-16", CurrentTime: "23:21:00"}
	}

	account := &Account{Platform: PlatformOpenAI}
	body := []byte(`{"input":"<timezone>UTC</timezone>"}`)

	enabled := &OpenAIGatewayService{}
	got := enabled.rewriteOpenAIClientLocalTimeIfEnabled(account, body)
	require.Contains(t, string(got), "<timezone>America/Los_Angeles</timezone>")
	require.NotContains(t, string(got), "Asia/Shanghai")
	require.NotContains(t, string(got), "<timezone>UTC</timezone>")

	disabled := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{DisableOpenAIClientLocalTimeRewrite: true}}}
	unchanged := disabled.rewriteOpenAIClientLocalTimeIfEnabled(account, body)
	require.Equal(t, string(body), string(unchanged))

	nonOpenAI := enabled.rewriteOpenAIClientLocalTimeIfEnabled(&Account{Platform: PlatformGrok}, body)
	require.Equal(t, string(body), string(nonOpenAI))
}

func TestRewriteOpenAIClientLocalTimeContext_EscapedJSONInUserTextUntouched(t *testing.T) {
	ctx := pacificTestContext(t, 8)
	body := []byte(`{"input":"example payload {\"timezone\":\"UTC\"}"}`)
	got, changed := rewriteOpenAIClientLocalTimeContextWith(body, ctx)
	require.False(t, changed)
	require.Equal(t, string(body), string(got))
}

func TestCurrentOpenAIOutboundLocalTimeContext_IsPacific(t *testing.T) {
	ctx := currentOpenAIOutboundLocalTimeContext()
	require.Equal(t, "America/Los_Angeles", ctx.Timezone)
	loc, err := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, err)
	require.Equal(t, time.Now().In(loc).Format("2006-01-02"), ctx.CurrentDate)
}
