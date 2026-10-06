package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/logredact"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// Playback service seams for the v2 adapter (internal/apiv2/playback.go).
//
// Every operation here runs the same application logic as the frozen v1
// handlers; the seams exist so the typed v2 adapter can call it without an
// http.ResponseWriter. The v2 additions over v1 are the installation check,
// durable per-attempt sequencing of progress and stop on the attempt row
// (playback.ProgressStoreV3), and the stream deny marker written on stop.

// PlaybackCaller carries the authenticated identity and bounded client facts
// of one v2 playback request. InstallationID is the value the client read
// from capabilities; a mutation from a different installation is refused.
type PlaybackCaller struct {
	UserID                                                int
	ProfileID, InstallationID                             string
	DeviceID, DeviceName, Platform                        string
	UserAgent, RemoteAddr                                 string
	ClientName, ClientVersion, ClientBuild, ClientChannel string
}

// PlaybackCapabilitiesView is the v2 capabilities body. State is always
// "available": there is no admission step in front of playback.
type PlaybackCapabilitiesView struct {
	InstallationID, Revision, State string
	Allowed                         bool
	ProtocolVersions                []int
	Features                        []string
	Deliveries                      []playback.DeliveryV3
}

const playbackCapabilityStateAvailable = "available"

// PlaybackOperationError is a typed application failure the v2 adapter maps
// to a problem; the v1 handlers write it with writePlaybackOperationError.
type PlaybackOperationError struct {
	Status        int
	Code, Message string
}

func (e *PlaybackOperationError) Error() string { return e.Message }
func playbackOperationError(status int, code, message string) *PlaybackOperationError {
	return &PlaybackOperationError{Status: status, Code: code, Message: message}
}
func writePlaybackOperationError(w http.ResponseWriter, err error) {
	if e, ok := errors.AsType[*PlaybackOperationError](err); ok {
		writeError(w, e.Status, e.Code, e.Message)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal_error", "Playback operation failed")
}
func playbackFileOperationError(err error) error {
	if errors.Is(err, catalog.ErrItemNotFound) || errors.Is(err, catalog.ErrEpisodeNotFound) {
		return playbackOperationError(http.StatusNotFound, "not_found", "Media file not found")
	}
	return playbackOperationError(http.StatusInternalServerError, "internal_error", "Failed to authorize media file")
}
func playbackPreflightOperationError(err error) error {
	if isPlaybackFileMissing(err) {
		return playbackOperationError(http.StatusNotFound, "not_found", "Source media file is missing")
	}
	return playbackOperationError(http.StatusInternalServerError, "internal_error", "Failed to access source media file")
}
func playbackPersistenceOperationError(err error) error {
	if errors.Is(err, playback.ErrIdempotencyKeyReusedV3) {
		return playbackOperationError(http.StatusConflict, "playback_attempt_reused", "The playback attempt ID belongs to a different request")
	}
	return playbackOperationError(http.StatusInternalServerError, "internal_error", "Failed to persist the playback decision")
}
func playbackSessionNotFoundOperationError() *PlaybackOperationError {
	return playbackOperationError(http.StatusNotFound, "session_not_found", "Playback session not found")
}
func playbackStoreOperationError() *PlaybackOperationError {
	return playbackOperationError(http.StatusServiceUnavailable, "unavailable", "Playback state is temporarily unavailable")
}

// PlaybackProgressCommand is one v2 progress sample. Sequence orders the
// samples of an attempt; a higher sequence wins even when position moves
// backward.
type PlaybackProgressCommand struct {
	Sequence int64   `json:"sequence"`
	Position float64 `json:"position"`
	IsPaused bool    `json:"is_paused"`
}

// PlaybackStopCommand is one v2 stop. StopID is the client-minted identity of
// the stop; the optional final sample (Sequence, Position, IsPaused) is applied
// before the stop writer runs when it is newer than the last progress.
type PlaybackStopCommand struct {
	StopID   string   `json:"stop_id"`
	Sequence int64    `json:"sequence"`
	Position *float64 `json:"position,omitempty"`
	IsPaused bool     `json:"is_paused"`
}

// PlaybackAcceptedProgress is the latest sample the attempt holds after a
// progress or stop call.
type PlaybackAcceptedProgress struct {
	Sequence int64   `json:"sequence"`
	Position float64 `json:"position"`
	IsPaused bool    `json:"is_paused"`
}

// PlaybackMutationView is the v2 progress/stop response body.
type PlaybackMutationView struct {
	Outcome   string                    `json:"outcome"`
	Accepted  *PlaybackAcceptedProgress `json:"accepted,omitempty"`
	StopID    string                    `json:"stop_id,omitempty"`
	HistoryID string                    `json:"history_id,omitempty"`
}

// PlaybackCodeProgressConflict is the error code for an equal sequence with a
// different sample; the v2 adapter maps it to its 409 problem type.
const PlaybackCodeProgressConflict = "progress_conflict"

// Mutation outcomes.
const (
	PlaybackOutcomeApplied     = playback.ProgressAppliedV3
	PlaybackOutcomeReplayed    = playback.ProgressReplayedV3
	PlaybackOutcomeStaleSample = playback.ProgressStaleSampleV3
	PlaybackOutcomeStopped     = "stopped"
)

// PlaybackReplanCommand is the typed v2 replan intent: the v3 wire body plus
// the digest of its canonical encoding, which fingerprints a reused request id.
type PlaybackReplanCommand struct {
	Request playback.ReplanRequestV3
	Digest  string
}

// PlaybackRouteEventCommand is the typed v2 route report: the v3 event plus
// the client-minted identity that makes a retry after a lost 202 a no-op.
type PlaybackRouteEventCommand struct {
	EventID string
	Event   playback.RouteEventV3
}

func (h *PlaybackHandler) validatePlaybackCaller(ctx context.Context, caller PlaybackCaller) error {
	if caller.UserID <= 0 || caller.UserID != apimw.GetUserID(ctx) || caller.ProfileID == "" || caller.ProfileID != apimw.GetProfileID(ctx) {
		return playbackOperationError(http.StatusForbidden, "forbidden", "Playback identity does not match the authenticated profile")
	}
	if h.InstallationID == "" {
		return playbackOperationError(http.StatusConflict, "capability_not_configured", "Playback installation identity is not configured")
	}
	if caller.InstallationID != h.InstallationID {
		return playbackOperationError(http.StatusConflict, "installation_changed", "Playback installation changed; refresh capabilities")
	}
	return nil
}

// PlaybackCapabilities is GET /api/v2/playback/capabilities. The installation
// id is diagnostics.ServerInstanceID, set on the handler at construction.
func (h *PlaybackHandler) PlaybackCapabilities(ctx context.Context, userID int, profileID string) (PlaybackCapabilitiesView, error) {
	view := PlaybackCapabilitiesView{State: playbackCapabilityStateAvailable, Allowed: true, ProtocolVersions: []int{playback.ProtocolV3}, Features: []string{}, Deliveries: []playback.DeliveryV3{}}
	if userID <= 0 || userID != apimw.GetUserID(ctx) || profileID == "" || profileID != apimw.GetProfileID(ctx) {
		return view, playbackOperationError(http.StatusForbidden, "forbidden", "Playback identity does not match the authenticated profile")
	}
	if h.InstallationID == "" {
		return view, playbackOperationError(http.StatusConflict, "capability_not_configured", "Playback installation identity is not configured")
	}
	view.InstallationID = h.InstallationID
	view.Features = append(playback.NativeServerFeaturesV3(), "sequenced_progress_v1", "fixed_media_file_v1", "marker_segments_v1", "trickplay_v1")
	if h.WatchTogetherAvailable {
		view.Features = append(view.Features, "watch_party_source_fallback_v1", "watch_party_coordinator_v1")
	}
	view.Deliveries = []playback.DeliveryV3{playback.DeliveryOriginalHTTPV3, playback.DeliveryRemuxProgressiveV3, playback.DeliveryRemuxHLSV3}
	if h.playbackConfig().TranscodeEnabled {
		view.Deliveries = append(view.Deliveries, playback.DeliveryTranscodeHLSV3)
	}
	capability, _ := json.Marshal(view) // This view contains only JSON-safe scalar values.
	digest := sha256.Sum256(capability)
	view.Revision = hex.EncodeToString(digest[:])
	return view, nil
}

// The application pipeline still uses private request-based routing helpers.
// This request contains only caller facts; it is never dispatched to an HTTP
// handler and never carries credentials, a body stream, or a response writer.
// nativeAPIV2ContextKey marks a request that arrived through /api/v2.
type nativeAPIV2ContextKey struct{}

// WithNativeAPIV2 marks ctx as serving /api/v2. The playback and stream
// handlers are shared with the frozen /api/v1 routes, and contract additions
// made after that freeze (subrip_sidecar_v1) apply only under this mark.
func WithNativeAPIV2(ctx context.Context) context.Context {
	return context.WithValue(ctx, nativeAPIV2ContextKey{}, true)
}

func isNativeAPIV2(ctx context.Context) bool {
	native, _ := ctx.Value(nativeAPIV2ContextKey{}).(bool)
	return native
}

// serverFeaturesForRequestV3 is the feature list a decision advertises on the
// surface the request arrived through. The decision is persisted as the
// attempt's StartResponse, so a /api/v2 decision also records durably that
// this server offered subrip_sidecar_v1 to the attempt.
func serverFeaturesForRequestV3(ctx context.Context) []string {
	if isNativeAPIV2(ctx) {
		return playback.NativeServerFeaturesV3()
	}
	return playback.ServerFeaturesV3()
}

// attemptNegotiatedSubRipV3 reports whether an attempt negotiated original SRT.
// The SRT representation its current plan published decides when there is
// one. With no SRT published yet, the attempt negotiated it only if the client
// sent subrip_sidecar_v1 and a /api/v2 decision of this server offered it. The
// stored token alone is not enough: a server that predates the feature stored
// it verbatim for any client, on either surface, and never offered it.
func attemptNegotiatedSubRipV3(record *playback.AttemptRecordV3) bool {
	if published, original := playback.PublishedSubRipRepresentationV3(record.CurrentPlan.Subtitle.Inventory); published {
		return original
	}
	return playback.HasFeatureV3(record.NormalizedRequest.ClientFeatures, playback.FeatureSubripSidecarV3) &&
		playback.HasFeatureV3(record.StartResponse.ServerFeatures, playback.FeatureSubripSidecarV3)
}

// requireAttemptAPISurfaceV3 keeps an attempt on the API surface that
// negotiated its SRT representation. /api/v1 must not continue an attempt that
// negotiated original SRT: v1 would replay or replan its .srt?original=1 URLs,
// including ones for tracks that appear later, on a route that serves WebVTT
// for them. And a start retried through /api/v2 with subrip_sidecar_v1 must
// not replay an attempt negotiated without it, which would break the feature's
// promise. requested is the retried start's feature list; replans pass nil
// because a replan keeps the negotiated representation anyway. The error
// reuses the existing playback_attempt_reused code so /api/v1 gains no new
// contract.
func requireAttemptAPISurfaceV3(ctx context.Context, record *playback.AttemptRecordV3, requested []string) error {
	if record == nil {
		return nil
	}
	negotiated := attemptNegotiatedSubRipV3(record)
	switch {
	case !isNativeAPIV2(ctx) && negotiated:
		return playbackOperationError(http.StatusConflict, "playback_attempt_reused", "The playback attempt belongs to an /api/v2 session")
	case isNativeAPIV2(ctx) && !negotiated && playback.HasFeatureV3(requested, playback.FeatureSubripSidecarV3):
		return playbackOperationError(http.StatusConflict, "playback_attempt_reused", "The playback attempt was negotiated without subrip_sidecar_v1")
	}
	return nil
}

// deferTrackInventoryNegotiatedV3 reports whether this start may defer its full
// track enumeration past the first-byte commit. Deferral is a new lifecycle
// (the plan carries tracks_pending and the client waits for a follow-up
// inventory_updated push or a poll of inventory_url), so it is scoped to the
// surface and client that negotiated it:
//
//   - the request must arrive through /api/v2: the shared handler also serves
//     the frozen /api/v1 bridge, whose contract must not grow tracks_pending;
//   - the client must advertise deferred_track_inventory_v1 in its
//     client_features. A v2 client that did not advertise it has no promise to
//     handle a provisional menu, so it keeps the pre-#228 synchronous upgrade
//     and never sees tracks_pending.
//
// Both are required; a missing either-way yields the unchanged pre-#228 path
// (the candidate-declared upgrade probe runs in the background).
func (h *PlaybackHandler) deferTrackInventoryNegotiatedV3(r *http.Request, req playback.StartRequestV3) bool {
	if r == nil || !isNativeAPIV2(r.Context()) {
		return false
	}
	return playback.HasFeatureV3(req.ClientFeatures, playback.FeatureDeferredTrackInventoryV3)
}

// attemptNegotiatedDeferTrackInventoryV3 reports whether an attempt negotiated
// the deferred track-inventory lifecycle: the client sent
// deferred_track_inventory_v1 and a decision of this server (the StartResponse)
// offered it. Like attemptNegotiatedSubRipV3, the stored client token alone is
// not enough on a v1 surface, where the server never advertised the feature.
// It is the durable test the deferred failed-notification fan-out uses to
// decide whether a session's client can consume a terminal failed inventory.
func attemptNegotiatedDeferTrackInventoryV3(record *playback.AttemptRecordV3) bool {
	if record == nil {
		return false
	}
	return playback.HasFeatureV3(record.NormalizedRequest.ClientFeatures, playback.FeatureDeferredTrackInventoryV3) &&
		playback.HasFeatureV3(record.StartResponse.ServerFeatures, playback.FeatureDeferredTrackInventoryV3)
}

// replanSubtitleFeaturesV3 returns the client features a replan attaches its
// subtitle artifact with: subrip_sidecar_v1 present exactly when the attempt
// negotiated original SRT. Every replan, not only a seek reanchor, keeps that
// representation for the attempt's lifetime.
func replanSubtitleFeaturesV3(record *playback.AttemptRecordV3, clientFeatures []string) []string {
	if record == nil {
		return clientFeatures
	}
	features := playback.WithoutFeatureV3(clientFeatures, playback.FeatureSubripSidecarV3)
	if attemptNegotiatedSubRipV3(record) {
		features = append(features, playback.FeatureSubripSidecarV3)
	}
	return features
}

// withNativeServerFeaturesV3 advertises the /api/v2-only features on a
// decision the shared start/replan application produced.
func withNativeServerFeaturesV3(response playback.DecisionResponseV3) playback.DecisionResponseV3 {
	if len(response.ServerFeatures) > 0 {
		response.ServerFeatures = playback.NativeServerFeaturesV3()
	}
	return response
}

func playbackCallerRequest(ctx context.Context, caller PlaybackCaller) *http.Request {
	headers := make(http.Header)
	headers.Set(deviceIDHeader, caller.DeviceID)
	headers.Set(deviceNameHeader, caller.DeviceName)
	headers.Set(devicePlatformHeader, caller.Platform)
	headers.Set("User-Agent", caller.UserAgent)
	headers.Set("X-Vio-Client", caller.ClientName)
	headers.Set("X-Vio-Client-Version", caller.ClientVersion)
	headers.Set("X-Vio-Client-Build", caller.ClientBuild)
	headers.Set("X-Vio-Client-Channel", caller.ClientChannel)
	headers.Set("X-Silo-Client", caller.ClientName)
	headers.Set("X-Silo-Client-Version", caller.ClientVersion)
	headers.Set("X-Silo-Client-Build", caller.ClientBuild)
	headers.Set("X-Silo-Client-Channel", caller.ClientChannel)
	return (&http.Request{Header: headers, RemoteAddr: caller.RemoteAddr, URL: &url.URL{}}).WithContext(WithNativeAPIV2(ctx))
}

// playbackCallerSessionRequest is playbackCallerRequest with the routed
// session id, for application seams that read chi.URLParam.
func playbackCallerSessionRequest(ctx context.Context, caller PlaybackCaller, sessionID string) *http.Request {
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("session_id", sessionID)
	return playbackCallerRequest(context.WithValue(ctx, chi.RouteCtxKey, routeCtx), caller)
}

// StartPlaybackV2 is POST /api/v2/playback/start: the v1 start application
// (idempotent on playback_attempt_id + request digest) behind the installation
// check.
func (h *PlaybackHandler) StartPlaybackV2(ctx context.Context, caller PlaybackCaller, request playback.StartRequestV3) (playback.DecisionResponseV3, error) {
	if err := h.validatePlaybackCaller(ctx, caller); err != nil {
		return playback.DecisionResponseV3{}, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusBadRequest, "bad_request", "Invalid playback request")
	}
	response, err := h.startPlaybackApplicationV3(playbackCallerRequest(ctx, caller), body)
	return withNativeServerFeaturesV3(response), err
}

// ApplyProgressV2 is POST /api/v2/playback/{session_id}/progress. The sample
// is sequenced durably on the attempt row (compare-and-set on last_sequence);
// an applied sample is then persisted through the v1 progress writer, using
// the in-memory session when this replica holds it and a session synthesized
// from the attempt row otherwise. Progress never requires the in-memory
// session to exist.
func (h *PlaybackHandler) ApplyProgressV2(ctx context.Context, caller PlaybackCaller, sessionID string, command PlaybackProgressCommand) (PlaybackMutationView, error) {
	if err := h.validatePlaybackCaller(ctx, caller); err != nil {
		return PlaybackMutationView{}, err
	}
	if err := validatePlaybackSessionID(sessionID); err != nil {
		return PlaybackMutationView{}, err
	}
	if command.Sequence <= 0 || command.Position < 0 || math.IsNaN(command.Position) || math.IsInf(command.Position, 0) {
		return PlaybackMutationView{}, playbackOperationError(http.StatusBadRequest, "bad_request", "A positive progress sequence and a finite position are required")
	}
	store, ok := h.PlanStoreV3.(playback.ProgressStoreV3)
	if !ok {
		return PlaybackMutationView{}, playbackOperationError(http.StatusNotImplemented, "capability_unsupported", "Sequenced playback progress is not supported by this plan store")
	}
	record, err := h.ownedAttemptV2(ctx, caller, sessionID)
	if err != nil {
		return PlaybackMutationView{}, err
	}
	if record.StoppedAt != nil {
		return PlaybackMutationView{}, playbackSessionNotFoundOperationError()
	}
	sample := playback.ProgressSampleV3{Sequence: command.Sequence, Position: command.Position, IsPaused: command.IsPaused}
	receipt, err := store.ApplyProgress(ctx, sessionID, sample)
	switch {
	case errors.Is(err, playback.ErrProgressConflictV3):
		return PlaybackMutationView{}, playbackOperationError(http.StatusConflict, PlaybackCodeProgressConflict, "The sequence already has different progress")
	case errors.Is(err, playback.ErrAttemptStoppedV3), errors.Is(err, playback.ErrSessionNotFound):
		return PlaybackMutationView{}, playbackSessionNotFoundOperationError()
	case err != nil:
		return PlaybackMutationView{}, playbackStoreOperationError()
	}
	view := PlaybackMutationView{Outcome: receipt.Outcome, Accepted: acceptedProgressV2(receipt.Accepted)}
	// An applied sample persists. A replayed one (the exact latest sample
	// again) persists too: the client retried because the first reply was
	// lost, which may have been before the side effects ran. The writers
	// are idempotent for an identical position, so redoing them is safe.
	if receipt.Outcome != playback.ProgressAppliedV3 && receipt.Outcome != playback.ProgressReplayedV3 {
		return view, nil
	}
	h.persistProgressV2(ctx, store, record, sessionID, sample)
	return view, nil
}

// maxProgressWritePassesV2 bounds how many times persistProgressV2 rewrites
// the side effects after the row moved under it. Each extra pass needs the
// row to have advanced while the previous write ran, so one is the norm.
const maxProgressWritePassesV2 = 3

// latestAcceptedSampleV2 reads the attempt's latest accepted sample by
// probing the row with sample: a repeat of the latest sample is "replayed",
// a stale one carries the newer sample in Accepted. ok is false when the
// attempt is stopped, gone, or the store failed, and nothing should be
// persisted.
func latestAcceptedSampleV2(ctx context.Context, store playback.ProgressStoreV3, sessionID string, sample playback.ProgressSampleV3) (latest playback.ProgressSampleV3, ok bool) {
	receipt, err := store.ApplyProgress(ctx, sessionID, sample)
	if err != nil {
		return playback.ProgressSampleV3{}, false
	}
	if receipt.Outcome == playback.ProgressStaleSampleV3 && receipt.Accepted != nil {
		return *receipt.Accepted, true
	}
	return sample, true
}

// persistProgressV2 runs the v1 progress side effects and leaves them at the
// attempt's latest accepted sample.
//
// The CAS orders samples on the row, but the writers run after it and the
// user-store write is last-write-wins, so two replicas can finish in the
// opposite order to the row: sequence 1 lands on replica A after sequence 2
// landed on replica B. A process-local lock cannot order that, so the row is
// the guard instead. The writers run for the row's latest sample, then the
// row is read again; if it moved while they ran, the newer sample is written
// on top. Whoever writes last therefore writes the latest: a later sample's
// writer either ran after this caller's final check or was the one that moved
// the row, and it finishes with the same check.
func (h *PlaybackHandler) persistProgressV2(ctx context.Context, store playback.ProgressStoreV3, record *playback.AttemptRecordV3, sessionID string, sample playback.ProgressSampleV3) {
	// Within one replica the passes are serialized per session so two local
	// writers cannot interleave inside a pass.
	unlock := h.progressSideEffectLock(sessionID)
	defer unlock()
	target := sample
	for pass := 0; ; pass++ {
		latest, ok := latestAcceptedSampleV2(ctx, store, sessionID, target)
		if !ok {
			return
		}
		if pass > 0 && latest == target {
			return
		}
		if pass == maxProgressWritePassesV2 {
			slog.WarnContext(ctx, "playback progress side effects trail the attempt row", "component", "api", "session", sessionID, "playback_session_id", sessionID, "sequence", latest.Sequence)
			return
		}
		target = latest
		h.writeProgressSideEffectsV2(ctx, record, sessionID, target)
	}
}

// writeProgressSideEffectsV2 applies one sample to the live session when this
// replica holds it and to the user-store writer either way.
func (h *PlaybackHandler) writeProgressSideEffectsV2(ctx context.Context, record *playback.AttemptRecordV3, sessionID string, sample playback.ProgressSampleV3) {
	session, err := h.sessionMgr.GetSession(sessionID)
	if err == nil && session != nil {
		wasPaused := session.IsPaused
		if err := h.sessionMgr.UpdateProgress(sessionID, sample.Position, sample.IsPaused); err != nil && !errors.Is(err, playback.ErrSessionNotFound) {
			slog.WarnContext(ctx, "failed to update live playback progress", "component", "api", "session", sessionID, "playback_session_id", sessionID, "error", err)
		}
		h.syncSessionsOnPauseChange(ctx, wasPaused, sample.IsPaused)
		if current, getErr := h.sessionMgr.GetSession(sessionID); getErr == nil && current != nil {
			h.persistProgress(ctx, current)
			h.scrobblePauseTransitionV2(ctx, current, wasPaused)
			return
		}
	}
	h.persistProgress(ctx, h.attemptSessionV2(ctx, record, sample.Position, sample.IsPaused))
}

// progressSideEffectLockEntry is one session's progress side-effect lock plus a
// reference count of the callers currently holding or waiting for it.
type progressSideEffectLockEntry struct {
	mu   sync.Mutex
	refs int
}

// progressSideEffectLock serializes per-session handler side effects (v2
// progress persistence and inventory-updated publishes) for one session.
//
// The entry is reference-counted: the first acquire creates it, and the unlock
// closure deletes it only when the last in-flight holder releases. The map is
// therefore bounded by concurrently running progress writers, not by the number
// of sessions the process has served, and an entry is never deleted while any
// caller still holds or is waiting on its mutex — deleting a held mutex would
// let a later caller create a second one for the same session and run side
// effects concurrently, which is exactly what this lock prevents.
func (h *PlaybackHandler) progressSideEffectLock(sessionID string) func() {
	if h == nil || sessionID == "" {
		return func() {}
	}
	h.progressSideEffectLocksMu.Lock()
	if h.progressSideEffectLocks == nil {
		h.progressSideEffectLocks = make(map[string]*progressSideEffectLockEntry)
	}
	entry := h.progressSideEffectLocks[sessionID]
	if entry == nil {
		entry = &progressSideEffectLockEntry{}
		h.progressSideEffectLocks[sessionID] = entry
	}
	entry.refs++
	h.progressSideEffectLocksMu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		h.progressSideEffectLocksMu.Lock()
		entry.refs--
		if entry.refs <= 0 {
			delete(h.progressSideEffectLocks, sessionID)
		}
		h.progressSideEffectLocksMu.Unlock()
	}
}

// forgetProgressSideEffectLock drops the per-session state once the attempt is
// terminal, so a long-lived replica does not retain one entry per historical
// session.
//
// progressSideEffectLocks self-evicts when the last in-flight holder releases
// (see progressSideEffectLock); deleting it here while a writer still holds the
// old mutex would reintroduce the two-mutex race. virtualDeliveryCleared is a
// once-per-session delivery flag with no holder, so it is dropped here: after
// the session is terminal no legitimate segment can need it. A late duplicate
// segment simply records the fenced, idempotent delivery evidence again.
func (h *PlaybackHandler) forgetProgressSideEffectLock(sessionID string) {
	if h == nil || sessionID == "" {
		return
	}
	h.virtualDeliveryCleared.Delete(sessionID)
}

func (h *PlaybackHandler) scrobblePauseTransitionV2(ctx context.Context, sess *playback.Session, wasPaused bool) {
	if sess.DisableProgressPersistence || h.WatchScrobbler == nil || wasPaused == sess.IsPaused {
		return
	}
	file, loadErr := h.loadFileByPreferredID(ctx, requestedMediaFileID(sess), sess.MediaFileID)
	if loadErr != nil || file == nil {
		return
	}
	targetID := playbackProgressTarget(file)
	if targetID == "" {
		return
	}
	event := h.scrobbleEventForSession(ctx, sess, targetID, float64(file.Duration), sess.Position)
	if sess.IsPaused {
		if err := h.WatchScrobbler.ScrobblePause(ctx, event); err != nil {
			slog.WarnContext(ctx, "failed to queue watch provider pause scrobble", "component", "api", "session", sess.ID, "error", err)
		}
	} else if err := h.WatchScrobbler.ScrobbleStart(ctx, event); err != nil {
		slog.WarnContext(ctx, "failed to queue watch provider resume scrobble", "component", "api", "session", sess.ID, "error", err)
	}
}

// StopPlaybackV2 is DELETE /api/v2/playback/{session_id}. The first stop wins
// the compare-and-set on stopped_at: it applies the optional final sample,
// runs the v1 stop/history writer, stops the local session and transcode,
// writes the stream deny marker, and records the receipt on the row. Every
// later stop, with any stop id, replays the stored receipt.
func (h *PlaybackHandler) StopPlaybackV2(ctx context.Context, caller PlaybackCaller, sessionID string, command PlaybackStopCommand) (PlaybackMutationView, error) {
	if err := h.validatePlaybackCaller(ctx, caller); err != nil {
		return PlaybackMutationView{}, err
	}
	if err := validatePlaybackSessionID(sessionID); err != nil {
		return PlaybackMutationView{}, err
	}
	if id, err := uuid.Parse(command.StopID); err != nil || id == uuid.Nil || id.String() != command.StopID {
		return PlaybackMutationView{}, playbackOperationError(http.StatusBadRequest, "bad_request", "A canonical stop_id is required")
	}
	if command.Sequence < 0 || (command.Position == nil) != (command.Sequence == 0) ||
		(command.Position != nil && (*command.Position < 0 || math.IsNaN(*command.Position) || math.IsInf(*command.Position, 0))) {
		return PlaybackMutationView{}, playbackOperationError(http.StatusBadRequest, "bad_request", "Invalid stop identity or final sample")
	}
	store, ok := h.PlanStoreV3.(playback.ProgressStoreV3)
	if !ok {
		return PlaybackMutationView{}, playbackOperationError(http.StatusNotImplemented, "capability_unsupported", "Sequenced playback stop is not supported by this plan store")
	}
	record, err := h.ownedAttemptV2(ctx, caller, sessionID)
	if err != nil {
		return PlaybackMutationView{}, err
	}
	var final *playback.ProgressSampleV3
	if command.Position != nil {
		final = &playback.ProgressSampleV3{Sequence: command.Sequence, Position: *command.Position, IsPaused: command.IsPaused}
	}
	receipt, first, err := store.StopAttempt(ctx, sessionID, command.StopID, final)
	switch {
	case errors.Is(err, playback.ErrInvalidStopIDV3):
		return PlaybackMutationView{}, playbackOperationError(http.StatusBadRequest, "bad_request", "A canonical stop_id is required")
	case errors.Is(err, playback.ErrSessionNotFound):
		return PlaybackMutationView{}, playbackSessionNotFoundOperationError()
	case err != nil:
		return PlaybackMutationView{}, playbackStoreOperationError()
	}
	outcome := PlaybackOutcomeStopped
	if !first {
		outcome = PlaybackOutcomeReplayed
	}
	if !receipt.Finalized {
		// This stop won, or a replay found the winner's side effects
		// unfinished (the winning replica died between the CAS and the
		// writers). The history writer mints a new row per call, so exactly
		// one caller may run it: claim finalization on the row first. A
		// caller that loses the claim replays the receipt as stored.
		receipt = h.finalizeStopV2(ctx, store, record, sessionID, receipt)
	}
	h.forgetProgressSideEffectLock(sessionID)
	return PlaybackMutationView{Outcome: outcome, Accepted: acceptedProgressV2(receipt.Accepted), StopID: receipt.StopID, HistoryID: receipt.HistoryID}, nil
}

// stopFinalizationLease bounds how long a claimed finalization may run before
// another replay may take it over.
const stopFinalizationLease = 30 * time.Second

// finalizeStopV2 claims finalization, runs the stop side effects, and records
// the finalized receipt. When the claim is lost the stored receipt is returned
// unchanged. The request cannot fail from here: the receipt is already durable.
func (h *PlaybackHandler) finalizeStopV2(ctx context.Context, store playback.ProgressStoreV3, record *playback.AttemptRecordV3, sessionID string, receipt playback.StopReceiptV3) playback.StopReceiptV3 {
	claimed, err := store.ClaimStopFinalization(ctx, sessionID, time.Now().Add(stopFinalizationLease))
	if err != nil {
		slog.WarnContext(ctx, "failed to claim playback stop finalization", "component", "api", "session", sessionID, "playback_session_id", sessionID, "error", err)
		return receipt
	}
	if !claimed {
		// Another caller holds the lease. Wait for it to finish, or for its
		// lease to lapse and take over, bounded by the request context. The
		// receipt is re-read each round so the lease judged is the one the
		// store holds now, not the one read before the winner claimed.
		for {
			if replay, _, err := store.StopAttempt(ctx, sessionID, receipt.StopID, nil); err == nil {
				if replay.Finalized {
					return replay
				}
				receipt = replay
			}
			if !receipt.FinalizingUntil.IsZero() && !time.Now().Before(receipt.FinalizingUntil) {
				claimed, err = store.ClaimStopFinalization(ctx, sessionID, time.Now().Add(stopFinalizationLease))
				if err == nil && claimed {
					break
				}
			}
			timer := time.NewTimer(25 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return receipt
			case <-timer.C:
			}
		}
	}
	receipt.HistoryID = h.finishStopV2(ctx, record, sessionID, receipt.Accepted)
	receipt.Finalized = true
	if err := store.RecordStopReceipt(ctx, sessionID, receipt); err != nil {
		slog.WarnContext(ctx, "failed to record playback stop receipt", "component", "api", "session", sessionID, "playback_session_id", sessionID, "error", err)
	}
	return receipt
}

// finishStopV2 runs the v1 stop side effects for the winning stop: the
// session stop + transcode teardown when this replica holds the session (its
// finalizer runs the history writer), the history writer directly from a
// session synthesized from the attempt row otherwise. The deny marker is
// written in both cases. It returns the watch-history id when one was made.
func (h *PlaybackHandler) finishStopV2(ctx context.Context, record *playback.AttemptRecordV3, sessionID string, accepted *playback.ProgressSampleV3) string {
	h.StreamDeny.Deny(ctx, sessionID)
	position, paused := 0.0, false
	if accepted != nil {
		position, paused = accepted.Position, accepted.IsPaused
	}
	if session, err := h.sessionMgr.GetSession(sessionID); err == nil && session != nil {
		if accepted != nil {
			if err := h.sessionMgr.UpdateProgress(sessionID, position, paused); err != nil && !errors.Is(err, playback.ErrSessionNotFound) {
				slog.WarnContext(ctx, "failed to apply final playback position", "component", "api", "session", sessionID, "playback_session_id", sessionID, "error", err)
			}
		}
		result, err := h.stopPlaybackSessionWithResult(ctx, session, true)
		if err == nil {
			return result.HistoryID
		}
		if !errors.Is(err, playback.ErrSessionNotFound) {
			slog.WarnContext(ctx, "failed to stop playback session", "component", "api", "session", sessionID, "playback_session_id", sessionID, "error", err)
			return ""
		}
		// Lost the race with another stop of the live session; fall through to
		// the row-synthesized writer so the final sample is still recorded.
	}
	return h.persistStopAndHistory(ctx, h.attemptSessionV2(ctx, record, position, paused)).HistoryID
}

// attemptSessionV2 synthesizes the session the v1 writers read when this
// replica does not hold the live one. Only the fields the writers consult are
// populated.
func (h *PlaybackHandler) attemptSessionV2(ctx context.Context, record *playback.AttemptRecordV3, position float64, paused bool) *playback.Session {
	session := &playback.Session{
		ID:                   record.SessionID,
		UserID:               record.UserID,
		ProfileID:            record.ProfileID,
		MediaFileID:          record.EffectiveMediaFileID,
		RequestedMediaFileID: record.RequestedMediaFileID,
		Position:             position,
		IsPaused:             paused,
	}
	session.DisableProgressPersistence = record.NormalizedRequest.ProgressPersistence == playback.ProgressPersistenceClientV3
	if !session.DisableProgressPersistence && h.fileResolver != nil {
		if file, err := h.fileResolver.GetByID(ctx, record.EffectiveMediaFileID); err == nil && !sessionOwnsResumeTimelineV3(file) {
			session.DisableProgressPersistence = true
		}
	}
	return session
}

// ownedAttemptV2 loads the live attempt row for sessionID and checks it
// belongs to the caller.
func (h *PlaybackHandler) ownedAttemptV2(ctx context.Context, caller PlaybackCaller, sessionID string) (*playback.AttemptRecordV3, error) {
	record, err := h.PlanStoreV3.GetAttempt(ctx, sessionID)
	if err != nil {
		if errors.Is(err, playback.ErrSessionNotFound) {
			return nil, playbackSessionNotFoundOperationError()
		}
		return nil, playbackStoreOperationError()
	}
	if record.UserID != caller.UserID || record.ProfileID != caller.ProfileID {
		return nil, playbackOperationError(http.StatusForbidden, "forbidden", "Session belongs to another profile")
	}
	return record, nil
}

func acceptedProgressV2(sample *playback.ProgressSampleV3) *PlaybackAcceptedProgress {
	if sample == nil {
		return nil
	}
	return &PlaybackAcceptedProgress{Sequence: sample.Sequence, Position: sample.Position, IsPaused: sample.IsPaused}
}

// ReplanPlaybackV2 is POST /api/v2/playback/{session_id}/replan: the full v1
// replan application (seek, track, quality and output changes).
func (h *PlaybackHandler) ReplanPlaybackV2(ctx context.Context, caller PlaybackCaller, sessionID string, command PlaybackReplanCommand) (playback.DecisionResponseV3, error) {
	if err := h.validatePlaybackCaller(ctx, caller); err != nil {
		return playback.DecisionResponseV3{}, err
	}
	if err := validatePlaybackSessionID(sessionID); err != nil {
		return playback.DecisionResponseV3{}, err
	}
	if command.Request.PlaybackAttemptID == "" || command.Request.ReplanRequestID == "" {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusBadRequest, "bad_request", "Invalid replan request")
	}
	body, err := json.Marshal(command.Request)
	if err != nil {
		return playback.DecisionResponseV3{}, playbackOperationError(http.StatusBadRequest, "bad_request", "Invalid replan request")
	}
	// A stop accepted on any replica ends the attempt; a late recovery or
	// seek must not launch a replacement transport the deny marker will
	// refuse to serve. The store's commit predicate refuses stopped rows too.
	if record, err := h.PlanStoreV3.GetAttempt(ctx, sessionID); err == nil && record != nil && record.StoppedAt != nil {
		return playback.DecisionResponseV3{}, playbackSessionNotFoundOperationError()
	}
	response, err := h.replanPlaybackApplicationV3(playbackCallerSessionRequest(ctx, caller, sessionID), sessionID, body)
	if errors.Is(err, playback.ErrAttemptStoppedV3) {
		return playback.DecisionResponseV3{}, playbackSessionNotFoundOperationError()
	}
	return withNativeServerFeaturesV3(response), err
}

// ReportRouteEventV2 is POST /api/v2/playback/route-events. The event is
// queued and never waits on the store; event_id dedups a retried report.
func (h *PlaybackHandler) ReportRouteEventV2(ctx context.Context, caller PlaybackCaller, command PlaybackRouteEventCommand) error {
	if err := h.validatePlaybackCaller(ctx, caller); err != nil {
		return err
	}
	if id, err := uuid.Parse(command.EventID); err != nil || id == uuid.Nil || id.String() != command.EventID {
		return playbackOperationError(http.StatusBadRequest, "bad_request", "A canonical event_id is required")
	}
	event := command.Event
	if !validRouteEventV3(event) {
		return playbackOperationError(http.StatusBadRequest, "bad_request", "Invalid route event")
	}
	if !h.allowRouteEventV3(caller.UserID, event.PlaybackAttemptID) {
		return playbackOperationError(http.StatusTooManyRequests, "event_rate_limited", "Playback route event rate exceeded")
	}
	var identity *playback.AttemptIdentityV3
	var err error
	if event.SessionID != "" {
		identity, err = h.PlanStoreV3.GetAttemptIdentity(ctx, event.SessionID)
	} else {
		identity, err = h.PlanStoreV3.GetAttemptIdentityByPlaybackAttemptID(ctx, event.PlaybackAttemptID)
	}
	if err != nil {
		if !errors.Is(err, playback.ErrSessionNotFound) {
			return playbackStoreOperationError()
		}
		return playbackOperationError(http.StatusForbidden, "forbidden", "Route event does not belong to this profile")
	}
	if identity.UserID != caller.UserID || identity.ProfileID != caller.ProfileID ||
		(event.SessionID != "" && identity.PlaybackAttemptID != event.PlaybackAttemptID) ||
		(identity.SessionID == "" && !terminalStartRouteEventV3(event)) {
		return playbackOperationError(http.StatusForbidden, "forbidden", "Route event does not belong to this profile")
	}
	event.Diagnostics = sanitizeDiagnosticsV3(event.Diagnostics)
	h.enqueueRouteEventV3(playback.RouteEventRecordV3{RouteEventV3: event, EventID: command.EventID, UserID: caller.UserID, ProfileID: caller.ProfileID, ClientName: caller.ClientName, ClientVersion: caller.ClientVersion, ClientBuild: caller.ClientBuild, ClientChannel: caller.ClientChannel, ClientModel: event.Diagnostics["device_model"]})
	return nil
}

// GetPlaybackInventoryV2 returns the live audio and subtitle track inventory for an active session.
func (h *PlaybackHandler) GetPlaybackInventoryV2(ctx context.Context, caller PlaybackCaller, sessionID string) (playback.PlaybackInventoryV3, error) {
	if err := h.validatePlaybackCaller(ctx, caller); err != nil {
		return playback.PlaybackInventoryV3{}, err
	}
	if err := validatePlaybackSessionID(sessionID); err != nil {
		return playback.PlaybackInventoryV3{}, err
	}
	session, err := h.sessionMgr.GetSession(sessionID)
	if err != nil {
		if errors.Is(err, playback.ErrSessionNotFound) {
			return playback.PlaybackInventoryV3{}, playbackSessionNotFoundOperationError()
		}
		return playback.PlaybackInventoryV3{}, playbackStoreOperationError()
	}
	if session.UserID != caller.UserID || (caller.ProfileID != "" && session.ProfileID != caller.ProfileID) {
		return playback.PlaybackInventoryV3{}, playbackOperationError(http.StatusForbidden, "forbidden", "Session belongs to another profile")
	}
	if h.PlanStoreV3 == nil {
		return playback.PlaybackInventoryV3{}, playbackStoreOperationError()
	}
	record, err := h.PlanStoreV3.GetAttempt(ctx, sessionID)
	if err != nil {
		if errors.Is(err, playback.ErrSessionNotFound) {
			return playback.PlaybackInventoryV3{}, playbackSessionNotFoundOperationError()
		}
		return playback.PlaybackInventoryV3{}, playbackStoreOperationError()
	}
	if record == nil {
		return playback.PlaybackInventoryV3{}, playbackStoreOperationError()
	}
	inventory, invErr := h.playbackInventoryForSession(ctx, session, record)
	if errors.Is(invErr, errPlaybackInventorySubtitlesUnavailable) {
		return playback.PlaybackInventoryV3{}, playbackStoreOperationError()
	}
	return inventory, invErr
}

// errPlaybackInventorySubtitlesUnavailable marks a live-inventory read that
// failed because the subtitle inventory dependency was unavailable. The v2 and
// v1 readers map it to their own established problem shapes; the sentinel keeps
// the shared resolver from picking one.
var errPlaybackInventorySubtitlesUnavailable = errors.New("playback inventory subtitle load failed")

// playbackInventoryForSession resolves the live inventory and the effective
// version identity for an active session. Both the v2 and v3 readers call it so
// they cannot disagree about which release is playing or which tracks it
// publishes.
//
// The session's bound virtual source is authoritative at commit time: a
// serve-layer rotation moves VirtualSourceURI to a live sibling before any plan
// is rebuilt, so the catalog row that names that URI is the effective version
// even when it is not probed yet. Its own declared (or empty) inventory is the
// correct answer for that release, and the previous release's tracks are never
// served under it.
func (h *PlaybackHandler) playbackInventoryForSession(ctx context.Context, session *playback.Session, record *playback.AttemptRecordV3) (playback.PlaybackInventoryV3, error) {
	if h == nil || session == nil {
		return playback.PlaybackInventoryV3{}, playbackSessionNotFoundOperationError()
	}
	if h.fileResolver == nil {
		return playback.PlaybackInventoryV3{}, errors.New("file resolver not configured")
	}
	file, err := h.fileResolver.GetByID(ctx, session.MediaFileID)
	if err != nil || file == nil {
		return playback.PlaybackInventoryV3{}, playbackOperationError(http.StatusNotFound, "not_found", "Media file not found")
	}

	effectiveVirtualURI := ""
	if isVirtualPlaybackFile(file) {
		candidateURI := strings.TrimSpace(session.VirtualSourceURI)
		if candidateURI == "" {
			candidateURI = strings.TrimSpace(file.FilePath)
		}
		effectiveVirtualURI = candidateURI
		file = h.inventoryEffectiveFile(ctx, file, session, candidateURI)
	}

	return h.playbackInventoryForFileV3(ctx, session, record, file, effectiveVirtualURI)
}

// playbackInventoryForFileV3 builds the wire inventory for one session from an
// already-resolved effective file. It is the shared tail of
// playbackInventoryForSession and the in-memory serve for a refused probe: the
// effective file, its identity, its status and its revision are all derived from
// the file argument, so a caller can describe a session's inventory from a
// catalog row or from probed-but-unwritten evidence without the two ever
// disagreeing about the shape.
func (h *PlaybackHandler) playbackInventoryForFileV3(ctx context.Context, session *playback.Session, record *playback.AttemptRecordV3, file *models.MediaFile, effectiveVirtualURI string) (playback.PlaybackInventoryV3, error) {
	if h == nil || session == nil {
		return playback.PlaybackInventoryV3{}, playbackSessionNotFoundOperationError()
	}

	var clientFeatures []string
	if record != nil {
		clientFeatures = replanSubtitleFeaturesV3(record, record.NormalizedRequest.ClientFeatures)
	}

	audioTracks := playback.AudioInventoryV3(file)
	additional, subErr := h.downloadedSubtitleInventoryWithErrorV3(ctx, file)
	if subErr != nil {
		return playback.PlaybackInventoryV3{}, errPlaybackInventorySubtitlesUnavailable
	}
	subtitleInventory := playback.ScopeSubtitleInventoryV3(session.ID, file, playback.BuildSubtitleInventoryV3(file, additional), clientFeatures)

	status := string(ProbeProvenanceDeclared)
	if file != nil && file.ProbeUpdatedAt != nil {
		status = string(ProbeProvenanceVerified)
	} else {
		// No probe stamp on the effective row. A deferred full-track
		// enumeration may be outstanding, may have committed its evidence
		// without a stamp, or may have terminally failed; the session carries
		// that disposition so an inventory reader can tell an unfinished probe
		// from a finished or failed one and leave the loading state either way.
		// Only the verified stamp outranks this, because it is the row's own
		// committed evidence; a verified outcome means the probe's durable write
		// committed, so the client may also stop treating the menu as loading.
		switch session.VirtualProbeOutcome {
		case probeOutcomePending:
			status = probeOutcomePending
		case probeOutcomeVerified:
			status = string(ProbeProvenanceVerified)
		case probeOutcomeFailed:
			status = string(ProbeProvenanceFailed)
		}
	}

	effectiveFileID := 0
	if file != nil {
		effectiveFileID = file.ID
	}
	// Include the effective source in the revision so a rotation to a sibling
	// with an identical inventory still changes the ETag the poll compares.
	revision := playback.ComputeInventoryRevisionV3(status, audioTracks, subtitleInventory, playback.InventorySourceIdentityV3{
		EffectiveMediaFileID:  effectiveFileID,
		EffectiveVirtualURI:   effectiveVirtualURI,
		VirtualSourceRevision: session.VirtualSourceRevision,
	})
	return playback.PlaybackInventoryV3{
		SessionID:             session.ID,
		InventoryRevision:     revision,
		InventoryStatus:       status,
		AudioTracks:           audioTracks,
		SubtitleInventory:     subtitleInventory,
		EffectiveMediaFileID:  effectiveFileID,
		EffectiveVirtualURI:   effectiveVirtualURI,
		VirtualSourceRevision: session.VirtualSourceRevision,
	}, nil
}

// inventoryEffectiveFile resolves the catalog row whose inventory describes the
// release the session is actually bound to. The bound candidate URI wins: when
// a row names it, that row speaks for the release (probed evidence if present,
// its own declared metadata otherwise). Plan-time evidence is overlaid only
// when it still matches that bound release. When no row names the bound release
// and the loaded row is a different one, an identity-only copy with no tracks is
// returned, so the previous release's inventory is never shown for it.
func (h *PlaybackHandler) inventoryEffectiveFile(ctx context.Context, file *models.MediaFile, session *playback.Session, candidateURI string) *models.MediaFile {
	if file == nil {
		return nil
	}
	if candidateURI == "" {
		return bindSessionVirtualSourceWithTracks(ctx, file, session, h.fileResolver)
	}
	base := file
	if h.VirtualFileLookup != nil {
		if row, err := h.VirtualFileLookup(ctx, candidateURI); err == nil && row != nil {
			base = row
		}
	}
	if base == file && strings.TrimSpace(file.FilePath) != candidateURI {
		// No row names the bound release. Do not serve the loaded row's
		// inventory under the bound identity.
		placeholder := *file
		placeholder.FilePath = candidateURI
		placeholder.AudioTracks = nil
		placeholder.SubtitleTracks = nil
		placeholder.ExternalSubtitles = nil
		placeholder.ProbeUpdatedAt = nil
		base = &placeholder
	}
	return bindSessionVirtualSourceWithTracks(ctx, base, session, h.fileResolver)
}

// PublishSourceCommitted pushes the effective version and its declared
// inventory to a live session the moment a transport commits to it. It is
// invoked on the start commit and on a serve-layer rotation so a playing client
// can follow the streamed version immediately, ahead of the background probe;
// the existing inventory poll then upgrades the declared list to probe
// evidence. Best-effort: a session without a realtime connection is a no-op.
func (h *PlaybackHandler) PublishSourceCommitted(ctx context.Context, sessionID string) {
	if h == nil || sessionID == "" {
		return
	}
	// Read the session and its candidate-binding generation from one lock, so
	// the pair the rotation probe is scheduled against cannot be torn by a
	// rotation that lands between a session read and a separate generation read.
	// A manager without the pairing capability falls back to a plain session read
	// plus a separate generation read (best-effort); the probe's effective-row
	// reload closes that gap before any write.
	session, generation, paired := h.sessionWithSourceGeneration(sessionID)
	if !paired {
		loaded, loadErr := h.sessionMgr.GetSession(sessionID)
		if loadErr != nil || loaded == nil {
			return
		}
		session = loaded
		generation, _ = h.inventorySourceGeneration(sessionID)
	}
	if session == nil {
		return
	}
	// A committed binding move is a rotation: re-probe the replacement so its
	// verified inventory lands without waiting for a replan. The probe is
	// scheduled before the realtime check because it is worth doing even for a
	// session that has not (yet) opened a realtime connection. Only a session
	// that carries a virtual binding is considered, and only a recorded rotation
	// (revision cleared, evidence anchored at the previous candidate) triggers
	// it, so the ordinary start commit — which just set the revision — does not
	// double-probe a row the start path already probed.
	//
	// The file is loaded from the paired session's effective row after the
	// capture; a rotation that lands in between is caught by the generation the
	// refresh carries (and again by the probe's live-row reload), so a file that
	// no longer names the live effective row is never probed or persisted.
	if h.fileResolver != nil && session.VirtualSourceURI != "" {
		if file, loadErr := h.fileResolver.GetByID(ctx, session.MediaFileID); loadErr == nil && file != nil &&
			virtualCandidateRotationRecordedV3(session, file) {
			h.refreshRotatedVirtualCandidateBackground(ctx, session, generation, file)
		}
	}
	if h.RealtimeHub == nil {
		return
	}
	// No live realtime connection: there is nothing to push and building the
	// inventory would be a wasted catalog read on the start path.
	if !session.HasRealtimeConnection {
		return
	}
	var record *playback.AttemptRecordV3
	if h.PlanStoreV3 != nil {
		if loaded, loadErr := h.PlanStoreV3.GetAttempt(ctx, sessionID); loadErr == nil {
			record = loaded
		}
	}
	inventory, invErr := h.playbackInventoryForSession(ctx, session, record)
	if invErr != nil {
		slog.DebugContext(ctx, "source committed event skipped: inventory unavailable",
			"component", "playback", "session", sessionID, "error", invErr)
		return
	}
	event, err := playback.NewSourceCommittedEvent(sessionID, playback.SourceCommittedPayload{
		EffectiveMediaFileID:  inventory.EffectiveMediaFileID,
		EffectiveVirtualURI:   inventory.EffectiveVirtualURI,
		VirtualSourceRevision: inventory.VirtualSourceRevision,
		InventoryStatus:       inventory.InventoryStatus,
		AudioTracks:           inventory.AudioTracks,
	})
	if err != nil {
		slog.WarnContext(ctx, "failed to encode source committed realtime event",
			"component", "playback", "session", sessionID, "error", err)
		return
	}
	if err := h.RealtimeHub.Send(sessionID, event); err != nil && !errors.Is(err, playback.ErrRealtimeConnectionNotFound) {
		slog.WarnContext(ctx, "failed to deliver source committed realtime event",
			"component", "playback", "session", sessionID, "error", err)
	}
}

// mediaFileSessionLookup enumerates the live playback sessions associated with
// a media file so a background probe can push an inventory upgrade to each of
// them. *playback.SessionManager implements it; a manager that does not is a
// no-op rather than an error.
type mediaFileSessionLookup interface {
	GetSessionsByMediaFileID(fileID int) []*playback.Session
}

// refreshRotatedVirtualCandidateBackground re-resolves and re-probes the
// replacement candidate a serve-layer rotation just committed, so its verified
// track evidence lands without waiting for a replan or a version-list refresh.
// The rotation itself only moved the session binding and cleared the carried
// evidence: until the replacement is probed, the serve path stays fail-closed
// (evidence mismatch rejects the stale inventory) and the version list shows
// declared metadata. This closes that window.
//
// session and generation are the pair PublishSourceCommitted captured (from one
// lock when the manager exposes sessionWithSourceGeneration, otherwise from a
// session read plus a separate generation read); file is the effective-row load
// for that session. Before the detached goroutine is launched the pairing is
// re-read and the scheduling is dropped unless the binding generation still
// matches and file still names the live effective row, so a rotation that landed
// while the file was loading does not start a probe against a stale row.
//
// It is best-effort and bounded by the detached-work gate. A handler without a
// resolver or prober is a no-op; a resolve or probe failure leaves the gate
// closed exactly as before, so this can never authorize serving stale tracks.
func (h *PlaybackHandler) refreshRotatedVirtualCandidateBackground(ctx context.Context, session *playback.Session, generation uint64, file *models.MediaFile) {
	if h == nil || session == nil || file == nil || !isVirtualPlaybackFile(file) || session.VirtualSourceURI == "" {
		return
	}
	// Re-read the (session, generation) pair and refuse to schedule if the
	// binding moved since the caller captured it, or if the caller's file no
	// longer names the live effective row. This is the gap between the caller's
	// file load and the goroutine launch. A manager without the paired reader
	// falls back to a plain session read plus a separate generation read; a
	// manager without any generation capability keeps generation 0 and relies on
	// the effective-row comparison alone (best-effort, as before).
	liveSession, liveGen, haveLive := h.sessionWithSourceGeneration(session.ID)
	if !haveLive {
		if loaded, loadErr := h.sessionMgr.GetSession(session.ID); loadErr == nil && loaded != nil {
			liveSession = loaded
			liveGen, _ = h.inventorySourceGeneration(session.ID)
			haveLive = true
		}
	}
	if haveLive {
		if liveGen != generation || liveSession.MediaFileID != file.ID {
			slog.InfoContext(ctx, "rotated virtual candidate probe dropped: binding moved before scheduling",
				"component", "api", "session", session.ID,
				"file_id", file.ID, "live_file_id", liveSession.MediaFileID)
			return
		}
	}
	if h.VirtualMediaDetailedResolver == nil && h.VirtualMediaResolver == nil {
		return
	}
	// The probe reads through the loopback relay, exactly as the start-path and
	// fallback probes do; without a relay the resolved URL is the raw provider
	// URL and the prober is not wired to read it.
	if h.RemoteStreamRelay == nil {
		return
	}
	gate := h.detachedGate()
	if !gate.tryAcquire() {
		slog.WarnContext(ctx, "rotated virtual candidate probe skipped: detached worker budget exhausted",
			"component", "api", "session", session.ID, "file_id", file.ID, "virtual_uri", session.VirtualSourceURI)
		return
	}
	go func() {
		defer gate.release()
		bgCtx, cancel := h.virtualDetachedContext(ctx, virtualBackgroundProbeBudget)
		defer cancel()
		h.probeRotatedVirtualCandidate(bgCtx, session, file, generation)
	}()
}

// probeRotatedVirtualCandidate resolves the rotated candidate's provider URL and
// probes it, then persists the evidence for the row that owns the candidate. It
// is the synchronous core of refreshRotatedVirtualCandidateBackground, split out
// so a test can drive rotation recovery without a goroutine.
func (h *PlaybackHandler) probeRotatedVirtualCandidate(ctx context.Context, session *playback.Session, file *models.MediaFile, generation uint64) bool {
	if h == nil || session == nil || file == nil || session.VirtualSourceURI == "" {
		return false
	}
	// The binding may have moved again since the snapshot: re-read it and refuse
	// when the rotation this probe was scheduled for is no longer current. The
	// probe belongs to the candidate the session now names.
	live, err := h.sessionMgr.GetSession(session.ID)
	if err != nil || live == nil {
		return false
	}
	// generationFence reports whether the binding this work was scheduled for is
	// still current. It is re-checked after every blocking stage so a rotation
	// that lands while the resolve or the probe runs cannot let the stale work
	// authorize a selection, persist evidence, or release the pin.
	generationFence := func() bool {
		if generation == 0 {
			return true
		}
		current, ok := h.inventorySourceGeneration(session.ID)
		return ok && current == generation
	}
	if !generationFence() {
		return false
	}
	// The caller's file was loaded from the scheduling session's effective row.
	// If the live effective row has since moved (a rotation landed between the
	// caller's capture and this read), the carried file names a superseded row:
	// drop rather than probe or persist the wrong row. Re-derive the file from
	// the live effective row so the transient clone, neutral key, and persist
	// call all reference the row the session actually serves; fail closed if it
	// cannot be loaded or does not match.
	liveFile := file
	if h.fileResolver != nil && live.MediaFileID > 0 {
		if loaded, loadErr := h.fileResolver.GetByID(ctx, live.MediaFileID); loadErr == nil && loaded != nil {
			liveFile = loaded
		}
	}
	if liveFile == nil || liveFile.ID <= 0 || liveFile.ID != live.MediaFileID || liveFile.ID != file.ID {
		slog.InfoContext(ctx, "rotated virtual candidate probe dropped: effective row moved",
			"component", "api", "session", live.ID,
			"file_id", file.ID, "live_file_id", live.MediaFileID)
		return false
	}
	if !isVirtualPlaybackFile(liveFile) {
		return false
	}
	resolved, cleanup, resolveErr := h.resolveVirtualInputURI(
		withVirtualSessionBindingV3(ctx, true), live.VirtualSourceURI, live.VirtualSourceOwnerInstallationID,
		live.UserID, live.ProfileID, false, nil, "",
	)
	if cleanup != nil {
		defer cleanup()
	}
	// The resolve (a provider round-trip) is the other blocking stage: a
	// rotation that landed while it ran means this resolved URL names a
	// superseded candidate. Drop it rather than probe or persist it.
	if !generationFence() {
		slog.InfoContext(ctx, "rotated virtual candidate re-resolve dropped: binding moved during resolution",
			"component", "api", "session", live.ID, "virtual_uri", live.VirtualSourceURI)
		return false
	}
	if resolveErr != nil {
		slog.WarnContext(ctx, "rotated virtual candidate re-resolve failed",
			"component", "api", "session", live.ID, "virtual_uri", live.VirtualSourceURI,
			"error", logredact.SanitizeURLError(resolveErr))
		return false
	}
	probeCand := VirtualPlaybackStream{
		URI:            resolved.URI,
		CodecAudio:     resolved.CodecAudio,
		AudioLanguages: resolved.AudioLanguages,
		RequestHeaders: resolved.RequestHeaders,
	}
	probeTransient := cloneVirtualProbeTransient(*liveFile)
	if resolved.URI != "" {
		probeTransient.FilePath = resolved.URI
	}
	h.probeVirtualSourceAndPersist(
		ctx,
		bestResultCacheKey(liveFile.ContentID, virtualPlaybackNeutralKey(live.VirtualSourceURI), live.VirtualSourceOwnerInstallationID),
		liveFile, resolved.URL, probeTransient, probeCand,
		h.virtualExpectedRuntimeMinutes(ctx, liveFile), live.VirtualSourceOwnerInstallationID,
		generationFence,
	)
	return true
}

// inventoryUpdatedPublishBudget bounds one inventory_updated fan-out so the
// background probe that triggered it (a detached start-path repair or a
// virtual-evidence worker) cannot be held up by a slow catalog or subtitle
// read.
const inventoryUpdatedPublishBudget = 3 * time.Second

// inventoryPublishSummary reports what one PublishInventoryUpdated fan-out
// actually delivered: the file id whose committed evidence was pushed, how many
// live sessions received an inventory_updated event, and the revision of the
// last delivery. Callers log it so a live run can confirm delivery instead of
// inferring it from the absence of errors.
type inventoryPublishSummary struct {
	FileID           int
	SessionsNotified int
	Revision         string
}

// PublishInventoryUpdated pushes the probe-verified track inventory to every
// live realtime session currently playing fileID. It is invoked from the
// background probe's persistence points — the local start-path repair and the
// virtual evidence pipeline — after the evidence is committed, so a client can
// replace the declared menu it took from source_committed or its plan without
// waiting for the inventory poll or a replan.
//
// Advisory and best-effort. The event carries menu data only: it never touches
// the executable recipe, plan generation, transport choice, or streamed bytes.
// A session manager that cannot enumerate sessions by file, or a session
// without a realtime connection, is a no-op. Only verified evidence is pushed;
// a declared inventory is what the client already holds, so re-sending it would
// be noise. The payload carries the session's inventory revision so a client
// gates duplicates and out-of-order pushes.
//
// This file-wide fan-out is for catalog-evidence callers whose evidence commit
// concerns every session on the file. Per-session terminal notifications (the
// deferred-probe dispatcher) must use PublishInventoryUpdatedToSession instead:
// they ack delivery per session and per binding generation, so pushing sibling
// sessions on the same file would break their exactly-once accounting.
func (h *PlaybackHandler) PublishInventoryUpdated(ctx context.Context, fileID int) inventoryPublishSummary {
	summary := inventoryPublishSummary{FileID: fileID}
	if h == nil || h.RealtimeHub == nil || fileID <= 0 {
		return summary
	}
	lookup, ok := h.sessionMgr.(mediaFileSessionLookup)
	if !ok {
		return summary
	}
	if ctx == nil {
		ctx = context.Background()
	}
	publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inventoryUpdatedPublishBudget)
	defer cancel()
	for _, session := range lookup.GetSessionsByMediaFileID(fileID) {
		if session == nil || session.ID == "" || !session.HasRealtimeConnection {
			continue
		}
		revision, delivered := h.publishInventoryUpdatedToSession(publishCtx, session, 0, "", nil)
		if delivered {
			summary.SessionsNotified++
			summary.Revision = revision
		}
	}
	return summary
}

// PublishInventoryUpdatedToSession pushes the committed track inventory to one
// specific session, in contrast to PublishInventoryUpdated's file-wide fan-out.
// It exists for the deferred-probe terminal-notification path: that machinery
// acknowledges delivery per session and per binding generation, so pushing a
// sibling session that happens to play the same file would deliver an event the
// sibling never acked — and ack this session's generation against a push the
// sibling also received, breaking exactly-once for both. The payload is built by
// the same publishInventoryUpdatedToSession the fan-out uses (and the realtime
// hello republish targets directly), so the delivered inventory is identical;
// only the recipient set differs. fileID is the intent's recorded file, used
// only to skip the send when the session no longer serves that file; the payload
// itself is always resolved from the live session. Best-effort: an unknown,
// ended, or realtime-less session is a no-op.
func (h *PlaybackHandler) PublishInventoryUpdatedToSession(ctx context.Context, sessionID string, fileID int) inventoryPublishSummary {
	summary := inventoryPublishSummary{FileID: fileID}
	if h == nil || h.RealtimeHub == nil || sessionID == "" {
		return summary
	}
	if ctx == nil {
		ctx = context.Background()
	}
	session, err := h.sessionMgr.GetSession(sessionID)
	if err != nil || session == nil {
		return summary
	}
	if !session.HasRealtimeConnection {
		return summary
	}
	// The intent was parked against a file the session served at park time. A
	// session that moved to a different file since then is absent from this
	// notification's scope; the move's own lifecycle carries its own push.
	if fileID > 0 && session.MediaFileID != fileID && session.RequestedMediaFileID != fileID {
		return summary
	}
	publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inventoryUpdatedPublishBudget)
	defer cancel()
	revision, delivered := h.publishInventoryUpdatedToSession(publishCtx, session, 0, "", nil)
	if delivered {
		summary.SessionsNotified++
		summary.Revision = revision
	}
	return summary
}

// publishRefusedProbeInventory serves probe evidence that the identity guard
// refused to write, without touching the catalog. The guard is right to block a
// write whose path ownership cannot be proven, but playback truth must not stay
// stale while it does: the probed tracks are the verified inventory of the
// release the client is watching, so they are pushed in memory to every live
// session bound to fileID. Only sessions whose bound virtual source names
// candidateURI, from the same catalog row fileID, receive the override; a
// mismatch (a sibling row that shares the candidate URI) falls back to the
// committed catalog inventory, so one row's probed tracks never paint another.
//
// It returns how many sessions received the overridden inventory and logs the
// delivery, so the next live run can confirm the fallback reached the menu.
func (h *PlaybackHandler) publishRefusedProbeInventory(ctx context.Context, fileID int, candidateURI string, probed *models.MediaFile) int {
	if h == nil || h.RealtimeHub == nil || fileID <= 0 || probed == nil || strings.TrimSpace(candidateURI) == "" {
		return 0
	}
	lookup, ok := h.sessionMgr.(mediaFileSessionLookup)
	if !ok {
		return 0
	}
	if ctx == nil {
		ctx = context.Background()
	}
	publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inventoryUpdatedPublishBudget)
	defer cancel()
	notified := 0
	for _, session := range lookup.GetSessionsByMediaFileID(fileID) {
		if session == nil || session.ID == "" || !session.HasRealtimeConnection {
			continue
		}
		if _, delivered := h.publishInventoryUpdatedToSession(publishCtx, session, fileID, candidateURI, probed); delivered {
			notified++
		}
	}
	if notified > 0 {
		slog.InfoContext(ctx, "virtual probe evidence served to live session without a catalog write",
			virtualEvidenceLogKeyComponent, virtualEvidenceLogValueAPI,
			virtualEvidenceLogKeyFileID, fileID,
			"candidate_uri", candidateURI,
			"sessions_notified", notified)
	}
	return notified
}

// refusedProbeInventoryFile overlays probed tracks onto a copy of the session's
// effective catalog row when the session is bound to the exact candidate the
// probe ran against. It returns nil when there is no override to apply, so the
// caller keeps the committed catalog inventory. The returned file keeps the
// row's id (the catalog is not written), takes the probed tracks, and is stamped
// so the built inventory reports verified evidence and a fresh revision.
//
// probedFileID is the catalog row the probe was captured for. Duplicate rows for
// one release share candidate URIs, so the URI match alone would paint the
// probed tracks onto a sibling row playing the same candidate; the effective row
// must be the same row the evidence came from. A session with no known effective
// row cannot be confirmed as that row and fails closed, mirroring the serve-path
// guard in stream.go (virtualEvidenceMatchesBoundFile). A non-positive
// probedFileID (a caller without that identity) keeps the historical URI-only
// behavior for sessions that do carry an effective row.
func (h *PlaybackHandler) refusedProbeInventoryFile(ctx context.Context, session *playback.Session, probedFileID int, candidateURI string, probed *models.MediaFile) *models.MediaFile {
	if session == nil || probed == nil || strings.TrimSpace(candidateURI) == "" {
		return nil
	}
	// An unknown bound row (id 0) carries no identity to confirm against the
	// evidence; it must not receive the override. This is the same fail-closed
	// rule the serving path applies when the evidence row is known but the
	// bound row is not.
	if session.MediaFileID <= 0 {
		return nil
	}
	if probedFileID > 0 && session.MediaFileID != probedFileID {
		return nil
	}
	if !sameVirtualCandidate(session.VirtualSourceURI, candidateURI) {
		return nil
	}
	base, err := h.fileResolver.GetByID(ctx, session.MediaFileID)
	if err != nil || base == nil {
		return nil
	}
	override := *base
	override.AudioTracks = probed.AudioTracks
	override.SubtitleTracks = probed.SubtitleTracks
	override.ExternalSubtitles = probed.ExternalSubtitles
	probedAt := time.Now().UTC()
	override.ProbeUpdatedAt = &probedAt
	return &override
}

// virtualSourceGenerationReader is the optional session-manager capability the
// inventory publisher fences on. A manager that does not expose it (a minimal
// test manager) keeps the prior best-effort behavior.
type virtualSourceGenerationReader interface {
	VirtualSourceGeneration(sessionID string) (uint64, error)
}

// inventorySourceGeneration reads the session's current source-binding
// generation and reports whether the manager can supply one.
func (h *PlaybackHandler) inventorySourceGeneration(sessionID string) (uint64, bool) {
	reader, ok := h.sessionMgr.(virtualSourceGenerationReader)
	if !ok {
		return 0, false
	}
	generation, err := reader.VirtualSourceGeneration(sessionID)
	if err != nil {
		return 0, false
	}
	return generation, true
}

// sessionWithSourceGenerationReader is the optional session-manager capability
// that returns a session copy paired with its candidate-binding generation from
// one lock. Both come from the same read, so a caller cannot compare a copy
// against a generation that belongs to a different binding move.
type sessionWithSourceGenerationReader interface {
	GetSessionWithSourceGeneration(sessionID string) (*playback.Session, uint64, error)
}

// sessionWithSourceGeneration reads the live session and its binding generation
// atomically. It reports false for a manager that does not expose the pairing
// (a minimal test manager) so the caller keeps its prior best-effort behavior.
func (h *PlaybackHandler) sessionWithSourceGeneration(sessionID string) (*playback.Session, uint64, bool) {
	reader, ok := h.sessionMgr.(sessionWithSourceGenerationReader)
	if !ok {
		return nil, 0, false
	}
	session, generation, err := reader.GetSessionWithSourceGeneration(sessionID)
	if err != nil || session == nil {
		return nil, 0, false
	}
	return session, generation, true
}

// publishInventoryUpdatedToSession builds and delivers one inventory_updated
// event from the session's live inventory. It re-resolves the effective release
// through the same playbackInventoryForSession the inventory endpoint uses, so
// a rotation, a re-probe, and a poll can never disagree about the tracks, the
// effective identity, or the revision.
//
// probed, when non-nil, is probe evidence that could not be written to the
// catalog (the identity guard refused the write). It is applied only to a
// session whose bound virtual source names candidateURI AND whose effective row
// is probedFileID, so a refused probe can still reach the live menu of the exact
// release — and only that row — it probed without being shown to a session
// playing a different row or a sibling that shares the candidate URI.
//
// It returns the delivered revision and whether an event was actually sent, so
// the caller can log delivery without re-deriving it.
func (h *PlaybackHandler) publishInventoryUpdatedToSession(ctx context.Context, session *playback.Session, probedFileID int, candidateURI string, probed *models.MediaFile) (string, bool) {
	// Two background probes (a start-path repair and a virtual-evidence worker)
	// can publish for the same session concurrently. Serialize the build and
	// the send under the session's per-session lock so an older build cannot be
	// delivered after a newer one for the same source identity: the revision is
	// a content digest, so a replayed older revision is otherwise
	// indistinguishable from a fresh state at the receiver.
	release := h.progressSideEffectLock(session.ID)
	defer release()
	// The caller enumerated sessions before the lock was held, so the snapshot
	// it passed can already name a superseded source binding. Re-read the live
	// session under the lock and take the binding generation from that same
	// read, then re-check the generation before the send: a binding move that
	// lands while the inventory is being resolved invalidates this build
	// instead of delivering an older release's revision after a newer one. The
	// move's own publish carries the current revision. Reading the generation
	// with the copy (not separately afterwards) means a move that lands between
	// the read and the comparison cannot make a payload built from the current
	// source look stale: only a build whose live copy is genuinely superseded is
	// dropped.
	generation, hasGeneration := h.inventorySourceGeneration(session.ID)
	live, err := h.sessionMgr.GetSession(session.ID)
	if err != nil || live == nil {
		return "", false
	}
	if refreshed, gen, ok := h.sessionWithSourceGeneration(session.ID); ok {
		live = refreshed
		generation = gen
		hasGeneration = true
	}
	var record *playback.AttemptRecordV3
	if h.PlanStoreV3 != nil {
		if loaded, err := h.PlanStoreV3.GetAttempt(ctx, session.ID); err == nil {
			record = loaded
		}
	}
	var inventory playback.PlaybackInventoryV3
	if override := h.refusedProbeInventoryFile(ctx, live, probedFileID, candidateURI, probed); override != nil {
		// Refused (unwritten) evidence for the exact release this session is
		// bound to: serve the probed tracks in memory. The catalog row is
		// untouched, so the effective identity stays the row's own id and the
		// client's revision gate still sees a new revision.
		inventory, err = h.playbackInventoryForFileV3(ctx, live, record, override, candidateURI)
	} else {
		inventory, err = h.playbackInventoryForSession(ctx, live, record)
	}
	if err != nil {
		slog.DebugContext(ctx, "inventory updated event skipped: inventory unavailable",
			"component", "playback", "session", session.ID, "error", err)
		return "", false
	}
	if inventory.InventoryStatus != string(ProbeProvenanceVerified) && inventory.InventoryStatus != string(ProbeProvenanceFailed) {
		// The probe has not upgraded this session's bound release and no
		// deferred probe has terminally failed, so the client already holds
		// exactly this declared inventory. A failed status is new information —
		// it ends the deferred loading state — so it is pushed.
		return inventory.InventoryRevision, false
	}
	if inventory.InventoryStatus == string(ProbeProvenanceFailed) &&
		record != nil && !attemptNegotiatedDeferTrackInventoryV3(record) {
		// A "failed" inventory is the deferred lifecycle's terminal signal: it
		// tells a client that negotiated deferred_track_inventory_v1 to leave
		// its provisional (tracks_pending) loading state. A session that did
		// not negotiate the lifecycle was never promised a provisional menu, so
		// a failed status is meaningless to it and must not be delivered. The
		// deferral gate already keeps such sessions out of the lifecycle; this
		// is the delivery-side guard that keeps a failed push from ever
		// reaching a session whose attempt did not negotiate. A nil record
		// (a minimal manager without a plan store) preserves the pre-existing
		// best-effort push. Verified pushes are untouched: they are the
		// pre-existing background upgrade any session may receive.
		return inventory.InventoryRevision, false
	}
	if hasGeneration {
		if current, ok := h.inventorySourceGeneration(session.ID); !ok || current != generation {
			slog.DebugContext(ctx, "inventory updated event skipped: source binding moved while building",
				"component", "playback", "session", session.ID, "built_generation", generation)
			return inventory.InventoryRevision, false
		}
	}
	event, err := playback.NewInventoryUpdatedEvent(session.ID, inventory)
	if err != nil {
		slog.WarnContext(ctx, "failed to encode inventory updated realtime event",
			"component", "playback", "session", session.ID, "error", err)
		return inventory.InventoryRevision, false
	}
	if err := h.RealtimeHub.Send(session.ID, event); err != nil {
		if !errors.Is(err, playback.ErrRealtimeConnectionNotFound) {
			slog.WarnContext(ctx, "failed to deliver inventory updated realtime event",
				"component", "playback", "session", session.ID, "error", err)
		}
		// A session without a realtime connection received nothing; do not
		// count it as notified.
		return inventory.InventoryRevision, false
	}
	return inventory.InventoryRevision, true
}

// ReplanDigestV3 fingerprints the exact replan body so a reused request id with
// different input is a detectable idempotency violation.
func ReplanDigestV3(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func validatePlaybackSessionID(sessionID string) error {
	id, err := uuid.Parse(sessionID)
	if err != nil || id == uuid.Nil || id.String() != sessionID {
		return playbackOperationError(http.StatusBadRequest, "bad_request", "Invalid playback session ID")
	}
	return nil
}

// expiredStopID is the server-minted stop identity recorded on the attempt
// row when a session ends without a client stop (expiry, abort).
func expiredStopID(sessionID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("silo-expired:"+sessionID)).String()
}

// markAttemptStoppedServerSide stops the attempt row under the server-minted
// stop id and writes the deny marker, so a stopped or expired session cannot
// be replayed from its start attempt or served from a valid token on another
// replica. Best effort: the local teardown has already happened.
func (h *PlaybackHandler) markAttemptStoppedServerSide(ctx context.Context, sessionID string) {
	if h == nil {
		return
	}
	markAttemptStoppedServerSide(ctx, h.PlanStoreV3, h.StreamDeny, sessionID)
	h.forgetProgressSideEffectLock(sessionID)
}

func markAttemptStoppedServerSide(ctx context.Context, planStore playback.PlanStoreV3, deny *playback.StreamDeny, sessionID string) {
	if sessionID == "" {
		return
	}
	deny.Deny(ctx, sessionID)
	store, ok := planStore.(playback.ProgressStoreV3)
	if !ok {
		return
	}
	receipt, first, err := store.StopAttempt(ctx, sessionID, expiredStopID(sessionID), nil)
	if err != nil {
		if !errors.Is(err, playback.ErrSessionNotFound) {
			slog.WarnContext(ctx, "failed to stop expired playback attempt", "component", "api", "session", sessionID, "playback_session_id", sessionID, "error", err)
		}
		return
	}
	if first {
		// The local teardown already ran; the receipt is complete.
		receipt.Finalized = true
		if err := store.RecordStopReceipt(ctx, sessionID, receipt); err != nil {
			slog.WarnContext(ctx, "failed to record expired playback stop receipt", "component", "api", "session", sessionID, "playback_session_id", sessionID, "error", err)
		}
	}
}

// attemptLive reports whether the attempt row exists and is not stopped.
func (h *PlaybackHandler) attemptLive(ctx context.Context, sessionID string) bool {
	if h == nil || h.PlanStoreV3 == nil || sessionID == "" {
		return false
	}
	record, err := h.PlanStoreV3.GetAttempt(ctx, sessionID)
	return err == nil && record != nil && record.StoppedAt == nil
}

// attemptStoppedElsewhere reports whether the attempt row is already stopped,
// so a replica reaping its stale local copy does not overwrite a stop another
// replica accepted. Unknown rows (no plan store, not found) report false.
func (h *PlaybackHandler) attemptStoppedElsewhere(ctx context.Context, sessionID string) bool {
	if h == nil || h.PlanStoreV3 == nil || sessionID == "" {
		return false
	}
	record, err := h.PlanStoreV3.GetAttempt(ctx, sessionID)
	return err == nil && record != nil && record.StoppedAt != nil
}

// attemptActiveElsewhere reports whether the attempt row saw progress more
// recently than this replica's copy did. Media and progress requests can land
// on another replica, leaving the local activity clock stale; that copy is
// dropped without finalizing so the live session elsewhere keeps serving.
func (h *PlaybackHandler) attemptActiveElsewhere(ctx context.Context, session *playback.Session) bool {
	if h == nil || h.PlanStoreV3 == nil || session == nil {
		return false
	}
	record, err := h.PlanStoreV3.GetAttempt(ctx, session.ID)
	if err != nil || record == nil || record.LastSample == nil || record.LastSampleAt.IsZero() {
		return false
	}
	return record.LastSampleAt.After(session.LastActivityAt)
}
