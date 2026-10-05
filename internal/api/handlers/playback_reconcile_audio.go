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

	"github.com/google/uuid"

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

// audioReconcileInvalidationDeadline is the deadline the withdrawal command
// carries. The client acknowledges the command and answers it with the result
// of its own replacement replan, which plans from scratch, so the deadline
// bounds that replan rather than a transport change on this side.
const audioReconcileInvalidationDeadline = 8 * time.Second

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
// inventory and, when the executable selection moved, persists the corrected
// decision and withdraws the active plan so the client replans onto it.
// Identical executable selection is a byte-equal no-op.
//
// The server never commits the replacement itself. Reconciliation corrects the
// durable inventory and records one canonical request; the plan replacement is
// the client's own replan, which captures its live position and pause state and
// lands on the corrected audio index. Two competing commit paths (a
// server-committed recipe plus a client replan) would race for the same
// transport, so there is exactly one: this one records and asks.
//
// Bounded by generation: every verified inventory is keyed by its probe
// generation (the row's probe stamp), and the attempt's settled ledger records
// one decision per (generation, session). A pair with a settled entry is never
// re-evaluated — a retried probe write or a second replica replays the stored
// decision instead of minting a second event — so heartbeats and duplicate
// evidence commits converge rather than emitting unbounded invalidations.
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
	record, err := h.PlanStoreV3.GetAttempt(ctx, live.ID)
	if err != nil || record == nil {
		return
	}
	if entry := playback.FindAudioReconcileEntry(record.AudioReconcileLedger, generation, live.ID); entry != nil {
		// The attempt row already carries the ledger, so this needs no second
		// store read. A settled generation is never re-evaluated and never
		// re-announced: a retried probe write, a second replica or a retried
		// heartbeat replays the stored decision instead of emitting a second
		// event for one correction. A delivery that failed had no client to
		// receive it, and that client's next start or reconnect plans against
		// the corrected inventory anyway.
		return
	}
	if strings.TrimSpace(record.SelectionOrigin) != "" && strings.TrimSpace(record.SelectionOrigin) != SelectionOriginAuto {
		// A racing explicit change persisted first: never override.
		h.recordAudioReconcileDecision(ctx, live.ID, generation, playback.AudioReconcileEntryV3{Generation: generation, SessionID: live.ID, Decision: playback.AudioReconcileRefused})
		return
	}
	if !virtualEvidenceMatchesBoundFile(verified, live) {
		// Source rotation (or a sibling-row write) means this evidence is
		// stale for the bound session: refuse, and surface the still-valid
		// tracks in memory only, exactly like the refused-write path.
		h.publishRefusedProbeInventory(ctx, fileID, strings.TrimSpace(live.VirtualSourceURI), verified)
		slog.InfoContext(ctx, "default audio reconciliation refused: stale evidence for the bound candidate",
			"component", "api", "session", session.ID, "file_id", fileID)
		h.recordAudioReconcileDecision(ctx, live.ID, generation, playback.AudioReconcileEntryV3{Generation: generation, SessionID: live.ID, Decision: playback.AudioReconcileRefused})
		return
	}
	// The live session must still carry the committed start selection: a
	// viewer track_change between start and probe supersedes the intent
	// (the race case), and reconciliation must not override the viewer's
	// newer choice. Compare executable signature identity, not the ordinal,
	// because the reorder is exactly what can shift ordinals.
	if !liveSelectionStillCommitted(live, record, intent) {
		h.recordAudioReconcileDecision(ctx, live.ID, generation, playback.AudioReconcileEntryV3{Generation: generation, SessionID: live.ID, Decision: playback.AudioReconcileRefused})
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
			Position:           live.Position,
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
		h.recordAudioReconcileDecision(ctx, live.ID, generation, playback.AudioReconcileEntryV3{Generation: generation, SessionID: live.ID, Decision: playback.AudioReconcileNoop})
		return
	}
	h.settleAudioReconcileInvalidation(ctx, live, record, verified, recomputed)
}

// reconcilePendingAudioStartup re-checks one session when it attaches after
// this replica owns a realtime connection for a client that negotiated the
// replacement-plan handshake, withdraws the active plan so the client replans.
//
// Ordering is the point: the canonical request and the decision it belongs to
// are written in one ledger CAS before anything is emitted. A duplicate probe
// write, a second replica or a retried heartbeat therefore finds the generation
// settled and replays the stored decision instead of emitting a second event,
// and an evaluation interrupted after the write replays the exact stored bytes
// — the same request id and the same digest — so the replan lease answers with
// the stored response rather than refusing the replay as a reused id.
//
// A client that never negotiated the handshake gets no event: the decision is
// still recorded, and the correction lands on its next start or reconnect,
// which plans against the verified inventory anyway.
func (h *PlaybackHandler) settleAudioReconcileInvalidation(ctx context.Context, live *playback.Session, record *playback.AttemptRecordV3, verified *models.MediaFile, audioIndex int) {
	generation := reconcileGenerationV3(verified)
	req, err := h.buildReconcileAudioRequest(live, record, verified, audioIndex)
	if err != nil {
		slog.WarnContext(ctx, "default audio reconciliation request build failed",
			"component", "api", "session", live.ID, "file_id", verified.ID, "error", err)
		return
	}
	digest := ReplanDigestV3(req.bytes)
	audioIndexCopy := audioIndex
	storedRequest := req.request
	stored, settledByThisWriter := h.commitAudioReconcileDecision(ctx, live.ID, generation, playback.AudioReconcileEntryV3{
		Decision:      playback.AudioReconcileInvalidated,
		AudioIndex:    &audioIndexCopy,
		PlanID:        record.CurrentPlanID,
		Request:       &storedRequest,
		RequestDigest: digest,
		Reason:        playback.PlanInvalidatedDefaultAudioReconciliation,
	})
	// Only the writer that actually settled this (generation, session) emits.
	// A replayed generation returns the stored decision, already delivered (or
	// being delivered by the writer that stored it), and a diverged evaluation
	// is refused outright — neither may hand a client a second invalidation for
	// one correction.
	if !settledByThisWriter || stored.RequestDigest != digest || stored.Decision != playback.AudioReconcileInvalidated {
		return
	}
	h.announceAudioReconcileInvalidation(ctx, live, record, stored)
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
	return playback.FindAudioReconcileEntry(ledger, generation, sessionID) != nil
}

// recordAudioReconcileDecision settles one generation in the durable ledger
// with bounded revision-conflict retry: concurrent writers converge on the
// union, and a replayed generation is a no-op returning the stored entry.
// A store without the ledger capability (legacy wrapper) keeps the
// reconcile path working unledgered rather than failing the probe write.
func (h *PlaybackHandler) recordAudioReconcileDecision(ctx context.Context, sessionID, generation string, entry playback.AudioReconcileEntryV3) {
	if entry.SessionID == "" {
		entry.SessionID = sessionID
	}
	_, _ = h.commitAudioReconcileDecision(ctx, sessionID, generation, entry)
}

// commitAudioReconcileDecision settles one generation in the durable ledger
// with bounded revision-conflict retry and reports what the ledger holds
// afterwards, together with whether THIS caller is the writer that settled it.
//
// That distinction is the dedup authority for the invalidation push: the loser
// of a CAS against an already-settled (generation, session) pair replays the
// stored decision without emitting, while the winner emits exactly once. It is
// also where an evaluation that built a different canonical request for a
// settled generation is refused and written down, rather than silently
// competing with the decision a client is already replanning against.
//
// A store without the ledger capability (legacy wrapper) keeps the reconcile
// path working unledgered rather than failing the probe write.
func (h *PlaybackHandler) commitAudioReconcileDecision(ctx context.Context, sessionID, generation string, entry playback.AudioReconcileEntryV3) (playback.AudioReconcileEntryV3, bool) {
	if h == nil || h.PlanStoreV3 == nil || generation == "" {
		return playback.AudioReconcileEntryV3{}, false
	}
	store, ok := h.PlanStoreV3.(playback.AudioReconcileStoreV3)
	if !ok {
		return playback.AudioReconcileEntryV3{}, false
	}
	entry.Generation = generation
	entry.SessionID = sessionID
	ledger, revision, err := store.GetAudioReconcileLedger(ctx, sessionID)
	if err != nil {
		return playback.AudioReconcileEntryV3{}, false
	}
	for attempt := 0; attempt < reconcileLedgerMaxAttempts; attempt++ {
		if existing := playback.FindAudioReconcileEntry(ledger, generation, sessionID); existing != nil {
			if entry.RequestDigest != "" && existing.RequestDigest != entry.RequestDigest {
				// Two evaluations of one generation disagree about the
				// canonical request. The first recorded decision stands:
				// the client may already be replanning against it. Record the
				// refusal so the divergence is durable and auditable, and
				// emit nothing.
				h.recordAudioReconcileDivergence(ctx, store, sessionID, ledger, revision, generation, sessionID, entry)
				slog.WarnContext(ctx, "default audio reconciliation refused: canonical request diverged for a settled generation",
					"component", "api", "session", sessionID, "generation", generation,
					"stored_digest", existing.RequestDigest, "rejected_digest", entry.RequestDigest)
			}
			return *existing, false
		}
		merged, _, writeErr := store.RecordAudioReconciliation(ctx, sessionID, revision, entry)
		switch {
		case writeErr == nil:
			if stored := playback.FindAudioReconcileEntry(merged, generation, sessionID); stored != nil {
				return *stored, true
			}
			return entry, true
		case errors.Is(writeErr, playback.ErrRecoveryRevisionConflictV3):
			ledger, revision, err = store.GetAudioReconcileLedger(ctx, sessionID)
			if err != nil {
				return playback.AudioReconcileEntryV3{}, false
			}
		default:
			slog.WarnContext(ctx, "audio reconciliation decision write failed",
				"component", "api", "session", sessionID, "generation", generation, "error", writeErr)
			return playback.AudioReconcileEntryV3{}, false
		}
	}
	slog.WarnContext(ctx, "audio reconciliation decision gave up on the ledger revision",
		"component", "api", "session", sessionID, "generation", generation)
	return playback.AudioReconcileEntryV3{}, false
}

// recordAudioReconcileDivergence appends the refusal a diverged evaluation
// earns against an already-settled generation. It is deliberately not
// retried: it runs on the path that lost, and a lost CAS there means another
// evaluator recorded the same refusal.
func (h *PlaybackHandler) recordAudioReconcileDivergence(ctx context.Context, store playback.AudioReconcileStoreV3, sessionID string, ledger playback.AudioReconcileLedgerV3, revision int64, generation, settledSession string, entry playback.AudioReconcileEntryV3) {
	refusal := playback.AudioReconcileEntryV3{
		Generation:    generation,
		SessionID:     settledSession,
		Decision:      playback.AudioReconcileRefused,
		PlanID:        entry.PlanID,
		RequestDigest: entry.RequestDigest,
	}
	// A refusal is a SECOND entry for a pair that already has one: it records
	// the rejected digest next to the stored one, so dedup by (generation,
	// session) is not what suppresses it. Its own idempotency key is the
	// rejected digest — a retried evaluation of the same diverged body records
	// nothing further.
	for _, existing := range ledger.Entries {
		if existing.Generation == refusal.Generation &&
			existing.Decision == playback.AudioReconcileRefused &&
			existing.RequestDigest == refusal.RequestDigest {
			return
		}
	}
	_, _, _ = store.RecordAudioReconciliation(ctx, sessionID, revision, refusal)
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
// reconcileAudioRequest is the canonical replacement request a settled
// decision is replayed under: the request value plus the exact bytes its digest
// is taken from, so a retry reuses verbatim what was stored.
type reconcileAudioRequest struct {
	request playback.ReplanRequestV3
	bytes   []byte
}

// buildReconcileAudioRequest builds the automatic track_change replan from the
// committed recipe, remapped onto the verified order, preserving position,
// with no route exclusion and no failure classification (track_change must not
// carry one). The replan-request id names the reconciliation reason so the
// durable lease history attributes the change to the server, not the viewer;
// no new operation or client-visible field is introduced.
//
// The body is built here and persisted with the decision instead of being
// executed. Two properties follow from freezing it here rather than at the
// replan:
//
//   - The stored bytes are the identity. The request id is derived from the
//     plan and target index, both immutable, while the body also carries the
//     live position, which is not. A replay of the stored decision therefore
//     carries the same id AND the same digest, so the replan lease answers
//     with its stored response instead of rejecting a reused id — while two
//     evaluations that disagree about position are caught as a digest
//     mismatch against the settled entry and refused.
//   - Nothing is executed before the decision is durable, so an interrupted
//     evaluation cannot leave a committed recipe with no record of the request
//     that produced it.
func (h *PlaybackHandler) buildReconcileAudioRequest(session *playback.Session, record *playback.AttemptRecordV3, verified *models.MediaFile, audioIndex int) (reconcileAudioRequest, error) {
	verifiedIndex := audioIndex
	audioID := playback.TrackIDV3(verified.ID, "audio", verifiedIndex)
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
		return reconcileAudioRequest{}, err
	}
	if err := req.Validate(); err != nil {
		return reconcileAudioRequest{}, fmt.Errorf("reconcile replan request invalid: %w", err)
	}
	return reconcileAudioRequest{request: req, bytes: body}, nil
}

// sessionNegotiatedPlanInvalidation reports whether this attempt's client
// advertised plan_invalidated_v1 — the promise to handle a mid-session plan
// withdrawal and replan off it.
//
// The attempt row is the authority rather than the live session: the feature is
// attempt-sticky, negotiated once at start, and the withdrawal may outlive the
// in-memory session (a rebuilt one from the card). A legacy row without the
// field never negotiated it, and an unnegotiated client must not be pushed a
// command it never promised to handle — its correction lands on the next start
// or reconnect instead.
func (h *PlaybackHandler) sessionNegotiatedPlanInvalidation(ctx context.Context, sessionID string) bool {
	if h == nil || h.PlanStoreV3 == nil {
		return false
	}
	record, err := h.PlanStoreV3.GetAttempt(ctx, sessionID)
	if err != nil || record == nil {
		return false
	}
	return playback.HasFeatureV3(record.NormalizedRequest.ClientFeatures, playback.FeaturePlanInvalidatedV3)
}

// sessionNegotiatedDefaultAudioReconcileResponse reports whether this
// attempt's client advertised the response-side capability for the
// default-audio reconciliation withdrawal. It is a stricter gate than
// sessionNegotiatedPlanInvalidation: a client that advertises only
// plan_invalidated_v1 handles every withdrawal as a route failure and folds
// the invalidated attempt key into attempted_plan_keys, which would exclude
// a perfectly healthy route from its own replacement. Such a client gets NO
// withdrawal; its decision is still recorded, and the correction lands on
// its next start or reconnect, which plans against the verified inventory
// anyway.
func (h *PlaybackHandler) sessionNegotiatedDefaultAudioReconcileResponse(ctx context.Context, sessionID string) bool {
	if h == nil || h.PlanStoreV3 == nil {
		return false
	}
	record, err := h.PlanStoreV3.GetAttempt(ctx, sessionID)
	if err != nil || record == nil {
		return false
	}
	return playback.HasFeatureV3(record.NormalizedRequest.ClientFeatures, playback.FeatureDefaultAudioReconcileResponseV3)
}

// pendingAudioReconciliationReplan consumes the same pending decision the
// withdrawal named: the client's replan, whatever body it carries, replans off
// the withdrawn plan, so it has to select the corrected audio stream or the
// correction never reaches the transport. Applying the stored selection is
// additive on the wire — it fills the audio identity the client omitted — and
// leaves every other field the client sent (position, pause state, attempt
// key, failure classification) exactly as it arrived, which is what preserves
// the viewer's position across the adoption.
//
// Only a client-issued replan off the withdrawn plan consumes it. The server's
// own automatic replan already names the corrected selection, and a replan off
// any other plan is a viewer decision that must not be overridden.
func (h *PlaybackHandler) pendingAudioReconciliationReplan(record *playback.AttemptRecordV3, req *playback.ReplanRequestV3) {
	if record == nil || req == nil || req.Automatic != "" {
		return
	}
	entry := FindPendingAudioReconciliation(record, req.FailedPlanID)
	if entry == nil {
		return
	}
	// Presence is not intent. The web request builder echoes the plan's own
	// audio identity on every replan (buildReplanRequestV3), so the answering
	// request normally carries the pre-reorder selection even though the viewer
	// chose nothing. Only a selection that names something OTHER than the
	// withdrawn plan's audio is a fresh viewer choice, and it supersedes the
	// automatic correction; an inherited echo must not suppress it.
	//
	// Note: the ingress boundary (replanPlaybackApplicationV3) strips a
	// client-supplied Automatic marker because a forged one would impersonate
	// server reconciliation and suppress the viewer's preference persistence.
	// AnswersPlanInvalidation is NOT trust-sensitive in that way: at worst it
	// can cause the server to apply a correction the SERVER itself decided to
	// apply and already announced, so the boundary deliberately does not
	// strip it.
	//
	// The answers_plan_invalidation echo is the authoritative discriminator:
	// when it matches the reason recorded with the pending entry, this request
	// IS the reconciliation response — the client read the withdrawal, chose
	// "apply the server-decided correction" as the answer — and we apply the
	// correction regardless of the echoed audio identity. That matters in the
	// reorder case, where the correction targets the same ordinal the withdrawn
	// plan named and the echoed identity is derived from that ordinal, so a
	// deliberate re-pick of the track the viewer already had is byte-identical
	// to the builder's echo and the identity heuristic alone would either
	// override a real viewer choice or suppress a real echo-lane correction.
	//
	// The identity heuristic stays as the fallback for clients that have not
	// adopted answers_plan_invalidation yet: it is a weaker discriminator (it
	// cannot tell a deliberate re-pick from an inherited echo in the reorder
	// case, as above), but it preserves correct behavior for the common echo
	// case those clients produce.
	isReconciliationAnswer := strings.TrimSpace(req.AnswersPlanInvalidation) != "" &&
		strings.TrimSpace(req.AnswersPlanInvalidation) == strings.TrimSpace(entry.Reason)
	if !isReconciliationAnswer && namesDifferentAudioIdentityV3(req.SelectedTracks.Audio, record.CurrentPlan) {
		slog.Debug("replan keeps the viewer's audio choice over a pending correction",
			"component", "api", "session", record.SessionID, "generation", entry.Generation)
		return
	}
	req.SelectedTracks.Audio = entry.Request.SelectedTracks.Audio
	// Restore server-owned provenance for the correction we are applying. The
	// inbound boundary strips a client-supplied marker, and the client cannot
	// set this one: the automatic correction replays stored intent, so
	// persisting its outcome would launder a server decision into a viewer
	// preference and steer every later start.
	req.Automatic = playback.ReplanAutomaticV3
	slog.Debug("replan consumes the pending default audio correction",
		"component", "api", "session", record.SessionID,
		"generation", entry.Generation, "audio_index", entry.AudioIndex)
}

// namesDifferentAudioIdentityV3 reports whether the replan carries an audio
// identity the withdrawn plan did not select. The web request builder echoes
// the current plan's audio on every replan, so a present identity usually means
// "unchanged", not "chosen": only a genuinely different selection is a viewer
// decision that must survive an automatic correction. A nil identity is
// treated as inherited, because the correction fills it in either way.
func namesDifferentAudioIdentityV3(identity *playback.TrackIdentityV3, plan playback.PlanV3) bool {
	if identity == nil {
		return false
	}
	committed := plan.SelectedTracks.Audio
	if committed == nil {
		return true
	}
	if identity.ID != committed.ID {
		return true
	}
	if identity.Index == nil || committed.Index == nil {
		return false
	}
	return *identity.Index != *committed.Index
}

// FindPendingAudioReconciliation returns the settled automatic correction a
// replan replanning off planID would consume, or nil when there is none.
//
// The entry is NOT consumed here: it is keyed by the plan the withdrawal
// named, and the plan this replan commits becomes current, so the client's next
// replan — off the new plan — finds nothing and plans with whatever selection
// it was built with. That is what makes the correction one-shot rather than a
// permanent override of viewer choice.
func FindPendingAudioReconciliation(record *playback.AttemptRecordV3, planID string) *playback.AudioReconcileEntryV3 {
	if record == nil || planID == "" {
		return nil
	}
	var found *playback.AudioReconcileEntryV3
	for i := range record.AudioReconcileLedger.Entries {
		entry := record.AudioReconcileLedger.Entries[i]
		// A divergence refusal is what a LATER evaluator recorded after
		// losing the claim on an already-settled generation — typically two
		// concurrent evaluations that captured a different playhead. It is
		// not a verdict on the winner: erasing the settled invalidation here
		// would discard a correction that was already announced to the client
		// and that this replan exists to consume. The winning decision stays
		// authoritative; the refusal remains in the ledger as an audit record.
		if entry.Decision != playback.AudioReconcileInvalidated || entry.PlanID != planID {
			continue
		}
		if entry.Request == nil || entry.Request.SelectedTracks.Audio == nil {
			continue
		}
		found = &entry
	}
	return found
}

// announceAudioReconcileInvalidation withdraws the plan the decision was
// recorded against, so the client replans onto the corrected audio index.
//
// It reuses the existing plan_invalidated command and the session's own hub
// lane — the same per-session delivery every other realtime push uses. A lane
// is registered by the replica serving that session's control socket, so this
// reaches the owner without any cross-replica RPC; the same evidence commit
// on another replica is deduplicated by the ledger and emits nothing.
//
// A session whose attempt never negotiated plan_invalidated_v1 gets no event:
// the hub refuses the push before the envelope is built. Its decision stays
// recorded, and the corrected selection lands on its next start or reconnect,
// which plans against the verified inventory anyway.
func (h *PlaybackHandler) announceAudioReconcileInvalidation(ctx context.Context, live *playback.Session, record *playback.AttemptRecordV3, stored playback.AudioReconcileEntryV3) {
	if h == nil || h.RealtimeHub == nil {
		return
	}
	if stored.AudioIndex == nil {
		return
	}
	planID := stored.PlanID
	if planID == "" {
		planID = record.CurrentPlanID
	}
	if planID == "" {
		return
	}
	if !h.sessionNegotiatedDefaultAudioReconcileResponse(ctx, live.ID) ||
		!h.sessionNegotiatedPlanInvalidation(ctx, live.ID) {
		// The withdrawal is gated on the NEW response capability, not merely
		// on plan_invalidated_v1: an older client that advertises only the
		// latter treats every withdrawal as a route failure and folds the
		// invalidated key into attempted_plan_keys, excluding a healthy
		// route from its own replacement. Such a client gets no withdrawal;
		// the correction lands on its next start/reconnect instead.
		slog.DebugContext(ctx, "default audio correction recorded without a withdrawal",
			"component", "api", "session", live.ID, "generation", stored.Generation,
			"audio_index", *stored.AudioIndex)
		return
	}
	command, err := playback.NewPlanInvalidatedCommandForGeneration(
		live.ID,
		uuid.NewString(),
		planID,
		playback.PlanInvalidatedDefaultAudioReconciliation,
		stored.Generation,
	)
	if err != nil {
		slog.WarnContext(ctx, "default audio withdrawal command could not be built",
			"component", "api", "session", live.ID, "error", err)
		return
	}
	// The documented replacement handshake requires a deadline: the client
	// answers the command with the result of its own replacement replan, which
	// plans from scratch.
	if command.DeadlineMS == 0 {
		command.DeadlineMS = int(audioReconcileInvalidationDeadline / time.Millisecond)
	}
	if err := h.RealtimeHub.Send(live.ID, command); err != nil {
		slog.DebugContext(ctx, "default audio withdrawal push undelivered",
			"component", "api", "session", live.ID, "plan_id", planID, "error", err)
		return
	}
	slog.InfoContext(ctx, "default audio reconciled to the verified inventory",
		"component", "api", "session", live.ID, "plan_id", planID,
		"generation", stored.Generation, "audio_index", *stored.AudioIndex,
		"reason", AudioReconciliationReplanReason)
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
		h.recordAudioReconcileDecision(ctx, live.ID, generation, playback.AudioReconcileEntryV3{Decision: playback.AudioReconcileNoop})
		return
	}
	slog.WarnContext(ctx, "explicit audio selection missing from the verified inventory",
		"component", "api", "session", live.ID, "file_id", fileID, "audio_index", committedAudioTrackIndexV3(record, live))
	h.recordAudioReconcileDecision(ctx, live.ID, generation, playback.AudioReconcileEntryV3{Decision: playback.AudioReconcileRefused})
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
