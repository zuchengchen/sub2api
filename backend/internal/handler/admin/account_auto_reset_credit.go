package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

type SetOpenAIAutoResetCreditEnabledRequest struct {
	Enabled *bool `json:"enabled" binding:"required"`
}

// SetOpenAIAutoResetCreditEnabled toggles automatic reset credits without
// replacing the account's thresholds or provider runtime state.
// POST /api/v1/admin/accounts/:id/auto-reset-credit-enabled
func (h *AccountHandler) SetOpenAIAutoResetCreditEnabled(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var req SetOpenAIAutoResetCreditEnabledRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	account, err := h.adminService.SetOpenAIAutoResetCreditEnabled(c.Request.Context(), accountID, *req.Enabled)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, h.buildAccountResponseWithRuntime(c.Request.Context(), account))
}
