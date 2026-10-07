package handlers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/metadata"
	"github.com/Silo-Server/silo-server/internal/models"
)

// titlecardPublicationImages stands in for the image cache: it records the
// request and returns the immutable stored path and thumbhash PublishArtworkSelection
// writes, without a download or an S3 write.
type titlecardPublicationImages struct {
	stored string
	hash   string
	calls  int
}

func (f *titlecardPublicationImages) FetchItemImages(context.Context, map[string]string, string, string, int) ([]metadata.RemoteImage, map[string]string, error) {
	return nil, nil, nil
}

func (f *titlecardPublicationImages) FetchSeasonImages(context.Context, map[string]string, string, int, int) ([]metadata.RemoteImage, map[string]string, error) {
	return nil, nil, nil
}

func (f *titlecardPublicationImages) ApplyItemImage(context.Context, metadata.ApplyItemImageRequest) (*metadata.ApplyItemImageResult, error) {
	f.calls++
	return &metadata.ApplyItemImageResult{StoredPath: f.stored, Thumbhash: f.hash, Revision: "rev"}, nil
}

// titlecardPublicationFiles is the in-memory FileVersionFetcher the detail and
// row builders call. It also implements the batch provider so listEpisodeFiles
// takes the batch path, the way scanner.FileRepository does.
type titlecardPublicationFiles struct{}

func (titlecardPublicationFiles) GetByContentID(context.Context, string) ([]*models.MediaFile, error) {
	return nil, nil
}
func (titlecardPublicationFiles) GetByEpisodeID(context.Context, string) ([]*models.MediaFile, error) {
	return nil, nil
}
func (titlecardPublicationFiles) GetByExtraID(context.Context, string) ([]*models.MediaFile, error) {
	return nil, nil
}
func (titlecardPublicationFiles) ListByContentIDs(context.Context, []string) (map[string][]*models.MediaFile, error) {
	return nil, nil
}
func (titlecardPublicationFiles) ListByEpisodeIDs(context.Context, []string) (map[string][]*models.MediaFile, error) {
	return nil, nil
}

// titlecardImageResolver maps every cached path to a recognizable URL, so a
// read proves which stored path it served without a real signer.
type titlecardImageResolver struct{}

func (titlecardImageResolver) ResolveImageURL(_ context.Context, path string, variant string) string {
	return "https://images.test/" + variant + "/" + path
}
func (r titlecardImageResolver) ResolveImageURLs(ctx context.Context, paths []string, variant string) map[string]string {
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		out[p] = r.ResolveImageURL(ctx, p, variant)
	}
	return out
}

// TestApplyTitlecardPublishesToDetailAndRows is the integration coverage behind
// the request-mapping test: a successful title-card apply on an episode writes
// that episode's still and is what the detail and season-row reads then serve.
// It also pins the blast radius: the sibling episode and the parent series keep
// their own artwork, and the manual lock lands on the series.
func TestApplyTitlecardPublishesToDetailAndRows(t *testing.T) {
	pool := catalogTransferPool(t)
	ctx := t.Context()

	prefix := fmt.Sprintf("titlecard-pub-%d", time.Now().UnixNano())
	seriesID := prefix + "-series"
	seasonID := prefix + "-season"
	targetID := prefix + "-e1"
	siblingID := prefix + "-e2"
	seriesPoster := "tmdb/series/" + prefix + "/poster/original.rev.webp"
	seriesBackdrop := "tmdb/series/" + prefix + "/backdrop/original.rev.webp"
	siblingStill := "tmdb/series/" + prefix + "/still/sibling.original.rev.webp"
	storedStill := fmt.Sprintf("tmdb/series/%s/still/original.rev.webp", prefix)

	var library int
	if err := pool.QueryRow(ctx, `INSERT INTO media_folders(type,name) VALUES('tv',$1) RETURNING id`, prefix).Scan(&library); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM artwork_revision_gc_candidates WHERE original_path = $1`, storedStill)
		_, _ = pool.Exec(bg, `DELETE FROM media_folders WHERE id=$1`, library)
		_, _ = pool.Exec(bg, `DELETE FROM media_items WHERE content_id=$1`, seriesID)
	})

	if _, err := pool.Exec(ctx, `INSERT INTO media_items(content_id,type,title,genres,poster_path,backdrop_path) VALUES($1,'series','Synthetic Series','{}',$2,$3)`, seriesID, seriesPoster, seriesBackdrop); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_item_libraries(content_id,media_folder_id) VALUES($1,$2)`, seriesID, library); err != nil {
		t.Fatal(err)
	}
	seasons := catalog.NewSeasonRepository(pool)
	if err := seasons.Upsert(ctx, &models.Season{ContentID: seasonID, SeriesID: seriesID, SeasonNumber: 1, Title: "Season 1", DefaultMetadataLanguage: "en"}); err != nil {
		t.Fatal(err)
	}
	episodes := catalog.NewEpisodeRepository(pool)
	if err := episodes.Upsert(ctx, &models.Episode{ContentID: targetID, SeriesID: seriesID, SeasonID: seasonID, SeasonNumber: 1, EpisodeNumber: 1, Title: "Target", DefaultMetadataLanguage: "en"}); err != nil {
		t.Fatal(err)
	}
	if err := episodes.Upsert(ctx, &models.Episode{ContentID: siblingID, SeriesID: seriesID, SeasonID: seasonID, SeasonNumber: 1, EpisodeNumber: 2, Title: "Sibling", DefaultMetadataLanguage: "en", StillPath: siblingStill, StillThumbhash: "sibling-hash"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{targetID, siblingID} {
		if _, err := pool.Exec(ctx, `INSERT INTO episode_libraries(episode_id,media_folder_id) VALUES($1,$2)`, id, library); err != nil {
			t.Fatal(err)
		}
	}

	items := catalog.NewItemRepository(pool)
	detail := catalog.NewDetailService(items, episodes, seasons, catalog.NewPersonRepository(pool), titlecardPublicationFiles{})
	detail.SetImageResolver(titlecardImageResolver{})

	imgSvc := &titlecardPublicationImages{stored: storedStill, hash: "titlecard-hash"}
	h := NewAdminImageHandler(items, seasons, episodes, nil, imgSvc, nil, detail)

	if _, err := h.ApplyAdminItemImage(ctx, targetID, AdminItemImageRequest{OriginalURL: "https://cdn.example.test/tc.jpg", Type: "titlecard", ProviderID: "tmdb"}); err != nil {
		t.Fatalf("ApplyAdminItemImage: %v", err)
	}
	if imgSvc.calls != 1 {
		t.Fatalf("image service calls = %d, want 1", imgSvc.calls)
	}

	// The target episode's still is published; the sibling's is untouched.
	target, err := episodes.GetByID(ctx, targetID)
	if err != nil {
		t.Fatal(err)
	}
	if target.StillPath != storedStill || target.StillThumbhash != "titlecard-hash" {
		t.Fatalf("target still = %q/%q, want %q/titlecard-hash", target.StillPath, target.StillThumbhash, storedStill)
	}
	sibling, err := episodes.GetByID(ctx, siblingID)
	if err != nil {
		t.Fatal(err)
	}
	if sibling.StillPath != siblingStill || sibling.StillThumbhash != "sibling-hash" {
		t.Fatalf("sibling still changed: %q/%q", sibling.StillPath, sibling.StillThumbhash)
	}

	// The parent series keeps its poster and backdrop; the manual lock lands on it.
	var posterPath, backdropPath string
	var locked []int
	if err := pool.QueryRow(ctx, `SELECT poster_path, backdrop_path, locked_fields FROM media_items WHERE content_id=$1`, seriesID).Scan(&posterPath, &backdropPath, &locked); err != nil {
		t.Fatal(err)
	}
	if posterPath != seriesPoster || backdropPath != seriesBackdrop {
		t.Fatalf("series artwork changed: %q/%q", posterPath, backdropPath)
	}
	if len(locked) != 1 || locked[0] != int(metadata.FieldImages) {
		t.Fatalf("series locked_fields = %v, want [%d]", locked, metadata.FieldImages)
	}

	filter := catalog.AccessFilter{AllowedLibraryIDs: []int{library}}

	// Detail read: the episode's still is served as poster_url, the series
	// backdrop stays separate as backdrop_url.
	itemDetail, err := detail.GetItemDetail(ctx, targetID, filter)
	if err != nil {
		t.Fatalf("GetItemDetail: %v", err)
	}
	if itemDetail.PosterThumbhash != "titlecard-hash" {
		t.Fatalf("detail poster_thumbhash = %q, want titlecard-hash", itemDetail.PosterThumbhash)
	}
	if !strings.Contains(itemDetail.PosterURL, "/still/") || !strings.Contains(itemDetail.PosterURL, prefix) {
		t.Fatalf("detail poster_url = %q, want the published still for %s", itemDetail.PosterURL, prefix)
	}
	if !strings.Contains(itemDetail.BackdropURL, "/backdrop/") {
		t.Fatalf("detail backdrop_url = %q, want the series backdrop", itemDetail.BackdropURL)
	}
	// The sibling detail still serves its own still, not the published one.
	siblingDetail, err := detail.GetItemDetail(ctx, siblingID, filter)
	if err != nil {
		t.Fatalf("GetItemDetail(sibling): %v", err)
	}
	if siblingDetail.PosterThumbhash != "sibling-hash" {
		t.Fatalf("sibling detail poster_thumbhash = %q, want sibling-hash", siblingDetail.PosterThumbhash)
	}

	// Row read: the season episodes list carries the published still as
	// still_url, and the sibling keeps its own.
	itemsHandler := &ItemsHandler{itemRepo: items, seasonRepo: seasons, episodeRepo: episodes, detailSvc: detail, fileRepo: titlecardPublicationFiles{}}
	rows, err := NewCatalogResourceHandler(itemsHandler).SeasonEpisodes(ctx, ItemViewer{Access: filter}, seriesID, 1)
	if err != nil {
		t.Fatalf("SeasonEpisodes: %v", err)
	}
	byID := make(map[string]EpisodeView, len(rows))
	for _, row := range rows {
		byID[row.ContentID] = row
	}
	targetRow, ok := byID[targetID]
	if !ok {
		t.Fatalf("target episode missing from rows: %#v", rows)
	}
	if !strings.Contains(targetRow.StillURL, "/still/") || targetRow.StillThumbhash != "titlecard-hash" {
		t.Fatalf("row still = %q/%q, want the published still", targetRow.StillURL, targetRow.StillThumbhash)
	}
	siblingRow := byID[siblingID]
	if !strings.Contains(siblingRow.StillURL, "/still/") || !strings.Contains(siblingRow.StillURL, "sibling") || siblingRow.StillThumbhash != "sibling-hash" {
		t.Fatalf("sibling row still changed: %q/%q", siblingRow.StillURL, siblingRow.StillThumbhash)
	}
}
