package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

// This file owns cold-start default-audio language stability: the selection
// the server made from declared (fast-path) metadata must survive the
// verified probe reorder, or the committed route keeps playing the wrong
// language while the menu already shows the corrected inventory.
//
// deferProbe stays: this runs only after probe evidence has been committed
// (persistVirtualEvidenceDirect, persistVirtualEvidenceTask), never on the
// start critical path. It re-enumerates live sessions independent of
// realtime delivery, replays the persisted selection intent against the
// committed verified inventory, and issues an automatic track_change replan
// only when the executable audio selection actually moved.
//
// Language stability — not ordinal stability — is the success condition:
// the replan names the verified track that carries the preferred language,
// even when its array position differs from the committed one.

// Selection origins persisted on the attempt record (see AttemptRecordV3).
const (
	// SelectionOriginAuto means the viewer sent no audio identity: the
	// server resolved an omitted selection through SelectAudioTrack.
	SelectionOriginAuto = "auto"
	// SelectionOriginExplicit means the viewer named an audio track.
	// Reconciliation must never override it; only verify it still exists.
	SelectionOriginExplicit = "explicit"
)

// AudioReconciliationReplanReason marks the automatic track_change body the
// reconcile path builds. track_change must not carry Failure (see
// ReplanRequestV3.Validate), so the reason travels in the deterministic
// replan-request id and the plan log, both additive and client-invisible.
const AudioReconciliationReplanReason = "default_audio_reconciliation"

// reconcileVerifiedDefaultAudio replays persisted audio selection intent
// against the committed verified inventory for fileID. It is invoked after
// probe persistence commits (both persistVirtualEvidenceDirect branches and
// the buffered worker), keeps an old code path recording, and never fires
// inside PublishInventoryUpdated: this reconciles the executable recipe, not
// the menu, independent of whether any session holds a realtime connection.
func (h *PlaybackHandler) reconcileVerifiedDefaultAudio(ctx context.Context, fileID int) {
	if h == nil || fileID <= 0 || h.sessionMgr == nil {
		return
	}
	lookup, ok := h.sessionMgr.(mediaFileSessionLookup)
	if !ok {
		return
	}
	for _, session := range lookup.GetSessionsByMediaFileID(fileID) {
		if session == nil || session.ID == "" {
			continue
		}
		h.reconcileSessionDefaultAudio(ctx, session, fileID)
	}
}

// reconcileSessionDefaultAudio reconciles one live session against the
// verified catalog inventory of the file it is bound to.
func (h *PlaybackHandler) reconcileSessionDefaultAudio(ctx context.Context, session *playback.Session, fileID int) {
	intent := h.audioSelectionIntentForSession(ctx, session)
	switch intent.origin {
	case SelectionOriginExplicit:
		h.verifyExplicitAudioSelection(ctx, session, intent, fileID)
	case SelectionOriginAuto:
		h.reconcileAutoAudioSelection(ctx, session, intent, fileID)
	default:
		// Legacy sessions and records without intent fields predate this
		// change: do nothing and never trigger a spurious correction.
		slog.DebugContext(ctx, "default audio reconciliation skipped: no selection intent",
			"component", "api", "session", session.ID, "file_id", fileID)
	}
}

// audioSelectionIntent is the persisted intent a session started with.
type audioSelectionIntent struct {
	origin           string
	preferredLang    string
	seriesSignature  *userstore.AudioTrackSignature
	selectedOverride *userstore.AudioTrackSignature
	seriesPref       *playback.AudioTrackPreference
}

// audioSelectionIntentForSession reads the durable intent: the attempt
// record first (it survives session-manager rebuilds), then the live
// session mirror for reconnects before the record is reachable.
func (h *PlaybackHandler) audioSelectionIntentForSession(ctx context.Context, session *playback.Session) audioSelectionIntent {
	var intent audioSelectionIntent
	if h != nil && h.PlanStoreV3 != nil {
		if record, err := h.PlanStoreV3.GetAttempt(ctx, session.ID); err == nil && record != nil {
			intent.origin = strings.TrimSpace(record.SelectionOrigin)
			intent.preferredLang = strings.TrimSpace(record.PreferredAudioLanguage)
			intent.seriesSignature = record.SeriesAudioPreferenceSignature
			intent.selectedOverride = record.SelectedAudioSignature
		}
		_ = ctx
	}
	if intent.origin == "" {
		intent.origin = strings.TrimSpace(session.SelectionOrigin)
	}
	if intent.preferredLang == "" {
		intent.preferredLang = strings.TrimSpace(session.PreferredAudioLanguage)
	}
	if intent.seriesSignature == nil {
		intent.seriesSignature = session.SeriesAudioPreferenceSignature
	}
	if intent.selectedOverride == nil {
		intent.selectedOverride = session.SelectedAudioSignature
	}
	if intent.seriesSignature != nil || intent.preferredLang != "" {
		intent.seriesPref = &playback.AudioTrackPreference{
			AudioLanguage:  intent.preferredLang,
			TrackSignature: intent.seriesSignature,
		}
	}
	return intent
}

// reconcileAutoAudioSelection re-runs SelectAudioTrack against the verified
// inventory and issues an automatic track_change replan when the executable
// selection moved. Identical executable selection is a byte-equal no-op.
func (h *PlaybackHandler) reconcileAutoAudioSelection(ctx context.Context, session *playback.Session, intent audioSelectionIntent, fileID int) {
	if h == nil || h.fileResolver == nil || h.PlanStoreV3 == nil {
		return
	}
	live, err := h.sessionMgr.GetSession(session.ID)
	if err != nil || live == nil {
		return
	}
	verified, err := h.fileResolver.GetByID(ctx, fileID)
	if err != nil || verified == nil || len(verified.AudioTracks) == 0 {
		return
	}
	if !virtualEvidenceMatchesBoundFile(verified, live) {
		// Source rotation (or a sibling-row write) means this evidence is
		// stale for the bound session: refuse, and surface the still-valid
		// tracks in memory only, exactly like the refused-write path.
		h.publishRefusedProbeInventory(ctx, fileID, strings.TrimSpace(live.VirtualSourceURI), verified)
		slog.InfoContext(ctx, "default audio reconciliation refused: stale evidence for the bound candidate",
			"component", "api", "session", session.ID, "file_id", fileID)
		return
	}
	record, err := h.PlanStoreV3.GetAttempt(ctx, live.ID)
	if err != nil || record == nil {
		return
	}
	if strings.TrimSpace(record.SelectionOrigin) != "" && strings.TrimSpace(record.SelectionOrigin) != SelectionOriginAuto {
		// A racing explicit change persisted first: never override.
		return
	}
	// The live session must still carry the committed start selection: a
	// viewer track_change between start and probe supersedes the intent
	// (the race case), and reconciliation must not override the viewer's
	// newer choice. Compare executable signature identity, not the ordinal,
	// because the reorder is exactly what can shift ordinals.
	if !liveSelectionStillCommitted(live, record, intent) {
		return
	}
	committedIndex := committedAudioTrackIndexV3(record, live)
	if committedIndex < 0 || committedIndex >= len(live.VirtualAudioTracks) {
		// Non-virtual or legacy session without a plan-time inventory: the
		// committed rows still map by verified order, which is exactly what
		// SelectAudioTrack replays below. Fall through with the verified set
		// as the plan-time set.
		live = &playback.Session{
			ID:                 live.ID,
			UserID:             live.UserID,
			ProfileID:          live.ProfileID,
			AudioTrackIndex:    committedIndex,
			VirtualAudioTracks: verified.AudioTracks,
		}
		if committedIndex < 0 || committedIndex >= len(verified.AudioTracks) {
			return
		}
	}
	recomputed := playback.SelectAudioTrack(verified.AudioTracks, intent.preferredLang, intent.seriesPref)
	if recomputed < 0 || recomputed >= len(verified.AudioTracks) {
		return
	}
	committed := live.VirtualAudioTracks[committedIndex]
	selected := verified.AudioTracks[recomputed]
	if audioSignatureMatchesCommittedTrack(selected, committed, intent, committedIndex, recomputed, live.VirtualAudioTracks, verified.AudioTracks) {
		// Byte-equal no-op: the executable selection is identical.
		return
	}
	if err := h.reconcileDefaultAudioReplan(ctx, live, record, verified, recomputed); err != nil {
		slog.WarnContext(ctx, "default audio reconciliation replan failed",
			"component", "api", "session", live.ID, "file_id", fileID, "error", err)
	}
}

// committedAudioTrackIndexV3 resolves the executable committed audio index:
// the plan's selected index when present, else the session's committed index.
func committedAudioTrackIndexV3(record *playback.AttemptRecordV3, session *playback.Session) int {
	if record != nil && record.CurrentPlan.SelectedTracks.Audio != nil && record.CurrentPlan.SelectedTracks.Audio.Index != nil {
		return *record.CurrentPlan.SelectedTracks.Audio.Index
	}
	if session != nil {
		return session.AudioTrackIndex
	}
	return 0
}

// audioSignatureMatchesCommittedTrack reports whether the recomputed verified
// selection is the same executable selection the recipe committed: same audio
// ordinal rank, same transport-relevant facts (codec + channel layout), and
// the same language evidence the intent produced at start. Same ordinal alone
// is NOT enough (a reorder keeps ordinals while moving languages), and same
// language alone is not enough (a different stream would need a fresh recipe).
func audioSignatureMatchesCommittedTrack(selected, committed models.AudioTrack, intent audioSelectionIntent, committedIndex, recomputed int, planTime, verified []models.AudioTrack) bool {
	if playback.AudioStreamOrdinal(planTime, committedIndex) != playback.AudioStreamOrdinal(verified, recomputed) {
		return false
	}
	if !audioTransportFactsEqual(selected, committed) {
		return false
	}
	if intent.selectedOverride != nil {
		if !audioTrackSignatureEqual(selected, *intent.selectedOverride) {
			return false
		}
	}
	return defaultAudioLanguageStillSatisfied(selected, intent)
}

// audioTransportFactsEqual is the transport reuse guard: the existing
// transport (and any fixed ffmpeg audio map on it) is only byte-identical
// when the chosen stream decodes the same codec with the same channel
// layout. When these differ the caller must not reuse the map; it issues a
// fresh recipe via the automatic replan instead.
func audioTransportFactsEqual(a, b models.AudioTrack) bool {
	return strings.EqualFold(strings.TrimSpace(a.Codec), strings.TrimSpace(b.Codec)) &&
		a.Channels == b.Channels &&
		strings.EqualFold(strings.TrimSpace(a.Layout), strings.TrimSpace(b.Layout))
}

// audioTrackSignatureEqual compares two probed tracks by the same stable
// signature the series preference persists: language (canonical), title,
// codec, layout and channel count.
func audioTrackSignatureEqual(track models.AudioTrack, sig userstore.AudioTrackSignature) bool {
	candidate := playback.AudioTrackSignatureFromTrack(track)
	if candidate == nil || sig.IsZero() {
		return candidate == nil && sig.IsZero()
	}
	return audioSignatureFieldsEqual(*candidate, sig)
}

// audioSignatureFieldsEqual compares signature snapshots field by field: the
// type carries slices, so it is not directly comparable.
func audioSignatureFieldsEqual(a, b userstore.AudioTrackSignature) bool {
	return a.Language == b.Language &&
		a.Title == b.Title &&
		a.EmbeddedTitle == b.EmbeddedTitle &&
		a.Codec == b.Codec &&
		a.Layout == b.Layout &&
		a.Channels == b.Channels &&
		slices.Equal(a.Languages, b.Languages)
}

// defaultAudioLanguageStillSatisfied reports whether the recomputed track
// still carries the preferred language (or no preference was ever resolved).
// MULTi membership goes through the shared membership helper's authority: a
// bare MULTI/DUAL never counts as a concrete match, a member list entry does.
func defaultAudioLanguageStillSatisfied(track models.AudioTrack, intent audioSelectionIntent) bool {
	return intent.preferredLang == "" || playback.TrackCarriesLanguage(track, intent.preferredLang)
}

// reconcileDefaultAudioReplan issues the automatic track_change replan built
// from the recipe, remapped onto the verified order, preserving position,
// with no route exclusion and no failure classification (track_change must
// not carry one). The replan-request id names the reconciliation reason so
// the durable lease history attributes the change to the server, not the
// viewer; no new operation or client-visible field is introduced.
func (h *PlaybackHandler) reconcileDefaultAudioReplan(ctx context.Context, session *playback.Session, record *playback.AttemptRecordV3, verified *models.MediaFile, audioIndex int) error {
	verifiedIndex := audioIndex
	audioID := playback.TrackIDV3(verified.ID, "audio", verifiedIndex)
	caller := PlaybackCaller{
		UserID:    session.UserID,
		ProfileID: session.ProfileID,
	}
	req := playback.ReplanRequestV3{
		ProtocolVersion:   playback.ProtocolV3,
		Operation:         playback.ReplanOperationTrackChangeV3,
		PlaybackAttemptID: record.PlaybackAttemptID,
		ReplanRequestID:   reconcileReplanRequestID(session, verifiedIndex, "req"),
		FailedPlanID:      record.CurrentPlanID,
		PlanAttemptID:     reconcileReplanRequestID(session, verifiedIndex, "plan"),
		PlanAttemptKey:    record.CurrentPlan.PlanAttemptKey,
		AttemptedPlanKeys: append([]string(nil), record.CurrentPlan.PlanAttemptKey),
		AttemptCount:      1,
		QualityPreference: record.NormalizedRequest.QualityPreference,
		PositionSeconds:   session.Position,
		Metered:           record.NormalizedRequest.Metered,
		SelectedTracks: playback.SelectedTracksV3{
			Audio:    &playback.TrackIdentityV3{ID: audioID, Index: &verifiedIndex},
			Subtitle: record.CurrentPlan.SelectedTracks.Subtitle,
		},
		Capabilities:          record.NormalizedRequest.Capabilities,
		ClientPlaybackContext: record.NormalizedRequest.ClientPlaybackContext,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	response, err := h.ReplanPlaybackV2(ctx, caller, session.ID, PlaybackReplanCommand{Request: req, Digest: ReplanDigestV3(body)})
	if err != nil {
		return err
	}
	_ = response
	slog.InfoContext(ctx, "default audio reconciled to the verified inventory",
		"component", "api", "session", session.ID, "file_id", verified.ID,
		"audio_index", verifiedIndex, "reason", AudioReconciliationReplanReason)
	return nil
}

// verifyExplicitAudioSelection checks that an explicit viewer selection still
// exists in the verified inventory. It never overrides: a vanished track
// surfaces a terminal diagnostic (route event + log) so the viewer learns the
// selection is gone instead of hearing a silently substituted track.
func (h *PlaybackHandler) verifyExplicitAudioSelection(ctx context.Context, session *playback.Session, intent audioSelectionIntent, fileID int) {
	if h == nil || h.fileResolver == nil {
		return
	}
	verified, err := h.fileResolver.GetByID(ctx, fileID)
	if err != nil || verified == nil || len(verified.AudioTracks) == 0 {
		return
	}
	live, err := h.sessionMgr.GetSession(session.ID)
	if err != nil || live == nil {
		return
	}
	if !virtualEvidenceMatchesBoundFile(verified, live) {
		return
	}
	var record *playback.AttemptRecordV3
	if h.PlanStoreV3 != nil {
		record, _ = h.PlanStoreV3.GetAttempt(ctx, live.ID)
	}
	committedIndex := committedAudioTrackIndexV3(record, live)
	track, ok := audioTrackAtCommittedIndex(verified.AudioTracks, committedIndex)
	if ok && intent.selectedOverride != nil && !intent.selectedOverride.IsZero() {
		ok = audioTrackSignatureEqual(track, *intent.selectedOverride)
	}
	if ok {
		return
	}
	slog.WarnContext(ctx, "explicit audio selection missing from the verified inventory",
		"component", "api", "session", live.ID, "file_id", fileID, "audio_index", committedIndex)
	if record != nil {
		h.enqueueRouteEventV3(playback.RouteEventRecordV3{
			RouteEventV3: playback.RouteEventV3{
				ProtocolVersion:   playback.ProtocolV3,
				PlaybackAttemptID: record.PlaybackAttemptID,
				SessionID:         live.ID,
				PlanID:            record.CurrentPlanID,
				Event:             playback.RouteEventTerminalV3,
				OutputContextID:   record.NormalizedRequest.ClientPlaybackContext.Output.OutputContextID,
			},
			UserID:    live.UserID,
			ProfileID: live.ProfileID,
		})
	}
}

// audioTrackAtCommittedIndex returns the verified track at the committed
// index, or false when the reorder dropped it out of range.
func audioTrackAtCommittedIndex(tracks []models.AudioTrack, index int) (models.AudioTrack, bool) {
	if index < 0 || index >= len(tracks) {
		return models.AudioTrack{}, false
	}
	return tracks[index], true
}

// importGuardReconcileAudio keeps ReplanDigestV3 discoverable for this file's
// command builder; the digest function lives in playback_service.go.
var _ = ReplanDigestV3

// reconcilePendingAudioStartup re-checks one session when it attaches after
// its probe already landed: probe landing with no registered session leaves
// nothing to reconcile, so the attach (and first heartbeats, which reuse the
// session lookup) replays the same per-session path once the attempt exists.
func (h *PlaybackHandler) reconcilePendingAudioStartup(ctx context.Context, sessionID string) {
	if h == nil || h.sessionMgr == nil || h.PlanStoreV3 == nil || sessionID == "" {
		return
	}
	session, err := h.sessionMgr.GetSession(sessionID)
	if err != nil || session == nil {
		return
	}
	record, err := h.PlanStoreV3.GetAttempt(ctx, sessionID)
	if err != nil || record == nil {
		return
	}
	if strings.TrimSpace(record.SelectionOrigin) == "" {
		return
	}
	deadline, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	h.reconcileSessionDefaultAudio(deadline, session, record.EffectiveMediaFileID)
}

// reconcileReplanRequestID mints a deterministic, human-readable replan
// identity for one automatic reconciliation: the reason prefix attributes
// the change, the session hash and target index keep it unique per
// (session, decision) without depending on the session id being a UUID.
func reconcileReplanRequestID(session *playback.Session, audioIndex int, kind string) string {
	sum := sha256.Sum256([]byte(session.ID))
	return fmt.Sprintf("%s-%s-%s-%d", AudioReconciliationReplanReason, kind, hex.EncodeToString(sum[:])[:8], audioIndex)
}

// liveSelectionStillCommitted reports whether the live session's committed
// audio selection still matches the start-time selected signature: the
// user-change race guard. A viewer track_change between start and probe
// moves the live selection off the start signature, and the automatic
// replan must then decline instead of overriding the viewer's choice.
func liveSelectionStillCommitted(session *playback.Session, record *playback.AttemptRecordV3, intent audioSelectionIntent) bool {
	if session == nil || intent.selectedOverride == nil || intent.selectedOverride.IsZero() {
		return true
	}
	committedIndex := committedAudioTrackIndexV3(record, session)
	tracks := session.VirtualAudioTracks
	track, ok := audioTrackAtCommittedIndex(tracks, committedIndex)
	if !ok {
		return true
	}
	return audioTrackSignatureEqual(track, *intent.selectedOverride)
}
