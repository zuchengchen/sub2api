package service

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type scopedAccountOperationKey struct{}

// WithScopedAdminAccountOperation 仅供已完成动作验权的处理器传递同一操作的后续查询。
// 例如重置后的额度刷新仍属于重置操作，不要求额外授予读取权限。
func WithScopedAdminAccountOperation(ctx context.Context, action string) context.Context {
	return context.WithValue(ctx, scopedAccountOperationKey{}, action)
}
func scopedAccountOperation(ctx context.Context, fallback string) string {
	if action, ok := ctx.Value(scopedAccountOperationKey{}).(string); ok && action != "" {
		return action
	}
	return fallback
}

func scopedAccountAccess(ctx context.Context, a *Account, action string) error {
	p := adminaccess.FromContext(ctx)
	if p == nil {
		return nil
	}
	denied := func() error { return infraerrors.NotFound("SCOPED_ACCOUNT_NOT_FOUND", "账号不存在") }
	if a == nil || !p.Policy.AllowsAccount(action, scopedAccountResource(a)) {
		return denied()
	}
	return nil
}
func scopedAccountIDAccess(ctx context.Context, repo AccountRepository, id int64, action string) error {
	if adminaccess.FromContext(ctx) == nil {
		return nil
	}
	if repo == nil {
		return infraerrors.NotFound("SCOPED_ACCOUNT_NOT_FOUND", "账号不存在")
	}
	a, err := repo.GetByID(ctx, id)
	if err != nil {
		return infraerrors.NotFound("SCOPED_ACCOUNT_NOT_FOUND", "账号不存在")
	}
	return scopedAccountAccess(ctx, a, action)
}
