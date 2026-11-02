package service

import (
	"log/slog"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const requestIntegrityModeKey = "request_integrity_mode"

// RequestIntegrityMode is explicitly configured and defaults to off.
// Retired protection markers never enable request validation.
func (a *Account) RequestIntegrityMode() string {
	if a == nil || a.Platform != PlatformOpenAI {
		return "off"
	}
	if mode, ok := a.Extra[requestIntegrityModeKey].(string); ok {
		switch mode {
		case "off", "observe", "enforce":
			return mode
		}
	}
	return "off"
}

func validateRequestIntegrityExtra(extra map[string]any) error {
	value, present := extra[requestIntegrityModeKey]
	if !present || value == nil {
		return nil
	}
	if mode, ok := value.(string); ok && (mode == "off" || mode == "observe" || mode == "enforce") {
		return nil
	}
	return infraerrors.BadRequest("REQUEST_INTEGRITY_INVALID", "请求完整性模式必须为关闭、仅观察或严格拦截")
}

func validateRequestIntegrityForAccount(a *Account, original, forwarded []byte) error {
	if a == nil || len(forwarded) == 0 {
		return nil
	}
	if !gjson.ValidBytes(forwarded) {
		return infraerrors.BadRequest("REQUEST_INTEGRITY_INVALID", "转发请求不是有效 JSON")
	}
	if a.UsesOpenAICodexProtocol() {
		instructions := gjson.GetBytes(forwarded, "instructions").String()
		if instructions != "" && len(original) > 0 {
			if gjson.GetBytes(original, "instructions").String() != instructions {
				return infraerrors.BadRequest("REQUEST_INTEGRITY_MISMATCH", "instructions 与入站请求不一致")
			}
		}
	}
	return nil
}

func checkAccountRequestIntegrity(c *gin.Context, a *Account, original, forwarded []byte) error {
	mode := a.RequestIntegrityMode()
	if mode == "off" {
		return nil
	}
	err := validateRequestIntegrityForAccount(a, original, forwarded)
	if err == nil {
		return nil
	}
	if c != nil {
		c.Set("request_integrity_difference", true)
	}
	slog.Warn("account_request_integrity_difference", "account_id", a.ID, "mode", mode, "error", err.Error())
	if mode == "enforce" {
		return err
	}
	return nil
}
