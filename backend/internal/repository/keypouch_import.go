package repository

import (
	"context"
	"errors"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type keyPouchImportStore struct{ client *dbent.Client }

func NewKeyPouchImportStore(client *dbent.Client) service.KeyPouchImportStore {
	return &keyPouchImportStore{client: client}
}

func (s *keyPouchImportStore) WithJob(ctx context.Context, id, email string, run func(context.Context, *service.KeyPouchImportJob) error) error {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	db := tx.Client()
	// Job locks bind a job to a single email. The separate email lock prevents
	// two independently authorized jobs from creating the same account.
	for _, key := range []string{"keypouch:job:" + id, "keypouch:email:" + email} {
		if _, err := db.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", key); err != nil {
			return err
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT email,phase,sealed_session,attempt,failure,expires_at,account_id,group_id
		FROM keypouch_import_jobs WHERE job_id=$1`, id)
	if err != nil {
		return err
	}
	job := service.KeyPouchImportJob{ID: id, Email: email}
	if rows.Next() {
		err = rows.Scan(&job.Email, &job.Phase, &job.SealedSession, &job.Attempt, &job.Failure, &job.ExpiresAt, &job.AccountID, &job.GroupID)
	}
	err = errors.Join(err, rows.Close(), rows.Err())
	if err != nil {
		return err
	}
	if job.Email != email {
		return service.KeyPouchEmailConflict
	}

	before := job
	if err := run(dbent.NewTxContext(ctx, tx), &job); err != nil {
		return err
	}
	if job != before {
		_, err = db.ExecContext(ctx, `INSERT INTO keypouch_import_jobs(job_id,email,phase,sealed_session,attempt,failure,expires_at,account_id,group_id)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT(job_id) DO UPDATE SET phase=EXCLUDED.phase,sealed_session=EXCLUDED.sealed_session,
			  attempt=EXCLUDED.attempt,failure=EXCLUDED.failure,expires_at=EXCLUDED.expires_at,
			  account_id=EXCLUDED.account_id,group_id=EXCLUDED.group_id`,
			id, email, job.Phase, job.SealedSession, job.Attempt, job.Failure, job.ExpiresAt, job.AccountID, job.GroupID)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *keyPouchImportStore) TargetGroup(ctx context.Context) (int64, error) {
	rows, err := dbent.TxFromContext(ctx).Client().QueryContext(ctx, `SELECT id FROM groups
		WHERE name=$1 AND platform='openai' AND status='active' AND deleted_at IS NULL
		ORDER BY id LIMIT 2 FOR SHARE`, "GPT-不降智")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var id int64
	for rows.Next() {
		if id != 0 {
			return 0, service.KeyPouchGroupConflict
		}
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if id == 0 {
		return 0, service.KeyPouchGroupUnavailable
	}
	return id, nil
}

func (s *keyPouchImportStore) FindIdentity(ctx context.Context, email, workspace, user string, groupID int64) (*service.KeyPouchIdentity, error) {
	db := dbent.TxFromContext(ctx).Client()
	if user != "" {
		// A changed email or two simultaneous authorizations can identify the
		// same provider user even though their email transaction locks differ.
		if _, err := db.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "keypouch:provider-user:"+user); err != nil {
			return nil, err
		}
	}
	// A display name cannot establish identity. A matching name on a legacy
	// record with no credential email must be resolved by an administrator.
	// Workspace alone identifies a team, and is never a positive match.
	rows, err := db.QueryContext(ctx, `SELECT a.id,
			lower(trim(coalesce(a.credentials->>'email',''))),
			trim(coalesce(a.credentials->>'chatgpt_account_id','')),
			trim(coalesce(a.credentials->>'chatgpt_user_id','')),
			EXISTS(SELECT 1 FROM account_groups g WHERE g.account_id=a.id AND g.group_id=$4),
			coalesce(a.credentials->>'id_token',''), coalesce(a.credentials->>'auth_mode',''), coalesce(a.credentials->>'openai_auth_mode','')
		FROM accounts a WHERE a.platform='openai' AND a.type='oauth' AND a.deleted_at IS NULL
		  AND (lower(trim(a.credentials->>'email'))=$1
		       OR lower(trim(a.name))=$1
		       OR ($2<>'' AND trim(a.credentials->>'chatgpt_account_id')=$2 AND
		           ($3=trim(a.credentials->>'chatgpt_user_id') OR nullif(trim(a.credentials->>'chatgpt_user_id'),'') IS NULL))
		       OR ($3<>'' AND trim(a.credentials->>'chatgpt_user_id')=$3))
		ORDER BY a.id LIMIT 2 FOR UPDATE OF a`, email, workspace, user, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var match *service.KeyPouchIdentity
	for rows.Next() {
		var account service.KeyPouchIdentity
		var idToken, mode, legacyMode string
		if err := rows.Scan(&account.AccountID, &account.Email, &account.Workspace, &account.User, &account.Member, &idToken, &mode, &legacyMode); err != nil {
			return nil, err
		}
		kind := service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Credentials: map[string]any{"auth_mode": mode, "openai_auth_mode": legacyMode}}
		if match != nil || account.Email != email || kind.IsOpenAIPersonalAccessToken() || kind.IsOpenAIAgentIdentity() || !keyPouchStoredClaimsAgree(&account, idToken) || (workspace != "" && (account.Workspace != workspace || account.User != user)) {
			return nil, service.KeyPouchIdentityConflict
		}
		match = &account
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var matchedID int64
	if match != nil {
		matchedID = match.AccountID
	}
	if err := checkKeyPouchLegacyRecords(ctx, db, email, workspace, user, matchedID); err != nil {
		return nil, err
	}
	return match, nil
}

// Older manual imports may have an arbitrary name and only an ID/access token.
// Compare just the JWT payloads of records missing searchable metadata. This
// is a veto even if another, fully indexed row matched. Unverified historical
// claims alone never authorize a positive match or a membership mutation.
func checkKeyPouchLegacyRecords(ctx context.Context, db sqlExecutor, email, workspace, user string, matchedID int64) error {
	rows, err := db.QueryContext(ctx, `SELECT split_part(coalesce(credentials->>'id_token',''),'.',2),
		split_part(coalesce(credentials->>'access_token',''),'.',2) FROM accounts
		WHERE platform='openai' AND type='oauth' AND deleted_at IS NULL AND id<>$1
		AND (nullif(trim(credentials->>'email'),'') IS NULL OR
		     ($2<>'' AND (nullif(trim(credentials->>'chatgpt_user_id'),'') IS NULL OR
		                   nullif(trim(credentials->>'chatgpt_account_id'),'') IS NULL)))
		AND (position('.' in credentials->>'id_token')>0 OR position('.' in credentials->>'access_token')>0)`, matchedID, user)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var idPayload, accessPayload string
		if err := rows.Scan(&idPayload, &accessPayload); err != nil {
			return err
		}
		for _, payload := range []string{idPayload, accessPayload} {
			claims, e := openai.DecodeKeyPouchCredentialPayload(payload)
			if e != nil {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(claims.Email), email) || strings.EqualFold(strings.TrimSpace(claims.Profile.Email), email) {
				return service.KeyPouchIdentityConflict
			}
			if claims.Auth != nil && user != "" {
				oldUser := claims.Auth.ChatGPTUserID
				if oldUser == "" {
					oldUser = claims.Auth.UserID
				}
				if oldUser == user || (oldUser == "" && claims.Auth.ChatGPTAccountID == workspace) {
					return service.KeyPouchIdentityConflict
				}
			}
		}
	}
	return rows.Err()
}

// The site may have merged a credential snapshot during reauthorization. Treat
// disagreement with the historical ID token as a conflict. An unverified
// payload can only veto a match, never supply a missing credential address.
func keyPouchStoredClaimsAgree(account *service.KeyPouchIdentity, idToken string) bool {
	if idToken == "" {
		return true
	}
	claims, err := openai.DecodeKeyPouchCredentialClaims(idToken)
	return err == nil && claims.AgreesWith(account.Email, account.Workspace, account.User)
}

func (s *keyPouchImportStore) ConfirmMembership(ctx context.Context, accountID, groupID int64, appendGroup bool) error {
	db := dbent.TxFromContext(ctx).Client()
	if appendGroup {
		// Omit priority so only the new membership gets the schema default.
		result, err := db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id)
			VALUES($1,$2) ON CONFLICT(account_id,group_id) DO NOTHING`, accountID, groupID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 1 {
			if err := enqueueSchedulerOutbox(ctx, db, service.SchedulerOutboxEventAccountGroupsChanged, &accountID, nil, buildSchedulerGroupPayload([]int64{groupID})); err != nil {
				return err
			}
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT 1 FROM account_groups WHERE account_id=$1 AND group_id=$2 FOR SHARE`, accountID, groupID)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return nil
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return service.KeyPouchMembershipChanged
}
