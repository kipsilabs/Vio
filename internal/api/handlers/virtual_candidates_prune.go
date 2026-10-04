package handlers

import (
	"context"
	"log/slog"
	"strings"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// VirtualCandidateRows resolves an item's persisted media files by content or
// episode id. It is the same read the refresh service uses to find source rows.
type VirtualCandidateRows func(ctx context.Context, id string) ([]*models.MediaFile, error)

// VirtualCandidateDeleteStore deletes the named persisted virtual candidate
// rows, re-applying the sweep's retention predicate in SQL. Implemented by
// *scanner.FileRepository.
type VirtualCandidateDeleteStore interface {
	PruneDeadAbsentVirtualCandidates(ctx context.Context, ids []int) (int, error)
}

// CandidateDeadPruner removes persisted provider-candidate rows that the fresh
// listing no longer offers and that are dead, while leaving every row the
// re-list sweep's retention keeps untouched. It is the executor's cleanup step:
// the sweep inside ReplaceVirtualCandidates only considers rows from the source
// groups it just re-listed, so a provider-candidate row whose source group is no
// longer resolvable at all (or a dead row the provider dropped) can linger
// forever. This pass gives dead, absent rows an explicit cleanup path that still
// honors the same keep-list.
//
// The row decision is deliberately conservative: a row is deleted only when it
// is a virtual selection row inside the just-listed scope, provably absent from
// the fresh listing (neither its result id nor its durable release identity
// appears), and dead (a failed_at verdict or an AltMount failed verdict). The
// actual delete re-applies last-played, delivered, in-window, live-attempt and
// open-ABS-session protection in SQL, so "when in doubt, keep" holds even if
// this decision logic is wrong.
type CandidateDeadPruner struct {
	ContentFiles VirtualCandidateRows
	EpisodeFiles VirtualCandidateRows
	// Delete performs the actual row deletion with the retention re-applied in
	// SQL. Nil disables the prune.
	Delete VirtualCandidateDeleteStore
	// ProviderFailed reports whether AltMount's authoritative snapshot records a
	// release as failed. Nil means "unknown": an empty name or an unconfigured
	// provider never makes a row dead on its own, though a failed_at stamp
	// still does.
	ProviderFailed func(releaseName string) (failed bool, known bool)
	Logger         *slog.Logger
}

// PruneDeadAbsentCandidates deletes the persisted candidate rows for contentID
// (and episodeID when set) that are dead and absent from the fresh listing,
// honoring the sweep's retention. It returns how many rows were actually
// deleted.
func (p *CandidateDeadPruner) PruneDeadAbsentCandidates(ctx context.Context, contentID, episodeID string, sources []*models.MediaFile, fresh []VirtualPlaybackStream) (int, error) {
	if p == nil || p.Delete == nil {
		return 0, nil
	}
	scopes := candidatePruneScopes(sources, fresh)
	if len(scopes) == 0 {
		// Nothing was listed, so "absent" cannot be established. Never prune on
		// an empty scope set.
		return 0, nil
	}
	freshCandidates := freshStreamCandidates(fresh)

	rows, err := p.rowsForContent(ctx, contentID, episodeID)
	if err != nil {
		return 0, err
	}
	dead := make([]int, 0)
	for _, row := range rows {
		if row == nil || row.ID <= 0 {
			continue
		}
		if !candidateSelectionRow(row) {
			continue
		}
		if !candidateRowInScope(row, scopes) {
			continue
		}
		if candidateRowPresentInFresh(row, fresh, freshCandidates) {
			continue
		}
		if !p.candidateRowDead(row) {
			continue
		}
		dead = append(dead, row.ID)
	}
	if len(dead) == 0 {
		return 0, nil
	}
	pruned, err := p.Delete.PruneDeadAbsentVirtualCandidates(ctx, dead)
	if err != nil {
		return 0, err
	}
	p.logger().InfoContext(ctx, "virtual candidates refresh: pruned dead absent candidates",
		"component", "api", contentIDKey, contentID, episodeIDKey, episodeID,
		"candidates", len(dead), "pruned", pruned)
	return pruned, nil
}

// rowsForContent returns the persisted candidate rows for the item, from the
// content lookup and (for an episode) the episode lookup, deduplicated by id.
func (p *CandidateDeadPruner) rowsForContent(ctx context.Context, contentID, episodeID string) ([]*models.MediaFile, error) {
	var rows []*models.MediaFile
	seen := make(map[int]struct{})
	appendRows := func(batch []*models.MediaFile, err error) error {
		if err != nil {
			return err
		}
		for _, row := range batch {
			if row == nil || row.ID <= 0 {
				continue
			}
			if _, ok := seen[row.ID]; ok {
				continue
			}
			seen[row.ID] = struct{}{}
			rows = append(rows, row)
		}
		return nil
	}
	if p.ContentFiles != nil && strings.TrimSpace(contentID) != "" {
		batch, err := p.ContentFiles(ctx, contentID)
		if err := appendRows(batch, err); err != nil {
			return nil, err
		}
	}
	if p.EpisodeFiles != nil && strings.TrimSpace(episodeID) != "" {
		batch, err := p.EpisodeFiles(ctx, episodeID)
		if err := appendRows(batch, err); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// candidateRowDead reports whether a persisted candidate is dead. A failed_at
// stamp is the persisted transport/decode verdict; an AltMount failed verdict
// for the row's release is the provider's own verdict on the same release. An
// unknown provider verdict (empty name, unconfigured AltMount) is never treated
// as dead.
func (p *CandidateDeadPruner) candidateRowDead(row *models.MediaFile) bool {
	if row.FailedAt != nil {
		return true
	}
	if p.ProviderFailed == nil {
		return false
	}
	name := strings.TrimSpace(row.ProviderReleaseName)
	if name == "" {
		return false
	}
	failed, known := p.ProviderFailed(name)
	return known && failed
}

func (p *CandidateDeadPruner) logger() *slog.Logger {
	if p == nil || p.Logger == nil {
		return slog.Default()
	}
	return p.Logger
}

// candidatePruneScopes returns the (media folder, owner installation) pairs the
// fresh listing covers, so the prune never touches a row outside the source
// groups that were actually re-listed. A row with no matching scope is left
// alone even if it looks dead and absent. At least one scope is required before
// any prune runs, so an unresolved listing can never authorize deletion.
func candidatePruneScopes(sources []*models.MediaFile, fresh []VirtualPlaybackStream) map[candidatePruneScope]struct{} {
	scopes := make(map[candidatePruneScope]struct{}, len(sources))
	owners := make(map[int]struct{})
	folders := make(map[int]struct{})
	for _, source := range sources {
		if source == nil {
			continue
		}
		scopes[candidatePruneScope{folderID: source.MediaFolderID, ownerID: source.VirtualOwnerInstallationID}] = struct{}{}
		owners[source.VirtualOwnerInstallationID] = struct{}{}
		folders[source.MediaFolderID] = struct{}{}
	}
	// The fresh candidates carry the owner they were persisted under. Fold any
	// additional owner seen in the listing into every listed folder so a source
	// row whose owner was resolved by the sink still matches its candidates.
	for _, stream := range fresh {
		if stream.OwnerInstallationID <= 0 {
			continue
		}
		if _, ok := owners[stream.OwnerInstallationID]; ok {
			continue
		}
		for folderID := range folders {
			scopes[candidatePruneScope{folderID: folderID, ownerID: stream.OwnerInstallationID}] = struct{}{}
		}
	}
	return scopes
}

// candidatePruneScope is one library/owner pair a fresh listing covers.
type candidatePruneScope struct {
	folderID int
	ownerID  int
}

func candidateRowInScope(row *models.MediaFile, scopes map[candidatePruneScope]struct{}) bool {
	_, ok := scopes[candidatePruneScope{folderID: row.MediaFolderID, ownerID: row.VirtualOwnerInstallationID}]
	return ok
}

// candidateSelectionRow reports whether a persisted row is a selectable
// provider candidate (a concrete `result=` pick) rather than the
// provider-neutral source row. Only selection rows are prune candidates.
func candidateSelectionRow(row *models.MediaFile) bool {
	if row == nil || !isVirtualPlaybackFile(row) {
		return false
	}
	return virtualResultCandidateID(row.FilePath) != ""
}

// freshStreamCandidates reconstructs the resolver candidate shape from the
// listed streams so the durable-identity re-match runs through the same tier
// rules the rest of the virtual layer uses. ProviderReleaseName is already the
// normalized release identity the listing sink stores, so it is carried as the
// candidate name for the name tier.
func freshStreamCandidates(fresh []VirtualPlaybackStream) []resolver.StreamCandidate {
	out := make([]resolver.StreamCandidate, 0, len(fresh))
	for _, stream := range fresh {
		candidate := resolver.StreamCandidate{
			Name:       stream.ProviderReleaseName,
			Title:      stream.ProviderReleaseName,
			FileSize:   stream.FileSize,
			SourceGUID: stream.ProviderGUID,
		}
		candidate.BehaviorHints.VideoHash = stream.ProviderVideoHash
		out = append(out, candidate)
	}
	return out
}

// candidateRowPresentInFresh reports whether the persisted row still appears in
// the fresh listing, by exact result id or by durable release identity. A
// renumbered listing of the same release therefore counts as present and is
// never pruned.
func candidateRowPresentInFresh(row *models.MediaFile, fresh []VirtualPlaybackStream, freshCandidates []resolver.StreamCandidate) bool {
	rowID := virtualResultCandidateID(row.FilePath)
	if rowID != "" {
		for i := range fresh {
			candidateID := virtualResultCandidateID(fresh[i].URI)
			if candidateID == "" {
				candidateID = fresh[i].ID
			}
			if candidateID != "" && candidateID == rowID {
				return true
			}
		}
	}
	if _, matched, _ := resolver.MatchCandidateByPersistedIdentityReport(
		freshCandidates, row.ProviderVideoHash, row.ProviderGUID, row.ProviderReleaseName, row.ProviderReleaseSize,
	); matched {
		return true
	}
	return false
}
