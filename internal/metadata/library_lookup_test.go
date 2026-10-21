package metadata

import (
	"context"
	"errors"
	"fmt"
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

// libraryLookupFailingProvider answers the season and episode requests with an
// error, so a provider failure can be told from an empty answer.
type libraryLookupFailingProvider struct {
	targetRefreshProvider
	err error
}

func (provider *libraryLookupFailingProvider) GetSeasons(context.Context, SeasonsRequest) ([]SeasonResult, error) {
	return nil, provider.err
}

func (provider *libraryLookupFailingProvider) GetEpisodes(context.Context, EpisodesRequest) ([]EpisodeResult, error) {
	return nil, provider.err
}

// libraryLookupChains installs the given providers for every chain level of
// library 42.
func libraryLookupChains(providers ...Provider) *testHarness {
	harness := newTestHarness()
	harness.service.chainCache = make(map[string]chainCacheEntry)
	for _, level := range []string{"movie", "series", "season", "episode"} {
		harness.service.chainCache["42:"+level] = chainCacheEntry{providers: providers, expiresAt: time.Now().Add(time.Hour)}
	}
	return harness
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
		harness := libraryLookupChains(provider)
		lookup, err := harness.service.FetchLibraryMetadata(context.Background(), 42, &models.MediaItem{Type: "movie", TmdbID: "123"}, nil)
		if conflicting {
			if !errors.Is(err, ErrMetadataNotFound) || lookup != nil {
				t.Fatalf("conflicting identity accepted: lookup=%+v error=%v", lookup, err)
			}
			continue
		}
		if err != nil || lookup == nil || lookup.Metadata == nil || !lookup.Metadata.HasMetadata ||
			lookup.Metadata.Title != "Provider title" || lookup.Metadata.PosterPath != "tmdb://poster.jpg" ||
			lookup.Metadata.BackdropPath != "tmdb://backdrop.jpg" || len(lookup.Episodes) != 0 {
			t.Fatalf("movie metadata = %+v, episodes=%v, error=%v", lookup, lookup.Episodes, err)
		}
	}
}

// TestFetchLibraryMetadataRejectsConflictWithAccumulatedIdentity covers a
// conflict the second provider introduces: the first provider's answer is
// folded in, so the second provider's differing canonical ID contradicts the
// identity the lookup already trusts and its metadata is not folded in.
func TestFetchLibraryMetadataRejectsConflictWithAccumulatedIdentity(t *testing.T) {
	first := &libraryLookupProvider{result: &MetadataResult{
		HasMetadata: true, Title: "First title", ProviderIDs: map[string]string{"tmdb": "123"},
	}}
	first.slug = "first"
	second := &libraryLookupProvider{result: &MetadataResult{
		HasMetadata: true, Title: "Second title", ProviderIDs: map[string]string{"tmdb": "456"},
	}}
	second.slug = "second"
	harness := libraryLookupChains(first, second)

	lookup, err := harness.service.FetchLibraryMetadata(context.Background(), 42, &models.MediaItem{Type: "movie", ImdbID: "tt1"}, nil)
	if err != nil || lookup == nil || lookup.Metadata == nil {
		t.Fatalf("lookup = %+v, error = %v", lookup, err)
	}
	if lookup.Metadata.Title != "First title" {
		t.Fatalf("title = %q, want the first provider's title", lookup.Metadata.Title)
	}
	if lookup.Metadata.ProviderIDs["tmdb"] != "123" {
		t.Fatalf("tmdb id = %q, want 123", lookup.Metadata.ProviderIDs["tmdb"])
	}
}

// TestFetchLibraryMetadataEmptyChainIsConfigurationFailure covers a library
// without a provider chain: that is a configuration failure an operator has
// to fix, not a transient miss.
func TestFetchLibraryMetadataEmptyChainIsConfigurationFailure(t *testing.T) {
	harness := newTestHarness()
	harness.service.chainCache = map[string]chainCacheEntry{
		"42:movie": {providers: nil, expiresAt: time.Now().Add(time.Hour)},
	}
	lookup, err := harness.service.FetchLibraryMetadata(context.Background(), 42, &models.MediaItem{Type: "movie", TmdbID: "123"}, nil)
	if !errors.Is(err, ErrMetadataNotConfigured) || lookup != nil {
		t.Fatalf("empty chain = %+v, error = %v", lookup, err)
	}
}

// TestFetchLibraryMetadataEmptySeason covers the difference between a season
// that has not aired yet and a season a provider answered incompletely. An
// empty future season is a normal answer; a released season that answers
// empty leaves the snapshot incomplete.
func TestFetchLibraryMetadataEmptySeason(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		airDate      string
		emptyAll     bool
		wantError    bool
		wantComplete bool
		wantEpisodes int
	}{
		{name: "future season", airDate: time.Now().AddDate(1, 0, 0).Format(time.DateOnly), wantComplete: true, wantEpisodes: 1},
		{name: "released season", airDate: "2000-01-01", wantEpisodes: 1},
		{name: "undated season", wantEpisodes: 1},
		{name: "no usable episodes", airDate: time.Now().AddDate(1, 0, 0).Format(time.DateOnly), emptyAll: true, wantError: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			provider := &libraryLookupProvider{
				result: &MetadataResult{HasMetadata: true, Title: "Series", ProviderIDs: map[string]string{"tmdb": "123"}},
				targetRefreshProvider: targetRefreshProvider{
					seasons:          []SeasonResult{{SeasonNumber: 1}, {SeasonNumber: 2, AirDate: testCase.airDate}},
					episodesBySeason: map[int][]EpisodeResult{1: {{SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot"}}},
				},
			}
			if testCase.emptyAll {
				provider.seasons = provider.seasons[1:]
			}
			harness := libraryLookupChains(provider)
			lookup, err := harness.service.FetchLibraryMetadata(context.Background(), 42, &models.MediaItem{Type: "series", TmdbID: "123"}, nil)
			if testCase.wantError {
				if !errors.Is(err, ErrMetadataNotFound) || lookup != nil {
					t.Fatalf("incomplete series accepted: lookup=%+v error=%v", lookup, err)
				}
				return
			}
			if err != nil || lookup == nil || lookup.Metadata == nil {
				t.Fatalf("lookup = %+v, error = %v", lookup, err)
			}
			if len(lookup.Episodes) != testCase.wantEpisodes {
				t.Fatalf("episodes = %d, want %d", len(lookup.Episodes), testCase.wantEpisodes)
			}
			if lookup.Complete != testCase.wantComplete {
				t.Fatalf("complete = %t, want %t", lookup.Complete, testCase.wantComplete)
			}
		})
	}
}

// TestFetchLibraryMetadataProviderFailureIsIncomplete covers a provider that
// fails: the episodes that did arrive are returned, and the snapshot says it
// is not authoritative, so the caller keeps the episodes it already holds.
func TestFetchLibraryMetadataProviderFailureIsIncomplete(t *testing.T) {
	provider := &libraryLookupFailingProvider{
		targetRefreshProvider: targetRefreshProvider{slug: "failing"},
		err:                   errors.New("provider is unavailable"),
	}
	harness := libraryLookupChains(provider)
	lookup, err := harness.service.FetchLibraryMetadata(context.Background(), 42, &models.MediaItem{Type: "series", TmdbID: "123"}, nil)
	if !errors.Is(err, ErrMetadataNotFound) {
		t.Fatalf("error = %v, want ErrMetadataNotFound", err)
	}
	if lookup != nil {
		t.Fatalf("lookup = %+v, want none", lookup)
	}
}

// TestFetchLibraryMetadataSkipsKnownSeasons covers the incremental refresh:
// the seasons the caller already holds are not fetched again, so a refresh
// spends its budget on what is missing.
func TestFetchLibraryMetadataSkipsKnownSeasons(t *testing.T) {
	provider := &libraryLookupProvider{
		result: &MetadataResult{HasMetadata: true, Title: "Series", ProviderIDs: map[string]string{"tmdb": "123"}},
		targetRefreshProvider: targetRefreshProvider{
			seasons:          []SeasonResult{{SeasonNumber: 1}, {SeasonNumber: 2}},
			episodesBySeason: map[int][]EpisodeResult{1: {{SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot"}}},
		},
	}
	harness := libraryLookupChains(provider)

	lookup, err := harness.service.FetchLibraryMetadata(context.Background(), 42, &models.MediaItem{Type: "series", TmdbID: "123"}, []int{1})
	if err != nil || lookup == nil {
		t.Fatalf("lookup = %+v, error = %v", lookup, err)
	}
	if len(lookup.Episodes) != 0 {
		t.Fatalf("episodes = %d, want none: season 1 is already known", len(lookup.Episodes))
	}
	for _, requested := range provider.episodeRequests {
		if requested == 1 {
			t.Fatal("a known season was fetched again")
		}
	}
}

// TestFetchLibraryMetadataBoundsSeasonTraversal covers the traversal bound: a
// long-running show cannot be read in one lookup, so the lookup takes the
// seasons it can and reports the snapshot as incomplete.
func TestFetchLibraryMetadataBoundsSeasonTraversal(t *testing.T) {
	provider := &libraryLookupProvider{
		result:                &MetadataResult{HasMetadata: true, Title: "Series", ProviderIDs: map[string]string{"tmdb": "123"}},
		targetRefreshProvider: targetRefreshProvider{episodesBySeason: map[int][]EpisodeResult{}},
	}
	for season := 1; season <= libraryMaxSeasonsPerLookup+3; season++ {
		provider.seasons = append(provider.seasons, SeasonResult{SeasonNumber: season, AirDate: "2000-01-01"})
		provider.episodesBySeason[season] = []EpisodeResult{{SeasonNumber: season, EpisodeNumber: 1, Title: fmt.Sprintf("Episode %d", season)}}
	}
	harness := libraryLookupChains(provider)

	lookup, err := harness.service.FetchLibraryMetadata(context.Background(), 42, &models.MediaItem{Type: "series", TmdbID: "123"}, nil)
	if err != nil || lookup == nil {
		t.Fatalf("lookup = %+v, error = %v", lookup, err)
	}
	if len(lookup.Episodes) != libraryMaxSeasonsPerLookup {
		t.Fatalf("episodes = %d, want %d", len(lookup.Episodes), libraryMaxSeasonsPerLookup)
	}
	if lookup.Complete {
		t.Fatal("a bounded traversal reported a complete snapshot")
	}
}
