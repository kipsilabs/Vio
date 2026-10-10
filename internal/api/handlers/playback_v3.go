package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/Silo-Server/silo-server/internal/access"
	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/clientip"
	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/logredact"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/netaccess"
	"github.com/Silo-Server/silo-server/internal/nodepool"
	"github.com/Silo-Server/silo-server/internal/noderouting"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/settingsresolve"
	"github.com/Silo-Server/silo-server/internal/streamlocation"
	"github.com/Silo-Server/silo-server/internal/streamtoken"
	"github.com/Silo-Server/silo-server/internal/subtitles"
	"github.com/Silo-Server/silo-server/internal/tonemap"
	"github.com/Silo-Server/silo-server/internal/transcodenode"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

const (
	maxPlaybackV3BodyBytes        = 256 << 10
	maxPlaybackV3EventBodyBytes   = 32 << 10
	replanLeaseDurationV3         = 15 * time.Second
	replanReleaseTimeoutV3        = 3 * time.Second
	v3NodeCapabilityTTL           = time.Minute
	playbackNodeIntegratedV3      = "integrated"
	subtitleFormatVTTV3           = "vtt"
	subtitleCodecPGSFFmpegV3      = "hdmv_pgs_subtitle"
	subtitleMIMEVTTV3             = "text/vtt"
	subtitleUnavailableReasonV3   = "subtitle_artifact_unavailable"
	subtitleTrackKindV3           = "subtitle"
	transcodeStartFailedReasonV3  = "transcode_start_failed"
	capabilityUnavailableReasonV3 = "transcode_node_capability_unavailable"
	// copySeekProbeRetryMargin is the slack a replan keeps beyond a second full
	// copy-video seek-anchor probe before attempting it. Process startup, the
	// probe-slot wait and result handling all sit outside the probe timeout, so
	// a retry without this margin would spend the caller's last budget and fail
	// with the caller deadline instead of the probe error.
	copySeekProbeRetryMargin = 5 * time.Second
	// copySeekAnchorTransientBackoff is the bounded pause before re-probing the
	// same candidate after an upstream 5xx. The provider is already failing, so
	// an immediate second probe hammers it; the pause is short enough to still
	// fit a resumed session's seek budget.
	copySeekAnchorTransientBackoff = 750 * time.Millisecond
	// maxVirtualTransientTransportAlternates bounds how many compatible
	// alternates one start may try after a transport failure whose cause is a
	// transient provider error (upstream 5xx). A failing upstream should not be
	// hammered once per alternate; one alternate keeps a chance to recover on a
	// different release without churning the whole list.
	maxVirtualTransientTransportAlternates = 1
	// trackUnavailableReasonV3 is the transport reason for an audio remap miss:
	// the candidate's audio inventory did not bind, not its video stream, so the
	// client may pick another track without swapping the release.
	trackUnavailableReasonV3 = "track_unavailable"
	// subtitleDroppedUnavailableCodeV3 is the degradation-warning code for a
	// successful plan whose selected subtitle was dropped; playback continues on
	// the same release without it.
	subtitleDroppedUnavailableCodeV3 = "subtitle_dropped_unavailable"
	// audioAdaptationFailedReasonV3 is the transport reason for a remote node
	// that could not confirm the audio adaptation recipe for a release. It is
	// deliberately distinct from transcodeStartFailedReasonV3: an audio problem
	// must be resolved on the release already mounted (another audio track, or
	// the planner's own audio transcode/downmix), so the start/replan
	// alternate-version hunts must not treat it as a video/policy trigger.
	audioAdaptationFailedReasonV3 = "audio_adaptation_failed"
	seekRestorationPlayerV3       = "player_position"
	// candidateSourceDecodeRejectedReasonV3 is the internal transport reason for
	// a local generation whose decoder already rejected the source. It is never
	// a client terminal: the start/replan rotation loop consumes it to substitute
	// another provider candidate, and exhaustion persists sourceDecodeFailedReasonV3.
	candidateSourceDecodeRejectedReasonV3 = "candidate_source_decode_rejected"
	// sourceDecodeFailedReasonV3 is the durable terminal reason for a decode
	// rejection that could not be recovered, matching the media-route
	// X-Vio-Decode-Error code clients already classify on.
	sourceDecodeFailedReasonV3 = playback.DecodeErrorSourceRejectedCode
	// sourceDecodeFailedMessageV3 is the user-facing text for
	// sourceDecodeFailedReasonV3, shared by the start terminal and the replan
	// paths so they cannot drift.
	sourceDecodeFailedMessageV3 = "The selected media source could not be decoded."
	// transportStartupReadyV3 is the "outcome" of a transport startup whose
	// first manifest became ready.
	transportStartupReadyV3 = "ready"
	// Substitution reasons published on a plan when the effective release
	// differs from the requested one. They are additive wire values; a client
	// that does not recognize one renders the generic substitution notice.
	substitutionReasonDeadReleaseV3     = "dead_release"
	substitutionReasonListingFailedV3   = "listing_failed"
	substitutionReasonTransportFailedV3 = "transport_failed"
	substitutionReasonDecodeRejectedV3  = "decode_rejected"
	substitutionReasonUnknownV3         = "unknown"
	// Failed capability fetches are memoized briefly so an unreachable node
	// costs one timeout per window instead of one per planning request.
	v3NodeCapabilityErrorTTL = 15 * time.Second
	// Planning degrades around slow pooled nodes. Full cold-probe budgets are
	// reserved for transport-time validation of the executor already selected.
	v3NodeCapabilityPlanTimeout = 5 * time.Second
	// Older or not-yet-contacted nodes cannot advertise their effective probe
	// matrix. This conservative cold-fetch bound avoids borrowing the central
	// server's unrelated hardware configuration.
	remoteNodeProbeFallbackTimeout = 2 * time.Minute
	// The node-recipe handoff is restart insurance written while the client is
	// blocked on the start response, so a stalled store must lose the insurance
	// rather than the start. Matches the jellycompat handoff's budget.
	nodeRecipeWriteTimeoutV3            = 2 * time.Second
	playbackStartSideEffectsTimeoutV3   = 15 * time.Second
	playbackStartSideEffectsWorkersV3   = 4
	playbackStartSideEffectsQueueSizeV3 = 256
	playbackCapabilityWarmupWorkersV3   = 4
	requestIDLogKeyV3                   = "request_id"
	bandwidthEstimateLogKeyV3           = "bandwidth_estimate_kbps"
	linkDownstreamLogKeyV3              = "link_downstream_kbps"
	playbackLogValueV3                  = "playback"
	playbackRemoteOutcomeFailedV3       = "failed"
	routeCapabilityUnavailableReasonV3  = "route_capability_unavailable"
)

// v3NodeCapabilityRefreshInterval is both how often the background refresher
// revisits pooled nodes and the freshness margin at which it re-probes one: a
// quarter of the TTL keeps a healthy node warmed ahead of expiry, so a plan-time
// read is a cache hit, without re-probing a node that just answered. It
// collapses with a planning-triggered lazy refresh through the same per-node
// singleflight slot (claimCapabilityRefreshLockedV3).
const v3NodeCapabilityRefreshInterval = v3NodeCapabilityTTL / 4

type serverBitrateCapContextKeyV3 struct{}

func withServerBitrateCapV3(ctx context.Context, capKbps int) context.Context {
	return context.WithValue(ctx, serverBitrateCapContextKeyV3{}, capKbps)
}

func serverBitrateCapV3(ctx context.Context) int {
	cap, _ := ctx.Value(serverBitrateCapContextKeyV3{}).(int)
	return cap
}

var errSubtitleStoreUnavailableV3 = errors.New("subtitle store unavailable")

type v3NodeCapabilityCache struct {
	transformations     []playback.TransformationV3
	transportFeatures   []string
	toneMapCapabilities tonemap.Capabilities
	err                 error
	expiresAt           time.Time
}

type preparedTransportV3 struct {
	url                string
	nodeURL            string
	transportID        string
	hwAccel            string
	toneMapMode        tonemap.Mode
	routingWorkload    noderouting.Workload
	routingExecution   noderouting.Execution
	routingExecutorID  int
	routingExecutorURL string
	routingEgress      noderouting.Egress
	routingEgressID    int
	routingEgressURL   string
	commit             func() *transportErrorV3
	rollback           func()
	rollbackRequired   func() error
	applySession       func() (func() error, error)
	afterDurableCommit func()
}

type preparedTimelineV3 struct {
	seekSeconds            float64
	streamOriginSeconds    float64
	startSegmentNumber     int
	copySeekAnchorResolved bool
}

type playbackStartTimingsV3 struct {
	started time.Time
	last    time.Time
	attrs   []any
}

func newPlaybackStartTimingsV3() *playbackStartTimingsV3 {
	now := time.Now()
	return &playbackStartTimingsV3{started: now, last: now}
}

func (t *playbackStartTimingsV3) mark(stage string) {
	now := time.Now()
	t.attrs = append(t.attrs, stage+"_ms", now.Sub(t.last).Milliseconds())
	t.last = now
}

func (t *playbackStartTimingsV3) log(ctx context.Context, attemptID string) {
	attrs := []any{
		logComponentKey, playbackLogValueV3,
		requestIDLogKeyV3, chimw.GetReqID(ctx),
		"playback_attempt_id", attemptID,
		"total_ms", time.Since(t.started).Milliseconds(),
	}
	attrs = append(attrs, t.attrs...)
	slog.InfoContext(ctx, "protocol v3 start timing", attrs...)
}

type playbackStartSideEffectsV3 struct {
	session         playback.Session
	file            models.MediaFile
	userID          int
	profileID       string
	audioTrackIndex int
	state           *playbackStartSideEffectsStateV3
}

type playbackStartSideEffectsStateV3 struct {
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	started       bool
	stopRequested bool
}

// mediaAuthModeV3 is the attempt's negotiated media transport mode: how a
// client-visible media URL authenticates, and therefore which origins may serve
// it. Both bits are resolved once from the attempt's (pinned) feature list and
// threaded down every branch rather than re-derived per URL builder.
type mediaAuthModeV3 struct {
	// headerAuth is header_authenticated_media_v1: no client-visible URL
	// carries a signed playback credential, and the client authenticates every
	// media request with its own access token instead.
	headerAuth bool
	// proxyEgress is authorized_media_origins_v1 negotiated on top of
	// headerAuth: the client also honors credential-free absolute URLs on
	// server-designated proxy origins, so media bytes need not all egress from
	// the API server. Never true without headerAuth — on a legacy attempt the
	// signed proxy URL already carries its own authority.
	proxyEgress bool
}

// headerAuthenticatedMediaV3 resolves the negotiated media transport mode from
// a client's advertised feature set.
//
// The mode is a bounded value threaded from the v3 request decoder down through
// transport preparation and into the session's stream state, the same way local
// egress is. It deliberately carries no credential, and the durable normalized
// request stays the source of truth for the attempt.
func headerAuthenticatedMediaV3(clientFeatures []string) mediaAuthModeV3 {
	headerAuth := playback.HasFeatureV3(clientFeatures, playback.FeatureHeaderAuthenticatedMediaV3)
	return mediaAuthModeV3{
		headerAuth:  headerAuth,
		proxyEgress: headerAuth && playback.HasFeatureV3(clientFeatures, playback.FeatureAuthorizedMediaOriginsV3),
	}
}

// recipeCardStoreV3 is the shared control-plane store central hands a recipe
// card to another Silo process through (*noderecipe.Store). One instance owns
// one key space; the handler holds two of them, and what a stored card
// authorizes or rebuilds is the field's business, not the interface's — see
// PlaybackHandler.ProxyGrantStore and PlaybackHandler.NodeRecipeStore.
type recipeCardStoreV3 interface {
	// Enabled reports whether the store can actually carry a card. A disabled
	// store accepts Put silently, so a URL that only a stored card can serve
	// must not be published without checking it.
	Enabled() bool
	// Get reads the card currently stored under key. A replan uses it to
	// remember the card it is about to overwrite, so a failed replacement can
	// hand the restored plan its authority back.
	Get(ctx context.Context, key string) (*playback.RecipeCard, bool)
	Put(ctx context.Context, key string, card playback.RecipeCard) error
	Delete(ctx context.Context, key string) error
}

type transportErrorV3 struct {
	reason    string
	message   string
	retryable bool
	cause     error
}

func subtitleArtifactErrorV3(message string, cause error) *transportErrorV3 {
	return &transportErrorV3{
		reason:    subtitleUnavailableReasonV3,
		message:   message,
		retryable: errors.Is(cause, errSubtitleStoreUnavailableV3),
		cause:     cause,
	}
}

func wrapSubtitleStoreErrorV3(err error) error {
	return fmt.Errorf("%w: %w", errSubtitleStoreUnavailableV3, err)
}

type v3ReplanLock struct {
	mu   sync.Mutex
	refs int
}

type v3EventRate struct {
	windowStart time.Time
	count       int
}

type replacementAdmissionCheckerV3 interface {
	CheckReplacementAllowed(context.Context, string, playback.PlayMethod, bool) error
}

type replacementReservationCancellerV3 interface {
	CancelReplacementReservation(string)
}

type replacementStateManagerV3 interface {
	ApplyReplacement(string, playback.SessionReplacement) (playback.SessionReplacementRollback, error)
	RollbackReplacement(string, playback.SessionReplacementRollback) error
}

type sessionReservationReleaserV3 interface {
	ReleaseSession(string)
}

func (e *transportErrorV3) Error() string {
	if e.cause != nil {
		return e.reason + ": " + e.cause.Error()
	}
	return e.reason
}

func (h *PlaybackHandler) transformationRegistryV3(ctx context.Context) *playback.TransformationRegistryV3 {
	h.v3RegistryMu.Lock()
	defer h.v3RegistryMu.Unlock()
	if !h.v3Registry.NeedsRefresh(time.Now()) {
		return h.v3Registry
	}
	probe := playback.ProbeTransformationRegistryWithToneMapV3Result
	if h.v3RegistryProbe != nil {
		probe = h.v3RegistryProbe
	}
	registry, err := probe(context.WithoutCancel(ctx), h.playbackConfig().FFmpegPath, nil)
	if err == nil {
		h.v3Registry = registry
	}
	return registry
}

// v3LocalToneMapNegativeTTL bounds how long an incomplete-but-error-free local
// probe result is reused before the probe is retried. It mirrors the tonemap
// package's own probeNegativeTTL so a missing executor that hardware contention
// hid from one probe is retried on the same cadence there and here.
const v3LocalToneMapNegativeTTL = 15 * time.Second

// localToneMapCapabilitiesV3 returns a defensive copy of the capabilities
// validated for the current local FFmpeg, backend, and device configuration.
func (h *PlaybackHandler) localToneMapCapabilitiesV3(ctx context.Context) (tonemap.Capabilities, error) {
	capabilities, _, err := h.localToneMapCapabilitiesWithBackendV3(ctx)
	return capabilities, err
}

// localToneMapCapabilitiesWithBackendV3 is localToneMapCapabilitiesV3 plus the
// resolved hardware backend the probe ran against, which the caller needs to
// judge whether the inventory is complete.
func (h *PlaybackHandler) localToneMapCapabilitiesWithBackendV3(ctx context.Context) (tonemap.Capabilities, string, error) {
	cfg := h.playbackConfig()
	ffmpegPath := playback.ResolveFFmpegPath(cfg.FFmpegPath)
	hwDevice := strings.TrimSpace(cfg.HWDevice)
	resolved := playback.ResolveHWAccelWithFFmpegContext(ctx, cfg.HWAccel, cfg.FFmpegPath, hwDevice)
	if err := ctx.Err(); err != nil {
		return nil, resolved, err
	}
	probe := tonemap.Probe
	if h.v3ToneMapProbe != nil {
		probe = h.v3ToneMapProbe
	}
	capabilities, err := probe(ctx, ffmpegPath, resolved, hwDevice)
	return append(tonemap.Capabilities(nil), capabilities...), resolved, err
}

// localToneMapCapabilitiesCachedV3 returns the local tone-map inventory,
// probing at most once per process. It mirrors transformationRegistryV3: the
// local FFmpeg binary and hardware configuration are fixed for the process
// lifetime, so a complete, successful probe is reused for every later playback
// start.
//
// A failed probe is deliberately not cached, so a transient failure (a busy
// encoder, a timeout) is retried by the next caller instead of being frozen for
// the life of the process. An error-free but incomplete inventory is treated
// the same way for a shorter interval: the probe can report a nil error while a
// configured hardware executor is missing (temporary device contention, a
// driver that has not finished coming up), and a nil error alone must not freeze
// that verdict for the process lifetime. Incomplete results are reused only for
// v3LocalToneMapNegativeTTL so a recovery is picked up within one negative
// window without every caller re-running the smoke matrix in the meantime.
//
// The first caller still pays for the probe on its own context. The startup
// warmup is expected to have paid it already; when it has not, falling back to
// a live probe preserves the planning outcome a live probe would produce.
func (h *PlaybackHandler) localToneMapCapabilitiesCachedV3(ctx context.Context) (tonemap.Capabilities, error) {
	h.v3LocalToneMapMu.Lock()
	if h.v3LocalToneMapCached {
		capabilities := append(tonemap.Capabilities(nil), h.v3LocalToneMapCaps...)
		h.v3LocalToneMapMu.Unlock()
		return capabilities, nil
	}
	if now := time.Now(); !h.v3LocalToneMapNegativeUntil.IsZero() && now.Before(h.v3LocalToneMapNegativeUntil) {
		// An incomplete result is still being negative-cached: serve it without
		// re-running the probe so a burst of starts cannot hammer the encoder.
		capabilities := append(tonemap.Capabilities(nil), h.v3LocalToneMapCaps...)
		h.v3LocalToneMapMu.Unlock()
		return capabilities, nil
	}
	h.v3LocalToneMapMu.Unlock()

	capabilities, resolvedBackend, err := h.localToneMapCapabilitiesWithBackendV3(ctx)
	if err != nil {
		return capabilities, err
	}
	complete := tonemap.CapabilitiesComplete(capabilities, resolvedBackend)
	h.v3LocalToneMapMu.Lock()
	if h.v3LocalToneMapCached {
		// A concurrent complete probe already won the lifetime cache; keep its
		// inventory rather than overwriting it with this (possibly older) result.
		cached := append(tonemap.Capabilities(nil), h.v3LocalToneMapCaps...)
		h.v3LocalToneMapMu.Unlock()
		return cached, nil
	}
	h.v3LocalToneMapCaps = append(tonemap.Capabilities(nil), capabilities...)
	if complete {
		h.v3LocalToneMapCached = true
		h.v3LocalToneMapNegativeUntil = time.Time{}
	} else {
		h.v3LocalToneMapNegativeUntil = time.Now().Add(v3LocalToneMapNegativeTTL)
	}
	h.v3LocalToneMapMu.Unlock()
	return capabilities, nil
}

func (h *PlaybackHandler) localToneMapCapabilitiesForTransportV3(ctx context.Context) (tonemap.Capabilities, error) {
	probeCtx, cancel := context.WithTimeout(ctx, h.localToneMapProbeTimeoutV3())
	defer cancel()
	return h.localToneMapCapabilitiesV3(probeCtx)
}

func (h *PlaybackHandler) localToneMapProbeTimeoutV3() time.Duration {
	cfg := h.playbackConfig()
	// The whole read, not its tone-map half: localToneMapCapabilitiesV3 resolves
	// the backend first, and on Linux that is a full hardware walk whose cost
	// scales with the configured device set.
	return playback.CapabilityEndpointTimeout(cfg.HWAccel, cfg.HWDevice)
}

// remoteToneMapProbeTimeoutV3 returns how long to allow one capability read of
// a node, from the budget that node last advertised.
//
// The budget is kept apart from the inventory because it describes the node
// rather than its hardware, and the two are invalidated for different reasons.
// An acceleration change makes the inventory wrong and the matrix cold — which
// is exactly when the next read is slowest — while how long that node takes to
// answer has not changed. Storing them together meant an invalidation dropped
// the budget with the inventory and the refresh it triggered fell back to two
// minutes, short of the ~136 seconds a two-device node legitimately asks for,
// so protocol-v3 planning lost its inventory precisely after an invalidation.
func (h *PlaybackHandler) remoteToneMapProbeTimeoutV3(nodeURL string) time.Duration {
	nodeURL = nodepool.NormalizeNodeURL(nodeURL)
	h.v3NodeCapabilitiesMu.Lock()
	budget := h.v3NodeProbeBudgets[nodeURL]
	h.v3NodeCapabilitiesMu.Unlock()
	// Never less than what this node currently describes, whatever was learned
	// from it before.
	//
	// A learned budget is kept across invalidations on purpose — an invalidation
	// is the moment the next read is coldest and slowest, so dropping the budget
	// with the inventory would fall back to a figure short of what the node
	// needs. But a per-node policy edit invalidates through the same path, and
	// widening hw_device_override is precisely a change that makes the learned
	// number too small: the node reloads, walks four devices instead of one, and
	// gets canceled at the one-device deadline. Nothing recovers from that on its
	// own, because a budget is only ever learned from a read that completes.
	if cold := h.coldNodeProbeTimeoutV3(nodeURL); cold > budget {
		return cold
	}
	return budget
}

// coldNodeProbeTimeoutV3 prices one capability read of a node this process has
// not read successfully yet, from that node rather than from a cluster-wide
// guess. Without a pooled record — a planner that cannot look nodes up, or a
// URL that is no longer in the pool — the cluster's own policy is the closest
// description available.
func (h *PlaybackHandler) coldNodeProbeTimeoutV3(nodeURL string) time.Duration {
	cfg := h.playbackConfig()
	var node *nodepool.Node
	if lookup, ok := h.NodePlanner.(transcodeNodeLookupV3); ok {
		if found, ok := lookup.TranscodeNodeByURL(nodeURL); ok {
			node = found
		}
	}
	if node == nil {
		if lookup, ok := h.NodePlanner.(proxyNodeLookupV3); ok {
			if found, ok := lookup.ProxyNodeByURL(nodeURL); ok {
				node = found
			}
		}
	}
	return playback.ColdCapabilityRequestTimeout(
		node.StoredCapabilities(),
		node.EffectiveHWAccel(cfg.HWAccel),
		node.EffectiveHWDevice(cfg.HWDevice),
		remoteNodeProbeFallbackTimeout,
	)
}

// transcodeNodeLookupV3 resolves the pooled record behind a transcode node URL,
// which carries that node's stored capability report and its acceleration
// override. Optional, like the planner itself: without it this path falls back
// to the cluster-wide setting. *nodepool.Planner implements it.
type transcodeNodeLookupV3 interface {
	TranscodeNodeByURL(nodeURL string) (*nodepool.Node, bool)
}

type proxyNodeLookupV3 interface {
	ProxyNodeByURL(nodeURL string) (*nodepool.Node, bool)
}

// rememberNodeProbeBudgetV3 records what a node says its capability read costs.
// Callers must hold v3NodeCapabilitiesMu.
func (h *PlaybackHandler) rememberNodeProbeBudgetLockedV3(nodeURL string, budget time.Duration) {
	if budget <= 0 {
		return
	}
	if h.v3NodeProbeBudgets == nil {
		h.v3NodeProbeBudgets = make(map[string]time.Duration)
	}
	h.v3NodeProbeBudgets[nodeURL] = budget
}

func (h *PlaybackHandler) toneMapPlanningTimeoutV3(localFallbackAllowed bool) time.Duration {
	if localFallbackAllowed {
		return v3NodeCapabilityPlanTimeout
	}
	return remoteNodeProbeFallbackTimeout
}

// remoteTransformationsV3 is the transport-time capability lookup for a
// selected node. It never trusts memoized failures: those may be planning
// deadlines far shorter than this path's fetch budget, and rejecting the
// already-selected node on a stale planning timeout would fail a start the
// fetch could still validate.
func (h *PlaybackHandler) remoteTransformationsV3(ctx context.Context, nodeURL string) ([]playback.TransformationV3, error) {
	return h.lookupRemoteTransformationsV3(ctx, nodeURL, false)
}

// remoteTransformationsPlanningV3 is the planning-time variant: it honors
// negatively-cached fetch failures so an unreachable node costs one timeout
// per error-TTL window instead of one per playback start.
func (h *PlaybackHandler) remoteTransformationsPlanningV3(ctx context.Context, nodeURL string) ([]playback.TransformationV3, error) {
	return h.lookupRemoteTransformationsV3(ctx, nodeURL, true)
}

// lookupRemoteTransformationsV3 returns a node's cached or freshly fetched transformation inventory.
func (h *PlaybackHandler) lookupRemoteTransformationsV3(ctx context.Context, nodeURL string, honorCachedFailure bool) ([]playback.TransformationV3, error) {
	entry, err := h.lookupRemoteCapabilitiesV3(ctx, nodeURL, honorCachedFailure)
	if err != nil {
		return nil, err
	}
	return append([]playback.TransformationV3(nil), entry.transformations...), nil
}

// lookupRemoteCapabilitiesV3 fetches and jointly caches a node's transformation
// and tone-map inventory, optionally reusing short-lived fetch failures.
func (h *PlaybackHandler) lookupRemoteCapabilitiesV3(ctx context.Context, nodeURL string, honorCachedFailure bool) (v3NodeCapabilityCache, error) {
	// Canonical here and in RefreshNodeCapabilitiesV3, which are the two ways
	// into these maps; everything below is reached from one of them and so is
	// already keyed the same way.
	nodeURL = nodepool.NormalizeNodeURL(nodeURL)
	now := time.Now()
	h.v3NodeCapabilitiesMu.Lock()
	entry, ok := h.v3NodeCapabilities[nodeURL]
	h.v3NodeCapabilitiesMu.Unlock()
	if ok && now.Before(entry.expiresAt) {
		if entry.err == nil {
			return entry, nil
		}
		if honorCachedFailure {
			return v3NodeCapabilityCache{}, entry.err
		}
	}
	if ok && entry.err == nil && honorCachedFailure {
		// A successful inventory is safe for planning after its freshness window:
		// transport-time validation still refreshes before relying on it, while
		// planning can use the stale snapshot and refresh behind the request. This
		// keeps sparse traffic from paying a multi-second node probe every minute.
		h.refreshRemoteCapabilitiesV3(nodeURL)
		return entry, nil
	}

	// An invalidation landing mid-fetch means the report this probe is about to
	// return describes the node as it was, not as it is — and this caller is
	// about to select transformations and a tone-map executor from it. Re-probe
	// rather than plan on it.
	//
	// Bounded at two attempts, and the second result is used even if it is
	// overtaken too. Failing the request instead would be worse than a slightly
	// stale inventory: most hash changes are not "the hardware went away" — a
	// driver update, a new identity field, a raised probe budget all move it —
	// so refusing would reject playback that the report in hand describes
	// perfectly well, and on a single-transcode-node deployment there is nothing
	// to fall back to. A node changing faster than two probes can read it is a
	// different problem, and one this path cannot fix by failing.
	var (
		info        playback.HWAccelInfo
		err         error
		completedAt time.Time
		overtaken   bool
	)
	for attempt := range v3CapabilityFetchAttempts {
		// Snapshot the invalidation count before probing: anything this fetch
		// learns describes the node as it was when the request left.
		invalidations := h.nodeCapabilityInvalidationsV3(nodeURL)
		requestCtx, cancel := context.WithTimeout(ctx, h.remoteToneMapProbeTimeoutV3(nodeURL))
		info, err = fetchRemoteTranscodeCapabilities(requestCtx, nodeURL, h.JWTSecret)
		cancel()
		completedAt = time.Now()
		overtaken = h.nodeCapabilityInvalidationsV3(nodeURL) != invalidations
		if !overtaken || attempt+1 == v3CapabilityFetchAttempts {
			break
		}
	}

	if err != nil {
		h.v3NodeCapabilitiesMu.Lock()
		if h.v3NodeCapabilities == nil {
			h.v3NodeCapabilities = make(map[string]v3NodeCapabilityCache)
		}
		if overtaken {
			h.v3NodeCapabilitiesMu.Unlock()
			return v3NodeCapabilityCache{}, err
		}
		if current, currentOK := h.v3NodeCapabilities[nodeURL]; currentOK && current.err == nil && completedAt.Before(current.expiresAt) {
			h.v3NodeCapabilitiesMu.Unlock()
			return current, nil
		}
		h.v3NodeCapabilities[nodeURL] = v3NodeCapabilityCache{err: err, expiresAt: completedAt.Add(v3NodeCapabilityErrorTTL)}
		h.v3NodeCapabilitiesMu.Unlock()
		return v3NodeCapabilityCache{}, err
	}
	entry = v3NodeCapabilityCache{
		transformations:     append([]playback.TransformationV3(nil), info.Transformations...),
		transportFeatures:   append([]string(nil), info.TransportFeatures...),
		toneMapCapabilities: append(tonemap.Capabilities(nil), info.ToneMapCapabilities...),
		expiresAt:           completedAt.Add(v3NodeCapabilityTTL),
	}
	h.v3NodeCapabilitiesMu.Lock()
	// Recorded even when the entry below is discarded as overtaken: what the
	// node says its read costs is true regardless of whether this particular
	// answer is still current.
	h.rememberNodeProbeBudgetLockedV3(nodeURL,
		playback.NormalizeProbeRequestTimeout(info.ProbeRequestTimeoutMillis, remoteNodeProbeFallbackTimeout))
	if overtaken {
		// Still overtaken after the retry. Hand the result to this caller, which
		// has nothing better, but leave the cache empty so the next lookup
		// re-probes instead of serving it for a full TTL to everyone else.
		h.v3NodeCapabilitiesMu.Unlock()
		return entry, nil
	}
	if h.v3NodeCapabilities == nil {
		h.v3NodeCapabilities = make(map[string]v3NodeCapabilityCache)
	}
	h.v3NodeCapabilities[nodeURL] = entry
	h.v3NodeCapabilitiesMu.Unlock()
	return entry, nil
}

// v3CapabilityFetchAttempts is how many times one lookup will re-probe a node
// whose capabilities changed while it was being read.
const v3CapabilityFetchAttempts = 2

func (h *PlaybackHandler) nodeCapabilityInvalidationsV3(nodeURL string) uint64 {
	h.v3NodeCapabilitiesMu.Lock()
	defer h.v3NodeCapabilitiesMu.Unlock()
	return h.v3NodeCapabilityInvalidations[nodeURL]
}

// RefreshNodeCapabilitiesV3 discards one node's cached capability inventory and
// re-probes it in the background. It exists for the node health sweep, which
// learns from a node's advertised capability hash that its hardware changed
// long before this cache's freshness window would expire — and a cache that
// outlives the hardware it describes plans transcodes onto a GPU that is gone.
func (h *PlaybackHandler) RefreshNodeCapabilitiesV3(nodeURL string) {
	if h == nil || nodeURL == "" {
		return
	}
	// Every map here is keyed by the node's canonical address. This entry point
	// is reached from the admin route with the URL exactly as the row stores it,
	// which may carry a trailing slash the pools have already dropped — and then
	// the entry this deletes is not the entry planning reads, so a node keeps
	// serving the backend it was just moved off until the old key expires.
	nodeURL = nodepool.NormalizeNodeURL(nodeURL)
	h.v3NodeCapabilitiesMu.Lock()
	delete(h.v3NodeCapabilities, nodeURL)
	if h.v3NodeCapabilityInvalidations == nil {
		h.v3NodeCapabilityInvalidations = make(map[string]uint64)
	}
	// Bumping the count is what makes the delete stick. A refresh already in
	// flight fetched this node before its hardware changed, so it must neither
	// re-install its answer over this delete nor be mistaken for the re-probe
	// this invalidation is owed.
	h.v3NodeCapabilityInvalidations[nodeURL]++
	claimed := h.claimCapabilityRefreshLockedV3(nodeURL)
	h.v3NodeCapabilitiesMu.Unlock()
	if claimed {
		h.runCapabilityRefreshV3(nodeURL)
	}
}

func (h *PlaybackHandler) refreshRemoteCapabilitiesV3(nodeURL string) {
	if h == nil || nodeURL == "" {
		return
	}
	h.v3NodeCapabilitiesMu.Lock()
	claimed := h.claimCapabilityRefreshLockedV3(nodeURL)
	h.v3NodeCapabilitiesMu.Unlock()
	if claimed {
		h.runCapabilityRefreshV3(nodeURL)
	}
}

// claimCapabilityRefreshLockedV3 takes the node's single background-refresh
// slot. The caller must hold v3NodeCapabilitiesMu: taking the slot under the
// same lock as the invalidation counter is what guarantees that an invalidation
// either starts a refresh or is seen by the one already running.
func (h *PlaybackHandler) claimCapabilityRefreshLockedV3(nodeURL string) bool {
	if _, inFlight := h.v3NodeCapabilityRefresh[nodeURL]; inFlight {
		return false
	}
	if h.v3NodeCapabilityRefresh == nil {
		h.v3NodeCapabilityRefresh = make(map[string]struct{})
	}
	h.v3NodeCapabilityRefresh[nodeURL] = struct{}{}
	return true
}

// runCapabilityRefreshV3 probes one node in the background until its result is
// current: a probe that an invalidation overtook was discarded, so it repeats
// rather than leave the cache empty until the next viewer pays for a probe.
func (h *PlaybackHandler) runCapabilityRefreshV3(nodeURL string) {
	go func() {
		for {
			invalidations := h.nodeCapabilityInvalidationsV3(nodeURL)
			ctx, cancel := context.WithTimeout(context.Background(), h.remoteToneMapProbeTimeoutV3(nodeURL))
			_, err := h.lookupRemoteCapabilitiesV3(ctx, nodeURL, false)
			cancel()
			if err != nil {
				slog.Debug("protocol v3 background node capability refresh failed", "component", "api", "node", logredact.SanitizeURL(nodeURL), "error", err)
			}
			h.v3NodeCapabilitiesMu.Lock()
			current := h.v3NodeCapabilityInvalidations[nodeURL] == invalidations
			if current {
				delete(h.v3NodeCapabilityRefresh, nodeURL)
			}
			h.v3NodeCapabilitiesMu.Unlock()
			if current {
				return
			}
		}
	}()
}

// StartCapabilityWarmupV3 moves local and pooled-node capability discovery off
// the first viewer's start request. It is best effort: failed probes remain
// retryable through the ordinary lookup path and never prevent API startup.
func (h *PlaybackHandler) StartCapabilityWarmupV3(ctx context.Context) {
	if h == nil || ctx == nil {
		return
	}
	go h.warmPlaybackCapabilitiesV3(ctx)
	h.startNodeCapabilityRefresherV3(ctx)
}

// startNodeCapabilityRefresherV3 keeps the pooled-node capability cache warm so
// plan-time reads are cache hits instead of node round-trips. The startup
// warmup populates the cache once; this refreshes each node before its TTL
// lapses, so sparse or bursty traffic never meets a cold entry. It is started
// once and stops with the application context.
func (h *PlaybackHandler) startNodeCapabilityRefresherV3(ctx context.Context) {
	h.v3RefresherOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(v3NodeCapabilityRefreshInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					h.refreshStaleNodeCapabilitiesV3()
				}
			}
		}()
	})
}

// refreshStaleNodeCapabilitiesV3 re-probes every pooled node whose cached
// capabilities are missing, failed, or close enough to expiry that they would
// lapse before the next sweep. Each refresh claims the node's existing
// singleflight slot, so it collapses with a planning-triggered lazy refresh
// rather than running beside it. A node whose entry is still comfortably fresh
// is left alone.
func (h *PlaybackHandler) refreshStaleNodeCapabilitiesV3() {
	now := time.Now()
	for _, nodeURL := range h.nodeCapabilityURLsV3() {
		if h.nodeCapabilityExpiringV3(nodeURL, now) {
			h.refreshRemoteCapabilitiesV3(nodeURL)
		}
	}
}

// nodeCapabilityURLsV3 lists every pooled node whose capabilities planning or
// transport may read, transcode and proxy alike. A planner that enumerates
// neither pool contributes none.
func (h *PlaybackHandler) nodeCapabilityURLsV3() []string {
	var nodeURLs []string
	if enumerator, ok := h.NodePlanner.(transcodeNodeEnumeratorV3); ok {
		nodeURLs = append(nodeURLs, enumerator.TranscodeNodeURLs()...)
	}
	if enumerator, ok := h.NodePlanner.(proxyNodeEnumeratorV3); ok {
		nodeURLs = append(nodeURLs, enumerator.ProxyNodeURLs()...)
	}
	return nodeURLs
}

// nodeCapabilityExpiringV3 reports whether a node's cached inventory should be
// refreshed now. A missing or failed entry is always due; a successful entry is
// due once it is within one refresh interval of expiry, which keeps the 1min
// TTL semantics while moving the probe off the plan-time path.
func (h *PlaybackHandler) nodeCapabilityExpiringV3(nodeURL string, now time.Time) bool {
	nodeURL = nodepool.NormalizeNodeURL(nodeURL)
	h.v3NodeCapabilitiesMu.Lock()
	defer h.v3NodeCapabilitiesMu.Unlock()
	entry, ok := h.v3NodeCapabilities[nodeURL]
	if !ok {
		return true
	}
	if entry.err != nil {
		return true
	}
	return !now.Add(v3NodeCapabilityRefreshInterval).Before(entry.expiresAt)
}

func (h *PlaybackHandler) warmPlaybackCapabilitiesV3(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, remoteNodeProbeFallbackTimeout)
	defer cancel()
	_ = h.transformationRegistryV3(ctx)

	settings := h.plannerSettingsV3(ctx)
	policy := tonemap.NewPolicy(settings.HardwareToneMapEnabled, settings.SoftwareToneMapEnabled)
	if policy != tonemap.PolicyNone {
		if _, err := h.localToneMapCapabilitiesCachedV3(ctx); err != nil {
			slog.DebugContext(ctx, "protocol v3 local capability warmup failed", "component", "api", "error", err)
		}
	}
	// Keep ordinary SDR playback warm independently of the tone-map probe: its
	// smoke graph has different filters and may already be satisfied by cache.
	cfg := h.playbackConfig()
	if err := playback.WarmHardwareEncoder(ctx, cfg.FFmpegPath, cfg.HWAccel, cfg.HWDevice); err != nil {
		slog.DebugContext(ctx, "protocol v3 hardware encoder warmup failed", "component", "api", "error", err)
	}
	enumerator, ok := h.NodePlanner.(transcodeNodeEnumeratorV3)
	if !ok {
		return
	}
	nodeURLs := enumerator.TranscodeNodeURLs()
	workerCount := min(playbackCapabilityWarmupWorkersV3, len(nodeURLs))
	if workerCount == 0 {
		return
	}
	jobs := make(chan string)
	var group sync.WaitGroup
	group.Add(workerCount)
	for range workerCount {
		go func() {
			defer group.Done()
			for nodeURL := range jobs {
				if _, err := h.lookupRemoteCapabilitiesV3(ctx, nodeURL, false); err != nil {
					slog.DebugContext(ctx, "protocol v3 node capability warmup failed", logComponentKey, "api", "node", logredact.SanitizeURL(nodeURL), "error", err)
				}
			}
		}()
	}
	for _, nodeURL := range nodeURLs {
		select {
		case jobs <- nodeURL:
		case <-ctx.Done():
			close(jobs)
			group.Wait()
			return
		}
	}
	close(jobs)
	group.Wait()
}

// remoteToneMapCapabilitiesV3 returns a defensive copy of one node's validated
// tone-map inventory; planning lookups may honor the negative cache.
func (h *PlaybackHandler) remoteToneMapCapabilitiesV3(ctx context.Context, nodeURL string, planning bool) (tonemap.Capabilities, error) {
	entry, err := h.lookupRemoteCapabilitiesV3(ctx, nodeURL, planning)
	if err != nil {
		return nil, err
	}
	return append(tonemap.Capabilities(nil), entry.toneMapCapabilities...), nil
}

// transcodeNodeEnumeratorV3 exposes the pooled transcode nodes whose
// advertised transformations widen HLS planning; *nodepool.Planner implements
// it.
type transcodeNodeEnumeratorV3 interface {
	TranscodeNodeURLs() []string
}

// proxyNodeEnumeratorV3 is the proxy-pool counterpart, letting identity
// planning narrow selection to proxies that can execute the plan's recipe.
type proxyNodeEnumeratorV3 interface {
	ProxyNodeURLs() []string
}

// hlsToneMapCapabilityInventoryV3 separates locally executable tone-map
// capabilities from the union advertised by local and pooled-node executors.
type hlsToneMapCapabilityInventoryV3 struct {
	local                    tonemap.Capabilities
	union                    tonemap.Capabilities
	localTranscodeFallbackOK bool
}

// localHLSExecutionRegistryV3 returns the transformations this API process can
// execute for an HLS delivery. Tone mapping is added only when local execution
// is allowed, the administrator permits a mode, and the configured FFmpeg and
// device have validated an executor in that mode. Planning and transport-time
// validation must share this view or a locally planned recipe can be rejected
// before FFmpeg starts.
func (h *PlaybackHandler) localHLSExecutionRegistryV3(ctx context.Context) (*playback.TransformationRegistryV3, error) {
	settings := h.plannerSettingsV3(ctx)
	policy := tonemap.NewPolicy(settings.HardwareToneMapEnabled, settings.SoftwareToneMapEnabled)
	inventory := hlsToneMapCapabilityInventoryV3{}
	if policy != tonemap.PolicyNone {
		var err error
		inventory.localTranscodeFallbackOK, inventory.local, err = h.localHLSToneMapCapabilitiesForTransportV3(ctx)
		if err != nil {
			return nil, err
		}
	}
	return h.localHLSExecutionRegistryWithInputsV3(ctx, settings, inventory), nil
}

// localHLSExecutionRegistryWithInputsV3 builds the local HLS execution
// registry from one consistent settings and capability snapshot.
func (h *PlaybackHandler) localHLSExecutionRegistryWithInputsV3(
	ctx context.Context,
	settings playback.PlannerSettingsV3,
	inventory hlsToneMapCapabilityInventoryV3,
) *playback.TransformationRegistryV3 {
	local := h.transformationRegistryV3(ctx)
	policy := tonemap.NewPolicy(settings.HardwareToneMapEnabled, settings.SoftwareToneMapEnabled)
	if policy == tonemap.PolicyNone || !inventory.localTranscodeFallbackOK {
		return local
	}
	capabilityAvailable := false
	for _, capability := range inventory.local {
		if policy.Allows(capability.Mode) && len(capability.SourceKinds) > 0 {
			capabilityAvailable = true
			break
		}
	}
	if capabilityAvailable {
		local = local.WithAdvertised([]playback.TransformationV3{{
			Name: playback.TransformationHDRToSDRToneMapV3, Executor: playback.ExecutorServerV3,
			RecipeVersion: playback.TransformationHDRToSDRToneMapRecipeVersionV3,
		}})
	}
	return local
}

// hlsPlanningRegistryV3 returns the video-transcode registry. Playback planning
// uses workload-specific registries so remux and video policy cannot leak
// executor capabilities into one another.
func (h *PlaybackHandler) hlsPlanningRegistryV3(ctx context.Context) *playback.TransformationRegistryV3 {
	settings := h.plannerSettingsV3(ctx)
	inventory := hlsToneMapCapabilityInventoryV3{}
	policy := tonemap.NewPolicy(settings.HardwareToneMapEnabled, settings.SoftwareToneMapEnabled)
	if policy != tonemap.PolicyNone {
		inventory, _ = h.hlsToneMapCapabilityInventoryV3(ctx)
	}
	return h.hlsPlanningRegistryWithInputsV3(ctx, settings, inventory)
}

// hlsPlanningRegistryWithInputsV3 combines locally executable
// transformations with transformations advertised by pooled transcode nodes.
func (h *PlaybackHandler) hlsPlanningRegistryWithInputsV3(
	ctx context.Context,
	settings playback.PlannerSettingsV3,
	inventory hlsToneMapCapabilityInventoryV3,
) *playback.TransformationRegistryV3 {
	return h.hlsPlanningRegistryWithInputsForWorkloadV3(ctx, settings, inventory, noderouting.WorkloadVideoTranscode)
}

// progressiveRemuxPlanningRegistryWithInputsV3 reports only the remux
// transformations executable by policy-eligible progressive routes. The API
// proxy, and transcode pools are admitted only when policy leaves a legal
// progressive route through that executor.
func (h *PlaybackHandler) progressiveRemuxPlanningRegistryWithInputsV3(
	ctx context.Context,
	local *playback.TransformationRegistryV3,
	proxyAllowed bool,
) *playback.TransformationRegistryV3 {
	local = local.OnlyAdvertised(progressiveRemuxTransformationsV3(local.Advertised()))
	localAllowed, proxyExecutionAllowed, transcodeExecutionAllowed := progressiveRemuxExecutorAvailabilityV3(
		h.playbackRoutingPolicyForContextV3(ctx), proxyAllowed,
	)
	var merged []playback.TransformationV3
	if proxyExecutionAllowed {
		if enumerator, ok := h.NodePlanner.(proxyNodeEnumeratorV3); ok {
			for _, transformations := range h.pooledNodeTransformationsV3(ctx, enumerator.ProxyNodeURLs()) {
				merged = append(merged, progressiveRemuxTransformationsV3(transformations)...)
			}
		}
	}
	if transcodeExecutionAllowed && h.NodeRecipeStore != nil && h.NodeRecipeStore.Enabled() &&
		h.progressiveRemuxRelayAvailableV3(ctx) {
		if enumerator, ok := h.NodePlanner.(transcodeNodeEnumeratorV3); ok {
			for _, entry := range h.pooledNodeCapabilitiesV3(ctx, enumerator.TranscodeNodeURLs()) {
				if slices.Contains(entry.transportFeatures, playback.TransportFeatureProgressiveRemuxExecutionV1) {
					merged = append(merged, progressiveRemuxTransformationsV3(entry.transformations)...)
				}
			}
		}
	}
	if !localAllowed {
		return local.OnlyAdvertised(merged)
	}
	return local.WithAdvertised(merged)
}

func (h *PlaybackHandler) progressiveRemuxRelayAvailableV3(ctx context.Context) bool {
	enumerator, ok := h.NodePlanner.(proxyNodeEnumeratorV3)
	if !ok {
		return false
	}
	for _, entry := range h.pooledNodeCapabilitiesV3(ctx, enumerator.ProxyNodeURLs()) {
		if slices.Contains(entry.transportFeatures, playback.TransportFeatureProgressiveRemuxRelayV1) {
			return true
		}
	}
	return false
}

func progressiveRemuxTransformationsV3(transformations []playback.TransformationV3) []playback.TransformationV3 {
	result := make([]playback.TransformationV3, 0, len(transformations))
	for _, transformation := range transformations {
		name := strings.TrimSpace(transformation.Name)
		if strings.EqualFold(name, playback.TransformationAudioToAACV3) ||
			strings.EqualFold(name, playback.TransformationServerDV7HDR10V3) {
			result = append(result, transformation)
		}
	}
	return result
}

func progressiveRemuxExecutorAvailabilityV3(policy config.PlaybackRoutingPolicy, proxyAllowed bool) (bool, bool, bool) {
	routes, err := noderouting.Candidates(noderouting.Request{
		Workload: noderouting.WorkloadRemux, Delivery: noderouting.DeliveryProgressiveRemux,
		Policy: policy, ProxyAllowed: proxyAllowed,
	})
	if err != nil {
		return false, false, false
	}
	var localAllowed, proxyExecutionAllowed, transcodeExecutionAllowed bool
	for _, candidate := range routes.Candidates {
		localAllowed = localAllowed || candidate.Execution == noderouting.ExecutionAPI
		proxyExecutionAllowed = proxyExecutionAllowed || candidate.Execution == noderouting.ExecutionProxy
		transcodeExecutionAllowed = transcodeExecutionAllowed || candidate.Execution == noderouting.ExecutionTranscode
	}
	return localAllowed, proxyExecutionAllowed, transcodeExecutionAllowed
}

// hlsCapabilityRegistryWithInputsV3 reports the union of transformations that
// can execute on any admitted playback workload. The capability contract is
// not workload-scoped, while planning must keep these registries separate.
func (h *PlaybackHandler) hlsCapabilityRegistryWithInputsV3(
	ctx context.Context,
	settings playback.PlannerSettingsV3,
	inventory hlsToneMapCapabilityInventoryV3,
) *playback.TransformationRegistryV3 {
	local := h.localHLSExecutionRegistryWithInputsV3(ctx, settings, inventory)
	progressive := h.progressiveRemuxPlanningRegistryWithInputsV3(
		ctx, local, h.JWTSecret != "" || h.proxyEgressOriginsAvailableV3(),
	)
	remux := h.hlsPlanningRegistryWithInputsForWorkloadV3(
		ctx, settings, hlsToneMapCapabilityInventoryV3{}, noderouting.WorkloadRemux,
	)
	video := h.hlsPlanningRegistryWithInputsForWorkloadV3(
		ctx, settings, inventory, noderouting.WorkloadVideoTranscode,
	)
	return progressive.WithAdvertised(remux.Advertised()).WithAdvertised(video.Advertised())
}

// hlsPlanningRegistryWithInputsForWorkloadV3 keeps transformation
// availability inside the hard execution boundary for one HLS workload.
func (h *PlaybackHandler) hlsPlanningRegistryWithInputsForWorkloadV3(
	ctx context.Context,
	settings playback.PlannerSettingsV3,
	inventory hlsToneMapCapabilityInventoryV3,
	workload noderouting.Workload,
) *playback.TransformationRegistryV3 {
	return h.hlsPlanningRegistryWithInputsForClientV3(ctx, settings, inventory, workload, true)
}

// hlsPlanningRegistryWithInputsForClientV3 keeps transformation availability
// inside both the workload's execution boundary and this client's admitted
// egress boundary. The unscoped capability document uses proxyAllowed=true;
// one playback attempt supplies the client's actual proxy support.
func (h *PlaybackHandler) hlsPlanningRegistryWithInputsForClientV3(
	ctx context.Context,
	settings playback.PlannerSettingsV3,
	inventory hlsToneMapCapabilityInventoryV3,
	workload noderouting.Workload,
	proxyAllowed bool,
) *playback.TransformationRegistryV3 {
	local := h.localHLSExecutionRegistryWithInputsV3(ctx, settings, inventory)
	routingPolicy := h.playbackRoutingPolicyForContextV3(ctx)
	localAllowed, workerAllowed := hlsRouteExecutorAvailabilityForClientV3(workload, routingPolicy, proxyAllowed)
	if !workerAllowed {
		if !localAllowed {
			return local.OnlyAdvertised(nil)
		}
		return local
	}
	enumerator, ok := h.NodePlanner.(transcodeNodeEnumeratorV3)
	if !ok {
		if !localAllowed {
			return local.OnlyAdvertised(nil)
		}
		return local
	}
	nodeURLs := enumerator.TranscodeNodeURLs()
	if len(nodeURLs) == 0 {
		if !localAllowed {
			return local.OnlyAdvertised(nil)
		}
		return local
	}
	var merged []playback.TransformationV3
	for _, transformations := range h.pooledNodeTransformationsV3(ctx, nodeURLs) {
		merged = append(merged, transformations...)
	}
	if !localAllowed {
		return local.OnlyAdvertised(merged)
	}
	return local.WithAdvertised(merged)
}

func (h *PlaybackHandler) localHLSToneMapCapabilitiesForTransportV3(ctx context.Context) (bool, tonemap.Capabilities, error) {
	if !localHLSVideoRouteAllowedV3(h.playbackRoutingPolicyForContextV3(ctx)) {
		return false, nil, nil
	}
	capabilities, err := h.localToneMapCapabilitiesForTransportV3(ctx)
	return true, capabilities, err
}

func localHLSVideoRouteAllowedV3(policy config.PlaybackRoutingPolicy) bool {
	return localHLSRouteAllowedV3(noderouting.WorkloadVideoTranscode, policy)
}

func localHLSRouteAllowedV3(workload noderouting.Workload, policy config.PlaybackRoutingPolicy) bool {
	localAllowed, _ := hlsRouteExecutorAvailabilityV3(workload, policy)
	return localAllowed
}

func hlsRouteExecutorAvailabilityV3(workload noderouting.Workload, policy config.PlaybackRoutingPolicy) (bool, bool) {
	return hlsRouteExecutorAvailabilityForClientV3(workload, policy, true)
}

func hlsRouteExecutorAvailabilityForClientV3(
	workload noderouting.Workload,
	policy config.PlaybackRoutingPolicy,
	proxyAllowed bool,
) (bool, bool) {
	var delivery noderouting.Delivery
	switch workload {
	case noderouting.WorkloadRemux:
		delivery = noderouting.DeliveryHLSRemux
	case noderouting.WorkloadVideoTranscode:
		delivery = noderouting.DeliveryHLSVideo
	default:
		return false, false
	}
	routes, err := noderouting.Candidates(noderouting.Request{
		Workload: workload, Delivery: delivery, Policy: policy, ProxyAllowed: proxyAllowed,
	})
	if err != nil {
		return false, false
	}
	var localAllowed, workerAllowed bool
	for _, candidate := range routes.Candidates {
		localAllowed = localAllowed || candidate.Execution == noderouting.ExecutionAPI
		workerAllowed = workerAllowed || candidate.NeedsTranscodeNode()
	}
	return localAllowed, workerAllowed
}

// hlsToneMapCapabilityInventoryV3 snapshots local and pooled-node tone-map
// capabilities for a single planning operation.
func (h *PlaybackHandler) hlsToneMapCapabilityInventoryV3(ctx context.Context) (hlsToneMapCapabilityInventoryV3, error) {
	return h.hlsToneMapCapabilityInventoryForClientV3(ctx, true)
}

func (h *PlaybackHandler) hlsToneMapCapabilityInventoryForClientV3(
	ctx context.Context,
	proxyAllowed bool,
) (hlsToneMapCapabilityInventoryV3, error) {
	routingPolicy := h.playbackRoutingPolicyForContextV3(ctx)
	localAllowed, workerAllowed := hlsRouteExecutorAvailabilityForClientV3(
		noderouting.WorkloadVideoTranscode, routingPolicy, proxyAllowed,
	)
	fetchCtx, cancel := context.WithTimeout(ctx, h.toneMapPlanningTimeoutV3(localAllowed))
	defer cancel()

	type capabilityResult struct {
		capabilities tonemap.Capabilities
		err          error
	}
	var localResult capabilityResult
	var localWG sync.WaitGroup
	if localAllowed {
		localWG.Add(1)
		go func() {
			defer localWG.Done()
			localResult.capabilities, localResult.err = h.localToneMapCapabilitiesCachedV3(fetchCtx)
		}()
	}

	var nodeURLs []string
	if enumerator, ok := h.NodePlanner.(transcodeNodeEnumeratorV3); ok && workerAllowed {
		nodeURLs = enumerator.TranscodeNodeURLs()
	}
	results := make([]capabilityResult, len(nodeURLs))
	var wg sync.WaitGroup
	for i, nodeURL := range nodeURLs {
		wg.Add(1)
		go func(i int, nodeURL string) {
			defer wg.Done()
			remote, err := h.remoteToneMapCapabilitiesV3(fetchCtx, nodeURL, true)
			results[i].err = err
			if err != nil {
				slog.DebugContext(ctx, "protocol v3 node tone-map capability unavailable for planning", "component", "api", "node", logredact.SanitizeURL(nodeURL), "error", err)
				return
			}
			results[i].capabilities = remote
		}(i, nodeURL)
	}
	wg.Wait()
	localWG.Wait()

	inventory := hlsToneMapCapabilityInventoryV3{localTranscodeFallbackOK: localAllowed}
	var probeErr error
	if localAllowed {
		if localResult.err != nil {
			probeErr = errors.Join(probeErr, localResult.err)
		} else {
			inventory.local = localResult.capabilities
			inventory.union = append(inventory.union, localResult.capabilities...)
		}
	}
	for _, remote := range results {
		if remote.err != nil {
			probeErr = errors.Join(probeErr, remote.err)
			continue
		}
		inventory.union = append(inventory.union, remote.capabilities...)
	}
	return inventory, probeErr
}

// hlsToneMapCapabilitiesV3 builds the executor union available to HLS planning
// from eligible local fallback and all reachable transcode nodes.
func (h *PlaybackHandler) hlsToneMapCapabilitiesV3(ctx context.Context) tonemap.Capabilities {
	inventory, _ := h.hlsToneMapCapabilityInventoryV3(ctx)
	return inventory.union
}

type hlsPlanningSnapshotV3 struct {
	handler             *PlaybackHandler
	ctx                 context.Context
	settings            playback.PlannerSettingsV3
	localRegistry       *playback.TransformationRegistryV3
	proxyAllowed        bool
	progressiveOnce     sync.Once
	progressiveRegistry *playback.TransformationRegistryV3
	remuxOnce           sync.Once
	remuxRegistry       *playback.TransformationRegistryV3
	videoOnce           sync.Once
	videoRegistry       *playback.TransformationRegistryV3
	inventoryOnce       sync.Once
	inventory           hlsToneMapCapabilityInventoryV3
	inventoryErr        error
	inventoryResolved   bool
}

func (snapshot *hlsPlanningSnapshotV3) progressiveRemuxRegistry() *playback.TransformationRegistryV3 {
	snapshot.progressiveOnce.Do(func() {
		snapshot.progressiveRegistry = snapshot.handler.progressiveRemuxPlanningRegistryWithInputsV3(
			snapshot.ctx, snapshot.localRegistry, snapshot.proxyAllowed,
		)
	})
	return snapshot.progressiveRegistry
}

func (snapshot *hlsPlanningSnapshotV3) resolveInventory() {
	snapshot.inventoryOnce.Do(func() {
		snapshot.inventoryResolved = true
		policy := tonemap.NewPolicy(snapshot.settings.HardwareToneMapEnabled, snapshot.settings.SoftwareToneMapEnabled)
		if policy != tonemap.PolicyNone {
			snapshot.inventory, snapshot.inventoryErr = snapshot.handler.hlsToneMapCapabilityInventoryForClientV3(
				snapshot.ctx, snapshot.proxyAllowed,
			)
		}
	})
}

func (snapshot *hlsPlanningSnapshotV3) hlsRemuxRegistry() *playback.TransformationRegistryV3 {
	snapshot.remuxOnce.Do(func() {
		snapshot.remuxRegistry = snapshot.handler.hlsPlanningRegistryWithInputsForClientV3(
			snapshot.ctx, snapshot.settings, hlsToneMapCapabilityInventoryV3{}, noderouting.WorkloadRemux,
			snapshot.proxyAllowed,
		)
	})
	return snapshot.remuxRegistry
}

func (snapshot *hlsPlanningSnapshotV3) hlsVideoRegistry() *playback.TransformationRegistryV3 {
	snapshot.videoOnce.Do(func() {
		snapshot.resolveInventory()
		snapshot.videoRegistry = snapshot.handler.hlsPlanningRegistryWithInputsForClientV3(
			snapshot.ctx, snapshot.settings, snapshot.inventory, noderouting.WorkloadVideoTranscode,
			snapshot.proxyAllowed,
		)
	})
	return snapshot.videoRegistry
}

func (snapshot *hlsPlanningSnapshotV3) toneMapCapabilities() tonemap.Capabilities {
	snapshot.resolveInventory()
	return snapshot.inventory.union
}

func (snapshot *hlsPlanningSnapshotV3) capabilityError() error {
	if !snapshot.inventoryResolved {
		return nil
	}
	return snapshot.inventoryErr
}

func retryIncompleteToneMapPlanningV3(result playback.PlannerResultV3, capabilityErr error) playback.PlannerResultV3 {
	if capabilityErr == nil || result.Terminal == nil {
		return result
	}
	switch result.Terminal.Reason {
	case playback.TerminalHDRTranscodeUnsupportedV3, terminalSubtitleConversionUnsupportedV3:
	default:
		return result
	}
	result.Terminal = &playback.TerminalV3{
		Reason:    transcodeStartFailedReasonV3,
		Message:   "Tone-map capability discovery is temporarily unavailable.",
		Retryable: true,
	}
	return result
}

func retryIncompletePlaybackSettingsV3(result playback.PlannerResultV3, settingsErr error) playback.PlannerResultV3 {
	if settingsErr == nil || result.Terminal == nil {
		return result
	}
	terminal := result.Terminal
	settingsDependent := terminal.Reason == playback.TerminalHDRTranscodeUnsupportedV3 ||
		(terminal.Reason == terminalNoAlternateVersionV3 && terminal.Message == playback.TerminalMessage4KTranscodeDisabledV3) ||
		(terminal.Reason == terminalSubtitleConversionUnsupportedV3 &&
			(strings.Contains(terminal.Message, "this HDR source") || strings.Contains(terminal.Message, "4K transcoding is disabled")))
	if !settingsDependent {
		return result
	}
	result.Terminal = &playback.TerminalV3{
		Reason:    transcodeStartFailedReasonV3,
		Message:   "Playback settings are temporarily unavailable.",
		Retryable: true,
	}
	return result
}

func (h *PlaybackHandler) planPlaybackWithCapabilitiesV3(ctx context.Context, input playback.PlannerInputV3) (playback.PlannerResultV3, error) {
	mode := headerAuthenticatedMediaV3(input.Request.ClientFeatures)
	proxyAllowed := !mode.headerAuth && h.JWTSecret != "" || mode.proxyEgress && h.proxyEgressOriginsAvailableV3()
	snapshot := &hlsPlanningSnapshotV3{
		handler: h, ctx: ctx, settings: input.Settings, localRegistry: input.Registry, proxyAllowed: proxyAllowed,
	}
	input.ProgressiveRemuxRegistry = snapshot.progressiveRemuxRegistry
	input.HLSRemuxRegistry = snapshot.hlsRemuxRegistry
	input.HLSVideoRegistry = snapshot.hlsVideoRegistry
	input.HLSToneMapCapabilities = snapshot.toneMapCapabilities
	result := playback.PlanPlaybackV3(input)
	if result.Terminal != nil {
		switch result.Terminal.Reason {
		case playback.TerminalHDRTranscodeUnsupportedV3, terminalSubtitleConversionUnsupportedV3:
			return result, snapshot.capabilityError()
		}
	}
	return result, nil
}

// pooledNodeTransformationsV3 collects the advertised transformations of the
// given transcode nodes, keyed by node URL. Stale cache entries are refreshed
// concurrently under a short planning deadline; nodes that cannot be reached
// contribute nothing (their failures are negatively cached), so planning
// degrades toward the local registry instead of blocking the start path.
func (h *PlaybackHandler) pooledNodeTransformationsV3(ctx context.Context, nodeURLs []string) map[string][]playback.TransformationV3 {
	capabilities := h.pooledNodeCapabilitiesV3(ctx, nodeURLs)
	byURL := make(map[string][]playback.TransformationV3, len(capabilities))
	for nodeURL, entry := range capabilities {
		byURL[nodeURL] = append([]playback.TransformationV3(nil), entry.transformations...)
	}
	return byURL
}

func (h *PlaybackHandler) pooledNodeCapabilitiesV3(ctx context.Context, nodeURLs []string) map[string]v3NodeCapabilityCache {
	fetchCtx, cancel := context.WithTimeout(ctx, v3NodeCapabilityPlanTimeout)
	defer cancel()
	results := make([]v3NodeCapabilityCache, len(nodeURLs))
	available := make([]bool, len(nodeURLs))
	var wg sync.WaitGroup
	for i, nodeURL := range nodeURLs {
		wg.Add(1)
		go func(i int, nodeURL string) {
			defer wg.Done()
			entry, err := h.lookupRemoteCapabilitiesV3(fetchCtx, nodeURL, true)
			if err != nil {
				slog.DebugContext(ctx, "protocol v3 node capability unavailable for planning", "component", "api", "node", logredact.SanitizeURL(nodeURL), "error", err)
				return
			}
			results[i] = entry
			available[i] = true
		}(i, nodeURL)
	}
	wg.Wait()
	byURL := make(map[string]v3NodeCapabilityCache, len(nodeURLs))
	for i, entry := range results {
		if available[i] {
			byURL[nodeURLs[i]] = entry
		}
	}
	return byURL
}

// planNodeSessionV3 remains as a focused test seam. Production starts resolve
// the complete policy in resolveHLSRouteV3 below; this wrapper asks that same
// resolver for a hard worker route with the requested egress.
func (h *PlaybackHandler) planNodeSessionV3(ctx context.Context, session *playback.Session, result playback.PlannerResultV3, localEgress bool) nodepool.Plan {
	return h.planNodeSessionExcludingV3(ctx, session, result, localEgress, nil)
}

// planNodeSessionExcludingV3 is planNodeSessionV3 with a set of node URLs the
// caller has already exhausted. The tone-map software fallback uses it so a
// second attempt cannot land back on the node that just refused the recipe.
func (h *PlaybackHandler) planNodeSessionExcludingV3(ctx context.Context, session *playback.Session, result playback.PlannerResultV3, localEgress bool, excluded map[string]struct{}) nodepool.Plan {
	workload, _, ok := routingClassV3(result)
	if !ok {
		return nodepool.Plan{}
	}
	egress := config.PlaybackEgressPreferProxy
	if localEgress {
		egress = config.PlaybackEgressAPIOnly
	}
	policy := config.DefaultPlaybackRoutingPolicy()
	if workload == noderouting.WorkloadRemux {
		policy.RemuxExecution = config.PlaybackExecutionWorkerOnly
		policy.RemuxEgress = egress
	} else {
		policy.VideoTranscodeExecution = config.PlaybackExecutionWorkerOnly
		policy.VideoTranscodeEgress = egress
	}
	return h.resolveHLSRouteWithPolicyV3(ctx, session, result, policy, true, excluded, nil).Plan
}

func routingClassV3(result playback.PlannerResultV3) (noderouting.Workload, noderouting.Delivery, bool) {
	if result.Plan == nil {
		return "", "", false
	}
	switch result.Plan.Delivery {
	case playback.DeliveryOriginalHTTPV3:
		return noderouting.WorkloadDirectPlay, noderouting.DeliveryDirect, true
	case playback.DeliveryRemuxProgressiveV3:
		return noderouting.WorkloadRemux, noderouting.DeliveryProgressiveRemux, true
	case playback.DeliveryRemuxHLSV3:
		return noderouting.WorkloadRemux, noderouting.DeliveryHLSRemux, true
	case playback.DeliveryTranscodeHLSV3:
		return noderouting.WorkloadVideoTranscode, noderouting.DeliveryHLSVideo, true
	default:
		return "", "", false
	}
}

func routingWorkloadV3(result playback.PlannerResultV3) noderouting.Workload {
	workload, _, _ := routingClassV3(result)
	return workload
}

func (h *PlaybackHandler) playbackRoutingPolicyV3() config.PlaybackRoutingPolicy {
	return config.EffectivePlaybackRoutingPolicy(h.playbackConfig().Routing)
}

type playbackRoutingPolicySnapshotKeyV3 struct{}

func withPlaybackRoutingPolicySnapshotV3(ctx context.Context, policy config.PlaybackRoutingPolicy) context.Context {
	return context.WithValue(ctx, playbackRoutingPolicySnapshotKeyV3{}, config.EffectivePlaybackRoutingPolicy(policy))
}

func (h *PlaybackHandler) playbackRoutingPolicyForContextV3(ctx context.Context) config.PlaybackRoutingPolicy {
	if ctx != nil {
		if policy, ok := ctx.Value(playbackRoutingPolicySnapshotKeyV3{}).(config.PlaybackRoutingPolicy); ok {
			return policy
		}
	}
	return h.playbackRoutingPolicyV3()
}

func (h *PlaybackHandler) resolveHLSRouteWithPolicyV3(
	ctx context.Context,
	session *playback.Session,
	result playback.PlannerResultV3,
	policy config.PlaybackRoutingPolicy,
	proxyAllowed bool,
	excludedNodes map[string]struct{},
	excludedShapes map[string]struct{},
) noderouting.Decision {
	workload, delivery, ok := routingClassV3(result)
	if !ok || session == nil {
		return noderouting.Decision{Outcome: noderouting.OutcomePolicyUnsatisfied}
	}
	eligible := h.transcodeEligibilityV3(ctx, result, excludedNodes)
	// A client that arrived through a network access provider can only use a
	// proxy that has a connected origin on that same overlay; on the default
	// path this is nil and every healthy proxy stays eligible. Filtering here
	// rather than at the URL builder keeps the resolver's fallbacks (API
	// egress, API relay to the transcode node) in charge of what happens next.
	proxyEligible := nodepool.ClientReachableVia(netaccess.PathFromContext(ctx), nil)
	decision, err := noderouting.Resolve(noderouting.AdaptSessionPlanner(h.NodePlanner), noderouting.ResolveRequest{
		Request: noderouting.Request{
			Workload: workload, Delivery: delivery, Policy: policy, ProxyAllowed: proxyAllowed,
		},
		SessionID: session.ID, CurrentTranscodeURL: session.TranscodeNodeURL,
		EstimatedBitrateKbps: result.TargetBitrateKbps,
		TranscodeEligible:    eligible, ProxyEligible: proxyEligible, ExcludedShapeIDs: excludedShapes,
	})
	if err != nil {
		slog.ErrorContext(ctx, "compile playback node route", "component", "noderouting", "error", err)
		return noderouting.Decision{Outcome: noderouting.OutcomePolicyUnsatisfied}
	}
	return decision
}

// predicateSessionPlannerV3 is the legacy planner seam that accepts an
// eligibility predicate without implementing the exact-route contract.
type predicateSessionPlannerV3 interface {
	PlanSessionWith(sessionID, currentTranscodeURL string, needsTranscode bool, estBitrateKbps int, eligible func(*nodepool.Node) bool) nodepool.Plan
}

// transcodeEligibilityV3 builds the node predicate the route resolver applies
// before it reserves. Plans that carry server transformations restrict
// selection to nodes whose advertised capabilities validate against the plan,
// so load balancing in a heterogeneous pool cannot land a recipe on a node that
// would reject it when a capable sibling exists. A nil predicate means every
// pooled node is acceptable.
func (h *PlaybackHandler) transcodeEligibilityV3(ctx context.Context, result playback.PlannerResultV3, excluded map[string]struct{}) func(*nodepool.Node) bool {
	// The per-node capability fan-out only pays for itself when a planner can
	// actually consume the predicate it produces. A planner that implements
	// neither the exact route contract nor the predicate seam would spend a
	// round of capability lookups on a filter nothing reads.
	_, exactPlanner := h.NodePlanner.(nodepool.RoutePlanner)
	_, predicatePlanner := h.NodePlanner.(predicateSessionPlannerV3)
	requiresProgressiveRemux := result.Plan != nil && result.Plan.Delivery == playback.DeliveryRemuxProgressiveV3
	if enumerator, ok := h.NodePlanner.(transcodeNodeEnumeratorV3); ok && (exactPlanner || predicatePlanner) &&
		(planRequiresServerTransformationsV3(result.Plan) || requiresProgressiveRemux) {
		capable := make(map[string]struct{})
		for nodeURL, entry := range h.pooledNodeCapabilitiesV3(ctx, enumerator.TranscodeNodeURLs()) {
			if requiresProgressiveRemux && !slices.Contains(entry.transportFeatures, playback.TransportFeatureProgressiveRemuxExecutionV1) {
				continue
			}
			if validateAdvertisedTransformationsV3(result.Plan, entry.transformations) != nil {
				continue
			}
			capable[nodeURL] = struct{}{}
		}
		// The predicate runs under the planner lock: a set lookup only.
		return func(node *nodepool.Node) bool {
			if node == nil {
				return false
			}
			if _, skip := excluded[node.URL]; skip {
				return false
			}
			_, found := capable[node.URL]
			return found
		}
	}
	if len(excluded) == 0 {
		return nil
	}
	// No capability filter applies to this plan, but an exhausted node must
	// still be kept out of the selection.
	return func(node *nodepool.Node) bool {
		if node == nil {
			return false
		}
		_, skip := excluded[node.URL]
		return !skip
	}
}

// planRequiresToneMapV3 reports whether a plan contains the server-owned HDR
// to SDR transformation.
func planRequiresToneMapV3(plan *playback.PlanV3) bool {
	if plan == nil {
		return false
	}
	for _, transformation := range plan.Transformations {
		if transformation.Executor == playback.ExecutorServerV3 &&
			transformation.Name == playback.TransformationHDRToSDRToneMapV3 {
			return true
		}
	}
	return false
}

// validateToneMapExecutorV3 confirms that the selected executor still supports
// the exact mode and source kind frozen by planning.
func validateToneMapExecutorV3(result playback.PlannerResultV3, capabilities tonemap.Capabilities) error {
	if !planRequiresToneMapV3(result.Plan) {
		return nil
	}
	if result.ToneMapMode == "" || result.ToneMapSourceKind == "" ||
		!capabilities.Supports(result.ToneMapMode, result.ToneMapSourceKind) {
		return fmt.Errorf("executor lacks %s %s tone mapping", result.ToneMapMode, result.ToneMapSourceKind)
	}
	return nil
}

// validateAdvertisedTransformationsV3 verifies that every server-executed
// transformation the plan requires is advertised — at the exact recipe
// version — by the executor under consideration (a pooled node's capability
// response or the local registry's Advertised set).
func validateAdvertisedTransformationsV3(plan *playback.PlanV3, advertised []playback.TransformationV3) error {
	available := make(map[string]string, len(advertised))
	for _, transformation := range advertised {
		available[strings.ToLower(strings.TrimSpace(transformation.Name))] = strings.TrimSpace(transformation.RecipeVersion)
	}
	if plan == nil {
		return errors.New("playback plan is unavailable")
	}
	for _, required := range plan.Transformations {
		if strings.EqualFold(required.Executor, "client") {
			continue
		}
		version, ok := available[strings.ToLower(strings.TrimSpace(required.Name))]
		if !ok || version != strings.TrimSpace(required.RecipeVersion) {
			return fmt.Errorf("executor lacks transformation %s@%s", required.Name, required.RecipeVersion)
		}
	}
	return nil
}

// HandlePlaybackCapabilityV3 reports only transformations that the installed
// runtime has actually probed. Protocol v3 is the server's only playback
// protocol, so `enabled` is constant; it stays in the response because clients
// feature-detect against it and the field is part of the frozen contract.
func (h *PlaybackHandler) HandlePlaybackCapabilityV3(w http.ResponseWriter, r *http.Request) {
	if apimw.GetUserID(r.Context()) == 0 {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}
	response := playback.CapabilityResponseV3{Enabled: true, ProtocolVersions: []int{playback.ProtocolV3}}
	response.Features = playback.ServerFeaturesV3()
	response.Deliveries = []playback.DeliveryV3{playback.DeliveryOriginalHTTPV3, playback.DeliveryRemuxProgressiveV3, playback.DeliveryRemuxHLSV3, playback.DeliveryTranscodeHLSV3}
	settings := h.plannerSettingsV3(r.Context())
	policy := tonemap.NewPolicy(settings.HardwareToneMapEnabled, settings.SoftwareToneMapEnabled)
	inventory := hlsToneMapCapabilityInventoryV3{}
	if policy != tonemap.PolicyNone {
		inventory, _ = h.hlsToneMapCapabilityInventoryV3(r.Context())
	}
	registry := h.hlsCapabilityRegistryWithInputsV3(r.Context(), settings, inventory)
	toneMapAvailable := false
	if policy != tonemap.PolicyNone {
		for _, capability := range inventory.union {
			if policy.Allows(capability.Mode) && len(capability.SourceKinds) > 0 {
				toneMapAvailable = true
				break
			}
		}
	}
	for _, transformation := range registry.Advertised() {
		if transformation.Name == playback.TransformationHDRToSDRToneMapV3 && !toneMapAvailable {
			continue
		}
		response.Transformations = append(response.Transformations, transformation)
	}
	writeJSON(w, http.StatusOK, response)
}

// handleStartPlaybackV3 validates, plans, and starts a protocol-v3 request.
func (h *PlaybackHandler) handleStartPlaybackV3(w http.ResponseWriter, r *http.Request, body []byte) {
	response, err := h.startPlaybackApplicationV3(r, body)
	if err != nil {
		writePlaybackOperationError(w, err)
		return
	}
	if response.Outcome == playback.OutcomePlayableV3 {
		h.recordStartingDevice(r.Context(), apimw.GetUserID(r.Context()), apimw.GetProfileID(r.Context()), deviceMetadataFromRequest(r))
	}
	writeJSON(w, http.StatusCreated, response)
}

// recordStartingDevice registers the device that started playback, so a
// device that plays without ever writing a device setting still appears in
// the profile's device registry with a current last_seen_at. Callers record
// only a playable decision; a terminal decision means the device did not play. A start always
// plays as the caller's own profile (profile_id must match X-Profile-Id), so
// the declared device is the caller's own device on the caller's own profile.
func (h *PlaybackHandler) recordStartingDevice(ctx context.Context, userID int, profileID string, device DeviceMetadata) {
	h.DeviceSightings.RecordFor(ctx, h.StoreProvider, userID, profileID, device)
}

func (h *PlaybackHandler) startPlaybackApplicationV3(r *http.Request, body []byte) (playback.DecisionResponseV3, error) {
	r = r.WithContext(withPlaybackRoutingPolicySnapshotV3(r.Context(), h.playbackRoutingPolicyV3()))
	timings := newPlaybackStartTimingsV3()
	var req playback.StartRequestV3
	defer func() { timings.log(r.Context(), req.PlaybackAttemptID) }()
	if err := json.Unmarshal(body, &req); err != nil {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusBadRequest, "bad_request", "Invalid protocol v3 request body")
	}
	// A selected track identity can name the effective file of an earlier
	// attempt, which virtual candidate rotation replaces. Drop it before the
	// structural id/index check, mirroring the replan path's re-key-before-
	// validation order: the start degrades to the default track pipeline
	// instead of failing 400. NormalizeAndValidate applies the same drop as a
	// second line of defense for callers that validate a copy (api/v2).
	dropStaleRequestTrackIdentitiesV3(r.Context(), &req)
	warnings, err := req.NormalizeAndValidate()
	if err != nil {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusBadRequest, "bad_request", err.Error())
	}
	if !isNativeAPIV2(r.Context()) {
		// The frozen /api/v1 surface negotiates only its original features.
		// subrip_sidecar_v1 is attempt-sticky, so dropping it here keeps it out
		// of every later replan of this attempt too.
		req.ClientFeatures = playback.WithoutFeatureV3(req.ClientFeatures, playback.FeatureSubripSidecarV3)
	}
	timings.mark("decode_validate")
	profileID := apimw.GetProfileID(r.Context())
	if profileID == "" {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusBadRequest, "bad_request", "X-Profile-Id header is required")
	}
	if req.ProfileID != profileID {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusBadRequest, "bad_request", "profile_id must match X-Profile-Id")
	}
	userID := apimw.GetUserID(r.Context())
	deviceID := deviceMetadataFromRequest(r).DeviceID
	requestDigests := newPlaybackStartRequestDigestsV3(body, deviceID)
	resolutionWasAssumed := false
	// resolutionProvenance is the virtual resolver's provenance for the source
	// this start actually resolves, published additively on the plan (see
	// PlanV3.InventoryProvenance). It stays empty for a local source.
	resolutionProvenance := ProbeProvenance("")
	if existing, lookupErr := h.PlanStoreV3.GetAttemptByPlaybackAttemptID(r.Context(), req.PlaybackAttemptID); lookupErr == nil {
		if existing.UserID != userID || existing.ProfileID != profileID || existing.RequestedMediaFileID != req.FileID ||
			!requestDigests.matches(existing.RequestDigest) {
			return playback.DecisionResponseV3{}, playbackOperationError(http.StatusConflict, "playback_attempt_reused", "The playback attempt ID belongs to a different request")
		}
		response := decisionResponseFromAttemptV3(existing)
		// A terminal publishes no subtitle URLs, so it replays on either
		// surface; the surface check protects only a playable plan.
		if response.Terminal != nil {
			return response, nil
		}
		if err := requireAttemptAPISurfaceV3(r.Context(), existing, req.ClientFeatures); err != nil {
			return playback.DecisionResponseV3{}, err
		}
		// The replayed plan is only usable while its session is alive; a dead
		// session must surface as a retryable terminal so the client mints a
		// fresh attempt instead of replaying a plan it can never stream. A
		// stopped attempt is dead on every replica, whether or not this one
		// still holds the session.
		if existing.SessionID == "" {
			return playback.DecisionResponseV3{}, playbackOperationError(http.StatusInternalServerError, "internal_error", "Stored playback attempt has no replayable decision")
		}
		if !startReplayResponseBelongsToAttemptV3(response, existing) {
			// The durable decision names a session other than the one this
			// attempt owns. Replaying it would hand the client a plan it can
			// never stream (transport preparation rejects a plan whose
			// SessionID differs) and the client would read that guard as a
			// replacement belonging to another session. Refuse the replay as a
			// retryable terminal so the client mints a fresh attempt id; the
			// replan path guards the same invariant in
			// completedReplanResponseMatchesAttemptV3.
			return playback.NewTerminalResponseV3("session_expired", "The playback plan for this attempt does not belong to its session.", true), nil
		}
		if existing.StoppedAt != nil {
			return playback.NewTerminalResponseV3("session_expired", "The playback session for this attempt has ended.", true), nil
		}
		if _, sessionErr := h.sessionMgr.GetSession(existing.SessionID); sessionErr != nil {
			return playback.NewTerminalResponseV3("session_expired", "The playback session for this attempt has ended.", true), nil
		}
		return response, nil
	} else if !errors.Is(lookupErr, playback.ErrSessionNotFound) {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusInternalServerError, "internal_error", "Failed to check playback attempt idempotency")
	}
	timings.mark("idempotency")
	if scope, ok := access.GetScope(r.Context()); ok {
		capKbps := streamlocation.BitrateCap(r.Context(), scope.MaxLocalStreamBitrateKbps, scope.MaxRemoteStreamBitrateKbps)
		r = r.WithContext(withServerBitrateCapV3(r.Context(), capKbps))
	}
	requestedFile, err := h.loadAuthorizedFile(r, req.FileID)
	if err != nil {
		return playback.DecisionResponseV3{}, playbackFileOperationError(err)
	}
	timings.mark("file_load")
	// The authorized catalog row is kept so a decode-rejection rotation can
	// re-resolve the same request with each rejected provider candidate
	// excluded, after requestedFile has been replaced by a resolved candidate.
	catalogRequestedFile := requestedFile
	// virtualDecision accumulates the virtual-source facts the plan log names.
	// It stays zero for a local source, and the log only reads it when the
	// plan carries a virtual candidate URI.
	virtualDecision := virtualPlanDecisionV3{candidateCount: -1}
	// resolvedEffectiveFileID is the catalog row id of the candidate the
	// virtual resolver actually served. It differs from the requested row when
	// the resolver substituted a different candidate that has its own row
	// (dedup keeper, rotation, sibling); the plan's effective_media_file_id
	// must name that row so the substitution is visible to the client.
	resolvedEffectiveFileID := 0
	// resolved holds the virtual resolver's result when this start took the
	// virtual path. It is kept so the post-commit spawn can read the deferred
	// probe the resolve handed back (see resolved.DeferredProbe).
	var resolved resolvedVirtualPlaybackSource
	// Virtual sources are provider-neutral URIs, not FFmpeg inputs. Resolve and
	// probe them through the virtual provider before the generic probe repair
	// path, which only understands local/HTTP media files.
	//
	// Jellycompat cannot see this split and is intentionally excluded from it.
	// The Jellyfin protocol surface (internal/jellycompat) reads its own
	// session/media-source model and never consumes PlanV3: it does not import
	// internal/playback and has no reference to RequestedMediaFileID,
	// EffectiveMediaFileID, EffectiveVirtualURI, or InventoryProvenance. A
	// client-visible jellycompat driver of the requested/effective split is
	// therefore out of scope here, not silently missing; adding one means
	// teaching its session model the split first.
	if isVirtualPlaybackFile(requestedFile) {
		requestedCatalogFileID := requestedFile.ID
		// An auto selection skips a catalog row the catalog marked failed, even
		// when the row already carries a concrete result= identity adopted by an
		// earlier auto pick; only an explicit pick or a forced relink re-tries
		// the known-bad candidate.
		allowFailedCandidate := req.FileSelection == playback.FileSelectionExplicitV3 || req.ForceRelink
		// The start path is a fresh selection: declare the unbound intent on the
		// request handed to the walk, which threads it (rather than forcing it)
		// so the stale-pin recovery may re-pin here while a future session-bound
		// caller reusing the walk keeps its own binding intent and refuses.
		var resolveErr error
		walkSourceReq := r.WithContext(withVirtualSessionBindingV3(r.Context(), false))
		resolved, resolveErr = h.resolveVirtualStartWithVersionFallback(walkSourceReq, requestedFile, profileID, req, allowFailedCandidate, intOrZeroHandlerV3(req.BandwidthCapKbps))
		if resolveErr != nil {
			// A confirmed-dead pinned release is indicted here so a retry does
			// not re-resolve it; an empty provider listing is not a verdict and
			// is left unmarked.
			h.stampStartVirtualCandidateFailed(r.Context(), requestedFile, resolveErr)
			termFileID := requestedFile.ID
			if requestedFile.EpisodeID != "" && h.VirtualEpisodeFileLookup != nil {
				if dbFile, err := h.VirtualEpisodeFileLookup(r.Context(), requestedFile.EpisodeID); err == nil && dbFile != nil && dbFile.ID > 0 {
					termFileID = dbFile.ID
				}
			} else if requestedFile.EpisodeID == "" && h.VirtualContentFileLookup != nil && requestedFile.ContentID != "" {
				if dbFile, err := h.VirtualContentFileLookup(r.Context(), requestedFile.ContentID); err == nil && dbFile != nil && dbFile.ID > 0 {
					termFileID = dbFile.ID
				}
			}
			req.FileID = termFileID
			response, persistErr := h.persistTerminalStartDecisionV3(r.Context(), userID, profileID, req, requestDigests, termFileID, termFileID, virtualStartUnresolvedTerminalV3(resolveErr))
			if persistErr != nil {
				return playback.DecisionResponseV3{}, playbackPersistenceOperationError(persistErr)
			}
			return response, nil
		}
		if resolved.File == nil {
			return playback.DecisionResponseV3{}, playbackOperationError(http.StatusBadGateway, "virtual_resolve_failed", "Failed to resolve virtual source")
		}
		resolvedEffectiveFileID = resolved.File.ID
		resolvedFile := *resolved.File
		resolvedFile.ID = requestedCatalogFileID
		requestedFile = &resolvedFile
		requestedFile.FilePath = resolved.URI
		requestedFile.VirtualOwnerInstallationID = resolved.OwnerID
		// Do NOT mutate req.FileID here: the original caller-supplied file ID
		// must survive into the attempt record for idempotent replay.
		resolutionWasAssumed = resolved.ResolutionAssumed
		resolutionProvenance = resolved.Provenance
		virtualDecision.candidateRank = resolved.CandidateRank
		virtualDecision.candidateCount = resolved.CandidateCount
		// The alternate-version walk resolved a different release than the one
		// asked for. Carry that truth onto the plan so the client can show an
		// honest substitution notice naming the requested release.
		if resolved.SubstitutedFromFileID > 0 {
			virtualDecision.substitutionReason = resolved.SubstitutionReason
		}
	} else {
		requestedFile = h.ensurePlaybackProbeStart(r.Context(), requestedFile)
	}
	if requestedFile.Duration <= 0 {
		if requestedFile.EpisodeID != "" && h.EpisodeLookup != nil {
			if ep, err := h.EpisodeLookup.GetByID(r.Context(), requestedFile.EpisodeID); err == nil && ep != nil && ep.Runtime > 0 {
				requestedFile.Duration = ep.Runtime * 60
			}
		} else if requestedFile.ContentID != "" && h.ItemLookup != nil {
			if item, err := h.ItemLookup.GetByID(r.Context(), requestedFile.ContentID); err == nil && item != nil && item.Runtime > 0 {
				requestedFile.Duration = item.Runtime * 60
			}
		}
	}
	if dropStaleAudioTrackIdentityV3(r.Context(), requestedFile, req.AudioTrackID) {
		req.AudioTrackID = ""
		req.AudioTrackIndex = nil
	}
	timings.mark("file_load_probe")
	audioIndex, audioDegraded, err := resolveV3AudioIndex(requestedFile, req.AudioTrackID, req.AudioTrackIndex)
	if err != nil {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusBadRequest, "bad_request", err.Error())
	}
	if req.CarriedAudioTrackID != "" {
		// Version switch: the viewer made an explicit audio choice on the previous
		// version; carry it onto the requested file by track family rather than
		// letting the server's preference override an explicit choice.
		if carriedIndex, ok := h.resolveCarriedAudioTrackV3(r.Context(), req.CarriedAudioTrackID, requestedFile); ok {
			audioIndex = carriedIndex
			req.AudioTrackIndex = &carriedIndex
			req.AudioTrackID = ""
		}
	}
	if audioDegraded {
		warnings = append(warnings, playback.AudioTrackUnavailableWarningV3())
	}
	if req.AudioTrackID == "" && req.AudioTrackIndex == nil {
		audioIndex, virtualDecision.preferredAudioLanguage, err = h.preferredAudioTrackIndexV3(r.Context(), userID, profileID, deviceID, requestedFile)
		if err != nil {
			return playback.DecisionResponseV3{}, playbackOperationError(http.StatusInternalServerError, "internal_error", "Failed to load the saved audio preference")
		}
		if req.CarriedAudioTrackID == "" {
			// A carried selection is an explicit viewer choice remapped onto
			// this file; only a fully omitted selection is server-resolved.
			virtualDecision.startAudioSelectionOrigin = SelectionOriginAuto
		} else {
			virtualDecision.startAudioSelectionOrigin = SelectionOriginExplicit
		}
	} else {
		virtualDecision.startAudioSelectionOrigin = SelectionOriginExplicit
	}
	timings.mark("audio_preference")
	effectiveFile := requestedFile
	// The plan's effective file is the candidate the resolver actually served.
	// requestedFile keeps the caller's requested row id (for the attempt record
	// and idempotent replay), so a substituted candidate needs its own copy
	// carrying the served row id; otherwise effective_media_file_id would
	// silently report the requested row for bytes it did not describe.
	if resolvedEffectiveFileID > 0 && resolvedEffectiveFileID != requestedFile.ID {
		effectiveCopy := *requestedFile
		effectiveCopy.ID = resolvedEffectiveFileID
		effectiveFile = &effectiveCopy
	}
	// downloadedSubtitleInventoryV3 is an indexed read, and the planner appends
	// the inventory after the effective file's own external and embedded tracks
	// (BuildSubtitleInventoryV3). A candidate therefore needs its own inventory,
	// so it cannot be hoisted to a single value across alternates. The same file
	// can still be planned more than once in one start (a subtitle-miss degrade,
	// or the transport-failure retry), so memoize by file ID for the duration of
	// this start instead of re-querying.
	subtitleInventoryByFileID := map[int][]playback.SubtitleInventoryEntryV3{}
	subtitleInventoryFor := func(file *models.MediaFile) []playback.SubtitleInventoryEntryV3 {
		if file == nil {
			return nil
		}
		if file.ID != 0 {
			if inventory, ok := subtitleInventoryByFileID[file.ID]; ok {
				return inventory
			}
		}
		inventory := h.downloadedSubtitleInventoryV3(r.Context(), file)
		if file.ID != 0 {
			subtitleInventoryByFileID[file.ID] = inventory
		}
		return inventory
	}
	settings, settingsErr := h.plannerSettingsV3Result(r.Context())
	timings.mark("planner_settings")
	if err := preflightPlaybackFile(r.Context(), effectiveFile, h.MissingMarker, h.EventsHub); err != nil {
		return playback.DecisionResponseV3{}, playbackPreflightOperationError(err)
	}
	timings.mark("file_preflight")
	if req.StartPosition == nil {
		req.StartPosition, err = h.resumePositionV3(r.Context(), userID, profileID, effectiveFile)
		if err != nil {
			return playback.DecisionResponseV3{}, playbackOperationError(http.StatusInternalServerError, "internal_error", "Failed to load saved playback progress")
		}
		// Multipart audiobook files share one item progress row, while the
		// planner starts a single file-local timeline. When the stored resume
		// point is item-absolute, select the corresponding part and translate
		// it to that part's local clock. Explicit client positions remain
		// unchanged.
		if req.StartPosition != nil && effectiveFile.PresentationPartTotal > 1 {
			target, local, resolveErr := h.multipartResumeFileV3(r.Context(), effectiveFile, *req.StartPosition, requestAccessFilter(r))
			if resolveErr != nil {
				// An item-absolute position cannot be projected onto this
				// file's part-local clock without the complete ordered part
				// list. Applying it to the requested part would seek that part
				// far past its end, so start from the beginning instead.
				slog.DebugContext(r.Context(), "protocol v3 multipart resume mapping unavailable", "component", "api", "file_id", effectiveFile.ID, "error", resolveErr)
				req.StartPosition = nil
			} else if target != nil && target.ID != requestedFile.ID && !req.AllowsAlternateVersions() {
				// A fixed-file attempt cannot resume into a different part.
				req.StartPosition = new(float64(0))
			} else if target != nil {
				effectiveFile = h.ensurePlaybackProbeStart(r.Context(), target)
				audioIndex = remapAudioIndexV3(requestedFile, effectiveFile, audioIndex)
				if err := preflightPlaybackFile(r.Context(), effectiveFile, h.MissingMarker, h.EventsHub); err != nil {
					return playback.DecisionResponseV3{}, playbackPreflightOperationError(err)
				}
				req.StartPosition = &local
			} else {
				req.StartPosition = nil
			}
		}
	}
	timings.mark("resume")
	// If resume selected a specific presentation part, alternate playback must
	// stay on that same part. Sibling parts are not interchangeable versions.
	alternateBase := requestedFile
	if effectiveFile.PresentationPartTotal > 1 && effectiveFile.PresentationPartIndex > 0 {
		alternateBase = effectiveFile
	}
	// Split the planning bucket so a cold start can attribute a multi-second
	// stall: the subtitle inventory read, the transformation registry /
	// DV-RPU probe closure build, and the planner call itself (which lazily
	// fetches node transformation and tone-map capabilities behind its own
	// bounded planning timeout). The old single planning_ms mark folded all
	// three together and could not say which one consumed the budget.
	effectiveSubtitles := subtitleInventoryFor(effectiveFile)
	timings.mark("subtitle_inventory")
	transformationRegistry := h.transformationRegistryV3(r.Context())
	dvrpuStrippable := h.lazyDVRPUStrippableV3(r.Context(), effectiveFile)
	timings.mark("transform_registry")
	lowerVersion := h.lowerVersionV3(r.Context(), req, alternateBase, settings, requestAccessFilter(r))
	result, toneMapCapabilityErr := h.planPlaybackWithCapabilitiesV3(r.Context(), playback.PlannerInputV3{
		Request: req, RequestedFile: requestedFile, EffectiveFile: effectiveFile, LowerVersion: lowerVersionForFileV3(lowerVersion, effectiveFile),
		ServerBitrateCapKbps: serverBitrateCapV3(r.Context()),
		AudioTrackIndex:      audioIndex, Settings: settings,
		Registry: transformationRegistry, DVRPUStrippable: dvrpuStrippable, Now: time.Now(),
		AdditionalSubtitles: effectiveSubtitles,
		InventoryProvenance: string(resolutionProvenance),
	})
	timings.mark("planning")
	// A subtitle-only refusal is resolved on the release already mounted: before
	// any alternate-file hunt, re-plan that file with the subtitle dropped. The
	// helper plans the effective file verbatim, so a virtual release is never
	// re-resolved or substituted. Only when that same-release degrade also fails
	// does the refusal fall through to the alternate-version failover, because
	// the failure proves the release itself is blocked.
	subtitleDegradeUnresolved := false
	if subtitleOnlyTerminalV3(result.Terminal) {
		if degradedReq, degradedResult, degradedToneMapErr, ok := h.degradeStartSubtitleInPlaceV3(r, req, requestedFile, effectiveFile, audioIndex, settings, resolutionProvenance); ok {
			req = degradedReq
			result = degradedResult
			toneMapCapabilityErr = degradedToneMapErr
		} else {
			// The same release cannot play without the subtitle either. The
			// reason is not subtitle-only in effect: the underlying policy
			// (HDR re-encode, 4K, transcoding disabled) is what blocks the
			// release, so the alternate-version failover applies as it does
			// for the genuine video/policy reasons. `terminalAllowsAlternateFileV3`
			// deliberately omits the subtitle reason as a direct trigger; this
			// is the one path that re-admits it, and only after the in-place
			// degrade has proved the release itself is blocked.
			subtitleDegradeUnresolved = true
		}
	}
	// An audio-only refusal takes the same same-release route: re-plan the file
	// already mounted with a different audio track before any alternate version
	// is considered. The helper keeps an explicit audio pick untouched, so a
	// viewer's choice is never silently overridden. Only when no track makes the
	// release playable does the refusal fall through, because that proves the
	// release itself is blocked rather than one track on it.
	audioDegradeUnresolved := false
	if audioOnlyTerminalV3(result.Terminal) && req.AudioTrackID == "" && req.AudioTrackIndex == nil {
		// Only an omitted (server-resolved) audio selection is substituted. An
		// explicit audio pick is the viewer's intent: its failure surfaces as
		// an audio terminal so the client can re-pick, and it never re-admits
		// the version hunt.
		if degradedReq, degradedIndex, degradedResult, degradedToneMapErr, ok := h.degradeStartAudioInPlaceV3(r, req, requestedFile, effectiveFile, audioIndex, settings, resolutionProvenance); ok {
			req = degradedReq
			audioIndex = degradedIndex
			result = degradedResult
			toneMapCapabilityErr = degradedToneMapErr
		} else {
			audioDegradeUnresolved = true
		}
	}
	huntAllowed := terminalAllowsAlternateFileV3(result.Terminal) ||
		(subtitleDegradeUnresolved && subtitleOnlyTerminalV3(result.Terminal)) ||
		(audioDegradeUnresolved && audioOnlyTerminalV3(result.Terminal))
	if req.AllowsAlternateVersions() && huntAllowed && shouldTryAlternateFileV3(req.QualityPreference) && req.FileSelection != playback.FileSelectionExplicitV3 {
		accessFilter := requestAccessFilter(r)
		alternateOrder := alternateOrderingForClient(req.Capabilities)
		if alternates, alternateErr := h.findAlternateFilesOrdered(r.Context(), alternateBase, accessFilter, alternateOrder); alternateErr == nil {
			if alternateBase != requestedFile {
				alternates = slices.DeleteFunc(alternates, func(candidate *models.MediaFile) bool {
					return candidate == nil || candidate.PresentationPartIndex != alternateBase.PresentationPartIndex
				})
			}
			baseReq := req
			baseAudioIndex := audioIndex
			var firstFailureResult playback.PlannerResultV3
			var firstFailureToneMapErr error
			var firstFailureFile *models.MediaFile
			var firstFailureReq playback.StartRequestV3
			var firstFailureProvenance ProbeProvenance
			firstFailureAudioIndex := 0
			var droppedSubtitle *alternateCandidateV3
			// A candidate that cannot honor the subtitle selection is held as a
			// last-resort degrade and only used after every candidate has been
			// tried, so an explicit pick still hunts for a version that has it.
			var subtitleMissFile *models.MediaFile
			var subtitleMissReq playback.StartRequestV3
			var subtitleMissProvenance ProbeProvenance
			subtitleMissAudioIndex := 0
			// A tone-map capability failure is server-wide, not per-candidate:
			// every sibling would plan to the same verdict, so paying a
			// resolve+probe+plan round-trip per file only delays the start the
			// client is already waiting on. Stop at the first.
			var capabilityBlocked bool
			for _, alternate := range alternates {
				if capabilityBlocked {
					break
				}
				candidateFile, candidateProvenance, err := h.prepareVirtualAlternateFileV3(r, alternate, profileID)
				if err != nil || candidateFile == nil {
					continue
				}
				// The sibling was authorized on its stored resolution, which
				// its first probe can fill in above the viewer ceiling: recheck
				// after the probe before planning it.
				if !catalog.FileAllowedByAccess(candidateFile, accessFilter) {
					continue
				}
				candidateReq := baseReq
				candidateAudioIndex := remapAudioIndexV3(alternateBase, candidateFile, baseAudioIndex)
				var candidateResult playback.PlannerResultV3
				var candidateToneMapErr error
				subtitleDropped, err := h.remapSubtitleSelectionV3(r.Context(), requestedFile, candidateFile, &candidateReq)
				if err != nil {
					candidateResult = playback.PlannerResultV3{Terminal: &playback.TerminalV3{Reason: terminalSubtitleUnavailableInVersionV3, Message: err.Error(), Retryable: false}}
					if errors.Is(err, errSubtitleUnavailableInTargetV3) && subtitleMissFile == nil {
						subtitleMissFile = candidateFile
						subtitleMissReq = candidateReq
						subtitleMissProvenance = candidateProvenance
						subtitleMissAudioIndex = candidateAudioIndex
					}
				} else {
					if err := preflightPlaybackFile(r.Context(), candidateFile, h.MissingMarker, h.EventsHub); err != nil {
						continue
					}
					candidateResult, candidateToneMapErr = h.planPlaybackWithCapabilitiesV3(r.Context(), playback.PlannerInputV3{Request: candidateReq, RequestedFile: requestedFile, EffectiveFile: candidateFile, ServerBitrateCapKbps: serverBitrateCapV3(r.Context()), AudioTrackIndex: candidateAudioIndex, Settings: settings, Registry: h.transformationRegistryV3(r.Context()), DVRPUStrippable: h.lazyDVRPUStrippableV3(r.Context(), candidateFile), Now: time.Now(), AdditionalSubtitles: subtitleInventoryFor(candidateFile), InventoryProvenance: string(candidateProvenance)})
					// A retryable tone-map discovery failure converts to
					// transcode_start_failed below; that verdict will not
					// change for a sibling file, so stop here.
					if candidateToneMapErr != nil && candidateResult.Terminal != nil &&
						candidateResult.Terminal.Reason == transcodeStartFailedReasonV3 {
						capabilityBlocked = true
					}
				}
				if candidateResult.Terminal == nil && subtitleDropped {
					// Prefer a version that keeps the viewer's subtitle; hold
					// this one back in case none does.
					if droppedSubtitle == nil {
						droppedSubtitle = &alternateCandidateV3{file: candidateFile, request: candidateReq, audioIndex: candidateAudioIndex, result: candidateResult, toneMapErr: candidateToneMapErr}
					}
					continue
				}
				clampPlannerTargetResolution(&candidateResult, candidateFile)
				if candidateResult.Terminal == nil {
					req = candidateReq
					effectiveFile = candidateFile
					audioIndex = candidateAudioIndex
					result = candidateResult
					toneMapCapabilityErr = candidateToneMapErr
					// The candidate is now the selected source, so the
					// provenance that reaches any replacement input (a degrade,
					// an escalation) must be its resolver verdict, not the
					// primary source's.
					resolutionProvenance = candidateProvenance
					break
				}
				if firstFailureFile == nil {
					firstFailureResult = candidateResult
					firstFailureToneMapErr = candidateToneMapErr
					firstFailureFile = candidateFile
					firstFailureReq = candidateReq
					firstFailureProvenance = candidateProvenance
					firstFailureAudioIndex = candidateAudioIndex
				}
			}
			if result.Terminal != nil && droppedSubtitle != nil {
				req = droppedSubtitle.request
				effectiveFile = droppedSubtitle.file
				audioIndex = droppedSubtitle.audioIndex
				result = droppedSubtitle.result
				toneMapCapabilityErr = droppedSubtitle.toneMapErr
				warnings = append(warnings, playback.SubtitleTrackUnavailableWarningV3())
			} else if result.Terminal != nil && firstFailureFile != nil {
				req = firstFailureReq
				effectiveFile = firstFailureFile
				audioIndex = firstFailureAudioIndex
				result = firstFailureResult
				toneMapCapabilityErr = firstFailureToneMapErr
				resolutionProvenance = firstFailureProvenance
			}
			if result.Terminal != nil && subtitleMissFile != nil {
				// Every alternate that could honor the subtitle selection has
				// been exhausted. Playability wins over fidelity: continue on
				// the first candidate that lacked the track with subtitles off
				// instead of terminalling the start.
				degradeReq := subtitleMissReq
				degradeReq.SubtitleTrackIndex = nil
				degradeReq.SubtitleTrackID = ""
				if preflightErr := preflightPlaybackFile(r.Context(), subtitleMissFile, h.MissingMarker, h.EventsHub); preflightErr == nil {
					degradeResult, degradeToneMapErr := h.planPlaybackWithCapabilitiesV3(r.Context(), playback.PlannerInputV3{
						Request: degradeReq, RequestedFile: requestedFile, EffectiveFile: subtitleMissFile,
						AudioTrackIndex: subtitleMissAudioIndex, Settings: settings,
						Registry: h.transformationRegistryV3(r.Context()), DVRPUStrippable: h.lazyDVRPUStrippableV3(r.Context(), subtitleMissFile), Now: time.Now(),
						AdditionalSubtitles: subtitleInventoryFor(subtitleMissFile),
						InventoryProvenance: string(subtitleMissProvenance),
					})
					clampPlannerTargetResolution(&degradeResult, subtitleMissFile)
					if degradeResult.Terminal == nil {
						slog.InfoContext(r.Context(), "subtitle unavailable in every alternate; continuing with subtitles off",
							logComponentKey, playbackLogValueV3, "alternate_file_id", subtitleMissFile.ID)
						req = degradeReq
						effectiveFile = subtitleMissFile
						audioIndex = subtitleMissAudioIndex
						result = degradeResult
						toneMapCapabilityErr = degradeToneMapErr
						// The held candidate is now the selected source; carry
						// its provenance so a later replacement input does not
						// fall back to the primary source's.
						resolutionProvenance = subtitleMissProvenance
						annotateSubtitleDroppedV3(&result)
					}
				}
			}
		}
	}
	result = retryIncompleteToneMapPlanningV3(result, toneMapCapabilityErr)
	result = retryIncompletePlaybackSettingsV3(result, settingsErr)
	h.clarifyOriginalQuality4KTerminalV3(r.Context(), requestAccessFilter(r), result.Terminal, requestedFile, !shouldTryAlternateFileV3(req.QualityPreference))
	hintExplicitSelectionAlternateAvailableV3(result.Terminal, req.FileSelection)
	// The exact app identity is logged with every decision so a route or
	// terminal reported against one build is attributable without asking the
	// user which version they are running.
	clientInfo := playbackClientInfoForStartV3(r, req.ClientPlaybackContext)
	if result.Terminal != nil {
		slog.InfoContext(r.Context(), "playback plan decided", append([]any{
			logComponentKey, playbackLogValueV3,
			requestIDLogKeyV3, chimw.GetReqID(r.Context()),
			"outcome", "terminal",
			"reason", result.Terminal.Reason,
			"detail", result.Terminal.Detail,
			"file_id", effectiveFile.ID,
			"quality_preference", req.QualityPreference,
		}, clientInfo.LogAttrs()...)...)
		response, persistErr := h.persistTerminalStartDecisionV3(r.Context(), userID, profileID, req, requestDigests, requestedFile.ID, effectiveFile.ID, playback.NewTerminalResponseFromTerminalV3(result.Terminal))
		if persistErr != nil {
			return playback.DecisionResponseV3{}, playbackPersistenceOperationError(persistErr)
		}
		if response.Terminal != nil {
			h.enqueueRouteEventV3(playback.RouteEventRecordV3{RouteEventV3: playback.RouteEventV3{ProtocolVersion: playback.ProtocolV3, PlaybackAttemptID: req.PlaybackAttemptID, Event: playback.RouteEventTerminalV3, FallbackReason: response.Terminal.Reason, OutputContextID: req.ClientPlaybackContext.Output.OutputContextID}, UserID: userID, ProfileID: profileID, ClientName: clientInfo.Name, ClientVersion: clientInfo.Version, ClientBuild: clientInfo.Build, ClientChannel: clientInfo.Channel, ClientModel: req.ClientPlaybackContext.Device.Model})
		}
		return response, nil
	}
	// A refused progressive remux is escalated before the decision is logged or
	// a session is opened, so the logged route is the one that will actually run.
	escalated, escalateErr := h.escalateRefusedProgressiveRemuxV3(r.Context(), headerAuthenticatedMediaV3(req.ClientFeatures),
		func() playback.PlannerInputV3 {
			input := h.plannerInputV3(r.Context(), req, requestedFile, effectiveFile, audioIndex, nil, resolutionProvenance)
			input.LowerVersion = lowerVersionForFileV3(lowerVersion, effectiveFile)
			return input
		}, result)
	if escalateErr != nil {
		persistedResponse, persistErr := h.startFailureDecisionV3(r.Context(), userID, profileID, req, requestDigests, requestedFile.ID, effectiveFile.ID, escalateErr)
		if persistErr != nil {
			return playback.DecisionResponseV3{}, playbackPersistenceOperationError(persistErr)
		}
		return persistedResponse, nil
	}
	result = escalated
	timings.mark("remux_escalation")
	appendStartWarningsV3(&result, warnings)
	if resolutionWasAssumed && result.Terminal == nil {
		result.Plan.DegradationWarnings = append(result.Plan.DegradationWarnings, playback.DegradationWarningV3{
			Code:    "resolution_assumed_1080p",
			Message: "Source resolution could not be verified on first touch; playback uses a baseline quality until probe evidence lands.",
		})
	}
	// session_transport_commit measures everything startPlannedPlaybackV3 does:
	// session creation, recipe/subtitle persistence (SaveAttempt is durable
	// before this mark), route registration, and the transport commit. Nothing
	// is deferred past it. For the locally-servable identity/direct and
	// progressive-remux shapes the client URL is /stream/<session>, whose
	// remux/transcode ffmpeg is started lazily by StreamHandler on the first
	// request, so no transport spawn or manifest wait is included here. The
	// mark only covers an eager local HLS/transcode startup for HLS deliveries.
	// The fresh-start resolve deferred its full audio/subtitle enumeration and
	// handed back the probe to run once the transport has committed. Mark the
	// plan provisional now, before the transport commit and before the attempt
	// record is written, so an idempotent replay of this attempt republishes the
	// same provisional truth instead of appearing fully probed.
	if resolved.DeferredProbe != nil && result.Plan != nil {
		result.Plan.TracksPending = true
	}
	// enqueueDeferredPostCommitProbe runs the fresh-start resolve's deferred
	// audio/subtitle enumeration once a transport has committed and the
	// first-byte URL is on the response. It is shared by the main start and the
	// same-release transient transport retry so a plan marked tracks_pending
	// always gets the follow-up it promises. The probe is admitted into the
	// bounded deferred-probe pool; a full pool or a saturated detached gate is
	// backpressure that leaves the session in the pending state, never a
	// synchronous probe on the response path.
	enqueueDeferredPostCommitProbe := func(sessionID string) {
		deferred := resolved.DeferredProbe
		if deferred == nil {
			return
		}
		deferred.sessionID = sessionID
		timings.mark("post_commit_probe_scheduled")
		h.enqueueDeferredVirtualProbeV3(r.Context(), deferred)
	}
	response, statusErr := h.startPlannedPlaybackV3(r, userID, profileID, req, requestDigests, requestedFile, effectiveFile, audioIndex, virtualDecision, result, clientInfo, virtualDecision.substitutionReason)
	timings.mark("session_transport_commit")
	if statusErr != nil {
		// A decoder-rejected source is a candidate failure, not a route failure:
		// rotate to the next provider release before any terminal. An explicit
		// pin is never substituted; it terminalls with the version-list hint.
		if statusErr.reason == candidateSourceDecodeRejectedReasonV3 && isVirtualPlaybackFile(requestedFile) {
			if req.FileSelection != playback.FileSelectionExplicitV3 {
				rejectedID := virtualResultCandidateID(effectiveFile.FilePath)
				if rejectedID == "" {
					rejectedID = virtualResultCandidateID(requestedFile.FilePath)
				}
				if rotated, ok := h.rotateRejectedVirtualCandidateStartV3(r, userID, profileID, req, requestDigests, catalogRequestedFile, settings, settingsErr, clientInfo, rejectedID); ok {
					return rotated, nil
				}
			}
			persisted, persistErr := h.persistTerminalStartDecisionV3(r.Context(), userID, profileID, req, requestDigests, requestedFile.ID, effectiveFile.ID, sourceDecodeFailedTerminalResponseV3(req.FileSelection))
			if persistErr != nil {
				return playback.DecisionResponseV3{}, playbackPersistenceOperationError(persistErr)
			}
			return persisted, nil
		}
		// A transport failure that names an audio condition (a node declining
		// the audio recipe, audio transcoding disabled) must not rotate the
		// release. The reason split already keeps those out of this branch; the
		// guard makes the audio exclusion explicit so a future caller cannot
		// fold an audio reason back into the video transport reason.
		if statusErr.reason == transcodeStartFailedReasonV3 && !audioOnlyTransportReasonV3(statusErr.reason) && isVirtualPlaybackFile(requestedFile) && req.FileSelection != playback.FileSelectionExplicitV3 {
			// A single transient provider error (an upstream 5xx or timeout on
			// the seek-anchor probe or the first bytes) must not change the
			// release. Retry the same release once with a fresh relay
			// registration and a short bounded backoff before considering a
			// sibling; only a second transient failure — or a hard signal —
			// walks the alternates below. The fresh marker mints a new relay
			// entry so the retry presents new bytes instead of replaying the
			// registration whose upstream just failed.
			if retriedResponse, retried := h.recoverVirtualTransportSameReleaseV3(r.Context(), playback.IsTransientProviderError(statusErr.cause), func(retryCtx context.Context) (playback.DecisionResponseV3, *transportErrorV3) {
				return h.startPlannedPlaybackV3(r.WithContext(retryCtx), userID, profileID, req, requestDigests, requestedFile, effectiveFile, audioIndex, virtualDecision, result, clientInfo)
			}); retried {
				// The retry committed a transport for the same deferred plan, so
				// run the enumeration it still promises before returning.
				enqueueDeferredPostCommitProbe(retriedResponse.SessionID)
				return retriedResponse, nil
			}
			alternateOrder := alternateOrderingForClient(req.Capabilities)
			// A transient provider (upstream 5xx) failure caps the alternates
			// tried in this start so the failing upstream is not hammered once
			// per alternate; a generic transport failure keeps the full list.
			if alternates, alternateErr := h.virtualTransportAlternatesV3(r.Context(), requestedFile, requestAccessFilter(r), alternateOrder, playback.IsTransientProviderError(statusErr.cause)); alternateErr == nil && len(alternates) > 0 {
				for alternateRank, altCandidate := range alternates {
					alternate, alternateProvenance, err := h.prepareVirtualAlternateFileV3(r, altCandidate, profileID)
					if err != nil || alternate == nil {
						continue
					}
					alternateAudio := remapAudioIndexV3(effectiveFile, alternate, audioIndex)
					alternateRequest := req
					_, subtitleErr := h.remapSubtitleSelectionV3(r.Context(), effectiveFile, alternate, &alternateRequest)
					if errors.Is(subtitleErr, errSubtitleUnavailableInTargetV3) {
						// The alternate has no equivalent track; degrade rather
						// than give up on a playable route.
						alternateRequest.SubtitleTrackIndex = nil
						alternateRequest.SubtitleTrackID = ""
						subtitleErr = nil
					}
					if subtitleErr == nil {
						alternateResult, _ := h.planPlaybackWithCapabilitiesV3(r.Context(), playback.PlannerInputV3{
							Request: alternateRequest, RequestedFile: requestedFile, EffectiveFile: alternate,
							AudioTrackIndex: alternateAudio, Settings: settings,
							Registry:        h.transformationRegistryV3(r.Context()),
							DVRPUStrippable: h.lazyDVRPUStrippableV3(r.Context(), alternate), Now: time.Now(),
							AdditionalSubtitles: subtitleInventoryFor(alternate),
							InventoryProvenance: string(alternateProvenance),
						})
						clampPlannerTargetResolution(&alternateResult, alternate)
						if alternateResult.Terminal == nil {
							slog.WarnContext(r.Context(), "virtual playback transport failed; retrying compatible alternate", "component", "playback", "requested_file_id", requestedFile.ID, "alternate_file_id", alternate.ID, "alternate_order_4k_first", alternateOrder.Prefer4K, "error", statusErr.cause)
							alternateDecision := virtualDecision
							alternateDecision.candidateRank = alternateRank
							alternateDecision.candidateCount = len(alternates)
							// A transport failure moved the release: name the
							// requested row and the cause on the plan.
							alternateDecision.substitutionReason = substitutionReasonTransportFailedV3
							if alternateResponse, alternateStatusErr := h.startPlannedPlaybackV3(r, userID, profileID, alternateRequest, requestDigests, requestedFile, alternate, alternateAudio, alternateDecision, alternateResult, clientInfo, alternateDecision.substitutionReason); alternateStatusErr == nil {
								return alternateResponse, nil
							}
						}
					}
				}
			}
		}
		if statusErr.reason == "playback_attempt_reused" {
			return playback.DecisionResponseV3{}, playbackOperationError(http.StatusConflict, "playback_attempt_reused", statusErr.message)
		}
		failureAttrs := []any{
			logComponentKey, playbackLogValueV3,
			"reason", statusErr.reason,
			"retryable", statusErr.retryable,
			"playback_attempt_id", req.PlaybackAttemptID,
			"requested_file_id", requestedFile.ID,
			"effective_file_id", effectiveFile.ID,
		}
		if statusErr.cause != nil {
			failureAttrs = append(failureAttrs, "error", statusErr.cause)
		}
		// The error text is sanitized centrally by the opslog sink; keep the
		// structured cause here so the failure line stays diagnosable.
		switch {
		case statusErr.reason == policyErrorInternal:
			slog.ErrorContext(r.Context(), "protocol v3 planned playback failed", failureAttrs...)
		case statusErr.cause != nil:
			slog.WarnContext(r.Context(), "protocol v3 planned playback failed", failureAttrs...)
		default:
			slog.InfoContext(r.Context(), "protocol v3 planned playback failed", failureAttrs...)
		}
		persistedResponse, persistErr := h.startFailureDecisionV3(r.Context(), userID, profileID, req, requestDigests, requestedFile.ID, effectiveFile.ID, statusErr)
		if persistErr != nil {
			return playback.DecisionResponseV3{}, playbackPersistenceOperationError(persistErr)
		}
		return persistedResponse, nil
	}
	// The transport has committed and the first-byte URL is on the response.
	// The fresh-start resolve deferred its full audio/subtitle enumeration, so
	// schedule it now: the client can start streaming immediately while ffprobe
	// runs, and the existing inventory_updated push (or the inventory poll)
	// delivers the probed track menu as a follow-up. The plan already carries
	// tracks_pending so a client keeps the provisional menu until then.
	enqueueDeferredPostCommitProbe(response.SessionID)
	timings.mark("response_ready")
	return response, nil
}

// virtualStartVersionFallbackWorkers bounds how many alternate versions the
// cold-start version-fallback walk resolves at once. Each alternate resolve is
// dominated by one upstream listing, so fan-out turns a walk of N dead pins
// from N serial listing rounds into roughly one. Four is small enough that a
// provider outage is not amplified into a storm of concurrent listings.
//
// The fan-out is a sliding wave, not a single round: a worker that finishes a
// candidate takes the next index immediately, so only the first
// virtualStartVersionFallbackWorkers candidates share the per-listing budget
// from the start. A candidate in a later wave inherits whatever remains of the
// decision budget, so four listings that each burn the full 1.5s
// virtualStartVersionFallbackListingBudget leave a fifth candidate at most
// ~0.5s before the 2s virtualStartVersionFallbackDecisionBudget terminals the
// walk. That is the deliberate tradeoff: a slow early wave is allowed to
// shorten a later candidate's window rather than let the walk stretch toward
// one full listing budget per candidate. It is pinned by
// TestRunVirtualVersionFallbackCandidatesLaterWaveSharesDecisionBudget.
const virtualStartVersionFallbackWorkers = 4

// virtualStartVersionFallbackListingBudget bounds one alternate's resolve in
// the walk. A dead pin (empty listing, provider outage, absent candidate)
// answers or times out inside this window, so a walk never pays a full startup
// budget per dead version. It is a var so tests can pin it.
var virtualStartVersionFallbackListingBudget = 1500 * time.Millisecond

// virtualStartVersionFallbackDecisionBudget bounds the walk as a whole: the
// first healthy candidate wins; if none does, the terminal is returned once
// every candidate has failed or this budget elapses, whichever is first. It is
// larger than one listing budget so a single fan-out wave can finish, and far
// below the cold-start budget so the walk cannot itself burn the 7-11s window
// the parallel walk exists to remove.
var virtualStartVersionFallbackDecisionBudget = 2 * time.Second

// runVirtualVersionFallbackCandidatesV3 resolves alternate version candidates
// with bounded parallel fan-out and returns the first healthy candidate. It is
// the scheduling half of the cold-start version-fallback walk, kept separate
// from the provider calls so the fan-out (worker bound, per-listing budget,
// first-wins, cancel stragglers, all-failed terminal) is testable without a
// provider.
//
// candidates is in fallback order. Each is resolved by resolve within a
// per-listing budget derived from ctx. The first success (ok=true) wins: the
// remaining listings are canceled and this returns at once, without waiting for
// them to unwind, so a healthy candidate is never delayed behind a straggler.
// false is returned only after every candidate has failed or the decision
// context elapses, whichever happens first; on both of those paths the in-flight
// listings are canceled and the workers are joined before returning, so no
// resolve mutates handler state after a terminal verdict. The caller keeps the
// original resolve error in the false case.
//
// The winner is returned with the index of the candidate that produced it, so
// the caller never has to correlate the returned source with a side-channel
// write made by the winning worker. Two workers can both succeed; whichever
// publishes first wins the buffered channel, and its own index travels with it.
// On a false result (or no publish) index is -1.
func runVirtualVersionFallbackCandidatesV3(
	decisionCtx context.Context,
	candidates []*models.MediaFile,
	workers int,
	listingBudget time.Duration,
	resolve func(context.Context, *models.MediaFile) (resolvedVirtualPlaybackSource, bool),
) (resolvedVirtualPlaybackSource, int, bool) {
	if len(candidates) == 0 || workers <= 0 {
		return resolvedVirtualPlaybackSource{}, -1, false
	}
	if workers > len(candidates) {
		workers = len(candidates)
	}
	ctx, cancel := context.WithCancel(decisionCtx)
	defer cancel()

	// virtualVersionFallbackResultV3 pairs a resolved source with the index of
	// the candidate that produced it, so the identity of the winning alternate
	// is published atomically with its source rather than through a shared
	// write a competing success could overwrite before the caller reads it.
	type virtualVersionFallbackResultV3 struct {
		source resolvedVirtualPlaybackSource
		index  int
	}

	indexes := make(chan int)
	winnerCh := make(chan virtualVersionFallbackResultV3, 1)

	var workersWG sync.WaitGroup
	workersWG.Add(workers)
	for range workers {
		go func() {
			defer workersWG.Done()
			for {
				var idx int
				select {
				case <-ctx.Done():
					return
				case i, ok := <-indexes:
					if !ok {
						return
					}
					idx = i
				}
				// A winner or the decision budget already canceled the walk;
				// do not start another provider listing.
				if ctx.Err() != nil {
					return
				}
				listCtx, listCancel := context.WithTimeout(ctx, listingBudget)
				resolved, ok := resolve(listCtx, candidates[idx])
				listCancel()
				if ok {
					select {
					case winnerCh <- virtualVersionFallbackResultV3{source: resolved, index: idx}:
					default:
					}
					cancel()
					return
				}
			}
		}()
	}

	go func() {
		defer close(indexes)
		for i := range candidates {
			select {
			case <-ctx.Done():
				return
			case indexes <- i:
			}
		}
	}()

	drained := make(chan struct{})
	go func() {
		workersWG.Wait()
		close(drained)
	}()

	// preferWinner drains a result that may have landed exactly as the final
	// worker exited, so a success is never dropped in favor of the terminal.
	preferWinner := func() (resolvedVirtualPlaybackSource, int, bool) {
		select {
		case winner := <-winnerCh:
			return winner.source, winner.index, true
		default:
			return resolvedVirtualPlaybackSource{}, -1, false
		}
	}

	select {
	case winner := <-winnerCh:
		return winner.source, winner.index, true
	case <-drained:
		return preferWinner()
	case <-decisionCtx.Done():
		// The walk's decision budget elapsed. Cancel the in-flight listings and
		// wait for the workers to stop, so no resolve mutates handler state
		// after this returns. A candidate that finished just as the budget fired
		// still counts as the winner.
		cancel()
		<-drained
		return preferWinner()
	}
}

// resolveVirtualStartWithVersionFallback resolves the virtual source for a
// fresh start. When the pinned release's listing fails and nothing is playing
// yet, it walks the content's alternate versions/files — each with its own
// listing attempt — and returns the first working candidate, so pressing play
// lands on a playable version whenever one exists.
//
// The walk only runs for an eligible auto selection (the same gate the
// planning-time alternate hunt uses) and never for an explicit user version
// pick: the viewer chose that release and must not be silently moved off it.
// A version the catalog already stamped failed (an AltMount SourceFailed
// verdict) is skipped, never attempted. An empty provider listing stays
// transient: it advances the walk without indicting the version, matching the
// deliberate decision that a zero-count answer is a provider hiccup and not a
// verdict about any release.
//
// The alternates are resolved with bounded parallel fan-out rather than one at
// a time: a cold start whose pin is dead commonly has every alternative dead
// too, and resolving them serially paid one full upstream listing timeout per
// dead version before the terminal. Each listing is bounded to
// virtualStartVersionFallbackListingBudget and the walk as a whole to
// virtualStartVersionFallbackDecisionBudget, so the walk itself terminals in
// about two seconds instead of seven to eleven. That two-second figure
// describes the alternate walk, not the end-to-end start request: when the walk
// finds no candidate, the caller's synchronous primary stamp
// (stampStartVirtualCandidateFailed) runs after it and can add up to
// startCandidateFailStampBudget (~3s) of database write before the terminal
// response. The first healthy candidate wins and cancels the rest. When no
// version resolves, the original listing failure is returned so the caller
// reports the honest underlying cause (an edge 5xx, an empty listing, or every
// version failed) instead of a generic error.
func (h *PlaybackHandler) resolveVirtualStartWithVersionFallback(
	r *http.Request,
	file *models.MediaFile,
	profileID string,
	req playback.StartRequestV3,
	allowFailedCandidate bool,
	bandwidthCapKbps int,
) (resolvedVirtualPlaybackSource, error) {
	resolved, resolveErr := h.resolveVirtualPlaybackSource(
		r, file, profileID, true, nil, "", req.QualityPreference, bandwidthCapKbps, req.ForceRelink,
		virtualResolveOptionsV3{
			allowFailedCandidate: allowFailedCandidate,
			sessionBound:         false,
			explicitSelection:    req.FileSelection == playback.FileSelectionExplicitV3,
			// Defer the candidate-declared upgrade probe past the transport
			// commit only when the surface AND the client negotiated the
			// deferred track-inventory lifecycle (see
			// deferTrackInventoryNegotiatedV3). On the frozen v1 surface, or a
			// v2 client that did not advertise deferred_track_inventory_v1, the
			// resolve keeps the pre-#228 behavior: it upgrades the inventory with
			// a background probe and never marks the plan tracks_pending, so the
			// v1 contract is byte-for-byte unchanged.
			deferProbePastCommit: h.deferTrackInventoryNegotiatedV3(r, req),
		},
	)
	if resolveErr == nil || !virtualStartVersionFallbackEligibleV3(req) || !virtualProviderListingOutage(resolveErr) {
		return resolved, resolveErr
	}
	// One deadline owns the whole walk. resolveVirtualPlaybackSource re-bases
	// its own cold path on r.Context() and context.WithTimeout keeps the
	// earlier deadline, so this bounds every alternate without restarting the
	// budget once per version. The decision budget is deliberately separate
	// from the per-listing budget: a healthy candidate is returned the moment
	// it resolves, while a walk of dead pins terminals once the decision budget
	// elapses rather than waiting out the full cold-start budget per version.
	walkCtx, cancel := context.WithTimeout(r.Context(), virtualStartVersionFallbackDecisionBudget)
	defer cancel()
	// Thread the caller's session-binding intent rather than forcing it false.
	// The fresh-start start path declares unbound on the request context before
	// calling this walk, so the stale-pin recovery may re-pin; a future
	// session-bound caller that reuses the walk keeps its declaration (or the
	// conservative session-bound default) and the recovery refuses to touch a
	// live session's binding.
	walkReq := r.WithContext(withVirtualSessionBindingV3(walkCtx, VirtualSessionBinding(r.Context())))

	// Guarded stale-pin recovery for identity-less rows: when the pin id is
	// absent from a non-empty live listing and the row carries no durable
	// provider identity to rematch on, re-pin in memory to the best
	// fingerprint-matched live candidate and retry once. Bounded to one attempt
	// inside its own sub-budget; a miss falls through to the alternate walk
	// below with the terminal intact.
	if recovered, recoveredOK := h.recoverStaleIdentityLessPinV3(walkReq, file, profileID); recoveredOK {
		return recovered, nil
	}

	alternates, alternateErr := h.findAlternateFilesOrdered(walkCtx, file, requestAccessFilter(r), alternateOrderingForClient(req.Capabilities))
	if alternateErr != nil || len(alternates) == 0 {
		return resolved, resolveErr
	}

	// candidateRows keeps one entry per walkable alternate, in fallback order.
	// Skipping an AltMount-failed version here (rather than inside the resolve
	// closure) keeps the walk order and the fan-out deterministic: a skipped
	// version is never handed a worker slot.
	candidateRows := make([]*models.MediaFile, 0, len(alternates))
	for _, alternate := range alternates {
		if alternate == nil || alternate.ID == file.ID || !isVirtualPlaybackFile(alternate) {
			continue
		}
		if virtualCandidateVerdictActive(alternate.FailedAt, time.Now()) {
			// The provider's AltMount verdict indicted this version; never
			// attempt it, however the primary listing failed.
			continue
		}
		candidateRows = append(candidateRows, alternate)
	}
	if len(candidateRows) == 0 {
		return resolved, resolveErr
	}

	// resolveAlternate runs one alternate's existing resolve. It is exactly the
	// call the serial walk made, so its side effects (pinning, caching) and its
	// verdict classification are unchanged. A confirmed-dead verdict is
	// persisted off this worker's critical path by
	// stampStartVirtualCandidateFailedAsync: the stamp detaches from listCtx and
	// carries its own budget, so the runner joining a worker that is mid-stamp
	// cannot stretch the walk past its decision budget. The candidate that won
	// is published together with its index on the runner's channel, never via a
	// shared variable, so two concurrent successes cannot race for it and a
	// straggler cannot overwrite the winner the caller reads.
	resolveAlternate := func(listCtx context.Context, alternate *models.MediaFile) (resolvedVirtualPlaybackSource, bool) {
		listReq := walkReq.WithContext(listCtx)
		altResolved, altErr := h.resolveVirtualPlaybackSource(
			listReq, alternate, profileID, true, nil, "", req.QualityPreference, bandwidthCapKbps, false,
			virtualResolveOptionsV3{sessionBound: false, bypassProviderFloor: true},
		)
		if altErr == nil && altResolved.File != nil {
			return altResolved, true
		}
		if altErr != nil {
			// A confirmed-dead version is indicted so a later start skips it;
			// an empty listing or provider outage is transient and only
			// advances the walk. Fire-and-forget: the stamp writes through a
			// detached budgeted context, so a canceled straggler still records
			// the verdict it earned without holding the walk open for it.
			h.stampStartVirtualCandidateFailedAsync(listCtx, alternate, altErr)
		}
		return resolvedVirtualPlaybackSource{}, false
	}

	winner, winnerIndex, ok := runVirtualVersionFallbackCandidatesV3(
		walkCtx, candidateRows, virtualStartVersionFallbackWorkers, virtualStartVersionFallbackListingBudget, resolveAlternate,
	)
	if !ok {
		return resolved, resolveErr
	}
	// Mark the substitution so the plan can tell the client the requested
	// release was replaced, and why. The requested row is the one the caller
	// asked for, which this walk deliberately walked away from because its
	// listing failed.
	winner.SubstitutedFromFileID = file.ID
	winner.SubstitutionReason = virtualSubstitutionReasonV3(resolveErr)
	// The winner resolved and ranked through the normal path, so it carries
	// its own measured rank/count; the primary's unknown or zero values must
	// not leak onto the substituted plan's log line.
	logAttrs := []any{
		logComponentKey, playbackLogValueV3,
		"requested_file_id", file.ID,
		"candidate_id", virtualResultCandidateID(winner.URI),
	}
	if winnerIndex >= 0 && winnerIndex < len(candidateRows) {
		logAttrs = append(logAttrs, "alternate_file_id", candidateRows[winnerIndex].ID)
	}
	slog.InfoContext(walkCtx, "virtual start fell back to an alternate version after a listing failure", logAttrs...)
	return winner, nil
}

// virtualStartVersionFallbackEligibleV3 is the cross-version fallback gate,
// mirroring the planning-time alternate hunt: the client must allow alternate
// versions, the quality preference must not pin the exact release, and the
// request must not be an explicit version pick the viewer chose.
func virtualStartVersionFallbackEligibleV3(req playback.StartRequestV3) bool {
	return req.AllowsAlternateVersions() &&
		shouldTryAlternateFileV3(req.QualityPreference) &&
		req.FileSelection != playback.FileSelectionExplicitV3
}

// prepareVirtualAlternateFileV3 resolves an alternate candidate row into the
// file a plan will serve. It returns the resolver's provenance alongside the
// file so each alternate planner input can publish the same inventory
// provenance the primary plan carries; a local alternate has no resolver and
// reports empty provenance.
func (h *PlaybackHandler) prepareVirtualAlternateFileV3(r *http.Request, alternate *models.MediaFile, profileID string) (*models.MediaFile, ProbeProvenance, error) {
	if alternate == nil {
		return nil, "", errors.New("nil alternate file")
	}
	if !isVirtualPlaybackFile(alternate) {
		return h.ensurePlaybackProbe(r.Context(), alternate), "", nil
	}
	resolved, err := h.resolveVirtualPlaybackSource(r, alternate, profileID, false, nil, "", "", 0, false, virtualResolveOptionsV3{sessionBound: false, bypassProviderFloor: true})
	if err != nil {
		return nil, "", err
	}
	if resolved.File == nil {
		return nil, "", errors.New("virtual playback resolver returned no file")
	}
	resolvedFile := *resolved.File
	// Keep the candidate's own catalog row id when the resolver substituted a
	// different release that has a row; only a same-row resolve carries the
	// alternate row id. Overwriting it with alternate.ID hid the substitution.
	resolved.File = &resolvedFile
	resolved.File.FilePath = resolved.URI
	resolved.File.VirtualOwnerInstallationID = resolved.OwnerID
	if resolved.File.Duration <= 0 {
		if resolved.File.EpisodeID != "" && h.EpisodeLookup != nil {
			if ep, err := h.EpisodeLookup.GetByID(r.Context(), resolved.File.EpisodeID); err == nil && ep != nil && ep.Runtime > 0 {
				resolved.File.Duration = ep.Runtime * 60
			}
		} else if resolved.File.ContentID != "" && h.ItemLookup != nil {
			if item, err := h.ItemLookup.GetByID(r.Context(), resolved.File.ContentID); err == nil && item != nil && item.Runtime > 0 {
				resolved.File.Duration = item.Runtime * 60
			}
		}
	}
	return resolved.File, resolved.Provenance, nil
}

// sourceDecodeFailedTerminalResponseV3 builds the durable terminal for a
// decode rejection that could not be recovered. The explicit-pick version-list
// hint is appended here because an explicit pin is never substituted.
func sourceDecodeFailedTerminalResponseV3(fileSelection playback.FileSelectionV3) playback.DecisionResponseV3 {
	terminal := &playback.TerminalV3{
		Reason:    sourceDecodeFailedReasonV3,
		Message:   sourceDecodeFailedMessageV3,
		Retryable: false,
	}
	hintExplicitSelectionAlternateAvailableV3(terminal, fileSelection)
	return playback.NewTerminalResponseFromTerminalV3(terminal)
}

// rotateRejectedVirtualCandidateStartV3 retries a virtual start whose local
// transport the decoder rejected, excluding each rejected provider result id so
// the provider offers the next-ranked release. It is bounded by
// maxVirtualFailoverAttempts; excluding the failed id on every iteration is what
// guarantees termination even when a provider keeps returning the dead release.
//
// It rotates on the provider result id rather than the catalog failed_at stamp,
// so rotation does not depend on the asynchronous mark having landed. It returns
// ok=false when no replacement candidate starts, leaving the caller to persist
// the source_decode_failed terminal.
func (h *PlaybackHandler) rotateRejectedVirtualCandidateStartV3(
	r *http.Request,
	userID int,
	profileID string,
	req playback.StartRequestV3,
	requestDigests playbackStartRequestDigestsV3,
	catalogFile *models.MediaFile,
	settings playback.PlannerSettingsV3,
	settingsErr error,
	clientInfo playback.ClientInfo,
	firstRejectedID string,
) (playback.DecisionResponseV3, bool) {
	if catalogFile == nil || firstRejectedID == "" {
		return playback.DecisionResponseV3{}, false
	}
	maxAttempts := h.maxVirtualFailoverAttempts(r.Context())
	excluded := []string{firstRejectedID}
	for attempt := 1; attempt < maxAttempts; attempt++ {
		// An auto selection: allowFailedCandidate=false keeps the catalog
		// failed_at rule, and the explicit exclusion carries the live verdict.
		resolved, resolveErr := h.resolveVirtualPlaybackSource(r, catalogFile, profileID, true, excluded, "", req.QualityPreference, intOrZeroHandlerV3(req.BandwidthCapKbps), req.ForceRelink, virtualResolveOptionsV3{rotateCandidates: true, sessionBound: false})
		if resolveErr != nil || resolved.File == nil {
			return playback.DecisionResponseV3{}, false
		}
		nextID := virtualResultCandidateID(resolved.URI)
		if nextID != "" && containsStringExactV3(excluded, nextID) {
			// The resolver handed back an already-excluded candidate; stop
			// rather than spin on the same release.
			return playback.DecisionResponseV3{}, false
		}
		resolvedFile := *resolved.File
		// Keep the rotated candidate's own catalog row id so the replacement
		// plan names the bytes it will play; the request keeps catalogFile as
		// its requested row below.
		resolvedFile.FilePath = resolved.URI
		resolvedFile.VirtualOwnerInstallationID = resolved.OwnerID
		audioIndex, _, audioErr := resolveV3AudioIndex(&resolvedFile, req.AudioTrackID, req.AudioTrackIndex)
		if audioErr != nil {
			return playback.DecisionResponseV3{}, false
		}
		planResult, toneMapCapabilityErr := h.planPlaybackWithCapabilitiesV3(r.Context(), playback.PlannerInputV3{
			Request: req, RequestedFile: catalogFile, EffectiveFile: &resolvedFile,
			AudioTrackIndex: audioIndex, Settings: settings,
			Registry:            h.transformationRegistryV3(r.Context()),
			DVRPUStrippable:     h.lazyDVRPUStrippableV3(r.Context(), &resolvedFile),
			Now:                 time.Now(),
			InventoryProvenance: string(resolved.Provenance),
		})
		planResult = retryIncompleteToneMapPlanningV3(planResult, toneMapCapabilityErr)
		planResult = retryIncompletePlaybackSettingsV3(planResult, settingsErr)
		clampPlannerTargetResolution(&planResult, &resolvedFile)
		if planResult.Terminal != nil || planResult.Plan == nil {
			return playback.DecisionResponseV3{}, false
		}
		response, statusErr := h.startPlannedPlaybackV3(r, userID, profileID, req, requestDigests, catalogFile, &resolvedFile, audioIndex, virtualPlanDecisionV3{candidateRank: resolved.CandidateRank, candidateCount: resolved.CandidateCount, substitutionReason: substitutionReasonDecodeRejectedV3, startAudioSelectionOrigin: virtualPlanSelectionOriginForRequestV3(req)}, planResult, clientInfo, substitutionReasonDecodeRejectedV3)
		if statusErr == nil {
			// The start committed a replacement candidate after excluding the
			// rejected ones. Persist the chain on the new attempt so a later
			// replan — on this replica or another after a reload — skips every
			// release this start already proved bad, even if the asynchronous
			// failed_at marker never landed.
			h.persistStartRotationExclusionsV3(r.Context(), response.SessionID, resolved.URI, excluded)
			return response, true
		}
		if statusErr.reason != candidateSourceDecodeRejectedReasonV3 || nextID == "" {
			return playback.DecisionResponseV3{}, false
		}
		excluded = append(excluded, nextID)
	}
	return playback.DecisionResponseV3{}, false
}

// persistStartRotationExclusionsV3 records the candidates a start rotation
// excluded onto the freshly committed attempt. It is best-effort only in the
// sense that the start has already succeeded and returned a playable plan;
// the exclusion is insurance for the next hop, so a failure here is logged and
// does not fail the start (the marker remains a supplementary suppression).
func (h *PlaybackHandler) persistStartRotationExclusionsV3(ctx context.Context, sessionID, candidateURI string, excludedIDs []string) {
	if sessionID == "" || len(excludedIDs) == 0 {
		return
	}
	store := h.recoveryStateStoreV3()
	if store == nil {
		slog.WarnContext(ctx, "start rotation exclusions could not be persisted: no recovery state store", "component", "api", "session_id", sessionID)
		return
	}
	record, err := h.PlanStoreV3.GetAttempt(ctx, sessionID)
	if err != nil || record == nil {
		slog.WarnContext(ctx, "start rotation exclusions could not load the committed attempt", "component", "api", "session_id", sessionID, "error", err)
		return
	}
	providerSource := virtualAttemptProviderSourceV3(candidateURI)
	if providerSource == "" {
		providerSource = virtualAttemptProviderSourceV3(record.CurrentPlan.EffectiveVirtualURI)
	}
	exclusions := make([]playback.RecoveryExclusionV3, 0, len(excludedIDs))
	for _, id := range excludedIDs {
		if strings.TrimSpace(id) == "" {
			continue
		}
		exclusions = append(exclusions, playback.RecoveryExclusionV3{
			ProviderSource: providerSource,
			CandidateID:    id,
			FileID:         record.EffectiveMediaFileID,
			ConfirmedAt:    time.Now().UTC(),
		})
	}
	if len(exclusions) == 0 {
		return
	}
	if _, err := h.appendRecoveryExclusionsV3(ctx, record, exclusions); err != nil {
		slog.WarnContext(ctx, "start rotation exclusions could not be persisted", "component", "api", "session_id", sessionID, "error", err)
	}
}

type playbackStartRequestDigestsV3 struct {
	current string
	legacy  string
}

// playbackClientInfoForStartV3 resolves the client's app identity for a v3
// start request. The X-Silo-Client-* headers win because they are present on
// every request; the start body's client_playback_context is the fallback for
// clients that report their app identity only there. All values stay opaque —
// they are trimmed and length-clamped when the session stamps them.
//
// The fallback applies only to a client that named itself. client_playback_context
// carries no app name, so nothing in the body can identify a nameless client
// anyway — it is labeled from its user agent, and its app_version is a
// free-form platform string rather than the marketing version client_version
// promises. The web player, for one, reports the literal "web" there; taking it
// unconditionally would write "web" into the one field that is contractually
// semver, on every browser session.
func playbackClientInfoForStartV3(r *http.Request, clientContext playback.ClientPlaybackContextV3) playback.ClientInfo {
	info := playbackClientInfoFromRequest(r)
	if info.Name == "" {
		return info
	}
	if info.Version == "" {
		info.Version = strings.TrimSpace(clientContext.AppVersion)
	}
	if info.Build == "" {
		info.Build = strings.TrimSpace(clientContext.AppBuild)
	}
	if info.Channel == "" {
		info.Channel = strings.TrimSpace(clientContext.AppChannel)
	}
	// The header half is already normalized; body-sourced values have to be
	// clamped too before they reach the decision log and the route event.
	return info.Normalized()
}

// playbackClientInfoWithSessionFallbackV3 completes a header-derived identity
// from the session the event belongs to. Route events posted out of band carry
// no client_playback_context, so without this a client that reports its build
// only in the start body would attribute its plan_selected event to a build and
// every later event of the same attempt to none.
func (h *PlaybackHandler) playbackClientInfoWithSessionFallbackV3(sessionID string, info playback.ClientInfo) playback.ClientInfo {
	if sessionID == "" || h.sessionMgr == nil {
		return info
	}
	if info.Name != "" && info.Version != "" && info.Build != "" && info.Channel != "" {
		return info
	}
	session, err := h.sessionMgr.GetSession(sessionID)
	if err != nil || session == nil {
		return info
	}
	stamped := session.ClientInfo()
	if info.Name == "" {
		info.Name = stamped.Name
	}
	if info.Version == "" {
		info.Version = stamped.Version
	}
	if info.Build == "" {
		info.Build = stamped.Build
	}
	if info.Channel == "" {
		info.Channel = stamped.Channel
	}
	return info
}

// newPlaybackStartRequestDigestsV3 fingerprints both the body and normalized
// device identity because either can change the selected playback plan. It
// also retains the pre-device digest while attempts written by an older
// server can still be replayed during a rolling deployment.
func newPlaybackStartRequestDigestsV3(body []byte, deviceID string) playbackStartRequestDigestsV3 {
	hasher := sha256.New()
	_, _ = fmt.Fprintf(hasher, "%d:", len(body))
	_, _ = hasher.Write(body)
	_, _ = hasher.Write([]byte(deviceID))
	legacy := sha256.Sum256(body)
	return playbackStartRequestDigestsV3{
		current: hex.EncodeToString(hasher.Sum(nil)),
		legacy:  hex.EncodeToString(legacy[:]),
	}
}

func (d playbackStartRequestDigestsV3) matches(stored string) bool {
	return stored == "" || stored == d.current || stored == d.legacy
}

func appendStartWarningsV3(result *playback.PlannerResultV3, warnings []playback.DegradationWarningV3) {
	if result == nil || len(warnings) == 0 || result.Plan == nil {
		return
	}
	for _, warning := range warnings {
		if slices.Contains(result.Plan.DegradationWarnings, warning) {
			continue
		}
		result.Plan.DegradationWarnings = append(result.Plan.DegradationWarnings, warning)
	}
}

// startPlannedPlaybackV3 creates a session and transport for an accepted plan.
// virtualPlanDecisionV3 carries the virtual-source decision facts the plan
// log names: the resolved playback.audio_language that steered audio
// selection, and the selected candidate's rank in the ranked list the resolver
// considered. The zero value means "not a ranked virtual resolve"; the log
// omits the rank fields then rather than printing a misleading 0.
type virtualPlanDecisionV3 struct {
	preferredAudioLanguage string
	candidateRank          int
	// candidateCount is the ranked candidate list's length, or -1 when the
	// resolve took a path that never ranked (a fast path with no list). The
	// plan-decision log distinguishes that unknown from a measured zero.
	candidateCount int
	// substitutionReason carries the alternate-version or rotation walk's
	// substitution cause to the plan. Empty for a same-release resolve.
	substitutionReason string
	// startAudioSelectionOrigin records whether the committed audio
	// selection was omitted ("auto") or explicitly picked ("explicit").
	// Captured where preferredAudioTrackIndexV3 returns, before defaults
	// erase the distinction, and consumed by intentForCommittedStartV3.
	startAudioSelectionOrigin string
}

// selectedAudioTrackLogFieldsV3 names the audio track a plan will play: its
// language, codec and selection ordinal, read from the plan's authoritative
// audio inventory. An out-of-range or absent inventory yields empty strings
// and the passed-in ordinal, so a missing track reads as unset rather than
// as a bogus language.
func selectedAudioTrackLogFieldsV3(result playback.PlannerResultV3, audioIndex int) (language, codec string, ordinal int) {
	if result.Plan == nil {
		return "", "", audioIndex
	}
	ordinal = audioIndex
	if selected := result.Plan.SelectedTracks.Audio; selected != nil && selected.Index != nil {
		ordinal = *selected.Index
	}
	if ordinal < 0 || ordinal >= len(result.Plan.AudioTracks) {
		return "", "", ordinal
	}
	track := result.Plan.AudioTracks[ordinal]
	language = strings.TrimSpace(track.Language)
	if language == "" && len(track.Languages) > 0 {
		language = strings.Join(track.Languages, "/")
	}
	return language, strings.TrimSpace(track.Codec), ordinal
}

// virtualPlanDecisionAttrsV3 is the virtual-source part of the plan-decision
// line: the selected candidate, its rank in the considered list, the audio
// track the plan will play, and the resolved playback.audio_language that
// steered it. A non-virtual plan yields no attributes. The count and rank
// are omitted when the count is unknown (negative), and the rank is omitted
// whenever it does not name a real position in the counted list (including
// a measured zero), so a missing value is never printed as a misleading
// zero. Absent preferences render as empty strings for the same reason.
func virtualPlanDecisionAttrsV3(result playback.PlannerResultV3, audioIndex int, decision virtualPlanDecisionV3) []any {
	uri := ""
	if result.Plan != nil {
		uri = result.Plan.EffectiveVirtualURI
	}
	if uri == "" {
		return nil
	}
	audioLanguage, audioCodec, audioOrdinal := selectedAudioTrackLogFieldsV3(result, audioIndex)
	attrs := []any{
		"candidate_uri", uri,
		"preferred_audio_language", decision.preferredAudioLanguage,
		"audio_track_language", audioLanguage,
		"audio_track_codec", audioCodec,
		"audio_track_index", audioOrdinal,
	}
	// candidate_count -1 means the resolve never ranked, so the count is
	// unknown and both fields are omitted. A measured count (including zero
	// from an empty ranked list, or defensively from any path that never set
	// the field) prints the count; the rank prints only when it names a real
	// position inside that list, so rank 0 into an empty list never renders.
	if decision.candidateCount < 0 {
		return attrs
	}
	attrs = append(attrs, "candidate_count", decision.candidateCount)
	if decision.candidateRank >= 0 && decision.candidateRank < decision.candidateCount {
		attrs = append(attrs, "candidate_rank", decision.candidateRank)
	}
	return attrs
}

func (h *PlaybackHandler) startPlannedPlaybackV3(r *http.Request, userID int, profileID string, req playback.StartRequestV3, requestDigests playbackStartRequestDigestsV3, requestedFile, effectiveFile *models.MediaFile, audioIndex int, virtualDecision virtualPlanDecisionV3, result playback.PlannerResultV3, clientInfo playback.ClientInfo, substitutionReason ...string) (playback.DecisionResponseV3, *transportErrorV3) {
	substitutionReasonV3 := ""
	if len(substitutionReason) > 0 {
		substitutionReasonV3 = substitutionReason[0]
	}
	if result.Plan == nil {
		return playback.DecisionResponseV3{}, &transportErrorV3{reason: "internal_error", message: "The server produced no playback plan."}
	}
	if checker, ok := h.sessionMgr.(transcodePermissionChecker); ok && (result.PlayMethod == playback.PlayTranscode || result.TranscodeAudio) {
		if err := checker.CheckTranscodingAllowed(r.Context(), userID, result.PlayMethod == playback.PlayTranscode); err != nil {
			reason := "transcoding_disabled"
			if errors.Is(err, playback.ErrAudioTranscodingDisabled) {
				reason = "audio_transcoding_disabled"
			}
			return playback.DecisionResponseV3{}, &transportErrorV3{reason: reason, message: "The selected server adaptation is disabled for this user."}
		}
	}
	mode := headerAuthenticatedMediaV3(req.ClientFeatures)
	ctx := playback.WithClientInfo(r.Context(), clientInfo)
	session, err := h.sessionMgr.StartSessionWithFilesContext(ctx, userID, profileID, effectiveFile.ID, requestedFile.ID, result.PlayMethod, result.TranscodeAudio)
	if err != nil {
		return playback.DecisionResponseV3{}, sessionStartErrorV3(err)
	}
	abort := func() { _ = h.stopPlaybackSessionByID(context.WithoutCancel(r.Context()), session.ID, false) }
	// Auto version fallback is negotiated once at start: an auto selection
	// leaves it on (a dead source may fail over across versions), an explicit
	// version pick turns it off (the viewer chose that release). The session
	// records it so a later replan honors the same intent, and the viewer can
	// flip it back on through the version menu's Auto entry.
	if setter, ok := h.sessionMgr.(interface {
		SetAutoFallback(sessionID string, enabled bool) error
	}); ok {
		autoFallback := req.AllowsAlternateVersions() && req.FileSelection != playback.FileSelectionExplicitV3
		if err := setter.SetAutoFallback(session.ID, autoFallback); err != nil {
			abort()
			return playback.DecisionResponseV3{}, &transportErrorV3{
				reason:  "internal_error",
				message: "Failed to establish the version fallback policy.",
				cause:   err,
			}
		}
	}
	if req.ProgressPersistence == playback.ProgressPersistenceClientV3 || !sessionOwnsResumeTimelineV3(effectiveFile) {
		if err := h.sessionMgr.SetProgressPersistenceDisabled(session.ID, true); err != nil {
			abort()
			return playback.DecisionResponseV3{}, &transportErrorV3{
				reason:  "internal_error",
				message: "Failed to establish the requested progress persistence policy.",
				cause:   err,
			}
		}
	}
	if err := h.sessionMgr.UpdateAudioTrack(session.ID, audioIndex, result.PlayMethod); err != nil {
		abort()
		return playback.DecisionResponseV3{}, &transportErrorV3{reason: "internal_error", message: "Failed to select the playback audio track.", cause: err}
	}
	position := floatOrZeroHandlerV3(req.StartPosition)
	if err := h.sessionMgr.UpdateProgress(session.ID, position, false); err != nil {
		abort()
		return playback.DecisionResponseV3{}, &transportErrorV3{reason: "internal_error", message: "Failed to initialize the playback timeline.", cause: err}
	}
	session, err = h.sessionMgr.GetSession(session.ID)
	if err != nil {
		abort()
		return playback.DecisionResponseV3{}, &transportErrorV3{reason: "internal_error", message: "Failed to load the initialized playback session.", cause: err}
	}
	result.Plan.SessionID = session.ID
	result.Plan.InventoryURL = "/api/v2/playback/" + session.ID + "/inventory"
	transport, transportErr := h.prepareTransportV3(r, session, effectiveFile, result, mode)
	if transportErr != nil {
		abort()
		return playback.DecisionResponseV3{}, transportErr
	}
	// Publish the substitution truth additively on the plan: when the effective
	// release differs from the requested row, name the requested row and a
	// machine-readable cause so the client can show an honest notice without
	// diffing ids first. These fields are set after the plan identity is
	// finalized inside the planner, so they never change the plan hash. The web
	// player still compares the live effective identity against the requested
	// one at render time, so a serve-layer rotation is covered too.
	publishSubstitutionFieldsV3(result.Plan, requestedFile.ID, effectiveFile.ID, substitutionReasonV3)
	// One line per final plan decision so route selection is reconstructible
	// from server logs.
	planDecisionAttrs := []any{
		logComponentKey, playbackLogValueV3,
		requestIDLogKeyV3, chimw.GetReqID(r.Context()),
		"outcome", "plan",
		"decision_reason", result.Plan.DecisionReason,
		"delivery", result.Plan.Delivery,
		"play_method", string(result.PlayMethod),
		"requested_file_id", requestedFile.ID,
		"effective_file_id", effectiveFile.ID,
		"dv_profile", result.Plan.Source.DVProfile,
		"dynamic_range", result.Plan.Source.DynamicRange,
		"target_resolution", result.TargetResolution,
		"target_bitrate_kbps", result.TargetBitrateKbps,
		"quality_preference", req.QualityPreference,
		bandwidthEstimateLogKeyV3, intOrZeroHandlerV3(req.BandwidthEstimateKbps),
	}
	// Virtual playback gets the candidate and language decision on the same
	// line: which provider candidate was selected, where it ranked in the
	// considered list, the audio track the plan will play, and the resolved
	// playback.audio_language that steered it. Without these a cold playback
	// that ignored the configured language was unattributable.
	planDecisionAttrs = append(planDecisionAttrs, virtualPlanDecisionAttrsV3(result, audioIndex, virtualDecision)...)
	slog.InfoContext(r.Context(), "playback plan decided", append(planDecisionAttrs, clientInfo.LogAttrs()...)...)
	applyTransportToneMapModeV3(&result, transport)
	frozenRecipe, frozenErr := h.freezeExecutableRecipeV3(r.Context(), effectiveFile, result)
	if frozenErr != nil {
		transport.rollback()
		abort()
		return playback.DecisionResponseV3{}, subtitleArtifactErrorV3("Failed to freeze the selected subtitle identity.", frozenErr)
	}
	result.Plan.Stream.URL = transport.url
	if err := h.attachSubtitleArtifactV3(r.Context(), session.ID, effectiveFile, result.Plan, result.SubtitleTrackIndex, &frozenRecipe, req.ClientFeatures); err != nil {
		transport.rollback()
		abort()
		return playback.DecisionResponseV3{}, subtitleArtifactErrorV3("Failed to prepare the selected subtitle artifact.", err)
	}
	response := playback.DecisionResponseV3{ProtocolVersion: playback.ProtocolV3, ServerFeatures: serverFeaturesForRequestV3(r.Context()), Outcome: playback.OutcomePlayableV3, SessionID: session.ID, PlaybackPlan: result.Plan}
	record := playback.AttemptRecordV3{PlaybackAttemptID: req.PlaybackAttemptID, SessionID: session.ID, UserID: userID, ProfileID: profileID, RequestedMediaFileID: requestedFile.ID, EffectiveMediaFileID: effectiveFile.ID, CurrentPlanID: result.Plan.PlanID, CurrentPlan: *result.Plan, FrozenRecipe: frozenRecipe, NormalizedRequest: req, ServerBitrateCapKbps: serverBitrateCapV3(r.Context()), StartResponse: response, RequestDigest: requestDigests.current, ExpiresAt: time.Now().Add(playback.MaxTokenTTL)}
	// Capture the durable audio selection intent before the session state
	// and attempt are committed: SelectionOrigin distinguishes an omitted
	// start (server-resolved language preference, reconcilable) from an
	// explicit viewer choice (never auto-overridden). The snapshot is taken
	// from the planned selection, so later synchronous probe writes inside
	// this start cannot erase the omitted-vs-explicit distinction.
	recordIntentV3, intentErr := intentForCommittedStartV3(r.Context(), h, userID, profileID, req, effectiveFile, plannedAudioTrackIndexV3(result, audioIndex), virtualDecision)
	if intentErr != nil {
		transport.rollback()
		abort()
		return playback.DecisionResponseV3{}, &transportErrorV3{reason: "internal_error", message: "Failed to resolve the playback audio preference.", cause: intentErr}
	}
	record.SelectionOrigin = recordIntentV3.origin
	record.PreferredAudioLanguage = recordIntentV3.preferredLang
	record.SeriesAudioPreferenceSignature = recordIntentV3.seriesSignature
	record.SelectedAudioSignature = recordIntentV3.selectedOverride
	if err := h.SetAudioSelectionIntent(session.ID, record.SelectionOrigin, record.PreferredAudioLanguage, record.SeriesAudioPreferenceSignature, record.SelectedAudioSignature); err != nil {
		slog.WarnContext(r.Context(), "protocol v3 start: audio selection intent not recorded on the session",
			"component", "api", "session", session.ID, "error", err)
	}
	if err := h.updateV3SessionState(r.Context(), session, effectiveFile, result, transport, mode); err != nil {
		transport.rollback()
		abort()
		return playback.DecisionResponseV3{}, &transportErrorV3{reason: "internal_error", message: "Failed to commit the live playback session.", cause: err}
	}
	if err := h.PlanStoreV3.SaveAttempt(r.Context(), record); err != nil {
		transport.rollback()
		abort()
		if errors.Is(err, playback.ErrPlaybackAttemptExistsV3) || errors.Is(err, playback.ErrIdempotencyKeyReusedV3) {
			existing, lookupErr := h.PlanStoreV3.GetAttemptByPlaybackAttemptID(r.Context(), req.PlaybackAttemptID)
			if lookupErr == nil && existing.UserID == userID && existing.ProfileID == profileID && existing.RequestedMediaFileID == req.FileID && requestDigests.matches(existing.RequestDigest) {
				if err := requireAttemptAPISurfaceV3(r.Context(), existing, req.ClientFeatures); err != nil {
					return playback.DecisionResponseV3{}, &transportErrorV3{reason: "playback_attempt_reused", message: "The playback attempt belongs to a different API surface."}
				}
				// Replaying a concurrent duplicate is only valid while its
				// session is alive; otherwise tell the client to mint a new
				// attempt rather than hand it a plan it can never stream.
				if _, sessionErr := h.sessionMgr.GetSession(existing.SessionID); sessionErr != nil {
					return playback.DecisionResponseV3{}, &transportErrorV3{reason: "session_expired", message: "The playback session for this attempt has ended.", retryable: true}
				}
				return decisionResponseFromAttemptV3(existing), nil
			}
			if errors.Is(err, playback.ErrIdempotencyKeyReusedV3) {
				return playback.DecisionResponseV3{}, &transportErrorV3{reason: "playback_attempt_reused", message: "The playback attempt ID was reused with different input."}
			}
		}
		return playback.DecisionResponseV3{}, &transportErrorV3{reason: "internal_error", message: "Failed to persist the playback plan.", cause: err}
	}
	if commitErr := transport.commit(); commitErr != nil {
		abort()
		return playback.DecisionResponseV3{}, commitErr
	}
	// Start-side effects belong after both the attempt and transport commits:
	// retries that lose the idempotency race must not emit duplicate provider
	// scrobbles or analysis work for the short-lived session they roll back.
	h.raceCopySafetyV3(effectiveFile.ID, result.Plan)
	// Publish the committed effective version and its declared inventory on the
	// session's realtime channel. On a fresh start the plan response already
	// carries this, but an attempt that replays a stored decision or a second
	// consumer watching the same session can still learn it from here; a
	// rotation is the case that depends on it.
	h.PublishSourceCommitted(r.Context(), session.ID)
	// The transport is committed above. Run the virtual subtitle and font warms
	// detached from this start request: each resolves its own relay registration
	// and demuxes the remote source, and running them on the response path let
	// full remote reads race the just-committed video transport. Scheduling here
	// keeps the transport start uncontended while the first client subtitle or
	// font fetch still finds a warm (or in-flight) entry.
	h.scheduleVirtualWarmAfterTransportV3(r.Context(), session, effectiveFile, result.SubtitleTrackIndex)
	h.enqueuePlaybackStartSideEffectsV3(r.Context(), session, effectiveFile, userID, profileID, plannedAudioTrackIndexV3(result, audioIndex))
	h.enqueueRouteEventV3(playback.RouteEventRecordV3{RouteEventV3: playback.RouteEventV3{ProtocolVersion: playback.ProtocolV3, PlaybackAttemptID: req.PlaybackAttemptID, SessionID: session.ID, PlanID: result.Plan.PlanID, Event: playback.RouteEventPlanSelectedV3, AppliedQuirkIDs: appliedQuirkIDsV3(result.Plan), QuirkRegistryRevision: appliedQuirkRevisionV3(result.Plan), OutputContextID: req.ClientPlaybackContext.Output.OutputContextID}, UserID: userID, ProfileID: profileID, ClientName: clientInfo.Name, ClientVersion: clientInfo.Version, ClientBuild: clientInfo.Build, ClientChannel: clientInfo.Channel, ClientModel: req.ClientPlaybackContext.Device.Model})
	return response, nil
}

func (h *PlaybackHandler) enqueuePlaybackStartSideEffectsV3(ctx context.Context, session *playback.Session, file *models.MediaFile, userID int, profileID string, audioTrackIndex int) {
	if h == nil || session == nil || file == nil {
		return
	}
	h.v3StartEffectsOnce.Do(func() {
		h.v3StartEffectsQueue = make(chan playbackStartSideEffectsV3, playbackStartSideEffectsQueueSizeV3)
		h.v3StartEffectsPending = make(map[string]*playbackStartSideEffectsStateV3)
		for range playbackStartSideEffectsWorkersV3 {
			go func() {
				for task := range h.v3StartEffectsQueue {
					h.runPlaybackStartSideEffectsV3(task)
				}
			}()
		}
	})

	taskCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), playbackStartSideEffectsTimeoutV3)
	state := &playbackStartSideEffectsStateV3{
		ctx:    taskCtx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	task := playbackStartSideEffectsV3{
		session:         *session,
		file:            *file,
		userID:          userID,
		profileID:       profileID,
		audioTrackIndex: audioTrackIndex,
		state:           state,
	}
	h.v3StartEffectsMu.Lock()
	h.v3StartEffectsPending[session.ID] = state
	h.v3StartEffectsMu.Unlock()
	select {
	case h.v3StartEffectsQueue <- task:
	default:
		// Preserve side-effect durability and ordering under overload. The queue
		// bounds background work; a saturated server pays the old synchronous
		// cost instead of silently dropping a provider event or preference write.
		h.runPlaybackStartSideEffectsV3(task)
	}
}

func (h *PlaybackHandler) runPlaybackStartSideEffectsV3(task playbackStartSideEffectsV3) {
	state := task.state
	if state == nil {
		return
	}
	h.v3StartEffectsMu.Lock()
	state.started = true
	stopRequested := state.stopRequested
	h.v3StartEffectsMu.Unlock()
	defer func() {
		state.cancel()
		close(state.done)
		h.v3StartEffectsMu.Lock()
		if h.v3StartEffectsPending[task.session.ID] == state {
			delete(h.v3StartEffectsPending, task.session.ID)
		}
		h.v3StartEffectsMu.Unlock()
	}()
	if stopRequested || state.ctx.Err() != nil {
		return
	}
	ctx := state.ctx

	if !task.session.DisableProgressPersistence && h.WatchScrobbler != nil {
		targetID := playbackProgressTarget(&task.file)
		if targetID != "" {
			event := h.scrobbleEventForSession(ctx, &task.session, targetID, float64(task.file.Duration), task.session.Position)
			if err := h.WatchScrobbler.ScrobbleStart(ctx, event); err != nil {
				slog.WarnContext(ctx, "failed to queue watch provider start scrobble", "component", "api", "session", task.session.ID, "error", err)
			}
		}
	}
	if ctx.Err() != nil {
		return
	}
	if h.ChapterThumbnailQueuer != nil {
		slog.InfoContext(ctx,
			"queueing chapter thumbnails", "component", "api",
			"source", "playback_start",
			"content_id", task.file.ContentID,
			"file_id", task.file.ID,
			"target_seconds", task.session.Position,
		)
		h.ChapterThumbnailQueuer.QueuePriorityFileAtPosition(ctx, task.file.ID, task.session.Position)
	}
	if ctx.Err() != nil {
		return
	}
	h.maybeQueueLazyPlaybackMarkers(ctx, &task.session, &task.file)
	if ctx.Err() != nil {
		return
	}
	h.persistSeriesPlaybackPreference(ctx, task.userID, task.profileID, &task.file)
	h.persistCurrentAudioPreferenceV3(ctx, task.session.ID, task.userID, task.profileID, &task.file, task.audioTrackIndex)
	h.syncSessionsNow(ctx, "v3_start")
}

func (h *PlaybackHandler) waitForPlaybackStartSideEffectsV3(ctx context.Context, sessionID string) {
	if h == nil || sessionID == "" {
		return
	}
	h.v3StartEffectsMu.Lock()
	state := h.v3StartEffectsPending[sessionID]
	h.v3StartEffectsMu.Unlock()
	if state == nil {
		return
	}
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), playbackStartSideEffectsTimeoutV3)
	defer cancel()
	select {
	case <-state.done:
	case <-waitCtx.Done():
		slog.WarnContext(ctx, "timed out waiting for playback start side effects", logComponentKey, "api", "session", sessionID)
	}
}

func (h *PlaybackHandler) cancelPlaybackStartSideEffectsV3(ctx context.Context, sessionID string) {
	if h == nil || sessionID == "" {
		return
	}
	h.v3StartEffectsMu.Lock()
	state := h.v3StartEffectsPending[sessionID]
	if state == nil {
		h.v3StartEffectsMu.Unlock()
		return
	}
	state.stopRequested = true
	state.cancel()
	started := state.started
	done := state.done
	h.v3StartEffectsMu.Unlock()
	// A queued task observes stopRequested before issuing any side effect. A
	// running task is allowed to unwind so provider start cannot cross the stop.
	if !started {
		return
	}
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), playbackStartSideEffectsTimeoutV3)
	defer cancel()
	select {
	case <-done:
	case <-waitCtx.Done():
		slog.WarnContext(ctx, "timed out canceling playback start side effects", logComponentKey, "api", "session", sessionID)
	}
}

// persistCurrentAudioPreferenceV3 serializes start and replan preference writes
// and rejects a delayed start value after the live session has already adopted
// a newer track. The lock makes the final write match replacement commit order.
func (h *PlaybackHandler) persistCurrentAudioPreferenceV3(ctx context.Context, sessionID string, userID int, profileID string, file *models.MediaFile, audioTrackIndex int) {
	h.v3AudioPreferenceMu.Lock()
	defer h.v3AudioPreferenceMu.Unlock()
	current, err := h.sessionMgr.GetSession(sessionID)
	if err != nil || current.AudioTrackIndex != audioTrackIndex {
		return
	}
	h.persistAudioPreference(ctx, userID, profileID, file, audioTrackIndex)
}

// prepareTransportV3 selects and prepares the local, remote, or identity transport.
func (h *PlaybackHandler) prepareTransportV3(r *http.Request, session *playback.Session, file *models.MediaFile, result playback.PlannerResultV3, mode mediaAuthModeV3) (preparedTransportV3, *transportErrorV3) {
	return h.prepareTransportWithPolicyV3(r, session, file, result, mode, h.playbackRoutingPolicyForContextV3(r.Context()))
}

func (h *PlaybackHandler) prepareTransportWithPolicyV3(r *http.Request, session *playback.Session, file *models.MediaFile, result playback.PlannerResultV3, mode mediaAuthModeV3, policy config.PlaybackRoutingPolicy) (preparedTransportV3, *transportErrorV3) {
	return h.prepareTransportWithPolicyAndExclusionsV3(r, session, file, result, mode, policy, nil)
}

func (h *PlaybackHandler) prepareTransportWithPolicyAndExclusionsV3(
	r *http.Request,
	session *playback.Session,
	file *models.MediaFile,
	result playback.PlannerResultV3,
	mode mediaAuthModeV3,
	policy config.PlaybackRoutingPolicy,
	initialExcludedShapes map[string]struct{},
) (preparedTransportV3, *transportErrorV3) {
	if !shouldUsePooledPlaybackNodeV3(file) {
		policy.RemuxExecution = config.PlaybackExecutionAPIOnly
		policy.VideoTranscodeExecution = config.PlaybackExecutionAPIOnly
		policy.RemuxEgress = config.PlaybackEgressAPIOnly
		policy.VideoTranscodeEgress = config.PlaybackEgressAPIOnly
		policy.DirectPlayEgress = config.PlaybackEgressAPIOnly
	} else if isVirtualPlaybackFile(file) || strings.HasPrefix(strings.ToLower(strings.TrimSpace(file.FilePath)), virtualPlaybackPrefix) {
		policy.RemuxExecution = config.PlaybackExecutionAPIOnly
		policy.RemuxEgress = config.PlaybackEgressAPIOnly
		policy.DirectPlayEgress = config.PlaybackEgressAPIOnly
	}
	timeline, timelineErr := h.prepareTransportTimelineV3(r.Context(), session, file, result)
	if timelineErr != nil {
		return preparedTransportV3{}, timelineErr
	}
	if result.Plan.Delivery != playback.DeliveryTranscodeHLSV3 && result.Plan.Delivery != playback.DeliveryRemuxHLSV3 {
		return h.prepareIdentityTransportV3(r, session, file, result, timeline, mode, policy, initialExcludedShapes)
	}
	proxyAllowed := mode.proxyEgress || (!mode.headerAuth && h.JWTSecret != "")
	excludedNodes := make(map[string]struct{})
	excludedShapes := make(map[string]struct{}, len(initialExcludedShapes))
	for shapeID := range initialExcludedShapes {
		excludedShapes[shapeID] = struct{}{}
	}
	var lastErr *transportErrorV3
	// Every candidate failure being audio-local is reported as the audio reason
	// rather than the generic route exhaustion, so the client gets a clear
	// audio outcome. A single non-audio failure makes the exhaustion a route
	// failure.
	audioOnlyFailure := true
	for attempts := 0; attempts < 32; attempts++ {
		decision := h.resolveHLSRouteWithPolicyV3(r.Context(), session, result, policy, proxyAllowed, excludedNodes, excludedShapes)
		if !decision.Selected() {
			if fallback, attempted, fallbackErr := h.prepareSoftwareToneMapFallbackWithPolicyV3(r, session, file, result, mode, policy); attempted {
				if fallbackErr == nil {
					return fallback, nil
				}
				return fallback, combineTransportErrorsV3(lastErr, fallbackErr)
			}
			if lastErr != nil {
				return preparedTransportV3{}, lastErr
			}
			retryable := decision.Outcome == noderouting.OutcomeCapacityUnavailable
			return preparedTransportV3{}, &transportErrorV3{
				reason: string(decision.Outcome), message: "No playback route satisfies the configured policy and current node availability.", retryable: retryable,
			}
		}

		if decision.Shape.Execution == noderouting.ExecutionAPI {
			if capabilityErr := h.validateLocalTransportCapabilitiesV3(r.Context(), result); capabilityErr != nil {
				audioOnlyFailure = false
				lastErr = combineTransportErrorsV3(lastErr, capabilityErr)
				excludedShapes[decision.Shape.ID] = struct{}{}
				continue
			}
			transport, transportErr := h.prepareLocalTransportV3(r, session, file, result, timeline, mode)
			if transportErr == nil {
				return transport, nil
			}
			if transportErr.reason == candidateSourceDecodeRejectedReasonV3 {
				// A source the decoder rejected is bad for every executor, so
				// rotating to another delivery shape would only re-encode the
				// same undecodable bytes. Surface it immediately so the start or
				// replan rotation loop can substitute a provider candidate.
				return preparedTransportV3{}, transportErr
			}
			if transportErr.reason == subtitleUnavailableReasonV3 {
				// A subtitle-local failure is not a route problem: every other
				// shape or node would run the same release and prepare the same
				// subtitle. Surface it unchanged so the release and the
				// subtitle decision stay with the caller; never route around it
				// into a different delivery.
				return preparedTransportV3{}, transportErr
			}
			if !audioOnlyTransportReasonV3(transportErr.reason) {
				audioOnlyFailure = false
			}
			lastErr = combineTransportErrorsV3(lastErr, transportErr)
			excludedShapes[decision.Shape.ID] = struct{}{}
			continue
		}

		plan := decision.Plan
		nodeURL := plan.TranscodeNode.URL
		transformations, capabilityErr := h.remoteTransformationsV3(r.Context(), nodeURL)
		if capabilityErr == nil {
			capabilityErr = validateAdvertisedTransformationsV3(result.Plan, transformations)
		}
		if capabilityErr == nil && planRequiresToneMapV3(result.Plan) {
			capabilities, err := h.remoteToneMapCapabilitiesV3(r.Context(), nodeURL, false)
			if err != nil {
				capabilityErr = err
			} else {
				capabilityErr = validateToneMapExecutorV3(result, capabilities)
			}
		}
		if capabilityErr != nil {
			slog.WarnContext(r.Context(), "protocol v3 transcode node capability mismatch", "node", logredact.SanitizeURL(nodeURL), "error", capabilityErr)
			if releaser, ok := h.NodePlanner.(sessionReservationReleaserV3); ok {
				releaser.ReleaseSession(session.ID)
			}
			excludedNodes[nodeURL] = struct{}{}
			audioOnlyFailure = false
			lastErr = combineTransportErrorsV3(lastErr, &transportErrorV3{reason: routeCapabilityUnavailableReasonV3, message: "No available worker can execute the selected playback recipe.", retryable: true, cause: capabilityErr})
			continue
		}

		transport, transportErr := h.prepareRemoteTransportV3(r, session, file, result, plan, timeline, mode)
		proxyAuthorityFailed := false
		if transportErr == nil {
			// URL construction is part of route preparation. A proxy route that
			// could not establish its authority must retry a legal API-egress shape,
			// never silently mutate the selected topology.
			if decision.Shape.Egress != noderouting.EgressProxy || strings.HasPrefix(transport.url, "http") {
				return transport, nil
			}
			apiEgressAllowed := false
			switch decision.Shape.Workload {
			case noderouting.WorkloadRemux:
				apiEgressAllowed = policy.RemuxEgress != config.PlaybackEgressProxyOnly
			case noderouting.WorkloadVideoTranscode:
				apiEgressAllowed = policy.VideoTranscodeEgress != config.PlaybackEgressProxyOnly
			}
			if apiEgressAllowed {
				// prepareRemoteTransportV3 already released only the unused proxy
				// half. Keep the running executor and publish its API relay rather
				// than stopping and restarting an identical generation.
				return transport, nil
			}
			transport.rollback()
			transportErr = &transportErrorV3{reason: "route_preparation_failed", message: "The selected proxy could not establish playback authority.", retryable: true}
			proxyAuthorityFailed = true
		}
		if releaser, ok := h.NodePlanner.(sessionReservationReleaserV3); ok {
			releaser.ReleaseSession(session.ID)
		}
		if proxyAuthorityFailed {
			excludedShapes[decision.Shape.ID] = struct{}{}
		} else {
			excludedNodes[nodeURL] = struct{}{}
		}
		if !audioOnlyTransportReasonV3(transportErr.reason) {
			audioOnlyFailure = false
		}
		lastErr = combineTransportErrorsV3(lastErr, transportErr)
	}
	if audioOnlyFailure && lastErr != nil {
		// Every route failed on the audio recipe, so the exhaustion is an audio
		// outcome: surface it locally instead of the generic route failure.
		return preparedTransportV3{}, lastErr
	}
	return preparedTransportV3{}, &transportErrorV3{reason: "route_preparation_failed", message: "Playback route preparation exhausted every candidate.", retryable: true, cause: lastErr}
}

func (h *PlaybackHandler) checkReplacementAdmissionV3(
	ctx context.Context,
	session *playback.Session,
	result playback.PlannerResultV3,
) *transportErrorV3 {
	if session == nil {
		return &transportErrorV3{reason: "internal_error", message: "Playback route admission has no live session."}
	}
	if checker, ok := h.sessionMgr.(replacementAdmissionCheckerV3); ok {
		if err := checker.CheckReplacementAllowed(ctx, session.ID, result.PlayMethod, result.TranscodeAudio); err != nil {
			return sessionStartErrorV3(err)
		}
		return nil
	}
	if checker, ok := h.sessionMgr.(transcodePermissionChecker); ok &&
		(result.PlayMethod == playback.PlayTranscode || result.TranscodeAudio) {
		if err := checker.CheckTranscodingAllowed(ctx, session.UserID, result.PlayMethod == playback.PlayTranscode); err != nil {
			return sessionStartErrorV3(err)
		}
	}
	return nil
}

// canRetrySoftwareToneMapV3 permits a software retry only for an initial
// hardware selection whose policy allows it; frozen recovery recipes stay exact.
func canRetrySoftwareToneMapV3(result playback.PlannerResultV3) bool {
	return result.FrozenSourceMetadata == nil && result.ToneMapMode == tonemap.ModeHardware &&
		result.ToneMapPolicy.Allows(tonemap.ModeSoftware)
}

// prepareSoftwareToneMapFallbackV3 retries an eligible failed hardware recipe
// on a software-capable remote node or, when allowed, on the API host.
func (h *PlaybackHandler) prepareSoftwareToneMapFallbackV3(r *http.Request, session *playback.Session, file *models.MediaFile, result playback.PlannerResultV3, mode mediaAuthModeV3) (preparedTransportV3, bool, *transportErrorV3) {
	return h.prepareSoftwareToneMapFallbackWithPolicyV3(r, session, file, result, mode, h.playbackRoutingPolicyForContextV3(r.Context()))
}

// prepareSoftwareToneMapFallbackWithPolicyV3 re-enters route preparation with a
// software tone-map recipe, so the retry re-derives its own timeline rather than
// inheriting the failed attempt's.
func (h *PlaybackHandler) prepareSoftwareToneMapFallbackWithPolicyV3(r *http.Request, session *playback.Session, file *models.MediaFile, result playback.PlannerResultV3, mode mediaAuthModeV3, policy config.PlaybackRoutingPolicy) (preparedTransportV3, bool, *transportErrorV3) {
	if !canRetrySoftwareToneMapV3(result) {
		return preparedTransportV3{}, false, nil
	}
	fallbackResult := result
	fallbackResult.ToneMapMode = tonemap.ModeSoftware
	fallback, fallbackErr := h.prepareTransportWithPolicyV3(r, session, file, fallbackResult, mode, policy)
	return fallback, true, fallbackErr
}

func (h *PlaybackHandler) validateLocalTransportCapabilitiesV3(ctx context.Context, result playback.PlannerResultV3) *transportErrorV3 {
	if !planRequiresServerTransformationsV3(result.Plan) {
		return nil
	}
	// Only a tone-mapped recipe depends on the tone-map probe. Any other recipe
	// is checked against the base registry, so an SDR transcode never waits on
	// a cold probe or fails when one times out.
	requiresToneMap := planRequiresToneMapV3(result.Plan)
	localRegistry := h.transformationRegistryV3(ctx)
	if requiresToneMap {
		var capabilityErr error
		localRegistry, capabilityErr = h.localHLSExecutionRegistryV3(ctx)
		if capabilityErr != nil {
			return &transportErrorV3{reason: capabilityUnavailableReasonV3, message: "Local transcode capability validation is temporarily unavailable.", retryable: true, cause: capabilityErr}
		}
	}
	if err := validateAdvertisedTransformationsV3(result.Plan, localRegistry.Advertised()); err != nil {
		return &transportErrorV3{reason: capabilityUnavailableReasonV3, message: "No available transcode executor can run the selected playback recipe.", retryable: true, cause: err}
	}
	if !requiresToneMap {
		return nil
	}
	capabilities, capabilityErr := h.localToneMapCapabilitiesForTransportV3(ctx)
	if capabilityErr != nil {
		return &transportErrorV3{reason: capabilityUnavailableReasonV3, message: "Local tone-map capability validation is temporarily unavailable.", retryable: true, cause: capabilityErr}
	}
	if err := validateToneMapExecutorV3(result, capabilities); err != nil {
		return &transportErrorV3{reason: capabilityUnavailableReasonV3, message: "No available transcode executor can run the selected tone-map recipe.", retryable: true, cause: err}
	}
	return nil
}

func (h *PlaybackHandler) validateLocalProgressiveCapabilitiesV3(ctx context.Context, result playback.PlannerResultV3) *transportErrorV3 {
	if !planRequiresServerTransformationsV3(result.Plan) {
		return nil
	}
	if err := validateAdvertisedTransformationsV3(result.Plan, h.transformationRegistryV3(ctx).Advertised()); err != nil {
		return &transportErrorV3{
			reason: routeCapabilityUnavailableReasonV3, message: "The API server cannot execute the selected progressive-remux recipe.",
			retryable: true, cause: err,
		}
	}
	return nil
}

// copySeekAnchorRetryFits reports whether the caller's remaining budget can
// accommodate a second copy-video seek-anchor probe. Without an explicit
// deadline the probe's own per-attempt cap bounds the retry, so it is allowed.
// With a deadline, a retry only helps when more than a full probe plus the
// scheduling margin remains; otherwise it would spend the rest of the budget
// and surface the caller deadline instead of the probe error.
func copySeekAnchorRetryFits(ctx context.Context) (bool, time.Duration) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true, 0
	}
	remaining := time.Until(deadline)
	return remaining > playback.CopySeekProbeTimeout+copySeekProbeRetryMargin, remaining
}

// copySeekAnchorRetryReserved reports whether the caller's remaining budget can
// hold a retry reserved from the start of the copy-video seek-anchor
// resolution: two full probes plus the scheduling margin. A caller that fails
// this gate never plans a retry, so the deadline cannot truncate a second probe
// mid-flight. It is the up-front reservation the in-loop copySeekAnchorRetryFits
// gate then narrows to the single retry probe once the first attempt has run. A
// caller with no deadline reserves the full probe cap and keeps the retry.
func copySeekAnchorRetryReserved(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return time.Until(deadline) > playback.CopySeekProbeRetryBudget()+copySeekProbeRetryMargin
}

// virtualTransportSameReleaseRetryKey marks a start request that has already
// spent its one same-release transport retry, so a retry that itself fails
// transiently walks to an alternate instead of retrying in a loop.
type virtualTransportSameReleaseRetryKey struct{}

func withVirtualTransportSameReleaseRetried(ctx context.Context) context.Context {
	if ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, virtualTransportSameReleaseRetryKey{}, true)
}

func virtualTransportSameReleaseRetried(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	retried, _ := ctx.Value(virtualTransportSameReleaseRetryKey{}).(bool)
	return retried
}

// publishSubstitutionFieldsV3 sets the additive substitution fields on a plan
// when the effective release differs from the requested row. It is called after
// the plan identity is finalized inside the planner, so the UI-only fields
// never change the plan hash, and it is a no-op for the ordinary same-release
// case (effectiveFileID == requestedFileID) or an unknown effective row. A
// reason already carried from the resolve walk wins over the caller's fallback
// so the more specific cause is preserved.
func publishSubstitutionFieldsV3(plan *playback.PlanV3, requestedFileID, effectiveFileID int, fallbackReason string) {
	if plan == nil || effectiveFileID <= 0 || effectiveFileID == requestedFileID {
		return
	}
	if plan.SubstitutionReason == "" {
		plan.SubstitutionReason = fallbackReason
	}
	plan.SubstitutedFromFileID = requestedFileID
}

// recoverVirtualTransportSameReleaseV3 spends one bounded same-release retry
// after a transient provider error, before the caller walks to another edition.
// It fires only for a transient cause, only once per request (the retry marks
// its context so a second transient failure does not loop), and only after a
// short pause so a provider that is already failing is not hammered.
//
// attempt re-runs the transport for the same release; it returns the retry's
// response and true only when that attempt succeeded. retried=false covers a
// non-transient cause, a request that already retried, a context that cannot
// fit the pause, and a retry that failed again — every case where the caller
// should proceed to its alternate walk or persist the terminal.
func (h *PlaybackHandler) recoverVirtualTransportSameReleaseV3(
	ctx context.Context,
	transient bool,
	attempt func(context.Context) (playback.DecisionResponseV3, *transportErrorV3),
) (playback.DecisionResponseV3, bool) {
	if !transient || attempt == nil || virtualTransportSameReleaseRetried(ctx) {
		return playback.DecisionResponseV3{}, false
	}
	retryCtx := withVirtualTransportSameReleaseRetried(withVirtualRelayFreshRegistration(ctx))
	if !h.waitCopySeekAnchorBackoff(retryCtx, 0) {
		return playback.DecisionResponseV3{}, false
	}
	slog.InfoContext(ctx, "virtual playback transport failed once on a transient provider error; retrying the same release",
		logComponentKey, playbackLogValueV3)
	response, retryErr := attempt(retryCtx)
	if retryErr != nil {
		return playback.DecisionResponseV3{}, false
	}
	return response, true
}

// waitCopySeekAnchorBackoff pauses before retrying a candidate after a
// transient provider (upstream 5xx) failure. It returns false when the wait or
// its result cannot fit the caller's remaining budget, so the retry is skipped
// instead of started without time to finish. remaining is the value already
// returned by copySeekAnchorRetryFits; 0 means the caller had no deadline.
func (h *PlaybackHandler) waitCopySeekAnchorBackoff(ctx context.Context, remaining time.Duration) bool {
	d := copySeekAnchorTransientBackoff
	if remaining > 0 {
		if room := remaining - (playback.CopySeekProbeTimeout + copySeekProbeRetryMargin); room < d {
			d = room
		}
	}
	if d <= 0 {
		return false
	}
	if h != nil && h.copySeekAnchorBackoff != nil {
		return h.copySeekAnchorBackoff(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (h *PlaybackHandler) prepareTransportTimelineV3(ctx context.Context, session *playback.Session, file *models.MediaFile, result playback.PlannerResultV3) (preparedTimelineV3, *transportErrorV3) {
	if result.Plan == nil {
		return preparedTimelineV3{}, nil
	}

	requested := result.Plan.Timeline.SourceStartSeconds
	switch result.Plan.Delivery {
	case playback.DeliveryRemuxProgressiveV3, playback.DeliveryRemuxHLSV3:
		// Audio-only remuxes have no copied video keyframe to resolve. Keep the
		// requested position as their exact stream origin: the chunked output
		// clock restarts at zero and later seeks require another server reanchor.
		if file != nil && file.IsAudioOnly() {
			configureCopyRemuxTimelineV3(result.Plan, requested)
			return preparedTimelineV3{seekSeconds: requested, streamOriginSeconds: requested}, nil
		}
		origin, startSegment := 0.0, 0
		if requested > 0 {
			if file == nil || strings.TrimSpace(file.FilePath) == "" {
				return preparedTimelineV3{}, &transportErrorV3{reason: transcodeStartFailedReasonV3, message: "Failed to resolve remux seek position.", retryable: true, cause: errors.New("copy seek anchor requires a media file path")}
			}
			// Virtual sources are provider-neutral URIs, not FFmpeg inputs.
			// Resolve the candidate to a pinned-IP relay URL — the same
			// resolution the transcode transport performs — so the anchor
			// probe reads the real stream instead of failing on a virtual://
			// protocol it cannot open.
			anchorInput := file.FilePath
			releaseAnchor := func() {}
			virtualAnchor := isVirtualPlaybackFile(file) && h.RemoteStreamRelay != nil && (h.VirtualMediaResolver != nil || h.VirtualMediaDetailedResolver != nil)
			anchorExpiresAt := time.Time{}
			// resolveAnchorInput resolves the candidate to a pinned-IP relay URL
			// — the same resolution the transcode transport performs — and
			// adopts the fresh registration. It runs again before a retry so the
			// second probe carries a new relay token instead of replaying a
			// token the provider already refused. The previous registration is
			// released first so a retry cannot leak the entry it replaced.
			resolveAnchorInput := func(resolveCtx context.Context) error {
				res, cleanup, resolveErr := h.resolveVirtualAnchorURIWithRotationV3(resolveCtx, session, file)
				if resolveErr != nil {
					return resolveErr
				}
				releaseAnchor()
				anchorInput = res.URL
				anchorExpiresAt = res.ExpiresAt
				releaseAnchor = cleanup
				return nil
			}
			if virtualAnchor {
				// Route the anchor through the same absent-pin rotation policy as
				// the serve layer and the replan rehydration: a provider that
				// renumbered its result ids must not terminal a remux seek when a
				// same-identity candidate is still listed. A different release is
				// refused rather than silently anchored.
				if resolveErr := resolveAnchorInput(ctx); resolveErr != nil {
					// This is the first live provider listing for an already-built
					// plan: a remux seek start binds the row's probe evidence via
					// the P0 fast path with no liveness check, by design. A
					// transient provider listing outage here (empty answer, 5xx)
					// is a dependency failure, not a verdict about the release, so
					// classify it as the retryable provider_unavailable — for
					// identity-less rows too, which the evidence-gated outage
					// retry cannot cover. A genuine absent-pin or marked-failed
					// release verdict is left as-is so the start's alternate walk
					// still runs.
					classified := classifyVirtualAnchorProviderOutage(resolveErr)
					return preparedTimelineV3{}, transportStartFailureV3(classified,
						&transportErrorV3{reason: transcodeStartFailedReasonV3, message: "Failed to resolve remux seek position.", retryable: true, cause: classified})
				}
				// A stored URL that has already lapsed is renewed before the probe,
				// bounded to virtualProbeBudget, so the seek does not fail on a
				// dead token. Best-effort: a failed renewal keeps the existing
				// registration and lets the probe/retry carry the verdict.
				if virtualResolvedURLExpired(anchorExpiresAt, time.Now()) {
					refreshCtx, refreshCancel := context.WithTimeout(ctx, virtualProbeBudget)
					if resolveErr := resolveAnchorInput(refreshCtx); resolveErr != nil {
						slog.WarnContext(ctx, "virtual seek anchor stored-url refresh failed; probing the existing registration",
							"component", "api",
							"playback_session_id", session.ID,
							"requested_seek_seconds", requested,
							"error", resolveErr,
						)
					}
					refreshCancel()
				}
			}
			// The stable source identity is the un-resolved file path: for a
			// virtual file that is the provider-neutral URI, identical across
			// concurrent starts even though each start pins its own relay URL.
			// Cache and singleflight key on it so identical probes coalesce
			// and survive across requests.
			sourceIdentity := file.FilePath
			ffmpegPath := h.playbackConfig().FFmpegPath
			probeAnchor := func(probeCtx context.Context) (float64, int, error) {
				if h.copySeekAnchor != nil {
					return h.copySeekAnchor(probeCtx, ffmpegPath, anchorInput, requested, 2)
				}
				return playback.ResolveCopySeekAnchorForSource(probeCtx, ffmpegPath, sourceIdentity, anchorInput, requested, 2)
			}
			// Virtual upstreams occasionally serve a range request slowly
			// enough to blow the anchor probe budget. One bounded retry
			// converts most of those transient timeouts into successful
			// seeks. The retry is skipped when the caller's remaining budget
			// cannot fit another full probe: a second attempt would consume the
			// rest of the budget and fail with the caller deadline instead of
			// the probe error, so failing fast on the first probe error is both
			// cheaper and clearer. A caller with no deadline keeps the retry.
			//
			// The retry budget is reserved up front: a caller that cannot hold
			// two full probes plus the scheduling margin never plans a retry, so
			// a second probe cannot be started only to be truncated by the
			// deadline.
			//
			// An upstream 5xx is a transient provider failure: it is retried the
			// same way, but only after a short bounded backoff, so a provider
			// that is already failing is not hammered by an immediate re-probe.
			retryReserved := copySeekAnchorRetryReserved(ctx)
			lastProbedInput := ""
			var err error
			for attempt := 1; attempt <= 2; attempt++ {
				if attempt > 1 && virtualAnchor {
					// A retry must probe fresh bytes: re-resolve the candidate
					// and register a new relay entry so the same token is never
					// probed twice. The fresh-registration context bypasses the
					// relay's content-key reuse, which would otherwise hand back
					// the live token for an unchanged provider URL and suppress
					// this retry before its second probe. A re-resolve that still
					// yields the same input means no new token is available, so a
					// second probe would repeat the request that just failed.
					if resolveErr := resolveAnchorInput(withVirtualRelayFreshRegistration(ctx)); resolveErr != nil {
						slog.WarnContext(ctx, "virtual seek anchor re-resolve failed; skipping retry",
							"component", "api",
							"playback_session_id", session.ID,
							"requested_seek_seconds", requested,
							"error", resolveErr,
						)
						break
					}
					if anchorInput == lastProbedInput {
						// The re-resolve handed back the token that just 5xxed:
						// probing it again repeats a known failure. On a
						// transient provider error, walk to an alternate
						// same-identity candidate once instead of giving up;
						// anything else (or no alternate) keeps the terminal.
						if playback.IsTransientProviderError(err) {
							if rotated, cleanup, rotErr := h.resolveVirtualAnchorURIExcludingFailedV3(ctx, session, file, virtualResultCandidateID(file.FilePath)); rotErr == nil {
								releaseAnchor()
								anchorInput = rotated.URL
								anchorExpiresAt = rotated.ExpiresAt
								releaseAnchor = cleanup
							} else {
								slog.WarnContext(ctx, "virtual seek anchor re-resolve returned the same relay token; skipping a duplicate probe",
									"component", "api",
									"playback_session_id", session.ID,
									"requested_seek_seconds", requested,
									"rotation_error", rotErr,
								)
								break
							}
						} else {
							slog.WarnContext(ctx, "virtual seek anchor re-resolve returned the same relay token; skipping a duplicate probe",
								"component", "api",
								"playback_session_id", session.ID,
								"requested_seek_seconds", requested,
							)
							break
						}
					}
					if fits, remaining := copySeekAnchorRetryFits(ctx); !fits {
						slog.WarnContext(ctx, "copy-video seek anchor retry skipped after re-resolve: insufficient remaining budget",
							"component", "api",
							"playback_session_id", session.ID,
							"requested_seek_seconds", requested,
							"remaining", remaining,
						)
						break
					}
				}
				lastProbedInput = anchorInput
				probeStarted := time.Now()
				origin, startSegment, err = probeAnchor(ctx)
				if err == nil || ctx.Err() != nil || attempt == 2 {
					break
				}
				fits, remaining := copySeekAnchorRetryFits(ctx)
				// A fast transient failure (relay 5xx in milliseconds, not a
				// consumed 15s probe) leaves room for a full second probe:
				// gate the retry on the single-probe fit rather than the
				// up-front two-probe reservation, which a slow resolve may
				// already have spent. Slow failures keep the conservative
				// gate so a truncated second probe cannot mask the error.
				fastTransient := playback.IsTransientProviderError(err) && time.Since(probeStarted) < playback.CopySeekProbeTimeout
				if !fits || (!retryReserved && !fastTransient) {
					slog.WarnContext(ctx, "copy-video seek anchor retry skipped: insufficient remaining budget",
						"component", "api",
						"playback_session_id", session.ID,
						"requested_seek_seconds", requested,
						"remaining", remaining,
						"required", playback.CopySeekProbeRetryBudget()+copySeekProbeRetryMargin,
						"error", err,
					)
					break
				}
				if playback.IsTransientProviderError(err) {
					if !h.waitCopySeekAnchorBackoff(ctx, remaining) {
						slog.WarnContext(ctx, "copy-video seek anchor retry skipped: no budget for the provider backoff",
							"component", "api",
							"playback_session_id", session.ID,
							"requested_seek_seconds", requested,
							"error", err,
						)
						break
					}
					slog.WarnContext(ctx, "copy-video seek anchor failed once on a transient provider error; backing off before retry",
						"component", "api",
						"playback_session_id", session.ID,
						"requested_seek_seconds", requested,
						"error", err,
					)
					continue
				}
				slog.WarnContext(ctx, "copy-video seek anchor failed once; retrying",
					"component", "api",
					"playback_session_id", session.ID,
					"requested_seek_seconds", requested,
					"error", err,
				)
			}
			releaseAnchor()
			if err != nil {
				slog.ErrorContext(ctx, "failed to resolve protocol v3 copy-video seek anchor",
					"component", "api",
					"playback_session_id", session.ID,
					"requested_seek_seconds", requested,
					"error", err,
				)
				return preparedTimelineV3{}, &transportErrorV3{reason: transcodeStartFailedReasonV3, message: "Failed to resolve remux seek position.", retryable: true, cause: err}
			}
		}
		configureCopyRemuxTimelineV3(result.Plan, origin)
		return preparedTimelineV3{seekSeconds: requested, streamOriginSeconds: origin, startSegmentNumber: startSegment, copySeekAnchorResolved: true}, nil
	case playback.DeliveryTranscodeHLSV3:
		sourceMetadata := sourceExecutionMetadataV3(file, result)
		seekSeconds, startSegment := configureHLSTimelineV3(result.Plan, result.TargetVideoCodec, playback.DefaultSegmentDuration, sourceMetadata.DurationSeconds)
		return preparedTimelineV3{seekSeconds: seekSeconds, streamOriginSeconds: result.Plan.Timeline.StreamOriginSeconds, startSegmentNumber: startSegment}, nil
	default:
		return preparedTimelineV3{}, nil
	}
}

// planRequiresServerTransformationsV3 reports whether the plan carries any
// transformation the serving executor (local binary or transcode node) must
// perform, as opposed to client-executed ones.
func planRequiresServerTransformationsV3(plan *playback.PlanV3) bool {
	if plan == nil {
		return false
	}
	for _, transformation := range plan.Transformations {
		if !strings.EqualFold(transformation.Executor, playback.ExecutorClientV3) {
			return true
		}
	}
	return false
}

func (h *PlaybackHandler) prepareIdentityTransportV3(r *http.Request, session *playback.Session, file *models.MediaFile, result playback.PlannerResultV3, timeline preparedTimelineV3, mode mediaAuthModeV3, policy config.PlaybackRoutingPolicy, excludedShapes map[string]struct{}) (preparedTransportV3, *transportErrorV3) {
	// The shared resolver removes proxy shapes when this client cannot address
	// an authorized origin, then applies the same policy ordering as every HLS
	// path. Legacy and authorized-origin attempts differ only in URL authority.
	decision, routeErr := h.resolveIdentityRouteV3(r, session.ID, result, mode, policy, excludedShapes)
	if routeErr != nil {
		return preparedTransportV3{}, routeErr
	}
	proxyNode := decision.Plan.ProxyNode
	transcodeNode := decision.Plan.TranscodeNode
	releaseReservation := func() {
		if releaser, ok := h.NodePlanner.(sessionReservationReleaserV3); ok {
			releaser.ReleaseSession(session.ID)
		}
	}
	var unlockLifecycle func()
	releaseLifecycle := func() {
		if unlockLifecycle == nil {
			return
		}
		unlockLifecycle()
		unlockLifecycle = nil
	}
	fallbackFromSelectedRoute := func(routeErr *transportErrorV3) (preparedTransportV3, *transportErrorV3) {
		releaseLifecycle()
		releaseReservation()
		fallbackExclusions := maps.Clone(excludedShapes)
		if fallbackExclusions == nil {
			fallbackExclusions = make(map[string]struct{})
		}
		fallbackExclusions[decision.Shape.ID] = struct{}{}
		fallback, fallbackErr := h.prepareIdentityTransportV3(r, session, file, result, timeline, mode, policy, fallbackExclusions)
		if fallbackErr == nil {
			return fallback, nil
		}
		return preparedTransportV3{}, combineTransportErrorsV3(routeErr, fallbackErr)
	}
	if transcodeNode != nil {
		transcodeCapabilities, capabilityErr := h.lookupRemoteCapabilitiesV3(r.Context(), transcodeNode.URL, false)
		if capabilityErr != nil || !slices.Contains(transcodeCapabilities.transportFeatures, playback.TransportFeatureProgressiveRemuxExecutionV1) {
			return fallbackFromSelectedRoute(&transportErrorV3{
				reason: routeCapabilityUnavailableReasonV3, message: "The selected transcode node cannot execute a progressive remux.",
				retryable: true, cause: capabilityErr,
			})
		}
	}
	if transcodeNode != nil && proxyNode != nil {
		proxyCapabilities, capabilityErr := h.lookupRemoteCapabilitiesV3(r.Context(), proxyNode.URL, false)
		if capabilityErr != nil || !slices.Contains(proxyCapabilities.transportFeatures, playback.TransportFeatureProgressiveRemuxRelayV1) {
			return fallbackFromSelectedRoute(&transportErrorV3{
				reason: routeCapabilityUnavailableReasonV3, message: "The selected proxy cannot relay a progressive remux from a transcode node.",
				retryable: true, cause: capabilityErr,
			})
		}
	}
	var executorNode *nodepool.Node
	switch decision.Shape.Execution {
	case noderouting.ExecutionProxy:
		executorNode = proxyNode
	case noderouting.ExecutionTranscode:
		executorNode = transcodeNode
	}
	if executorNode != nil && planRequiresServerTransformationsV3(result.Plan) {
		advertised, capabilityErr := h.remoteTransformationsV3(r.Context(), executorNode.URL)
		if capabilityErr == nil {
			capabilityErr = validateAdvertisedTransformationsV3(result.Plan, advertised)
		}
		if capabilityErr != nil {
			return fallbackFromSelectedRoute(&transportErrorV3{
				reason: routeCapabilityUnavailableReasonV3, message: "The selected node cannot execute the progressive-remux recipe.",
				retryable: true, cause: capabilityErr,
			})
		}
	}

	// A stop and a replacement must agree on the exact moment a progressive
	// authority becomes externally usable. Re-read the session after entering
	// their shared lifecycle boundary: if stop won, fail before publishing the
	// successor recipe or proxy grant; if replacement won, stop waits until its
	// commit or rollback has made that authority match the live session.
	if h.beforeIdentityLifecycleLockV3 != nil {
		h.beforeIdentityLifecycleLockV3()
	}
	unlockLifecycle = h.tm.LockSessionLifecycle(session.ID)
	// Accepted start/replan plans always carry their live session ID. Low-level
	// transport tests deliberately omit it so they can exercise route assembly
	// without reconstructing the surrounding request transaction.
	if result.Plan.SessionID != "" {
		if result.Plan.SessionID != session.ID {
			releaseLifecycle()
			releaseReservation()
			return preparedTransportV3{}, &transportErrorV3{
				reason: "internal_error", message: "The playback plan does not belong to the session being prepared.",
			}
		}
		currentSession, err := h.sessionMgr.GetSession(session.ID)
		if err != nil {
			releaseLifecycle()
			releaseReservation()
			return preparedTransportV3{}, &transportErrorV3{
				reason: "session_expired", message: "The playback session ended before its route could be prepared.", retryable: true, cause: err,
			}
		}
		session = currentSession
	}
	routeSession := *session
	// The URL builders below refuse to mint a stream token for a session that
	// requires media authorization. The live session only learns the mode when
	// its stream state is committed, so stamp the route copy the builders see.
	routeSession.RequireMediaAuthorization = mode.headerAuth
	routeSession.PlayMethod = result.PlayMethod
	routeSession.BasePlayMethod = result.PlayMethod
	routeSession.MediaFileID = result.Plan.EffectiveMediaFileID
	routeSession.AudioTrackIndex = plannedAudioTrackIndexV3(result, session.AudioTrackIndex)
	routeSession.TranscodeAudio = result.TranscodeAudio
	routeSession.TargetAudioCodec = result.TargetAudioCodec
	routeSession.SourceAudioChannels = result.SourceAudioChannels
	routeSession.TargetAudioChannels = result.TargetAudioChannels
	routeSession.TargetAudioBitrateKbps = result.TargetAudioBitrateKbps
	routeSession.RemuxDVMode = remuxDVModeForPlanV3(result.Plan)
	routeSession.RemuxResumeLeadingPictureDrop = result.RemuxResumeLeadingPictureDrop
	// A replan starts without an egress identity. Do not let the previous
	// proxy's row or internal URL leak into an API-served replacement route.
	routeSession.RoutingEgressNodeID = 0
	routeSession.RoutingEgressNodeURL = ""
	routeSession.RoutingNetworkProvider = new(netaccess.PathFromContext(r.Context()).Provider)
	routeSession.RoutingWorkload = string(routingWorkloadV3(result))
	routeSession.RoutingExecution = string(decision.Shape.Execution)
	routeSession.RoutingEgress = string(decision.Shape.Egress)
	routeSession.RoutingExecutionNodeID = 0
	routeSession.RoutingExecutionNodeURL = ""
	if executorNode != nil {
		routeSession.RoutingExecutionNodeID = executorNode.ID
		routeSession.RoutingExecutionNodeURL = executorNode.URL
	}
	if proxyNode != nil {
		routeSession.RoutingEgressNodeID = proxyNode.ID
		routeSession.RoutingEgressNodeURL = proxyNode.URL
	}
	transportID := ""
	routeSession.TranscodeNodeURL = ""
	routeSession.TranscodeTransportID = ""
	if transcodeNode != nil {
		transportID = transportGenerationV3(session.ID, result.Plan.PlanID)
		routeSession.TranscodeNodeURL = transcodeNode.URL
		routeSession.TranscodeTransportID = transportID
	}
	if transcodeNode != nil {
		card := identityRecipeCard(&routeSession)
		card.InputPath = file.FilePath
		card.DVProfile = file.PrimaryDVProfile()
		card.AudioOnly = file.IsAudioOnly()
		if isVirtualPlaybackFile(file) {
			card.VirtualSourceOwnerInstallationID = effectiveVirtualOwner(file.VirtualOwnerInstallationID, routeSession.VirtualSourceOwnerInstallationID)
		}
		if err := h.putRequiredNodeRecipeV3(r.Context(), transportID, card); err != nil {
			h.deleteNodeRecipeV3(r.Context(), transportID)
			authorityErr := &transportErrorV3{
				reason: string(noderouting.OutcomeCapacityUnavailable), message: "The transcode remux authority is temporarily unavailable.", retryable: true, cause: err,
			}
			return fallbackFromSelectedRoute(authorityErr)
		}
	}
	streamURL := fmt.Sprintf("/stream/%s", routeSession.ID)
	servedByProxy := false
	// priorGrant is the egress authority this attempt overwrote, if any. A
	// replan of a session that was already proxy-served must be able to put it
	// back: rolling back to the restored old plan leaves that plan's published
	// proxy URL live, and a deleted grant would 404 it.
	var priorGrant *playback.RecipeCard
	accessPath := netaccess.PathFromContext(r.Context())
	switch {
	case !mode.headerAuth:
		streamURL, servedByProxy = h.identityStreamURLV3(&routeSession, file, proxyNode, accessPath)
	case mode.proxyEgress:
		streamURL, servedByProxy, priorGrant = h.identityGrantStreamURLV3(r.Context(), &routeSession, file, proxyNode, accessPath)
	}
	reservationReleased := false
	if proxyNode != nil && !servedByProxy {
		// A planned proxy that could not be addressed (no signable token, no
		// file record, no writable grant) falls back to the local path, so its
		// reservation must be dropped now rather than pinning that node's budget
		// until it ages out.
		//
		// The fallback is local execution, so it honors the same
		// local-fallback gate the no-origins mode enforces: an authorized-origins
		// remux whose grant could not be written must not quietly spawn the
		// ffmpeg the operator disabled. Start-time escalation cannot cover this
		// case — it was legitimately skipped because the pool does offer a proxy.
		//
		// The pool did offer one, so the policy is satisfiable and only this
		// attempt's authority write failed. That is transient infrastructure, and
		// it is reported as such: the HLS path classifies the identical
		// proxy-authority failure retryable, and a non-retryable answer would
		// make a client give up on a route the next attempt can take.
		reservationReleased = true
		if transcodeNode != nil {
			h.deleteNodeRecipeV3(r.Context(), transportID)
			grantErr := &transportErrorV3{
				reason: string(noderouting.OutcomeCapacityUnavailable), message: "The transcode-to-proxy remux route is temporarily unavailable.", retryable: true,
			}
			return fallbackFromSelectedRoute(grantErr)
		}
		releaseReservation()
		if !identityLocalFallbackAllowedV3(result, policy) || h.validateLocalProgressiveCapabilitiesV3(r.Context(), result) != nil {
			releaseLifecycle()
			return preparedTransportV3{}, &transportErrorV3{
				reason:    string(noderouting.OutcomeCapacityUnavailable),
				message:   "The proxy egress route is temporarily unavailable.",
				retryable: true,
			}
		}
	}

	previousNodeURL := session.TranscodeNodeURL
	previousTransportID := remoteTransportID(session)
	committed := false
	if result.Plan != nil && result.Plan.Delivery == playback.DeliveryRemuxProgressiveV3 {
		if seek := timeline.seekSeconds; seek > 0 {
			streamURL = appendPlaybackQueryV3(streamURL, "seek", strconv.FormatFloat(seek, 'f', -1, 64))
		}
	}
	workload := routingWorkloadV3(result)
	routingExecution := decision.Shape.Execution
	routingEgress := noderouting.EgressAPI
	egressNodeID := 0
	egressNodeURL := ""
	if servedByProxy {
		routingEgress = decision.Shape.Egress
		egressNodeID = proxyNode.ID
		egressNodeURL = proxyNode.URL
	} else {
		transcodeNode = nil
		transportID = ""
		if workload == noderouting.WorkloadRemux {
			routingExecution = noderouting.ExecutionAPI
		} else {
			routingExecution = noderouting.ExecutionNone
		}
	}
	// Only a proxy that actually runs the remux is this route's executor. Direct
	// play executes nothing, so naming the proxy there would report an execution
	// node for a workload with no execution at all — the same distinction
	// jellycompat draws when it records its identity assignment.
	executorID := 0
	executorURL := ""
	if routingExecution == noderouting.ExecutionProxy {
		executorID = egressNodeID
		executorURL = egressNodeURL
	} else if routingExecution == noderouting.ExecutionTranscode && transcodeNode != nil {
		executorID = transcodeNode.ID
		executorURL = transcodeNode.URL
	}
	nodeURL := ""
	if transcodeNode != nil {
		nodeURL = transcodeNode.URL
	}
	adoptSuccessor := func() {
		h.tm.CloseTranscodeSession(session.ID, "")
		if previousNodeURL != "" {
			h.tm.StopRemoteTranscode(previousTransportID, previousNodeURL)
			h.deleteNodeRecipeV3(r.Context(), previousTransportID)
		}
		h.revokeStaleProxyGrantOnCommitV3(r.Context(), session.ID, mode, servedByProxy)
		h.applyRemoteTransportMarkV3(r.Context(), session.ID, servedByProxy)
	}
	rollbackTransport := func(requireCancellation bool) error {
		if committed {
			return nil
		}
		if requireCancellation && nodeURL != "" && transportID != "" {
			if err := h.tm.CancelRemoteTranscode(r.Context(), transportID, nodeURL); err != nil {
				// applySession has already made this the live route. Keep its
				// authority and reservation intact so a later stop can retry the
				// cancellation instead of orphaning an admitted remote stream.
				committed = true
				adoptSuccessor()
				releaseLifecycle()
				return err
			}
		}
		committed = true
		if !requireCancellation && nodeURL != "" && transportID != "" {
			h.tm.StopRemoteTranscode(transportID, nodeURL)
		}
		// The session never reached the client, so a proxy admitted for it
		// must not keep consuming that node's job/bandwidth budget until the
		// reservation ages out — nor keep an egress grant for a transport
		// that was never committed.
		if servedByProxy && !reservationReleased {
			releaseReservation()
			h.restoreProxyGrantV3(r.Context(), session.ID, priorGrant)
		}
		h.deleteNodeRecipeV3(r.Context(), transportID)
		releaseLifecycle()
		return nil
	}
	return preparedTransportV3{
		url:                streamURL,
		nodeURL:            nodeURL,
		transportID:        transportID,
		routingWorkload:    workload,
		routingExecution:   routingExecution,
		routingExecutorID:  executorID,
		routingExecutorURL: executorURL,
		routingEgress:      routingEgress,
		routingEgressID:    egressNodeID,
		routingEgressURL:   egressNodeURL,
		commit: func() *transportErrorV3 {
			if committed {
				return nil
			}
			committed = true
			adoptSuccessor()
			releaseLifecycle()
			return nil
		},
		rollback: func() {
			_ = rollbackTransport(false)
		},
		rollbackRequired: func() error { return rollbackTransport(true) },
	}, nil
}

// Virtual sources resolve through the integrated server and relay before
// remote transcode dispatch, allowing pooled transcode nodes to serve them.
func shouldUsePooledPlaybackNodeV3(file *models.MediaFile) bool {
	return file != nil
}

// planIdentityProxyV3 selects the proxy node that will serve a direct-play or
// progressive-remux session. These deliveries need no transcode node — the
// bytes are either the source file or a single remux pipe — so the planner is
// asked for a proxy alone, exactly as the Jellyfin-compat transport does.
//
// The proxy serves from the grant alone, so the grant has to carry everything
// the API-local path would have read from the session and the file record — the
// media path it opens, and the source facts (Dolby Vision profile, audio-only)
// its remux needs. Omitting either would not fail loudly: the proxy would serve
// a subtly different stream than the plan promised.
//
// The bool reports whether the returned URL is actually a proxy URL, so the
// caller can release the planner reservation when it is not. A grant that
// cannot be written is not fatal: this attempt simply stays on the API origin,
// which is exactly the behavior of a header-authenticated attempt that
// negotiated no origins at all. The third value is the grant this write
// displaced, for the caller's rollback.
//
// path is the client's access path: a proxy with no origin on it is treated
// like no proxy at all, before any grant is written, so the API relays.
func (h *PlaybackHandler) identityGrantStreamURLV3(ctx context.Context, s *playback.Session, file *models.MediaFile, proxyNode *nodepool.Node, path netaccess.Path) (string, bool, *playback.RecipeCard) {
	if proxyNode == nil || file == nil || s == nil {
		return h.playbackStreamURL(s), false, nil
	}
	base := proxyNode.ClientURLFor(path)
	if base == "" {
		return h.playbackStreamURL(s), false, nil
	}
	card := identityRecipeCard(s)
	card.InputPath = file.FilePath
	card.DVProfile = file.PrimaryDVProfile()
	card.AudioOnly = file.IsAudioOnly()
	card.RoutingEgressNodeID = proxyNode.ID
	prior, stored := h.putProxyGrantV3(ctx, s.ID, card)
	if !stored {
		return h.playbackStreamURL(s), false, nil
	}
	return base + "/stream/v3/" + s.ID, true, prior
}

// putProxyGrantV3 stores the recipe a designated proxy origin serves this
// session from, reporting whether the grant is actually retrievable. A replan
// overwrites the previous grant under the same session id, so the grant it
// displaces is returned for the caller to thread into its rollback: a
// replacement that fails to commit restores the old plan, and that plan's
// already-published proxy URL is only serviceable while its grant exists.
//
// The overwrite is deliberately not staged behind the commit. Between this Put
// and the transport commit the previously published client URL resolves the new
// recipe — same session, same user, same media authority, bounded by the replan
// window — which is the accepted cost of keeping one grant per session.
//
// A disabled store is a negative answer rather than a silent success: it
// accepts writes it cannot retrieve (the Redis-less integrated box), and
// publishing a proxy URL against one would hand the client a route that 404s.
func (h *PlaybackHandler) putProxyGrantV3(ctx context.Context, sessionID string, card playback.RecipeCard) (*playback.RecipeCard, bool) {
	if h.ProxyGrantStore == nil || !h.ProxyGrantStore.Enabled() || sessionID == "" {
		return nil, false
	}
	prior, hadPrior := h.ProxyGrantStore.Get(ctx, sessionID)
	if !hadPrior {
		prior = nil
	}
	if err := h.ProxyGrantStore.Put(ctx, sessionID, card); err != nil {
		slog.WarnContext(ctx, "protocol v3 proxy egress grant write failed; serving from the API origin",
			"component", "api", "playback_session_id", sessionID, "error", err)
		return nil, false
	}
	return prior, true
}

// restoreProxyGrantV3 undoes a replan's grant overwrite. A session that was
// already egressing from a proxy keeps serving its restored plan, so its grant
// has to come back rather than be revoked; a session that had none is revoked
// as before, because a grant for a transport that never committed would point a
// proxy at work that no longer exists.
func (h *PlaybackHandler) restoreProxyGrantV3(ctx context.Context, sessionID string, prior *playback.RecipeCard) {
	if h == nil || h.ProxyGrantStore == nil || sessionID == "" {
		return
	}
	if prior == nil {
		h.deleteProxyGrantV3(ctx, sessionID)
		return
	}
	if err := h.ProxyGrantStore.Put(context.WithoutCancel(ctx), sessionID, *prior); err != nil {
		slog.WarnContext(ctx, "failed to restore the previous proxy egress grant",
			"component", "api", "playback_session_id", sessionID, "error", err)
	}
}

// deleteProxyGrantV3 revokes a session's proxy egress authority. It runs
// wherever the session ends or its transport fails to commit: a grant that
// outlived its session would let a proxy keep serving bytes for playback the
// server considers over.
func (h *PlaybackHandler) deleteProxyGrantV3(ctx context.Context, sessionID string) {
	if h == nil || h.ProxyGrantStore == nil || sessionID == "" {
		return
	}
	if err := h.ProxyGrantStore.Delete(context.WithoutCancel(ctx), sessionID); err != nil {
		slog.WarnContext(ctx, "failed to revoke proxy egress grant",
			"component", "api", "playback_session_id", sessionID, "error", err)
	}
}

// putNodeRecipeV3 hands the transcode node the recipe it rebuilds this job from
// after a restart, keyed by the transport id the node serves it under (which is
// the id in every relayed node URL, not the playback session id).
//
// It exists because a header-authenticated attempt publishes no stream token, so
// neither the client nor this server's relay has a recipe to forward when the
// node comes back empty — the node would 404 until the client replanned. A node
// dying mid-stream is a normal event, so tokenless playback recovers from it the
// way a legacy token attempt already does.
//
// Best effort, exactly like the jellycompat handoff: the write is bounded and a
// failure only forfeits restart resilience for this session, never the start.
func (h *PlaybackHandler) putNodeRecipeV3(ctx context.Context, transportID string, card playback.RecipeCard) {
	if h == nil || h.NodeRecipeStore == nil || transportID == "" {
		return
	}
	putCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), nodeRecipeWriteTimeoutV3)
	defer cancel()
	if err := h.NodeRecipeStore.Put(putCtx, transportID, card); err != nil {
		slog.WarnContext(ctx, "persist node transcode recipe failed; this session cannot survive a node restart",
			"component", "api", "playback_session_id", card.SessionID, "transport", transportID,
			"node", card.TranscodeNodeURL, "error", err)
	}
}

// putRequiredNodeRecipeV3 persists the active authority for a progressive
// remux executed by a transcode node. Unlike the reconstruction-only write
// above, this one is part of route admission: the node checks the record before
// every new remux request, and stop deletes it, so a still-valid stream token
// cannot resurrect FFmpeg after a node replacement.
func (h *PlaybackHandler) putRequiredNodeRecipeV3(ctx context.Context, transportID string, card playback.RecipeCard) error {
	if h == nil || h.NodeRecipeStore == nil || !h.NodeRecipeStore.Enabled() || transportID == "" {
		return errors.New("node recipe store unavailable")
	}
	putCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), nodeRecipeWriteTimeoutV3)
	defer cancel()
	return h.NodeRecipeStore.Put(putCtx, transportID, card)
}

// deleteNodeRecipeV3 drops a transport's stored recipe so a buffered or retrying
// request cannot resurrect a transcode the server has replaced or ended. The
// store's TTL is only the backstop for the paths that never run (a crashed API
// process); every deliberate teardown deletes here.
func (h *PlaybackHandler) deleteNodeRecipeV3(ctx context.Context, transportID string) {
	if h == nil || h.NodeRecipeStore == nil || transportID == "" {
		return
	}
	if err := h.NodeRecipeStore.Delete(context.WithoutCancel(ctx), transportID); err != nil {
		slog.WarnContext(ctx, "failed to drop the node transcode recipe",
			"component", "api", "transport", transportID, "error", err)
	}
}

// deleteRequiredProgressiveRemuxAuthorityV3 makes a user-visible stop durable
// before the session and its retry path disappear. A progressive remux token
// can remain valid for 24 hours, so an unavailable authority store is a failed
// stop, not a best-effort cleanup: the caller must retain the session and let
// the client retry.
func (h *PlaybackHandler) deleteRequiredProgressiveRemuxAuthorityV3(ctx context.Context, session *playback.Session) error {
	if !requiresProgressiveRemuxAuthorityV3(session) {
		return nil
	}
	if h == nil || h.NodeRecipeStore == nil || !h.NodeRecipeStore.Enabled() {
		return errors.New("node recipe store unavailable")
	}
	deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), nodeRecipeWriteTimeoutV3)
	defer cancel()
	return h.NodeRecipeStore.Delete(deleteCtx, session.TranscodeTransportID)
}

func requiresProgressiveRemuxAuthorityV3(session *playback.Session) bool {
	return session != nil && session.TranscodeTransportID != "" && session.TranscodeNodeURL != "" &&
		session.RoutingWorkload == string(noderouting.WorkloadRemux) &&
		session.RoutingExecution == string(noderouting.ExecutionTranscode)
}

// resolveIdentityRouteV3 resolves direct and progressive routes through the
// same policy compiler as HLS, including the transcode-execution/proxy-egress
// progressive route.
func (h *PlaybackHandler) resolveIdentityRouteV3(r *http.Request, sessionID string, result playback.PlannerResultV3, mode mediaAuthModeV3, policy config.PlaybackRoutingPolicy, initialExcludedShapes map[string]struct{}) (noderouting.Decision, *transportErrorV3) {
	workload, delivery, ok := routingClassV3(result)
	if !ok {
		return noderouting.Decision{}, &transportErrorV3{reason: string(noderouting.OutcomePolicyUnsatisfied), message: "The playback delivery has no legal node route.", retryable: false}
	}
	proxyAllowed := mode.proxyEgress || (!mode.headerAuth && h.JWTSecret != "")
	excludedShapes := maps.Clone(initialExcludedShapes)
	if excludedShapes == nil {
		excludedShapes = make(map[string]struct{})
	}
	if result.Plan != nil && result.Plan.Delivery == playback.DeliveryRemuxProgressiveV3 &&
		h.validateLocalProgressiveCapabilitiesV3(r.Context(), result) != nil {
		excludedShapes[noderouting.ShapeProgressiveRemuxAPI] = struct{}{}
	}
	if delivery == noderouting.DeliveryProgressiveRemux &&
		(h.NodeRecipeStore == nil || !h.NodeRecipeStore.Enabled()) {
		// A signed stream token can outlive a transcode-node process by 24 hours.
		// Without durable active-transport authority, a stopped remux could be
		// replayed after that node restarts and start FFmpeg again.
		excludedShapes[noderouting.ShapeProgressiveRemuxTranscodeProxy] = struct{}{}
	}
	var relayEligible func(*nodepool.Node) bool
	if delivery == noderouting.DeliveryProgressiveRemux {
		relayEligible = h.identityProxyRelayEligibilityV3(r.Context())
	}
	// Both proxy predicates are narrowed to proxies the client can actually
	// reach on its access path (see resolveHLSRouteWithPolicyV3). With no
	// reachable proxy the resolver falls through to the API-egress shapes, or
	// reports capacity unavailable under a proxy_only policy.
	accessPath := netaccess.PathFromContext(r.Context())
	decision, err := noderouting.Resolve(noderouting.AdaptSessionPlanner(h.NodePlanner), noderouting.ResolveRequest{
		Request: noderouting.Request{
			Workload: workload, Delivery: delivery,
			Policy: policy, ProxyAllowed: proxyAllowed,
		},
		SessionID: sessionID, EstimatedBitrateKbps: identityStreamBitrateKbpsV3(result),
		TranscodeEligible: h.transcodeEligibilityV3(r.Context(), result, nil), ProxyEligible: nodepool.ClientReachableVia(accessPath, relayEligible),
		ProxyExecutionEligible: nodepool.ClientReachableVia(accessPath, h.identityProxyEligibilityV3(r.Context(), result)), ExcludedShapeIDs: excludedShapes,
	})
	if err != nil {
		return noderouting.Decision{}, &transportErrorV3{reason: string(noderouting.OutcomePolicyUnsatisfied), message: "The playback routing policy is invalid.", retryable: false, cause: err}
	}
	if !decision.Selected() {
		retryable := decision.Outcome == noderouting.OutcomeCapacityUnavailable
		return noderouting.Decision{}, &transportErrorV3{reason: string(decision.Outcome), message: "No playback route satisfies the configured policy and current node availability.", retryable: retryable}
	}
	return decision, nil
}

func (h *PlaybackHandler) identityProxyRelayEligibilityV3(ctx context.Context) func(*nodepool.Node) bool {
	enumerator, ok := h.NodePlanner.(proxyNodeEnumeratorV3)
	if !ok {
		return func(*nodepool.Node) bool { return false }
	}
	capable := make(map[string]struct{})
	for nodeURL, entry := range h.pooledNodeCapabilitiesV3(ctx, enumerator.ProxyNodeURLs()) {
		if slices.Contains(entry.transportFeatures, playback.TransportFeatureProgressiveRemuxRelayV1) {
			capable[nodeURL] = struct{}{}
		}
	}
	return func(node *nodepool.Node) bool {
		if node == nil {
			return false
		}
		_, supported := capable[node.URL]
		return supported
	}
}

// applyRemoteTransportMarkV3 records whether the committed route's media bytes
// leave this server or another node.
//
// Every committed transition calls it, including the ones that serve locally:
// a session replanned from a proxy onto the integrated transcoder would
// otherwise keep a stale mark, and the widened idle grace it grants would hold
// that session's stream and transcode slots for five minutes after the local
// stream disconnected without an explicit stop.
func (h *PlaybackHandler) applyRemoteTransportMarkV3(ctx context.Context, sessionID string, remote bool) {
	if err := h.sessionMgr.SetRemoteTransport(sessionID, remote); err != nil &&
		!errors.Is(err, playback.ErrSessionNotFound) {
		slog.WarnContext(ctx, "failed to record transport locality",
			"component", "api", "playback_session_id", sessionID, "remote", remote, "error", err)
	}
}

// identityProxyEligibilityV3 narrows proxy selection to proxies that can
// execute the plan's frozen recipe when one is required. A nil predicate means
// any healthy proxy will do.
//
// The narrowing happens before selection rather than after, mirroring how HLS
// filters transcode nodes: rejecting a single round-robin pick would abandon
// the whole pool, so a capable sibling with free capacity would sit unused
// while playback fell back to the API — or was refused outright when policy
// forbids API execution.
//
// Direct play copies bytes and needs no recipe, so it skips the probe entirely
// and any healthy proxy serves it.
func (h *PlaybackHandler) identityProxyEligibilityV3(ctx context.Context, result playback.PlannerResultV3) func(*nodepool.Node) bool {
	enumerator, enumerable := h.NodePlanner.(proxyNodeEnumeratorV3)
	if !enumerable || !planRequiresServerTransformationsV3(result.Plan) {
		return nil
	}

	capable := make(map[string]struct{})
	for nodeURL, advertised := range h.pooledNodeTransformationsV3(ctx, enumerator.ProxyNodeURLs()) {
		if validateAdvertisedTransformationsV3(result.Plan, advertised) == nil {
			capable[nodeURL] = struct{}{}
		}
	}
	if len(capable) == 0 {
		slog.WarnContext(ctx, "protocol v3 no proxy advertises the planned recipe",
			"component", "api", "delivery", result.Plan.Delivery)
	}
	// The predicate runs under the planner lock: a set lookup only.
	return func(node *nodepool.Node) bool {
		if node == nil {
			return false
		}
		_, ok := capable[node.URL]
		return ok
	}
}

func identityLocalFallbackAllowedV3(result playback.PlannerResultV3, policy config.PlaybackRoutingPolicy) bool {
	workload, _, ok := routingClassV3(result)
	if !ok {
		return false
	}
	if workload == noderouting.WorkloadDirectPlay {
		return policy.DirectPlayEgress != config.PlaybackEgressProxyOnly
	}
	return policy.RemuxExecution != config.PlaybackExecutionWorkerOnly &&
		policy.RemuxEgress != config.PlaybackEgressProxyOnly
}

// plannerInputV3 assembles the planner input for one route decision. The
// escalation below re-plans with the same inputs the original decision used,
// plus the refused route's attempt key. The HLS registries and tone-map
// capabilities are deliberately left unset: planPlaybackWithCapabilitiesV3
// installs its own lazily memoized snapshot, so the inputs can never disagree.
func (h *PlaybackHandler) plannerInputV3(ctx context.Context, req playback.StartRequestV3, requestedFile, effectiveFile *models.MediaFile, audioIndex int, attemptedKeys []string, provenance ProbeProvenance) playback.PlannerInputV3 {
	return playback.PlannerInputV3{
		Request:              req,
		RequestedFile:        requestedFile,
		EffectiveFile:        effectiveFile,
		ServerBitrateCapKbps: serverBitrateCapV3(ctx),
		AudioTrackIndex:      audioIndex,
		Settings:             h.plannerSettingsV3(ctx),
		Registry:             h.transformationRegistryV3(ctx),
		DVRPUStrippable:      h.lazyDVRPUStrippableV3(ctx, effectiveFile),
		Now:                  time.Now(),
		AttemptedKeys:        attemptedKeys,
		AdditionalSubtitles:  h.downloadedSubtitleInventoryV3(ctx, effectiveFile),
		// The escalation rebuilds the same decision from the same source, so the
		// selected source's provenance travels with it. Dropping it here would
		// publish an empty provenance on an escalated plan whose primary plan
		// carried one.
		InventoryProvenance: string(provenance),
	}
}

// escalateRefusedProgressiveRemuxV3 replaces a progressive remux that the
// header-authenticated transport is guaranteed to refuse.
//
// Without authorized media origins that mode bypasses the proxy identity routes
// (a proxy authenticates from the signed URL token this mode exists to remove),
// so any remux left on the API is work with nowhere to run once policy forbids
// API execution — the route resolver turns it into a terminal routing conflict
// that nothing will ever satisfy. remux_execution=worker_only refuses the
// container-only copy exactly as it refuses one carrying server
// transformations, so both escalate: what decides is whether the resolver
// leaves this attempt any progressive route at all, not how heavy the recipe
// is. HLS is the same recipe on a delivery the API can relay from a pooled
// transcode node, so plan it here rather than making the client discover the
// refusal and recover through a replan round trip.
//
// A client that cannot execute an HLS delivery has no such alternative: it gets
// a non-retryable error naming the policy, because retrying is exactly what it
// must not do.
//
// An attempt that negotiated authorized media origins has an executor again —
// a proxy runs the remux from its grant, exactly as it does for a legacy
// attempt — so nothing is escalated while the pool actually offers a proxy.
// With origins negotiated but no proxy configured the refusal is back, and so
// is this escalation.
//
// plannerInput is evaluated only on the escalation path: rebuilding it costs a
// settings resolution and a downloaded-subtitle listing, which the overwhelming
// majority of starts must not pay for a route they never take.
func (h *PlaybackHandler) escalateRefusedProgressiveRemuxV3(ctx context.Context, mode mediaAuthModeV3, plannerInput func() playback.PlannerInputV3, result playback.PlannerResultV3) (playback.PlannerResultV3, *transportErrorV3) {
	if result.Terminal != nil || result.Plan == nil ||
		result.Plan.Delivery != playback.DeliveryRemuxProgressiveV3 ||
		h.progressiveRemuxRouteAvailableV3(ctx, mode) {
		return result, nil
	}
	input := plannerInput()
	outputContextID := input.Request.ClientPlaybackContext.Output.OutputContextID
	next := input
	next.AttemptedKeys = append(append([]string(nil), input.AttemptedKeys...),
		playback.PlanAttemptKeyV3(*result.Plan, outputContextID, nil))
	next.Now = time.Now()
	escalated, capabilityErr := h.planPlaybackWithCapabilitiesV3(ctx, next)
	if capabilityErr != nil {
		// The escalation target depends on a capability lookup that failed, so
		// nothing is known about whether HLS would run. Ask the client to retry
		// rather than report the remux terminal as final.
		return result, &transportErrorV3{
			reason:    capabilityUnavailableReasonV3,
			message:   "Transcode capability validation is temporarily unavailable.",
			retryable: true,
			cause:     capabilityErr,
		}
	}
	if escalated.Terminal != nil || escalated.Plan == nil || escalated.Plan.Delivery == playback.DeliveryRemuxProgressiveV3 {
		reason := ""
		if escalated.Terminal != nil {
			reason = escalated.Terminal.Reason
		}
		slog.WarnContext(ctx, "protocol v3 header-authenticated remux has no executable delivery",
			logComponentKey, playbackLogValueV3,
			"delivery", result.Plan.Delivery,
			"replanned_terminal_reason", reason,
		)
		return result, &transportErrorV3{
			reason:  "local_transcode_disabled",
			message: "This server does not run playback conversions locally, and the client accepts no delivery that a transcode node can serve.",
		}
	}
	slog.InfoContext(ctx, "protocol v3 escalated refused progressive remux",
		logComponentKey, playbackLogValueV3,
		"delivery", escalated.Plan.Delivery,
		"decision_reason", escalated.Plan.DecisionReason,
	)
	return escalated, nil
}

// progressiveRemuxRouteAvailableV3 reports whether policy leaves this attempt
// any progressive-remux route it could actually reach: an API-egress shape the
// API may execute, or a proxy-egress shape whose proxies this client can
// address. It gates the escalation above, so a false answer means the delivery
// is refused for every candidate, not merely that the preferred one is busy.
func (h *PlaybackHandler) progressiveRemuxRouteAvailableV3(ctx context.Context, mode mediaAuthModeV3) bool {
	proxyAllowed := mode.proxyEgress || (!mode.headerAuth && h.JWTSecret != "")
	compiled, err := noderouting.Candidates(noderouting.Request{
		Workload: noderouting.WorkloadRemux, Delivery: noderouting.DeliveryProgressiveRemux,
		Policy: h.playbackRoutingPolicyForContextV3(ctx), ProxyAllowed: proxyAllowed,
	})
	if err != nil {
		return false
	}
	for _, shape := range compiled.Candidates {
		if shape.Egress == noderouting.EgressAPI {
			return true
		}
		if mode.headerAuth {
			if h.proxyEgressOriginsAvailableV3() {
				return true
			}
			continue
		}
		enumerator, ok := h.NodePlanner.(proxyNodeEnumeratorV3)
		if ok && h.JWTSecret != "" && len(enumerator.ProxyNodeURLs()) > 0 {
			return true
		}
	}
	return false
}

// identityStreamBitrateKbpsV3 estimates the bitrate a proxy will egress for an
// identity delivery, so bandwidth-capped proxies admit it accurately. The plan's
// effective recipe is authoritative (a remux that downmixes audio egresses less
// than the source); the source descriptor is the fallback.
func identityStreamBitrateKbpsV3(result playback.PlannerResultV3) int {
	if result.Plan == nil {
		return 0
	}
	if effective := result.Plan.EffectiveRecipe.BitrateKbps; effective != nil && *effective > 0 {
		return *effective
	}
	if source := result.Plan.Source.BitrateKbps; source > 0 {
		return source
	}
	return 0
}

// identityStreamURLV3 builds the stream URL for a direct-play or
// progressive-remux session: an absolute proxy URL when one was planned,
// otherwise the API-local path.
//
// The proxy serves from the token alone, so the token has to carry everything
// the API-local path would have read from the session and the file record: the
// media path it opens, and the Dolby Vision profile its remux needs to strip a
// dangling Profile 7 RPU. Omitting either would not fail loudly — the proxy
// would serve a subtly different stream than the plan promised.
//
// The bool reports whether the returned URL is actually a proxy URL, so the
// caller can release the planner reservation when it is not.
//
// A session that requires media authorization never gets a proxy URL: the proxy
// serves from the signed token alone, which is exactly the credential that mode
// keeps out of client-visible URLs. It falls back to the API-local path, whose
// builder omits the token for the same reason.
//
// path is the client's access path; a proxy with no origin on it falls back
// the same way, and the caller releases the reservation.
func (h *PlaybackHandler) identityStreamURLV3(s *playback.Session, file *models.MediaFile, proxyNode *nodepool.Node, path netaccess.Path) (string, bool) {
	if proxyNode == nil || file == nil || (s != nil && s.RequireMediaAuthorization) {
		return h.playbackStreamURL(s), false
	}
	base := proxyNode.ClientURLFor(path)
	if base == "" {
		return h.playbackStreamURL(s), false
	}
	card := identityRecipeCard(s)
	card.InputPath = file.FilePath
	card.RoutingEgressNodeID = proxyNode.ID
	claims := card.ToClaims()
	claims.DVProfile = file.PrimaryDVProfile()
	claims.AudioOnly = file.IsAudioOnly()
	token := h.signStreamClaims(claims)
	if token == "" {
		return h.playbackStreamURL(s), false
	}
	if s.PlayMethod == playback.PlayRemux {
		if claims.PlayMethod == streamtoken.PlayMethodAudioDownmixRemux {
			return base + "/stream/remux/audio-v2/" + token, true
		}
		return base + "/stream/remux/" + token, true
	}
	return base + "/stream/direct/" + token, true
}

// sessionOwnsResumeTimelineV3 reports whether the session's own position is a
// valid resume point for the item it belongs to.
//
// Resume state is keyed on the item (playbackProgressTarget resolves a file to
// its episode or content ID), but every part of a multipart presentation shares
// that key while carrying its own file-local clock. Persisting part 4's
// position would therefore store "12 minutes into the book" as the book's
// resume point. The client that stitches the parts into one timeline is the
// only party that knows the item-absolute position, and it reports that through
// the sync/progress surface instead.
//
// This is derived rather than requested: the mismatch is a property of the
// media, not of the client, so a client that forgot to ask would corrupt resume
// exactly the same way.
func sessionOwnsResumeTimelineV3(file *models.MediaFile) bool {
	return file == nil || file.PresentationPartTotal <= 1
}

// multipartResumeFileV3 maps an item-absolute resume position to the part
// whose local timeline should be planned. It only returns a mapping when all
// ordered parts have positive durations; guessing across incomplete metadata
// would seek to the wrong file, so callers retain the existing safe fallback.
func (h *PlaybackHandler) multipartResumeFileV3(ctx context.Context, file *models.MediaFile, absolute float64, access catalog.AccessFilter) (*models.MediaFile, float64, error) {
	if h == nil || h.FileVersionFetcher == nil || file == nil || file.PresentationPartTotal <= 1 || absolute <= 0 {
		return nil, 0, nil
	}
	parts, err := h.FileVersionFetcher.GetByContentID(ctx, file.ContentID)
	if err != nil {
		return nil, 0, err
	}
	// The requested file has already passed requestAccessFilter. Keep the
	// resume timeline inside that same media folder; a content ID may be shared
	// by copies in several libraries, and an unscoped part lookup could move
	// playback onto a folder the viewer cannot access.
	parts = slices.DeleteFunc(parts, func(part *models.MediaFile) bool {
		return part == nil || part.MediaFolderID != file.MediaFolderID || !catalog.FileAllowedByAccess(part, access)
	})
	if len(parts) != file.PresentationPartTotal {
		return nil, 0, fmt.Errorf("multipart sequence incomplete")
	}
	for _, part := range parts {
		if part.PresentationGroupKey != file.PresentationGroupKey || part.PresentationPartTotal != file.PresentationPartTotal || part.PresentationPartIndex < 1 || part.PresentationPartIndex > file.PresentationPartTotal {
			return nil, 0, fmt.Errorf("multipart sequence inconsistent")
		}
	}
	parts = slices.Clone(parts)
	slices.SortStableFunc(parts, func(a, b *models.MediaFile) int {
		if a == nil && b == nil {
			return 0
		}
		if a == nil {
			return 1
		}
		if b == nil {
			return -1
		}
		if a.PresentationPartIndex != b.PresentationPartIndex {
			return a.PresentationPartIndex - b.PresentationPartIndex
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	for i, part := range parts {
		if part.PresentationPartIndex != i+1 {
			return nil, 0, fmt.Errorf("multipart sequence has missing part")
		}
	}
	var offset float64
	for _, part := range parts {
		if part == nil || part.Duration <= 0 {
			return nil, 0, fmt.Errorf("part duration unavailable")
		}
		end := offset + float64(part.Duration)
		if absolute < end || part == parts[len(parts)-1] {
			local := absolute - offset
			if local < 0 {
				local = 0
			}
			if local > float64(part.Duration) {
				local = float64(part.Duration)
			}
			return part, local, nil
		}
		offset = end
	}
	return nil, 0, nil
}

// committedStartAudioIntentV3 is the durable audio selection intent for one
// committed start: the origin, the resolved preference language, the series
// snapshot and the signature of the committed track.
type committedStartAudioIntentV3 struct {
	origin           string
	preferredLang    string
	seriesSignature  *userstore.AudioTrackSignature
	selectedOverride *userstore.AudioTrackSignature
}

// virtualPlanSelectionOriginForRequestV3 names the audio selection origin
// for a start that never passed through the omitted-selection resolution
// above (decode-rejection rotations, version fallback alternates): explicit
// when the request names a track, auto otherwise.
func virtualPlanSelectionOriginForRequestV3(req playback.StartRequestV3) string {
	if strings.TrimSpace(req.AudioTrackID) != "" || req.AudioTrackIndex != nil || strings.TrimSpace(req.CarriedAudioTrackID) != "" {
		return SelectionOriginExplicit
	}
	return SelectionOriginAuto
}

// intentForCommittedStartV3 snapshots the audio selection intent for the
// committed start. An omitted audio selection resolves through the same
// preference pipeline the catalog menu used (identical inputs: the committed
// audio index, the resolved language and the stored series preference), so
// reconciliation replays exactly the start's decision against the verified
// inventory. An explicit selection is marked explicit with only its committed
// signature: reconciliation may verify that it still exists, never move it.
func intentForCommittedStartV3(ctx context.Context, h *PlaybackHandler, userID int, profileID string, req playback.StartRequestV3, file *models.MediaFile, committedIndex int, decision virtualPlanDecisionV3) (committedStartAudioIntentV3, error) {
	var intent committedStartAudioIntentV3
	if file != nil && committedIndex >= 0 && committedIndex < len(file.AudioTracks) {
		intent.selectedOverride = playback.AudioTrackSignatureFromTrack(file.AudioTracks[committedIndex])
	}
	if strings.TrimSpace(req.AudioTrackID) != "" || req.AudioTrackIndex != nil || strings.TrimSpace(req.CarriedAudioTrackID) != "" {
		intent.origin = SelectionOriginExplicit
		return intent, nil
	}
	if decision.startAudioSelectionOrigin != "" {
		intent.origin = decision.startAudioSelectionOrigin
	} else {
		intent.origin = SelectionOriginAuto
	}
	intent.preferredLang = strings.TrimSpace(decision.preferredAudioLanguage)
	if h == nil || h.StoreProvider == nil {
		return intent, nil
	}
	store, err := h.StoreProvider.ForUser(ctx, userID)
	if err != nil {
		return committedStartAudioIntentV3{}, err
	}
	seriesID := h.resolveSeriesID(ctx, file)
	if seriesID != "" {
		stored, prefErr := store.GetAudioPreference(ctx, profileID, seriesID)
		if prefErr != nil {
			return committedStartAudioIntentV3{}, prefErr
		}
		if stored != nil {
			intent.seriesSignature = stored.TrackSignature
		}
	}
	return intent, nil
}

// preferredAudioTrackIndexV3 answers what an omitted audio track means: the
// language this profile has settled on for this series, this library, this
// device, or generally — the same resolution the catalog performs when it
// publishes `effective_audio_track_index`, so the track a client sees on the
// detail page is the track that plays when it does not ask for one.
//
// The client sends a track identity only when the viewer picked one. Defaulting
// to ordinal zero instead would silently play the first track on the reel.
func (h *PlaybackHandler) preferredAudioTrackIndexV3(ctx context.Context, userID int, profileID, deviceID string, file *models.MediaFile) (int, string, error) {
	if file == nil || len(file.AudioTracks) == 0 || h.StoreProvider == nil {
		return 0, "", nil
	}
	store, err := h.StoreProvider.ForUser(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, "protocol v3 start: audio preference store lookup failed", "component", "api", "user_id", userID, "error", err)
		return 0, "", err
	}
	seriesID := h.resolveSeriesID(ctx, file)
	var seriesPref *playback.AudioTrackPreference
	if seriesID != "" {
		stored, prefErr := store.GetAudioPreference(ctx, profileID, seriesID)
		if prefErr != nil {
			slog.ErrorContext(ctx, "protocol v3 start: series audio preference lookup failed", "component", "api", "profile_id", profileID, "series_id", seriesID, "error", prefErr)
			return 0, "", prefErr
		}
		if stored != nil {
			seriesPref = &playback.AudioTrackPreference{AudioTrackIndex: stored.AudioTrackIndex, AudioLanguage: stored.AudioLanguage, TrackSignature: stored.TrackSignature}
		}
	}
	rc := settingsresolve.Context{
		ProfileID:  profileID,
		DeviceID:   deviceID,
		LibraryIDs: []int{file.MediaFolderID},
	}
	if seriesID != "" {
		rc.SeriesIDs = []string{seriesID}
	}
	preferredLang, err := resolvedPlaybackAudioLanguage(ctx, store, rc)
	if err != nil {
		return 0, "", err
	}
	if playback.IsOriginalLanguagePreference(preferredLang) {
		preferredLang = h.resolveOriginalLanguage(ctx, file)
		if preferredLang == "" {
			var fallbackErr error
			preferredLang, fallbackErr = resolvedPlaybackAudioLanguage(ctx, store, settingsresolve.Context{ProfileID: profileID})
			if fallbackErr != nil {
				slog.WarnContext(ctx, "protocol v3 start: profile audio preference fallback failed", "component", "api", "profile_id", profileID, "error", fallbackErr)
				preferredLang = ""
			}
			if playback.IsOriginalLanguagePreference(preferredLang) {
				preferredLang = h.resolveOriginalLanguage(ctx, file)
			}
		}
	}
	if seriesPref != nil {
		// The specialized row supplies concrete track identity; canonical
		// settings own the language and its scope precedence.
		seriesPref.AudioLanguage = preferredLang
	}
	return normalizeAudioTrackIndex(file, playback.SelectAudioTrack(file.AudioTracks, preferredLang, seriesPref)), preferredLang, nil
}

// resumePositionV3 answers what an omitted `start_position` means: resume where
// this profile left off. It runs before planning rather than after session
// creation because the plan's timeline is cut at the start position — a route
// chosen for zero and then seeked to 40 minutes is a different route.
//
// A client that wants to start over sends an explicit `start_position: 0`; only
// omission asks the server for its resume policy. Multipart progress is stored
// as one item-level position and is translated to a part-local seek only when
// the complete ordered part timeline is available.
func (h *PlaybackHandler) resumePositionV3(ctx context.Context, userID int, profileID string, file *models.MediaFile) (*float64, error) {
	if h.StoreProvider == nil {
		return nil, nil
	}
	targetID := playbackProgressTarget(file)
	if targetID == "" {
		return nil, nil
	}
	store, err := h.StoreProvider.ForUser(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, "protocol v3 start: resume store lookup failed", "component", "api", "user_id", userID, "error", err)
		return nil, err
	}
	progress, err := store.GetProgress(ctx, profileID, targetID)
	if err != nil {
		slog.ErrorContext(ctx, "protocol v3 start: resume progress lookup failed", "component", "api", "target", targetID, "error", err)
		return nil, err
	}
	if progress == nil || progress.Completed || progress.PositionSeconds <= 0 {
		return nil, nil
	}
	position := progress.PositionSeconds
	return &position, nil
}

// A copy remux starts at the preceding keyframe selected by the demuxer, not
// necessarily at the requested source position. Its player clock therefore
// begins at the resolved stream origin and advances through the copied pre-roll
// before reaching the requested position. Neither progressive nor growing HLS
// copy transports can seek arbitrarily inside their current response.
func configureCopyRemuxTimelineV3(plan *playback.PlanV3, origin float64) {
	if plan == nil {
		return
	}
	plan.Timeline.PlayerStartSeconds = max(0, plan.Timeline.SourceStartSeconds-origin)
	plan.Timeline.StreamOriginSeconds = origin
	plan.Timeline.TimelineOffsetSeconds = origin
	plan.Timeline.SeekWindowStartSeconds = &origin
	plan.Timeline.SeekWindowEndSeconds = nil
	plan.Timeline.CanSeekAnywhere = false
	plan.Timeline.SeekRestoration = "source_position"
}

func appendPlaybackQueryV3(rawURL, key, value string) string {
	separator := "?"
	if strings.ContainsRune(rawURL, '?') {
		separator = "&"
	}
	return rawURL + separator + key + "=" + value
}

// softwareToneMapRetryOptsV3 returns the software executor for a one-shot
// hardware fallback when the recipe is allowed to adapt to live capabilities.
func (h *PlaybackHandler) softwareToneMapRetryOptsV3(ctx context.Context, opts playback.TranscodeOpts, frozenSourceMetadata bool) (playback.TranscodeOpts, bool) {
	// The software executor forces HWAccel=none, so it decodes and encodes on
	// the CPU. gpu_only forbids that fallback even when the frozen tone-map
	// policy would otherwise permit it.
	if !h.softwareFallbackAllowedV3() {
		return opts, false
	}
	if frozenSourceMetadata || opts.ToneMapMode != tonemap.ModeHardware ||
		!opts.ToneMapPolicy.Allows(tonemap.ModeSoftware) {
		return opts, false
	}
	capabilities, err := h.localToneMapCapabilitiesForTransportV3(ctx)
	if err != nil {
		return opts, false
	}
	if !capabilities.Supports(tonemap.ModeSoftware, opts.ToneMapSourceKind) {
		return opts, false
	}
	opts.ToneMapMode = tonemap.ModeSoftware
	opts.ToneMapFilter = capabilities.FilterFor(tonemap.ModeSoftware, opts.ToneMapSourceKind)
	opts.HWAccel = playback.HWAccelNone
	return opts, true
}

type localTransportStartupFailureV3 struct {
	cause         error
	failedToStart bool
	wasRunning    bool
	failedDevice  string
}

// startReadyLocalPlaybackTransportV3 returns only after the first manifest is
// safe to serve. A session that fails readiness is closed before the failure is
// returned so every caller gets the same startup cleanup behavior.
func (h *PlaybackHandler) startReadyLocalPlaybackTransportV3(ctx context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, *localTransportStartupFailureV3) {
	startedAt := time.Now()
	ts, err := h.startLocalPlaybackTransport(ctx, opts)
	spawnFinishedAt := time.Now()
	if err != nil {
		slog.InfoContext(ctx, "playback transport startup timing",
			logComponentKey, playbackLogValueV3,
			requestIDLogKeyV3, chimw.GetReqID(ctx),
			"transport", "local",
			"session", opts.SessionID,
			"spawn_ms", spawnFinishedAt.Sub(startedAt).Milliseconds(),
			"manifest_wait_ms", int64(0),
			"total_ms", time.Since(startedAt).Milliseconds(),
			"outcome", "spawn_failed",
		)
		// A revoked generation surfaces its decode verdict through the
		// transport start: it is a readiness outcome (the process ran and
		// rejected the bytes), not a spawn failure, so the rotation loop
		// must see it as such instead of a generic transcode_start_failed.
		if errors.Is(err, playback.ErrSourceDecodeRejected) {
			return nil, &localTransportStartupFailureV3{cause: err}
		}
		return nil, &localTransportStartupFailureV3{cause: err, failedToStart: true}
	}
	if _, err := ts.WaitForManifestContext(ctx, playback.ManifestStartupTimeout); err != nil {
		slog.InfoContext(ctx, "playback transport startup timing",
			logComponentKey, playbackLogValueV3,
			requestIDLogKeyV3, chimw.GetReqID(ctx),
			"transport", "local",
			"session", opts.SessionID,
			"spawn_ms", spawnFinishedAt.Sub(startedAt).Milliseconds(),
			"manifest_wait_ms", time.Since(spawnFinishedAt).Milliseconds(),
			"total_ms", time.Since(startedAt).Milliseconds(),
			"outcome", "readiness_failed",
		)
		failure := &localTransportStartupFailureV3{
			cause:        err,
			wasRunning:   ts.IsRunning(),
			failedDevice: ts.Opts().HWDevice,
		}
		_ = ts.Close()
		return nil, failure
	}
	slog.InfoContext(ctx, "playback transport startup timing",
		logComponentKey, playbackLogValueV3,
		requestIDLogKeyV3, chimw.GetReqID(ctx),
		"transport", "local",
		"session", opts.SessionID,
		"spawn_ms", spawnFinishedAt.Sub(startedAt).Milliseconds(),
		"manifest_wait_ms", time.Since(spawnFinishedAt).Milliseconds(),
		"total_ms", time.Since(startedAt).Milliseconds(),
		"outcome", transportStartupReadyV3,
	)
	return ts, nil
}

// newAutoTranscodePipelineV3 prepares the hw_accel=auto fallback order for a
// local start. The pipeline is disabled for every other request.
func (h *PlaybackHandler) newAutoTranscodePipelineV3(ctx context.Context, opts playback.TranscodeOpts) *playback.AutoTranscodePipeline {
	if h.autoTranscodePipelineV3 != nil {
		return h.autoTranscodePipelineV3(ctx, opts)
	}
	return playback.NewAutoTranscodePipeline(ctx, opts)
}

// localAutoStartupBudgetV3 bounds a local hw_accel=auto readiness-gated start.
// The remote path budgets TranscodeStartReadyMaxDuration + 5s for the same
// fallback; the local path runs runTranscodeStartup in-process, where each of
// the MaxAutoTranscodeStartupAttempts paths may consume its own
// ManifestStartupTimeout. A variable so tests can shrink it.
var localAutoStartupBudgetV3 = time.Duration(playback.MaxAutoTranscodeStartupAttempts)*playback.ManifestStartupTimeout + 5*time.Second

// startReadyAutoLocalPlaybackTransportV3 walks an enabled hw_accel=auto
// pipeline until one path produces its first manifest. Failed attempts are
// closed by the pipeline loop, so the failure carries the same cleanup
// guarantee as startReadyLocalPlaybackTransportV3.
//
// The whole walk is bounded by one explicit budget (maxAttempts × per-attempt
// timeout + margin), mirroring remotePlaybackTransportTimeout on the remote
// path. Without it the walk runs under the bare request context, so 3 × 30s of
// fallback could outlive the handler's own deadline and surface as a canceled
// request instead of a classified readiness failure.
func (h *PlaybackHandler) startReadyAutoLocalPlaybackTransportV3(ctx context.Context, pipeline *playback.AutoTranscodePipeline) (*playback.TranscodeSession, *localTransportStartupFailureV3) {
	startedAt := time.Now()
	attempts := 0
	ts, err := playback.StartReadyTranscode(ctx, pipeline, playback.TranscodeStartup{
		Budget: localAutoStartupBudgetV3,
		Start: func(ctx context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			attempts++
			return h.startLocalPlaybackTransport(ctx, opts)
		},
	})
	outcome := transportStartupReadyV3
	var failure *localTransportStartupFailureV3
	var startupErr *playback.TranscodeStartupError
	switch {
	case err == nil:
	case errors.As(err, &startupErr):
		outcome = "readiness_failed"
		failure = &localTransportStartupFailureV3{
			cause:        startupErr.Err,
			wasRunning:   startupErr.WasRunning,
			failedDevice: startupErr.FailedDevice,
		}
	case errors.Is(err, playback.ErrSourceDecodeRejected):
		// Same revoked-generation verdict as the non-auto path below: a
		// readiness outcome for rotation, never a spawn failure.
		outcome = "readiness_failed"
		failure = &localTransportStartupFailureV3{cause: err}
	default:
		outcome = "spawn_failed"
		failure = &localTransportStartupFailureV3{cause: err, failedToStart: true}
	}
	slog.InfoContext(ctx, "playback transport startup timing",
		logComponentKey, playbackLogValueV3,
		requestIDLogKeyV3, chimw.GetReqID(ctx),
		"transport", "local",
		"session", pipeline.Current().SessionID,
		"attempts", attempts,
		"total_ms", time.Since(startedAt).Milliseconds(),
		"outcome", outcome,
	)
	return ts, failure
}

// prepareLocalTransportV3 starts a local HLS generation for the selected plan.
func (h *PlaybackHandler) prepareLocalTransportV3(r *http.Request, session *playback.Session, file *models.MediaFile, result playback.PlannerResultV3, timeline preparedTimelineV3, mode mediaAuthModeV3) (preparedTransportV3, *transportErrorV3) {
	cfg := h.playbackConfig()
	if err := os.MkdirAll(cfg.TranscodeDir, 0o755); err != nil {
		return preparedTransportV3{}, &transportErrorV3{reason: "internal_error", message: "Failed to prepare the transcode directory.", cause: err}
	}
	outputSubdir := transportGenerationV3(session.ID, result.Plan.PlanID)
	outputDir := filepath.Join(cfg.TranscodeDir, outputSubdir)
	videoCodec := result.TargetVideoCodec
	if result.Plan.Delivery == playback.DeliveryRemuxHLSV3 {
		videoCodec = "copy"
	}
	sourceMetadata := sourceExecutionMetadataV3(file, result)
	sourceProfile, sourceBitDepth := sourceVideoTranscodeFactsV3(file, result)
	unlock := h.tm.LockSessionLifecycle(session.ID)
	opts := playback.TranscodeOpts{
		InputPath:                        file.FilePath,
		MediaFileID:                      file.ID,
		VirtualSourceOwnerInstallationID: effectiveVirtualOwner(file.VirtualOwnerInstallationID, session.VirtualSourceOwnerInstallationID),
		OutputDir:                        outputDir,
		OutputSubdir:                     outputSubdir,
		SessionID:                        session.ID,
		SourceVideoCodec:                 sourceMetadata.VideoCodec,
		SourceVideoProfile:               sourceProfile,
		SourceVideoBitDepth:              sourceBitDepth,
		SourceAudioChannels:              result.SourceAudioChannels,
		SourceFrameRate:                  result.SourceFrameRate,
		SourceHeight:                     result.SourceHeight,
		SoftwareVideoDecode:              sourceMetadata.SoftwareVideoDecode,
		ToneMapPolicy:                    result.ToneMapPolicy,
		ToneMapMode:                      result.ToneMapMode,
		ToneMapSourceKind:                result.ToneMapSourceKind,
		ToneMapRecipeVersion:             result.ToneMapRecipeVersion,
		ToneMapPreflightRequired:         result.ToneMapPreflightRequired,
		ToneMapSourceRevision:            result.ToneMapSourceRevision,
		VideoBitstreamFilter:             videoBitstreamFilterForPlanV3(result.Plan),
		VideoSampleEntry:                 videoSampleEntryForPlanV3(result.Plan),
		SeekSeconds:                      timeline.seekSeconds,
		StreamOriginSeconds:              timeline.streamOriginSeconds,
		CopySeekAnchorResolved:           timeline.copySeekAnchorResolved,
		StartSegmentNumber:               timeline.startSegmentNumber,
		TargetResolution:                 result.TargetResolution,
		TargetCodecVideo:                 videoCodec,
		TargetCodecAudio:                 result.TargetAudioCodec,
		TargetAudioChannels:              result.TargetAudioChannels,
		TargetAudioBitrateKbps:           result.TargetAudioBitrateKbps,
		TargetBitrateKbps:                result.TargetBitrateKbps,
		SegmentDuration:                  playback.DefaultSegmentDuration,
		SegmentRetentionSeconds:          cfg.SegmentRetentionSeconds,
		FFmpegPath:                       cfg.FFmpegPath,
		HWAccel:                          cfg.HWAccel,
		HWDevice:                         cfg.HWDevice,
		AudioTrackIndex:                  audioStreamOrdinalV3(file, plannedAudioTrackIndexV3(result, session.AudioTrackIndex)),
		SubtitleTrackIndex:               result.SubtitleTransportTrackIndex,
		SubtitleBurnIn:                   result.SubtitleBurnIn,
		SubtitleCodec:                    result.SubtitleCodec,
		TotalDuration:                    sourceMetadata.DurationSeconds,
		FastStart:                        true,
		NodeType:                         playbackNodeIntegratedV3,
		ExecutionMode:                    playbackNodeIntegratedV3,
		FFmpegLogSink:                    h.FFmpegLogSink,
	}
	if opts.ToneMapMode != "" {
		opts.ToneMapDVConfigPresent = sourceMetadata.ToneMapDVConfigPresent
		opts.ToneMapDVBLCompatIDPresent = sourceMetadata.ToneMapDVBLCompatIDPresent
		opts.ToneMapDVBLPresent = sourceMetadata.ToneMapDVBLPresent
		opts.ToneMapDVRPUPresent = sourceMetadata.ToneMapDVRPUPresent
		capabilities, capabilityErr := h.localToneMapCapabilitiesForTransportV3(r.Context())
		if capabilityErr != nil {
			unlock()
			return preparedTransportV3{}, &transportErrorV3{reason: transcodeStartFailedReasonV3, message: "Local tone-map capability validation is temporarily unavailable.", retryable: true, cause: capabilityErr}
		}
		opts.ToneMapFilter = capabilities.FilterFor(opts.ToneMapMode, opts.ToneMapSourceKind)
		if opts.ToneMapMode == tonemap.ModeHardware {
			opts.HWAccel = capabilities.BackendFor(opts.ToneMapMode, opts.ToneMapSourceKind)
		} else {
			opts.HWAccel = playback.HWAccelNone
		}
		// The opt-in VPP tone map replaces the OpenCL recipe only on QSV.
		// Every other backend ignores the setting and keeps its validated chain.
		opts.ToneMapFilter = playback.ResolveToneMapFilterV3(opts.ToneMapFilter, opts.ToneMapMode, opts.HWAccel, result.ToneMapVPPEnabled)
	}
	usedToneMapFallback := false
	var ts *playback.TranscodeSession
	var startupFailure *localTransportStartupFailureV3
	if autoPipeline := h.newAutoTranscodePipelineV3(r.Context(), opts); autoPipeline.Enabled() {
		// hw_accel=auto walks GPU decode and encode, CPU decode with GPU
		// encode, then software, so the legacy retry below never applies. An
		// enabled pipeline never carries a tone-map recipe.
		ts, startupFailure = h.startReadyAutoLocalPlaybackTransportV3(r.Context(), autoPipeline)
		if startupFailure != nil {
			unlock()
			if startupFailure.failedToStart {
				return preparedTransportV3{}, transportStartFailureV3(startupFailure.cause,
					toneMapExecutionTransportErrorV3(startupFailure.cause, "Failed to start the playback transport."))
			}
			return preparedTransportV3{}, manifestStartupTransportErrorV3(startupFailure.wasRunning, startupFailure.cause)
		}
	} else {
		ts, startupFailure = h.startReadyLocalPlaybackTransportV3(r.Context(), opts)
	}
	if startupFailure != nil && !startupFailure.failedToStart && r.Context().Err() != nil {
		// The client left while FFmpeg worked toward its first manifest. A
		// retry would run for nobody while holding the session lifecycle
		// lock, and the failure says nothing about the device.
		unlock()
		return preparedTransportV3{}, manifestStartupTransportErrorV3(startupFailure.wasRunning, startupFailure.cause)
	}
	if startupFailure != nil && startupFailure.failedToStart {
		if softwareOpts, eligible := h.softwareToneMapRetryOptsV3(r.Context(), opts, result.FrozenSourceMetadata != nil); eligible {
			slog.WarnContext(r.Context(), "hardware tone-map failed to start; retrying once in software",
				logComponentKey, playbackLogValueV3, "playback_session_id", session.ID, "error", startupFailure.cause)
			opts = softwareOpts
			usedToneMapFallback = true
			ts, startupFailure = h.startReadyLocalPlaybackTransportV3(r.Context(), opts)
		}
	}
	if startupFailure != nil && startupFailure.failedToStart {
		unlock()
		fallback := &transportErrorV3{reason: transcodeStartFailedReasonV3, message: "Failed to start the playback transport.", retryable: false, cause: startupFailure.cause}
		if opts.ToneMapMode != "" {
			fallback = toneMapExecutionTransportErrorV3(startupFailure.cause, "Failed to start the playback transport.")
		}
		// A provider-outage classify outranks the tone-map/transcode reason: the
		// transport never started because the release could not be re-resolved,
		// so the client should retry the release, not rebuild the recipe.
		return preparedTransportV3{}, transportStartFailureV3(startupFailure.cause, fallback)
	}
	if startupFailure != nil {
		transportErr := localTransportReadinessErrorV3(opts, startupFailure.wasRunning, startupFailure.cause)
		if usedToneMapFallback {
			unlock()
			return preparedTransportV3{}, transportErr
		}

		if errors.Is(startupFailure.cause, playback.ErrSourceDecodeRejected) {
			// A confirmed decoder rejection is terminal for this candidate:
			// rebuilding the same undecodable bytes cannot help, and the
			// candidate rotation loop owns the substitution. Return the typed
			// readiness outcome before the generic one-clean-generation retry
			// below can start an extra transcode on the rejected release.
			unlock()
			return preparedTransportV3{}, localTransportReadinessErrorV3(opts, startupFailure.wasRunning, startupFailure.cause)
		}
		if fallbackOpts, eligible := h.softwareToneMapRetryOptsV3(r.Context(), opts, result.FrozenSourceMetadata != nil); eligible {
			slog.WarnContext(r.Context(), "hardware tone-map failed during startup; retrying once in software",
				logComponentKey, playbackLogValueV3,
				"playback_session_id", session.ID,
				"failed_device", startupFailure.failedDevice,
				"error", startupFailure.cause)
			opts = fallbackOpts
			ts, startupFailure = h.startReadyLocalPlaybackTransportV3(r.Context(), opts)
			if startupFailure != nil && startupFailure.failedToStart {
				unlock()
				return preparedTransportV3{}, toneMapExecutionTransportErrorV3(startupFailure.cause, "Failed to start the software tone-map fallback.")
			}
			if startupFailure != nil {
				unlock()
				return preparedTransportV3{}, localTransportReadinessErrorV3(opts, startupFailure.wasRunning, startupFailure.cause)
			}
		} else if startupFailure.wasRunning {
			unlock()
			return preparedTransportV3{}, transportErr
		} else {
			// FFmpeg and GPU drivers can fail before producing their first segment
			// even though the recipe is valid. Retry one clean generation, preferring
			// another configured render device so a transient device failure does not
			// become an immediate client-visible transport error. gpu_only suppresses
			// the VideoToolbox retry to HWAccelNone, which decodes and encodes on the
			// CPU, and surfaces the startup failure instead.
			retryAccel := playback.StartupRetryHWAccel(opts)
			if !h.startupRetryAllowedV3(opts, retryAccel) {
				unlock()
				return preparedTransportV3{}, transportErr
			}
			retryOpts := opts
			retryOpts.AvoidHWDevice = startupFailure.failedDevice
			retryOpts.HWAccel = retryAccel
			slog.WarnContext(r.Context(), "local transcode crashed during startup; retrying once",
				logComponentKey, playbackLogValueV3,
				"playback_session_id", session.ID,
				"failed_device", startupFailure.failedDevice,
				"configured_devices", retryOpts.HWDevice,
				"error", startupFailure.cause)
			ts, startupFailure = h.startReadyLocalPlaybackTransportV3(r.Context(), retryOpts)
			if startupFailure != nil && startupFailure.failedToStart {
				unlock()
				return preparedTransportV3{}, toneMapExecutionTransportErrorV3(startupFailure.cause, "Failed to start the playback transport.")
			}
			if startupFailure != nil {
				unlock()
				return preparedTransportV3{}, localTransportReadinessErrorV3(retryOpts, startupFailure.wasRunning, startupFailure.cause)
			}
		}
	}
	// Start-commit decode pre-check. A virtual generation whose decoder already
	// rejected the source during startup cannot produce a playable plan, so fail
	// it before it is published and let the start/replan rotation loop substitute
	// another provider candidate. This is a flag read on the candidate we are
	// about to commit, never a wait, and only a video encode has a decode verdict
	// to give up on. It deliberately reads the SUCCESSOR ts, not the registered
	// predecessor: a replan to a healthy sibling must not be aborted by the
	// generation it is replacing. Non-virtual files keep the existing behavior —
	// the start commits and the media routes answer 422 + X-Vio-Decode-Error,
	// which remains the fallback for clients without the recovery path.
	if isVirtualPlaybackFile(file) && result.Plan != nil && planHasVideoEncodeV3(*result.Plan) && ts.IsSourceRejected() {
		// The generation is never published; close its process before the
		// lifecycle lock is released so a rejected candidate does not leak a
		// running ffmpeg.
		_ = ts.Close()
		unlock()
		return preparedTransportV3{}, &transportErrorV3{
			reason:  candidateSourceDecodeRejectedReasonV3,
			message: "The selected media source was rejected by the video decoder.",
		}
	}
	cardOpts := ts.Opts()
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(file.FilePath)), "virtual://") {
		cardOpts.CanonicalInputPath = file.FilePath
		cardOpts.VirtualSourceOwnerInstallationID = effectiveVirtualOwner(file.VirtualOwnerInstallationID, session.VirtualSourceOwnerInstallationID)
	}
	url := fmt.Sprintf("/playback/transcode/%s/master.m3u8", session.ID)
	if !mode.headerAuth {
		card := playback.NewRecipeCard(session.UserID, session.ProfileID, file.ID, "", cardOpts)
		card.OriginalStartedAt = session.StartedAt
		card.StreamLocation = session.StreamLocation
		card.RoutingNetworkProvider = new(netaccess.PathFromContext(r.Context()).Provider)
		card.RoutingWorkload = string(routingWorkloadV3(result))
		card.RoutingExecution = string(noderouting.ExecutionAPI)
		card.RoutingEgress = string(noderouting.EgressAPI)
		url = appendStreamToken(url, h.signSessionToken(card, mode.headerAuth))
	}
	committed := false
	previousNodeURL := session.TranscodeNodeURL
	previousTransportID := remoteTransportID(session)
	return preparedTransportV3{
		url:              url,
		hwAccel:          ts.Opts().EffectiveEncoderHWAccel(),
		toneMapMode:      ts.Opts().ToneMapMode,
		routingWorkload:  routingWorkloadV3(result),
		routingExecution: noderouting.ExecutionAPI,
		routingEgress:    noderouting.EgressAPI,
		commit: func() *transportErrorV3 {
			if committed {
				return nil
			}
			committed = true
			previous, accepted := h.tm.SwapTranscodeSession(session.ID, ts)
			if !accepted {
				unlock()
				return &transportErrorV3{
					reason:    transcodeStartFailedReasonV3,
					message:   "The playback transport could not be published during shutdown.",
					retryable: true,
				}
			}
			// A local transcode is never proxy-served, so an authorized-origins
			// replan landing here has to revoke the grant it is replacing.
			h.revokeStaleProxyGrantOnCommitV3(r.Context(), session.ID, mode, false)
			h.applyRemoteTransportMarkV3(r.Context(), session.ID, false)
			unlock()
			if previous != nil && previous != ts {
				// Keep the displaced generation's already-produced bytes
				// servable for a bounded overlap so the client's in-flight
				// old playlist (before it adopts the new plan) is not 503'd
				// while the new generation warms. The process is stopped;
				// only its output directory is retained.
				h.tm.RetireTranscodeSessionPredecessor(session.ID, previous, playback.RetainedGenerationRetention)
			}
			if previousNodeURL != "" {
				h.tm.StopRemoteTranscode(previousTransportID, previousNodeURL)
			}
			ts.SetRestartHook(func(ctx context.Context) {
				h.maybeStartThrottler(ctx, ts)
				h.tm.MonitorLocalTranscodeExit(session.ID, ts)
			})
			h.maybeStartThrottler(r.Context(), ts)
			h.tm.MonitorLocalTranscodeExit(session.ID, ts)
			return nil
		},
		rollback: func() {
			if committed {
				return
			}
			committed = true
			_ = ts.Close()
			unlock()
		},
	}, nil
}

func manifestStartupTransportErrorV3(running bool, cause error) *transportErrorV3 {
	message := "The playback transport failed before media became ready."
	if running {
		message = "The playback transport did not become ready in time."
	}
	// A revoked generation surfaces its decode verdict through the readiness
	// wait: without this mapping the rotation loop would see a generic
	// transcode_start_failed and retry the same undecodable bytes instead of
	// substituting another provider candidate.
	if errors.Is(cause, playback.ErrSourceDecodeRejected) {
		return &transportErrorV3{
			reason:  candidateSourceDecodeRejectedReasonV3,
			message: "The selected media source was rejected by the video decoder.",
			cause:   cause,
		}
	}
	return &transportErrorV3{reason: transcodeStartFailedReasonV3, message: message, retryable: running, cause: cause}
}

// localTransportReadinessErrorV3 classifies a local first-window timeout. A
// plan that burns in a subtitle and whose FFmpeg is still running is slow, not
// dead: a generic transcode_start_failed would let the start/replan candidate
// machinery substitute another release for a subtitle problem. It is reported
// as the subtitle-local reason instead, so the release is unchanged and the
// client gets a clear subtitle outcome.
func localTransportReadinessErrorV3(opts playback.TranscodeOpts, running bool, cause error) *transportErrorV3 {
	// Decode verdict outranks the subtitle-local slow path below: undecodable
	// video is a candidate failure on every release, never a subtitle problem.
	if errors.Is(cause, playback.ErrSourceDecodeRejected) {
		return &transportErrorV3{
			reason:  candidateSourceDecodeRejectedReasonV3,
			message: "The selected media source was rejected by the video decoder.",
			cause:   cause,
		}
	}
	if running && opts.SubtitleBurnIn && opts.SubtitleTrackIndex >= 0 {
		return &transportErrorV3{
			reason:    subtitleUnavailableReasonV3,
			message:   "The selected subtitle could not be prepared in time on this release; retry, or start with subtitles off. The video release is unchanged.",
			retryable: true,
			cause:     cause,
		}
	}
	return manifestStartupTransportErrorV3(running, cause)
}

func toneMapExecutionTransportErrorV3(cause error, message string) *transportErrorV3 {
	return &transportErrorV3{
		reason:    transcodeStartFailedReasonV3,
		message:   message,
		retryable: !errors.Is(cause, tonemap.ErrSourceRevisionChanged) && !errors.Is(cause, tonemap.ErrSourcePreflightRejected),
		cause:     cause,
	}
}

func combineTransportErrorsV3(first, second *transportErrorV3) *transportErrorV3 {
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	combined := *second
	combined.retryable = first.retryable || second.retryable
	combined.cause = errors.Join(first.cause, second.cause)
	return &combined
}

// prepareRemoteTransportV3 starts an HLS generation on the selected transcode node.
func (h *PlaybackHandler) prepareRemoteTransportV3(r *http.Request, session *playback.Session, file *models.MediaFile, result playback.PlannerResultV3, nodePlan nodepool.Plan, timeline preparedTimelineV3, mode mediaAuthModeV3) (preparedTransportV3, *transportErrorV3) {
	node := nodePlan.TranscodeNode
	transportID := transportGenerationV3(session.ID, result.Plan.PlanID)
	videoCodec := result.TargetVideoCodec
	if result.Plan.Delivery == playback.DeliveryRemuxHLSV3 {
		videoCodec = "copy"
	}
	sourceMetadata := sourceExecutionMetadataV3(file, result)
	sourceProfile, sourceBitDepth := sourceVideoTranscodeFactsV3(file, result)
	// The node's own override wins over this host's cluster-wide setting; every
	// other node gets the cluster value verbatim, "auto" included, so the node
	// still resolves it against live hardware at session start.
	hwAccel := node.EffectiveHWAccel(h.playbackConfig().HWAccel)
	toneMapFilter := ""
	if result.ToneMapMode != "" {
		capabilities, err := h.remoteToneMapCapabilitiesV3(r.Context(), node.URL, false)
		if err != nil || !capabilities.Supports(result.ToneMapMode, result.ToneMapSourceKind) {
			return preparedTransportV3{}, &transportErrorV3{reason: capabilityUnavailableReasonV3, message: "The selected node cannot run the tone-map recipe.", retryable: true, cause: err}
		}
		toneMapFilter = capabilities.FilterFor(result.ToneMapMode, result.ToneMapSourceKind)
		if result.ToneMapMode == tonemap.ModeHardware {
			hwAccel = capabilities.BackendFor(result.ToneMapMode, result.ToneMapSourceKind)
		} else {
			hwAccel = playback.HWAccelNone
		}
	}
	inputPath := file.FilePath
	var virtualCleanup func()
	if isVirtualPlaybackFile(file) || strings.HasPrefix(strings.ToLower(strings.TrimSpace(file.FilePath)), virtualPlaybackPrefix) {
		ownerInstallationID := file.VirtualOwnerInstallationID
		userID, profileID := 0, ""
		if session != nil {
			ownerInstallationID = effectiveVirtualOwner(file.VirtualOwnerInstallationID, session.VirtualSourceOwnerInstallationID)
			userID, profileID = session.UserID, session.ProfileID
		}
		resolveCtx := virtualResolveContextWithPersistedIdentity(r.Context(), file)
		if h.tm != nil && h.tm.ResolveInput != nil {
			var resolveErr error
			inputPath, virtualCleanup, resolveErr = h.tm.ResolveInput(resolveCtx, file.ID, ownerInstallationID, userID, profileID, file.FilePath)
			if resolveErr != nil {
				return preparedTransportV3{}, &transportErrorV3{
					reason:    transcodeStartFailedReasonV3,
					message:   "Failed to resolve virtual media for remote transcode.",
					retryable: true,
					cause:     resolveErr,
				}
			}
		} else {
			res, cleanup, resolveErr := h.ResolveVirtualTransportInput(resolveCtx, file.FilePath, ownerInstallationID, userID, profileID)
			if resolveErr != nil {
				return preparedTransportV3{}, &transportErrorV3{
					reason:    transcodeStartFailedReasonV3,
					message:   "Failed to resolve virtual media for remote transcode.",
					retryable: true,
					cause:     resolveErr,
				}
			}
			inputPath = res.URL
			virtualCleanup = cleanup
		}
	}
	cleanupOnFailure := func() {
		if virtualCleanup != nil {
			virtualCleanup()
			virtualCleanup = nil
		}
	}
	req := transcodenode.TranscodeStartRequest{SessionID: transportID, InputPath: inputPath, SourceVideoCodec: sourceMetadata.VideoCodec, SourceVideoProfile: sourceProfile, SourceVideoBitDepth: sourceBitDepth, SourceAudioChannels: result.SourceAudioChannels, SourceFrameRate: result.SourceFrameRate, SourceHeight: result.SourceHeight, SoftwareVideoDecode: sourceMetadata.SoftwareVideoDecode, ToneMapPolicy: result.ToneMapPolicy, ToneMapMode: result.ToneMapMode, ToneMapSourceKind: result.ToneMapSourceKind, ToneMapRecipeVersion: result.ToneMapRecipeVersion, ToneMapPreflightRequired: result.ToneMapPreflightRequired, ToneMapSourceRevision: result.ToneMapSourceRevision, VideoBitstreamFilter: videoBitstreamFilterForPlanV3(result.Plan), VideoSampleEntry: videoSampleEntryForPlanV3(result.Plan), SeekSeconds: timeline.seekSeconds, StreamOriginSeconds: timeline.streamOriginSeconds, CopySeekAnchorResolved: timeline.copySeekAnchorResolved, StartSegmentNumber: timeline.startSegmentNumber, TargetResolution: result.TargetResolution, TargetCodecVideo: videoCodec, TargetCodecAudio: result.TargetAudioCodec, TargetAudioChannels: result.TargetAudioChannels, TargetAudioBitrateKbps: result.TargetAudioBitrateKbps, TargetBitrateKbps: result.TargetBitrateKbps, SegmentDuration: playback.DefaultSegmentDuration, HWAccel: hwAccel, AudioTrackIndex: audioStreamOrdinalV3(file, plannedAudioTrackIndexV3(result, session.AudioTrackIndex)), SubtitleTrackIndex: result.SubtitleTransportTrackIndex, SubtitleBurnIn: result.SubtitleBurnIn, SubtitleCodec: result.SubtitleCodec, TotalDuration: sourceMetadata.DurationSeconds, RequireReady: true}
	req.ThrottleSeconds = playback.ConfiguredTranscodeThrottleSeconds(r.Context(), h.SettingsRepo)
	if strings.EqualFold(videoCodec, "copy") {
		req.CopyFMP4RecipeVersion = playback.CopyFMP4RecipeVersion
	}
	if playback.IsAudioToAACStereoDownmixV3(req.SourceAudioChannels, req.TargetCodecAudio, req.TargetAudioChannels) {
		// Remote attestation uses the explicit effective layout even though zero
		// means stereo to the local AAC argument builder.
		req.TargetAudioChannels = 2
		req.AudioRecipeVersion = playback.TransformationAudioToAACRecipeVersionV3
	} else {
		// SourceAudioChannels is a v2 recipe field at the node boundary. Omit it
		// for ordinary encodes so they cannot be mistaken for partial v2 work.
		req.SourceAudioChannels = 0
	}
	if req.ToneMapMode != "" {
		req.ToneMapDVConfigPresent = sourceMetadata.ToneMapDVConfigPresent
		req.ToneMapDVBLCompatIDPresent = sourceMetadata.ToneMapDVBLCompatIDPresent
		req.ToneMapDVBLPresent = sourceMetadata.ToneMapDVBLPresent
		req.ToneMapDVRPUPresent = sourceMetadata.ToneMapDVRPUPresent
	}
	remoteStartAt := time.Now()
	nodeResp, status, err := h.startRemotePlaybackTransport(r.Context(), node.URL, req)
	remoteOutcome := transportStartupReadyV3
	if err != nil || status != http.StatusAccepted {
		remoteOutcome = playbackRemoteOutcomeFailedV3
	}
	slog.InfoContext(r.Context(), "playback transport startup timing",
		logComponentKey, playbackLogValueV3,
		requestIDLogKeyV3, chimw.GetReqID(r.Context()),
		"transport", "remote",
		"session", session.ID,
		"total_ms", time.Since(remoteStartAt).Milliseconds(),
		"status", status,
		"outcome", remoteOutcome,
	)
	if err != nil {
		cleanupOnFailure()
		if req.ToneMapMode != "" && (errors.Is(err, tonemap.ErrSourceRevisionChanged) ||
			errors.Is(err, tonemap.ErrSourcePreflightRejected) ||
			errors.Is(err, playback.ErrToneMapSourceValidationUnavailable) ||
			errors.Is(err, playback.ErrToneMapExecutorUnavailable)) {
			h.tm.StopRemoteTranscode(transportID, node.URL)
			return preparedTransportV3{}, toneMapExecutionTransportErrorV3(err, "The selected transcode node rejected the playback transport.")
		}
		// A timeout can fire after the node actually started the job; the
		// stop is a harmless 404 when it never did, and reaps an orphan
		// full-length transcode when it did.
		h.tm.StopRemoteTranscode(transportID, node.URL)
		return preparedTransportV3{}, &transportErrorV3{reason: "transcode_node_unavailable", message: "The selected transcode node is unavailable.", retryable: true, cause: err}
	}
	if status != http.StatusAccepted {
		cleanupOnFailure()
		h.tm.StopRemoteTranscode(transportID, node.URL)
		return preparedTransportV3{}, &transportErrorV3{reason: transcodeStartFailedReasonV3, message: "The selected transcode node rejected the playback transport.", retryable: true}
	}
	if err := transcodenode.ValidateAudioRecipeAttestation(req, nodeResp); err != nil {
		cleanupOnFailure()
		h.tm.StopRemoteTranscode(transportID, node.URL)
		// A node that cannot confirm the audio adaptation recipe is an audio
		// failure, not a video one. The distinct reason keeps the start/replan
		// alternate-version hunts from rotating the release over it; the
		// transport loop still tries the same recipe on another node, which is
		// the same-release resolution for a node-local audio condition.
		return preparedTransportV3{}, &transportErrorV3{reason: audioAdaptationFailedReasonV3, message: "The selected transcode node did not confirm the audio recipe.", retryable: true, cause: err}
	}
	if err := transcodenode.ValidateCopyFMP4RecipeAttestation(req, nodeResp); err != nil {
		cleanupOnFailure()
		h.tm.StopRemoteTranscode(transportID, node.URL)
		return preparedTransportV3{}, &transportErrorV3{reason: transcodeStartFailedReasonV3, message: "The selected transcode node did not confirm the copy-video recipe.", retryable: true, cause: err}
	}
	if err := transcodenode.ValidateThrottleAttestation(req, nodeResp); err != nil {
		cleanupOnFailure()
		h.tm.StopRemoteTranscode(transportID, node.URL)
		return preparedTransportV3{}, &transportErrorV3{reason: transcodeStartFailedReasonV3, message: "The selected transcode node did not confirm the throttle policy.", retryable: true, cause: err}
	}
	if req.ToneMapMode != "" && nodeResp.ToneMapMode != req.ToneMapMode {
		cleanupOnFailure()
		h.tm.StopRemoteTranscode(transportID, node.URL)
		return preparedTransportV3{}, &transportErrorV3{reason: transcodeStartFailedReasonV3, message: "The selected transcode node did not confirm the tone-map recipe.", retryable: true}
	}
	confirmedToneMapMode := tonemap.Mode("")
	if req.ToneMapMode != "" {
		confirmedToneMapMode = nodeResp.ToneMapMode
	}
	card := remoteTranscodeRecipeCardV3(session, file, node.URL, transportID, req, nodeResp, toneMapFilter)
	card.RoutingWorkload = string(routingWorkloadV3(result))
	card.RoutingNetworkProvider = new(netaccess.PathFromContext(r.Context()).Provider)
	card.RoutingExecutionNodeID = node.ID
	card.RoutingExecution = string(noderouting.ExecutionTranscode)
	card.RoutingEgress = string(noderouting.EgressAPI)
	if nodePlan.ProxyNode != nil {
		card.RoutingEgress = string(noderouting.EgressProxy)
		card.RoutingEgressNodeID = nodePlan.ProxyNode.ID
	}
	confirmedHWAccel := card.EffectiveEncoderHWAccel()
	// Keep the provider-neutral virtual source identity on the stored
	// recipe so a node/central restart resolves the same release.
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(file.FilePath)), "virtual://") {
		card.InputPath = file.FilePath
		card.VirtualSourceOwnerInstallationID = effectiveVirtualOwner(file.VirtualOwnerInstallationID, session.VirtualSourceOwnerInstallationID)
	}
	card.OriginalStartedAt = session.StartedAt
	url := fmt.Sprintf("/playback/transcode/%s/master.m3u8", session.ID)
	// Either URL builder only returns an absolute proxy URL when a proxy was
	// planned and its authority (a signed token, or a stored grant) could
	// actually be established; otherwise the client fetches the manifest from
	// this server and the local liveness path applies.
	servedByProxy := false
	var priorGrant *playback.RecipeCard
	accessPath := netaccess.PathFromContext(r.Context())
	switch {
	case !mode.headerAuth:
		url = h.buildProxyManifestURL(card, nodePlan.ProxyNode, mode.headerAuth, accessPath)
		servedByProxy = nodePlan.ProxyNode != nil && strings.HasPrefix(url, "http")
	case mode.proxyEgress:
		url, servedByProxy, priorGrant = h.grantManifestURLV3(r.Context(), card, nodePlan.ProxyNode, accessPath)
	}
	if mode.headerAuth {
		h.putNodeRecipeV3(r.Context(), transportID, card)
	}
	if nodePlan.ProxyNode != nil && !servedByProxy {
		if releaser, ok := h.NodePlanner.(sessionProxyReservationReleaserV3); ok {
			releaser.ReleaseSessionProxy(session.ID)
		}
	}
	committed := false
	previousNodeURL := session.TranscodeNodeURL
	previousTransportID := remoteTransportID(session)
	unlock := h.tm.LockSessionLifecycle(session.ID)
	routingEgress := noderouting.EgressAPI
	egressNodeID := 0
	egressNodeURL := ""
	if servedByProxy {
		routingEgress = noderouting.EgressProxy
		egressNodeID = nodePlan.ProxyNode.ID
		egressNodeURL = nodePlan.ProxyNode.URL
	}
	adoptSuccessor := func() {
		h.tm.CloseTranscodeSession(session.ID, "")
		if previousNodeURL != "" {
			h.tm.StopRemoteTranscode(previousTransportID, previousNodeURL)
			h.deleteNodeRecipeV3(r.Context(), previousTransportID)
		}
		h.revokeStaleProxyGrantOnCommitV3(r.Context(), session.ID, mode, servedByProxy)
		h.applyRemoteTransportMarkV3(r.Context(), session.ID, servedByProxy)
	}
	rollbackTransport := func(requireCancellation bool) error {
		cleanupOnFailure()
		if committed {
			return nil
		}
		if requireCancellation {
			if err := h.tm.CancelRemoteTranscode(r.Context(), transportID, node.URL); err != nil {
				// Preserve the admitted successor route for a retryable stop. Its
				// live session, authority, and reservation must continue to agree.
				committed = true
				adoptSuccessor()
				unlock()
				return err
			}
		} else {
			h.tm.StopRemoteTranscode(transportID, node.URL)
		}
		committed = true
		// The node job this recipe rebuilds is gone, so the recipe must go too.
		h.deleteNodeRecipeV3(r.Context(), transportID)
		if releaser, ok := h.NodePlanner.(sessionReservationReleaserV3); ok {
			releaser.ReleaseSession(session.ID)
		}
		if servedByProxy {
			h.restoreProxyGrantV3(r.Context(), session.ID, priorGrant)
		}
		unlock()
		return nil
	}
	var rollbackRequired func() error
	if routingWorkloadV3(result) == noderouting.WorkloadRemux {
		rollbackRequired = func() error { return rollbackTransport(true) }
	}
	return preparedTransportV3{url: url, nodeURL: node.URL, transportID: transportID, hwAccel: confirmedHWAccel, toneMapMode: confirmedToneMapMode,
		routingWorkload: routingWorkloadV3(result), routingExecution: noderouting.ExecutionTranscode, routingExecutorID: node.ID, routingExecutorURL: node.URL,
		routingEgress: routingEgress, routingEgressID: egressNodeID, routingEgressURL: egressNodeURL, commit: func() *transportErrorV3 {
			if committed {
				return nil
			}
			committed = true
			adoptSuccessor()
			unlock()
			return nil
		}, rollback: func() {
			_ = rollbackTransport(false)
		}, rollbackRequired: rollbackRequired}, nil
}

// remoteTranscodeRecipeCardV3 captures the byte-affecting recipe of a started
// remote transcode. It is what a proxy relays from (grant) or a client carries
// (signed token), and what a restarted node reconstructs from, so it must
// reflect the parameters the node accepted rather than the ones requested: the
// node reports the hardware acceleration it actually used, and whether it
// decoded video on the CPU. Software decode is the request's value OR the
// node's, so an older node that omits the field keeps the requested value.
//
// toneMapFilter is the resolved FFmpeg filter for the confirmed executor; it is
// not part of the node's request/response contract, so the caller supplies it.
func remoteTranscodeRecipeCardV3(session *playback.Session, file *models.MediaFile, nodeURL, transportID string, req transcodenode.TranscodeStartRequest, nodeResp transcodenode.TranscodeStartResponse, toneMapFilter string) playback.RecipeCard {
	hw := firstNonEmptyHandlerV3(strings.TrimSpace(nodeResp.HWAccel), strings.TrimSpace(req.HWAccel))
	card := playback.NewRecipeCard(session.UserID, session.ProfileID, file.ID, nodeURL, playback.TranscodeOpts{InputPath: req.InputPath, SessionID: session.ID, TranscodeTransportID: transportID, SourceVideoCodec: req.SourceVideoCodec, SourceVideoProfile: req.SourceVideoProfile, SourceVideoBitDepth: req.SourceVideoBitDepth, SourceAudioChannels: req.SourceAudioChannels, SourceFrameRate: req.SourceFrameRate, SourceHeight: req.SourceHeight, SoftwareVideoDecode: req.SoftwareVideoDecode || nodeResp.SoftwareVideoDecode, ToneMapPolicy: req.ToneMapPolicy, ToneMapMode: req.ToneMapMode, ToneMapSourceKind: req.ToneMapSourceKind, ToneMapFilter: toneMapFilter, ToneMapRecipeVersion: req.ToneMapRecipeVersion, ToneMapPreflightRequired: req.ToneMapPreflightRequired, ToneMapSourceRevision: req.ToneMapSourceRevision, VideoBitstreamFilter: req.VideoBitstreamFilter, VideoSampleEntry: req.VideoSampleEntry, SeekSeconds: req.SeekSeconds, StreamOriginSeconds: req.StreamOriginSeconds, CopySeekAnchorResolved: req.CopySeekAnchorResolved, StartSegmentNumber: req.StartSegmentNumber, TargetResolution: req.TargetResolution, TargetCodecVideo: req.TargetCodecVideo, TargetCodecAudio: req.TargetCodecAudio, TargetAudioChannels: req.TargetAudioChannels, TargetAudioBitrateKbps: req.TargetAudioBitrateKbps, TargetBitrateKbps: req.TargetBitrateKbps, SegmentDuration: req.SegmentDuration, HWAccel: hw, AudioTrackIndex: req.AudioTrackIndex, SubtitleTrackIndex: req.SubtitleTrackIndex, SubtitleBurnIn: req.SubtitleBurnIn, SubtitleCodec: req.SubtitleCodec, TotalDuration: req.TotalDuration, ThrottleSeconds: req.ThrottleSeconds})
	card.ToneMapDVConfigPresent = req.ToneMapDVConfigPresent
	if encoderHWAccel := strings.TrimSpace(nodeResp.EncoderHWAccel); encoderHWAccel != "" {
		card.EncoderHWAccel = encoderHWAccel
	}
	card.ToneMapDVBLCompatIDPresent = req.ToneMapDVBLCompatIDPresent
	card.ToneMapDVBLPresent = req.ToneMapDVBLPresent
	card.ToneMapDVRPUPresent = req.ToneMapDVRPUPresent
	// Stamp the immutable session creation time onto every copy of this recipe —
	// client token, proxy grant, and node recipe alike — so telemetry can age the
	// session correctly after any reconstruct.
	card.OriginalStartedAt = session.StartedAt
	card.StreamLocation = session.StreamLocation
	return card
}

// grantManifestURLV3 is buildProxyManifestURL's authorized-origins sibling: it
// stores the session's transcode recipe as a proxy grant and returns the
// credential-free manifest URL on that origin. Segment URIs stay relative to
// the manifest, so the same /stream/v3/{session_id}/... family serves both.
//
// Without a planned proxy — or a proxy the client cannot reach on its access
// path, or when the grant cannot be stored — the client fetches the manifest
// from this server, which relays the same node. The third value is the grant
// this write displaced, for the caller's rollback.
func (h *PlaybackHandler) grantManifestURLV3(ctx context.Context, card playback.RecipeCard, proxyNode *nodepool.Node, path netaccess.Path) (string, bool, *playback.RecipeCard) {
	localURL := fmt.Sprintf("/playback/transcode/%s/master.m3u8", card.SessionID)
	if proxyNode == nil {
		return localURL, false, nil
	}
	base := proxyNode.ClientURLFor(path)
	if base == "" {
		return localURL, false, nil
	}
	card.RoutingEgressNodeID = proxyNode.ID
	prior, stored := h.putProxyGrantV3(ctx, card.SessionID, card)
	if !stored {
		return localURL, false, nil
	}
	return base + "/stream/v3/" + card.SessionID + "/master.m3u8", true, prior
}

// sourceExecutionMetadataV3 freezes the source facts used by a remote executor.
func sourceExecutionMetadataV3(file *models.MediaFile, result playback.PlannerResultV3) playback.SourceExecutionMetadataV3 {
	if result.FrozenSourceMetadata != nil {
		metadata := *result.FrozenSourceMetadata
		if result.Plan != nil && result.Plan.EffectiveRecipe.SoftwareVideoDecode {
			metadata.SoftwareVideoDecode = true
		}
		return scopeToneMapSourceMetadataV3(metadata, result.ToneMapMode)
	}
	if file == nil {
		return playback.SourceExecutionMetadataV3{}
	}
	videoCodec, profile, bitDepth := playback.SourceVideoTranscodeFacts(file)
	track := models.VideoTrack{}
	if len(file.VideoTracks) > 0 {
		track = file.VideoTracks[0]
	}
	softwareDecode := playback.RequiresSoftwareVideoDecode(videoCodec, profile, bitDepth)
	if result.Plan != nil {
		softwareDecode = softwareDecode || result.Plan.EffectiveRecipe.SoftwareVideoDecode
	}
	metadata := playback.SourceExecutionMetadataV3{
		VideoCodec:                 videoCodec,
		VideoProfile:               profile,
		VideoBitDepth:              bitDepth,
		SoftwareVideoDecode:        softwareDecode,
		DurationSeconds:            float64(file.Duration),
		ToneMapSourceKind:          result.ToneMapSourceKind,
		ToneMapPreflightRequired:   result.ToneMapPreflightRequired,
		ToneMapSourceRevision:      result.ToneMapSourceRevision,
		ToneMapDVConfigPresent:     track.DVConfigPresent,
		ToneMapDVBLCompatIDPresent: track.DVBLCompatIDPresent,
		ToneMapDVBLPresent:         track.DVBLPresent,
		ToneMapDVRPUPresent:        track.DVRPUPresent,
	}
	return scopeToneMapSourceMetadataV3(metadata, result.ToneMapMode)
}

// Dolby Vision provenance is part of a frozen tone-map recipe, not a general
// source description. Leaving it attached to a video-copy remux makes the
// execution boundary correctly reject the orphaned fields as a partial recipe.
func scopeToneMapSourceMetadataV3(metadata playback.SourceExecutionMetadataV3, mode tonemap.Mode) playback.SourceExecutionMetadataV3 {
	if mode != "" {
		return metadata
	}
	metadata.ToneMapSourceKind = ""
	metadata.ToneMapPreflightRequired = false
	metadata.ToneMapSourceRevision = tonemap.SourceRevision{}
	metadata.ToneMapDVConfigPresent = false
	metadata.ToneMapDVBLCompatIDPresent = false
	metadata.ToneMapDVBLPresent = false
	metadata.ToneMapDVRPUPresent = false
	return metadata
}

func sourceVideoTranscodeFactsV3(file *models.MediaFile, result playback.PlannerResultV3) (string, int) {
	if result.FrozenSourceMetadata != nil {
		return result.FrozenSourceMetadata.VideoProfile, result.FrozenSourceMetadata.VideoBitDepth
	}
	_, profile, bitDepth := playback.SourceVideoTranscodeFacts(file)
	return profile, bitDepth
}

// v3SessionStreamState builds the durable stream state for a prepared transport.
func (h *PlaybackHandler) v3SessionStreamState(ctx context.Context, session *playback.Session, file *models.MediaFile, result playback.PlannerResultV3, transport preparedTransportV3, mode mediaAuthModeV3) playback.SessionStreamState {
	state := playback.SessionStreamState{
		PlayMethod:                    result.PlayMethod,
		BasePlayMethod:                result.PlayMethod,
		AudioTrackIndex:               plannedAudioTrackIndexV3(result, session.AudioTrackIndex),
		TranscodeAudio:                result.TranscodeAudio,
		RemuxDVMode:                   remuxDVModeForPlanV3(result.Plan),
		DVProfile:                     planDVProfileV3(result.Plan),
		DVProfilePin:                  playback.PinDVPinV3(playback.DVPinV3{}, file, planDVProfileV3(result.Plan)),
		RemuxResumeLeadingPictureDrop: result.RemuxResumeLeadingPictureDrop,
		TranscodeHWAccel:              transport.hwAccel,
		ToneMapMode:                   transport.toneMapMode,
		TranscodeNodeURL:              transport.nodeURL,
		TranscodeTransportID:          transport.transportID,
		TranscodeRouteSet:             true,
		RoutingNetworkProvider:        new(netaccess.PathFromContext(ctx).Provider),
		RoutingWorkload:               string(transport.routingWorkload),
		RoutingExecution:              string(transport.routingExecution),
		RoutingExecutionNodeID:        transport.routingExecutorID,
		RoutingExecutionNodeURL:       transport.routingExecutorURL,
		RoutingEgress:                 string(transport.routingEgress),
		RoutingEgressNodeID:           transport.routingEgressID,
		RoutingEgressNodeURL:          transport.routingEgressURL,
		RequireMediaAuthorization:     mode.headerAuth,
		MediaAuthorizationSet:         true,
		// A v3 stream state is a complete snapshot that owns the virtual-source
		// binding outright: it may move the binding to a different candidate and
		// must be able to clear carried evidence when the new effective file is
		// not virtual. Legacy partial updates leave these unset.
		VirtualSourceSet:          true,
		VirtualSourceOwnershipSet: true,
		ClientIP:                  clientip.FromContext(ctx),
		ClientName:                session.ClientName,
		ClientVersion:             session.ClientVersion,
		ClientUserAgent:           session.ClientUserAgent,
		StreamBitrateKbps:         result.TargetBitrateKbps,
		TargetVideoCodec:          result.TargetVideoCodec,
		TargetAudioCodec:          result.TargetAudioCodec,
		SourceAudioChannels:       result.SourceAudioChannels,
		TargetAudioChannels:       result.TargetAudioChannels,
		TargetAudioBitrateKbps:    result.TargetAudioBitrateKbps,
		TargetResolution:          result.TargetResolution,
		SubtitleTrackIndex:        result.SubtitleTransportTrackIndex,
		SubtitleBurnIn:            result.SubtitleBurnIn,
	}
	if result.Plan != nil && (result.Plan.Delivery == playback.DeliveryTranscodeHLSV3 || result.Plan.Delivery == playback.DeliveryRemuxHLSV3) {
		state.SegmentDuration = playback.DefaultSegmentDuration
		state.OutputProtocol = playback.OutputProtocolHLS
		videoCodec := result.TargetVideoCodec
		if result.Plan.Delivery == playback.DeliveryRemuxHLSV3 {
			videoCodec = "copy"
		}
		state.TargetVideoCodec = videoCodec
		state.OutputContainer = playback.HLSOutputContainer(playback.TranscodeOpts{
			TargetCodecVideo: videoCodec,
			SourceVideoCodec: sourceExecutionMetadataV3(file, result).VideoCodec,
		})
	}
	if result.Plan != nil && result.Plan.Delivery == playback.DeliveryRemuxProgressiveV3 {
		state.OutputContainer, state.OutputProtocol = playback.OutputContainerFMP4, playback.OutputProtocolHTTP
	}
	if result.Plan != nil && result.Plan.Delivery == playback.DeliveryOriginalHTTPV3 && file != nil {
		state.OutputContainer, state.OutputProtocol = file.Container, playback.OutputProtocolHTTP
	}
	if state.StreamBitrateKbps <= 0 {
		state.StreamBitrateKbps = result.TargetAudioBitrateKbps
	}
	if state.StreamBitrateKbps <= 0 {
		state.StreamBitrateKbps = fileBitrateKbps(file)
	}
	// Bind the session to the exact virtual URI that was resolved and probed
	// during planning, so later serving resolves the same release rather than
	// re-reading a mutable catalog path. The subtitle inventories travel with
	// it: candidate rotation re-probes the catalog row and can overwrite its
	// tracks between planning and a later subtitle fetch, and the serve path
	// must extract from the same evidence the plan promised. The evidence URI
	// and the source revision anchor that evidence to the exact candidate, so
	// the serve path can discard it when the bound candidate has moved.
	if isVirtualPlaybackFile(file) && strings.HasPrefix(file.FilePath, "virtual://") {
		state.VirtualSourceURI = file.FilePath
		state.VirtualSourceOwnerInstallationID = effectiveVirtualOwner(file.VirtualOwnerInstallationID, session.VirtualSourceOwnerInstallationID)
		state.VirtualSourceRevision = planVirtualSourceRevisionV3(result)
		state.VirtualSubtitleTracks = file.SubtitleTracks
		state.VirtualExternalSubtitles = file.ExternalSubtitles
		state.VirtualAudioTracks = file.AudioTracks
		state.VirtualSubtitleEvidenceURI = file.FilePath
		// The evidence row id is as important as the URI: duplicate catalog
		// rows for one release share candidate URIs and neutral keys, so a
		// later serve must apply this inventory only to the exact row it was
		// captured from, never to a sibling row the probe rotated onto.
		state.VirtualSubtitleEvidenceFileID = file.ID
		state.VirtualSubtitleEvidenceSet = file.ProbeUpdatedAt != nil
	}
	return state
}

func (h *PlaybackHandler) updateV3SessionState(ctx context.Context, session *playback.Session, file *models.MediaFile, result playback.PlannerResultV3, transport preparedTransportV3, mode mediaAuthModeV3) error {
	return h.sessionMgr.UpdateStreamState(session.ID, h.v3SessionStreamState(ctx, session, file, result, transport, mode))
}

// virtualSubtitleWarmSourceURI picks the source URI the text-subtitle warm
// keys its cache identities on. The live session's bound URI wins when set —
// that is the value updateV3SessionState writes and the serve path reads — and
// otherwise the effective file's pinned URI is used, because the start path's
// session copy predates the state write. Mirrors warmVirtualFontBundleV3 so
// the warm and serve identities agree.
func virtualSubtitleWarmSourceURI(session *playback.Session, file *models.MediaFile) string {
	if session != nil && session.VirtualSourceURI != "" {
		return session.VirtualSourceURI
	}
	if file != nil {
		return file.FilePath
	}
	return ""
}

// scheduleVirtualWarmAfterTransportV3 runs the virtual subtitle and font-bundle
// warms detached from the start request, after the transport commit. Both warms
// resolve their own relay registration and demux the remote source; running
// them inline on the response path let full remote reads race the
// just-committed video transport and inflated startup. Detaching keeps the
// transport start uncontended while the first client subtitle/font fetch still
// finds a warm (or in-flight fill) entry. Best-effort: a nil cache or file is a
// no-op.
func (h *PlaybackHandler) scheduleVirtualWarmAfterTransportV3(ctx context.Context, session *playback.Session, file *models.MediaFile, selectedSubtitleIndex int) {
	if h == nil || h.SubtitleCache == nil || file == nil || session == nil {
		return
	}
	if !isVirtualPlaybackFile(file) {
		// Local files keep their existing serve-path warming; this detour is
		// only for virtual relay sources, which resolve a request-scoped
		// registration the warm must own.
		return
	}
	warmCtx := context.WithoutCancel(ctx)
	go func() {
		h.warmVirtualSubtitlesV3(warmCtx, session, file, selectedSubtitleIndex)
		h.warmVirtualFontBundleV3(warmCtx, session, file)
	}()
}

// warmVirtualSubtitlesV3 pre-warms the subtitle cache for a virtual source's
// selected embedded track after the transport commit. The serve path always
// fetches text subtitles windowed (the client appends position/duration), so a
// first click otherwise pays a full remote demux against a fresh relay
// registration; a committed full-track entry turns every windowed fetch into a
// near-instant scan of a small cached artifact instead. PGS already gets this
// fast path from serveWindowedSUP; this warm supplies the entry so both classes
// start instant.
//
// Only the session's selected embedded track is warmed. The windowed serve
// branch looks the cache up by the full-track key — cachedFormatEntryPath
// carries no window component — and, on a hit, runs the client's windowed
// extract against that artifact. A bounded-window artifact committed under the
// same key would look like a complete track and silently truncate every window
// past the warmed slice, so a correct warm must extract the whole track. Warming
// exactly the selected track (instead of every track) and running detached after
// the transport commit keeps the startup path free of competing remote demuxes.
//
// The warm resolves its own relay registration (the transport's is held by
// the transcode session, not shareable) and holds it open until the track's
// warm finishes — a relay URL released mid-warm would 404 under ffmpeg.
// Best-effort by design: resolution failure, warm-slot exhaustion, and cache
// misses (the 24h generation bucket rotates) all degrade to the existing
// cold-serve path with no user-visible error.
func (h *PlaybackHandler) warmVirtualSubtitlesV3(ctx context.Context, session *playback.Session, file *models.MediaFile, selectedSubtitleIndex int) {
	if h == nil || h.SubtitleCache == nil || file == nil || session == nil {
		return
	}
	if !isVirtualPlaybackFile(file) {
		return
	}
	// Resolve the selected combined subtitle index (externals, then embedded,
	// then downloaded) to its inventory segment. Only an embedded selection has
	// an extractable container stream; external and downloaded selections are
	// served by other paths and need no virtual warm.
	location, ok := classifySubtitleIndexV3(file, selectedSubtitleIndex)
	if !ok || location.source != playback.SubtitleSourceEmbeddedV3 {
		return
	}
	// The start path holds a session copy captured before UpdateStreamState
	// wrote VirtualSourceURI, so the warm must key off the effective file's
	// pinned URI or it silently no-ops exactly where it matters most. Mirror
	// warmVirtualFontBundleV3: prefer the live session's bound URI when it has
	// one, otherwise fall back to the effective file. Both name the same
	// release the serve path binds (updateV3SessionState sets
	// state.VirtualSourceURI = file.FilePath), so the warm and serve cache
	// identities agree.
	virtualURI := virtualSubtitleWarmSourceURI(session, file)
	if virtualURI == "" {
		return
	}
	tracks := session.VirtualSubtitleTracks
	if len(tracks) <= location.offset {
		// Fall back to the catalog row's inventory when the session carries
		// no virtual evidence (a drift remap or a rotated candidate) or a
		// shorter layout than the selected ordinal.
		tracks = file.SubtitleTracks
	}
	trackIndex := location.offset
	if trackIndex < 0 || trackIndex >= len(tracks) {
		return
	}
	resolved, cleanup, err := h.resolveVirtualInputURI(
		ctx, file.FilePath, file.VirtualOwnerInstallationID,
		session.UserID, session.ProfileID, false, nil, "",
	)
	if err != nil {
		slog.DebugContext(ctx, "virtual subtitle pre-warm skipped: resolve failed",
			"component", "api", "file_id", file.ID, "error", err)
		return
	}
	warmDone := h.SubtitleCache.WarmTrackInBackground(playback.StreamExtractOpts{
		InputPath:     resolved.URL,
		CacheIdentity: playback.VirtualSubtitleCacheIdentity(file.ID, virtualURI, trackIndex),
		TrackIndex:    trackIndex,
		SourceCodec:   tracks[trackIndex].Codec,
		FFmpegPath:    h.playbackConfig().FFmpegPath,
	}, playback.StreamExtractSubtitle, chimw.GetReqID(ctx))
	// Release the relay registration exactly once, after the warm settled
	// (ran, failed, or was skipped). The entry itself is also bounded by the
	// relay's 24h lifetime, so a lost release never pins a slot forever.
	go func() {
		<-warmDone
		if cleanup != nil {
			cleanup()
		}
	}()
}

// warmVirtualFontBundleV3 pre-warms the font-bundle cache for a virtual
// source's effective file when it carries at least one ASS/SSA embedded track.
// Font bundles are keyed per file (not per track), so exactly one extraction is
// scheduled however many ASS tracks the release has. It mirrors
// warmVirtualSubtitlesV3: it resolves its own relay registration, runs the
// extraction detached under the cache's warm budget, and holds the relay open
// until the extraction settles — a relay URL released mid-extract would 404
// under ffmpeg. Best-effort by design: resolution failure, warm-slot
// exhaustion, and an already-cached bundle all degrade to the existing
// cold-serve path with no user-visible error. The returned channel closes when
// the warm settled (or was skipped); production ignores it.
func (h *PlaybackHandler) warmVirtualFontBundleV3(ctx context.Context, session *playback.Session, file *models.MediaFile) <-chan struct{} {
	done := make(chan struct{})
	if h == nil || h.SubtitleCache == nil || file == nil || session == nil {
		close(done)
		return done
	}
	if !isVirtualPlaybackFile(file) {
		close(done)
		return done
	}
	// The start path holds a session copy captured before UpdateStreamState
	// wrote VirtualSourceURI, so the effective file's URI is the reliable
	// source; prefer the live session when it has one. Both name the same
	// release the serve path will bind.
	virtualURI := file.FilePath
	if session.VirtualSourceURI != "" {
		virtualURI = session.VirtualSourceURI
	}
	if virtualURI == "" {
		close(done)
		return done
	}
	tracks := session.VirtualSubtitleTracks
	if len(tracks) == 0 {
		tracks = file.SubtitleTracks
	}
	hasASS := false
	for _, track := range tracks {
		if playback.IsASS(track.Codec) {
			hasASS = true
			break
		}
	}
	if !hasASS {
		close(done)
		return done
	}
	ffmpegPath := h.playbackConfig().FFmpegPath
	cacheKey := fontBundleCacheKey(file, virtualURI, ffmpegPath)
	if cacheKey.PinnedResult == "" {
		// Uncacheable without the pinned candidate anchor: the identity would
		// rotate with the relay URL, so there is nothing stable to warm.
		close(done)
		return done
	}
	go func() {
		defer close(done)
		resolved, cleanup, err := h.resolveVirtualInputURI(
			context.WithoutCancel(ctx), file.FilePath, file.VirtualOwnerInstallationID,
			session.UserID, session.ProfileID, false, nil, "",
		)
		if err != nil {
			slog.DebugContext(ctx, "virtual font bundle pre-warm skipped: resolve failed",
				"component", "api", "file_id", file.ID, "error", err)
			return
		}
		if cleanup != nil {
			defer cleanup()
		}
		warmDone := h.SubtitleCache.WarmFontBundleInBackground(cacheKey, func(extractCtx context.Context) ([]byte, error) {
			fonts, extractErr := playback.ExtractAttachedSubtitleFonts(extractCtx, resolved.URL, ffmpegPath)
			if extractErr != nil {
				return nil, extractErr
			}
			return json.Marshal(playback.EncodeSubtitleFontBundle(fonts))
		})
		<-warmDone
	}()
	return done
}

func plannedAudioTrackIndexV3(result playback.PlannerResultV3, fallback int) int {
	if result.Plan != nil && result.Plan.SelectedTracks.Audio != nil && result.Plan.SelectedTracks.Audio.Index != nil {
		return *result.Plan.SelectedTracks.Audio.Index
	}
	return fallback
}

// audioStreamOrdinalV3 translates a selected audio track's array position in
// file.AudioTracks to the audio-only stream ordinal ffmpeg's `0:a:N` expects.
// It delegates to the shared playback.AudioStreamOrdinal so v3 and jellycompat
// convert through one implementation; see that function for the two index
// domains and the synthesized-track fallback.
func audioStreamOrdinalV3(file *models.MediaFile, selectedIndex int) int {
	if file == nil {
		return selectedIndex
	}
	return playback.AudioStreamOrdinal(file.AudioTracks, selectedIndex)
}

func transportGenerationV3(sessionID, planID string) string {
	planSuffix := strings.TrimPrefix(planID, "plan:")
	if len(planSuffix) > 12 {
		planSuffix = planSuffix[:12]
	}
	return sessionID + "-" + planSuffix + "-" + uuid.NewString()[:8]
}

// attachSubtitleArtifactV3 republishes the plan's subtitle inventory with
// session-scoped URLs, then resolves the plan's selected ordinal against it and
// stamps that entry's URL onto the artifact. Publishing and resolution share one
// ordering implementation, so an artifact URL can never point at a different
// track than the inventory entry the client selected.
//
// The inventory is scoped unconditionally, not only when a track is selected:
// spec §8 makes it the authoritative track list and says a sidecar entry carries
// a `url` "once a session exists to scope it to" — which is true here for every
// entry, whatever the current selection is. Gating it on the selection published
// a URL-less menu whenever playback started with subtitles off, so a client
// building its picker from the inventory (the Cast receiver's text tracks, for
// one) had nothing fetchable to offer.
func (h *PlaybackHandler) attachSubtitleArtifactV3(ctx context.Context, sessionID string, file *models.MediaFile, plan *playback.PlanV3, selectedIndex int, recipe *playback.ExecutableRecipeV3, clientFeatures []string) error {
	if plan == nil || file == nil {
		return nil
	}
	var frozenDownloaded *subtitles.DownloadedSubtitle
	if recipe != nil && recipe.SubtitleSource == playback.SubtitleSourceDownloadedV3 {
		if h == nil || h.SubtitleRepo == nil || recipe.DownloadedSubtitleID <= 0 {
			return errors.New("the frozen downloaded subtitle is unavailable")
		}
		selected, err := h.SubtitleRepo.GetDownloadedSubtitle(ctx, recipe.DownloadedSubtitleID)
		if err != nil {
			return wrapSubtitleStoreErrorV3(err)
		}
		if selected == nil || selected.MediaFileID != file.ID {
			return errors.New("the frozen downloaded subtitle is unavailable for the selected media file")
		}
		frozenDownloaded = selected
	}
	inventory := playback.ScopeSubtitleInventoryV3(sessionID, file, plan.Subtitle.Inventory, clientFeatures)
	// A plan restored from JSON no longer carries the server-only downloaded
	// row IDs. Rebuild only in that case; a fresh plan stays on the exact
	// planning snapshot instead of listing a mutable repository twice.
	if playback.SubtitleInventoryNeedsDownloadedIdentityV3(plan.Subtitle.Inventory) {
		if h == nil || h.SubtitleRepo == nil {
			return errors.New("the downloaded subtitle inventory is unavailable")
		}
		downloaded, err := h.SubtitleRepo.ListDownloadedSubtitles(ctx, file.ID)
		if err != nil {
			return wrapSubtitleStoreErrorV3(err)
		}
		inventory = playback.ScopeSubtitleInventoryV3(sessionID, file, playback.BuildSubtitleInventoryV3(file, downloadedSubtitleEntriesV3(file, downloaded)), clientFeatures)
	}
	plan.Subtitle.Inventory = inventory
	// Only render and convert publish a client-fetchable artifact; off and
	// burn_in have none by definition. Clear rather than leave whatever the plan
	// arrived with: a seek reanchor replays record.CurrentPlan verbatim
	// (frozenSeekReanchorResultV3), so a plan that once rendered a sidecar would
	// otherwise keep republishing that artifact after the selection changed —
	// which is exactly the stale `mode: "off"` plus artifact pair observed in
	// the field, and enough for a client to alias the artifact onto the track it
	// is actually playing. An off decision carries no track either, so its
	// track_id goes with the artifact; burn_in keeps the track it burns in.
	if selectedIndex < 0 || (plan.Subtitle.Mode != playback.SubtitleRenderV3 && plan.Subtitle.Mode != playback.SubtitleConvertV3) {
		plan.Subtitle.Artifact = nil
		plan.Subtitle.Embedded = nil
		if plan.Subtitle.Mode == playback.SubtitleOffV3 {
			plan.Subtitle.TrackID = ""
		}
		return nil
	}
	item, ok := playback.SubtitleInventoryItemAtV3(inventory, selectedIndex)
	if !ok && frozenDownloaded == nil {
		return errors.New("selected subtitle artifact is absent from the frozen inventory")
	}
	if embedded := plan.Subtitle.Embedded; embedded != nil {
		if plan.Delivery != playback.DeliveryOriginalHTTPV3 || plan.Subtitle.Mode != playback.SubtitleRenderV3 || !ok || item.Source != playback.SubtitleSourceEmbeddedV3 || item.TrackID != plan.Subtitle.TrackID {
			return errors.New("invalid embedded subtitle route")
		}
		// selectedIndex is a published ordinal; the underlying container track
		// lives at the source ordinal the de-duplication assigns it.
		sourceIndex, mapped := playback.SubtitleInventoryOwnSourceIndexV3(file, selectedIndex)
		if !mapped {
			return errors.New("the selected embedded subtitle identity changed")
		}
		ordinal := sourceIndex - len(file.ExternalSubtitles)
		// A frozen route without a container track ID was selected by stream
		// index, so a container ID recorded on the file since (the Matroska
		// track number backfill) does not change the track the client plays.
		if ordinal < 0 || ordinal >= len(file.SubtitleTracks) || file.SubtitleTracks[ordinal].Index != embedded.StreamIndex ||
			(embedded.ContainerTrackID != "" && file.SubtitleTracks[ordinal].ContainerTrackID != embedded.ContainerTrackID) {
			return errors.New("the selected embedded subtitle identity changed")
		}
		plan.Subtitle.Artifact = nil
		return nil
	}
	if frozenDownloaded == nil && item.URL == "" {
		return fmt.Errorf("subtitle track %d is %s and has no fetchable artifact", selectedIndex, item.Delivery)
	}
	format := strings.ToLower(item.Codec)
	source := item.Source
	url := item.URL
	if frozenDownloaded != nil {
		format = strings.ToLower(string(frozenDownloaded.Format))
		source = playback.SubtitleSourceDownloadedV3
		ext := playback.SubtitleSidecarExtV3(format, source, clientFeatures)
		url = playback.DownloadedSubtitleStreamURLV3(sessionID, selectedIndex, ext, file.ID, frozenDownloaded.ID)
		// The plan's selected ordinal must advertise the same opaque URL as the
		// artifact even if another downloaded row was inserted before a seek.
		for index := range plan.Subtitle.Inventory {
			if plan.Subtitle.Inventory[index].CombinedIndex == selectedIndex {
				plan.Subtitle.Inventory[index].URL = url
				plan.Subtitle.Inventory[index].Codec = string(frozenDownloaded.Format)
				break
			}
		}
	}
	// Inventory codec names describe the source; artifact metadata describes
	// the bytes served at its URL (SRT/mov_text are delivered as WebVTT unless
	// the client negotiated original SRT).
	format = strings.TrimPrefix(playback.SubtitleSidecarExtV3(format, source, clientFeatures), ".")
	mime := subtitleMIMEV3(format)
	if plan.Subtitle.Mode == playback.SubtitleConvertV3 {
		format = playback.SubtitleFormatVTTV3
		mime = playback.SubtitleMIMEVTTV3
		url = forceSubtitleExtensionV3(url, playback.SubtitleExtVTTV3)
	}
	plan.Subtitle.Artifact = &playback.SubtitleArtifactV3{URL: url, MIMEType: mime, Format: format, TimingOriginSeconds: 0}
	return nil
}

// downloadedSubtitleInventoryWithErrorV3 lists downloaded/AI tracks and
// propagates any repository read failure so callers can fail closed instead of
// silently serving a partial inventory.
func (h *PlaybackHandler) downloadedSubtitleInventoryWithErrorV3(ctx context.Context, file *models.MediaFile) ([]playback.SubtitleInventoryEntryV3, error) {
	if h == nil || h.SubtitleRepo == nil || file == nil {
		return nil, nil
	}
	downloaded, err := h.SubtitleRepo.ListDownloadedSubtitles(ctx, file.ID)
	if err != nil {
		return nil, err
	}
	return downloadedSubtitleEntriesV3(file, downloaded), nil
}

// downloadedSubtitleInventoryV3 lists the downloaded and AI-generated tracks
// that follow the file's own tracks in the combined-ordinal space. The
// repository orders by created_at, so the ordinals it produces are stable. A
// failed lookup lists nothing, so planning carries on as if the file had no
// such tracks; listDownloadedSubtitlesV3 reports the failure instead.
func (h *PlaybackHandler) downloadedSubtitleInventoryV3(ctx context.Context, file *models.MediaFile) []playback.SubtitleInventoryEntryV3 {
	entries, _ := h.listDownloadedSubtitlesV3(ctx, file)
	return entries
}

// listDownloadedSubtitlesV3 is downloadedSubtitleInventoryV3 that reports a
// failed lookup, wrapped as a subtitle-store outage.
func (h *PlaybackHandler) listDownloadedSubtitlesV3(ctx context.Context, file *models.MediaFile) ([]playback.SubtitleInventoryEntryV3, error) {
	if h == nil || h.SubtitleRepo == nil || file == nil {
		return nil, nil
	}
	downloaded, err := h.SubtitleRepo.ListDownloadedSubtitles(ctx, file.ID)
	if err != nil {
		return nil, wrapSubtitleStoreErrorV3(err)
	}
	return downloadedSubtitleEntriesV3(file, downloaded), nil
}

// selectsDownloadedSubtitleV3 reports whether request selects a subtitle past
// file's own external and embedded tracks, the range downloaded subtitles
// occupy.
func selectsDownloadedSubtitleV3(file *models.MediaFile, request playback.StartRequestV3) bool {
	if file == nil {
		return false
	}
	index := -1
	if request.SubtitleTrackIndex != nil {
		index = *request.SubtitleTrackIndex
	} else if request.SubtitleTrackID != "" {
		if fileID, kind, ordinal, ok := playback.ParseTrackIDV3(request.SubtitleTrackID); ok && kind == subtitleTrackKindV3 && fileID == file.ID {
			index = ordinal
		}
	}
	return index >= len(file.ExternalSubtitles)+len(file.SubtitleTracks)
}

// downloadedSubtitleEntriesV3 converts downloaded rows into inventory entries
// at the ordinals that follow the file's external and embedded tracks.
func downloadedSubtitleEntriesV3(file *models.MediaFile, downloaded []subtitles.DownloadedSubtitle) []playback.SubtitleInventoryEntryV3 {
	if file == nil {
		return nil
	}
	base := len(file.ExternalSubtitles) + len(file.SubtitleTracks)
	result := make([]playback.SubtitleInventoryEntryV3, 0, len(downloaded))
	for index, value := range downloaded {
		result = append(result, playback.SubtitleInventoryEntryV3{
			CombinedIndex:        base + index,
			Codec:                string(value.Format),
			Source:               playback.SubtitleSourceDownloadedV3,
			Language:             value.Language,
			Label:                downloadedSubtitleLabelV3(value),
			HearingImpaired:      value.HearingImpaired,
			DownloadedSubtitleID: value.ID,
		})
	}
	return result
}

func downloadedSubtitleLabelV3(value subtitles.DownloadedSubtitle) string {
	if value.ReleaseName == "" && value.Provider == "" {
		return ""
	}
	return value.ReleaseName + " (" + value.Provider + ")"
}

// stripClientSuppliedAutomatic clears the server-owned `Automatic` provenance
// marker on a decoded client replan request. Reconciliation builds its own
// replan in-process with the marker already set; that path never passes
// through this function. A client that forges the marker would otherwise
// impersonate server reconciliation and suppress its own preference
// persistence, so the inbound boundary drops whatever arrived on the wire.
func stripClientSuppliedAutomatic(req *playback.ReplanRequestV3) {
	if req == nil {
		return
	}
	req.Automatic = ""
}

// HandleReplanPlaybackV3 provides persistent idempotency and preserves the old
// transport until a successor has entered its startup state and the new plan is
// durably committed.
func (h *PlaybackHandler) HandleReplanPlaybackV3(w http.ResponseWriter, r *http.Request) {
	userID := apimw.GetUserID(r.Context())
	profileID := apimw.GetProfileID(r.Context())
	if userID == 0 || profileID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication and profile are required")
		return
	}
	body, err := readBoundedV3Body(w, r, maxPlaybackV3BodyBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}
	response, err := h.replanPlaybackApplicationV3(r, chiURLParamV3(r, "session_id"), body)
	if err != nil {
		writePlaybackOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// replanPlaybackApplicationV3 is the replan application shared by the v1
// handler and the v2 service seam: idempotent on replan_request_id + body
// digest, serialized per session, and atomic between the durable plan and the
// live transport. Errors are *PlaybackOperationError.
func (h *PlaybackHandler) replanPlaybackApplicationV3(r *http.Request, sessionID string, body []byte) (playback.DecisionResponseV3, error) {
	userID := apimw.GetUserID(r.Context())
	profileID := apimw.GetProfileID(r.Context())
	if userID == 0 || profileID == "" {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusUnauthorized, "unauthorized", "Authentication and profile are required")
	}
	var req playback.ReplanRequestV3
	if err := json.Unmarshal(body, &req); err != nil {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusBadRequest, "bad_request", "Invalid replan request")
	}
	// Automatic marks a server-built reconciliation replan. It is internal
	// provenance: a client that forges it would suppress preference
	// persistence and impersonate reconciliation, so strip whatever the
	// client sent and let the server set it only on its own path.
	stripClientSuppliedAutomatic(&req)
	// Reject malformed identity/bounds before doing any session lookup. When
	// client_features is omitted, temporarily allow the only validation rule
	// that depends on the durable start request; the authoritative merge and a
	// second full validation happen after the attempt is loaded below.
	// Validate is structural only — it neither normalizes nor drops a
	// capability — so this preflight cannot consume the un-negotiated
	// transformations the single post-merge normalization must later report.
	preflightReq := req
	if preflightReq.ClientFeatures == nil {
		preflightReq.ClientFeatures = []string{playback.FeatureClientVideoTransforms}
	}
	if err := preflightReq.Validate(); err != nil {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusBadRequest, "bad_request", "Invalid replan request")
	}
	// Resolve the cheap attempt/session lookups before queueing for a replan
	// slot. A reaped in-memory session (or an expired attempt) must surface as
	// a fast 404 instead of waiting behind the replan capacity bound;
	// production saw an ~11s queue wait before the session_not_found verdict.
	// The store read is a single pooled query that releases its connection
	// immediately and holds none of the replan locks, so it cannot invert the
	// slot -> per-session mutex -> advisory-lock order below or starve a lock
	// holder's inner queries. It is an early-out only: the attempt is re-read
	// under those locks before any lease decision, and CompleteReplan's
	// compare-and-swap remains the final authority.
	record, err := h.PlanStoreV3.GetAttempt(r.Context(), sessionID)
	if err != nil {
		// A store outage must read as retryable, not as the session being
		// gone: clients tear playback down on session_not_found.
		if !errors.Is(err, playback.ErrSessionNotFound) {
			return playback.DecisionResponseV3{}, playbackOperationError(http.StatusInternalServerError, "internal_error", "Failed to load the playback attempt")
		}
		return playback.DecisionResponseV3{}, replanSessionNotFoundV3()
	}
	if record.UserID != userID || record.ProfileID != profileID {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusForbidden, "forbidden", "Session belongs to another profile")
	}
	if err := requireAttemptAPISurfaceV3(r.Context(), record, nil); err != nil {
		return playback.DecisionResponseV3{}, err
	}
	if record.PlaybackAttemptID != req.PlaybackAttemptID {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusConflict, "stale_playback_plan", "The failed plan is no longer current")
	}
	// Replan feature advertisement is optional. An omitted list and an
	// explicitly empty list both mean "unchanged"; validate transformations
	// against the durable start-time features in either case, otherwise a valid
	// replan can be rejected before executeReplanV3 gets the chance to perform
	// the same merge.
	if len(req.ClientFeatures) == 0 {
		req.ClientFeatures = append([]string(nil), record.NormalizedRequest.ClientFeatures...)
	}

	// Attempt-sticky features negotiated on the original attempt stay pinned
	// across every replan of that attempt.
	req.ClientFeatures = playback.PinAttemptStickyFeaturesV3(req.ClientFeatures, record.NormalizedRequest.ClientFeatures)
	if err := req.Validate(); err != nil {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusBadRequest, "bad_request", "Invalid replan request")
	}
	if _, err := h.sessionMgr.GetSession(sessionID); err != nil {
		return playback.DecisionResponseV3{}, replanSessionNotFoundV3()
	}
	releaseSlot, err := h.acquireReplanSlotV3(r.Context())
	if err != nil {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusServiceUnavailable, "replan_capacity_exhausted", "The server is replanning too many sessions; retry shortly")
	}
	defer releaseSlot()
	unlockReplan := h.lockReplanV3(sessionID)
	defer unlockReplan()
	unlockStore, err := h.PlanStoreV3.AcquireSessionLock(r.Context(), sessionID)
	if err != nil {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusInternalServerError, "internal_error", "Failed to serialize the replan request")
	}
	defer unlockStore()
	// Re-read the attempt now that the replan locks are held. The fast 404 above
	// deliberately reads before queueing on the capacity bound and the
	// per-session locks, so a duplicate request that waited behind a completing
	// sibling still holds the pre-lock snapshot here. Without this read,
	// BeginReplan can return the sibling's ReplanLeaseCompletedV3 and the stale
	// record makes the handler reject the replay as stale_playback_plan instead
	// of returning the stored response; BeginReplan/CompleteReplan would also
	// build on a stale base revision. The post-lock read is authoritative for
	// both, and CompleteReplan stays the final compare-and-swap.
	record, err = h.PlanStoreV3.GetAttempt(r.Context(), sessionID)
	if err != nil {
		// Match the fast 404: a session that vanished while this request waited
		// reads as gone, any other store failure as retryable.
		if !errors.Is(err, playback.ErrSessionNotFound) {
			return playback.DecisionResponseV3{}, playbackOperationError(http.StatusInternalServerError, "internal_error", "Failed to load the playback attempt")
		}
		return playback.DecisionResponseV3{}, replanSessionNotFoundV3()
	}
	// The durable attempt can outlive the in-memory session: a stop or an idle
	// reap can land while this request waits on the replan slot and the
	// per-session locks, between the pre-lock check and here. Re-check now so it
	// reads as the fast 404; otherwise the request would reserve a replan lease
	// and persist executeReplanV3's session_expired as a terminal 200, which the
	// web client does not rebuild from. CompleteReplan stays the final
	// compare-and-swap.
	if _, err := h.sessionMgr.GetSession(sessionID); err != nil {
		return playback.DecisionResponseV3{}, replanSessionNotFoundV3()
	}
	digestBytes := sha256.Sum256(body)
	digest := hex.EncodeToString(digestBytes[:])
	lease, err := h.PlanStoreV3.BeginReplan(
		r.Context(),
		sessionID,
		req.ReplanRequestID,
		digest,
		record.CurrentReplanRequestID,
		time.Now().Add(replanLeaseDurationV3),
	)
	if errors.Is(err, playback.ErrIdempotencyKeyReusedV3) {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusConflict, "idempotency_key_reused", "The replan request ID was reused with different input")
	}
	if errors.Is(err, playback.ErrStaleReplanLeaseV3) {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusConflict, "stale_playback_plan", "A newer replacement plan is already active")
	}
	if err != nil {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusInternalServerError, "internal_error", "Failed to reserve the replan request")
	}
	if lease.State == playback.ReplanLeaseInFlightV3 {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusConflict, "replan_in_progress", "An identical replan is still in progress")
	}
	if lease.State == playback.ReplanLeaseCompletedV3 {
		if record.CurrentReplanRequestID != req.ReplanRequestID || !completedReplanResponseMatchesAttemptV3(lease.Response, record) {
			return playback.DecisionResponseV3{}, playbackOperationError(http.StatusConflict, "stale_playback_plan", "A newer replacement plan is already active")
		}
		if _, err := h.sessionMgr.GetSession(sessionID); err != nil {
			return playback.DecisionResponseV3{}, replanSessionNotFoundV3()
		}
		var replay playback.DecisionResponseV3
		if err := json.Unmarshal(lease.Response, &replay); err != nil {
			return playback.DecisionResponseV3{}, playbackOperationError(http.StatusInternalServerError, "internal_error", "Failed to decode the completed replan decision")
		}
		// The cached durable decision was minted for the surface that first
		// completed it. A replay through a different surface must not re-emit
		// v2-only contract additions (today delivery_change) on the frozen v1
		// bridge: clear the marker on a response-local plan copy here, at the
		// same response boundary the fresh v1 execution uses.
		if !isNativeAPIV2(r.Context()) && replay.PlaybackPlan != nil {
			replay.PlaybackPlan.DeliveryChange = nil
		}
		return replay, nil
	}
	leaseCompleted := false
	defer func() {
		if leaseCompleted {
			return
		}
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), replanReleaseTimeoutV3)
		defer cancel()
		if err := h.PlanStoreV3.ReleaseReplan(releaseCtx, sessionID, req.ReplanRequestID, lease.LeaseToken); err != nil {
			slog.ErrorContext(r.Context(), "protocol v3 replan lease release failed", "component", "api", "session", sessionID, "replan_request_id", req.ReplanRequestID, "error", err)
		}
	}()
	if record.CurrentPlanID != req.FailedPlanID {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusConflict, "stale_playback_plan", "The failed plan is no longer current")
	}
	// The replan budget must cover the declared virtual resolution budgets
	// (60s startup / 15s probe) without allowing hangs; 30s is a bounded value
	// that fits a slow provider's synchronous rehydration while still failing
	// fast on a genuinely stuck provider.
	replanCtx, cancelReplan := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancelReplan()
	// A viewer who re-armed Auto mid-session states it on the replan, so a
	// later dead-source recovery rotates even though the session started
	// explicit. Applying it before execution is what lets executeReplanV3's
	// autoFallbackForSession read the same intent the client is showing; it is
	// also written to the durable normalized request so a reconstructed session
	// keeps the negotiated policy instead of reverting to the start request. A
	// policy the session cannot adopt refuses the replan rather than continuing
	// with a stale intent.
	//
	// The apply is speculative: executeReplanV3 may still fail, and the live
	// flag is read mid-execution, so capture the prior policy first and restore
	// both halves on every failure path below. Only a replacement plan that
	// actually commits keeps the new policy, which is when live and durable
	// already agree on it.
	autoFallbackRollback := h.captureAutoFallbackRollbackV3(sessionID, record)
	autoFallbackCommitted := false
	defer func() {
		if !autoFallbackCommitted {
			autoFallbackRollback.restore()
		}
	}()
	if err := h.renegotiateAutoFallbackV3(sessionID, record, req); err != nil {
		slog.WarnContext(r.Context(), "protocol v3 replan auto-fallback re-negotiation failed",
			"component", "api", "session", sessionID, "error", err)
		if errors.Is(err, playback.ErrSessionNotFound) {
			return playback.DecisionResponseV3{}, replanSessionNotFoundV3()
		}
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusInternalServerError, "internal_error", "Failed to apply the version fallback policy")
	}
	response, updated, transport, replanErr := h.executeReplanV3(r.WithContext(replanCtx), record, req)
	if replanErr != nil {
		if transport != nil {
			transport.rollback()
		}
		// The post-lock live-session check above is an early-out only: it holds
		// no lock across BeginReplan or executeReplanV3, so a stop or idle reap
		// can still land before execution reads the session. Persisting that
		// session_expired through CompleteReplan would store it as the terminal
		// decision and answer HTTP 200 — a response the web client does not
		// rebuild from. Translate it to the same fast 404 and leave the lease to
		// the deferred non-terminal ReleaseReplan, so no completed 200 terminal
		// for a dead session is ever persisted. CompleteReplan stays the final
		// compare-and-swap for every other terminal path.
		if replanErr.reason == "session_expired" {
			return playback.DecisionResponseV3{}, replanSessionNotFoundV3()
		}
		response := playback.NewTerminalResponseV3(replanErr.reason, replanErr.message, replanErr.retryable)
		encoded, _ := json.Marshal(response)
		// The failed replan did not adopt the stated policy, so the terminal
		// record must persist the policy in force before it, not the one this
		// request tried. Restore the durable half now, before the record is
		// copied into the terminal decision; the deferred restore covers the
		// live half and re-applies harmlessly.
		autoFallbackRollback.restore()
		terminalRecord := *record
		terminalRecord.CurrentReplanRequestID = req.ReplanRequestID
		if err := h.PlanStoreV3.CompleteReplan(r.Context(), sessionID, req.ReplanRequestID, lease.LeaseToken, record.CurrentReplanRequestID, encoded, terminalRecord); err != nil {
			if errors.Is(err, playback.ErrAttemptStoppedV3) {
				return playback.DecisionResponseV3{}, replanSessionNotFoundV3()
			}
			if errors.Is(err, playback.ErrReplanSupersededV3) {
				return playback.DecisionResponseV3{}, playbackOperationError(http.StatusConflict, "stale_playback_plan", "A newer replacement plan is already active")
			}
			if isRouteCapacityUnavailableError(replanErr) {
				return playback.DecisionResponseV3{}, playbackOperationError(http.StatusServiceUnavailable, string(noderouting.OutcomeCapacityUnavailable), "The playback route capacity is temporarily unavailable; retry shortly")
			}
			return playback.DecisionResponseV3{}, playbackOperationError(http.StatusInternalServerError, "internal_error", "Failed to persist the terminal replan decision")
		}
		leaseCompleted = true
		return response, nil
	}

	updated.CurrentReplanRequestID = req.ReplanRequestID
	encoded, _ := json.Marshal(response)
	var rollbackSession func() error
	if transport != nil && transport.applySession != nil {
		var err error
		rollbackSession, err = transport.applySession()
		if err != nil {
			if rollbackErr, _ := rollbackFailedReplanV3(transport, nil); rollbackErr != nil {
				slog.ErrorContext(r.Context(), "protocol v3 unapplied replacement transport cancellation failed", "session", sessionID, "error", rollbackErr)
				return playback.DecisionResponseV3{}, playbackOperationError(http.StatusInternalServerError, "internal_error", "Failed to cancel the unapplied replacement transport")
			}
			if isRouteCapacityUnavailableError(err) {
				return playback.DecisionResponseV3{}, playbackOperationError(http.StatusServiceUnavailable, string(noderouting.OutcomeCapacityUnavailable), "The playback route capacity is temporarily unavailable; retry shortly")
			}
			return playback.DecisionResponseV3{}, playbackOperationError(http.StatusInternalServerError, "internal_error", "Failed to commit the live replacement session")
		}
	}
	if err := h.PlanStoreV3.CompleteReplan(r.Context(), sessionID, req.ReplanRequestID, lease.LeaseToken, record.CurrentReplanRequestID, encoded, updated); err != nil {
		if errors.Is(err, playback.ErrAttemptStoppedV3) {
			// A stop landed on another replica while this replan ran: tear the
			// replacement transport and the local session down and report the
			// session gone; the deny marker refuses the plan anyway.
			if transportRollbackErr, _ := rollbackFailedReplanV3(transport, rollbackSession); transportRollbackErr != nil {
				slog.ErrorContext(r.Context(), "protocol v3 replacement transport cancellation failed", "session", sessionID, "error", transportRollbackErr)
			}
			_ = h.abortPlaybackSessionByID(context.WithoutCancel(r.Context()), sessionID)
			return playback.DecisionResponseV3{}, replanSessionNotFoundV3()
		}
		transportRollbackErr, sessionRollbackErr := rollbackFailedReplanV3(transport, rollbackSession)
		if transportRollbackErr != nil {
			slog.ErrorContext(r.Context(), "protocol v3 replacement transport cancellation failed", "session", sessionID, "error", transportRollbackErr)
			return playback.DecisionResponseV3{}, playbackOperationError(http.StatusInternalServerError, "internal_error", "Failed to cancel the replacement transport")
		}
		if sessionRollbackErr != nil {
			slog.ErrorContext(r.Context(), "protocol v3 replacement rollback failed", "session", sessionID, "error", sessionRollbackErr)
			_ = h.stopPlaybackSessionByID(context.WithoutCancel(r.Context()), sessionID, false)
		}
		if errors.Is(err, playback.ErrReplanSupersededV3) {
			return playback.DecisionResponseV3{}, playbackOperationError(http.StatusConflict, "stale_playback_plan", "A newer replacement plan is already active")
		}
		if isRouteCapacityUnavailableError(err) {
			return playback.DecisionResponseV3{}, playbackOperationError(http.StatusServiceUnavailable, string(noderouting.OutcomeCapacityUnavailable), "The playback route capacity is temporarily unavailable; retry shortly")
		}
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusInternalServerError, "internal_error", "Failed to commit the replacement plan")
	}
	leaseCompleted = true
	// The replacement plan is durable, so the speculative policy application is
	// now the adopted policy: live and durable agree and the deferred rollback
	// must not undo it. A later transport commit failure aborts the live session
	// without reverting the durable plan.
	autoFallbackCommitted = true
	if transport != nil {
		if commitErr := transport.commit(); commitErr != nil {
			_ = h.abortPlaybackSessionByID(context.WithoutCancel(r.Context()), sessionID)
			return playback.DecisionResponseV3{}, playbackOperationError(http.StatusServiceUnavailable, commitErr.reason, commitErr.message)
		}
		if transport.afterDurableCommit != nil {
			transport.afterDurableCommit()
		}
	}
	h.raceCopySafetyV3(updated.EffectiveMediaFileID, response.PlaybackPlan)
	return response, nil
}

// replanSessionNotFoundV3 is the v1 playback_session_not_found body; the v2
// adapter maps its status to the not-found problem.
func replanSessionNotFoundV3() *PlaybackOperationError {
	return playbackOperationError(http.StatusNotFound, playbackSessionNotFoundErrorCode, "Playback session not found")
}

// isRouteCapacityUnavailableError reports whether an error names route-capacity
// exhaustion so the replan handler keeps the client-visible code retryable (503)
// instead of collapsing it into a 500 internal_error. It matches the structured
// transport reason on the error or its cause, and falls back to the sentinel
// string the candidate-exhaustion join carries.
func isRouteCapacityUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	var transportErr *transportErrorV3
	if errors.As(err, &transportErr) {
		if transportErr.reason == string(noderouting.OutcomeCapacityUnavailable) {
			return true
		}
		if transportErr.cause != nil && strings.Contains(transportErr.cause.Error(), string(noderouting.OutcomeCapacityUnavailable)) {
			return true
		}
	}
	return strings.Contains(err.Error(), string(noderouting.OutcomeCapacityUnavailable))
}

// rollbackFailedReplanV3 cancels a remotely admitted replacement before it
// restores the predecessor session. If cancellation cannot be confirmed, the
// successor remains live and authoritative so a later stop can retry it.
func rollbackFailedReplanV3(transport *preparedTransportV3, rollbackSession func() error) (transportErr, sessionErr error) {
	if transport != nil {
		if transport.rollbackRequired != nil {
			if err := transport.rollbackRequired(); err != nil {
				return err, nil
			}
		} else {
			transport.rollback()
		}
	}
	if rollbackSession != nil {
		return nil, rollbackSession()
	}
	return nil, nil
}

// raceCopySafetyV3 resolves an unknown H.264 copy-safety verdict behind a plan
// that stream-copies video. It is called after the durable commit on both the
// start and replan paths, so the scan only ever chases a route a client was
// actually handed, and it returns immediately — no response waits on it.
func (h *PlaybackHandler) raceCopySafetyV3(fileID int, plan *playback.PlanV3) {
	if h == nil || h.CopySafetyRacer == nil || fileID <= 0 || plan == nil {
		return
	}
	h.CopySafetyRacer.RaceScanForPlan(fileID, plan)
}

type candidateFailureStage string

const (
	candidateStageResolve          candidateFailureStage = "resolve"
	candidateStageAudioRemap       candidateFailureStage = "audio_remap"
	candidateStageSubtitleRemap    candidateFailureStage = "subtitle_remap"
	candidateStagePreflight        candidateFailureStage = "preflight"
	candidateStageNormalize        candidateFailureStage = "normalize"
	candidateStagePlan             candidateFailureStage = "plan"
	candidateStageAdmission        candidateFailureStage = "admission"
	candidateStageTranscodePerm    candidateFailureStage = "transcode_permission"
	candidateStageTransport        candidateFailureStage = "transport"
	candidateStageRecipe           candidateFailureStage = "recipe"
	candidateStageSubtitleArtifact candidateFailureStage = "subtitle_artifact"
)

type candidateErrorV3 struct {
	Stage          candidateFailureStage
	Reason         string
	Message        string
	TerminalReason string
	TransportErr   *transportErrorV3
	Err            error
}

func candidateStagePriority(s candidateFailureStage) int {
	switch s {
	case candidateStageSubtitleArtifact:
		return 10
	case candidateStageRecipe:
		return 9
	case candidateStageTransport:
		return 8
	case candidateStageTranscodePerm:
		return 7
	case candidateStageAdmission:
		return 6
	case candidateStagePlan:
		return 5
	case candidateStageNormalize:
		return 4
	case candidateStagePreflight:
		return 3
	case candidateStageAudioRemap, candidateStageSubtitleRemap:
		return 2
	case candidateStageResolve:
		return 1
	default:
		return 0
	}
}

func (e *candidateErrorV3) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil {
		return fmt.Sprintf("[%s] %s: %v", e.Stage, e.Message, logredact.SanitizeURLError(e.Err))
	}
	return fmt.Sprintf("[%s] %s", e.Stage, e.Message)
}

func (e *candidateErrorV3) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// candidateSubtitleLocalFailureV3 reports whether a candidate was rejected for
// a subtitle reason rather than a video/transport one. Such a candidate is held
// as the subtitle-degrade target: dropping the subtitle and playing its video
// changes the release less than skipping it for a sibling that also cannot
// honor the selection. A video/transport rejection is deliberately excluded so
// the genuine failover paths are untouched.
func candidateSubtitleLocalFailureV3(err *candidateErrorV3) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errSubtitleUnavailableInTargetV3) {
		return true
	}
	return err.Stage == candidateStageSubtitleArtifact
}

func classifyVirtualReplanExhaustionV3(initialVirtualErr error, candidateErrs []*candidateErrorV3) *transportErrorV3 {
	var joinedErr error
	if initialVirtualErr != nil {
		joinedErr = initialVirtualErr
	}
	var highestCandidate *candidateErrorV3
	highestPriority := -1
	for _, ce := range candidateErrs {
		if ce != nil {
			if ce.Err != nil {
				joinedErr = errors.Join(joinedErr, ce.Err)
			} else if ce.Message != "" {
				joinedErr = errors.Join(joinedErr, errors.New(ce.Message))
			}
			p := candidateStagePriority(ce.Stage)
			if p > highestPriority {
				highestPriority = p
				highestCandidate = ce
			}
		}
	}

	if errors.Is(initialVirtualErr, context.DeadlineExceeded) && (highestCandidate == nil || highestCandidate.Stage == candidateStageResolve) {
		return &transportErrorV3{
			reason:    "replan_virtual_input_timeout",
			message:   "Virtual stream input resolution timed out during replan.",
			retryable: true,
			cause:     joinedErr,
		}
	}
	for _, ce := range candidateErrs {
		if ce != nil && ce.Stage == candidateStageResolve && errors.Is(ce.Err, context.DeadlineExceeded) {
			if highestCandidate == nil || highestCandidate.Stage == candidateStageResolve {
				return &transportErrorV3{
					reason:    "replan_virtual_input_timeout",
					message:   "Virtual stream input resolution timed out during replan.",
					retryable: true,
					cause:     joinedErr,
				}
			}
		}
	}

	// Route-capacity exhaustion is an honest, retryable verdict: the selected
	// workload has no route capacity right now, not a broken plan. Surface it
	// even when another candidate failed at a higher-priority stage first (for
	// example a canceled virtual re-resolve), because the client's correct
	// response is to retry. Without this the final reason can be a masking
	// transport error and the replan handler answers 500 internal_error
	// instead of a retryable 503, which stranded the HLS fallback in the field.
	for _, ce := range candidateErrs {
		if ce != nil && ce.TransportErr != nil &&
			ce.TransportErr.reason == string(noderouting.OutcomeCapacityUnavailable) {
			return &transportErrorV3{
				reason:    string(noderouting.OutcomeCapacityUnavailable),
				message:   "The playback route capacity is temporarily unavailable; retry shortly",
				retryable: true,
				cause:     joinedErr,
			}
		}
	}

	// A limit-provider blip is not a capacity limit. Replacement admission
	// fails open on it, but if it still surfaces (for example through a later
	// transcode-permission lookup) classify it as the transient dependency
	// failure it is instead of reporting capacity_unavailable or
	// transcoding_disabled.
	if highestCandidate != nil &&
		(highestCandidate.Stage == candidateStageAdmission || highestCandidate.Stage == candidateStageTranscodePerm) &&
		errors.Is(highestCandidate.Err, playback.ErrLimitProviderUnavailable) {
		return &transportErrorV3{
			reason:    "limit_provider_unavailable",
			message:   "Playback limits could not be checked. Please retry.",
			retryable: true,
			cause:     joinedErr,
		}
	}

	if highestCandidate != nil {
		if highestCandidate.TransportErr != nil {
			cloned := *highestCandidate.TransportErr
			cloned.cause = joinedErr
			return &cloned
		}
		switch highestCandidate.Stage {
		case candidateStageTransport:
			reason := highestCandidate.Reason
			if reason == "" {
				reason = transcodeStartFailedReasonV3
			}
			return &transportErrorV3{
				reason:    reason,
				message:   "Playback transport failed for all alternate media versions.",
				retryable: true,
				cause:     joinedErr,
			}
		case candidateStageSubtitleArtifact:
			return subtitleArtifactErrorV3("Failed to prepare the selected subtitle artifact.", joinedErr)
		case candidateStageSubtitleRemap:
			// A subtitle remap miss is a subtitle-local outcome. Reporting it
			// as virtual_source_unavailable would let the generic candidate
			// machinery treat a track that every compatible version lacks as
			// a failed media source. Name the subtitle instead so the client
			// can drop or re-pick it without a release swap.
			return subtitleArtifactErrorV3("The selected subtitle is unavailable in every compatible media version.", joinedErr)
		case candidateStageAudioRemap:
			// An audio remap miss is an audio-local outcome. The candidate's
			// audio inventory is what failed to bind, not its video stream.
			// Reporting virtual_source_unavailable would let the generic
			// candidate machinery indict the release for a track the file
			// lacks or the client sent malformed. Name the audio so the client
			// can pick another track without a release swap.
			return &transportErrorV3{
				reason:    trackUnavailableReasonV3,
				message:   "The selected audio track is unavailable in every compatible media version.",
				retryable: false,
				cause:     joinedErr,
			}
		case candidateStageTranscodePerm:
			return &transportErrorV3{
				reason:    "transcoding_disabled",
				message:   "The required server adaptation is disabled for this user.",
				retryable: false,
				cause:     joinedErr,
			}
		case candidateStageAdmission:
			return &transportErrorV3{
				reason:    "capacity_unavailable",
				message:   "No playback capacity is available for alternate versions.",
				retryable: true,
				cause:     joinedErr,
			}
		}
	}

	return &transportErrorV3{
		reason:    "virtual_source_unavailable",
		message:   "The virtual source could not be refreshed for playback.",
		retryable: true,
		cause:     joinedErr,
	}
}

type candidateEvaluationV3 struct {
	start           playback.StartRequestV3
	file            *models.MediaFile
	result          playback.PlannerResultV3
	frozenRecipe    playback.ExecutableRecipeV3
	transport       preparedTransportV3
	reservationHeld bool
	toneMapErr      error
}

func (h *PlaybackHandler) evaluateReplanCandidateV3(
	r *http.Request,
	session *playback.Session,
	record *playback.AttemptRecordV3,
	req playback.ReplanRequestV3,
	baseStart playback.StartRequestV3,
	sourceFile *models.MediaFile,
	candidateAlternate *models.MediaFile,
	plannerRequestedFile *models.MediaFile,
	plannerSettings playback.PlannerSettingsV3,
	plannerSettingsErr error,
	attemptedKeys []string,
) (*candidateEvaluationV3, *candidateErrorV3) {
	if candidateAlternate == nil {
		return nil, &candidateErrorV3{Stage: candidateStageResolve, Message: "nil candidate alternate"}
	}
	candidateFile, candidateProvenance, err := h.prepareVirtualAlternateFileV3(r, candidateAlternate, record.ProfileID)
	if err != nil || candidateFile == nil {
		if err == nil {
			err = errors.New("candidate alternate unavailable")
		}
		return nil, &candidateErrorV3{Stage: candidateStageResolve, Message: fmt.Sprintf("candidate %d resolution failed", candidateAlternate.ID), Err: err}
	}
	return h.evaluatePreparedReplanCandidateV3(r, session, record, req, baseStart, sourceFile, candidateFile, candidateProvenance, plannerRequestedFile, plannerSettings, plannerSettingsErr, attemptedKeys)
}

// evaluatePreparedReplanCandidateV3 is evaluateReplanCandidateV3 with the
// candidate file already resolved. It exists so a subtitle degrade can re-plan
// the session's own file in place: re-resolving the same virtual candidate
// through the provider could return a different release, which is exactly what
// a subtitle problem must never cause.
func (h *PlaybackHandler) evaluatePreparedReplanCandidateV3(
	r *http.Request,
	session *playback.Session,
	record *playback.AttemptRecordV3,
	req playback.ReplanRequestV3,
	baseStart playback.StartRequestV3,
	sourceFile *models.MediaFile,
	candidateFile *models.MediaFile,
	candidateProvenance ProbeProvenance,
	plannerRequestedFile *models.MediaFile,
	plannerSettings playback.PlannerSettingsV3,
	plannerSettingsErr error,
	attemptedKeys []string,
) (*candidateEvaluationV3, *candidateErrorV3) {
	candidateStart := baseStart
	// A carried selection was minted against the plan-time inventory. Use the
	// session's captured evidence as the remap source for a virtual session:
	// the loaded candidate row may have been re-probed in place (same id,
	// different release), in which case source==target by id but the ordinal
	// still names the previous release's track.
	remapSource := h.replanRemapSourceV3(sourceFile, session)
	if err := remapAudioSelectionV3(remapSource, candidateFile, &candidateStart); err != nil {
		return nil, &candidateErrorV3{Stage: candidateStageAudioRemap, Message: fmt.Sprintf("candidate %d audio remap failed", candidateFile.ID), Err: err}
	}
	candidateAudioIndex, _, err := resolveV3AudioIndex(candidateFile, candidateStart.AudioTrackID, candidateStart.AudioTrackIndex)
	if err != nil {
		return nil, &candidateErrorV3{Stage: candidateStageAudioRemap, Message: fmt.Sprintf("candidate %d audio track resolve failed", candidateFile.ID), Err: err}
	}
	var candidateResult playback.PlannerResultV3
	var candidateToneMapErr error
	// A carried selection already naming this candidate was minted against
	// this file's inventory: remap from the candidate itself rather than
	// from the nominal source, whose inventory may describe a different
	// edition (e.g. a speculative detour that failed). Restricted to
	// non-virtual files, where a stable row id means a stable release;
	// virtual rows rotate releases under a fixed id, so they keep the
	// evidence-anchored remap above.
	subtitleRemapSource := remapSource
	if selFileID, selKind, _, selOK := playback.ParseTrackIDV3(candidateStart.SubtitleTrackID); selOK && selKind == subtitleTrackKindV3 && selFileID == candidateFile.ID && !isVirtualPlaybackFile(candidateFile) {
		subtitleRemapSource = candidateFile
	}
	if _, err := h.remapSubtitleSelectionV3(r.Context(), subtitleRemapSource, candidateFile, &candidateStart); err != nil {
		candidateResult = playback.PlannerResultV3{Terminal: &playback.TerminalV3{
			Reason:    terminalSubtitleUnavailableInVersionV3,
			Message:   "The selected subtitle track is unavailable in the fallback media version.",
			Retryable: false,
		}}
		return &candidateEvaluationV3{
			file:       candidateFile,
			start:      candidateStart,
			result:     candidateResult,
			toneMapErr: candidateToneMapErr,
		}, &candidateErrorV3{Stage: candidateStageSubtitleRemap, TerminalReason: terminalSubtitleUnavailableInVersionV3, Message: fmt.Sprintf("candidate %d subtitle remap failed", candidateFile.ID), Err: err}
	} else {
		candidateStart.FileID = candidateFile.ID
		if _, err := candidateStart.NormalizeAndValidate(); err != nil {
			return nil, &candidateErrorV3{Stage: candidateStageNormalize, Message: fmt.Sprintf("candidate %d normalize failed", candidateFile.ID), Err: err}
		}
		if err := preflightPlaybackFile(r.Context(), candidateFile, h.MissingMarker, h.EventsHub); err != nil {
			return nil, &candidateErrorV3{Stage: candidateStagePreflight, Message: fmt.Sprintf("candidate %d preflight failed", candidateFile.ID), Err: err}
		}
		candidateResult, candidateToneMapErr = h.planPlaybackWithCapabilitiesV3(r.Context(), playback.PlannerInputV3{
			Request: candidateStart, RequestedFile: plannerRequestedFile, EffectiveFile: candidateFile,
			AudioTrackIndex: candidateAudioIndex, Settings: plannerSettings, Registry: h.transformationRegistryV3(r.Context()),
			DVRPUStrippable: h.lazyDVRPUStrippableV3(r.Context(), candidateFile), Now: time.Now(),
			AttemptedKeys: attemptedKeys, AdditionalSubtitles: h.downloadedSubtitleInventoryV3(r.Context(), candidateFile),
			InventoryProvenance: string(candidateProvenance),
		})
	}
	clampPlannerTargetResolution(&candidateResult, candidateFile)
	candidateResult = retryIncompleteToneMapPlanningV3(candidateResult, candidateToneMapErr)
	candidateResult = retryIncompletePlaybackSettingsV3(candidateResult, plannerSettingsErr)
	if candidateResult.Terminal != nil {
		return &candidateEvaluationV3{
			file:       candidateFile,
			start:      candidateStart,
			result:     candidateResult,
			toneMapErr: candidateToneMapErr,
		}, &candidateErrorV3{Stage: candidateStagePlan, TerminalReason: candidateResult.Terminal.Reason, Message: fmt.Sprintf("candidate %d planner terminal %s: %s", candidateFile.ID, candidateResult.Terminal.Reason, candidateResult.Terminal.Message)}
	}

	candidateResult.Plan.SessionID = session.ID
	var candidateReservationHeld bool
	if checker, ok := h.sessionMgr.(replacementAdmissionCheckerV3); ok {
		if err := checker.CheckReplacementAllowed(r.Context(), session.ID, candidateResult.PlayMethod, candidateResult.TranscodeAudio); err != nil {
			return nil, &candidateErrorV3{Stage: candidateStageAdmission, Message: fmt.Sprintf("candidate %d replacement admission denied", candidateFile.ID), Err: err}
		}
		_, candidateReservationHeld = h.sessionMgr.(replacementReservationCancellerV3)
	}
	cancelCandidateReservation := func() {
		if candidateReservationHeld {
			if canceller, ok := h.sessionMgr.(replacementReservationCancellerV3); ok {
				canceller.CancelReplacementReservation(session.ID)
			}
			candidateReservationHeld = false
		}
	}

	if checker, ok := h.sessionMgr.(transcodePermissionChecker); ok && (candidateResult.PlayMethod == playback.PlayTranscode || candidateResult.TranscodeAudio) {
		if err := checker.CheckTranscodingAllowed(r.Context(), session.UserID, candidateResult.PlayMethod == playback.PlayTranscode); err != nil {
			cancelCandidateReservation()
			return nil, &candidateErrorV3{Stage: candidateStageTranscodePerm, Message: fmt.Sprintf("candidate %d transcoding disallowed", candidateFile.ID), Err: err}
		}
	}

	candTransport, transportErr := h.prepareTransportV3(r, session, candidateFile, candidateResult, headerAuthenticatedMediaV3(req.ClientFeatures))
	if transportErr != nil {
		cancelCandidateReservation()
		errMsg := transportErr.message
		if transportErr.cause != nil {
			errMsg = fmt.Sprintf("%s (%v)", transportErr.message, logredact.SanitizeURLError(transportErr.cause))
		}
		return nil, &candidateErrorV3{Stage: candidateStageTransport, Reason: transportErr.reason, Message: fmt.Sprintf("candidate %d transport preparation failed (%s): %s", candidateFile.ID, transportErr.reason, errMsg), TransportErr: transportErr, Err: transportErr.cause}
	}

	applyTransportToneMapModeV3(&candidateResult, candTransport)
	candidateFrozenRecipe, frozenErr := h.freezeExecutableRecipeV3(r.Context(), candidateFile, candidateResult)
	if frozenErr != nil {
		candTransport.rollback()
		cancelCandidateReservation()
		return nil, &candidateErrorV3{Stage: candidateStageRecipe, Message: fmt.Sprintf("candidate %d freeze recipe failed", candidateFile.ID), Err: frozenErr}
	}

	candidateResult.Plan.Stream.URL = candTransport.url
	if err := h.attachSubtitleArtifactV3(r.Context(), session.ID, candidateFile, candidateResult.Plan, candidateResult.SubtitleTrackIndex, &candidateFrozenRecipe, req.ClientFeatures); err != nil {
		candTransport.rollback()
		cancelCandidateReservation()
		return nil, &candidateErrorV3{Stage: candidateStageSubtitleArtifact, Message: fmt.Sprintf("candidate %d attach subtitle artifact failed", candidateFile.ID), Err: err}
	}

	return &candidateEvaluationV3{
		start:           candidateStart,
		file:            candidateFile,
		result:          candidateResult,
		frozenRecipe:    candidateFrozenRecipe,
		transport:       candTransport,
		reservationHeld: candidateReservationHeld,
		toneMapErr:      candidateToneMapErr,
	}, nil
}

// evaluateSubtitleDegradedCandidateV3 re-evaluates a candidate alternate with
// the subtitle selection cleared, so a version that has no equivalent track
// still yields a playable subtitles-off plan. Callers run it only after every
// candidate that can honor the selection has been tried: an explicit user pick
// keeps its hunt for a matching version, while a stale selection degrades
// instead of terminalling playback.
func (h *PlaybackHandler) evaluateSubtitleDegradedCandidateV3(
	r *http.Request,
	session *playback.Session,
	record *playback.AttemptRecordV3,
	req playback.ReplanRequestV3,
	baseStart playback.StartRequestV3,
	sourceFile *models.MediaFile,
	candidateAlternate *models.MediaFile,
	plannerRequestedFile *models.MediaFile,
	plannerSettings playback.PlannerSettingsV3,
	plannerSettingsErr error,
	attemptedKeys []string,
) (*candidateEvaluationV3, *candidateErrorV3) {
	degradedStart := subtitleDegradedStartV3(baseStart)
	eval, evalErr := h.evaluateReplanCandidateV3(r, session, record, req, degradedStart, sourceFile, candidateAlternate, plannerRequestedFile, plannerSettings, plannerSettingsErr, attemptedKeys)
	if eval != nil {
		annotateSubtitleDroppedV3(&eval.result)
	}
	return eval, evalErr
}

// evaluateSubtitleDegradeInPlaceV3 re-plans the session's own file with the
// subtitle selection cleared, without re-resolving the provider candidate. It
// is the same-release counterpart of evaluateSubtitleDegradedCandidateV3: a
// subtitle-only refusal keeps the playing release and drops the subtitle,
// because the viewer picked the release and a subtitle must not move them off
// it. The candidate file is used verbatim, so a virtual release is never
// re-listed or substituted mid-degrade.
func (h *PlaybackHandler) evaluateSubtitleDegradeInPlaceV3(
	r *http.Request,
	session *playback.Session,
	record *playback.AttemptRecordV3,
	req playback.ReplanRequestV3,
	baseStart playback.StartRequestV3,
	file *models.MediaFile,
	provenance ProbeProvenance,
	plannerRequestedFile *models.MediaFile,
	plannerSettings playback.PlannerSettingsV3,
	plannerSettingsErr error,
	attemptedKeys []string,
) (*candidateEvaluationV3, *candidateErrorV3) {
	if file == nil {
		return nil, &candidateErrorV3{Stage: candidateStageResolve, Message: "nil same-release subtitle degrade target"}
	}
	degradedStart := subtitleDegradedStartV3(baseStart)
	eval, evalErr := h.evaluatePreparedReplanCandidateV3(r, session, record, req, degradedStart, file, file, provenance, plannerRequestedFile, plannerSettings, plannerSettingsErr, attemptedKeys)
	if eval != nil {
		annotateSubtitleDroppedV3(&eval.result)
	}
	return eval, evalErr
}

func subtitleDegradedStartV3(baseStart playback.StartRequestV3) playback.StartRequestV3 {
	degradedStart := baseStart
	degradedStart.SubtitleTrackIndex = nil
	degradedStart.SubtitleTrackID = ""
	return degradedStart
}

// annotateSubtitleDroppedV3 states plainly on a successful degraded plan that
// the subtitle was dropped. The plan keeps the same effective file and
// delivery; only the subtitle selection is gone, and the warning is how the
// client learns that rather than inferring a route failure.
func annotateSubtitleDroppedV3(result *playback.PlannerResultV3) {
	if result == nil || result.Terminal != nil || result.Plan == nil {
		return
	}
	result.Plan.DegradationWarnings = append(result.Plan.DegradationWarnings, playback.DegradationWarningV3{
		Code:    subtitleDroppedUnavailableCodeV3,
		Message: "The selected subtitle is unavailable on this release; playback continues without it on the same release.",
	})
}

// degradeStartSubtitleInPlaceV3 re-plans a fresh start's effective file with the
// subtitle selection cleared, keeping the release. It is the start-path
// counterpart of evaluateSubtitleDegradeInPlaceV3: a subtitle-only refusal must
// not send the viewer hunting for another version. The effective file is planned
// verbatim, so a virtual release is never re-resolved or substituted. It reports
// false when the same release still has no playable plan, leaving the caller's
// original subtitle terminal in place.
func (h *PlaybackHandler) degradeStartSubtitleInPlaceV3(
	r *http.Request,
	req playback.StartRequestV3,
	requestedFile, effectiveFile *models.MediaFile,
	audioIndex int,
	settings playback.PlannerSettingsV3,
	provenance ProbeProvenance,
) (playback.StartRequestV3, playback.PlannerResultV3, error, bool) {
	degradedReq := subtitleDegradedStartV3(req)
	degradedResult, degradedToneMapErr := h.planPlaybackWithCapabilitiesV3(r.Context(), playback.PlannerInputV3{
		Request: degradedReq, RequestedFile: requestedFile, EffectiveFile: effectiveFile,
		AudioTrackIndex: audioIndex, Settings: settings,
		Registry:            h.transformationRegistryV3(r.Context()),
		DVRPUStrippable:     h.lazyDVRPUStrippableV3(r.Context(), effectiveFile),
		Now:                 time.Now(),
		AdditionalSubtitles: h.downloadedSubtitleInventoryV3(r.Context(), effectiveFile),
		// The degraded plan serves the same release with the subtitle dropped,
		// so it keeps the selected source's provenance.
		InventoryProvenance: string(provenance),
	})
	clampPlannerTargetResolution(&degradedResult, effectiveFile)
	if degradedResult.Terminal != nil {
		return req, playback.PlannerResultV3{}, degradedToneMapErr, false
	}
	annotateSubtitleDroppedV3(&degradedResult)
	return degradedReq, degradedResult, degradedToneMapErr, true
}

// maxInPlaceAudioCandidatesV3 bounds how many alternative tracks a same-release
// audio resolution will plan. Planning can resolve a registry and probe, and a
// file with many commentary tracks must not turn one audio failure into an
// unbounded planning loop.
const maxInPlaceAudioCandidatesV3 = 4

// annotateAudioTrackSubstitutedV3 states plainly on a successful degraded plan
// that the failing audio track was swapped for another track in the same file.
// The plan keeps the same effective file and delivery; the warning is how the
// client learns the audio selection changed rather than inferring a route
// failure. The substituted track is also visible in the plan's selected tracks.
func annotateAudioTrackSubstitutedV3(result *playback.PlannerResultV3, index int) {
	if result == nil || result.Terminal != nil || result.Plan == nil {
		return
	}
	result.Plan.DegradationWarnings = append(result.Plan.DegradationWarnings, playback.DegradationWarningV3{
		Code:    "audio_track_substituted",
		Message: fmt.Sprintf("The selected audio track could not be played on this release; audio track %d in the same file is used instead.", index),
	})
}

// inPlaceAudioCandidateOrderV3 orders the file's alternative audio tracks for a
// same-release resolution: tracks the client can already render come first,
// because the planner only needs a copy for those, then the rest. Each index is
// planned at most once and the list is bounded by maxInPlaceAudioCandidatesV3.
func inPlaceAudioCandidateOrderV3(req playback.StartRequestV3, file *models.MediaFile, current int) []int {
	if file == nil || len(file.AudioTracks) < 2 {
		return nil
	}
	playable := playback.AudioTrackPlayableFuncV3(req)
	order := make([]int, 0, len(file.AudioTracks))
	for index := range file.AudioTracks {
		if index != current && playable(file.AudioTracks[index]) {
			order = append(order, index)
		}
	}
	for index := range file.AudioTracks {
		if index != current && !playable(file.AudioTracks[index]) {
			order = append(order, index)
		}
	}
	if len(order) > maxInPlaceAudioCandidatesV3 {
		order = order[:maxInPlaceAudioCandidatesV3]
	}
	return order
}

// audioDegradedStartV3 rebinds a start request's audio selection to another
// track in the same file. Both the id and the index are set so the pair agrees
// at the request boundary.
func audioDegradedStartV3(baseStart playback.StartRequestV3, file *models.MediaFile, index int) playback.StartRequestV3 {
	degradedStart := baseStart
	indexCopy := index
	degradedStart.AudioTrackIndex = &indexCopy
	if file != nil {
		degradedStart.AudioTrackID = playback.TrackIDV3(file.ID, "audio", index)
	}
	return degradedStart
}

// degradeStartAudioInPlaceV3 re-plans a fresh start's effective file with a
// different audio track when the selected one cannot be adapted. It is the
// audio counterpart of degradeStartSubtitleInPlaceV3: an audio problem must be
// resolved on the release already mounted, never by rotating the video
// candidate. The effective file is planned verbatim, so a virtual release is
// never re-resolved or substituted. An explicit audio pick is left untouched: a
// viewer's track choice surfaces its failure instead of being silently
// overridden. It reports false when no alternative track yields a playable plan.
func (h *PlaybackHandler) degradeStartAudioInPlaceV3(
	r *http.Request,
	req playback.StartRequestV3,
	requestedFile, effectiveFile *models.MediaFile,
	currentAudioIndex int,
	settings playback.PlannerSettingsV3,
	provenance ProbeProvenance,
) (playback.StartRequestV3, int, playback.PlannerResultV3, error, bool) {
	if req.AudioTrackID != "" || req.AudioTrackIndex != nil {
		return req, currentAudioIndex, playback.PlannerResultV3{}, nil, false
	}
	for _, index := range inPlaceAudioCandidateOrderV3(req, effectiveFile, currentAudioIndex) {
		degradedReq := audioDegradedStartV3(req, effectiveFile, index)
		degradedResult, degradedToneMapErr := h.planPlaybackWithCapabilitiesV3(r.Context(), playback.PlannerInputV3{
			Request: degradedReq, RequestedFile: requestedFile, EffectiveFile: effectiveFile,
			AudioTrackIndex: index, Settings: settings,
			Registry:            h.transformationRegistryV3(r.Context()),
			DVRPUStrippable:     h.lazyDVRPUStrippableV3(r.Context(), effectiveFile),
			Now:                 time.Now(),
			AdditionalSubtitles: h.downloadedSubtitleInventoryV3(r.Context(), effectiveFile),
			// The degraded plan serves the same release with another audio
			// track, so it keeps the selected source's provenance.
			InventoryProvenance: string(provenance),
		})
		clampPlannerTargetResolution(&degradedResult, effectiveFile)
		if degradedResult.Terminal == nil {
			annotateAudioTrackSubstitutedV3(&degradedResult, index)
			return degradedReq, index, degradedResult, degradedToneMapErr, true
		}
	}
	return req, currentAudioIndex, playback.PlannerResultV3{}, nil, false
}

// evaluateAudioDegradeInPlaceV3 re-plans the session's own file with a different
// audio track, without re-resolving the provider candidate. It is the replan
// counterpart of degradeStartAudioInPlaceV3: an audio-only refusal keeps the
// playing release. The file is used verbatim, so a virtual release is never
// re-listed or substituted mid-degrade. An explicit audio pick in this replan
// (selected_tracks.audio present) is left untouched. It reports false when no
// alternative track yields a playable plan, leaving the caller's terminal in
// place.
func (h *PlaybackHandler) evaluateAudioDegradeInPlaceV3(
	r *http.Request,
	session *playback.Session,
	record *playback.AttemptRecordV3,
	req playback.ReplanRequestV3,
	baseStart playback.StartRequestV3,
	file *models.MediaFile,
	provenance ProbeProvenance,
	plannerRequestedFile *models.MediaFile,
	plannerSettings playback.PlannerSettingsV3,
	plannerSettingsErr error,
	attemptedKeys []string,
) (*candidateEvaluationV3, *candidateErrorV3, bool) {
	if file == nil {
		return nil, &candidateErrorV3{Stage: candidateStageResolve, Message: "nil same-release audio degrade target"}, false
	}
	if req.SelectedTracks.Audio != nil {
		return nil, &candidateErrorV3{Stage: candidateStageAudioRemap, Message: "explicit audio selection is not auto-substituted"}, false
	}
	current := 0
	if baseStart.AudioTrackIndex != nil {
		current = *baseStart.AudioTrackIndex
	}
	for _, index := range inPlaceAudioCandidateOrderV3(baseStart, file, current) {
		degradedStart := audioDegradedStartV3(baseStart, file, index)
		eval, evalErr := h.evaluatePreparedReplanCandidateV3(r, session, record, req, degradedStart, file, file, provenance, plannerRequestedFile, plannerSettings, plannerSettingsErr, attemptedKeys)
		if evalErr == nil && eval != nil && eval.result.Terminal == nil {
			annotateAudioTrackSubstitutedV3(&eval.result, index)
			return eval, nil, true
		}
	}
	return nil, &candidateErrorV3{Stage: candidateStageAudioRemap, Message: "no alternative audio track is playable on this release"}, false
}

// executeReplanV3 prepares an atomic replacement for a failed playback route.
func (h *PlaybackHandler) executeReplanV3(r *http.Request, record *playback.AttemptRecordV3, req playback.ReplanRequestV3) (playback.DecisionResponseV3, playback.AttemptRecordV3, *preparedTransportV3, *transportErrorV3) {
	r = r.WithContext(withPlaybackRoutingPolicySnapshotV3(r.Context(), h.playbackRoutingPolicyV3()))
	r = r.WithContext(withServerBitrateCapV3(r.Context(), record.ServerBitrateCapKbps))
	reservationHeld := false
	reservationHandedOff := false
	cancelReservation := func() {
		if reservationHeld {
			if canceller, ok := h.sessionMgr.(replacementReservationCancellerV3); ok {
				canceller.CancelReplacementReservation(record.SessionID)
			}
			reservationHeld = false
		}
	}
	defer func() {
		if !reservationHandedOff {
			cancelReservation()
		}
	}()
	start := record.NormalizedRequest
	operation := req.EffectiveOperation()
	seekReanchor := operation == playback.ReplanOperationSeekReanchorV3
	seekFailureRecovery := operation == playback.ReplanOperationSeekFailureRecoveryV3
	seekScopedRecovery := seekReanchor || seekFailureRecovery
	trackChange := operation == playback.ReplanOperationTrackChangeV3
	qualityChange := operation == playback.ReplanOperationQualityChangeV3
	outputChange := operation == playback.ReplanOperationOutputChangeV3
	// A failure recovery that abandoned a delivery for a transport reason
	// demotes that delivery on the durable request, so every later replan in
	// this attempt plans with it disabled. Without the stickiness a
	// track_change replan re-reads the client's still-advertised claims and
	// steers straight back to a route that just failed, thrashing the player
	// between direct play and remux (each flip reloading the stream and
	// orphaning the client's subtitle track state — the field symptom where a
	// menu shows a selection that is not actually playing).
	//
	// The demotion is a server-side evidence write on the durable attempt, not
	// a client-authority change: a fresh start of the same file still negotiates
	// normally, and an explicit user retry can re-enable the route by going
	// through a new start.
	// A decode failure defers the demotion while the next hop can still recover
	// the same server-transcode delivery: either an untried software-decode
	// variant of the same source, or a provider candidate rotation under the
	// same virtual release. Demoting first would strand the session on a route
	// shape that rotation needs. Every other transport failure demotes exactly
	// as before.
	//
	// virtualDecodeRotation is the rotation predicate for a non-explicit
	// virtual failure: candidate substitution is the recovery, so the delivery
	// stays eligible until rotation exhausts.
	virtualDecodeRotation := h.virtualCandidateRotationPendingV3(record, req)
	if failureRecoveryAbandonedDeliveryV3(operation, req.Failure.Classification) &&
		(!decodeFailureClassificationV3(req.Failure.Classification) ||
			(!h.softwareDecodeRetryPendingV3(record, req) && !virtualDecodeRotation)) {
		// Demote on both copies: the record (the durable attempt this replan
		// may still terminal-persist) and the seeded start, whose payload the
		// success commit writes back via updated.NormalizedRequest. Demoting
		// only the record would be wiped by that write-back — the seed was
		// copied before the demotion and the overlay re-enables the client's
		// still-advertised claim.
		demoteDeliveryCapabilityV3(&record.NormalizedRequest, record.CurrentPlan.Delivery)
		demoteDeliveryCapabilityV3(&start, record.CurrentPlan.Delivery)
	}
	// User-intent operations replace the legacy audio PATCH and client-recipe
	// transcode start. Nothing failed, so their previous route stays eligible:
	// neither attempted-key history nor the failed-plan exclusion applies.
	userIntentOperation := trackChange || qualityChange || outputChange
	// A proxy-origin source refusal means the media route may be sound while
	// the selected egress is not. Retry the same plan through the API origin
	// before excluding its plan key and considering another source or route.
	proxyOriginRecovery := !userIntentOperation &&
		req.Failure.Classification == "sourceRefused" &&
		strings.HasPrefix(record.CurrentPlan.Stream.URL, "http")
	intentChange := false
	if seekScopedRecovery {
		if err := validateSeekRecoveryRequestV3(record, req); err != nil {
			reason := "seek_reanchor_intent_mismatch"
			if seekFailureRecovery {
				reason = "seek_failure_recovery_intent_mismatch"
			}
			return playback.DecisionResponseV3{}, *record, nil, &transportErrorV3{
				reason:  reason,
				message: err.Error(),
			}
		}
		// Reconstruct the complete route intent from the durable current attempt.
		// A seek request is not an authority boundary for replacing capability or
		// device evidence: accepting those fields here could make the same file
		// select a materially different route based on request-only claims.
		start.FileID = record.EffectiveMediaFileID
		start.StartPosition = &req.PositionSeconds
		applySelectedTracksToStartV3(&start, record.CurrentPlan.SelectedTracks)
	} else {
		// Failure replans may omit unchanged tracks. The durable current plan
		// holds the authoritative effective-file selections; the normalized
		// request can still carry requested-edition identities after an
		// alternate-version fallback, and validating those against the
		// effective file would reject an otherwise valid replan. Seed from
		// the plan first, then overlay the request's explicit changes.
		applySelectedTracksToStartV3(&start, record.CurrentPlan.SelectedTracks)
		switch {
		case trackChange:
			intentChange = audioSelectionDiffersFromStartV3(req.SelectedTracks, start) ||
				subtitleSelectionDiffersFromStartV3(req.SelectedTracks, start)
		case qualityChange:
			nextQuality, _ := playback.NormalizeQualityV3(req.QualityPreference)
			intentChange = nextQuality != start.QualityPreference
		case outputChange:
			intentChange = true
		default:
			switch req.Failure.Classification {
			case "quality_changed":
				nextQuality, _ := playback.NormalizeQualityV3(req.QualityPreference)
				intentChange = nextQuality != start.QualityPreference
			case "audio_track_changed":
				intentChange = audioSelectionDiffersFromStartV3(req.SelectedTracks, start)
			case "subtitle_track_changed":
				intentChange = subtitleSelectionDiffersFromStartV3(req.SelectedTracks, start)
			case "output_route_changed":
				intentChange = req.ClientPlaybackContext.Output.OutputContextID != start.ClientPlaybackContext.Output.OutputContextID
			}
		}
		// Failure replans use the current effective file. Quality/output intent may
		// restart source selection from the requested edition, but a track change
		// is expressed in the mounted alternate's inventory and must stay pinned to
		// that file or its combined ordinals can select unrelated tracks.
		start.FileID = record.EffectiveMediaFileID
		if intentChange && !trackChange {
			start.FileID = record.RequestedMediaFileID
		}
		if strings.TrimSpace(req.QualityPreference) != "" {
			// Replans may omit unchanged intent. Normalizing an absent quality
			// would silently reset "original" or a fixed rung to "auto".
			start.QualityPreference = req.QualityPreference
		}
		start.StartPosition = &req.PositionSeconds
		start.Metered = req.Metered
		start.BandwidthEstimateKbps = copyOptionalIntV3(req.BandwidthEstimateKbps)
		start.BandwidthCapKbps = copyOptionalIntV3(req.BandwidthCapKbps)
		start.Capabilities = req.Capabilities
		start.ClientPlaybackContext = req.ClientPlaybackContext
		// The overlay above aliases the replan body's deliveries map, which
		// every shallow copy made since decode — the preflight request
		// included — also points at. Deep-clone it here so the single
		// normalization the merged start request runs cannot write through to
		// req, and a later preflight mutation cannot leak into the durable
		// request stored on the attempt.
		start.ClientPlaybackContext.Deliveries = playback.CloneDeliveryCapabilitiesV3(req.ClientPlaybackContext.Deliveries)
		// The client's capability payload just replaced the seeded one.
		// Re-apply the durable record's server-side delivery demotions so a
		// route a previous failure recovery abandoned stays disabled — the
		// client cannot advertise itself back into a route that failed.
		reapplyDeliveryDemotionsV3(&start, record.NormalizedRequest)
		if req.ClientFeatures != nil {
			// Feature advertisement is single-location (top-level); a replan
			// that sends it refreshes the durable request's copy alongside the
			// capability payloads. Omission keeps the start-time features.
			start.ClientFeatures = req.ClientFeatures
		}
		// A replan that answers a default-audio reconciliation withdrawal
		// carries the correction the settled decision recorded: the client
		// replans off the withdrawn plan, which still names the pre-reorder
		// stream, so without this the correction never reaches the transport.
		// This must run BEFORE the selection is applied onto start: audio
		// resolution reads start.AudioTrackID/AudioTrackIndex, so a correction
		// layered on afterwards would never reach the plan or the executor's
		// audio map.
		h.pendingAudioReconciliationReplan(record, &req)
		if trackChange {
			// A track_change is the only operation where an omitted subtitle
			// means "subtitles off". Failure, seek, and quality replans may omit
			// unchanged identities and must not erase the durable selection.
			applySelectedTracksToStartV3(&start, req.SelectedTracks)
		} else {
			applySelectedTrackOverridesToStartV3(&start, req.SelectedTracks)
		}
	}
	// Native selection is negotiated at start and can only be disabled during
	// an attempt. Keep a confirmed failure disabled even when a later client
	// replan resends its full capability/feature advertisement.
	if !playback.HasFeatureV3(record.NormalizedRequest.ClientFeatures, playback.FeatureEmbeddedSubtitlesV3) || req.Failure.Classification == "subtitle_embedded_failed" {
		start.ClientFeatures = slices.DeleteFunc(slices.Clone(start.ClientFeatures), func(feature string) bool {
			return strings.EqualFold(strings.TrimSpace(feature), playback.FeatureEmbeddedSubtitlesV3)
		})
	}
	requestedFallbackID := record.EffectiveMediaFileID
	effectiveFallbackID := record.RequestedMediaFileID
	if seekScopedRecovery {
		// Edition fallback is useful for ordinary failure replans, but never for
		// a seek operation: the caller asked to move within the currently mounted
		// source, not to select another version when that source disappears.
		requestedFallbackID = 0
		effectiveFallbackID = 0
	}
	requestedFile, err := h.loadFileByPreferredID(r.Context(), record.RequestedMediaFileID, requestedFallbackID)
	requestedEditionResolved := err == nil && requestedFile != nil && requestedFile.ID == record.RequestedMediaFileID
	if err != nil || requestedFile == nil {
		if !seekScopedRecovery {
			return playback.DecisionResponseV3{}, *record, nil, &transportErrorV3{reason: "source_unavailable", message: "The requested media source is unavailable."}
		}
		// The requested edition is identity-only once another effective edition
		// is mounted. Seeking must depend on that effective file remaining
		// available, not on an inactive original edition still resolving.
		requestedFile = &models.MediaFile{ID: record.RequestedMediaFileID}
	} else {
		// Mirror the start path's on-demand probe repair: a session outlives the
		// scan that produced its file row, and a replan re-reads that row from
		// the catalog. Without this, a replan refuses a route as
		// source_metadata_incomplete for metadata the start path would have
		// healed before planning.
		requestedFile = h.ensurePlaybackProbe(r.Context(), requestedFile)
	}
	plannerRequestedFile := requestedFile
	if requestedFile.ID != record.RequestedMediaFileID {
		// The live loader may fall back to the current effective file when the
		// original edition is gone. Keep that file for metadata/remapping while
		// preserving the durable requested-edition identity in every new plan.
		plannerRequestedFile = &models.MediaFile{ID: record.RequestedMediaFileID}
	}
	currentEffectiveFile, err := h.loadFileByPreferredID(r.Context(), record.EffectiveMediaFileID, effectiveFallbackID)
	if err != nil || currentEffectiveFile == nil {
		return playback.DecisionResponseV3{}, *record, nil, &transportErrorV3{reason: "source_unavailable", message: "The effective media source is unavailable."}
	}
	session, err := h.sessionMgr.GetSession(record.SessionID)
	if err != nil {
		return playback.DecisionResponseV3{}, *record, nil, &transportErrorV3{reason: "session_expired", message: "The playback session has expired.", retryable: true}
	}
	replacementManager, ok := h.sessionMgr.(replacementStateManagerV3)
	if !ok {
		return playback.DecisionResponseV3{}, *record, nil, &transportErrorV3{reason: "internal_error", message: "The live session manager does not support atomic replacement."}
	}
	// A transport cancel is evidence about one request, not a verdict on the
	// session: the viewer may have navigated off, or hls.js may have given up
	// and retried. It must never fence a live client's failure_recovery or
	// answer it as a missing session — at this layer a live recovery is
	// indistinguishable from a stale one, and running a superfluous recovery
	// costs far less than retiring a healthy session. Keep the mark
	// route-scoped so a predecessor's cancel cannot outlive its route, log it
	// for diagnostics, and drop a superseded one. A genuine stop stays fenced
	// by the session lookup above, which answers session_expired when the
	// session is gone or stopped.
	if failureRecoveryOperationV3(operation) &&
		sessionClientCanceledRecentlyV3(session.ClientCanceled.At, time.Now()) {
		currentRoute := session.ClientCancelNamesCurrentRoute()
		outcome := "zombie_recovery_candidate"
		if !currentRoute {
			outcome = "zombie_recovery_stale"
		}
		slog.InfoContext(r.Context(), "playback failure recovery ran after a canceled transport",
			logComponentKey, playbackLogValueV3,
			"outcome", outcome,
			"session_id", record.SessionID,
			"operation", string(operation),
			"failure_classification", req.Failure.Classification,
			"canceled_at", session.ClientCanceled.At.UTC().Format(time.RFC3339Nano),
			"canceled_revision", session.ClientCanceled.StreamRevision,
			"canceled_binding_generation", session.ClientCanceled.BindingGeneration,
		)
		if !currentRoute {
			// The mark names a route this session has already replaced. Drop it
			// so a predecessor's cancel cannot color a later read.
			_ = h.sessionMgr.ClearClientCanceled(record.SessionID)
		}
	}
	// An explicit virtual version pin is never substituted. A decode rejection
	// on it terminalls immediately with the version-list hint so the viewer can
	// pick another release; rotation is reserved for auto selections.
	if strings.TrimSpace(session.VirtualSourceURI) != "" &&
		operation == playback.ReplanOperationFailureRecoveryV3 &&
		decodeFailureClassificationV3(req.Failure.Classification) &&
		record.CurrentPlan.Delivery == playback.DeliveryTranscodeHLSV3 &&
		planHasVideoEncodeV3(record.CurrentPlan) &&
		record.NormalizedRequest.FileSelection == playback.FileSelectionExplicitV3 {
		terminal := &playback.TerminalV3{
			Reason:    sourceDecodeFailedReasonV3,
			Message:   "The selected media version could not be decoded.",
			Retryable: false,
		}
		hintExplicitSelectionAlternateAvailableV3(terminal, record.NormalizedRequest.FileSelection)
		return playback.NewTerminalResponseFromTerminalV3(terminal), *record, nil, nil
	}
	// Reactive software-decode recovery. A hardware decoder can reject a source
	// the planner believed it could decode (invalid-bitstream failures from the
	// first frame). The planner has no way to express a software-decode
	// retry, so when the live transcode session reports the decoder gave up,
	// force the next server-transcode plan onto the software decode path. This
	// stays client-driven: the retry happens inside the existing
	// failure_recovery replan, with no server->client command.
	forceSoftwareDecode := false
	decodeFailureSample := ""
	decodeFailureCount := 0
	decodeAttemptDetail := ""
	if operation == playback.ReplanOperationFailureRecoveryV3 &&
		record.CurrentPlan.Delivery == playback.DeliveryTranscodeHLSV3 &&
		!record.CurrentPlan.EffectiveRecipe.SoftwareVideoDecode &&
		planHasVideoEncodeV3(record.CurrentPlan) {
		executedHWAccel := strings.TrimSpace(session.TranscodeHWAccel)
		if executedHWAccel != "" && !strings.EqualFold(executedHWAccel, playback.HWAccelNone) &&
			!playback.RequiresSoftwareVideoDecode(record.CurrentPlan.Source.VideoCodec, record.CurrentPlan.Source.VideoProfile, record.CurrentPlan.Source.BitDepth) {
			if ts := h.tm.GetTranscodeSession(record.SessionID); ts != nil && ts.IsDecodeFailed() {
				// gpu_only forbids the reactive CPU-decode retry: the operator
				// asked for GPU playback, so a hardware decoder rejection is a
				// terminal verdict for this candidate, not a prompt to decode
				// on the CPU. The hardware plan key is already in the attempted
				// set, so the planner surfaces adaptation_exhausted (or the
				// virtual recovery loop re-grabs a different candidate with the
				// failed one excluded). allow keeps the existing retry.
				forceSoftwareDecode = h.softwareFallbackAllowedV3() && softwareDecodeVariantPendingV3(record, req)
				if forceSoftwareDecode {
					decodeFailureSample, decodeFailureCount = ts.DecodeFailureEvidence()
				}
			}
		}
	} else if operation == playback.ReplanOperationFailureRecoveryV3 &&
		record.CurrentPlan.Delivery == playback.DeliveryTranscodeHLSV3 &&
		record.CurrentPlan.EffectiveRecipe.SoftwareVideoDecode &&
		planHasVideoEncodeV3(record.CurrentPlan) {
		// The software variant was the plan that just failed, so both decoder
		// modes are exhausted. Name them on the terminal detail so it is not
		// empty when the planner returns adaptation_exhausted.
		decodeAttemptDetail = fmt.Sprintf("hardware and software video decode both attempted for %s (hw_accel=%s)", record.CurrentPlan.Delivery, strings.TrimSpace(session.TranscodeHWAccel))
	}
	// Rotation is candidate substitution, never a decode-mode change: an
	// explicit pin is not substituted, and for a virtual non-explicit decode
	// failure the recovery is another provider release under the same delivery,
	// not a forced CPU decode of the rejected one. The operator's gpu_only /
	// software-fallback policy therefore stays untouched for local sources.
	if virtualDecodeRotation {
		forceSoftwareDecode = false
	}
	virtualRehydrationFailed := false
	var virtualRehydrationErr error
	// replanVirtualProvenance is the resolver's provenance for the rehydrated
	// effective source, published additively on the replan's plan. Empty when
	// the source is not virtual or the rehydration did not resolve.
	replanVirtualProvenance := ProbeProvenance("")
	if isVirtualPlaybackFile(currentEffectiveFile) {
		if session.VirtualSourceURI == "" {
			slog.WarnContext(r.Context(), "virtual playback rehydration has no pinned source", "component", "api", "session_id", record.SessionID, "file_id", currentEffectiveFile.ID)
			virtualRehydrationFailed = true
		} else {
			pinnedFile := *currentEffectiveFile
			pinnedFile.FilePath = session.VirtualSourceURI
			pinnedFile.VirtualOwnerInstallationID = session.VirtualSourceOwnerInstallationID
			// When the catalog row already points at the exact session-bound
			// candidate and carries complete probed evidence, rehydrating it
			// again would force a synchronous provider probe (up to 15s) for
			// metadata the planner already has. Skip the round-trip entirely
			// and use the loaded row directly.
			// Widened reuse guard: a same-release identity (same provider result
			// id under the same provider-neutral path) counts as unchanged even
			// when the catalog row string differs, so a same-file re-plan reuses
			// the probed row and lets the attempted-key guard advance the
			// transformation rung instead of short-circuiting into a re-list.
			candidateUnchanged := currentEffectiveFile.FilePath == session.VirtualSourceURI ||
				(virtualResultCandidateID(currentEffectiveFile.FilePath) != "" &&
					virtualResultCandidateID(currentEffectiveFile.FilePath) == virtualResultCandidateID(session.VirtualSourceURI) &&
					virtualPlaybackNeutralKey(currentEffectiveFile.FilePath) == virtualPlaybackNeutralKey(session.VirtualSourceURI))
			evidenceComplete := completeVirtualVideoEvidenceV3(currentEffectiveFile) &&
				completeVirtualAudioEvidenceV3(currentEffectiveFile) &&
				completeVirtualContainerEvidenceV3(currentEffectiveFile)
			// A decode-classified failure on a non-explicit selection always
			// takes the exclusion path, even when the catalog row still names
			// the session-bound candidate and its evidence is complete: that
			// short-circuit would re-mount the exact release the decoder just
			// rejected. Rotation must happen on the provider result id.
			if candidateUnchanged && evidenceComplete && !virtualDecodeRotation {
				currentEffectiveFile.FilePath = session.VirtualSourceURI
				currentEffectiveFile.VirtualOwnerInstallationID = session.VirtualSourceOwnerInstallationID
			} else {
				// The session-bound release is preferred so re-ranking cannot
				// drift to a different provider candidate.
				preferredCandidateID := virtualResultCandidateID(session.VirtualSourceURI)
				var excludedCandidateIDs []string
				// Only a verdict that indicts the release may exclude the
				// session-bound candidate. `virtualDecodeRotation` is the
				// server-confirmed decode rejection; a dead or unavailable
				// provider surfaces as the pinned id being absent from the
				// provider list, which the resolver still substitutes. Every
				// other failure — display-driven fallback, route error, stale
				// credential — keeps the same file so the attempted-key guard
				// advances to the next transformation rung on it (for example
				// the server DV7->HDR10 strip) instead of swapping the release.
				failedID := virtualResultCandidateID(session.VirtualSourceURI)
				if failedID == "" {
					// The session holds no binding (e.g. a fresh start that
					// failed before committing one); fall back to the catalog
					// row the plan was built against.
					failedID = virtualResultCandidateID(currentEffectiveFile.FilePath)
				}
				providerSource := virtualAttemptProviderSourceV3(session.VirtualSourceURI)
				if providerSource == "" {
					providerSource = virtualAttemptProviderSourceV3(currentEffectiveFile.FilePath)
				}
				if failedID != "" && virtualDecodeRotation {
					// Persist the confirmed verdict on the attempt before
					// rotating. The durable chain is what makes a later replan
					// — on this replica or another after a reload — skip every
					// candidate already indicted, instead of relying on the
					// asynchronous failed_at marker landing in time. A failed
					// write must surface as a controlled error rather than let
					// the rotation publish an untracked candidate the next hop
					// could re-select.
					exclusion := playback.RecoveryExclusionV3{
						ProviderSource: providerSource,
						CandidateID:    failedID,
						ReleaseID:      virtualCandidateReleaseIdentityV3(currentEffectiveFile),
						FileID:         record.EffectiveMediaFileID,
						ConfirmedAt:    time.Now().UTC(),
					}
					chain, persistErr := h.appendRecoveryExclusionsV3(r.Context(), record, []playback.RecoveryExclusionV3{exclusion})
					if persistErr != nil {
						slog.ErrorContext(r.Context(), "virtual decode rejection exclusions could not be persisted", "component", "api", "session_id", record.SessionID, "candidate_id", failedID, "error", persistErr)
						return playback.DecisionResponseV3{}, *record, nil, &transportErrorV3{
							reason:    "recovery_state_unavailable",
							message:   "The server could not durably record the failed source candidate; retry the replay.",
							retryable: true,
							cause:     persistErr,
						}
					}
					record.RecoveryState = chain
					// Union every confirmed exclusion in this provider scope,
					// not just the candidate that just failed: a prior hop the
					// durable chain already recorded must stay excluded even
					// when its failed_at marker never landed or was cleared.
					excludedCandidateIDs = playback.RecoveryExcludedCandidateIDsV3(chain, providerSource)
					if !containsStringExactV3(excludedCandidateIDs, failedID) {
						excludedCandidateIDs = append(excludedCandidateIDs, failedID)
					}
					// A suspected-but-unconfirmed sibling is still eligible:
					// only confirmed verdicts enter the chain, so no extra
					// filtering is applied here.
				}
				// Only a confirmed rotation may re-select a candidate the
				// catalog marked failed (for example one this serve layer just
				// stamped after it produced no bytes). Every other replan must
				// honor the known-bad stamp so the resolver skips it and either
				// finds a live sibling or fails with a retryable terminal,
				// instead of looping back onto the dead pin.
				//
				// An absent/renumbered session anchor is its own rotation cause:
				// resolveRehydratedVirtualSourceV3 retries with rotation declared
				// when the resolver reports the pinned candidate is no longer
				// listed. Session-bound stays true; only the anchor rotates.
				// Rotation honors active failed_at stamps: allowFailed stays
				// false so a sibling the marker already stamped (a prior hop
				// in this chain) is skipped rather than re-mounted. The
				// durable chain travels explicitly in excludedCandidateIDs,
				// so nothing needs the bypass; bypassing stamps here is what
				// would let sequential rotations cycle A→B→C→A instead of
				// terminating when every sibling is known-bad.
				//
				// One rotation policy for the session. The retry's same-release
				// assertion is the single arbiter of what an explicit pin may
				// accept: a renumbered or dropped anchor is re-identified by its
				// durable provider identity and allowed through (the same
				// recovery the transport anchor and serve-layer doors allow),
				// while a genuinely different release, or one with no durable
				// identity to prove the match, is refused and keeps the original
				// absent/marked-failed cause. An explicit pick is therefore never
				// substituted with a different release on any door, and an auto
				// selection keeps its documented renumbered/dead-pin recovery.
				// The terminal-driven alternate-file hunt below is separately
				// gated by the negotiated fallback policy, so a failed
				// rehydration never silently swaps in a sibling version.
				resolved, resolveErr := h.resolveRehydratedVirtualSourceV3(r, &pinnedFile, record.ProfileID, excludedCandidateIDs, preferredCandidateID, start.QualityPreference, intOrZeroHandlerV3(start.BandwidthCapKbps), virtualResolveOptionsV3{allowFailedCandidate: false, rotateCandidates: virtualDecodeRotation, sessionBound: true, sessionAnchorURI: session.VirtualSourceURI, bypassProviderFloor: true})
				if resolveErr != nil {
					slog.WarnContext(r.Context(), "virtual playback rehydration failed", "component", "api", "session_id", record.SessionID, "file_id", currentEffectiveFile.ID, "owner_installation_id", session.VirtualSourceOwnerInstallationID, "error", logredact.SanitizeURLError(resolveErr))
					virtualRehydrationFailed = true
					virtualRehydrationErr = resolveErr
				} else if resolved.File == nil {
					slog.WarnContext(r.Context(), "virtual playback rehydration returned no file", "component", "api", "session_id", record.SessionID, "file_id", currentEffectiveFile.ID, "owner_installation_id", session.VirtualSourceOwnerInstallationID)
					virtualRehydrationFailed = true
				} else {
					resolvedFile := *resolved.File
					// The effective file is the candidate the resolver actually
					// served; keep its own catalog row id (the substituted
					// candidate) instead of overwriting it with the record's
					// previous effective id, so the replan plan can name the
					// bytes it will play. The plan's requested id stays the
					// record's requested row.
					resolvedFile.FilePath = resolved.URI
					resolvedFile.VirtualOwnerInstallationID = resolved.OwnerID
					currentEffectiveFile = &resolvedFile
					replanVirtualProvenance = resolved.Provenance
				}
			}
		}
	} else {
		currentEffectiveFile = h.ensurePlaybackProbe(r.Context(), currentEffectiveFile)
	}
	// Freeze the session's first-verified Dolby Vision profile onto the freshly
	// read effective file before planning. A replan re-reads the catalog row and
	// can see a drifted dv_profile on bytes that never changed; the pin keeps
	// route selection stable for the session without touching catalog state.
	applyDVPinToEffectiveFileV3(session, currentEffectiveFile)
	if virtualRehydrationFailed && operation != playback.ReplanOperationFailureRecoveryV3 {
		return playback.DecisionResponseV3{}, *record, nil, &transportErrorV3{
			reason: "virtual_source_unavailable", message: "The virtual source could not be refreshed for playback.", retryable: true, cause: virtualRehydrationErr,
		}
	}
	failedEffectiveFile := currentEffectiveFile
	if virtualRehydrationFailed && errors.Is(virtualRehydrationErr, context.DeadlineExceeded) {
		return playback.DecisionResponseV3{}, *record, nil, &transportErrorV3{
			reason: "replan_virtual_input_timeout", message: "Virtual stream input resolution timed out during replan.", retryable: true, cause: virtualRehydrationErr,
		}
	}
	// A canceled virtual re-resolve is not a verdict about the release: the
	// provider call was interrupted (most often the client or the replan
	// deadline gave up). Continuing into the alternate-version hunt would fold
	// that cancellation into a candidate-exhaustion verdict and can surface as
	// a 500 after a later route-capacity decision. Stop here with the same
	// retryable timeout reason so the client retries the whole replan instead
	// of being told its plan is broken. The check also catches a request whose
	// context is already done even when the inner error is not wrapped.
	if virtualRehydrationFailed &&
		(errors.Is(virtualRehydrationErr, context.Canceled) || r.Context().Err() != nil) {
		return playback.DecisionResponseV3{}, *record, nil, &transportErrorV3{
			reason: "replan_virtual_input_timeout", message: "Virtual stream input resolution was interrupted during replan.", retryable: true, cause: virtualRehydrationErr,
		}
	}

	var result playback.PlannerResultV3
	var toneMapCapabilityErr error
	var plannerSettings playback.PlannerSettingsV3
	var plannerSettingsErr error
	// audioIndex, attemptedKeys and lowerVersion are function-scoped: the
	// terminal-driven planning below and the refused-remux escalation at the
	// end both read them, while the rehydration block in between assigns them.
	audioIndex := 0
	attemptedKeys := []string(nil)
	var lowerVersion *playback.LowerVersionV3
	var effectiveFile *models.MediaFile
	var preparedTransport *preparedTransportV3
	var artifactRecipe playback.ExecutableRecipeV3
	var mode mediaAuthModeV3
	// replanCapabilityWarnings holds the degradation warnings produced by the
	// one normalization of the merged start request, so they can be surfaced on
	// whichever plan this replan ultimately returns.
	var replanCapabilityWarnings []playback.DegradationWarningV3
	transportPrepared := false
	// One version-fallback policy for the whole replan. Both alternate-version
	// hunts — the rehydration-failure recovery below and the terminal-driven one
	// further down — read this single value, so an explicit version pick is never
	// substituted on either door and a viewer's mid-session re-arm of Auto is
	// honored consistently. resolveReplanAutoFallbackV3 prefers the live session
	// flag over the durable start request, so a reconstructed session keeps the
	// negotiated intent. Without this gate the rehydration path silently swapped
	// an explicit pin's release whenever its rehydration failed but a sibling
	// edition was still available.
	replanFallbackAllowed := resolveReplanAutoFallbackV3(h.sessionMgr, session.ID, record) &&
		(replanAllowsAlternateFileV3(operation, start.QualityPreference) ||
			(isVirtualPlaybackFile(requestedFile) && operation == playback.ReplanOperationFailureRecoveryV3))

	// detourDroppedSubtitle remembers a selection a speculative detour
	// through the requested edition could not honor; validation would then
	// drop anything still naming another file. A plan that plays on without
	// the viewer's subtitle reports it instead of going quiet.
	detourDroppedSubtitle := false
	// reportDetourSubtitleDrop appends the dropped-subtitle warning when
	// a speculative detour cost the viewer a selection no played version
	// honors. It runs on the final plan, so a fallback that keeps the
	// subtitle stays quiet while one that plays muted reports it.
	reportDetourSubtitleDrop := func() {
		if !detourDroppedSubtitle || result.Terminal != nil || result.Plan == nil || result.Plan.SelectedTracks.Subtitle != nil {
			return
		}
		for _, warning := range result.Plan.DegradationWarnings {
			if warning.Code == playback.DegradationWarningSubtitleTrackUnavailableV3 {
				return
			}
		}
		result.Plan.DegradationWarnings = append(result.Plan.DegradationWarnings, playback.SubtitleTrackUnavailableWarningV3())
	}

	if virtualRehydrationFailed {
		// Gate the alternate hunt on the same negotiated fallback policy the
		// non-rehydration path applies. An explicit pin whose release fails to
		// rehydrate must not be substituted: the resolver refused the sibling
		// above, and hunting for one here would contradict that refusal. When the
		// policy withholds fallback no alternate is tried, and the honest
		// refresh cause is returned through the exhaustion classifier below.
		var alternates []*models.MediaFile
		if replanFallbackAllowed {
			alternateOrder := alternateOrderingForClient(req.Capabilities)
			found, alternateErr := h.findAlternateFilesOrdered(r.Context(), requestedFile, requestAccessFilter(r), alternateOrder)
			if alternateErr != nil {
				virtualRehydrationErr = errors.Join(virtualRehydrationErr, alternateErr)
			}
			alternates = found
		}
		plannerSettings, plannerSettingsErr = h.plannerSettingsV3Result(r.Context())
		attemptedKeys := append([]string(nil), req.AttemptedPlanKeys...)
		if !containsStringExactV3(attemptedKeys, req.PlanAttemptKey) {
			attemptedKeys = append(attemptedKeys, req.PlanAttemptKey)
		}
		currentKey := playback.PlanAttemptKeyV3(record.CurrentPlan, record.NormalizedRequest.ClientPlaybackContext.Output.OutputContextID, req.LocalMutations)
		if !containsStringExactV3(attemptedKeys, currentKey) {
			attemptedKeys = append(attemptedKeys, currentKey)
		}

		var candidateErrs []*candidateErrorV3
		virtualRecoverySucceeded := false
		var subtitleMissAlternate *models.MediaFile
		for _, alternate := range alternates {
			if alternate.ID == failedEffectiveFile.ID {
				continue
			}
			eval, err := h.evaluateReplanCandidateV3(r, session, record, req, start, failedEffectiveFile, alternate, plannerRequestedFile, plannerSettings, plannerSettingsErr, attemptedKeys)
			if err != nil {
				candidateErrs = append(candidateErrs, err)
				if candidateSubtitleLocalFailureV3(err) && subtitleMissAlternate == nil {
					subtitleMissAlternate = alternate
				}
				slog.WarnContext(r.Context(), "virtual replan candidate alternate failed",
					"component", "api", "session_id", record.SessionID,
					"requested_file_id", record.RequestedMediaFileID, "failed_file_id", failedEffectiveFile.ID,
					"candidate_file_id", alternate.ID, "stage", string(err.Stage), "error", logredact.SanitizeURLError(err),
				)
			}
			if eval != nil && eval.result.Terminal == nil {
				start = eval.start
				effectiveFile = eval.file
				result = eval.result
				toneMapCapabilityErr = eval.toneMapErr
				artifactRecipe = eval.frozenRecipe
				preparedTransport = &eval.transport
				transportPrepared = true
				reservationHeld = eval.reservationHeld
				virtualRecoverySucceeded = true
				break
			}
		}
		if !virtualRecoverySucceeded && subtitleMissAlternate != nil {
			// No candidate had an equivalent track. Degrade the first such
			// candidate to subtitles-off rather than terminalling playback.
			if eval, evalErr := h.evaluateSubtitleDegradedCandidateV3(r, session, record, req, start, failedEffectiveFile, subtitleMissAlternate, plannerRequestedFile, plannerSettings, plannerSettingsErr, attemptedKeys); evalErr == nil && eval != nil && eval.result.Terminal == nil {
				start = eval.start
				effectiveFile = eval.file
				result = eval.result
				toneMapCapabilityErr = eval.toneMapErr
				artifactRecipe = eval.frozenRecipe
				preparedTransport = &eval.transport
				transportPrepared = true
				reservationHeld = eval.reservationHeld
				virtualRecoverySucceeded = true
			}
		}
		if !seekReanchor && plannerSettingsErr == nil {
			if requestedEditionResolved {
				lowerVersion = h.lowerVersionV3(r.Context(), start, requestedFile, plannerSettings, requestAccessFilter(r))
			}
		}
		if !virtualRecoverySucceeded {
			if virtualDecodeRotation {
				// The rejected release had no untried sibling (and no sibling
				// edition recovered it). Retire the delivery and terminal with
				// the decode reason so the viewer can pick another release.
				demoteDeliveryCapabilityV3(&record.NormalizedRequest, record.CurrentPlan.Delivery)
				demoteDeliveryCapabilityV3(&start, record.CurrentPlan.Delivery)
				terminal := &playback.TerminalV3{
					Reason:    sourceDecodeFailedReasonV3,
					Message:   sourceDecodeFailedMessageV3,
					Retryable: false,
				}
				hintExplicitSelectionAlternateAvailableV3(terminal, record.NormalizedRequest.FileSelection)
				return playback.NewTerminalResponseFromTerminalV3(terminal), *record, nil, nil
			}
			return playback.DecisionResponseV3{}, *record, nil, classifyVirtualReplanExhaustionV3(virtualRehydrationErr, candidateErrs)
		}
	} else {
		effectiveFile = currentEffectiveFile
		// A replan's carried selection was minted against the plan-time
		// inventory, which the session still holds as virtual evidence. When
		// the catalog row was re-probed in place (same id, different release)
		// the freshly loaded effective file's tracks no longer describe what
		// the ordinal names, so remap from the evidence rather than the row.
		remapSource := h.replanRemapSourceV3(currentEffectiveFile, session)
		currentEffectiveStart := start
		if intentChange && !trackChange {
			// Prefer returning to the requested edition, but a quality/output/track
			// change must not abandon a healthy active alternate merely because the
			// inactive original has gone missing since playback started. For virtual
			// sources, the requested file is a catalog placeholder without probed streams;
			// preserve the probed candidate stream.
			if !isVirtualPlaybackFile(currentEffectiveFile) && requestedEditionResolved && preflightPlaybackFile(r.Context(), requestedFile, h.MissingMarker, h.EventsHub) == nil {
				effectiveFile = requestedFile
			}
			// Track identities only need remapping when the effective inventory
			// actually changes. Remapping within an unchanged inventory would
			// degrade an exact selection to a best-match lookup — e.g. moving a
			// listener from an eng/ac3 commentary track to the identically-shaped
			// main track on a quality change. A same-row candidate rotation keeps
			// the id but replaces the tracks, so the guard is the union of an id
			// change (a real edition switch, which always remaps), an inventory
			// fingerprint change under a fixed id, and a recorded candidate
			// rotation (a moved revision) whose tracks happen to compare equal.
			if currentEffectiveFile.ID != effectiveFile.ID ||
				!sameMediaInventoryV3(remapSource, effectiveFile) ||
				virtualCandidateRotationRecordedV3(session, currentEffectiveFile) {
				candidateStart := start
				remapErr := remapAudioSelectionV3(remapSource, effectiveFile, &candidateStart)
				if remapErr == nil && (candidateStart.SubtitleTrackIndex != nil || candidateStart.SubtitleTrackID != "") {
					_, remapErr = h.remapSubtitleSelectionV3(r.Context(), remapSource, effectiveFile, &candidateStart)
				}
				if remapErr != nil && outputChange {
					// An output refresh may make the requested edition viable again,
					// but it must not retire a healthy active alternate merely because
					// the viewer selected a track unique to that alternate.
					effectiveFile = currentEffectiveFile
				} else if errors.Is(remapErr, errSubtitleUnavailableInTargetV3) {
					// The target edition has no equivalent track. Playability wins:
					// plan it with subtitles off instead of terminalling. The
					// cleared selection is scoped to this speculative detour:
					// start keeps the carried selection (with the remapped
					// audio) so a failed detour falls back with the viewer's
					// pick intact instead of losing it to a version that was
					// never going to play; the detour plan itself degrades the
					// stale selection gracefully through the subtitle policy.
					// Remember the drop for the warning below.
					detourDroppedSubtitle = true
					candidateStart.SubtitleTrackIndex = start.SubtitleTrackIndex
					candidateStart.SubtitleTrackID = start.SubtitleTrackID
					start = candidateStart
				} else if errors.Is(remapErr, errSubtitleStoreUnavailableV3) {
					// A failed lookup says nothing about whether the selected track
					// exists on the target edition, so it is not a track miss. The
					// playing plan keeps its subtitle; the change can be repeated
					// once the downloaded subtitles can be read.
					return playback.DecisionResponseV3{}, *record, nil, subtitleArtifactErrorV3("Downloaded subtitles are temporarily unavailable.", remapErr)
				} else if remapErr != nil {
					return playback.DecisionResponseV3{}, *record, nil, &transportErrorV3{reason: trackUnavailableReasonV3, message: remapErr.Error()}
				} else {
					start = candidateStart
				}
			}
		}
		start.FileID = effectiveFile.ID
		if err := preflightPlaybackFile(r.Context(), effectiveFile, h.MissingMarker, h.EventsHub); err != nil {
			return playback.DecisionResponseV3{}, *record, nil, &transportErrorV3{
				reason:  "source_unavailable",
				message: "The effective media source is unavailable.",
				cause:   err,
			}
		}
		seekDuration := float64(effectiveFile.Duration)
		if seekReanchor && record.FrozenRecipe.ValidFor(record.CurrentPlan) {
			seekDuration = record.FrozenRecipe.SourceDurationSeconds
		}
		if seekScopedRecovery && seekDuration > 0 && req.PositionSeconds > seekDuration {
			return playback.DecisionResponseV3{}, *record, nil, &transportErrorV3{
				reason:  "invalid_seek_position",
				message: "The requested seek position is beyond the end of the selected media source.",
			}
		}
		warnings, err := start.NormalizeAndValidate()
		if err != nil {
			return playback.DecisionResponseV3{}, *record, nil, &transportErrorV3{reason: "invalid_replan", message: err.Error()}
		}
		// This is the single normalization of the merged start request. Keep
		// its degradation warnings, including the drop of a client
		// transformation the merged feature list did not negotiate, and attach
		// them to the plan once it is selected.
		replanCapabilityWarnings = warnings
		audioIndex = 0
		if !seekReanchor {
			if dropStaleAudioTrackIdentityV3(r.Context(), effectiveFile, start.AudioTrackID) {
				start.AudioTrackID = ""
				start.AudioTrackIndex = nil
			}
			audioIndex, _, err = resolveV3AudioIndex(effectiveFile, start.AudioTrackID, start.AudioTrackIndex)
			if err != nil {
				return playback.DecisionResponseV3{}, *record, nil, &transportErrorV3{reason: trackUnavailableReasonV3, message: err.Error()}
			}
		}
		attemptedKeys = []string(nil)
		if !intentChange && !seekReanchor && !userIntentOperation {
			attemptedKeys = append(attemptedKeys, req.AttemptedPlanKeys...)
			if !containsStringExactV3(attemptedKeys, req.PlanAttemptKey) {
				attemptedKeys = append(attemptedKeys, req.PlanAttemptKey)
			}
		}
		if !seekReanchor && !userIntentOperation && (!intentChange || seekFailureRecovery) {
			// Always exclude the durable server recipe so stale or malformed client
			// history cannot immediately re-select the route that just failed and
			// ping-pong the session. A client-reported local mutation (for example a
			// PCM recovery route) is folded into the failed plan's key here — the
			// server owns the hash; clients only echo opaque keys.
			currentKey := playback.PlanAttemptKeyV3(record.CurrentPlan, record.NormalizedRequest.ClientPlaybackContext.Output.OutputContextID, req.LocalMutations)
			if !containsStringExactV3(attemptedKeys, currentKey) {
				attemptedKeys = append(attemptedKeys, currentKey)
			}
			if len(req.LocalMutations) > 0 {
				// The unmutated recipe already failed before the client mutated it
				// locally; exclude it as well.
				unmutatedKey := playback.PlanAttemptKeyV3(record.CurrentPlan, record.NormalizedRequest.ClientPlaybackContext.Output.OutputContextID, nil)
				if !containsStringExactV3(attemptedKeys, unmutatedKey) {
					attemptedKeys = append(attemptedKeys, unmutatedKey)
				}
			}
		}
		if virtualDecodeRotation {
			// The plan attempt key does not encode the provider result= URI, so
			// the decode-rejected candidate and its rotated sibling share a key.
			// Excluding the failed key would make the planner report
			// adaptation_exhausted for the sibling before it is ever tried.
			// Drop the failed candidate's key (and the client-echoed copies)
			// from the loop guard for this rotation only; plan identity itself is
			// unchanged, and distinct media-version keys stay excluded.
			failedKey := playback.PlanAttemptKeyV3(record.CurrentPlan, record.NormalizedRequest.ClientPlaybackContext.Output.OutputContextID, nil)
			mutatedKey := playback.PlanAttemptKeyV3(record.CurrentPlan, record.NormalizedRequest.ClientPlaybackContext.Output.OutputContextID, req.LocalMutations)
			attemptedKeys = slices.DeleteFunc(attemptedKeys, func(key string) bool {
				trimmed := strings.TrimSpace(key)
				return trimmed == failedKey || trimmed == mutatedKey
			})
		}
		if !seekReanchor {
			plannerSettings, plannerSettingsErr = h.plannerSettingsV3Result(r.Context())
		}
		if seekReanchor {
			if err := h.validateFrozenSubtitleIdentityV3(r.Context(), effectiveFile, record.FrozenRecipe); err != nil {
				return playback.DecisionResponseV3{}, *record, nil, subtitleArtifactErrorV3("The selected subtitle is no longer available at its frozen route.", err)
			}
			var frozenErr error
			result, frozenErr = frozenSeekReanchorResultV3(record, req.PositionSeconds, time.Now())
			if frozenErr != nil {
				return playback.DecisionResponseV3{}, *record, nil, &transportErrorV3{
					reason:    "seek_reanchor_recipe_unavailable",
					message:   "The active playback recipe cannot be reopened; start a new playback attempt.",
					retryable: true,
				}
			}
		} else {
			additional, inventoryErr := h.listDownloadedSubtitlesV3(r.Context(), effectiveFile)
			if inventoryErr != nil && (trackChange || qualityChange) && selectsDownloadedSubtitleV3(effectiveFile, start) {
				// A failed lookup says nothing about whether the selected track
				// exists. Planning without it would turn the subtitle off, and
				// later replans start from that plan's tracks, so it would stay
				// off for the rest of the session. The playing plan carries on
				// and the viewer can repeat the change.
				return playback.DecisionResponseV3{}, *record, nil, subtitleArtifactErrorV3("Downloaded subtitles are temporarily unavailable.", inventoryErr)
			}
			result, toneMapCapabilityErr = h.planPlaybackWithCapabilitiesV3(r.Context(), playback.PlannerInputV3{Request: start, RequestedFile: plannerRequestedFile, EffectiveFile: effectiveFile, ServerBitrateCapKbps: serverBitrateCapV3(r.Context()), AudioTrackIndex: audioIndex, Settings: plannerSettings, Registry: h.transformationRegistryV3(r.Context()), DVRPUStrippable: h.lazyDVRPUStrippableV3(r.Context(), effectiveFile), Now: time.Now(), AttemptedKeys: attemptedKeys, AdditionalSubtitles: additional, ForceSoftwareVideoDecode: forceSoftwareDecode, DecodeAttemptDetail: decodeAttemptDetail, StickyDelivery: replanStickyDeliveryV3(operation, record.CurrentPlan.Delivery), InventoryProvenance: string(replanVirtualProvenance)})
			clampPlannerTargetResolution(&result, effectiveFile)
			reportDetourSubtitleDrop()
		}
		if outputChange && result.Terminal != nil && effectiveFile.ID != currentEffectiveFile.ID {
			// Returning to the requested edition is speculative during an output
			// refresh. Any terminal from that probe must fall back to the edition
			// already playing, not only HDR/alternate-selection terminals: its audio,
			// subtitle, or delivery constraints may still differ from the active file.
			start = currentEffectiveStart
			start.FileID = currentEffectiveFile.ID
			effectiveFile = currentEffectiveFile
			if dropStaleAudioTrackIdentityV3(r.Context(), effectiveFile, start.AudioTrackID) {
				start.AudioTrackID = ""
				start.AudioTrackIndex = nil
			}
			audioIndex, _, err = resolveV3AudioIndex(effectiveFile, start.AudioTrackID, start.AudioTrackIndex)
			if err != nil {
				return playback.DecisionResponseV3{}, *record, nil, &transportErrorV3{reason: trackUnavailableReasonV3, message: err.Error()}
			}
			result, toneMapCapabilityErr = h.planPlaybackWithCapabilitiesV3(r.Context(), playback.PlannerInputV3{Request: start, RequestedFile: plannerRequestedFile, EffectiveFile: effectiveFile, ServerBitrateCapKbps: serverBitrateCapV3(r.Context()), AudioTrackIndex: audioIndex, Settings: plannerSettings, Registry: h.transformationRegistryV3(r.Context()), DVRPUStrippable: h.lazyDVRPUStrippableV3(r.Context(), effectiveFile), Now: time.Now(), AttemptedKeys: attemptedKeys, AdditionalSubtitles: h.downloadedSubtitleInventoryV3(r.Context(), effectiveFile), ForceSoftwareVideoDecode: forceSoftwareDecode, DecodeAttemptDetail: decodeAttemptDetail, StickyDelivery: replanStickyDeliveryV3(operation, record.CurrentPlan.Delivery)})
			clampPlannerTargetResolution(&result, effectiveFile)

		}
		// A subtitle-only refusal and a video/policy refusal take different
		// recovery routes. The in-place subtitle degrade is gated on the same
		// operation/quality/selection rules the alternate hunt uses, but not on
		// terminalAllowsAlternateFileV3: subtitle_conversion_unsupported is no
		// longer in that set precisely so a subtitle problem cannot reach the
		// sibling hunt.
		// replanFallbackAllowed (computed once above) is the session's negotiated
		// auto-fallback policy: an explicit start turns it off so a replan must
		// not silently substitute another version, and the viewer re-selecting
		// Auto turns it back on even for a session that started explicit. An
		// unset flag (a reconstruction) falls back to the durable normalized
		// request, which a mid-session re-arm updates, so the reconstructed
		// policy still matches the viewer's intent instead of the original start
		// request.
		subtitleOnlyAllowed := replanFallbackAllowed && subtitleOnlyTerminalV3(result.Terminal)
		// An audio-only refusal takes the same in-place route as a subtitle
		// one: keep the mounted release and re-plan it with another audio
		// track. audio_conversion_unsupported is not in
		// terminalAllowsAlternateFileV3, so an audio problem never reaches the
		// sibling hunt. On a replan the mounted release is known to be
		// playable, so a failed in-place resolution leaves the terminal in
		// place rather than substituting a sibling.
		audioOnlyAllowed := replanFallbackAllowed && audioOnlyTerminalV3(result.Terminal)
		alternateFileAllowed := replanFallbackAllowed && terminalAllowsAlternateFileV3(result.Terminal)
		// versionPickFallback remembers whether the hunt below runs for an
		// explicitly picked but unplannable release, so its success publishes
		// the substitution notice (the shared hunt is silent by default).
		versionPickFallback := false
		if !alternateFileAllowed && trackChange && result.Terminal != nil &&
			result.Terminal.Reason == sourceMetadataIncompleteReasonV3 &&
			requestedFile != nil && effectiveFile != nil &&
			requestedFile.ID != 0 && requestedFile.ID != effectiveFile.ID &&
			playback.VirtualRouteVideoMetadataGapsV3(requestedFile) != "" &&
			playback.VirtualRouteVideoMetadataGapsV3(effectiveFile) == "" {
			// A track_change naming a different file than mounted is a
			// version pick, not a subtitle/audio pick (those name the
			// mounted file and never reach this branch). When planning the
			// pick terminals on missing metadata while the mounted release
			// is fully characterized, fall back through the same alternate
			// hunt (with substitution notice) instead of stranding on a
			// terminal for a row that was never probed. Subtitle/audio
			// terminals keep their in-place degrade above; this branch
			// deliberately bypasses the auto-fallback flag because an
			// explicitly picked but unplannable release with a playable
			// bound sibling is exactly what the notice exists for.
			alternateFileAllowed = true
			versionPickFallback = true
		}
		if subtitleOnlyAllowed {
			// A subtitle-only refusal must not move the release. Re-plan the
			// file already mounted with the subtitle dropped; the in-place
			// helper uses the resolved file verbatim, so a virtual release is
			// never re-listed or substituted. If the same release cannot play
			// without the subtitle either, the original subtitle terminal
			// stands and no sibling is tried.
			if eval, evalErr := h.evaluateSubtitleDegradeInPlaceV3(r, session, record, req, start, effectiveFile, replanVirtualProvenance, plannerRequestedFile, plannerSettings, plannerSettingsErr, attemptedKeys); evalErr == nil && eval != nil && eval.result.Terminal == nil {
				start = eval.start
				effectiveFile = eval.file
				result = eval.result
				toneMapCapabilityErr = eval.toneMapErr
				artifactRecipe = eval.frozenRecipe
				preparedTransport = &eval.transport
				transportPrepared = true
				reservationHeld = eval.reservationHeld
			}
		} else if audioOnlyAllowed {
			// An audio-only refusal must not move the release. Re-plan the file
			// already mounted with another audio track; the helper uses the
			// resolved file verbatim, so a virtual release is never re-listed or
			// substituted. If no track is playable the original audio terminal
			// stands and no sibling is tried.
			if eval, evalErr, ok := h.evaluateAudioDegradeInPlaceV3(r, session, record, req, start, effectiveFile, replanVirtualProvenance, plannerRequestedFile, plannerSettings, plannerSettingsErr, attemptedKeys); ok && evalErr == nil && eval != nil && eval.result.Terminal == nil {
				start = eval.start
				effectiveFile = eval.file
				result = eval.result
				toneMapCapabilityErr = eval.toneMapErr
				artifactRecipe = eval.frozenRecipe
				preparedTransport = &eval.transport
				transportPrepared = true
				reservationHeld = eval.reservationHeld
			}
		} else if alternateFileAllowed {
			alternateOrder := alternateOrderingForClient(req.Capabilities)
			if alternates, alternateErr := h.findAlternateFilesOrdered(r.Context(), requestedFile, requestAccessFilter(r), alternateOrder); alternateErr == nil {
				baseStart := start
				// A speculative detour through the requested edition (above)
				// may have dropped a selection its inventory cannot honor,
				// and validation drops anything still naming another file.
				// The durable plan still carries the viewer's pick: restore
				// it for the hunt so a sibling that CAN honor it keeps
				// subtitles instead of starting muted. Restricted to
				// non-virtual mounts: virtual rows rotate releases under a
				// fixed id, so their selections re-resolve from plan-time
				// evidence, never from a bare carried ordinal.
				if baseStart.SubtitleTrackID == "" && baseStart.SubtitleTrackIndex == nil && !isVirtualPlaybackFile(currentEffectiveFile) {
					if sel := record.CurrentPlan.SelectedTracks.Subtitle; sel != nil {
						baseStart.SubtitleTrackID = sel.ID
						baseStart.SubtitleTrackIndex = copyOptionalIntV3(sel.Index)
					}
				}

				baseEffectiveFile := effectiveFile
				var firstFailureEval *candidateEvaluationV3
				var subtitleMissAlternate *models.MediaFile
				// A terminal that names a server-wide condition (no tone-map
				// recipe, transcoding disabled) is not per-candidate: every
				// remaining sibling would plan to the same verdict, so paying
				// a resolve+probe+plan round-trip per file only delays the
				// replan the client is already waiting on. Stop at the first.
				var capabilityBlocked bool
				for _, alternate := range alternates {
					if capabilityBlocked {
						break
					}
					if alternate.ID == baseEffectiveFile.ID {
						continue
					}
					if virtualCandidateVerdictActive(alternate.FailedAt, time.Now()) {
						// AltMount indicted this version; never attempt it,
						// however the pinned release failed. This mirrors the
						// start-path fallback's authority rule.
						continue
					}
					eval, err := h.evaluateReplanCandidateV3(r, session, record, req, baseStart, baseEffectiveFile, alternate, plannerRequestedFile, plannerSettings, plannerSettingsErr, attemptedKeys)
					if err != nil {
						slog.WarnContext(r.Context(), "replan alternate candidate rejected",
							"component", "api", "session_id", record.SessionID,
							"requested_file_id", record.RequestedMediaFileID, "base_file_id", baseEffectiveFile.ID,
							"candidate_file_id", alternate.ID, "error", logredact.SanitizeURLError(err),
						)
						if candidateSubtitleLocalFailureV3(err) && subtitleMissAlternate == nil {
							subtitleMissAlternate = alternate
						}
						// A planner terminal of transcode_start_failed here is
						// the tone-map discovery retry: it is a server-wide
						// capability condition, so later candidates cannot
						// succeed either.
						if err.Stage == candidateStagePlan && err.TerminalReason == transcodeStartFailedReasonV3 {
							capabilityBlocked = true
						}
					}
					if eval != nil && eval.result.Terminal == nil {
						start = eval.start
						effectiveFile = eval.file
						result = eval.result
						toneMapCapabilityErr = eval.toneMapErr
						artifactRecipe = eval.frozenRecipe
						preparedTransport = &eval.transport
						transportPrepared = true
						reservationHeld = eval.reservationHeld
						if versionPickFallback {
							// The hunt ran for an explicitly picked but
							// unplannable release: name the substitution so
							// the client shows the notice instead of playing
							// a different release silently. Unknown reason:
							// the pick was unplannable (missing metadata),
							// not confirmed dead.
							publishSubstitutionFieldsV3(result.Plan, requestedFile.ID, effectiveFile.ID, substitutionReasonUnknownV3)
						}
						break
					}
					if firstFailureEval == nil && eval != nil {
						firstFailureEval = eval
					}
				}
				if result.Terminal != nil && subtitleMissAlternate != nil {
					// Every candidate with an equivalent track is exhausted.
					// Degrade to subtitles-off rather than terminalling, but only
					// after the honoring candidates above have all been tried.
					if eval, evalErr := h.evaluateSubtitleDegradedCandidateV3(r, session, record, req, baseStart, baseEffectiveFile, subtitleMissAlternate, plannerRequestedFile, plannerSettings, plannerSettingsErr, attemptedKeys); evalErr == nil && eval != nil && eval.result.Terminal == nil {
						start = eval.start
						effectiveFile = eval.file
						result = eval.result
						toneMapCapabilityErr = eval.toneMapErr
						artifactRecipe = eval.frozenRecipe
						preparedTransport = &eval.transport
						transportPrepared = true
						reservationHeld = eval.reservationHeld
					}
				}
				if result.Terminal != nil && firstFailureEval != nil {
					start = firstFailureEval.start
					effectiveFile = firstFailureEval.file
					result = firstFailureEval.result
					toneMapCapabilityErr = firstFailureEval.toneMapErr
				}
			}
		}
	}
	result = retryIncompleteToneMapPlanningV3(result, toneMapCapabilityErr)
	result = retryIncompletePlaybackSettingsV3(result, plannerSettingsErr)
	reportDetourSubtitleDrop()
	h.clarifyOriginalQuality4KTerminalV3(r.Context(), requestAccessFilter(r), result.Terminal, requestedFile, replanAlternateFilePinnedByOriginalQualityV3(operation, start.QualityPreference))
	// A planner terminal on a rotated sibling is surfaced as-is: it names the
	// actual blocker (capability, capacity, recipe) rather than being rewritten
	// as a decode rejection the sibling never produced. The decode reason is
	// reserved for the paths that have per-candidate decode evidence: the
	// no-sibling exhaustion below and a successor the transport actually
	// rejected.
	if decodeAttemptDetail != "" && result.Terminal != nil && strings.TrimSpace(result.Terminal.Detail) == "" {
		// The software decode mode was the plan that just failed. Name both
		// attempted decode modes on any terminal so an exhausted route never
		// reports an empty detail.
		result.Terminal.Detail = decodeAttemptDetail
	}
	if forceSoftwareDecode {
		// The software retry is a deliberate quality trade, not a silent
		// fallback: record it on the plan so clients can surface it, and emit
		// one classifier line carrying the decoder evidence that triggered it.
		if result.Plan != nil {
			result.Plan.DegradationWarnings = append(result.Plan.DegradationWarnings, playback.DegradationWarningV3{
				Code:    degradationSoftwareDecodeFallbackV3,
				Message: "The hardware video decoder could not decode this source; retrying with software decoding.",
			})
		}
		slog.WarnContext(r.Context(), "playback software decode fallback",
			logComponentKey, "playback",
			"session_id", record.SessionID,
			"operation", string(operation),
			"delivery", record.CurrentPlan.Delivery,
			"hw_accel", strings.TrimSpace(session.TranscodeHWAccel),
			"source_video_codec", record.CurrentPlan.Source.VideoCodec,
			"source_video_profile", record.CurrentPlan.Source.VideoProfile,
			"source_video_bit_depth", record.CurrentPlan.Source.BitDepth,
			"decode_failure_sample", decodeFailureSample,
			"decode_failure_count", decodeFailureCount,
		)
	}
	// Media authentication is attempt-sticky (pinned in HandleReplanPlaybackV3),
	// so this mode always equals the one the attempt started under: a reused
	// transport cannot change the session's media security contract.
	mode = headerAuthenticatedMediaV3(start.ClientFeatures)
	if !seekReanchor {
		// A freshly planned replan can land on the same refused progressive
		// remux a start would have; escalate it identically. A seek reanchor
		// replays the frozen recipe verbatim and must not change route identity,
		// so it is excluded — its route was escalated when the attempt started.
		escalated, escalateErr := h.escalateRefusedProgressiveRemuxV3(r.Context(), mode,
			func() playback.PlannerInputV3 {
				input := h.plannerInputV3(r.Context(), start, plannerRequestedFile, effectiveFile, audioIndex, attemptedKeys, replanVirtualProvenance)
				input.LowerVersion = lowerVersionForFileV3(lowerVersion, effectiveFile)
				return input
			}, result)
		if escalateErr != nil {
			return playback.DecisionResponseV3{}, *record, nil, escalateErr
		}
		result = escalated
	}
	// One decision line per replan, mirroring the start endpoint's record:
	// replans choose routes (and terminals) just as consequential, and an
	// unlogged terminal made client reports impossible to reconstruct from
	// server logs alone.
	clientInfo := playbackClientInfoForStartV3(r, req.ClientPlaybackContext)
	if result.Terminal != nil {
		slog.InfoContext(r.Context(), "playback replan decided", append([]any{
			logComponentKey, "playback",
			"outcome", "terminal",
			"reason", result.Terminal.Reason,
			"detail", result.Terminal.Detail,
			"operation", string(operation),
			"session_id", record.SessionID,
			"requested_file_id", record.RequestedMediaFileID,
			"effective_file_id", effectiveFile.ID,
			"quality_preference", start.QualityPreference,
			"software_video_decode", forceSoftwareDecode,
			"decode_failure_count", decodeFailureCount,
		}, clientInfo.LogAttrs()...)...)
		return playback.NewTerminalResponseFromTerminalV3(result.Terminal), *record, nil, nil
	}
	// Surface the warnings from the merged start request's single
	// normalization on the selected plan. A seek reanchor replays the durable
	// plan, which may already carry the same warning from its own start, so the
	// helper skips duplicates.
	appendStartWarningsV3(&result, replanCapabilityWarnings)
	// Explicit mid-session delivery-swap signaling. A failure-recovery replan
	// may legitimately change the serving route (for example transcode-HLS to
	// remux-progressive). Without a marker the client only sees the new plan
	// body and cannot distinguish a planned route change from a silent mid-play
	// swap. Record the change additively on the plan and name old->new on the one
	// decision line below. A seek reanchor replays the durable route verbatim and
	// cannot change it, so it never produces a marker. The marker is UI-only and
	// excluded from plan identity hashing (the identity was finalized in the
	// planner before this point).
	var previousPlayMethod playback.PlayMethod
	if session != nil {
		previousPlayMethod = session.PlayMethod
	}
	var deliveryChange *playback.DeliveryChangeV3
	if !seekReanchor {
		deliveryChange = deliveryChangeV3(record.CurrentPlan.Delivery, previousPlayMethod, result.Plan.Delivery, result.PlayMethod)
	}
	// The marker is scoped to the native v2 surface, like the deferred
	// track-inventory hint (tracks_pending): the frozen /api/v1 bridge must not
	// grow a new field, so a v1 response never exposes it. It is assigned — to
	// nil on v1 and on a seek reanchor — unconditionally, never only when
	// non-nil: a seek replays the durable route verbatim via
	// frozenSeekReanchorResultV3, which copies record.CurrentPlan wholesale and
	// therefore already carries any marker stamped on the recovery plan that
	// produced the current route, so an omitted assignment would re-emit a stale
	// A->B swap. A nil assignment clears it on both surfaces.
	if isNativeAPIV2(r.Context()) {
		result.Plan.DeliveryChange = deliveryChange
	} else {
		result.Plan.DeliveryChange = nil
	}
	replanLogAttrs := []any{
		logComponentKey, "playback",
		"outcome", "plan",
		"decision_reason", result.Plan.DecisionReason,
		"delivery", result.Plan.Delivery,
		"play_method", string(result.PlayMethod),
		"operation", string(operation),
		"session_id", record.SessionID,
		"requested_file_id", record.RequestedMediaFileID,
		"effective_file_id", effectiveFile.ID,
		"dv_profile", result.Plan.Source.DVProfile,
		"dynamic_range", result.Plan.Source.DynamicRange,
		"target_resolution", result.TargetResolution,
		"target_bitrate_kbps", result.TargetBitrateKbps,
		"quality_preference", start.QualityPreference,
		"bandwidth_estimate_kbps", intOrZeroHandlerV3(start.BandwidthEstimateKbps),
		"software_video_decode", forceSoftwareDecode,
		"decode_failure_count", decodeFailureCount,
		"decode_failure_sample", decodeFailureSample,
	}
	if deliveryChange != nil {
		replanLogAttrs = append(replanLogAttrs,
			"previous_delivery", deliveryChange.PreviousDelivery,
			"new_delivery", deliveryChange.Delivery,
			"previous_play_method", string(deliveryChange.PreviousPlayMethod),
			"new_play_method", string(deliveryChange.PlayMethod),
			"delivery_changed", deliveryChange.DeliveryChanged,
			"play_method_changed", deliveryChange.PlayMethodChanged,
		)
	}
	// Record the delivery-stickiness decision beside the swap marker: a replan
	// with no failure behind it prefers the running pipeline (see
	// replanStickyDeliveryV3), so operators can tell a deliberate keep from a
	// swap that never happened to occur. delivery_sticky is true only when the
	// planner had to retry to keep the class; sticky_delivery names the class.
	replanLogAttrs = append(replanLogAttrs, "delivery_sticky", result.StickyDeliveryKept)
	if result.StickyDeliveryKept {
		replanLogAttrs = append(replanLogAttrs, "sticky_delivery", result.Plan.Delivery)
	}
	slog.InfoContext(r.Context(), "playback replan decided", append(replanLogAttrs, clientInfo.LogAttrs()...)...)
	mode = headerAuthenticatedMediaV3(start.ClientFeatures)
	_, reservationHeld = h.sessionMgr.(replacementReservationCancellerV3)
	result.Plan.SessionID = session.ID
	result.Plan.InventoryURL = "/api/v2/playback/" + session.ID + "/inventory"
	artifactRecipe = record.FrozenRecipe
	if !seekReanchor {
		frozenRecipe, frozenErr := h.freezeExecutableRecipeV3(r.Context(), effectiveFile, result)
		if frozenErr != nil {
			return playback.DecisionResponseV3{}, *record, nil, subtitleArtifactErrorV3("Failed to freeze the selected subtitle identity.", frozenErr)
		}
		artifactRecipe = frozenRecipe
	}
	// Resolve every fallible subtitle and frozen-route check before transport
	// preparation publishes a stable proxy grant. An existing client can issue
	// a GET against that grant immediately, even though this replan response has
	// not returned yet, so errors after publication would require canceling an
	// already admitted successor.
	// Every replan keeps the SRT representation the attempt already published;
	// a seek reanchor must also reproduce its frozen artifact exactly. An
	// attempt started by a server that did not know subrip_sidecar_v1 may carry
	// the feature beside WebVTT URLs.
	artifactFeatures := replanSubtitleFeaturesV3(record, start.ClientFeatures)
	if err := h.attachSubtitleArtifactV3(r.Context(), session.ID, effectiveFile, result.Plan, result.SubtitleTrackIndex, &artifactRecipe, artifactFeatures); err != nil {
		return playback.DecisionResponseV3{}, *record, nil, subtitleArtifactErrorV3("Failed to prepare the selected subtitle artifact.", err)
	}
	if seekReanchor {
		if err := validateSeekReanchorPlanV3(record, result.Plan); err != nil {
			changedFields := seekReanchorIdentityChangesV3(record, result.Plan)
			slog.ErrorContext(r.Context(), "protocol v3 seek reanchor changed route identity",
				"session", record.SessionID,
				"playback_attempt_id", record.PlaybackAttemptID,
				"changed_fields", changedFields,
			)
			return playback.DecisionResponseV3{}, *record, nil, &transportErrorV3{
				reason:  "seek_reanchor_route_changed",
				message: err.Error(),
			}
		}
	}
	transportReused := false
	// A track change or a seek reanchor that keeps a byte-identical A/V recipe
	// can keep the active transport. The segment layer serves the new position
	// from the same growing HLS generation — restarting FFmpeg in place when
	// the target is past the produced head — so rebuilding the transport would
	// only add a teardown/respawn and, for virtual sources, a provider
	// re-resolution that turns an in-session seek into a fresh-start stall.
	// validateSeekReanchorPlanV3 has already rejected any route-identity drift,
	// and a target before the active window's start still rebuilds because the
	// reused generation cannot produce bytes before its origin.
	if (trackChange || (seekReanchor && seekReanchorWithinActiveWindowV3(record.CurrentPlan, req.PositionSeconds))) &&
		h.hasActiveReusableTransportV3(session, record.CurrentPlan.Delivery) {
		proxyAllowed := mode.proxyEgress || (!mode.headerAuth && h.JWTSecret != "")
		policy := h.playbackRoutingPolicyForContextV3(r.Context())
		if reusedRecipe, ok := sidecarOnlyReuseReplanV3(record, result.Plan, artifactRecipe, req.ClientPlaybackContext.Output.OutputContextID); ok &&
			reusedHLSRouteAllowedV3(session, result, policy, proxyAllowed) {
			artifactRecipe = reusedRecipe
			result.ToneMapMode = reusedRecipe.ToneMapMode
			transportReused = true
		}
	}
	var transport preparedTransportV3
	if transportPrepared && preparedTransport != nil {
		transport = *preparedTransport
	} else {
		reusedStream := record.CurrentPlan.Stream
		credentialRefreshed := false
		if transportReused {
			// Preserving the prior credential is only safe while it still
			// authorizes the live session for this resource. The old plan is
			// about to be retired, and a signed stream token is session- and
			// file-bound rather than plan-bound, so retirement alone does not
			// invalidate it — but an expired, mismatched, or near-expiry
			// credential is re-minted in place, keeping the session and
			// transport identity so playback is not restarted.
			credentialRefreshed, transportReused = h.authorizeReusedStreamCredentialV3(session, &reusedStream)
		}
		if transportReused {
			// A sidecar selection changes the plan and subtitle artifact, but it
			// does not change the bytes FFmpeg produces. Keep the active
			// generation and its transport window so a client remount cannot
			// strand itself between the killed old window and a replacement
			// window that starts elsewhere. The requested source position still
			// belongs to this replan: translate it onto the reused window
			// instead of rewinding to the previous plan's start.
			result.Plan.Stream = reusedStream
			reusedTimeline := record.CurrentPlan.Timeline
			reusedTimeline.SourceStartSeconds = result.Plan.Timeline.SourceStartSeconds
			reusedTimeline.PlayerStartSeconds = max(0, reusedTimeline.SourceStartSeconds-reusedTimeline.StreamOriginSeconds)
			result.Plan.Timeline = reusedTimeline
			result.Plan.ExpiresAt = record.CurrentPlan.ExpiresAt
			if credentialRefreshed {
				// The credential now carries a full lifetime; the published plan
				// expiry must not outlive it.
				result.Plan.ExpiresAt = playback.NewPlanExpiryV3(time.Now())
			}
			transport = reusedHLSTransportV3(session, reusedStream.URL)
			slog.InfoContext(r.Context(), "protocol v3 replan reused active A/V transport",
				logComponentKey, playbackLogValueV3,
				"playback_session_id", session.ID,
				"previous_plan_id", record.CurrentPlanID,
				"plan_id", result.Plan.PlanID,
				"delivery", result.Plan.Delivery,
				"credential_refreshed", credentialRefreshed,
			)
		} else {
			var transportErr *transportErrorV3
			transportRequest := r
			if proxyOriginRecovery {
				policy := h.playbackRoutingPolicyForContextV3(r.Context())
				policy.DirectPlayEgress = config.PlaybackEgressAPIOnly
				policy.RemuxEgress = config.PlaybackEgressAPIOnly
				policy.VideoTranscodeEgress = config.PlaybackEgressAPIOnly
				transportRequest = r.WithContext(withPlaybackRoutingPolicySnapshotV3(r.Context(), policy))
			}
			// A decode-driven candidate rotation tells transport preparation to
			// serve the plan's replacement candidate rather than the session's
			// still-rejected binding. The intent travels in the request context
			// so the live session is not mutated before the durable session
			// replacement commits; the local transport's session clamp reads it
			// in preference to session.VirtualSourceURI.
			if virtualDecodeRotation && isVirtualPlaybackFile(effectiveFile) &&
				strings.TrimSpace(session.VirtualSourceURI) != "" &&
				effectiveFile.FilePath != session.VirtualSourceURI {
				transportRequest = transportRequest.WithContext(withVirtualSourceRotationV3(
					transportRequest.Context(), effectiveFile.FilePath, effectiveFile.VirtualOwnerInstallationID,
				))
			}
			transport, transportErr = h.prepareTransportV3(transportRequest, session, effectiveFile, result, mode)
			if transportErr != nil {
				if transportErr.reason == candidateSourceDecodeRejectedReasonV3 {
					// The candidate this replan prepared was rejected by its
					// decoder during startup. That is a genuine decode rejection
					// with per-candidate evidence, so report the client-facing
					// reason instead of leaking the internal transport token as a
					// terminal. A rotation retires the exhausted delivery; any
					// other replan just surfaces the decode verdict. (A bounded
					// multi-candidate replan retry is a follow-up; the start path
					// already loops.)
					if virtualDecodeRotation {
						demoteDeliveryCapabilityV3(&record.NormalizedRequest, record.CurrentPlan.Delivery)
						demoteDeliveryCapabilityV3(&start, record.CurrentPlan.Delivery)
					}
					return playback.DecisionResponseV3{}, *record, nil, &transportErrorV3{
						reason:    sourceDecodeFailedReasonV3,
						message:   sourceDecodeFailedMessageV3,
						retryable: false,
						cause:     transportErr.cause,
					}
				}
				return playback.DecisionResponseV3{}, *record, nil, transportErr
			}
			applyTransportToneMapModeV3(&result, transport)
			// Transport preparation can only attest the executor's tone-map
			// mode; every other frozen identity field was validated above. Copy
			// that one receipt into the already validated recipe instead of
			// rerunning a fallible subtitle-identity freeze after authority
			// publication.
			artifactRecipe.ToneMapMode = result.ToneMapMode
		}
	}
	result.Plan.Stream.URL = transport.url
	response := playback.DecisionResponseV3{ProtocolVersion: playback.ProtocolV3, ServerFeatures: serverFeaturesForRequestV3(r.Context()), Outcome: playback.OutcomePlayableV3, SessionID: session.ID, PlaybackPlan: result.Plan}
	updated := *record
	updated.CurrentPlanID = result.Plan.PlanID
	updated.CurrentPlan = *result.Plan
	// A seek reanchor replays the durable recipe verbatim (updated already
	// carries it); re-freezing from live inventory could only re-introduce
	// the drift this path exists to exclude. Every other replan just accepted
	// a freshly planned route and must freeze its recipe — loudly, because a
	// recipe with a silently missing subtitle identity would disable drift
	// detection for every later seek on this attempt.
	if !seekReanchor {
		updated.FrozenRecipe = artifactRecipe
	}
	updated.NormalizedRequest = start
	updated.EffectiveMediaFileID = effectiveFile.ID
	updated.ExpiresAt = time.Now().Add(playback.MaxTokenTTL)
	if transportReused {
		if expiresAt, parseErr := time.Parse(time.RFC3339, result.Plan.ExpiresAt); parseErr == nil {
			updated.ExpiresAt = expiresAt
		}
	}
	originalRollback := transport.rollback
	originalRollbackRequired := transport.rollbackRequired
	replacement := playback.SessionReplacement{
		EffectiveMediaFileID: effectiveFile.ID,
		StreamState:          h.v3SessionStreamState(r.Context(), session, effectiveFile, result, transport, headerAuthenticatedMediaV3(req.ClientFeatures)),
	}
	if seekScopedRecovery {
		replacement.PositionSeconds = &req.PositionSeconds
		replacement.PreservePaused = true
	}
	transport.applySession = func() (func() error, error) {
		rollback, err := replacementManager.ApplyReplacement(session.ID, replacement)
		if err != nil {
			return nil, err
		}
		return func() error {
			return replacementManager.RollbackReplacement(session.ID, rollback)
		}, nil
	}
	transport.afterDurableCommit = func() {
		cancelReservation()
		afterCtx, afterCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer afterCancel()
		if trackChange && req.Automatic != playback.ReplanAutomaticV3 {
			// A deliberate track switch is the same signal the legacy audio
			// PATCH recorded; a failure recovery is not, so its forced audio
			// route must not be written back as a user preference. An
			// automatic reconciliation is not a viewer choice either: it
			// replays the start-time language preference, so persisting its
			// outcome would launder a server correction into a stored user
			// preference and steer every later start. The Automatic marker
			// (server-side only, see ReplanRequestV3) suppresses that write
			// for reconciliation ops; explicit user track changes persist.
			h.persistCurrentAudioPreferenceV3(afterCtx, session.ID, session.UserID, session.ProfileID, effectiveFile, plannedAudioTrackIndexV3(result, session.AudioTrackIndex))
		}
		h.syncSessionsNow(afterCtx, "v3_replan")
		event := playback.RouteEventPlanSelectedV3
		clientModel := req.ClientPlaybackContext.Device.Model
		if seekReanchor {
			event = playback.RouteEventRuntimeCorrectionSucceededV3
			clientModel = start.ClientPlaybackContext.Device.Model
		}
		h.enqueueRouteEventV3(playback.RouteEventRecordV3{RouteEventV3: playback.RouteEventV3{ProtocolVersion: playback.ProtocolV3, PlaybackAttemptID: req.PlaybackAttemptID, SessionID: session.ID, PlanID: result.Plan.PlanID, PlanAttemptID: req.PlanAttemptID, PlanAttemptKey: playback.PlanAttemptKeyV3(*result.Plan, start.ClientPlaybackContext.Output.OutputContextID, nil), Event: event, FallbackReason: req.Failure.Classification, AppliedQuirkIDs: appliedQuirkIDsV3(result.Plan), QuirkRegistryRevision: appliedQuirkRevisionV3(result.Plan), OutputContextID: start.ClientPlaybackContext.Output.OutputContextID, Diagnostics: softwareDecodeRouteDiagnosticsV3(forceSoftwareDecode, session, decodeFailureSample, decodeFailureCount)}, UserID: session.UserID, ProfileID: session.ProfileID, ClientName: session.ClientName, ClientVersion: session.ClientVersion, ClientBuild: session.ClientBuild, ClientChannel: session.ClientChannel, ClientModel: clientModel})
	}
	transport.rollback = func() {
		originalRollback()
		cancelReservation()
	}
	if originalRollbackRequired != nil {
		transport.rollbackRequired = func() error {
			if err := originalRollbackRequired(); err != nil {
				return err
			}
			cancelReservation()
			return nil
		}
	}
	reservationHandedOff = true
	return response, updated, &transport, nil
}

// replanStickyDeliveryV3 decides whether a replan should prefer to keep the
// delivery the session is already serving. Stickiness is a user-intent backstop,
// not a failure-recovery policy: a failure recovery (or its seek-scoped variant)
// carries an explicit verdict about the current route and must remain free to
// move. Every other operation — track, quality, output, seek reanchor, and the
// automatic audio correction — has no failure behind it, so re-deriving the
// delivery from scratch can only thrash the pipeline. The returned value is the
// current delivery for those operations; the planner keeps it only when it
// genuinely can still serve the request, and a principled switch (burn-in,
// forced encode, quality ladder) still wins.
func replanStickyDeliveryV3(operation playback.ReplanOperationV3, current playback.DeliveryV3) playback.DeliveryV3 {
	switch operation {
	case playback.ReplanOperationFailureRecoveryV3, playback.ReplanOperationSeekFailureRecoveryV3:
		return ""
	default:
		return current
	}
}

// deliveryChangeV3 builds the additive mid-session delivery-swap marker, or
// returns nil when the serving route did not change. A delivery change is always
// detectable; a play-method change is only reported when the previous method is
// known, so a reconstructed session with no cached method does not emit a
// spurious marker for an unchanged route.
func deliveryChangeV3(previousDelivery playback.DeliveryV3, previousPlayMethod playback.PlayMethod, nextDelivery playback.DeliveryV3, nextPlayMethod playback.PlayMethod) *playback.DeliveryChangeV3 {
	deliveryChanged := previousDelivery != "" && previousDelivery != nextDelivery
	playMethodChanged := previousPlayMethod != "" && previousPlayMethod != nextPlayMethod
	if !deliveryChanged && !playMethodChanged {
		return nil
	}
	return &playback.DeliveryChangeV3{
		PreviousDelivery:   previousDelivery,
		Delivery:           nextDelivery,
		PreviousPlayMethod: previousPlayMethod,
		PlayMethod:         nextPlayMethod,
		DeliveryChanged:    deliveryChanged,
		PlayMethodChanged:  playMethodChanged,
	}
}

// softwareDecodeRouteDiagnosticsV3 records the reactive software-decode
// fallback and the decoder evidence on the durable plan-selected route event.
// It returns nil for an ordinary replan so the event shape is unchanged.
func softwareDecodeRouteDiagnosticsV3(force bool, session *playback.Session, sample string, count int) map[string]string {
	if !force {
		return nil
	}
	diagnostics := map[string]string{
		"software_video_decode": "true",
		"decode_failure_count":  strconv.Itoa(count),
	}
	if session != nil {
		if hw := strings.TrimSpace(session.TranscodeHWAccel); hw != "" {
			diagnostics["hw_accel"] = hw
		}
	}
	if strings.TrimSpace(sample) != "" {
		diagnostics["decode_failure_sample"] = sample
	}
	return diagnostics
}

// applyTransportToneMapModeV3 records an executor fallback in the result before
// the durable recipe and session state are committed.
func applyTransportToneMapModeV3(result *playback.PlannerResultV3, transport preparedTransportV3) {
	if result != nil && transport.toneMapMode != "" {
		result.ToneMapMode = transport.toneMapMode
	}
}

func frozenSeekReanchorResultV3(record *playback.AttemptRecordV3, position float64, now time.Time) (playback.PlannerResultV3, error) {
	if record == nil || !record.FrozenRecipe.ValidFor(record.CurrentPlan) {
		return playback.PlannerResultV3{}, errors.New("the active playback recipe is unavailable")
	}
	plan := record.CurrentPlan
	plan.ExpiresAt = playback.NewPlanExpiryV3(now)
	plan.Timeline = playback.TimelineV3{
		SourceStartSeconds: position,
		PlayerStartSeconds: position,
		CanSeekAnywhere:    true,
		SeekRestoration:    seekRestorationPlayerV3,
	}
	return record.FrozenRecipe.PlannerResult(&plan), nil
}

type subtitleIndexLocationV3 struct {
	source string
	offset int
}

// classifySubtitleIndexV3 maps the dense published combined subtitle index used
// by buildSubtitleURLs to its inventory segment and segment-local offset. The
// published space suppresses de-duplicated tracks, so the file's own range is
// translated back to its source ordinal before the external/embedded split; an
// index past the file's own published count is a downloaded entry whose offset
// follows that same published base.
func classifySubtitleIndexV3(file *models.MediaFile, index int) (subtitleIndexLocationV3, bool) {
	if file == nil || index < 0 {
		return subtitleIndexLocationV3{}, false
	}
	ownPublished := len(playback.BuildSubtitleInventoryV3(file, nil))
	if index < ownPublished {
		sourceIndex, ok := playback.SubtitleInventoryOwnSourceIndexV3(file, index)
		if !ok {
			return subtitleIndexLocationV3{}, false
		}
		if sourceIndex < len(file.ExternalSubtitles) {
			return subtitleIndexLocationV3{source: playback.SubtitleSourceExternalV3, offset: sourceIndex}, true
		}
		return subtitleIndexLocationV3{source: playback.SubtitleSourceEmbeddedV3, offset: sourceIndex - len(file.ExternalSubtitles)}, true
	}
	return subtitleIndexLocationV3{
		source: playback.SubtitleSourceDownloadedV3,
		offset: index - ownPublished,
	}, true
}

// freezeExecutableRecipeV3 extends the pure planner freeze with the identity
// of the selected sidecar subtitle. The combined subtitle index space
// (externals, then embedded, then downloaded — see buildSubtitleURLs) is not
// stable across inventory changes, so the index alone cannot anchor a durable
// selection. A downloaded selection whose identity cannot be established is
// an error: silently omitting it would disable drift detection for exactly
// the seeks this recipe exists to protect.
func (h *PlaybackHandler) freezeExecutableRecipeV3(_ context.Context, file *models.MediaFile, result playback.PlannerResultV3) (playback.ExecutableRecipeV3, error) {
	recipe := playback.FreezeExecutableRecipeV3(result)
	if file != nil {
		sourceMetadata := sourceExecutionMetadataV3(file, playback.PlannerResultV3{})
		recipe.SourceVideoCodec = sourceMetadata.VideoCodec
		recipe.SourceVideoProfile = sourceMetadata.VideoProfile
		recipe.SourceVideoBitDepth = sourceMetadata.VideoBitDepth
		recipe.SoftwareVideoDecode = sourceMetadata.SoftwareVideoDecode
		if result.Plan != nil && result.Plan.EffectiveRecipe.SoftwareVideoDecode {
			// The plan, not the source facts, is authoritative for a reactive
			// software-decode retry: freeze the decode mode so a reconstruct
			// cannot re-enable the hardware decoder that just failed.
			recipe.SoftwareVideoDecode = true
		}
		recipe.SourceDurationSeconds = sourceMetadata.DurationSeconds
		recipe.ToneMapDVConfigPresent = sourceMetadata.ToneMapDVConfigPresent
		recipe.ToneMapDVBLCompatIDPresent = sourceMetadata.ToneMapDVBLCompatIDPresent
		recipe.ToneMapDVBLPresent = sourceMetadata.ToneMapDVBLPresent
		recipe.ToneMapDVRPUPresent = sourceMetadata.ToneMapDVRPUPresent
		if recipe.ToneMapDVConfigPresent || recipe.ToneMapDVBLCompatIDPresent || recipe.ToneMapDVBLPresent || recipe.ToneMapDVRPUPresent {
			recipe.Version = 2
		}
	}
	if file == nil || result.SubtitleTrackIndex < 0 {
		return recipe, nil
	}
	// A downloaded row ID was selected from the planner's inventory snapshot.
	// Treat it as authoritative before consulting the mutable combined-index
	// segments: an external or embedded subtitle added after planning must not
	// make this downloaded selection look like a different source.
	if recipe.DownloadedSubtitleID > 0 {
		recipe.SubtitleSource = playback.SubtitleSourceDownloadedV3
		return recipe, nil
	}
	location, ok := classifySubtitleIndexV3(file, result.SubtitleTrackIndex)
	if !ok {
		return recipe, nil
	}
	switch location.source {
	case playback.SubtitleSourceExternalV3:
		recipe.SubtitleSource = playback.SubtitleSourceExternalV3
		recipe.ExternalSubtitlePath = file.ExternalSubtitles[location.offset].Path
	case playback.SubtitleSourceEmbeddedV3:
		recipe.SubtitleSource = playback.SubtitleSourceEmbeddedV3
		recipe.EmbeddedStreamIndex = file.SubtitleTracks[location.offset].Index
	case playback.SubtitleSourceDownloadedV3:
		if recipe.DownloadedSubtitleID <= 0 {
			return playback.ExecutableRecipeV3{}, errors.New("the selected downloaded subtitle has no stable identity")
		}
	}
	return recipe, nil
}

// validateFrozenSubtitleIdentityV3 confirms the frozen combined subtitle
// index still resolves to the identical inventory entry it was frozen
// against. It mirrors the segment layout of buildSubtitleURLs so a change in
// any earlier segment's size — which shifts every later index — is detected
// as an identity mismatch rather than silently re-resolved.
func (h *PlaybackHandler) validateFrozenSubtitleIdentityV3(ctx context.Context, file *models.MediaFile, recipe playback.ExecutableRecipeV3) error {
	if recipe.SubtitleSource == "" {
		return nil
	}
	if file == nil || recipe.SubtitleTrackIndex < 0 {
		return errors.New("the frozen subtitle selection is unavailable")
	}
	if recipe.SubtitleSource == playback.SubtitleSourceDownloadedV3 {
		if h == nil || h.SubtitleRepo == nil || recipe.DownloadedSubtitleID <= 0 {
			return errors.New("the downloaded subtitle inventory is unavailable")
		}
		downloaded, err := h.SubtitleRepo.GetDownloadedSubtitle(ctx, recipe.DownloadedSubtitleID)
		if err != nil {
			return wrapSubtitleStoreErrorV3(err)
		}
		if downloaded == nil || downloaded.MediaFileID != file.ID {
			return errors.New("the frozen downloaded subtitle identity changed")
		}
		return nil
	}
	location, ok := classifySubtitleIndexV3(file, recipe.SubtitleTrackIndex)
	if !ok || location.source != recipe.SubtitleSource {
		return errors.New("the frozen subtitle inventory segment changed")
	}
	switch recipe.SubtitleSource {
	case playback.SubtitleSourceExternalV3:
		if file.ExternalSubtitles[location.offset].Path != recipe.ExternalSubtitlePath {
			return errors.New("the frozen external subtitle identity changed")
		}
	case playback.SubtitleSourceEmbeddedV3:
		if file.SubtitleTracks[location.offset].Index != recipe.EmbeddedStreamIndex {
			return errors.New("the frozen embedded subtitle identity changed")
		}
	default:
		return errors.New("the frozen subtitle identity is unrecognized")
	}
	return nil
}

func validateSeekRecoveryRequestV3(record *playback.AttemptRecordV3, req playback.ReplanRequestV3) error {
	if record == nil {
		return errors.New("the current playback attempt is unavailable")
	}
	wantedQuality, _ := playback.NormalizeQualityV3(record.NormalizedRequest.QualityPreference)
	requestedQuality, _ := playback.NormalizeQualityV3(req.QualityPreference)
	if requestedQuality != wantedQuality {
		return errors.New("seek recovery cannot change playback quality")
	}
	if req.ClientPlaybackContext.Output.OutputContextID != record.NormalizedRequest.ClientPlaybackContext.Output.OutputContextID {
		return errors.New("seek recovery cannot change the output route")
	}
	if !sameSelectedTracksV3(req.SelectedTracks, record.CurrentPlan.SelectedTracks) {
		return errors.New("seek recovery cannot change selected tracks")
	}
	return nil
}

// seekReanchorIdentityChangesV3 returns only bounded, non-secret field names.
// It is safe for structured logs: values, URLs, headers, tokens, and subtitle
// artifact locations are deliberately excluded.
func seekReanchorIdentityChangesV3(record *playback.AttemptRecordV3, candidate *playback.PlanV3) []string {
	if record == nil || candidate == nil {
		return []string{"route"}
	}
	current := record.CurrentPlan
	changed := make([]string, 0, 16)
	add := func(name string, differs bool) {
		if differs {
			changed = append(changed, name)
		}
	}
	add("plan_id", candidate.PlanID != record.CurrentPlanID || candidate.PlanID != current.PlanID)
	add("requested_file_id", candidate.RequestedMediaFileID != record.RequestedMediaFileID)
	add("effective_file_id", candidate.EffectiveMediaFileID != record.EffectiveMediaFileID)
	add("delivery", candidate.Delivery != current.Delivery)
	add("protocol", candidate.Stream.Protocol != current.Stream.Protocol)
	add("container", candidate.Stream.Container != current.Stream.Container)
	add("mime_type", candidate.Stream.MIMEType != current.Stream.MIMEType)
	add("header_refresh", candidate.Stream.HeaderRefresh != current.Stream.HeaderRefresh)
	add("video_codec", candidate.EffectiveRecipe.VideoCodec != current.EffectiveRecipe.VideoCodec)
	add("video_sample_entry", candidate.EffectiveRecipe.VideoSampleEntry != current.EffectiveRecipe.VideoSampleEntry)
	add("audio_codec", candidate.EffectiveRecipe.AudioCodec != current.EffectiveRecipe.AudioCodec)
	add("resolution", !optionalIntEqualV3(candidate.EffectiveRecipe.Width, current.EffectiveRecipe.Width) || !optionalIntEqualV3(candidate.EffectiveRecipe.Height, current.EffectiveRecipe.Height))
	add("frame_rate", !optionalFloatEqualV3(candidate.EffectiveRecipe.FrameRate, current.EffectiveRecipe.FrameRate))
	add("bitrate", !optionalIntEqualV3(candidate.EffectiveRecipe.BitrateKbps, current.EffectiveRecipe.BitrateKbps))
	add("dynamic_range", candidate.EffectiveRecipe.DynamicRange != current.EffectiveRecipe.DynamicRange)
	add("audio_channels", !optionalIntEqualV3(candidate.EffectiveRecipe.AudioChannels, current.EffectiveRecipe.AudioChannels) || candidate.EffectiveRecipe.AudioLayout != current.EffectiveRecipe.AudioLayout)
	add("selected_audio", !sameTrackIdentityV3(candidate.SelectedTracks.Audio, current.SelectedTracks.Audio))
	add("selected_subtitle", !sameTrackIdentityV3(candidate.SelectedTracks.Subtitle, current.SelectedTracks.Subtitle))
	add("subtitle_mode", candidate.Subtitle.Mode != current.Subtitle.Mode || candidate.Subtitle.TrackID != current.Subtitle.TrackID)
	add("subtitle_embedded_route", !sameEmbeddedSubtitleRouteV3(candidate.Subtitle.Embedded, current.Subtitle.Embedded))
	add("subtitle_artifact_route", !sameSubtitleArtifactRouteV3(candidate.Subtitle.Artifact, current.Subtitle.Artifact))
	add("subtitle_fidelity", candidate.SubtitleFidelityPolicy != current.SubtitleFidelityPolicy)
	add("transformations", !sameTransformationsV3(candidate.Transformations, current.Transformations))
	add("quirks", !sameAppliedQuirksV3(candidate.AppliedQuirks, current.AppliedQuirks))
	add("runtime_corrections", !sameStringMultisetV3(candidate.RuntimeCorrections, current.RuntimeCorrections))
	add("claims", candidate.Claims != current.Claims)
	return changed
}

func validateSeekReanchorPlanV3(record *playback.AttemptRecordV3, candidate *playback.PlanV3) error {
	if record == nil || candidate == nil {
		return errors.New("seek reanchor produced no playback route")
	}
	changedFields := seekReanchorIdentityChangesV3(record, candidate)
	if len(changedFields) == 0 {
		return nil
	}
	if containsStringExactV3(changedFields, "plan_id") {
		return errors.New("seek reanchor changed the playback plan identity")
	}
	if containsStringExactV3(changedFields, "requested_file_id") || containsStringExactV3(changedFields, "effective_file_id") {
		return errors.New("seek reanchor changed the selected media version")
	}
	if containsStringExactV3(changedFields, "selected_audio") || containsStringExactV3(changedFields, "selected_subtitle") {
		return errors.New("seek reanchor changed selected tracks")
	}
	return errors.New("seek reanchor changed the playback route semantics")
}

func sameEmbeddedSubtitleRouteV3(left, right *playback.EmbeddedSubtitleV3) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameSubtitleArtifactRouteV3(left, right *playback.SubtitleArtifactV3) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	// Signed URLs and timing origins are allowed to rotate when a transport is
	// reopened; the player-facing artifact representation is not.
	return left.MIMEType == right.MIMEType && left.Format == right.Format
}

func sameTransformationsV3(left, right []playback.TransformationV3) bool {
	if len(left) != len(right) {
		return false
	}
	matched := make([]bool, len(right))
	for _, candidate := range left {
		found := false
		for index, current := range right {
			if !matched[index] && candidate.Name == current.Name && candidate.Executor == current.Executor &&
				candidate.RecipeVersion == current.RecipeVersion && sameStringMultisetV3(candidate.ValidatedClaims, current.ValidatedClaims) {
				matched[index] = true
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func sameAppliedQuirksV3(left, right []playback.AppliedQuirkV3) bool {
	if len(left) != len(right) {
		return false
	}
	matched := make([]bool, len(right))
	for _, candidate := range left {
		found := false
		for index, current := range right {
			if !matched[index] && candidate == current {
				matched[index] = true
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func sameStringMultisetV3(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, value := range left {
		counts[value]++
	}
	for _, value := range right {
		counts[value]--
		if counts[value] < 0 {
			return false
		}
	}
	return true
}

// sidecarOnlyReuseReplanV3 proves that a track-change plan can keep the active
// A/V generation. Subtitle identity and claims are intentionally excluded:
// those are the point of the replan and are delivered by the independently
// addressed sidecar artifact. Every field that can change FFmpeg's audio/video
// output remains part of the comparison.
func sidecarOnlyReuseReplanV3(record *playback.AttemptRecordV3, candidate *playback.PlanV3, candidateRecipe playback.ExecutableRecipeV3, outputContextID string) (playback.ExecutableRecipeV3, bool) {
	if record == nil || candidate == nil || record.CurrentPlan.Stream.URL == "" ||
		!record.FrozenRecipe.ValidFor(record.CurrentPlan) || !candidateRecipe.ValidFor(*candidate) ||
		record.NormalizedRequest.ClientPlaybackContext.Output.OutputContextID != outputContextID ||
		record.EffectiveMediaFileID != candidate.EffectiveMediaFileID ||
		record.CurrentPlan.RequestedMediaFileID != candidate.RequestedMediaFileID ||
		record.CurrentPlan.EffectiveMediaFileID != candidate.EffectiveMediaFileID ||
		!reuseEligibleDeliveryV3(record.CurrentPlan.Delivery) || record.CurrentPlan.Delivery != candidate.Delivery ||
		record.CurrentPlan.Subtitle.Mode == playback.SubtitleBurnInV3 || candidate.Subtitle.Mode == playback.SubtitleBurnInV3 ||
		!sameTrackIdentityV3(record.CurrentPlan.SelectedTracks.Audio, candidate.SelectedTracks.Audio) {
		return candidateRecipe, false
	}
	// The planner prefers hardware whenever both executors are currently
	// available, but the active transport may have fallen back to software after
	// its plan was accepted. A sidecar-only replan must compare against and carry
	// that effective mode or it will replace byte-identical running output merely
	// because planning preferred hardware again.
	effectiveCandidateRecipe := candidateRecipe
	if effectiveMode := record.FrozenRecipe.ToneMapMode; effectiveMode != "" && candidateRecipe.ToneMapPolicy.Allows(effectiveMode) {
		effectiveCandidateRecipe.ToneMapMode = effectiveMode
	}
	if record.CurrentPlan.Stream.Protocol != candidate.Stream.Protocol ||
		record.CurrentPlan.Stream.Container != candidate.Stream.Container ||
		record.CurrentPlan.Stream.MIMEType != candidate.Stream.MIMEType ||
		record.CurrentPlan.Stream.HeaderRefresh != candidate.Stream.HeaderRefresh ||
		!sameExecutableAVRecipeV3(record.FrozenRecipe, effectiveCandidateRecipe) ||
		!sameEffectiveAVRecipeV3(record.CurrentPlan.EffectiveRecipe, candidate.EffectiveRecipe) ||
		record.CurrentPlan.Claims.Video != candidate.Claims.Video ||
		record.CurrentPlan.Claims.Audio != candidate.Claims.Audio ||
		!sameTransformationsV3(record.CurrentPlan.Transformations, candidate.Transformations) ||
		!sameAppliedQuirksV3(record.CurrentPlan.AppliedQuirks, candidate.AppliedQuirks) ||
		!sameStringMultisetV3(record.CurrentPlan.RuntimeCorrections, candidate.RuntimeCorrections) {
		return candidateRecipe, false
	}
	return effectiveCandidateRecipe, true
}

func isHLSDeliveryV3(delivery playback.DeliveryV3) bool {
	return delivery == playback.DeliveryRemuxHLSV3 || delivery == playback.DeliveryTranscodeHLSV3
}

// reuseEligibleDeliveryV3 reports whether a delivery keeps a single addressable
// stream window whose bytes do not change when a sidecar-only track switch
// replans. HLS generations are keyed to the session, and both a local
// progressive remux and a direct HTTP play are served lazily from
// /stream/{sessionID}, so they survive a subtitle switch without a client
// remount. Direct HTTP is included because its URL is transport-stable while
// the route is unchanged: the only per-plan variation was the signed token,
// which the reuse path preserves verbatim. The reused token still carries the
// active plan's claims.
func reuseEligibleDeliveryV3(delivery playback.DeliveryV3) bool {
	return delivery == playback.DeliveryOriginalHTTPV3 ||
		delivery == playback.DeliveryRemuxProgressiveV3 ||
		delivery == playback.DeliveryRemuxHLSV3 ||
		delivery == playback.DeliveryTranscodeHLSV3
}

// seekReanchorWithinActiveWindowV3 reports whether a seek target can be served
// in place by the active transport's window. Only a segment-addressable HLS
// transport can: its manifest resolves an arbitrary position against the
// growing generation, and the segment layer restarts FFmpeg in place for
// targets past the produced head. A progressive remux response is a single
// continuous byte stream from its original start, so reusing it for a seek
// hands the client a URL whose bytes already (or never) cover the target; a
// progressive reanchor must rebuild instead.
//
// A target before an HLS window's start needs a fresh generation (the reused
// one cannot produce bytes before its origin); a target past the produced head
// is deliberately allowed through. A closed window end is honored so reuse
// never claims a range the transport no longer serves.
//
// A can_seek_anywhere plan publishes no window start, but the reused generation
// still begins at a known stream origin: the segment layer can only serve
// bytes at or after that origin, and a target before it makes
// copyForwardJumpSeekTarget decline with ErrSegmentNotFound, turning the seek
// into a 404 -> failure-recovery rebuild. Reject it here so the caller rebuilds
// directly. An unknown origin (zero) keeps the previous permissive behavior.
func seekReanchorWithinActiveWindowV3(plan playback.PlanV3, position float64) bool {
	if !isHLSDeliveryV3(plan.Delivery) {
		return false
	}
	if plan.Timeline.SeekWindowStartSeconds != nil && position < *plan.Timeline.SeekWindowStartSeconds {
		return false
	}
	if plan.Timeline.StreamOriginSeconds > 0 && position < plan.Timeline.StreamOriginSeconds {
		return false
	}
	if plan.Timeline.SeekWindowEndSeconds != nil && position > *plan.Timeline.SeekWindowEndSeconds {
		return false
	}
	return true
}

// sameExecutableAVRecipeV3 reports whether two frozen A/V recipes are equivalent.
func sameExecutableAVRecipeV3(left, right playback.ExecutableRecipeV3) bool {
	return left.PlayMethod == right.PlayMethod &&
		left.TranscodeAudio == right.TranscodeAudio &&
		left.TargetVideoCodec == right.TargetVideoCodec &&
		left.TargetAudioCodec == right.TargetAudioCodec &&
		left.SourceAudioChannels == right.SourceAudioChannels &&
		left.TargetAudioChannels == right.TargetAudioChannels &&
		left.TargetAudioBitrateKbps == right.TargetAudioBitrateKbps &&
		left.TargetResolution == right.TargetResolution &&
		left.TargetBitrateKbps == right.TargetBitrateKbps &&
		left.SourceVideoCodec == right.SourceVideoCodec &&
		left.SourceVideoProfile == right.SourceVideoProfile &&
		left.SourceVideoBitDepth == right.SourceVideoBitDepth &&
		left.SoftwareVideoDecode == right.SoftwareVideoDecode &&
		left.SourceDurationSeconds == right.SourceDurationSeconds &&
		left.ToneMapPolicy == right.ToneMapPolicy &&
		left.ToneMapMode == right.ToneMapMode &&
		left.ToneMapSourceKind == right.ToneMapSourceKind &&
		left.ToneMapRecipeVersion == right.ToneMapRecipeVersion &&
		left.ToneMapPreflightRequired == right.ToneMapPreflightRequired &&
		left.ToneMapSourceRevision == right.ToneMapSourceRevision &&
		left.ToneMapDVConfigPresent == right.ToneMapDVConfigPresent &&
		left.ToneMapDVBLCompatIDPresent == right.ToneMapDVBLCompatIDPresent &&
		left.ToneMapDVBLPresent == right.ToneMapDVBLPresent &&
		left.ToneMapDVRPUPresent == right.ToneMapDVRPUPresent
}

func sameEffectiveAVRecipeV3(left, right playback.EffectiveRecipeV3) bool {
	return left.VideoCodec == right.VideoCodec && left.AudioCodec == right.AudioCodec &&
		optionalIntEqualV3(left.Width, right.Width) && optionalIntEqualV3(left.Height, right.Height) &&
		optionalFloatEqualV3(left.FrameRate, right.FrameRate) &&
		optionalIntEqualV3(left.BitrateKbps, right.BitrateKbps) &&
		left.DynamicRange == right.DynamicRange &&
		optionalIntEqualV3(left.AudioChannels, right.AudioChannels) && left.AudioLayout == right.AudioLayout
}

// reusedStreamCredentialRefreshMarginV3 is how much of a reused signed stream
// credential's lifetime must remain before a transport may reuse it verbatim.
// A credential closer to expiry is re-minted in place.
const reusedStreamCredentialRefreshMarginV3 = time.Hour

// authorizeReusedStreamCredentialV3 proves that the signed credential carried by
// a reused stream URL still authorizes the live session and its resource, and
// re-mints it in place when it is missing, mismatched, or close to expiry. It
// exists because preserving record.CurrentPlan.Stream verbatim is only safe
// while that credential remains valid: retiring the plan does not retire a
// session-bound token, so the credential is bound to the session and the media
// file, not to the plan. A credential that no longer proves that binding — or
// that is about to expire — must be re-minted rather than reused.
//
// Only the st query parameter is re-signed; the URL path, its other query
// parameters, and the session identity are untouched, so a sidecar-only replan
// cannot force a remount. A URL with no st credential (header-authenticated
// attempts, signing disabled, or a proxy path token) is returned unchanged:
// there is no plan-scoped credential here to validate.
//
// The returned bool is whether the credential was re-minted. ok is false when
// the URL's credential cannot be proven to authorize the resource and could not
// be refreshed, in which case the caller must rebuild the transport.
func (h *PlaybackHandler) authorizeReusedStreamCredentialV3(session *playback.Session, stream *playback.StreamV3) (refreshed bool, ok bool) {
	if h == nil || session == nil || stream == nil {
		return false, false
	}
	token := streamTokenFromURLV3(stream.URL)
	if token == "" || h.JWTSecret == "" {
		// No signed credential to validate. Header-authenticated attempts
		// deliberately publish a credential-free URL, and an empty signing
		// secret cannot mint or verify one; both preserve the URL byte-for-byte.
		return false, true
	}
	if claims, err := streamtoken.Verify(token, h.JWTSecret); err == nil && claims.SessionID == session.ID &&
		(session.MediaFileID == 0 || claims.MediaFileID == session.MediaFileID) {
		if claims.ExpiresAt != nil && time.Until(claims.ExpiresAt.Time) > reusedStreamCredentialRefreshMarginV3 {
			return false, true
		}
		// A verified, correctly bound credential that is close to expiry is
		// re-signed from its own claims, so a transcode recipe is preserved
		// exactly rather than rebuilt from session identity.
		if fresh, signErr := streamtoken.Sign(*claims, h.JWTSecret, playback.MaxTokenTTL); signErr == nil && fresh != "" {
			stream.URL = setStreamTokenV3(stream.URL, fresh)
			return true, true
		}
	}
	// The credential could not be verified, was bound to another resource, or
	// could not be re-signed from its own claims. A session whose identity
	// recipe the server can reconstruct without the token gets a fresh
	// credential; anything else must rebuild.
	card, identityReconstructable := h.identityReconstructableCardV3(session)
	if !identityReconstructable {
		return false, false
	}
	fresh := h.signSessionToken(card, false)
	if fresh == "" {
		return false, false
	}
	stream.URL = setStreamTokenV3(stream.URL, fresh)
	return true, true
}

// identityReconstructableCardV3 reports whether a session's stream credential
// can be re-minted from its own identity, and returns that card. Only direct
// and remux sessions can: a transcode session's recipe lives in the token, not
// in the Session, so it must keep the verified claims it was issued with.
func (h *PlaybackHandler) identityReconstructableCardV3(session *playback.Session) (playback.RecipeCard, bool) {
	if session == nil {
		return playback.RecipeCard{}, false
	}
	switch session.PlayMethod {
	case playback.PlayDirect, playback.PlayRemux:
		return identityRecipeCard(session), true
	default:
		return playback.RecipeCard{}, false
	}
}

// streamTokenFromURLV3 extracts the native signed-credential query parameter
// from a stream URL. An absent or unparseable URL yields "".
func streamTokenFromURLV3(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Query().Get(streamTokenParam)
}

// setStreamTokenV3 replaces a stream URL's signed-credential query parameter,
// preserving every other query parameter and the URL path.
func setStreamTokenV3(rawURL, token string) string {
	if token == "" {
		return rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return appendStreamToken(rawURL, token)
	}
	query := parsed.Query()
	query.Set(streamTokenParam, token)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// reusedHLSTransportV3 reconstructs transport facts for an existing HLS or
// identity (direct-play/progressive-remux) session.
func reusedHLSTransportV3(session *playback.Session, streamURL string) preparedTransportV3 {
	transport := preparedTransportV3{url: streamURL}
	if session != nil {
		transport.routingWorkload = noderouting.Workload(session.RoutingWorkload)
		transport.routingExecution = noderouting.Execution(session.RoutingExecution)
		transport.routingExecutorID = session.RoutingExecutionNodeID
		transport.routingExecutorURL = session.RoutingExecutionNodeURL
		transport.routingEgress = noderouting.Egress(session.RoutingEgress)
		transport.routingEgressID = session.RoutingEgressNodeID
		transport.routingEgressURL = session.RoutingEgressNodeURL
		transport.nodeURL = session.TranscodeNodeURL
		transport.transportID = session.TranscodeTransportID
		transport.hwAccel = session.TranscodeHWAccel
		transport.toneMapMode = session.ToneMapMode
	}
	transport.commit = func() *transportErrorV3 { return nil }
	transport.rollback = func() {}
	return transport
}

// reusedHLSRouteAllowedV3 checks the running transport against the routing
// snapshot for this replan. Soft preferences do not interrupt healthy bytes,
// but a hard execution/egress boundary or client-incompatible proxy route must
// force normal replacement preparation.
func reusedHLSRouteAllowedV3(session *playback.Session, result playback.PlannerResultV3, policy config.PlaybackRoutingPolicy, proxyAllowed bool) bool {
	workload, delivery, ok := routingClassV3(result)
	if !ok || session == nil {
		return false
	}
	compiled, err := noderouting.Candidates(noderouting.Request{
		Workload: workload, Delivery: delivery, Policy: policy, ProxyAllowed: proxyAllowed,
	})
	if err != nil {
		return false
	}
	for _, shape := range compiled.Candidates {
		if shape.Workload == noderouting.Workload(session.RoutingWorkload) &&
			shape.Execution == noderouting.Execution(session.RoutingExecution) &&
			shape.Egress == noderouting.Egress(session.RoutingEgress) {
			return true
		}
	}
	return false
}

func (h *PlaybackHandler) hasActiveReusableTransportV3(session *playback.Session, delivery playback.DeliveryV3) bool {
	if h == nil || session == nil {
		return false
	}
	if session.TranscodeNodeURL != "" {
		return true
	}
	if h.tm.GetTranscodeSession(session.ID) != nil {
		return true
	}
	if delivery == playback.DeliveryRemuxProgressiveV3 {
		// Local progressive remux is served lazily from /stream/{sessionID}
		// and never holds a transcode-manager session. The committed routing
		// facts are its active-transport evidence; remote/proxy progressive
		// already returned above via TranscodeNodeURL.
		return session.RoutingWorkload == string(noderouting.WorkloadRemux) && session.RoutingExecution != ""
	}
	if delivery == playback.DeliveryOriginalHTTPV3 {
		// Direct HTTP is likewise served from the live session by
		// /stream/{sessionID} and holds no transcode-manager session, so the
		// checks above do not cover it: direct play stores any egress proxy in
		// RoutingEgressNodeURL, never TranscodeNodeURL. Committed routing facts
		// are the active-transport evidence. An uncommitted or reconstructed-old
		// session has an empty execution and must rebuild.
		return session.RoutingWorkload == string(noderouting.WorkloadDirectPlay) && session.RoutingExecution != ""
	}
	return false
}

func applySelectedTracksToStartV3(start *playback.StartRequestV3, selected playback.SelectedTracksV3) {
	if start == nil {
		return
	}
	if selected.Audio != nil {
		start.AudioTrackID = selected.Audio.ID
		start.AudioTrackIndex = copyOptionalIntV3(selected.Audio.Index)
	}
	if selected.Subtitle != nil {
		start.SubtitleTrackID = selected.Subtitle.ID
		start.SubtitleTrackIndex = copyOptionalIntV3(selected.Subtitle.Index)
	} else {
		start.SubtitleTrackID = ""
		start.SubtitleTrackIndex = nil
	}
}

// applySelectedTrackOverridesToStartV3 overlays only identities the caller
// actually sent. Replan bodies are intentionally sparse for every operation
// except track_change, so an omitted subtitle here means "unchanged", not
// "off". The exact-replacement helper above remains the authority for an
// explicit track_change and for reconstructing a durable plan selection.
func applySelectedTrackOverridesToStartV3(start *playback.StartRequestV3, selected playback.SelectedTracksV3) {
	if start == nil {
		return
	}
	if selected.Audio != nil {
		start.AudioTrackID = selected.Audio.ID
		start.AudioTrackIndex = copyOptionalIntV3(selected.Audio.Index)
	}
	if selected.Subtitle != nil {
		start.SubtitleTrackID = selected.Subtitle.ID
		start.SubtitleTrackIndex = copyOptionalIntV3(selected.Subtitle.Index)
	}
}

// audioSelectionDiffersFromStartV3 reports whether the replan's audio
// selection names a track other than the start request's. An omitted audio
// identity means "unchanged" — clients may not resend the current track.
func audioSelectionDiffersFromStartV3(selected playback.SelectedTracksV3, start playback.StartRequestV3) bool {
	return selected.Audio != nil &&
		(selected.Audio.ID != start.AudioTrackID || !optionalIntEqualV3(selected.Audio.Index, start.AudioTrackIndex))
}

// subtitleSelectionDiffersFromStartV3 reports whether the replan's subtitle
// selection differs from the start request's. Unlike audio, a nil subtitle is
// an explicit "subtitles off" and counts as a change when one was selected.
func subtitleSelectionDiffersFromStartV3(selected playback.SelectedTracksV3, start playback.StartRequestV3) bool {
	if selected.Subtitle == nil {
		return start.SubtitleTrackIndex != nil
	}
	return selected.Subtitle.ID != start.SubtitleTrackID || !optionalIntEqualV3(selected.Subtitle.Index, start.SubtitleTrackIndex)
}

func sameSelectedTracksV3(left, right playback.SelectedTracksV3) bool {
	return sameTrackIdentityV3(left.Audio, right.Audio) && sameTrackIdentityV3(left.Subtitle, right.Subtitle)
}

func sameTrackIdentityV3(left, right *playback.TrackIdentityV3) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.ID == right.ID && optionalIntEqualV3(left.Index, right.Index)
}

func copyOptionalIntV3(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// lowerVersionV3 finds the version that serves lower qualities of a 4K
// version when 4K transcoding is disabled: the first non-4K candidate in the
// alternate-version fallback order, the one that fallback adopts when a lower
// quality refuses the 4K source. It reads stored metadata only; the menu is
// built on every start and must not pay for probes. Nil when the policy lets
// the 4K source be transcoded, the request pins its version, or the item has
// no lower version.
func (h *PlaybackHandler) lowerVersionV3(ctx context.Context, req playback.StartRequestV3, base *models.MediaFile, settings playback.PlannerSettingsV3, filter catalog.AccessFilter) *playback.LowerVersionV3 {
	if base == nil || settings.Allow4KTranscode || !settings.TranscodeEnabled || !req.AllowsAlternateVersions() || !playback.Is4KMediaFileV3(base) {
		return nil
	}
	alternates, err := h.findAlternateFiles(ctx, base, filter)
	if err != nil {
		return nil
	}
	for _, alternate := range alternates {
		// A part of a multi-part version falls back only to the same part.
		if base.PresentationPartTotal > 1 && alternate.PresentationPartIndex != base.PresentationPartIndex {
			continue
		}
		if playback.Is4KMediaFileV3(alternate) {
			// Candidates are ordered non-4K first; the rest are 4K too.
			return nil
		}
		return &playback.LowerVersionV3{
			Requested: playback.SourceDescriptorFromFileV3(base, 0),
			Lower:     playback.SourceDescriptorFromFileV3(alternate, 0),
		}
	}
	return nil
}

// lowerVersionForFileV3 is the lower version for a plan of file. When the
// alternate-version fallback plays a lower version other than the first one,
// that plan's menu describes the version it plays, with the probed metadata
// the fallback just read.
func lowerVersionForFileV3(lower *playback.LowerVersionV3, file *models.MediaFile) *playback.LowerVersionV3 {
	if lower == nil || file == nil || playback.Is4KMediaFileV3(file) {
		return lower
	}
	return &playback.LowerVersionV3{Requested: lower.Requested, Lower: playback.SourceDescriptorFromFileV3(file, 0)}
}

func shouldTryAlternateFileV3(qualityPreference string) bool {
	return !strings.EqualFold(strings.TrimSpace(qualityPreference), "original")
}

const (
	terminalNoAlternateVersionV3            = "no_alternate_version"
	terminalHDRTranscodeUnsupportedV3       = playback.TerminalHDRTranscodeUnsupportedV3
	terminalSubtitleConversionUnsupportedV3 = "subtitle_conversion_unsupported"
	terminalSubtitleUnavailableInVersionV3  = "subtitle_unavailable_in_version"
	terminalBitratePolicyUnavailableV3      = playback.TerminalBitratePolicyUnavailableV3
	// sourceMetadataIncompleteReasonV3 is the planner terminal for a source
	// missing video metadata. A version pick (not a subtitle/audio pick)
	// terminaling on it with a fully-characterized bound sibling falls back
	// through the alternate hunt instead of stranding on terminal.
	sourceMetadataIncompleteReasonV3 = "source_metadata_incomplete"
)

// terminalAllowsAlternateFileV3 reports whether a refusal is the kind another
// version of the same item could satisfy.
//
// subtitle_conversion_unsupported is deliberately absent as a direct trigger.
// It is emitted when a subtitle burn-in is the sole trigger of a video
// adaptation the source cannot take (HDR re-encode, 4K policy, transcoding
// disabled) or when a subtitle simply cannot be delivered, and deselecting the
// subtitle restores playback on the same release. A subtitle problem must never
// move the video candidate, so the reason is routed to the in-place subtitle
// degrade first; only when that same-release degrade also fails does the start
// path re-admit the alternate-version failover, because the failure proves the
// release itself is blocked and the refusal is a video/policy one in disguise.
// The genuine video/policy refusals (HDR pipeline, 4K policy without a subtitle
// trigger, a corrupt source) keep their alternate-version failover directly.
//
// audio_conversion_unsupported is deliberately absent for the same reason as the
// subtitle reason: it names an audio codec or track the available executors
// cannot adapt, not a video stream they cannot play. It is routed to the
// in-place audio resolution (another track in the same file, or the planner's
// own transcode/downmix) before any sibling hunt. The decode reason is only
// video-scoped evidence (videoStreamEvidenceV3); an audio decoder/demux failure
// can no longer produce it.
//
// bitrate_policy_unavailable belongs here for the same reason: it replaces the
// 4K and HDR refusals of a version that exceeds the stream's bitrate limit, and
// a lower-bitrate version may fit that limit.
//
// source_unreadable belongs here because the refusal is about one damaged
// file, not the item: a readable version of the same item still plays.
func terminalAllowsAlternateFileV3(terminal *playback.TerminalV3) bool {
	if terminal == nil {
		return false
	}
	switch terminal.Reason {
	case playback.TerminalAudioConversionUnsupportedV3:
		return false
	case terminalNoAlternateVersionV3, terminalHDRTranscodeUnsupportedV3, terminalBitratePolicyUnavailableV3,
		sourceDecodeFailedReasonV3, playback.TerminalSourceUnreadableV3:
		return true
	default:
		return false
	}
}

// subtitleOnlyTerminalV3 reports whether a refusal names a subtitle problem
// rather than a video/policy one. Such a refusal is first resolved on the
// release already mounted by dropping the subtitle (with the drop stated on the
// plan). On a replan the subtitle terminal then stands unchanged, because the
// playing release is known to be playable. On a fresh start, a same-release
// degrade that also fails proves the release itself is blocked, so the start
// path may still fall back to another version as a video/policy refusal. The
// video-failure reasons keep their existing alternate-file failover.
func subtitleOnlyTerminalV3(terminal *playback.TerminalV3) bool {
	if terminal == nil {
		return false
	}
	return terminal.Reason == terminalSubtitleConversionUnsupportedV3
}

// audioOnlyTerminalV3 reports whether a refusal names an audio problem rather
// than a video/policy one. The planner emits audio_conversion_unsupported when
// the selected audio track needs an adaptation no eligible executor can run.
// That is a statement about the audio stream, not the picture, so the refusal
// is first resolved on the release already mounted by selecting another audio
// track in the same file (the planner already chooses an audio transcode or
// downmix during planning). A track change is the viewer's call, so an explicit
// audio pick is never silently replaced. The video/policy reasons keep their
// alternate-file failover. On a fresh start a same-release resolution that also
// fails proves the release itself is blocked, and only then may the start path
// re-admit the version failover.
func audioOnlyTerminalV3(terminal *playback.TerminalV3) bool {
	if terminal == nil {
		return false
	}
	return terminal.Reason == playback.TerminalAudioConversionUnsupportedV3
}

// audioOnlyTransportReasonV3 reports whether a transport failure names an audio
// condition. Such a failure must stay on the release already mounted: it is not
// evidence the video candidate is bad. The audio recipe a node declined to
// confirm is the audio counterpart of the subtitle-local readiness timeout, and
// audio_transcoding_disabled is a policy statement about the audio adaptation,
// never about the picture.
func audioOnlyTransportReasonV3(reason string) bool {
	switch reason {
	case audioAdaptationFailedReasonV3, "audio_transcoding_disabled":
		return true
	default:
		return false
	}
}

// autoFallbackForSession reads a session's negotiated auto-fallback flag. ok is
// false when the manager does not expose it or the session never set it, so the
// caller keeps its own default.
func autoFallbackForSession(sessionMgr SessionManagerInterface, sessionID string) (enabled bool, ok bool) {
	getter, ok := sessionMgr.(interface {
		AutoFallback(sessionID string) (bool, bool)
	})
	if !ok {
		return false, false
	}
	return getter.AutoFallback(sessionID)
}

// resolveReplanAutoFallbackV3 resolves the attempt's effective version-fallback
// policy for a replan. The live session flag wins while it is set (it carries a
// mid-session re-arm); otherwise the durable normalized request is
// authoritative, so a reconstructed session whose in-memory flag was lost with
// the process keeps the negotiated policy instead of reverting to the start
// request.
func resolveReplanAutoFallbackV3(sessionMgr SessionManagerInterface, sessionID string, record *playback.AttemptRecordV3) bool {
	autoFallback := false
	if record != nil {
		autoFallback = record.NormalizedRequest.AllowsAlternateVersions() &&
			record.NormalizedRequest.FileSelection != playback.FileSelectionExplicitV3
	}
	if negotiated, ok := autoFallbackForSession(sessionMgr, sessionID); ok {
		autoFallback = negotiated
	}
	return autoFallback
}

// errAutoFallbackUnsupportedV3 reports a session manager that cannot negotiate
// auto-fallback. The replan refuses rather than applying a policy the session
// would not honor.
var errAutoFallbackUnsupportedV3 = errors.New("session manager does not expose auto-fallback negotiation")

// renegotiateAutoFallbackV3 applies a replan's auto-fallback intent to the live
// session and the durable attempt record. Clients that start on an explicit
// pick and later re-select Auto from the version menu have no start request to
// carry the intent, so the next replan states it here. Recording it on the
// durable normalized request as well is what lets a reconstructed session keep
// the negotiated policy instead of reverting to the start request. A set
// failure is returned to the caller so the replan fails closed: running with a
// policy the session will not honor, or one that will not survive a
// reconstruction, is worse than a retryable failure. An absent field leaves the
// negotiated intent untouched.
func (h *PlaybackHandler) renegotiateAutoFallbackV3(sessionID string, record *playback.AttemptRecordV3, req playback.ReplanRequestV3) error {
	if req.AutoFallback == nil {
		return nil
	}
	setter, ok := h.sessionMgr.(interface {
		SetAutoFallback(sessionID string, enabled bool) error
	})
	if !ok {
		return errAutoFallbackUnsupportedV3
	}
	if err := setter.SetAutoFallback(sessionID, *req.AutoFallback); err != nil {
		return err
	}
	persistAutoFallbackPolicyV3(&record.NormalizedRequest, *req.AutoFallback)
	return nil
}

// autoFallbackRollbackV3 captures the auto-fallback policy in force before a
// replan speculatively applies a new one. The handler applies the new intent to
// the live session before executeReplanV3 so execution reads the same policy the
// client is showing; if execution then fails, the speculative write must be
// undone. Both halves are restored: the live session's boolean and set-bit (an
// unset session must go back to reporting ok=false, not a spurious explicit
// "off"), and the durable normalized request the record carries, so a terminal
// failure does not persist the policy the failed replan never adopted.
type autoFallbackRollbackV3 struct {
	handler   *PlaybackHandler
	sessionID string
	record    *playback.AttemptRecordV3

	enabled bool
	set     bool

	allowAlternate *bool
	fileSelection  playback.FileSelectionV3
}

// captureAutoFallbackRollbackV3 snapshots the live and durable policy so a
// later failure can restore it. Call it before renegotiateAutoFallbackV3.
func (h *PlaybackHandler) captureAutoFallbackRollbackV3(sessionID string, record *playback.AttemptRecordV3) autoFallbackRollbackV3 {
	rollback := autoFallbackRollbackV3{handler: h, sessionID: sessionID, record: record}
	rollback.enabled, rollback.set = autoFallbackForSession(h.sessionMgr, sessionID)
	if record != nil {
		if record.NormalizedRequest.AllowAlternateVersions != nil {
			allow := *record.NormalizedRequest.AllowAlternateVersions
			rollback.allowAlternate = &allow
		}
		rollback.fileSelection = record.NormalizedRequest.FileSelection
	}
	return rollback
}

// restore puts the captured policy back. The live restore tolerates a session
// that vanished while the replan ran (session_expired) — there is nothing left
// to restore on in that case. The durable restore always applies, because the
// record is a local snapshot the caller may still persist.
func (rb autoFallbackRollbackV3) restore() {
	if rb.record != nil {
		rb.record.NormalizedRequest.AllowAlternateVersions = rb.allowAlternate
		rb.record.NormalizedRequest.FileSelection = rb.fileSelection
	}
	restorer, ok := rb.handler.sessionMgr.(interface {
		RestoreAutoFallback(sessionID string, enabled bool, set bool) error
	})
	if !ok {
		return
	}
	if err := restorer.RestoreAutoFallback(rb.sessionID, rb.enabled, rb.set); err != nil && !errors.Is(err, playback.ErrSessionNotFound) {
		slog.Warn("protocol v3 replan auto-fallback rollback failed", "component", "api", "session", rb.sessionID, "error", err)
	}
}

// persistAutoFallbackPolicyV3 materializes a negotiated version-fallback policy
// into the durable normalized request. The reconstruction read derives fallback
// from the request's selection intent, so enabling clears an explicit pin and
// disabling records one; the explicit boolean is written too, so a reader does
// not have to infer it from the selection when a client sends an explicit
// allow_alternate_versions.
func persistAutoFallbackPolicyV3(req *playback.StartRequestV3, enabled bool) {
	if req == nil {
		return
	}
	allow := enabled
	req.AllowAlternateVersions = &allow
	if enabled {
		req.FileSelection = playback.FileSelectionAutoV3
	} else {
		req.FileSelection = playback.FileSelectionExplicitV3
	}
}

// alternateCandidateV3 is a planned alternate version held back while the
// fallback loop looks for a better one.
type alternateCandidateV3 struct {
	file       *models.MediaFile
	request    playback.StartRequestV3
	audioIndex int
	result     playback.PlannerResultV3
	toneMapErr error
}

func replanAllowsAlternateFileV3(operation playback.ReplanOperationV3, qualityPreference string) bool {
	switch operation {
	case playback.ReplanOperationFailureRecoveryV3, playback.ReplanOperationQualityChangeV3, playback.ReplanOperationOutputChangeV3:
		// Quality and output changes can make another version the only viable
		// route. In particular, a bitmap subtitle can require video burn-in
		// that an HDR source cannot support while an SDR alternate can. The
		// subtitle identity is remapped before the alternate is adopted.
		//
		// A track_change deliberately does NOT fall back to another version:
		// the viewer chose a subtitle or audio track from THIS version's
		// dropdown, which is scoped to the currently playing file. If that
		// track cannot be delivered on this version, the replan refuses with a
		// terminal instead of silently swapping the video underneath the
		// selection — a track that "doesn't work" must not change which
		// version is playing. Only a video that itself failed to load
		// (unreadable/corrupted, surfaced as a failure recovery) can move the
		// version. Seek operations stay pinned to the mounted source.
		return shouldTryAlternateFileV3(qualityPreference)
	default:
		return false
	}
}

func replanAlternateFilePinnedByOriginalQualityV3(operation playback.ReplanOperationV3, qualityPreference string) bool {
	if shouldTryAlternateFileV3(qualityPreference) {
		return false
	}
	return operation == playback.ReplanOperationFailureRecoveryV3 || operation == playback.ReplanOperationQualityChangeV3 || operation == playback.ReplanOperationOutputChangeV3
}

func (h *PlaybackHandler) clarifyOriginalQuality4KTerminalV3(ctx context.Context, filter catalog.AccessFilter, terminal *playback.TerminalV3, requestedFile *models.MediaFile, alternateFilePinned bool) {
	if !alternateFilePinned || terminal == nil || terminal.Reason != terminalNoAlternateVersionV3 || terminal.Message != playback.TerminalMessage4KTranscodeDisabledV3 {
		return
	}
	if alternate, err := h.findAlternateFile(ctx, requestedFile, filter); err == nil && alternate != nil && !playback.Is4KMediaFileV3(alternate) {
		terminal.Message = "4K transcoding is disabled and quality 'original' pins the 4K version; a compatible lower-resolution version of this title is available."
	}
}

// hintExplicitSelectionAlternateAvailableV3 appends a version-list hint to a
// terminal when the viewer explicitly picked a version that cannot be delivered
// but another version of the same title could. The explicit pick is never
// silently swapped (the start-path fallback is gated off), so the client is
// pointed at the version list instead. The terminal reason and retryable flag
// are left untouched.
func hintExplicitSelectionAlternateAvailableV3(terminal *playback.TerminalV3, fileSelection playback.FileSelectionV3) {
	if terminal == nil || fileSelection != playback.FileSelectionExplicitV3 || !terminalAllowsAlternateFileV3(terminal) {
		return
	}
	if terminal.Message != "" {
		terminal.Message += " "
	}
	terminal.Message += "A compatible version of this title is available; choose it from the version list."
}

func (h *PlaybackHandler) lockReplanV3(sessionID string) func() {
	h.v3ReplanMu.Lock()
	if h.v3ReplanLocks == nil {
		h.v3ReplanLocks = make(map[string]*v3ReplanLock)
	}
	entry := h.v3ReplanLocks[sessionID]
	if entry == nil {
		entry = &v3ReplanLock{}
		h.v3ReplanLocks[sessionID] = entry
	}
	entry.refs++
	h.v3ReplanMu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		h.v3ReplanMu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(h.v3ReplanLocks, sessionID)
		}
		h.v3ReplanMu.Unlock()
	}
}

// maxConcurrentReplansV3 bounds simultaneous replan executions. Each replan
// pins one pooled DB connection for its advisory session lock while issuing
// further store queries from the same pool; without a bound, a recovery storm
// (a transcode node dying with dozens of active sessions) turns every pool
// connection into a lock holder and the inner queries deadlock against them.
const maxConcurrentReplansV3 = 8

// sessionLockCapacityAdvisorV3 lets a plan store cap replan concurrency below
// the fixed default when its own connection budget is smaller; a pool sized at
// or below the default would otherwise let lock holders starve the inner
// store queries that must complete before any lock is released.
type sessionLockCapacityAdvisorV3 interface {
	SessionLockCapacity() int
}

// acquireReplanSlotV3 blocks until a replan slot frees or the request context
// is cancelled; excess replans queue here holding no DB resources at all.
func (h *PlaybackHandler) acquireReplanSlotV3(ctx context.Context) (func(), error) {
	h.v3ReplanSlotsOnce.Do(func() {
		capacity := maxConcurrentReplansV3
		if advisor, ok := h.PlanStoreV3.(sessionLockCapacityAdvisorV3); ok {
			if advised := advisor.SessionLockCapacity(); advised > 0 && advised < capacity {
				capacity = advised
			}
		}
		h.v3ReplanSlots = make(chan struct{}, capacity)
	})
	select {
	case h.v3ReplanSlots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-h.v3ReplanSlots }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (h *PlaybackHandler) HandlePlaybackRouteEventV3(w http.ResponseWriter, r *http.Request) {
	userID := apimw.GetUserID(r.Context())
	profileID := apimw.GetProfileID(r.Context())
	if userID == 0 || profileID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication and profile are required")
		return
	}
	body, err := readBoundedV3Body(w, r, maxPlaybackV3EventBodyBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid event body")
		return
	}
	var event playback.RouteEventV3
	if err := json.Unmarshal(body, &event); err != nil || !validRouteEventV3(event) {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid route event")
		return
	}
	// The rate limiter runs before the ownership lookup so the per-minute
	// budget bounds the store reads as well as the writes.
	if !h.allowRouteEventV3(userID, event.PlaybackAttemptID) {
		writeError(w, http.StatusTooManyRequests, "event_rate_limited", "Playback route event rate exceeded")
		return
	}
	var identity *playback.AttemptIdentityV3
	var identityErr error
	if event.SessionID != "" {
		identity, identityErr = h.PlanStoreV3.GetAttemptIdentity(r.Context(), event.SessionID)
	} else {
		identity, identityErr = h.PlanStoreV3.GetAttemptIdentityByPlaybackAttemptID(r.Context(), event.PlaybackAttemptID)
	}
	if identityErr != nil {
		// A store outage is not an ownership violation; keep 403 for genuine
		// mismatches so clients stop sending events for foreign sessions.
		if !errors.Is(identityErr, playback.ErrSessionNotFound) {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to authorize the route event")
			return
		}
		writeError(w, http.StatusForbidden, "forbidden", "Route event does not belong to this profile")
		return
	}
	if identity.UserID != userID || identity.ProfileID != profileID ||
		(event.SessionID != "" && identity.PlaybackAttemptID != event.PlaybackAttemptID) ||
		(identity.SessionID == "" && !terminalStartRouteEventV3(event)) {
		writeError(w, http.StatusForbidden, "forbidden", "Route event does not belong to this profile")
		return
	}
	event.Diagnostics = sanitizeDiagnosticsV3(event.Diagnostics)
	client := h.playbackClientInfoWithSessionFallbackV3(firstNonEmptyValue(event.SessionID, identity.SessionID), playbackClientInfoFromRequest(r))
	h.enqueueRouteEventV3(playback.RouteEventRecordV3{RouteEventV3: event, UserID: userID, ProfileID: profileID, ClientName: client.Name, ClientVersion: client.Version, ClientBuild: client.Build, ClientChannel: client.Channel, ClientModel: event.Diagnostics["device_model"]})
	w.WriteHeader(http.StatusAccepted)
}

func terminalStartRouteEventV3(event playback.RouteEventV3) bool {
	return event.Event == playback.RouteEventTerminalV3 &&
		event.SessionID == "" && event.PlanID == "" &&
		event.PlanAttemptID == "" && event.PlanAttemptKey == ""
}

// StartV3Maintenance expires cached signed responses and old telemetry on the
// application lifecycle rather than on latency-sensitive playback requests.
func (h *PlaybackHandler) StartV3Maintenance(ctx context.Context) {
	if h == nil || h.PlanStoreV3 == nil || ctx == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if _, err := h.PlanStoreV3.CleanupExpired(cleanupCtx, now); err != nil {
					slog.Warn("playback v3 cleanup failed", "error", err)
				}
				cancel()
			}
		}
	}()
}

func (h *PlaybackHandler) allowRouteEventV3(userID int, attemptID string) bool {
	attemptKey := fmt.Sprintf("attempt:%d:%s", userID, attemptID)
	userKey := fmt.Sprintf("user:%d", userID)
	now := time.Now()
	h.v3EventRateMu.Lock()
	defer h.v3EventRateMu.Unlock()
	if h.v3EventRates == nil {
		h.v3EventRates = make(map[string]v3EventRate)
	}
	attemptEntry := h.v3EventRates[attemptKey]
	if attemptEntry.windowStart.IsZero() || now.Sub(attemptEntry.windowStart) >= time.Minute {
		attemptEntry = v3EventRate{windowStart: now}
	}
	userEntry := h.v3EventRates[userKey]
	if userEntry.windowStart.IsZero() || now.Sub(userEntry.windowStart) >= time.Minute {
		userEntry = v3EventRate{windowStart: now}
	}
	if attemptEntry.count >= 120 || userEntry.count >= 600 {
		return false
	}
	attemptEntry.count++
	userEntry.count++
	h.v3EventRates[attemptKey] = attemptEntry
	h.v3EventRates[userKey] = userEntry
	if len(h.v3EventRates) > 10_000 {
		for candidate, value := range h.v3EventRates {
			if now.Sub(value.windowStart) > 2*time.Minute {
				delete(h.v3EventRates, candidate)
			}
		}
	}
	return true
}

func (h *PlaybackHandler) enqueueRouteEventV3(event playback.RouteEventRecordV3) {
	if h == nil || h.PlanStoreV3 == nil {
		return
	}
	h.v3EventOnce.Do(func() {
		h.v3EventQueue = make(chan playback.RouteEventRecordV3, 512)
		// The store is captured with the queue rather than read per event. The
		// goroutine below outlives the request that started it, so re-reading
		// the field would be an unsynchronized read of handler state — harmless
		// in production, where the router wires PlanStoreV3 once before serving,
		// and a real data race against any caller that replaces it afterwards.
		// Capturing also matches what the queue is: work batched for the store
		// that existed when it was created.
		store := h.PlanStoreV3
		go func() {
			for value := range h.v3EventQueue {
				recordRouteEventV3(store, value)
			}
		}()
	})
	select {
	case h.v3EventQueue <- event:
	default:
		slog.Warn("playback route event dropped", "event", event.Event, "playback_attempt_id", event.PlaybackAttemptID)
	}
}

// recordRouteEventV3 writes one queued route event and feeds the metrics
// derived from it. Only a newly inserted row is observed: a v2 retry after a
// lost 202 reuses its event_id and must not be counted twice.
func recordRouteEventV3(store playback.PlanStoreV3, event playback.RouteEventRecordV3) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	inserted, err := store.RecordRouteEvent(ctx, event)
	if err != nil {
		slog.Warn("playback route event write failed", "error", err, "event", event.Event)
		return
	}
	if inserted {
		playback.ObserveStoredRouteEvent(event)
	}
}

// plannerSettingsV3 reads the live settings used by capability discovery,
// where failures intentionally degrade to an omitted capability in a 200.
func (h *PlaybackHandler) plannerSettingsV3(ctx context.Context) playback.PlannerSettingsV3 {
	settings, _ := h.plannerSettingsV3Result(ctx)
	return settings
}

// plannerSettingsV3Result reads the live settings used for an actual planning
// decision. Callers must not persist a policy terminal when the store is down.
func (h *PlaybackHandler) plannerSettingsV3Result(ctx context.Context) (settings playback.PlannerSettingsV3, err error) {
	cfg := h.playbackConfig()
	settings.TranscodeEnabled = cfg.TranscodeEnabled
	settings.HWAccel = cfg.HWAccel
	viewerTranscodeDisabled := h.viewerTranscodeDisabledV3(ctx)
	defer func() { settings.ViewerTranscodeDisabled = <-viewerTranscodeDisabled }()
	if h.SettingsRepo != nil {
		var values [5]string
		var errs [5]error
		keys := [...]string{
			config.Allow4KTranscodeSettingKey,
			config.PlaybackTranscodeHardwareToneMapSettingKey,
			config.PlaybackTranscodeSoftwareToneMapSettingKey,
			config.PlaybackTranscodeVPPToneMapSettingKey,
			config.PlaybackAllowHEVCEncodingSettingKey,
		}
		var group sync.WaitGroup
		group.Add(len(keys))
		for index, key := range keys {
			go func() {
				defer group.Done()
				values[index], errs[index] = h.SettingsRepo.Get(ctx, key)
			}()
		}
		group.Wait()
		if errs[0] != nil {
			return settings, fmt.Errorf("load 4K transcode setting: %w", errs[0])
		}
		if errs[1] != nil {
			return settings, fmt.Errorf("load hardware tone-map setting: %w", errs[1])
		}
		if errs[2] != nil {
			return settings, fmt.Errorf("load software tone-map setting: %w", errs[2])
		}
		if errs[3] != nil {
			return settings, fmt.Errorf("load VPP tone-map setting: %w", errs[3])
		}
		if errs[4] != nil {
			return settings, fmt.Errorf("load HEVC encoding setting: %w", errs[4])
		}
		settings.Allow4KTranscode = config.AdminSettingEnabled(keys[0], values[0])
		settings.HardwareToneMapEnabled = config.AdminSettingEnabled(keys[1], values[1])
		settings.SoftwareToneMapEnabled = config.AdminSettingEnabled(keys[2], values[2])
		settings.VPPToneMapEnabled = config.AdminSettingEnabled(keys[3], values[3])
		settings.AllowHEVCEncoding = config.AdminSettingEnabled(keys[4], values[4])
	}
	return settings, nil
}

// softwareFallbackAllowedV3 reports whether the live playback policy permits
// an automated software-decode fallback. The value is read live from the
// playback config, so an operator's gpu_only choice takes effect without a
// restart; the empty string is the default "allow".
func (h *PlaybackHandler) softwareFallbackAllowedV3() bool {
	return playback.SoftwareFallbackAllowed(h.playbackConfig().SoftwareFallback)
}

// startupRetryAllowedV3 reports whether the local startup retry may run with
// retryAccel. retryAccel is a CPU decode+encode fallback when it is
// playback.HWAccelNone while the configured accelerator still is a hardware
// one; gpu_only forbids that. A recipe already on HWAccelNone is not falling
// back and keeps its retry. Only VideoToolbox produces the downgrade, and only
// for a non-hardware-tone-map recipe (see playback.StartupRetryHWAccel).
func (h *PlaybackHandler) startupRetryAllowedV3(opts playback.TranscodeOpts, retryAccel string) bool {
	if retryAccel == playback.HWAccelNone && !strings.EqualFold(opts.HWAccel, playback.HWAccelNone) {
		return h.softwareFallbackAllowedV3()
	}
	return true
}

// dropStaleTrackIdentityV3 reports whether trackID is a well-formed selected
// identity of kind bound to a file other than fileID, and logs the discard.
// Virtual candidate rotation replaces media_files rows whenever the provider
// surfaces a new release, so a client replaying a cached selection would
// otherwise fail playback outright; callers drop the selection and fall back to
// the default/preferred-track pipeline instead. A malformed identity or one
// bound to fileID itself is not stale.
func dropStaleTrackIdentityV3(ctx context.Context, kind string, fileID int, trackID string) bool {
	if !playback.StaleTrackIdentityV3(kind, fileID, trackID) {
		return false
	}
	slog.WarnContext(ctx, "stale track identity; discarding selection",
		"component", "playback",
		"kind", kind,
		"sent_track_id", trackID,
		"current_file_id", fileID)
	return true
}

type viewerLimitsReaderV3 interface {
	LimitsForUser(ctx context.Context, userID int) (playback.SessionLimits, error)
}

// viewerTranscodeDisabledV3 looks up, concurrently with the server settings,
// whether the requesting account may start a video transcode. Admission is the
// authority; a failed lookup only leaves the quality ladder advertised.
func (h *PlaybackHandler) viewerTranscodeDisabledV3(ctx context.Context) <-chan bool {
	result := make(chan bool, 1)
	userID := apimw.GetUserID(ctx)
	reader, ok := h.sessionMgr.(viewerLimitsReaderV3)
	if userID <= 0 || !ok {
		result <- false
		return result
	}
	go func() {
		limits, err := reader.LimitsForUser(ctx, userID)
		if err != nil {
			slog.WarnContext(ctx, "load viewer playback limits for planning", "component", "playback", "user_id", userID, "error", err)
		}
		result <- err == nil && limits.TranscodingDisabled
	}()
	return result
}

// dropStaleAudioTrackIdentityV3 reports whether the request's audio track ID
// embeds a file identity that no longer matches file. Callers drop the
// selection and fall back to the preferred-track pipeline instead.
func dropStaleAudioTrackIdentityV3(ctx context.Context, file *models.MediaFile, trackID string) bool {
	if file == nil {
		return false
	}
	return dropStaleTrackIdentityV3(ctx, "audio", file.ID, trackID)
}

// dropStaleRequestTrackIdentitiesV3 drops the start request's selected audio
// and subtitle identities when they name a file other than the requested file,
// logging each discard. It runs before structural validation so a rotated-out
// selection degrades instead of failing the start.
func dropStaleRequestTrackIdentitiesV3(ctx context.Context, req *playback.StartRequestV3) {
	if req == nil {
		return
	}
	if dropStaleTrackIdentityV3(ctx, "audio", req.FileID, req.AudioTrackID) {
		req.AudioTrackID = ""
		req.AudioTrackIndex = nil
	}
	if dropStaleTrackIdentityV3(ctx, "subtitle", req.FileID, req.SubtitleTrackID) {
		req.SubtitleTrackID = ""
		req.SubtitleTrackIndex = nil
	}
}

// resolveV3AudioIndex returns the audio index to play, whether the requested
// selection had to be degraded to get there, and an error only when the request
// itself is malformed.
func resolveV3AudioIndex(file *models.MediaFile, trackID string, fallback *int) (int, bool, error) {
	index := 0
	if trackID != "" {
		fileID, kind, ordinal, ok := playback.ParseTrackIDV3(trackID)
		if !ok || kind != "audio" || file == nil {
			// A malformed identity is a client bug, not a content mismatch.
			// A well-formed identity bound to a different file was already
			// discarded as stale by dropStaleAudioTrackIdentityV3 at the request
			// boundary; anything still mismatched here is a genuine client bug.
			return 0, false, errors.New("selected audio track identity is invalid")
		}
		if fileID != file.ID {
			return 0, false, errors.New("selected audio track identity is invalid")
		}
		index = ordinal
	} else if fallback != nil {
		index = *fallback
	}
	if index < 0 {
		// Request validation rejects this first; a negative index is a client
		// bug, not a track the file lacks.
		return 0, false, errors.New("selected audio track index is invalid")
	}
	if file == nil || len(file.AudioTracks) == 0 {
		return 0, index != 0, nil
	}
	if index < 0 || index >= len(file.AudioTracks) {
		// A carried/restored ordinal can outrun the effective file's track list
		// after a version switch or candidate rotation, and an explicit pick can
		// name a track the effective edition does not have. Both degrade to the
		// file's default track instead of terminalling: playability wins, and the
		// viewer can re-pick from the audio menu.
		fallbackIndex := normalizeAudioTrackIndex(file, index)
		slog.Info("audio selection out of range for effective file; using default track",
			"component", "playback", "file_id", file.ID, "requested_index", index, "resolved_index", fallbackIndex)
		return fallbackIndex, true, nil
	}
	return index, false, nil
}

func remapAudioIndexV3(source, target *models.MediaFile, index int) int {
	if source == nil || target == nil {
		return normalizeAudioTrackIndex(target, index)
	}
	return playback.MatchAudioTrackAcrossVersions(source.AudioTracks, target.AudioTracks, index)
}

// sameSubtitleInventoryV3 reports whether two files describe the same subtitle
// inventory (externals then embedded, including the container track indices the
// combined ordinal is derived from). A same-row virtual candidate rotation
// replaces a catalog row's probed tracks in place, so id equality alone would
// treat a genuinely new release as unchanged and carry a stale ordinal onto it.
func sameSubtitleInventoryV3(a, b *models.MediaFile) bool {
	return playback.SubtitleLayoutsEqualIncludingExternal(
		a.SubtitleTracks, a.ExternalSubtitles, b.SubtitleTracks, b.ExternalSubtitles)
}

// sameMediaInventoryV3 reports whether two files carry identical audio and
// subtitle inventories. It is the "did the effective tracks actually move"
// predicate for a replan: a same-row candidate rotation keeps the file id but
// replaces the probed tracks, so id equality is not a sufficient no-op guard.
func sameMediaInventoryV3(a, b *models.MediaFile) bool {
	if a == nil || b == nil {
		return a == b
	}
	return sameSubtitleInventoryV3(a, b) && playback.AudioLayoutsEqual(a.AudioTracks, b.AudioTracks)
}

// replanRemapSourceV3 returns the file whose inventory a carried selection was
// minted against. For a virtual session the plan-time evidence is the source of
// truth: the loaded catalog row may have been re-probed in place (same id,
// different release) after the selection was made, so only the session's
// captured audio and subtitle inventories describe what the carried ordinal
// actually names. The evidence is used regardless of which candidate it is
// anchored to — unlike serving, where a provenance mismatch means the evidence
// must be discarded, a remap needs exactly the inventory the ordinal was minted
// against. Presence is decided by the evidence-set flag, not by slice length:
// a captured-empty inventory is genuine evidence (the old candidate had no such
// track), and only absent evidence permits falling back to the loaded row.
func (h *PlaybackHandler) replanRemapSourceV3(effectiveFile *models.MediaFile, session *playback.Session) *models.MediaFile {
	if effectiveFile == nil || session == nil || !session.VirtualSubtitleEvidenceSet {
		return effectiveFile
	}
	if !isVirtualPlaybackFile(effectiveFile) {
		return effectiveFile
	}
	evidence := *effectiveFile
	evidence.AudioTracks = session.VirtualAudioTracks
	evidence.SubtitleTracks = session.VirtualSubtitleTracks
	evidence.ExternalSubtitles = session.VirtualExternalSubtitles
	return &evidence
}

// virtualCandidateRotationRecordedV3 reports whether the session recorded a
// virtual candidate move that no plan has re-probed yet. A binding move leaves
// the carried subtitle evidence anchored at the previous candidate
// (VirtualSubtitleEvidenceURI) while clearing VirtualSourceRevision (see
// SetVirtualSource); the loaded effective row still names a virtual candidate.
// In that window the row's inventory may look identical to what the plan
// carried — a provider can rotate to a release with the same track shapes — so
// id/inventory comparison alone must not treat the selection as unchanged. This
// is the signal the fingerprint cannot provide; it fires only on a recorded
// rotation, so an ordinary same-file replan is unaffected.
func virtualCandidateRotationRecordedV3(session *playback.Session, effectiveFile *models.MediaFile) bool {
	if session == nil || effectiveFile == nil || !isVirtualPlaybackFile(effectiveFile) {
		return false
	}
	if session.VirtualSourceRevision != "" || session.VirtualSourceURI == "" {
		return false
	}
	return session.VirtualSubtitleEvidenceSet && !virtualEvidenceMatchesBoundFile(effectiveFile, session)
}

// planVirtualSourceRevisionV3 returns the opaque virtual-source revision the
// plan published, or "" when the plan carries none.
func planVirtualSourceRevisionV3(result playback.PlannerResultV3) string {
	if result.Plan == nil {
		return ""
	}
	return result.Plan.VirtualSourceRevision
}

// remapAudioSelectionV3 rebinds the request's audio selection when the
// effective media file changes. ID-only selections are equally file-bound:
// the stale ID would be rejected against the new file's track list
// downstream, so derive the source index from it and remap like any other.
//
// Identity alone is not enough. A virtual candidate rotation can re-probe the
// same catalog row in place (same id, different underlying release), so the
// guard also compares the audio inventory fingerprint: when it differs, the
// carried ordinal names a different track and must be remapped by
// codec/language family exactly like an edition switch.
func remapAudioSelectionV3(source, target *models.MediaFile, request *playback.StartRequestV3) error {
	if request == nil || source == nil || target == nil {
		return nil
	}
	if source.ID == target.ID && playback.AudioLayoutsEqual(source.AudioTracks, target.AudioTracks) {
		return nil
	}
	if request.AudioTrackIndex == nil {
		if request.AudioTrackID == "" {
			return nil
		}
		_, kind, ordinal, ok := playback.ParseTrackIDV3(request.AudioTrackID)
		if !ok || kind != "audio" {
			return errors.New("selected audio track identity is invalid for the source file")
		}
		// A stale file identity (virtual candidate rotation) still carries a
		// meaningful ordinal; the semantic remap below finds the equivalent
		// track on the target regardless of which row the ID referenced.
		request.AudioTrackIndex = &ordinal
	}
	remapped := remapAudioIndexV3(source, target, *request.AudioTrackIndex)
	request.AudioTrackIndex = &remapped
	request.AudioTrackID = playback.TrackIDV3(target.ID, "audio", remapped)
	return nil
}

// resolveCarriedAudioTrackV3 re-resolves the viewer's audio selection from a
// previous version onto the requested file. The carried identity is file-bound,
// so its ordinal is remapped by codec/language family (MatchAudioTrackAcrossVersions),
// not by raw position, which is unstable across encodes. Falls back (ok=false)
// to the server's preference when the source version is gone or the identity
// cannot be parsed; the caller keeps its already-resolved index, so a stale
// carried selection degrades rather than terminating.
func (h *PlaybackHandler) resolveCarriedAudioTrackV3(ctx context.Context, carriedID string, target *models.MediaFile) (int, bool) {
	srcID, kind, ordinal, ok := playback.ParseTrackIDV3(carriedID)
	if !ok || kind != "audio" || target == nil {
		slog.InfoContext(ctx, "carried audio selection is invalid for the effective file; using resolved track",
			"component", "playback", "carried_track_id", carriedID)
		return 0, false
	}
	if srcID == target.ID {
		// Same file: the ordinal is directly usable (normalize bounds).
		return normalizeAudioTrackIndex(target, ordinal), true
	}
	if h.fileResolver == nil {
		slog.InfoContext(ctx, "carried audio source version is unavailable; using resolved track",
			"component", "playback", "carried_file_id", srcID, "target_file_id", target.ID)
		return 0, false
	}
	source, err := h.fileResolver.GetByID(ctx, srcID)
	if err != nil || source == nil {
		slog.InfoContext(ctx, "carried audio source version could not be loaded; using resolved track",
			"component", "playback", "carried_file_id", srcID, "target_file_id", target.ID)
		return 0, false
	}
	return playback.MatchAudioTrackAcrossVersions(source.AudioTracks, target.AudioTracks, ordinal), true
}

// errSubtitleUnavailableInTargetV3 reports that the target media file has no
// equivalent of the selected subtitle track. It is deliberately non-terminal:
// a caller iterating candidates keeps hunting for one that honors the
// selection and only degrades to subtitles-off once every candidate has been
// tried, so a stale carried selection never terminates playback. Malformed
// subtitle identities use a plain error and still terminate.
var errSubtitleUnavailableInTargetV3 = errors.New("no equivalent subtitle track in the target file version")

func (h *PlaybackHandler) remapSubtitleSelectionV3(ctx context.Context, source, target *models.MediaFile, request *playback.StartRequestV3) (bool, error) {
	if request == nil || source == nil || target == nil {
		return false, nil
	}
	// A same-id pairing is only a no-op when the subtitle inventory is
	// unchanged. A virtual candidate rotation re-probes the catalog row in
	// place (same id, different release), so a fingerprint match — not id
	// equality — decides whether the carried ordinal still names the same
	// track. When the inventory differs, the selection remaps by
	// language/format/forced/HI exactly like an edition switch.
	if source.ID == target.ID && sameSubtitleInventoryV3(source, target) {
		return false, nil
	}
	// A source with no subtitle inventory has nothing to remap FROM. That is
	// either a virtual parent row (virtual://movie/ttNNNN with no result= param)
	// — a catalog placeholder with no probed streams — or captured-empty
	// plan-time evidence, where the candidate genuinely had no subtitles. In
	// both cases any selection the client made names no track on this
	// inventory: clear the stale selection and let the target file's own
	// default apply, rather than reinterpreting the ordinal against the
	// replacement and inventing a selection the viewer never made. Mirrors
	// resolveV3AudioIndex's graceful empty-tracks fallback.
	if len(source.ExternalSubtitles) == 0 && len(source.SubtitleTracks) == 0 {
		noDownloaded := h.SubtitleRepo == nil
		if !noDownloaded {
			downloaded, err := h.SubtitleRepo.ListDownloadedSubtitles(ctx, source.ID)
			if err != nil {
				// A failed lookup says nothing about whether the track exists;
				// dropping the selection here would store subtitles-off for the
				// rest of the session over a transient error.
				return false, fmt.Errorf("load downloaded subtitles: %w", wrapSubtitleStoreErrorV3(err))
			}
			noDownloaded = len(downloaded) == 0
		}
		if noDownloaded {
			request.SubtitleTrackIndex = nil
			request.SubtitleTrackID = ""
			return true, nil
		}
	}
	if request.SubtitleTrackIndex == nil {
		// ID-only selections are equally file-bound: the stale ID would be
		// parsed against the alternate file's track list downstream, so
		// derive the source index from it and remap like any other.
		if request.SubtitleTrackID == "" {
			return false, nil
		}
		_, kind, ordinal, ok := playback.ParseTrackIDV3(request.SubtitleTrackID)
		if !ok || kind != subtitleTrackKindV3 {
			return false, errors.New("selected subtitle track identity is invalid for the source file")
		}
		// Stale identities from candidate rotation still carry a usable
		// ordinal; the language/format matching below finds the equivalent
		// track on the target.
		request.SubtitleTrackIndex = &ordinal
	}
	index := *request.SubtitleTrackIndex
	if index < 0 {
		return false, errors.New("selected subtitle track index is invalid")
	}
	// index is a published combined ordinal. classify maps it to the source
	// segment and segment-local track, and the equivalent target track is
	// translated back to the target's published ordinal before it is written:
	// the target's own published base can differ from its source array sizes
	// once de-duplication suppressed a track, and downloaded entries follow
	// that published base.
	sourceLocation, classified := classifySubtitleIndexV3(source, index)
	if !classified {
		return false, errSubtitleUnavailableInTargetV3
	}
	targetIndex := -1
	// The selection's identity, for the cross-format fallback below. Downloaded
	// subtitles carry no forced/SDH flags, so they only match exactly.
	var wantLanguage, wantTitle string
	var wantForced, wantHearingImpaired bool
	switch sourceLocation.source {
	case playback.SubtitleSourceExternalV3:
		wanted := source.ExternalSubtitles[sourceLocation.offset]
		wantLanguage, wantTitle, wantForced, wantHearingImpaired = wanted.Language, displayedSubtitleTitleV3(wanted.Title, wanted.EmbeddedTitle), wanted.Forced, wanted.HearingImpaired
		for candidateIndex, candidate := range target.ExternalSubtitles {
			if !strings.EqualFold(candidate.Language, wanted.Language) || !strings.EqualFold(candidate.Format, wanted.Format) || candidate.Forced != wanted.Forced || candidate.HearingImpaired != wanted.HearingImpaired {
				continue
			}
			if published, mapped := playback.SubtitleInventoryOwnPublishedIndexV3(target, candidateIndex); mapped {
				targetIndex = published
				break
			}
		}
	case playback.SubtitleSourceEmbeddedV3:
		wanted := source.SubtitleTracks[sourceLocation.offset]
		wantLanguage, wantTitle, wantForced, wantHearingImpaired = wanted.Language, displayedSubtitleTitleV3(wanted.Title, wanted.EmbeddedTitle), wanted.Forced, wanted.HearingImpaired
		for candidateIndex, candidate := range target.SubtitleTracks {
			if !strings.EqualFold(candidate.Language, wanted.Language) || !strings.EqualFold(candidate.Codec, wanted.Codec) || candidate.Forced != wanted.Forced || candidate.HearingImpaired != wanted.HearingImpaired {
				continue
			}
			if published, mapped := playback.SubtitleInventoryOwnPublishedIndexV3(target, len(target.ExternalSubtitles)+candidateIndex); mapped {
				targetIndex = published
				break
			}
		}
	default:
		if h.SubtitleRepo != nil {
			sourceDownloaded, sourceErr := h.SubtitleRepo.ListDownloadedSubtitles(ctx, source.ID)
			targetDownloaded, targetErr := h.SubtitleRepo.ListDownloadedSubtitles(ctx, target.ID)
			if err := errors.Join(sourceErr, targetErr); err != nil {
				// A failed lookup says nothing about whether the track exists;
				// dropping the selection here would store subtitles-off for the
				// rest of the session over a transient error.
				return false, fmt.Errorf("load downloaded subtitles: %w", wrapSubtitleStoreErrorV3(err))
			}
			downloadedIndex := sourceLocation.offset
			if downloadedIndex >= 0 && downloadedIndex < len(sourceDownloaded) {
				wanted := sourceDownloaded[downloadedIndex]
				base := len(playback.BuildSubtitleInventoryV3(target, nil))
				// The stable downloaded-subtitle row id (carried in the plan
				// inventory) is the only reliable identity here. Virtual rows
				// persist release_name as "" (see ReplaceVirtualCandidates and
				// upsertVirtualFileVariant), so two same-language/format
				// downloaded tracks are indistinguishable by ReleaseName and a
				// first-match scan would collapse them onto one target row.
				// Resolve the id first, then fall back to the historical
				// language/format/ReleaseName match for rows that carry no id.
				if wanted.ID > 0 {
					for candidateIndex, candidate := range targetDownloaded {
						if candidate.ID == wanted.ID {
							targetIndex = base + candidateIndex
							break
						}
					}
				}
				if targetIndex < 0 {
					for candidateIndex, candidate := range targetDownloaded {
						if strings.EqualFold(candidate.Language, wanted.Language) && strings.EqualFold(string(candidate.Format), string(wanted.Format)) && strings.EqualFold(candidate.ReleaseName, wanted.ReleaseName) {
							targetIndex = base + candidateIndex
							break
						}
					}
				}
			}
		}
	}
	if targetIndex < 0 && wantLanguage != "" {
		// Editions often carry the same subtitle in different formats (PGS on
		// an HDR remux, SRT on an SDR encode). Keep the viewer's language and
		// forced/SDH variant in whatever format the effective file has.
		targetIndex = subtitleVariantIndexV3(target, wantLanguage, wantTitle, wantForced, wantHearingImpaired)
	}
	if targetIndex < 0 {
		// No equivalent track exists on the target version. Preserve the
		// selection and report a non-terminal miss: a caller iterating
		// candidates keeps hunting for one that can honor it, and only degrades
		// to subtitles-off once every candidate has been tried. Clearing the
		// selection here would accept the first candidate that cannot honor an
		// explicit user choice even when a later version can.
		slog.InfoContext(ctx, "no equivalent subtitle track in effective file; trying other candidates",
			"component", "playback", "source_file_id", source.ID, "target_file_id", target.ID,
			"subtitle_track_index", index)
		return false, errSubtitleUnavailableInTargetV3
	}
	request.SubtitleTrackIndex = &targetIndex
	request.SubtitleTrackID = playback.TrackIDV3(target.ID, "subtitle", targetIndex)
	return false, nil
}

// transportFailureClassificationsV3 are the client failure classifications
// that mean the route itself did not deliver bytes the device could render —
// as opposed to benign signals (quality_changed, track changes) that leave
// the previous route fully eligible. The vocabulary is the client failure
// taxonomy; prod route events name decoder_failure, player_failure, and
// http_failure as the observed direct-play failure classes.
// audio_renderer_error is deliberately excluded: a client-local audio hiccup
// does not indict the video route, and demoting on it would retire the only
// viable delivery for a session that is otherwise direct-playing fine.
// Unclassified failures do NOT demote either: several benign recovery paths
// send no classification, and demoting on those would retire the only viable
// route for a healthy stream.
var transportFailureClassificationsV3 = map[string]bool{
	"decoder_failure": true,
	"decode_error":    true,
	"player_failure":  true,
	"http_failure":    true,
	"parser_failure":  true,
}

// failureClassificationKeyV3 lowercases and trims a client-reported failure
// classification so the transport-failure lookup is spelling-insensitive.
func failureClassificationKeyV3(classification string) string {
	return strings.ToLower(strings.TrimSpace(classification))
}

// failureRecoveryAbandonedDeliveryV3 reports whether this replan is a failure
// recovery (or its seek-scoped variant) that abandoned the current plan's
// delivery with an explicit transport-failure classification — the moment
// the attempt learns, durably, that the delivery failed.
func failureRecoveryAbandonedDeliveryV3(operation playback.ReplanOperationV3, failureClassification string) bool {
	if !failureRecoveryOperationV3(operation) {
		return false
	}
	return transportFailureClassificationsV3[failureClassificationKeyV3(failureClassification)]
}

// failureRecoveryOperationV3 reports whether operation is a failure recovery or
// its seek-scoped variant: a replan that exists because the current route
// failed, as opposed to a user-intent change or an automatic correction.
func failureRecoveryOperationV3(operation playback.ReplanOperationV3) bool {
	return operation == playback.ReplanOperationFailureRecoveryV3 ||
		operation == playback.ReplanOperationSeekFailureRecoveryV3
}

// clientCanceledRecoveryWindow bounds how long after an observed transport
// cancel a failure_recovery that still names the same route is reported as a
// zombie candidate. It only colors the diagnostic: a cancel is never proof the
// session died, so it does not fence the recovery. The recovery path still
// drops a mark a committed successor has superseded.
const clientCanceledRecoveryWindow = 90 * time.Second

// sessionClientCanceledRecentlyV3 reports whether a transport cancel was
// observed for the session inside the diagnostic window. A zero cancel time
// means no cancel was recorded.
func sessionClientCanceledRecentlyV3(canceledAt, now time.Time) bool {
	if canceledAt.IsZero() {
		return false
	}
	age := now.Sub(canceledAt)
	return age >= 0 && age <= clientCanceledRecoveryWindow
}

// planHasVideoEncodeV3 reports whether a plan carries a real video encode.
// Only such a plan has a decode-mode dimension: a copy/remux route cannot
// recover from a decoder failure by changing decode mode.
func planHasVideoEncodeV3(plan playback.PlanV3) bool {
	for _, transformation := range plan.Transformations {
		switch transformation.Name {
		case playback.TransformationVideoToH264V3, playback.TransformationVideoToHEVCV3, playback.TransformationVideoToAV1V3:
			return true
		}
	}
	return false
}

// decodeFailureClassificationV3 reports whether a client failure classification
// indicts the video decoder specifically. Only these defer delivery demotion
// for a pending software-decode variant; other transport failures demote as
// before.
func decodeFailureClassificationV3(classification string) bool {
	switch failureClassificationKeyV3(classification) {
	case "decoder_failure", "decode_error":
		return true
	default:
		return false
	}
}

// corroboratedStartupTimeoutV3 admits a generic startup-timeout failure
// classification into candidate-rotation consideration. A timeout alone
// diagnoses nothing — the player reports it whenever the transport never
// produces, for any reason — so it is only a candidate here; the live
// generation verdict check below corroborates it before anything rotates.
// The production incident classified an undecodable provider stream exactly
// this way (startup_timeout → adaptation_exhausted with no rotation), because
// only explicit decode classes passed the old gate.
func corroboratedStartupTimeoutV3(classification string) bool {
	return failureClassificationKeyV3(classification) == "startup_timeout"
}

// softwareDecodeVariantPendingV3 reports whether the failed plan still has an
// untried software-decode variant of the same server-transcode HLS delivery.
// When one is pending, a transport failure must not demote the whole delivery:
// the decoded error indicts the hardware decoder, not the route. A delivery
// with no decode-mode dimension has no such variant and demotes as before.
func softwareDecodeVariantPendingV3(record *playback.AttemptRecordV3, req playback.ReplanRequestV3) bool {
	if record == nil || record.CurrentPlan.Delivery != playback.DeliveryTranscodeHLSV3 {
		return false
	}
	if record.CurrentPlan.EffectiveRecipe.SoftwareVideoDecode || !planHasVideoEncodeV3(record.CurrentPlan) {
		return false
	}
	softwarePlan := record.CurrentPlan
	softwarePlan.EffectiveRecipe.SoftwareVideoDecode = true
	// The durable plan key is always computed with no local mutations (see
	// finalizePlanIdentityV3); a software variant that already became the
	// current plan carries that same key, so compare against it directly.
	key := playback.PlanAttemptKeyV3(softwarePlan, record.NormalizedRequest.ClientPlaybackContext.Output.OutputContextID, nil)
	attempted := append([]string(nil), req.AttemptedPlanKeys...)
	if !containsStringExactV3(attempted, req.PlanAttemptKey) {
		attempted = append(attempted, req.PlanAttemptKey)
	}
	return !containsStringExactV3(attempted, key)
}

// softwareDecodeRetryPendingV3 reports whether a decode failure on the current
// plan should defer delivery demotion for a software-decode retry. It requires
// both the structural variant and the live decoder verdict, so a remote or
// undetected decode failure still demotes as before.
func (h *PlaybackHandler) softwareDecodeRetryPendingV3(record *playback.AttemptRecordV3, req playback.ReplanRequestV3) bool {
	// gpu_only means there is no pending software retry to protect, so the
	// delivery demotes like any other transport failure instead of being held
	// open for a CPU attempt that policy forbids.
	if !h.softwareFallbackAllowedV3() || !softwareDecodeVariantPendingV3(record, req) {
		return false
	}
	ts := h.tm.GetTranscodeSession(record.SessionID)
	return ts != nil && ts.IsDecodeFailed()
}

// virtualCandidateRotationPendingV3 reports whether a decode-classified (or
// corroborated-timeout) failure recovery on a non-explicit virtual selection
// can still rotate to another provider candidate under the same
// server-transcode delivery. When true the delivery must not be demoted:
// rotation, not delivery retirement, is the next hop, and demoting first
// would make the planner abandon the only delivery the replacement candidate
// can use.
//
// The predicate requires the live generation's own decode verdict: a client's
// classification alone never triggers rotation, whether it names the decoder
// or reports a generic startup timeout the verdict corroborates. It is
// otherwise structural and deliberately conservative: it does not list
// candidates (that would put a provider round-trip on the demotion decision).
// If rotation later finds no sibling, executeReplanV3 retires the delivery
// explicitly on the exhausted path. An explicit pin is never a rotation.
func (h *PlaybackHandler) virtualCandidateRotationPendingV3(record *playback.AttemptRecordV3, req playback.ReplanRequestV3) bool {
	if h == nil || record == nil {
		return false
	}
	if req.EffectiveOperation() != playback.ReplanOperationFailureRecoveryV3 {
		return false
	}
	if !decodeFailureClassificationV3(req.Failure.Classification) &&
		!corroboratedStartupTimeoutV3(req.Failure.Classification) {
		return false
	}
	if h.explicitVirtualPinV3(record) {
		return false
	}
	if record.CurrentPlan.Delivery != playback.DeliveryTranscodeHLSV3 || !planHasVideoEncodeV3(record.CurrentPlan) {
		return false
	}
	session, err := h.sessionMgr.GetSession(record.SessionID)
	if err != nil || session == nil || strings.TrimSpace(session.VirtualSourceURI) == "" {
		return false
	}
	// The client's classification alone is not evidence: only a live generation
	// the decoder actually rejected is a candidate failure. This mirrors
	// softwareDecodeRetryPendingV3's live-verdict requirement and prevents a
	// spurious decode_error on a healthy session from rotating or demoting.
	ts := h.tm.GetTranscodeSession(record.SessionID)
	if ts == nil || !ts.IsSourceRejected() {
		return false
	}
	return true
}

// explicitVirtualPinV3 reports whether the attempt's requested release is an
// explicit user version pick, which is never substituted. It is the single
// predicate every rotation door reads, so the decode-rotation demotion hold and
// the rehydration's absent/dead-pin auto-retry cannot disagree about whether a
// release may rotate: an explicit pin protects, an auto selection does not.
func (h *PlaybackHandler) explicitVirtualPinV3(record *playback.AttemptRecordV3) bool {
	if record == nil {
		return false
	}
	return record.NormalizedRequest.FileSelection == playback.FileSelectionExplicitV3
}

// virtualAttemptProviderSourceV3 returns the provider/source scope for a
// session-bound virtual candidate: the provider-neutral URI with the concrete
// result pick removed. It scopes durable exclusions so an identically numbered
// result from another provider or release set cannot suppress this attempt's
// candidate.
func virtualAttemptProviderSourceV3(uri string) string {
	neutral := strings.TrimSpace(virtualPlaybackNeutralKey(uri))
	if neutral != "" {
		return neutral
	}
	return strings.TrimSpace(uri)
}

// recoveryStateStoreV3 returns the durable exclusion capability of the active
// plan store, or nil when the store cannot persist recovery state (a legacy
// in-memory fake). A nil store is not silently tolerated on a confirmed
// rotation: the caller surfaces a controlled failure instead.
func (h *PlaybackHandler) recoveryStateStoreV3() playback.RecoveryStateStoreV3 {
	if h == nil || h.PlanStoreV3 == nil {
		return nil
	}
	store, _ := h.PlanStoreV3.(playback.RecoveryStateStoreV3)
	return store
}

// appendRecoveryExclusionsV3 durably records the confirmed candidate exclusions
// on the attempt, unioning them with the chain the store already holds. It
// retries a losing revision compare against the committed state, so concurrent
// replans converge on the union instead of clobbering each other. It returns
// the first persistence error so the caller can fail the rotation as a
// controlled error rather than publish an untracked candidate.
func (h *PlaybackHandler) appendRecoveryExclusionsV3(ctx context.Context, record *playback.AttemptRecordV3, exclusions []playback.RecoveryExclusionV3) (playback.RecoveryStateV3, error) {
	if record == nil || len(exclusions) == 0 {
		return playback.RecoveryStateV3{}, nil
	}
	store := h.recoveryStateStoreV3()
	if store == nil {
		return playback.RecoveryStateV3{}, fmt.Errorf("recovery state store unavailable")
	}
	revision := record.RecoveryRevision
	var lastErr error
	for attempt := 0; attempt < recoveryAppendMaxAttempts; attempt++ {
		merged, _, err := store.AppendRecoveryExclusions(ctx, record.SessionID, revision, exclusions)
		if err == nil {
			record.RecoveryState = merged
			return merged, nil
		}
		if !errors.Is(err, playback.ErrRecoveryRevisionConflictV3) {
			return playback.RecoveryStateV3{}, err
		}
		// The committed chain advanced after this record was read. Re-read and
		// retry the union against the newer revision.
		lastErr = err
		_, currentRevision, readErr := store.GetRecoveryState(ctx, record.SessionID)
		if readErr != nil {
			return playback.RecoveryStateV3{}, readErr
		}
		revision = currentRevision
	}
	return playback.RecoveryStateV3{}, lastErr
}

// recoveryAppendMaxAttempts bounds the revision-conflict retry. The union is
// monotone and the only concurrent writer is another confirmed verdict for the
// same attempt, so a handful of retries converges; the bound keeps a
// pathological writer loop from spinning a replan.
const recoveryAppendMaxAttempts = 8

// virtualCandidateReleaseIdentityV3 returns the durable release identity for a
// session-bound candidate (provider GUID or video hash), so an exclusion can
// match a renumbered result for the same release.
func virtualCandidateReleaseIdentityV3(file *models.MediaFile) string {
	if file == nil {
		return ""
	}
	if id := strings.TrimSpace(file.ProviderGUID); id != "" {
		return id
	}
	return strings.TrimSpace(file.ProviderVideoHash)
}

// demoteDeliveryCapabilityV3 disables one delivery class in the context's
// capability payload and clears its validated claims (the DV base-layer
// fallback among them), so the planner cannot reselect the delivery for the
// rest of this attempt. Idempotent and nil-safe.
func demoteDeliveryCapabilityV3(request *playback.StartRequestV3, delivery playback.DeliveryV3) {
	if request == nil || delivery == "" {
		return
	}
	class := playback.DeliveryClassV3(delivery)
	if request.ClientPlaybackContext.Deliveries == nil {
		return
	}
	capability, ok := request.ClientPlaybackContext.Deliveries[class]
	if !ok || !capability.Enabled {
		return
	}
	capability.Enabled = false
	capability.SupportedOnDevice = false
	capability.ValidatedClaims = nil
	capability.FailureReason = demoteDeliveryReasonV3
	request.ClientPlaybackContext.Deliveries[class] = capability
	slog.Debug("playback delivery demoted after transport failure",
		"component", "api", "delivery_class", class)
}

// demoteDeliveryReasonV3 is the marker demoteDeliveryCapabilityV3 stamps on a
// capability's FailureReason, distinguishing a server-side route demotion
// from a delivery the client itself advertised as unsupported (also
// Enabled=false). Only the marker form is re-applied across replans.
const demoteDeliveryReasonV3 = "transport_failed_demoted_by_server"

// degradationSoftwareDecodeFallbackV3 marks a plan that deliberately switched
// to CPU video decoding after the hardware decoder rejected the source.
const degradationSoftwareDecodeFallbackV3 = "software_decode_fallback"

// demotedDeliveryClassesV3 lists the delivery classes the durable request
// demotes (server-side demotion marker written by a previous failure
// recovery). The client cannot advertise itself back into a route the server
// demoted: the demotion is server-side evidence, so it must survive the
// client capability overlay every replan performs.
func demotedDeliveryClassesV3(durable playback.StartRequestV3) []string {
	var demoted []string
	for class, capability := range durable.ClientPlaybackContext.Deliveries {
		if strings.EqualFold(strings.TrimSpace(capability.FailureReason), demoteDeliveryReasonV3) {
			demoted = append(demoted, class)
		}
	}
	return demoted
}

// reapplyDeliveryDemotionsV3 writes the durable record's delivery demotions
// onto a freshly overlaid request. Called after the client's capability
// payload replaces the seeded one, so a delivery a previous failure recovery
// demoted stays disabled for the rest of the attempt. The re-application
// re-stamps the demotion marker: the overlay replaced the seeded payload's
// map, and without the stamp the commit would persist an Enabled=false entry
// the next replan can no longer distinguish from a client-advertised
// unsupported delivery — the stickiness would die after one hop.
//
// The re-application is independent of what the client sent this round: an
// incoming entry that exists but is disabled still gets the marker (so the
// commit keeps the server-owned evidence), and a class the client omitted
// entirely gets a synthesized disabled entry — otherwise a successful
// intermediate replan that carried the class absent would erase the marker
// and let a later replan re-enable the delivery that failed.
func reapplyDeliveryDemotionsV3(start *playback.StartRequestV3, durable playback.StartRequestV3) {
	if start == nil {
		return
	}
	if start.ClientPlaybackContext.Deliveries == nil {
		start.ClientPlaybackContext.Deliveries = make(map[string]playback.DeliveryCapabilityV3)
	}
	for _, class := range demotedDeliveryClassesV3(durable) {
		capability, ok := start.ClientPlaybackContext.Deliveries[class]
		if !ok {
			// Omitted this round: synthesize the disabled entry so the
			// demotion marker survives into the commit.
			capability = playback.DeliveryCapabilityV3{}
		}
		capability.Enabled = false
		capability.SupportedOnDevice = false
		capability.ValidatedClaims = nil
		capability.FailureReason = demoteDeliveryReasonV3
		start.ClientPlaybackContext.Deliveries[class] = capability
	}
}

// subtitleVariantIndexV3 returns the combined index of the one deliverable
// external or embedded subtitle in file with the given language and forced/SDH
// flags, whatever its format, or -1. Several such tracks (main dialog and
// commentary, say) are narrowed by title; a match that stays ambiguous is not
// treated as the same selection.
// displayedSubtitleTitleV3 is the title the subtitle inventory shows a
// viewer: the stored title, else the container's embedded one.
func displayedSubtitleTitleV3(title, embeddedTitle string) string {
	if strings.TrimSpace(title) != "" {
		return title
	}
	return embeddedTitle
}

func subtitleVariantIndexV3(file *models.MediaFile, language, title string, forced, hearingImpaired bool) int {
	type candidate struct {
		index int
		title string
	}
	var candidates []candidate
	for i, track := range file.ExternalSubtitles {
		// The policy burns in embedded bitmaps only and serves no external
		// bitmap sidecar, so an external candidate must be text.
		if strings.EqualFold(track.Language, language) && track.Forced == forced && track.HearingImpaired == hearingImpaired && playback.SubtitleFormatDeliverableV3(track.Format) && !playback.NeedsBurnIn(track.Format) {
			candidates = append(candidates, candidate{index: i, title: displayedSubtitleTitleV3(track.Title, track.EmbeddedTitle)})
		}
	}
	for i, track := range file.SubtitleTracks {
		if strings.EqualFold(track.Language, language) && track.Forced == forced && track.HearingImpaired == hearingImpaired && playback.SubtitleFormatDeliverableV3(track.Codec) {
			candidates = append(candidates, candidate{index: len(file.ExternalSubtitles) + i, title: displayedSubtitleTitleV3(track.Title, track.EmbeddedTitle)})
		}
	}
	if len(candidates) == 1 {
		return candidates[0].index
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return -1
	}
	match := -1
	for _, c := range candidates {
		if strings.EqualFold(strings.TrimSpace(c.title), title) {
			if match >= 0 {
				return -1
			}
			match = c.index
		}
	}
	return match
}

func sessionStartErrorV3(err error) *transportErrorV3 {
	switch {
	case errors.Is(err, playback.ErrTooManyStreams), errors.Is(err, playback.ErrTooManyTranscodes):
		return &transportErrorV3{reason: "capacity_unavailable", message: "Playback capacity is currently unavailable.", retryable: true}
	case errors.Is(err, playback.ErrAudioTranscodingDisabled):
		return &transportErrorV3{reason: "audio_transcoding_disabled", message: "The selected audio adaptation is disabled."}
	case errors.Is(err, playback.ErrTranscodingDisabled):
		return &transportErrorV3{reason: "transcoding_disabled", message: "The selected server adaptation is disabled."}
	case errors.Is(err, playback.ErrPlaybackNotAllowed):
		return &transportErrorV3{reason: "policy_denied", message: "Playback is denied by server policy."}
	default:
		return &transportErrorV3{reason: "internal_error", message: "Failed to start the playback session.", cause: err}
	}
}

func (h *PlaybackHandler) persistTerminalStartDecisionV3(ctx context.Context, userID int, profileID string, req playback.StartRequestV3, requestDigests playbackStartRequestDigestsV3, requestedFileID, effectiveFileID int, response playback.DecisionResponseV3) (playback.DecisionResponseV3, error) {
	record := playback.AttemptRecordV3{
		PlaybackAttemptID:    req.PlaybackAttemptID,
		UserID:               userID,
		ProfileID:            profileID,
		RequestedMediaFileID: requestedFileID,
		EffectiveMediaFileID: effectiveFileID,
		NormalizedRequest:    req,
		ServerBitrateCapKbps: serverBitrateCapV3(ctx),
		StartResponse:        response,
		RequestDigest:        requestDigests.current,
		ExpiresAt:            time.Now().Add(playback.MaxTokenTTL),
	}
	if err := h.PlanStoreV3.SaveAttempt(ctx, record); err == nil {
		return response, nil
	} else if !errors.Is(err, playback.ErrPlaybackAttemptExistsV3) && !errors.Is(err, playback.ErrIdempotencyKeyReusedV3) {
		return playback.DecisionResponseV3{}, err
	}

	existing, err := h.PlanStoreV3.GetAttemptByPlaybackAttemptID(ctx, req.PlaybackAttemptID)
	if err != nil {
		return playback.DecisionResponseV3{}, err
	}
	if existing.UserID != userID || existing.ProfileID != profileID ||
		existing.RequestedMediaFileID != requestedFileID || !requestDigests.matches(existing.RequestDigest) {
		return playback.DecisionResponseV3{}, playback.ErrIdempotencyKeyReusedV3
	}
	existingResponse := decisionResponseFromAttemptV3(existing)
	// A terminal publishes no subtitle URLs and no playable plan, so it replays
	// on either surface; the surface guard protects only a playable plan. This
	// is the terminal-save collision: a concurrent duplicate start lost the
	// SaveAttempt CAS, re-read a winner whose plan may have negotiated the
	// deferred track-inventory lifecycle (or original SRT), and must not hand
	// that plan back through the other surface. Without this guard the losing
	// request's terminal save would replay the winner's playable plan —
	// tracks_pending and all — through a surface that never negotiated it.
	if existingResponse.Terminal == nil {
		if err := requireAttemptAPISurfaceV3(ctx, existing, req.ClientFeatures); err != nil {
			return playback.DecisionResponseV3{}, err
		}
	}
	// A publication failure can leave a durable attempt whose session was
	// aborted. A terminal-save collision must not resurrect that playable plan.
	if existing.SessionID != "" {
		if _, err := h.sessionMgr.GetSession(existing.SessionID); err != nil {
			return playback.NewTerminalResponseV3("session_expired", "The playback session for this attempt has ended.", true), nil //nolint:nilerr // Expiration is a successful terminal protocol response.
		}
	}
	return existingResponse, nil
}

func (h *PlaybackHandler) startFailureDecisionV3(ctx context.Context, userID int, profileID string, req playback.StartRequestV3, requestDigests playbackStartRequestDigestsV3, requestedFileID, effectiveFileID int, failure *transportErrorV3) (playback.DecisionResponseV3, error) {
	response := playback.NewTerminalResponseV3(failure.reason, failure.message, failure.retryable)
	return h.persistTerminalStartDecisionV3(ctx, userID, profileID, req, requestDigests, requestedFileID, effectiveFileID, response)
}

func writeStartAttemptPersistenceErrorV3(w http.ResponseWriter, err error) {
	if errors.Is(err, playback.ErrIdempotencyKeyReusedV3) {
		writeError(w, http.StatusConflict, "playback_attempt_reused", "The playback attempt ID belongs to a different request")
		return
	}
	writeError(w, http.StatusInternalServerError, "internal_error", "Failed to persist the playback decision")
}

func decisionResponseFromAttemptV3(record *playback.AttemptRecordV3) playback.DecisionResponseV3 {
	if record == nil {
		return playback.DecisionResponseV3{}
	}
	if record.StartResponse.Outcome != "" || record.StartResponse.Terminal != nil || record.StartResponse.PlaybackPlan != nil {
		return normalizeDecisionResponseV3(record.StartResponse)
	}
	plan := record.CurrentPlan
	if plan.AppliedQuirks == nil {
		plan.AppliedQuirks = []playback.AppliedQuirkV3{}
	}
	if plan.RuntimeCorrections == nil {
		plan.RuntimeCorrections = []string{}
	}
	return normalizeDecisionResponseV3(playback.DecisionResponseV3{ProtocolVersion: playback.ProtocolV3, ServerFeatures: playback.ServerFeaturesV3(), Outcome: playback.OutcomePlayableV3, SessionID: record.SessionID, PlaybackPlan: &plan})
}

// startReplayResponseBelongsToAttemptV3 reports whether the decision replayed
// for a durable attempt describes the session that attempt owns. A response
// whose SessionID or plan SessionID names another session must never be
// replayed as this attempt's plan. The replan replay path guards the same
// invariant in completedReplanResponseMatchesAttemptV3.
func startReplayResponseBelongsToAttemptV3(response playback.DecisionResponseV3, record *playback.AttemptRecordV3) bool {
	if record == nil || record.SessionID == "" {
		return false
	}
	if response.SessionID != record.SessionID {
		return false
	}
	// A playable response carries a plan; mirror the replan guard's strict
	// equality rather than tolerating an empty plan session.
	return response.PlaybackPlan == nil || response.PlaybackPlan.SessionID == record.SessionID
}

func normalizeDecisionResponseV3(response playback.DecisionResponseV3) playback.DecisionResponseV3 {
	if response.ServerFeatures == nil {
		response.ServerFeatures = playback.ServerFeaturesV3()
	}
	if response.PlaybackPlan == nil {
		return response
	}
	plan := response.PlaybackPlan
	if plan.Stream.Headers == nil {
		plan.Stream.Headers = map[string]string{}
	}
	if plan.Transformations == nil {
		plan.Transformations = []playback.TransformationV3{}
	}
	if plan.AppliedQuirks == nil {
		plan.AppliedQuirks = []playback.AppliedQuirkV3{}
	}
	if plan.RuntimeCorrections == nil {
		plan.RuntimeCorrections = []string{}
	}
	if plan.AvailableQualities == nil {
		plan.AvailableQualities = []playback.AvailableQualityV3{}
	}
	if plan.DegradationWarnings == nil {
		plan.DegradationWarnings = []playback.DegradationWarningV3{}
	}
	if plan.Subtitle.Inventory == nil {
		plan.Subtitle.Inventory = []playback.SubtitleInventoryItemV3{}
	}
	return response
}

func completedReplanResponseMatchesAttemptV3(raw json.RawMessage, record *playback.AttemptRecordV3) bool {
	if record == nil {
		return false
	}
	var response playback.DecisionResponseV3
	if len(raw) == 0 || json.Unmarshal(raw, &response) != nil {
		return false
	}
	if response.PlaybackPlan == nil {
		// Terminal responses deliberately leave the attempt plan untouched. Their
		// freshness is carried by CurrentReplanRequestID (and its DB trigger).
		return response.Terminal != nil
	}
	if response.SessionID != record.SessionID || response.PlaybackPlan.SessionID != record.SessionID {
		return false
	}
	candidate, candidateErr := json.Marshal(response.PlaybackPlan)
	current, currentErr := json.Marshal(record.CurrentPlan)
	return candidateErr == nil && currentErr == nil && bytes.Equal(candidate, current)
}

func appliedQuirkIDsV3(plan *playback.PlanV3) []string {
	if plan == nil {
		return nil
	}
	result := make([]string, 0, len(plan.AppliedQuirks))
	for _, quirk := range plan.AppliedQuirks {
		result = append(result, quirk.ID)
	}
	return result
}

func appliedQuirkRevisionV3(plan *playback.PlanV3) string {
	if plan == nil || len(plan.AppliedQuirks) == 0 {
		return ""
	}
	return plan.AppliedQuirks[0].RegistryRevision
}

func writeV3FileError(w http.ResponseWriter, err error) {
	if errors.Is(err, catalog.ErrItemNotFound) || errors.Is(err, catalog.ErrEpisodeNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Media file not found")
		return
	}
	writeError(w, http.StatusInternalServerError, "internal_error", "Failed to authorize media file")
}
func readBoundedV3Body(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	return ioReadAllV3(http.MaxBytesReader(w, r.Body, limit))
}
func ioReadAllV3(reader interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buffer bytes.Buffer
	_, err := buffer.ReadFrom(reader)
	return buffer.Bytes(), err
}
func chiURLParamV3(r *http.Request, key string) string { return chi.URLParam(r, key) }
func floatOrZeroHandlerV3(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func intOrZeroHandlerV3(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}
func firstNonEmptyHandlerV3(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func subtitleMIMEV3(format string) string {
	switch strings.ToLower(format) {
	case "ass", "ssa":
		return "text/x-ssa"
	case "srt", "subrip":
		return "application/x-subrip"
	case "pgs", subtitleFormatSUP, subtitleCodecPGSFFmpegV3:
		return "application/octet-stream"
	default:
		return subtitleMIMEVTTV3
	}
}

// forceSubtitleExtensionV3 replaces the URL's sidecar extension. It also drops
// original=1, which only means something on a .srt URL.
func forceSubtitleExtensionV3(rawURL, extension string) string {
	pathPart, query, hasQuery := strings.Cut(rawURL, "?")
	if extension != playback.SubtitleExtSRTV3 {
		query = strings.Join(slices.DeleteFunc(strings.Split(query, "&"), func(field string) bool {
			return field == playback.SubtitleOriginalParamV3+"=1"
		}), "&")
		hasQuery = query != ""
	}
	if slash := strings.LastIndex(pathPart, "/"); slash >= 0 {
		if dot := strings.LastIndex(pathPart[slash+1:], "."); dot >= 0 {
			pathPart = pathPart[:slash+1+dot] + extension
		} else {
			pathPart += extension
		}
	}
	if hasQuery {
		return pathPart + "?" + query
	}
	return pathPart
}

func remuxDVModeForPlanV3(plan *playback.PlanV3) playback.RemuxDVMode {
	if plan == nil {
		return ""
	}
	for _, transformation := range plan.Transformations {
		if transformation.Name == playback.TransformationServerDV7HDR10V3 {
			return playback.RemuxDVStripToHDR10V3
		}
	}
	if plan.Source.DVProfile == 0 {
		return ""
	}
	if plan.Source.DVProfile == 7 {
		// Without the strip transformation a P7 remux would drop the
		// enhancement layer and leave dangling RPUs. A P7 plan claiming Dolby
		// Vision is a client-side transform of the original bytes, so any
		// remux attempt against this session must still be rejected.
		return playback.RemuxDVRejectP7V3
	}
	if plan.Claims.Video.DolbyVision {
		return playback.RemuxDVPreserveV3
	}
	return ""
}

// planDVProfileV3 returns the Dolby Vision profile the plan's probe validated
// (0 when the plan carries none). Stream time re-deriving the profile from the
// catalog row can return 0 for virtual candidates whose video_tracks are
// empty, so the session persists the plan's ground truth.
func planDVProfileV3(plan *playback.PlanV3) int {
	if plan == nil {
		return 0
	}
	return plan.Source.DVProfile
}

// videoBitstreamFilterForPlanV3 returns the copy-mode filter chain a plan's
// server-run DV7 strip needs. It is the current chain whatever recipe version
// the plan names: executors validate that version against what they advertise
// before starting, and a node that predates the chain rejects it, so a plan
// frozen at an older recipe fails rather than copying Dolby Vision unstripped.

// applyDVPinToEffectiveFileV3 overwrites a freshly loaded effective media
// file's Dolby Vision profile with the session's first-verified-wins pin when
// the pin names that same file. A replan re-reads the catalog row and can see
// a drifting dv_profile on bytes that never changed; pinning the planner's
// input keeps route selection stable for the session without touching catalog
// state (the file is a per-request load). The pin never crosses files, so a
// substitution or candidate rotation still plans from the new file's own read.
func applyDVPinToEffectiveFileV3(session *playback.Session, file *models.MediaFile) {
	if session == nil || file == nil {
		return
	}
	playback.ApplyDVPinToFileV3(file, session.DVProfilePin)
}

func videoBitstreamFilterForPlanV3(plan *playback.PlanV3) string {
	if plan == nil {
		return ""
	}
	for _, transformation := range plan.Transformations {
		if transformation.Executor == playback.ExecutorServerV3 && transformation.Name == playback.TransformationServerDV7HDR10V3 {
			return playback.DV7ToHDR10BitstreamFilter
		}
	}
	return ""
}

const playbackVideoCodecHEVC = "hevc"

func videoSampleEntryForPlanV3(plan *playback.PlanV3) string {
	if plan == nil {
		return ""
	}
	if plan.Delivery == playback.DeliveryTranscodeHLSV3 && plan.EffectiveRecipe.VideoCodec == playbackVideoCodecHEVC && plan.EffectiveRecipe.VideoSampleEntry == playback.VideoSampleEntryHVC1 {
		return playback.VideoSampleEntryHVC1
	}
	if plan.Delivery != playback.DeliveryRemuxHLSV3 {
		return ""
	}
	if plan.EffectiveRecipe.VideoSampleEntry != "" {
		return plan.EffectiveRecipe.VideoSampleEntry
	}
	for _, transformation := range plan.Transformations {
		if transformation.Name == playback.TransformationServerDV7HDR10V3 {
			return playback.VideoSampleEntryHVC1
		}
	}
	if plan.EffectiveRecipe.DynamicRange == playback.DynamicRangeDolbyVisionV3 &&
		(plan.Source.DVProfile == 5 || plan.Source.DVProfile == 8) {
		return playback.VideoSampleEntryDVH1
	}
	return ""
}

// dvRPUMemoKeyV3 identifies a catalog file row for the DV RPU verdict memo.
// Keyed on the stable file-row identity rather than the transport URL, which
// rotates per relay registration and defeats the shared probe cache.
type dvRPUMemoKeyV3 struct {
	fileID        int
	size          int64
	mtimeUnixNano int64
}

// lazyDVRPUStrippableV3 defers (and memoizes) the per-source RPU probe so the
// planner only shells out to ffmpeg when a Dolby Vision strip route is
// genuinely on the table; every other start never touches it.
//
// The probe belongs to planning, not to the transport: the plan's HDR10 promise
// and the durable session's RemuxDVMode are both derived from the strip
// decision and are re-read by the restart and audio-switch paths, so
// suppressing the filter downstream would leave those claims describing a
// stream the server is no longer producing.
func (h *PlaybackHandler) lazyDVRPUStrippableV3(ctx context.Context, file *models.MediaFile) func() bool {
	if file == nil || strings.TrimSpace(file.FilePath) == "" || isVirtualPlaybackFile(file) || strings.HasPrefix(strings.ToLower(file.FilePath), "virtual://") {
		return nil
	}
	// The shared DV RPU probe cache keys on bin|inputPath; the transport URL
	// rotates per relay registration, so a sidecar-only replan would miss it
	// and re-probe every time. Memoize on the stable file-row identity instead.
	// A missing mtime or size still falls through to the probe: the memo must
	// never claim a verdict the probe could not have reached.
	var memoKey dvRPUMemoKeyV3
	memoize := false
	if file.FileModifiedAt != nil && file.FileSize > 0 {
		memoKey = dvRPUMemoKeyV3{fileID: file.ID, size: file.FileSize, mtimeUnixNano: file.FileModifiedAt.UnixNano()}
		h.v3DVRPUMu.Lock()
		verdict, ok := h.v3DVRPUVerds[memoKey]
		h.v3DVRPUMu.Unlock()
		if ok {
			return func() bool { return verdict }
		}
		memoize = true
	}
	var once sync.Once
	strippable := true
	return func() bool {
		once.Do(func() {
			strippable = playback.DVRPUStrippable(ctx, h.playbackConfig().FFmpegPath, file.FilePath)
			// DVRPUStrippable returns only a bool, so a transient probe failure
			// (timeout, unreadable mount) is indistinguishable from a real
			// verdict and pins `true` here — the safe default: the strip is
			// attempted, and the transcoder handles failure gracefully.
			if memoize {
				h.v3DVRPUMu.Lock()
				if h.v3DVRPUVerds == nil {
					h.v3DVRPUVerds = make(map[dvRPUMemoKeyV3]bool)
				}
				// Bound the memo: evict a random entry when the map exceeds
				// the cap so it never grows unbounded over the process
				// lifetime. A full LRU is overkill for a probe-verdict cache
				// whose entries are cheap to recompute on miss.
				const dvRPUMemoMax = 256
				if len(h.v3DVRPUVerds) >= dvRPUMemoMax {
					for k := range h.v3DVRPUVerds {
						delete(h.v3DVRPUVerds, k)
						break
					}
				}
				h.v3DVRPUVerds[memoKey] = strippable
				h.v3DVRPUMu.Unlock()
			}
		})
		return strippable
	}
}

func configureHLSTimelineV3(plan *playback.PlanV3, videoCodec string, segmentDuration int, durationSeconds float64) (float64, int) {
	if plan == nil {
		return 0, 0
	}
	requested := plan.Timeline.SourceStartSeconds
	seek := alignedSeekSeconds(requested, segmentDuration, videoCodec)
	startSegment := computeStartSegment(seek, segmentDuration)
	plan.Timeline.SourceStartSeconds = requested
	usesGrowingManifest := !playback.CanGenerateSyntheticManifest(durationSeconds, segmentDuration)
	if usesGrowingManifest {
		// Encoded streams seek to the preceding segment boundary. Preserve the
		// requested sub-segment offset so playback still begins at the exact
		// requested source position. Copy remuxes are configured separately with
		// their probed keyframe origin.
		plan.Timeline.PlayerStartSeconds = max(0, requested-seek)
		plan.Timeline.StreamOriginSeconds = seek
		plan.Timeline.TimelineOffsetSeconds = seek
		windowStart := seek
		plan.Timeline.SeekWindowStartSeconds = &windowStart
		// This transport is served from FFmpeg's live, still-growing playlist
		// (see BuildPlaybackManifest), so the seekable extent is whatever has
		// been produced so far — a value this plan cannot know and could not
		// keep current if it did. Publishing the media runtime here instead
		// made the window look *complete*, which clients read as proof that
		// any target inside it is locally seekable; they then native-seek past
		// the produced head instead of asking for a reanchor. Leaving the end
		// open marks the window incomplete, which with can_seek_anywhere=false
		// routes every seek back through the server.
		//
		// The media runtime is published on source.duration_seconds, which is
		// a fact about the file rather than a claim about this transport.
		plan.Timeline.SeekWindowEndSeconds = nil
		plan.Timeline.CanSeekAnywhere = false
		plan.Timeline.SeekRestoration = "source_position"
	} else {
		plan.Timeline.PlayerStartSeconds = requested
		plan.Timeline.StreamOriginSeconds = 0
		plan.Timeline.TimelineOffsetSeconds = 0
		plan.Timeline.SeekWindowStartSeconds = nil
		plan.Timeline.SeekWindowEndSeconds = nil
		plan.Timeline.CanSeekAnywhere = durationSeconds > 0
		plan.Timeline.SeekRestoration = seekRestorationPlayerV3
	}
	return seek, startSegment
}

var diagnosticKeysV3 = map[string]struct{}{
	"decoder_name": {}, "decoder_init_ms": {}, "first_frame_ms": {},
	"device_model": {}, "requested_quality": {}, "effective_quality": {},
	"pcm_recovery": {}, "retry_outcome": {}, "replan_request_id": {},
	"video_mime": {}, "video_codecs": {}, "video_width": {}, "video_height": {},
	"color_transfer": {}, "color_range": {},
	"error_code": {}, "error_code_name": {}, "error_cause": {},
	"transformation_name": {}, "transformation_version": {}, "transformation_stage": {},
	"input_dv_profile": {}, "output_dv_profile": {}, "rpu_converted_count": {},
	"rpu_failed_count": {}, "el_nal_dropped_count": {}, "sample_count": {},
	"transform_buffer_peak_bytes": {}, "requested_media_file_id": {}, "effective_media_file_id": {},
	"audio_output_mode": {}, "audio_mime": {}, "audio_channels": {}, "audio_decoder_name": {},
	"correction_id": {}, "correction_stage": {},
	"network_transport": {}, "network_metered": {}, "network_validated": {},
	bandwidthEstimateLogKeyV3: {}, linkDownstreamLogKeyV3: {},
	"target_source_position_seconds": {}, "reason": {},
}

func validRouteEventV3(event playback.RouteEventV3) bool {
	if event.ProtocolVersion != playback.ProtocolV3 || len(event.PlaybackAttemptID) < 8 || len(event.PlaybackAttemptID) > 128 || len(event.OutputContextID) > 128 || len(event.SessionID) > 128 || len(event.PlanID) > 128 || len(event.PlanAttemptID) > 128 || len(event.PlanAttemptKey) > 128 || len(event.FailureClassification) > 64 || len(event.FallbackReason) > 64 || len(event.AppliedQuirkIDs) > 16 || len(event.QuirkRegistryRevision) > 128 || len(event.Diagnostics) > 32 {
		return false
	}
	for _, id := range event.AppliedQuirkIDs {
		if len(id) == 0 || len(id) > 128 {
			return false
		}
	}
	return playback.ValidRouteEventNameV3(event.Event)
}
func sanitizeDiagnosticsV3(values map[string]string) map[string]string {
	// Iterate the approved keys, not the client map: map iteration order is
	// random, so a count-limited walk over client keys would keep an
	// arbitrary subset and drop different diagnostics on identical retries.
	result := make(map[string]string)
	for key := range diagnosticKeysV3 {
		value, ok := values[key]
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) > 256 {
			value = value[:256]
		}
		result[key] = value
	}
	return result
}

func containsStringFoldV3(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(value, wanted) {
			return true
		}
	}
	return false
}

// containsStringExactV3 compares attempt keys byte-for-byte: they are
// case-sensitive FNV hex digests, so case-folding would treat distinct keys
// as equal.
func containsStringExactV3(values []string, wanted string) bool {
	wanted = strings.TrimSpace(wanted)
	for _, value := range values {
		if strings.TrimSpace(value) == wanted {
			return true
		}
	}
	return false
}

func optionalIntEqualV3(left, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func optionalFloatEqualV3(left, right *float64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

// LocalTransformationAvailableV3 reports whether this API node's cached
// FFmpeg registry can run the named transformation. Theme conversions use it
// to decide whether the API-executed route is legal.
func (h *PlaybackHandler) LocalTransformationAvailableV3(ctx context.Context, name string) bool {
	if h == nil {
		return false
	}
	registry := h.transformationRegistryV3(ctx)
	return registry != nil && registry.Available(name)
}

func (h *PlaybackHandler) proxyEgressOriginsAvailableV3() bool {
	if h == nil || h.NodePlanner == nil {
		return false
	}
	if h.ProxyGrantStore == nil || !h.ProxyGrantStore.Enabled() {
		return false
	}
	enumerator, ok := h.NodePlanner.(proxyNodeEnumeratorV3)
	return ok && len(enumerator.ProxyNodeURLs()) > 0
}

// sessionProxyReservationReleaserV3 gives back only the proxy half of a node
// reservation, for a start that keeps its transcode node but publishes a URL the
// planned proxy does not serve. Optional: a planner without the method simply
// keeps the whole reservation until it ages out. *nodepool.Planner implements it.
type sessionProxyReservationReleaserV3 interface {
	ReleaseSessionProxy(string)
}

// revokeStaleProxyGrantOnCommitV3 drops the egress grant when an
// authorized-origins attempt commits onto a transport this server serves
// itself. A replan that moves a proxy-served session onto the API origin (or
// onto a local transcode) publishes a URL the proxy has no part in, and the
// surviving grant would keep the proxy authorized to serve the previous recipe
// for the rest of its TTL.
func (h *PlaybackHandler) revokeStaleProxyGrantOnCommitV3(ctx context.Context, sessionID string, mode mediaAuthModeV3, servedByProxy bool) {
	if !mode.proxyEgress || servedByProxy {
		return
	}
	h.deleteProxyGrantV3(ctx, sessionID)
}
