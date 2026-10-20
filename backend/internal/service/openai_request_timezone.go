package service

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// openAIRequestTimezoneExtraKey 是账号 extra 里手动覆盖请求时区的键。
// 决定 4：账号可手动覆盖，覆盖优先于按出口 IP 自动确定的结果。
const openAIRequestTimezoneExtraKey = "openai_request_timezone"

// DefaultOpenAIRequestTimezone 是拿不到出口时区时的回退值。
// 沿用此前 openai_local_time_rewrite.go 的硬编码美西时区，保证回滚语义不变。
const DefaultOpenAIRequestTimezone = "America/Los_Angeles"

// directEgressProxyID 是「无代理、直连出口」在 proxy:latency 缓存里的 sentinel 键。
// 真实代理 ID 都是正数，负数不会和数据库自增冲突。
// 决定 4：缓存按「代理 / 直连」分键，不按账号分键——同一个代理下的多个账号
// 共享同一个出口时区，直连账号共享直连出口的时区。
const directEgressProxyID int64 = -1

// OpenAIRequestTimezone 返回账号应该使用的 IANA 请求时区。
// 优先取账号 extra 的手动覆盖（须为合法时区），否则返回空串交给解析器按出口决定。
func (a *Account) OpenAIRequestTimezone() string {
	if a == nil || !a.IsOpenAI() {
		return ""
	}
	return NormalizeOpenAIRequestTimezone(a.getExtraString(openAIRequestTimezoneExtraKey))
}

// NormalizeOpenAIRequestTimezone 校验并规整一个 IANA 时区名；非法返回空串。
func NormalizeOpenAIRequestTimezone(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if _, err := time.LoadLocation(name); err != nil {
		return ""
	}
	return name
}

// ValidateOpenAIRequestTimezoneExtra 校验账号 extra 里的手动时区覆盖。
// 只在 platform == openai 且该键存在时校验；非 OpenAI 平台不允许设置该键。
func ValidateOpenAIRequestTimezoneExtra(platform string, extra map[string]any) error {
	if extra == nil {
		return nil
	}
	raw, exists := extra[openAIRequestTimezoneExtraKey]
	if !exists || raw == nil {
		return nil
	}
	name, ok := raw.(string)
	if !ok {
		return infraerrors.New(http.StatusBadRequest, "INVALID_OPENAI_REQUEST_TIMEZONE", "openai_request_timezone must be a string")
	}
	if name != "" {
		if platform != PlatformOpenAI {
			return infraerrors.New(http.StatusBadRequest, "INVALID_OPENAI_REQUEST_TIMEZONE", "openai_request_timezone is only valid for OpenAI accounts")
		}
		if NormalizeOpenAIRequestTimezone(name) == "" {
			return infraerrors.New(http.StatusBadRequest, "INVALID_OPENAI_REQUEST_TIMEZONE", "openai_request_timezone must be a valid IANA timezone")
		}
	}
	return nil
}

// OpenAIRequestTimezoneResolution 是一次时区解析的结果。
type OpenAIRequestTimezoneResolution struct {
	Timezone string
	// Source 记录时区来源：override=账号手动覆盖 / proxy=代理出口缓存 /
	// direct=直连出口缓存 / fallback=回退默认值。
	Source   string
	Fallback bool
}

// OpenAIRequestTimezoneResolver 按出口 IP 自动确定 OpenAI 账号的请求时区。
//
// 决定 4：有代理看代理出口，没代理看直连出口；缓存按「代理 / 直连」分键；
// 账号手动覆盖优先；拿不到时区时用可配置默认并在日志/管理端标记为回退。
type OpenAIRequestTimezoneResolver struct {
	latencyCache    ProxyLatencyCache
	defaultTimezone string
}

func NewOpenAIRequestTimezoneResolver(latencyCache ProxyLatencyCache, defaultTimezone string) *OpenAIRequestTimezoneResolver {
	if NormalizeOpenAIRequestTimezone(defaultTimezone) == "" {
		defaultTimezone = DefaultOpenAIRequestTimezone
	}
	return &OpenAIRequestTimezoneResolver{
		latencyCache:    latencyCache,
		defaultTimezone: defaultTimezone,
	}
}

// Resolve 决定一个账号出站请求应该使用的时区。
// 顺序：手动覆盖 → 该账号代理出口的缓存时区 → 直连出口的缓存时区 → 回退默认。
func (r *OpenAIRequestTimezoneResolver) Resolve(ctx context.Context, account *Account) OpenAIRequestTimezoneResolution {
	if account == nil || !account.IsOpenAI() {
		return OpenAIRequestTimezoneResolution{Timezone: r.defaultTimezone, Source: "fallback", Fallback: true}
	}
	if override := account.OpenAIRequestTimezone(); override != "" {
		return OpenAIRequestTimezoneResolution{Timezone: override, Source: "override"}
	}
	cacheKey := directEgressProxyID
	if account.ProxyID != nil && *account.ProxyID > 0 {
		cacheKey = *account.ProxyID
	}
	if r.latencyCache != nil {
		if latencies, err := r.latencyCache.GetProxyLatencies(ctx, []int64{cacheKey}); err == nil {
			if info := latencies[cacheKey]; info != nil {
				if tz := NormalizeOpenAIRequestTimezone(info.Timezone); tz != "" {
					source := "direct"
					if cacheKey != directEgressProxyID {
						source = "proxy"
					}
					return OpenAIRequestTimezoneResolution{Timezone: tz, Source: source}
				}
			}
		}
	}
	logger.FromContext(ctx).Debug("openai request timezone fallback",
		zap.Int64("account_id", account.ID), zap.Int64("egress_cache_key", cacheKey),
		zap.String("default_timezone", r.defaultTimezone))
	return OpenAIRequestTimezoneResolution{Timezone: r.defaultTimezone, Source: "fallback", Fallback: true}
}

// ─── 定时刷新 ──────────────────────────────────────────────────────────────

// openAIRequestTimezoneRefreshInterval 出口时区的周期重探间隔。
// ip-api 免费档限速 45 次/分钟；活跃代理数量有限，1 小时一次远低于限速。
const openAIRequestTimezoneRefreshInterval = time.Hour

// OpenAIRequestTimezoneRefreshWorker 周期性重探「所有活跃代理出口 + 直连出口」，
// 把 ip-api 的 timezone 写进按代理/直连分键的 latency 缓存，供请求路径读取。
//
// 代理质检（CheckProxyQuality / TestProxy / 创建时的异步 probe）已经顺带刷新
// 各自代理的时区；这个 worker 补上「没人点质检也会更新」以及直连出口的刷新。
type OpenAIRequestTimezoneRefreshWorker struct {
	prober       ProxyExitInfoProber
	proxyRepo    ProxyRepository
	latencyCache ProxyLatencyCache
	interval     time.Duration
	stopCh       chan struct{}
	stopOnce     sync.Once
	wg           sync.WaitGroup
}

func NewOpenAIRequestTimezoneRefreshWorker(
	prober ProxyExitInfoProber,
	proxyRepo ProxyRepository,
	latencyCache ProxyLatencyCache,
	interval time.Duration,
) *OpenAIRequestTimezoneRefreshWorker {
	if interval <= 0 {
		interval = openAIRequestTimezoneRefreshInterval
	}
	return &OpenAIRequestTimezoneRefreshWorker{
		prober:       prober,
		proxyRepo:    proxyRepo,
		latencyCache: latencyCache,
		interval:     interval,
		stopCh:       make(chan struct{}),
	}
}

func (w *OpenAIRequestTimezoneRefreshWorker) Start() {
	if w == nil || w.prober == nil || w.latencyCache == nil || w.interval <= 0 {
		return
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.refreshAll()
			case <-w.stopCh:
				return
			}
		}
	}()
}

func (w *OpenAIRequestTimezoneRefreshWorker) Stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() { close(w.stopCh) })
	w.wg.Wait()
}

// refreshAll 先刷直连出口（sentinel 键），再逐个刷活跃代理。
// 单代理失败只影响该代理的缓存，不中断整轮。
func (w *OpenAIRequestTimezoneRefreshWorker) refreshAll() {
	ctx, cancel := context.WithTimeout(context.Background(), openAIRequestTimezoneRefreshInterval)
	defer cancel()
	w.refreshDirect(ctx)
	if w.proxyRepo == nil {
		return
	}
	proxies, err := w.proxyRepo.ListActive(ctx)
	if err != nil {
		logger.LegacyPrintf("service.openai_timezone", "list active proxies for timezone refresh failed: %v", err)
		return
	}
	for i := range proxies {
		w.refreshProxy(ctx, &proxies[i])
	}
}

// refreshDirect 探测服务器直连出口（空 proxyURL），写 sentinel 键。
func (w *OpenAIRequestTimezoneRefreshWorker) refreshDirect(ctx context.Context) {
	w.probeAndStore(ctx, directEgressProxyID, "")
}

// refreshProxy 探测一个代理出口，写它自己的缓存键。
func (w *OpenAIRequestTimezoneRefreshWorker) refreshProxy(ctx context.Context, proxy *Proxy) {
	if proxy == nil {
		return
	}
	w.probeAndStore(ctx, proxy.ID, proxy.URL())
}

func (w *OpenAIRequestTimezoneRefreshWorker) probeAndStore(ctx context.Context, cacheKey int64, proxyURL string) {
	exitInfo, latencyMs, err := w.prober.ProbeProxy(ctx, proxyURL)
	if err != nil {
		return
	}
	if exitInfo == nil || NormalizeOpenAIRequestTimezone(exitInfo.Timezone) == "" {
		// 探测成功但源没给时区（兜底解析器），不覆盖已有值。
		return
	}
	existing, _ := w.latencyCache.GetProxyLatencies(ctx, []int64{cacheKey})
	info := &ProxyLatencyInfo{Success: true, LatencyMs: &latencyMs, UpdatedAt: time.Now()}
	if prev := existing[cacheKey]; prev != nil {
		*info = *prev
		info.LatencyMs = &latencyMs
		info.UpdatedAt = time.Now()
	}
	info.IPAddress = exitInfo.IP
	info.Country = exitInfo.Country
	info.CountryCode = exitInfo.CountryCode
	info.Region = exitInfo.Region
	info.City = exitInfo.City
	info.Timezone = exitInfo.Timezone
	if err := w.latencyCache.SetProxyLatency(ctx, cacheKey, info); err != nil {
		logger.LegacyPrintf("service.openai_timezone", "store egress timezone failed key=%d: %v", cacheKey, err)
	}
}
