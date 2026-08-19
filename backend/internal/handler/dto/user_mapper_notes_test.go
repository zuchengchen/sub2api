package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUserMappersKeepAdminNotesOutOfUserDTO(t *testing.T) {
	user := &service.User{
		ID:       7,
		Username: "self-managed-name",
		Notes:    "internal admin note",
	}

	userDTO, err := json.Marshal(UserFromService(user))
	require.NoError(t, err)
	require.Contains(t, string(userDTO), `"username":"self-managed-name"`)
	require.NotContains(t, string(userDTO), `"notes"`)

	adminDTO, err := json.Marshal(UserFromServiceAdmin(user))
	require.NoError(t, err)
	require.Contains(t, string(adminDTO), `"notes":"internal admin note"`)
}
