package service

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContentModerationArchiveCipherRoundTripAndIntegrity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keyring.json")
	writeModerationArchiveTestKeyRing(t, path, "k1", map[string][]byte{
		"k1": []byte("0123456789abcdef0123456789abcdef"),
	})
	cipher := NewContentModerationArchiveCipher(NewContentModerationArchiveKeyRingFile(path), 7)
	plaintext := []byte("raw\x00request\nwith arbitrary bytes\xff and enough data for chunks")

	archive, err := cipher.Encrypt("event-42", plaintext)
	require.NoError(t, err)
	require.Greater(t, len(archive.Chunks), 2)
	require.Equal(t, "k1", archive.KeyID)

	got, err := cipher.Decrypt(archive)
	require.NoError(t, err)
	require.Equal(t, plaintext, got)

	t.Run("ciphertext tamper", func(t *testing.T) {
		broken := cloneModerationEncryptedArchive(archive)
		broken.Chunks[0].Ciphertext[0] ^= 0xff
		_, err := cipher.Decrypt(broken)
		require.ErrorIs(t, err, ErrModerationArchiveIntegrity)
	})

	t.Run("missing chunk", func(t *testing.T) {
		broken := cloneModerationEncryptedArchive(archive)
		broken.Chunks = append(broken.Chunks[:1], broken.Chunks[2:]...)
		_, err := cipher.Decrypt(broken)
		require.ErrorIs(t, err, ErrModerationArchiveIntegrity)
	})

	t.Run("reordered chunks", func(t *testing.T) {
		broken := cloneModerationEncryptedArchive(archive)
		broken.Chunks[0], broken.Chunks[1] = broken.Chunks[1], broken.Chunks[0]
		_, err := cipher.Decrypt(broken)
		require.ErrorIs(t, err, ErrModerationArchiveIntegrity)
	})

	t.Run("archive identity substitution", func(t *testing.T) {
		broken := cloneModerationEncryptedArchive(archive)
		broken.ArchiveID = "event-43"
		_, err := cipher.Decrypt(broken)
		require.ErrorIs(t, err, ErrModerationArchiveIntegrity)
	})
}

func TestContentModerationArchiveKeyRingHotRotationAndHistoricalKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keyring.json")
	k1 := []byte("0123456789abcdef0123456789abcdef")
	k2 := []byte("abcdef0123456789abcdef0123456789")
	writeModerationArchiveTestKeyRing(t, path, "k1", map[string][]byte{"k1": k1})
	cipher := NewContentModerationArchiveCipher(NewContentModerationArchiveKeyRingFile(path), 8)

	first, err := cipher.Encrypt("first", []byte("one"))
	require.NoError(t, err)
	require.Equal(t, "k1", first.KeyID)

	writeModerationArchiveTestKeyRing(t, path, "k2", map[string][]byte{"k1": k1, "k2": k2})
	second, err := cipher.Encrypt("second", []byte("two"))
	require.NoError(t, err)
	require.Equal(t, "k2", second.KeyID)

	gotFirst, err := cipher.Decrypt(first)
	require.NoError(t, err)
	require.Equal(t, []byte("one"), gotFirst)
	gotSecond, err := cipher.Decrypt(second)
	require.NoError(t, err)
	require.Equal(t, []byte("two"), gotSecond)

	writeModerationArchiveTestKeyRing(t, path, "k2", map[string][]byte{"k2": k2})
	_, err = cipher.Decrypt(first)
	require.ErrorIs(t, err, ErrModerationArchiveKeyUnavailable)
}

func TestContentModerationArchiveKeyRingRejectsUnsafeOrInvalidFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keyring.json")
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	require.NoError(t, os.WriteFile(path, []byte(`{"current_key_id":"k1","keys":{"k1":"`+key+`"}}`), 0o644))

	_, _, err := NewContentModerationArchiveKeyRingFile(path).Current()
	require.ErrorIs(t, err, ErrModerationArchiveKeyUnavailable)

	require.NoError(t, os.Chmod(path, 0o600))
	require.NoError(t, os.WriteFile(path, []byte(`{"current_key_id":"k1","keys":{"k1":"short"}}`), 0o600))
	_, _, err = NewContentModerationArchiveKeyRingFile(path).Current()
	require.ErrorIs(t, err, ErrModerationArchiveKeyUnavailable)

	require.NoError(t, os.WriteFile(path, []byte(`{"current_key_id":"k1","keys":{"k1":"`+key+`"}} trailing`), 0o600))
	_, _, err = NewContentModerationArchiveKeyRingFile(path).Current()
	require.True(t, errors.Is(err, ErrModerationArchiveKeyUnavailable))
}

func writeModerationArchiveTestKeyRing(t *testing.T, path, current string, keys map[string][]byte) {
	t.Helper()
	disk := ContentModerationArchiveKeyRing{CurrentKeyID: current, Keys: make(map[string]string, len(keys))}
	for id, key := range keys {
		disk.Keys[id] = base64.StdEncoding.EncodeToString(key)
	}
	raw, err := json.Marshal(disk)
	require.NoError(t, err)
	tmp := path + ".tmp"
	require.NoError(t, os.WriteFile(tmp, raw, 0o600))
	require.NoError(t, os.Rename(tmp, path))
}

func cloneModerationEncryptedArchive(in *ContentModerationEncryptedArchive) *ContentModerationEncryptedArchive {
	out := *in
	out.PlaintextHash = append([]byte(nil), in.PlaintextHash...)
	out.Chunks = make([]ContentModerationArchiveChunk, len(in.Chunks))
	for i := range in.Chunks {
		out.Chunks[i] = in.Chunks[i]
		out.Chunks[i].Nonce = append([]byte(nil), in.Chunks[i].Nonce...)
		out.Chunks[i].Ciphertext = append([]byte(nil), in.Chunks[i].Ciphertext...)
	}
	return &out
}
