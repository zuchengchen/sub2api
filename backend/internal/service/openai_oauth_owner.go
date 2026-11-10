package service

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
)

// An OAuth workspace may have several different authenticated users. Keep the
// owner separate from token revisions so a refresh can retain its conversation
// while a reauthorization cannot borrow the previous user's request state.
type openAIOAuthOwner struct {
	accountID          int64
	platform           string
	kind               string
	workspace          string
	user               string
	authMode           string
	legacyAuthMode     string
	unknownCredentials [sha256.Size]byte
}

func openAIOAuthOwnerForAccount(account *Account) openAIOAuthOwner {
	if account == nil || !account.IsOpenAIOAuth() {
		return openAIOAuthOwner{}
	}
	owner := openAIOAuthOwner{accountID: account.ID, platform: account.Platform, kind: account.Type,
		workspace: account.GetChatGPTAccountID(), user: account.GetChatGPTUserID(),
		authMode: account.GetCredential(openAIAuthModeCredentialKey), legacyAuthMode: account.GetCredential(openAIAuthModeLegacyCredentialKey)}
	if strings.TrimSpace(owner.workspace) == "" || strings.TrimSpace(owner.user) == "" {
		hash := sha256.New()
		var size [8]byte
		for _, value := range [...]string{account.GetOpenAIAccessToken(), account.GetOpenAIRefreshToken(), account.GetCredential("id_token")} {
			binary.BigEndian.PutUint64(size[:], uint64(len(value)))
			_, _ = hash.Write(size[:])
			_, _ = hash.Write([]byte(value))
		}
		hash.Sum(owner.unknownCredentials[:0])
	}
	return owner
}

func sameOpenAIOAuthOwner(expected, actual *Account) bool {
	return expected != nil && actual != nil && expected.IsOpenAIOAuth() && actual.IsOpenAIOAuth() &&
		openAIOAuthOwnerForAccount(expected) == openAIOAuthOwnerForAccount(actual)
}
