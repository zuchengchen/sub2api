package service

import (
	"context"
	"time"
)

type ProxyLatencyInfo struct {
	Success     bool   `json:"success"`
	LatencyMs   *int64 `json:"latency_ms,omitempty"`
	Message     string `json:"message,omitempty"`
	IPAddress   string `json:"ip_address,omitempty"`
	Country     string `json:"country,omitempty"`
	CountryCode string `json:"country_code,omitempty"`
	Region      string `json:"region,omitempty"`
	City        string `json:"city,omitempty"`
	// Timezone 是该代理出口 IP 对应的 IANA 时区，由 ip-api 探测顺带写入。
	// 决定 4 的「按出口 IP 自动确定请求时区」直接读这个缓存值；空串表示
	// 探测源没给时区（比如命中了 ipify/chatgpt-trace 兜底）或从未探测成功。
	Timezone         string    `json:"timezone,omitempty"`
	QualityStatus    string    `json:"quality_status,omitempty"`
	QualityScore     *int      `json:"quality_score,omitempty"`
	QualityGrade     string    `json:"quality_grade,omitempty"`
	QualitySummary   string    `json:"quality_summary,omitempty"`
	QualityCheckedAt *int64    `json:"quality_checked_at,omitempty"`
	QualityCFRay     string    `json:"quality_cf_ray,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type ProxyLatencyCache interface {
	GetProxyLatencies(ctx context.Context, proxyIDs []int64) (map[int64]*ProxyLatencyInfo, error)
	SetProxyLatency(ctx context.Context, proxyID int64, info *ProxyLatencyInfo) error
}
