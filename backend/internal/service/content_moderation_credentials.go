package service

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	ContentModerationCredentialVersion = 1
	contentModerationDeepSeekKeyDomain = "sub2api/content-moderation/deepseek-channel-key"
)

var (
	ErrModerationCredentialKeyUnavailable = errors.New("content moderation credential key unavailable")
	ErrModerationCredentialIntegrity      = errors.New("content moderation credential integrity check failed")
)

// ContentModerationCredentialEnvelope is the only API-key representation
// persisted in the settings JSON. The domain is deliberately separate from
// the archive cipher even though both use the deployment-owned key ring.
type ContentModerationCredentialEnvelope struct {
	Version    int    `json:"version"`
	Domain     string `json:"domain"`
	KeyID      string `json:"key_id"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

type ContentModerationCredentialCipher struct {
	keyRing *ContentModerationArchiveKeyRingFile
	rand    func([]byte) (int, error)
}

func NewContentModerationCredentialCipher(keyRing *ContentModerationArchiveKeyRingFile) *ContentModerationCredentialCipher {
	return &ContentModerationCredentialCipher{keyRing: keyRing, rand: rand.Read}
}

func (c *ContentModerationCredentialCipher) EncryptDeepSeekAPIKey(channelID, apiKey string) (*ContentModerationCredentialEnvelope, error) {
	channelID = strings.TrimSpace(channelID)
	apiKey = strings.TrimSpace(apiKey)
	if channelID == "" || apiKey == "" {
		return nil, errors.New("DeepSeek channel ID and API key are required")
	}
	if c == nil || c.keyRing == nil {
		return nil, ErrModerationCredentialKeyUnavailable
	}
	keyID, key, err := c.keyRing.Current()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrModerationCredentialKeyUnavailable, err)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("create content moderation credential cipher: %w", err)
	}
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := c.rand(nonce); err != nil {
		return nil, fmt.Errorf("generate content moderation credential nonce: %w", err)
	}
	envelope := &ContentModerationCredentialEnvelope{
		Version: ContentModerationCredentialVersion,
		Domain:  contentModerationDeepSeekKeyDomain,
		KeyID:   keyID,
		Nonce:   nonce,
	}
	envelope.Ciphertext = aead.Seal(nil, nonce, []byte(apiKey), contentModerationCredentialAAD(envelope.Domain, envelope.Version, channelID, keyID))
	return envelope, nil
}

func (c *ContentModerationCredentialCipher) DecryptDeepSeekAPIKey(channelID string, envelope *ContentModerationCredentialEnvelope) (string, error) {
	channelID = strings.TrimSpace(channelID)
	if c == nil || c.keyRing == nil || envelope == nil {
		return "", ErrModerationCredentialKeyUnavailable
	}
	if channelID == "" || envelope.Version != ContentModerationCredentialVersion ||
		envelope.Domain != contentModerationDeepSeekKeyDomain || strings.TrimSpace(envelope.KeyID) == "" ||
		len(envelope.Nonce) != chacha20poly1305.NonceSizeX || len(envelope.Ciphertext) == 0 {
		return "", ErrModerationCredentialIntegrity
	}
	key, err := c.keyRing.ByID(envelope.KeyID)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrModerationCredentialKeyUnavailable, err)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return "", fmt.Errorf("create content moderation credential cipher: %w", err)
	}
	plaintext, err := aead.Open(nil, envelope.Nonce, envelope.Ciphertext,
		contentModerationCredentialAAD(envelope.Domain, envelope.Version, channelID, envelope.KeyID))
	if err != nil || strings.TrimSpace(string(plaintext)) == "" {
		return "", ErrModerationCredentialIntegrity
	}
	return strings.TrimSpace(string(plaintext)), nil
}

func contentModerationCredentialAAD(domain string, version int, channelID, keyID string) []byte {
	var aad bytes.Buffer
	_, _ = aad.WriteString(contentModerationDeepSeekKeyDomain + "\x00")
	_ = binary.Write(&aad, binary.BigEndian, uint32(version))
	writeModerationArchiveAADString(&aad, domain)
	writeModerationArchiveAADString(&aad, channelID)
	writeModerationArchiveAADString(&aad, keyID)
	return aad.Bytes()
}
