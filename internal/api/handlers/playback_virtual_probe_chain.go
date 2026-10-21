package handlers

import (
	"context"
	"log/slog"
	"maps"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

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
	virtualProbeChainReasonRecentlyFailed     = "recently_failed"
	virtualProbeChainReasonGateBusy           = "gate_busy"
)

// logVirtualProbeChainSkip records why one discovered item was not probed.
// The first play still probes the row, so these lines are observability, not
// alerts: expected no-ops stay at debug, while a refused, expired, or failed
// chain leaves one info line naming the content and the discovery trigger.
func logVirtualProbeChainSkip(ctx context.Context, level slog.Level, contentID, reason, detail string, attrs ...any) {
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
	return VirtualDefaultProbeTargetFunc(func(ctx context.Context, contentID string, filter catalog.AccessFilter) (*models.MediaFile, error) {
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
		// A selected catalog entry authorizes the association named by the
		// listing, not an unrelated row that happens to carry the same id.
		if row.ID != selected || (row.EpisodeID != "" && row.EpisodeID != contentID) ||
			(row.EpisodeID == "" && row.ContentID != contentID) {
			return nil, nil
		}
		if _, none := filter.LibraryScope([]int{row.MediaFolderID}); none {
			return nil, nil
		}
		if filter.ScopeFilesToLibrary && filter.PresentationLibraryID != nil && row.MediaFolderID != *filter.PresentationLibraryID {
			return nil, nil
		}
		return row, nil
	})
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
	mu       sync.Mutex
	active   map[string]struct{}
	admitted int
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
	// One chain per item and selected version. Discovery persists one source
	// group at a time; keying admission by source as well let five groups for
	// the same title each schedule a chain, exhausting the process-wide slots
	// with duplicates. The chain re-reads the selected row and probes that row's
	// own stored URL, so any discovery group can safely start it.
	admissionKey := contentID + "\x00" + strconv.Itoa(filter.SelectedFileID)
	release, refusal := h.defaultProbeChains.acquire(admissionKey)
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
	oid := row.VirtualOwnerInstallationID
	probeCand := virtualDefaultProbeCandidate(row, source, streams, filter.SelectedFileID != 0)
	if probeCand == nil {
		// Without an exact byte match there is nothing safe to enumerate: the
		// default may be the neutral group row, and a neutral default may
		// resolve to a sibling candidate. The play path's own probe handles
		// those; a chain must never guess.
		logVirtualProbeChainSkip(ctx, slog.LevelDebug, contentID,
			virtualProbeChainReasonNoExactCandidate, "", virtualEvidenceLogKeyFileID, row.ID)
		return
	}
	if virtualProbeFailures.recent(virtualProbeFailureKey(probeCand.URI, oid)) {
		logVirtualProbeChainSkip(ctx, slog.LevelDebug, contentID,
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
	probeURL, probeHeaders, hasProbeURL := h.virtualChainProbeURL(ctx, row, *probeCand, filter)
	if !hasProbeURL {
		logVirtualProbeChainSkip(ctx, slog.LevelDebug, contentID,
			virtualProbeChainReasonNoExactCandidate, "no_provider_url", virtualEvidenceLogKeyFileID, row.ID)
		return
	}
	probeCand.RequestHeaders = probeHeaders
	// The chain probes the selected/default row's own candidate bytes and uses
	// the durable evidence path: an already-loaded detail/version page can only
	// be invalidated after the tracks are actually committed. No session binding
	// exists here, so the fence is nil and the identity tiers are zero — the same
	// posture the unstamped-row fast path takes, where the row owns its candidate.
	outcome, _ := h.probeVirtualSourceAndPersistWith(
		ctx, stickyKey, row, probeURL, probeTransient, *probeCand,
		resolver.PersistedIdentityTiers{}, expectedRuntimeMinutes, oid, true, nil,
	)
	if outcome == probeOutcomeVerified && h.EventsHub != nil {
		publishCtx, publishCancel := context.WithTimeout(context.WithoutCancel(ctx), virtualCandidatePublishBudget)
		publishVirtualVersionsUpdatedEvent(publishCtx, nil, h.EventsHub, contentID)
		publishCancel()
	}
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
}

// virtualChainProbeURL resolves the HTTP source the pre-play probe must open.
// The catalog row and the discovery listing both name the candidate by its
// durable virtual URI; that URI is not a provider transport and must never be
// handed to ffprobe. The fresh listing's resolved URL is preferred, then the
// row's stored URL while unexpired, then a fresh resolve under the same access
// filter that authorized discovery.
func (h *PlaybackHandler) virtualChainProbeURL(ctx context.Context, row *models.MediaFile, cand VirtualPlaybackStream, filter catalog.AccessFilter) (string, map[string]string, bool) {
	if row == nil {
		return "", nil, false
	}
	if url := strings.TrimSpace(cand.ProviderURL); url != "" {
		return url, maps.Clone(cand.RequestHeaders), true
	}
	if url := strings.TrimSpace(row.ResolvedURL); url != "" &&
		(row.ResolvedURLExpiresAt == nil || row.ResolvedURLExpiresAt.After(time.Now())) {
		return url, maps.Clone(row.ProviderRequestHeaders), true
	}
	if h.VirtualMediaDetailedResolver == nil {
		return "", nil, false
	}
	resolved, err := h.VirtualMediaDetailedResolver.ResolveVirtualMediaDetailed(
		ctx, row.FilePath, row.VirtualOwnerInstallationID, filter.UserID, filter.ProfileID,
		false, nil, "",
	)
	if err != nil || strings.TrimSpace(resolved.URL) == "" {
		return "", nil, false
	}
	return resolved.URL, maps.Clone(resolved.RequestHeaders), true
}

// virtualDefaultProbeCandidate resolves the exact bytes the chain may probe.
// An exact URI match against the listing the discovery run just persisted is
// preferred. A selected version may also use its own persisted row when the
// fresh listing dropped that profile variant: the target read authorized the
// row, and a neutral source covers every profile variant beneath its base path.
// A profile-specific source covers only its own group. The neutral group row
// itself is never admissible — it names no specific release and may resolve to
// a sibling candidate, so the play path's own probe handles it.
func virtualDefaultProbeCandidate(row *models.MediaFile, source *models.MediaFile, streams []VirtualPlaybackStream, allowPersistedSelected bool) *VirtualPlaybackStream {
	if row == nil || source == nil {
		return nil
	}
	target := strings.TrimSpace(row.FilePath)
	if target == "" {
		return nil
	}
	if source.FilePath == target {
		return nil
	}
	for i := range streams {
		if streams[i].URI == target && streams[i].OwnerInstallationID == row.VirtualOwnerInstallationID {
			return &streams[i]
		}
	}
	if !allowPersistedSelected {
		return nil
	}
	if virtualResultCandidateID(target) == "" {
		return nil
	}
	// A selected row can be a persisted candidate from an earlier provider
	// listing. The target read authorized its content, episode, owner, and
	// library; its own concrete result id names the bytes to probe. The
	// discovery source only supplies the scheduling hand-off, so a chain started
	// by a sibling source may still probe this row's stored URL. A neutral row
	// (no result id) is never probed speculatively; the default path keeps the
	// exact-listing requirement above.
	return virtualPlaybackStreamFromFile(row)
}

func virtualPlaybackStreamFromFile(row *models.MediaFile) *VirtualPlaybackStream {
	if row == nil {
		return nil
	}
	stream := &VirtualPlaybackStream{
		URI:                 row.FilePath,
		ProviderURL:         row.ResolvedURL,
		ProviderExpiresAt:   row.ResolvedURLExpiresAt,
		OwnerInstallationID: row.VirtualOwnerInstallationID,
		Resolution:          row.Resolution,
		CodecVideo:          row.CodecVideo,
		CodecAudio:          row.CodecAudio,
		Container:           row.Container,
		Bitrate:             row.Bitrate,
		FileSize:            row.FileSize,
		RequestHeaders:      maps.Clone(row.ProviderRequestHeaders),
	}
	for _, track := range row.AudioTracks {
		if track.Language != "" {
			stream.AudioLanguages = append(stream.AudioLanguages, track.Language)
		}
	}
	for _, track := range row.SubtitleTracks {
		if track.Language != "" {
			stream.SubtitleLanguages = append(stream.SubtitleLanguages, track.Language)
		}
	}
	return stream
}
