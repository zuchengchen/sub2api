package service

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContentModerationCredentialCipherRoundTripAndDomainBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyring.json")
	writeModerationArchiveTestKeyRing(t, path, "k1", map[string][]byte{
		"k1": []byte("0123456789abcdef0123456789abcdef"),
	})
	cipher := NewContentModerationCredentialCipher(NewContentModerationArchiveKeyRingFile(path))

	envelope, err := cipher.EncryptDeepSeekAPIKey("official", "sk-test-not-a-real-secret")
	require.NoError(t, err)
	require.Equal(t, ContentModerationCredentialVersion, envelope.Version)
	require.Equal(t, contentModerationDeepSeekKeyDomain, envelope.Domain)
	require.NotContains(t, string(envelope.Ciphertext), "sk-test")

	got, err := cipher.DecryptDeepSeekAPIKey("official", envelope)
	require.NoError(t, err)
	require.Equal(t, "sk-test-not-a-real-secret", got)

	_, err = cipher.DecryptDeepSeekAPIKey("fallback", envelope)
	require.ErrorIs(t, err, ErrModerationCredentialIntegrity)

	broken := *envelope
	broken.Domain = "archive"
	_, err = cipher.DecryptDeepSeekAPIKey("official", &broken)
	require.ErrorIs(t, err, ErrModerationCredentialIntegrity)

	broken = *envelope
	broken.Ciphertext = append([]byte(nil), envelope.Ciphertext...)
	broken.Ciphertext[0] ^= 0xff
	_, err = cipher.DecryptDeepSeekAPIKey("official", &broken)
	require.ErrorIs(t, err, ErrModerationCredentialIntegrity)
}

func TestContentModerationCredentialCipherUsesHistoricalKeyAfterRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyring.json")
	k1 := []byte("0123456789abcdef0123456789abcdef")
	k2 := []byte("abcdef0123456789abcdef0123456789")
	writeModerationArchiveTestKeyRing(t, path, "k1", map[string][]byte{"k1": k1})
	cipher := NewContentModerationCredentialCipher(NewContentModerationArchiveKeyRingFile(path))

	first, err := cipher.EncryptDeepSeekAPIKey("official", "sk-first")
	require.NoError(t, err)
	writeModerationArchiveTestKeyRing(t, path, "k2", map[string][]byte{"k1": k1, "k2": k2})
	second, err := cipher.EncryptDeepSeekAPIKey("official", "sk-second")
	require.NoError(t, err)
	require.Equal(t, "k1", first.KeyID)
	require.Equal(t, "k2", second.KeyID)

	got, err := cipher.DecryptDeepSeekAPIKey("official", first)
	require.NoError(t, err)
	require.Equal(t, "sk-first", got)
	got, err = cipher.DecryptDeepSeekAPIKey("official", second)
	require.NoError(t, err)
	require.Equal(t, "sk-second", got)

	writeModerationArchiveTestKeyRing(t, path, "k2", map[string][]byte{"k2": k2})
	_, err = cipher.DecryptDeepSeekAPIKey("official", first)
	require.True(t, errors.Is(err, ErrModerationCredentialKeyUnavailable))
}
