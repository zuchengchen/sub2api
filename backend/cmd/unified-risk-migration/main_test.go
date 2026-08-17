package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestVerifyMigrationRuntimeLockRequiresActiveOwner(t *testing.T) {
	retryDir := t.TempDir()
	require.NoError(t, os.Chmod(retryDir, 0o700))
	lockPath := filepath.Join(retryDir, ".sub2api.lock")
	owner, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close() })

	require.ErrorContains(t, verifyMigrationRuntimeLock(retryDir), "is not held")
	require.NoError(t, syscall.Flock(int(owner.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	require.NoError(t, verifyMigrationRuntimeLock(retryDir))
	require.NoError(t, syscall.Flock(int(owner.Fd()), syscall.LOCK_UN))
}

func TestIsolatedRestoreListKeepsInternalForeignKeyAndSkipsExternalParents(t *testing.T) {
	list := strings.Join([]string{
		"1; 0 0 TABLE public prompt_audit_jobs sub2api",
		"2; 0 0 TABLE DATA public prompt_audit_jobs sub2api",
		"3; 0 0 TABLE public prompt_audit_events sub2api",
		"4; 0 0 TABLE DATA public prompt_audit_events sub2api",
		"5; 0 0 INDEX public idx_prompt_audit_jobs_schedule sub2api",
		"6; 0 0 INDEX public idx_prompt_audit_events_job sub2api",
		"7; 0 0 FK CONSTRAINT public prompt_audit_events prompt_audit_events_api_key_id_fkey sub2api",
		"8; 0 0 FK CONSTRAINT public prompt_audit_events prompt_audit_events_group_id_fkey sub2api",
		"9; 0 0 FK CONSTRAINT public prompt_audit_events prompt_audit_events_job_id_fkey sub2api",
		"10; 0 0 FK CONSTRAINT public prompt_audit_events prompt_audit_events_user_id_fkey sub2api",
		"11; 0 0 FK CONSTRAINT public prompt_audit_jobs prompt_audit_jobs_api_key_id_fkey sub2api",
		"12; 0 0 FK CONSTRAINT public prompt_audit_jobs prompt_audit_jobs_group_id_fkey sub2api",
		"13; 0 0 FK CONSTRAINT public prompt_audit_jobs prompt_audit_jobs_user_id_fkey sub2api",
	}, "\n") + "\n"

	require.NoError(t, validateRestoreList([]byte(list)))
	filtered, err := isolatedRestoreList([]byte(list))
	require.NoError(t, err)
	require.Contains(t, string(filtered), "prompt_audit_events_job_id_fkey")
	for _, external := range []string{
		"prompt_audit_events_api_key_id_fkey", "prompt_audit_events_group_id_fkey",
		"prompt_audit_events_user_id_fkey", "prompt_audit_jobs_api_key_id_fkey",
		"prompt_audit_jobs_group_id_fkey", "prompt_audit_jobs_user_id_fkey",
	} {
		require.NotContains(t, string(filtered), external)
	}
}

func TestIsolatedRestoreListRejectsUnexpectedForeignKeyLayout(t *testing.T) {
	_, err := isolatedRestoreList([]byte("1; 0 0 TABLE public prompt_audit_jobs sub2api\n"))
	require.ErrorContains(t, err, "unexpected Prompt Audit foreign-key layout")
}

func TestVerifyMigrationArchiveKeyReferencesIncludesDeepSeekCredentialEnvelopes(t *testing.T) {
	keyRingPath := filepath.Join(t.TempDir(), "keyring.json")
	require.NoError(t, os.WriteFile(keyRingPath, []byte(`{
  "current_key_id": "archive-key-2026-01",
  "keys": {
    "archive-key-2026-01": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
  }
}`), 0o600))

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectQuery(`(?s)WITH referenced_keys AS .*jsonb_path_query.*api_key_envelope.key_id.*SELECT DISTINCT key_id`).
		WithArgs("content_moderation_config").
		WillReturnRows(sqlmock.NewRows([]string{"key_id"}).
			AddRow("archive-key-2026-01").
			AddRow("credential-key-2026-02"))

	err = verifyMigrationArchiveKeyReferences(context.Background(), db, keyRingPath)

	require.ErrorContains(t, err, `content moderation key ring is missing referenced key ID "credential-key-2026-02"`)
	require.NoError(t, mock.ExpectationsWereMet())
}
