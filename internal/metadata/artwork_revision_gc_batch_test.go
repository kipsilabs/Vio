package metadata

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/blobstore"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/s3client"
)

type artworkRevisionDeleteFunc func(context.Context, []string) (int, error)

func (f artworkRevisionDeleteFunc) Delete(ctx context.Context, keys []string) (int, error) {
	return f(ctx, keys)
}

func TestArtworkRevisionGCRunLocksBatchDuringDeletion(t *testing.T) {
	pool := artworkRevisionGCTestPool(t)
	ctx := t.Context()
	paths := []string{"tmdb/movies/gc-run-lock-1/poster/original.old.webp", "tmdb/movies/gc-run-lock-2/poster/original.old.webp"}
	for _, path := range paths {
		_, err := pool.Exec(ctx, `INSERT INTO artwork_revision_gc_candidates
   (original_path, object_keys, not_before, next_attempt_at)
   VALUES ($1, ARRAY[$1]::text[], NOW() - interval '1 hour', NOW() - interval '1 hour')`, path)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM artwork_revision_gc_candidates WHERE original_path = ANY($1)`, paths)
	})
	calls := 0
	deleter := artworkRevisionDeleteFunc(func(ctx context.Context, keys []string) (int, error) {
		calls++
		if len(keys) != len(paths) {
			t.Errorf("delete keys = %v, want both revisions in one call", keys)
		}
		for _, path := range paths {
			tx, err := pool.Begin(ctx)
			if err != nil {
				return 0, err
			}
			_, lockErr := tx.Exec(ctx, `SELECT id FROM artwork_revision_gc_candidates WHERE original_path = $1 FOR UPDATE NOWAIT`, path)
			_ = tx.Rollback(ctx)
			pgErr, ok := errors.AsType[*pgconn.PgError](lockErr)
			if !ok || pgErr.Code != "55P03" {
				t.Errorf("revision %s is not locked during object deletion: %v", path, lockErr)
			}
		}
		return len(keys), nil
	})
	stats, err := NewArtworkRevisionGarbageCollector(pool, deleter).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || stats.Deleted != 2 {
		t.Fatalf("calls = %d, stats = %+v, want one call and two deletions", calls, stats)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM artwork_revision_gc_candidates WHERE original_path = ANY($1)`, paths).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("remaining candidates = %d", remaining)
	}
}

func TestArtworkRevisionGCBatchRechecksClaimedState(t *testing.T) {
	pool := artworkRevisionGCTestPool(t)
	ctx := t.Context()
	const workerID = "gc-batch-state"
	paths := []string{"gc-batch-referenced", "gc-batch-retracked", "gc-batch-manifest", "gc-batch-pending-heal"}
	var candidates []artworkRevisionGCCandidate
	for _, path := range paths {
		candidate := artworkRevisionGCCandidate{originalPath: path, objectKeys: []string{path + "/stale"}}
		err := pool.QueryRow(ctx, `INSERT INTO artwork_revision_gc_candidates
   (original_path, object_keys, not_before, next_attempt_at, locked_at, locked_by)
   VALUES ($1, ARRAY[$1 || '/current']::text[], NOW() - interval '1 hour', NOW() - interval '1 hour', NOW(), $2)
   RETURNING id`, path, workerID).Scan(&candidate.id)
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, candidate)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id = ANY($1)`, paths)
		_, _ = pool.Exec(context.Background(), `DELETE FROM artwork_revision_gc_candidates WHERE original_path = ANY($1)`, paths)
	})
	// These changes occur after claiming and the optimistic reference pre-check.
	for _, path := range []string{paths[0], paths[3]} {
		_, err := pool.Exec(ctx, `INSERT INTO media_items (content_id, type, title, status, genres, poster_path)
   VALUES ($1, 'movie', 'GC batch reference', 'matched', '{}'::text[], $1)`, path)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := catalog.NewArtworkRevisionTracker(pool).TrackArtworkRevision(ctx, paths[1], "poster", []string{paths[1] + "/new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE artwork_revision_gc_candidates SET deleted_at = NOW() WHERE original_path = $1`, paths[3]); err != nil {
		t.Fatal(err)
	}
	calls := 0
	deleter := artworkRevisionDeleteFunc(func(_ context.Context, keys []string) (int, error) {
		calls++
		want := []string{paths[2] + "/current", paths[3] + "/current"}
		if !slices.Equal(keys, want) {
			t.Errorf("deleted keys = %v, want current manifests %v", keys, want)
		}
		return len(keys), nil
	})
	pending, stats, err := NewArtworkRevisionGarbageCollector(pool, deleter).processCandidatesToHeal(ctx, candidates, workerID)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || stats.Referenced != 1 || len(pending) != 2 {
		t.Fatalf("calls = %d, stats = %+v, pending = %v", calls, stats, pending)
	}
	for _, path := range paths[2:] {
		var tombstone bool
		if err := pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM artwork_revision_gc_candidates WHERE original_path = $1`, path).Scan(&tombstone); err != nil {
			t.Fatal(err)
		}
		if !tombstone {
			t.Errorf("missing durable tombstone for %s", path)
		}
	}
	var nextAttempt *time.Time
	var lockedBy string
	if err := pool.QueryRow(ctx, `SELECT next_attempt_at, locked_by FROM artwork_revision_gc_candidates WHERE original_path = $1`, paths[0]).Scan(&nextAttempt, &lockedBy); err != nil {
		t.Fatal(err)
	}
	if nextAttempt != nil || lockedBy != "" {
		t.Errorf("referenced revision was not parked with its lease released: next_attempt_at=%v locked_by=%q", nextAttempt, lockedBy)
	}
}

func TestArtworkRevisionGCRunRetriesIncompleteBatch(t *testing.T) {
	for _, failBatch := range []bool{false, true} {
		t.Run(fmt.Sprintf("error=%t", failBatch), func(t *testing.T) {
			pool := artworkRevisionGCTestPool(t)
			ctx := t.Context()
			paths := []string{"tmdb/movies/gc-batch-success/poster/original.old.webp", "gc-batch-failure"}
			for _, path := range paths {
				_, err := pool.Exec(ctx, `INSERT INTO artwork_revision_gc_candidates
     (original_path, object_keys, not_before, next_attempt_at)
     VALUES ($1, ARRAY[$1]::text[], NOW() - interval '1 hour', NOW() - interval '1 hour')`, path)
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id = $1`, "gc-partial-publication")
				_, _ = pool.Exec(context.Background(), `DELETE FROM artwork_revision_gc_candidates WHERE original_path = ANY($1)`, paths)
			})
			calls := 0
			deleter := artworkRevisionDeleteFunc(func(ctx context.Context, keys []string) (int, error) {
				calls++
				if len(keys) == 2 {
					_, err := pool.Exec(ctx, `INSERT INTO media_items (content_id, type, title, status, genres, poster_path, poster_source_path)
						VALUES ('gc-partial-publication', 'movie', 'GC partial publication', 'matched', '{}'::text[], $1, 'https://images.example/partial.jpg')`, paths[0])
					if err != nil {
						return 0, err
					}
					if failBatch {
						return 1, errors.New("injected batch error")
					}
					return 1, nil
				}
				if len(keys) == 1 && keys[0] == paths[0] {
					return 1, nil
				}
				return 0, nil
			})
			stats, err := NewArtworkRevisionGarbageCollector(pool, deleter).Run(ctx)
			if err == nil {
				t.Fatal("expected the partial batch error")
			}
			if calls != 1 || stats.Deleted != 0 || stats.Retried != 2 {
				t.Fatalf("calls = %d, stats = %+v, want one call and two durable retries", calls, stats)
			}
			var tombstone bool
			var attempts int
			var nextAttempt time.Time
			var lockedBy string
			if err := pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL, attempt_count, next_attempt_at, locked_by
    FROM artwork_revision_gc_candidates WHERE original_path = $1`, paths[1]).Scan(&tombstone, &attempts, &nextAttempt, &lockedBy); err != nil {
				t.Fatal(err)
			}
			if !tombstone || attempts != 1 || !nextAttempt.After(time.Now()) || lockedBy != "" {
				t.Fatalf("failed candidate: tombstone=%t attempts=%d next=%v locked_by=%q", tombstone, attempts, nextAttempt, lockedBy)
			}
		})
	}
}

func TestArtworkRevisionGCRunHealsReferencePublishedDuringDelete(t *testing.T) {
	pool := artworkRevisionGCTestPool(t)
	ctx := t.Context()
	const path = "tmdb/movies/gc-run-heal/poster/original.gone.webp"
	const contentID = "gc-run-heal"
	const source = "https://images.example/gc-run-heal.jpg"
	if _, err := pool.Exec(ctx, `INSERT INTO artwork_revision_gc_candidates
  (original_path, image_type, object_keys, not_before, next_attempt_at)
  VALUES ($1, 'poster', ARRAY[$1]::text[], NOW() - interval '1 hour', NOW() - interval '1 hour')`, path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id = $1`, contentID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM artwork_revision_gc_candidates WHERE original_path = $1`, path)
	})
	deleter := artworkRevisionDeleteFunc(func(ctx context.Context, keys []string) (int, error) {
		_, err := pool.Exec(ctx, `INSERT INTO media_items (content_id, type, title, status, genres, poster_path, poster_source_path)
   VALUES ($1, 'movie', 'GC concurrent publication', 'matched', '{}'::text[], $2, $3)`, contentID, path, source)
		return len(keys), err
	})
	stats, err := NewArtworkRevisionGarbageCollector(pool, deleter).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Healed != 1 || stats.Deleted != 1 {
		t.Fatalf("stats = %+v, want one deletion and one healed reference", stats)
	}
	var got string
	if err := pool.QueryRow(ctx, `SELECT poster_path FROM media_items WHERE content_id = $1`, contentID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != source {
		t.Fatalf("poster_path = %q, want remote source %q", got, source)
	}
}

// newArtworkDeleteStore returns a blobstore S3 artwork store whose batch deletes
// answer every requested key with the given error code.
func newArtworkDeleteStore(t *testing.T, code string) *blobstore.S3 {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !r.URL.Query().Has("delete") {
			w.WriteHeader(http.StatusOK)
			return
		}
		var request struct {
			Objects []struct {
				Key string `xml:"Key"`
			} `xml:"Object"`
		}
		if err := xml.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode delete request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		type deleteError struct {
			Key  string `xml:"Key"`
			Code string `xml:"Code"`
		}
		response := struct {
			XMLName xml.Name      `xml:"DeleteResult"`
			Errors  []deleteError `xml:"Error"`
		}{}
		for _, object := range request.Objects {
			response.Errors = append(response.Errors, deleteError{Key: object.Key, Code: code})
		}
		_ = xml.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.Close)
	return blobstore.NewS3(s3client.NewClient(s3client.BucketConfig{
		Endpoint: server.URL, Bucket: "artwork", PathStyle: true, AccessKey: "test", SecretKey: "test",
	}))
}

func insertDueArtworkGCCandidate(t *testing.T, pool *pgxpool.Pool, path string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `INSERT INTO artwork_revision_gc_candidates
   (original_path, object_keys, not_before, next_attempt_at)
   VALUES ($1, ARRAY[$1]::text[], NOW() - interval '1 hour', NOW() - interval '1 hour')`, path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM artwork_revision_gc_candidates WHERE original_path = $1`, path)
	})
}

// TestArtworkRevisionGCRunTreatsAlreadyDeletedObjectsAsSuccess wires the real
// S3 artwork store to a server that reports every requested key as NoSuchKey,
// reproducing the deployed failure. An already-absent object satisfies the
// cleanup, so the run must succeed, finalize the candidate, and stay quiet.
func TestArtworkRevisionGCRunTreatsAlreadyDeletedObjectsAsSuccess(t *testing.T) {
	pool := artworkRevisionGCTestPool(t)
	ctx := t.Context()
	path := fmt.Sprintf("tmdb/movies/gc-absent-%d/poster/original.old.webp", time.Now().UnixNano())
	insertDueArtworkGCCandidate(t, pool, path)

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	stats, err := NewArtworkRevisionGarbageCollector(pool, newArtworkDeleteStore(t, "NoSuchKey")).Run(ctx)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil when the objects are already gone", err)
	}
	if stats.Deleted != 1 {
		t.Fatalf("stats = %+v, want one finalized revision", stats)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM artwork_revision_gc_candidates WHERE original_path = $1`, path).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("remaining candidates = %d, want 0 so the retry loop ends", remaining)
	}
	if strings.Contains(logs.String(), "partial failure") {
		t.Fatalf("already-absent object logged a partial-failure warning: %s", logs.String())
	}
}

// TestArtworkRevisionGCRunFailsOnGenuineDeleteError keeps a genuinely failing
// storage error fatal: the task fails, reports the real failure count, and
// reschedules the candidate instead of treating the key as gone.
func TestArtworkRevisionGCRunFailsOnGenuineDeleteError(t *testing.T) {
	pool := artworkRevisionGCTestPool(t)
	ctx := t.Context()
	path := fmt.Sprintf("tmdb/movies/gc-denied-%d/poster/original.old.webp", time.Now().UnixNano())
	insertDueArtworkGCCandidate(t, pool, path)

	stats, err := NewArtworkRevisionGarbageCollector(pool, newArtworkDeleteStore(t, "AccessDenied")).Run(ctx)
	if err == nil {
		t.Fatal("Run() error = nil, want a genuine storage failure")
	}
	if !strings.Contains(err.Error(), "1 of 1 objects failed") {
		t.Fatalf("Run() error = %q, want the real failure count", err)
	}
	if stats.Claimed != 1 || stats.Retried != 1 {
		t.Fatalf("stats = %+v, want one claimed and one retried", stats)
	}
	var deletedAt, nextAttempt *time.Time
	var lockedBy, lastError string
	if err := pool.QueryRow(ctx, `SELECT deleted_at, next_attempt_at, locked_by, last_error
   FROM artwork_revision_gc_candidates WHERE original_path = $1`, path).
		Scan(&deletedAt, &nextAttempt, &lockedBy, &lastError); err != nil {
		t.Fatal(err)
	}
	if nextAttempt == nil || !nextAttempt.After(time.Now()) || lockedBy != "" {
		t.Fatalf("candidate was not rescheduled: next=%v locked_by=%q", nextAttempt, lockedBy)
	}
	if !strings.Contains(lastError, "1 of 1 objects failed") {
		t.Fatalf("candidate last_error = %q, want the real failure count", lastError)
	}
}
