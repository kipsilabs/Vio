package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/adminjob"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/collections/templates"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests exercise the real collection-creation paths against the
// package's disposable Postgres (see materializeTestPool and
// disposable_db_test.go). They are skipped when SILO_TEST_DATABASE_URL is
// unset, like the rest of the suite.

// newVPTestRegistry builds a one-template registry whose only template is a
// TMDB-franchise placeholder (collection_id 0). That source skips the initial
// sync, so an apply creates the collection and stores its source_config
// without any provider network traffic.
func newVPTestRegistry() *templates.Registry {
	reg := templates.NewRegistry()
	reg.Register(templates.Template{
		ID:             "vp_test_placeholder",
		Title:          "VP Test Placeholder",
		Description:    "Placeholder for virtual playback persistence tests.",
		Icon:           "x",
		Category:       templates.CategoryEditorial,
		Source:         templates.SourceTMDBCollection,
		MediaKind:      templates.MediaMovie,
		TMDBCollection: &templates.TMDBCollectionSpec{CollectionID: 0},
	})
	reg.RegisterBundle(templates.Bundle{
		ID:          "vp_test_bundle",
		Title:       "VP Test Bundle",
		Description: "One placeholder template.",
		TemplateIDs: []string{"vp_test_placeholder"},
	})
	return reg
}

func seedVPFolder(t *testing.T, pool *pgxpool.Pool, id int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO media_folders(id,name,type,enabled) VALUES($1,$2,'movies',true)`, id, fmt.Sprintf("VP Test %d", id)); err != nil {
		t.Fatalf("seed folder %d: %v", id, err)
	}
}

func seedVPUser(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var id int
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO users(username,role) VALUES($1,'admin') RETURNING id`, fmt.Sprintf("vp-test-%d", time.Now().UnixNano())).Scan(&id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func newVPTestHandler(t *testing.T, pool *pgxpool.Pool, registry *templates.Registry) (*LibraryCollectionHandler, *catalog.LibraryCollectionRepository) {
	t.Helper()
	itemRepo := catalog.NewItemRepository(pool)
	collRepo := catalog.NewLibraryCollectionRepository(pool)
	service := catalog.NewLibraryCollectionService(collRepo, itemRepo, nil, nil)
	h := NewLibraryCollectionHandler(collRepo, service, itemRepo, nil)
	h.FolderRepo = catalog.NewFolderRepository(pool)
	h.JobRepo = adminjob.NewRepository(pool)
	if registry != nil {
		h.TemplateRegistry = registry
	}
	return h, collRepo
}

func findVPCollectionByTitle(t *testing.T, ctx context.Context, repo *catalog.LibraryCollectionRepository, libraryID int, title string) *models.LibraryCollection {
	t.Helper()
	collections, err := repo.ListByLibrary(ctx, libraryID, catalog.ListLibraryCollectionsOptions{IncludeHidden: true})
	if err != nil {
		t.Fatalf("list collections: %v", err)
	}
	for _, c := range collections {
		if c.Title == title {
			return c
		}
	}
	t.Fatalf("collection %q not found in library %d", title, libraryID)
	return nil
}

func findVPCollectionByManagementSource(t *testing.T, ctx context.Context, repo *catalog.LibraryCollectionRepository, libraryID int, source string) *models.LibraryCollection {
	t.Helper()
	collections, err := repo.ListByLibrary(ctx, libraryID, catalog.ListLibraryCollectionsOptions{IncludeHidden: true})
	if err != nil {
		t.Fatalf("list collections: %v", err)
	}
	for _, c := range collections {
		if c.ManagementSource == source {
			return c
		}
	}
	t.Fatalf("collection managed by %q not found in library %d", source, libraryID)
	return nil
}

var vpTestVariants = []struct {
	name  string
	input *bool
	want  bool
}{
	{name: "omitted_defaults_on", input: nil, want: true},
	{name: "explicit_false", input: new(false), want: false},
	{name: "explicit_true", input: new(true), want: true},
}

// TestQueueAdminCollectionTemplatePersistsResolvedVirtualPlayback is the
// durable-job regression test: it queues a bundle apply through the real
// handler, inspects the stored RequestPayload for an explicit boolean, then
// decodes that payload and executes it and asserts the created collection's
// stored source_config. Deleting the resolution in QueueAdminCollectionTemplate
// now fails the omitted and explicit-true cases.
func TestQueueAdminCollectionTemplatePersistsResolvedVirtualPlayback(t *testing.T) {
	pool := materializeTestPool(t)
	ctx := context.Background()
	h, collRepo := newVPTestHandler(t, pool, newVPTestRegistry())
	userID := seedVPUser(t, pool)

	for i, tc := range vpTestVariants {
		t.Run(tc.name, func(t *testing.T) {
			libraryID := 9601 + i
			seedVPFolder(t, pool, libraryID)

			job, err := h.QueueAdminCollectionTemplate(ctx, "vp_test_bundle", applyTemplateBundleRequest{
				LibraryIDs:      []int{libraryID},
				VirtualPlayback: tc.input,
			}, userID)
			if err != nil {
				t.Fatalf("queue bundle apply: %v", err)
			}

			// (a) The stored payload must carry an explicit boolean.
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(job.RequestPayload, &payload); err != nil {
				t.Fatalf("decode stored payload: %v", err)
			}
			raw, ok := payload["virtual_playback"]
			if !ok {
				t.Fatalf("stored payload omitted virtual_playback: %s", job.RequestPayload)
			}
			var stored bool
			if err := json.Unmarshal(raw, &stored); err != nil {
				t.Fatalf("decode stored virtual_playback: %v", err)
			}
			if stored != tc.want {
				t.Fatalf("stored virtual_playback = %v, want %v (payload %s)", stored, tc.want, job.RequestPayload)
			}

			// (b) Decode the stored job and run it through the real executor.
			var req adminjob.TemplateBundleApplyRequest
			if err := json.Unmarshal(job.RequestPayload, &req); err != nil {
				t.Fatalf("decode stored job request: %v", err)
			}
			if req.VirtualPlayback != tc.want {
				t.Fatalf("decoded job virtual_playback = %v, want %v", req.VirtualPlayback, tc.want)
			}
			if _, err := h.ExecuteTemplateBundleApply(ctx, req, nil); err != nil {
				t.Fatalf("execute stored job: %v", err)
			}

			collection := findVPCollectionByManagementSource(t, ctx, collRepo, libraryID, "vp_test_bundle")
			if got := catalog.SourceEnablesVirtualPlayback(collection.SourceConfig); got != tc.want {
				t.Fatalf("stored source_config virtual playback = %v, want %v (%s)", got, tc.want, collection.SourceConfig)
			}
		})
	}
}

// TestQueuedTemplateBundleApplyLegacyPayloadStaysOff documents the migration
// rule for jobs already queued before virtual_playback was persisted: a
// field-less payload must keep its original off meaning rather than inherit
// the newer "omitted means on" API default.
func TestQueuedTemplateBundleApplyLegacyPayloadStaysOff(t *testing.T) {
	pool := materializeTestPool(t)
	ctx := context.Background()
	h, collRepo := newVPTestHandler(t, pool, newVPTestRegistry())

	const libraryID = 9610
	seedVPFolder(t, pool, libraryID)

	legacy := json.RawMessage(fmt.Sprintf(`{"bundle_id":"vp_test_bundle","library_ids":[%d]}`, libraryID))
	var req adminjob.TemplateBundleApplyRequest
	if err := json.Unmarshal(legacy, &req); err != nil {
		t.Fatalf("decode legacy payload: %v", err)
	}
	if req.VirtualPlayback {
		t.Fatal("legacy field-less payload decoded on; must stay off")
	}
	if _, err := h.ExecuteTemplateBundleApply(ctx, req, nil); err != nil {
		t.Fatalf("execute legacy job: %v", err)
	}

	collection := findVPCollectionByManagementSource(t, ctx, collRepo, libraryID, "vp_test_bundle")
	if got := catalog.SourceEnablesVirtualPlayback(collection.SourceConfig); got {
		t.Fatalf("legacy job stored virtual playback on, want off (%s)", collection.SourceConfig)
	}
}

// TestImportCreationPersistsResolvedVirtualPlayback drives each import type
// through the handler method that builds and stores the collection (the
// MDBList/TMDB create paths and the exported Trakt import), rather than
// reconstructing the source_config builders in the test.
func TestImportCreationPersistsResolvedVirtualPlayback(t *testing.T) {
	pool := materializeTestPool(t)
	ctx := context.Background()
	h, collRepo := newVPTestHandler(t, pool, nil)

	const libraryID = 9620
	seedVPFolder(t, pool, libraryID)

	sources := []struct {
		name   string
		create func(t *testing.T, title string, vp *bool)
	}{
		{
			name: "mdblist",
			create: func(t *testing.T, title string, vp *bool) {
				if _, err := h.createMDBListCollection(ctx, importMDBListRequest{
					LibraryID:       libraryID,
					Title:           title,
					URL:             "https://mdblist.com/lists/user/list",
					VirtualPlayback: vp,
				}); err != nil {
					t.Fatalf("create MDBList collection: %v", err)
				}
			},
		},
		{
			name: "tmdb_preset",
			create: func(t *testing.T, title string, vp *bool) {
				if _, err := h.createTMDBCollection(ctx, importTMDBRequest{
					LibraryID:       libraryID,
					Title:           title,
					Preset:          "popular",
					MediaType:       "movie",
					VirtualPlayback: vp,
				}); err != nil {
					t.Fatalf("create TMDB collection: %v", err)
				}
			},
		},
		{
			name: "tmdb_franchise",
			create: func(t *testing.T, title string, vp *bool) {
				if _, err := h.createTMDBFranchiseCollection(ctx, importTMDBFranchiseRequest{
					LibraryID:       libraryID,
					Title:           title,
					CollectionID:    86311,
					VirtualPlayback: vp,
				}); err != nil {
					t.Fatalf("create TMDB franchise collection: %v", err)
				}
			},
		},
		{
			name: "tmdb_discover",
			create: func(t *testing.T, title string, vp *bool) {
				if _, err := h.createTMDBDiscoverCollection(ctx, importTMDBDiscoverRequest{
					LibraryID:       libraryID,
					Title:           title,
					MediaType:       "movie",
					Spec:            importTMDBDiscoverSpecBody{SortBy: "popularity.desc"},
					VirtualPlayback: vp,
				}); err != nil {
					t.Fatalf("create TMDB discover collection: %v", err)
				}
			},
		},
	}

	for _, src := range sources {
		for _, variant := range vpTestVariants {
			t.Run(src.name+"/"+variant.name, func(t *testing.T) {
				title := fmt.Sprintf("VP %s %s", src.name, variant.name)
				src.create(t, title, variant.input)
				collection := findVPCollectionByTitle(t, ctx, collRepo, libraryID, title)
				if got := catalog.SourceEnablesVirtualPlayback(collection.SourceConfig); got != variant.want {
					t.Fatalf("stored source_config virtual playback = %v, want %v (%s)", got, variant.want, collection.SourceConfig)
				}
			})
		}
	}
}
