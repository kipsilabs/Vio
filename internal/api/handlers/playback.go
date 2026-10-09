package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/clientip"
	"github.com/Silo-Server/silo-server/internal/config"
	evt "github.com/Silo-Server/silo-server/internal/events"
	"github.com/Silo-Server/silo-server/internal/httpheader"
	"github.com/Silo-Server/silo-server/internal/httpstream"
	"github.com/Silo-Server/silo-server/internal/logredact"
	"github.com/Silo-Server/silo-server/internal/markers"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/netaccess"
	"github.com/Silo-Server/silo-server/internal/nodepool"
	"github.com/Silo-Server/silo-server/internal/noderouting"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
	"github.com/Silo-Server/silo-server/internal/scanner"
	"github.com/Silo-Server/silo-server/internal/settingscontract"
	"github.com/Silo-Server/silo-server/internal/settingskeys"
	"github.com/Silo-Server/silo-server/internal/settingsresolve"
	"github.com/Silo-Server/silo-server/internal/streamtelemetry"
	"github.com/Silo-Server/silo-server/internal/streamtoken"
	"github.com/Silo-Server/silo-server/internal/subtitles"
	"github.com/Silo-Server/silo-server/internal/telemetry"
	"github.com/Silo-Server/silo-server/internal/tonemap"
	"github.com/Silo-Server/silo-server/internal/transcodenode"
	"github.com/Silo-Server/silo-server/internal/transcodeproxy"
	"github.com/Silo-Server/silo-server/internal/userstore"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/watchstate"
	"github.com/Silo-Server/silo-server/internal/watchsync"
)

// SessionManagerInterface defines the operations the PlaybackHandler needs
// on the session manager.
type SessionManagerInterface interface {
	StartSession(userID int, profileID string, fileID int, method playback.PlayMethod, transcodeAudio bool) (*playback.Session, error)
	StartSessionWithFiles(userID int, profileID string, effectiveFileID int, requestedFileID int, method playback.PlayMethod, transcodeAudio bool) (*playback.Session, error)
	// StartSessionWithFilesContext is required rather than probed for at run
	// time: the context is how the reporting client's identity reaches the new
	// session, so an implementation without it would start sessions that
	// silently carry no client name, version, build or channel.
	StartSessionWithFilesContext(ctx context.Context, userID int, profileID string, effectiveFileID int, requestedFileID int, method playback.PlayMethod, transcodeAudio bool) (*playback.Session, error)
	UpdateProgress(sessionID string, position float64, isPaused bool) error
	UpdateAudioTrack(sessionID string, audioTrackIndex int, method playback.PlayMethod) error
	UpdateStreamState(sessionID string, state playback.SessionStreamState) error
	TouchActivity(sessionID string) error
	BeginTransport(sessionID string) error
	EndTransport(sessionID string) error
	SetRemoteTransport(sessionID string, remote bool) error
	SetTranscodeNodeURL(sessionID, url string) error
	SetTranscodeRoute(sessionID string, route playback.TranscodeRoute) error
	ApplyReplacement(sessionID string, replacement playback.SessionReplacement) (playback.SessionReplacementRollback, error)
	ApplyReplacementIfRoute(sessionID string, expected playback.TranscodeRoute, replacement playback.SessionReplacement) (playback.SessionReplacementRollback, bool, error)
	RollbackReplacement(sessionID string, rollback playback.SessionReplacementRollback) error
	SetRealtimeConnection(sessionID string, connected bool) error
	SetEffectiveMediaFileID(sessionID string, fileID int) error
	SetProgressPersistenceDisabled(sessionID string, disabled bool) error
	StopSession(sessionID string) error
	GetSession(sessionID string) (*playback.Session, error)
}

type transcodePermissionChecker interface {
	CheckTranscodingAllowed(ctx context.Context, userID int, requiresVideoTranscode bool) error
}

func (h *PlaybackHandler) ensureUserTranscodingAllowed(w http.ResponseWriter, r *http.Request, userID int, requiresVideoTranscode bool) bool {
	checker, ok := h.sessionMgr.(transcodePermissionChecker)
	if !ok {
		return true
	}
	if err := checker.CheckTranscodingAllowed(r.Context(), userID, requiresVideoTranscode); err != nil {
		if errors.Is(err, playback.ErrTranscodingDisabled) {
			writeError(w, http.StatusForbidden, "transcoding_disabled", "Transcoding is disabled for your user")
			return false
		}
		if errors.Is(err, playback.ErrAudioTranscodingDisabled) {
			writeError(w, http.StatusForbidden, "audio_transcoding_disabled", "Audio transcoding is disabled for your user")
			return false
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to verify transcoding access")
		return false
	}
	return true
}

type PlaybackItemAccessChecker interface {
	EnsureAccessible(ctx context.Context, contentID string, filter catalog.AccessFilter) error
}

type PlaybackItemLookup interface {
	GetByID(ctx context.Context, contentID string) (*models.MediaItem, error)
}

type PlaybackEpisodeLookup interface {
	GetByID(ctx context.Context, contentID string) (*models.Episode, error)
}

// PlaybackExtraLookup resolves local extras (media_extras) so their files
// authorize through the parent item, like episodes authorize through their
// series.
type PlaybackExtraLookup interface {
	GetByID(ctx context.Context, contentID string) (*models.MediaExtra, error)
}

type PlaybackSessionSyncer interface {
	SyncNow(ctx context.Context) error
}

// PlaybackSettingsReader reads server settings for playback decisions.
type PlaybackSettingsReader interface {
	Get(ctx context.Context, key string) (string, error)
}

// PlaybackFileVersionFetcher retrieves alternate file versions for a content item.
type PlaybackFileVersionFetcher interface {
	GetByContentID(ctx context.Context, contentID string) ([]*models.MediaFile, error)
	GetByEpisodeID(ctx context.Context, episodeID string) ([]*models.MediaFile, error)
}

type PlaybackProbeEnsurer interface {
	EnsureProbeOnly(ctx context.Context, file *models.MediaFile) (*models.MediaFile, error)
	EnsureCopySafetyCached(ctx context.Context, file *models.MediaFile) (*models.MediaFile, error)
}

type PlaybackChapterThumbnailQueuer interface {
	QueuePriorityFileAtPosition(ctx context.Context, fileID int, targetSeconds float64)
}

// ResolvedVirtualMedia represents a resolved virtual stream with provider details,
// temporary URL, candidate identity, and proxy request headers.
type ResolvedVirtualMedia struct {
	URL            string
	URI            string
	CandidateID    string
	RequestHeaders map[string]string
	ExpiresAt      time.Time
	// OwnerID is the plugin installation that served the candidate, stamped
	// by the provider resolver. It is the runtime inheritance for a virtual
	// file whose stored owner is 0 and takes precedence over the file owner
	// when deciding whether allow_private_streams applies.
	OwnerID int
	// ProviderVideoHash, ProviderGUID, ProviderReleaseName and
	// ProviderReleaseSize are the resolved candidate's durable identity, in
	// the same tier order as the dedup key. The persistence path stores them
	// on the candidate row so it can be re-matched after a re-list.
	ProviderVideoHash   string
	ProviderGUID        string
	ProviderReleaseName string
	ProviderReleaseSize int64
	// IdentityRematched is true when the requested pin's result id was absent
	// from a fresh listing but the resolver found the same durable identity
	// under a new result id. The resolved candidate is then the same release
	// re-identified, not a substitution, and the caller adopts the new
	// identity instead of reporting a release swap.
	IdentityRematched bool
	// CodecAudio, AudioLanguages and SubtitleLanguages are the resolved
	// candidate's provider-declared inventory. They are not probe evidence:
	// they come from the release metadata, may be incomplete or wrong, and are
	// only used to seed a declared inventory while the real probe catches up.
	CodecAudio        string
	AudioLanguages    []string
	SubtitleLanguages []string
}

// effectiveVirtualOwner returns the first positive installation owner from the
// candidates, or 0 when none is known. Order matters: a provider-resolved
// owner is authoritative and can never be masked by a 0 file owner. A file row
// with owner 0 inherits the parent item's virtual_owner_installation_id in the
// catalog (internal/catalog/item_repo.go), and the resolver surfaces that
// inherited owner as ResolvedVirtualMedia.OwnerID, so callers never re-read
// media_items themselves. A legacy row with no resolved and no stored owner
// yields 0, which keeps the strict SSRF validator.
func effectiveVirtualOwner(owners ...int) int {
	for _, owner := range owners {
		if owner > 0 {
			return owner
		}
	}
	return 0
}

// isClientCancellation reports whether err is the viewer going away rather
// than a transport or provider failure. The request context being canceled is
// authoritative: net/http surfaces a canceled request as a wrapped *url.Error,
// so errors.Is(err, context.Canceled) catches both the direct and wrapped
// forms, and a non-nil canceled ctx catches an error the provider masked.
// DeadlineExceeded is deliberately NOT client cancellation: it is a real
// timeout and must keep its WARN.
func isClientCancellation(ctx context.Context, err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	return ctx != nil && errors.Is(ctx.Err(), context.Canceled)
}

// logVirtualStreamFailure records the sanitized cause behind a virtual-stream
// 502. A provider URL can be embedded in a wrapped *url.Error, so the cause is
// passed through logredact; only the provider-neutral virtual URI, the file,
// the session, the pinned and delivered candidate identities, and the owner
// installation are logged directly.
//
// deliveredPath is the virtual:// path the transport actually served (the
// ?result= identity the bytes came from), or "" when the failure happened
// before any candidate was served — a resolve failure, an unparseable relay
// target, a lapsed-registration retry that never re-resolved. It must be
// passed explicitly because the catalog row (file.FilePath) still names the
// pinned candidate after a heal or rotation rebinds the served path: reading
// the identity off the row would blame the pin for a sibling's failure.
// pinned_candidate_id is the ?result= on the row; delivered_candidate_id is
// the ?result= on deliveredPath, empty when nothing was served. The two agree
// on the common path and disagree exactly when attribution matters.
//
// A client cancellation is not a transport failure: the upstream fetch runs on
// the request context, so the viewer navigating away (or hls.js giving up)
// already canceled it. That case is a debug line with an explicit reason so it
// cannot be mistaken for a provider outage; genuine timeouts and provider
// errors stay at WARN.
func logVirtualStreamFailure(ctx context.Context, sessionID string, file *models.MediaFile, err error, deliveredPath ...string) {
	if err == nil || file == nil {
		return
	}
	pinnedID := virtualCandidateID(file)
	deliveredID := ""
	if len(deliveredPath) > 0 {
		deliveredID = virtualResultCandidateID(deliveredPath[0])
	}
	if isClientCancellation(ctx, err) {
		slog.DebugContext(ctx, "virtual stream transport canceled by client",
			"component", "api",
			"session", sessionID,
			"playback_session_id", sessionID,
			"file_id", file.ID,
			"pinned_candidate_id", pinnedID,
			"delivered_candidate_id", deliveredID,
			"reason", "client_canceled",
		)
		return
	}
	slog.WarnContext(ctx, "virtual stream transport failed",
		"component", "api",
		"session", sessionID,
		"playback_session_id", sessionID,
		"file_id", file.ID,
		"pinned_candidate_id", pinnedID,
		"delivered_candidate_id", deliveredID,
		"owner_installation_id", file.VirtualOwnerInstallationID,
		"virtual_uri", file.FilePath,
		"error", logredact.SanitizeURLError(err),
	)
}

type VirtualMediaResolver interface {
	ResolveVirtualMedia(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string) (string, error)
}

type VirtualMediaResolverFunc func(context.Context, string, int, int, string) (string, error)

func (f VirtualMediaResolverFunc) ResolveVirtualMedia(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string) (string, error) {
	return f(ctx, virtualURI, ownerInstallationID, userID, profileID)
}

type VirtualMediaRefreshResolver interface {
	RefreshVirtualMedia(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string) (string, error)
}

type VirtualMediaRefreshResolverFunc func(context.Context, string, int, int, string) (string, error)

func (f VirtualMediaRefreshResolverFunc) RefreshVirtualMedia(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string) (string, error) {
	return f(ctx, virtualURI, ownerInstallationID, userID, profileID)
}

type VirtualMediaDetailedResolver interface {
	ResolveVirtualMediaDetailed(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string, forceRefresh bool, excludedCandidateIDs []string, preferredCandidateID string) (ResolvedVirtualMedia, error)
}

type VirtualMediaDetailedResolverFunc func(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string, forceRefresh bool, excludedCandidateIDs []string, preferredCandidateID string) (ResolvedVirtualMedia, error)

func (f VirtualMediaDetailedResolverFunc) ResolveVirtualMediaDetailed(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string, forceRefresh bool, excludedCandidateIDs []string, preferredCandidateID string) (ResolvedVirtualMedia, error) {
	return f(ctx, virtualURI, ownerInstallationID, userID, profileID, forceRefresh, excludedCandidateIDs, preferredCandidateID)
}

type VirtualPlaybackSourceProber func(context.Context, string, *models.MediaFile) (*models.MediaFile, error)
type VirtualPlaybackSourceProberWithHeaders func(context.Context, string, *models.MediaFile, map[string]string) (*models.MediaFile, error)

// VirtualProbeCacheLookup returns a completed probe from the virtual probe
// cache without starting one. A nil result means no completed probe is
// available. It lets the probe-failure damper recover evidence from a probe
// that outlived the caller's wait instead of leaving the row unprobed.
type VirtualProbeCacheLookup func(sourceURL string, file *models.MediaFile) *models.MediaFile

// VirtualFileSaver atomically persists probed virtual inventory and optionally
// adopts a new file_path in a single CAS-fenced UPDATE. Returns the number of
// rows updated (0 means the snapshot was stale — a newer write landed first).
type VirtualFileSaver func(ctx context.Context, args models.VirtualFilePersistArgs) (int64, error)

// VirtualFileMetadataSaver is VirtualFileSaver with an explicit result that
// separates metadata persistence from identity adoption. Prefer it wherever
// the caller must distinguish "evidence landed" from "the row adopted the
// requested path"; the row-count contract cannot express that difference.
type VirtualFileMetadataSaver func(ctx context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error)

// VirtualPlaybackSourceProber resolves a virtual provider URL and probes the
// stream metadata.

// SubtitleSearchTrigger fires a background subtitle search when a virtual
// stream enters playback without embedded or external subtitle tracks.
type SubtitleSearchTrigger func(ctx context.Context, contentID, imdbID, title string, year, season, episode, fileID int, languages []string)

// PlaybackOriginalLanguageLookup fetches the original language for a content item.
type PlaybackOriginalLanguageLookup interface {
	GetOriginalLanguage(ctx context.Context, contentID string) (string, error)
}

type copySeekAnchorResolver func(
	ctx context.Context,
	ffmpegPath string,
	inputPath string,
	requestedSeekSeconds float64,
	segmentDuration int,
) (float64, int, error)

// VirtualFileLookup looks up an existing media file row by its file path or virtual URI.
type VirtualFileLookup func(ctx context.Context, path string) (*models.MediaFile, error)

type VirtualCandidateFileLookup func(ctx context.Context, path, contentID, episodeID string, ownerInstallationID int) (*models.MediaFile, error)

var ErrVirtualCandidateNotFound = errors.New("virtual candidate not found")

func isVirtualCandidateNotFound(err error) bool {
	return err != nil && (errors.Is(err, ErrVirtualCandidateNotFound) || errors.Is(err, scanner.ErrFileNotFound))
}

type VirtualPlaybackPrefetchRequest struct {
	FileIDs []int `json:"file_ids"`
}

func (h *PlaybackHandler) HandlePrefetchVirtualPlayback(w http.ResponseWriter, r *http.Request) {
	var req VirtualPlaybackPrefetchRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.FileIDs) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "At least one file ID is required")
		return
	}
	if len(req.FileIDs) > maxVirtualPlaybackPrefetchFiles {
		req.FileIDs = req.FileIDs[:maxVirtualPlaybackPrefetchFiles]
	}
	files := make([]*models.MediaFile, 0, len(req.FileIDs))
	for _, id := range req.FileIDs {
		if id <= 0 {
			continue
		}
		file, err := h.loadAuthorizedFile(r, id)
		if err != nil || file == nil || !isVirtualPlaybackFile(file) {
			continue
		}
		files = append(files, file)
	}
	h.PrefetchVirtualPlayback(r.Context(), files, apimw.GetProfileID(r.Context()))
	w.WriteHeader(http.StatusAccepted)
}

// VirtualContentFileLookup looks up an existing media file row by its content ID.
type VirtualContentFileLookup func(ctx context.Context, contentID string) (*models.MediaFile, error)

// VirtualEpisodeFileLookup looks up an existing media file row by its episode ID.
type VirtualEpisodeFileLookup func(ctx context.Context, episodeID string) (*models.MediaFile, error)

// PlaybackHandler handles playback session HTTP endpoints.
type PlaybackHandler struct {
	sessionMgr                  SessionManagerInterface
	fileResolver                FilePathResolver // optional; enables stream_url in responses
	VirtualPlaybackResolver     VirtualPlaybackResolver
	VirtualPlaybackStreamLister VirtualPlaybackStreamLister
	VirtualPlaybackStreamSink   VirtualPlaybackStreamSink
	VirtualFileLookup           VirtualFileLookup
	VirtualCandidateFileLookup  VirtualCandidateFileLookup
	VirtualContentFileLookup    VirtualContentFileLookup
	VirtualEpisodeFileLookup    VirtualEpisodeFileLookup
	StoreProvider               userstore.UserStoreProvider // optional; enables progress/history persistence
	WatchScrobbler              PlaybackWatchScrobbler
	StableIdentityResolver      *watchstate.StableIdentityResolver
	CompletionObserver          watchstate.CompletionObserver // optional; auto-removes watched items from the watchlist
	profileStaler               ProfileStaler
	profileRefreshRequester     ProfileRefreshRequester
	AdminStore                  PlaybackAdminStore    // optional; enables admin playback history/live session cleanup
	SessionSyncer               PlaybackSessionSyncer // optional; enables immediate session sync to shared admin view
	EventsHub                   *evt.Hub
	MissingMarker               MissingFileMarker
	NodePlanner                 nodepool.SessionPlanner   // optional; enables proxy/transcode node selection
	JWTSecret                   string                    // needed for signing stream tokens
	StreamTelemetry             *streamtelemetry.Registry // local observation-only telemetry
	StreamDeny                  *playback.StreamDeny
	// InstallationID is diagnostics.ServerInstanceID; v2 playback mutations
	// carry it and are refused when it differs. Empty leaves v2 unconfigured.
	InstallationID string
	// WatchTogetherAvailable is set when the room service and authenticated
	// socket are wired, so capability discovery reflects their dependencies.
	WatchTogetherAvailable bool
	// progressSideEffectLocks serializes per-session handler side effects (v2
	// progress persistence and inventory-updated publishes; see persistProgressV2
	// and publishInventoryUpdatedToSession). It is reference-counted and bounded:
	// the entry is created on the first acquire for a session and deleted when the
	// last in-flight holder releases, so the map tracks concurrent writers rather
	// than every session the process has ever served. progressSideEffectLocksMu
	// guards the map and each entry's refcount; the entry's own mutex is the
	// per-session side-effect lock.
	progressSideEffectLocks   map[string]*progressSideEffectLockEntry
	progressSideEffectLocksMu sync.Mutex
	// virtualDeliveryCleared records playback sessions whose first fully
	// delivered HLS/transcode segment already recorded delivery evidence and
	// cleared the candidate's failed mark. One entry per served session keeps a
	// long segment stream from issuing a read+write per segment; a newer failure
	// after the first delivery is preserved, mirroring the direct-play path's
	// transport-start capture. The entry is dropped on session end (see
	// forgetProgressSideEffectLock), so it is bounded by live sessions; unlike
	// the side-effect lock it is not refcounted, because a late duplicate
	// segment request simply records the (idempotent, fenced) evidence again.
	virtualDeliveryCleared sync.Map
	// ProxyGrantStore hands a proxy the recipe it serves a header-authenticated
	// session from. Optional: without it an attempt that negotiated
	// authorized_media_origins_v1 simply stays on the API origin.
	ProxyGrantStore recipeCardStoreV3
	// NodeRecipeStore hands a transcode node the recipe it rebuilds a
	// header-authenticated remote transcode from after its own restart, keyed by
	// the transport id the node serves it under. It is also the active authority
	// for transcode-executed progressive remuxes, whose signed tokens otherwise
	// outlive a node's in-memory stop fence. Without a usable store those remuxes
	// use another legal route; tokenless HLS sessions replan instead of recovering.
	NodeRecipeStore recipeCardStoreV3
	ItemAccess      PlaybackItemAccessChecker // optional; enables file authorization checks
	ItemLookup      PlaybackItemLookup
	EpisodeLookup   PlaybackEpisodeLookup // optional; resolves episode files to their series
	// virtualSticky remembers the last virtual candidate URI that played
	// successfully per content key, so candidate rotation between equally
	// ranked releases cannot churn a viewer's session (and invalidate cached
	// client track identities) while the pinned source stays healthy.
	virtualStickyMu    sync.Mutex
	virtualStickyPins  map[string]virtualStickyPin
	ExtraLookup        PlaybackExtraLookup // optional; resolves extras files to their parent item
	OriginalLangLookup PlaybackOriginalLanguageLookup
	SettingsRepo       PlaybackSettingsReader     // optional; reads server settings (e.g., allow_4k_transcode)
	FileVersionFetcher PlaybackFileVersionFetcher // optional; queries sibling file versions for 4K guard
	ProbeEnsurer       PlaybackProbeEnsurer       // optional; repairs missing probe metadata on demand
	// probeRefreshed memoizes start-path probe refreshes by file id so a burst
	// of starts for one file schedules a single detached repair. probeStartBudget
	// overrides the default bounded wait in tests. probeRefreshWG tracks the
	// detached refreshes so tests can wait for them; production never waits.
	probeRefreshMu   sync.Mutex
	probeRefreshed   map[int]*playbackProbeRefresh
	probeStartBudget time.Duration
	probeRefreshWG   sync.WaitGroup
	// CopySafetyRacer resolves an unknown H.264 copy-safety verdict behind an
	// already-issued stream-copy plan. Optional: nil keeps unknown verdicts
	// unknown and never withdraws a copy route.
	CopySafetyRacer        PlaybackCopySafetyRacer
	ChapterThumbnailQueuer PlaybackChapterThumbnailQueuer
	IntroAnalyzer          PlaybackEpisodeAnalyzer
	IntroRepository        PlaybackIntroEligibilityChecker
	MarkerRegistry         *markers.Registry
	MarkerPopulation       MarkerPopulationService
	MarkerUpdateNotifier   PlaybackMarkerUpdateNotifier
	StartTranscodeFunc     func(context.Context, playback.TranscodeOpts) (*playback.TranscodeSession, error)
	MarkerLazyContext      context.Context
	MarkerLazyInFlight     sync.Map
	v3StartEffectsOnce     sync.Once
	v3StartEffectsQueue    chan playbackStartSideEffectsV3
	v3StartEffectsMu       sync.Mutex
	v3StartEffectsPending  map[string]*playbackStartSideEffectsStateV3
	SubtitleRepo           subtitles.Repository // optional; enables downloaded subtitles in playback
	// SubtitleCache warms virtual subtitle extracts at plan time so the
	// first subtitle click is served from cache instead of paying a full
	// remote demux. Shared with StreamHandler's serve path — wired in the
	// router from the single NewSubtitleCache instance.
	SubtitleCache                *playback.SubtitleCache
	RealtimeHub                  *playback.RealtimeHub
	CommandTracker               *playback.CommandTracker
	CommandDispatcher            *playback.CommandDispatcher
	VirtualMediaResolver         VirtualMediaResolver
	VirtualMediaRefreshResolver  VirtualMediaRefreshResolver
	VirtualMediaDetailedResolver VirtualMediaDetailedResolver
	RemoteStreamRelay            *remotestream.Relay
	// RelayRegistrationStatus reports whether a pinned relay URL still names a
	// live registration. Optional; when nil the handler queries
	// RemoteStreamRelay. It lets tests exercise absent, expired, evicted, and
	// upstream-rejected pins without reaching into relay internals.
	RelayRegistrationStatus func(relayURL string) remotestream.RegistrationStatus
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
	// VirtualCandidateRecoveredMarker clears a virtual candidate's known-bad
	// stamp after an HLS/transcode delivery actually served a full segment to
	// the client. It mirrors StreamHandler.VirtualCandidateRecoveredMarker:
	// the write is fenced on the delivered candidate identity and the failure
	// state observed before the delivering request, so a rotation or a newer
	// failure is never cleared. Nil disables the clear.
	VirtualCandidateRecoveredMarker func(ctx context.Context, fileID int, deliveredFilePath string, observedFailedAt *time.Time) error
	// VirtualCandidateFailMarker stamps a virtual candidate row as known-bad
	// after the start path resolved it and got a confirmed-dead verdict (the
	// provider listed candidates and the pinned release is gone or unusable), so
	// a retry within the verdict window rotates to a sibling or fails fast
	// instead of re-resolving the same dead candidate. It mirrors
	// StreamHandler.VirtualCandidateFailMarker and the versions check's
	// stampVirtualCandidateFailed: the write is fenced on the candidate identity
	// and observed failed_at, and an empty provider listing is deliberately NOT
	// a dead verdict (see isVirtualCandidateDeadError). Nil disables the stamp.
	VirtualCandidateFailMarker func(ctx context.Context, fileID int, expectedFilePath string, observedFailedAt *time.Time) error
	// VirtualCandidateClearFailedMarker clears a virtual candidate's failed_at
	// verdict after a successful same-identity re-resolve that the explicit
	// retry (allowFailedCandidate) or stale-pin fall-through let through, so the
	// next replay is eligible for the P0 repeat-play fast path again. It is the
	// playback-path counterpart of CatalogResourceHandler.ClearVirtualFailed
	// (the versions-check liveness recovery) and is fenced on the same identity
	// and observed verdict, so a rotation or a newer failure survives it. Nil
	// disables the clear.
	VirtualCandidateClearFailedMarker      func(ctx context.Context, fileID int, expectedFilePath string, observedFailedAt *time.Time) error
	VirtualPlaybackSourceProber            VirtualPlaybackSourceProber
	VirtualPlaybackSourceProberWithHeaders VirtualPlaybackSourceProberWithHeaders
	VirtualProbeCacheLookup                VirtualProbeCacheLookup
	BestResultCache                        *VirtualBestResultCache
	VirtualFileSaver                       VirtualFileSaver
	// VirtualFileMetadataSaver, when wired, is preferred over VirtualFileSaver
	// by paths that must distinguish metadata persistence from identity
	// adoption (stale fallback, evidence workers). Nil keeps legacy behavior.
	VirtualFileMetadataSaver VirtualFileMetadataSaver
	VirtualSubtitleSearcher  SubtitleSearchTrigger
	SubtitleSearchInFlight   *sync.Map
	DeviceCapabilitySource   DeviceCapabilityProfileSource
	// PlaybackConfig returns the current playback config (ffmpeg path,
	// hwaccel, transcode dir). Wired to the live config in integrated mode
	// so admin changes apply to newly started transcodes. Read it through
	// playbackConfig(), which falls back to defaults when unset.
	PlaybackConfig func() config.PlaybackConfig
	FFmpegLogSink  playback.FFmpegLogSink
	copySeekAnchor copySeekAnchorResolver
	// copySeekAnchorBackoff is a test seam for the bounded pause before
	// retrying the same candidate after a transient provider (upstream 5xx)
	// anchor failure. Nil uses the real timer wait.
	copySeekAnchorBackoff func(ctx context.Context, d time.Duration) bool
	// autoTranscodePipelineV3 is a test seam for the hw_accel=auto fallback
	// pipeline; nil uses playback.NewAutoTranscodePipeline.
	autoTranscodePipelineV3 func(context.Context, playback.TranscodeOpts) *playback.AutoTranscodePipeline
	// beforeIdentityLifecycleLockV3 is a test seam for proving that identity
	// route authority remains unpublished until the shared lifecycle boundary.
	beforeIdentityLifecycleLockV3 func()
	realtimeCommandMu             sync.Mutex
	realtimeCommands              map[string]playbackCommandRecord
	// tm owns the transcode-session lifecycle (live map, recipe cards, and
	// restart reconstruct) shared with the jellycompat handler. The handler
	// delegates all transcode-session and recipe operations to it.
	tm *playback.TranscodeManager
	// PlanStoreV3 owns the short-lived protocol-v3 control-plane state. Router
	// wiring replaces the in-memory default with PostgreSQL in integrated mode.
	PlanStoreV3          playback.PlanStoreV3
	v3RegistryMu         sync.Mutex
	v3Registry           *playback.TransformationRegistryV3
	v3RegistryProbe      func(context.Context, string, tonemap.Capabilities) (*playback.TransformationRegistryV3, error)
	v3ToneMapProbe       func(context.Context, string, string, string) (tonemap.Capabilities, error)
	v3NodeCapabilitiesMu sync.Mutex
	v3NodeCapabilities   map[string]v3NodeCapabilityCache

	// v3LocalToneMapMu guards the process-lifetime local tone-map inventory.
	// Unlike the per-node inventory it is not refreshed on a TTL: the local
	// FFmpeg/hardware configuration is fixed for the process, so a complete
	// probe is reused for every start exactly like v3Registry. A failed probe
	// is not cached and is retried by the next caller. An error-free but
	// incomplete inventory is cached only until v3LocalToneMapNegativeUntil,
	// so a hardware executor hidden by transient contention is re-probed
	// instead of being frozen at software-only for the process lifetime.
	v3LocalToneMapMu            sync.Mutex
	v3LocalToneMapCaps          tonemap.Capabilities
	v3LocalToneMapCached        bool
	v3LocalToneMapNegativeUntil time.Time
	// v3NodeProbeBudgets holds what each node last said a capability read of it
	// costs, guarded by v3NodeCapabilitiesMu. It is kept apart from the
	// inventory above because the two are invalidated for different reasons: an
	// acceleration change makes the inventory wrong and the next read slow,
	// while how long that node takes to answer is unchanged. See
	// remoteToneMapProbeTimeoutV3.
	v3NodeProbeBudgets map[string]time.Duration
	// v3NodeCapabilityInvalidations counts invalidations per node URL, guarded
	// by v3NodeCapabilitiesMu. A probe that started before the count moved
	// describes hardware the health sweep has already reported as changed, so
	// its result must not be installed. See RefreshNodeCapabilitiesV3.
	v3NodeCapabilityInvalidations map[string]uint64
	// v3NodeCapabilityRefresh holds the nodes with a background refresh in
	// flight, one at a time each. Guarded by v3NodeCapabilitiesMu, the same
	// lock as the invalidation counter, so a refresh cannot release its slot in
	// between an invalidation and that invalidation's claim on it.
	v3NodeCapabilityRefresh map[string]struct{}
	v3RefresherOnce         sync.Once // starts the background capability refresher once; see StartCapabilityWarmupV3
	v3EventOnce             sync.Once
	v3EventQueue            chan playback.RouteEventRecordV3
	v3AudioPreferenceMu     sync.Mutex
	v3ReplanMu              sync.Mutex
	v3ReplanLocks           map[string]*v3ReplanLock
	v3ReplanSlotsOnce       sync.Once
	v3ReplanSlots           chan struct{}
	v3EventRateMu           sync.Mutex
	v3EventRates            map[string]v3EventRate
	// v3DVRPUVerds memoizes the Dolby Vision RPU strip verdict per catalog
	// file-row identity (ID + size + mtime). The shared probe cache keys on
	// bin|inputPath and the transport URL rotates per relay registration, so
	// without this memo every sidecar replan would re-run the ~6s probe.
	// Guarded by v3DVRPUMu.
	v3DVRPUMu    sync.Mutex
	v3DVRPUVerds map[dvRPUMemoKeyV3]bool

	// ServiceContext is the server lifecycle context. Detached virtual work
	// (background probes, optimistic revalidation, candidate-sink writes,
	// subtitle searches, prefetch) is parented to it so shutdown cancels
	// outstanding work instead of leaking it. nil behaves as
	// context.Background() for handlers built outside the router (tests).
	ServiceContext context.Context

	// detachedWorkOnce guards lazy construction of detachedWorkGate for
	// handlers built as literals rather than through NewPlaybackHandler. The
	// gate bounds all detached virtual work server-wide; see
	// playback_virtual.go.
	detachedWorkOnce sync.Once
	detachedWorkGate *virtualDetachedGate

	// virtualEvidenceOnce guards lazy construction of the coalescing buffer
	// and worker pool that persist virtual probe evidence. Evidence
	// persistence is queued rather than admitted through the aggregate gate so
	// a burst of probes cannot crowd delivery/failure evidence out; see
	// playback_virtual_evidence.go.
	virtualEvidenceOnce   sync.Once
	virtualEvidenceBuffer *virtualEvidenceBuffer
	// virtualEvidenceWG tracks the evidence workers so shutdown can await work
	// they already dequeued before draining the accepted remainder. A dequeued
	// task is still in a worker's hands, so an empty pending buffer alone is
	// not proof that shutdown is safe.
	virtualEvidenceWG sync.WaitGroup
	// virtualEvidenceStopOnce makes the shutdown sequence single-flight. The
	// application shutdown sequence calls StopVirtualEvidence explicitly (wired
	// through api.Dependencies.RegisterShutdownFunc); the service-context
	// watcher is only a safety net. A caller that races the other blocks until
	// the one closure, worker await and drain have all completed, so tests and
	// shutdown observe the same terminal state.
	virtualEvidenceStopOnce sync.Once

	// subtitleSlotsOnce guards lazy construction of the dedicated subtitle
	// search gate. Subtitle searches can run for the full two-minute provider
	// budget, so they are additionally capped well below the aggregate
	// detached gate: a hung provider can tie up at most subtitleSearchCap
	// aggregate slots and still leave capacity for probes and prefetch. A slot
	// (there and on the aggregate gate) is held until the callback returns,
	// never released when only its context expired, so a non-cooperative
	// provider cannot cause replacement goroutines to pile up.
	subtitleSlotsOnce sync.Once
	subtitleSlots     *virtualDetachedGate

	// deferredProbeOnce guards lazy construction of the bounded post-commit
	// probe pool (see enqueueDeferredVirtualProbeV3). A fresh-start resolve
	// hands its deferred full-track enumeration to this pool instead of probing
	// inline, so a saturated aggregate gate becomes backpressure that leaves the
	// client in the provisional inventory state, not a synchronous probe on the
	// first-byte path. The queue and workers are fixed; admission never spawns a
	// goroutine per request.
	deferredProbeOnce  sync.Once
	deferredProbeQueue chan *virtualDeferredProbeV3
	deferredProbeWG    sync.WaitGroup
	// deferredProbeRetry parks probes that arrived while the queue was full.
	// A worker drains one per completed probe, so a saturated burst completes in
	// bounded steps without a second worker pool and without a synchronous probe
	// on the request path.
	deferredProbeMu    sync.Mutex
	deferredProbeRetry map[string]*virtualDeferredProbeV3
	// deferredProbeSignal wakes an idle worker to drain the retry set. It is
	// buffered (size 1) and never blocks the request path.
	deferredProbeSignal chan struct{}
	// deferredPublishOnce guards lazy construction of the single
	// terminal-notification dispatcher (see startDeferredPublishDispatcher).
	// One goroutine per handler, tied to the service context.
	deferredPublishOnce sync.Once
	// deferredPublishMu guards deferredPublishPending, deferredPublishRescan,
	// and deferredPublishInFlight.
	//
	// LOCK ORDER: deferredPublishMu is always the OUTER lock. A caller may
	// hold deferredPublishMu and then call a session-manager method (which
	// takes the session lock internally — see the watermark check inside
	// parkDeferredProbePublish and the rescan's per-candidate re-validation);
	// the reverse — session lock held while acquiring deferredPublishMu —
	// never happens, and every code path here must keep it that way.
	//
	// deferredPublishPending is the bounded set of terminal inventory
	// notifications still owed a push, keyed by session ID so a later terminal
	// state for the same session coalesces onto one entry. An entry lands here
	// by one of three routes: a park when the aggregate detached gate was full, a
	// retryable attempt (a transient catalog/plan-store read or a realtime write
	// that did not reach the client) re-parked with a bounded backoff, or a
	// selected intent returned by the failed-acquisition repark after the gate
	// claim lost the race for a slot. The value carries the file whose inventory
	// the push re-reads, the binding generation the notification acks after
	// publish, and — for a retryable entry — the attempt count and the
	// not-before instant that hold it out of selection until its delay elapses.
	// Coalescing rule: a park replaces the parked entry only when the new
	// generation is at least the parked
	// generation, and is dropped entirely when the session's notified watermark
	// already covers the new generation — an older or already-delivered intent
	// can never overwrite a newer outstanding one. deferredPublishRescan is the
	// coalesced overflow flag: a park, a retry-insert, or a failed-acquisition
	// repark that finds the map full sets it rather than dropping intent, and
	// the dispatcher re-parks the overflow from the session manager as soon as
	// the map has room (not only when it drains — a retryable entry can keep it
	// occupied while it waits out its backoff). The map is the record of what to
	// publish; the signal channel is only a wake hint, so a lost signal can never
	// strand a parked notification — the dispatcher always re-reads the map
	// after any wake.
	//
	// Dispatchability predicate: a parked entry is dispatchable only while its
	// session is not in deferredPublishInFlight, the session's notified
	// watermark does not already cover the entry's generation, and — for a
	// retryable entry — its not-before instant has passed. The dispatcher
	// re-checks all three at selection time under this mutex, so a session
	// mid-publish is never handed a second worker, an already-delivered
	// generation is never republished, and an entry in retry backoff never spins
	// a batch.
	deferredPublishMu      sync.Mutex
	deferredPublishPending map[string]deferredPublishIntent
	deferredPublishRescan  bool
	// deferredPublishInFlight tracks sessions whose selected intent was popped
	// by a batch and handed to a publish worker but whose attempt has not yet
	// recorded its final disposition (an ack, or a re-park on a retryable
	// outcome). Selection deletes the map entry before the gate claim, so a
	// session is briefly neither parked nor in-flight between the two; the
	// claim's re-validation and the failed-acquisition repark close that window.
	// A session in this marker's window still reads as "outstanding" to the
	// overflow rescan (its generation is newer than the delivered watermark,
	// which only advances on the worker's post-publish ack), so without this
	// guard the rescan would re-park it and a second worker would publish the
	// same notification again — a duplicate push. The rescan skips in-flight
	// sessions; the marker is cleared after the disposition is recorded, so a
	// session whose attempt finished is eligible for the rescan only if it
	// genuinely still owes a notification (a re-parked retryable entry is
	// covered by its own parked intent, not this marker).
	deferredPublishInFlight map[string]struct{}
	// deferredPublishSignal is the buffered-1 wake hint for the dispatcher. A
	// park sends it nonblocking; a full buffer is fine because the dispatcher
	// re-checks the map regardless of how many signals it coalesced.
	deferredPublishSignal chan struct{}
	// deferredPublishDone is the buffered-1 completion signal: a publish
	// worker sends it nonblocking when it clears its in-flight marker, so a
	// dispatcher that found every parked entry in-flight (nothing dispatchable)
	// is woken the moment one finishes instead of busy-looping batches against
	// the in-flight set or waiting a full backstop tick. Coalescing is safe for
	// the same reason as the park signal: the dispatcher always re-reads the
	// map after any wake.
	deferredPublishDone chan struct{}

	// prefetchOnce guards the lazy prefetch worker pool. Prefetch work is
	// admitted into a bounded queue (prefetchQueue) before any goroutine
	// handles it, deduplicated by source+profile equivalence key
	// (prefetchInFlight), and drained by a fixed pool of
	// virtualPrefetchWorkers goroutines. Active, pending and dedup state are
	// therefore all bounded, and prefetch can never spawn one goroutine per
	// request. See PrefetchVirtualPlayback.
	prefetchOnce     sync.Once
	prefetchQueue    chan virtualPrefetchTask
	prefetchMu       sync.Mutex
	prefetchInFlight map[string]struct{}
	// prefetchStopped is set under prefetchMu when the service context ends.
	// New admissions are refused after that so a request racing shutdown cannot
	// enqueue work the workers will never drain.
	prefetchStopped bool
}

type PlaybackWatchScrobbler interface {
	ScrobbleStart(ctx context.Context, event watchsync.ScrobbleEvent) error
	ScrobblePause(ctx context.Context, event watchsync.ScrobbleEvent) error
	ScrobbleStop(ctx context.Context, event watchsync.ScrobbleEvent) error
}

type sessionExpirationHookAdder interface {
	AddExpirationHook(func(*playback.Session))
}

type sessionFinishHookAdder interface {
	AddFinishHook(func(context.Context, *playback.Session))
}

// NewPlaybackHandler creates a new PlaybackHandler backed by the given
// session manager. Pass optional FilePathResolver to enable stream_url
// and subtitle_urls in start playback responses.
func NewPlaybackHandler(sessionMgr SessionManagerInterface, opts ...FilePathResolver) *PlaybackHandler {
	h := &PlaybackHandler{
		sessionMgr:             sessionMgr,
		BestResultCache:        NewVirtualBestResultCache(defaultBestResultCacheTTL, defaultBestResultCacheEntries),
		SubtitleSearchInFlight: &sync.Map{},
		realtimeCommands:       make(map[string]playbackCommandRecord),
		tm:                     playback.NewTranscodeManager(),
		PlanStoreV3:            playback.NewMemoryPlanStoreV3(),
		// Detached virtual work is parented to this until the router replaces
		// it with the server lifecycle context.
		ServiceContext: context.Background(),
	}
	if len(opts) > 0 {
		h.fileResolver = opts[0]
	}
	// Wire the shared transcode manager with closures so it reads the handler's
	// (often late-set) config/store/secret fields lazily at call time, avoiding a
	// field-ordering hazard during router setup.
	h.tm.JWTSecretFn = func() string { return h.JWTSecret }
	h.tm.LogSinkFn = func() playback.FFmpegLogSink { return h.FFmpegLogSink }
	h.tm.Config = func() playback.TranscodeRuntimeConfig {
		c := h.playbackConfig()
		return playback.TranscodeRuntimeConfig{
			TranscodeDir:            c.TranscodeDir,
			FFmpegPath:              c.FFmpegPath,
			HWAccel:                 c.HWAccel,
			HWDevice:                c.HWDevice,
			SegmentRetentionSeconds: c.SegmentRetentionSeconds,
		}
	}
	h.tm.StartThrottler = func(ctx context.Context, ts *playback.TranscodeSession) {
		h.maybeStartThrottler(ctx, ts)
	}
	h.tm.OnFFmpegCrash = func(ctx context.Context, sessionID string, dead *playback.TranscodeSession) {
		// ffmpeg crash — tear the session down; a client holding a valid stream
		// token can reconstruct it on the next request.
		//
		// Compare-and-delete the dead transcode first: between ffmpeg's error exit
		// and this teardown a reconstruct may have registered a fresh successor
		// under the same id. CloseTranscodeSessionIf only removes (and Close()s, which
		// reaps the shared output dir) the entry when it is still the dead session;
		// if a successor won, it leaves the live one untouched and we must NOT tear
		// down the reconstructed playback session that now backs it.
		var nodeURL string
		if s, err := h.sessionMgr.GetSession(sessionID); err == nil {
			nodeURL = s.TranscodeNodeURL
		}
		if successor := h.tm.GetTranscodeSession(sessionID); successor != nil && successor != dead {
			// A reconstruct already replaced the crashed process; the live successor
			// and its session stand. Cheap fast-path only — the authoritative gate is
			// the compare-and-delete result below.
			return
		}
		// CloseTranscodeSessionIf is the authoritative gate: a successor may register
		// under the same id between the pre-check above and here. We only tear down the
		// upstream playback session when the compare-and-delete actually matched the
		// dead transcode. When it returns false a successor owns the session — do
		// nothing further, or finalizeSessionStop's unconditional CloseTranscodeSession
		// would reap the live successor's output dir mid-serve.
		if !h.tm.CloseTranscodeSessionIf(sessionID, dead, nodeURL) {
			return
		}
		if err := h.stopPlaybackSessionByID(ctx, sessionID, false); err != nil && !errors.Is(err, playback.ErrSessionNotFound) {
			slog.ErrorContext(ctx, "failed to stop playback after local transcode exit", "component", "api", "session", sessionID, "error", err, "playback_session_id", sessionID)
		}
	}
	if reg, ok := sessionMgr.(interface {
		RegisterReconstructed(s *playback.Session) *playback.Session
		RegisterReconstructedWithLimits(ctx context.Context, s *playback.Session) (*playback.Session, error)
	}); ok {
		h.tm.Sessions = reg
	}
	if adder, ok := sessionMgr.(sessionExpirationHookAdder); ok {
		adder.AddExpirationHook(h.handleExpiredSession)
	}
	if adder, ok := sessionMgr.(sessionFinishHookAdder); ok {
		adder.AddFinishHook(h.handleFinishedSession)
	}
	return h
}

// TranscodeManager returns the shared transcode/reconstruct manager so sibling
// handlers (e.g. StreamHandler) can reuse the same recipe-card store, live
// transcode map, and reconstruct front door rather than wiring a second one.
func (h *PlaybackHandler) TranscodeManager() *playback.TranscodeManager {
	return h.tm
}

// SetProfileStaler configures an optional staleness trigger for taste profiles.
func (h *PlaybackHandler) SetProfileStaler(ps ProfileStaler) {
	h.profileStaler = ps
}

// SetProfileRefreshRequester configures an optional background refresh queue for taste profiles.
func (h *PlaybackHandler) SetProfileRefreshRequester(requester ProfileRefreshRequester) {
	h.profileRefreshRequester = requester
}

// playbackConfig returns the current playback config, falling back to the
// same defaults as config loading (transcode enabled, temp transcode dir)
// when no provider is wired (tests, minimal setups).
func (h *PlaybackHandler) playbackConfig() config.PlaybackConfig {
	if h.PlaybackConfig != nil {
		return h.PlaybackConfig()
	}
	return config.PlaybackConfig{
		TranscodeEnabled: true,
		TranscodeDir:     filepath.Join(os.TempDir(), "silo-transcode"),
		Routing:          config.DefaultPlaybackRoutingPolicy(),
	}
}

func (h *PlaybackHandler) probeVirtualSource(ctx context.Context, sourceURL string, file *models.MediaFile, headers map[string]string) (*models.MediaFile, error) {
	if h == nil {
		return file, errors.New("playback handler is not configured")
	}
	if h.VirtualPlaybackSourceProberWithHeaders != nil {
		return h.VirtualPlaybackSourceProberWithHeaders(ctx, sourceURL, file, headers)
	}
	if h.VirtualPlaybackSourceProber != nil {
		return h.VirtualPlaybackSourceProber(ctx, sourceURL, file)
	}
	return file, errors.New("virtual playback source prober is not configured")
}

// CleanupOrphanedTranscodes removes stale per-session temp directories for
// transcodes that are no longer tracked in memory, sparing dirs whose recipe
// card still exists. Delegates to the shared transcode manager.
func (h *PlaybackHandler) CleanupOrphanedTranscodes() (int, error) {
	return h.tm.CleanupOrphanedTranscodes()
}

// playbackThresholds reads the playback.watched_threshold and
// playback.min_resume_threshold settings. Zero values mean "use defaults".
func (h *PlaybackHandler) playbackThresholds(ctx context.Context) userstore.ProgressThresholds {
	if h.SettingsRepo == nil {
		return userstore.ProgressThresholds{}
	}
	var t userstore.ProgressThresholds
	if v, _ := h.SettingsRepo.Get(ctx, "playback.watched_threshold"); v != "" {
		if pct, err := strconv.Atoi(v); err == nil && pct > 0 {
			t.WatchedPct = pct
		}
	}
	if v, _ := h.SettingsRepo.Get(ctx, "playback.min_resume_threshold"); v != "" {
		if pct, err := strconv.Atoi(v); err == nil && pct > 0 {
			t.MinResumePct = pct
		}
	}
	return t
}

// --- Request/Response types ---

// progressRequest represents the JSON body for POST /playback/{session_id}/progress.
type progressRequest struct {
	Position float64 `json:"position"`
	IsPaused bool    `json:"is_paused"`
}

func semanticPlayMethod(s *playback.Session) playback.PlayMethod {
	if s == nil {
		return ""
	}
	if s.BasePlayMethod != "" {
		return s.BasePlayMethod
	}
	return s.PlayMethod
}

func (h *PlaybackHandler) ensurePlaybackProbe(ctx context.Context, file *models.MediaFile) *models.MediaFile {
	if h == nil || h.ProbeEnsurer == nil || file == nil {
		return file
	}
	repaired, err := h.ProbeEnsurer.EnsureCopySafetyCached(ctx, file)
	if err != nil {
		slog.WarnContext(ctx, "playback probe repair failed", "component", "api", "file_id", file.ID, "path", file.FilePath, "error", err)
		return file
	}
	if repaired != nil {
		return repaired
	}
	return file
}

// Start-path probe refresh bounds. ensurePlaybackProbeStart waits at most
// playbackProbeStartBudgetDefault for an on-demand repair before serving the
// row's known metadata.
const (
	// playbackProbeStartBudgetDefault is the most a start request waits for a
	// probe repair that is not already cached. A cold probe on a remote library
	// costs multi-second reads and used to run unbounded on the start path; the
	// wait is capped so a slow probe can never hold the response for its full
	// course, while a cheap cached repair still completes before planning.
	playbackProbeStartBudgetDefault = 2 * time.Second
	// playbackProbeRefreshTimeout bounds the detached repair the start schedules
	// when the budget lapses. It sits above the ensurer's own probe deadline
	// (10s) so an ordinary slow remote read still completes and persists.
	playbackProbeRefreshTimeout = 30 * time.Second
	// playbackProbePreparedTTL is how long a completed refresh suppresses a
	// repeat schedule for the same unchanged file generation.
	playbackProbePreparedTTL = 5 * time.Minute
	// playbackProbePreparedMaxEntries bounds the refresh memo.
	playbackProbePreparedMaxEntries = 4096
)

// playbackProbeRefresh records one start-path probe refresh: the file
// generation it was prepared against and a channel closed when the repair
// lands. done is immutable after construction: fresh entries carry an
// already-closed channel and in-flight entries are closed exactly once by
// finishPlaybackProbeRefresh, so readers never race a writer.
type playbackProbeRefresh struct {
	fingerprint string
	preparedAt  time.Time
	done        chan struct{}
	// repaired is the ensurer's result, read under probeRefreshMu.
	repaired *models.MediaFile
}

// probeFingerprintUnset marks a missing timestamp inside a probe fingerprint.
const probeFingerprintUnset = "none"

// playbackProbeFingerprint identifies the file generation and probe state a
// refresh was prepared against. Probe repair rewrites ProbeUpdatedAt (and
// ProbeSource), so the post-repair fingerprint differs and the next start
// recognizes the repaired row as already prepared instead of re-queueing.
func playbackProbeFingerprint(file *models.MediaFile) string {
	if file == nil {
		return ""
	}
	mtime := probeFingerprintUnset
	if file.FileModifiedAt != nil {
		mtime = strconv.FormatInt(file.FileModifiedAt.UnixMicro(), 10)
	}
	probe := probeFingerprintUnset
	if file.ProbeUpdatedAt != nil {
		probe = strconv.FormatInt(file.ProbeUpdatedAt.UnixMicro(), 10)
	}
	return fmt.Sprintf("%d:%d:%s:%s:%s", file.ID, file.FileSize, mtime, probe, strings.TrimSpace(file.ProbeSource))
}

// isPlaybackPlanSafe reports whether a media file has sufficient metadata to
// form a validated playback plan without blocking playback start on probe repair.
// A video file must carry proven video evidence AND known audio evidence (or a
// verified probe stamp proving absence of audio). An audio-only file must carry
// a proven audio codec.
func isPlaybackPlanSafe(file *models.MediaFile) bool {
	if file == nil {
		return false
	}
	if file.IsAudioOnly() {
		return strings.TrimSpace(file.CodecAudio) != "" && (len(file.AudioTracks) > 0 || file.Duration > 0)
	}
	if !playback.VirtualRouteVideoMetadataCompleteV3(file) {
		return false
	}
	hasAudioEvidence := (len(file.AudioTracks) > 0 && strings.TrimSpace(file.AudioTracks[0].Codec) != "") ||
		strings.TrimSpace(file.CodecAudio) != ""
	return hasAudioEvidence || file.ProbeUpdatedAt != nil
}

// ensurePlaybackProbeStart is the playback-start probe. Unlike the synchronous
// ensurePlaybackProbe, it bounds how long the request waits: on a remote
// library the on-demand repair costs multi-second reads, and that cost sat
// directly on the start path. The request waits a short budget so a fast or
// already-cached probe can still heal the row before planning; when the budget
// lapses, start is served from the metadata already on the row and the repair
// finishes on a detached, bounded background refresh.
//
// The refresh is memoized per file generation, mirroring the catalog's watch
// preparation: the first start for an unchanged file schedules one repair and
// records the post-repair fingerprint when it lands, so repeated and concurrent
// starts neither block on nor re-queue it. A failed repair drops the claim so a
// later start retries; a client disconnect never cancels the repair.
func (h *PlaybackHandler) ensurePlaybackProbeStart(ctx context.Context, file *models.MediaFile) *models.MediaFile {
	if h == nil || h.ProbeEnsurer == nil || file == nil {
		return file
	}
	entry, owner := h.claimPlaybackProbeRefresh(file)
	if !owner {
		// A refresh is already in flight for this generation, or the memo
		// refused admission. Either way this caller is a joiner: the helper's
		// contract is that concurrent starts do not block on the refresh, so
		// serve the row's known metadata. If the repair already landed, honor
		// it immediately without waiting.
		if entry != nil {
			select {
			case <-entry.done:
				h.probeRefreshMu.Lock()
				repaired := entry.repaired
				h.probeRefreshMu.Unlock()
				if repaired != nil {
					return repaired
				}
			default:
			}
		}
		return file
	}
	h.refreshPlaybackProbeAsync(ctx, entry, file)

	// If repair already completed synchronously (or landed immediately):
	select {
	case <-entry.done:
		h.probeRefreshMu.Lock()
		repaired := entry.repaired
		h.probeRefreshMu.Unlock()
		if repaired != nil {
			return repaired
		}
		return file
	default:
	}

	// When stored metadata already supports a safe playback decision, do not
	// block playback start on the probe repair budget: serve known metadata
	// immediately while repair finishes in the background and notifies the
	// live playback session.
	if isPlaybackPlanSafe(file) {
		return file
	}

	timer := time.NewTimer(h.playbackProbeStartBudget())
	defer timer.Stop()
	select {
	case <-entry.done:
		// The repair landed within the budget; honor its result so planning
		// sees the repaired metadata.
		h.probeRefreshMu.Lock()
		repaired := entry.repaired
		h.probeRefreshMu.Unlock()
		if repaired != nil {
			return repaired
		}
		return file
	case <-timer.C:
		slog.DebugContext(ctx, "playback start served from known metadata; probe repair continues in background",
			"component", "api", "file_id", file.ID, "path", file.FilePath)
		return file
	case <-ctx.Done():
		// The viewer went away; the detached refresh still owns the repair.
		return file
	}
}

// claimPlaybackProbeRefresh returns the memo entry for a file and whether the
// caller owns starting its refresh. A caller owns the refresh when the memo has
// no unexpired entry for this file generation. A fresh entry carries a closed
// done channel; an in-flight entry's channel closes when the repair lands.
//
// The memo never exceeds playbackProbePreparedMaxEntries. When it is at the
// ceiling and expiry has not freed a slot, the claim is refused: it returns no
// entry and no owner, and the caller must serve known metadata without
// scheduling a probe. A later start retries once an entry ages out.
func (h *PlaybackHandler) claimPlaybackProbeRefresh(file *models.MediaFile) (*playbackProbeRefresh, bool) {
	fingerprint := playbackProbeFingerprint(file)
	now := time.Now()
	h.probeRefreshMu.Lock()
	defer h.probeRefreshMu.Unlock()
	if h.probeRefreshed == nil {
		h.probeRefreshed = make(map[int]*playbackProbeRefresh)
	}
	if existing, ok := h.probeRefreshed[file.ID]; ok {
		if existing.fingerprint == fingerprint && now.Sub(existing.preparedAt) < playbackProbePreparedTTL {
			return existing, false
		}
		// A stale or superseded entry no longer suppresses a refresh; drop it
		// before the admission check so it does not count against the ceiling.
		delete(h.probeRefreshed, file.ID)
	}
	if len(h.probeRefreshed) >= playbackProbePreparedMaxEntries {
		if h.prunePlaybackProbeRefreshesLocked(now) == 0 {
			// Every entry is still live. Refuse admission rather than grow the
			// memo past its ceiling; the caller schedules no probe and a later
			// start retries once an entry ages out.
			return nil, false
		}
	}
	entry := &playbackProbeRefresh{fingerprint: fingerprint, preparedAt: now, done: make(chan struct{})}
	h.probeRefreshed[file.ID] = entry
	return entry, true
}

// prunePlaybackProbeRefreshesLocked drops expired entries and reports how many
// it removed. It is called only when the memo is at its ceiling, so the sweep
// runs at most once per claim while full.
func (h *PlaybackHandler) prunePlaybackProbeRefreshesLocked(now time.Time) int {
	removed := 0
	for id, existing := range h.probeRefreshed {
		if now.Sub(existing.preparedAt) >= playbackProbePreparedTTL {
			delete(h.probeRefreshed, id)
			removed++
		}
	}
	return removed
}

// refreshPlaybackProbeAsync runs one probe repair on a detached goroutine. The
// start has already been served from the row's known metadata once the budget
// lapses, so the repair must outlive the request: a client that disconnects
// while the probe runs must not cancel the repair the next start will read.
// context.WithoutCancel keeps the request's logging and tracing values while
// dropping its cancellation; playbackProbeRefreshTimeout guards a wedged
// ensurer.
func (h *PlaybackHandler) refreshPlaybackProbeAsync(ctx context.Context, entry *playbackProbeRefresh, file *models.MediaFile) {
	// The request builds its response from this file, so hand the goroutine its
	// own copy; probe repair must not race a live response.
	snapshot := *file
	base := context.WithoutCancel(ctx)
	h.probeRefreshWG.Add(1)
	go func() {
		defer h.probeRefreshWG.Done()
		refreshCtx, cancel := context.WithTimeout(base, playbackProbeRefreshTimeout)
		defer cancel()
		repaired, err := h.ProbeEnsurer.EnsureCopySafetyCached(refreshCtx, &snapshot)
		if err != nil {
			slog.WarnContext(refreshCtx, "playback probe refresh failed", "component", "api", "file_id", file.ID, "path", file.FilePath, "error", err)
			// Fail open: the ensurer may return a file alongside its error, but
			// a failed repair is not a repaired row.
			repaired = nil
		}
		h.finishPlaybackProbeRefresh(entry, file.ID, repaired)
		if repaired != nil {
			// The probe upgraded this row's declared metadata to probe evidence.
			// Push the verified inventory to any live session playing it so its
			// track menu stops showing the plan's declared snapshot. The refresh
			// context may be exhausted by now, so the publish bounds itself.
			h.PublishInventoryUpdated(ctx, repaired.ID)
		}
	}()
}

// finishPlaybackProbeRefresh completes a refresh and closes its done channel,
// waking every waiter. A landed repair records the post-repair fingerprint so
// the next start of the unchanged repaired row is served without re-queueing. A
// failed repair drops the claim instead, so a later start retries rather than
// being suppressed for the TTL.
func (h *PlaybackHandler) finishPlaybackProbeRefresh(entry *playbackProbeRefresh, fileID int, repaired *models.MediaFile) {
	h.probeRefreshMu.Lock()
	if repaired == nil {
		if current, ok := h.probeRefreshed[fileID]; ok && current == entry {
			delete(h.probeRefreshed, fileID)
		}
	} else {
		entry.fingerprint = playbackProbeFingerprint(repaired)
		entry.preparedAt = time.Now()
		entry.repaired = repaired
	}
	h.probeRefreshMu.Unlock()
	close(entry.done)
}

// playbackProbeStartBudget is the bounded request wait before failing open.
// Tests set probeStartBudget directly to keep the bound short.
func (h *PlaybackHandler) playbackProbeStartBudget() time.Duration {
	if h.probeStartBudget > 0 {
		return h.probeStartBudget
	}
	return playbackProbeStartBudgetDefault
}

// streamTokenParam is the query parameter that carries the signed stream token
// on the native integrated serve path. The token is the durable reconstruction
// descriptor: a front-end that lost its in-memory session rebuilds from it. It
// rides a query parameter (not a path segment) because the integrated server is
// hit directly by the client — there is no query-stripping proxy hop in between,
// and the transcode manifest rewriter already appends the request RawQuery to
// every segment URI, so segment requests inherit the token for free. The
// proxy/node path keeps the token in the URL path (see the proxy server).
const streamTokenParam = "st"

// signSessionToken mints a stream token carrying the session's full
// reconstruction recipe. Returns "" when no signing secret is configured
// (reconstruct effectively disabled, e.g. in tests).
func (h *PlaybackHandler) signSessionToken(card playback.RecipeCard, requireMediaAuth bool) string {
	if requireMediaAuth {
		return ""
	}
	return h.signStreamClaims(card.ToClaims())
}

// signStreamClaims mints a stream token from claims that are already assembled.
// Callers serving a session from another node use it to add the claims a
// RecipeCard does not model (the file's Dolby Vision profile, audio-only flag),
// which a remote executor cannot look up for itself.
// PlaybackCopySafetyRacer resolves an unknown H.264 copy-safety verdict out of
// band, after a plan that stream-copies video has already been issued.
// *playback.CopySafetyRace implements it.
type PlaybackCopySafetyRacer interface {
	RaceScanForPlan(fileID int, plan *playback.PlanV3)
	// RaceScan re-engages the race for a file whose verdict is still open. The
	// serve paths use it when they revive a stream-copy transport: the replica
	// that planned it may be gone, and only a race running *here* can withdraw
	// the route from the session this replica just rebuilt.
	RaceScan(fileID int)
	// VideoCopyUnsafeKnown answers, without ffmpeg and without waiting, whether
	// this replica can already condemn a video stream-copy of the file —
	// including from a verdict whose write to the row failed.
	VideoCopyUnsafeKnown(ctx context.Context, file *models.MediaFile) bool
}

func (h *PlaybackHandler) signStreamClaims(claims streamtoken.Claims) string {
	if h.JWTSecret == "" {
		return ""
	}
	token, err := streamtoken.Sign(claims, h.JWTSecret, playback.MaxTokenTTL)
	if err != nil {
		slog.Warn("sign stream token failed", "error", err, "session", claims.SessionID, "playback_session_id", claims.SessionID)
		return ""
	}
	return token
}

// loadTranscodeServeSession resolves the playback Session for the transcode
// manifest/segment serve routes while keeping stream-token verification off the
// hot path. A V3 session that negotiated header-authenticated media requires a
// live authenticated owner on every request; a legacy session retains its UUID
// bearer behavior. The overwhelmingly common case is a live in-memory session,
// which needs no token at all, so the cheap GetSession lookup runs first and the
// (HMAC + JSON) token decode is performed only when the session cannot serve by
// itself: on a not-found miss where a reconstruct is required, and on a live
// session with no runtime, where any valid recipe card (plain or tone-mapped)
// joins the manager's atomic playback+runtime reconstruction operation. On the
// miss it delegates to the shared LoadOrReconstructSession front door so
// reconstruct/ownership semantics stay identical. The returned card (nil on the
// live-session path when no token is presented) is the decoded recipe the
// caller's own reconstruct branch consumes.
func (h *PlaybackHandler) loadTranscodeServeSession(r *http.Request, sessionID string, requestedSegment int) (*playback.Session, playback.SessionLoadStatus, *playback.RecipeCard, *streamtoken.Claims, error) {
	requestUserID := apimw.GetUserID(r.Context())
	// A denied session is over everywhere: neither the live entry nor a valid
	// token may serve or reconstruct it.
	if h.StreamDeny.Denied(r.Context(), sessionID) {
		return nil, playback.SessionUnavailable, nil, nil, errPlaybackSessionEnded
	}
	session, err := h.sessionMgr.GetSession(sessionID)
	if err == nil {
		// Defense in depth: LoadOrReconstructSession enforces the same rule for
		// every serve handler, but this fast path never reaches it.
		if session.RequireMediaAuthorization && requestUserID == 0 {
			return nil, playback.SessionUnauthorized, nil, nil, nil
		}
		// Live session: secure transports require a user above; legacy bearer
		// routes allow zero. Either way, a present but mismatched identity is
		// forbidden. No token verification on this hot path.
		if requestUserID != 0 && session.UserID != requestUserID {
			return nil, playback.SessionForbidden, nil, nil, nil
		}
		// A non-API or incomplete route is returned to the handler for the
		// committed-egress guard below. Do not rebuild a local runtime first:
		// even discarded output would cross the route's execution boundary.
		if nativeAPIEgressStatusV3(session.RoutingWorkload, session.RoutingExecution, session.RoutingEgress) != 0 {
			return session, playback.SessionLoaded, nil, nil, nil
		}
		if session.TranscodeNodeURL == "" && h.tm.GetTranscodeSession(sessionID) == nil {
			// A live session whose runtime died can recover from the client's
			// recipe token. Tone-mapped cards may back a provisional capability
			// reconstruction; plain cards just respawn the encode. Either way the
			// atomic playback+runtime operation is the same front door.
			card, claims := verifiedStreamCardFromToken(r.URL.Query().Get(streamTokenParam), sessionID, h.JWTSecret)
			if card != nil && nativeAPIEgressStatusV3(card.RoutingWorkload, card.RoutingExecution, card.RoutingEgress) != 0 {
				// The live session may have been replanned since this token was
				// issued. Its API assignment is authoritative; a stale proxy card
				// cannot revive the old runtime on this origin.
				return session, playback.SessionLoaded, nil, nil, nil
			}
			if card != nil {
				session, _, status, reconstructErr := h.tm.LoadOrReconstructTranscodeWithError(r.Context(), h.sessionMgr.GetSession, sessionID, requestUserID, requestedSegment, card)
				return session, status, card, claims, reconstructErr
			}
		}
		return session, playback.SessionLoaded, nil, nil, nil
	}
	if !errors.Is(err, playback.ErrSessionNotFound) {
		return nil, playback.SessionLoadFailed, nil, nil, nil
	}
	// Genuine miss (e.g. after a restart): now — and only now — pay for the token
	// decode so the recipe is available for reconstruction.
	card, claims := verifiedStreamCardFromToken(r.URL.Query().Get(streamTokenParam), sessionID, h.JWTSecret)
	if card != nil {
		if routeStatus := nativeAPIEgressStatusV3(card.RoutingWorkload, card.RoutingExecution, card.RoutingEgress); routeStatus != 0 {
			return nil, playback.SessionUnavailable, card, claims, &nativeRouteBindingErrorV3{status: routeStatus}
		}
	}
	// The copy-safety verdict gates the revival before it happens, not after.
	// Reconstruction registers the playback session against the user's stream
	// caps, so a refusal that ran later would leave a session nobody serves
	// holding an admission slot the client's fresh attempt needs — and a
	// remote-node recipe never reaches the local transport reconstruct at all
	// (the serve handlers proxy to the node instead), so a gate down there would
	// miss it entirely. A revival the verdict does not condemn re-engages the
	// race here, so the session about to be rebuilt is covered by a race this
	// replica owns. See playback_copy_safety.go.
	if videoCopyReconstructRefused(r.Context(), h.fileResolver, h.CopySafetyRacer, card) {
		return nil, playback.SessionMissing, nil, nil, nil
	}
	if card != nil && card.ToneMapMode != "" {
		session, _, status, reconstructErr := h.tm.LoadOrReconstructTranscodeWithError(r.Context(), h.sessionMgr.GetSession, sessionID, requestUserID, requestedSegment, card)
		return session, status, card, claims, reconstructErr
	}
	session, status := h.tm.LoadOrReconstructSession(r.Context(), h.sessionMgr.GetSession, sessionID, requestUserID, card)
	return session, status, card, claims, nil
}

// streamCardFromToken verifies a stream token and decodes its reconstruction
// recipe, returning nil when the token is absent, unparseable/expired, or bound
// to a different session id. Shared by the native serve handlers (PlaybackHandler
// and StreamHandler).
func streamCardFromToken(tokenStr, sessionID, secret string) *playback.RecipeCard {
	if tokenStr == "" || secret == "" {
		return nil
	}
	claims, err := streamtoken.Verify(tokenStr, secret)
	if err != nil || claims.SessionID != sessionID {
		return nil
	}
	card := playback.RecipeCardFromClaims(claims)
	return &card
}

// verifiedStreamCardFromToken is streamCardFromToken for serve paths that also
// need the verified claims (telemetry attribution, header-auth checks).
func verifiedStreamCardFromToken(tokenStr, sessionID, secret string) (*playback.RecipeCard, *streamtoken.Claims) {
	if tokenStr == "" || secret == "" {
		return nil, nil
	}
	claims, err := streamtoken.Verify(tokenStr, secret)
	if err != nil || claims.SessionID != sessionID {
		return nil, nil
	}
	card := playback.RecipeCardFromClaims(claims)
	return &card, claims
}

// requireNativeAPIEgressV3 enforces the origin frozen into a v3 playback
// assignment. Empty assignments predate node routing and remain API-compatible;
// a partially populated assignment is a failed commit and must not fail open.
func requireNativeAPIEgressV3(w http.ResponseWriter, workload, execution, egress string) bool {
	status := nativeAPIEgressStatusV3(workload, execution, egress)
	if status == 0 {
		return true
	}
	writeNativeRouteStatusV3(w, status)
	return false
}

func writeNativeRouteStatusV3(w http.ResponseWriter, status int) {
	switch status {
	case http.StatusConflict:
		writeError(w, status, "playback_route_unbound", "Request a new playback plan before serving media")
	default:
		writeError(w, status, string(noderouting.OutcomePolicyUnsatisfied), "The media request does not match the route bound by the playback plan")
	}
}

func nativeAPIEgressStatusV3(workload, execution, egress string) int {
	workload = strings.TrimSpace(workload)
	execution = strings.TrimSpace(execution)
	egress = strings.TrimSpace(egress)
	if workload == "" && execution == "" && egress == "" {
		return 0
	}
	if workload == "" || execution == "" || egress == "" {
		return http.StatusConflict
	}
	if egress != string(noderouting.EgressAPI) {
		return http.StatusServiceUnavailable
	}
	return 0
}

func requireNativeSessionAPIEgressV3(w http.ResponseWriter, session *playback.Session) bool {
	if session == nil {
		return false
	}
	return requireNativeAPIEgressV3(w, session.RoutingWorkload, session.RoutingExecution, session.RoutingEgress)
}

func requireNativeRecipeAPIEgressV3(w http.ResponseWriter, card *playback.RecipeCard) bool {
	if card == nil {
		return true
	}
	return requireNativeAPIEgressV3(w, card.RoutingWorkload, card.RoutingExecution, card.RoutingEgress)
}

type nativeRouteBindingErrorV3 struct {
	status int
}

func (e *nativeRouteBindingErrorV3) Error() string {
	return "media request does not match its bound playback route"
}

func writeNativeRouteBindingErrorV3(w http.ResponseWriter, err error) bool {
	var routeErr *nativeRouteBindingErrorV3
	if !errors.As(err, &routeErr) {
		return false
	}
	writeNativeRouteStatusV3(w, routeErr.status)
	return true
}

// attachPlaybackSession stamps the stream-telemetry context with the session's
// identity and start-time provenance for every serve request that reaches it.
func attachPlaybackSession(ctx context.Context, session *playback.Session, claims *streamtoken.Claims) {
	if session == nil {
		return
	}
	startedAt := session.StartedAt
	startedSource := streamtelemetry.StartedAtSourceSession
	tokenIssuedAt := time.Time{}
	tokenSource := streamtelemetry.TokenIssuedAtSourceNone
	if claims != nil {
		if resolved, source := claims.StartedAt(); !resolved.IsZero() {
			switch source {
			case streamtoken.StartedAtSourceClaim:
				startedAt = resolved
				startedSource = streamtelemetry.StartedAtSourceClaim
			case streamtoken.StartedAtSourceIssuedAt:
				if startedAt.IsZero() {
					startedAt = resolved
					startedSource = streamtelemetry.StartedAtSourceIssuedAt
				}
			}
		}
		if claims.IssuedAt != nil {
			tokenIssuedAt = claims.IssuedAt.Time
			tokenSource = streamtelemetry.TokenIssuedAtSourceVerified
		}
	}
	streamtelemetry.Attach(ctx, streamtelemetry.Attachment{Subject: streamtelemetry.UserSubject(session.UserID),
		ProfileID: session.ProfileID, SessionID: session.ID, MediaFileID: session.MediaFileID,
		PlayMethod: string(session.PlayMethod), StartedAt: startedAt, StartedAtSource: startedSource,
		TokenIssuedAt: tokenIssuedAt, TokenIssuedAtSource: tokenSource})
}

func attachTransfer(ctx context.Context, userID int, profileID string, mediaFileID int) {
	streamtelemetry.Attach(ctx, streamtelemetry.Attachment{Subject: streamtelemetry.UserSubject(userID),
		ProfileID: profileID, MediaFileID: mediaFileID, StartedAtSource: streamtelemetry.StartedAtSourceFirstSeen,
		TokenIssuedAtSource: streamtelemetry.TokenIssuedAtSourceNone})
}

// appendStreamToken adds the ?st=<token> parameter to a native serve URL.
func appendStreamToken(rawURL, token string) string {
	if token == "" {
		return rawURL
	}
	sep := "?"
	if strings.ContainsRune(rawURL, '?') {
		sep = "&"
	}
	return rawURL + sep + streamTokenParam + "=" + token
}

// playbackStreamURL builds the native serve URL for a session and appends an
// identity stream token so a direct-play/remux session survives a restart (the
// client re-supplies its byte position). Transcode sessions are told which URL
// to play by their v3 plan; the URL here is an informational placeholder that
// the plan's delivery URL supersedes.
func (h *PlaybackHandler) playbackStreamURL(s *playback.Session) string {
	if s == nil {
		return ""
	}
	if s.PlayMethod == playback.PlayTranscode {
		return fmt.Sprintf("/playback/transcode/%s/master.m3u8", s.ID)
	}
	// A session that requires media authorization never carries a signed
	// playback credential in its URL; the client authenticates media requests
	// with its own access token instead.
	if s.RequireMediaAuthorization {
		return fmt.Sprintf("/stream/%s", s.ID)
	}
	card := identityRecipeCard(s)
	return appendStreamToken(fmt.Sprintf("/stream/%s", s.ID), h.signSessionToken(card, false))
}

// identityRecipeCard builds the identity-only recipe for a direct-play or remux
// session: reconstruction needs only ownership plus the audio selection, since
// the bytes are served by HTTP Range / a re-spawned remux pipe at the
// client-supplied position.
func identityRecipeCard(s *playback.Session) playback.RecipeCard {
	var card playback.RecipeCard
	switch s.PlayMethod {
	case playback.PlayRemux:
		card = playback.NewRemuxRecipeCard(s.ID, s.UserID, s.ProfileID, s.MediaFileID, s.TranscodeAudio, s.AudioTrackIndex, s.RemuxDVMode)
		card.RemuxResumeLeadingPictureDrop = s.RemuxResumeLeadingPictureDrop
		card.TargetCodecAudio = s.TargetAudioCodec
		card.TargetAudioChannels = s.TargetAudioChannels
		card.TargetAudioBitrateKbps = s.TargetAudioBitrateKbps
		if s.TranscodeAudio && playback.IsAudioToAACStereoDownmixV3(s.SourceAudioChannels, s.TargetAudioCodec, s.TargetAudioChannels) {
			card.SourceAudioChannels = s.SourceAudioChannels
		}
	default:
		card = playback.NewDirectRecipeCard(s.ID, s.UserID, s.ProfileID, s.MediaFileID)
	}
	card.OriginalStartedAt = s.StartedAt
	card.RoutingNetworkProvider = s.RoutingNetworkProvider
	card.StreamLocation = s.StreamLocation
	card.RoutingWorkload = s.RoutingWorkload
	card.RoutingExecution = s.RoutingExecution
	card.RoutingExecutionNodeID = s.RoutingExecutionNodeID
	card.RoutingEgress = s.RoutingEgress
	card.RoutingEgressNodeID = s.RoutingEgressNodeID
	card.TranscodeNodeURL = s.TranscodeNodeURL
	card.TranscodeTransportID = s.TranscodeTransportID
	return card
}

func fileBitrateKbps(file *models.MediaFile) int {
	if file == nil || file.Bitrate <= 0 {
		return 0
	}
	return file.Bitrate
}

func requestedMediaFileID(session *playback.Session) int {
	if session == nil {
		return 0
	}
	if session.RequestedMediaFileID > 0 {
		return session.RequestedMediaFileID
	}
	return session.MediaFileID
}

func remoteTransportID(session *playback.Session) string {
	if session != nil && session.TranscodeTransportID != "" {
		return session.TranscodeTransportID
	}
	if session == nil {
		return ""
	}
	return session.ID
}

func (h *PlaybackHandler) closeTranscodeForSession(session *playback.Session) {
	if session == nil {
		return
	}
	// Local sessions remain keyed by the public playback session. Remote v3
	// processes use a plan-scoped transport identity so a prepared successor can
	// coexist with its predecessor until commit.
	h.tm.CloseTranscodeSession(session.ID, "")
	if session.TranscodeNodeURL != "" {
		h.tm.StopRemoteTranscode(remoteTransportID(session), session.TranscodeNodeURL)
	}
}

func (h *PlaybackHandler) loadFileByPreferredID(
	ctx context.Context,
	preferredID int,
	fallbackID int,
) (*models.MediaFile, error) {
	if h.fileResolver == nil {
		return nil, fmt.Errorf("file resolver not configured")
	}
	if preferredID > 0 {
		file, err := h.fileResolver.GetByID(ctx, preferredID)
		if err == nil && file != nil {
			return file, nil
		}
		if err != nil && (fallbackID == 0 || fallbackID == preferredID) {
			return nil, err
		}
	}
	if fallbackID > 0 && fallbackID != preferredID {
		return h.fileResolver.GetByID(ctx, fallbackID)
	}
	return nil, nil
}

func directPlayAudioTrackIndex(file *models.MediaFile) int {
	if file == nil || len(file.AudioTracks) == 0 {
		return 0
	}
	for i, track := range file.AudioTracks {
		if track.Default {
			return i
		}
	}
	return 0
}

func normalizeAudioTrackIndex(file *models.MediaFile, audioTrackIndex int) int {
	if file == nil || len(file.AudioTracks) == 0 {
		return 0
	}
	if audioTrackIndex >= 0 && audioTrackIndex < len(file.AudioTracks) {
		return audioTrackIndex
	}
	return directPlayAudioTrackIndex(file)
}

func (h *PlaybackHandler) resolveSeriesID(ctx context.Context, file *models.MediaFile) string {
	if file.EpisodeID == "" || h.EpisodeLookup == nil {
		return ""
	}
	ep, err := h.EpisodeLookup.GetByID(ctx, file.EpisodeID)
	if err != nil || ep == nil {
		return ""
	}
	return ep.SeriesID
}

// resolveOriginalLanguage fetches the original language for a media file's content item.
// For episodes, it looks up the parent series. Returns empty string if unavailable.
func (h *PlaybackHandler) resolveOriginalLanguage(ctx context.Context, file *models.MediaFile) string {
	if h.OriginalLangLookup == nil {
		return ""
	}
	contentID := file.ContentID
	if file.EpisodeID != "" {
		contentID = h.resolveSeriesID(ctx, file)
	}
	if contentID == "" {
		return ""
	}
	lang, err := h.OriginalLangLookup.GetOriginalLanguage(ctx, contentID)
	if err != nil {
		return ""
	}
	return lang
}

// resolvedPlaybackAudioLanguage returns the effective playback.audio_language
// for one canonical settings context. It may return an original-language
// preference (playback.IsOriginalLanguagePreference), which the caller
// resolves to a concrete language. Returns "" when nothing is stored: the contract default is null,
// "no preference". Resolution and decoding failures are returned so playback
// does not silently substitute a different track.
func resolvedPlaybackAudioLanguage(ctx context.Context, store userstore.UserStore, rc settingsresolve.Context) (string, error) {
	if store == nil || rc.ProfileID == "" {
		return "", nil
	}
	contract, err := settingscontract.Load()
	if err != nil {
		return "", fmt.Errorf("loading settings contract: %w", err)
	}
	resolved, err := settingsresolve.New(contract).Resolve(ctx, store, rc,
		[]string{settingskeys.PlaybackAudioLanguage}, nil)
	if err != nil {
		return "", fmt.Errorf("resolving playback audio language: %w", err)
	}
	if len(resolved) == 0 {
		return "", nil
	}
	var language string
	if err := json.Unmarshal(resolved[0].Value, &language); err != nil {
		return "", fmt.Errorf("decoding playback audio language: %w", err)
	}
	return strings.TrimSpace(language), nil
}

// --- Persistence helpers ---

// progressPersistenceFile resolves which media_files row progress and version
// hints should record. A virtual session binds VirtualSourceURI to the exact
// candidate selected and probed at plan time; that candidate is a different
// catalog row from the neutral requested row, so preferring the requested row
// leaves last_file_id pointing at the neutral VIRTUAL version and the media
// page never adopts the version that actually played. Fall back to the
// requested/effective session IDs when the candidate row cannot be resolved
// (for example it was replaced between resolve and persist).
func (h *PlaybackHandler) progressPersistenceFile(ctx context.Context, session *playback.Session) (*models.MediaFile, error) {
	if session != nil && session.VirtualSourceURI != "" && h.VirtualFileLookup != nil {
		if file, err := h.VirtualFileLookup(ctx, session.VirtualSourceURI); err == nil && file != nil && file.ID > 0 {
			return file, nil
		}
	}
	return h.loadFileByPreferredID(ctx, requestedMediaFileID(session), session.MediaFileID)
}

// persistProgress saves the current playback position to the UserStore.
// It resolves the mediaFileID to a mediaItemID via the file resolver.
// Errors are logged but do not fail the HTTP request.
func (h *PlaybackHandler) persistProgress(ctx context.Context, session *playback.Session) {
	if h.StoreProvider == nil || h.fileResolver == nil {
		return
	}
	if session == nil || session.DisableProgressPersistence {
		return
	}
	// Position 0 carries no resume information (mirrors persistStopAndHistory
	// and the jellycompat report path). Progress is last-write-wins, so an
	// early zero heartbeat — e.g. before a client finishes seeking to its
	// resume point — must not wipe the stored resume position.
	if session.Position <= 0 {
		return
	}

	file, err := h.progressPersistenceFile(ctx, session)
	targetID := playbackProgressTarget(file)
	if err != nil || targetID == "" {
		return // file not found or not yet matched to a media item
	}

	store, err := h.StoreProvider.ForUser(ctx, session.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get user store", "component", "api", "user_id", session.UserID, "error", err)
		return
	}

	duration := float64(file.Duration)
	completed, err := userstore.UpdateProgressReportingCompletion(ctx, store, session.ProfileID, targetID, session.Position, duration, h.playbackThresholds(ctx))
	if err != nil {
		slog.ErrorContext(ctx, "failed to persist progress", "component", "api", "session", session.ID, "error", err)
	} else if completed {
		// A heartbeat that only moves the position leaves the taste profile
		// alone; the stop (persistStopAndHistory) refreshes it for the play,
		// and marking the item watched refreshes it here.
		triggerProfileRefresh(ctx, h.profileStaler, h.profileRefreshRequester, session.UserID, session.ProfileID)
	}

	if err := store.UpdateProgressHints(ctx, session.ProfileID, targetID, userstore.VersionHints{
		FileID:     file.ID,
		Resolution: file.Resolution,
		HDR:        file.HDR,
		CodecVideo: file.CodecVideo,
		EditionKey: file.EditionKey,
	}); err != nil {
		slog.ErrorContext(ctx, "failed to persist version hints", "component", "api", "session", session.ID, "error", err)
	}
}

// persistStopAndHistory saves the final position and adds a watch history entry
// when a playback session is stopped. Errors are logged but do not fail the
// HTTP request.
func (h *PlaybackHandler) persistStopAndHistory(ctx context.Context, session *playback.Session) watchstate.PlaybackStopResult {
	if h.StoreProvider == nil || h.fileResolver == nil {
		return watchstate.PlaybackStopResult{}
	}
	if session == nil || session.DisableProgressPersistence || session.Position <= 0 {
		return watchstate.PlaybackStopResult{}
	}

	file, err := h.progressPersistenceFile(ctx, session)
	targetID := playbackProgressTarget(file)
	if err != nil || targetID == "" {
		return watchstate.PlaybackStopResult{}
	}

	duration := float64(file.Duration)
	thresholds := h.playbackThresholds(ctx)
	watchSvc := watchstate.NewService(h.StoreProvider).
		WithStableIdentityResolver(h.StableIdentityResolver).
		WithCompletionObserver(h.CompletionObserver)
	stoppedAt := time.Now().UTC()
	hints := userstore.VersionHints{
		FileID:     file.ID,
		Resolution: file.Resolution,
		HDR:        file.HDR,
		CodecVideo: file.CodecVideo,
		EditionKey: file.EditionKey,
	}
	var result watchstate.PlaybackStopResult
	if session.IsJellyfinCompat {
		result, err = watchSvc.RecordPlaybackStopOnce(ctx, session.UserID, session.ProfileID, targetID, duration, session.Position, stoppedAt, hints, thresholds, compatPlayHistoryID(session.ID))
	} else {
		result, err = watchSvc.RecordPlaybackStop(ctx, session.UserID, session.ProfileID, targetID, duration, session.Position, stoppedAt, hints, thresholds)
	}
	if err != nil {
		slog.ErrorContext(ctx, "failed to persist playback stop", "component", "api", "session", session.ID, "error", err)
	}
	// A history row this stop wrote still counts when a later write failed:
	// no later stop of a once-recorded play refreshes the profile for it.
	if (err == nil && !result.AlreadyRecorded) || result.HistoryID != "" {
		triggerProfileRefresh(ctx, h.profileStaler, h.profileRefreshRequester, session.UserID, session.ProfileID)
	}
	return result
}

func (h *PlaybackHandler) scrobbleEventForSession(ctx context.Context, session *playback.Session, mediaItemID string, duration, position float64) watchsync.ScrobbleEvent {
	event := watchsync.ScrobbleEvent{
		PlaybackSessionID: session.ID,
		UserID:            session.UserID,
		ProfileID:         session.ProfileID,
		MediaItemID:       mediaItemID,
		PositionSeconds:   position,
		DurationSeconds:   duration,
		OccurredAt:        time.Now().UTC(),
	}
	return watchsync.ResolveScrobbleIdentity(ctx, h.StableIdentityResolver, event)
}

func (h *PlaybackHandler) scrobbleEventForStoppedSession(
	ctx context.Context,
	session *playback.Session,
	stopResult watchstate.PlaybackStopResult,
) (watchsync.ScrobbleEvent, bool) {
	if session == nil || session.DisableProgressPersistence {
		return watchsync.ScrobbleEvent{}, false
	}

	mediaItemID := stopResult.MediaItemID
	duration := stopResult.DurationSeconds
	position := stopResult.FinalPositionSeconds
	if mediaItemID == "" {
		if h.fileResolver == nil {
			return watchsync.ScrobbleEvent{}, false
		}
		file, err := h.loadFileByPreferredID(ctx, requestedMediaFileID(session), session.MediaFileID)
		if err != nil || file == nil {
			return watchsync.ScrobbleEvent{}, false
		}
		mediaItemID = playbackProgressTarget(file)
		if mediaItemID == "" {
			return watchsync.ScrobbleEvent{}, false
		}
		duration = float64(file.Duration)
		position = session.Position
	}

	event := h.scrobbleEventForSession(ctx, session, mediaItemID, duration, position)
	event.HistoryID = stopResult.HistoryID
	event.Completed = stopResult.Completed
	return event, true
}

func (h *PlaybackHandler) buildAdminHistoryEntry(
	ctx context.Context,
	session *playback.Session,
) (*AdminPlaybackHistoryEntry, error) {
	if h.AdminStore == nil || h.fileResolver == nil || session == nil {
		return nil, nil
	}

	file, err := h.loadFileByPreferredID(ctx, requestedMediaFileID(session), session.MediaFileID)
	if err != nil {
		return nil, fmt.Errorf("loading media file: %w", err)
	}

	targetID := playbackProgressTarget(file)
	profileName := session.ProfileID
	if h.StoreProvider != nil {
		store, storeErr := h.StoreProvider.ForUser(ctx, session.UserID)
		if storeErr != nil {
			slog.ErrorContext(ctx, "failed to get user store for admin history", "component", "api", "session", session.ID, "error", storeErr)
		} else if store != nil {
			profile, profileErr := store.GetProfile(ctx, session.ProfileID)
			if profileErr != nil {
				slog.ErrorContext(ctx, "failed to load profile for admin history", "component", "api", "session", session.ID, "error", profileErr)
			} else if profile != nil && strings.TrimSpace(profile.Name) != "" {
				profileName = profile.Name
			}
		}
	}

	var durationPtr *float64
	completed := false
	if file != nil {
		duration := float64(file.Duration)
		durationPtr = &duration
		if duration > 0 && session.Position/duration > userstore.WatchedFraction(h.playbackThresholds(ctx).WatchedPct) {
			completed = true
		}
	}

	entry := &AdminPlaybackHistoryEntry{
		SessionID:       session.ID,
		UserID:          session.UserID,
		ProfileID:       session.ProfileID,
		ProfileName:     profileName,
		MediaItemID:     targetID,
		MediaFileID:     requestedMediaFileID(session),
		PlayMethod:      string(semanticPlayMethod(session)),
		StartedAt:       session.StartedAt.UTC().Format(time.RFC3339Nano),
		EndedAt:         time.Now().UTC().Format(time.RFC3339Nano),
		WatchedSeconds:  session.Position,
		DurationSeconds: durationPtr,
		Completed:       completed,
		ClientIP:        clientip.FromContext(ctx),
	}
	return entry, nil
}

func (h *PlaybackHandler) syncSessionsNow(ctx context.Context, reason string) {
	if h.SessionSyncer == nil {
		return
	}
	if err := h.SessionSyncer.SyncNow(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to sync sessions", "component", "api", "reason", reason, "error", err)
	}
}

// syncSessionsOnPauseChange syncs after a progress sample only when it flips
// the pause state. A sync upserts every session on this node and invalidates
// the admin session caches on every replica, which is too much for each
// heartbeat; position alone waits for the periodic reconcile tick.
func (h *PlaybackHandler) syncSessionsOnPauseChange(ctx context.Context, wasPaused, isPaused bool) {
	if wasPaused != isPaused {
		h.syncSessionsNow(ctx, "progress_pause")
	}
}

func (h *PlaybackHandler) touchSessionActivity(sessionID string) {
	if h == nil || sessionID == "" {
		return
	}
	if err := h.sessionMgr.TouchActivity(sessionID); err != nil && !errors.Is(err, playback.ErrSessionNotFound) {
		slog.Warn("failed to refresh playback activity", "session", sessionID, "error", err, "playback_session_id", sessionID)
	}
}

func (h *PlaybackHandler) finalizeSessionStop(ctx context.Context, session *playback.Session, syncNow bool, syncReason string, userInitiated bool) {
	h.finalizeSessionStopWithResult(ctx, session, syncNow, syncReason, userInitiated)
}

// finalizeSessionStopWithResult is finalizeSessionStop reporting the history
// writer's result, which the v2 stop receipt carries.
func (h *PlaybackHandler) finalizeSessionStopWithResult(ctx context.Context, session *playback.Session, syncNow bool, syncReason string, userInitiated bool) watchstate.PlaybackStopResult {
	if h == nil || session == nil || session.ID == "" {
		return watchstate.PlaybackStopResult{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.cancelPlaybackStartSideEffectsV3(ctx, session.ID)
	// The session is gone: a terminal inventory notification parked behind a
	// full detached gate would re-publish against a session ID the manager no
	// longer knows, so discard it rather than let the drain retry a dead session.
	h.dropDeferredPublish(session.ID)

	stopResult := h.recordStopHistory(ctx, session)
	if h.WatchScrobbler != nil {
		if event, ok := h.scrobbleEventForStoppedSession(ctx, session, stopResult); ok && (userInitiated || stopResult.Completed) {
			if err := h.WatchScrobbler.ScrobbleStop(ctx, event); err != nil {
				slog.WarnContext(ctx, "failed to queue watch provider stop scrobble", "component", "api", "session", session.ID, "error", err)
			}
		} else if ok {
			if err := h.WatchScrobbler.ScrobblePause(ctx, event); err != nil {
				slog.WarnContext(ctx, "failed to queue watch provider pause scrobble", "component", "api", "session", session.ID, "error", err)
			}
		}
	}
	if h.AdminStore != nil {
		if err := h.AdminStore.DeleteSession(ctx, session.ID); err != nil {
			slog.ErrorContext(ctx, "failed to delete synced session", "component", "api", "session", session.ID, "error", err)
		}
	}

	h.deleteProxyGrantV3(ctx, session.ID)
	h.deleteNodeRecipeV3(ctx, session.TranscodeTransportID)
	h.closeTranscodeForSession(session)
	if syncNow {
		h.syncSessionsNow(ctx, syncReason)
	}
	return stopResult
}

// recordStopHistory writes a stopped session to watch history and the admin
// playback log. The admin log keeps one row per session.
func (h *PlaybackHandler) recordStopHistory(ctx context.Context, session *playback.Session) watchstate.PlaybackStopResult {
	// A Jellyfin session copy without a position never saw the play's
	// progress: a start that failed to route, or a replica that only served
	// media while another replica took the reports. It must not record the
	// play in place of a copy that did.
	if session.IsJellyfinCompat && session.Position <= 0 {
		return watchstate.PlaybackStopResult{}
	}
	result := h.persistStopAndHistory(ctx, session)
	if entry, err := h.buildAdminHistoryEntry(ctx, session); err != nil {
		slog.ErrorContext(ctx, "failed to build admin history", "component", "api", "session", session.ID, "error", err)
	} else if entry != nil && h.AdminStore != nil {
		if err := h.AdminStore.RecordHistory(ctx, *entry); err != nil {
			slog.ErrorContext(ctx, "failed to record admin history", "component", "api", "session", session.ID, "error", err)
		}
	}
	return result
}

// compatPlayHistoryNamespace derives a Jellyfin play's watch-history row ID
// from its native session ID.
var compatPlayHistoryNamespace = uuid.MustParse("5d0f2b8e-3c4a-4f61-9e7b-2a8c1d6e4b90")

// compatPlayHistoryID names the watch-history row of a Jellyfin session. The
// replica that receives the client's stop finishes its copy, and any replica
// holding another copy of the session expires that copy once the play is
// over; with one row ID per session, the first copy that can record the play
// does and the rest change nothing.
func compatPlayHistoryID(sessionID string) string {
	return uuid.NewSHA1(compatPlayHistoryNamespace, []byte(sessionID)).String()
}

func (h *PlaybackHandler) finalizeSessionAbort(ctx context.Context, session *playback.Session, syncNow bool, syncReason string) {
	if h == nil || session == nil || session.ID == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.cancelPlaybackStartSideEffectsV3(ctx, session.ID)

	if h.WatchScrobbler != nil && h.fileResolver != nil {
		if file, err := h.loadFileByPreferredID(ctx, requestedMediaFileID(session), session.MediaFileID); err == nil && file != nil {
			targetID := playbackProgressTarget(file)
			if targetID != "" {
				event := h.scrobbleEventForSession(ctx, session, targetID, float64(file.Duration), session.Position)
				if err := h.WatchScrobbler.ScrobblePause(ctx, event); err != nil {
					slog.WarnContext(ctx, "failed to queue watch provider abort scrobble", "component", "api", "session", session.ID, "error", err)
				}
			}
		}
	}

	if h.AdminStore != nil {
		if err := h.AdminStore.DeleteSession(ctx, session.ID); err != nil {
			slog.ErrorContext(ctx, "failed to delete synced session", "component", "api", "session", session.ID, "error", err)
		}
	}

	// Abort is a connection drop / non-terminal teardown — keep the recipe card
	// so the client can reconstruct on reconnect.
	h.closeTranscodeForSession(session)
	if syncNow {
		h.syncSessionsNow(ctx, syncReason)
	}
}

func (h *PlaybackHandler) handleExpiredSession(session *playback.Session) {
	if h == nil || session == nil {
		return
	}
	sessionCopy := *session
	go func() {
		slog.Info("expired inactive playback session", append([]any{
			"session", sessionCopy.ID, "playback_session_id", sessionCopy.ID,
		}, sessionCopy.ClientInfo().LogAttrs()...)...)
		ctx := context.Background()
		// Another replica may own the live copy: its progress and media
		// requests never touch this replica's activity clock. A row that saw
		// progress after this copy went idle, or that is already stopped,
		// means this copy is stale, not the session. Drop it without writing
		// history, the deny marker, or a stop over the other replica's.
		if h.attemptStoppedElsewhere(ctx, sessionCopy.ID) || h.attemptActiveElsewhere(ctx, &sessionCopy) {
			slog.Info("dropped stale local playback session copy", "session", sessionCopy.ID, "playback_session_id", sessionCopy.ID)
			h.closeTranscodeForSession(&sessionCopy)
			return
		}
		// Expiry is a liveness reap, not a user stop — keep the recipe card so a
		// resume reconstructs under the same id (the card's own TTL reaps it if
		// the session is truly abandoned).
		h.finalizeSessionStop(ctx, &sessionCopy, false, "", false)
		// The attempt is over: mark its row stopped under a server-minted stop
		// id so a start replay reports session_expired on every replica, and
		// deny its tokens so no replica serves it again.
		h.markAttemptStoppedServerSide(ctx, sessionCopy.ID)
	}()
}

// handleFinishedSession records a play that another playback frontend ended
// through FinishSession. That frontend owns the rest of its stop: transcode
// and transport teardown, watch-provider scrobbles, and the live-session sync.
func (h *PlaybackHandler) handleFinishedSession(ctx context.Context, session *playback.Session) {
	if h == nil || session == nil || session.ID == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Clients often drop the connection right after reporting a stop.
	h.recordStopHistory(context.WithoutCancel(ctx), session)
}

func playbackProgressTarget(file *models.MediaFile) string {
	if file == nil {
		return ""
	}
	if file.EpisodeID != "" {
		return file.EpisodeID
	}
	return file.ContentID
}

func (h *PlaybackHandler) persistSeriesPlaybackPreference(
	ctx context.Context,
	userID int,
	profileID string,
	file *models.MediaFile,
) {
	if h.StoreProvider == nil || file == nil {
		return
	}

	seriesID := h.resolveSeriesID(ctx, file)
	if seriesID == "" {
		return
	}

	store, err := h.StoreProvider.ForUser(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to access user store for series playback preference", "component", "api", "user_id", userID, "error", err)
		return
	}

	if err := store.SetSeriesPlaybackPreference(ctx, userstore.SeriesPlaybackPreference{
		ProfileID:  profileID,
		SeriesID:   seriesID,
		Resolution: file.Resolution,
		HDR:        file.HDR,
		CodecVideo: file.CodecVideo,
	}); err != nil {
		slog.ErrorContext(ctx, "failed to persist series playback preference", "component", "api", "series_id", seriesID, "profile_id", profileID, "error", err)
	}
}

func (h *PlaybackHandler) persistAudioPreference(
	ctx context.Context,
	userID int,
	profileID string,
	file *models.MediaFile,
	trackIndex int,
) {
	if h.StoreProvider == nil || file == nil || trackIndex < 0 || trackIndex >= len(file.AudioTracks) {
		return
	}

	seriesID := h.resolveSeriesID(ctx, file)
	if seriesID == "" {
		return
	}

	store, err := h.StoreProvider.ForUser(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to access user store for audio preference", "component", "api", "user_id", userID, "error", err)
		return
	}

	track := file.AudioTracks[trackIndex]
	if err := store.SetAudioPreference(ctx, userstore.AudioPreference{
		ProfileID:       profileID,
		SeriesID:        seriesID,
		AudioTrackIndex: trackIndex,
		AudioLanguage:   track.Language,
		TrackSignature:  playback.AudioTrackSignatureFromTrack(track),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to persist audio preference", "component", "api", "series_id", seriesID, "profile_id", profileID, "error", err)
	}
}

// --- Handler methods ---

// HandleStartPlayback starts playback. Protocol v3 is the only protocol this
// endpoint speaks: a start that does not declare it comes from a build that
// predates the contract and cannot interpret a plan, so it is refused with
// 426 rather than served something it would misread.
func (h *PlaybackHandler) HandleStartPlayback(w http.ResponseWriter, r *http.Request) {
	if apimw.GetUserID(r.Context()) == 0 {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPlaybackV3BodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}
	var envelope struct {
		ProtocolVersion        *int            `json:"protocol_version"`
		AllowAlternateVersions json.RawMessage `json:"allow_alternate_versions"`
		Capabilities           *struct {
			VideoEvidence *string `json:"video_evidence"`
			AudioEvidence *string `json:"audio_evidence"`
		} `json:"client_capabilities"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}
	if envelope.ProtocolVersion == nil || *envelope.ProtocolVersion != playback.ProtocolV3 ||
		envelope.Capabilities == nil || envelope.Capabilities.VideoEvidence == nil || envelope.Capabilities.AudioEvidence == nil {
		upgrade := playback.LegacyUpgradeErrorV3()
		writeError(w, http.StatusUpgradeRequired, upgrade.Error, upgrade.Message)
		return
	}
	// V1 remains frozen: ignore the v2-only fixed-source control just as the
	// legacy decoder ignored this unknown field before it was introduced.
	if len(envelope.AllowAlternateVersions) > 0 {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(body, &fields) // The envelope was validated above.
		delete(fields, "allow_alternate_versions")
		body, _ = json.Marshal(fields)
	}
	h.handleStartPlaybackV3(w, r, body)
}

func playbackClientInfoFromRequest(r *http.Request) playback.ClientInfo {
	if r == nil {
		return playback.ClientInfo{}
	}
	// Clamped here, at the boundary, rather than only where the session stamps
	// them: the decision logs and playback_route_events are written from this
	// value directly, so a client sending a header-sized build would otherwise
	// reach both despite the published bound. Values stay opaque — trimmed and
	// length-clamped, never parsed or validated against an enum.
	return playback.ClientInfo{
		Name:      httpheader.GetClientInfo(r.Header).Name,
		Version:   httpheader.GetClientInfo(r.Header).Version,
		Build:     httpheader.GetClientInfo(r.Header).Build,
		Channel:   httpheader.GetClientInfo(r.Header).Channel,
		UserAgent: r.UserAgent(),
	}.Normalized()
}

// HandleUpdateProgress handles POST /playback/{session_id}/progress.
func (h *PlaybackHandler) HandleUpdateProgress(w http.ResponseWriter, r *http.Request) {
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
	session, err := h.sessionMgr.GetSession(sessionID)
	if err != nil {
		if errors.Is(err, playback.ErrSessionNotFound) {
			// Progress for a session that is already gone (e.g. a version
			// switch deleted it while a 10s progress tick was in flight) is a
			// benign no-op, not an error the client must handle. Answer 204 so
			// browsers don't log a 404 on every switch.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to load playback session")
		return
	}
	if session.UserID != userID {
		writeError(w, http.StatusForbidden, "forbidden", "Session belongs to another user")
		return
	}
	wasPaused := session.IsPaused

	var req progressRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	err = h.sessionMgr.UpdateProgress(sessionID, req.Position, req.IsPaused)
	if err != nil {
		if errors.Is(err, playback.ErrSessionNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to update progress")
		return
	}
	h.syncSessionsOnPauseChange(r.Context(), wasPaused, req.IsPaused)

	// Persist progress to UserStore (best-effort).
	if sess, getErr := h.sessionMgr.GetSession(sessionID); getErr == nil {
		h.persistProgress(r.Context(), sess)
		// The first progress report after attach is the probe-before-attach
		// hook: evidence may have landed while no session was registered, so
		// the attach-side check replayed nothing yet. Replays are idempotent
		// (a settled selection is a byte-equal no-op), bounded to one
		// heartbeat-triggered check per session attach generation.
		h.reconcilePendingAudioStartup(r.Context(), sessionID)
		if !sess.DisableProgressPersistence && h.WatchScrobbler != nil && wasPaused != sess.IsPaused {
			if file, loadErr := h.loadFileByPreferredID(r.Context(), requestedMediaFileID(sess), sess.MediaFileID); loadErr == nil && file != nil {
				targetID := playbackProgressTarget(file)
				if targetID != "" {
					event := h.scrobbleEventForSession(r.Context(), sess, targetID, float64(file.Duration), sess.Position)
					if sess.IsPaused {
						if err := h.WatchScrobbler.ScrobblePause(r.Context(), event); err != nil {
							slog.WarnContext(r.Context(), "failed to queue watch provider pause scrobble", "component", "api", "session", sessionID, "error", err)
						}
					} else if err := h.WatchScrobbler.ScrobbleStart(r.Context(), event); err != nil {
						slog.WarnContext(r.Context(), "failed to queue watch provider resume scrobble", "component", "api", "session", sessionID, "error", err)
					}
				}
			}
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleStopPlayback handles DELETE /playback/{session_id}.
func (h *PlaybackHandler) HandleStopPlayback(w http.ResponseWriter, r *http.Request) {
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

	session, err := h.sessionMgr.GetSession(sessionID)
	if err != nil {
		if errors.Is(err, playback.ErrSessionNotFound) {
			writePlaybackSessionNotFound(w)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to load playback session")
		return
	}
	if session.UserID != userID {
		writeError(w, http.StatusForbidden, "forbidden", "Session belongs to another user")
		return
	}

	err = h.stopPlaybackSession(r.Context(), session, true)
	if err != nil {
		if errors.Is(err, playback.ErrSessionNotFound) {
			writePlaybackSessionNotFound(w)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to stop playback session")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *PlaybackHandler) loadAuthorizedFile(r *http.Request, fileID int) (*models.MediaFile, error) {
	if h.fileResolver == nil || h.ItemAccess == nil {
		return nil, fmt.Errorf("playback authorization dependencies not configured")
	}
	file, err := h.fileResolver.GetByID(r.Context(), fileID)
	if err != nil {
		return nil, mapMediaFileLookupError(err)
	}
	if file == nil || file.MissingSince != nil {
		return nil, catalog.ErrItemNotFound
	}

	filter := requestAccessFilter(r)
	switch {
	case file.EpisodeID != "":
		if h.EpisodeLookup == nil {
			return nil, fmt.Errorf("episode lookup not configured")
		}
		episode, err := h.EpisodeLookup.GetByID(r.Context(), file.EpisodeID)
		if err != nil {
			return nil, err
		}
		if episode == nil {
			return nil, catalog.ErrEpisodeNotFound
		}
		if err := h.ItemAccess.EnsureAccessible(r.Context(), episode.SeriesID, filter); err != nil {
			return nil, err
		}
	case file.ContentID != "":
		if err := h.ItemAccess.EnsureAccessible(r.Context(), file.ContentID, filter); err != nil {
			return nil, err
		}
	case file.ExtraID != "":
		if h.ExtraLookup == nil {
			return nil, fmt.Errorf("extra lookup not configured")
		}
		extra, err := h.ExtraLookup.GetByID(r.Context(), file.ExtraID)
		if err != nil {
			if errors.Is(err, catalog.ErrExtraNotFound) {
				return nil, catalog.ErrItemNotFound
			}
			return nil, err
		}
		if extra == nil {
			return nil, catalog.ErrItemNotFound
		}
		if err := h.ItemAccess.EnsureAccessible(r.Context(), extra.ParentID, filter); err != nil {
			return nil, err
		}
	default:
		return nil, catalog.ErrItemNotFound
	}

	if !catalog.FileAllowedByAccess(file, filter) {
		return nil, catalog.ErrItemNotFound
	}

	return file, nil
}

// computeStartSegment returns the HLS segment number corresponding to a seek
// position given the segment duration. Both remote and local transcode paths
// use this to align ffmpeg output filenames with the VOD manifest.
func computeStartSegment(seekSeconds float64, segmentDuration int) int {
	if segmentDuration <= 0 {
		segmentDuration = 2
	}
	if seekSeconds <= 0 {
		return 0
	}
	return int(seekSeconds / float64(segmentDuration))
}

// alignedSeekSeconds snaps an encoded transcode's ffmpeg start position down
// to the boundary of the segment computeStartSegment assigns it. The synthetic
// VOD manifest declares segment N to begin at exactly N×segmentDuration;
// spawning ffmpeg at the raw seek position makes segment N actually begin up
// to one segment later, and hls.js aligns that content to the declared
// position — shifting the session's entire timeline (audio, video, and every
// out-of-band subtitle cue) late by seek mod segmentDuration. Copy-mode
// sessions serve ffmpeg's real manifest, whose declared timings match the
// fragments it produces, so they keep the raw seek.
func alignedSeekSeconds(seekSeconds float64, segmentDuration int, targetVideoCodec string) float64 {
	if strings.EqualFold(targetVideoCodec, "copy") || seekSeconds <= 0 {
		return seekSeconds
	}
	if segmentDuration <= 0 {
		segmentDuration = 2
	}
	return float64(computeStartSegment(seekSeconds, segmentDuration) * segmentDuration)
}

// HandleGetTranscodeManifest handles GET /playback/transcode/{session_id}/master.m3u8.
// Auth is optional — the session UUID serves as an access token (same pattern
// as /stream/{session_id}). When auth context is present, ownership is verified.
//
// Known-duration encoded sessions expose a synthetic full VOD manifest so the
// player can seek immediately. Copy-video sessions expose FFmpeg's real
// keyframe-aligned manifest and use the resolved stream origin the v3 plan
// reports as the timeline's stream_origin_seconds.
//
// Errors: 404 (playback_session_not_found / not_found) when the session is
// missing or cannot be reconstructed, 503 (unavailable) while the transcode is
// temporarily unavailable, and — for tone-map execution failures — 422
// (unsupported) with an X-Vio-Tone-Map-Execution-Error header of
// source_revision_changed or source_preflight_rejected. The 422 responses are
// additive to the existing 404 and 503 cases; the Jellyfin-compatible 415
// mapping is a separate surface and unchanged.
func (h *PlaybackHandler) HandleGetTranscodeManifest(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "session_id")
	session, status, card, claims, reconstructErr := h.loadTranscodeServeSession(r, sessionID, -1)
	switch status {
	case playback.SessionMissing:
		writePlaybackSessionNotFound(w)
		return
	case playback.SessionLoadFailed:
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to load playback session")
		return
	case playback.SessionForbidden:
		writeError(w, http.StatusForbidden, "forbidden", "Session belongs to another user")
		return
	case playback.SessionUnauthorized:
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	case playback.SessionUnavailable:
		if writePlaybackSessionEndedError(w, reconstructErr) || writeNativeRouteBindingErrorV3(w, reconstructErr) {
			return
		}
		if writePlaybackToneMapExecutionError(w, reconstructErr) {
			return
		}
		if card == nil || card.ToneMapMode == "" {
			// A plain (non-tone-mapped) transcode whose token recipe cannot be
			// rebuilt is served the same 404 the second-chance reconstruct
			// branch produces: the session is effectively gone, and the client
			// re-plans rather than retrying a dead encode.
			writeError(w, http.StatusNotFound, "not_found", "Transcode session not found")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Transcode session is temporarily unavailable")
		return
	}
	if !requireOwningProfile(w, r, session) {
		return
	}
	if !requireNativeSessionAPIEgressV3(w, session) {
		return
	}
	attachPlaybackSession(r.Context(), session, claims)

	transcodeSession := h.tm.GetTranscodeSession(sessionID)
	if transcodeSession == nil {
		// No local session — try proxying to remote transcode node.
		if session.TranscodeNodeURL != "" {
			h.touchSessionActivity(sessionID)
			_ = h.proxyToTranscodeNode(w, r, session.TranscodeNodeURL,
				"/transcode/"+remoteTransportID(session)+"/master.m3u8")
			return
		}
		// Local transcode whose process state was lost: reconstruct it from the
		// token recipe. The manifest path has no segment context, so pass -1 (use
		// the token's seek position).
		var secondChanceErr error
		transcodeSession, secondChanceErr = h.reconstructTransportForServe(r.Context(), sessionID, -1, card)
		if secondChanceErr != nil && writePlaybackToneMapExecutionError(w, secondChanceErr) {
			return
		}
		if transcodeSession == nil {
			writeError(w, http.StatusNotFound, "not_found", "Transcode session not found")
			return
		}
	}
	h.touchSessionActivity(sessionID)

	// A generation whose decoder has already rejected the source cannot produce
	// a playable manifest; answer permanently so the client replans instead of
	// reloading a playlist it can never play. The hardware->software retry runs
	// through the client's failure_recovery replan, keyed on this verdict.
	if transcodeSession.IsSourceRejected() {
		writePlaybackDecodeError(w)
		return
	}

	manifest, err := transcodeSession.BuildPlaybackManifest("segment/", r.URL.RawQuery)
	if err != nil {
		// Switchover overlap: a replan publishes its successor only after the
		// successor's first manifest is ready, so this live path normally
		// succeeds. When it does not (the predecessor's directory is retained
		// while the client's old playlist is in flight), serve the displaced
		// generation instead of 503.
		if fallbackManifest, ok := h.retainedGenerationManifest(sessionID, "segment/", r.URL.RawQuery); ok {
			slog.InfoContext(r.Context(), "transcode manifest served from the retained switchover generation",
				"component", "api", "session", sessionID, "playback_session_id", sessionID)
			manifest, err = fallbackManifest, nil
		}
	}
	if err != nil {
		// A client stop (DELETE) cancels the transcode context, killing the
		// encoder while an in-flight manifest build is running. That race is
		// the expected teardown path, not a server fault.
		if manifestBuildFailureIsClientStop(h, sessionID) {
			slog.InfoContext(r.Context(), "transcode manifest skipped; session stopped by client",
				"component", "api", "session", sessionID, "playback_session_id", sessionID)
			writeError(w, http.StatusNotFound, "not_found", "Transcode session not found")
			return
		}
		// A slow-but-alive encoder (upstream stall, slow probe) must not kill
		// the session: answer 503 + Retry-After with a retryable code so the
		// client polls the manifest again instead of erroring out. A 200 with
		// a segment-less playlist is fatal to hls.js ("no levels found"), so
		// the retry signal stays a 503 — but a retryable one, distinct from
		// the dead-encoder case below. This is what the ff4ecbcf incident
		// showed: ffmpeg ran 20 minutes producing segments while four polls
		// 503'd with a body the client treated as terminal.
		if transcodeSession != nil && !transcodeSession.IsSourceRejected() && transcodeSession.IsRunning() {
			w.Header().Set("Retry-After", "2")
			writeError(w, http.StatusServiceUnavailable, "not_ready_retry", "Transcode manifest not ready yet")
			return
		}
		slog.ErrorContext(r.Context(), "build transcode manifest", "component", "api", "error", err, "session", sessionID, "playback_session_id", sessionID)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Transcode manifest not ready")
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(manifest)
}

func writePlaybackToneMapExecutionError(w http.ResponseWriter, err error) bool {
	if errors.Is(err, tonemap.ErrSourceRevisionChanged) {
		w.Header().Set(transcodenode.ToneMapExecutionErrorHeader, transcodenode.ToneMapSourceRevisionChangedCode)
		writeError(w, http.StatusUnprocessableEntity, "unsupported", "Tone-map source changed")
		return true
	}
	if errors.Is(err, playback.ErrToneMapSourceValidationUnavailable) {
		w.Header().Set(transcodenode.ToneMapExecutionErrorHeader, transcodenode.ToneMapSourceValidationUnavailableCode)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Transcode session is temporarily unavailable")
		return true
	}
	if errors.Is(err, tonemap.ErrSourcePreflightRejected) {
		w.Header().Set(transcodenode.ToneMapExecutionErrorHeader, transcodenode.ToneMapSourcePreflightRejectedCode)
		writeError(w, http.StatusUnprocessableEntity, "unsupported", "Tone-map source is unsupported by the selected executor")
		return true
	}
	return false
}

// transcodeDecodeErrorHeader names the machine-readable decode verdict on a
// manifest/segment response that revokes a generation whose decoder rejected
// the source. Clients classify on it to replan instead of retrying the stream.
// transcodeDecodeErrorCode is the value of that header, distinct from the
// tone-map header so a client can tell an undecodable source from an
// executor-recipe mismatch.
const (
	transcodeDecodeErrorHeader = playback.DecodeErrorHeader
	transcodeDecodeErrorCode   = playback.DecodeErrorSourceRejectedCode
)

// writePlaybackDecodeError answers a media route whose running decoder has
// rejected the source. It is deliberately permanent (422, not a 404 retry
// loop): hls.js would otherwise exhaust its recovery budget and report only a
// startup timeout, leaving the reason invisible to the player and the owner.
func writePlaybackDecodeError(w http.ResponseWriter) {
	w.Header().Set(transcodeDecodeErrorHeader, transcodeDecodeErrorCode)
	writeError(w, http.StatusUnprocessableEntity, "decode_failed", "The media source could not be decoded.")
}

// writePlaybackSegmentError maps a segment-retrieval failure to its HTTP
// response. A segment that is absent (ErrSegmentNotFound) or whose transcode
// process exited before the segment materialized (ErrTranscodeFailed) is
// terminal for this generation: it will never appear, so the client re-plans
// instead of retrying a dead encode. A user stop that kills ffmpeg while a
// segment request is already waiting surfaces as ErrTranscodeFailed, and a
// stopped session is not a server defect. Both map to 404, mirroring
// hlsSegmentErrorResponse on the Jellyfin-compatible surface. A playlist that
// is still being produced (ErrManifestNotReady) is transient and stays
// retryable as 503. A restart whose virtual-provider resolve hit the
// session-bound absent-pin / trusted-persisted sentinels is a dependency
// failure the client can recover from on the next segment request (provider
// relists renumber ids), so it is a retryable 503 with a
// virtual_resolve_failed problem code instead of a fatal 500 the player
// treats as a dead generation. Everything else is an unexpected server error.
func writePlaybackSegmentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, playback.ErrSegmentNotFound), errors.Is(err, playback.ErrTranscodeFailed):
		writeError(w, http.StatusNotFound, "not_found", "Segment not found")
	case errors.Is(err, playback.ErrStaleSegmentGeneration):
		// The URL named a generation that is no longer live and has no retained
		// bytes to serve. A valid response would mix generations, so refuse with
		// a permanent precondition failure; the client reloads the manifest and
		// re-addresses the segment to the current generation.
		writeError(w, http.StatusPreconditionFailed, "stale_generation", "Segment generation is stale")
	case errors.Is(err, playback.ErrManifestNotReady):
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Transcode session is temporarily unavailable")
	case errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent), errors.Is(err, virtuallibrary.ErrPersistedCandidateTrusted):
		writeError(w, http.StatusServiceUnavailable, "virtual_resolve_failed", "Failed to resolve virtual source")
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to load segment")
	}
}

// retainedGenerationManifest builds the displaced generation's playlist for
// the served route, reporting false when no retained generation exists or it
// cannot produce a manifest.
func (h *PlaybackHandler) retainedGenerationManifest(sessionID, segPrefix, rawQuery string) ([]byte, bool) {
	retained := h.tm.GetRetainedTranscodeSession(sessionID)
	if retained == nil {
		return nil, false
	}
	manifest, err := retained.BuildPlaybackManifest(segPrefix, rawQuery)
	if err != nil {
		return nil, false
	}
	return manifest, true
}

// HandleGetTranscodeSegment handles GET /playback/transcode/{session_id}/segment/{name}.
// Authorization follows the same negotiated legacy-versus-header-authenticated
// rule as the manifest endpoint above.
//
// Errors: 404 (playback_session_not_found / not_found) when the session is
// missing or cannot be reconstructed, or the segment does not exist, or the
// transcode process exited before the segment materialized (including a stop
// that killed ffmpeg mid-request); 503 (unavailable) while the transcode is
// temporarily unavailable; and — for tone-map execution failures — 422
// (unsupported) with an X-Vio-Tone-Map-Execution-Error header of
// source_revision_changed or source_preflight_rejected. The 422 responses are
// additive to the existing 404 and 503 cases; the Jellyfin-compatible 415
// mapping is a separate surface and unchanged.
func (h *PlaybackHandler) HandleGetTranscodeSegment(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "session_id")
	requestedSegment := -1
	if segNum, parseErr := playback.ParseSegmentNumber(chi.URLParam(r, "name")); parseErr == nil {
		requestedSegment = segNum
	}
	session, status, card, claims, reconstructErr := h.loadTranscodeServeSession(r, sessionID, requestedSegment)
	switch status {
	case playback.SessionMissing:
		writePlaybackSessionNotFound(w)
		return
	case playback.SessionLoadFailed:
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to load playback session")
		return
	case playback.SessionForbidden:
		writeError(w, http.StatusForbidden, "forbidden", "Session belongs to another user")
		return
	case playback.SessionUnauthorized:
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	case playback.SessionUnavailable:
		if writePlaybackSessionEndedError(w, reconstructErr) || writeNativeRouteBindingErrorV3(w, reconstructErr) {
			return
		}
		if writePlaybackToneMapExecutionError(w, reconstructErr) {
			return
		}
		if card == nil || card.ToneMapMode == "" {
			// A plain (non-tone-mapped) transcode whose token recipe cannot be
			// rebuilt is served the same 404 the second-chance reconstruct
			// branch produces: the session is effectively gone, and the client
			// re-plans rather than retrying a dead encode.
			writeError(w, http.StatusNotFound, "not_found", "Transcode session not found")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Transcode session is temporarily unavailable")
		return
	}
	if !requireOwningProfile(w, r, session) {
		return
	}
	if !requireNativeSessionAPIEgressV3(w, session) {
		return
	}
	attachPlaybackSession(r.Context(), session, claims)

	// Capture the delivered virtual candidate and its health state before the
	// segment is served. A full-segment delivery may clear exactly the failure
	// stamp observed here; a failure that lands while bytes are in flight is
	// newer and the identity/failed_at fence preserves it, mirroring the
	// direct-play path's transport-start capture. Recovery runs once per
	// delivered session (the first fully served segment), so a long stream does
	// not issue a read+write per segment.
	virtualFileID, virtualDeliveredPath, virtualDelivery := virtualTranscodeDelivery(session, card)
	if virtualDelivery {
		if _, cleared := h.virtualDeliveryCleared.Load(sessionID); cleared {
			virtualDelivery = false
		}
	}
	var virtualObservedFailedAt *time.Time
	if virtualDelivery {
		virtualObservedFailedAt = h.observeVirtualCandidateFailure(r.Context(), virtualFileID)
	}

	transcodeSession := h.tm.GetTranscodeSession(sessionID)
	if transcodeSession == nil {
		if session.TranscodeNodeURL != "" {
			h.touchSessionActivity(sessionID)
			segmentName := chi.URLParam(r, "name")
			delivered := h.proxyToTranscodeNode(w, r, session.TranscodeNodeURL,
				"/transcode/"+remoteTransportID(session)+"/segment/"+segmentName)
			if delivered && virtualDelivery && h.clearVirtualCandidateRecovered(r.Context(), virtualFileID, virtualDeliveredPath, virtualObservedFailedAt) {
				h.virtualDeliveryCleared.Store(sessionID, struct{}{})
			}
			return
		}
		// Resume near the segment the client is fetching so reconstruct does not
		// restart from the original seek point and stall. A non-segment name
		// (e.g. init.mp4) parses as negative and falls back to the token position.
		var secondChanceErr error
		transcodeSession, secondChanceErr = h.reconstructTransportForServe(r.Context(), sessionID, requestedSegment, card)
		if secondChanceErr != nil && writePlaybackToneMapExecutionError(w, secondChanceErr) {
			return
		}
		if transcodeSession == nil {
			writeError(w, http.StatusNotFound, "not_found", "Transcode session not found")
			return
		}
	}
	h.touchSessionActivity(sessionID)

	// Fail the generation before opening any segment: once the decoder has
	// rejected the source, the files on disk are garbage output and serving
	// them only delays the client's fatal error until its startup guard times
	// out. The permanent verdict drives the client's failure_recovery replan.
	if transcodeSession.IsSourceRejected() {
		writePlaybackDecodeError(w)
		return
	}

	segmentName := chi.URLParam(r, "name")
	requestedGeneration := strings.TrimSpace(r.URL.Query().Get(playback.GenerationQueryParam))
	// servedSession is the generation the returned lease came from: the live
	// one normally, the retained predecessor during the switchover overlap. The
	// delivery report below must name the session that owns the lease's
	// generation.
	servedSession := transcodeSession
	// A URL minted against an older generation is only answerable by a retained
	// generation that explicitly owns that token. Anything else is a stale read
	// that would silently mix generations, so refuse it before touching the
	// filesystem.
	var segmentLease *playback.SegmentLease
	var err error
	if requestedGeneration != "" && !transcodeSession.MatchesGenerationToken(requestedGeneration) {
		if retained := h.tm.GetRetainedTranscodeSession(sessionID); retained != nil && retained.MatchesGenerationToken(requestedGeneration) {
			segmentLease, err = retained.OpenSegmentForGeneration(segmentName, requestedGeneration)
			servedSession = retained
		} else {
			err = playback.ErrStaleSegmentGeneration
		}
	} else {
		segmentLease, err = transcodeSession.OpenSegmentForGeneration(segmentName, requestedGeneration)
	}
	if err != nil && errors.Is(err, playback.ErrSegmentNotFound) && requestedGeneration == "" {
		// Switchover overlap: a same-session replan publishes its successor
		// once the successor's first manifest is ready, but a client on a
		// token minted before segment URLs carried a generation keeps
		// requesting the session-keyed playlist and its old segments for a
		// moment after a track/version switch. A segment the new generation has
		// not produced, but the displaced generation still holds, is served
		// from the retained predecessor instead of entering the wait/restart
		// machinery (or 404-ing). A generation-scoped request is routed by its
		// token above, so it never falls through to here; the new generation
		// becomes authoritative as soon as it produces the segment. The
		// retained entry is replaced on the next switch and expires after the
		// bounded window, so stale bytes cannot be served indefinitely.
		if retained := h.tm.GetRetainedTranscodeSession(sessionID); retained != nil {
			if lease, retainedErr := retained.OpenSegment(segmentName); retainedErr == nil {
				slog.InfoContext(r.Context(), "transcode segment served from the retained switchover generation",
					"component", "api", "session", sessionID, "playback_session_id", sessionID, "segment", segmentName)
				segmentLease, err = lease, nil
				servedSession = retained
			}
		}
	}
	// Wait/restart recovery may only run against the generation the request
	// named. A matching retained generation that lacks the segment is terminal
	// (its bytes were never produced and the live generation is a different
	// stream), and a token that no longer names the live generation must not
	// buy the request a wait or an FFmpeg restart it will refuse under the
	// final lease fence. Convert both to the stale-generation verdict so the
	// client reloads the manifest instead.
	if err != nil && errors.Is(err, playback.ErrSegmentNotFound) &&
		requestedGeneration != "" && !transcodeSession.MatchesGenerationToken(requestedGeneration) {
		err = playback.ErrStaleSegmentGeneration
	}
	if err != nil && errors.Is(err, playback.ErrSegmentNotFound) {
		segNum, parseErr := playback.ParseSegmentNumber(segmentName)
		if parseErr == nil {
			now := time.Now()
			decision := transcodeSession.SegmentRecoveryDecision(segNum, now)
			lastProducedAgeMS := int64(-1)
			if !decision.Progress.LastProducedAt.IsZero() {
				lastProducedAgeMS = now.Sub(decision.Progress.LastProducedAt).Milliseconds()
			}
			slog.InfoContext(r.Context(), "transcode segment missing", "component", "api",
				"segment", segmentName,
				"requested_segment", segNum,
				"produced_head", decision.Progress.ProducedHead,
				"last_requested_segment", decision.Progress.LastRequestedSegment,
				"start_segment_number", decision.Progress.StartSegmentNumber,
				"last_produced_age_ms", lastProducedAgeMS,
				"wait_timeout_ms", decision.WaitTimeout.Milliseconds(),
				"restart_on_timeout", decision.RestartOnTimeout,
				"reason", decision.Reason,
				"session", sessionID,
				"playback_session_id", sessionID,
			)
			if decision.Wait {
				slog.InfoContext(r.Context(), "transcode segment wait", "component", "api",
					"segment", segmentName,
					"requested_segment", segNum,
					"produced_head", decision.Progress.ProducedHead,
					"last_requested_segment", decision.Progress.LastRequestedSegment,
					"start_segment_number", decision.Progress.StartSegmentNumber,
					"last_produced_age_ms", lastProducedAgeMS,
					"wait_timeout_ms", decision.WaitTimeout.Milliseconds(),
					"restart_on_timeout", decision.RestartOnTimeout,
					"reason", decision.Reason,
					"session", sessionID,
					"playback_session_id", sessionID,
				)
				segmentLease, err = transcodeSession.WaitForOpenSegment(segmentName, decision.WaitTimeout)
				if err != nil && errors.Is(err, playback.ErrSegmentNotFound) {
					slog.InfoContext(r.Context(), "transcode segment wait timeout", "component", "api",
						"segment", segmentName,
						"requested_segment", segNum,
						"produced_head", decision.Progress.ProducedHead,
						"last_requested_segment", decision.Progress.LastRequestedSegment,
						"start_segment_number", decision.Progress.StartSegmentNumber,
						"last_produced_age_ms", lastProducedAgeMS,
						"wait_timeout_ms", decision.WaitTimeout.Milliseconds(),
						"restart_on_timeout", decision.RestartOnTimeout,
						"reason", decision.Reason,
						"session", sessionID,
						"playback_session_id", sessionID,
					)
				}
			}

			// If the segment is still missing (timed out, or outside the
			// active encode range), either restart at the exact manifest-derived
			// timeline position or return 404 for copy-mode segments outside the
			// current manifest window.
			if err != nil && errors.Is(err, playback.ErrSegmentNotFound) && decision.RestartOnTimeout {
				target, ok, restartErr := h.tm.RestartSegmentLocked(
					r.Context(),
					sessionID,
					transcodeSession,
					segNum,
				)
				if restartErr != nil && !errors.Is(restartErr, playback.ErrManifestNotReady) {
					slog.ErrorContext(r.Context(), "restart transcode at missing segment", "component", "api", "error", restartErr, "segment", segmentName, "session", sessionID, "playback_session_id", sessionID)
				}

				// Copy-mode with an unresolved seek target (ok=false, no error)
				// means the manifest can't place this segment yet. Don't restart
				// at a fabricated position; surface ErrSegmentNotFound so the
				// client retries while the session keeps producing manifest.
				// Mirrors the transcode-node guard in
				// internal/transcodenode/server.go.
				if !ok && restartErr == nil && transcodeSession.IsCopyVideo() {
					err = playback.ErrSegmentNotFound
				}

				if restartErr != nil {
					err = restartErr
				} else if ok {
					slog.InfoContext(r.Context(), "transcode seek restart", "component", "api",
						"segment", segmentName,
						"requested_segment", segNum,
						"produced_head", decision.Progress.ProducedHead,
						"last_requested_segment", decision.Progress.LastRequestedSegment,
						"start_segment_number", decision.Progress.StartSegmentNumber,
						"last_produced_age_ms", lastProducedAgeMS,
						"wait_timeout_ms", decision.WaitTimeout.Milliseconds(),
						"restart_on_timeout", decision.RestartOnTimeout,
						"reason", decision.Reason,
						"seek_seconds", target.SeekSeconds,
						"stream_origin_seconds", target.StreamOriginSeconds,
						"resolved_start_segment", target.StartSegmentNumber,
						"session", sessionID,
						"playback_session_id", sessionID,
					)
					// Throttler + exit monitor re-arm via the session's
					// restart hook.
					segmentLease, err = transcodeSession.WaitForOpenSegment(segmentName, 30*time.Second)
					if err == nil && strings.EqualFold(transcodeSession.Opts().TargetCodecVideo, "copy") {
						// Copy-mode seeks can resume as soon as the target segment
						// exists, but that sometimes leaves the player one segment
						// away from stalling while FFmpeg catches up. Briefly wait
						// for a single lookahead fragment when available so the
						// first resumed playback window is less brittle.
						nextSegmentName := fmt.Sprintf("seg_%05d%s", segNum+1, filepath.Ext(segmentName))
						if nextSegment, nextErr := transcodeSession.WaitForOpenSegment(nextSegmentName, 1200*time.Millisecond); nextErr == nil {
							_ = nextSegment.Close()
						}
					}
				}
			}
		} else if transcodeSession.IsRunning() {
			// Non-numbered segment (e.g., init.mp4 for fMP4 HLS).
			// Wait briefly — the init segment is written almost immediately.
			segmentLease, err = transcodeSession.WaitForOpenSegment(segmentName, 10*time.Second)
		}
	}
	// Any recovery path above (a wait or a restart) can return a lease from a
	// different generation than the URL named. Re-apply the fence before serving
	// so a request minted against one generation is never answered with another's
	// bytes.
	if err == nil {
		if segmentLease, err = playback.FenceSegmentLease(segmentLease, requestedGeneration); err != nil {
			segmentLease = nil
		}
	}
	if err != nil {
		if writePlaybackToneMapExecutionError(w, err) {
			return
		}
		writePlaybackSegmentError(w, err)
		return
	}

	w.Header().Set("Cache-Control", "no-store, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	defer func() { _ = segmentLease.Close() }()
	sw := httpstream.NewRollingDeadlineWriter(w)
	http.ServeContent(sw, r, segmentLease.Info.Name(), segmentLease.Info.ModTime(), segmentLease.File)
	if r.Method == http.MethodGet &&
		sw.CompletedFullResponse(segmentLease.Info.Size()) {
		if segNum, parseErr := playback.ParseSegmentNumber(segmentName); parseErr == nil {
			servedSession.ReportSegmentDownloadedForGeneration(segNum, segmentLease.Generation)
		}
		// The complete representation reached the client, so this is the
		// HLS/transcode "it really delivered" signal — the counterpart of the
		// direct-play positive-byte evidence. Report it before the lease closes.
		if virtualDelivery && h.clearVirtualCandidateRecovered(r.Context(), virtualFileID, virtualDeliveredPath, virtualObservedFailedAt) {
			h.virtualDeliveryCleared.Store(sessionID, struct{}{})
		}
	}
}

// virtualTranscodeDelivery reports the catalog row and pinned candidate URI an
// HLS/transcode session is delivering when that session is virtual. The
// delivered path is the durable virtual URI captured by the session or recipe
// card (the pinned result= candidate), which the row retains until a candidate
// rotation rewrites it; the fence in MarkVirtualCandidateRecovered is what
// makes a rotated row a no-op. Non-virtual or unidentified deliveries return
// ok=false, so no recovery is attempted.
func virtualTranscodeDelivery(session *playback.Session, card *playback.RecipeCard) (fileID int, deliveredPath string, ok bool) {
	if session != nil {
		fileID = session.MediaFileID
		deliveredPath = strings.TrimSpace(session.VirtualSourceURI)
	}
	if card != nil {
		if fileID <= 0 {
			fileID = card.MediaFileID
		}
		if deliveredPath == "" {
			deliveredPath = strings.TrimSpace(card.InputPath)
		}
	}
	if fileID <= 0 || !strings.HasPrefix(strings.ToLower(deliveredPath), virtualPlaybackPrefix) {
		return 0, "", false
	}
	return fileID, deliveredPath, true
}

// observeVirtualCandidateFailure reads the delivered candidate's current
// failed_at before a segment is served. The returned stamp is the only health
// state a successful delivery may clear; a failure that lands after this read
// is newer and the fence preserves it. A read error or an absent resolver
// yields nil, which can only match a row that is already unstamped.
func (h *PlaybackHandler) observeVirtualCandidateFailure(ctx context.Context, fileID int) *time.Time {
	if h == nil || h.fileResolver == nil || fileID <= 0 {
		return nil
	}
	current, err := h.fileResolver.GetByID(ctx, fileID)
	if err != nil || current == nil {
		return nil
	}
	return current.FailedAt
}

// clearVirtualCandidateRecovered clears a virtual candidate's known-bad stamp
// after an HLS/transcode delivery actually served a full segment to the client,
// and records the delivery evidence. It is the transcode counterpart of
// StreamHandler.clearVirtualCandidateRecovered and shares its fence: only the
// delivered candidate identity and the failure state observed before the
// delivering request may be cleared. It reports whether the write ran so the
// caller can record the session as recovered and stop re-reading per segment.
// Best-effort: a persistence failure must not fail a delivering stream.
func (h *PlaybackHandler) clearVirtualCandidateRecovered(ctx context.Context, fileID int, deliveredFilePath string, observedFailedAt *time.Time) bool {
	if h == nil || fileID <= 0 || strings.TrimSpace(deliveredFilePath) == "" || h.VirtualCandidateRecoveredMarker == nil {
		return false
	}
	clearCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := h.VirtualCandidateRecoveredMarker(clearCtx, fileID, deliveredFilePath, observedFailedAt); err != nil {
		slog.WarnContext(ctx, "clear virtual candidate recovered from transcode delivery",
			"component", "api", "file_id", fileID, "delivered", deliveredFilePath, "error", err)
		return false
	}
	return true
}

// buildProxyManifestURL signs a stream token carrying the session's full
// reconstruction recipe and builds the manifest URL. proxyNode is the planner's
// pick; when nil the URL falls back to the API-local path, where the token rides
// the ?st= query parameter so the integrated server can reconstruct from it.
//
// requireMediaAuth is the attempt's negotiated media-auth mode: such a session
// never receives a token, and therefore never a proxy origin either, since a
// proxy authenticates from the token in the URL path alone. It gets the
// API-local manifest path, which the client fetches with its own credential.
//
// path is the client's access path. A proxy with no origin on it is the same
// as no proxy: the manifest stays API-local and this server relays the node.
func (h *PlaybackHandler) buildProxyManifestURL(card playback.RecipeCard, proxyNode *nodepool.Node, requireMediaAuth bool, path netaccess.Path) string {
	localURL := fmt.Sprintf("/playback/transcode/%s/master.m3u8", card.SessionID)
	base := proxyNode.ClientURLFor(path)
	if base == "" {
		return appendStreamToken(localURL, h.signSessionToken(card, requireMediaAuth))
	}
	card.RoutingEgressNodeID = proxyNode.ID
	token := h.signSessionToken(card, requireMediaAuth)
	if token == "" {
		return localURL
	}
	return nodepool.NodeEndpoint(base, "/stream/transcode/"+token+"/master.m3u8")
}

// proxyToTranscodeNode forwards a request to the remote transcode node and
// reports whether a complete media segment reached the downstream client. A
// manifest rewrite or a failed/proxied-away response returns false; the report
// is the remote-executor counterpart of the local segment's
// CompletedFullResponse evidence used for virtual-candidate recovery.
func (h *PlaybackHandler) proxyToTranscodeNode(w http.ResponseWriter, r *http.Request, transcodeNodeURL, path string) bool {
	sessionID := chi.URLParam(r, "session_id")
	targetURL := transcodeNodeURL + path
	isSegmentRoute := strings.Contains(path, "/segment/")
	_, segmentParseErr := playback.ParseSegmentNumber(filepath.Base(path))
	isMediaSegment := segmentParseErr == nil
	// Capture the signed stream token ("st") before stripping it from the URL.
	// We forward it out-of-band as a header so the node can reconstruct after a
	// self-restart, while keeping it out of the forwarded/logged URL.
	stToken := r.URL.Query().Get("st")
	// Strip the signed stream token ("st") before forwarding/logging: it is a
	// 24h bearer reconstruction descriptor exposing media path + recipe claims.
	// Other query params are preserved.
	query := r.URL.Query()
	query.Del("st")
	if encoded := query.Encode(); encoded != "" {
		targetURL += "?" + encoded
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, targetURL, nil)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	req.Header.Set("Authorization", "Bearer "+h.JWTSecret)
	if isSegmentRoute {
		// The node's immediate transport peer is this API process, so receiving a
		// complete response there does not prove that the browser received it.
		// Suppress node-local accounting and acknowledge only after the downstream
		// writer completes. Forward range validators so both hops serve the same
		// representation.
		transcodeproxy.PrepareRequest(req, r)
	}
	// Best-effort forward of the stream token as a header so the node's
	// reconstruct path (X-Vio-Stream-Token) can rebuild after a self-restart.
	// Verify at the API boundary and confirm it belongs to this session; an
	// invalid or missing token never blocks the live proxy. validToken is kept so
	// the same verified token can be re-injected into the node's manifest segment
	// URIs below.
	var validToken string
	if stToken != "" && h.JWTSecret != "" {
		claims, verifyErr := streamtoken.Verify(stToken, h.JWTSecret)
		if verifyErr == nil && claims.SessionID == sessionID {
			req.Header.Set("X-Vio-Stream-Token", stToken)
			validToken = stToken
		} else if verifyErr != nil {
			slog.WarnContext(r.Context(), "stream token not forwarded to transcode node", "component", "api", "error", verifyErr, "playback_session_id", sessionID)
		}
	}

	resp, err := telemetry.DoTrustedNode(transcodeproxy.NodeClient(), req, "stream")
	if err != nil {
		slog.ErrorContext(r.Context(), "proxy to transcode node", "component", "api", "error", err, "url", targetURL, "playback_session_id", sessionID)
		http.Error(w, "transcode node unavailable", http.StatusBadGateway)
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	// The node strips "st" from the request query (kept out of node URLs/logs),
	// so the segment/init URIs in the manifest it builds carry no token. Without
	// it, a segment fetched after a node or API restart cannot reconstruct the
	// session and 404s. Re-inject the client-facing token into every URI at this
	// boundary so the client's later segment requests carry "st" again. Only the
	// manifest body is rewritten; segments stream through untouched.
	if validToken != "" && resp.StatusCode == http.StatusOK && strings.HasSuffix(path, ".m3u8") {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			slog.ErrorContext(r.Context(), "read transcode node manifest", "component", "api", "error", readErr, "url", targetURL, "playback_session_id", sessionID)
			http.Error(w, "transcode node unavailable", http.StatusBadGateway)
			return false
		}
		rewritten := playback.AppendManifestQueryParam(body, streamTokenParam, validToken)
		for k, vv := range resp.Header {
			if http.CanonicalHeaderKey(k) == "Content-Length" {
				continue
			}
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(rewritten)))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(rewritten)
		return false
	}

	generation := resp.Header.Get(transcodeproxy.GenerationHeader)
	transcodeproxy.CopyResponseHeaders(w.Header(), resp.Header)
	// Proxied transcode output can stream past the server's absolute
	// WriteTimeout; roll the write deadline with progress instead.
	sw := httpstream.NewRollingDeadlineWriter(w)
	sw.WriteHeader(resp.StatusCode)
	if _, copyErr := io.Copy(sw, resp.Body); copyErr != nil {
		return false
	}
	fullSize := transcodeproxy.FullRepresentationSize(resp)
	delivered := isMediaSegment && generation != "" && r.Method == http.MethodGet &&
		sw.CompletedFullResponse(fullSize)
	if delivered {
		if ackErr := transcodeproxy.Acknowledge(r.Context(), transcodeproxy.NodeClient(), transcodeNodeURL+path, h.JWTSecret, generation); ackErr != nil {
			slog.WarnContext(r.Context(), "acknowledge transcode segment completion", "component", "api", "error", ackErr, "playback_session_id", sessionID)
		}
	}
	return delivered
}

// maybeStartThrottler reads throttle settings and starts the throttler if enabled.
func (h *PlaybackHandler) maybeStartThrottler(ctx context.Context, session *playback.TranscodeSession) {
	playback.StartConfiguredTranscodeThrottler(ctx, h.SettingsRepo, session)
}

// findAlternateFile finds another file version for the same content. It
// prefers non-4K versions because they can escape a disabled-4K-transcode
// terminal, but when none exist it returns a 4K candidate so the planner can
// still accept one that direct-plays or remuxes without forbidden video
// encoding.
// Within each class it prefers SDR, then resolution, then bitrate.
func (h *PlaybackHandler) findAlternateFile(ctx context.Context, source *models.MediaFile, filter catalog.AccessFilter) (*models.MediaFile, error) {
	candidates, err := h.findAlternateFiles(ctx, source, filter)
	if err != nil || len(candidates) == 0 {
		return nil, err
	}
	return candidates[0], nil
}

// alternateOrdering expresses the client's ceiling for version fallback
// ordering. The zero value is the conservative default: prefer non-4K and SDR
// siblings regardless of what the client can decode.
type alternateOrdering struct {
	Prefer4K  bool
	PreferHDR bool
}

// alternateOrderingForClient derives the version-fallback ordering from the
// client's declared capabilities. Prefer4K requires a normalized max
// resolution of 2160p or higher; PreferHDR requires a declared HDR capability.
// An absent or unknown ceiling yields the zero value, so a device whose limits
// are unknown keeps the conservative non-4K/SDR-first order.
//
// HDR is a secondary key: it orders within the 4K preference tier (after the
// 4K-ness comparison, before resolution/bitrate), not before it.
func alternateOrderingForClient(caps playback.ClientCodecCapabilitiesV3) alternateOrdering {
	normalized, _ := playback.NormalizeQualityV3(caps.MaxResolution)
	return alternateOrdering{
		Prefer4K:  resolutionRank(normalized) >= resolutionRank(transcodeResolution2160p),
		PreferHDR: caps.HDR,
	}
}

// findAlternateFiles returns every compatible edition/version candidate in
// fallback order. Callers that plan candidates must keep trying after a
// terminal: a lower-resolution candidate can still fail while a later 4K
// candidate direct-plays or remuxes without forbidden video encoding.
//
// Only versions the viewer may play are returned: the per-file checks
// loadAuthorizedFile applies to a requested file (library access and the
// maximum playback quality), and present on disk. The sibling query itself is
// unfiltered, and the requested file's authorization says nothing about which
// library a sibling version lives in or how high its resolution is.
func (h *PlaybackHandler) findAlternateFiles(ctx context.Context, source *models.MediaFile, filter catalog.AccessFilter) ([]*models.MediaFile, error) {
	return h.findAlternateFilesOrdered(ctx, source, filter, alternateOrdering{})
}

// findAlternateFilesOrdered is findAlternateFiles with an explicit client
// ceiling for version fallback ordering: Prefer4K/PreferHDR reverse the
// 4K/HDR group direction. The zero value keeps the conservative
// non-4K/SDR-first order and matches findAlternateFiles exactly.
func (h *PlaybackHandler) findAlternateFilesOrdered(ctx context.Context, source *models.MediaFile, filter catalog.AccessFilter, order alternateOrdering) ([]*models.MediaFile, error) {
	if h.FileVersionFetcher == nil {
		return nil, fmt.Errorf("file version fetcher not configured")
	}

	var files []*models.MediaFile
	var err error
	if source.EpisodeID != "" {
		files, err = h.FileVersionFetcher.GetByEpisodeID(ctx, source.EpisodeID)
	} else {
		files, err = h.FileVersionFetcher.GetByContentID(ctx, source.ContentID)
	}
	if err != nil {
		return nil, err
	}

	candidates := make([]*models.MediaFile, 0, len(files))
	for _, f := range files {
		if f == nil || f.ID == source.ID {
			continue
		}
		if f.MissingSince != nil || !catalog.FileAllowedByAccess(f, filter) {
			continue
		}
		if source.EditionKey != "" && f.EditionKey != source.EditionKey {
			continue
		}
		if source.EditionKey == "" && f.EditionKey != "" {
			continue
		}
		if source.PresentationGroupKey != "" && f.PresentationGroupKey != "" && f.PresentationGroupKey != source.PresentationGroupKey {
			continue
		}
		if source.PresentationKind != "" && f.PresentationKind != "" && f.PresentationKind != source.PresentationKind {
			continue
		}
		candidates = append(candidates, f)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	// Group by 4K-ness, then by HDR, then resolution, then bitrate. The
	// preference flags only reverse a group's direction: with the zero value
	// this is the historical non-4K/SDR-first order, which keeps a
	// lower-resolution sibling ahead of a 4K one that may hit the
	// disabled-4K-transcode terminal. A 4K-capable client gets 4K first, and a
	// failing 4K start still falls through to every later sibling because the
	// retry loop iterates the whole list and commits the first that starts.
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		a4K := playback.Is4KMediaFileV3(a)
		b4K := playback.Is4KMediaFileV3(b)
		if a4K != b4K {
			if order.Prefer4K {
				return a4K
			}
			return !a4K
		}
		if a.HDR != b.HDR {
			if order.PreferHDR {
				return a.HDR
			}
			return !a.HDR
		}
		aRes := resolutionRank(a.Resolution)
		bRes := resolutionRank(b.Resolution)
		if aRes != bRes {
			return aRes > bRes
		}
		return a.Bitrate > b.Bitrate
	})

	return candidates, nil
}

// virtualTransportAlternatesV3 lists the compatible alternates once for a
// transport-failure fallback and bounds how many a transient provider failure
// may try. The list is fetched once per start; the bound applies only when the
// failure cause is an upstream 5xx, so a provider that is already failing is
// not hammered once per alternate. A non-transient transport failure keeps
// trying every alternate, preserving the existing recovery breadth.
func (h *PlaybackHandler) virtualTransportAlternatesV3(
	ctx context.Context,
	source *models.MediaFile,
	filter catalog.AccessFilter,
	order alternateOrdering,
	transient bool,
) ([]*models.MediaFile, error) {
	alternates, err := h.findAlternateFilesOrdered(ctx, source, filter, order)
	if err != nil || len(alternates) == 0 {
		return nil, err
	}
	if transient && len(alternates) > maxVirtualTransientTransportAlternates {
		return alternates[:maxVirtualTransientTransportAlternates], nil
	}
	return alternates, nil
}

// clampEncodedTargetResolution clamps targetResolution so it never exceeds sourceResolution for encoded video targets.
func clampEncodedTargetResolution(requestedResolution, sourceResolution string) string {
	requestedHeight, requestedKnown := transcodeResolutionHeight(requestedResolution)
	sourceHeight, sourceKnown := transcodeResolutionHeight(sourceResolution)
	if !requestedKnown || !sourceKnown || requestedHeight <= sourceHeight {
		return requestedResolution
	}
	return sourceResolution
}

func clampPlannerTargetResolution(result *playback.PlannerResultV3, source *models.MediaFile) {
	if result == nil || source == nil || result.PlayMethod != playback.PlayTranscode || strings.EqualFold(result.TargetVideoCodec, "copy") {
		return
	}
	result.TargetResolution = clampEncodedTargetResolution(result.TargetResolution, source.Resolution)
}

const (
	transcodeResolution2160p = "2160p"
	transcodeResolution1080p = "1080p"
	transcodeResolution720p  = "720p"
	transcodeResolution480p  = "480p"
	transcodeResolution420p  = "420p"
	transcodeResolution328p  = "328p"
)

// resolutionRank returns a numeric rank for resolution sorting.
func resolutionRank(res string) int {
	height, known := transcodeResolutionHeight(res)
	if !known {
		return 0
	}

	switch {
	case height >= 2160:
		return 4
	case height >= 1080:
		return 3
	case height >= 720:
		return 2
	case height >= 480:
		return 1
	default:
		return 0
	}
}

func transcodeResolutionHeight(resolution string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(resolution)) {
	case transcodeResolution2160p, "4k", "uhd":
		return 2160, true
	case "4320p", "8k":
		return 4320, true
	case transcodeResolution1080p:
		return 1080, true
	case transcodeResolution720p:
		return 720, true
	case transcodeResolution480p:
		return 480, true
	case transcodeResolution420p:
		return 420, true
	case transcodeResolution328p:
		return 328, true
	default:
		if left, right, ok := strings.Cut(strings.TrimSpace(resolution), "x"); ok {
			if w, err := strconv.Atoi(strings.TrimSpace(left)); err == nil && w > 0 && w <= 32768 {
				if h, err := strconv.Atoi(strings.TrimSpace(right)); err == nil && h > 0 && h <= 32768 {
					return h, true
				}
			}
		}
		return 0, false
	}
}

// manifestBuildFailureIsClientStop reports whether a transcode manifest build
// failure is explained by concurrent session teardown: a client stop (DELETE)
// removes the session and cancels the encoder, so a manifest request racing
// that teardown sees a killed process. Expected behavior, not a fault.
func manifestBuildFailureIsClientStop(h *PlaybackHandler, sessionID string) bool {
	if h == nil || h.sessionMgr == nil {
		return false
	}
	_, err := h.sessionMgr.GetSession(sessionID)
	return err != nil
}
