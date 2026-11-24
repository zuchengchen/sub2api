package admin

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type scopedAccountReader interface {
	GetAccount(context.Context, int64) (*service.Account, error)
}

func scopedAccountResource(a *service.Account) adminaccess.Account {
	return adminaccess.Account{ID: a.ID, GroupIDs: a.GroupIDs, Platform: a.Platform, Type: a.Type}
}

// 仅校验当前操作涉及的账号。
func scopedAuthorizedAccount(c *gin.Context, reader scopedAccountReader, id int64, action string) (*service.Account, bool) {
	p := adminaccess.FromContext(c.Request.Context())
	if p == nil {
		return nil, true
	}
	if reader == nil || id <= 0 {
		response.Error(c, 404, "账号不存在")
		return nil, false
	}
	a, err := reader.GetAccount(c.Request.Context(), id)
	if err != nil || a == nil || !p.Policy.AllowsAccount(action, scopedAccountResource(a)) {
		response.Error(c, 404, "账号不存在")
		return nil, false
	}
	return a, true
}
