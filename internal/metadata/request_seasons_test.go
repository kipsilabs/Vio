package metadata

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// requestSeasonsProvider is the fake EpisodeProvider used by the
// FetchSeriesTVDBSeasons tests. It answers both GetSeasons and GetEpisodes from
// fixed tables and records every request, including the ones fetched
// concurrently.
type requestSeasonsProvider struct {
	slug             string
	seasons          []SeasonResult
	seasonsErr       error
	episodesBySeason map[int][]EpisodeResult
	episodesErr      map[int]error

	mu              sync.Mutex
	seasonRequests  []SeasonsRequest
	episodeRequests []EpisodesRequest
}

func (provider *requestSeasonsProvider) Slug() string       { return provider.slug }
func (provider *requestSeasonsProvider) Name() string       { return provider.slug }
func (provider *requestSeasonsProvider) ForTypes() []string { return []string{"series"} }

func (provider *requestSeasonsProvider) GetSeasons(ctx context.Context, req SeasonsRequest) ([]SeasonResult, error) {
	provider.mu.Lock()
	provider.seasonRequests = append(provider.seasonRequests, req)
	provider.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if provider.seasonsErr != nil {
		return nil, provider.seasonsErr
	}
	return append([]SeasonResult(nil), provider.seasons...), nil
}

func (provider *requestSeasonsProvider) GetEpisodes(ctx context.Context, req EpisodesRequest) ([]EpisodeResult, error) {
	provider.mu.Lock()
	provider.episodeRequests = append(provider.episodeRequests, req)
	provider.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := provider.episodesErr[req.SeasonNumber]; err != nil {
		return nil, err
	}
	return append([]EpisodeResult(nil), provider.episodesBySeason[req.SeasonNumber]...), nil
}

func (provider *requestSeasonsProvider) recordedEpisodeSeasons() []int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	seasons := make([]int, 0, len(provider.episodeRequests))
	for _, req := range provider.episodeRequests {
		seasons = append(seasons, req.SeasonNumber)
	}
	slices.Sort(seasons)
	return seasons
}

func (provider *requestSeasonsProvider) seasonRequestCount() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return len(provider.seasonRequests)
}

// requestSeasonsMetadataProvider is a chain member that carries a slug but does
// not implement EpisodeProvider.
type requestSeasonsMetadataProvider struct {
	slug string
}

func (provider requestSeasonsMetadataProvider) Slug() string       { return provider.slug }
func (provider requestSeasonsMetadataProvider) Name() string       { return provider.slug }
func (provider requestSeasonsMetadataProvider) ForTypes() []string { return []string{"series"} }
func (provider requestSeasonsMetadataProvider) GetMetadata(context.Context, MetadataRequest) (*MetadataResult, error) {
	return nil, nil
}

// requestSeasonsService seeds the cached season chain directly, the way the
// request page would find it after admin configuration has been resolved.
func requestSeasonsService(providers ...Provider) *MetadataService {
	return &MetadataService{
		chainCache: map[string]chainCacheEntry{
			"0:season": {providers: providers, expiresAt: time.Now().Add(time.Hour)},
		},
		chainCacheTTL: time.Hour,
	}
}

func TestFetchSeriesTVDBSeasonsSelectsTVDBOverTMDB(t *testing.T) {
	tmdb := &requestSeasonsProvider{
		slug: "tmdb",
		seasons: []SeasonResult{
			{SeasonNumber: 1, Title: "TMDB Season 1"},
		},
		episodesBySeason: map[int][]EpisodeResult{
			1: {{SeasonNumber: 1, EpisodeNumber: 1}, {SeasonNumber: 1, EpisodeNumber: 2}},
		},
	}
	tvdb := &requestSeasonsProvider{
		slug: "tvdb",
		seasons: []SeasonResult{
			{SeasonNumber: 1, Title: "TVDB Season 1"},
			{SeasonNumber: 2, Title: "TVDB Season 2"},
		},
		episodesBySeason: map[int][]EpisodeResult{
			1: {{SeasonNumber: 1, EpisodeNumber: 1}},
			2: {{SeasonNumber: 2, EpisodeNumber: 1}, {SeasonNumber: 2, EpisodeNumber: 2},
				{SeasonNumber: 2, EpisodeNumber: 3}},
		},
	}
	// TMDB is first in the chain: selection must happen by slug, not by order.
	service := requestSeasonsService(tmdb, tvdb)

	seasons, err := service.FetchSeriesTVDBSeasons(context.Background(), 123, "en")
	if err != nil {
		t.Fatalf("FetchSeriesTVDBSeasons: %v", err)
	}
	if len(seasons) != 2 {
		t.Fatalf("seasons = %v, want the two TVDB seasons", seasons)
	}
	if seasons[0].Title != "TVDB Season 1" || seasons[1].Title != "TVDB Season 2" {
		t.Fatalf("seasons = %v, want TVDB titles, not merged TMDB data", seasons)
	}
	if len(seasons[0].Episodes) != 1 || len(seasons[1].Episodes) != 3 {
		t.Fatalf("episode counts = %d, %d; want 1 and 3 from TVDB",
			len(seasons[0].Episodes), len(seasons[1].Episodes))
	}
	if tmdb.seasonRequestCount() != 0 || len(tmdb.recordedEpisodeSeasons()) != 0 {
		t.Fatalf("TMDB provider was queried: seasons=%d episodes=%v",
			tmdb.seasonRequestCount(), tmdb.recordedEpisodeSeasons())
	}
	if len(tvdb.seasonRequests) != 1 {
		t.Fatalf("TVDB season requests = %d, want 1", len(tvdb.seasonRequests))
	}
	seasonReq := tvdb.seasonRequests[0]
	if seasonReq.ProviderIDs["tvdb"] != "123" || seasonReq.ContentType != "series" || seasonReq.Language != "en" {
		t.Fatalf("TVDB season request = %+v", seasonReq)
	}
	if len(tvdb.episodeRequests) != 2 {
		t.Fatalf("TVDB episode requests = %d, want 2", len(tvdb.episodeRequests))
	}
	for _, req := range tvdb.episodeRequests {
		if req.ProviderIDs["tvdb"] != "123" || req.Language != "en" {
			t.Fatalf("TVDB episode request = %+v", req)
		}
	}
}

func TestFetchSeriesTVDBSeasonsSortsSeasonsAndCountsEpisodes(t *testing.T) {
	tvdb := &requestSeasonsProvider{
		slug: "tvdb",
		seasons: []SeasonResult{
			{SeasonNumber: 3, Title: "Season 3"},
			{SeasonNumber: 1, Title: "Season 1"},
			{SeasonNumber: 2, Title: "Season 2"},
		},
		episodesBySeason: map[int][]EpisodeResult{
			1: {
				{SeasonNumber: 1, EpisodeNumber: 1},
				{SeasonNumber: 1, EpisodeNumber: 2},
				{SeasonNumber: 1, EpisodeNumber: 0},
				{SeasonNumber: 2, EpisodeNumber: 5},
			},
			2: {{SeasonNumber: 2, EpisodeNumber: 1}},
			3: {
				{SeasonNumber: 3, EpisodeNumber: 1},
				{SeasonNumber: 3, EpisodeNumber: 2},
				{SeasonNumber: 3, EpisodeNumber: 3},
			},
		},
	}

	seasons, err := requestSeasonsService(tvdb).FetchSeriesTVDBSeasons(context.Background(), 456, "en")
	if err != nil {
		t.Fatalf("FetchSeriesTVDBSeasons: %v", err)
	}
	if len(seasons) != 3 {
		t.Fatalf("seasons = %v, want 3", seasons)
	}
	for i, want := range []int{1, 2, 3} {
		if seasons[i].SeasonNumber != want {
			t.Fatalf("season %d number = %d, want %d", i, seasons[i].SeasonNumber, want)
		}
	}
	for i, want := range []int{2, 1, 3} {
		if len(seasons[i].Episodes) != want {
			t.Fatalf("season %d episodes = %v, want %d kept", seasons[i].SeasonNumber, seasons[i].Episodes, want)
		}
	}
	if episodes := seasons[0].Episodes; len(episodes) != 2 ||
		episodes[0].EpisodeNumber != 1 || episodes[1].EpisodeNumber != 2 {
		t.Fatalf("season 1 episodes = %v, want numbered episodes 1 and 2", episodes)
	}
	if got := tvdb.recordedEpisodeSeasons(); !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("episode fetches = %v, want one per numbered season", got)
	}
}

func TestFetchSeriesTVDBSeasonsSkipsSpecialsEpisodes(t *testing.T) {
	tvdb := &requestSeasonsProvider{
		slug: "tvdb",
		seasons: []SeasonResult{
			{SeasonNumber: 1, Title: "Season 1"},
			{SeasonNumber: 0, Title: "Specials"},
		},
		episodesBySeason: map[int][]EpisodeResult{
			0: {{SeasonNumber: 0, EpisodeNumber: 1}},
			1: {{SeasonNumber: 1, EpisodeNumber: 1}},
		},
	}

	seasons, err := requestSeasonsService(tvdb).FetchSeriesTVDBSeasons(context.Background(), 789, "en")
	if err != nil {
		t.Fatalf("FetchSeriesTVDBSeasons: %v", err)
	}
	if len(seasons) != 2 || seasons[0].SeasonNumber != 0 || seasons[1].SeasonNumber != 1 {
		t.Fatalf("seasons = %v, want specials first then season 1", seasons)
	}
	if got := tvdb.recordedEpisodeSeasons(); !slices.Equal(got, []int{1}) {
		t.Fatalf("episode fetches = %v, want season 1 only", got)
	}
	if len(seasons[0].Episodes) != 0 {
		t.Fatalf("specials gained episodes: %v", seasons[0].Episodes)
	}
	if len(seasons[1].Episodes) != 1 {
		t.Fatalf("season 1 episodes = %v, want 1", seasons[1].Episodes)
	}
}

func TestFetchSeriesTVDBSeasonsWithoutTVDBProvider(t *testing.T) {
	tmdb := &requestSeasonsProvider{
		slug:             "tmdb",
		seasons:          []SeasonResult{{SeasonNumber: 1, Title: "TMDB Season 1"}},
		episodesBySeason: map[int][]EpisodeResult{1: {{SeasonNumber: 1, EpisodeNumber: 1}}},
	}

	for _, testCase := range []struct {
		name      string
		providers []Provider
	}{
		{name: "chain has only TMDB", providers: []Provider{tmdb}},
		{name: "TVDB provider is not an episode provider", providers: []Provider{
			requestSeasonsMetadataProvider{slug: "tvdb"},
		}},
		{name: "chain is empty", providers: nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			seasons, err := requestSeasonsService(testCase.providers...).
				FetchSeriesTVDBSeasons(context.Background(), 123, "en")
			if err == nil {
				t.Fatalf("expected an error, got seasons %v", seasons)
			}
			if !errors.Is(err, ErrMetadataNotFound) {
				t.Fatalf("error = %v, want ErrMetadataNotFound", err)
			}
			if seasons != nil {
				t.Fatalf("seasons = %v, want nil", seasons)
			}
		})
	}
	if tmdb.seasonRequestCount() != 0 {
		t.Fatalf("TMDB seasons were queried as a fallback: %d", tmdb.seasonRequestCount())
	}
}

func TestFetchSeriesTVDBSeasonsPropagatesSeasonError(t *testing.T) {
	outage := errors.New("tvdb: HTTP 503")
	tvdb := &requestSeasonsProvider{slug: "tvdb", seasonsErr: outage}

	seasons, err := requestSeasonsService(tvdb).FetchSeriesTVDBSeasons(context.Background(), 123, "en")
	if !errors.Is(err, outage) {
		t.Fatalf("error = %v, want the provider error", err)
	}
	if seasons != nil {
		t.Fatalf("seasons = %v, want nil", seasons)
	}
}

func TestFetchSeriesTVDBSeasonsPropagatesEpisodeError(t *testing.T) {
	outage := errors.New("tvdb: season 2 unavailable")
	tvdb := &requestSeasonsProvider{
		slug: "tvdb",
		seasons: []SeasonResult{
			{SeasonNumber: 1},
			{SeasonNumber: 2},
			{SeasonNumber: 3},
		},
		episodesBySeason: map[int][]EpisodeResult{
			1: {{SeasonNumber: 1, EpisodeNumber: 1}},
			3: {{SeasonNumber: 3, EpisodeNumber: 1}},
		},
		episodesErr: map[int]error{2: outage},
	}

	seasons, err := requestSeasonsService(tvdb).FetchSeriesTVDBSeasons(context.Background(), 123, "en")
	if !errors.Is(err, outage) {
		t.Fatalf("error = %v, want the provider error", err)
	}
	if seasons != nil {
		t.Fatalf("seasons = %v, want no partial data", seasons)
	}
}

func TestFetchSeriesTVDBSeasonsReturnsEmptyForNoSeasons(t *testing.T) {
	tvdb := &requestSeasonsProvider{slug: "tvdb"}

	seasons, err := requestSeasonsService(tvdb).FetchSeriesTVDBSeasons(context.Background(), 123, "en")
	if err != nil {
		t.Fatalf("FetchSeriesTVDBSeasons: %v", err)
	}
	if seasons != nil {
		t.Fatalf("seasons = %v, want nil", seasons)
	}
	if got := tvdb.recordedEpisodeSeasons(); len(got) != 0 {
		t.Fatalf("episode fetches = %v, want none", got)
	}
}

func TestFetchSeriesTVDBSeasonsRejectsInvalidInput(t *testing.T) {
	service := requestSeasonsService(&requestSeasonsProvider{slug: "tvdb"})

	for _, tvdbID := range []int{0, -1} {
		seasons, err := service.FetchSeriesTVDBSeasons(context.Background(), tvdbID, "en")
		if err == nil {
			t.Fatalf("tvdb id %d: expected an error", tvdbID)
		}
		if seasons != nil {
			t.Fatalf("tvdb id %d: seasons = %v, want nil", tvdbID, seasons)
		}
	}

	var nilService *MetadataService
	if _, err := nilService.FetchSeriesTVDBSeasons(context.Background(), 123, "en"); err == nil {
		t.Fatal("nil service: expected an error")
	}
}

func TestFetchSeriesTVDBSeasonsReturnsContextError(t *testing.T) {
	tvdb := &requestSeasonsProvider{
		slug:    "tvdb",
		seasons: []SeasonResult{{SeasonNumber: 1}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	seasons, err := requestSeasonsService(tvdb).FetchSeriesTVDBSeasons(ctx, 123, "en")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if seasons != nil {
		t.Fatalf("seasons = %v, want nil", seasons)
	}
}
