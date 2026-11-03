package apiv2

import (
	"context"
	"net/http"
	"testing"
	"time"

	catalogpkg "github.com/Silo-Server/silo-server/internal/catalog"
)

// The catalog leaf detail wait prices itself against the deployment's
// configured budget; every failure mode (unwired reader, unreadable setting,
// non-positive answer) falls back to the built-in default rather than
// stretching the wait.
func TestPreplayDiscoveryCatalogBudgetForFallsBackToDefault(t *testing.T) {
	ctx := context.Background()

	if got := (&Registry{}).preplayDiscoveryCatalogBudgetFor(ctx); got != preplayDiscoveryCatalogBudget {
		t.Fatalf("unwired budget reader = %s, want the built-in default %s", got, preplayDiscoveryCatalogBudget)
	}

	for _, test := range []struct {
		name   string
		want   time.Duration
		reader func(context.Context) int
	}{
		{name: "configured default", want: 3 * time.Second, reader: func(context.Context) int { return 3000 }},
		{name: "configured custom", want: 900 * time.Millisecond, reader: func(context.Context) int { return 900 }},
		{name: "configured ceiling", want: 20 * time.Second, reader: func(context.Context) int { return 20000 }},
		{name: "non-positive answer", want: preplayDiscoveryCatalogBudget, reader: func(context.Context) int { return -1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			reg := &Registry{deps: Dependencies{PreplayDiscoveryBudgetMs: test.reader}}
			if got := reg.preplayDiscoveryCatalogBudgetFor(ctx); got != test.want {
				t.Fatalf("budget = %s, want %s", got, test.want)
			}
		})
	}
}

// watchDiscoveryFixture returns a watch detail handler whose provider listing
// blocks on gate, plus the fake that records the outcome. The detail starts with
// one persisted version (120); a completed listing publishes 121.
func watchDiscoveryFixture(t *testing.T) (*fakePreplayCatalog, http.Handler) {
	t.Helper()
	deps, base := catalogDeps(t)
	fake := &fakePreplayCatalog{
		fakeCatalog: base,
		detail: catalogpkg.ItemDetail{ContentID: "movie:heat-1995", Type: "movie",
			Versions: []catalogpkg.FileVersion{{FileID: 120, FilePath: "virtual://movie/tt0113277", AddedAt: fixedTime()}}},
		hasVirtual: true,
	}
	deps.CatalogItems = fake
	deps.Watch = &fakePreplayWatch{fakeWatch: &fakeWatch{}, catalog: fake}
	deps.VirtualCandidatesDiscovery = fake
	deps.PreplayDiscoveryEnabled = func(context.Context) bool { return true }
	return fake, newTestHandler(t, deps)
}

// setWatchDiscoveryBudget shrinks the watch wait for one test and restores it.
func setWatchDiscoveryBudget(t *testing.T, budget time.Duration) {
	t.Helper()
	previous := preplayDiscoveryWatchBudget
	preplayDiscoveryWatchBudget = budget
	t.Cleanup(func() { preplayDiscoveryWatchBudget = previous })
}

// A provider listing that outlives the watch budget must not fail the read. The
// watch detail still answers 200 from the persisted version list, which is what
// the start path's own resolve consumes; the listing is simply abandoned.
func TestWatchDetailDiscoveryTimeoutStillAnswers(t *testing.T) {
	fake, handler := watchDiscoveryFixture(t)
	setWatchDiscoveryBudget(t, 40*time.Millisecond)
	// The gate is never closed: the listing stays blocked until its budget ends.
	fake.discoveryGate = make(chan struct{})

	begin := time.Now()
	rec := do(t, handler, http.MethodGet, "/api/v2/watch/movie:heat-1995?library_id=2&file_id=120", "", viewerHeaders())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("watch detail took %v after the discovery budget expired", elapsed)
	}
	var body struct {
		Versions []struct {
			FileID string `json:"file_id"`
		} `json:"versions"`
	}
	decodeJSON(t, rec.Body, &body)
	if len(body.Versions) != 1 || body.Versions[0].FileID != "120" {
		t.Fatalf("versions = %+v, want the persisted 120 after the timeout", body.Versions)
	}
}

// The same slow listing that a short budget abandons must complete under the
// extended budget, so a 17–25 s provider has room to land where the old 2 s
// watch wait gave up. The second half proves the budget is what changed, not the
// fixture: the identical listing is abandoned under the short budget and lands
// under the extended one.
func TestWatchDetailDiscoveryExtendedBudgetCompletesSlowListing(t *testing.T) {
	const listingDelay = 120 * time.Millisecond
	run := func(t *testing.T, budget time.Duration, wantFileID string, wantElapsed time.Duration) {
		t.Helper()
		fake, handler := watchDiscoveryFixture(t)
		gate := make(chan struct{})
		fake.discoveryGate = gate
		setWatchDiscoveryBudget(t, budget)
		time.AfterFunc(listingDelay, func() { close(gate) })

		begin := time.Now()
		rec := do(t, handler, http.MethodGet, "/api/v2/watch/movie:heat-1995?library_id=2&file_id=120", "", viewerHeaders())
		elapsed := time.Since(begin)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Versions []struct {
				FileID string `json:"file_id"`
			} `json:"versions"`
		}
		decodeJSON(t, rec.Body, &body)
		if len(body.Versions) != 1 || body.Versions[0].FileID != wantFileID {
			t.Fatalf("versions = %+v, want %s", body.Versions, wantFileID)
		}
		if wantElapsed > 0 && elapsed < wantElapsed {
			t.Fatalf("watch detail answered in %v, before the listing's %v completed", elapsed, listingDelay)
		}
	}

	t.Run("short budget abandons the slow listing", func(t *testing.T) {
		run(t, 40*time.Millisecond, "120", 0)
	})
	t.Run("extended budget keeps waiting and serves the listing", func(t *testing.T) {
		run(t, time.Second, "121", listingDelay)
	})
}
