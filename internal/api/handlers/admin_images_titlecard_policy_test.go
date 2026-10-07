package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/Silo-Server/silo-server/internal/metadata"
	"github.com/Silo-Server/silo-server/internal/models"
)

// TestApplyImageScopeMatrixV1BridgeCorrectness pins the shared v1/v2 apply
// bridge against the policy ruling for episode title cards (docs/catalog-api.md,
// "Episode title cards"; docs/admin-catalog-api.md, "Item image selection"):
//
//   - "titlecard" resolves to the episode still, so on a movie, series, or
//     season it is refused with 400 unsupported_image_type before any download.
//     Before that alias, "titlecard" fell through ImageTypeFromString's default
//     to ImagePoster and was silently stored as a poster on those scopes. That
//     silent wrong-type store was a bug, not a contract, so refusing it is an
//     allowed bridge correctness fix.
//   - Every request that was valid before stays valid and maps to the same
//     image type: poster/backdrop/logo on a movie, series, or season; poster on
//     a season; any type on an episode (coerced to its still, including
//     "poster", "profile", and an unknown name, which historically arrived as
//     a poster and is still coerced). "still" and "profile" were already
//     refused on non-episode scope and remain refused.
//
// The image service fails after recording so the test pins the preflight and
// request mapping without a database.
func TestApplyImageScopeMatrixV1BridgeCorrectness(t *testing.T) {
	movieItem := imageItemLookupFake{"movie:heat-1995": {ContentID: "movie:heat-1995", Type: "movie", TmdbID: "42"}}
	seriesItem := imageItemLookupFake{"series:severance": {ContentID: "series:severance", Type: "series", TmdbID: "42"}}
	seasonID := "season:severance-s01"
	season := imageSeasonLookupFake{seasonID: {ContentID: seasonID, SeriesID: "series:severance", SeasonNumber: 1}}
	episodeID := "episode:severance-s01e01"
	episode := &models.Episode{ContentID: episodeID, SeriesID: "series:severance", SeasonNumber: 1, EpisodeNumber: 1}

	tests := []struct {
		name      string
		contentID string
		imageType string
		items     imageItemLookupFake
		seasons   imageSeasonLookupFake
		episodes  titlecardEpisodes
		refused   bool
		wantType  metadata.ImageType
	}{
		// Previously-valid movie/series/season types are unchanged.
		{name: "movie poster", contentID: "movie:heat-1995", imageType: "poster", items: movieItem, wantType: metadata.ImagePoster},
		{name: "movie backdrop", contentID: "movie:heat-1995", imageType: "backdrop", items: movieItem, wantType: metadata.ImageBackdrop},
		{name: "movie logo", contentID: "movie:heat-1995", imageType: "logo", items: movieItem, wantType: metadata.ImageLogo},
		{name: "movie unknown falls back to poster", contentID: "movie:heat-1995", imageType: "bogus", items: movieItem, wantType: metadata.ImagePoster},
		{name: "series poster", contentID: "series:severance", imageType: "poster", items: seriesItem, wantType: metadata.ImagePoster},
		{name: "series backdrop", contentID: "series:severance", imageType: "backdrop", items: seriesItem, wantType: metadata.ImageBackdrop},
		{name: "series logo", contentID: "series:severance", imageType: "logo", items: seriesItem, wantType: metadata.ImageLogo},
		{name: "season poster", contentID: seasonID, imageType: "poster", items: seriesItem, seasons: season, wantType: metadata.ImagePoster},

		// Already-refused combinations stay refused.
		{name: "movie still", contentID: "movie:heat-1995", imageType: "still", items: movieItem, refused: true},
		{name: "series still", contentID: "series:severance", imageType: "still", items: seriesItem, refused: true},
		{name: "season still", contentID: seasonID, imageType: "still", items: seriesItem, seasons: season, refused: true},
		{name: "season backdrop", contentID: seasonID, imageType: "backdrop", items: seriesItem, seasons: season, refused: true},
		{name: "movie profile", contentID: "movie:heat-1995", imageType: "profile", items: movieItem, refused: true},

		// The ruling: titlecard on non-episode scope is a new refusal (it used
		// to be silently stored as a poster).
		{name: "movie titlecard", contentID: "movie:heat-1995", imageType: "titlecard", items: movieItem, refused: true},
		{name: "series titlecard", contentID: "series:severance", imageType: "titlecard", items: seriesItem, refused: true},
		{name: "season titlecard", contentID: seasonID, imageType: "titlecard", items: seriesItem, seasons: season, refused: true},

		// An episode coerces every accepted type to its still.
		{name: "episode poster coerces to still", contentID: episodeID, imageType: "poster", items: seriesItem, episodes: titlecardEpisodes{episode: episode}, wantType: metadata.ImageStill},
		{name: "episode backdrop coerces to still", contentID: episodeID, imageType: "backdrop", items: seriesItem, episodes: titlecardEpisodes{episode: episode}, wantType: metadata.ImageStill},
		{name: "episode logo coerces to still", contentID: episodeID, imageType: "logo", items: seriesItem, episodes: titlecardEpisodes{episode: episode}, wantType: metadata.ImageStill},
		{name: "episode profile coerces to still", contentID: episodeID, imageType: "profile", items: seriesItem, episodes: titlecardEpisodes{episode: episode}, wantType: metadata.ImageStill},
		{name: "episode unknown coerces to still", contentID: episodeID, imageType: "bogus", items: seriesItem, episodes: titlecardEpisodes{episode: episode}, wantType: metadata.ImageStill},
		{name: "episode still", contentID: episodeID, imageType: "still", items: seriesItem, episodes: titlecardEpisodes{episode: episode}, wantType: metadata.ImageStill},
		{name: "episode titlecard", contentID: episodeID, imageType: "titlecard", items: seriesItem, episodes: titlecardEpisodes{episode: episode}, wantType: metadata.ImageStill},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &titlecardImageService{fail: true}
			seasons := tt.seasons
			if seasons == nil {
				seasons = imageSeasonLookupFake{}
			}
			h := NewAdminImageHandler(tt.items, seasons, tt.episodes, nil, svc, nil, nil)
			router := chi.NewRouter()
			router.Post("/{id}/images/apply", h.HandleApplyItemImage)

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/"+tt.contentID+"/images/apply",
				strings.NewReader(`{"original_url":"https://cdn.example.test/x.jpg","type":"`+tt.imageType+`"}`)))

			if tt.refused {
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
				}
				if !strings.Contains(rec.Body.String(), `"unsupported_image_type"`) {
					t.Fatalf("body = %s, want unsupported_image_type", rec.Body.String())
				}
				if svc.calls != 0 {
					t.Fatalf("apply calls = %d, want 0 (refused before download)", svc.calls)
				}
				return
			}
			if rec.Code == http.StatusBadRequest {
				t.Fatalf("previously-valid request now refused: %d %s", rec.Code, rec.Body.String())
			}
			if svc.calls != 1 {
				t.Fatalf("apply calls = %d, want 1", svc.calls)
			}
			if svc.request.ImageType != tt.wantType {
				t.Fatalf("image type = %v, want %v", svc.request.ImageType, tt.wantType)
			}
		})
	}
}

// TestGetItemImagesV1OmitsStillURL pins the frozen v1 wire shape: the shared
// current-selection body never carries still_url, even for an episode whose
// title card is set. The field rides the Go struct for apiv2 (json:"-") and is
// emitted only there.
func TestGetItemImagesV1OmitsStillURL(t *testing.T) {
	episode := &models.Episode{ContentID: "episode:severance-s01e01", SeriesID: "series:severance", SeasonNumber: 1, EpisodeNumber: 1, StillPath: "tmdb/series/1/still/original.rev.webp"}
	h := newTitlecardHandler(episode, &titlecardImageService{})
	view, err := h.GetAdminItemImages(context.Background(), episode.ContentID)
	if err != nil {
		t.Fatalf("GetAdminItemImages: %v", err)
	}
	if view.Current.StillURL == "" {
		t.Fatal("fixture did not set the shared still; the omission assertion would be vacuous")
	}
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "still_url") {
		t.Fatalf("v1 current body leaks still_url: %s", data)
	}
}
