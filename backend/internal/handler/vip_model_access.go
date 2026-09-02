package handler

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type modelAccessErrorWriter func(c *gin.Context, status int, errType, message string)

// requireUserModelAccess enforces user-level model policy before account
// selection. The resolved composite model is checked as well as the public
// request model so an alias cannot expose a VIP-only upstream model.
func requireUserModelAccess(c *gin.Context, apiKey *service.APIKey, writeError modelAccessErrorWriter, models ...string) bool {
	if apiKeyCanAccessModels(c, apiKey, models...) {
		return true
	}
	service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalPolicyDenied)
	writeError(c, http.StatusForbidden, "permission_error", service.VipExclusiveModelAccessMessage)
	return false
}

// requireUserAccountModelAccess checks the model that the selected account
// will actually send upstream. OpenAI accounts use the same mapping chain as
// their forwarder, including compact_model_mapping; other platforms use their
// normal account model mapping.
func requireUserAccountModelAccess(
	c *gin.Context,
	apiKey *service.APIKey,
	account *service.Account,
	writeError modelAccessErrorWriter,
	requireCompact bool,
	models ...string,
) bool {
	if apiKeyCanAccessAccountModels(c, apiKey, account, requireCompact, models...) {
		return true
	}
	service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalPolicyDenied)
	writeError(c, http.StatusForbidden, "permission_error", service.VipExclusiveModelAccessMessage)
	return false
}

func apiKeyCanAccessModels(c *gin.Context, apiKey *service.APIKey, models ...string) bool {
	var user *service.User
	if apiKey != nil {
		user = apiKey.User
	}
	for _, model := range models {
		if !service.UserCanAccessModel(user, model) {
			return false
		}
	}
	if c != nil && c.Request != nil {
		if model, ok := service.ResolvedUpstreamModelFromContext(c.Request.Context()); ok && !service.UserCanAccessModel(user, model) {
			return false
		}
	}
	return true
}

func apiKeyCanAccessAccountModels(
	c *gin.Context,
	apiKey *service.APIKey,
	account *service.Account,
	requireCompact bool,
	models ...string,
) bool {
	candidates := make([]string, 0, len(models)*2)
	for _, model := range models {
		candidates = append(candidates, model)
		if account == nil {
			continue
		}
		candidates = append(candidates, account.GetMappedModel(model))
		if account.IsOpenAI() {
			candidates = append(candidates, service.ResolveOpenAIAccountUpstreamModelForRequest(account, model, requireCompact))
		}
	}
	return apiKeyCanAccessModels(c, apiKey, candidates...)
}
