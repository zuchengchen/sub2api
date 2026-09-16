//go:build unit

package handler

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPIKeyCreateRequestConcurrencyDefaultsUnlimited(t *testing.T) {
	req := CreateAPIKeyRequest{}
	require.Equal(t, 0, req.Concurrency)
	require.NoError(t, validateAPIKeyCreateRequest(req))
}

func TestAPIKeyUpdateRequestConcurrencyAllowsZeroAndPositive(t *testing.T) {
	zero := 0
	positive := 7
	require.NoError(t, validateAPIKeyUpdateRequest(UpdateAPIKeyRequest{Concurrency: &zero}))
	require.NoError(t, validateAPIKeyUpdateRequest(UpdateAPIKeyRequest{Concurrency: &positive}))
}
