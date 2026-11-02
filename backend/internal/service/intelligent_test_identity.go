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

type IntelligentTestRuntimeState struct {
	// Strategy is retained only to decode historical snapshots; new runs leave it empty.
	Strategy        string `json:"strategy,omitempty"`
	EffectiveTLS    string `json:"effective_tls,omitempty"`
	TLSReason       string `json:"tls_reason,omitempty"`
	Concurrency     *int   `json:"concurrency,omitempty"`
	IntegrityMode   string `json:"integrity_mode,omitempty"`
	IdentityMode    string `json:"identity_mode,omitempty"`
	RequestedModel  string `json:"requested_model,omitempty"`
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	GroupName       string `json:"group_name,omitempty"`
}

func snapshotIntelligentRuntime(account *Account) *IntelligentTestRuntimeState {
	if account == nil {
		return &IntelligentTestRuntimeState{}
	}
	concurrency := account.Concurrency
	return &IntelligentTestRuntimeState{
		IdentityMode:  string(account.GetCodexFingerprintMode()),
		Concurrency:   &concurrency,
		IntegrityMode: account.RequestIntegrityMode(),
	}
}

// prepareIntelligentTestIdentity follows the independent fingerprint setting
// and issues a fresh session/thread per run without writing account extra.
func prepareIntelligentTestIdentity(c *gin.Context, account *Account, payload map[string]any) error {
	if c == nil || c.Request == nil || account == nil {
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

func applyIntelligentTestIdentity(c *gin.Context, account *Account, headers http.Header, payload []byte) error {
	if c != nil {
		value, exists := c.Get(intelligentTestIdentityKey)
		identity, ok := value.(intelligentTestIdentity)
		if exists && ok {
			applyStagedCodexFingerprintHeaders(c, account, headers)
			applyCodexFingerprintConvergenceHeaders(c, codexAccountIdentitySource(c, account), headers)
			headers.Set("conversation_id", identity.sessionID)
			headers.Set("thread-id", identity.threadID)
		}
	}
	// Integrity is explicit and independent of whether fingerprint headers were staged.
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
