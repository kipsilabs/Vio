package apiv2

import (
	"context"
	"fmt"
	"strconv"
)

const opCheckCatalogVersions = "checkCatalogVersions"

// CatalogVersionsCheckInput is the checkCatalogVersions request: the file
// versions whose liveness to probe. The v1 route takes plain integers; the
// v2 contract uses the same opaque string identifiers every other v2
// operation uses.
type CatalogVersionsCheckInput struct {
	Body struct {
		FileIDs []ID `json:"file_ids" minItems:"1" maxItems:"40" doc:"File identifiers from listCatalogItemVersions, at most 40 per request" example:"[\"120\",\"121\"]"`
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
}

// CatalogVersionsCheck is the checkCatalogVersions response body.
type CatalogVersionsCheck struct {
	Results []CatalogVersionCheckResult `json:"results" doc:"One result per requested file identifier; empty, never null"`
}

// CatalogVersionsCheckOutput is the checkCatalogVersions response.
type CatalogVersionsCheckOutput struct {
	Body CatalogVersionsCheck
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
	results := svc.CheckVersions(ctx, fileIDs)
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
		out = append(out, item)
	}
	return &CatalogVersionsCheckOutput{Body: CatalogVersionsCheck{Results: out}}, nil
}
