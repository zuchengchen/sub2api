package admin

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// UploadGuideAsset stores a file for use in the usage guide. Any file type is
// accepted; the public download route decides whether it may render inline.
func (h *SettingHandler) UploadGuideAsset(c *gin.Context) {
	if h.guideAssets == nil {
		response.Error(c, http.StatusServiceUnavailable, "guide uploads are not configured")
		return
	}

	// Cap the request body before parsing so an oversized upload is rejected
	// without buffering it. The extra megabyte covers multipart framing.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, service.GuideAssetMaxBytes+(1<<20))

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		response.BadRequest(c, "请选择要上传的文件")
		return
	}
	defer func() { _ = file.Close() }()

	asset, err := h.guideAssets.Save(c.Request.Context(), header.Filename, header.Size, file)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, asset)
}

// ListGuideAssets returns the uploaded files, newest first.
func (h *SettingHandler) ListGuideAssets(c *gin.Context) {
	if h.guideAssets == nil {
		response.Success(c, []service.GuideAsset{})
		return
	}

	assets, err := h.guideAssets.List(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, assets)
}

// DeleteGuideAsset removes an uploaded file. Chapters referencing it are left
// untouched, so the admin decides whether to edit the surrounding text.
func (h *SettingHandler) DeleteGuideAsset(c *gin.Context) {
	if h.guideAssets == nil {
		response.Error(c, http.StatusServiceUnavailable, "guide uploads are not configured")
		return
	}

	if err := h.guideAssets.Delete(c.Request.Context(), c.Param("id")); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": true})
}
