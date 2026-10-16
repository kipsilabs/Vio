package handlers

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/artworkurl"
	"github.com/Silo-Server/silo-server/internal/blobstore"
	"github.com/Silo-Server/silo-server/internal/blobstore/blobstoretest"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/metadata"
)

// localArtworkPosters models the resolver a local-disk artwork backend wires:
// every stored poster key comes back as the root-relative signed route the
// server itself serves, with no scheme or host.
type localArtworkPosters struct {
	signer *artworkurl.Signer
	keys   map[string]string
}

func (r localArtworkPosters) PresignImageURLs(_ context.Context, paths []string, _, _ string) map[string]string {
	out := make(map[string]string, len(paths))
	for _, path := range paths {
		if key, ok := r.keys[path]; ok {
			url, _ := r.signer.Sign(key, time.Now())
			out[path] = url
		}
	}
	return out
}

// absoluteArtworkPosters models an S3 or public resolver: every stored poster
// key comes back as an absolute URL the composer fetches over HTTP.
type absoluteArtworkPosters struct {
	base string
	keys map[string]string
}

func (r absoluteArtworkPosters) PresignImageURLs(_ context.Context, paths []string, _, _ string) map[string]string {
	out := make(map[string]string, len(paths))
	for _, path := range paths {
		if key, ok := r.keys[path]; ok {
			out[path] = r.base + "/" + key
		}
	}
	return out
}

func collageSourceKey(prefix, id string) string {
	return "collection-images/" + id + "/poster/w500." + prefix + ".jpg"
}

// A collection collage built from local-disk artwork storage must read its
// source posters straight from that store. The local resolver hands back a
// root-relative signed route, which no HTTP client can request, so fetching it
// over HTTP fails with "unsupported protocol scheme".
func TestComposeCollectionCollageReadsLocalArtworkFromStore(t *testing.T) {
	ctx := t.Context()
	posterA, posterB := testCollectionPosterJPEG(t), testCollectionPosterJPEG(t)

	srcA, srcB := "movies/one/poster.jpg", "movies/two/poster.jpg"
	keyA, keyB := collageSourceKey("aaaa", "one"), collageSourceKey("bbbb", "two")

	store := blobstoretest.New()
	if err := store.Put(ctx, keyA, posterA); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, keyB, posterB); err != nil {
		t.Fatal(err)
	}

	signer := artworkurl.NewSigner("collage-local-artwork-test-secret", time.Hour)
	gen := collectionCollageComposer{
		prefix: adminCollectionImagePrefix,
		store:  store,
		posters: localArtworkPosters{
			signer: signer,
			keys:   map[string]string{srcA: keyA, srcB: keyB},
		},
		signer:     signer,
		httpClient: newCollectionImageClient(),
	}

	const key = "0123456789abcdef"
	path, thumbhash, err := gen.ComposeCollectionCollage(ctx, "c1", key, []string{srcA, srcB})
	if err != nil {
		t.Fatalf("compose from local artwork storage: %v", err)
	}
	if path != gen.CollectionCollagePath("c1", key) || thumbhash == "" {
		t.Fatalf("stored %q with thumbhash %q", path, thumbhash)
	}
	for _, objectKey := range []string{
		"collection-images/c1/collage/original." + key + ".webp",
		"collection-images/c1/collage/w500." + key + ".webp",
		"collection-images/c1/collage/w300." + key + ".webp",
	} {
		if _, ok := store.Objects[objectKey]; !ok {
			t.Fatalf("object %s was not stored", objectKey)
		}
	}
}

// The two resolver shapes -- a root-relative local route read from the store,
// and an absolute S3/public URL fetched over HTTP -- must produce the same
// composed collage.
func TestComposeCollectionCollageLocalAndRemoteSourcesAgree(t *testing.T) {
	ctx := t.Context()
	posterA, posterB := testCollectionPosterJPEG(t), testCollectionPosterJPEG(t)

	srcA, srcB := "movies/one/poster.jpg", "movies/two/poster.jpg"
	keyA, keyB := collageSourceKey("aaaa", "one"), collageSourceKey("bbbb", "two")

	localStore := blobstoretest.New()
	if err := localStore.Put(ctx, keyA, posterA); err != nil {
		t.Fatal(err)
	}
	if err := localStore.Put(ctx, keyB, posterB); err != nil {
		t.Fatal(err)
	}
	localSigner := artworkurl.NewSigner("collage-local-artwork-test-secret", time.Hour)
	localGen := collectionCollageComposer{
		prefix: adminCollectionImagePrefix,
		store:  localStore,
		posters: localArtworkPosters{
			signer: localSigner,
			keys:   map[string]string{srcA: keyA, srcB: keyB},
		},
		signer:     localSigner,
		httpClient: newCollectionImageClient(),
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + keyA:
			_, _ = w.Write(posterA)
		case "/" + keyB:
			_, _ = w.Write(posterB)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	remoteStore := blobstoretest.New()
	remoteGen := collectionCollageComposer{
		prefix: adminCollectionImagePrefix,
		store:  remoteStore,
		posters: absoluteArtworkPosters{
			base: server.URL,
			keys: map[string]string{srcA: keyA, srcB: keyB},
		},
		httpClient: server.Client(),
	}

	const key = "0123456789abcdef"
	localPath, localThumb, err := localGen.ComposeCollectionCollage(ctx, "c1", key, []string{srcA, srcB})
	if err != nil {
		t.Fatalf("compose from local artwork storage: %v", err)
	}
	remotePath, remoteThumb, err := remoteGen.ComposeCollectionCollage(ctx, "c1", key, []string{srcA, srcB})
	if err != nil {
		t.Fatalf("compose from absolute artwork URLs: %v", err)
	}
	if localPath != remotePath || localThumb != remoteThumb {
		t.Fatalf("local %q / %q, remote %q / %q; want the same collage", localPath, localThumb, remotePath, remoteThumb)
	}
	for _, objectKey := range []string{
		"collection-images/c1/collage/original." + key + ".webp",
		"collection-images/c1/collage/w500." + key + ".webp",
		"collection-images/c1/collage/w300." + key + ".webp",
	} {
		localData, ok := localStore.Objects[objectKey]
		if !ok {
			t.Fatalf("local compose did not store %s", objectKey)
		}
		remoteData, ok := remoteStore.Objects[objectKey]
		if !ok {
			t.Fatalf("remote compose did not store %s", objectKey)
		}
		if !bytes.Equal(localData, remoteData) {
			t.Fatalf("object %s differs between local and remote compose", objectKey)
		}
	}
}

// A local route that names a missing store object fails the build with a clean
// error and stores nothing.
func TestComposeCollectionCollageMissingLocalArtworkFails(t *testing.T) {
	ctx := t.Context()
	signer := artworkurl.NewSigner("collage-local-artwork-test-secret", time.Hour)
	store := blobstoretest.New()

	src := "movies/missing/poster.jpg"
	gen := collectionCollageComposer{
		prefix: adminCollectionImagePrefix,
		store:  store,
		posters: localArtworkPosters{
			signer: signer,
			keys:   map[string]string{src: collageSourceKey("cccc", "missing")},
		},
		signer:     signer,
		httpClient: newCollectionImageClient(),
	}

	_, _, err := gen.ComposeCollectionCollage(ctx, "c1", "0123456789abcdef", []string{src})
	if err == nil {
		t.Fatal("a collage whose local source is missing was built")
	}
	if !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("compose with a missing local source = %v, want ErrNotFound", err)
	}
	if len(store.Objects) != 0 {
		t.Fatalf("a failed build stored %d objects", len(store.Objects))
	}
}

// recordingArtworkRepair records the keys a collage build queues for repair.
type recordingArtworkRepair struct{ keys []string }

func (r *recordingArtworkRepair) EnqueueArtworkRepair(_ context.Context, keys []string, _ int) (int, error) {
	r.keys = append(r.keys, keys...)
	return len(keys), nil
}

// A local source that names a missing revisioned object is queued for
// regeneration, as the signed artwork route does, so the collage can be
// rebuilt once the object is back.
func TestComposeCollectionCollageMissingLocalArtworkQueuesRepair(t *testing.T) {
	ctx := t.Context()
	signer := artworkurl.NewSigner("collage-local-artwork-test-secret", time.Hour)
	repair := &recordingArtworkRepair{}

	src := "movies/missing/poster.jpg"
	key := collageSourceKey("cccc", "missing")
	gen := collectionCollageComposer{
		prefix: adminCollectionImagePrefix,
		store:  blobstoretest.New(),
		posters: localArtworkPosters{
			signer: signer,
			keys:   map[string]string{src: key},
		},
		signer:     signer,
		repair:     repair,
		httpClient: newCollectionImageClient(),
	}

	if _, _, err := gen.ComposeCollectionCollage(ctx, "c1", "0123456789abcdef", []string{src}); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("compose with a missing local source = %v, want ErrNotFound", err)
	}
	if want := "collection-images/missing/poster/original.cccc.jpg"; len(repair.keys) != 1 || repair.keys[0] != want {
		t.Fatalf("queued repair keys = %v, want [%s]", repair.keys, want)
	}
}

// The production local-artwork wiring -- a real filesystem blob store, the
// artwork signer, and the catalog resolver chain -- composes a collage from
// sources in that store with no HTTP request.
func TestComposeCollectionCollageProductionLocalWiring(t *testing.T) {
	ctx := t.Context()
	posterA, posterB := testCollectionPosterJPEG(t), testCollectionPosterJPEG(t)

	store, err := blobstore.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	signer := artworkurl.NewSigner("collage-local-artwork-test-secret", time.Hour)
	resolver := metadata.NewPluginImageResolver()
	t.Cleanup(resolver.Close)
	resolver.SetArtworkResolver(artworkurl.NewServerResolver(signer))
	detail := &catalog.DetailService{}
	detail.SetImageResolver(resolver)

	// Cached catalog poster paths, as media_items.poster_path holds them.
	srcA := "tmdb/movies/550/poster/original.aaaaaaaaaaaaaaaa.webp"
	srcB := "tmdb/movies/551/poster/original.bbbbbbbbbbbbbbbb.webp"

	// The production resolver names the store key for each source; put the
	// poster bytes there, as a real local backend would have them.
	resolved := detail.PresignImageURLs(ctx, []string{srcA, srcB}, "poster", "small")
	for src, poster := range map[string][]byte{srcA: posterA, srcB: posterB} {
		key, ok := signer.SignedKey(resolved[src], time.Now())
		if !ok {
			t.Fatalf("resolver produced %q for %s, not a signed local route", resolved[src], src)
		}
		if err := store.Put(ctx, key, poster); err != nil {
			t.Fatal(err)
		}
	}

	gen := NewPersonalCollectionCollageGenerator(store, detail, signer, nil, nil)
	if gen == nil {
		t.Fatal("no collage generator")
	}

	const key = "0123456789abcdef"
	path, thumbhash, err := gen.ComposeCollectionCollage(ctx, "c1", key, []string{srcA, srcB})
	if err != nil {
		t.Fatalf("compose with production local wiring: %v", err)
	}
	if want := gen.CollectionCollagePath("c1", key); path != want || thumbhash == "" {
		t.Fatalf("stored %q (%q), want %q with a thumbhash", path, thumbhash, want)
	}
	if _, _, err := store.Get(ctx, path); err != nil {
		t.Fatalf("composed collage not readable from the store: %v", err)
	}
}
