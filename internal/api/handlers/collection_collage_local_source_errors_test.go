package handlers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/artworkurl"
	"github.com/Silo-Server/silo-server/internal/blobstore"
	"github.com/Silo-Server/silo-server/internal/blobstore/blobstoretest"
	"github.com/Silo-Server/silo-server/internal/collage"
)

const collageSourceSecret = "collage-local-artwork-test-secret"

// countingGetStore wraps the in-memory store and counts every Get, hit or
// miss, so a test can prove the composer never read the store.
type countingGetStore struct {
	*blobstoretest.Memory
	gets atomic.Int32
}

func (s *countingGetStore) Get(ctx context.Context, key string) (io.ReadCloser, blobstore.ObjectInfo, error) {
	s.gets.Add(1)
	return s.Memory.Get(ctx, key)
}

// staticPosters returns a precomputed URL for each path, so a test controls the
// exact URL the composer receives, including one no signer would mint.
type staticPosters map[string]string

func (p staticPosters) PresignImageURLs(_ context.Context, paths []string, _, _ string) map[string]string {
	out := make(map[string]string, len(paths))
	for _, path := range paths {
		if url, ok := p[path]; ok {
			out[path] = url
		}
	}
	return out
}

// localCollageComposer wires a composer whose resolver signs local artwork keys
// with signer, as the local backend does.
func localCollageComposer(store blobstore.Store, signer *artworkurl.Signer, sources map[string]string) collectionCollageComposer {
	return collectionCollageComposer{
		prefix:     adminCollectionImagePrefix,
		store:      store,
		posters:    localArtworkPosters{signer: signer, keys: sources},
		signer:     signer,
		httpClient: newCollectionImageClient(),
	}
}

// A stored source over the 10 MB limit fails the build before anything is
// composed, and stores no collage.
func TestComposeCollectionCollageRejectsOversizedStoredSource(t *testing.T) {
	ctx := t.Context()
	signer := artworkurl.NewSigner(collageSourceSecret, time.Hour)
	store := blobstoretest.New()
	src := "movies/big/poster.jpg"
	key := collageSourceKey("dddd", "big")
	if err := store.Put(ctx, key, bytes.Repeat([]byte{0x7f}, collectionImageMaxBytes+1)); err != nil {
		t.Fatal(err)
	}
	gen := localCollageComposer(store, signer, map[string]string{src: key})

	const key0 = "0123456789abcdef"
	_, _, err := gen.ComposeCollectionCollage(ctx, "c1", key0, []string{src})
	if err == nil {
		t.Fatal("a collage from an oversized stored source was built")
	}
	if !strings.Contains(err.Error(), "over the") || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized stored source = %v, want the size-limit error", err)
	}
	if _, ok := store.Objects[gen.CollectionCollagePath("c1", key0)]; ok {
		t.Fatal("an oversized stored source stored a collage")
	}
}

// An expired local source URL fails with the explicit signature error and never
// reaches the store. The storedArtworkKey clock seam pins expiry to a fixed
// instant instead of the wall clock.
func TestCollectionCollageExpiredLocalSourceFailsWithSignatureError(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	signer := artworkurl.NewSigner(collageSourceSecret, time.Hour)
	// A key without a revision keeps the short expiry bucket, so a URL signed
	// in the past is already expired.
	key := "collection-images/c1/poster/original.jpg"
	expired, exp := signer.SignFor(key, now.Add(-2*time.Hour), time.Minute)
	if !exp.Before(now) {
		t.Fatalf("test URL expires at %s, not before %s", exp, now)
	}
	store := &countingGetStore{Memory: blobstoretest.New()}
	gen := localCollageComposer(store, signer, nil)

	if got, ok := gen.storedArtworkKey(expired, now); ok {
		t.Fatalf("storedArtworkKey(expired) = %q, want rejection", got)
	}
	if _, err := gen.fetchImage(t.Context(), expired); err == nil || !strings.Contains(err.Error(), "expired or bad signature") {
		t.Fatalf("expired local source = %v, want the signature error", err)
	}
	if n := store.gets.Load(); n != 0 {
		t.Fatalf("expired local source read the store %d times", n)
	}
}

// A local URL signed with a different secret is not honored: the composer
// reports a signature error and never reads the store, even though the object
// is present.
func TestCollectionCollageWrongSignerSecretDoesNotReadStore(t *testing.T) {
	ctx := t.Context()
	key := collageSourceKey("eeee", "one")
	store := &countingGetStore{Memory: blobstoretest.New()}
	if err := store.Put(ctx, key, testCollectionPosterJPEG(t)); err != nil {
		t.Fatal(err)
	}
	signer := artworkurl.NewSigner(collageSourceSecret, time.Hour)
	foreign, _ := artworkurl.NewSigner("some-other-signer-secret", time.Hour).Sign(key, time.Now())
	gen := localCollageComposer(store, signer, nil)

	if _, err := gen.fetchImage(ctx, foreign); err == nil || !strings.Contains(err.Error(), "expired or bad signature") {
		t.Fatalf("foreign-signed local source = %v, want the signature error", err)
	}
	if n := store.gets.Load(); n != 0 {
		t.Fatalf("foreign-signed local source read the store %d times", n)
	}
}

// A stored source that is not decodable image data fails the build cleanly and
// stores no collage.
func TestComposeCollectionCollageRejectsCorruptStoredSource(t *testing.T) {
	ctx := t.Context()
	signer := artworkurl.NewSigner(collageSourceSecret, time.Hour)
	store := blobstoretest.New()
	src := "movies/corrupt/poster.jpg"
	key := collageSourceKey("ffff", "corrupt")
	if err := store.Put(ctx, key, []byte("this is not image data")); err != nil {
		t.Fatal(err)
	}
	gen := localCollageComposer(store, signer, map[string]string{src: key})

	const key0 = "0123456789abcdef"
	_, _, err := gen.ComposeCollectionCollage(ctx, "c1", key0, []string{src})
	if !errors.Is(err, collage.ErrNotEnoughImages) {
		t.Fatalf("corrupt stored source = %v, want collage.ErrNotEnoughImages", err)
	}
	if _, ok := store.Objects[gen.CollectionCollagePath("c1", key0)]; ok {
		t.Fatal("a corrupt stored source stored a collage")
	}
}

// A composer with no signer cannot read a root-relative source, and says so
// explicitly instead of attempting an HTTP fetch of a URL with no host.
func TestCollectionCollageLocalSourceWithoutSignerFailsConfiguration(t *testing.T) {
	store := &countingGetStore{Memory: blobstoretest.New()}
	src := "movies/one/poster.jpg"
	gen := collectionCollageComposer{
		prefix:     adminCollectionImagePrefix,
		store:      store,
		posters:    staticPosters{src: "/api/v2/artwork/collection-images/one/poster/original.jpg?exp=9999999999&sig=AAAA"},
		httpClient: newCollectionImageClient(),
	}

	if _, _, err := gen.ComposeCollectionCollage(t.Context(), "c1", "0123456789abcdef", []string{src}); !errors.Is(err, errCollageArtworkSignerNotConfigured) {
		t.Fatalf("nil-signer local source = %v, want the configuration error", err)
	}
	if n := store.gets.Load(); n != 0 {
		t.Fatalf("nil-signer local source read the store %d times", n)
	}
}

// A route-prefixed URL whose key has a ".." or empty path segment is refused by
// key shape alone, before expiry or signature is consulted, and never reaches
// the store.
func TestCollectionCollageRejectsMalformedArtworkKeyPath(t *testing.T) {
	signer := artworkurl.NewSigner(collageSourceSecret, time.Hour)
	store := &countingGetStore{Memory: blobstoretest.New()}
	gen := localCollageComposer(store, signer, nil)
	now := time.Now()

	for name, rawURL := range map[string]string{
		"parent segment": "/api/v2/artwork/collection-images/../secret.webp?exp=9999999999&sig=AAAA",
		"empty segment":  "/api/v2/artwork/collection-images//secret.webp?exp=9999999999&sig=AAAA",
	} {
		if key, ok := gen.storedArtworkKey(rawURL, now); ok {
			t.Errorf("%s: storedArtworkKey(%s) = %q, want rejection", name, rawURL, key)
		}
		if _, err := gen.fetchImage(t.Context(), rawURL); err == nil || !strings.Contains(err.Error(), "expired or bad signature") {
			t.Errorf("%s: fetchImage = %v, want the signature error", name, err)
		}
	}
	if n := store.gets.Load(); n != 0 {
		t.Fatalf("malformed artwork key path read the store %d times", n)
	}
}
