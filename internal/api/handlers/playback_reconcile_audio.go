package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
//
// Multi-node note: session enumeration here is local-only (the manager holds
// this replica's in-memory sessions), so a session owned by another replica
// is not reconciled by this call. Every replica persists probe evidence to
// the shared catalog row and replays the same record-first path: the owning
// replica reconciles on its own evidence commit, on session attach, and on
// the heartbeat re-check below. The attempt record is the cross-replica
// rendezvous — intent and the settled ledger live on it, not in process
// memory — so no cross-replica RPC is needed for replicas to converge.
func (h *PlaybackHandler) reconcileVerifiedDefaultAudio(ctx context.Context, fileID int) {
	if h == nil || fileID <= 0 || h.sessionMgr == nil {
		return
	}
	lookup, ok := h.sessionMgr.(mediaFileSessionLookup)
	if !ok {
		return
	}
	// One deadline bounds the whole per-file reconciliation sweep; every
	// interior caller propagates this ctx instead of resetting it.
	deadlineCtx, cancel := context.WithTimeout(ctx, reconcileProbeBudgetV3)
	defer cancel()
	for _, session := range lookup.GetSessionsByMediaFileID(fileID) {
		if session == nil || session.ID == "" {
			continue
		}
		h.reconcileSessionDefaultAudio(deadlineCtx, session, fileID)
	}
}

// reconcileSessionDefaultAudio reconciles one live session against the
// verified catalog inventory of the file it is bound to.
func (h *PlaybackHandler) reconcileSessionDefaultAudio(ctx context.Context, session *playback.Session, fileID int) {
	// ctx carries the single entry deadline; interior callers propagate it.
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
//
// Bounded by generation: every verified inventory is keyed by its probe
// generation (the row's probe stamp), and the attempt's settled ledger
// records one decision per generation. A generation with a settled entry is
// never re-evaluated — a retried probe write or a second replica replays the
// stored decision instead of minting a second replan — so heartbeats and
// duplicate evidence commits converge rather than issuing unbounded replans.
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
	if !reconcileProbeVerifiedV3(verified) {
		// Gate on verified probe provenance before any session work: a
		// declared (never probed) row is exactly what the client already
		// holds, so reconciling against it would only churn.
		return
	}
	generation := reconcileGenerationV3(verified)
	if h.audioReconcileSettled(ctx, live.ID, generation) {
		return
	}
	record, err := h.PlanStoreV3.GetAttempt(ctx, live.ID)
	if err != nil || record == nil {
		return
	}
	if strings.TrimSpace(record.SelectionOrigin) != "" && strings.TrimSpace(record.SelectionOrigin) != SelectionOriginAuto {
		// A racing explicit change persisted first: never override.
		h.recordAudioReconcileDecision(ctx, live.ID, generation, playback.AudioReconcileEntryV3{Generation: generation, Decision: playback.AudioReconcileRefused})
		return
	}
	if !virtualEvidenceMatchesBoundFile(verified, live) {
		// Source rotation (or a sibling-row write) means this evidence is
		// stale for the bound session: refuse, and surface the still-valid
		// tracks in memory only, exactly like the refused-write path.
		h.publishRefusedProbeInventory(ctx, fileID, strings.TrimSpace(live.VirtualSourceURI), verified)
		slog.InfoContext(ctx, "default audio reconciliation refused: stale evidence for the bound candidate",
			"component", "api", "session", session.ID, "file_id", fileID)
		h.recordAudioReconcileDecision(ctx, live.ID, generation, playback.AudioReconcileEntryV3{Generation: generation, Decision: playback.AudioReconcileRefused})
		return
	}
	// The live session must still carry the committed start selection: a
	// viewer track_change between start and probe supersedes the intent
	// (the race case), and reconciliation must not override the viewer's
	// newer choice. Compare executable signature identity, not the ordinal,
	// because the reorder is exactly what can shift ordinals.
	if !liveSelectionStillCommitted(live, record, intent) {
		h.recordAudioReconcileDecision(ctx, live.ID, generation, playback.AudioReconcileEntryV3{Generation: generation, Decision: playback.AudioReconcileRefused})
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
		// Byte-equal no-op: the executable selection is identical. Settle
		// the generation so later heartbeats and duplicate evidence writes
		// replay the decision instead of re-evaluating.
		h.recordAudioReconcileDecision(ctx, live.ID, generation, playback.AudioReconcileEntryV3{Generation: generation, Decision: playback.AudioReconcileNoop})
		return
	}
	req, body, err := h.reconcileDefaultAudioReplan(ctx, live, record, verified, recomputed)
	if err != nil {
		slog.WarnContext(ctx, "default audio reconciliation replan failed",
			"component", "api", "session", live.ID, "file_id", fileID, "error", err)
		return
	}
	audioIndex := recomputed
	h.recordAudioReconcileDecision(ctx, live.ID, generation, playback.AudioReconcileEntryV3{
		Generation: generation, Decision: playback.AudioReconcileReplanned,
		AudioIndex: &audioIndex, Request: &req, RequestDigest: ReplanDigestV3(body),
	})
}

// reconcileProbeBudgetV3 bounds one reconcile evaluation: catalog read,
// session re-read, guard checks and the ledger write. The automatic replan
// itself runs under the replan path's own budgets once issued.
const reconcileProbeBudgetV3 = 10 * time.Second

// reconcileGenerationV3 keys one verified inventory generation: the row's
// probe stamp when present, else the inventory revision of its audio tracks.
// A retried probe write for the same generation replays the settled
// decision; a genuinely new probe (new stamp) re-evaluates.
func reconcileGenerationV3(file *models.MediaFile) string {
	if file == nil {
		return ""
	}
	if file.ProbeUpdatedAt != nil && !file.ProbeUpdatedAt.IsZero() {
		return "probe:" + file.ProbeUpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	sum := sha256.Sum256([]byte(audioInventoryFingerprintV3(file)))
	return "tracks:" + hex.EncodeToString(sum[:])[:16]
}

// audioInventoryFingerprintV3 is the stable identity of an audio inventory
// for generation keying: absolute stream index, language, codec, channels
// and layout per track, in order. Two probes of the same bytes produce the
// same fingerprint even across process restarts, so a retried write replays
// the settled decision instead of re-evaluating.
func audioInventoryFingerprintV3(file *models.MediaFile) string {
	if file == nil {
		return ""
	}
	var sb strings.Builder
	for _, track := range file.AudioTracks {
		fmt.Fprintf(&sb, "%d|%s|%s|%d|%s;", track.Index,
			strings.ToLower(strings.TrimSpace(track.Language)),
			strings.ToLower(strings.TrimSpace(track.Codec)),
			track.Channels, strings.ToLower(strings.TrimSpace(track.Layout)))
	}
	return sb.String()
}

// reconcileProbeVerifiedV3 gates reconciliation on verified probe
// provenance: only a row whose probe stamp the catalog committed may move
// the executable selection. Declared-only rows carry no ground truth, so a
// reorder there is not evidence — it is absence of evidence.
func reconcileProbeVerifiedV3(file *models.MediaFile) bool {
	return file != nil && file.ProbeUpdatedAt != nil && !file.ProbeUpdatedAt.IsZero()
}

// audioReconcileSettled reports whether the generation already has a settled
// ledger entry. The ledger is read through the AudioReconcileStoreV3
// capability when the store offers it; a store that predates the ledger
// (legacy wrapper) reports unset, and the reconcile path runs unledgered
// exactly as before.
func (h *PlaybackHandler) audioReconcileSettled(ctx context.Context, sessionID, generation string) bool {
	if h == nil || h.PlanStoreV3 == nil || generation == "" {
		return false
	}
	store, ok := h.PlanStoreV3.(playback.AudioReconcileStoreV3)
	if !ok {
		return false
	}
	ledger, _, err := store.GetAudioReconcileLedger(ctx, sessionID)
	if err != nil {
		return false
	}
	return playback.FindAudioReconcileEntry(ledger, generation) != nil
}

// recordAudioReconcileDecision settles one generation in the durable ledger
// with bounded revision-conflict retry: concurrent writers converge on the
// union, and a replayed generation is a no-op returning the stored entry.
// A store without the ledger capability (legacy wrapper) keeps the
// reconcile path working unledgered rather than failing the probe write.
func (h *PlaybackHandler) recordAudioReconcileDecision(ctx context.Context, sessionID, generation string, entry playback.AudioReconcileEntryV3) {
	if h == nil || h.PlanStoreV3 == nil || generation == "" {
		return
	}
	store, ok := h.PlanStoreV3.(playback.AudioReconcileStoreV3)
	if !ok {
		return
	}
	ledger, revision, err := store.GetAudioReconcileLedger(ctx, sessionID)
	if err != nil {
		return
	}
	if playback.FindAudioReconcileEntry(ledger, generation) != nil {
		return
	}
	for attempt := 0; attempt < reconcileLedgerMaxAttempts; attempt++ {
		if _, _, err := store.RecordAudioReconciliation(ctx, sessionID, revision, entry); err == nil {
			return
		} else if !errors.Is(err, playback.ErrRecoveryRevisionConflictV3) {
			return
		} else {
			current, currentRevision, readErr := store.GetAudioReconcileLedger(ctx, sessionID)
			if readErr != nil {
				return
			}
			if playback.FindAudioReconcileEntry(current, generation) != nil {
				return
			}
			revision = currentRevision
		}
	}
}

// reconcileLedgerMaxAttempts bounds the revision-conflict retry when two
// replicas settle the same generation at once. The append is monotone and
// idempotent per generation, so a handful of retries converges; the bound
// keeps a pathological writer loop from spinning the probe path.
const reconcileLedgerMaxAttempts = 8

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
	return defaultAudioLanguageStillSatisfied(selected, verified, recomputed, intent)
}

// audioTransportFactsEqual is the transport reuse guard: the existing
// transport (and any fixed ffmpeg audio map on it) is only byte-identical
// when the chosen stream decodes the same codec with the same channel
// layout. When these differ the caller must not reuse the map; it issues a
// fresh recipe via the automatic replan instead.
//
// Executor replacement is deliberately not decided here. A fixed ffmpeg
// audio map naming a moved stream needs worker replacement via a fresh
// recipe, and that call belongs to the replan path (sidecarOnlyReuseReplanV3
// plus the executor check in executeReplanV3), which sees the full route —
// delivery, executor support for the operation, and transport-compat —
// rather than this predicate's codec/layout slice. The guard only answers
// "same bytes would flow"; the replan answers "same worker may serve them".
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
// keeps the executable outcome the intent would choose: it must be the track
// SelectAudioTrack itself returns for the verified inventory. A bare
// MULTI/DUAL never counts as a concrete match, a member list entry does
// (see the shared membership helper). Preference-unavailable is not a
// separate failure mode here — the selector already fell back — so the
// comparison is pure executable equality, and a fallback that lands on the
// same stream is a no-op rather than a spurious replan.
func defaultAudioLanguageStillSatisfied(selected models.AudioTrack, verified []models.AudioTrack, recomputed int, intent audioSelectionIntent) bool {
	if recomputed < 0 || recomputed >= len(verified) {
		return false
	}
	if !audioTrackSignatureEqual(selected, *playback.AudioTrackSignatureFromTrack(verified[recomputed])) {
		return false
	}
	return intent.preferredLang == "" || playback.TrackCarriesLanguage(selected, intent.preferredLang)
}

// reconcileDefaultAudioReplan issues the automatic track_change replan built
// from the recipe, remapped onto the verified order, preserving position,
// with no route exclusion and no failure classification (track_change must
// not carry one). The replan-request id names the reconciliation reason so
// the durable lease history attributes the change to the server, not the
// viewer; no new operation or client-visible field is introduced.
//
// Delivery: the replan application commits the replacement plan, stream URL
// and transport server-side, then the same inventory_updated push every
// probe persistence already emits tells the player the revision changed.
// The web player folds that push through its existing inventory path
// (applyInventoryUpdate -> inventory poll/replan of its own), so a running
// client adopts the replacement on the channel it already watches; no new
// client surface is required. The settled ledger entry (recorded by the
// caller only after the replan commits) is what stops a duplicate probe
// write from minting a second replan — not the push.
func (h *PlaybackHandler) reconcileDefaultAudioReplan(ctx context.Context, session *playback.Session, record *playback.AttemptRecordV3, verified *models.MediaFile, audioIndex int) (playback.ReplanRequestV3, []byte, error) {
	verifiedIndex := audioIndex
	audioID := playback.TrackIDV3(verified.ID, "audio", verifiedIndex)
	caller := PlaybackCaller{
		UserID:    session.UserID,
		ProfileID: session.ProfileID,
	}
	req := playback.ReplanRequestV3{
		ProtocolVersion:   playback.ProtocolV3,
		Operation:         playback.ReplanOperationTrackChangeV3,
		Automatic:         playback.ReplanAutomaticV3,
		PlaybackAttemptID: record.PlaybackAttemptID,
		ReplanRequestID:   reconcileReplanRequestID(record, verifiedIndex, "req"),
		FailedPlanID:      record.CurrentPlanID,
		PlanAttemptID:     reconcileReplanRequestID(record, verifiedIndex, "plan"),
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
		return playback.ReplanRequestV3{}, nil, err
	}
	response, err := h.ReplanPlaybackV2(ctx, caller, session.ID, PlaybackReplanCommand{Request: req, Digest: ReplanDigestV3(body)})
	if err != nil {
		return playback.ReplanRequestV3{}, nil, err
	}
	_ = response
	slog.InfoContext(ctx, "default audio reconciled to the verified inventory",
		"component", "api", "session", session.ID, "file_id", verified.ID,
		"audio_index", verifiedIndex, "reason", AudioReconciliationReplanReason)
	return req, body, nil
}

// verifyExplicitAudioSelection checks that an explicit viewer selection still
// exists in the verified inventory. It never overrides: a vanished track
// surfaces a terminal diagnostic (route event + log) so the viewer learns the
// selection is gone instead of hearing a silently substituted track.
//
// The event stays diagnostic-only: it names the attempt and plan for
// attribution but changes no attempt or plan state, so the stranded session
// keeps playing its committed route. The player learns of a genuine
// disappearance through the channel it already watches — the inventory_updated
// push (or the next inventory poll) shows the verified list without the
// track, and its picker drops the unplayable entry exactly as it does for any
// other inventory revision. No new client surface is required.
func (h *PlaybackHandler) verifyExplicitAudioSelection(ctx context.Context, session *playback.Session, intent audioSelectionIntent, fileID int) {
	if h == nil || h.fileResolver == nil {
		return
	}
	verified, err := h.fileResolver.GetByID(ctx, fileID)
	if err != nil || verified == nil || len(verified.AudioTracks) == 0 {
		return
	}
	if !reconcileProbeVerifiedV3(verified) {
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
	generation := reconcileGenerationV3(verified)
	if h.audioReconcileSettled(ctx, live.ID, generation) {
		return
	}
	// Match track identity across the whole verified inventory, not just
	// the old array index: a reorder moves the explicit track without
	// removing it, and only a genuinely absent signature is missing.
	if explicitAudioSelectionPresent(verified.AudioTracks, committedAudioTrackIndexV3(record, live), intent.selectedOverride) {
		h.recordAudioReconcileDecision(ctx, live.ID, generation, playback.AudioReconcileEntryV3{Generation: generation, Decision: playback.AudioReconcileNoop})
		return
	}
	slog.WarnContext(ctx, "explicit audio selection missing from the verified inventory",
		"component", "api", "session", live.ID, "file_id", fileID, "audio_index", committedAudioTrackIndexV3(record, live))
	h.recordAudioReconcileDecision(ctx, live.ID, generation, playback.AudioReconcileEntryV3{Generation: generation, Decision: playback.AudioReconcileRefused})
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

// explicitAudioSelectionPresent reports whether the committed explicit
// selection still exists anywhere in the verified inventory: the committed
// index first (the common unchanged case), then a full signature scan so a
// reordered-but-present track is not falsely reported missing. A nil or zero
// signature means start predates signatures: index presence alone decides.
func explicitAudioSelectionPresent(tracks []models.AudioTrack, committedIndex int, selected *userstore.AudioTrackSignature) bool {
	if track, ok := audioTrackAtCommittedIndex(tracks, committedIndex); ok {
		if selected == nil || selected.IsZero() || audioTrackSignatureEqual(track, *selected) {
			return true
		}
		for _, candidate := range tracks {
			if audioTrackSignatureEqual(candidate, *selected) {
				return true
			}
		}
		return false
	}
	if selected == nil || selected.IsZero() {
		return false
	}
	for _, candidate := range tracks {
		if audioTrackSignatureEqual(candidate, *selected) {
			return true
		}
	}
	return false
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
//
// Bounded like the probe-landing path: the verified-provenance gate and the
// settled-ledger check inside reconcileSessionDefaultAudio make a settled
// generation a no-op, so heartbeats converge instead of re-evaluating every
// tick. The store read below runs inside the reconcile budget (not outside
// it), and a record-fallback trigger covers rebuilds and remote sessions:
// the attempt record is the cross-replica rendezvous, so a session the
// owning replica never saw still converges when this replica attaches it
// and finds intent on the shared row. No cross-replica RPC is involved.
func (h *PlaybackHandler) reconcilePendingAudioStartup(ctx context.Context, sessionID string) {
	if h == nil || h.sessionMgr == nil || h.PlanStoreV3 == nil || sessionID == "" {
		return
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, reconcileProbeBudgetV3)
	defer cancel()
	session, err := h.sessionMgr.GetSession(sessionID)
	if err != nil || session == nil {
		return
	}
	record, err := h.PlanStoreV3.GetAttempt(deadlineCtx, sessionID)
	if err != nil || record == nil {
		return
	}
	if strings.TrimSpace(record.SelectionOrigin) == "" {
		return
	}
	h.reconcileSessionDefaultAudio(deadlineCtx, session, record.EffectiveMediaFileID)
}

// reconcileReplanRequestID mints a generation-aware identity for one
// automatic reconciliation decision: the reason prefix attributes the
// change, the failed-plan id binds it to the exact plan generation it was
// decided against, and the target index names the decision. The same
// decision therefore always replays under the same key with the same body
// (the replan lease answers with the stored response instead of colliding),
// while a new decision — a different index, or a later plan generation —
// mints a new key. It never depends on the session id being a UUID.
func reconcileReplanRequestID(record *playback.AttemptRecordV3, audioIndex int, kind string) string {
	failedPlan := ""
	if record != nil {
		failedPlan = record.CurrentPlanID
	}
	sum := sha256.Sum256([]byte(failedPlan))
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
