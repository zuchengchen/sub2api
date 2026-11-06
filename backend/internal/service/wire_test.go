package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/zeromicro/go-zero/core/collection"
)

func TestProvideTimingWheelService_ReturnsError(t *testing.T) {
	original := newTimingWheel
	t.Cleanup(func() { newTimingWheel = original })

	newTimingWheel = func(_ time.Duration, _ int, _ collection.Execute) (*collection.TimingWheel, error) {
		return nil, errors.New("boom")
	}

	svc, err := ProvideTimingWheelService()
	if err == nil {
		t.Fatalf("期望返回 error，但得到 nil")
	}
	if svc != nil {
		t.Fatalf("期望返回 nil svc，但得到非空")
	}
}

func TestProvideTimingWheelService_Success(t *testing.T) {
	svc, err := ProvideTimingWheelService()
	if err != nil {
		t.Fatalf("期望 err 为 nil，但得到: %v", err)
	}
	if svc == nil {
		t.Fatalf("期望 svc 非空，但得到 nil")
	}
	svc.Stop()
}
func TestProvideOpenAIRequestTimezoneRefreshWorkerWiresGatewayResolver(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIRequestTimezoneFallback = "Asia/Dubai"
	gateway := &OpenAIGatewayService{}
	worker := ProvideOpenAIRequestTimezoneRefreshWorker(nil, nil, nil, cfg, gateway)
	defer worker.Stop()
	if gateway.requestTimezoneResolver == nil {
		t.Fatal("timezone worker provider must also wire the gateway resolver")
	}
	got := gateway.requestTimezoneResolver.Resolve(context.Background(), &Account{})
	if got.Timezone != "Asia/Dubai" {
		t.Fatalf("gateway timezone fallback = %q, want Asia/Dubai", got.Timezone)
	}
}
