package api

import (
	"context"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
)

// wireVirtualMediaResolver gives playback the core virtual-library resolver.
// Virtual URI resolution is core-owned: when the core service is unavailable,
// playback reports virtual media unavailable rather than dispatching the URI
// to an arbitrary enabled plugin.
func wireVirtualMediaResolver(playbackHandler *handlers.PlaybackHandler, deps Dependencies) {
	if playbackHandler == nil || deps.VirtualLibraryService == nil {
		return
	}
	playbackHandler.VirtualMediaResolver = handlers.VirtualMediaResolverFunc(func(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string) (string, error) {
		return deps.VirtualLibraryService.Resolve(ctx, virtualURI)
	})
}
