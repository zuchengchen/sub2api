package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

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
