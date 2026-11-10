package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// OpenAIResponseAuthOwnerCache is an optional cache capability for the response
// origin, separate from scheduling affinity and downstream HTTP authorization.
// Set is first-writer-wins; its false result requires reading the original value.
type OpenAIResponseAuthOwnerCache interface {
	SetOpenAIResponseAuthOwner(context.Context, string, []byte, time.Duration) (bool, error)
	GetOpenAIResponseAuthOwner(context.Context, string) ([]byte, error)
}

type openAIResponseAuthOwnerBinding struct {
	Owner           string `json:"owner"`
	UserID          int64  `json:"user_id,omitempty"`
	APIKeyID        int64  `json:"api_key_id,omitempty"`
	ExpiresUnixNano int64  `json:"expires_unix_nano"`
}
type openAIResponseAuthOwnerStore interface {
	bindResponseAuthOwner(context.Context, int64, string, openAIResponseAuthOwnerBinding, time.Duration) error
	getResponseAuthOwner(context.Context, int64, string) (openAIResponseAuthOwnerBinding, bool, error)
}

var errOpenAIResponseAuthOwnerMismatch = errors.New("previous_response_id is not available for this credential or user")

func openAIResponseAuthOwnerFingerprint(account *Account) string {
	if account == nil || !account.IsOpenAIOAuth() {
		return ""
	}
	owner := openAIOAuthOwnerForAccount(account)
	h := sha256.New()
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(owner.accountID))
	_, _ = h.Write(size[:])
	for _, part := range []string{owner.platform, owner.kind, owner.workspace, owner.user, owner.authMode, owner.legacyAuthMode} {
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(part))
	}
	_, _ = h.Write(owner.unknownCredentials[:])
	return hex.EncodeToString(h.Sum(nil))
}

func openAIResponseDownstreamOwner(c *gin.Context) (int64, int64) {
	if c == nil {
		return 0, 0
	}
	if raw, ok := c.Get(openAIHTTPResponseOwnerContextKey); ok {
		if owner, ok := raw.(openAIHTTPResponseOwner); ok {
			return owner.userID, owner.apiKeyID
		}
	}
	if key := getAPIKeyFromContext(c); key != nil {
		return key.UserID, key.ID
	}
	return 0, 0
}

// Missing metadata is intentionally not an authorization decision: existing
// HTTP tenant validation and native WS continuation rules still apply. A known
// response origin, however, must never cross credentials or downstream users.
func (s *OpenAIGatewayService) validateOpenAIResponseAuthOwner(ctx context.Context, c *gin.Context, account *Account, responseID string) error {
	if s == nil || account == nil || !account.IsOpenAI() || strings.TrimSpace(responseID) == "" {
		return nil
	}
	store, ok := s.getOpenAIWSStateStore().(openAIResponseAuthOwnerStore)
	if !ok {
		return nil
	}
	owner, found, err := store.getResponseAuthOwner(ctx, getOpenAIGroupIDFromContext(c), responseID)
	if err != nil {
		return fmt.Errorf("response credential origin is unavailable: %w", err)
	}
	if !found {
		return nil
	}
	if owner.Owner != openAIResponseAuthOwnerFingerprint(account) {
		return errOpenAIResponseAuthOwnerMismatch
	}
	userID, apiKeyID := openAIResponseDownstreamOwner(c)
	if owner.UserID > 0 && userID > 0 {
		if owner.UserID != userID {
			return errOpenAIResponseAuthOwnerMismatch
		}
	} else if owner.APIKeyID > 0 && owner.APIKeyID != apiKeyID {
		return errOpenAIResponseAuthOwnerMismatch
	}
	return nil
}

func (s *OpenAIGatewayService) bindOpenAIResponseAuthOwner(ctx context.Context, c *gin.Context, account *Account, responseID string) {
	if s == nil || account == nil || !account.IsOpenAIOAuth() || strings.TrimSpace(responseID) == "" {
		return
	}
	store, ok := s.getOpenAIWSStateStore().(openAIResponseAuthOwnerStore)
	if !ok {
		return
	}
	userID, keyID := openAIResponseDownstreamOwner(c)
	ttl := s.openAIWSResponseStickyTTL()
	binding := openAIResponseAuthOwnerBinding{Owner: openAIResponseAuthOwnerFingerprint(account), UserID: userID, APIKeyID: keyID, ExpiresUnixNano: time.Now().Add(ttl).UnixNano()}
	if ctx == nil {
		ctx = context.Background()
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openAIWSStateStoreRedisTimeout)
	defer cancel()
	err := store.bindResponseAuthOwner(writeCtx, getOpenAIGroupIDFromContext(c), responseID, binding, ttl)
	logOpenAIWSBindResponseAccountWarn(getOpenAIGroupIDFromContext(c), account.ID, responseID, err)
}

func writeOpenAIResponseAuthOwnerError(c *gin.Context, err error) {
	if c == nil {
		return
	}
	status, kind := http.StatusServiceUnavailable, "server_error"
	if errors.Is(err, errOpenAIResponseAuthOwnerMismatch) {
		status, kind = http.StatusBadRequest, "invalid_request_error"
	}
	writeOpenAIResponsesErrorJSON(c, status, gin.H{"error": gin.H{"type": kind, "message": err.Error(), "param": "previous_response_id"}})
}

func openAIResponseAuthOwnerCacheKey(groupID int64, responseID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(responseID)))
	return fmt.Sprintf("%d:%x", groupID, sum)
}

// Publish only a confirmed canonical origin. Pending/failed writes have their
// own local marker, so a subsequent cache miss cannot turn a failed known-origin
// write into permission to treat that response as an unknown native source.
func (s *defaultOpenAIWSStateStore) bindResponseAuthOwner(ctx context.Context, groupID int64, responseID string, binding openAIResponseAuthOwnerBinding, ttl time.Duration) error {
	if strings.TrimSpace(responseID) == "" || binding.Owner == "" {
		return nil
	}
	s.maybeCleanup()
	key := openAIWSResponseAccountMapKey(groupID, strings.TrimSpace(responseID))
	now := time.Now().UnixNano()
	s.responseOwnerMu.Lock()
	if previous, ok := s.responseAuthOwners[key]; ok && previous.ExpiresUnixNano > now {
		s.responseOwnerMu.Unlock()
		return nil
	}
	cache, shared := s.cache.(OpenAIResponseAuthOwnerCache)
	if shared {
		if s.responseAuthOwnerPending == nil {
			s.responseAuthOwnerPending = make(map[string]int64)
		}
		ensureBindingCapacity(s.responseAuthOwnerPending, key, openAIWSStateStoreMaxEntriesPerMap)
		s.responseAuthOwnerPending[key] = binding.ExpiresUnixNano
	}
	s.responseOwnerMu.Unlock()
	canonical := binding
	if shared {
		raw, err := json.Marshal(binding)
		if err != nil {
			return err
		}
		applied, err := cache.SetOpenAIResponseAuthOwner(ctx, openAIResponseAuthOwnerCacheKey(groupID, responseID), raw, normalizeOpenAIWSTTL(ttl))
		if err != nil {
			return err
		}
		if !applied {
			existing, err := cache.GetOpenAIResponseAuthOwner(ctx, openAIResponseAuthOwnerCacheKey(groupID, responseID))
			if err != nil {
				return err
			}
			if len(existing) == 0 || json.Unmarshal(existing, &canonical) != nil || canonical.Owner == "" || canonical.ExpiresUnixNano <= time.Now().UnixNano() {
				return errors.New("cached response credential origin is unavailable")
			}
		}
	}
	s.responseOwnerMu.Lock()
	defer s.responseOwnerMu.Unlock()
	if s.responseAuthOwners == nil {
		s.responseAuthOwners = make(map[string]openAIResponseAuthOwnerBinding)
	}
	// Without a shared cache, another goroutine may have published first while
	// this one computed its value. Keep that first confirmed origin immutable.
	if previous, ok := s.responseAuthOwners[key]; !shared && ok && previous.ExpiresUnixNano > time.Now().UnixNano() {
		return nil
	}
	ensureBindingCapacity(s.responseAuthOwners, key, openAIWSStateStoreMaxEntriesPerMap)
	s.responseAuthOwners[key] = canonical
	delete(s.responseAuthOwnerPending, key)
	return nil
}
func (s *defaultOpenAIWSStateStore) getResponseAuthOwner(ctx context.Context, groupID int64, responseID string) (openAIResponseAuthOwnerBinding, bool, error) {
	empty := openAIResponseAuthOwnerBinding{}
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return empty, false, nil
	}
	s.maybeCleanup()
	key := openAIWSResponseAccountMapKey(groupID, responseID)
	now := time.Now().UnixNano()
	s.responseOwnerMu.RLock()
	binding, found := s.responseAuthOwners[key]
	pendingUntil := s.responseAuthOwnerPending[key]
	s.responseOwnerMu.RUnlock()
	if found && binding.ExpiresUnixNano > now {
		return binding, true, nil
	}
	cache, ok := s.cache.(OpenAIResponseAuthOwnerCache)
	if !ok {
		if pendingUntil > now {
			return empty, false, errors.New("response credential origin is unavailable")
		}
		return empty, false, nil
	}
	readCtx, cancel := withOpenAIWSStateStoreRedisTimeout(ctx)
	defer cancel()
	raw, err := cache.GetOpenAIResponseAuthOwner(readCtx, openAIResponseAuthOwnerCacheKey(groupID, responseID))
	if err != nil {
		return empty, false, err
	}
	if len(raw) == 0 {
		if pendingUntil > now {
			return empty, false, errors.New("response credential origin is unavailable")
		}
		return empty, false, nil
	}
	if err = json.Unmarshal(raw, &binding); err != nil || binding.Owner == "" {
		return empty, false, errors.New("invalid cached response credential origin")
	}
	if binding.ExpiresUnixNano <= now {
		if pendingUntil > now {
			return empty, false, errors.New("response credential origin is unavailable")
		}
		return empty, false, nil
	}
	s.responseOwnerMu.Lock()
	if s.responseAuthOwners == nil {
		s.responseAuthOwners = make(map[string]openAIResponseAuthOwnerBinding)
	}
	ensureBindingCapacity(s.responseAuthOwners, key, openAIWSStateStoreMaxEntriesPerMap)
	s.responseAuthOwners[key] = binding
	delete(s.responseAuthOwnerPending, key)
	s.responseOwnerMu.Unlock()
	return binding, true, nil
}
func cleanupExpiredResponseAuthOwners(bindings map[string]openAIResponseAuthOwnerBinding, now time.Time, maxScan int) {
	scanned := 0
	for key, binding := range bindings {
		if binding.ExpiresUnixNano <= now.UnixNano() {
			delete(bindings, key)
		}
		scanned++
		if scanned >= maxScan {
			break
		}
	}
}

func cleanupExpiredResponseAuthOwnerPending(pending map[string]int64, now time.Time, maxScan int) {
	scanned := 0
	for key, expires := range pending {
		if expires <= now.UnixNano() {
			delete(pending, key)
		}
		scanned++
		if scanned >= maxScan {
			break
		}
	}
}

func successfulOpenAIResponseAuthOwnerTerminal(event string) bool {
	return event == "response.completed" || event == "response.done"
}
