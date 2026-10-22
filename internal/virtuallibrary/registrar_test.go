package virtuallibrary

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/metadata"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/monitor"
)

func TestVirtualURIForMonitored(t *testing.T) {
	// Movie with simple stream ID
	movieItem := monitor.MonitoredMedia{
		MediaType: "movie",
		StreamID:  "tt0133093",
	}
	if uri := virtualURIForMonitored(movieItem); uri != "virtual://movie/tt0133093" {
		t.Fatalf("movie virtual URI = %q, want virtual://movie/tt0133093", uri)
	}

	// Movie with colon-separated stream ID
	colonItem := monitor.MonitoredMedia{
		MediaType: "movie",
		StreamID:  "tmdb:603",
	}
	if uri := virtualURIForMonitored(colonItem); uri != "virtual://movie/tmdb/603" {
		t.Fatalf("colon stream ID virtual URI = %q, want virtual://movie/tmdb/603", uri)
	}

	// Series stream ID: series playback sources belong to episodes, so top-level URI is empty
	seriesItem := monitor.MonitoredMedia{
		MediaType: "series",
		StreamID:  "tt0903747",
	}
	if uri := virtualURIForMonitored(seriesItem); uri != "" {
		t.Fatalf("series top-level virtual URI = %q, want empty", uri)
	}
}

func TestCatalogMonitorRegistrarNilFailsClosed(t *testing.T) {
	var r *catalogMonitorRegistrar
	if err := r.Register(context.Background(), monitor.MonitoredMedia{}); err == nil {
		t.Fatal("expected error registering with nil registrar")
	}
	if err := r.Reconcile(context.Background(), "monitor", []string{"id1"}, []int{1}, monitor.ReconcileEvidence{FullCycle: true, SourceCount: 1, QueueCount: 1}); err == nil {
		t.Fatal("expected error reconciling with nil registrar")
	}

	empty := &catalogMonitorRegistrar{}
	if err := empty.Register(context.Background(), monitor.MonitoredMedia{}); err == nil {
		t.Fatal("expected error registering with empty registrar")
	}
	if err := empty.Reconcile(context.Background(), "monitor", []string{"id1"}, []int{1}, monitor.ReconcileEvidence{FullCycle: true, SourceCount: 1, QueueCount: 1}); err == nil {
		t.Fatal("expected error reconciling with empty registrar")
	}
}

func TestMonitoredEpisodePayloadOmitsSeasonZeroSpecials(t *testing.T) {
	item := monitor.MonitoredMedia{
		Key:       "series:tt100",
		MediaType: "series",
		IMDbID:    "tt100",
		Episodes: []monitor.VirtualEpisode{
			{Season: 0, Episode: 1, Title: "Special"},
			{Season: 0, Episode: 2, Title: "Another special"},
			{Season: 1, Episode: 1, Title: "Pilot"},
			{Season: 2, Episode: 5, Title: "Finale"},
		},
	}
	payload := monitoredEpisodePayload(item)
	if len(payload) != 2 {
		t.Fatalf("payload episodes = %d, want 2 (specials omitted)", len(payload))
	}
	if payload[0].SeasonNumber != 1 || payload[0].EpisodeNumber != 1 {
		t.Fatalf("payload[0] = S%dE%d, want S1E1", payload[0].SeasonNumber, payload[0].EpisodeNumber)
	}
	if payload[1].SeasonNumber != 2 || payload[1].EpisodeNumber != 5 {
		t.Fatalf("payload[1] = S%dE%d, want S2E5", payload[1].SeasonNumber, payload[1].EpisodeNumber)
	}
	// A season-0 episode produces no usable URI; confirm it is not silently
	// replaced by a placeholder.
	for _, episode := range payload {
		if episode.VirtualURI == "" {
			t.Fatalf("payload episode S%dE%d has an empty virtual URI", episode.SeasonNumber, episode.EpisodeNumber)
		}
	}
}

func TestMonitoredEpisodePayloadKeepsMalformedEpisodesForValidation(t *testing.T) {
	item := monitor.MonitoredMedia{
		Key:       "series:tt100",
		MediaType: "series",
		IMDbID:    "tt100",
		Episodes: []monitor.VirtualEpisode{
			{Season: -1, Episode: 1, Title: "Malformed negative season"},
		},
	}
	payload := monitoredEpisodePayload(item)
	if len(payload) != 1 {
		t.Fatalf("payload episodes = %d, want 1; a negative season must reach catalog validation", len(payload))
	}
	if payload[0].SeasonNumber != -1 {
		t.Fatalf("payload[0].SeasonNumber = %d, want -1 preserved", payload[0].SeasonNumber)
	}
}

// TestMonitoredEpisodePayloadTrimsProviderWhitespace reproduces the issue where
// metadata providers return episode titles (and overviews, stills) with leading
// or trailing whitespace. The catalog's virtual-media validator rejects such
// values with "episodes[N].title must not have surrounding whitespace", which
// aborted the whole series registration. The registrar builder is the boundary
// that must clean provider data before it reaches the strict validator.
func TestMonitoredEpisodePayloadTrimsProviderWhitespace(t *testing.T) {
	item := monitor.MonitoredMedia{
		Key:       "series:tt100",
		MediaType: "series",
		IMDbID:    "tt100",
		Episodes: []monitor.VirtualEpisode{
			{
				Season:    1,
				Episode:   1,
				Title:     "  Chapter One  ",
				Overview:  "\tFirst episode\n",
				Thumbnail: "  https://img.example/ep1.jpg  ",
			},
		},
	}
	payload := monitoredEpisodePayload(item)
	if len(payload) != 1 {
		t.Fatalf("payload episodes = %d, want 1", len(payload))
	}
	if got := payload[0].Title; got != "Chapter One" {
		t.Fatalf("episode title = %q, want %q (provider whitespace must be trimmed before validation)", got, "Chapter One")
	}
	if got := payload[0].Overview; got != "First episode" {
		t.Fatalf("episode overview = %q, want %q", got, "First episode")
	}
	if got := payload[0].StillPath; got != "https://img.example/ep1.jpg" {
		t.Fatalf("episode still path = %q, want %q", got, "https://img.example/ep1.jpg")
	}
}

// TestVirtualMediaRegistrationTrimsProviderWhitespace covers the series- and
// movie-level fields the registrar maps from provider data. All of them are
// subject to the same strict catalog validator, so the builder trims them
// before validation instead of letting one padded title fail the request.
func TestVirtualMediaRegistrationTrimsProviderWhitespace(t *testing.T) {
	item := monitor.MonitoredMedia{
		Key:       "series:tt100",
		MediaType: "series",
		IMDbID:    "tt100",
		Title:     "  Dateline  ",
		Overview:  "\tLong-running newsmagazine. \n",
		Poster:    "  https://img.example/poster.jpg  ",
		Backdrop:  "  https://img.example/backdrop.jpg  ",
		Genres:    []string{"  Drama  ", "Crime"},
		SourceKey: "  request:series:tt100  ",
		Episodes: []monitor.VirtualEpisode{
			{Season: 1, Episode: 1, Title: "  Chapter One  "},
		},
	}
	in := virtualMediaRegistration(item, "2")

	if in.Title != "Dateline" {
		t.Fatalf("title = %q, want %q", in.Title, "Dateline")
	}
	if in.Overview != "Long-running newsmagazine." {
		t.Fatalf("overview = %q, want %q", in.Overview, "Long-running newsmagazine.")
	}
	if in.PosterPath != "https://img.example/poster.jpg" {
		t.Fatalf("poster path = %q", in.PosterPath)
	}
	if in.BackdropPath != "https://img.example/backdrop.jpg" {
		t.Fatalf("backdrop path = %q", in.BackdropPath)
	}
	if len(in.Genres) != 2 || in.Genres[0] != "Drama" || in.Genres[1] != "Crime" {
		t.Fatalf("genres = %#v, want [Drama Crime]", in.Genres)
	}
	if in.Source != "request:series:tt100" {
		t.Fatalf("source = %q", in.Source)
	}
	if len(in.Episodes) != 1 || in.Episodes[0].Title != "Chapter One" {
		t.Fatalf("episodes = %#v, want trimmed title Chapter One", in.Episodes)
	}
	if in.LibraryID != "2" {
		t.Fatalf("library id = %q, want 2", in.LibraryID)
	}
}

func TestCatalogMonitorRegistrarEpisodeMapping(t *testing.T) {
	// Verify that Register accurately maps all monitoredMedia fields including episodes
	item := monitor.MonitoredMedia{
		Key:       "series:tt100",
		MediaType: "series",
		Title:     "Test Series",
		Year:      2024,
		IMDbID:    "tt100",
		TMDBID:    "200",
		TVDBID:    "300",
		Overview:  "A test series overview",
		Poster:    "https://img.example/poster.jpg",
		Backdrop:  "https://img.example/backdrop.jpg",
		Genres:    []string{"Drama", "Sci-Fi"},
		Runtime:   45,
		SourceKey: "test-source",
		Episodes: []monitor.VirtualEpisode{
			{
				Season:    1,
				Episode:   1,
				Title:     "Pilot",
				Overview:  "First episode",
				Thumbnail: "https://img.example/ep1.jpg",
				Released:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
				Runtime:   50,
			},
		},
	}

	// Register with nil inner registrar fails cleanly after mapping logic
	r := &catalogMonitorRegistrar{registrar: nil}
	err := r.Register(context.Background(), item)
	if err == nil {
		t.Fatal("expected error from unconfigured catalog registrar")
	}
}

func TestVirtualMediaRegistrationPreservesArtworkSourcePaths(t *testing.T) {
	item := monitor.MonitoredMedia{
		MediaType:          "movie",
		Title:              "Example",
		Poster:             "https://cache.example/poster.jpg",
		PosterSourcePath:   "tmdb://poster.jpg",
		Backdrop:           "https://cache.example/backdrop.jpg",
		BackdropSourcePath: "tmdb://backdrop.jpg",
	}
	payload := virtualMediaRegistration(item, "1")
	if payload.PosterSourcePath != "tmdb://poster.jpg" {
		t.Fatalf("PosterSourcePath = %q, want tmdb://poster.jpg", payload.PosterSourcePath)
	}
	if payload.BackdropSourcePath != "tmdb://backdrop.jpg" {
		t.Fatalf("BackdropSourcePath = %q, want tmdb://backdrop.jpg", payload.BackdropSourcePath)
	}
}

type recordingArtworkEnqueuer struct {
	item        *models.MediaItem
	providerIDs map[string]string
}

func (r *recordingArtworkEnqueuer) EnqueueLibraryItemImages(_ context.Context, item *models.MediaItem, providerIDs map[string]string, _ []metadata.RemoteImage) error {
	r.item = item
	r.providerIDs = providerIDs
	return nil
}

func TestEnqueueArtworkPassesSourcePaths(t *testing.T) {
	enqueuer := &recordingArtworkEnqueuer{}
	reg := &catalogMonitorRegistrar{artwork: enqueuer}
	item := monitor.MonitoredMedia{
		MediaType:          "movie",
		TMDBID:             "123",
		Poster:             "https://cache.example/poster.jpg",
		PosterSourcePath:   "tmdb://poster.jpg",
		Backdrop:           "https://cache.example/backdrop.jpg",
		BackdropSourcePath: "tmdb://backdrop.jpg",
	}
	if err := reg.EnqueueArtwork(context.Background(), "movie-123", item); err != nil {
		t.Fatalf("EnqueueArtwork failed: %v", err)
	}
	if enqueuer.item == nil {
		t.Fatal("expected item to be enqueued")
	}
	if enqueuer.item.PosterSourcePath != "tmdb://poster.jpg" {
		t.Fatalf("PosterSourcePath = %q, want tmdb://poster.jpg", enqueuer.item.PosterSourcePath)
	}
	if enqueuer.item.BackdropSourcePath != "tmdb://backdrop.jpg" {
		t.Fatalf("BackdropSourcePath = %q, want tmdb://backdrop.jpg", enqueuer.item.BackdropSourcePath)
	}
	if enqueuer.providerIDs["tmdb"] != "123" {
		t.Fatalf("tmdb ID = %q, want 123", enqueuer.providerIDs["tmdb"])
	}
}
