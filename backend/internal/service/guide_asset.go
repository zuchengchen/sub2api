package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	// GuideAssetMaxBytes caps one upload. nginx allows 64m on the strictest
	// server block, so this leaves headroom for multipart overhead.
	GuideAssetMaxBytes = 32 * 1024 * 1024
	// GuideAssetMaxTotalBytes caps the directory so uploads cannot fill the disk.
	GuideAssetMaxTotalBytes = 2 * 1024 * 1024 * 1024
	// GuideAssetMaxNameLength bounds the stored display name.
	GuideAssetMaxNameLength = 120

	guideAssetDirName = "guide"
)

// inlineImageContentTypes are the only types served inline. Everything else is
// forced to download, which is what keeps an uploaded .svg or .html from
// executing script under our own origin (stored XSS). SVG is deliberately
// absent: it is an image to users but an executable document to browsers.
var inlineImageContentTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".avif": "image/avif",
	".bmp":  "image/bmp",
	".ico":  "image/x-icon",
}

// GuideAsset is one uploaded file available to the guide.
type GuideAsset struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	Inline     bool   `json:"inline"`
	URL        string `json:"url"`
	UploadedAt string `json:"uploaded_at"`
}

// GuideAssetStore persists guide attachments on local disk. The metadata index
// lives in the settings table so the file list survives a restart and stays
// consistent with what the download route will serve.
type GuideAssetStore struct {
	mu       sync.Mutex
	baseDir  string
	settings *SettingService
}

// NewGuideAssetStore resolves the upload directory the same way the plugin
// installer does: DATA_DIR when set, otherwise ./data relative to the working
// directory. Under systemd this lands in /opt/sub2api/data, which is already
// listed in ReadWritePaths.
func NewGuideAssetStore(settings *SettingService) *GuideAssetStore {
	base := strings.TrimSpace(os.Getenv("DATA_DIR"))
	if base == "" {
		base = "./data"
	}
	return &GuideAssetStore{
		baseDir:  filepath.Join(base, "uploads", guideAssetDirName),
		settings: settings,
	}
}

// Save streams reader to disk under a generated id, rejecting anything that
// would exceed the per-file or total quota.
func (s *GuideAssetStore) Save(ctx context.Context, originalName string, size int64, reader io.Reader) (*GuideAsset, error) {
	if size > GuideAssetMaxBytes {
		return nil, infraerrors.BadRequest("GUIDE_ASSET_TOO_LARGE", "file must not exceed 32 MiB")
	}

	name := sanitizeGuideAssetName(originalName)
	if name == "" {
		return nil, infraerrors.BadRequest("GUIDE_ASSET_NAME_INVALID", "file name is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	assets, err := s.load(ctx)
	if err != nil {
		return nil, err
	}

	var used int64
	for _, asset := range assets {
		used += asset.Size
	}
	if used+size > GuideAssetMaxTotalBytes {
		return nil, infraerrors.BadRequest("GUIDE_ASSET_QUOTA_EXCEEDED", "total uploaded size must not exceed 2 GiB")
	}

	if err := os.MkdirAll(s.baseDir, 0o750); err != nil {
		return nil, fmt.Errorf("create guide asset dir: %w", err)
	}

	id, err := newGuideAssetID()
	if err != nil {
		return nil, err
	}
	ext := strings.ToLower(filepath.Ext(name))
	storedName := id + ext
	// filepath.Join on a generated hex id plus a validated extension cannot
	// escape baseDir, and the extension is re-checked below.
	if strings.ContainsAny(ext, `/\`) || strings.Contains(ext, "..") {
		return nil, infraerrors.BadRequest("GUIDE_ASSET_NAME_INVALID", "file extension is invalid")
	}
	target := filepath.Join(s.baseDir, storedName)

	written, err := writeGuideAssetFile(target, reader, GuideAssetMaxBytes)
	if err != nil {
		_ = os.Remove(target)
		return nil, err
	}

	asset := GuideAsset{
		ID:         storedName,
		Name:       name,
		Size:       written,
		Inline:     isInlineGuideAsset(storedName),
		URL:        "/api/v1/guide/assets/" + storedName,
		UploadedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if err := s.store(ctx, append(assets, asset)); err != nil {
		_ = os.Remove(target)
		return nil, err
	}
	return &asset, nil
}

// List returns the stored assets, newest first.
func (s *GuideAssetStore) List(ctx context.Context) ([]GuideAsset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	assets, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(assets, func(i, j int) bool {
		return assets[i].UploadedAt > assets[j].UploadedAt
	})
	return assets, nil
}

// Delete removes one asset from disk and from the index.
func (s *GuideAssetStore) Delete(ctx context.Context, id string) error {
	path, err := s.resolve(id)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	assets, err := s.load(ctx)
	if err != nil {
		return err
	}
	kept := make([]GuideAsset, 0, len(assets))
	found := false
	for _, asset := range assets {
		if asset.ID == id {
			found = true
			continue
		}
		kept = append(kept, asset)
	}
	if !found {
		return infraerrors.NotFound("GUIDE_ASSET_NOT_FOUND", "file was not found")
	}
	if err := s.store(ctx, kept); err != nil {
		return err
	}
	// The index is authoritative, so a missing file is not an error here.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove guide asset: %w", err)
	}
	return nil
}

// Open resolves an id to a readable file plus the headers the download route
// must send. inline is false for everything except the image whitelist.
func (s *GuideAssetStore) Open(ctx context.Context, id string) (path, name, contentType string, inline bool, err error) {
	path, err = s.resolve(id)
	if err != nil {
		return "", "", "", false, err
	}

	s.mu.Lock()
	assets, loadErr := s.load(ctx)
	s.mu.Unlock()
	if loadErr != nil {
		return "", "", "", false, loadErr
	}

	// Only an indexed asset is servable; a stray file on disk is not exposed.
	for _, asset := range assets {
		if asset.ID != id {
			continue
		}
		if _, statErr := os.Stat(path); statErr != nil {
			return "", "", "", false, infraerrors.NotFound("GUIDE_ASSET_NOT_FOUND", "file was not found")
		}
		inline = asset.Inline
		contentType = "application/octet-stream"
		if inline {
			contentType = inlineImageContentTypes[strings.ToLower(filepath.Ext(id))]
		}
		return path, asset.Name, contentType, inline, nil
	}
	return "", "", "", false, infraerrors.NotFound("GUIDE_ASSET_NOT_FOUND", "file was not found")
}

// resolve validates an id and maps it to a path inside baseDir. The id is
// generated by us, so anything containing a separator or traversal segment is
// rejected outright rather than cleaned.
func (s *GuideAssetStore) resolve(id string) (string, error) {
	if id == "" || len(id) > 128 {
		return "", infraerrors.BadRequest("GUIDE_ASSET_ID_INVALID", "file id is invalid")
	}
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") || strings.ContainsRune(id, 0) {
		return "", infraerrors.BadRequest("GUIDE_ASSET_ID_INVALID", "file id is invalid")
	}
	if id != filepath.Base(id) {
		return "", infraerrors.BadRequest("GUIDE_ASSET_ID_INVALID", "file id is invalid")
	}

	path := filepath.Join(s.baseDir, id)
	// Defence in depth: confirm the result really is under baseDir.
	base, err := filepath.Abs(s.baseDir)
	if err != nil {
		return "", fmt.Errorf("resolve guide asset dir: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve guide asset path: %w", err)
	}
	if abs != filepath.Join(base, filepath.Base(abs)) || !strings.HasPrefix(abs, base+string(os.PathSeparator)) {
		return "", infraerrors.BadRequest("GUIDE_ASSET_ID_INVALID", "file id is invalid")
	}
	return abs, nil
}

func (s *GuideAssetStore) load(ctx context.Context) ([]GuideAsset, error) {
	raw, err := s.settings.settingRepo.GetValue(ctx, SettingKeyGuideAssets)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return make([]GuideAsset, 0), nil
		}
		return nil, fmt.Errorf("get guide assets: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return make([]GuideAsset, 0), nil
	}
	assets := make([]GuideAsset, 0, 16)
	if err := json.Unmarshal([]byte(raw), &assets); err != nil {
		return nil, fmt.Errorf("parse guide assets: %w", err)
	}
	return assets, nil
}

func (s *GuideAssetStore) store(ctx context.Context, assets []GuideAsset) error {
	encoded, err := json.Marshal(assets)
	if err != nil {
		return fmt.Errorf("marshal guide assets: %w", err)
	}
	if err := s.settings.settingRepo.Set(ctx, SettingKeyGuideAssets, string(encoded)); err != nil {
		return fmt.Errorf("save guide assets: %w", err)
	}
	return nil
}

// writeGuideAssetFile copies at most limit bytes, failing if the reader has
// more so a lying Content-Length cannot exceed the cap.
func writeGuideAssetFile(target string, reader io.Reader, limit int64) (int64, error) {
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return 0, fmt.Errorf("create guide asset: %w", err)
	}
	defer func() { _ = file.Close() }()

	written, err := io.Copy(file, io.LimitReader(reader, limit+1))
	if err != nil {
		return 0, fmt.Errorf("write guide asset: %w", err)
	}
	if written > limit {
		return 0, infraerrors.BadRequest("GUIDE_ASSET_TOO_LARGE", "file must not exceed 32 MiB")
	}
	return written, nil
}

func isInlineGuideAsset(name string) bool {
	_, ok := inlineImageContentTypes[strings.ToLower(filepath.Ext(name))]
	return ok
}

func newGuideAssetID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate guide asset id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// sanitizeGuideAssetName keeps a human-readable display name while removing
// path separators, control characters and anything that could be interpreted
// as a directory. The stored filename never comes from this value.
func sanitizeGuideAssetName(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// A browser may send a full path; keep only the last segment.
	raw = raw[strings.LastIndexAny(raw, `/\`)+1:]
	if !utf8.ValidString(raw) {
		return ""
	}

	var builder strings.Builder
	for _, r := range raw {
		switch {
		case r == 0 || unicode.IsControl(r):
			continue
		case r == '/' || r == '\\':
			continue
		default:
			builder.WriteRune(r)
		}
	}
	name := strings.TrimSpace(builder.String())
	name = strings.TrimLeft(name, ".")
	if name == "" {
		return ""
	}
	if len(name) > GuideAssetMaxNameLength {
		ext := filepath.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		keep := GuideAssetMaxNameLength - len(ext)
		if keep < 1 {
			keep = 1
		}
		name = name[:keep] + ext
	}
	return name
}
