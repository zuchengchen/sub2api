package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
)

const (
	SettingKeyExcelBPSImageRelayEnabled = "excel_bps_image_relay_enabled"
	SettingKeyExcelBPSImageBaseURL      = "excel_bps_image_base_url"
	SettingKeyExcelBPSImageBodyLimitMiB = "excel_bps_image_body_limit_mib"
	SettingKeyExcelBPSImageBudgetMiB    = "excel_bps_image_budget_mib"
	SettingKeyExcelBPSImageMaxRequests  = "excel_bps_image_max_requests"

	DefaultExcelBPSImageBodyLimitMiB = 64
	DefaultExcelBPSImageBudgetMiB    = 512
	DefaultExcelBPSImageMaxRequests  = 32
)

type ExcelBPSImageRelaySettings struct {
	Enabled      bool
	BaseURL      string
	BodyLimitMiB int
	BudgetMiB    int
	MaxRequests  int
}

func normalizeExcelBPSImageRelaySettings(enabled bool, baseURL string) (ExcelBPSImageRelaySettings, error) {
	baseURL = strings.TrimSpace(baseURL)
	if enabled || baseURL != "" {
		if err := basispoints.ValidateImageRelayOrigin(baseURL); err != nil {
			return ExcelBPSImageRelaySettings{}, infraerrors.BadRequest("INVALID_EXCEL_BPS_IMAGE_BASE_URL", err.Error())
		}
	}
	return ExcelBPSImageRelaySettings{
		Enabled: enabled, BaseURL: strings.TrimRight(baseURL, "/"),
		BodyLimitMiB: DefaultExcelBPSImageBodyLimitMiB,
		BudgetMiB:    DefaultExcelBPSImageBudgetMiB,
		MaxRequests:  DefaultExcelBPSImageMaxRequests,
	}, nil
}

func validateExcelBPSImageCapacity(bodyLimitMiB, budgetMiB, maxRequests int) error {
	if bodyLimitMiB < 1 || bodyLimitMiB > 128 {
		return infraerrors.BadRequest("INVALID_EXCEL_BPS_IMAGE_CAPACITY", "Image request body limit must be 1-128 MiB")
	}
	if budgetMiB < 512 || budgetMiB > 2048 || budgetMiB < bodyLimitMiB*8 {
		return infraerrors.BadRequest("INVALID_EXCEL_BPS_IMAGE_CAPACITY", "Image request budget must be 512-2048 MiB and at least eight times the body limit")
	}
	if maxRequests < 1 || maxRequests > 128 {
		return infraerrors.BadRequest("INVALID_EXCEL_BPS_IMAGE_CAPACITY", "Image concurrent requests must be 1-128")
	}
	return nil
}

func parseExcelBPSImageCapacity(value string, fallback int) (int, error) {
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid image relay capacity setting: %w", err)
	}
	return parsed, nil
}

// Read current settings for each request so saves take effect immediately,
// including on other instances sharing the settings database.
func (s *SettingService) GetExcelBPSImageRelaySettings(ctx context.Context) (ExcelBPSImageRelaySettings, error) {
	if s == nil || s.settingRepo == nil {
		return ExcelBPSImageRelaySettings{}, nil
	}
	dbCtx, cancel := context.WithTimeout(ctx, gatewayForwardingDBTimeout)
	defer cancel()
	values, err := s.settingRepo.GetMultiple(dbCtx, []string{
		SettingKeyExcelBPSImageRelayEnabled, SettingKeyExcelBPSImageBaseURL,
		SettingKeyExcelBPSImageBodyLimitMiB, SettingKeyExcelBPSImageBudgetMiB, SettingKeyExcelBPSImageMaxRequests,
	})
	if err != nil {
		return ExcelBPSImageRelaySettings{}, infraerrors.ServiceUnavailable("EXCEL_BPS_IMAGE_SETTINGS_UNAVAILABLE", "Excel BPS image settings are unavailable")
	}
	settings, err := normalizeExcelBPSImageRelaySettings(values[SettingKeyExcelBPSImageRelayEnabled] == "true", values[SettingKeyExcelBPSImageBaseURL])
	if err != nil {
		return ExcelBPSImageRelaySettings{}, err
	}
	settings.BodyLimitMiB, err = parseExcelBPSImageCapacity(values[SettingKeyExcelBPSImageBodyLimitMiB], DefaultExcelBPSImageBodyLimitMiB)
	if err == nil {
		settings.BudgetMiB, err = parseExcelBPSImageCapacity(values[SettingKeyExcelBPSImageBudgetMiB], DefaultExcelBPSImageBudgetMiB)
	}
	if err == nil {
		settings.MaxRequests, err = parseExcelBPSImageCapacity(values[SettingKeyExcelBPSImageMaxRequests], DefaultExcelBPSImageMaxRequests)
	}
	if err != nil || validateExcelBPSImageCapacity(settings.BodyLimitMiB, settings.BudgetMiB, settings.MaxRequests) != nil {
		return ExcelBPSImageRelaySettings{}, infraerrors.ServiceUnavailable("EXCEL_BPS_IMAGE_SETTINGS_UNAVAILABLE", "Excel BPS image settings are unavailable")
	}
	return settings, nil
}
