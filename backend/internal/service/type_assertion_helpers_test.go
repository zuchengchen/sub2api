package service

import (
	"reflect"
	"testing"
)

func mustTestValue[T any](t *testing.T, value any) T {
	t.Helper()
	result, ok := value.(T)
	if !ok {
		t.Fatalf("expected %v, got %T", reflect.TypeFor[T](), value)
	}
	return result
}
