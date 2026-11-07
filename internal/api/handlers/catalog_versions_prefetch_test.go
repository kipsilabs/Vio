package handlers

import (
	"context"
	"fmt"
	"testing"
	"time"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

// recordingPrefetcher captures the rows one detached prefetch admitted.
type recordingPrefetcher struct {
	calls chan []*models.MediaFile
}

func (f *recordingPrefetcher) PrefetchVirtualPlayback(_ context.Context, files []*models.MediaFile, _ string) {
	f.calls <- files
}

// TestVersionsReadWarmsAtMostTwoProviderListings is the load bound for the
// versions read: however many virtual versions a large item lists, one read
// warms at most two provider listings. The warm is metadata-only and detached;
// the read itself never waits for it.
func TestVersionsReadWarmsAtMostTwoProviderListings(t *testing.T) {
	const listed = 6
	versions := make([]catalog.FileVersion, 0, listed)
	files := make(map[int]*models.MediaFile, listed)
	for i := 1; i <= listed; i++ {
		versions = append(versions, catalog.FileVersion{FileID: i, FilePath: fmt.Sprintf("virtual://movie/tt0113277?result=%d", i)})
		files[i] = &models.MediaFile{ID: i, FilePath: fmt.Sprintf("virtual://movie/tt0113277?result=%d", i)}
	}
	loader := &countingVirtualFiles{files: files}
	prefetcher := &recordingPrefetcher{calls: make(chan []*models.MediaFile, 2)}

	items := &ItemsHandler{}
	items.SetVirtualPrefetcher(prefetcher, loader)
	h := &CatalogResourceHandler{items: items}
	ctx := apimw.SetClaims(context.Background(), &auth.Claims{UserID: 42})
	h.PrefetchItemVersionsVirtual(ctx, versions, "profile-1")

	select {
	case warmed := <-prefetcher.calls:
		if len(warmed) > 2 {
			t.Fatalf("prefetch admitted %d provider listings, want at most 2", len(warmed))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("versions warm never reached the prefetcher")
	}
	// The row loads are the other half of the bound: six listed rows must not
	// become six GetByID calls.
	if got := loader.loads(); got > 2 {
		t.Fatalf("versions warm loaded %d rows, want at most 2", got)
	}
}

// TestVersionsWarmIsNoOpWhenUnwired: a handler without the items seam, and an
// items handler without a prefetcher, stay side-effect free instead of
// panicking on a versions read.
func TestVersionsWarmIsNoOpWhenUnwired(t *testing.T) {
	versions := []catalog.FileVersion{{FileID: 1, FilePath: "virtual://movie/tt0113277?result=1"}}
	ctx := apimw.SetClaims(context.Background(), &auth.Claims{UserID: 42})
	(&CatalogResourceHandler{}).PrefetchItemVersionsVirtual(ctx, versions, "profile-1")
	(&CatalogResourceHandler{items: &ItemsHandler{}}).PrefetchItemVersionsVirtual(ctx, versions, "profile-1")
}
