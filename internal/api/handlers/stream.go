package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/Silo-Server/silo-server/internal/activitylog"
	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/config"
	evt "github.com/Silo-Server/silo-server/internal/events"
	"github.com/Silo-Server/silo-server/internal/httpstream"
	"github.com/Silo-Server/silo-server/internal/logredact"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
	"github.com/Silo-Server/silo-server/internal/streamtoken"
	"github.com/Silo-Server/silo-server/internal/subtitles"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/altmount"
)

const (
	subtitleFormatASS  = "ass"
	subtitleFormatSSA  = "ssa"
	subtitleFormatSUP  = "sup"
	subtitleFormatSRT  = "srt"
	subtitleMIMESubRip = "application/x-subrip"
	// subtitleSourceUnavailableErrorCode is the subtitle-local resolve failure.
	// A subtitle sidecar/font request that cannot resolve its provider source is
	// a subtitle-delivery problem, never the playback transport's
	// virtual_resolve_failed: the client must not read it as the video source
	// having failed and move off the release.
	subtitleSourceUnavailableErrorCode = "subtitle_source_unavailable"
)

// FilePathResolver looks up a media file by its ID.
type FilePathResolver interface {
	GetByID(ctx context.Context, id int) (*models.MediaFile, error)
}

// StreamHandler handles HTTP endpoints for streaming media content.
type StreamHandler struct {
	sessionMgr    SessionManagerInterface
	fileResolver  FilePathResolver
	MissingMarker MissingFileMarker
	EventsHub     *evt.Hub
	AdminStore    PlaybackAdminStore
	SessionSyncer PlaybackSessionSyncer
	// TM is the shared transcode/reconstruct manager (same instance as the
	// PlaybackHandler's). It lets a direct/remux stream rebuild its playback
	// Session from the recipe card after a server restart instead of 404-ing.
	// May be nil (tests / minimal setups) — reconstruct is then simply off.
	TM *playback.TranscodeManager
	// JWTSecret verifies the stream token carried on the serve URL (?st=), which
	// is the reconstruction descriptor for direct/remux after a restart. Empty
	// disables token-based reconstruct (tests / minimal setups).
	JWTSecret string
	// StreamDeny is the shared session-deny marker (same instance as the
	// PlaybackHandler's). A denied session is answered 410 and never
	// reconstructed. Nil-safe: without Redis nothing is ever denied.
	StreamDeny *playback.StreamDeny
	// PlanStoreV3 is the shared attempt store (same instance as the
	// PlaybackHandler's); an aborted session's attempt row is marked stopped
	// through it. May be nil (tests / minimal setups).
	PlanStoreV3 playback.PlanStoreV3
	// SourceCommittedNotifier publishes the effective version a serve-layer
	// rotation just committed to, with its declared inventory, so a playing
	// client can follow the streamed release before any replan. Optional: a nil
	// notifier keeps the rotation commit but sends no event.
	SourceCommittedNotifier sourceCommittedNotifier
	// PlaybackConfig returns the current playback config; read it through
	// ffmpegPath(). May be nil (tests).
	PlaybackConfig func() config.PlaybackConfig
	// CopySafetyRacer gates and covers a revived progressive remux: it answers
	// whether this replica already condemns a video stream-copy of the source,
	// and re-engages the copy-safety race for one whose verdict is still open.
	// Optional — without it a revived remux is gated on the persisted row alone
	// and no race is started here.
	CopySafetyRacer PlaybackCopySafetyRacer
	// SubtitleCache stores complete embedded subtitle extracts under the transcode
	// dir so repeat selections skip the whole-file ffmpeg demux. May be nil
	// (tests / minimal setups) — extraction then always streams uncached.
	SubtitleCache *playback.SubtitleCache
	// subtitleWarmWG tracks handler-owned detached subtitle warms
	// (warmVirtualSubtitleAfterWindowMiss). They outlive the request by design —
	// the request's relay registration is released when the request ends, so the
	// handler starts its own warm — and a caller that owns the cache directory
	// (a test with a temp dir, a graceful drain) waits on this to keep the warm
	// from writing into a directory being torn down.
	subtitleWarmWG sync.WaitGroup
	SubtitleRepo   subtitles.Repository // optional; enables S3-sourced subtitles
	S3Client       subtitles.S3Client   // optional; needed for fetching S3 subtitles
	S3Bucket       string               // bucket for subtitle storage
	// VirtualMediaResolver resolves virtual:// URIs to a real provider URL.
	// Required for embedded subtitle extraction from virtual sources.
	VirtualMediaResolver         VirtualMediaResolver
	VirtualMediaRefreshResolver  VirtualMediaRefreshResolver
	VirtualMediaDetailedResolver VirtualMediaDetailedResolver
	// VirtualFileSaver/VirtualFileMetadataSaver persist a refreshed provider URL
	// through the existing Phase-1 write path when a candidate's stored URL has
	// expired. Nil disables the refresh; the resolve still lists as before.
	VirtualFileSaver         VirtualFileSaver
	VirtualFileMetadataSaver VirtualFileMetadataSaver
	// VirtualReleaseCacheStatus reports whether a pinned release already has a
	// cached copy in AltMount. known=false, or a nil probe, preserves today's
	// serve behavior: the session streams the provider source and no handoff is
	// attempted.
	VirtualReleaseCacheStatus VirtualReleaseCacheStatusFunc
	// VirtualReleaseCacheFiller starts (or joins) a provider-side cache fill for
	// an uncached pinned release. Pressing play on an uncached release calls it;
	// the provider fetches the media, Vio only serves the addon URL. Nil
	// disables play-triggered fills while leaving remote streaming intact.
	VirtualReleaseCacheFiller VirtualReleaseCacheFillFunc
	// DownloadProgress delivers download.progress events to a live session.
	// *playback.RealtimeHub implements it. Nil disables the push.
	DownloadProgress DownloadProgressPublisher
	// cacheWaiters indexes the live sessions streaming an uncached pinned
	// release, so the monitor's release-confirmed signal can hand each off to
	// its cached copy. See HandleVirtualReleaseConfirmed.
	cacheWaiters virtualCacheHandoffRegistry
	// RemoteStreamRelay pins the resolved provider URL to a loopback relay
	// so ffmpeg reads through it with a stable IP.
	RemoteStreamRelay *remotestream.Relay
	// AllowInsecureVirtual reports whether the owning plugin installation has
	// explicitly enabled allow_insecure_http for HTTP manifests on
	// private/local provider hosts.
	AllowInsecureVirtual func(installationID int) bool
	// AllowPrivateStreams reports whether the core
	// virtual_library.allow_private_streams opt-in permits virtual streams
	// from private/local network destinations.
	AllowPrivateStreams func(installationID int) bool
	// VirtualCandidateTrustWindow reports how long a persisted virtual
	// candidate row may be trusted for replay after its last listing or
	// resolution. A positive value keeps a delisted same-identity candidate
	// preferred (and retained) inside the window; zero disables the window and
	// keeps the pre-window behavior. Wired lazily from the settings store.
	VirtualCandidateTrustWindow func() time.Duration
	// VirtualCandidateFailMarker stamps a virtual candidate row as known-bad
	// after a transport produced no bytes, so the auto-pick skips it on the
	// next play while the dropdown still shows it for a manual retry. It is
	// fenced on the candidate identity the transport served (expectedFilePath)
	// and the failure state observed when it started (observedFailedAt): a row
	// rotated in place to a sibling is never stamped by a late failure of the
	// candidate the session actually served.
	VirtualCandidateFailMarker func(ctx context.Context, fileID int, expectedFilePath string, observedFailedAt *time.Time) error
	// VirtualCandidateRecoveredMarker clears a known-bad stamp after the
	// candidate actually delivered media bytes to a client — the only evidence
	// that forgives a transport failure. The callback is fenced on the
	// delivered candidate identity and the failure state observed when the
	// transport started, so a rotation or a newer failure is never cleared.
	// Metadata-only liveness checks resolve URLs without opening media and
	// must never clear it.
	VirtualCandidateRecoveredMarker func(ctx context.Context, fileID int, deliveredFilePath string, observedFailedAt *time.Time) error
	SubtitleBlobs                   subtitles.BlobStore // optional; backs downloaded subtitle reads
	// fontExtractFailures throttles the repetitive "subtitle font extraction
	// failed" warning to the first failure per file+track; repeats drop to
	// debug. A successful extraction clears the key, so a later regression warns
	// again. Font extraction never fails playback: the endpoint's 500
	// (font_extract_failed) stays the client-visible signal and the debug line
	// keeps the diagnostic trail.
	fontExtractFailures fontExtractFailLog
}

// fontExtractFailLog throttles the subtitle-font extraction warning. See the
// field doc on StreamHandler.fontExtractFailures.
type fontExtractFailLog struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

// failed logs the first failure for key at warn and repeats at debug.
func (l *fontExtractFailLog) failed(ctx context.Context, key string, attrs ...any) {
	l.mu.Lock()
	if l.seen == nil {
		l.seen = make(map[string]struct{})
	}
	_, repeat := l.seen[key]
	if !repeat {
		l.seen[key] = struct{}{}
	}
	l.mu.Unlock()
	args := append([]any{virtualEvidenceLogKeyComponent, virtualEvidenceLogValueAPI}, attrs...)
	if repeat {
		slog.DebugContext(ctx, "subtitle font extraction failed", args...)
		return
	}
	slog.WarnContext(ctx, "subtitle font extraction failed", args...)
}

// recovered clears the throttle key after a successful extraction so a later
// failure is reported at warn again.
func (l *fontExtractFailLog) recovered(key string) {
	l.mu.Lock()
	delete(l.seen, key)
	l.mu.Unlock()
}

// fontExtractFailureKey identifies an extraction target across retries.
func fontExtractFailureKey(fileID, trackIndex int) string {
	return strconv.Itoa(fileID) + ":" + strconv.Itoa(trackIndex)
}

// ffmpegPath returns the currently configured ffmpeg binary path.
func (h *StreamHandler) ffmpegPath() string {
	if h.PlaybackConfig != nil {
		return h.PlaybackConfig().FFmpegPath
	}
	return ""
}

// waitForBackgroundSubtitleWarms blocks until every handler-owned detached
// subtitle warm started so far has settled. A warm that was admitted by the
// cache can run for its full warm budget, so callers that own the cache
// directory should release any gate the warm is blocked on first. A nil
// receiver returns immediately.
func (h *StreamHandler) waitForBackgroundSubtitleWarms() {
	if h == nil {
		return
	}
	h.subtitleWarmWG.Wait()
}

// bindSessionVirtualSource returns a copy of a virtual file bound to the
// provider-neutral source captured by the playback session.
func bindSessionVirtualSource(file *models.MediaFile, session *playback.Session) *models.MediaFile {
	if file == nil || session == nil || session.VirtualSourceURI == "" || !isVirtualPlaybackFile(file) {
		return file
	}
	bound := *file
	bound.FilePath = session.VirtualSourceURI
	bound.VirtualOwnerInstallationID = session.VirtualSourceOwnerInstallationID
	return &bound
}

// virtualSessionSourceBinder binds a live session to a provider-neutral virtual
// candidate. *playback.SessionManager implements it; the narrow interface keeps
// the serve-layer rotation commit optional for minimal/test managers, which
// simply keep the binding captured at plan time.
type virtualSessionSourceBinder interface {
	SetVirtualSource(sessionID, virtualURI string, ownerInstallationID int) error
}

// sourceCommittedNotifier publishes the effective version a transport just
// committed to, with its declared inventory, to the live session. The
// PlaybackHandler implements it; a minimal/test setup without a notifier skips
// the push while keeping the rotation commit itself.
type sourceCommittedNotifier interface {
	PublishSourceCommitted(ctx context.Context, sessionID string)
}

// sourceCommittedPublishBudget bounds the detached publish of a committed
// source so a slow catalog read cannot outlive the stream far beyond the media
// response.
const sourceCommittedPublishBudget = 5 * time.Second

// commitRotatedVirtualSessionSource rebinds a live session to the candidate a
// serve-layer rotation just resolved. The session id and account are unchanged:
// only the pinned anchor moves, so the client-visible session identity survives
// while later serves and replans bind to the live release instead of the dead
// pin. Best-effort — a manager without SetVirtualSource keeps its prior binding,
// and the current request already holds a valid resolved URL.
func (h *StreamHandler) commitRotatedVirtualSessionSource(ctx context.Context, sessionID string, resolved ResolvedVirtualMedia) {
	if h == nil || resolved.URI == "" {
		return
	}
	binder, ok := h.sessionMgr.(virtualSessionSourceBinder)
	if !ok {
		return
	}
	if err := binder.SetVirtualSource(sessionID, resolved.URI, resolved.OwnerID); err != nil {
		slog.WarnContext(ctx, "failed to rebind virtual session after candidate rotation",
			"component", "api", "session", sessionID, "virtual_uri", resolved.URI, "error", err)
		return
	}
	h.afterVirtualSourceCommit(ctx, sessionID, resolved)
}

// afterVirtualSourceCommit performs the bookkeeping shared by every binding
// move: associate the session with the committed release's catalog row, then
// publish the new effective version. The plan's MediaFileID still names the
// release the plan was built with, so notifiers that look sessions up by the
// new file id (marker and subtitle events) would otherwise miss this session.
// The manager keeps RequestedMediaFileID untouched. The publish is detached: it
// reads the catalog for the new release's declared inventory and must not delay
// the media response that is about to serve.
func (h *StreamHandler) afterVirtualSourceCommit(ctx context.Context, sessionID string, resolved ResolvedVirtualMedia) {
	h.associateEffectiveMediaFile(ctx, sessionID, resolved.URI)
	h.publishSourceCommittedAsync(ctx, sessionID)
}

// virtualSessionGenerationBinder is the generation-fenced binding surface.
// *playback.SessionManager implements it; the narrow interface keeps the
// handoff optional for minimal/test managers.
type virtualSessionGenerationBinder interface {
	VirtualSourceBinding(sessionID string) (playback.VirtualSourceBindingSnapshot, error)
	SetVirtualSourceIfBinding(sessionID string, expected playback.VirtualSourceBindingSnapshot, virtualURI string, ownerInstallationID int, effectiveMediaFileID int) (uint64, bool, error)
	VirtualSourceGeneration(sessionID string) (uint64, error)
	SetVirtualSourceIfGeneration(sessionID string, expectedGeneration uint64, virtualURI string, ownerInstallationID int) (uint64, bool, error)
}

func (h *StreamHandler) virtualSourceBinding(sessionID string) (playback.VirtualSourceBindingSnapshot, bool) {
	if binder, ok := h.sessionMgr.(virtualSessionGenerationBinder); ok {
		if snap, err := binder.VirtualSourceBinding(sessionID); err == nil {
			return snap, true
		}
	}
	return playback.VirtualSourceBindingSnapshot{}, false
}

// commitHandoffVirtualSessionSource rebinds a session to the cached copy of the
// release it is already pinned to. When a binding snapshot was captured it is
// enforced, so a handoff that resolved its replacement before a newer binding
// move is a benign no-op rather than a clobber. It commits the new virtual URI,
// owner, and effective media file ID atomically under the session manager lock.
// Without a generation-capable manager it falls back to the best-effort rotation
// commit.
func (h *StreamHandler) commitHandoffVirtualSessionSource(ctx context.Context, sessionID string, expected playback.VirtualSourceBindingSnapshot, expectedSet bool, resolved ResolvedVirtualMedia) bool {
	if h == nil || resolved.URI == "" {
		return false
	}
	var effectiveFileID int
	if pathResolver, ok := h.fileResolver.(interface {
		GetByPath(context.Context, string) (*models.MediaFile, error)
	}); ok {
		if row, err := pathResolver.GetByPath(ctx, resolved.URI); err == nil && row != nil && row.ID > 0 {
			effectiveFileID = row.ID
		}
	}
	if binder, ok := h.sessionMgr.(virtualSessionGenerationBinder); ok && expectedSet {
		if _, applied, err := binder.SetVirtualSourceIfBinding(sessionID, expected, resolved.URI, resolved.OwnerID, effectiveFileID); err != nil {
			slog.WarnContext(ctx, "cache handoff: failed to rebind virtual session",
				"component", "api", "session", sessionID, "virtual_uri", resolved.URI, "error", err)
			return false
		} else if !applied {
			return false
		}
		h.publishSourceCommittedAsync(ctx, sessionID)
		return true
	}
	h.commitRotatedVirtualSessionSource(ctx, sessionID, resolved)
	return true
}

// VirtualReleaseCacheStatusFunc reports whether a release already has a cached
// copy, looked up by its provider-neutral virtual URI. known=false means the
// provider could not be consulted and the caller must not change behavior.
type VirtualReleaseCacheStatusFunc func(ctx context.Context, virtualURI string, ownerInstallationID int) (cached bool, known bool)

// VirtualReleaseCacheFillFunc starts (or joins) a provider-side cache fill for
// one pinned release. The implementation resolves and requests the addon URL;
// the handler never fetches media. report is called with progress until the fill
// completes or fails and must remain safe to call after the fill returns.
type VirtualReleaseCacheFillFunc func(ctx context.Context, virtualURI string, ownerInstallationID int, report func(playback.DownloadProgressPayload)) error

// FillVirtualReleaseCache adapts the function to a nil-checkable value.
func (f VirtualReleaseCacheFillFunc) FillVirtualReleaseCache(ctx context.Context, virtualURI string, ownerInstallationID int, report func(playback.DownloadProgressPayload)) error {
	if f == nil {
		return errors.New("virtual release cache filler is not configured")
	}
	return f(ctx, virtualURI, ownerInstallationID, report)
}

// DownloadProgressPublisher delivers download.progress events to a live
// playback session. *playback.RealtimeHub implements it. Nil disables the push.
type DownloadProgressPublisher interface {
	PublishDownloadProgress(sessionID string, payload playback.DownloadProgressPayload) bool
}

const (
	// virtualCacheFillBudget bounds a detached play-triggered cache fill so a
	// provider that never finishes cannot hold the goroutine forever. A fill
	// that outlives it is reported failed; playback already continued on the
	// remote source.
	virtualCacheFillBudget = 30 * time.Minute
	// virtualCacheHandoffBudget bounds one release-confirmed handoff reaction.
	virtualCacheHandoffBudget = 30 * time.Second
)

// virtualCacheHandoffWaiter is a live session streaming an uncached pinned
// release. It is registered at serve start and removed when the transport ends,
// so a later release-confirmation only reaches sessions still playing.
type virtualCacheHandoffWaiter struct {
	regID      uint64
	sessionID  string
	fileID     int
	releaseKey string
	ownerID    int
	binding    playback.VirtualSourceBindingSnapshot
	pinnedID   string
}

// virtualCacheHandoffRegistry indexes waiters by normalized release key. The
// zero value is usable; the map is created lazily. Each registration receives a
// unique registration ID so overlapping transports for the same session/release
// do not overwrite each other or delete a newer request's waiter on cleanup.
type virtualCacheHandoffRegistry struct {
	mu        sync.Mutex
	byRelease map[string]map[uint64]virtualCacheHandoffWaiter
	nextID    uint64
}

// register adds a waiter and returns a function that idempotently removes it.
// A waiter with no session or release key is not registered and its release
// function is a no-op.
func (r *virtualCacheHandoffRegistry) register(w virtualCacheHandoffWaiter) func() {
	if r == nil || w.sessionID == "" || w.releaseKey == "" {
		return func() {}
	}
	r.mu.Lock()
	if r.byRelease == nil {
		r.byRelease = make(map[string]map[uint64]virtualCacheHandoffWaiter)
	}
	r.nextID++
	id := r.nextID
	w.regID = id
	sessions := r.byRelease[w.releaseKey]
	if sessions == nil {
		sessions = make(map[uint64]virtualCacheHandoffWaiter)
		r.byRelease[w.releaseKey] = sessions
	}
	sessions[id] = w
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			current := r.byRelease[w.releaseKey]
			if current == nil {
				return
			}
			delete(current, id)
			if len(current) == 0 {
				delete(r.byRelease, w.releaseKey)
			}
		})
	}
}

// forRelease returns the waiters for a release key. The caller must not mutate
// the returned slice.
func (r *virtualCacheHandoffRegistry) forRelease(releaseKey string) []virtualCacheHandoffWaiter {
	if r == nil || releaseKey == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	sessions := r.byRelease[releaseKey]
	if len(sessions) == 0 {
		return nil
	}
	out := make([]virtualCacheHandoffWaiter, 0, len(sessions))
	for _, w := range sessions {
		out = append(out, w)
	}
	return out
}

// virtualHandoffReleaseKey returns the normalized release identity that keys the
// cache-handoff registry for a serving session. A row without a persisted
// release name cannot be matched to a provider completion signal, so it yields
// "" and no handoff is attempted.
func virtualHandoffReleaseKey(file *models.MediaFile, resolved ResolvedVirtualMedia) string {
	if file != nil {
		if key := altmount.ReleaseKey(file.ProviderReleaseName); key != "" {
			return key
		}
	}
	return altmount.ReleaseKey(resolved.ProviderReleaseName)
}

// sameVirtualReleaseCandidate reports whether a resolved candidate id names the
// same release the session is pinned to. An empty id on either side cannot be
// compared, so it is not a match: a neutral row (no concrete pin) or a resolver
// that reports no candidate id must not hand off, because a force-refresh
// resolve ranks candidates anew and could substitute a ranked candidate from
// another release under the session binding.
func sameVirtualReleaseCandidate(pinnedID, resolvedID string) bool {
	return pinnedID != "" && resolvedID != "" && pinnedID == resolvedID
}

// handoffVirtualSessionToCached re-resolves the exact release a session is
// pinned to and rebinds the session to the cached copy. cachedConfirmed lets the
// release-confirmed path skip the status probe it already answered. A resolve
// that names a different release is refused so a cache handoff can never
// silently switch releases, and a resolve that cannot be tied to a concrete
// pinned candidate id is refused rather than trusted; a stale binding
// generation is a benign no-op. The returned cleanup must be released by the
// caller when non-nil.
func (h *StreamHandler) handoffVirtualSessionToCached(ctx context.Context, session *playback.Session, file *models.MediaFile, cachedConfirmed bool) (ResolvedVirtualMedia, func(), bool, error) {
	snap, snapSet := h.virtualSourceBinding(session.ID)
	return h.handoffVirtualSessionToCachedWithSnapshot(ctx, session, file, snap, snapSet, cachedConfirmed)
}

func (h *StreamHandler) handoffVirtualSessionToCachedWithSnapshot(ctx context.Context, session *playback.Session, file *models.MediaFile, snap playback.VirtualSourceBindingSnapshot, snapSet bool, cachedConfirmed bool) (ResolvedVirtualMedia, func(), bool, error) {
	if h == nil || session == nil || file == nil || !isVirtualPlaybackFile(file) || !hasVirtualMediaResolver(h) {
		return ResolvedVirtualMedia{}, nil, false, nil
	}
	if !cachedConfirmed {
		if h.VirtualReleaseCacheStatus == nil {
			// Without a cache probe there is no way to tell the cached copy
			// from the remote one, so today's behavior is preserved.
			return ResolvedVirtualMedia{}, nil, false, nil
		}
		if cached, known := h.VirtualReleaseCacheStatus(ctx, file.FilePath, effectiveVirtualOwner(0, file.VirtualOwnerInstallationID)); !known || !cached {
			return ResolvedVirtualMedia{}, nil, false, nil
		}
	}
	pinnedID := virtualResultCandidateID(file.FilePath)
	// forceRefresh lists afresh so the provider reports its now-cached URL; a
	// plain re-resolve could serve the stored (remote) URL again.
	resolved, cleanup, err := h.resolveVirtualInputURIExcluding(ctx, file, session.UserID, session.ProfileID, true, nil, false)
	if err != nil {
		return ResolvedVirtualMedia{}, nil, false, err
	}
	if !sameVirtualReleaseCandidate(pinnedID, resolved.CandidateID) {
		if cleanup != nil {
			cleanup()
		}
		if pinnedID == "" || resolved.CandidateID == "" {
			return ResolvedVirtualMedia{}, nil, false, fmt.Errorf("cache handoff cannot verify the pinned release: pinned candidate %q, resolved candidate %q; refusing an unverifiable handoff", pinnedID, resolved.CandidateID)
		}
		return ResolvedVirtualMedia{}, nil, false, fmt.Errorf("cache handoff resolved candidate %q for pinned candidate %q; refusing a release swap", resolved.CandidateID, pinnedID)
	}
	if !h.commitHandoffVirtualSessionSource(ctx, session.ID, snap, snapSet, resolved) {
		if cleanup != nil {
			cleanup()
		}
		return ResolvedVirtualMedia{}, nil, false, nil
	}
	return resolved, cleanup, true, nil
}

// HandleVirtualReleaseConfirmed reacts to AltMount first reporting a release
// completed. It finds live sessions still streaming that release and hands each
// off to the cached copy, preserving the session's virtual pin and stream
// timeline. It matches the altmount.ReleaseConfirmationObserver signature so a
// monitor can be wired to it directly. Best-effort and bounded: an unknown
// release, a session that already moved on, or a resolve failure is logged and
// never disrupts playback.
//
// The observer contract requires a quick, non-blocking callback (see
// altmount.ReleaseConfirmationObserver): the caller is a classification or
// refresh on the serve path. Every handoff resolves the cached copy under a
// network budget, so the work is dispatched to its own goroutine and the
// callback returns immediately.
func (h *StreamHandler) HandleVirtualReleaseConfirmed(releaseKey string) {
	if h == nil || strings.TrimSpace(releaseKey) == "" {
		return
	}
	go h.handoffConfirmedRelease(releaseKey)
}

// handoffConfirmedRelease performs the bounded handoff work for one confirmed
// release. It owns its context so the caller's return cannot cancel it.
func (h *StreamHandler) handoffConfirmedRelease(releaseKey string) {
	ctx, cancel := context.WithTimeout(context.Background(), virtualCacheHandoffBudget)
	defer cancel()
	for _, waiter := range h.cacheWaiters.forRelease(releaseKey) {
		session, err := h.sessionMgr.GetSession(waiter.sessionID)
		if err != nil || session == nil {
			continue
		}
		// Bound to the release that registered the waiter: if the session has
		// moved to another candidate, was replaced, or replanned, its binding
		// generation or identity will have changed. Refuse the handoff and do
		// not attribute completion of releaseKey to this session.
		currentBinding, hasBinder := h.virtualSourceBinding(waiter.sessionID)
		if hasBinder {
			if currentBinding.Generation != waiter.binding.Generation ||
				strings.TrimSpace(currentBinding.VirtualURI) != strings.TrimSpace(waiter.binding.VirtualURI) ||
				currentBinding.OwnerInstallationID != waiter.binding.OwnerInstallationID ||
				(waiter.binding.EffectiveMediaFileID > 0 && currentBinding.EffectiveMediaFileID != waiter.binding.EffectiveMediaFileID) {
				continue
			}
		}
		file, err := h.fileResolver.GetByID(ctx, waiter.fileID)
		if err != nil || file == nil {
			continue
		}
		file = bindSessionVirtualSource(file, session)
		// Ensure the file's current pinned candidate still matches the waiter.
		if waiter.pinnedID != "" && virtualResultCandidateID(file.FilePath) != waiter.pinnedID {
			continue
		}
		resolved, cleanup, applied, handoffErr := h.handoffVirtualSessionToCachedWithSnapshot(ctx, session, file, waiter.binding, hasBinder, true)
		if handoffErr != nil {
			slog.WarnContext(ctx, "cache handoff failed for confirmed release",
				"component", "api", "session", waiter.sessionID, "file_id", waiter.fileID,
				"release_key", releaseKey, "status", "handoff_failed",
				"error", logredact.SanitizeURLError(handoffErr))
			continue
		}
		if applied {
			if cleanup != nil {
				cleanup()
			}
			h.reportDownloadProgress(waiter.sessionID, waiter.fileID, releaseKey, playback.DownloadProgressPayload{
				State:   playback.DownloadProgressStateCompleted,
				Message: "cached copy is now servable",
			})
			slog.InfoContext(ctx, "cache handoff rebound session to the cached copy",
				"component", "api", "session", waiter.sessionID, "file_id", waiter.fileID,
				"release_key", releaseKey, "status", "handed_off", "virtual_uri", resolved.URI)
		}
	}
}

// beginVirtualCacheHandoff registers a session streaming a pinned release for a
// later release-confirmation handoff and, when a filler is wired, asks the
// provider to start a cache fill while streaming download.progress. It returns a
// release function the caller defers so the registration is dropped when the
// transport ends. The fill is detached and bounded: the caller keeps streaming
// the remote source and never blocks on it, and a fill that cannot start is
// reported failed rather than hanging.
func (h *StreamHandler) beginVirtualCacheHandoff(ctx context.Context, session *playback.Session, file *models.MediaFile, resolved ResolvedVirtualMedia) func() {
	if h == nil || session == nil || file == nil {
		return func() {}
	}
	releaseKey := virtualHandoffReleaseKey(file, resolved)
	if releaseKey == "" {
		return func() {}
	}
	ownerID := effectiveVirtualOwner(resolved.OwnerID, file.VirtualOwnerInstallationID)
	snap, _ := h.virtualSourceBinding(session.ID)
	release := h.cacheWaiters.register(virtualCacheHandoffWaiter{
		sessionID:  session.ID,
		fileID:     file.ID,
		releaseKey: releaseKey,
		ownerID:    ownerID,
		binding:    snap,
		pinnedID:   virtualResultCandidateID(file.FilePath),
	})
	if h.VirtualReleaseCacheFiller == nil {
		return release
	}
	filler := h.VirtualReleaseCacheFiller
	sessionID, fileID := session.ID, file.ID
	virtualURI := resolved.URI
	go func() {
		fillCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), virtualCacheFillBudget)
		defer cancel()
		err := filler.FillVirtualReleaseCache(fillCtx, virtualURI, ownerID, func(progress playback.DownloadProgressPayload) {
			h.reportDownloadProgress(sessionID, fileID, releaseKey, progress)
		})
		if err != nil {
			h.reportDownloadProgress(sessionID, fileID, releaseKey, playback.DownloadProgressPayload{
				State:   playback.DownloadProgressStateFailed,
				Message: "cache fill could not start; continuing from the provider",
			})
		}
	}()
	return release
}

// reportDownloadProgress sends one download.progress event, best-effort.
func (h *StreamHandler) reportDownloadProgress(sessionID string, fileID int, releaseKey string, progress playback.DownloadProgressPayload) {
	if h == nil || h.DownloadProgress == nil || sessionID == "" {
		return
	}
	progress.FileID = fileID
	if progress.ReleaseID == "" {
		progress.ReleaseID = releaseKey
	}
	h.DownloadProgress.PublishDownloadProgress(sessionID, progress)
}

// associateEffectiveMediaFile points the live session at the catalog row of the
// candidate a rotation just bound, preserving its requested file selection.
func (h *StreamHandler) associateEffectiveMediaFile(ctx context.Context, sessionID, candidateURI string) {
	if h == nil || h.sessionMgr == nil || strings.TrimSpace(candidateURI) == "" {
		return
	}
	pathResolver, ok := h.fileResolver.(interface {
		GetByPath(context.Context, string) (*models.MediaFile, error)
	})
	if !ok {
		return
	}
	row, err := pathResolver.GetByPath(ctx, candidateURI)
	if err != nil || row == nil || row.ID <= 0 {
		return
	}
	if err := h.sessionMgr.SetEffectiveMediaFileID(sessionID, row.ID); err != nil {
		slog.WarnContext(ctx, "failed to associate virtual session with the rotated release row",
			"component", "api", "session", sessionID, "virtual_uri", candidateURI, "file_id", row.ID, "error", err)
	}
}

// publishSourceCommittedAsync delivers the committed-source event on a detached
// five-second context. A missing notifier is a no-op.
func (h *StreamHandler) publishSourceCommittedAsync(ctx context.Context, sessionID string) {
	if h == nil || h.SourceCommittedNotifier == nil {
		return
	}
	notifier := h.SourceCommittedNotifier
	bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sourceCommittedPublishBudget)
	go func() {
		defer cancel()
		notifier.PublishSourceCommitted(bgCtx, sessionID)
	}()
}

// bindSessionVirtualSourceWithTracks binds the session's virtual source and
// prefers the subtitle evidence captured at plan time. The catalog row is
// mutable: candidate rotation re-probes it and can replace its subtitle
// tracks after this session planned against a specific release. Pinned
// subtitle URLs name plan-time ordinals/stream indices, so the extraction
// must use the evidence the plan promised, not whatever the row holds now.
//
// Carried evidence is only trustworthy while it still belongs to the candidate
// actually being served. A serve-layer rotation can move the session binding
// without moving the catalog row's subtitle inventory, and the planner can
// replan onto a different file; in both cases the carried inventory describes
// the previous release, so applying it would silently serve the wrong tracks.
// The evidence URI records the candidate it was captured for: when the bound
// file no longer names that candidate, the evidence is discarded and the bound
// file's own catalog tracks are served instead.
func bindSessionVirtualSourceWithTracks(ctx context.Context, file *models.MediaFile, session *playback.Session, resolver FilePathResolver) *models.MediaFile {
	bound := bindSessionVirtualSource(file, session)
	if bound == nil || !isVirtualPlaybackFile(bound) {
		return bound
	}

	// The evidence-set flag, not slice length, is the signal. A candidate probed
	// with no subtitle tracks is captured-empty evidence, and it is still
	// authoritative: the plan promised this release has none, so serving the
	// row's later-populated inventory would serve a track the client never
	// selected. Absent evidence (the flag unset) is the only case with nothing
	// to apply, and the only case the live row may speak for.
	hasEvidence := session.VirtualSubtitleEvidenceSet
	if hasEvidence && !virtualEvidenceMatchesBoundFile(bound, session) {
		slog.WarnContext(ctx, "virtual session track evidence belongs to a different candidate; using the bound file's tracks",
			"component", "api",
			"session", session.ID,
			"file_id", file.ID,
			"evidence_uri", session.VirtualSubtitleEvidenceURI,
			"file_path", bound.FilePath)
		hasEvidence = false
	}
	if hasEvidence {
		boundCopy := *bound
		boundCopy.SubtitleTracks = session.VirtualSubtitleTracks
		boundCopy.ExternalSubtitles = session.VirtualExternalSubtitles
		return &boundCopy
	}

	// No applicable session evidence (reconstructed session, or evidence for a
	// previous candidate): fall back to the live candidate row when the bound
	// file only carries provider-declared placeholders, mirroring the
	// historical behavior.
	if resolver == nil || hasUsableSubtitleTracks(bound) {
		return bound
	}

	var candidate *models.MediaFile
	if session.MediaFileID > 0 && session.MediaFileID != file.ID {
		candidate, _ = resolver.GetByID(ctx, session.MediaFileID)
	}
	if (candidate == nil || !hasUsableSubtitleTracks(candidate)) && session.VirtualSourceURI != "" {
		if pathResolver, ok := resolver.(interface {
			GetByPath(context.Context, string) (*models.MediaFile, error)
		}); ok {
			candidate, _ = pathResolver.GetByPath(ctx, session.VirtualSourceURI)
		}
	}
	if candidate != nil && hasUsableSubtitleTracks(candidate) {
		boundCopy := *bound
		boundCopy.SubtitleTracks = candidate.SubtitleTracks
		if len(boundCopy.ExternalSubtitles) == 0 {
			boundCopy.ExternalSubtitles = candidate.ExternalSubtitles
		}
		return &boundCopy
	}

	return bound
}

// virtualEvidenceMatchesBoundFile reports whether the session's carried virtual
// subtitle evidence was captured for the candidate the bound file names. The
// evidence URI is authoritative when present. Sessions created before that field
// existed fall back to the plan-time binding: evidence captured while the file
// path matched the session's virtual URI still belongs to the bound file.
func virtualEvidenceMatchesBoundFile(bound *models.MediaFile, session *playback.Session) bool {
	if session == nil || bound == nil {
		return false
	}
	evidenceURI := strings.TrimSpace(session.VirtualSubtitleEvidenceURI)
	if evidenceURI == "" {
		// Legacy sessions predate the provenance field. The only binding the
		// evidence had was the session's virtual URI, so it still matches when
		// the bound file names that same candidate.
		return session.VirtualSourceURI != "" && sameVirtualCandidate(session.VirtualSourceURI, bound.FilePath)
	}
	return sameVirtualCandidate(evidenceURI, bound.FilePath)
}

// sameVirtualCandidate reports whether two provider-neutral virtual paths name
// the same candidate. Exact equality is sufficient across the codebase; the
// result-id comparison additionally tolerates a URI whose non-result query
// parameters were re-encoded (`withVirtualResultKey` preserves the rest).
func sameVirtualCandidate(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == b {
		return true
	}
	if a == "" || b == "" {
		return false
	}
	aID, bID := virtualResultCandidateID(a), virtualResultCandidateID(b)
	if aID == "" || bID == "" || aID != bID {
		return false
	}
	return virtualPlaybackNeutralKey(a) == virtualPlaybackNeutralKey(b)
}

// hasUsableSubtitleTracks reports whether a file carries embedded subtitle
// tracks with real codec evidence. Provider-declared language placeholders
// (Index 0, no Codec, no ContainerTrackID) are not usable for extraction:
// the stream handler would pick the wrong output format and ffmpeg would
// fail against the real provider stream.
func hasUsableSubtitleTracks(file *models.MediaFile) bool {
	if file == nil {
		return false
	}
	for _, track := range file.SubtitleTracks {
		if strings.TrimSpace(track.Codec) != "" {
			return true
		}
	}
	return false
}

func resolvedVirtualCandidatePath(resolved ResolvedVirtualMedia) string {
	uri := strings.TrimSpace(resolved.URI)
	id := virtualResultCandidateID(uri)
	if !strings.HasPrefix(uri, "virtual://") || id == "" || resolved.CandidateID == "" || resolved.CandidateID != id {
		return ""
	}
	return uri
}

func hasVirtualMediaResolver(h *StreamHandler) bool {
	return h != nil && (h.VirtualMediaResolver != nil || h.VirtualMediaDetailedResolver != nil || h.VirtualMediaRefreshResolver != nil)
}

func (h *StreamHandler) resolveVirtualInputURI(
	ctx context.Context,
	file *models.MediaFile,
	userID int,
	profileID string,
	forceRefresh bool,
) (ResolvedVirtualMedia, func(), error) {
	// A plain re-resolve indicts nothing, so it must not authorize a release
	// substitution.
	return h.resolveVirtualInputURIExcluding(ctx, file, userID, profileID, forceRefresh, nil, false)
}

// resolveVirtualInputURIExcluding resolves a virtual input, optionally
// excluding a failed candidate so the next-ranked release is tried. The
// excluded candidate ID is threaded into the detailed resolver, which re-lists
// and skips it (see plugins.ResolveVirtualPlaybackDetailedWithRouting).
//
// rotateCandidates declares whether the exclusion is the serve layer's own
// indictment of that release — a delivery that produced no bytes, a decode
// rejection, or a dead candidate — which is what authorizes substituting a
// sibling. It is explicit, not inferred from a non-empty exclusion list: an
// exclusion alone is not a verdict, so a caller that excludes a candidate for
// any other reason must pass false and let the resolver keep refusing a silent
// release swap.
func (h *StreamHandler) resolveVirtualInputURIExcluding(
	ctx context.Context,
	file *models.MediaFile,
	userID int,
	profileID string,
	forceRefresh bool,
	excludedCandidateIDs []string,
	rotateCandidates bool,
) (ResolvedVirtualMedia, func(), error) {
	resolved := ResolvedVirtualMedia{}
	var err error
	// Stored-URL shortcut, mirroring the transport resolver: a session-bound
	// re-resolve of a pinned candidate serves the row's own persisted URL when
	// it is present, unexpired and passes the resolver's URL validator. The
	// row is re-read by the exact candidate path so a row bound to a different
	// virtual source cannot contribute its stored URL. forceRefresh (a
	// failover retry) and an exclusion list always list afresh.
	var storedExpiredRow *models.MediaFile
	if !forceRefresh && len(excludedCandidateIDs) == 0 {
		if usable, row, state := h.lookupStoredVirtualURLCandidate(ctx, file.FilePath, file.VirtualOwnerInstallationID); state == virtualStoredURLUsable {
			resolved = usable
		} else if state == virtualStoredURLExpiredWithinWindow || state == virtualStoredURLExpired {
			// An expired stored URL is never served. Inside the window the
			// refresh below is also allowed to keep the same candidate; outside
			// it today's refresh-or-rotate behavior is unchanged.
			storedExpiredRow = row
		}
	}
	// The serve-layer row carries the durable identity the same-release
	// re-match needs; a row with none (legacy) is left untouched. Inside the
	// trust window the row is also marked trusted so a delisted same-identity
	// candidate is not reported as absent (which would rotate to a sibling).
	ctx = virtualResolveContextWithPersistedTrust(ctx, file, time.Now(), h.virtualStoredURLTrustWindow())
	if resolved.URL == "" {
		if h.VirtualMediaDetailedResolver != nil {
			// The caller declares whether excluding the candidate indicted the
			// release; a display-driven same-file re-plan never does.
			ctx = withVirtualCandidateRotationV3(ctx, rotateCandidates)
			// The session-binding intent travels on the context and is
			// declared by the caller: absent means session-bound, the
			// conservative default that refuses a profile-removed or absent
			// pin instead of silently swapping the release. The serve layer
			// always re-resolves a release an existing session serves, so it
			// leaves the default in place; a fresh selection that does not
			// serve a session declares false (withVirtualSessionBindingV3) and
			// falls through to a profile-satisfying sibling.
			// A transient provider-listing blackout for the session's own
			// trusted candidate must not be read as an indictment of the
			// release. Retry it with a short bounded backoff before giving up,
			// and classify the final failure as a retryable provider outage so
			// the client keeps retrying the release it picked. The retry keeps
			// the caller's exact parameters (exclusions, forceRefresh), so an
			// intentional rotation still rotates on the first successful
			// relist; only a failed listing is retried.
			outageTrusted := h.virtualCandidateTrustedForOutageRetry(file)
			resolved, err = retryVirtualProviderOutageResolve(ctx, outageTrusted, func(retryCtx context.Context, relist bool) (ResolvedVirtualMedia, error) {
				if relist {
					retryCtx = virtuallibrary.WithProviderOutageRelist(retryCtx)
				}
				return h.VirtualMediaDetailedResolver.ResolveVirtualMediaDetailed(
					retryCtx, file.FilePath, file.VirtualOwnerInstallationID, userID, profileID, forceRefresh || relist, excludedCandidateIDs, "",
				)
			})
			if err == nil && resolved.IdentityRematched {
				// Same release, new provider id: adopt the new ?result= and the
				// resolution's identity under the existing CAS/fence write.
				adoptRematchedVirtualResolution(ctx, file, resolved, h.VirtualFileMetadataSaver, h.VirtualFileSaver)
			} else if err == nil && storedExpiredRow != nil {
				// Same best-effort contract as the transport resolver: the URL
				// is already in hand, so persistence must not fail the serve.
				_, _ = refreshStoredVirtualResolution(ctx, storedExpiredRow, resolved, h.VirtualFileMetadataSaver, h.VirtualFileSaver)
			} else if err != nil {
				err = classifyVirtualProviderOutage(err, outageTrusted)
			}
		} else if forceRefresh && h.VirtualMediaRefreshResolver != nil {
			resolved.URL, err = h.VirtualMediaRefreshResolver.RefreshVirtualMedia(
				ctx, file.FilePath, file.VirtualOwnerInstallationID, userID, profileID,
			)
		} else {
			resolved.URL, err = resolveVirtualMediaPath(
				ctx, h.VirtualMediaResolver, file.FilePath,
				file.VirtualOwnerInstallationID, userID, profileID,
			)
		}
	}
	if err != nil {
		return ResolvedVirtualMedia{}, nil, fmt.Errorf("resolve virtual input: %w", err)
	}
	if h.RemoteStreamRelay == nil {
		return resolved, func() {}, nil
	}
	var relayURL string
	var cleanup func()
	ownerID := effectiveVirtualOwner(resolved.OwnerID, file.VirtualOwnerInstallationID)
	insecure := h.AllowPrivateStreams != nil && h.AllowPrivateStreams(ownerID)
	// A serve-path retry that follows a transient relay/edge failure sets the
	// fresh-registration marker (see withVirtualRelayFreshRegistration): it must
	// present a newly minted relay entry instead of reusing the live token whose
	// upstream just answered 502, mirroring the remux seek-anchor retry. Every
	// other caller keeps the content-key reuse that shares one upstream and one
	// range-cache scope.
	fresh := virtualRelayFreshRegistrationRequested(ctx)
	switch {
	case insecure && fresh:
		relayURL, cleanup, err = h.RemoteStreamRelay.RegisterInsecureWithHeadersFresh(ctx, resolved.URL, resolved.RequestHeaders)
	case insecure:
		relayURL, cleanup, err = h.RemoteStreamRelay.RegisterInsecureWithHeaders(ctx, resolved.URL, resolved.RequestHeaders)
	case fresh:
		relayURL, cleanup, err = h.RemoteStreamRelay.RegisterWithHeadersFresh(ctx, resolved.URL, resolved.RequestHeaders)
	default:
		relayURL, cleanup, err = h.RemoteStreamRelay.RegisterWithHeaders(ctx, resolved.URL, resolved.RequestHeaders)
	}
	if err != nil {
		return ResolvedVirtualMedia{}, nil, err
	}
	resolved.URL = relayURL
	return resolved, cleanup, nil
}

// NewStreamHandler creates a new StreamHandler backed by the given session
// manager and file resolver.
func NewStreamHandler(sessionMgr SessionManagerInterface, fileResolver FilePathResolver) *StreamHandler {
	return &StreamHandler{
		sessionMgr:   sessionMgr,
		fileResolver: fileResolver,
		// A bare manager (no recipe store) behaves as "no reconstruct" — plain
		// GetSession + ownership — so HandleStream has a single code path. The
		// router overwrites this with the shared manager to enable reconstruct.
		TM: playback.NewTranscodeManager(),
	}
}

// HandleStream serves the video stream for a playback session.
// For direct play: serves the file with HTTP byte-range support.
// For remux: starts an ffmpeg remux and streams the output.
// For transcode: returns 400 (transcode uses manifest/segment endpoints).
func (h *StreamHandler) HandleStream(w http.ResponseWriter, r *http.Request) {
	requestStart := time.Now()
	userID := apimw.GetUserID(r.Context())
	if userID == 0 {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}

	sessionID := chi.URLParam(r, "session_id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Session ID is required")
		return
	}
	setPlaybackSessionLogContext(r, sessionID)
	if h.StreamDeny.Denied(r.Context(), sessionID) {
		writePlaybackSessionEnded(w)
		return
	}

	// Look up the session, reconstructing it from the recipe card on a not-found
	// miss (e.g. after a server restart) so a direct/remux stream resumes instead
	// of 404-ing. The client re-supplies its position (HTTP Range for direct, the
	// ?seek= query for remux), so no runtime beyond the Session needs rebuilding.
	// Without a token (or signing secret) reconstruct is off, collapsing to a
	// plain GetSession + ownership check.
	card, claims := verifiedStreamCardFromToken(r.URL.Query().Get(streamTokenParam), sessionID, h.JWTSecret)
	loadCard := card
	if _, err := h.sessionMgr.GetSession(sessionID); err == nil {
		// A live route may have been replanned since this token was issued. Do not
		// let stale recipe routing override the current session, and do not revive
		// the stale recipe if the live session disappears during this request.
		loadCard = nil
	} else if errors.Is(err, playback.ErrSessionNotFound) && !requireNativeRecipeAPIEgressV3(w, card) {
		return
	} else if err != nil && !errors.Is(err, playback.ErrSessionNotFound) {
		// Do not turn an inconsistent backend read into authority to reconstruct
		// from a recipe whose route was never checked against a clean miss.
		loadCard = nil
	}
	session, status, reconstructed, sessionErr := h.TM.LoadOrReconstructSessionDetailWithError(r.Context(), h.sessionMgr.GetSession, sessionID, userID, loadCard)
	switch status {
	case playback.SessionMissing:
		writePlaybackSessionNotFound(w)
		return
	case playback.SessionLoadFailed:
		writeErrorCause(w, http.StatusInternalServerError, "internal_error", "Failed to load playback session", sessionErr)
		return
	case playback.SessionForbidden:
		writeError(w, http.StatusForbidden, "forbidden", "Session belongs to another user")
		return
	}
	if !requireNativeSessionAPIEgressV3(w, session) {
		return
	}
	if !requireOwningProfile(w, r, session) {
		return
	}

	file, err := h.fileResolver.GetByID(r.Context(), session.MediaFileID)
	if err != nil {
		if isPlaybackFileLookupMissing(err) {
			h.abortPlaybackSession(r.Context(), session)
			writeError(w, http.StatusNotFound, "not_found", "Media file not found")
			return
		}
		writeErrorCause(w, http.StatusInternalServerError, "internal_error", "Failed to load media file", err)
		return
	}
	if file == nil {
		h.abortPlaybackSession(r.Context(), session)
		writeError(w, http.StatusNotFound, "not_found", "Media file not found")
		return
	}
	if err := preflightPlaybackFile(r.Context(), file, h.MissingMarker, h.EventsHub); err != nil {
		if isPlaybackFileMissing(err) {
			h.abortPlaybackSession(r.Context(), session)
		}
		writePlaybackFilePreflightError(w, err)
		return
	}
	attachPlaybackSession(r.Context(), session, claims)

	if reconstructed && session.PlayMethod == playback.PlayRemux &&
		videoCopyRevivalRefused(r.Context(), h.CopySafetyRacer, file, sessionID) {
		h.abortPlaybackSession(r.Context(), session)
		writePlaybackSessionNotFound(w)
		return
	}

	// Bind to the session's planned virtual URI when available: the catalog
	// row's path is mutable (candidate rotation), but the session captured
	// the exact URI that was resolved and probed during planning.
	file = bindSessionVirtualSource(file, session)

	// Capture the delivered identity and observed health state at transport
	// start: the delivered candidate is the path the transport will serve
	// (retained by the session even if the row rotates mid-stream), and the
	// failure stamp seen now is the only health state a successful delivery
	// may clear. A rotation to B or a newer failure on A that lands while the
	// stream is being served is preserved.
	virtualObservedFailedAt := (*time.Time)(nil)
	if file != nil && isVirtualPlaybackFile(file) {
		if current, err := h.fileResolver.GetByID(r.Context(), file.ID); err == nil && current != nil {
			virtualObservedFailedAt = current.FailedAt
		}
	}

	inputPath := file.FilePath
	deliveredPath := ""
	releaseInput := func() {}
	// releaseCacheHandoff drops the session's cache-handoff registration when
	// the transport ends. It is a no-op for a non-virtual source.
	releaseCacheHandoff := func() {}
	if isVirtualPlaybackFile(file) && hasVirtualMediaResolver(h) {
		resolved, cleanup, resolveErr := h.resolveVirtualInputURI(r.Context(), file, session.UserID, session.ProfileID, false)
		if resolveErr != nil && (errors.Is(resolveErr, virtuallibrary.ErrSessionBoundCandidateAbsent) || errors.Is(resolveErr, ErrVirtualCandidateMarkedFailed)) {
			// The session's pinned release is absent from the provider's current
			// list (it renumbered or dropped the result id). Mirror the transcode
			// startup loop: relist fresh, exclude the absent pin, and declare the
			// exclusion a verdict so the resolver may serve a live sibling. The
			// session identity is preserved — only the anchor rotates.
			pinnedID := virtualResultCandidateID(file.FilePath)
			excluded := []string(nil)
			if pinnedID != "" {
				excluded = []string{pinnedID}
			}
			rotated, rotatedCleanup, rotateErr := h.resolveVirtualInputURIExcluding(
				r.Context(), file, session.UserID, session.ProfileID, true, excluded, true,
			)
			if rotateErr == nil {
				slog.InfoContext(r.Context(), "virtual stream rotated an absent session-bound candidate",
					"component", "api", "session", sessionID, "file_id", file.ID,
					"status", "rotated", "old_candidate_id", pinnedID,
					"new_candidate_id", rotated.CandidateID, "virtual_uri", rotated.URI)
				resolved, cleanup, resolveErr = rotated, rotatedCleanup, nil
				h.commitRotatedVirtualSessionSource(r.Context(), sessionID, rotated)
			} else {
				slog.WarnContext(r.Context(), "virtual stream candidate rotation failed",
					"component", "api", "session", sessionID, "file_id", file.ID,
					"status", "rotation_failed", "old_candidate_id", pinnedID,
					"error", logredact.SanitizeURLError(rotateErr))
				resolveErr = rotateErr
			}
		}
		if resolveErr != nil {
			logVirtualStreamFailure(r.Context(), sessionID, file, resolveErr)
			// A provider resolve failure is a dependency problem, not an
			// internal error. A transient provider-listing blackout for the
			// session's own trusted candidate is answered 503
			// provider_unavailable (retryable) so hls.js keeps retrying the
			// release the viewer picked instead of rotating; every other
			// resolve failure keeps the v1 502 that the v2 byte-delivery
			// adapter maps to a retryable dependency_unavailable.
			if errors.Is(resolveErr, errVirtualProviderUnavailable) {
				writeErrorCause(w, http.StatusServiceUnavailable, "provider_unavailable", "The virtual source provider is temporarily unavailable", resolveErr)
				return
			}
			writeErrorCause(w, http.StatusBadGateway, "virtual_resolve_failed", "Failed to resolve virtual source", resolveErr)
			return
		}
		inputPath = resolved.URL
		deliveredPath = resolvedVirtualCandidatePath(resolved)
		releaseInput = cleanup
		// A pinned release streaming from the provider is a candidate for a
		// cache handoff: pressing play asks the provider to fill its cache (Vio
		// never fetches the media) and, when the fill completes, the session is
		// rebound to the cached copy without changing the release.
		releaseCacheHandoff = h.beginVirtualCacheHandoff(r.Context(), session, file, resolved)
	}
	defer releaseCacheHandoff()
	defer func() {
		if releaseInput != nil {
			releaseInput()
		}
	}()

	switch session.PlayMethod {
	case playback.PlayDirect:
		if err := h.sessionMgr.BeginTransport(sessionID); err == nil {
			defer func() {
				_ = h.sessionMgr.EndTransport(sessionID)
			}()
		}
		if isVirtualPlaybackFile(file) {
			streamWriter := httpstream.NewRollingDeadlineWriter(w)
			targetURL, err := url.Parse(inputPath)
			if err == nil && targetURL.Scheme != "http" {
				err = fmt.Errorf("unsupported virtual stream scheme %q", targetURL.Scheme)
			}
			if err != nil {
				logVirtualStreamFailure(r.Context(), sessionID, file, err)
				h.handleTransportStartFailure(r.Context(), session, file, err)
				writeErrorCause(streamWriter, http.StatusBadGateway, "virtual_stream_unavailable", "Failed to stream virtual media source", err)
				return
			}
			// This proxy forwards client headers to the target by design; that
			// is only safe because virtual inputs always resolve to the local
			// relay. Assert the invariant rather than trusting every caller.
			host := targetURL.Hostname()
			if host != "127.0.0.1" && host != "::1" && host != "[::1]" {
				err := fmt.Errorf("virtual direct-play proxy target %q is not the local relay", targetURL.Host)
				logVirtualStreamFailure(r.Context(), sessionID, file, err)
				h.handleTransportStartFailure(r.Context(), session, file, err)
				writeErrorCause(streamWriter, http.StatusBadGateway, "virtual_stream_unavailable", "Failed to stream virtual media source", err)
				return
			}
			var lastProxyErr error
			proxy := &httputil.ReverseProxy{
				Rewrite: func(pr *httputil.ProxyRequest) {
					pr.Out.URL = targetURL
					pr.Out.Host = targetURL.Host
				},
				ModifyResponse: func(res *http.Response) error {
					if res.StatusCode >= http.StatusInternalServerError {
						return fmt.Errorf("relay returned HTTP %d", res.StatusCode)
					}
					return nil
				},
				ErrorHandler: func(rw http.ResponseWriter, req *http.Request, proxyErr error) {
					lastProxyErr = proxyErr
				},
			}
			proxy.ServeHTTP(streamWriter, r)
			if lastProxyErr != nil {
				// A client that navigated away is not a candidate failure:
				// never stamp the pinned release known-bad or spend a failover
				// resolve on it. The upstream request already aborted with the
				// canceled request context.
				if !isClientCancellation(r.Context(), lastProxyErr) && streamWriter.StatusCode() == 0 && (h.VirtualMediaDetailedResolver != nil || h.VirtualMediaRefreshResolver != nil) {
					if releaseInput != nil {
						releaseInput()
						releaseInput = nil
					}
					// A pinned release that already has a cached copy heals by
					// handing off to it: the failed fetch was the remote
					// provider, not the release, so the release is never
					// indicted. This runs before the sibling-rotation path so a
					// cache-aware heal never silently switches releases; when no
					// cached copy exists the existing rotation is unchanged.
					handoffAttempted := false
					if healedResolved, healCleanup, healedOK, _ := h.handoffVirtualSessionToCached(r.Context(), session, file, false); healedOK {
						healed := false
						if healedURL, parseErr := url.Parse(healedResolved.URL); parseErr == nil && healedURL.Scheme == "http" {
							healedHost := healedURL.Hostname()
							if healedHost == "127.0.0.1" || healedHost == "::1" || healedHost == "[::1]" {
								targetURL = healedURL
								deliveredPath = resolvedVirtualCandidatePath(healedResolved)
								releaseInput = healCleanup
								lastProxyErr = nil
								proxy.ServeHTTP(streamWriter, r)
								// The heal is a handoff only when the retried
								// stream actually delivered. A non-loopback
								// target, a parse failure, or a second proxy
								// error must fall through to failure marking and
								// sibling rotation rather than answer 502 while a
								// live sibling exists.
								healed = lastProxyErr == nil
							}
						}
						if healed {
							handoffAttempted = true
						} else if healCleanup != nil {
							healCleanup()
						}
					}
					if !handoffAttempted {
						// The pinned candidate served no bytes (corrupted NZB, dead
						// provider URL). Mark it failed and re-resolve with it
						// excluded so the next-ranked release is tried.
						failedID := virtualResultCandidateID(deliveredPath)
						if failedID != "" {
							h.markVirtualCandidateFailed(r.Context(), file, failedID)
						}
						excluded := []string{failedID}
						if failedID == "" {
							excluded = nil
						}
						// The retry is only reached because this serve layer just
						// indicted the delivered candidate (failedID non-empty) and
						// excluded it; declare substitution so the resolver may
						// serve a sibling. A retry with no indictment (failedID
						// empty) keeps refusing.
						// The retry re-resolves with a forced relist and, because
						// the edge just failed, a fresh relay registration: a
						// reused token would replay the registration whose
						// upstream returned 502 instead of presenting newly
						// resolved bytes, mirroring the remux seek-anchor retry.
						retryCtx := withVirtualRelayFreshRegistration(r.Context())
						refreshedMedia, refreshCleanup, refreshErr := h.resolveVirtualInputURIExcluding(retryCtx, file, session.UserID, session.ProfileID, true, excluded, failedID != "")
						if refreshErr == nil {
							expectedCandidateID := ""
							if parsed, err := url.Parse(file.FilePath); err == nil {
								expectedCandidateID = parsed.Query().Get("result")
							}
							// A failed pin this serve layer just marked is an
							// intended substitution: the retry excluded that id, so
							// it can only return a sibling. The guard exists to stop
							// a *silent* swap of a live candidate, so it only applies
							// when no failed candidate was identified.
							if failedID == "" && expectedCandidateID != "" && refreshedMedia.CandidateID != "" && refreshedMedia.CandidateID != expectedCandidateID {
								if refreshCleanup != nil {
									refreshCleanup()
								}
								lastProxyErr = fmt.Errorf("refreshed candidate %q does not match pinned candidate %q", refreshedMedia.CandidateID, expectedCandidateID)
							} else {
								releaseInput = refreshCleanup
								refreshedURL, parseErr := url.Parse(refreshedMedia.URL)
								if parseErr == nil && refreshedURL.Scheme == "http" {
									refreshedHost := refreshedURL.Hostname()
									if refreshedHost == "127.0.0.1" || refreshedHost == "::1" || refreshedHost == "[::1]" {
										targetURL = refreshedURL
										deliveredPath = resolvedVirtualCandidatePath(refreshedMedia)
										lastProxyErr = nil
										proxy.ServeHTTP(streamWriter, r)
									}
								}
							}
						}
					}
				}
				if lastProxyErr != nil {
					h.handleTransportStartFailure(r.Context(), session, file, lastProxyErr)
					if streamWriter.StatusCode() == 0 {
						logVirtualStreamFailure(r.Context(), sessionID, file, lastProxyErr)
						writeErrorCause(streamWriter, http.StatusBadGateway, "virtual_stream_unavailable", "Failed to stream virtual media source", lastProxyErr)
					}
				}
			}
			if lastProxyErr == nil && virtualCandidateDeliveryEvidence(streamWriter.StatusCode(), streamWriter.BytesWritten()) {
				h.clearVirtualCandidateRecovered(r.Context(), file, deliveredPath, virtualObservedFailedAt)
			}
			return
		}
		if err := playback.ServeDirectPlay(w, r, inputPath); err != nil {
			h.handleTransportStartFailure(r.Context(), session, file, err)
		}

	case playback.PlayRemux:
		if setter, ok := h.sessionMgr.(interface {
			SetOutputFormat(string, string, string) error
		}); ok {
			_ = setter.SetOutputFormat(sessionID, playback.OutputContainerFMP4, playback.OutputProtocolHTTP)
		}
		if err := h.sessionMgr.BeginTransport(sessionID); err == nil {
			defer func() {
				_ = h.sessionMgr.EndTransport(sessionID)
			}()
		}
		seekSeconds := 0.0
		if seekStr := r.URL.Query().Get("seek"); seekStr != "" {
			if s, err := strconv.ParseFloat(seekStr, 64); err == nil && s >= 0 {
				seekSeconds = s
			}
		}
		// An audio-only source muxes an audio-only fMP4. The v3 plan promises
		// audio/mp4 for it, and a declared-tier client refuses to attach a
		// source buffer whose advertised type its probe rejected — so the
		// response has to keep the same promise the plan made.
		dvProfile := session.DVProfile
		if dvProfile == 0 {
			dvProfile = file.PrimaryDVProfile()
		}
		serveRemux := func() error {
			// The remux writes through the raw writer; wrap it to observe how
			// many bytes actually reached the client. nil return is NOT
			// delivery evidence (remux.go can return nil when the first
			// client write fails) — recovery is gated on positive bytes.
			remuxWriter := httpstream.NewRollingDeadlineWriter(w)
			err := playback.ServeRemuxWithOptions(remuxWriter, r, inputPath, "mp4", seekSeconds, session.TranscodeAudio, audioStreamOrdinalV3(file, session.AudioTrackIndex), dvProfile, playback.RemuxServeOptions{
				DVMode:                 session.RemuxDVMode,
				FFmpegPath:             h.ffmpegPath(),
				ContentType:            playback.RemuxContentType(file.IsAudioOnly()),
				AudioOnly:              file.IsAudioOnly(),
				SourceAudioChannels:    session.SourceAudioChannels,
				TargetAudioChannels:    session.TargetAudioChannels,
				TargetAudioBitrateKbps: session.TargetAudioBitrateKbps,
				TimingStart:            requestStart,
				// A pre-body start failure must leave the response
				// uncommitted so the handler can fail over to a sibling
				// candidate; the v2 writer locks the first >=400 status and
				// would discard a successful retry.
				DeferStartError: true,
			})
			if err == nil && isVirtualPlaybackFile(file) &&
				virtualCandidateDeliveryEvidence(http.StatusOK, remuxWriter.BytesWritten()) {
				// Media bytes actually flowed to the client for the candidate
				// the session planned — the only evidence that forgives a
				// transport failure. Fenced on the delivered identity and the
				// health state observed at transport start.
				h.clearVirtualCandidateRecovered(r.Context(), file, deliveredPath, virtualObservedFailedAt)
			}
			return err
		}
		remuxErr := serveRemux()
		if remuxErr != nil {
			// The remux only commits 200 after FFmpeg produces media bytes, so
			// a failure here means the provider release served no output
			// (corrupted NZB, dead URL). Mark the candidate failed and retry
			// once with it excluded so the next-ranked release is tried. A
			// client that disconnected is not a candidate failure: the remux
			// (and its provider fetch) already aborted on the request context.
			if !isClientCancellation(r.Context(), remuxErr) && isVirtualPlaybackFile(file) && hasVirtualMediaResolver(h) {
				if releaseInput != nil {
					releaseInput()
					releaseInput = nil
				}
				// Cache-aware heal first: a cached copy of the pinned release
				// heals the failure without indicting the release. Only when no
				// cached copy exists does the existing rotation-to-a-sibling
				// path run, so a cache-aware heal never silently changes the
				// release.
				handled := false
				if healedResolved, healCleanup, healedOK, _ := h.handoffVirtualSessionToCached(r.Context(), session, file, false); healedOK {
					if healedURL, parseErr := url.Parse(healedResolved.URL); parseErr == nil && healedURL.Scheme == "http" {
						healedHost := healedURL.Hostname()
						if healedHost == "127.0.0.1" || healedHost == "::1" || healedHost == "[::1]" {
							inputPath = healedResolved.URL
							deliveredPath = resolvedVirtualCandidatePath(healedResolved)
							releaseInput = healCleanup
							remuxErr = serveRemux()
							// Only a heal that actually delivered is handled;
							// a cached retry that failed too falls through to
							// failure marking and sibling rotation.
							if remuxErr == nil {
								handled = true
							} else if healCleanup != nil {
								healCleanup()
							}
						} else if healCleanup != nil {
							healCleanup()
						}
					} else if healCleanup != nil {
						healCleanup()
					}
				}
				if !handled {
					failedID := virtualResultCandidateID(deliveredPath)
					if failedID != "" {
						h.markVirtualCandidateFailed(r.Context(), file, failedID)
					}
					excluded := []string{failedID}
					if failedID == "" {
						excluded = nil
					}
					// The remux failed because this serve layer just indicted the
					// delivered candidate; declare substitution so the resolver may
					// serve a sibling release. A retry with no indictment keeps
					// refusing.
					retried, retryCleanup, retryErr := h.resolveVirtualInputURIExcluding(r.Context(), file, session.UserID, session.ProfileID, true, excluded, failedID != "")
					if retryErr == nil {
						// Same pinned-candidate guard direct play has: a retry that
						// resolved a different release than the session-bound pin
						// must not silently swap the bytes mid-stream. A failed pin
						// this serve layer just marked is an intended substitution
						// (the retry excluded it, so it can only return a sibling),
						// so the guard applies only when no failed candidate was
						// identified.
						expectedCandidateID := ""
						if parsed, err := url.Parse(file.FilePath); err == nil {
							expectedCandidateID = parsed.Query().Get("result")
						}
						if failedID == "" && expectedCandidateID != "" && retried.CandidateID != "" && retried.CandidateID != expectedCandidateID {
							if retryCleanup != nil {
								retryCleanup()
							}
							remuxErr = fmt.Errorf("retried candidate %q does not match pinned candidate %q", retried.CandidateID, expectedCandidateID)
						} else {
							releaseInput = retryCleanup
							retryURL, parseErr := url.Parse(retried.URL)
							if parseErr == nil && retryURL.Scheme == "http" {
								retryHost := retryURL.Hostname()
								if retryHost == "127.0.0.1" || retryHost == "::1" || retryHost == "[::1]" {
									inputPath = retried.URL
									deliveredPath = resolvedVirtualCandidatePath(retried)
									remuxErr = serveRemux()
								}
							}
						}
					}
				}
			}
			if remuxErr != nil {
				h.handleTransportStartFailure(r.Context(), session, file, remuxErr)
				// The remux defers pre-body start failures, so with that option set
				// nothing has committed a status and the v2 writer is still
				// unlocked; a mid-stream failure returns nil instead. Commit one
				// coherent error here. A client that disconnected needs no body.
				if !isClientCancellation(r.Context(), remuxErr) {
					http.Error(w, "failed to start remux", http.StatusBadGateway)
				}
			}
		}

	case playback.PlayTranscode:
		writeError(w, http.StatusBadRequest, "bad_request",
			"Transcode streams use manifest/segment endpoints")

	default:
		writeError(w, http.StatusInternalServerError, "internal_error",
			"Unknown play method")
	}
}

// loadSidecarSession resolves the session a subtitle or font request names.
// It reconstructs from the signed stream reference after a restart or on a
// replica that never served the media, and checks account and selected-profile
// ownership before exposing a sidecar.
func (h *StreamHandler) loadSidecarSession(ctx context.Context, reference, sessionID string, userID int) (*playback.Session, *streamtoken.Claims, error) {
	card, claims := verifiedStreamCardFromToken(reference, sessionID, h.JWTSecret)
	loadCard := card
	if _, err := h.sessionMgr.GetSession(sessionID); err == nil {
		loadCard = nil
	} else if !errors.Is(err, playback.ErrSessionNotFound) {
		loadCard = nil
	}
	session, status, _, sessionErr := h.TM.LoadOrReconstructSessionDetailWithError(ctx, h.sessionMgr.GetSession, sessionID, userID, loadCard)
	switch status {
	case playback.SessionMissing:
		return nil, nil, apiError(http.StatusNotFound, playbackSessionNotFoundErrorCode, "Playback session not found")
	case playback.SessionLoadFailed:
		return nil, nil, apiError(http.StatusInternalServerError, "internal_error", "Failed to load playback session").WithCause(sessionErr)
	case playback.SessionForbidden:
		return nil, nil, apiError(http.StatusForbidden, "forbidden", "Session belongs to another user")
	case playback.SessionUnauthorized:
		return nil, nil, apiError(http.StatusUnauthorized, "unauthorized", "Authentication required")
	}
	if session == nil {
		return nil, nil, apiError(http.StatusNotFound, playbackSessionNotFoundErrorCode, "Playback session not found")
	}
	if profileID := apimw.GetProfileID(ctx); profileID != "" && session.ProfileID != "" && profileID != session.ProfileID {
		return nil, nil, apiError(http.StatusForbidden, "forbidden", "Session belongs to another profile")
	}
	return session, claims, nil
}

// HandleSubtitle extracts a subtitle track from the media file associated with
// a playback session and serves it as WebVTT or raw ASS depending on the
// URL extension (e.g. /subtitles/2.ass or /subtitles/2.vtt).
func (h *StreamHandler) handleSubtitle(w http.ResponseWriter, r *http.Request) {
	userID := apimw.GetUserID(r.Context())
	if userID == 0 {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}

	sessionID := chi.URLParam(r, "session_id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Session ID is required")
		return
	}
	setPlaybackSessionLogContext(r, sessionID)
	if h.StreamDeny.Denied(r.Context(), sessionID) {
		writePlaybackSessionEnded(w)
		return
	}

	trackParam := chi.URLParam(r, "track")
	trackIndex, requestedFormat, err := playback.ParseSubtitleTrackParam(trackParam)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid subtitle track index")
		return
	}

	session, claims, err := h.loadSidecarSession(r.Context(), r.URL.Query().Get(streamTokenParam), sessionID, userID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	attachPlaybackSession(r.Context(), session, claims)

	if session.UserID != userID {
		writeError(w, http.StatusForbidden, "forbidden", "Session belongs to another user")
		return
	}

	fileID, err := subtitleSourceFileID(r, session)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	file, err := h.fileResolver.GetByID(r.Context(), fileID)
	if err != nil || file == nil {
		writeError(w, http.StatusNotFound, "not_found", "Media file not found")
		return
	}

	// Capture the catalog row's full subtitle layout before the session
	// overlay. A virtual release can rotate between planning and extraction:
	// the row is re-probed against the current candidate while this session's
	// URLs still name the layout captured at plan time. When the two diverge,
	// extraction must verify the live source (and possibly re-map the plan
	// ordinal) before spawning ffmpeg, because the ordinal is only valid
	// against the pinned release's actual layout. External subtitles are part
	// of that ordinal space (they precede the embedded segment), so the
	// comparison covers them too.
	rowEmbedded := file.SubtitleTracks
	rowExternal := file.ExternalSubtitles

	// Resolve the source file this request actually serves from. A subtitle
	// URL may name the session's requested (old) edition after an edition
	// switch moved the effective file; such a request is foreign, and its
	// ordinal must be re-mapped onto the effective inventory rather than
	// interpreted against a version no longer playing. Virtual sources bind to
	// the session's planned candidate at the same time.
	file, trackIndex, err = h.resolveSubtitleSourceRequest(r.Context(), file, session, trackIndex, r.URL.Query())
	if err != nil {
		if errors.Is(err, errSubtitleIdentityInvalid) {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		} else {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		}
		return
	}

	// driftSuspected means the layout this request's plan-time ordinal was
	// minted against may differ from the release now being served, so the
	// pre-spawn live-layout probe must run before ffmpeg is spawned. It is
	// true when the mutable catalog row no longer matches the plan-time
	// evidence, and also when the carried evidence provably belongs to a
	// different candidate than the session is bound to (the serve layer
	// rotated without updating the evidence) — in that case the plan ordinal
	// is untrustworthy against every candidate on hand.
	evidenceTrusted := session.VirtualSubtitleEvidenceSet && virtualEvidenceMatchesBoundFile(file, session)
	driftSuspected := isVirtualPlaybackFile(file) &&
		session.VirtualSourceURI != "" &&
		session.VirtualSubtitleEvidenceSet &&
		(!evidenceTrusted || !playback.SubtitleLayoutsEqualIncludingExternal(
			rowEmbedded, rowExternal, session.VirtualSubtitleTracks, session.VirtualExternalSubtitles))

	// trackIndex is a combined ordinal, resolved through the same three
	// consecutive ranges playback.BuildSubtitleInventoryV3 assigns them from:
	// externals, then embedded container tracks, then downloaded ones. The
	// ranges cover the full track arrays — including bitmap tracks that have no
	// sidecar shape — so an ordinal always names the same track here as it does
	// in the published inventory.
	// Downloaded subtitle URLs additionally bind that ordinal to a stable row
	// identity. The path ordinal remains for compatibility and display, but it
	// must not be re-resolved against a mutable inventory after a seek reanchor.
	if rawID := strings.TrimSpace(r.URL.Query().Get(playback.DownloadedSubtitleIDParamV3)); rawID != "" {
		downloadedID, parseErr := strconv.Atoi(rawID)
		if parseErr != nil || downloadedID <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "Invalid downloaded subtitle identity")
			return
		}
		if h.SubtitleRepo == nil || h.SubtitleBlobs == nil {
			writeError(w, http.StatusNotFound, "not_found", "Subtitle track not found")
			return
		}
		downloaded, lookupErr := h.SubtitleRepo.GetDownloadedSubtitle(r.Context(), downloadedID)
		if lookupErr != nil {
			slog.ErrorContext(r.Context(), "get downloaded subtitle failed", "component", "api",
				"file_id", file.ID,
				"downloaded_subtitle_id", downloadedID,
				"error", lookupErr,
			)
			writeErrorCause(w, http.StatusInternalServerError, "internal_error", "Failed to load downloaded subtitle", lookupErr)
			return
		}
		if downloaded == nil || downloaded.MediaFileID != file.ID {
			writeError(w, http.StatusNotFound, "not_found", "Subtitle track not found")
			return
		}
		if r.Method == http.MethodHead {
			writeSubtitleRepresentationHead(w, subtitleRepresentationFormat(requestedFormat, servesOriginalSubRip(r, string(downloaded.Format), requestedFormat)))
			return
		}
		h.serveDownloadedSubtitle(w, r, *downloaded, requestedFormat)
		return
	}
	externalCount := len(file.ExternalSubtitles)
	if trackIndex < externalCount {
		sub := file.ExternalSubtitles[trackIndex]
		if !subtitleSidecarFormatSupported(sub.Format, requestedFormat, false) {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
				"Requested subtitle extension does not match the selected track")
			return
		}
		if r.Method == http.MethodHead {
			writeSubtitleRepresentationHead(w, subtitleRepresentationFormat(requestedFormat, servesOriginalSubRip(r, sub.Format, requestedFormat)))
			return
		}

		// Serve ASS/SSA external subtitles as raw data for client-side
		// rendering, and SRT the same way when the URL asks for .srt.
		if servesOriginalSubRip(r, sub.Format, requestedFormat) {
			data, err := playback.LoadExternalSubtitleRaw(sub.Path)
			if err != nil {
				writeErrorCause(w, http.StatusInternalServerError, "internal_error",
					"Failed to load external subtitle", err)
				return
			}
			serveOriginalSubRip(w, data)
			return
		}
		if playback.IsASS(sub.Format) && requestedFormat != "vtt" {
			data, err := playback.LoadExternalSubtitleRaw(sub.Path)
			if err != nil {
				writeErrorCause(w, http.StatusInternalServerError, "internal_error",
					"Failed to load external subtitle", err)
				return
			}
			playback.ServeSubtitle(w, data, subtitleFormatASS)
			return
		}

		vttData, err := playback.LoadExternalSubtitleAsVTT(r.Context(), sub.Path, sub.Format, h.ffmpegPath())
		if err != nil {
			writeErrorCause(w, http.StatusInternalServerError, "internal_error",
				"Failed to load external subtitle", err)
			return
		}
		playback.ServeSubtitle(w, vttData, "vtt")
		return
	}

	embeddedIndex := trackIndex - externalCount

	// Check embedded tracks.
	if embeddedIndex < len(file.SubtitleTracks) {
		track := file.SubtitleTracks[embeddedIndex]
		// PGS is the one bitmap codec we can deliver without burn-in: the
		// track is copied losslessly into a .sup stream and rendered
		// client-side. DVD/DVB bitmap subs still require burn-in.
		if playback.NeedsBurnIn(track.Codec) && !playback.IsPGS(track.Codec) {
			writeError(w, http.StatusBadRequest, "bad_request",
				"Bitmap subtitle tracks cannot be extracted as text")
			return
		}
		if !subtitleSidecarFormatSupported(track.Codec, requestedFormat, true) {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
				"Requested subtitle extension does not match the selected track")
			return
		}
		if r.Method == http.MethodHead && requestedFormat != subtitleFormatSUP {
			// Embedded text streams as WebVTT whatever the extension.
			writeSubtitleRepresentationHead(w, subtitleRepresentationFormat(requestedFormat, false))
			return
		}

		// Dedicated streaming extract — ffmpeg seeks to the current
		// playback position and pipes cues to the response as they're
		// demuxed, so the first byte lands within ~1s even on network
		// storage. Works identically for direct-play, remux, and
		// transcode because it doesn't depend on any other ffmpeg.
		h.streamEmbeddedSubtitle(w, r, file, embeddedIndex, session, driftSuspected, requestedFormat)
		return
	}

	// Check downloaded subtitles (from blob storage).
	if h.SubtitleRepo != nil && h.SubtitleBlobs != nil {
		downloaded, err := h.SubtitleRepo.ListDownloadedSubtitles(r.Context(), file.ID)
		if err != nil {
			// A DB failure here must not masquerade as "track not found":
			// surface it as an internal error (with a server-side signal)
			// so the real failure is diagnosable instead of looking like an
			// intermittent 404 to the client.
			slog.ErrorContext(r.Context(), "list downloaded subtitles failed", "component", "api",
				"file_id", file.ID,
				"track", trackIndex,
				"error", err,
			)
			writeErrorCause(w, http.StatusInternalServerError, "internal_error", "Failed to list downloaded subtitles", err)
			return
		}

		downloadedIndex := embeddedIndex - len(file.SubtitleTracks)
		if downloadedIndex >= 0 && downloadedIndex < len(downloaded) {
			if r.Method == http.MethodHead {
				writeSubtitleRepresentationHead(w, subtitleRepresentationFormat(requestedFormat, servesOriginalSubRip(r, string(downloaded[downloadedIndex].Format), requestedFormat)))
				return
			}
			h.serveDownloadedSubtitle(w, r, downloaded[downloadedIndex], requestedFormat)
			return
		}
	}

	writeError(w, http.StatusNotFound, "not_found", "Subtitle track not found")
}

func (h *StreamHandler) serveDownloadedSubtitle(w http.ResponseWriter, r *http.Request, subtitle subtitles.DownloadedSubtitle, requestedFormat string) {
	if !subtitleSidecarFormatSupported(string(subtitle.Format), requestedFormat, false) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"Requested subtitle extension does not match the selected track")
		return
	}
	data, err := h.SubtitleBlobs.Get(r.Context(), subtitle.S3Key)
	if err != nil {
		writeErrorCause(w, http.StatusBadGateway, "s3_error", "Failed to load subtitle from storage", err)
		return
	}

	// Serve ASS/SSA downloaded subtitles as raw data, and SRT the same way
	// when the URL asks for .srt.
	if servesOriginalSubRip(r, string(subtitle.Format), requestedFormat) {
		serveOriginalSubRip(w, data)
		return
	}
	if playback.IsASS(string(subtitle.Format)) && requestedFormat != "vtt" {
		playback.ServeSubtitle(w, data, subtitleFormatASS)
		return
	}

	// If the subtitle is already VTT, serve directly.
	if subtitle.Format == subtitles.FormatVTT {
		playback.ServeSubtitle(w, data, "vtt")
		return
	}

	// Convert other text formats to VTT using the playback conversion pipeline.
	vttData, err := playback.ConvertToVTTWithFFmpeg(r.Context(), data, string(subtitle.Format), h.ffmpegPath())
	if err != nil {
		writeErrorCause(w, http.StatusInternalServerError, "convert_error", "Failed to convert subtitle", err)
		return
	}
	playback.ServeSubtitle(w, vttData, "vtt")
}

// subtitleSidecarFormatSupported keeps bitmap and styled-text requests within
// the representations the server can produce. Plain text tracks preserve the
// v1 endpoint's permissive extension behavior and are always returned as VTT;
// ASS/SSA may also be served losslessly, and only an embedded PGS track has a
// binary .sup representation.
func subtitleSidecarFormatSupported(codec, requestedFormat string, embeddedPGS bool) bool {
	requestedFormat = strings.ToLower(strings.TrimSpace(requestedFormat))
	if requestedFormat == "" {
		return true
	}
	if playback.IsPGS(codec) {
		return embeddedPGS && requestedFormat == subtitleFormatSUP
	}
	if playback.NeedsBurnIn(codec) {
		return false
	}
	if playback.IsASS(codec) {
		return requestedFormat == subtitleFormatASS || requestedFormat == subtitleFormatSSA || requestedFormat == "vtt"
	}
	return true
}

// servesOriginalSubRip reports whether a sidecar request is answered with the
// original SRT bytes: only a SubRip track requested through /api/v2 as .srt
// with original=1, which is the URL subrip_sidecar_v1 publishes. Every other
// request for a SubRip track, including any on the frozen /api/v1 route, keeps
// the historical WebVTT response.
func servesOriginalSubRip(r *http.Request, codec, requestedFormat string) bool {
	return playback.IsSubRip(codec) &&
		strings.EqualFold(strings.TrimSpace(requestedFormat), subtitleFormatSRT) &&
		r.URL.Query().Get(playback.SubtitleOriginalParamV3) == "1" &&
		isNativeAPIV2(r.Context())
}

// serveOriginalSubRip writes stored SRT bytes as they are. SRT declares no
// encoding, so the response claims UTF-8 only when the bytes are valid UTF-8;
// a legacy-encoded file is labeled without a charset rather than mislabeled.
func serveOriginalSubRip(w http.ResponseWriter, data []byte) {
	contentType := subtitleMIMESubRip
	if utf8.Valid(data) {
		contentType += "; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data) //nolint:errcheck
}

// subtitleRepresentationFormat names the representation a GET would serve, so
// a HEAD reports the same content type: a .srt request that is not answered
// with the original SRT is answered with WebVTT.
func subtitleRepresentationFormat(requestedFormat string, servesOriginalSRT bool) string {
	if strings.EqualFold(strings.TrimSpace(requestedFormat), subtitleFormatSRT) && !servesOriginalSRT {
		return "vtt"
	}
	return requestedFormat
}

func writeSubtitleRepresentationHead(w http.ResponseWriter, requestedFormat string) {
	switch strings.ToLower(strings.TrimSpace(requestedFormat)) {
	case subtitleFormatASS, subtitleFormatSSA:
		w.Header().Set("Content-Type", "text/x-ssa; charset=utf-8")
	case subtitleFormatSRT:
		// HEAD does not read the stored bytes, so it cannot vouch for UTF-8.
		w.Header().Set("Content-Type", subtitleMIMESubRip)
	case subtitleFormatSUP:
		w.Header().Set("Content-Type", "application/octet-stream")
	default:
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	}
	w.WriteHeader(http.StatusOK)
}

// subtitleSourceFileID pins a subtitle URL to the file whose track list was
// used to create it. A quality/seek restart may change session.MediaFileID to
// an alternate version; interpreting the old combined track index against the
// alternate file can silently serve a different language. Only the session's
// requested or current effective file may be named by the authenticated URL.
func subtitleSourceFileID(r *http.Request, session *playback.Session) (int, error) {
	return subtitleSourceFile(r.URL.Query().Get("file_id"), session)
}

func subtitleSourceFile(raw string, session *playback.Session) (int, error) {
	if session == nil {
		return 0, errors.New("playback session is required")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return session.MediaFileID, nil
	}
	fileID, err := strconv.Atoi(raw)
	if err != nil || fileID <= 0 {
		return 0, errors.New("invalid subtitle source file")
	}
	if fileID != session.MediaFileID && fileID != session.RequestedMediaFileID {
		return 0, errors.New("subtitle source file does not belong to playback session")
	}
	return fileID, nil
}

// SubtitleFontRequest identifies the session and frozen subtitle inventory
// selection. Query preserves repeated identity parameters so validation cannot
// silently choose one of conflicting stream pins.
type SubtitleFontRequest struct {
	SessionID string
	Track     string
	Query     url.Values
}

// SubtitleFonts loads a bounded bundle of embedded ASS/SSA fonts. Both API
// transports use this operation so reconstruction, deny markers and source-file
// admission stay identical without invoking another transport's HTTP handler.
func (h *StreamHandler) SubtitleFonts(ctx context.Context, in SubtitleFontRequest) ([]playback.SubtitleFontBundleItem, error) {
	userID := apimw.GetUserID(ctx)
	if userID == 0 {
		return nil, apiError(http.StatusUnauthorized, "unauthorized", "Authentication required")
	}
	if in.SessionID == "" {
		return nil, apiError(http.StatusBadRequest, "bad_request", "Session ID is required")
	}
	if lc := activitylog.GetPlaybackLogContext(ctx); lc != nil {
		lc.PlaybackSessionID = in.SessionID
	}
	if h.StreamDeny.Denied(ctx, in.SessionID) {
		return nil, apiError(http.StatusGone, playbackSessionEndedErrorCode, "Playback session has ended")
	}
	session, claims, err := h.loadSidecarSession(ctx, in.Query.Get(streamTokenParam), in.SessionID, userID)
	if err != nil {
		return nil, err
	}
	attachPlaybackSession(ctx, session, claims)

	fileID, err := subtitleSourceFile(in.Query.Get("file_id"), session)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, "bad_request", err.Error())
	}
	file, err := h.fileResolver.GetByID(ctx, fileID)
	if err != nil || file == nil {
		return nil, apiError(http.StatusNotFound, "not_found", "Media file not found")
	}

	trackIndex, _, err := playback.ParseSubtitleTrackParam(in.Track)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, "bad_request", "Invalid subtitle track index")
	}
	file, trackIndex, err = h.resolveSubtitleSourceRequest(ctx, file, session, trackIndex, in.Query)
	if err != nil {
		if errors.Is(err, errSubtitleIdentityInvalid) {
			return nil, apiError(http.StatusBadRequest, "bad_request", err.Error())
		}
		return nil, apiError(http.StatusNotFound, "not_found", err.Error())
	}
	if err := preflightPlaybackFile(ctx, file, h.MissingMarker, h.EventsHub); err != nil {
		if isPlaybackFileMissing(err) {
			h.abortPlaybackSession(ctx, session)
			return nil, apiError(http.StatusNotFound, "not_found", "Source media file is missing")
		}
		return nil, apiError(http.StatusInternalServerError, "internal_error", "Failed to access source media file")
	}

	embeddedIndex := trackIndex - len(file.ExternalSubtitles)
	if embeddedIndex < 0 || embeddedIndex >= len(file.SubtitleTracks) {
		return nil, apiError(http.StatusNotFound, "not_found", "Embedded subtitle track not found")
	}
	if !playback.IsASS(file.SubtitleTracks[embeddedIndex].Codec) {
		return nil, apiError(http.StatusBadRequest, "bad_request", "Subtitle font bundles are only available for ASS/SSA tracks")
	}
	// A virtual file's stored path is a virtual:// URI that ffprobe cannot
	// read; resolve the provider input for this request, mirroring the v1
	// uncached path, and release the relay registration when we return.
	inputPath := file.FilePath
	releaseInput := func() {}
	if isVirtualPlaybackFile(file) && session.VirtualSourceURI != "" && hasVirtualMediaResolver(h) {
		var resolved ResolvedVirtualMedia
		resolved, releaseInput, err = h.resolveVirtualInputURI(ctx, file, session.UserID, session.ProfileID, false)
		if err != nil {
			logVirtualStreamFailure(ctx, session.ID, file, err)
			return nil, apiError(http.StatusBadGateway, subtitleSourceUnavailableErrorCode, "Failed to resolve virtual source for the subtitle font bundle")
		}
		inputPath = resolved.URL
	}
	defer releaseInput()

	fonts, err := playback.ExtractAttachedSubtitleFonts(ctx, inputPath, h.ffmpegPath())
	if err != nil {
		h.fontExtractFailures.failed(ctx, fontExtractFailureKey(file.ID, trackIndex),
			"file_id", file.ID, "track", trackIndex, "error", err)
		return nil, apiError(http.StatusInternalServerError, "font_extract_failed", "Failed to extract subtitle fonts")
	}
	h.fontExtractFailures.recovered(fontExtractFailureKey(file.ID, trackIndex))
	return playback.EncodeSubtitleFontBundle(fonts), nil
}

// HandleSubtitleFonts preserves the bridge API's array response and serves
// font bundles through the subtitle cache for virtual-source pre-warm.
func (h *StreamHandler) HandleSubtitleFonts(w http.ResponseWriter, r *http.Request) {
	userID := apimw.GetUserID(r.Context())
	if userID == 0 {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}

	sessionID := chi.URLParam(r, "session_id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Session ID is required")
		return
	}
	setPlaybackSessionLogContext(r, sessionID)
	if h.StreamDeny.Denied(r.Context(), sessionID) {
		writePlaybackSessionEnded(w)
		return
	}

	session, claims, err := h.loadSidecarSession(r.Context(), r.URL.Query().Get(streamTokenParam), sessionID, userID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	attachPlaybackSession(r.Context(), session, claims)

	if session.UserID != userID {
		writeError(w, http.StatusForbidden, "forbidden", "Session belongs to another user")
		return
	}

	fileID, err := subtitleSourceFileID(r, session)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	file, err := h.fileResolver.GetByID(r.Context(), fileID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Media file not found")
		return
	}
	if file == nil {
		writeError(w, http.StatusNotFound, "not_found", "Media file not found")
		return
	}

	trackParam := chi.URLParam(r, "track")
	trackIndex, _, err := playback.ParseSubtitleTrackParam(trackParam)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid subtitle track index")
		return
	}
	// Resolve the file/ordinal actually served: re-map an edition-switch
	// request that names the old edition, then bind the session's planned
	// virtual candidate so the same release the plan probed is extracted.
	file, trackIndex, err = h.resolveSubtitleSourceRequest(r.Context(), file, session, trackIndex, r.URL.Query())
	if err != nil {
		if errors.Is(err, errSubtitleIdentityInvalid) {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		} else {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		}
		return
	}
	if err := preflightPlaybackFile(r.Context(), file, h.MissingMarker, h.EventsHub); err != nil {
		if isPlaybackFileMissing(err) {
			h.abortPlaybackSession(r.Context(), session)
		}
		writePlaybackFilePreflightError(w, err)
		return
	}

	embeddedIndex := trackIndex - len(file.ExternalSubtitles)
	if embeddedIndex < 0 || embeddedIndex >= len(file.SubtitleTracks) {
		writeError(w, http.StatusNotFound, "not_found", "Embedded subtitle track not found")
		return
	}
	if !playback.IsASS(file.SubtitleTracks[embeddedIndex].Codec) {
		writeError(w, http.StatusBadRequest, "bad_request", "Subtitle font bundles are only available for ASS/SSA tracks")
		return
	}

	// Build the font-bundle cache key before any virtual resolve: the identity
	// must never depend on the resolved relay URL, which rotates per
	// registration. Shared with the playback pre-warm so the two cannot drift.
	virtualFontSource := isVirtualPlaybackFile(file) && session.VirtualSourceURI != ""
	cacheKey := fontBundleCacheKey(file, session.VirtualSourceURI, h.ffmpegPath())

	// Virtual keys without a pinned result= param are intentionally
	// uncacheable: the identity would be unstable without the candidate
	// anchor, so we fall through to the uncached extract path below.
	if virtualFontSource && cacheKey.PinnedResult == "" {
		slog.DebugContext(r.Context(), "virtual font bundle has no pinned result= param; skipping cache", "component", "api", "file_id", file.ID)
	}

	// A cache hit serves the encoded bundle immediately: no provider round-trip,
	// no relay registration, no ffmpeg spawn.
	if h.SubtitleCache != nil {
		if cached, ok := h.SubtitleCache.LookupFontBundle(cacheKey); ok {
			writeFontBundleResponse(w, cached)
			return
		}
	}

	if h.SubtitleCache == nil {
		// No cache configured: keep the historical uncached path, resolved and
		// released within the request.
		inputPath := file.FilePath
		releaseInput := func() {}
		if virtualFontSource && hasVirtualMediaResolver(h) {
			var resolved ResolvedVirtualMedia
			resolved, releaseInput, err = h.resolveVirtualInputURI(r.Context(), file, session.UserID, session.ProfileID, false)
			if err != nil {
				logVirtualStreamFailure(r.Context(), session.ID, file, err)
				writeError(w, http.StatusBadGateway, subtitleSourceUnavailableErrorCode, "Failed to resolve virtual source for the subtitle font bundle")
				return
			}
			inputPath = resolved.URL
		}
		defer releaseInput()

		fonts, extractErr := playback.ExtractAttachedSubtitleFonts(r.Context(), inputPath, h.ffmpegPath())
		if extractErr != nil {
			h.fontExtractFailures.failed(r.Context(), fontExtractFailureKey(file.ID, trackIndex),
				"file_id", file.ID,
				"track", trackIndex,
				"error", extractErr,
			)
			writeError(w, http.StatusInternalServerError, "font_extract_failed", "Failed to extract subtitle fonts")
			return
		}
		h.fontExtractFailures.recovered(fontExtractFailureKey(file.ID, trackIndex))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(playback.EncodeSubtitleFontBundle(fonts)); err != nil {
			slog.WarnContext(r.Context(), "subtitle font response encode failed", "component", "api", "error", err)
		}
		return
	}

	// The detached single-flight owns the virtual relay registration: it is
	// resolved inside the extract closure and released when that closure
	// returns, so a cold HTTP-led flight that outlives the short client wait
	// still has a valid source for the whole extraction. Releasing at HTTP
	// return instead would let ffmpeg start against a relay entry that no
	// longer exists.
	localPath := file.FilePath
	bundle, ready, err := h.SubtitleCache.ExtractFontBundleWithin(r.Context(), cacheKey, fontBundleClientWait, func(extractCtx context.Context) ([]byte, error) {
		inputPath := localPath
		releaseInput := func() {}
		if virtualFontSource && hasVirtualMediaResolver(h) {
			resolved, cleanup, resolveErr := h.resolveVirtualInputURI(extractCtx, file, session.UserID, session.ProfileID, false)
			if resolveErr != nil {
				logVirtualStreamFailure(extractCtx, session.ID, file, resolveErr)
				return nil, fmt.Errorf("%w: %w", errVirtualFontResolve, resolveErr)
			}
			inputPath = resolved.URL
			releaseInput = cleanup
		}
		defer releaseInput()

		fonts, extractErr := playback.ExtractAttachedSubtitleFonts(extractCtx, inputPath, h.ffmpegPath())
		if extractErr != nil {
			return nil, extractErr
		}
		return json.Marshal(playback.EncodeSubtitleFontBundle(fonts))
	})
	if err != nil {
		if errors.Is(err, errVirtualFontResolve) {
			logVirtualStreamFailure(r.Context(), session.ID, file, err)
			writeError(w, http.StatusBadGateway, subtitleSourceUnavailableErrorCode, "Failed to resolve virtual source for the subtitle font bundle")
			return
		}
		h.fontExtractFailures.failed(r.Context(), fontExtractFailureKey(file.ID, trackIndex),
			"file_id", file.ID,
			"track", trackIndex,
			"error", err,
		)
		writeError(w, http.StatusInternalServerError, "font_extract_failed", "Failed to extract subtitle fonts")
		return
	}
	h.fontExtractFailures.recovered(fontExtractFailureKey(file.ID, trackIndex))
	if !ready {
		// The extraction is still running detached. Hold the request only for
		// fontBundleClientWait, then hand the client a distinguishable,
		// uncacheable pending bundle so it falls back to default fonts
		// immediately and re-fetches; the single-flighted extraction keeps
		// running and lands in the cache for that next fetch.
		slog.DebugContext(r.Context(), "subtitle font bundle extraction in flight; serving pending bundle",
			"component", "api", "file_id", file.ID, "track", trackIndex)
		writePendingFontBundleResponse(w)
		return
	}
	writeFontBundleResponse(w, bundle)
}

// errVirtualFontResolve marks a font-bundle extraction failure caused by the
// virtual relay resolution rather than the extraction itself, so the HTTP
// handler can answer with the provider-resolution status instead of a generic
// extraction failure.
var errVirtualFontResolve = errors.New("virtual font bundle resolve failed")

// fontBundleClientWait bounds how long the font-bundle handler waits for a
// cold extraction before returning an empty bundle. It must stay well below
// the web client's FONT_BUNDLE_BUDGET_MS (3s) so the client never waits on the
// server; the extraction continues in the background.
var fontBundleClientWait = 2 * time.Second

// emptyFontBundle is the valid, empty JSON bundle served when an extraction is
// still in flight or has no fonts to return.
var emptyFontBundle = []byte("[]")

// fontBundleCacheKey builds the FontBundleKey for a media file the same way the
// stream font handler does, and is shared with the playback-time pre-warm so
// the two can never drift. Virtual rows key on the pinned result candidate
// because relay URLs rotate per registration; local rows key on the file row's
// size and mtime so a re-probed or replaced file reads as a miss.
func fontBundleCacheKey(file *models.MediaFile, virtualSourceURI, ffmpegPath string) playback.FontBundleKey {
	if file != nil && isVirtualPlaybackFile(file) && virtualSourceURI != "" {
		return playback.FontBundleKey{
			FileID:       file.ID,
			PinnedResult: virtualResultCandidateID(virtualSourceURI),
			FFmpegPath:   ffmpegPath,
		}
	}
	key := playback.FontBundleKey{FFmpegPath: ffmpegPath}
	if file != nil {
		key.FileID = file.ID
		key.Size = file.FileSize
		if file.FileModifiedAt != nil {
			key.MtimeUnixNano = file.FileModifiedAt.UnixNano()
		}
	}
	return key
}

// writeFontBundleResponse writes an encoded font-bundle payload with the
// shared cache headers. Both the cache-hit and cache-miss paths serve the same
// bytes, so the response is identical whichever path produced them.
func writeFontBundleResponse(w http.ResponseWriter, bundle []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "private, max-age=600")
	_, _ = w.Write(bundle)
}

// fontBundlePendingHeader marks a font-bundle response whose extraction is
// still in flight. The web client treats either this header or a no-store
// Cache-Control as pending: it must not persist the empty body as a definitive
// font-less result and instead re-fetches for the completed bundle.
const fontBundlePendingHeader = "X-Vio-Font-Bundle-Pending"

// writePendingFontBundleResponse writes a valid empty bundle for an extraction
// that is still running. Unlike a definitive font-less file, it is marked
// pending and uncacheable, so a client cannot retain the empty state for ten
// minutes while the real bundle is being produced.
func writePendingFontBundleResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(fontBundlePendingHeader, "true")
	_, _ = w.Write(emptyFontBundle)
}

func (h *StreamHandler) syncSessionsNow(ctx context.Context, reason string) {
	if h == nil || h.SessionSyncer == nil {
		return
	}
	if err := h.SessionSyncer.SyncNow(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to sync sessions", "component", "api", "reason", reason, "error", err)
	}
}

func (h *StreamHandler) finalizeSessionAbort(ctx context.Context, session *playback.Session, syncNow bool, syncReason string) {
	if h == nil || session == nil || session.ID == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	if h.AdminStore != nil {
		if err := h.AdminStore.DeleteSession(ctx, session.ID); err != nil {
			slog.ErrorContext(ctx, "failed to delete synced session", "component", "api", "session", session.ID, "error", err)
		}
	}
	if syncNow {
		h.syncSessionsNow(ctx, syncReason)
	}
}

func (h *StreamHandler) abortPlaybackSession(ctx context.Context, session *playback.Session) {
	if h == nil || session == nil || session.ID == "" {
		return
	}
	if err := h.sessionMgr.StopSession(session.ID); err != nil {
		return
	}
	h.finalizeSessionAbort(ctx, session, true, "stream_abort")
	markAttemptStoppedServerSide(ctx, h.PlanStoreV3, h.StreamDeny, session.ID)
}

func (h *StreamHandler) handleTransportStartFailure(ctx context.Context, session *playback.Session, file *models.MediaFile, err error) {
	if ctx == nil || session == nil || err == nil {
		return
	}
	// Client cancellation is not a transport failure: the viewer navigated off
	// (or hls.js gave up) while the upstream fetch was in flight. The request
	// context propagates into the virtual provider resolve / relay, so the
	// upstream work is already canceled. Check this before preflight so a
	// canceled request is never rewritten into a missing-file abort, and
	// downgrade to debug; a timeout or provider error stays at WARN.
	if isClientCancellation(ctx, err) {
		slog.DebugContext(ctx, "stream transport canceled by client",
			"component", "api",
			"session", session.ID,
			"file_id", session.MediaFileID,
			"reason", "client_canceled",
			"playback_session_id", session.ID,
		)
		return
	}
	if preflightErr := preflightPlaybackFile(ctx, file, h.MissingMarker, h.EventsHub); preflightErr != nil {
		err = preflightErr
	}
	if isPlaybackFileMissing(err) || errors.Is(err, os.ErrNotExist) {
		h.abortPlaybackSession(ctx, session)
		return
	}
	slog.WarnContext(ctx, "stream transport startup failed", "component", "api",
		"session", session.ID,
		"file_id", session.MediaFileID,
		"error", err,
		"playback_session_id", session.ID,
	)
}

// markVirtualCandidateFailed stamps the catalog row for a virtual candidate
// as known-bad after a transport produced no bytes, so the auto-pick skips it
// on the next play while the dropdown still shows it (clickable) for a manual
// retry. Best-effort: a persistence failure must not turn a 502 into a 500.
func (h *StreamHandler) markVirtualCandidateFailed(ctx context.Context, file *models.MediaFile, candidateID string) {
	if h == nil || file == nil || candidateID == "" || candidateID != virtualResultCandidateID(file.FilePath) {
		return
	}
	if h.VirtualCandidateFailMarker == nil {
		return
	}
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := h.VirtualCandidateFailMarker(markCtx, file.ID, file.FilePath, file.FailedAt); err != nil {
		slog.WarnContext(ctx, "mark virtual candidate failed", "component", "api", "file_id", file.ID, "candidate", candidateID, "error", err)
	}
}

// clearVirtualCandidateRecovered clears a virtual candidate's known-bad stamp
// after the candidate actually delivered media bytes to a client. This is the
// only evidence that forgives a transport failure: a resolved URL (liveness
// check) is not, because resolution never opens the media, and written
// response headers alone are not either (the relay forwards header-only 204,
// 304, 416, and zero-length 200 responses). The clear is fenced on the
// DELIVERED candidate identity (the file path the transport served, which the
// session retains even after the catalog row rotates) and the failure state
// observed when the transport started, so a late delivery of candidate A never
// clears a rotation to B or a newer failure on A.
// Best-effort: a persistence failure must not fail a delivering stream.
func (h *StreamHandler) clearVirtualCandidateRecovered(ctx context.Context, file *models.MediaFile, deliveredFilePath string, observedFailedAt *time.Time) {
	if h == nil || file == nil || strings.TrimSpace(deliveredFilePath) == "" {
		return
	}
	if h.VirtualCandidateRecoveredMarker == nil {
		return
	}
	clearCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := h.VirtualCandidateRecoveredMarker(clearCtx, file.ID, deliveredFilePath, observedFailedAt); err != nil {
		slog.WarnContext(ctx, "clear virtual candidate recovered", "component", "api", "file_id", file.ID, "delivered", deliveredFilePath, "error", err)
	}
}

// virtualCandidateDeliveryEvidence reports whether a direct-play transfer
// actually delivered media: a 200/206 status AND positive body bytes.
// Header-only responses (204/304/416/zero-length 200) are explicitly forwarded
// by the relay and are not evidence the media endpoint works.
func virtualCandidateDeliveryEvidence(statusCode int, bytesWritten int64) bool {
	if statusCode != http.StatusOK && statusCode != http.StatusPartialContent {
		return false
	}
	return bytesWritten > 0
}

// streamEmbeddedSubtitle runs a dedicated ffmpeg for a single embedded
// track, optionally windowed by explicit client parameters, and pipes its
// stdout directly to w. Because this ffmpeg is independent of the video
// pipeline, it works the same for direct play, remux, and transcode.
func (h *StreamHandler) streamEmbeddedSubtitle(w http.ResponseWriter, r *http.Request, file *models.MediaFile, embeddedIndex int, session *playback.Session, driftSuspected bool, requestedFormat ...string) {
	track := file.SubtitleTracks[embeddedIndex]
	outFormat := "vtt"
	switch {
	case playback.IsASS(track.Codec):
		outFormat = subtitleFormatASS
	case playback.IsPGS(track.Codec):
		outFormat = subtitleFormatSUP
	}

	// A subtitle URL describes the complete track unless the caller supplies
	// an explicit window. Native players fetch once and must retain cues beyond
	// ten minutes and before a resumed playback position. WebVTT and ASS honor
	// explicit ?position/?duration (ASS windows only when a position is given,
	// preserving the whole script for native consumers); PGS window consumers
	// opt in with ?windowed=1.
	allowWindow, seek, duration := subtitleExtractWindow(r, outFormat)
	windowRequested := subtitleWindowRequested(r)
	slog.InfoContext(r.Context(), "subtitle stream requested", "component", "api",
		"file_id", file.ID,
		"embedded_index", embeddedIndex,
		"track_language", track.Language,
		"track_codec", track.Codec,
		"track_probed_index", track.Index,
		"seek_seconds", seek,
		"duration_seconds", duration,
		"virtual_drift", driftSuspected,
	)

	opts := playback.StreamExtractOpts{
		InputPath:       file.FilePath,
		TrackIndex:      embeddedIndex,
		SourceCodec:     track.Codec,
		SeekSeconds:     seek,
		DurationSeconds: duration,
		AllowWindow:     allowWindow,
		WindowRequested: windowRequested,
		FFmpegPath:      h.ffmpegPath(),
	}
	// Virtual sources are provider-neutral URIs, not FFmpeg inputs. Resolve
	// through the relay so ffmpeg reads the real stream. Subtitle extraction
	// spawns its own ffmpeg independent of the video pipeline, so it must
	// resolve separately even though the transcode transport already did.
	releaseInput := func() {}
	virtualResolved := false
	if isVirtualPlaybackFile(file) && hasVirtualMediaResolver(h) && h.RemoteStreamRelay != nil && session != nil {
		resolved, cleanup, resolveErr := h.resolveVirtualInputURI(r.Context(), file, session.UserID, session.ProfileID, false)
		if resolveErr != nil {
			writeError(w, http.StatusBadGateway, subtitleSourceUnavailableErrorCode,
				"Failed to resolve the virtual source for subtitle extraction; the video source is unchanged.")
			return
		}
		opts.InputPath = resolved.URL
		releaseInput = cleanup
		virtualResolved = true
	}
	defer releaseInput()
	if len(requestedFormat) > 0 && requestedFormat[0] == "vtt" {
		// Only text sources can be converted to WebVTT. A bitmap track (PGS
		// reaches here because it is deliverable as .sup; DVD/DVB are rejected
		// upstream) carries no text, so honoring the override would spawn an
		// ffmpeg that always fails after the 200 and headers are committed.
		// Reject before any spawn or header write.
		if playback.NeedsBurnIn(track.Codec) {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
				"Bitmap subtitle tracks cannot be converted to WebVTT")
			return
		}
		opts.TargetFormat = "vtt"
	}

	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Only complete successful extracts enter the cache; explicit windows
	// remain streamed. Keep failures distinguishable from a clean subtitle EOF.
	response := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
	virtualActive := virtualResolved && session != nil && session.VirtualSourceURI != ""

	// Virtual relay inputs never enter the payload cache under their rotating
	// URL; key on the pinned source + effective ordinal instead. The identity
	// is computed before the drift probe so a committed artifact can
	// short-circuit it, and recomputed after a remap to keep the warm and
	// serve keys aligned. The handler owns the detached warm
	// (warmVirtualSubtitleAfterWindowMiss), because this request's relay
	// registration is released when the request ends; the cache's own detached
	// warm is therefore disabled for virtual inputs.
	if virtualActive {
		opts.CacheIdentity = playback.VirtualSubtitleCacheIdentity(file.ID, session.VirtualSourceURI, opts.TrackIndex)
		opts.DisableBackgroundWarm = true
	}

	// Row-vs-evidence drift (flagged in handleSubtitle) means the catalog row
	// no longer describes the release this session planned against. Probe the
	// live relay input once before any spawn or header commit and re-map the
	// plan ordinal onto a same-class live track when the pinned release
	// rotated. Mandatory for PGS, whose .sup response commits 200 before
	// ffmpeg spawns and therefore can never be retrofitted after a failed map.
	// A committed full-track text artifact pins the plan-time release, and a
	// cached-text input remaps to its sole stream, so the probe is unnecessary
	// then — skip its multi-second tax and serve the warm.
	if virtualActive && driftSuspected {
		if artifact, ok := h.resolveCommittedTextSubtitleEntry(&opts); ok {
			// A committed full-track text artifact pins the plan-time release and
			// a windowed extract reads it instead of the container, so the live
			// probe is unnecessary. Pin the exact artifact: the serve uses that
			// path (or reports ErrCommittedTextArtifactGone) rather than
			// re-resolving, so a generation-bucket rollover or eviction cannot
			// turn the validated artifact into an unvalidated source extract.
			opts.PinnedTextArtifact = &artifact
		} else {
			proceed, probeErr := h.verifyVirtualSubtitleLayout(r.Context(), track, file.SubtitleTracks, &opts)
			if !proceed {
				writeSubtitleSourceChanged(w)
				return
			}
			if probeErr != nil {
				// The live layout could not be established (probe error, relay
				// timeout, transient upstream failure). Serving the plan ordinal
				// unverified can silently emit the wrong release's track: the
				// URL names a plan-time ordinal whose validity depended on the
				// release the probe was supposed to confirm. PGS already failed
				// closed here because its .sup commits 200 before ffmpeg spawns.
				// Text/ASS now does the same rather than trusting a post-spawn map
				// error that a same-ordinal different-language rotation would
				// never raise. A retryable error lets the client fall back to
				// subtitles-off, which the server already treats as legitimate.
				clearSubtitleCoverageHeaders(w.Header())
				writeSubtitleSourceUnavailable(w)
				return
			}
			// The probe may have remapped the ordinal; the cache identity must
			// track the effective map so a remapped extraction lands under its
			// own key.
			opts.CacheIdentity = playback.VirtualSubtitleCacheIdentity(file.ID, session.VirtualSourceURI, opts.TrackIndex)
		}
	}

	// A HEAD probe (.sup is the only embedded format that does not early-return)
	// exists to read representation headers, not to extract: keep its previous
	// whole-track/committed behavior and never synthesize a window or warm from
	// it.
	if r.Method != http.MethodHead {
		h.applyImplicitVirtualWindow(&opts, file, session, virtualActive)
	}
	// Advertise the bounded range before any header commits. A whole-track
	// serve (committed artifact, small/local source, or a codec that cannot be
	// sliced) omits the header, so a client that asked for the complete track
	// can tell it received only a window.
	playback.SetSubtitleCoverageHeader(w.Header(), opts)

	_, extractErr := h.SubtitleCache.ServeExtractWithResult(response, r, opts, playback.StreamExtractSubtitle)
	if errors.Is(extractErr, playback.ErrCommittedTextArtifactGone) {
		// The artifact that allowed the drift probe to be skipped was evicted
		// before the serve read it. No response has been written, so drop the
		// pin, validate the live layout, and retry rather than stream an
		// unvalidated source extract.
		opts.PinnedTextArtifact = nil
		proceed, probeErr := h.verifyVirtualSubtitleLayout(r.Context(), track, file.SubtitleTracks, &opts)
		if !proceed {
			writeSubtitleSourceChanged(w)
			return
		}
		if probeErr != nil {
			// The committed artifact is gone and the live layout cannot be
			// established: fail closed rather than stream an unverified
			// extraction. This is the same retryable response PGS uses.
			clearSubtitleCoverageHeaders(w.Header())
			writeSubtitleSourceUnavailable(w)
			return
		}
		opts.CacheIdentity = playback.VirtualSubtitleCacheIdentity(file.ID, session.VirtualSourceURI, opts.TrackIndex)
		// The pinned artifact is gone, so the committed-entry check that
		// suppressed the implicit window on the first attempt no longer holds.
		// Re-evaluate against the now-cold source before retrying.
		h.applyImplicitVirtualWindow(&opts, file, session, virtualActive)
		playback.SetSubtitleCoverageHeader(w.Header(), opts)
		_, extractErr = h.SubtitleCache.ServeExtractWithResult(response, r, opts, playback.StreamExtractSubtitle)
	}
	if extractErr != nil {
		playback.LogSubtitleStreamError(r.Context(), extractErr, file.ID, embeddedIndex)
		if r.Context().Err() != nil {
			clearSubtitleCoverageHeaders(w.Header())
			return
		}
		// A successful HTTP EOF would make clients accept the partial track.
		if response.Status() != 0 {
			panic(http.ErrAbortHandler)
		}
		if !virtualActive || !playback.IsSubtitleStreamMapError(extractErr) {
			clearSubtitleCoverageHeaders(w.Header())
			writeError(w, http.StatusInternalServerError, "subtitle_extract_failed", "Failed to extract subtitles")
			return
		}
		// Post-spawn safety net: the plan ordinal named a subtitle stream the
		// live input does not have, meaning the pinned release rotated between
		// the last probe and this spawn (or no pre-spawn probe ran because the
		// row still matched the plan evidence). Re-probe the already-registered
		// relay URL once — no second resolve — and re-map; a source that still
		// cannot satisfy the requested representation gets a clean retryable 4xx
		// instead of a 500.
		proceed, probeErr := h.verifyVirtualSubtitleLayout(r.Context(), track, file.SubtitleTracks, &opts)
		if !proceed {
			writeSubtitleSourceChanged(w)
			return
		}
		if probeErr != nil {
			// A map error already proved the plan ordinal names no live stream;
			// if the re-probe then cannot establish the layout either, there is
			// no verified ordinal to retry with. Fail closed rather than re-spawn
			// the same unverified extraction.
			clearSubtitleCoverageHeaders(w.Header())
			writeSubtitleSourceUnavailable(w)
			return
		}
		// The retry may have remapped to a different live ordinal; the cache
		// identity must track the effective map so a remapped extraction lands
		// under its own key.
		if virtualActive {
			opts.CacheIdentity = playback.VirtualSubtitleCacheIdentity(file.ID, session.VirtualSourceURI, opts.TrackIndex)
		}
		if retryErr := h.SubtitleCache.ServeExtract(response, r, opts, playback.StreamExtractSubtitle); retryErr != nil {
			playback.LogSubtitleStreamError(r.Context(), retryErr, file.ID, embeddedIndex)
			if r.Context().Err() != nil {
				clearSubtitleCoverageHeaders(w.Header())
				return
			}
			if response.Status() != 0 {
				panic(http.ErrAbortHandler)
			}
			if playback.IsSubtitleStreamMapError(retryErr) {
				writeSubtitleSourceChanged(w)
				return
			}
			clearSubtitleCoverageHeaders(w.Header())
			writeError(w, http.StatusInternalServerError, "subtitle_extract_failed", "Failed to extract subtitles")
			return
		}
	}

	// The windowed request just read the remote source and, by design,
	// committed nothing. Populate the full-track cache so the next window hits
	// the small artifact instead of re-demuxing the source.
	h.warmVirtualSubtitleAfterWindowMiss(file, session, opts, virtualActive)
}

// resolveCommittedTextSubtitleEntry resolves the committed full-track text
// artifact that a windowed extract for this identity would read, returning a
// binding token. Callers that skip track-identity validation because the
// artifact pins the plan-time release must pin this token on the options: the
// serve then reads that exact path (or reports
// ErrCommittedTextArtifactGone) rather than re-resolving, so a generation-bucket
// rollover or eviction between the identity check and the read cannot turn the
// validated artifact into an unvalidated source extract. Bitmap (PGS) codecs, an
// unkeyable source, and a nil cache read as false.
func (h *StreamHandler) resolveCommittedTextSubtitleEntry(opts *playback.StreamExtractOpts) (playback.CommittedTextArtifact, bool) {
	if h == nil || h.SubtitleCache == nil || opts == nil {
		return playback.CommittedTextArtifact{}, false
	}
	return h.SubtitleCache.ResolveCommittedTextEntry(opts.InputPath, opts.CacheIdentity, opts.TrackIndex, opts.SourceCodec, opts.TargetFormat)
}

// virtualSubtitleWarmResolveTimeout bounds the relay resolution a detached
// virtual subtitle warm performs. It is a child of the extraction context the
// cache hands the warm, so resolution can never outlive extraction; a resolver
// that never returns is canceled with the extraction instead of pinning the
// cache's warm slot and in-flight fill for the whole extraction budget. Tests
// override it to keep the release assertion fast.
var virtualSubtitleWarmResolveTimeout = 2 * time.Minute

// warmVirtualSubtitleAfterWindowMiss starts a detached full-track warm for a
// windowed virtual subtitle request (text, ASS/SSA, or PGS) that could not be
// served from the cache. It is called after the window itself has streamed, so
// the warm cannot contend with the triggering request's own read.
//
// The warm resolves and holds its own relay registration: the request's
// registration is released when the request ends, which is exactly why the
// cache's own detached warm is disabled for virtual inputs
// (opts.DisableBackgroundWarm). Resolution happens lazily inside the extract
// closure on a context derived from the extraction context, so the cache's warm
// budget (warmSem) and in-flight coalescing (beginFill) gate it before any
// remote work and a stuck resolver cannot pin either: concurrent window misses
// on the same serve identity resolve once and demux once, and the registration
// is released exactly once when the warm settles.
func (h *StreamHandler) warmVirtualSubtitleAfterWindowMiss(file *models.MediaFile, session *playback.Session, opts playback.StreamExtractOpts, virtualActive bool) {
	if h == nil || h.SubtitleCache == nil || file == nil || session == nil || !virtualActive {
		return
	}
	if opts.CacheIdentity == "" {
		return
	}
	// Every sidecar class gets this handler-owned warm. PGS matters most: its
	// whole-track .sup exceeds a client fetch deadline on a large virtual
	// source, so without a warm no committed artifact ever appears and every
	// repeat is a fresh full remote demux. The warm's mandatory drift probe and
	// progressive .sup handling are unchanged — only the populate step is new.
	// Full-track requests already fill the cache inline through ServeExtract's
	// tee; only explicit windows need a detached warm.
	if opts.SeekSeconds == 0 && opts.DurationSeconds == 0 {
		return
	}
	// A committed entry means this request served its window from the cached
	// artifact; nothing to warm. Format-aware, so a committed .sup suppresses
	// the warm exactly as a committed VTT/ASS does.
	if h.SubtitleCache.HasCommittedEntry(opts.InputPath, opts.CacheIdentity, opts.TrackIndex, opts.SourceCodec, opts.TargetFormat) {
		return
	}

	h.subtitleWarmWG.Add(1)
	go func() {
		defer h.subtitleWarmWG.Done()
		var cleanup func()
		defer func() {
			if cleanup != nil {
				cleanup()
			}
		}()
		extract := func(extractCtx context.Context, extractOpts playback.StreamExtractOpts) error {
			// Resolve on a child of the extraction context, never the detached
			// request context: a resolver that hangs is canceled when the warm
			// budget is exhausted, releasing the warm slot and fill instead of
			// holding them forever.
			resolveCtx, cancel := context.WithTimeout(extractCtx, virtualSubtitleWarmResolveTimeout)
			defer cancel()
			resolved, resolvedCleanup, err := h.resolveVirtualInputURI(resolveCtx, file, session.UserID, session.ProfileID, false)
			if err != nil {
				logVirtualStreamFailure(resolveCtx, session.ID, file, err)
				return fmt.Errorf("resolve virtual input for subtitle warm: %w", err)
			}
			cleanup = resolvedCleanup
			extractOpts.InputPath = resolved.URL
			return playback.StreamExtractSubtitle(extractCtx, extractOpts)
		}
		<-h.SubtitleCache.WarmTrackInBackground(opts, extract)
	}()
}

// Defaults for the implicit first window served to a cold whole-track subtitle
// request against a large/unknown virtual/remote source. The duration matches
// the web player's sliding-window cadence (see web/src/player/hooks/useSubtitleTracks.ts)
// so a client that manages its own windows and one that does not observe the
// same coverage. The backoff pulls the window start behind the session
// position so a small scrub back stays covered, mirroring that player's
// SEEK_BACKOFF.
const (
	virtualSubtitleImplicitWindowSeconds        = 600.0
	virtualSubtitleImplicitWindowBackoffSeconds = 2.0
	// virtualSubtitleImplicitWindowMinSourceBytes is the source size above
	// which a whole-track virtual read is considered untenable and is bounded
	// to the implicit window. A small known source is read whole (cheap and
	// complete); an unknown size (0) is treated as large, because a virtual
	// row's size is not always populated.
	virtualSubtitleImplicitWindowMinSourceBytes = 256 << 20
)

// applyImplicitVirtualWindow bounds a cold whole-track subtitle fetch against a
// large/unknown virtual/remote source to the first playback-sized window
// instead of an unbounded whole-container extract. It applies to every
// sidecar class the server can serve — converted text (WebVTT), lossless
// ASS/SSA, and PGS — because a virtual source is served over HTTP from a
// provider and a complete extract of any class requires demuxing every byte of
// the container. A 6.6 GB remux therefore costs ~100 s of sequential reads
// before the response can complete, while the client's fetch deadline is
// ~30-60 s; the request is aborted and the partial fill is discarded, so
// retrying never makes progress. Local files are cheap to read whole and keep
// the existing whole-track behavior.
//
// The window is only synthesized when the caller did not ask for a specific
// window, the source is not small/known, and no committed full-track artifact
// exists. A windowed serve is fast (ffmpeg seeks near the requested position)
// and the handler starts the usual detached full-track warm afterwards, so this
// request completes in seconds and later requests — or a client that fetched
// this window — read the small cached artifact. When an artifact is already
// present the whole track is served, so an established virtual track still
// returns every cue in one response.
//
// Correctness: this changes what a cold, implicit whole-track virtual response
// contains — a bounded window rather than every cue — so a client that never
// re-requests will stop seeing cues past the window. The response advertises
// the covered range with X-Subtitle-Coverage/X-Subtitle-Windowed so such a
// client can tell and request subsequent windows. Explicitly-windowed callers
// (the web player) are unaffected, and local and small/known sources keep their
// whole-track paths. An explicit position/duration is honored exactly as asked
// and bypasses this default; a client-managed window is never widened.
//
// This deliberately bends the documented default in
// docs/architecture/playback-protocol-v3.md §4.2/§8 ("embedded
// URLs return the complete track from source time zero by default; consumers
// that maintain a sliding window may explicitly supply position and duration").
// A remote multi-GB source makes that default unservable within a client fetch
// deadline. The fully contract-conformant fix is for every client to request
// windows the way the web player does (or a v3 contract amendment making
// windowed sidecars the default for virtual sources); this helper is a
// server-side stopgap until that lands. It is gated to large/unknown virtual
// sources so nothing else changes.
//
// Which cases stay whole-track, and why:
//   - local files: cheap to read whole, and bounding would silently drop cues;
//   - small known sources (< virtualSubtitleImplicitWindowMinSourceBytes):
//     cheap to read whole;
//   - an already-committed full-track artifact: cheap to serve whole and the
//     validated bytes a pinned text artifact promised;
//   - an explicit client window with a duration (position+duration, or PGS
//     ?windowed=1 with a duration): authoritative, never overridden.
//
// A position without a duration is window intent but open-ended: its missing
// cap is filled with the implicit window (the caller's position is kept) so it
// cannot demux to EOF and advertise `start-*`. A codec that cannot window
// (PGS without the ?windowed=1 opt-in) is left untouched.
func (h *StreamHandler) applyImplicitVirtualWindow(opts *playback.StreamExtractOpts, file *models.MediaFile, session *playback.Session, virtualActive bool) {
	if h == nil || opts == nil || !virtualActive {
		return
	}
	// A small known source costs little to read whole, so keep the complete
	// artifact. Unknown size (0) stays windowed: virtual rows do not always
	// carry a populated size.
	if file != nil && file.FileSize > 0 && file.FileSize < virtualSubtitleImplicitWindowMinSourceBytes {
		return
	}
	// An explicit duration (with or without a position) is authoritative;
	// leave it exactly as the client asked.
	if opts.DurationSeconds > 0 {
		return
	}
	// A committed full-track artifact is cheap to serve whole, so do not slice
	// it. (A pinned text artifact resolves as committed here, so the
	// drift-probe skip path also serves whole.)
	if h.SubtitleCache.HasCommittedEntry(opts.InputPath, opts.CacheIdentity, opts.TrackIndex, opts.SourceCodec, opts.TargetFormat) {
		return
	}
	// A position without a duration is window intent but open-ended: fill in the
	// implicit window so it cannot extract to EOF (advertised as `start-*`). The
	// caller's position is kept; only the missing cap is supplied.
	if opts.ClampOpenEndedWindow(virtualSubtitleImplicitWindowSeconds) {
		slog.Info("virtual subtitle open window clamped to implicit window",
			"component", "api",
			"codec", opts.SourceCodec,
			"seek_seconds", opts.SeekSeconds,
			"duration_seconds", opts.DurationSeconds,
			"track", opts.TrackIndex)
		return
	}
	// Any other explicit intent is authoritative: a position-only codec that
	// cannot window, or the PGS ?windowed=1 opt-in with no position/duration.
	if opts.WindowRequested || opts.SeekSeconds > 0 || opts.AllowWindow {
		return
	}

	opts.WindowRequested = true
	opts.SeekSeconds = implicitVirtualWindowStart(session)
	opts.DurationSeconds = virtualSubtitleImplicitWindowSeconds
	if playback.IsPGS(opts.SourceCodec) {
		// PGS routes through ServeSUPExtract, which windows only when
		// AllowWindow is set (the explicit ?windowed=1 opt-in). This window is
		// server-authored, so grant it here.
		opts.AllowWindow = true
	}
	slog.Info("virtual subtitle served as an implicit window",
		"component", "api",
		"codec", opts.SourceCodec,
		"seek_seconds", opts.SeekSeconds,
		"duration_seconds", opts.DurationSeconds,
		"track", opts.TrackIndex)
}

// implicitVirtualWindowStart returns the source-time start of the implicit
// window: the session position pulled back slightly, or zero when the session
// has no meaningful position yet (a fresh start, so the window covers the
// opening of the track).
func implicitVirtualWindowStart(session *playback.Session) float64 {
	if session == nil || session.Position <= virtualSubtitleImplicitWindowBackoffSeconds {
		return 0
	}
	return session.Position - virtualSubtitleImplicitWindowBackoffSeconds
}

// verifyVirtualSubtitleLayout probes the live relay input once and, when its
// subtitle layout drifted from the layout this request's ordinal was resolved
// against (expected), re-maps the extract options onto a same-class live track.
// It reports whether extraction may proceed, plus the probe error when the
// probe itself could not run. proceed=false means a successful probe positively
// found that the live source cannot satisfy the requested representation —
// rotation to a different subtitle class, or an ambiguous or absent match — and
// the caller must answer with a clean retryable 4xx before ffmpeg spawns or
// headers commit. A non-nil probeErr means the live layout could not be
// established: the caller must fail closed for every codec, because serving the
// plan ordinal unverified can emit the wrong release's track and a
// same-ordinal rotation never trips the post-spawn map net. Virtual inputs are
// request-local probe state; the session's published evidence is never
// rewritten.
func (h *StreamHandler) verifyVirtualSubtitleLayout(ctx context.Context, requestedTrack models.SubtitleTrack, expected []models.SubtitleTrack, opts *playback.StreamExtractOpts) (bool, error) {
	if opts == nil || strings.TrimSpace(opts.InputPath) == "" {
		return true, nil
	}
	liveTracks, err := playback.ProbeSubtitleLayout(ctx, h.ffmpegPath(), opts.InputPath)
	if err != nil {
		// A probe failure (context canceled, relay timeout, transient upstream
		// error) is not evidence that the pinned source rotated, but without
		// the live layout there is nothing to validate the plan ordinal
		// against. The caller fails closed with a retryable response; a client
		// falls back to subtitles-off, which the server already treats as
		// legitimate. The error is returned so the caller can distinguish it
		// from a positive mismatch.
		slog.WarnContext(ctx, "virtual subtitle layout probe failed", "component", "api",
			"track_codec", requestedTrack.Codec,
			"error", err)
		return true, err
	}
	if playback.SubtitleLayoutsEqual(liveTracks, expected) {
		// The release the ordinal was resolved against is unchanged. The plan
		// ordinal already names the live layout.
		return true, nil
	}
	liveOrdinal, liveTrack, matched := playback.MatchEmbeddedSubtitleTrack(requestedTrack, liveTracks)
	if !matched {
		slog.WarnContext(ctx, "virtual subtitle layout rotated without a usable match", "component", "api",
			"requested_codec", requestedTrack.Codec,
			"requested_language", requestedTrack.Language,
			"live_subtitle_count", len(liveTracks))
		return false, nil
	}
	// Class preservation is the hard rule: the URL extension was minted at
	// plan time, so a re-map may only land on a codec whose extraction uses
	// the same output muxer.
	if playback.SubtitleExtractMuxer(requestedTrack.Codec, opts.TargetFormat) != playback.SubtitleExtractMuxer(liveTrack.Codec, opts.TargetFormat) {
		slog.WarnContext(ctx, "virtual subtitle remap rejected: output muxer mismatch", "component", "api",
			"plan_codec", requestedTrack.Codec,
			"live_codec", liveTrack.Codec)
		return false, nil
	}
	planOrdinal := opts.TrackIndex
	opts.TrackIndex = liveOrdinal
	opts.SourceCodec = liveTrack.Codec
	slog.InfoContext(ctx, "virtual subtitle track remapped onto live layout", "component", "api",
		"plan_ordinal", planOrdinal,
		"live_ordinal", liveOrdinal,
		"codec", liveTrack.Codec,
		"language", liveTrack.Language)
	return true, nil
}

// clearSubtitleCoverageHeaders removes the bounded-window markers before an
// error response is written. SetSubtitleCoverageHeader records the window
// before the serve so a successful response advertises its covered range, but
// an error that never committed a 200 must not be readable as a bounded window
// by a header-only classifier. Safe to call before the headers are set.
func clearSubtitleCoverageHeaders(h http.Header) {
	if h == nil {
		return
	}
	h.Del(playback.SubtitleCoverageHeader)
	h.Del(playback.SubtitleWindowedHeader)
}

// writeSubtitleSourceChanged answers a clean retryable 4xx when a virtual
// release rotated so the requested subtitle representation can no longer be
// produced from the live source. Clients already retry through the
// sliding-window fetcher / replan flow, so the response is deliberately a
// retryable 4xx, never a 500 or an ambiguous partial stream.
func writeSubtitleSourceChanged(w http.ResponseWriter) {
	clearSubtitleCoverageHeaders(w.Header())
	writeError(w, http.StatusConflict, "subtitle_source_changed",
		"The selected subtitle track changed on the media source; retry")
}

// writeSubtitleSourceUnavailable answers a retryable 503 when a virtual bitmap
// (PGS) source's live layout could not be established before its .sup response
// would commit 200. A .sup commits its status before ffmpeg spawns and has no
// post-spawn recovery, so the server fails closed rather than stream a
// possibly-truncated track; the client can retry once the source settles.
func writeSubtitleSourceUnavailable(w http.ResponseWriter) {
	clearSubtitleCoverageHeaders(w.Header())
	writeError(w, http.StatusServiceUnavailable, "subtitle_source_unavailable",
		"Unable to verify the subtitle source; retry")
}

// subtitleExtractWindow resolves the extraction window for an embedded
// subtitle request from its explicit query parameters. WebVTT and ASS slices
// read ?position/?duration directly; the explicit window intent is carried
// separately on StreamExtractOpts.WindowRequested (see subtitleWindowRequested)
// so a position=0 window is not mistaken for a whole-track fetch. PGS requires
// the explicit ?windowed=1 opt-in via PGSWindowRequest so a whole-track consumer
// never silently loses cues outside an implicit window.
func subtitleExtractWindow(r *http.Request, outFormat string) (allowWindow bool, seek, duration float64) {
	switch outFormat {
	case "vtt", subtitleFormatASS:
		return false, subtitleSeekPosition(r), subtitleWindowDuration(r)
	case subtitleFormatSUP:
		return playback.PGSWindowRequest(r.URL.Query())
	}
	return false, 0, 0
}

// subtitleWindowRequested reports whether the caller explicitly asked for a
// bounded window by supplying a position parameter. A present position counts
// even at zero: position=0&duration=600 is the first bounded slice, whereas a
// request that omits position (and duration) is a whole-track fetch. Only an
// explicit position sets the intent; a duration-only request keeps its existing
// behavior (text is still capped by DurationSeconds, ASS stays whole-track), so
// this cannot silently turn an ordinary artifact fetch into a window.
func subtitleWindowRequested(r *http.Request) bool {
	raw := strings.TrimSpace(r.URL.Query().Get("position"))
	if raw == "" {
		return false
	}
	v, err := strconv.ParseFloat(raw, 64)
	return err == nil && v >= 0 && !math.IsInf(v, 0) && !math.IsNaN(v)
}

// subtitleSeekPosition uses only the caller's explicit position. Session
// progress must never silently remove cues from a complete subtitle artifact.
func subtitleSeekPosition(r *http.Request) float64 {
	if raw := r.URL.Query().Get("position"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v >= 0 && !math.IsInf(v, 0) && !math.IsNaN(v) {
			return v
		}
	}
	return 0
}

// subtitleWindowDuration bounds extraction only when the client explicitly
// requests a valid duration. Ordinary artifact consumers fetch the whole track.
func subtitleWindowDuration(r *http.Request) float64 {
	const maxDuration = 3600.0
	if raw := r.URL.Query().Get("duration"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 && v <= maxDuration {
			return v
		}
	}
	return 0
}
