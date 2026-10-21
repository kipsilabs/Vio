package metadata

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

func (s *MetadataService) FetchLibraryMetadata(ctx context.Context, folderID int, item *models.MediaItem) (*MetadataResult, []EpisodeResult, error) {
	if s == nil || folderID <= 0 || item == nil || (item.Type != "movie" && item.Type != "series") {
		return nil, nil, fmt.Errorf("library metadata requires a destination library and movie or series identity")
	}
	language := s.resolveFolderLanguage(ctx, folderID)
	providerIDs, err := s.resolveSeriesRefreshProviderIDs(ctx, item, folderID, language, true)
	if err != nil {
		return nil, nil, err
	}
	chain, err := s.resolveChainCached(ctx, folderID, item.Type)
	if err != nil {
		return nil, nil, err
	}
	result := &MetadataResult{ProviderIDs: maps.Clone(providerIDs)}
	for _, provider := range chain {
		metadataProvider, ok := provider.(MetadataProvider)
		if !ok {
			continue
		}
		value, err := metadataProvider.GetMetadata(ctx, MetadataRequest{
			ProviderIDs: result.ProviderIDs,
			ContentType: item.Type,
			Language:    language,
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			slog.WarnContext(ctx, "library metadata provider failed", "component", "metadata", "provider", provider.Slug(), "error", err)
			continue
		}
		if value == nil || !value.HasMetadata {
			continue
		}
		if searchResultConflictsWithTrustedIDs(providerIDs, value.ProviderIDs) {
			continue
		}
		_, isIdentityHinter := provider.(IdentityHintProvider)
		foldProviderResult(result, value, language, provider.Slug(), isIdentityHinter, nil)
		result.HasMetadata = true
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if !result.HasMetadata {
		return nil, nil, ErrMetadataNotFound
	}
	if item.Type != "series" {
		return result, nil, nil
	}
	seasons, _, err := s.fetchTargetSeasonResults(ctx, result.ProviderIDs, folderID, language, -1, seriesChildLocalContext{}, true)
	if err != nil {
		return nil, nil, err
	}
	var episodes []EpisodeResult
	for _, season := range seasons {
		if season.SeasonNumber <= 0 {
			continue
		}
		rows, _, err := s.fetchTargetEpisodeResults(ctx, result.ProviderIDs, folderID, language, season.SeasonNumber, 0, seriesChildLocalContext{}, true)
		if err != nil {
			return nil, nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if len(rows) == 0 {
			if airDate, err := time.Parse(time.DateOnly, season.AirDate); err == nil && airDate.After(time.Now()) {
				continue
			}
			return nil, nil, fmt.Errorf("season %d: %w", season.SeasonNumber, ErrMetadataNotFound)
		}
		episodes = append(episodes, rows...)
	}
	if len(episodes) == 0 {
		return nil, nil, ErrMetadataNotFound
	}
	return result, episodes, nil
}
