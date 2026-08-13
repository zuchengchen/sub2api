package service

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	ContentModerationArchiveVersion    = 1
	defaultModerationArchiveChunkBytes = 1 << 20
)

var (
	ErrModerationArchiveKeyUnavailable = errors.New("content moderation archive key unavailable")
	ErrModerationArchiveIntegrity      = errors.New("content moderation archive integrity check failed")
)

// ContentModerationArchiveKeyRing is the on-disk key-ring format. Key values
// are standard or raw base64 encoded 32-byte XChaCha20-Poly1305 keys.
type ContentModerationArchiveKeyRing struct {
	CurrentKeyID string            `json:"current_key_id"`
	Keys         map[string]string `json:"keys"`
}

type contentModerationLoadedKeyRing struct {
	currentKeyID string
	keys         map[string][]byte
}

// ContentModerationArchiveKeyRingFile reloads a key ring when the file's
// identity changes. Existing encrypted rows keep their key ID, so rotation is
// a file replacement that retains every still-referenced historical key.
type ContentModerationArchiveKeyRingFile struct {
	path string

	mu     sync.Mutex
	digest [sha256.Size]byte
	loaded *contentModerationLoadedKeyRing
}

func NewContentModerationArchiveKeyRingFile(path string) *ContentModerationArchiveKeyRingFile {
	return &ContentModerationArchiveKeyRingFile{path: strings.TrimSpace(path)}
}

func (f *ContentModerationArchiveKeyRingFile) Path() string {
	if f == nil {
		return ""
	}
	return f.path
}

func (f *ContentModerationArchiveKeyRingFile) Current() (string, []byte, error) {
	ring, err := f.load()
	if err != nil {
		return "", nil, err
	}
	key := ring.keys[ring.currentKeyID]
	if len(key) != chacha20poly1305.KeySize {
		return "", nil, fmt.Errorf("%w: current key %q is missing", ErrModerationArchiveKeyUnavailable, ring.currentKeyID)
	}
	return ring.currentKeyID, append([]byte(nil), key...), nil
}

func (f *ContentModerationArchiveKeyRingFile) ByID(keyID string) ([]byte, error) {
	ring, err := f.load()
	if err != nil {
		return nil, err
	}
	key := ring.keys[strings.TrimSpace(keyID)]
	if len(key) != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("%w: key %q is missing", ErrModerationArchiveKeyUnavailable, keyID)
	}
	return append([]byte(nil), key...), nil
}

func (f *ContentModerationArchiveKeyRingFile) KeyIDs() ([]string, error) {
	ring, err := f.load()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(ring.keys))
	for id := range ring.keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (f *ContentModerationArchiveKeyRingFile) load() (*contentModerationLoadedKeyRing, error) {
	if f == nil || f.path == "" {
		return nil, fmt.Errorf("%w: key-ring path is empty", ErrModerationArchiveKeyUnavailable)
	}
	info, err := os.Stat(f.path)
	if err != nil {
		return nil, fmt.Errorf("%w: stat key ring: %v", ErrModerationArchiveKeyUnavailable, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: key ring is not a regular file", ErrModerationArchiveKeyUnavailable)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: key ring permissions must not allow group or other access", ErrModerationArchiveKeyUnavailable)
	}

	raw, err := os.ReadFile(f.path)
	if err != nil {
		return nil, fmt.Errorf("%w: read key ring: %v", ErrModerationArchiveKeyUnavailable, err)
	}
	digest := sha256.Sum256(raw)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loaded != nil && digest == f.digest {
		return f.loaded, nil
	}
	loaded, err := parseContentModerationArchiveKeyRing(raw)
	if err != nil {
		return nil, err
	}
	f.digest = digest
	f.loaded = loaded
	return loaded, nil
}

func parseContentModerationArchiveKeyRing(raw []byte) (*contentModerationLoadedKeyRing, error) {
	var disk ContentModerationArchiveKeyRing
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&disk); err != nil {
		return nil, fmt.Errorf("%w: decode key ring: %v", ErrModerationArchiveKeyUnavailable, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: key ring contains trailing data", ErrModerationArchiveKeyUnavailable)
	}
	disk.CurrentKeyID = strings.TrimSpace(disk.CurrentKeyID)
	if disk.CurrentKeyID == "" || len(disk.Keys) == 0 {
		return nil, fmt.Errorf("%w: current_key_id and keys are required", ErrModerationArchiveKeyUnavailable)
	}
	keys := make(map[string][]byte, len(disk.Keys))
	for rawID, encoded := range disk.Keys {
		id := strings.TrimSpace(rawID)
		if id == "" || id != rawID {
			return nil, fmt.Errorf("%w: key IDs must be non-empty and already trimmed", ErrModerationArchiveKeyUnavailable)
		}
		key, err := decodeModerationArchiveKey(encoded)
		if err != nil {
			return nil, fmt.Errorf("%w: key %q: %v", ErrModerationArchiveKeyUnavailable, id, err)
		}
		keys[id] = key
	}
	if len(keys[disk.CurrentKeyID]) != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("%w: current key %q does not exist", ErrModerationArchiveKeyUnavailable, disk.CurrentKeyID)
	}
	return &contentModerationLoadedKeyRing{currentKeyID: disk.CurrentKeyID, keys: keys}, nil
}

func decodeModerationArchiveKey(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	var lastErr error
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		decoded, err := encoding.DecodeString(value)
		if err != nil {
			lastErr = err
			continue
		}
		if len(decoded) != chacha20poly1305.KeySize {
			return nil, fmt.Errorf("decoded length is %d, want %d", len(decoded), chacha20poly1305.KeySize)
		}
		return decoded, nil
	}
	return nil, lastErr
}

type ContentModerationArchiveChunk struct {
	Index          int    `json:"index"`
	Total          int    `json:"total"`
	Nonce          []byte `json:"nonce"`
	Ciphertext     []byte `json:"ciphertext"`
	PlaintextBytes int    `json:"plaintext_bytes"`
}

type ContentModerationEncryptedArchive struct {
	ArchiveID     string                          `json:"archive_id"`
	Version       int                             `json:"version"`
	KeyID         string                          `json:"key_id"`
	PlaintextHash []byte                          `json:"plaintext_hash"`
	PlaintextSize int64                           `json:"plaintext_size"`
	Chunks        []ContentModerationArchiveChunk `json:"chunks"`
}

type ContentModerationArchiveCipher struct {
	keyRing   *ContentModerationArchiveKeyRingFile
	chunkSize int
	rand      func([]byte) (int, error)
}

func NewContentModerationArchiveCipher(keyRing *ContentModerationArchiveKeyRingFile, chunkSize int) *ContentModerationArchiveCipher {
	if chunkSize <= 0 {
		chunkSize = defaultModerationArchiveChunkBytes
	}
	return &ContentModerationArchiveCipher{keyRing: keyRing, chunkSize: chunkSize, rand: rand.Read}
}

func (c *ContentModerationArchiveCipher) Encrypt(archiveID string, plaintext []byte) (*ContentModerationEncryptedArchive, error) {
	archiveID = strings.TrimSpace(archiveID)
	if archiveID == "" {
		return nil, errors.New("content moderation archive ID is required")
	}
	if c == nil || c.keyRing == nil {
		return nil, ErrModerationArchiveKeyUnavailable
	}
	keyID, key, err := c.keyRing.Current()
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("create archive cipher: %w", err)
	}
	chunkSize := c.chunkSize
	if chunkSize <= 0 {
		chunkSize = defaultModerationArchiveChunkBytes
	}
	total := (len(plaintext) + chunkSize - 1) / chunkSize
	if total == 0 {
		total = 1
	}
	archive := &ContentModerationEncryptedArchive{
		ArchiveID: archiveID,
		Version:   ContentModerationArchiveVersion,
		KeyID:     keyID,
		Chunks:    make([]ContentModerationArchiveChunk, 0, total),
	}
	hash := sha256Bytes(plaintext)
	archive.PlaintextHash = hash
	archive.PlaintextSize = int64(len(plaintext))
	seenNonces := make(map[string]struct{}, total)
	for index := 0; index < total; index++ {
		start := index * chunkSize
		end := start + chunkSize
		if end > len(plaintext) {
			end = len(plaintext)
		}
		part := plaintext[start:end]
		nonce := make([]byte, chacha20poly1305.NonceSizeX)
		for {
			if _, err := c.rand(nonce); err != nil {
				return nil, fmt.Errorf("generate archive nonce: %w", err)
			}
			if _, duplicate := seenNonces[string(nonce)]; !duplicate {
				seenNonces[string(nonce)] = struct{}{}
				break
			}
		}
		aad := moderationArchiveAAD(archiveID, ContentModerationArchiveVersion, keyID, index, total)
		archive.Chunks = append(archive.Chunks, ContentModerationArchiveChunk{
			Index: index, Total: total, Nonce: nonce,
			Ciphertext: aead.Seal(nil, nonce, part, aad), PlaintextBytes: len(part),
		})
	}
	return archive, nil
}

func (c *ContentModerationArchiveCipher) Decrypt(archive *ContentModerationEncryptedArchive) ([]byte, error) {
	if c == nil || c.keyRing == nil || archive == nil {
		return nil, ErrModerationArchiveKeyUnavailable
	}
	if archive.Version != ContentModerationArchiveVersion || strings.TrimSpace(archive.ArchiveID) == "" || strings.TrimSpace(archive.KeyID) == "" {
		return nil, ErrModerationArchiveIntegrity
	}
	key, err := c.keyRing.ByID(archive.KeyID)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("create archive cipher: %w", err)
	}
	if len(archive.Chunks) == 0 {
		return nil, ErrModerationArchiveIntegrity
	}
	total := len(archive.Chunks)
	var plaintext bytes.Buffer
	seenNonces := make(map[string]struct{}, total)
	for index, chunk := range archive.Chunks {
		if chunk.Index != index || chunk.Total != total || len(chunk.Nonce) != chacha20poly1305.NonceSizeX || chunk.PlaintextBytes < 0 {
			return nil, ErrModerationArchiveIntegrity
		}
		if _, duplicate := seenNonces[string(chunk.Nonce)]; duplicate {
			return nil, ErrModerationArchiveIntegrity
		}
		seenNonces[string(chunk.Nonce)] = struct{}{}
		aad := moderationArchiveAAD(archive.ArchiveID, archive.Version, archive.KeyID, index, total)
		part, err := aead.Open(nil, chunk.Nonce, chunk.Ciphertext, aad)
		if err != nil || len(part) != chunk.PlaintextBytes {
			return nil, ErrModerationArchiveIntegrity
		}
		_, _ = plaintext.Write(part)
	}
	out := plaintext.Bytes()
	if int64(len(out)) != archive.PlaintextSize || !bytes.Equal(sha256Bytes(out), archive.PlaintextHash) {
		return nil, ErrModerationArchiveIntegrity
	}
	return append([]byte(nil), out...), nil
}

func moderationArchiveAAD(archiveID string, version int, keyID string, index, total int) []byte {
	var aad bytes.Buffer
	_, _ = aad.WriteString("sub2api/content-moderation/archive\x00")
	_ = binary.Write(&aad, binary.BigEndian, uint32(version))
	writeModerationArchiveAADString(&aad, archiveID)
	writeModerationArchiveAADString(&aad, keyID)
	_ = binary.Write(&aad, binary.BigEndian, uint32(index))
	_ = binary.Write(&aad, binary.BigEndian, uint32(total))
	return aad.Bytes()
}

func writeModerationArchiveAADString(dst *bytes.Buffer, value string) {
	_ = binary.Write(dst, binary.BigEndian, uint32(len(value)))
	_, _ = dst.WriteString(value)
}

func sha256Bytes(value []byte) []byte {
	digest := sha256Sum(value)
	return digest[:]
}

func sha256Sum(value []byte) [32]byte {
	// Kept behind a small helper so archive code never accidentally hashes a
	// string representation that could corrupt arbitrary request bytes.
	return sha256.Sum256(value)
}
