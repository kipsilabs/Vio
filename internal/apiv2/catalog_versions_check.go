package apiv2

import (
	"context"
	"fmt"
	"strconv"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
	catalogpkg "github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

const opCheckCatalogVersions = "checkCatalogVersions"

// CatalogVersionsCheckInput is the checkCatalogVersions request: the file
// versions whose liveness to probe. The v1 route takes plain integers; the
// v2 contract uses the same opaque string identifiers every other v2
// operation uses.
type CatalogVersionsCheckInput struct {
	Body struct {
		FileIDs []ID `json:"file_ids" minItems:"1" maxItems:"40" doc:"File identifiers from listCatalogItemVersions, at most 40 per request" example:"[\"120\",\"121\"]"`
		// Preprobe asks for the one file's enumerated track inventory in the
		// same answer. It is the single-file inventory mode: the server runs
		// the same probe a version selection schedules, waits for it under a
		// short bound, and reports what it established per file. Absent keeps
		// the batched liveness answer exactly as it was.
		Preprobe bool `json:"preprobe,omitempty" doc:"Ask for the single file's track inventory, enumerated synchronously under a short bound; requires exactly one file identifier"`
	}
}

// CatalogVersionCheckResult is one file's liveness verdict. It mirrors the v1
// route's result and the version rows' durable health fields.
type CatalogVersionCheckResult struct {
	FileID ID `json:"file_id" doc:"The probed file identifier"`
	// Available is the durable health signal: false when a virtual release is
	// confirmed unservable or a local file is missing. Always present, unlike
	// the version row's omission of available=true.
	Available bool `json:"available"`
	// Failed marks the confirmed virtual-only unservable verdict. Unlike
	// available=false it means the release is dead, not merely transiently
	// unreachable, so menus remove it instead of offering a retry.
	Failed bool `json:"failed,omitempty"`
	// SourceMismatch is a recent probe's wrong-source verdict for this row.
	// It is advisory: it never changes available and never means failed.
	SourceMismatch *VersionSourceMismatch `json:"source_mismatch,omitempty"`
	// InventoryProvenance is present only when the request asked for the
	// single-file inventory mode (preprobe). It says whether the inventory
	// fields are probe evidence, an attempted probe with no committed evidence
	// yet, or the row's declared registration metadata.
	InventoryProvenance string `json:"inventory_provenance,omitempty" enum:"declared,pending,verified" doc:"How much of the returned inventory is evidence: verified (probe-enumerated and stamped), pending (a probe ran for this read without committed evidence), or declared (registration metadata only)"`
	// ProbedAt is when a probe last enumerated this row's real stream table.
	ProbedAt *Instant `json:"probed_at,omitempty" doc:"When a probe last enumerated this row's stream table; absent means declared metadata only"`
	// The inventory fields are the same projections the version rows carry, so
	// a client can apply them to the row it is rendering without reshaping.
	// They are present only in the single-file inventory mode.
	VideoTracks    []models.VideoTrack               `json:"video_tracks,omitempty" doc:"The row's video tracks; present only in the single-file inventory mode"`
	AudioTracks    []models.AudioTrack               `json:"audio_tracks,omitempty" doc:"The row's audio tracks; present only in the single-file inventory mode"`
	SubtitleTracks []catalogpkg.VersionSubtitleTrack `json:"subtitle_tracks,omitempty" doc:"The row's embedded and external subtitle tracks; present only in the single-file inventory mode"`
}

// CatalogVersionsCheck is the checkCatalogVersions response body.
type CatalogVersionsCheck struct {
	Results []CatalogVersionCheckResult `json:"results" doc:"One result per requested file identifier; empty, never null"`
}

// CatalogVersionsCheckOutput is the checkCatalogVersions response.
type CatalogVersionsCheckOutput struct {
	Body CatalogVersionsCheck
}

// catalogVersionsInventory is the optional single-file inventory mode of the
// check seam. *handlers.CatalogResourceHandler implements it; a seam that does
// not keeps the operations' batched liveness answer and refuses the mode rather
// than answering it without inventory.
type catalogVersionsInventory interface {
	CheckVersionsWithInventory(ctx context.Context, fileIDs []int) []handlers.VersionCheckResult
}

// checkCatalogVersions adapts the shared CatalogResourceHandler liveness seam
// to the v2 wire types. The operation has no required ordering against
// playback: a client that never calls it starts and replans exactly as before.
func (reg *Registry) checkCatalogVersions(ctx context.Context, in *CatalogVersionsCheckInput) (*CatalogVersionsCheckOutput, error) {
	svc := reg.deps.CatalogVersionsCheck
	if svc == nil {
		return nil, unavailable("catalog")
	}
	fileIDs := make([]int, 0, len(in.Body.FileIDs))
	for i, raw := range in.Body.FileIDs {
		n, p := raw.positive64(fmt.Sprintf("body.file_ids[%d]", i))
		if p != nil {
			return nil, p
		}
		fileIDs = append(fileIDs, int(n))
	}

	check := svc.CheckVersions
	if in.Body.Preprobe {
		// The inventory mode enumerates one file's bytes under a short bound;
		// a batch would multiply that wait, so it is a single-file mode.
		if len(fileIDs) != 1 {
			return nil, NewProblem(TypeValidationFailed, "The request did not pass validation; see errors.").
				WithErrors(ProblemError{Location: "body.file_ids", Code: codeInvalid, Detail: "preprobe requires exactly one file_id"})
		}
		inventory, ok := svc.(catalogVersionsInventory)
		if !ok {
			return nil, unavailable("catalog")
		}
		check = inventory.CheckVersionsWithInventory
	}

	results := check(ctx, fileIDs)
	out := make([]CatalogVersionCheckResult, 0, len(results))
	for _, result := range results {
		item := CatalogVersionCheckResult{
			FileID:    ID(strconv.Itoa(result.FileID)),
			Available: result.Available,
			Failed:    result.Failed,
		}
		if result.SourceMismatch != nil {
			item.SourceMismatch = &VersionSourceMismatch{
				ServedBytes:   result.SourceMismatch.ServedBytes,
				DeclaredBytes: result.SourceMismatch.DeclaredBytes,
			}
		}
		if result.Inventory != nil {
			item.InventoryProvenance = result.Inventory.Provenance
			item.ProbedAt = probeInstantPtr(result.Inventory.ProbedAt)
			item.VideoTracks = result.Inventory.VideoTracks
			item.AudioTracks = result.Inventory.AudioTracks
			item.SubtitleTracks = result.Inventory.SubtitleTracks
		}
		out = append(out, item)
	}
	return &CatalogVersionsCheckOutput{Body: CatalogVersionsCheck{Results: out}}, nil
}
