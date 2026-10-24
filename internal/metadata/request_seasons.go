package metadata

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"golang.org/x/sync/errgroup"
)

// requestSeasonsEpisodeConcurrency bounds how many seasons one
// FetchSeriesTVDBSeasons call fills in at once. A request-detail page needs
// every season of a show; fetching them one by one would make long series wait
// on a chain of remote calls, while unbounded fan-out would hammer the TVDB
// plugin.
const requestSeasonsEpisodeConcurrency = 4

// FetchSeriesTVDBSeasons returns the TVDB season list for a series together
// with the episodes of each numbered season, which the request-detail page uses
// to show season and episode counts before the series is in the catalog.
//
// It reads the season data from the enabled TVDB provider alone instead of
// merging the library's season chain. The chain also contains TMDB, which
// collapses long-running series into fewer seasons than TVDB lists; merging
// both would either hide TVDB seasons or attach one provider's episodes to the
// other provider's numbering. Season selection is by provider slug, so chain
// order and other providers cannot influence the result.
//
// Seasons are ordered by SeasonNumber. Specials (season 0) are returned but get
// no episode fetch. Any provider failure fails the whole call: the caller uses
// any error to fall back to TMDB, so partial TVDB data must not look complete.
func (s *MetadataService) FetchSeriesTVDBSeasons(ctx context.Context, tvdbID int, language string) ([]SeasonResult, error) {
	if s == nil {
		return nil, errors.New("fetch TVDB seasons: metadata service is nil")
	}
	if tvdbID <= 0 {
		return nil, fmt.Errorf("fetch TVDB seasons: invalid TVDB ID %d", tvdbID)
	}

	// No library folder ID: the request page resolves a show before it belongs
	// to a library, so the chain has to come from the server's default season
	// configuration.
	chain, err := s.resolveChainCached(ctx, 0, "season")
	if err != nil {
		return nil, fmt.Errorf("fetch TVDB seasons: resolving season provider chain: %w", err)
	}

	var episodeProvider EpisodeProvider
	for _, provider := range chain {
		if provider.Slug() != "tvdb" {
			continue
		}
		tvdbProvider, ok := provider.(EpisodeProvider)
		if !ok {
			continue
		}
		episodeProvider = tvdbProvider
		break
	}
	if episodeProvider == nil {
		return nil, fmt.Errorf("fetch TVDB seasons: no enabled TVDB episode provider: %w", ErrMetadataNotFound)
	}

	providerIDs := map[string]string{"tvdb": strconv.Itoa(tvdbID)}
	seasons, err := episodeProvider.GetSeasons(ctx, SeasonsRequest{
		ProviderIDs: providerIDs,
		ContentType: "series",
		Language:    language,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("fetch TVDB seasons for series %d: %w", tvdbID, err)
	}
	if len(seasons) == 0 {
		return nil, nil
	}

	// Copy before sorting and filling in episodes: the slice belongs to the
	// provider, and the caller gets the only mutable view of it.
	ordered := append([]SeasonResult(nil), seasons...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].SeasonNumber < ordered[j].SeasonNumber
	})

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(requestSeasonsEpisodeConcurrency)
	for i := range ordered {
		season := &ordered[i]
		if season.SeasonNumber <= 0 {
			// Specials have no reliable episode numbering and the request page
			// does not count them, so skip the remote call entirely.
			continue
		}
		group.Go(func() error {
			episodes, err := episodeProvider.GetEpisodes(groupCtx, EpisodesRequest{
				// A fresh map per call: providers may keep or mutate the request
				// they are handed.
				ProviderIDs:  map[string]string{"tvdb": strconv.Itoa(tvdbID)},
				SeasonNumber: season.SeasonNumber,
				Language:     language,
			})
			if err != nil {
				return fmt.Errorf("fetch TVDB episodes for season %d: %w", season.SeasonNumber, err)
			}
			kept := make([]EpisodeResult, 0, len(episodes))
			for _, episode := range episodes {
				if episode.EpisodeNumber <= 0 || episode.SeasonNumber != season.SeasonNumber {
					continue
				}
				kept = append(kept, episode)
			}
			season.Episodes = kept
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}

	return ordered, nil
}
