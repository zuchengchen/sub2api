package middleware

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestTiboHealthyGroupModelHTTP(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/chat/completions", "/v1/messages", "/v1/embeddings"} {
		for _, model := range []string{"luna", "provider/gpt6luna-high", "openai/GPT5.6LUNA-max"} {
			t.Run(path+"/"+model, func(t *testing.T) {
				key := &service.APIKey{Group: &service.Group{Name: service.TiboHealthyGroupName, Platform: service.PlatformOpenAI}}
				router, calls := newGroupModelAllowlistTestRouter(key, "/v1")
				got := doJSON(t, router, http.MethodPost, path, `{"model":"gpt-6-astra","Model":"`+model+`"}`)
				require.Equal(t, http.StatusNotFound, got.Code)
				require.Empty(t, *calls, "rejection must precede forwarding")
			})
		}
	}
	for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol", "gpt-6.1-terra", "gpt-image-2"} {
		key := &service.APIKey{Group: &service.Group{Name: service.TiboHealthyGroupName, Platform: service.PlatformOpenAI,
			ModelAllowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{"gpt-6-luna"}}}}
		router, calls := newGroupModelAllowlistTestRouter(key, "/v1")
		got := doJSON(t, router, http.MethodPost, "/v1/responses", `{"model":"`+model+`"}`)
		require.Equal(t, http.StatusOK, got.Code, model)
		require.Len(t, *calls, 1)
	}
	key := &service.APIKey{Group: &service.Group{Name: service.TiboWelfareGroupName, Platform: service.PlatformOpenAI}}
	router, calls := newGroupModelAllowlistTestRouter(key, "/v1")
	require.Equal(t, http.StatusOK, doJSON(t, router, http.MethodPost, "/v1/responses", `{"model":"gpt-6-luna"}`).Code)
	require.Len(t, *calls, 1)
}
