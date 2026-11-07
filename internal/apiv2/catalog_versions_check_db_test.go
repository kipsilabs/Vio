package apiv2

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
	catalogpkg "github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/scanner"
)

// TestCheckCatalogVersionsStampVisibleInVersionsDB is the coherence check for
// the F6 wiring gap: a dead pin stamped through the v2 liveness check is the
// same durable row listCatalogItemVersions reports as failed. Both operations
// read and write one catalog, so a client can trust the check's verdict when it
// renders the version menu.
func TestCheckCatalogVersionsStampVisibleInVersionsDB(t *testing.T) {
	pool := viewerAccessTestPool(t)
	sfx := strconv.FormatInt(time.Now().UnixNano(), 10)
	libraryID := seedLibrary(t, pool, "versions-check-"+sfx)
	contentID := "movie:versions-check-" + sfx
	t.Cleanup(func() {
		mustExec(t, pool, `DELETE FROM media_files WHERE content_id = $1`, contentID)
		mustExec(t, pool, `DELETE FROM media_item_libraries WHERE content_id = $1`, contentID)
		mustExec(t, pool, `DELETE FROM media_items WHERE content_id = $1`, contentID)
	})
	mustExec(t, pool, `INSERT INTO media_items (content_id,type,title,genres,default_metadata_language) VALUES ($1,'movie','Versions Check','{}','en')`, contentID)
	mustExec(t, pool, `INSERT INTO media_item_libraries (content_id,media_folder_id) VALUES ($1,$2)`, contentID, libraryID)

	var fileID int
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO media_files (content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES ($1,$2,$3,1000,'virtual',7) RETURNING id`,
		contentID, libraryID, fmt.Sprintf("virtual://movie/tt%d?result=dead", time.Now().UnixNano())).Scan(&fileID); err != nil {
		t.Fatal(err)
	}

	itemsRepo := catalogpkg.NewItemRepository(pool)
	episodeRepo := catalogpkg.NewEpisodeRepository(pool)
	seasonRepo := catalogpkg.NewSeasonRepository(pool)
	files := scanner.NewFileRepository(pool)
	detail := catalogpkg.NewDetailService(itemsRepo, episodeRepo, seasonRepo, catalogpkg.NewPersonRepository(pool), files)
	items := handlers.NewItemsHandler(catalogpkg.NewBrowseRepository(pool), itemsRepo, episodeRepo, seasonRepo, nil, files, nil, detail, catalogpkg.NewProviderIDRepository(pool))

	svc := handlers.NewCatalogResourceHandler(items)
	svc.FileResolver = files
	svc.VirtualResolver = handlers.VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (handlers.ResolvedVirtualMedia, error) {
		return handlers.ResolvedVirtualMedia{}, errors.New("virtual stream provider returned no matching candidate")
	})
	svc.MarkVirtualFailed = files.MarkVirtualCandidateFailed
	svc.ClearVirtualFailed = files.ClearVirtualCandidateFailed
	svc.ItemAccess = itemsRepo
	svc.EpisodeLookup = episodeRepo

	deps, _ := catalogDeps(t)
	deps.CatalogAccess = items
	deps.CatalogItems = svc
	deps.CatalogVersionsCheck = svc
	h := newTestHandler(t, deps)

	checked := do(t, h, http.MethodPost, Prefix+"/catalog/versions/check",
		fmt.Sprintf(`{"file_ids":["%d"]}`, fileID), viewerHeaders())
	if checked.Code != http.StatusOK {
		t.Fatalf("check status = %d: %s", checked.Code, checked.Body.String())
	}
	var checkBody struct {
		Results []struct {
			FileID    string `json:"file_id"`
			Available bool   `json:"available"`
			Failed    bool   `json:"failed"`
		} `json:"results"`
	}
	decodeJSON(t, checked.Body, &checkBody)
	if len(checkBody.Results) != 1 || !checkBody.Results[0].Failed || checkBody.Results[0].Available {
		t.Fatalf("check results = %+v", checkBody.Results)
	}

	listed := do(t, h, http.MethodGet, Prefix+"/catalog/items/"+contentID+"/versions", "", viewerHeaders())
	if listed.Code != http.StatusOK {
		t.Fatalf("versions status = %d: %s", listed.Code, listed.Body.String())
	}
	var versions struct {
		Items []struct {
			FileID    string `json:"file_id"`
			Available *bool  `json:"available"`
			Failed    bool   `json:"failed"`
		} `json:"items"`
	}
	decodeJSON(t, listed.Body, &versions)
	if len(versions.Items) != 1 {
		t.Fatalf("versions = %+v", versions.Items)
	}
	row := versions.Items[0]
	if row.FileID != strconv.Itoa(fileID) || !row.Failed {
		t.Fatalf("versions row did not carry the check's failed stamp: %+v", row)
	}
	if row.Available == nil || *row.Available {
		t.Fatalf("versions row available = %v, want false", row.Available)
	}
}
