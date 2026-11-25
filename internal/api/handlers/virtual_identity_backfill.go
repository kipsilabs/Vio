package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/Silo-Server/silo-server/internal/adminjob"
	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/scanner"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// virtualIdentityBackfillBatch is the number of legacy rows one page scans.
// The page is also the unit of cursor advance, so a crash redoes at most one
// page and a provider is re-listed at most once per candidate group per page.
const virtualIdentityBackfillBatch = 200

// VirtualIdentityBackfillLister lists a virtual source group's provider
// candidates. It is the core virtual library's ListStreams, which uses the
// bounded provider cache so the backfill never storms a provider.
type VirtualIdentityBackfillLister func(ctx context.Context, virtualPath string) ([]virtuallibrary.PlaybackStream, error)

// VirtualIdentityBackfillStore is the catalog slice the backfill needs:
// select the legacy rows, count them, and fill a confidently matched identity.
// It is implemented by *scanner.FileRepository.
type VirtualIdentityBackfillStore interface {
	ListVirtualIdentityBackfillRows(ctx context.Context, afterID, limit int) ([]*models.MediaFile, error)
	CountVirtualIdentityBackfillRows(ctx context.Context) (int, error)
	FillVirtualProviderIdentity(ctx context.Context, fileID int, expectedFilePath string, identity scanner.VirtualProviderIdentity) (bool, error)
}

// VirtualIdentityBackfillJobs is the admin-job slice the bulk gate needs.
// It is a narrow interface so the gate is unit-testable without a database;
// *adminjob.Repository implements it.
type VirtualIdentityBackfillJobs interface {
	HasFinishedJobOfType(ctx context.Context, jobType string) (bool, error)
	RequeueLatestFailedOfType(ctx context.Context, jobType string) (bool, error)
	LatestJobRequestedAt(ctx context.Context, jobType string) (time.Time, bool, error)
	Create(ctx context.Context, input adminjob.CreateJobInput) (*models.AdminJob, error)
}

// VirtualIdentityBackfillExecutor implements the one-shot resumable backfill
// of durable provider identity onto legacy virtual rows.
//
// It walks the legacy set in id order, groups each page by candidate source,
// re-lists each group once, and writes the fresh candidate's identity onto a
// row only when the existing dedup-key tiers prove the same release. A row
// that already has identity is never selected, so the pass is idempotent and a
// resumed page redoes no work that already landed.
type VirtualIdentityBackfillExecutor struct {
	// Lister re-lists one source group's provider candidates. Nil disables the
	// backfill (Execute fails with a clear message).
	Lister VirtualIdentityBackfillLister
	// Store reads the legacy rows and writes the matched identity.
	Store VirtualIdentityBackfillStore
	// Jobs queues the bulk job. It is only needed by EnsureBulkBackfillJob;
	// Execute does not use it.
	Jobs   VirtualIdentityBackfillJobs
	Logger *slog.Logger

	batchSize int
	nowFunc   func() time.Time
}

// virtualIdentityBulkBackfillCooldown is the minimum gap between two bulk
// enqueues. The finished-job gate already stops a completed or canceled pass
// from running again; the cooldown additionally stops a failing pass from
// being revived on every restart, which is how one deployment accumulated
// eighteen concurrent sweeps. It is visible in the job's own history: the
// operator sees the previous attempt's requested_at.
const virtualIdentityBulkBackfillCooldown = 24 * time.Hour

// Execute runs the backfill page loop, resuming from resume.Cursor. A provider
// that cannot be listed for one candidate group is counted and skipped. The
// pass stops at that page with the cursor left on the last fully-processed page
// boundary, so the runner fails the job and a later run retries the page
// instead of advancing past rows that were never considered. It returns an
// error only for a catalog read/write failure that a retry might fix.
func (e *VirtualIdentityBackfillExecutor) Execute(
	ctx context.Context,
	_ adminjob.VirtualIdentityBackfillRequest,
	resume adminjob.VirtualIdentityBackfillResume,
	progress func(current, total, cursor int, message string),
) (*adminjob.VirtualIdentityBackfillResult, error) {
	if e == nil || e.Lister == nil || e.Store == nil {
		return nil, errors.New("virtual identity backfill executor is not configured")
	}
	result := &adminjob.VirtualIdentityBackfillResult{Cursor: resume.Cursor, Tiers: map[string]int{}}

	total, err := e.Store.CountVirtualIdentityBackfillRows(ctx)
	if err != nil {
		return nil, fmt.Errorf("count legacy virtual rows: %w", err)
	}
	result.RowsTotal = total
	report := func(message string) {
		if progress != nil {
			progress(result.RowsScanned, total, result.Cursor, message)
		}
	}
	report("Backfilling virtual provider identity")

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows, err := e.Store.ListVirtualIdentityBackfillRows(ctx, result.Cursor, e.batch())
		if err != nil {
			return nil, fmt.Errorf("list legacy virtual rows: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		result.RowsScanned += len(rows)
		failed, err := e.processPage(ctx, rows, result)
		if err != nil {
			return nil, err
		}
		report(fmt.Sprintf("Backfilled %d of %d legacy virtual rows", result.RowsScanned, total))
		if failed {
			// A provider group in this page could not be re-listed, so the
			// page is not fully processed and its unmatched rows must stay
			// retriable. Stop with the cursor on the page boundary so the next
			// claim resumes after the last fully-processed page.
			break
		}
		if len(rows) < e.batch() {
			break
		}
	}
	return result, nil
}

// processPage handles one id-ordered page: it groups the rows by candidate
// source, re-lists each group once, matches each row, and advances the cursor
// to the page's highest id. The cursor only advances after every row in the
// page has been considered, so a crash mid-page resumes before it and no row
// is silently skipped.
//
// A provider group that cannot be re-listed leaves the page not fully
// processed: the cursor is restored to the page boundary and failed is true so
// the caller stops and a later run retries the page instead of skipping the
// group's rows.
func (e *VirtualIdentityBackfillExecutor) processPage(ctx context.Context, rows []*models.MediaFile, result *adminjob.VirtualIdentityBackfillResult) (bool, error) {
	pageCursor := result.Cursor
	type group struct {
		path string
		rows []*models.MediaFile
	}
	groupsByKey := make(map[string]*group)
	order := make([]string, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		path, ok := virtualCandidateGroupURI(row.FilePath)
		if !ok {
			// Not a virtual candidate group: nothing to re-list, but the row
			// still counts as scanned and advances the cursor.
			result.RowsUnmatched++
			if row.ID > result.Cursor {
				result.Cursor = row.ID
			}
			continue
		}
		key := fmt.Sprintf("%s\x00%s\x00%d\x00%d", path, row.EpisodeID, row.MediaFolderID, row.VirtualOwnerInstallationID)
		existing, ok := groupsByKey[key]
		if !ok {
			existing = &group{path: path}
			groupsByKey[key] = existing
			order = append(order, key)
		}
		existing.rows = append(existing.rows, row)
	}
	// Every row in the page advances the cursor, whether or not it could be
	// grouped, so a crash after this point never re-lists work already done. A
	// provider failure below restores the cursor to the page boundary.
	for _, row := range rows {
		if row != nil && row.ID > result.Cursor {
			result.Cursor = row.ID
		}
	}
	sort.Strings(order)

	failed := false
	for _, key := range order {
		group := groupsByKey[key]
		result.GroupsScanned++
		streams, err := e.Lister(ctx, group.path)
		if err != nil {
			// A provider outage is not fatal to the pass, but the page is left
			// incomplete. Count the group's rows unmatched, restore the cursor
			// to the page boundary, and report the page so the caller fails the
			// job and a later run retries instead of orphaning these rows.
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			result.GroupsFailed++
			result.RowsUnmatched += len(group.rows)
			failed = true
			e.logger().WarnContext(ctx, "virtual identity backfill: provider re-list failed",
				"component", "api", "virtual_path", group.path, "rows", len(group.rows), "error", err)
			continue
		}
		for _, row := range group.rows {
			candidate, tier, ok := matchBackfillCandidate(row, streams)
			if !ok {
				result.RowsUnmatched++
				continue
			}
			wrote, err := e.Store.FillVirtualProviderIdentity(ctx, row.ID, row.FilePath, virtualProviderIdentityOf(candidate))
			if err != nil {
				return false, fmt.Errorf("fill virtual provider identity: %w", err)
			}
			if !wrote {
				// The row gained identity between the page read and this write
				// (an opportunistic adoption), or the candidate carried no
				// usable tier. Neither is an error and neither overwrites.
				result.RowsSkipped++
				continue
			}
			result.RowsMatched++
			result.Tiers[tier]++
		}
	}
	if failed {
		result.Cursor = pageCursor
	}
	return failed, nil
}

// matchBackfillCandidate returns the fresh candidate a legacy row can be
// confidently bound to, and the tier that proved it.
//
// The row's listing id is checked first: if the provider still lists the exact
// result the row was persisted from, the bytes are the same listing entry and
// the candidate's identity can be adopted outright. Otherwise the durable tiers
// are compared with MatchCandidateIdentityForBackfill, which requires size
// corroboration on the name tier so a bare release-name coincidence never
// invents identity.
func matchBackfillCandidate(row *models.MediaFile, streams []virtuallibrary.PlaybackStream) (virtuallibrary.PlaybackStream, string, bool) {
	if row == nil {
		return virtuallibrary.PlaybackStream{}, "", false
	}
	if resultID := virtualResultCandidateID(row.FilePath); resultID != "" {
		for _, stream := range streams {
			if stream.ID == resultID {
				return stream, "listing", true
			}
		}
	}
	persisted := resolver.NewPersistedIdentityTiers(
		row.ProviderVideoHash, row.ProviderGUID, row.ProviderReleaseName, row.ProviderReleaseSize,
	)
	for _, stream := range streams {
		candidate := resolver.NewPersistedIdentityTiers(
			stream.ProviderVideoHash, stream.ProviderGUID, stream.ProviderReleaseName, stream.FileSize,
		)
		if tier, ok := resolver.MatchCandidateIdentityForBackfill(persisted, candidate); ok {
			return stream, tier, true
		}
	}
	return virtuallibrary.PlaybackStream{}, "", false
}

// EnsureBulkBackfillJob queues the one-shot bulk sweep. It is only called when
// the operator explicitly selected virtual_library.identity_repair_mode=bulk;
// the default mode never reaches here.
//
// Every gate must pass before a job is created or revived:
//
//   - a completed (or explicitly canceled) job is the one-shot marker: rows that
//     could not be matched against a reachable provider stay legacy on purpose,
//     and re-listing every provider again would be a permanent tax;
//   - legacy rows must still exist, so an up-to-date catalog queues nothing;
//   - the newest job of this type must be at least
//     virtualIdentityBulkBackfillCooldown old, so a pass that keeps failing is
//     retried once a day instead of on every restart;
//   - an already-active job is a no-op (the insert's unique active-job index
//     rejects a duplicate and the conflict is swallowed), so a restart during a
//     run never queues a second one.
//
// A partially failed run is not a finished job: its durable watermark makes the
// remaining rows retriable, so the newest failed job is requeued instead of
// starting a fresh pass from zero. A server with no admin account cannot own a
// job and is skipped.
func (e *VirtualIdentityBackfillExecutor) EnsureBulkBackfillJob(ctx context.Context, ownerUserID int) error {
	if e == nil || e.Store == nil || e.Jobs == nil || ownerUserID <= 0 {
		return nil
	}
	done, err := e.Jobs.HasFinishedJobOfType(ctx, adminjob.JobTypeVirtualIdentityBackfill)
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	rows, err := e.Store.ListVirtualIdentityBackfillRows(ctx, 0, 1)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	latest, ok, err := e.Jobs.LatestJobRequestedAt(ctx, adminjob.JobTypeVirtualIdentityBackfill)
	if err != nil {
		return err
	}
	if ok && e.now().Sub(latest) < virtualIdentityBulkBackfillCooldown {
		e.logger().InfoContext(ctx, "virtual identity backfill: bulk run cooling down",
			"component", "api", "last_requested_at", latest, "cooldown", virtualIdentityBulkBackfillCooldown)
		return nil
	}
	requeued, err := e.Jobs.RequeueLatestFailedOfType(ctx, adminjob.JobTypeVirtualIdentityBackfill)
	if err != nil {
		return err
	}
	if requeued {
		return nil
	}
	_, err = e.Jobs.Create(ctx, adminjob.CreateJobInput{
		JobType:         adminjob.JobTypeVirtualIdentityBackfill,
		CreatedByUserID: ownerUserID,
		Message:         "Queued virtual identity backfill",
	})
	if err != nil {
		var conflict *adminjob.ActiveJobConflictError
		if errors.As(err, &conflict) {
			return nil
		}
		return err
	}
	return nil
}

// QueueBulkIdentityBackfillAtBoot is the whole boot-time decision for the bulk
// sweep, so the default-off rule lives in one testable place instead of an
// `if` buried in the server startup path. The default mode is off and
// on_demand never runs here: browsing cannot reproduce the bulk listing load.
func QueueBulkIdentityBackfillAtBoot(ctx context.Context, mode string, executor *VirtualIdentityBackfillExecutor, ownerUserID int, logger *slog.Logger) {
	if executor == nil || config.VirtualIdentityRepairMode(mode) != config.VirtualIdentityRepairBulk {
		return
	}
	if err := executor.EnsureBulkBackfillJob(ctx, ownerUserID); err != nil {
		if logger == nil {
			logger = slog.Default()
		}
		logger.WarnContext(ctx, "virtual identity backfill: queue failed", "component", "api", "error", err)
	}
}

func (e *VirtualIdentityBackfillExecutor) batch() int {
	if e != nil && e.batchSize > 0 {
		return e.batchSize
	}
	return virtualIdentityBackfillBatch
}

// now is the clock the bulk cooldown compares against; tests pin it.
func (e *VirtualIdentityBackfillExecutor) now() time.Time {
	if e != nil && e.nowFunc != nil {
		return e.nowFunc()
	}
	return time.Now()
}

func (e *VirtualIdentityBackfillExecutor) logger() *slog.Logger {
	if e == nil || e.Logger == nil {
		return slog.Default()
	}
	return e.Logger
}
