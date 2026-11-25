package handlers

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/scanner"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

const (
	// virtualIdentityRepairTimeout bounds one item's provider listing and
	// catalog write. A hung provider must not hold a repair slot forever.
	virtualIdentityRepairTimeout = 30 * time.Second
	// virtualIdentityRepairMaxGroups bounds how many candidate groups one item
	// view may list. A movie with several quality profiles has one group per
	// profile, and the detail page shows the same small set the listing
	// prefetch caps at two; more than that is not a page view's job.
	virtualIdentityRepairMaxGroups = 2
	// virtualIdentityRepairListingsPerMinute is the global budget for this
	// process. Browsing cannot reproduce the bulk sweep's listing rate even if
	// every page it opens holds a legacy row: six listings a minute covers the
	// provider cache refilling behind normal browsing and stops there.
	virtualIdentityRepairListingsPerMinute = 6
	// virtualIdentityRepairListBurst is how many listings may start back to
	// back before the per-minute budget throttles.
	virtualIdentityRepairListBurst = 2
	// virtualIdentityRepairConcurrency caps in-flight repairs. The cap is also
	// the shed boundary: an item over it is dropped, not queued, because a page
	// view must never accumulate provider work.
	virtualIdentityRepairConcurrency = 2
	// virtualIdentityRepairNegativeTTL is how long an item that listed but did
	// not match is skipped before another view may try again. It is
	// in-process only: media_files has no "identity checked" column, so a
	// restart forgets it. See the PR's follow-up for the durable column.
	virtualIdentityRepairNegativeTTL = 6 * time.Hour
	// virtualIdentityRepairNegativeSweepAt is the entry count that triggers a
	// sweep of expired negative-cache entries, so a long-running process that
	// opens many unmatched items does not grow the map forever.
	virtualIdentityRepairNegativeSweepAt = 1024

	virtualIdentityRepairMovieType   = "movie"
	virtualIdentityRepairEpisodeType = "episode"
)

// VirtualIdentityRepairStore is the catalog slice an on-demand repair needs:
// the rows of one item and the fenced identity write the bulk pass already
// uses. *scanner.FileRepository implements it.
type VirtualIdentityRepairStore interface {
	GetByContentID(ctx context.Context, contentID string) ([]*models.MediaFile, error)
	GetByEpisodeID(ctx context.Context, episodeID string) ([]*models.MediaFile, error)
	FillVirtualProviderIdentity(ctx context.Context, fileID int, expectedFilePath string, identity scanner.VirtualProviderIdentity) (bool, error)
}

// VirtualIdentityRepairer repairs durable provider identity on the legacy
// virtual rows of one item, triggered by that item's detail page.
//
// Legacy rows were persisted before the provider identity columns existed, so
// they carry no video hash or GUID and cannot re-match a release after the
// provider renumbers it. The bulk sweep repairs them all at once, which costs
// one provider listing per candidate group across the whole legacy set — the
// volume that got an operator banned from two indexers. This replaces that with
// a per-item repair that only runs when someone actually opens the item.
//
// Every run is detached from the request and guarded four ways: it is
// single-flight per item (concurrent opens coalesce), it lists nothing when the
// item already carries identity, it negative-caches an item that listed but did
// not match, and a process-wide token bucket plus concurrency cap bounds the
// listing rate no matter how fast pages are opened.
type VirtualIdentityRepairer struct {
	// Lister re-lists one candidate group's provider candidates. It is the
	// core virtual library's ListStreams, which uses the bounded provider
	// cache. Nil disables the repairer.
	Lister VirtualIdentityBackfillLister
	// Store reads the item's rows and writes the matched identity.
	Store VirtualIdentityRepairStore
	// Enabled is the boot-time answer of
	// virtual_library.identity_repair_mode == on_demand. Off keeps every view
	// free of provider traffic.
	Enabled bool
	Logger  *slog.Logger

	mu       sync.Mutex
	inflight map[string]struct{}
	negative map[string]time.Time
	slots    chan struct{}
	limiter  *rate.Limiter
	// now is the clock the negative cache compares against; tests pin it.
	now func() time.Time
	// run starts the detached work. It defaults to a goroutine, and the tests
	// replace it to make the run synchronous and deterministic.
	run func(func())
}

// NewVirtualIdentityRepairer builds a repairer with its guards initialized.
// enabled is the on_demand mode answer, not the raw setting.
func NewVirtualIdentityRepairer(lister VirtualIdentityBackfillLister, store VirtualIdentityRepairStore, enabled bool, logger *slog.Logger) *VirtualIdentityRepairer {
	return &VirtualIdentityRepairer{
		Lister:   lister,
		Store:    store,
		Enabled:  enabled,
		Logger:   logger,
		inflight: make(map[string]struct{}),
		negative: make(map[string]time.Time),
		slots:    make(chan struct{}, virtualIdentityRepairConcurrency),
		limiter: rate.NewLimiter(
			rate.Every(time.Minute/virtualIdentityRepairListingsPerMinute),
			virtualIdentityRepairListBurst,
		),
		run: func(work func()) { go work() },
	}
}

// ScheduleVirtualIdentityRepair is the detail-page trigger. It returns as soon
// as the work is admitted, so the page never waits for a provider listing, and
// it does nothing for an item that is not a movie or an episode: a series has
// no version rows of its own, and fanning out across its episodes from one
// series view would be the bulk sweep with extra steps.
func (r *VirtualIdentityRepairer) ScheduleVirtualIdentityRepair(ctx context.Context, contentID, mediaType string) {
	if r == nil || !r.Enabled || r.Lister == nil || r.Store == nil {
		return
	}
	contentID = strings.TrimSpace(contentID)
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if contentID == "" {
		return
	}
	if mediaType != virtualIdentityRepairMovieType && mediaType != virtualIdentityRepairEpisodeType {
		return
	}
	key := mediaType + "\x00" + contentID
	if !r.admit(key) {
		return
	}
	// The request is only the trigger. Keep its values (request id, trace) but
	// drop its cancellation: a client that closes the page must not cancel a
	// listing it already paid for, and the timeout below is the real bound.
	workCtx := context.WithoutCancel(ctx)
	run := r.run
	if run == nil {
		run = func(work func()) { go work() }
	}
	run(func() { r.repair(workCtx, key, contentID, mediaType) })
}

// admit applies the per-item and process-wide gates. A refused item is dropped
// rather than queued: the next page view is the retry, and the negative cache
// keeps a hopeless item cheap.
func (r *VirtualIdentityRepairer) admit(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if expires, ok := r.negative[key]; ok {
		if r.clock().Before(expires) {
			return false
		}
		delete(r.negative, key)
	}
	if _, ok := r.inflight[key]; ok {
		return false
	}
	select {
	case r.slots <- struct{}{}:
	default:
		// Saturated: shed instead of building a provider-work backlog.
		return false
	}
	r.inflight[key] = struct{}{}
	return true
}

// finish releases the item and its concurrency slot.
func (r *VirtualIdentityRepairer) finish(key string) {
	r.mu.Lock()
	delete(r.inflight, key)
	r.mu.Unlock()
	<-r.slots
}

// markNegative records an item whose listing succeeded but matched nothing, so
// opening it again within the TTL does not re-list the provider.
func (r *VirtualIdentityRepairer) markNegative(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.negative == nil {
		r.negative = make(map[string]time.Time)
	}
	now := r.clock()
	r.negative[key] = now.Add(virtualIdentityRepairNegativeTTL)
	if len(r.negative) >= virtualIdentityRepairNegativeSweepAt {
		for candidate, expires := range r.negative {
			if !now.Before(expires) {
				delete(r.negative, candidate)
			}
		}
	}
}

// repair is the one bounded pass for one item: resolve its legacy rows, list
// each candidate group once, match, write, and stop. It never pages the legacy
// set.
func (r *VirtualIdentityRepairer) repair(ctx context.Context, key, contentID, mediaType string) {
	defer r.finish(key)
	ctx, cancel := context.WithTimeout(ctx, virtualIdentityRepairTimeout)
	defer cancel()

	rows, err := r.itemRows(ctx, contentID, mediaType)
	if err != nil {
		// A read failure is transient and uncached: the next view retries.
		r.logger().DebugContext(ctx, "virtual identity repair: item rows unavailable",
			"component", "api", "item_id", contentID, "media_type", mediaType, "error", err)
		return
	}
	groups := virtualIdentityRepairGroups(rows)
	if len(groups) == 0 {
		// Either the item carries identity already or it has no virtual
		// candidate rows: nothing to list, nothing to remember.
		return
	}

	matched, complete := false, true
	for _, group := range groups {
		if !r.limiter.Allow() {
			// Out of listing budget for now. Leave the item retriable instead
			// of waiting: a detail page must not hold provider work.
			return
		}
		streams, err := r.Lister(ctx, group.path)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			complete = false
			r.logger().WarnContext(ctx, "virtual identity repair: provider re-list failed",
				"component", "api", "virtual_path", group.path, "rows", len(group.rows), "error", err)
			continue
		}
		for _, row := range group.rows {
			candidate, _, ok := matchBackfillCandidate(row, streams)
			if !ok {
				continue
			}
			wrote, err := r.Store.FillVirtualProviderIdentity(ctx, row.ID, row.FilePath, virtualProviderIdentityOf(candidate))
			if err != nil {
				r.logger().WarnContext(ctx, "virtual identity repair: fill failed",
					"component", "api", "file_id", row.ID, "error", err)
				complete = false
				continue
			}
			if wrote {
				matched = true
			}
		}
	}
	if complete && !matched {
		// The provider answered and this item still cannot be bound to a
		// release. Remember that so every later view within the TTL is free.
		r.markNegative(key)
	}
}

// itemRows resolves the file rows of the item that owns this page. Series and
// season details have no rows of their own; their episodes are repaired when an
// episode is opened.
func (r *VirtualIdentityRepairer) itemRows(ctx context.Context, contentID, mediaType string) ([]*models.MediaFile, error) {
	switch mediaType {
	case virtualIdentityRepairMovieType:
		return r.Store.GetByContentID(ctx, contentID)
	case virtualIdentityRepairEpisodeType:
		return r.Store.GetByEpisodeID(ctx, contentID)
	default:
		return nil, nil
	}
}

// virtualIdentityRepairGroup is one candidate group's legacy rows.
type virtualIdentityRepairGroup struct {
	path string
	rows []*models.MediaFile
}

// virtualIdentityRepairGroups selects the legacy virtual rows of one item and
// groups them by candidate source, mirroring the bulk pass's selection and
// grouping so both repair the same rows the same way. Rows that already carry a
// video hash or GUID are never selected, which is the "identity already
// exists" skip. At most virtualIdentityRepairMaxGroups groups are returned, in
// path order so the same view always picks the same groups.
func virtualIdentityRepairGroups(rows []*models.MediaFile) []virtualIdentityRepairGroup {
	byPath := make(map[string]*virtualIdentityRepairGroup, len(rows))
	for _, row := range rows {
		if !isLegacyVirtualIdentityRow(row) {
			continue
		}
		path, ok := virtualCandidateGroupURI(row.FilePath)
		if !ok {
			continue
		}
		key := virtualIdentityRepairGroupKey(path, row)
		group, ok := byPath[key]
		if !ok {
			if len(byPath) >= virtualIdentityRepairMaxGroups {
				continue
			}
			group = &virtualIdentityRepairGroup{path: path}
			byPath[key] = group
		}
		group.rows = append(group.rows, row)
	}
	if len(byPath) == 0 {
		return nil
	}
	keys := make([]string, 0, len(byPath))
	for key := range byPath {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	groups := make([]virtualIdentityRepairGroup, 0, len(keys))
	for _, key := range keys {
		groups = append(groups, *byPath[key])
	}
	return groups
}

// virtualIdentityRepairGroupKey is the bulk pass's group key, so a movie's
// quality profiles and an episode's owner stay distinct groups here too.
func virtualIdentityRepairGroupKey(path string, row *models.MediaFile) string {
	return path + "\x00" + row.EpisodeID + "\x00" + strconv.Itoa(row.MediaFolderID) + "\x00" + strconv.Itoa(row.VirtualOwnerInstallationID)
}

// isLegacyVirtualIdentityRow is the on-demand equivalent of the bulk pass's
// selection predicate: a virtual row that was persisted with an owner but
// never carried a provider video hash or GUID.
func isLegacyVirtualIdentityRow(row *models.MediaFile) bool {
	if row == nil || !row.VirtualOwnerInstallationSet {
		return false
	}
	isVirtual := strings.EqualFold(row.Container, "virtual") ||
		strings.HasPrefix(strings.ToLower(row.FilePath), virtualURIScheme+"://")
	if !isVirtual {
		return false
	}
	return strings.TrimSpace(row.ProviderVideoHash) == "" && strings.TrimSpace(row.ProviderGUID) == ""
}

// virtualProviderIdentityOf projects a matched candidate onto the identity
// payload, in the resolver's dedup-key tier order.
func virtualProviderIdentityOf(candidate virtuallibrary.PlaybackStream) scanner.VirtualProviderIdentity {
	return scanner.VirtualProviderIdentity{
		VideoHash:   candidate.ProviderVideoHash,
		GUID:        candidate.ProviderGUID,
		ReleaseName: candidate.ProviderReleaseName,
		ReleaseSize: candidate.FileSize,
	}
}

func (r *VirtualIdentityRepairer) clock() time.Time {
	if r != nil && r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *VirtualIdentityRepairer) logger() *slog.Logger {
	if r == nil || r.Logger == nil {
		return slog.Default()
	}
	return r.Logger
}
