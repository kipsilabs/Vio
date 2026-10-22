package jellycompat

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/notifications"
	"github.com/Silo-Server/silo-server/internal/watchstate"
)

// seasonContentService lists the season's episodes that have a library file,
// as the catalog does.
type seasonContentService struct {
	stubContentService
	onDisk []string
	// seasonIDListings counts season-row listings; coordinateListings counts
	// the (series, season number) listing the durable season identity uses.
	seasonIDListings   int
	coordinateListings int
}

func (s *seasonContentService) episodes() []upstreamEpisode {
	episodes := make([]upstreamEpisode, 0, len(s.onDisk))
	for _, id := range s.onDisk {
		episodes = append(episodes, upstreamEpisode{ContentID: id})
	}
	return episodes
}

func (s *seasonContentService) ListEpisodesBySeasonID(context.Context, *Session, string, *int) ([]upstreamEpisode, error) {
	s.seasonIDListings++
	return s.episodes(), nil
}

func (s *seasonContentService) ListEpisodes(context.Context, *Session, string, int, *int) ([]upstreamEpisode, error) {
	s.coordinateListings++
	return s.episodes(), nil
}

// syntheticSeasonContentService models a season derived from episode groups
// with no seasons row: the detail lookup and the season-id listing miss, while
// the durable (series, season number) listing resolves the episodes.
type syntheticSeasonContentService struct {
	stubContentService
	seriesID     string
	seasonNumber int
	onDisk       []string
	// seasonIDListings must stay zero: the season-row listing returns the
	// catalog's not-found before the coordinate expansion runs.
	seasonIDListings   int
	coordinateListings int
}

func (s *syntheticSeasonContentService) GetItemDetail(context.Context, *Session, string, *int) (*upstreamItemDetail, error) {
	return nil, &HTTPError{StatusCode: 404, Message: "season not found"}
}

func (s *syntheticSeasonContentService) GetSeason(_ context.Context, _ *Session, seriesID string, seasonNumber int, _ *int) (*upstreamSeason, error) {
	if seriesID != s.seriesID || seasonNumber != s.seasonNumber {
		return nil, &HTTPError{StatusCode: 404, Message: "season not found"}
	}
	return &upstreamSeason{
		ContentID:    fmt.Sprintf("%s-S%02d", seriesID, seasonNumber),
		SeasonNumber: seasonNumber,
		Title:        fmt.Sprintf("Season %d", seasonNumber),
		EpisodeCount: len(s.onDisk),
	}, nil
}

func (s *syntheticSeasonContentService) ListEpisodesBySeasonID(context.Context, *Session, string, *int) ([]upstreamEpisode, error) {
	s.seasonIDListings++
	return nil, &HTTPError{StatusCode: 404, Message: "season not found"}
}

func (s *syntheticSeasonContentService) ListEpisodes(_ context.Context, _ *Session, seriesID string, seasonNumber int, _ *int) ([]upstreamEpisode, error) {
	s.coordinateListings++
	if seriesID != s.seriesID || seasonNumber != s.seasonNumber {
		return nil, context.Canceled
	}
	episodes := make([]upstreamEpisode, 0, len(s.onDisk))
	for _, id := range s.onDisk {
		episodes = append(episodes, upstreamEpisode{ContentID: id})
	}
	return episodes, nil
}

type stubEpisodePartitioner struct {
	withFile, fileless []string
}

func (p stubEpisodePartitioner) PartitionIDsBySeries(context.Context, string) ([]string, []string, error) {
	return p.withFile, p.fileless, nil
}

func (p stubEpisodePartitioner) PartitionIDsBySeason(context.Context, string) ([]string, []string, error) {
	return p.withFile, p.fileless, nil
}

// A Jellyfin client marking a season unplayed must also clear a watched
// episode whose file was deleted; the episode listing leaves it out, and its
// completed progress would keep the series in Next Up.
func TestParentUnplayedClearsWatchedEpisodesWithoutFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*UserDataHandler, *httptest.ResponseRecorder, string, *Session)
	}{
		{"mark unplayed", func(h *UserDataHandler, rec *httptest.ResponseRecorder, id string, session *Session) {
			h.HandleMarkUnplayed(rec, viewerRequest("DELETE", "/", "", "itemId", id, session))
		}},
		{"update user data", func(h *UserDataHandler, rec *httptest.ResponseRecorder, id string, session *Session) {
			h.HandleUpdateUserData(rec, viewerRequest("POST", "/", `{"Played":false}`, "itemId", id, session))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newJellycompatUserStore(t)
			provider := notifications.WrapUserStoreProvider(compatTestUserStoreProvider{store: store}, &notifications.System{})
			service := &directUserDataService{
				storeProvider: provider,
				watchState:    watchstate.NewService(provider),
				episodes:      stubEpisodePartitioner{withFile: []string{"on-disk"}, fileless: []string{"deleted", "metadata-only"}},
			}
			if err := service.watchState.RecordJellycompatMarkPlayedBatch(t.Context(), 1, "profile-1", []string{"on-disk", "deleted"}, time.Now().UTC().Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			codec := NewResourceIDCodec()
			content := &seasonContentService{stubContentService: stubContentService{detail: &upstreamItemDetail{ContentID: "season-1", Type: "season"}}, onDisk: []string{"on-disk"}}
			h := NewUserDataHandler(content, service, codec, &config.Config{})
			session := &Session{StreamAppUserID: 1, ProfileID: "profile-1"}

			rec := httptest.NewRecorder()
			tc.run(h, rec, codec.EncodeStringID(EncodedIDSeason, "season-1"), session)
			if rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if content.seasonIDListings == 0 || content.coordinateListings != 0 {
				t.Fatalf("season listings = id:%d coordinates:%d, want the season-id listing without coordinates", content.seasonIDListings, content.coordinateListings)
			}
			for _, id := range []string{"on-disk", "deleted"} {
				progress, err := store.GetProgress(t.Context(), "profile-1", id)
				if err != nil {
					t.Fatal(err)
				}
				if progress != nil {
					t.Fatalf("%s progress after unplayed = %+v, want cleared", id, progress)
				}
			}
		})
	}
}

type seasonKeyedPartitioner struct {
	withFile, fileless []string
	seriesID           string
	seasonNumber       int
	seasonKeyed        bool
	seasonIDKeyed      bool
}

func (p *seasonKeyedPartitioner) PartitionIDsBySeries(context.Context, string) ([]string, []string, error) {
	return p.withFile, p.fileless, nil
}

func (p *seasonKeyedPartitioner) PartitionIDsBySeason(context.Context, string) ([]string, []string, error) {
	p.seasonIDKeyed = true
	return p.withFile, p.fileless, nil
}

func (p *seasonKeyedPartitioner) PartitionIDsBySeriesSeason(_ context.Context, seriesID string, seasonNumber int) ([]string, []string, error) {
	p.seasonKeyed = true
	if seriesID != p.seriesID || seasonNumber != p.seasonNumber {
		return nil, nil, context.Canceled
	}
	return p.withFile, p.fileless, nil
}

// The Jellyfin surface must use the durable season identity too. A synthetic or
// partly linked season has no reliable season_id group, so a season-keyed
// unplayed mark would otherwise miss watched file-less episodes.
func TestParentUnplayedUsesSeriesSeasonIdentity(t *testing.T) {
	store := newJellycompatUserStore(t)
	provider := notifications.WrapUserStoreProvider(compatTestUserStoreProvider{store: store}, &notifications.System{})
	partitioner := &seasonKeyedPartitioner{
		withFile: []string{"on-disk"}, fileless: []string{"deleted"},
		seriesID: "series-1", seasonNumber: 2,
	}
	service := &directUserDataService{
		storeProvider: provider,
		watchState:    watchstate.NewService(provider),
		episodes:      partitioner,
	}
	if err := service.watchState.RecordJellycompatMarkPlayedBatch(t.Context(), 1, "profile-1", []string{"on-disk", "deleted"}, time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	codec := NewResourceIDCodec()
	content := &seasonContentService{
		stubContentService: stubContentService{detail: &upstreamItemDetail{
			ContentID: "season-1", Type: "season", SeriesID: "series-1", SeasonNumber: intPtr(2),
		}},
		onDisk: []string{"on-disk"},
	}
	h := NewUserDataHandler(content, service, codec, &config.Config{})
	session := &Session{StreamAppUserID: 1, ProfileID: "profile-1"}
	rec := httptest.NewRecorder()
	h.HandleMarkUnplayed(rec, viewerRequest("DELETE", "/", "", "itemId", codec.EncodeStringID(EncodedIDSeason, "season-1"), session))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !partitioner.seasonKeyed || partitioner.seasonIDKeyed {
		t.Fatalf("season keys = seasonKeyed:%v seasonIDKeyed:%v, want only the durable season keys", partitioner.seasonKeyed, partitioner.seasonIDKeyed)
	}
	if content.coordinateListings == 0 || content.seasonIDListings != 0 {
		t.Fatalf("season listings = coordinates:%d id:%d, want the coordinate listing only", content.coordinateListings, content.seasonIDListings)
	}
	for _, id := range []string{"on-disk", "deleted"} {
		progress, err := store.GetProgress(t.Context(), "profile-1", id)
		if err != nil {
			t.Fatal(err)
		}
		if progress != nil {
			t.Fatalf("%s progress after unplayed = %+v, want cleared", id, progress)
		}
	}
}

// A synthetic season has no seasons row, so the season-id listing answers
// not-found from the catalog. The unplayed mark must resolve the season's
// durable (series, season number) coordinates from its id and clear the
// watched episodes through the coordinate listing instead of aborting.
func TestParentUnplayedSyntheticSeasonWithoutRow(t *testing.T) {
	store := newJellycompatUserStore(t)
	provider := notifications.WrapUserStoreProvider(compatTestUserStoreProvider{store: store}, &notifications.System{})
	partitioner := &seasonKeyedPartitioner{
		withFile: []string{"on-disk"}, fileless: []string{"deleted"},
		seriesID: "series-1", seasonNumber: 2,
	}
	service := &directUserDataService{
		storeProvider: provider,
		watchState:    watchstate.NewService(provider),
		episodes:      partitioner,
	}
	if err := service.watchState.RecordJellycompatMarkPlayedBatch(t.Context(), 1, "profile-1", []string{"on-disk", "deleted"}, time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	codec := NewResourceIDCodec()
	content := &syntheticSeasonContentService{
		seriesID: "series-1", seasonNumber: 2,
		onDisk: []string{"on-disk"},
	}
	h := NewUserDataHandler(content, service, codec, &config.Config{})
	session := &Session{StreamAppUserID: 1, ProfileID: "profile-1"}

	rec := httptest.NewRecorder()
	h.HandleMarkUnplayed(rec, viewerRequest("DELETE", "/", "", "itemId", codec.EncodeStringID(EncodedIDSeason, "series-1-S02"), session))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if content.seasonIDListings != 0 {
		t.Fatalf("season-id listings = %d, want none for a synthetic season", content.seasonIDListings)
	}
	if content.coordinateListings == 0 {
		t.Fatalf("coordinate listings = %d, want at least one", content.coordinateListings)
	}
	if !partitioner.seasonKeyed || partitioner.seasonIDKeyed {
		t.Fatalf("season keys = seasonKeyed:%v seasonIDKeyed:%v, want only the durable season keys", partitioner.seasonKeyed, partitioner.seasonIDKeyed)
	}
	for _, id := range []string{"on-disk", "deleted"} {
		progress, err := store.GetProgress(t.Context(), "profile-1", id)
		if err != nil {
			t.Fatal(err)
		}
		if progress != nil {
			t.Fatalf("%s progress after unplayed = %+v, want cleared", id, progress)
		}
	}
}
