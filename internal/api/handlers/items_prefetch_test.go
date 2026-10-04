package handlers

import (
	"context"
	"testing"
	"time"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

// fakePrefetcher mirrors PrefetchVirtualPlayback's real admission gate: no
// user ID, no work.
type fakePrefetcher struct{ calls chan struct{} }

func (f *fakePrefetcher) PrefetchVirtualPlayback(ctx context.Context, _ []*models.MediaFile, profileID string) {
	if apimw.GetUserID(ctx) == 0 || profileID == "" {
		return
	}
	select {
	case f.calls <- struct{}{}:
	default:
	}
}

type fakeVirtualFiles struct {
	files map[int]*models.MediaFile
}

func (f *fakeVirtualFiles) GetByID(_ context.Context, id int) (*models.MediaFile, error) {
	return f.files[id], nil
}

// Watch-detail prefetch must carry request identity into the background task:
// the real PrefetchVirtualPlayback drops work with no user ID, so a prefetch
// that loses auth silently warms nothing after doing the row reads.
func TestWatchDetailPrefetchKeepsIdentity(t *testing.T) {
	prefetcher := &fakePrefetcher{calls: make(chan struct{}, 1)}
	h := &ItemsHandler{}
	h.SetVirtualPrefetcher(prefetcher, &fakeVirtualFiles{files: map[int]*models.MediaFile{
		7: {ID: 7, FilePath: "virtual://movie/tt1?result=a"},
	}})
	ctx := apimw.SetClaims(context.Background(), &auth.Claims{UserID: 42})
	detail := &catalog.WatchDetail{ContentID: "m1", Type: "movie", Versions: []catalog.FileVersion{{FileID: 7}}}
	h.prefetchWatchDetailVirtual(ctx, detail, "profile-1")
	select {
	case <-prefetcher.calls:
	case <-time.After(2 * time.Second):
		t.Fatal("prefetch task never admitted: background context lost auth identity")
	}
}
