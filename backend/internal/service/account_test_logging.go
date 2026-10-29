package service

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Bind attribution before lookup so missing accounts are identifiable too. The
// request logger retains HTTP request IDs.
func initAccountTestLogger(c *gin.Context, accountID int64, modelID, mode string) {
	ctx := c.Request.Context()
	l := logger.FromContext(ctx).With(
		zap.String("component", "service.account_test"),
		zap.Int64("account_id", accountID),
		zap.String("test_id", uuid.NewString()),
		zap.String("test_source", "http"),
		zap.String("requested_model", modelID),
		zap.String("test_mode", normalizeAccountTestMode(mode)),
	)
	c.Request = c.Request.WithContext(logger.IntoContext(ctx, l))
}

func bindAccountTestPlatform(c *gin.Context, account *Account) {
	ctx := c.Request.Context()
	l := logger.FromContext(ctx).With(
		zap.String("platform", account.Platform),
		zap.String("account_type", account.Type),
	)
	c.Request = c.Request.WithContext(logger.IntoContext(ctx, l))
}

func logAccountTestError(c *gin.Context, message string) {
	// Redact known credential fields before bounding text; truncating first can
	// cut a quoted credential in half and prevent the redactor from matching it.
	message = logredact.RedactText(message, "api_key", "authorization")
	logger.FromContext(c.Request.Context()).Warn("account_test.failed",
		zap.String("error", truncateString(message, 2048)),
	)
}
