package apiv2

import (
	"context"
	"net/http"
	"testing"

	catalogpkg "github.com/Silo-Server/silo-server/internal/catalog"
)

// TestDetailPreplayDiscoveryCoalescesRepeatedWatchReads proves the player's
// inventory poll cannot re-list the provider and rewrite candidates on every
// watch read. A different selected file is a different preparation target and
// must still run discovery.
func TestDetailPreplayDiscoveryCoalescesRepeatedWatchReads(t *testing.T) {
	deps, base := catalogDeps(t)
	fake := &fakePreplayCatalog{
		fakeCatalog: base,
		detail: catalogpkg.ItemDetail{
			ContentID: "movie:heat-1995",
			Type:      "movie",
			Versions:  []catalogpkg.FileVersion{{FileID: 120, FilePath: "virtual://movie/tt0113277", AddedAt: fixedTime()}},
		},
		hasVirtual: true,
	}
	deps.CatalogItems = fake
	deps.Watch = &fakePreplayWatch{fakeWatch: &fakeWatch{}, catalog: fake}
	deps.VirtualCandidatesDiscovery = fake
	deps.PreplayDiscoveryEnabled = func(context.Context) bool { return true }
	handler := newTestHandler(t, deps)

	for attempt := 0; attempt < 3; attempt++ {
		rec := do(t, handler, http.MethodGet, "/api/v2/watch/movie:heat-1995?library_id=2&file_id=120", "", viewerHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d: %s", attempt, rec.Body.String())
		}
	}
	if fake.discoveries != 1 {
		t.Fatalf("discoveries after three identical watch reads = %d, want 1", fake.discoveries)
	}

	rec := do(t, handler, http.MethodGet, "/api/v2/watch/movie:heat-1995?library_id=2&file_id=121", "", viewerHeaders())
	if rec.Code != http.StatusOK {
		t.Fatalf("selected-file read: %s", rec.Body.String())
	}
	if fake.discoveries != 2 {
		t.Fatalf("discoveries after selecting another file = %d, want 2", fake.discoveries)
	}
}
