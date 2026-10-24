package watchstate

import (
	"context"
	"fmt"
	"strings"
)

// EpisodeAliasLookup resolves a concrete layout-alias episode row to the
// canonical episode the catalog displays. catalog.EpisodeAliasRepository
// implements it.
type EpisodeAliasLookup interface {
	ResolveCanonicalEpisodeIDByAlias(ctx context.Context, aliasEpisodeID string) (string, bool, error)
}

// CanonicalEpisodeTarget maps a watch-state target to its canonical episode.
// Movies, series, canonical episodes, and ids with no alias mapping are
// returned unchanged. A lookup failure is returned, never swallowed: callers
// must retry the write rather than record progress under a hidden alias
// identity that canonical reads ignore.
func CanonicalEpisodeTarget(ctx context.Context, lookup EpisodeAliasLookup, mediaItemID string) (string, error) {
	mediaItemID = strings.TrimSpace(mediaItemID)
	if lookup == nil || mediaItemID == "" {
		return mediaItemID, nil
	}
	canonicalID, found, err := lookup.ResolveCanonicalEpisodeIDByAlias(ctx, mediaItemID)
	if err != nil {
		return "", fmt.Errorf("resolving canonical episode for %q: %w", mediaItemID, err)
	}
	if found && strings.TrimSpace(canonicalID) != "" {
		return canonicalID, nil
	}
	return mediaItemID, nil
}
