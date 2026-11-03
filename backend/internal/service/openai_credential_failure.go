package service

import (
	"context"
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
)

const (
	OpenAICredentialReasonReauthRequired     GatewayFailureReason = "openai_oauth_reauth_required"
	OpenAICredentialUnavailableClientMessage                      = "No healthy OpenAI OAuth account is currently available"
)

// Credential acquisition precedes inference. A rejected refresh token must
// enter the same bounded account failover loop as an upstream auth failure,
// without attributing the token endpoint's status or proxy to inference.
func openAICredentialFailoverError(ctx context.Context, c *gin.Context, account *Account, err error) error {
	if err == nil || account == nil || !account.IsOpenAIOAuth() {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var rejection *openai.RefreshTokenError
	if !errors.As(err, &rejection) || !rejection.Permanent {
		return err
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:   nil,
		ProxyName: opsProxyNameUnknown,
		Platform:  PlatformOpenAI,
		AccountID: account.ID,
		Stage:     string(GatewayFailureStageAccountAuth),
		Scope:     string(GatewayFailureScopeAccount),
		Reason:    string(OpenAICredentialReasonReauthRequired),
		Kind:      "credential_failover",
		Message:   rejection.Error(),
	})
	return &UpstreamFailoverError{
		Stage:             GatewayFailureStageAccountAuth,
		Scope:             GatewayFailureScopeAccount,
		Reason:            OpenAICredentialReasonReauthRequired,
		NextAccountAction: NextAccountRetry,
		ClientStatusCode:  http.StatusServiceUnavailable,
		ClientMessage:     OpenAICredentialUnavailableClientMessage,
	}
}
