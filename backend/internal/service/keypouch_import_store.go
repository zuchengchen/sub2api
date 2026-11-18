package service

import (
	"context"
	"time"
)

// KeyPouchImportJob only holds workflow data. Pending OAuth material is sealed
// with the deployment's SecretEncryptor. Completed jobs retain membership IDs.
type KeyPouchImportJob struct {
	ID, Email, Phase, SealedSession, Attempt, Failure string
	ExpiresAt                                         time.Time
	AccountID, GroupID                                int64
}

type KeyPouchIdentity struct {
	AccountID              int64
	Email, Workspace, User string
	Member                 bool
}

// All methods after WithJob run in its transaction. The callback's changes to
// the job commit with the account/group changes, or roll back together.
type KeyPouchImportStore interface {
	WithJob(ctx context.Context, id, email string, run func(context.Context, *KeyPouchImportJob) error) error
	TargetGroup(ctx context.Context) (int64, error)
	FindIdentity(ctx context.Context, email, workspace, user string, groupID int64) (*KeyPouchIdentity, error)
	ConfirmMembership(ctx context.Context, accountID, groupID int64, appendGroup bool) error
}

// KeyPouchError carries only a fixed, safe identifier across the local API.
type KeyPouchError string

func (e KeyPouchError) Error() string { return string(e) }

const (
	KeyPouchBadRequest        KeyPouchError = "invalid_request"
	KeyPouchDisabled          KeyPouchError = "import_not_configured"
	KeyPouchUnauthorized      KeyPouchError = "unauthorized"
	KeyPouchUnknownJob        KeyPouchError = "job_not_found"
	KeyPouchEmailConflict     KeyPouchError = "job_email_conflict"
	KeyPouchGroupUnavailable  KeyPouchError = "target_group_unavailable"
	KeyPouchGroupConflict     KeyPouchError = "target_group_ambiguous"
	KeyPouchIdentityConflict  KeyPouchError = "identity_conflict"
	KeyPouchIdentityMismatch  KeyPouchError = "identity_mismatch"
	KeyPouchUnverified        KeyPouchError = "identity_unverified"
	KeyPouchMembershipChanged KeyPouchError = "membership_changed"
	KeyPouchOAuthMissing      KeyPouchError = "oauth_not_configured"
	KeyPouchStateMismatch     KeyPouchError = "state_mismatch"
	KeyPouchExpired           KeyPouchError = "authorization_expired"
	KeyPouchExchanging        KeyPouchError = "import_in_progress"
	KeyPouchExchangeFailed    KeyPouchError = "oauth_exchange_failed"
	KeyPouchUnavailable       KeyPouchError = "import_unavailable"
)
