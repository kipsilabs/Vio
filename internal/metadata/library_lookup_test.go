package metadata

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

type libraryLookupProvider struct {
	targetRefreshProvider
	result *MetadataResult
}

func (provider *libraryLookupProvider) GetMetadata(context.Context, MetadataRequest) (*MetadataResult, error) {
	return provider.result, nil
}

func TestFetchLibraryMetadataMovie(t *testing.T) {
	for _, conflicting := range []bool{false, true} {
		provider := &libraryLookupProvider{result: &MetadataResult{
			HasMetadata: true, Title: "Provider title", ProviderIDs: map[string]string{"tmdb": "123"},
			PosterPath: "tmdb://poster.jpg", BackdropPath: "tmdb://backdrop.jpg",
		}}
		if conflicting {
			provider.result.ProviderIDs["tmdb"] = "456"
		}
		harness := newTestHarness()
		harness.service.chainCache = make(map[string]chainCacheEntry)
		harness.service.chainCache["42:movie"] = chainCacheEntry{providers: []Provider{provider}, expiresAt: time.Now().Add(time.Hour)}
		result, episodes, err := harness.service.FetchLibraryMetadata(context.Background(), 42, &models.MediaItem{Type: "movie", TmdbID: "123"})
		if conflicting {
			if !errors.Is(err, ErrMetadataNotFound) || result != nil {
				t.Fatalf("conflicting identity accepted: result=%+v error=%v", result, err)
			}
			continue
		}
		if err != nil || result == nil || !result.HasMetadata || result.Title != "Provider title" || result.PosterPath != "tmdb://poster.jpg" || result.BackdropPath != "tmdb://backdrop.jpg" || len(episodes) != 0 {
			t.Fatalf("movie metadata = %+v, episodes=%v, error=%v", result, episodes, err)
		}
	}
}

func TestFetchLibraryMetadataEmptySeason(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		airDate   string
		emptyAll  bool
		wantError bool
	}{
		{name: "future season", airDate: time.Now().AddDate(1, 0, 0).Format(time.DateOnly)},
		{name: "released season", airDate: "2000-01-01", wantError: true},
		{name: "undated season", wantError: true},
		{name: "no usable episodes", airDate: time.Now().AddDate(1, 0, 0).Format(time.DateOnly), emptyAll: true, wantError: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			provider := &libraryLookupProvider{
				result: &MetadataResult{HasMetadata: true, Title: "Series", ProviderIDs: map[string]string{"tmdb": "123"}},
				targetRefreshProvider: targetRefreshProvider{
					seasons: []SeasonResult{{SeasonNumber: 1}, {SeasonNumber: 2, AirDate: testCase.airDate}},
					episodesBySeason: map[int][]EpisodeResult{1: {{SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot"}}},
				},
			}
			if testCase.emptyAll {
				provider.seasons = provider.seasons[1:]
			}
			harness := newTestHarness()
			harness.service.chainCache = make(map[string]chainCacheEntry)
			for _, level := range []string{"series", "season", "episode"} {
				harness.service.chainCache["42:"+level] = chainCacheEntry{providers: []Provider{provider}, expiresAt: time.Now().Add(time.Hour)}
			}
			result, episodes, err := harness.service.FetchLibraryMetadata(context.Background(), 42, &models.MediaItem{Type: "series", TmdbID: "123"})
			if testCase.wantError {
				if !errors.Is(err, ErrMetadataNotFound) || result != nil || episodes != nil {
					t.Fatalf("incomplete series accepted: result=%+v episodes=%v error=%v", result, episodes, err)
				}
				return
			}
			if err != nil || result == nil || !result.HasMetadata || len(episodes) != 1 || episodes[0].Title != "Pilot" {
				t.Fatalf("released episodes lost: result=%+v episodes=%v error=%v", result, episodes, err)
			}
		})
	}
}