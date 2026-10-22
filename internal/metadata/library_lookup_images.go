package metadata

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/Silo-Server/silo-server/internal/models"
)

// LibraryImageDiscoveryResult holds the artwork selected from a destination
// library's configured image providers for an item that has not been persisted
// yet. PosterPath and BackdropPath are ready to copy onto the request item or
// catalog item; Images is the raw provider result for later cache enqueue.
type LibraryImageDiscoveryResult struct {
	PosterPath         string
	PosterSourcePath   string
	BackdropPath       string
	BackdropSourcePath string
	Images             []RemoteImage
}

// DiscoverLibraryItemImages queries the configured image providers for a
// destination library and applies the best poster/backdrop to a copy of item
// without persisting it. The selected paths (and their source paths) are
// returned for the caller to carry into registration. Malformed or unsupported
// image URLs are dropped before selection, so they cannot replace valid
// artwork already on item.
func (s *MetadataService) DiscoverLibraryItemImages(
	ctx context.Context,
	folderID int,
	item *models.MediaItem,
	providerIDs map[string]string,
	contentType string,
	language string,
) (string, string, []RemoteImage, error) {
	result, err := s.discoverLibraryItemImages(ctx, folderID, item, providerIDs, contentType, language)
	if err != nil {
		return "", "", nil, err
	}
	return result.PosterPath, result.BackdropPath, result.Images, nil
}

// DiscoverLibraryItemImage is the struct-returning form of
// DiscoverLibraryItemImages.
func (s *MetadataService) DiscoverLibraryItemImage(
	ctx context.Context,
	folderID int,
	item *models.MediaItem,
	providerIDs map[string]string,
	contentType string,
	language string,
) (*LibraryImageDiscoveryResult, error) {
	return s.discoverLibraryItemImages(ctx, folderID, item, providerIDs, contentType, language)
}

// FetchLibraryItemImages and FetchLibraryItemImage are aliases for callers that
// prefer the Fetch* naming used by FetchLibraryMetadata.
func (s *MetadataService) FetchLibraryItemImages(
	ctx context.Context,
	folderID int,
	item *models.MediaItem,
	providerIDs map[string]string,
	contentType string,
	language string,
) (string, string, []RemoteImage, error) {
	return s.DiscoverLibraryItemImages(ctx, folderID, item, providerIDs, contentType, language)
}

func (s *MetadataService) FetchLibraryItemImage(
	ctx context.Context,
	folderID int,
	item *models.MediaItem,
	providerIDs map[string]string,
	contentType string,
	language string,
) (*LibraryImageDiscoveryResult, error) {
	return s.DiscoverLibraryItemImage(ctx, folderID, item, providerIDs, contentType, language)
}

func (s *MetadataService) discoverLibraryItemImages(
	ctx context.Context,
	folderID int,
	item *models.MediaItem,
	providerIDs map[string]string,
	contentType string,
	language string,
) (*LibraryImageDiscoveryResult, error) {
	if s == nil {
		return nil, errors.New("metadata service is required")
	}
	if folderID <= 0 {
		return nil, errors.New("library image discovery requires a destination folder")
	}
	if item == nil {
		return nil, errors.New("library image discovery requires an item")
	}
	if contentType == "" {
		contentType = item.Type
	}
	if contentType != anchoredItemTypeMovie && contentType != anchoredItemTypeSeries {
		return nil, errors.New("library image discovery requires movie or series content type")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	images, _, err := s.FetchItemImages(ctx, providerIDs, contentType, language, folderID)
	if err != nil {
		return nil, err
	}

	filtered := make([]RemoteImage, 0, len(images))
	for _, image := range images {
		if validLibraryImageURL(image.URL) {
			filtered = append(filtered, image)
		}
	}

	target := *item
	applyBestImages(&target, filtered, MergeReplaceUnlocked, language)
	applyLibraryImageSourcePaths(&target)

	item.PosterPath = target.PosterPath
	item.PosterSourcePath = target.PosterSourcePath
	item.PosterThumbhash = target.PosterThumbhash
	item.BackdropPath = target.BackdropPath
	item.BackdropSourcePath = target.BackdropSourcePath
	item.BackdropThumbhash = target.BackdropThumbhash
	item.LogoPath = target.LogoPath
	item.LogoSourcePath = target.LogoSourcePath

	return &LibraryImageDiscoveryResult{
		PosterPath:         target.PosterPath,
		PosterSourcePath:   target.PosterSourcePath,
		BackdropPath:       target.BackdropPath,
		BackdropSourcePath: target.BackdropSourcePath,
		Images:             filtered,
	}, nil
}

// EnqueueLibraryItemImages queues cache jobs for the artwork already selected
// for a persisted catalog item. It wraps enqueueItemImages and is non-fatal:
// callers should log and continue when it returns an error.
func (s *MetadataService) EnqueueLibraryItemImages(
	ctx context.Context,
	item *models.MediaItem,
	providerIDs map[string]string,
	images []RemoteImage,
) error {
	if s == nil || item == nil {
		return nil
	}
	if err := s.enqueueItemImages(ctx, item, providerIDs, images); err != nil {
		slog.WarnContext(ctx, "library image cache enqueue failed", "component", "metadata",
			"content_id", item.ContentID, "error", err)
		return err
	}
	return nil
}

// EnqueueLibraryItemImage is an alias for EnqueueLibraryItemImages.
func (s *MetadataService) EnqueueLibraryItemImage(
	ctx context.Context,
	item *models.MediaItem,
	providerIDs map[string]string,
	images []RemoteImage,
) error {
	return s.EnqueueLibraryItemImages(ctx, item, providerIDs, images)
}

// EnqueueLibraryItemImageCache is another alias for callers that want the cache
// wording explicit.
func (s *MetadataService) EnqueueLibraryItemImageCache(
	ctx context.Context,
	item *models.MediaItem,
	providerIDs map[string]string,
	images []RemoteImage,
) error {
	return s.EnqueueLibraryItemImages(ctx, item, providerIDs, images)
}

func applyLibraryImageSourcePaths(item *models.MediaItem) {
	if item == nil {
		return
	}
	for _, field := range itemArtworkFields(item) {
		path := strings.TrimSpace(*field.path)
		if path == "" {
			continue
		}
		if isLocalImageSourcePath(path) {
			*field.source = path
			*field.path = ""
			continue
		}
		if isCachedImagePath(path) {
			if *field.source == "" {
				*field.source = ""
			}
			continue
		}
		if isRemoteImageSourcePath(path) {
			*field.source = path
			continue
		}
		// Unsupported or malformed scheme: drop it so it cannot be served or
		// queued, preserving any prior valid artwork handled by the caller.
		*field.path = ""
		*field.source = ""
	}
}

func validLibraryImageURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		// Require a host so "https://" alone is rejected.
		trimmed := raw
		if i := strings.Index(trimmed, "://"); i >= 0 {
			trimmed = trimmed[i+3:]
		}
		trimmed = strings.TrimLeft(trimmed, "/")
		return trimmed != "" && !strings.ContainsAny(string(trimmed[0]), " \t")
	}
	if isLocalImageSourcePath(raw) {
		return true
	}
	// A provider or plugin scheme must be syntactically valid and name an image
	// resolver this server supports. Check this before isCachedImagePath, which
	// accepts almost any non-URL string and would otherwise treat a malformed
	// "bad scheme://..." as a storage key.
	if strings.Contains(raw, "://") {
		i := strings.Index(raw, "://")
		if i <= 0 {
			return false
		}
		scheme := raw[:i]
		rest := strings.TrimSpace(raw[i+3:])
		return validLibraryImageScheme(scheme) && ValidImageResolverScheme(scheme) && rest != ""
	}
	if isCachedImagePath(raw) {
		return true
	}
	// A bare cached path with no scheme is acceptable.
	return !strings.ContainsAny(raw, " \t")
}

// validLibraryImageScheme accepts an RFC 3986 scheme: a lowercase letter
// followed by letters, digits, '+', '-', or '.'. ValidImageResolverScheme
// deliberately allows any trimmed lowercase token because plugin schemes are
// dynamic, but artwork discovery must reject malformed schemes such as
// "bad scheme" before treating them as a provider image path.
func validLibraryImageScheme(scheme string) bool {
	if scheme == "" || scheme != strings.ToLower(scheme) {
		return false
	}
	for i, r := range scheme {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9', r == '+', r == '-', r == '.':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
