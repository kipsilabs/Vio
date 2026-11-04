package apiv2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
	catalogpkg "github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

// fakeVersionCheck records the IDs the v2 adapter forwarded to the shared
// liveness seam and answers a fixed verdict list.
type fakeVersionCheck struct {
	results []handlers.VersionCheckResult
	ids     []int
}

func (f *fakeVersionCheck) CheckVersions(_ context.Context, fileIDs []int) []handlers.VersionCheckResult {
	f.ids = append([]int(nil), fileIDs...)
	return f.results
}

// TestCheckCatalogVersionsWiresTheSharedSeam: the v2 operation converts the
// opaque string identifiers, forwards every ID in order, and projects the
// seam's durable verdicts, including the advisory source-mismatch block.
func TestCheckCatalogVersionsWiresTheSharedSeam(t *testing.T) {
	deps, _ := catalogDeps(t)
	seam := &fakeVersionCheck{results: []handlers.VersionCheckResult{
		{FileID: 42, Available: true},
		{FileID: 43, Available: false, Failed: true},
		{FileID: 44, Available: true, SourceMismatch: &handlers.VersionCheckSourceMismatch{ServedBytes: 7_427_636_945, DeclaredBytes: 8_449_999_999}},
	}}
	deps.CatalogVersionsCheck = seam
	h := newTestHandler(t, deps)

	rec := do(t, h, http.MethodPost, Prefix+"/catalog/versions/check", `{"file_ids":["42","43","44"]}`, viewerHeaders())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if want := []int{42, 43, 44}; fmt.Sprint(seam.ids) != fmt.Sprint(want) {
		t.Fatalf("seam ids = %v, want %v", seam.ids, want)
	}
	var body struct {
		Results []struct {
			FileID         string `json:"file_id"`
			Available      bool   `json:"available"`
			Failed         bool   `json:"failed"`
			SourceMismatch *struct {
				ServedBytes   int64 `json:"served_bytes"`
				DeclaredBytes int64 `json:"declared_bytes"`
			} `json:"source_mismatch"`
		} `json:"results"`
	}
	decodeJSON(t, rec.Body, &body)
	if len(body.Results) != 3 {
		t.Fatalf("results = %+v", body.Results)
	}
	if body.Results[0].FileID != "42" || !body.Results[0].Available || body.Results[0].Failed || body.Results[0].SourceMismatch != nil {
		t.Fatalf("live result = %+v", body.Results[0])
	}
	if body.Results[1].FileID != "43" || body.Results[1].Available || !body.Results[1].Failed {
		t.Fatalf("failed result = %+v", body.Results[1])
	}
	if body.Results[2].SourceMismatch == nil || body.Results[2].SourceMismatch.ServedBytes != 7_427_636_945 || body.Results[2].SourceMismatch.DeclaredBytes != 8_449_999_999 {
		t.Fatalf("source mismatch = %+v", body.Results[2].SourceMismatch)
	}
}

// fakeInventoryVersionCheck adds the single-file inventory mode to the liveness
// fake and records which method the adapter chose.
type fakeInventoryVersionCheck struct {
	fakeVersionCheck
	inventoryIDs   []int
	inventoryCalls int
}

func (f *fakeInventoryVersionCheck) CheckVersionsWithInventory(_ context.Context, fileIDs []int) []handlers.VersionCheckResult {
	f.inventoryCalls++
	f.inventoryIDs = append([]int(nil), fileIDs...)
	return f.results
}

// TestCheckCatalogVersionsInventoryMode pins the additive mode: preprobe reaches
// the inventory seam, the flat wire fields carry what it reported, and the
// batched path never touches it.
func TestCheckCatalogVersionsInventoryMode(t *testing.T) {
	deps, _ := catalogDeps(t)
	probedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	seam := &fakeInventoryVersionCheck{fakeVersionCheck: fakeVersionCheck{results: []handlers.VersionCheckResult{{
		FileID:    42,
		Available: true,
		Inventory: &handlers.VersionCheckInventory{
			Provenance:     handlers.VersionInventoryVerified,
			ProbedAt:       &probedAt,
			VideoTracks:    []models.VideoTrack{{Codec: "hevc", Width: 3840, Height: 2160}},
			AudioTracks:    []models.AudioTrack{{Index: 1, Codec: "truehd", Language: "eng", Channels: 8}},
			SubtitleTracks: []catalogpkg.VersionSubtitleTrack{{Index: 2, Language: "eng", Codec: "subrip"}},
		},
	}}}}
	deps.CatalogVersionsCheck = seam
	h := newTestHandler(t, deps)

	rec := do(t, h, http.MethodPost, Prefix+"/catalog/versions/check", `{"file_ids":["42"],"preprobe":true}`, viewerHeaders())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if seam.inventoryCalls != 1 || fmt.Sprint(seam.inventoryIDs) != "[42]" {
		t.Fatalf("inventory seam calls = %d ids %v, want the single requested id", seam.inventoryCalls, seam.inventoryIDs)
	}
	var body struct {
		Results []struct {
			FileID              string `json:"file_id"`
			InventoryProvenance string `json:"inventory_provenance"`
			ProbedAt            string `json:"probed_at"`
			VideoTracks         []struct {
				Codec string `json:"codec"`
			} `json:"video_tracks"`
			AudioTracks []struct {
				Codec string `json:"codec"`
			} `json:"audio_tracks"`
			SubtitleTracks []struct {
				Codec string `json:"codec"`
			} `json:"subtitle_tracks"`
		} `json:"results"`
	}
	decodeJSON(t, rec.Body, &body)
	if len(body.Results) != 1 {
		t.Fatalf("results = %+v", body.Results)
	}
	result := body.Results[0]
	if result.FileID != "42" || result.InventoryProvenance != handlers.VersionInventoryVerified {
		t.Fatalf("inventory result = %+v", result)
	}
	if result.ProbedAt != "2026-01-02T03:04:05.000Z" {
		t.Fatalf("probed_at = %q", result.ProbedAt)
	}
	if len(result.VideoTracks) != 1 || result.VideoTracks[0].Codec != "hevc" ||
		len(result.AudioTracks) != 1 || result.AudioTracks[0].Codec != "truehd" ||
		len(result.SubtitleTracks) != 1 || result.SubtitleTracks[0].Codec != "subrip" {
		t.Fatalf("inventory tracks = %+v", result)
	}
}

// TestCheckCatalogVersionsInventoryModeRejectsABatch: the mode's wait is per
// file, so more than one identifier is a validation failure before any probe.
func TestCheckCatalogVersionsInventoryModeRejectsABatch(t *testing.T) {
	deps, _ := catalogDeps(t)
	seam := &fakeInventoryVersionCheck{}
	deps.CatalogVersionsCheck = seam
	h := newTestHandler(t, deps)

	rec := do(t, h, http.MethodPost, Prefix+"/catalog/versions/check", `{"file_ids":["42","43"],"preprobe":true}`, viewerHeaders())
	requireProblem(t, rec, TypeValidationFailed)
	if seam.inventoryCalls != 0 || seam.ids != nil {
		t.Fatalf("batch reached the seam: inventory=%d ids=%v", seam.inventoryCalls, seam.ids)
	}
}

// TestCheckCatalogVersionsInventoryModeUnwired: a seam without the inventory
// method answers dependency_unavailable instead of answering without inventory.
func TestCheckCatalogVersionsInventoryModeUnwired(t *testing.T) {
	deps, _ := catalogDeps(t)
	deps.CatalogVersionsCheck = &fakeVersionCheck{}
	h := newTestHandler(t, deps)

	rec := do(t, h, http.MethodPost, Prefix+"/catalog/versions/check", `{"file_ids":["42"],"preprobe":true}`, viewerHeaders())
	requireProblem(t, rec, TypeDependencyUnavailable)
}

// TestCheckCatalogVersionsBatchedPathSkipsInventory: without preprobe the
// adapter keeps the liveness call and never reaches the inventory seam.
func TestCheckCatalogVersionsBatchedPathSkipsInventory(t *testing.T) {
	deps, _ := catalogDeps(t)
	seam := &fakeInventoryVersionCheck{fakeVersionCheck: fakeVersionCheck{results: []handlers.VersionCheckResult{{FileID: 42, Available: true}}}}
	deps.CatalogVersionsCheck = seam
	h := newTestHandler(t, deps)

	rec := do(t, h, http.MethodPost, Prefix+"/catalog/versions/check", `{"file_ids":["42"]}`, viewerHeaders())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if seam.inventoryCalls != 0 {
		t.Fatalf("batched path reached the inventory seam %d times", seam.inventoryCalls)
	}
	if strings.Contains(rec.Body.String(), "inventory_provenance") {
		t.Fatalf("batched answer carried inventory: %s", rec.Body.String())
	}
}

// TestCheckCatalogVersionsRejectsTooLargeBatch: the v2 contract refuses more
// than 40 IDs at validation, before the seam runs any provider work.
func TestCheckCatalogVersionsRejectsTooLargeBatch(t *testing.T) {
	deps, _ := catalogDeps(t)
	seam := &fakeVersionCheck{}
	deps.CatalogVersionsCheck = seam
	h := newTestHandler(t, deps)

	ids := make([]string, 0, 41)
	for i := 1; i <= 41; i++ {
		ids = append(ids, fmt.Sprintf(`"%d"`, i))
	}
	body := `{"file_ids":[` + strings.Join(ids, ",") + `]}`
	rec := do(t, h, http.MethodPost, Prefix+"/catalog/versions/check", body, viewerHeaders())
	requireProblem(t, rec, TypeValidationFailed)
	if seam.ids != nil {
		t.Fatalf("too-large batch reached the seam: %v", seam.ids)
	}
}

// TestCheckCatalogVersionsRequiresVerifiedProfile: the operation inherits the
// profile-scoped gate, so a PIN-locked profile without its proof is refused
// before the body is read.
func TestCheckCatalogVersionsRequiresVerifiedProfile(t *testing.T) {
	deps, _ := catalogDeps(t)
	deps.CatalogVersionsCheck = &fakeVersionCheck{}
	h := newTestHandler(t, deps)

	rec := do(t, h, http.MethodPost, Prefix+"/catalog/versions/check", `{"file_ids":["42"]}`,
		with(bearer(memberToken), "X-Profile-Id", "p-locked"))
	requireProblem(t, rec, TypeProfileVerificationRequired)
}

// TestCheckCatalogVersionsUnwired: a registry without the seam answers the
// standard dependency_unavailable problem rather than panicking.
func TestCheckCatalogVersionsUnwired(t *testing.T) {
	deps, _ := catalogDeps(t)
	deps.CatalogVersionsCheck = nil
	h := newTestHandler(t, deps)

	rec := do(t, h, http.MethodPost, Prefix+"/catalog/versions/check", `{"file_ids":["42"]}`, viewerHeaders())
	requireProblem(t, rec, TypeDependencyUnavailable)
}

// fixtureVersionCheck is the deterministic liveness seam the committed
// fixtures record.
type fixtureVersionCheck struct{}

func (fixtureVersionCheck) CheckVersions(_ context.Context, fileIDs []int) []handlers.VersionCheckResult {
	results := make([]handlers.VersionCheckResult, 0, len(fileIDs))
	for _, id := range fileIDs {
		switch id {
		case 121:
			results = append(results, handlers.VersionCheckResult{FileID: id, Available: false, Failed: true})
		case 122:
			results = append(results, handlers.VersionCheckResult{FileID: id, Available: true,
				SourceMismatch: &handlers.VersionCheckSourceMismatch{ServedBytes: 7_427_636_945, DeclaredBytes: 8_449_999_999}})
		default:
			results = append(results, handlers.VersionCheckResult{FileID: id, Available: true})
		}
	}
	return results
}

// CheckVersionsWithInventory is the deterministic single-file inventory seam
// the committed fixture records: file 124 answers enumerated tracks, so the
// fixture pins the inventory wire shape.
func (fixtureVersionCheck) CheckVersionsWithInventory(ctx context.Context, fileIDs []int) []handlers.VersionCheckResult {
	results := fixtureVersionCheck{}.CheckVersions(ctx, fileIDs)
	if len(results) != 1 {
		return results
	}
	probedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	results[0].Inventory = &handlers.VersionCheckInventory{
		Provenance:     handlers.VersionInventoryVerified,
		ProbedAt:       &probedAt,
		VideoTracks:    []models.VideoTrack{{Codec: "hevc", Width: 3840, Height: 2160}},
		AudioTracks:    []models.AudioTrack{{Index: 1, Codec: "truehd", Language: "eng", Channels: 8}},
		SubtitleTracks: []catalogpkg.VersionSubtitleTrack{{Index: 2, Language: "eng", Codec: "subrip"}},
	}
	return results
}

// catalogVersionsCheckFixtureCases documents the v2 liveness check: a mixed
// verdict list, the too-large validation boundary, and the profile gate.
func catalogVersionsCheckFixtureCases() []fixtureCase {
	tooLarge := make([]string, 0, 41)
	for i := 1; i <= 41; i++ {
		tooLarge = append(tooLarge, fmt.Sprintf(`"%d"`, i))
	}
	return []fixtureCase{
		{name: "check_catalog_versions_ok", operationID: opCheckCatalogVersions,
			scenario: "A mixed batch reports the live rows, the durable failed row, and the advisory source-mismatch verdict.",
			method:   http.MethodPost, path: Prefix + "/catalog/versions/check",
			body:    `{"file_ids":["120","121","122"]}`,
			headers: viewerHeaders(),
			status:  http.StatusOK, assertHeaders: []string{"Content-Type", "Cache-Control"}, schema: "#/components/schemas/CatalogVersionsCheck"},
		{name: "check_catalog_versions_too_large", operationID: opCheckCatalogVersions,
			scenario: "More than 40 file identifiers is a validation failure at body.file_ids before any provider work.",
			method:   http.MethodPost, path: Prefix + "/catalog/versions/check",
			body:    `{"file_ids":[` + strings.Join(tooLarge, ",") + `]}`,
			headers: viewerHeaders(),
			status:  http.StatusUnprocessableEntity, assertHeaders: []string{"Content-Type", "Cache-Control"}, schema: "#/components/schemas/Problem"},
		{name: "check_catalog_versions_inventory", operationID: opCheckCatalogVersions,
			scenario: "The single-file inventory mode answers the enumerated tracks with their provenance beside the liveness verdict.",
			method:   http.MethodPost, path: Prefix + "/catalog/versions/check",
			body:    `{"file_ids":["124"],"preprobe":true}`,
			headers: viewerHeaders(),
			status:  http.StatusOK, assertHeaders: []string{"Content-Type", "Cache-Control"}, schema: "#/components/schemas/CatalogVersionsCheck"},
		{name: "check_catalog_versions_profile_verification_required", operationID: opCheckCatalogVersions,
			scenario: "A profile-scoped check for a PIN-locked profile without X-Profile-Token is refused before the body is read.",
			method:   http.MethodPost, path: Prefix + "/catalog/versions/check",
			body:    `{"file_ids":["120"]}`,
			headers: with(bearer(memberToken), "X-Profile-Id", "p-locked"),
			status:  http.StatusForbidden, assertHeaders: []string{"Content-Type", "Cache-Control"}, schema: "#/components/schemas/Problem"},
	}
}

// TestPlaybackStartsAndReplansWithoutAVersionsCheck pins the fallback rule:
// checkCatalogVersions is discoverability, never a gate. A client that never
// calls it, and never negotiates versions_check_v1, starts and replans exactly
// as it did before the operation existed.
func TestPlaybackStartsAndReplansWithoutAVersionsCheck(t *testing.T) {
	deps, _ := catalogDeps(t)
	fake := &fakePlaybackService{}
	deps.Playback = fake
	data, err := os.ReadFile("../playback/testdata/protocol_v3/decision_response.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fake.response); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t, deps)

	// The start body is the pre-existing protocol-v3 body: no check receipt,
	// no new required member, and no versions_check_v1 client feature.
	start := playbackStartFixture(t)
	if _, sent := start["versions_check"]; sent {
		t.Fatal("the start fixture must not send a check receipt")
	}
	rec := do(t, h, http.MethodPost, Prefix+"/playback/start", playbackJSON(t, start), viewerHeaders())
	if rec.Code != http.StatusCreated {
		t.Fatalf("start without a check: %d %s", rec.Code, rec.Body.String())
	}

	replan := playbackReplanFixture(t)
	rec = do(t, h, http.MethodPost, Prefix+"/playback/11111111-1111-4111-8111-111111111111/replan", playbackJSON(t, replan), viewerHeaders())
	if rec.Code != http.StatusOK {
		t.Fatalf("replan without a check: %d %s", rec.Code, rec.Body.String())
	}
}

// fakeVersionWarm records the warm a versions read requested.
type fakeVersionWarm struct {
	versions []catalogpkg.FileVersion
	profile  string
	calls    int
}

func (f *fakeVersionWarm) PrefetchItemVersionsVirtual(_ context.Context, versions []catalogpkg.FileVersion, profileID string) {
	f.calls++
	f.versions = append([]catalogpkg.FileVersion(nil), versions...)
	f.profile = profileID
}

// TestCatalogVersionsReadWarmsVirtualListings: the v2 versions read asks the
// shared seam to warm what it surfaced, under the verified profile, without
// changing the response.
func TestCatalogVersionsReadWarmsVirtualListings(t *testing.T) {
	deps, base := catalogDeps(t)
	warm := &fakeVersionWarm{}
	deps.CatalogVersionsWarm = warm
	deps.CatalogItems = &fakePreplayCatalog{fakeCatalog: base,
		detail: catalogpkg.ItemDetail{ContentID: "movie:heat-1995", Type: "movie",
			Versions: []catalogpkg.FileVersion{{FileID: 120, FilePath: "virtual://movie/tt0113277", AddedAt: fixedTime()}}}}
	h := newTestHandler(t, deps)

	rec := do(t, h, http.MethodGet, Prefix+"/catalog/items/movie:heat-1995/versions", "", viewerHeaders())
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if warm.calls != 1 || warm.profile != "p-owner" {
		t.Fatalf("warm calls = %d profile = %q", warm.calls, warm.profile)
	}
	if len(warm.versions) != 1 || warm.versions[0].FileID != 120 {
		t.Fatalf("warmed versions = %+v", warm.versions)
	}
}
