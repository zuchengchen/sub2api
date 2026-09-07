package admin

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSetVIPGrantsAndCancels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	adminSvc := newStubAdminService()
	h := NewUserHandler(adminSvc, nil, nil, nil, nil, nil, nil)
	router.POST("/api/v1/admin/users/:id/vip", h.SetVIP)

	grant := doJSON(t, router, http.MethodPost, "/api/v1/admin/users/7/vip", map[string]any{"vip": true})
	require.Equal(t, http.StatusOK, grant.Code)
	require.True(t, gjson.Get(grant.Body.String(), "data.is_vip").Bool())

	cancel := doJSON(t, router, http.MethodPost, "/api/v1/admin/users/7/vip", map[string]any{"vip": false})
	require.Equal(t, http.StatusOK, cancel.Code)
	require.False(t, gjson.Get(cancel.Body.String(), "data.is_vip").Bool())
}

func TestSetVIPRejectsInvalidUserID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	h := NewUserHandler(newStubAdminService(), nil, nil, nil, nil, nil, nil)
	router.POST("/api/v1/admin/users/:id/vip", h.SetVIP)

	rec := doJSON(t, router, http.MethodPost, "/api/v1/admin/users/abc/vip", map[string]any{"vip": true})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}
