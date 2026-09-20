package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const intelligentTestIdentityKey = "intelligent_test_identity"

type intelligentTestIdentity struct {
	sessionID string
	threadID  string
}

type ProtectionRuntimeState struct {
	Strategy        string `json:"strategy,omitempty"`
	IdentityMode    string `json:"identity_mode,omitempty"`
	RequestedModel  string `json:"requested_model,omitempty"`
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	GroupName       string `json:"group_name,omitempty"`
}

func snapshotIntelligentProtectionRuntime(account *Account) *ProtectionRuntimeState {
	if account == nil {
		return &ProtectionRuntimeState{}
	}
	return &ProtectionRuntimeState{
		Strategy:     account.ProtectionMode(),
		IdentityMode: string(account.GetCodexFingerprintMode()),
	}
}

// prepareIntelligentTestProtection reuses the persisted device identity of a
// protected OpenAI account and issues a fresh session/thread per run. It never
// writes extra.
func prepareIntelligentTestProtection(c *gin.Context, account *Account, payload map[string]any) error {
	if c == nil || c.Request == nil || account == nil || !account.AntiDegradationEnabled() {
		return nil
	}
	if !account.IsOpenAIOAuthLike() || account.GetCodexFingerprintMode() == codexFingerprintOff {
		return nil
	}
	if _, err := json.Marshal(payload); err != nil {
		return err
	}
	rawSession := uuid.NewString()
	ids := resolveCodexFingerprintIDs(account, rawSession, account.GetCodexFingerprintMode())
	stageCodexFingerprintIDs(c, ids)
	identity := intelligentTestIdentity{sessionID: rawSession, threadID: uuid.NewString()}
	if ids != nil {
		if ids.sessionID != "" {
			identity.sessionID = ids.sessionID
		}
		if ids.threadID != "" {
			identity.threadID = ids.threadID
		}
	}
	c.Request.Header.Set("session-id", rawSession)
	c.Set(intelligentTestIdentityKey, identity)
	if payload != nil {
		applyStagedCodexFingerprintClientMetadata(c, account, payload)
		if metadata, ok := payload["client_metadata"].(map[string]any); ok {
			metadata["session_id"] = identity.sessionID
			metadata["thread_id"] = identity.threadID
		}
	}
	return nil
}

func applyIntelligentTestProtection(c *gin.Context, account *Account, headers http.Header, payload []byte) error {
	if c == nil {
		return nil
	}
	value, exists := c.Get(intelligentTestIdentityKey)
	identity, ok := value.(intelligentTestIdentity)
	if !exists || !ok {
		return nil
	}
	applyStagedCodexFingerprintHeaders(c, account, headers)
	headers.Set("conversation_id", identity.sessionID)
	headers.Set("thread-id", identity.threadID)
	return checkAccountRequestIntegrity(c, account, nil, payload)
}

var ErrIntelligentAccountBusy = errors.New("account busy")

type TestAdmissionWaitError struct {
	Until  time.Time
	Reason string
}

func (e *TestAdmissionWaitError) Error() string {
	if e == nil || e.Reason == "" {
		return ErrIntelligentAccountBusy.Error()
	}
	return e.Reason
}

func (e *TestAdmissionWaitError) Unwrap() error { return ErrIntelligentAccountBusy }
