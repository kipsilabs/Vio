package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/artworkkey"
	"github.com/Silo-Server/silo-server/internal/artworkurl"
	"github.com/Silo-Server/silo-server/internal/blobstore"
	"github.com/Silo-Server/silo-server/internal/blobstore/blobstoretest"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/imagecache"
	"github.com/Silo-Server/silo-server/internal/metadata"
	"github.com/Silo-Server/silo-server/internal/netguard"
	"github.com/jackc/pgx/v5/pgxpool"
)

// artworkRepairJobStatus reads the single image cache job a collage repair
// enqueues for an item poster. The DB-backed test owns this content ID, so the
// scoped lookup needs no advisory lock of its own.
func artworkRepairJobStatus(t *testing.T, pool *pgxpool.Pool, contentID string) (string, int) {
	t.Helper()
	var status string
	var attempts int
	if err := pool.QueryRow(context.Background(), `
		SELECT status, attempt_count
		FROM metadata_image_cache_jobs
		WHERE target_type = 'item'
		  AND target_content_id = $1
		  AND image_type = 'poster'
		  AND target_language = ''
	`, contentID).Scan(&status, &attempts); err != nil {
		t.Fatalf("read artwork repair job state: %v", err)
	}
	return status, attempts
}

// TestCollectionCollageRepairLoopPostgres drives the repair -> collage loop end
// to end against the migrated schema. A genuinely provider-backed title has a
// cached poster whose requested variant has gone missing, so the real collage
// composer reads a miss and enqueues a repair; the real ImageCacheProcessor
// then rebuilds the variants through a real cacher and blob store from a
// stubbed provider, and the second compose succeeds. It skips cleanly when
// SILO_TEST_DATABASE_URL is unset.
func TestCollectionCollageRepairLoopPostgres(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()

	poster := testCollectionPosterJPEG(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/poster.jpg" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(poster)
	}))
	t.Cleanup(provider.Close)

	const (
		tmdbID   = "550"
		revision = "aaaaaaaaaaaaaaaa"
	)
	contentID := fmt.Sprintf("artwork-repair-collage-%d", time.Now().UnixNano())
	sourceURL := provider.URL + "/poster.jpg"
	seededOriginal := "tmdb/movies/" + tmdbID + "/poster/original." + revision + ".webp"

	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items (content_id, type, title, tmdb_id, poster_path, poster_source_path)
		VALUES ($1, 'movie', 'Artwork repair collage', $2, $3, $4)`,
		contentID, tmdbID, seededOriginal, sourceURL); err != nil {
		t.Fatalf("seed media item: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id = $1`, contentID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM metadata_image_cache_jobs WHERE target_content_id = $1`, contentID)
	})

	// The title's variants are genuine cached artwork; only the variant the
	// collage asks for -- the "small" poster rung -- has disappeared.
	store := blobstoretest.New()
	for _, key := range artworkkey.ObjectKeys(seededOriginal, artworkkey.ImagePoster) {
		if err := store.Put(ctx, key, poster); err != nil {
			t.Fatalf("seed cached variant %s: %v", key, err)
		}
	}
	missing := artworkkey.Variant(seededOriginal, "w300")
	if _, err := store.Delete(ctx, []string{missing}); err != nil {
		t.Fatalf("remove cached variant: %v", err)
	}

	signer := artworkurl.NewSigner(collageSourceSecret, time.Hour)
	resolver := metadata.NewPluginImageResolver()
	t.Cleanup(resolver.Close)
	resolver.SetArtworkResolver(artworkurl.NewServerResolver(signer))
	detail := &catalog.DetailService{}
	detail.SetImageResolver(resolver)

	repo := metadata.NewImageCacheJobRepository(pool)
	gen := NewPersonalCollectionCollageGenerator(store, detail, signer, repo, nil)
	if gen == nil {
		t.Fatal("no collage generator")
	}

	const collageKey = "1111111111111111"
	if _, _, err := gen.ComposeCollectionCollage(ctx, "c1", collageKey, []string{seededOriginal}); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("compose with a missing cached variant = %v, want blobstore.ErrNotFound", err)
	}

	status, attempts := artworkRepairJobStatus(t, pool, contentID)
	if status != metadata.ImageCacheStatusQueued || attempts != 0 {
		t.Fatalf("repair job state = %q attempt %d, want %q attempt 0", status, attempts, metadata.ImageCacheStatusQueued)
	}

	cacher := imagecache.New(store)
	items := catalog.NewItemRepository(pool)
	processor := metadata.NewImageCacheProcessorWithTargets(repo, cacher, nil, metadata.ImageCacheProcessorTargets{Items: items})
	stats, err := processor.RunOnce(netguard.WithPrivateAccess(ctx), "artwork-repair-test", 10, 1)
	if err != nil {
		t.Fatalf("run image cache processor: %v", err)
	}
	if stats.Succeeded < 1 {
		t.Fatalf("processor stats = %+v, want at least one repaired job", stats)
	}

	status, _ = artworkRepairJobStatus(t, pool, contentID)
	if status != metadata.ImageCacheStatusSucceeded {
		t.Fatalf("repair job status = %q, want %q", status, metadata.ImageCacheStatusSucceeded)
	}

	var repaired string
	if err := pool.QueryRow(ctx, `SELECT poster_path FROM media_items WHERE content_id = $1`, contentID).Scan(&repaired); err != nil {
		t.Fatalf("read repaired poster path: %v", err)
	}
	if repaired == "" || repaired == seededOriginal {
		t.Fatalf("poster_path = %q, want a rebuilt revision", repaired)
	}
	body, _, err := store.Get(ctx, artworkkey.Variant(repaired, "w300"))
	if err != nil {
		t.Fatalf("repaired variant is not in the store: %v", err)
	}
	_ = body.Close()

	if _, _, err := gen.ComposeCollectionCollage(ctx, "c1", collageKey, []string{repaired}); err != nil {
		t.Fatalf("compose after repair: %v", err)
	}
}
