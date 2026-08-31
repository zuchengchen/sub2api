package admin

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

type updateGuideRequest struct {
	Chapters        []service.GuideChapter `json:"chapters"`
	ExpectedVersion *int                   `json:"expected_version"`
}

type restoreGuideRequest struct {
	RevisionVersion int  `json:"revision_version"`
	ExpectedVersion *int `json:"expected_version"`
}

type resetGuideRequest struct {
	ExpectedVersion *int `json:"expected_version"`
}

// GetGuide returns the published content and the bounded revision history.
func (h *SettingHandler) GetGuide(c *gin.Context) {
	settings, err := h.settingService.GetGuideSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

// UpdateGuide publishes edited Markdown as a new version.
func (h *SettingHandler) UpdateGuide(c *gin.Context) {
	var req updateGuideRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request body")
		return
	}
	if req.ExpectedVersion == nil {
		response.BadRequest(c, "expected_version is required")
		return
	}

	settings, err := h.settingService.SaveGuideSettings(c.Request.Context(), req.Chapters, *req.ExpectedVersion)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

// RestoreGuide republishes a selected snapshot as a new version.
func (h *SettingHandler) RestoreGuide(c *gin.Context) {
	var req restoreGuideRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request body")
		return
	}
	if req.ExpectedVersion == nil {
		response.BadRequest(c, "expected_version is required")
		return
	}
	if req.RevisionVersion <= 0 {
		response.Error(c, http.StatusBadRequest, "revision_version must be positive")
		return
	}

	settings, err := h.settingService.RestoreGuideSettings(c.Request.Context(), req.RevisionVersion, *req.ExpectedVersion)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

// ResetGuide switches back to the tutorial bundled with the current release.
func (h *SettingHandler) ResetGuide(c *gin.Context) {
	var req resetGuideRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request body")
		return
	}
	if req.ExpectedVersion == nil {
		response.BadRequest(c, "expected_version is required")
		return
	}

	settings, err := h.settingService.ResetGuideSettings(c.Request.Context(), *req.ExpectedVersion)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}
