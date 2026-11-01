// internal/subtitles/provider.go
package subtitles

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Provider defines the interface that each subtitle source must implement.
type Provider interface {
	// Name returns the provider identifier (e.g., "opensubtitles", "subdl", "subsource").
	Name() string

	// Search queries the provider for subtitles matching the request.
	Search(ctx context.Context, req SearchRequest) ([]SubtitleResult, error)

	// Download fetches the subtitle file content by provider-specific ID. A
	// season-pack archive yields the entry for target's episode.
	Download(ctx context.Context, id string, target DownloadTarget) ([]byte, SubtitleFormat, error)
}

// ConnectionTester is implemented by providers whose Search does not validate
// their credentials. OpenSubtitles searches with a shared default API key, so
// a search proves nothing about the configured username/password; the admin
// connection test must call this instead to verify the account can actually
// authenticate.
type ConnectionTester interface {
	// TestConnection verifies the provider's configured credentials against
	// the upstream. Returns nil when they authenticate, an error otherwise.
	TestConnection(ctx context.Context) error
}

// S3Client is the interface for S3 operations needed by the subtitle system.
// Defined here for testability — the concrete implementation is s3client.Client.
type S3Client interface {
	PutObject(ctx context.Context, bucket, key string, data []byte) error
	GetObject(ctx context.Context, bucket, key string) ([]byte, error)
	DeleteObject(ctx context.Context, bucket, key string) error
}

// BlobStore is the object storage the subtitle system needs. Subtitle objects
// are always read through the server, never by a redirect to storage, so this
// is key-only: no bucket, no signed URLs. blobstore.NewByteStore adapts either
// backend to it.
type BlobStore interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

// SameOriginRedirects is the redirect policy for provider API clients. A
// request follows redirects only on the scheme and host it started on, so a
// provider reply cannot send the account's key or login, or the server
// itself, to another host.
func SameOriginRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if !SameOrigin(req.URL, via[0].URL) || req.URL.User != nil {
		return fmt.Errorf("refused redirect off %s", via[0].URL.Host)
	}
	return nil
}

// SameOrigin reports whether a and b have the same scheme and host.
func SameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}
