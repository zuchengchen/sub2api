package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type stubProxyLatencyCache struct {
	infos map[int64]*ProxyLatencyInfo
	sets  map[int64]*ProxyLatencyInfo
}

func (s *stubProxyLatencyCache) GetProxyLatencies(_ context.Context, ids []int64) (map[int64]*ProxyLatencyInfo, error) {
	out := make(map[int64]*ProxyLatencyInfo, len(ids))
	for _, id := range ids {
		if info, ok := s.infos[id]; ok {
			out[id] = info
		}
	}
	return out, nil
}

func (s *stubProxyLatencyCache) SetProxyLatency(_ context.Context, id int64, info *ProxyLatencyInfo) error {
	if s.sets == nil {
		s.sets = make(map[int64]*ProxyLatencyInfo)
	}
	s.sets[id] = info
	return nil
}

func TestNormalizeOpenAIRequestTimezone(t *testing.T) {
	require.Equal(t, "America/Los_Angeles", NormalizeOpenAIRequestTimezone(" America/Los_Angeles "))
	require.Equal(t, "Asia/Shanghai", NormalizeOpenAIRequestTimezone("Asia/Shanghai"))
	require.Equal(t, "", NormalizeOpenAIRequestTimezone(""))
	require.Equal(t, "", NormalizeOpenAIRequestTimezone("Mars/Olympus"))
	require.Equal(t, "", NormalizeOpenAIRequestTimezone("  "))
}

func TestValidateOpenAIRequestTimezoneExtra(t *testing.T) {
	// 无键或非 OpenAI 平台不校验
	require.NoError(t, ValidateOpenAIRequestTimezoneExtra(PlatformOpenAI, nil))
	require.NoError(t, ValidateOpenAIRequestTimezoneExtra(PlatformOpenAI, map[string]any{}))
	require.NoError(t, ValidateOpenAIRequestTimezoneExtra(PlatformAnthropic, nil))

	// 合法时区（OpenAI）
	require.NoError(t, ValidateOpenAIRequestTimezoneExtra(PlatformOpenAI, map[string]any{
		openAIRequestTimezoneExtraKey: "Asia/Tokyo",
	}))
	// 空串 = 清除覆盖，合法
	require.NoError(t, ValidateOpenAIRequestTimezoneExtra(PlatformOpenAI, map[string]any{
		openAIRequestTimezoneExtraKey: "",
	}))

	// 非法值
	require.Error(t, ValidateOpenAIRequestTimezoneExtra(PlatformOpenAI, map[string]any{
		openAIRequestTimezoneExtraKey: "Not/AZone",
	}))
	require.Error(t, ValidateOpenAIRequestTimezoneExtra(PlatformOpenAI, map[string]any{
		openAIRequestTimezoneExtraKey: 123,
	}))
	// 非 OpenAI 平台设置非空值被拒绝
	require.Error(t, ValidateOpenAIRequestTimezoneExtra(PlatformAnthropic, map[string]any{
		openAIRequestTimezoneExtraKey: "Asia/Tokyo",
	}))
}

func TestOpenAIRequestTimezoneResolver_OverrideWins(t *testing.T) {
	cache := &stubProxyLatencyCache{infos: map[int64]*ProxyLatencyInfo{
		7: {Timezone: "Asia/Singapore"},
	}}
	r := NewOpenAIRequestTimezoneResolver(cache, "")
	proxyID := int64(7)
	account := &Account{
		Platform: PlatformOpenAI,
		ProxyID:  &proxyID,
		Extra:    map[string]any{openAIRequestTimezoneExtraKey: "Europe/Berlin"},
	}
	res := r.Resolve(context.Background(), account)
	require.Equal(t, "Europe/Berlin", res.Timezone)
	require.Equal(t, "override", res.Source)
	require.False(t, res.Fallback)
}

func TestOpenAIRequestTimezoneResolver_ProxyEgress(t *testing.T) {
	cache := &stubProxyLatencyCache{infos: map[int64]*ProxyLatencyInfo{
		42: {Timezone: "Asia/Tokyo"},
	}}
	r := NewOpenAIRequestTimezoneResolver(cache, "")
	proxyID := int64(42)
	account := &Account{Platform: PlatformOpenAI, ProxyID: &proxyID}
	res := r.Resolve(context.Background(), account)
	require.Equal(t, "Asia/Tokyo", res.Timezone)
	require.Equal(t, "proxy", res.Source)
	require.False(t, res.Fallback)
}

func TestOpenAIRequestTimezoneResolver_DirectEgress(t *testing.T) {
	cache := &stubProxyLatencyCache{infos: map[int64]*ProxyLatencyInfo{
		directEgressProxyID: {Timezone: "America/Chicago"},
	}}
	r := NewOpenAIRequestTimezoneResolver(cache, "")
	// 无代理账号 → 直连 sentinel 键
	account := &Account{Platform: PlatformOpenAI}
	res := r.Resolve(context.Background(), account)
	require.Equal(t, "America/Chicago", res.Timezone)
	require.Equal(t, "direct", res.Source)
	require.False(t, res.Fallback)
}

func TestOpenAIRequestTimezoneResolver_ProxyAndDirectSeparateKeys(t *testing.T) {
	// 代理缓存和直连缓存互不串：账号带代理就只看代理键，没填代理键才回退。
	cache := &stubProxyLatencyCache{infos: map[int64]*ProxyLatencyInfo{
		directEgressProxyID: {Timezone: "America/Chicago"},
	}}
	r := NewOpenAIRequestTimezoneResolver(cache, "")
	proxyID := int64(9) // 该代理缓存没时区
	account := &Account{Platform: PlatformOpenAI, ProxyID: &proxyID}
	res := r.Resolve(context.Background(), account)
	require.Equal(t, "fallback", res.Source)
	require.True(t, res.Fallback)
	require.Equal(t, DefaultOpenAIRequestTimezone, res.Timezone)
}

func TestOpenAIRequestTimezoneResolver_FallbackMarks(t *testing.T) {
	r := NewOpenAIRequestTimezoneResolver(&stubProxyLatencyCache{}, "")
	// 无任何缓存 → 回退默认并标记
	res := r.Resolve(context.Background(), &Account{Platform: PlatformOpenAI})
	require.True(t, res.Fallback)
	require.Equal(t, DefaultOpenAIRequestTimezone, res.Timezone)

	// 非 OpenAI 账号也回退（调用方会先挡掉，这里仅保底）
	res = r.Resolve(context.Background(), &Account{Platform: PlatformGrok})
	require.True(t, res.Fallback)

	// nil 账号
	res = r.Resolve(context.Background(), nil)
	require.True(t, res.Fallback)
}

func TestOpenAIRequestTimezoneResolver_IgnoresInvalidCachedTimezone(t *testing.T) {
	// 缓存里存了非法时区（探测源异常）→ 不用它，回退。
	cache := &stubProxyLatencyCache{infos: map[int64]*ProxyLatencyInfo{
		directEgressProxyID: {Timezone: "Not/AZone"},
	}}
	r := NewOpenAIRequestTimezoneResolver(cache, "")
	res := r.Resolve(context.Background(), &Account{Platform: PlatformOpenAI})
	require.True(t, res.Fallback)
	require.Equal(t, DefaultOpenAIRequestTimezone, res.Timezone)
}

func TestOpenAIRequestTimezoneResolver_CustomDefault(t *testing.T) {
	r := NewOpenAIRequestTimezoneResolver(&stubProxyLatencyCache{}, "Asia/Dubai")
	res := r.Resolve(context.Background(), &Account{Platform: PlatformOpenAI})
	require.Equal(t, "Asia/Dubai", res.Timezone)

	// 非法配置默认回落到 LA
	r = NewOpenAIRequestTimezoneResolver(&stubProxyLatencyCache{}, "Bogus/Zone")
	res = r.Resolve(context.Background(), &Account{Platform: PlatformOpenAI})
	require.Equal(t, DefaultOpenAIRequestTimezone, res.Timezone)
}

func TestOpenAILocalTimeContextFor_UsesTargetTimezone(t *testing.T) {
	ctxAsia := openAILocalTimeContextFor("Asia/Tokyo")
	require.Equal(t, "Asia/Tokyo", ctxAsia.Timezone)
	require.NotEmpty(t, ctxAsia.CurrentDate)
	require.NotEmpty(t, ctxAsia.CurrentTime)

	// 非法时区名回落（保留原名），不 panic
	ctxBad := openAILocalTimeContextFor("Not/AZone")
	require.Equal(t, "Not/AZone", ctxBad.Timezone)
}

func TestRewriteOpenAIClientLocalTimeIfEnabled_UsesResolvedTimezone(t *testing.T) {
	// 直连出口缓存命中 Asia/Tokyo → 请求体时区改写成它，而不是回退美西。
	cache := &stubProxyLatencyCache{infos: map[int64]*ProxyLatencyInfo{
		directEgressProxyID: {Timezone: "Asia/Tokyo"},
	}}
	svc := &OpenAIGatewayService{}
	svc.SetRequestTimezoneResolver(NewOpenAIRequestTimezoneResolver(cache, ""))
	account := &Account{Platform: PlatformOpenAI}
	body := []byte(`{"input":"<environment_context><timezone>UTC</timezone></environment_context>"}`)
	got := svc.rewriteOpenAIClientLocalTimeIfEnabled(account, body)
	require.Contains(t, string(got), "<timezone>Asia/Tokyo</timezone>")
	require.NotContains(t, string(got), "America/Los_Angeles")

	// 账号手动覆盖优先于出口缓存。
	accountOverride := &Account{Platform: PlatformOpenAI, Extra: map[string]any{openAIRequestTimezoneExtraKey: "Europe/Paris"}}
	got = svc.rewriteOpenAIClientLocalTimeIfEnabled(accountOverride, body)
	require.Contains(t, string(got), "<timezone>Europe/Paris</timezone>")
}

func TestRewriteOpenAIClientLocalTimeIfEnabled_WebSearchUserLocation(t *testing.T) {
	// 出口缓存命中时区也要改写 web_search 的 user_location.timezone（决定 4 作用面）。
	cache := &stubProxyLatencyCache{infos: map[int64]*ProxyLatencyInfo{
		directEgressProxyID: {Timezone: "Asia/Tokyo"},
	}}
	svc := &OpenAIGatewayService{}
	svc.SetRequestTimezoneResolver(NewOpenAIRequestTimezoneResolver(cache, ""))
	account := &Account{Platform: PlatformOpenAI}
	body := []byte(`{"tools":[{"type":"web_search","user_location":{"timezone":"UTC"}}]}`)
	got := svc.rewriteOpenAIClientLocalTimeIfEnabled(account, body)
	require.Contains(t, string(got), `"timezone":"Asia/Tokyo"`)
}
