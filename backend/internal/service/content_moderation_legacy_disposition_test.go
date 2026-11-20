//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCyberPolicyRevokesKeysWithLegacyRepositories(t *testing.T) {
	for _, tc := range []struct {
		name            string
		withDisposition bool
	}{
		{"two-method disposition repository", true},
		{"user and API key repositories only", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const userID int64 = 7
			key := APIKey{ID: 12, UserID: userID, Status: StatusActive, Key: "test-key"}
			keys := &apiKeyRepoStub{
				apiKey:               &key,
				allowListAllByUserID: true,
				listAllByUserIDKeys: []APIKey{
					key,
					{ID: 13, UserID: userID, Status: "inactive"},
					{ID: 14, UserID: 8, Status: StatusActive},
				},
			}
			users := &contentModerationTestUserRepo{user: &User{ID: userID, Role: RoleUser, Status: StatusActive}}
			var repo ContentModerationRepository = &contentModerationTestRepo{}
			old := &cyberDispositionTestRepo{userActive: true, keyActive: true, keyCredential: key.Key}
			if tc.withDisposition {
				repo = struct {
					ContentModerationRepository
					ContentModerationDispositionRepository
				}{repo, old}
			}
			cache := &contentModerationTestAuthCacheInvalidator{}
			svc := NewContentModerationService(nil, repo, nil, nil, users, nil, cache, nil)
			svc.apiKeyRepo = keys
			svc.conversationTerminator = nil

			changed, err := svc.applyCyberPolicyDisposition(context.Background(), CyberPolicyRecordInput{UserID: userID}, &ContentModerationLog{})
			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, []int64{userID}, keys.listAllByUserIDCalls)
			require.Equal(t, []APIKeyListFilters{{Status: StatusActive}}, keys.listAllByUserIDFilters)
			require.Equal(t, []string{key.Key}, cache.keys)
			require.Equal(t, []int64{userID}, cache.userIDs)
			if tc.withDisposition {
				require.Equal(t, 1, old.disableUserCalls)
				require.Equal(t, 1, old.disableKeyCalls)
				require.Empty(t, users.updated)
				require.Empty(t, keys.updatedKeys)
			} else {
				require.Equal(t, StatusDisabled, users.user.Status)
				require.Len(t, keys.updatedKeys, 1)
				require.Equal(t, key.ID, keys.updatedKeys[0].ID)
				require.Equal(t, StatusAPIKeyDisabled, keys.updatedKeys[0].Status)
			}
		})
	}
}
