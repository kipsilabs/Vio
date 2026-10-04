package transcodenode

import (
	"context"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/themesongs"
)

// InputPathAuthorizer approves a media input before a node passes it to FFmpeg.
// Implementations must reject unsafe protocol schemes and paths outside the
// authoritative media catalog, while permitting approved relay and HTTP(S) stream URLs.
type InputPathAuthorizer interface {
	Allowed(ctx context.Context, path string) (bool, error)
}

type catalogPathSource interface {
	IsActivePath(ctx context.Context, path string) (bool, error)
}

// CatalogPathAuthorizer permits existing regular files whose exact logical
// path is active in the media catalog as well as approved relay and HTTP(S) stream
// URLs. The scanner deliberately keeps logical paths for readable symlinks, so catalog
// membership is the correct authority: resolving the target and requiring it to
// remain under the logical library root would reject media layouts the scanner
// explicitly supports.
type CatalogPathAuthorizer struct {
	paths catalogPathSource
}

// NewCatalogPathAuthorizer creates an FFmpeg input authorizer backed by the
// authoritative media-file catalog.
func NewCatalogPathAuthorizer(paths catalogPathSource) *CatalogPathAuthorizer {
	return &CatalogPathAuthorizer{paths: paths}
}

// Allowed reports whether path is an active catalog entry that resolves to a
// regular file on this node, or an approved relay/http(s) stream URL. os.Stat
// follows scanner-approved symlinks while rejecting dangling links,
// directories, and other non-regular inputs.
func (a *CatalogPathAuthorizer) Allowed(ctx context.Context, path string) (bool, error) {
	if a == nil {
		return false, nil
	}
	path = strings.TrimSpace(path)
	if isAllowedStreamURL(path) {
		return true, nil
	}
	if a.paths == nil || !plainAbsolutePath(path) {
		return false, nil
	}
	active, err := a.paths.IsActivePath(ctx, path)
	if err != nil {
		return false, err
	}
	if !active {
		return false, nil
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular(), nil
}

func isAllowedStreamURL(pathStr string) bool {
	if strings.ContainsRune(pathStr, '\x00') {
		return false
	}
	u, err := url.Parse(pathStr)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Host == "" {
		return false
	}
	if u.RawPath != "" {
		lowerRaw := strings.ToLower(u.RawPath)
		if strings.Contains(lowerRaw, "%2e") || strings.Contains(lowerRaw, "%2f") {
			return false
		}
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	cleaned := path.Clean(u.Path)
	rest, found := strings.CutPrefix(cleaned, "/source/")
	if !found {
		return false
	}
	token, _, _ := strings.Cut(rest, "/")
	return token != "" && token != "." && token != ".."
}

// ThemeInputApprover approves a detail-page theme file as the input of a
// progressive AAC conversion. Theme files are not media_files rows, so they
// have their own authority, reachable only from theme tokens: every other node
// input still goes through InputPathAuthorizer.
type ThemeInputApprover interface {
	AllowedTheme(ctx context.Context, id int64, path string, size int64, modified time.Time) (bool, error)
}

type themePathSource interface {
	IsActiveTheme(ctx context.Context, id int64, path string) (bool, error)
}

// ThemeInputAuthorizer permits a theme only when its id still names that exact
// path in an enabled library and the file on this node is the one the token
// described, so a signed token cannot turn FFmpeg on any other file.
type ThemeInputAuthorizer struct {
	themes themePathSource
}

// NewThemeInputAuthorizer creates a theme input authority backed by the theme
// catalog.
func NewThemeInputAuthorizer(themes themePathSource) *ThemeInputAuthorizer {
	return &ThemeInputAuthorizer{themes: themes}
}

func (a *ThemeInputAuthorizer) AllowedTheme(ctx context.Context, id int64, path string, size int64, modified time.Time) (bool, error) {
	if a == nil || a.themes == nil || id <= 0 || !plainAbsolutePath(path) {
		return false, nil
	}
	active, err := a.themes.IsActiveTheme(ctx, id, path)
	if err != nil || !active {
		return false, err
	}
	return themesongs.Unchanged(path, size, modified), nil
}

func plainAbsolutePath(path string) bool {
	path = strings.TrimSpace(path)
	return path != "" && !strings.ContainsRune(path, '\x00') && filepath.IsAbs(path)
}

// pathWithinRoot validates a not-yet-created output by resolving the root and
// target parent. The caller creates the basename only after this check.
func pathWithinRoot(root, target string) bool {
	if !plainAbsolutePath(root) || !plainAbsolutePath(target) {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return false
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(filepath.Clean(target)))
	if err != nil {
		return false
	}
	resolvedTarget := filepath.Join(resolvedParent, filepath.Base(target))
	return resolvedPathContained(resolvedRoot, resolvedTarget)
}

func resolvedPathContained(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
