package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"

	"github.com/gin-gonic/gin"
)

// GetPublicGuide returns only the currently published guide. Revision content
// remains available exclusively through the authenticated admin API.
func (h *SettingHandler) GetPublicGuide(c *gin.Context) {
	settings, err := h.settingService.GetGuideSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, struct {
		Content          string `json:"content"`
		Version          int    `json:"version"`
		UpdatedAt        string `json:"updated_at"`
		HasCustomContent bool   `json:"has_custom_content"`
	}{
		Content:          settings.Content,
		Version:          settings.Version,
		UpdatedAt:        settings.UpdatedAt,
		HasCustomContent: settings.HasCustomContent,
	})
}
