package handler

import "github.com/gin-gonic/gin"

func (h *OpenAIGatewayHandler) ExcelBPSImage(c *gin.Context) {
	h.gatewayService.ServeExcelBPSImage(c)
}
