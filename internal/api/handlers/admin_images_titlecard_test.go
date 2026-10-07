package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/metadata"
	"github.com/Silo-Server/silo-server/internal/models"
)

// titlecardEpisodes answers exactly one episode content ID and misses others.
type titlecardEpisodes struct {
	episode *models.Episode
}

func (f titlecardEpisodes) GetByID(_ context.Context, contentID string) (*models.Episode, error) {
	if f.episode != nil && contentID == f.episode.ContentID {
		return f.episode, nil
	}
	return nil, catalog.ErrEpisodeNotFound
}

// titlecardImageService records the apply request an episode title card sends.
// fail makes ApplyItemImage return before publication, which lets a test assert
// the preflight scope without a database.
type titlecardImageService struct {
	calls   int
	request metadata.ApplyItemImageRequest
	fail    bool
}

func (f *titlecardImageService) FetchItemImages(context.Context, map[string]string, string, string, int) ([]metadata.RemoteImage, map[string]string, error) {
	return []metadata.RemoteImage{{ProviderID: "tmdb", URL: "tmdb://episode-still.jpg", Type: metadata.ImageStill}}, nil, nil
}

func (f *titlecardImageService) FetchSeasonImages(context.Context, map[string]string, string, int, int) ([]metadata.RemoteImage, map[string]string, error) {
	return nil, nil, nil
}

func (f *titlecardImageService) ApplyItemImage(_ context.Context, r metadata.ApplyItemImageRequest) (*metadata.ApplyItemImageResult, error) {
	f.calls++
	f.request = r
	if f.fail {
		return nil, errTitlecardImageDown
	}
	return &metadata.ApplyItemImageResult{StoredPath: "tmdb/series/1/still/original.rev.webp", Thumbhash: "hash", Revision: "rev"}, nil
}

var errTitlecardImageDown = errors.New("synthetic image failure")

func newTitlecardHandler(episode *models.Episode, svc *titlecardImageService) *AdminImageHandler {
	items := imageItemLookupFake{"series:severance": {ContentID: "series:severance", Type: "series", TmdbID: "42"}}
	return NewAdminImageHandler(items, imageSeasonLookupFake{}, titlecardEpisodes{episode: episode}, nil, svc, nil, nil)
}

// TestApplyTitlecardStoresEpisodeStill pins that "titlecard" is accepted on an
// episode and resolved as that episode's still, with the season and episode
// numbers scoping the cache key so siblings do not collide. The image service
// fails after recording so publication, which needs a database, is not reached.
func TestApplyTitlecardStoresEpisodeStill(t *testing.T) {
	episode := &models.Episode{ContentID: "episode:severance-s01e01", SeriesID: "series:severance", SeasonNumber: 1, EpisodeNumber: 1}
	svc := &titlecardImageService{fail: true}
	h := newTitlecardHandler(episode, svc)
	router := chi.NewRouter()
	router.Post("/{id}/images/apply", h.HandleApplyItemImage)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/episode:severance-s01e01/images/apply",
		strings.NewReader(`{"original_url":"https://cdn.example.test/e1.jpg","type":"titlecard","provider_id":"tmdb"}`)))

	if svc.calls != 1 {
		t.Fatalf("apply calls = %d, want 1", svc.calls)
	}
	if svc.request.ImageType != metadata.ImageStill {
		t.Fatalf("image type = %v, want ImageStill", svc.request.ImageType)
	}
	if svc.request.SeasonNumber == nil || *svc.request.SeasonNumber != 1 || svc.request.EpisodeNumber == nil || *svc.request.EpisodeNumber != 1 {
		t.Fatalf("cache key scope = season %v episode %v, want 1/1", svc.request.SeasonNumber, svc.request.EpisodeNumber)
	}
}

// TestApplyTitlecardRejectedForNonEpisodeScope pins that "titlecard" on a
// movie, series, or season is refused with a clean 4xx before any download,
// rather than silently stored as a poster.
func TestApplyTitlecardRejectedForNonEpisodeScope(t *testing.T) {
	tests := []struct {
		name    string
		content string
		items   imageItemLookupFake
		seasons imageSeasonLookupFake
	}{
		{
			name:    "movie",
			content: "movie:heat-1995",
			items:   imageItemLookupFake{"movie:heat-1995": {ContentID: "movie:heat-1995", Type: "movie", TmdbID: "42"}},
			seasons: imageSeasonLookupFake{},
		},
		{
			name:    "series",
			content: "series:severance",
			items:   imageItemLookupFake{"series:severance": {ContentID: "series:severance", Type: "series", TmdbID: "42"}},
			seasons: imageSeasonLookupFake{},
		},
		{
			name:    "season",
			content: "season:severance-s01",
			items:   imageItemLookupFake{"series:severance": {ContentID: "series:severance", Type: "series", TmdbID: "42"}},
			seasons: imageSeasonLookupFake{"season:severance-s01": {ContentID: "season:severance-s01", SeriesID: "series:severance", SeasonNumber: 1}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &titlecardImageService{}
			h := NewAdminImageHandler(tt.items, tt.seasons, titlecardEpisodes{}, nil, svc, nil, nil)
			router := chi.NewRouter()
			router.Post("/{id}/images/apply", h.HandleApplyItemImage)

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/"+tt.content+"/images/apply",
				strings.NewReader(`{"original_url":"https://cdn.example.test/tc.jpg","type":"titlecard"}`)))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"unsupported_image_type"`) {
				t.Fatalf("body = %s, want unsupported_image_type", rec.Body.String())
			}
			if svc.calls != 0 {
				t.Fatalf("apply calls = %d, want 0 (rejected before download)", svc.calls)
			}
		})
	}
}

// TestGetItemImagesReportsEpisodeTitleCard pins that the image picker's current
// selection surfaces an episode's stored still, and omits it when the episode
// has none. The shared view is read directly because still_url is v2-only.
func TestGetItemImagesReportsEpisodeTitleCard(t *testing.T) {
	tests := []struct {
		name    string
		episode *models.Episode
		want    string
	}{
		{
			name:    "set",
			episode: &models.Episode{ContentID: "episode:severance-s01e01", SeriesID: "series:severance", SeasonNumber: 1, EpisodeNumber: 1, StillPath: "tmdb/series/1/still/original.rev.webp"},
			want:    "tmdb/series/1/still/original.rev.webp",
		},
		{
			name:    "unset",
			episode: &models.Episode{ContentID: "episode:severance-s01e02", SeriesID: "series:severance", SeasonNumber: 1, EpisodeNumber: 2},
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTitlecardHandler(tt.episode, &titlecardImageService{})
			view, err := h.GetAdminItemImages(context.Background(), tt.episode.ContentID)
			if err != nil {
				t.Fatalf("GetAdminItemImages: %v", err)
			}
			if view.Current.StillURL != tt.want {
				t.Fatalf("current still = %q, want %q", view.Current.StillURL, tt.want)
			}
			if len(view.Images) != 1 || view.Images[0].Type != "still" {
				t.Fatalf("images = %#v, want one still choice", view.Images)
			}
		})
	}
}
