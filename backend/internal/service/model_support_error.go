package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrModelNotSupportedByAccounts marks the first selection attempt where the
// configured account pool is otherwise routable, but no account supports the
// requested public model.
var ErrModelNotSupportedByAccounts = errors.New("model not supported by available accounts")

type ModelNotSupportedByAccountsError struct {
	RequestedModel string
}

func (e *ModelNotSupportedByAccountsError) Error() string {
	model := strings.TrimSpace(e.RequestedModel)
	if model == "" {
		return ErrNoAvailableAccounts.Error()
	}
	return fmt.Sprintf("%s: %s", ErrModelNotSupportedByAccounts.Error(), model)
}

func (e *ModelNotSupportedByAccountsError) Is(target error) bool {
	return target == ErrNoAvailableAccounts || target == ErrModelNotSupportedByAccounts
}

func (e *ModelNotSupportedByAccountsError) Unwrap() error {
	return ErrNoAvailableAccounts
}

func newModelNotSupportedByAccountsError(requestedModel string) error {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return ErrNoAvailableAccounts
	}
	return &ModelNotSupportedByAccountsError{RequestedModel: requestedModel}
}

func ModelNotSupportedRequestedModel(err error) (string, bool) {
	var modelErr *ModelNotSupportedByAccountsError
	if !errors.As(err, &modelErr) || modelErr == nil {
		return "", false
	}
	model := strings.TrimSpace(modelErr.RequestedModel)
	return model, model != ""
}

type publicModelSupportMiss404ContextKey struct{}

func WithPublicModelSupportMiss404(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, publicModelSupportMiss404ContextKey{}, true)
}

func publicModelSupportMiss404Enabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, _ := ctx.Value(publicModelSupportMiss404ContextKey{}).(bool)
	return enabled
}
