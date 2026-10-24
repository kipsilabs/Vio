package virtuallibrary

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/Silo-Server/silo-server/internal/metadata"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/monitor"
)

// fakeLibraryMetadataResolver pages through a fixed season list the way the
// metadata service does: each call examines at most MaxSeasons seasons after
// AfterSeason and reports the last attempted season as the next cursor.
type fakeLibraryMetadataResolver struct {
	seasons map[int][]metadata.EpisodeResult
	order   []int
	// failAt is the 1-based call that returns its page alongside failErr.
	failAt  int
	failErr error
	// stall makes every call report no progress without completing, which
	// stands in for a provider that repeats the same cursor.
	stall bool

	calls []metadata.LibraryMetadataLookupOptions
}

func newFakeLibraryMetadataResolver(seasonCount int) *fakeLibraryMetadataResolver {
	resolver := &fakeLibraryMetadataResolver{
		seasons: make(map[int][]metadata.EpisodeResult, seasonCount),
		order:   make([]int, 0, seasonCount),
	}
	for season := 1; season <= seasonCount; season++ {
		for episode := 1; episode <= 2; episode++ {
			resolver.seasons[season] = append(resolver.seasons[season], metadata.EpisodeResult{
				SeasonNumber:  season,
				EpisodeNumber: episode,
				Title:         "Episode",
			})
		}
		resolver.order = append(resolver.order, season)
	}
	return resolver
}

func (f *fakeLibraryMetadataResolver) FetchLibraryMetadata(
	ctx context.Context,
	_ int,
	_ *models.MediaItem,
	opts metadata.LibraryMetadataLookupOptions,
) (*metadata.LibraryMetadataLookupResult, error) {
	f.calls = append(f.calls, opts)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := &metadata.LibraryMetadataLookupResult{
		Metadata: &metadata.MetadataResult{Title: "Series"},
	}
	if f.stall {
		result.NextSeason = opts.AfterSeason
		return result, nil
	}
	candidates := make([]int, 0, len(f.order))
	for _, season := range f.order {
		if season > opts.AfterSeason {
			candidates = append(candidates, season)
		}
	}
	limit := opts.MaxSeasons
	if limit <= 0 {
		limit = 1
	}
	batch := candidates
	if len(batch) > limit {
		batch = batch[:limit]
	}
	next := opts.AfterSeason
	for _, season := range batch {
		result.Episodes = append(result.Episodes, f.seasons[season]...)
		next = season
	}
	if len(candidates) <= limit {
		result.NextSeason = 0
		result.Complete = true
	} else {
		result.NextSeason = next
	}
	if f.failAt > 0 && len(f.calls) == f.failAt {
		result.Complete = false
		return result, f.failErr
	}
	return result, nil
}

func TestEnrichFromLibraryMetadataLoadsEverySeason(t *testing.T) {
	const seasonCount = 17
	resolver := newFakeLibraryMetadataResolver(seasonCount)
	s := &Service{}
	item := monitor.MonitoredMedia{
		Key:            "series:tvdb:999",
		MediaType:      "series",
		Title:          "Series",
		TVDBID:         "999",
		MediaFolderID:  7,
		MetadataSource: "tvdb",
	}

	enriched, err := s.enrichFromLibraryMetadata(context.Background(), slog.New(slog.DiscardHandler), resolver, item)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(enriched.Episodes) != seasonCount*2 {
		t.Fatalf("episodes = %d, want %d", len(enriched.Episodes), seasonCount*2)
	}
	seen := make(map[[2]int]bool, len(enriched.Episodes))
	for _, episode := range enriched.Episodes {
		seen[[2]int{episode.Season, episode.Episode}] = true
	}
	for season := 1; season <= seasonCount; season++ {
		for episode := 1; episode <= 2; episode++ {
			if !seen[[2]int{season, episode}] {
				t.Fatalf("season %d episode %d missing from merged result", season, episode)
			}
		}
	}
	if !enriched.MetadataComplete {
		t.Fatal("a finished traversal must mark the item complete")
	}
	if enriched.MetadataNextSeason != 0 {
		t.Fatalf("next season = %d, want 0 after a complete traversal", enriched.MetadataNextSeason)
	}
	if enriched.MediaFolderID != 7 {
		t.Fatalf("media folder = %d, want 7", enriched.MediaFolderID)
	}

	wantCursors := []int{0, 2, 4, 6, 8, 10, 12, 14, 16}
	if len(resolver.calls) != len(wantCursors) {
		t.Fatalf("resolver calls = %d, want %d", len(resolver.calls), len(wantCursors))
	}
	for i, want := range wantCursors {
		if resolver.calls[i].AfterSeason != want {
			t.Fatalf("call %d AfterSeason = %d, want %d", i+1, resolver.calls[i].AfterSeason, want)
		}
		if resolver.calls[i].MaxSeasons != libraryMetadataSeasonsPerCall {
			t.Fatalf("call %d MaxSeasons = %d, want %d", i+1, resolver.calls[i].MaxSeasons, libraryMetadataSeasonsPerCall)
		}
		if resolver.calls[i].PreferredProvider != "tvdb" {
			t.Fatalf("call %d PreferredProvider = %q, want tvdb", i+1, resolver.calls[i].PreferredProvider)
		}
	}
}

func TestEnrichFromLibraryMetadataKeepsPagesBeforeAnError(t *testing.T) {
	resolver := newFakeLibraryMetadataResolver(17)
	resolver.failAt = 3
	resolver.failErr = errors.Join(metadata.ErrLibraryMetadataIncomplete, errors.New("season 5 provider failed"))
	s := &Service{}
	item := monitor.MonitoredMedia{
		Key:           "series:tvdb:999",
		MediaType:     "series",
		Title:         "Series",
		TVDBID:        "999",
		MediaFolderID: 7,
	}

	enriched, err := s.enrichFromLibraryMetadata(context.Background(), slog.New(slog.DiscardHandler), resolver, item)
	if !errors.Is(err, metadata.ErrLibraryMetadataIncomplete) {
		t.Fatalf("enrich error = %v, want ErrLibraryMetadataIncomplete", err)
	}
	// The failing call still contributes its page; earlier pages must survive.
	if want := 6; len(enriched.Episodes) != want*2 {
		t.Fatalf("episodes = %d, want %d merged before the failure", len(enriched.Episodes), want*2)
	}
	if enriched.MetadataComplete {
		t.Fatal("a failed traversal must not mark the item complete")
	}
	seen := make(map[[2]int]bool, len(enriched.Episodes))
	for _, episode := range enriched.Episodes {
		if episode.Season > 6 {
			t.Fatalf("episode from unrequested season %d appeared", episode.Season)
		}
		seen[[2]int{episode.Season, episode.Episode}] = true
	}
	for season := 1; season <= 6; season++ {
		for episode := 1; episode <= 2; episode++ {
			if !seen[[2]int{season, episode}] {
				t.Fatalf("season %d episode %d missing after the mid-traversal error", season, episode)
			}
		}
	}
}

func TestEnrichFromLibraryMetadataStopsOnARepeatedCursor(t *testing.T) {
	resolver := newFakeLibraryMetadataResolver(17)
	resolver.stall = true
	s := &Service{}
	item := monitor.MonitoredMedia{
		Key:           "series:tvdb:999",
		MediaType:     "series",
		Title:         "Series",
		TVDBID:        "999",
		MediaFolderID: 7,
	}

	enriched, err := s.enrichFromLibraryMetadata(context.Background(), slog.New(slog.DiscardHandler), resolver, item)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(resolver.calls) != 1 {
		t.Fatalf("resolver calls = %d, want 1 for a cursor that never advances", len(resolver.calls))
	}
	if enriched.MetadataComplete {
		t.Fatal("a stalled traversal must not mark the item complete")
	}
}
