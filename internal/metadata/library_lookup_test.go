package metadata

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

type libraryLookupProvider struct {
	targetRefreshProvider
	metadata     *MetadataResult
	metadataErr  error
	seasonsErr   error
	episodesErr  map[int]error
	metadataReqs []MetadataRequest
	onEpisodes   func(season int)
}

func (provider *libraryLookupProvider) GetMetadata(_ context.Context, req MetadataRequest) (*MetadataResult, error) {
	provider.metadataReqs = append(provider.metadataReqs, req)
	return provider.metadata, provider.metadataErr
}

func (provider *libraryLookupProvider) GetSeasons(ctx context.Context, req SeasonsRequest) ([]SeasonResult, error) {
	if provider.seasonsErr != nil {
		return nil, provider.seasonsErr
	}
	return provider.targetRefreshProvider.GetSeasons(ctx, req)
}

func (provider *libraryLookupProvider) GetEpisodes(ctx context.Context, req EpisodesRequest) ([]EpisodeResult, error) {
	provider.episodeRequests = append(provider.episodeRequests, req.SeasonNumber)
	if err := provider.episodesErr[req.SeasonNumber]; err != nil {
		return nil, err
	}
	if provider.onEpisodes != nil {
		provider.onEpisodes(req.SeasonNumber)
	}
	return provider.targetRefreshProvider.GetEpisodes(ctx, req)
}

type libraryMetadataOnlyProvider struct {
	slug string
}

func (provider libraryMetadataOnlyProvider) Slug() string       { return provider.slug }
func (provider libraryMetadataOnlyProvider) Name() string       { return provider.slug }
func (provider libraryMetadataOnlyProvider) ForTypes() []string { return []string{"series"} }
func (provider libraryMetadataOnlyProvider) GetMetadata(context.Context, MetadataRequest) (*MetadataResult, error) {
	return nil, nil
}

func seedLibraryLookupChains(t *testing.T, harness *testHarness, folderID int, chains map[string][]Provider) {
	t.Helper()
	harness.service.chainCache = make(map[string]chainCacheEntry)
	expiresAt := time.Now().Add(time.Hour)
	for level, providers := range chains {
		harness.service.chainCache[fmt.Sprintf("%d:%s", folderID, level)] = chainCacheEntry{
			providers: providers,
			expiresAt: expiresAt,
		}
	}
}

func seriesLibraryLookupChains(provider Provider) map[string][]Provider {
	return map[string][]Provider{
		"series":  {provider},
		"season":  {provider},
		"episode": {provider},
	}
}

func TestFetchLibraryMetadataRejectsConflictsAgainstAccumulatedIDs(t *testing.T) {
	first := &libraryLookupProvider{metadata: &MetadataResult{
		HasMetadata: true, Title: "Accepted title", Overview: "accepted overview",
		ProviderIDs: map[string]string{"tmdb": "111"},
	}}
	conflicting := &libraryLookupProvider{metadata: &MetadataResult{
		HasMetadata: true, Title: "Intruder title", Overview: "intruder overview",
		ProviderIDs: map[string]string{"tmdb": "222"},
	}}
	extension := &libraryLookupProvider{metadata: &MetadataResult{
		HasMetadata: true, ProviderIDs: map[string]string{"tvdb": "9"},
	}}
	harness := newTestHarness()
	seedLibraryLookupChains(t, harness, 42, map[string][]Provider{"movie": {first, conflicting, extension}})

	result, err := harness.service.FetchLibraryMetadata(context.Background(), 42,
		&models.MediaItem{Type: "movie", ImdbID: "tt0000001"}, LibraryMetadataLookupOptions{})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	metadata := result.Metadata
	if metadata == nil || metadata.Title != "Accepted title" || metadata.Overview != "accepted overview" {
		t.Fatalf("conflicting response fields merged: %+v", metadata)
	}
	if metadata.ProviderIDs["tmdb"] != "111" || metadata.ProviderIDs["tvdb"] != "9" || metadata.ProviderIDs["imdb"] != "tt0000001" {
		t.Fatalf("accepted ids = %v", metadata.ProviderIDs)
	}
	if !result.Complete || result.NextSeason != 0 {
		t.Fatalf("movie progress = complete %v next %d", result.Complete, result.NextSeason)
	}
}

func TestFetchLibraryMetadataDoesNotMutateProvidersOrTrustedIDs(t *testing.T) {
	mutator := &libraryLookupProvider{metadata: &MetadataResult{HasMetadata: true, Title: "Mutator"}}
	mutator.targetRefreshProvider.slug = "mutator"
	// The provider writes into the request map it was handed and returns its
	// own map for the lookup to fold; neither may reach the trusted identity.
	mutatingProvider := &mutatingLibraryLookupProvider{inner: mutator}
	observer := &libraryLookupProvider{metadata: &MetadataResult{HasMetadata: true}}
	observer.targetRefreshProvider.slug = "observer"

	harness := newTestHarness()
	seedLibraryLookupChains(t, harness, 42, map[string][]Provider{"movie": {mutatingProvider, observer}})
	result, err := harness.service.FetchLibraryMetadata(context.Background(), 42,
		&models.MediaItem{Type: "movie", TmdbID: "123"}, LibraryMetadataLookupOptions{})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got := result.Metadata.ProviderIDs["tmdb"]; got != "123" {
		t.Fatalf("trusted tmdb id = %q, want 123", got)
	}
	if _, injected := result.Metadata.ProviderIDs["imdb"]; injected {
		t.Fatalf("provider request mutation reached accepted ids: %v", result.Metadata.ProviderIDs)
	}
	if len(observer.metadataReqs) != 1 || observer.metadataReqs[0].ProviderIDs["tmdb"] != "123" {
		t.Fatalf("second provider request = %+v", observer.metadataReqs)
	}
	if len(observer.metadataReqs[0].ProviderIDs) != 1 {
		t.Fatalf("second provider request gained ids: %v", observer.metadataReqs[0].ProviderIDs)
	}
	if len(mutator.metadata.ProviderIDs) != 0 {
		t.Fatalf("provider result mutated: %v", mutator.metadata.ProviderIDs)
	}
}

type mutatingLibraryLookupProvider struct {
	inner *libraryLookupProvider
}

func (provider *mutatingLibraryLookupProvider) Slug() string       { return provider.inner.Slug() }
func (provider *mutatingLibraryLookupProvider) Name() string       { return provider.inner.Name() }
func (provider *mutatingLibraryLookupProvider) ForTypes() []string { return provider.inner.ForTypes() }
func (provider *mutatingLibraryLookupProvider) GetMetadata(ctx context.Context, req MetadataRequest) (*MetadataResult, error) {
	req.ProviderIDs["imdb"] = "tt-injected"
	return provider.inner.GetMetadata(ctx, req)
}

func TestFetchLibraryMetadataEmptySeasonOutcomes(t *testing.T) {
	var providerErr = errors.New("provider unavailable")
	future := time.Now().AddDate(1, 0, 0).Format(time.DateOnly)
	for _, testCase := range []struct {
		name         string
		airDate      string
		failure      error
		episodeChain []Provider
		wantErr      bool
		wantSentinel error
		wantMessage  string
		wantComplete bool
		wantEpisodes int
	}{
		{name: "successful empty future season", airDate: future, wantComplete: true},
		{name: "released empty season", airDate: "2000-01-01", wantErr: true, wantSentinel: ErrMetadataNotFound},
		{name: "undated empty season", wantErr: true, wantSentinel: ErrMetadataNotFound},
		{name: "provider failure", airDate: future, failure: providerErr, wantErr: true, wantSentinel: providerErr},
		{
			name: "no eligible provider", airDate: future, wantErr: true, wantSentinel: ErrMetadataNotFound,
			wantMessage: "no episode provider configured",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			provider := &libraryLookupProvider{
				metadata:    &MetadataResult{HasMetadata: true, Title: "Series", ProviderIDs: map[string]string{"tmdb": "123"}},
				episodesErr: map[int]error{1: testCase.failure},
				targetRefreshProvider: targetRefreshProvider{
					seasons:          []SeasonResult{{SeasonNumber: 1, AirDate: testCase.airDate}},
					episodesBySeason: map[int][]EpisodeResult{},
				},
			}
			chains := map[string][]Provider{"series": {provider}, "season": {provider}, "episode": {provider}}
			if testCase.wantMessage != "" {
				chains["episode"] = []Provider{libraryMetadataOnlyProvider{slug: "metadata-only"}}
			}
			harness := newTestHarness()
			seedLibraryLookupChains(t, harness, 42, chains)
			result, err := harness.service.FetchLibraryMetadata(context.Background(), 42,
				&models.MediaItem{Type: "series", TmdbID: "123"}, LibraryMetadataLookupOptions{})
			if result == nil || result.Metadata == nil {
				t.Fatalf("partial series result missing: result=%+v error=%v", result, err)
			}
			if result.Complete != testCase.wantComplete {
				t.Fatalf("complete = %v, want %v (error=%v)", result.Complete, testCase.wantComplete, err)
			}
			if len(result.Episodes) != testCase.wantEpisodes {
				t.Fatalf("episodes = %v, want %d", result.Episodes, testCase.wantEpisodes)
			}
			if !testCase.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error, got result %+v", result)
			}
			if testCase.wantSentinel != nil && !errors.Is(err, testCase.wantSentinel) {
				t.Fatalf("error = %v, want %v", err, testCase.wantSentinel)
			}
			if testCase.wantMessage != "" && !strings.Contains(err.Error(), testCase.wantMessage) {
				t.Fatalf("error = %v, want message %q", err, testCase.wantMessage)
			}
		})
	}
}

func TestFetchLibraryMetadataKeepsPartialProgressAcrossSeasonFailures(t *testing.T) {
	provider := &libraryLookupProvider{
		metadata: &MetadataResult{HasMetadata: true, Title: "Series", ProviderIDs: map[string]string{"tmdb": "123"}},
		episodesErr: map[int]error{
			2: errors.New("season 2 unavailable"),
		},
		targetRefreshProvider: targetRefreshProvider{
			seasons: []SeasonResult{
				{SeasonNumber: 1, AirDate: "2001-01-01"},
				{SeasonNumber: 2, AirDate: "2002-01-01"},
				{SeasonNumber: 3, AirDate: "2003-01-01"},
			},
			episodesBySeason: map[int][]EpisodeResult{
				1: {{SeasonNumber: 1, EpisodeNumber: 1, Title: "One"}},
				3: {{SeasonNumber: 3, EpisodeNumber: 1, Title: "Three"}},
			},
		},
	}
	harness := newTestHarness()
	seedLibraryLookupChains(t, harness, 42, seriesLibraryLookupChains(provider))

	result, err := harness.service.FetchLibraryMetadata(context.Background(), 42,
		&models.MediaItem{Type: "series", TmdbID: "123"}, LibraryMetadataLookupOptions{MaxSeasons: 3})
	if err == nil {
		t.Fatal("expected an incomplete lookup error")
	}
	if !errors.Is(err, ErrLibraryMetadataIncomplete) {
		t.Fatalf("error = %v, want incomplete sentinel", err)
	}
	if result == nil || result.Metadata == nil {
		t.Fatalf("partial result lost: result=%+v error=%v", result, err)
	}
	if result.Complete {
		t.Fatal("failed season reported complete")
	}
	if len(result.Episodes) != 2 || result.Episodes[0].Title != "One" || result.Episodes[1].Title != "Three" {
		t.Fatalf("episodes = %v, want seasons 1 and 3", result.Episodes)
	}
	if result.NextSeason != 0 {
		t.Fatalf("next season = %d, want cursor reset after traversal", result.NextSeason)
	}
}

func TestFetchLibraryMetadataAdvancesCursorOneSeasonPerCall(t *testing.T) {
	seasons := make([]SeasonResult, 0, 100)
	episodesBySeason := make(map[int][]EpisodeResult, 100)
	for season := 1; season <= 100; season++ {
		seasons = append(seasons, SeasonResult{SeasonNumber: season, AirDate: "2000-01-01"})
		episodesBySeason[season] = []EpisodeResult{{SeasonNumber: season, EpisodeNumber: 1, Title: fmt.Sprintf("S%02dE01", season)}}
	}
	provider := &libraryLookupProvider{
		metadata: &MetadataResult{HasMetadata: true, Title: "Long Show", ProviderIDs: map[string]string{"tmdb": "123"}},
		targetRefreshProvider: targetRefreshProvider{
			seasons:          seasons,
			episodesBySeason: episodesBySeason,
		},
	}
	harness := newTestHarness()
	seedLibraryLookupChains(t, harness, 42, seriesLibraryLookupChains(provider))

	first, err := harness.service.FetchLibraryMetadata(context.Background(), 42,
		&models.MediaItem{Type: "series", TmdbID: "123"}, LibraryMetadataLookupOptions{})
	if err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	if len(first.Episodes) != 1 || first.Episodes[0].SeasonNumber != 1 {
		t.Fatalf("first call episodes = %v, want season 1 only", first.Episodes)
	}
	if first.NextSeason != 1 || first.Complete {
		t.Fatalf("first call progress = next %d complete %v", first.NextSeason, first.Complete)
	}

	second, err := harness.service.FetchLibraryMetadata(context.Background(), 42,
		&models.MediaItem{Type: "series", TmdbID: "123"}, LibraryMetadataLookupOptions{AfterSeason: first.NextSeason})
	if err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if len(second.Episodes) != 1 || second.Episodes[0].SeasonNumber != 2 || second.NextSeason != 2 {
		t.Fatalf("second call = episodes %v next %d", second.Episodes, second.NextSeason)
	}

	total := len(first.Episodes) + len(second.Episodes)
	cursor := second.NextSeason
	for calls := 2; !second.Complete && calls < 100; calls++ {
		next, err := harness.service.FetchLibraryMetadata(context.Background(), 42,
			&models.MediaItem{Type: "series", TmdbID: "123"}, LibraryMetadataLookupOptions{AfterSeason: cursor})
		if err != nil {
			t.Fatalf("call %d: %v", calls+1, err)
		}
		if len(next.Episodes) != 1 {
			t.Fatalf("call %d episodes = %v, want one season", calls+1, next.Episodes)
		}
		total += len(next.Episodes)
		cursor = next.NextSeason
		second = next
	}
	if !second.Complete || total != 100 {
		t.Fatalf("traversal finished complete=%v total episodes=%d", second.Complete, total)
	}
	if second.NextSeason != 0 {
		t.Fatalf("final next season = %d, want reset to 0", second.NextSeason)
	}
}

func TestFetchLibraryMetadataPreservesProgressOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := &libraryLookupProvider{
		metadata: &MetadataResult{HasMetadata: true, Title: "Series", ProviderIDs: map[string]string{"tmdb": "123"}},
		targetRefreshProvider: targetRefreshProvider{
			seasons: []SeasonResult{
				{SeasonNumber: 1, AirDate: "2001-01-01"},
				{SeasonNumber: 2, AirDate: "2002-01-01"},
				{SeasonNumber: 3, AirDate: "2003-01-01"},
			},
			episodesBySeason: map[int][]EpisodeResult{
				1: {{SeasonNumber: 1, EpisodeNumber: 1, Title: "One"}},
				2: {{SeasonNumber: 2, EpisodeNumber: 1, Title: "Two"}},
				3: {{SeasonNumber: 3, EpisodeNumber: 1, Title: "Three"}},
			},
		},
	}
	provider.onEpisodes = func(season int) {
		if season == 2 {
			cancel()
		}
	}
	harness := newTestHarness()
	seedLibraryLookupChains(t, harness, 42, seriesLibraryLookupChains(provider))

	result, err := harness.service.FetchLibraryMetadata(ctx, 42,
		&models.MediaItem{Type: "series", TmdbID: "123"}, LibraryMetadataLookupOptions{MaxSeasons: 3})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if result == nil || result.Metadata == nil {
		t.Fatalf("partial result lost on cancellation: result=%+v", result)
	}
	if result.Complete {
		t.Fatal("canceled lookup reported complete")
	}
	if len(result.Episodes) == 0 || result.Episodes[0].SeasonNumber != 1 {
		t.Fatalf("completed observations lost: %v", result.Episodes)
	}
	for _, episode := range result.Episodes {
		if episode.SeasonNumber == 3 {
			t.Fatalf("lookup continued past cancellation: %v", result.Episodes)
		}
	}
	if result.NextSeason != 1 {
		t.Fatalf("next season = %d, want resume before interrupted season", result.NextSeason)
	}
}

func TestFetchLibraryMetadataSkipsMalformedEpisodesAndMarksIncomplete(t *testing.T) {
	provider := &libraryLookupProvider{
		metadata: &MetadataResult{HasMetadata: true, Title: "Series", ProviderIDs: map[string]string{"tmdb": "123"}},
		targetRefreshProvider: targetRefreshProvider{
			seasons: []SeasonResult{{SeasonNumber: 1, AirDate: "2001-01-01"}},
			episodesBySeason: map[int][]EpisodeResult{1: {
				{SeasonNumber: 1, EpisodeNumber: 1, Title: "Valid"},
				{SeasonNumber: 1, EpisodeNumber: 0, Title: "Missing number"},
				{SeasonNumber: 1, EpisodeNumber: 2, Title: "Bad date", AirDate: "not-a-date"},
				{SeasonNumber: 1, EpisodeNumber: 3},
			}},
		},
	}
	harness := newTestHarness()
	seedLibraryLookupChains(t, harness, 42, seriesLibraryLookupChains(provider))

	result, err := harness.service.FetchLibraryMetadata(context.Background(), 42,
		&models.MediaItem{Type: "series", TmdbID: "123"}, LibraryMetadataLookupOptions{})
	if !errors.Is(err, ErrLibraryMetadataIncomplete) {
		t.Fatalf("error = %v, want incomplete sentinel", err)
	}
	if result == nil || len(result.Episodes) != 1 || result.Episodes[0].Title != "Valid" {
		t.Fatalf("valid siblings lost: result=%+v error=%v", result, err)
	}
	if result.Complete {
		t.Fatal("malformed observations reported complete")
	}
}
