package handlers

import (
	"context"
	"log/slog"
	"maps"
	"net/url"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// The pre-play probe chain closes the gap between discovery persistence and
// the first play attempt. Discovery (the v2 catalog leaf detail path) lists a
// virtual item's provider candidates and persists them, but the persisted
// rows carry declared metadata only: ffprobe evidence — the exact stream
// table — lands the first time the version actually plays. A client that
// opens the detail page and immediately presses Play therefore pays the
// probe latency on its first start even though discovery just ran. Android
// has no background refetch of its own, so that cost always lands on the
// first open there.
//
// The chain re-reads the item's default playback version after persistence,
// requires that its bytes are known exactly (the row must be a persisted
// candidate whose URI appears in the just-persisted listing — a neutral
// group row may resolve to a sibling candidate, so it is never probed
// speculatively), and probes it through the same background evidence path
// the resolve uses (queued, coalescing write; never the foreground direct
// fallback). Nothing here gates playback: the play path is untouched and
// never observes the chain. A chain may finish its probe before a play
// attempt resolves the same row — that attempt then reuses the recorded
// evidence — or play may win first; the queued evidence write follows the
// ordinary persistence rules either way. Every chain is optional best-effort
// work: any failure, including an unexpected panic, may never affect the
// server process serving it.

// virtualDefaultProbeChainBudget bounds one admitted chain's whole life: the
// default-version re-read, the candidate match, and the ffprobe enumeration
// behind the probe call all run under this deadline. probeVirtualSourceAndPersist
// applies its own inner budget on top, so the total cost of a chain is bounded
// twice: once here, by the chain owner, and once inside the probe.
var virtualDefaultProbeChainBudget = 90 * time.Second

// virtualDefaultProbeChainConcurrency bounds how many chains run at once.
// Two keeps a household-wide discovery burst from saturating the detached
// gate while still admitting a genuinely concurrent browse.
const virtualDefaultProbeChainConcurrency = 2

// virtualProbeChainTriggerDiscovery names the one caller that schedules
// chains. It is a log attribute, not a dispatch key: refresh runs never
// schedule (see VirtualCandidatesRefreshService.listSource).
const virtualProbeChainTriggerDiscovery = "virtual-candidates:discover"

// Reasons a discovered item ends up not probed. They are log values only —
// nothing branches on them — so an operator can tell a chain that was never
// admitted (schedule_refused) from one that ran out of budget before it
// reached the probe (budget_expired) and one whose probe failed
// (probe_failed). The remaining values name ordinary skips.
const (
	virtualProbeChainReasonScheduleRefused    = "schedule_refused"
	virtualProbeChainReasonBudgetExpired      = "budget_expired"
	virtualProbeChainReasonDefaultUnavailable = "default_unavailable"
	virtualProbeChainReasonProbeFailed        = "probe_failed"
	virtualProbeChainReasonProbeIncomplete    = "probe_incomplete"
	virtualProbeChainReasonNoDefaultVersion   = "no_default_version"
	virtualProbeChainReasonNotVirtual         = "not_virtual"
	virtualProbeChainReasonAlreadyProbed      = "already_probed"
	virtualProbeChainReasonNoExactCandidate   = "no_exact_candidate"
	virtualProbeChainReasonSiblingTarget      = "sibling_target"
	virtualProbeChainReasonRecentlyFailed     = "recently_failed"
	virtualProbeChainReasonGateBusy           = "gate_busy"
	virtualProbeChainReasonRevalidateFailed   = "revalidate_failed"
	virtualProbeChainReasonLeaseRefused       = "lease_refused"
	virtualProbeChainReasonProviderStartLimit = "provider_start_limited"
	virtualProbeChainReasonSourceCooldown     = "source_cooldown"
)

// Refusal logging has constant memory and a process-wide budget, independent
// of caller-controlled item IDs. Suppression never affects probe admission.
var virtualProbeRefusalLogs = rate.NewLimiter(rate.Every(time.Minute), 1)

// logVirtualProbeChainSkip records why one discovered item was not probed.
// The first play still probes the row, so these lines are observability, not
// alerts: expected no-ops stay at debug, while a refused, expired, or failed
// chain leaves one info line naming the content and the discovery trigger.
func logVirtualProbeChainSkip(ctx context.Context, level slog.Level, contentID, reason, detail string, attrs ...any) {
	if reason == virtualProbeChainReasonScheduleRefused && level >= slog.LevelInfo && !virtualProbeRefusalLogs.Allow() {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	args := []any{
		virtualEvidenceLogKeyComponent, virtualEvidenceLogValueAPI,
		contentIDKey, contentID,
		virtualEvidenceLogKeyReason, reason,
		"trigger", virtualProbeChainTriggerDiscovery,
	}
	if detail != "" {
		args = append(args, "detail", detail)
	}
	args = append(args, attrs...)
	slog.Log(ctx, level, "virtual pre-play probe chain skipped", args...)
}

// VirtualDefaultProbeTarget re-reads the item's default (top-sorted) playback
// version after discovery persistence. The chain must probe the row a viewer
// will actually open — the default — and it must re-read those rows after
// persistence instead of trusting the discovery response, because discovery
// may have replaced rows between the request and the persist landing.
//
// The read carries the access filter of the request that triggered discovery:
// "the default" is the default of the versions that viewer may see, and a
// version outside their visibility is not a version they can open.
type VirtualDefaultProbeTarget interface {
	DefaultVirtualProbeTarget(ctx context.Context, contentID string, filter catalog.AccessFilter) (*models.MediaFile, error)
}

// VirtualProbeTargetRows is the optional extension of VirtualDefaultProbeTarget
// that exposes every authorized version row of an item, in the same top-sorted
// order the browse picker shows. A chain uses it when the resolved row names
// no release in the just-persisted listing — the default of a virtual item is
// often a neutral group row, and the listing names no release for it.
type VirtualProbeTargetRows interface {
	VirtualProbeTargetRows(ctx context.Context, contentID string, filter catalog.AccessFilter) ([]*models.MediaFile, error)
}

// VirtualDefaultProbeTargetFunc allows wiring the default-version lookup as a
// plain function.
type VirtualDefaultProbeTargetFunc func(ctx context.Context, contentID string, filter catalog.AccessFilter) (*models.MediaFile, error)

// DefaultVirtualProbeTarget re-reads the item's default playback version.
func (f VirtualDefaultProbeTargetFunc) DefaultVirtualProbeTarget(ctx context.Context, contentID string, filter catalog.AccessFilter) (*models.MediaFile, error) {
	return f(ctx, contentID, filter)
}

// VirtualProbeVersionLister is the catalog version read behind the production
// default-probe target: the same top-sorted, access-filtered version list the
// browse picker uses. *catalog.DetailService implements it.
type VirtualProbeVersionLister interface {
	GetItemVersions(ctx context.Context, contentID string, filter catalog.AccessFilter) ([]catalog.FileVersion, error)
}

// VirtualProbeFileReader resolves a version's media file row.
// *scanner.FileRepository implements it.
type VirtualProbeFileReader interface {
	GetByID(ctx context.Context, id int) (*models.MediaFile, error)
}

// NewVirtualDefaultProbeTarget builds the production default-probe target from
// the catalog detail service and the file repository. It resolves the default
// under the access filter of the discovery request that scheduled the chain
// (see VirtualDefaultProbeTarget), so versions the viewer cannot see are never
// read and never probed. A nil reader disables the target, and with it the
// chain.
func NewVirtualDefaultProbeTarget(versions VirtualProbeVersionLister, files VirtualProbeFileReader) VirtualDefaultProbeTarget {
	if versions == nil || files == nil {
		return nil
	}
	return &virtualDefaultProbeTarget{
		revalidatingVirtualProbeTarget: revalidatingVirtualProbeTarget{VirtualDefaultProbeTargetFunc(func(ctx context.Context, contentID string, filter catalog.AccessFilter) (*models.MediaFile, error) {
			list, err := versions.GetItemVersions(ctx, contentID, filter)
			if err != nil || len(list) == 0 {
				return nil, err
			}
			selected := list[0].FileID
			if filter.SelectedFileID != 0 {
				selected = 0
				for _, version := range list {
					if version.FileID == filter.SelectedFileID {
						selected = version.FileID
						break
					}
				}
				if selected == 0 {
					return nil, nil
				}
			}
			row, err := files.GetByID(ctx, selected)
			if err != nil || row == nil {
				return nil, err
			}
			// A catalog listing authorizes an association, not unrestricted access
			// to a numeric ID. Revalidate the fetched row after the listing/read
			// boundary; an episode row must match the episode, not its series.
			if row.ID != selected || !virtualProbeRowAuthorized(row, contentID, filter) {
				return nil, nil
			}
			return row, nil
		})},
		versions: versions,
		files:    files,
	}
}

// virtualDefaultProbeTarget is the production probe target. It reads the
// catalog's top-sorted, access-filtered version list, resolves the default
// (or the selected) row of that list, revalidates that row immediately before
// submission, and — for a chain that needs a row the listing names a release
// for — exposes every authorized row of the same list.
type virtualDefaultProbeTarget struct {
	revalidatingVirtualProbeTarget
	versions VirtualProbeVersionLister
	files    VirtualProbeFileReader
}

// virtualProbeRowAuthorized applies the association and visibility rules a
// probe target must satisfy: the row must belong to the item or episode the
// read asked for, and it must be inside the viewer's library scope.
func virtualProbeRowAuthorized(row *models.MediaFile, contentID string, filter catalog.AccessFilter) bool {
	if row == nil {
		return false
	}
	if (row.EpisodeID != "" && row.EpisodeID != contentID) ||
		(row.EpisodeID == "" && row.ContentID != contentID) {
		return false
	}
	if _, none := filter.LibraryScope([]int{row.MediaFolderID}); none {
		return false
	}
	if filter.ScopeFilesToLibrary && filter.PresentationLibraryID != nil && row.MediaFolderID != *filter.PresentationLibraryID {
		return false
	}
	return true
}

// VirtualProbeTargetRows returns every authorized version row of the item, in
// the same top-sorted order the browse picker shows. A row that cannot be
// read, or that fails the association or visibility rules, is not a candidate
// and is left out: the list is the authorization boundary.
func (t *virtualDefaultProbeTarget) VirtualProbeTargetRows(ctx context.Context, contentID string, filter catalog.AccessFilter) ([]*models.MediaFile, error) {
	list, err := t.versions.GetItemVersions(ctx, contentID, filter)
	if err != nil {
		return nil, err
	}
	rows := make([]*models.MediaFile, 0, len(list))
	for _, version := range list {
		row, err := t.files.GetByID(ctx, version.FileID)
		if err != nil || row == nil || row.ID != version.FileID {
			continue
		}
		if !virtualProbeRowAuthorized(row, contentID, filter) {
			continue
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// The production resolver repeats the authorized catalog read immediately
// before submission. Test-only target implementations need not implement it.
type revalidatingVirtualProbeTarget struct{ VirtualDefaultProbeTarget }

func (r revalidatingVirtualProbeTarget) Revalidate(ctx context.Context, contentID string, filter catalog.AccessFilter, row *models.MediaFile) bool {
	filter.SelectedFileID = row.ID
	current, err := r.DefaultVirtualProbeTarget(ctx, contentID, filter)
	return err == nil && current != nil && current.FilePath == row.FilePath && current.VirtualOwnerInstallationID == row.VirtualOwnerInstallationID
}

// defaultProbeChainAdmission deduplicates and bounds pre-play probe chains.
// The map holds one entry per live chain, keyed by the content id the viewer
// opened, and admitted counts live chains against
// virtualDefaultProbeChainConcurrency.
//
// acquire is the whole admission operation: it checks the item key and the
// concurrency bound under one lock and performs no I/O, waits for nothing,
// and spawns nothing. A rejected caller returns immediately; the database
// reads and the goroutine belong to admitted work only. The zero value is
// ready to use.
type defaultProbeChainAdmission struct {
	mu             sync.Mutex
	active         map[string]struct{}
	admitted       int
	cooldowns      map[string]*virtualProbeCooldown
	cooldownQueue  []string
	providerStarts map[string][]time.Time
}

const virtualProbeSourceCooldown = time.Minute
const virtualProbeCooldownCapacity = 1024

type virtualProbeCooldown struct {
	expires time.Time
}

// recentlyStarted performs at most eight expiry removals per admission. Queue
// entries stay bounded even on rollback; each reservation has a unique owner.
func (a *defaultProbeChainAdmission) recentlyStarted(key string, reserve bool) (bool, *virtualProbeCooldown) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for n := 0; n < 8 && len(a.cooldownQueue) > 0; n++ {
		oldest := a.cooldownQueue[0]
		if now.Before(a.cooldowns[oldest].expires) {
			break
		}
		delete(a.cooldowns, oldest)
		a.cooldownQueue = a.cooldownQueue[1:]
	}
	if current, ok := a.cooldowns[key]; ok {
		if !current.expires.IsZero() {
			return true, nil
		}
		if reserve {
			owner := &virtualProbeCooldown{expires: now.Add(virtualProbeSourceCooldown)}
			a.cooldowns[key] = owner
			return false, owner
		}
		return false, nil
	}
	if len(a.cooldowns) >= virtualProbeCooldownCapacity {
		return true, nil
	}
	if reserve {
		if a.cooldowns == nil {
			a.cooldowns = make(map[string]*virtualProbeCooldown)
		}
		owner := &virtualProbeCooldown{expires: now.Add(virtualProbeSourceCooldown)}
		a.cooldowns[key] = owner
		a.cooldownQueue = append(a.cooldownQueue, key)
		return false, owner
	}
	return false, nil
}

func (a *defaultProbeChainAdmission) rollbackCooldown(key string, owner *virtualProbeCooldown) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if owner != nil && a.cooldowns[key] == owner {
		// Keep the queue entry for bounded pruning, without a release-time scan.
		owner.expires = time.Time{}
	}
}

// allowProviderStart permits at most four starts in any minute per installation.
// Saturation rejects new installations rather than evicting live allowances.
func (a *defaultProbeChainAdmission) allowProviderStart(provider string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if a.providerStarts == nil {
		a.providerStarts = make(map[string][]time.Time)
	}
	for key, starts := range a.providerStarts {
		for len(starts) > 0 && !now.Before(starts[0].Add(time.Minute)) {
			starts = starts[1:]
		}
		if len(starts) == 0 {
			delete(a.providerStarts, key)
		} else {
			a.providerStarts[key] = starts
		}
	}
	starts := a.providerStarts[provider]
	if len(starts) >= 4 || (len(starts) == 0 && len(a.providerStarts) >= virtualProbeCooldownCapacity) {
		return false
	}
	a.providerStarts[provider] = append(starts, now)
	return true
}

// defaultProbeChainRefusal names why admission rejected a chain. The empty
// value means the chain was admitted.
type defaultProbeChainRefusal string

const (
	defaultProbeChainRefusalNone     defaultProbeChainRefusal = ""
	defaultProbeChainRefusalLive     defaultProbeChainRefusal = "chain_already_live"
	defaultProbeChainRefusalCapacity defaultProbeChainRefusal = "concurrency_full"
)

// acquire admits one chain for contentID. refusal is empty when admitted;
// otherwise it names the reason the callback was dropped. A live chain for the
// item already covers it (duplicate callbacks coalesce into that chain), and
// when every slot is taken the callback is dropped — the play path and any
// later discovery run still probe normally. The returned func releases the
// slot and must be called exactly once.
func (a *defaultProbeChainAdmission) acquire(contentID string) (func(), defaultProbeChainRefusal) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active == nil {
		a.active = make(map[string]struct{})
	}
	if _, live := a.active[contentID]; live {
		return nil, defaultProbeChainRefusalLive
	}
	if a.admitted >= virtualDefaultProbeChainConcurrency {
		return nil, defaultProbeChainRefusalCapacity
	}
	a.active[contentID] = struct{}{}
	a.admitted++
	return func() { a.release(contentID) }, defaultProbeChainRefusalNone
}

func (a *defaultProbeChainAdmission) release(contentID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, live := a.active[contentID]; !live {
		return
	}
	delete(a.active, contentID)
	a.admitted--
}

// ScheduleDefaultFileProbe is the subscription point the discovery services
// call after a successful detached persistence (see
// VirtualCandidatesRefreshService.ScheduleDefaultFileProbe). It is
// non-blocking by contract: admission is a single atomic check, and
// everything else — including the database re-read — happens inside the
// admitted chain, never on the discovery caller. filter is the access filter
// of the request that triggered discovery; the chain resolves its default
// under that same filter (see VirtualDefaultProbeTarget).
func (h *PlaybackHandler) ScheduleDefaultFileProbe(contentID string, source *models.MediaFile, streams []VirtualPlaybackStream, filter catalog.AccessFilter) {
	if h == nil {
		logVirtualProbeChainSkip(context.Background(), slog.LevelInfo, contentID,
			virtualProbeChainReasonScheduleRefused, "nil_handler")
		return
	}
	if contentID == "" || source == nil || len(streams) == 0 {
		logVirtualProbeChainSkip(context.Background(), slog.LevelInfo, contentID,
			virtualProbeChainReasonScheduleRefused, "invalid_input")
		return
	}
	if h.DefaultProbeTarget == nil {
		logVirtualProbeChainSkip(context.Background(), slog.LevelInfo, contentID,
			virtualProbeChainReasonScheduleRefused, "target_unwired")
		return
	}
	if h.VirtualPlaybackSourceProber == nil && h.VirtualPlaybackSourceProberWithHeaders == nil {
		logVirtualProbeChainSkip(context.Background(), slog.LevelInfo, contentID,
			virtualProbeChainReasonScheduleRefused, "prober_unwired")
		return
	}
	if recent, _ := h.defaultProbeChains.recentlyStarted(contentID+"\x00"+virtualSourceRefreshKey(source), false); recent {
		return
	}
	release, refusal := h.defaultProbeChains.acquire(contentID + "\x00" + strconv.Itoa(filter.SelectedFileID) + "\x00" + virtualSourceRefreshKey(source))
	if refusal != defaultProbeChainRefusalNone {
		// A chain for this item is already live, or both slots are taken.
		// Either way the callback is dropped: the live chain re-reads the same
		// defaults, and a dropped probe still happens on the first play. No
		// queue, no retry, no delay for the caller.
		logVirtualProbeChainSkip(context.Background(), slog.LevelInfo, contentID,
			virtualProbeChainReasonScheduleRefused, string(refusal))
		return
	}
	// Snapshot the exact inputs so a later mutation of the caller's structures
	// cannot change what is probed: the row is copied by value, the filter's
	// reference members are cloned, and the listing entries are deep-copied
	// for the mutable members the probe reads.
	probeSource := *source
	filterSnapshot := virtualProbeAccessFilterSnapshot(filter)
	streamsSnapshot := make([]VirtualPlaybackStream, len(streams))
	for i := range streams {
		streamsSnapshot[i] = virtualProbeStreamSnapshot(streams[i])
	}
	// The chain detaches from the discovery caller: its context carries the
	// chain budget and ends with the service (virtualDetachedContext cancels
	// on service shutdown), never with the browse request that listed the
	// candidates. The single cleanup defer runs in a fixed order — recover
	// log, admission release, context cancel, completion signal — so a panic
	// anywhere in the chain body is an observation, not a process death
	// unrelated playback would share, and Done() reports the slot truly
	// free: a Wait() caller must be able to re-schedule the same item
	// without racing the previous chain's teardown.
	ctx, cancel := h.virtualDetachedContext(context.Background(), virtualDefaultProbeChainBudget)
	h.probeChainWG.Add(1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.ErrorContext(context.WithoutCancel(ctx), "virtual pre-play probe chain panicked",
					"component", "api", "content_id", contentID, "stack", string(debug.Stack()))
			}
			release()
			cancel()
			h.probeChainWG.Done()
		}()
		h.runDefaultFileProbeChain(ctx, contentID, &probeSource, streamsSnapshot, filterSnapshot)
	}()
}

// virtualProbeAccessFilterSnapshot deep-copies the reference members of an
// access filter so the detached chain reads exactly the constraints of the
// request that scheduled it, even if that request's structures are reused
// afterwards. Scalar members copy with the value.
func virtualProbeAccessFilterSnapshot(filter catalog.AccessFilter) catalog.AccessFilter {
	clone := filter
	clone.AllowedLibraryIDs = slices.Clone(filter.AllowedLibraryIDs)
	clone.AllowedContentIDs = slices.Clone(filter.AllowedContentIDs)
	clone.DisabledLibraryIDs = slices.Clone(filter.DisabledLibraryIDs)
	clone.ExcludedMediaTypes = slices.Clone(filter.ExcludedMediaTypes)
	clone.MetadataLanguageOverrides = maps.Clone(filter.MetadataLanguageOverrides)
	if filter.PresentationLibraryID != nil {
		presentationLibrary := *filter.PresentationLibraryID
		clone.PresentationLibraryID = &presentationLibrary
	}
	return clone
}

// virtualProbeStreamSnapshot deep-copies the mutable members a probe reads
// from one listing entry. Shallow struct copies share the request-header map
// and the declared track slices with the discovery caller, so a later caller
// mutation could change or race a running probe; every member the chain
// consumes is cloned.
func virtualProbeStreamSnapshot(base VirtualPlaybackStream) VirtualPlaybackStream {
	clone := base
	clone.RequestHeaders = maps.Clone(base.RequestHeaders)
	clone.AudioLanguages = slices.Clone(base.AudioLanguages)
	clone.SubtitleLanguages = slices.Clone(base.SubtitleLanguages)
	return clone
}

// runDefaultFileProbeChain is the body of one admitted chain. Every step is
// bounded by the chain context; a failure or an unsafe target leaves one
// structured skip line naming the reason (see logVirtualProbeChainSkip).
func (h *PlaybackHandler) runDefaultFileProbeChain(ctx context.Context, contentID string, source *models.MediaFile, streams []VirtualPlaybackStream, filter catalog.AccessFilter) {
	if err := ctx.Err(); err != nil {
		logVirtualProbeChainSkip(ctx, slog.LevelInfo, contentID,
			virtualProbeChainReasonBudgetExpired, "chain_start", virtualEvidenceLogKeyError, err)
		return
	}
	// Re-read the persisted default. Discovery may have replaced the rows
	// between the listing and the persistence of this chain's inputs, so the
	// target is never the request-time row: it is whatever the catalog
	// serves now for this item, under the triggering request's access filter.
	row, err := h.DefaultProbeTarget.DefaultVirtualProbeTarget(ctx, contentID, filter)
	if err != nil {
		if ctx.Err() != nil {
			logVirtualProbeChainSkip(ctx, slog.LevelInfo, contentID,
				virtualProbeChainReasonBudgetExpired, "default_read", virtualEvidenceLogKeyError, ctx.Err())
			return
		}
		logVirtualProbeChainSkip(ctx, slog.LevelInfo, contentID,
			virtualProbeChainReasonDefaultUnavailable, "default_read", virtualEvidenceLogKeyError, err)
		return
	}
	if ctx.Err() != nil {
		logVirtualProbeChainSkip(ctx, slog.LevelInfo, contentID,
			virtualProbeChainReasonBudgetExpired, "default_read", virtualEvidenceLogKeyError, ctx.Err())
		return
	}
	if row == nil {
		logVirtualProbeChainSkip(ctx, slog.LevelDebug, contentID,
			virtualProbeChainReasonNoDefaultVersion, "")
		return
	}
	if !isVirtualPlaybackFile(row) {
		logVirtualProbeChainSkip(ctx, slog.LevelDebug, contentID,
			virtualProbeChainReasonNotVirtual, "")
		return
	}
	// Stamp skip and damper check happen inside admitted work — the admission
	// callback only reserves the slot, by design.
	if row.ProbeUpdatedAt != nil {
		logVirtualProbeChainSkip(ctx, slog.LevelDebug, contentID,
			virtualProbeChainReasonAlreadyProbed, "", virtualEvidenceLogKeyFileID, row.ID)
		return
	}
	probeCand := virtualDefaultProbeCandidate(row, source, streams)
	if probeCand == nil {
		// The resolved row names no release in the just-persisted listing: the
		// default of a virtual item is often a neutral group row, and the
		// listing names no release for it. A pinned row of the same authorized
		// list that the listing itself names IS an admissible target, and it
		// is the row a play attempt resolves for the item. The play path's own
		// resolution stays untouched: this only warms up evidence for bytes
		// the listing already named.
		sibling, siblingCand := h.probeableSiblingTarget(ctx, contentID, row, source, streams, filter)
		if sibling != nil {
			row, probeCand = sibling, siblingCand
			slog.InfoContext(ctx, "virtual pre-play probe target selected",
				virtualEvidenceLogKeyComponent, virtualEvidenceLogValueAPI,
				contentIDKey, contentID,
				virtualEvidenceLogKeyFileID, row.ID,
				"row_source", row.ProbeSource,
				"selected_as", virtualProbeChainReasonSiblingTarget)
		}
	}
	if probeCand == nil {
		// Without a release the listing names there is nothing safe to
		// enumerate: the row may be a neutral group row, and a neutral row may
		// resolve to a sibling candidate the listing never named. The play
		// path's own probe handles those; a chain must never guess. This line
		// is info, not debug, so an operator can see an item that is never
		// prepared.
		logVirtualProbeChainSkip(ctx, slog.LevelInfo, contentID,
			virtualProbeChainReasonNoExactCandidate, "", virtualEvidenceLogKeyFileID, row.ID,
			"row_source", row.ProbeSource)
		return
	}
	oid := row.VirtualOwnerInstallationID
	if virtualProbeFailures.recent(virtualProbeFailureKey(probeCand.URI, oid)) {
		// A candidate that failed recently is intentionally not re-probed on
		// every open, but that suppression is the one refusal that otherwise
		// looks identical to a chain that never ran, so it is named at info.
		logVirtualProbeChainSkip(ctx, slog.LevelInfo, contentID,
			virtualProbeChainReasonRecentlyFailed, "", virtualEvidenceLogKeyFileID, row.ID)
		return
	}
	// The enumeration runs behind the shared detached gate, the same bounded
	// admission every background virtual probe uses. No slot, no probe.
	gate := h.detachedGate()
	if gate == nil || !gate.tryAcquire() {
		logVirtualProbeChainSkip(ctx, slog.LevelInfo, contentID,
			virtualProbeChainReasonGateBusy, "", virtualEvidenceLogKeyFileID, row.ID)
		return
	}
	defer gate.release()
	expectedRuntimeMinutes := h.virtualExpectedRuntimeMinutes(ctx, row)
	stickyKey := bestResultCacheKey(contentID, virtualPlaybackNeutralKey(row.FilePath), oid)
	probeTransient := cloneVirtualProbeTransient(*row)
	// Zero the duration so the probe measures the empirical duration instead
	// of inheriting the catalog value.
	probeTransient.Duration = 0
	// The chain probes the concrete provider URL the listing named for this
	// release — the same bytes a play attempt of the row resolves and ffprobes
	// — and persists through the queued, coalescing evidence path. The prober
	// runs a real URL behind the remote-stream relay; a virtual:// row path is
	// not a URL it can open. The candidate's durable identity travels with the
	// probe, as the play path's own background probe carries it. No session
	// binding exists here, so the fence is nil.
	if target, ok := h.DefaultProbeTarget.(interface {
		Revalidate(context.Context, string, catalog.AccessFilter, *models.MediaFile) bool
	}); ok && !target.Revalidate(ctx, contentID, filter, row) {
		// The authorized read that selected the target no longer returns it
		// under the same filter: discovery replaced or re-scoped the rows while
		// this chain was queued. Nothing safe to enumerate, but the refusal is
		// named — without this line a refused revalidation looks exactly like a
		// chain that silently stopped.
		logVirtualProbeChainSkip(ctx, slog.LevelInfo, contentID,
			virtualProbeChainReasonRevalidateFailed, "revalidate", virtualEvidenceLogKeyFileID, row.ID)
		return
	}
	// Only an exact, runnable probe consumes the item's lease. Default and
	// selected targets of the same item and provider share it, so duplicate
	// discovery callbacks coalesce across replicas; a different item stays
	// independent and prepares while this one probes.
	releaseLease, admitted := h.ProbeLease.acquire(ctx, contentID+"\x00"+strconv.Itoa(oid))
	if !admitted {
		// A configured Redis that is unreachable, or another chain holding this
		// provider's lease, refuses optional work. Both are correct, and both
		// must be visible: a permanent silent refusal disables preparation
		// while every other signal still says the chain selected a target.
		logVirtualProbeChainSkip(ctx, slog.LevelInfo, contentID,
			virtualProbeChainReasonLeaseRefused, "lease", virtualEvidenceLogKeyFileID, row.ID)
		return
	}
	defer releaseLease()
	if !h.defaultProbeChains.allowProviderStart(strconv.Itoa(oid)) {
		logVirtualProbeChainSkip(ctx, slog.LevelInfo, contentID,
			virtualProbeChainReasonProviderStartLimit, "provider_start", virtualEvidenceLogKeyFileID, row.ID)
		return
	}
	cooldownKey := contentID + "\x00" + virtualSourceRefreshKey(source)
	recent, owner := h.defaultProbeChains.recentlyStarted(cooldownKey, true)
	if recent {
		logVirtualProbeChainSkip(ctx, slog.LevelDebug, contentID,
			virtualProbeChainReasonSourceCooldown, "cooldown", virtualEvidenceLogKeyFileID, row.ID)
		return
	}
	completed := false
	defer func() {
		if !completed {
			h.defaultProbeChains.rollbackCooldown(cooldownKey, owner)
		}
	}()
	// Name the enumeration before it starts and its verdict after it returns.
	// A chain that reaches the probe and then persists nothing otherwise looks
	// identical to one that never probed at all.
	slog.InfoContext(ctx, "virtual pre-play probe started",
		virtualEvidenceLogKeyComponent, virtualEvidenceLogValueAPI,
		contentIDKey, contentID,
		virtualEvidenceLogKeyFileID, row.ID,
		"candidate_uri", probeCand.URI,
		"probe_host", virtualProbeURLHost(probeCand.ProviderURL))
	outcome := h.probeVirtualSourceAndPersist(
		ctx, stickyKey, row, strings.TrimSpace(probeCand.ProviderURL), probeTransient, *probeCand,
		resolver.NewPersistedIdentityTiers(
			probeCand.ProviderVideoHash, probeCand.ProviderGUID, probeCand.ProviderReleaseName, probeCand.FileSize,
		),
		expectedRuntimeMinutes, oid,
	)
	completed = outcome != probeOutcomeFailed
	switch outcome {
	case probeOutcomeFailed:
		if ctx.Err() != nil {
			logVirtualProbeChainSkip(ctx, slog.LevelInfo, contentID,
				virtualProbeChainReasonBudgetExpired, "probe", virtualEvidenceLogKeyError, ctx.Err())
			return
		}
		logVirtualProbeChainSkip(ctx, slog.LevelInfo, contentID,
			virtualProbeChainReasonProbeFailed, "probe", virtualEvidenceLogKeyFileID, row.ID)
	case probeOutcomePartial:
		logVirtualProbeChainSkip(ctx, slog.LevelInfo, contentID,
			virtualProbeChainReasonProbeIncomplete, "probe", virtualEvidenceLogKeyFileID, row.ID)
	}
	// The chain is done and the evidence write has been queued. Naming the
	// verdict here is what separates "probed, evidence refused later" from
	// "never probed": without it a successful enumeration that persists
	// nothing leaves no trace at all.
	slog.InfoContext(ctx, "virtual pre-play probe finished",
		virtualEvidenceLogKeyComponent, virtualEvidenceLogValueAPI,
		contentIDKey, contentID,
		virtualEvidenceLogKeyFileID, row.ID,
		"outcome", outcome)
}

// virtualProbeURLHost returns the host of a concrete provider URL for logs.
// The provider URL may carry a token in its path or query, so only the host is
// recorded; the candidate URI already identifies the release.
func virtualProbeURLHost(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Host
}

// probeableSiblingTarget returns the first authorized version row the
// just-persisted listing names a release for, in picker order. A row qualifies
// only when the listing names its release — the same concrete ?result=
// candidate id under the row's provider-neutral path — so the chain never
// probes a row the listing does not name and never probes a neutral group row:
// a neutral row names no release, so it never matches. The row already tried
// is skipped, as are rows that are not virtual playback files and rows that
// already carry probe evidence.
func (h *PlaybackHandler) probeableSiblingTarget(ctx context.Context, contentID string, tried *models.MediaFile, source *models.MediaFile, streams []VirtualPlaybackStream, filter catalog.AccessFilter) (*models.MediaFile, *VirtualPlaybackStream) {
	lister, ok := h.DefaultProbeTarget.(VirtualProbeTargetRows)
	if !ok {
		return nil, nil
	}
	rows, err := lister.VirtualProbeTargetRows(ctx, contentID, filter)
	if err != nil {
		return nil, nil
	}
	for _, row := range rows {
		if row == nil {
			continue
		}
		if tried != nil && row.ID == tried.ID {
			continue
		}
		if ctx.Err() != nil {
			return nil, nil
		}
		if !isVirtualPlaybackFile(row) {
			continue
		}
		if row.ProbeUpdatedAt != nil {
			continue
		}
		if cand := virtualDefaultProbeCandidate(row, source, streams); cand != nil {
			return row, cand
		}
	}
	return nil, nil
}

// virtualDefaultProbeCandidate returns the listing entry that names the row's
// own release — the candidate a play attempt of the row would resolve and
// probe.
//
// The listing is the provider's candidate list, not a catalog snapshot: every
// entry is built with OwnerInstallationID 0 (resolve.go, variants.go) and the
// provider's own URI. The persisted row carries its own installation id and
// may add a profile qualifier the provider does not know. Requiring an
// installation-id match, or a byte-identical URI, therefore made every real
// listing unnameable: a pinned row on owner 1 never equaled a listing entry on
// owner 0. What the listing must name is the release, so the match is the
// concrete ?result= candidate id under the row's provider-neutral path — the
// same identity virtualCandidateRowVerified already accepts as the row's own
// evidence.
//
// A neutral group row names no release (?result= absent), so it never matches:
// probing it would mean guessing which candidate it resolves to at play time.
// A hidden or expired candidate is not admissible either, and the entry must
// carry the concrete URL the probe opens.
func virtualDefaultProbeCandidate(row *models.MediaFile, source *models.MediaFile, streams []VirtualPlaybackStream) *VirtualPlaybackStream {
	if row == nil || source == nil {
		return nil
	}
	target := strings.TrimSpace(row.FilePath)
	if target == "" || virtualResultCandidateID(target) == "" {
		return nil
	}
	if source.FilePath == target {
		return nil
	}
	now := time.Now()
	for i := range streams {
		stream := &streams[i]
		if !sameVirtualReleaseIdentity(target, stream.URI) {
			continue
		}
		if stream.VisibilitySpecified && !stream.Visible {
			continue
		}
		if stream.ProviderExpiresAt != nil && !stream.ProviderExpiresAt.After(now) {
			continue
		}
		if strings.TrimSpace(stream.ProviderURL) == "" {
			continue
		}
		return stream
	}
	return nil
}
