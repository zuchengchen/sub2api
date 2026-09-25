package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type excelBPSImageSettingsRepo struct {
	SettingRepository
	mu     sync.Mutex
	values map[string]string
	err    error
}

func (r *excelBPSImageSettingsRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		values[key] = r.values[key]
	}
	return values, r.err
}

func (r *excelBPSImageSettingsRepo) GetAll(_ context.Context) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	values := make(map[string]string, len(r.values))
	for key, value := range r.values {
		values[key] = value
	}
	return values, r.err
}

func (r *excelBPSImageSettingsRepo) GetValue(ctx context.Context, key string) (string, error) {
	values, err := r.GetMultiple(ctx, []string{key})
	return values[key], err
}

func (r *excelBPSImageSettingsRepo) SetMultiple(_ context.Context, values map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	if r.values == nil {
		r.values = make(map[string]string)
	}
	for key, value := range values {
		r.values[key] = value
	}
	return nil
}

func TestExcelBPSImageSettingsPersistAndApplyImmediately(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	ctx := context.Background()
	repo := &excelBPSImageSettingsRepo{}
	settings := NewSettingService(repo, &config.Config{})
	gateway := &OpenAIGatewayService{settingService: settings}
	t.Cleanup(func() { require.NoError(t, gateway.CloseExcelBPSImages()) })
	relay, err := gateway.excelBPSImageRelay(ctx)
	require.NoError(t, err)
	require.Nil(t, relay)
	require.NoError(t, settings.UpdateSettings(ctx, &SystemSettings{
		ExcelBPSImageRelayEnabled: true, ExcelBPSImageBaseURL: " https://images.example/ ",
		ExcelBPSImageBodyLimitMiB: 32, ExcelBPSImageBudgetMiB: 768, ExcelBPSImageMaxRequests: 48,
	}))
	saved, err := settings.GetAllSettings(ctx)
	require.NoError(t, err)
	require.True(t, saved.ExcelBPSImageRelayEnabled)
	require.Equal(t, "https://images.example", saved.ExcelBPSImageBaseURL)
	require.Equal(t, 32, saved.ExcelBPSImageBodyLimitMiB)
	require.Equal(t, 768, saved.ExcelBPSImageBudgetMiB)
	require.Equal(t, 48, saved.ExcelBPSImageMaxRequests)
	runtime, err := settings.GetExcelBPSImageRelaySettings(ctx)
	require.NoError(t, err)
	require.Equal(t, 32, runtime.BodyLimitMiB)
	require.Equal(t, 768, runtime.BudgetMiB)
	require.Equal(t, 48, runtime.MaxRequests)
	require.NoError(t, settings.UpdateSettings(ctx, &SystemSettings{
		ExcelBPSImageRelayEnabled: true, ExcelBPSImageBaseURL: "https://images.example",
		ExcelBPSImageBodyLimitMiB: 128, ExcelBPSImageBudgetMiB: 1024, ExcelBPSImageMaxRequests: 128,
	}))
	runtime, err = settings.GetExcelBPSImageRelaySettings(ctx)
	require.NoError(t, err)
	require.Equal(t, 128, runtime.BodyLimitMiB)
	require.Equal(t, 1024, runtime.BudgetMiB)
	require.Equal(t, 128, runtime.MaxRequests)
	relay, err = gateway.excelBPSImageRelay(ctx)
	require.NoError(t, err)
	require.NotNil(t, relay)
	// A small valid PNG lets this exercise the real cache and HTTP handler.
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRusAAAAASUVORK5CYII=")
	require.NoError(t, err)
	raw := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,%s"}]}]}`, base64.StdEncoding.EncodeToString(data)))
	out, err := relay.Rewrite(raw, "scope")
	require.NoError(t, err)
	imageURL := gjson.GetBytes(out, "input.0.content.0.image_url").String()
	fetch := func(url string) int {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, url, nil)
		gateway.ServeExcelBPSImage(c)
		return w.Code
	}
	require.Equal(t, 200, fetch(imageURL))
	require.NoError(t, settings.UpdateSettings(ctx, &SystemSettings{ExcelBPSImageRelayEnabled: true, ExcelBPSImageBaseURL: "https://new.example"}))
	updated, err := gateway.excelBPSImageRelay(ctx)
	require.NoError(t, err)
	require.Same(t, relay, updated, "changing the domain must not lose in-flight images")
	out, err = updated.Rewrite(raw, "scope")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(gjson.GetBytes(out, "input.0.content.0.image_url").String(), "https://new.example/"))
	require.Equal(t, 200, fetch(imageURL))
	require.NoError(t, settings.UpdateSettings(ctx, &SystemSettings{ExcelBPSImageRelayEnabled: false, ExcelBPSImageBaseURL: "https://new.example"}))
	disabled, err := gateway.excelBPSImageRelay(ctx)
	require.NoError(t, err)
	require.Nil(t, disabled)
	require.Equal(t, 404, fetch(imageURL), "disabling must immediately stop image reads")
}

func TestExcelBPSImageSettingsRejectInvalidUpdatesAtomically(t *testing.T) {
	ctx := context.Background()
	repo := &excelBPSImageSettingsRepo{}
	settings := NewSettingService(repo, &config.Config{})
	require.NoError(t, settings.UpdateSettings(ctx, &SystemSettings{ExcelBPSImageRelayEnabled: true, ExcelBPSImageBaseURL: "https://images.example"}))
	for _, limits := range []struct{ body, budget, requests int }{
		{129, 2048, 32}, {64, 511, 32}, {64, 2049, 32}, {64, 512, 129}, {128, 512, 32},
	} {
		err := settings.UpdateSettings(ctx, &SystemSettings{
			ExcelBPSImageRelayEnabled: true, ExcelBPSImageBaseURL: "https://images.example",
			ExcelBPSImageBodyLimitMiB: limits.body, ExcelBPSImageBudgetMiB: limits.budget, ExcelBPSImageMaxRequests: limits.requests,
		})
		require.Error(t, err)
		runtime, err := settings.GetExcelBPSImageRelaySettings(ctx)
		require.NoError(t, err)
		require.Equal(t, DefaultExcelBPSImageBodyLimitMiB, runtime.BodyLimitMiB)
		require.Equal(t, DefaultExcelBPSImageBudgetMiB, runtime.BudgetMiB)
		require.Equal(t, DefaultExcelBPSImageMaxRequests, runtime.MaxRequests)
	}
	for _, origin := range []string{"", "http://images.example", "https://images.example/v1", "https://user:secret@images.example", "https://images.example?token=secret"} {
		err := settings.UpdateSettings(ctx, &SystemSettings{ExcelBPSImageRelayEnabled: true, ExcelBPSImageBaseURL: origin})
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret")
		runtime, err := settings.GetExcelBPSImageRelaySettings(ctx)
		require.NoError(t, err)
		require.True(t, runtime.Enabled)
		require.Equal(t, "https://images.example", runtime.BaseURL)
	}
	repo.err = errors.New("database-private-error")
	runtime, err := settings.GetExcelBPSImageRelaySettings(ctx)
	require.Error(t, err)
	require.False(t, runtime.Enabled)
	require.NotContains(t, err.Error(), "database-private-error")
}
