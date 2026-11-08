package handlers

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

const (
	// subtitleSelectionBreakerMaxSessions bounds the per-handler tracker. The
	// map holds one entry per live session that has had an unhonorable subtitle
	// pick rejected, so the cap is far above a normal deployment's live session
	// count and still bounds a leaked or never-stopped session.
	subtitleSelectionBreakerMaxSessions = 4096
	// subtitleSelectionBreakerTTL is the backstop for a session whose stop was
	// handled by another replica or whose end event never reached this one.
	// An entry older than this is treated as absent.
	subtitleSelectionBreakerTTL = 24 * time.Hour
	// replanReasonRepeatedUnhonorableSubtitleV3 names the no-op in the replan
	// answered log. It is a log value, never part of the wire contract.
	replanReasonRepeatedUnhonorableSubtitleV3 = "repeated_unhonorable_subtitle"
	// Replan log keys local to this path, kept out of the literals so the
	// goconst changed-line gate stays quiet without reusing an unrelated
	// virtual-source constant.
	subtitleLoopLogKeyOutcome         = "outcome"
	subtitleLoopLogKeyReason          = "reason"
	subtitleLoopLogKeyEffectiveFileID = "effective_file_id"
)

// subtitleSelectionKeyV3 is the raw subtitle pick a track_change replan carried,
// scoped to the effective file it named. The identity is deliberately the
// client's raw id/index rather than a resolved ordinal: the breaker exists for
// clients that replay exactly the same rejected request.
type subtitleSelectionKeyV3 struct {
	fileID  int
	trackID string
	index   *int
}

func subtitleSelectionKeyForRequestV3(fileID int, track *playback.TrackIdentityV3) (subtitleSelectionKeyV3, bool) {
	if track == nil {
		return subtitleSelectionKeyV3{}, false
	}
	return subtitleSelectionKeyV3{
		fileID:  fileID,
		trackID: track.ID,
		index:   copyOptionalIntV3(track.Index),
	}, true
}

func (k subtitleSelectionKeyV3) matches(other subtitleSelectionKeyV3) bool {
	return k.fileID == other.fileID && k.trackID == other.trackID && optionalIntEqualV3(k.index, other.index)
}

type subtitleSelectionBreakerEntryV3 struct {
	key      subtitleSelectionKeyV3
	recorded time.Time
}

// subtitleSelectionBreakerV3 remembers the last subtitle pick a track_change
// replan rejected with selection_unavailable for each playback session. The
// first rejection is returned to the client so a capable client can stop; an
// identical repeat returns the current plan unchanged instead of minting a new
// plan and rebuilding the transport. It is per session and bounded.
type subtitleSelectionBreakerV3 struct {
	mu      sync.Mutex
	entries map[string]subtitleSelectionBreakerEntryV3
}

// repeated reports whether sessionID's last rejected subtitle pick is exactly
// key and still inside the TTL.
func (b *subtitleSelectionBreakerV3) repeated(sessionID string, key subtitleSelectionKeyV3, now time.Time) bool {
	if b == nil || sessionID == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.entries[sessionID]
	if !ok {
		return false
	}
	if now.Sub(entry.recorded) > subtitleSelectionBreakerTTL {
		delete(b.entries, sessionID)
		return false
	}
	return entry.key.matches(key)
}

// record remembers key as sessionID's last rejected subtitle pick. The entry is
// written after the rejection plan commits, so a failed replan does not arm the
// no-op for a plan the client never received.
func (b *subtitleSelectionBreakerV3) record(sessionID string, key subtitleSelectionKeyV3, now time.Time) {
	if b == nil || sessionID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entries == nil {
		b.entries = make(map[string]subtitleSelectionBreakerEntryV3)
	}
	if _, exists := b.entries[sessionID]; !exists && len(b.entries) >= subtitleSelectionBreakerMaxSessions {
		b.evictOldestLocked()
	}
	b.entries[sessionID] = subtitleSelectionBreakerEntryV3{key: key, recorded: now}
}

// forget drops a session's entry when the session ends or stops.
func (b *subtitleSelectionBreakerV3) forget(sessionID string) {
	if b == nil || sessionID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, sessionID)
}

func (b *subtitleSelectionBreakerV3) size() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}

func (b *subtitleSelectionBreakerV3) evictOldestLocked() {
	oldestID := ""
	var oldest time.Time
	for sessionID, entry := range b.entries {
		if oldestID == "" || entry.recorded.Before(oldest) {
			oldestID = sessionID
			oldest = entry.recorded
		}
	}
	if oldestID != "" {
		delete(b.entries, oldestID)
	}
}

// replanSubtitleLoopBreakerV3 answers a repeated unhonorable subtitle pick with
// the durable plan unchanged. It returns handled=true when the caller must
// return response as-is; otherwise recordAfterCommit, when non-nil, arms the
// breaker once the rejection plan commits.
func (h *PlaybackHandler) replanSubtitleLoopBreakerV3(
	r *http.Request,
	record *playback.AttemptRecordV3,
	req playback.ReplanRequestV3,
	operation playback.ReplanOperationV3,
	trackChange bool,
	effectiveFile *models.MediaFile,
	plan *playback.PlanV3,
	clientInfo playback.ClientInfo,
) (playback.DecisionResponseV3, func(), bool) {
	if h == nil || record == nil || !trackChange {
		return playback.DecisionResponseV3{}, nil, false
	}
	if req.SelectedTracks.Subtitle == nil || effectiveFile == nil || plan == nil ||
		plan.Claims.Subtitles.Reason != playback.SubtitleReasonSelectionUnavailableV3 {
		// Any other track change moves the selection: the old rejection can no
		// longer describe what the viewer asked for, so a later repeat of it is
		// a fresh first occurrence.
		h.subtitleSelectionBreaker.forget(record.SessionID)
		return playback.DecisionResponseV3{}, nil, false
	}
	key, ok := subtitleSelectionKeyForRequestV3(effectiveFile.ID, req.SelectedTracks.Subtitle)
	if !ok {
		return playback.DecisionResponseV3{}, nil, false
	}
	otherIntent := trackChangeCarriesOtherIntentV3(req, record)
	if !otherIntent && h.subtitleSelectionBreaker.repeated(record.SessionID, key, time.Now()) {
		current := record.CurrentPlan
		current.SessionID = record.SessionID
		current.InventoryURL = "/api/v2/playback/" + record.SessionID + "/inventory"
		response := playback.DecisionResponseV3{
			ProtocolVersion: playback.ProtocolV3,
			ServerFeatures:  serverFeaturesForRequestV3(r.Context()),
			Outcome:         playback.OutcomePlayableV3,
			SessionID:       record.SessionID,
			PlaybackPlan:    &current,
		}
		attrs := []any{
			logComponentKey, playbackLogValueV3,
			subtitleLoopLogKeyOutcome, "no_change",
			subtitleLoopLogKeyReason, replanReasonRepeatedUnhonorableSubtitleV3,
			replanLogKeyOperationV3, string(operation),
			replanLogKeySessionIDV3, record.SessionID,
			subtitleLoopLogKeyEffectiveFileID, effectiveFile.ID,
			"subtitle_track_id", req.SelectedTracks.Subtitle.ID,
			"subtitle_track_index", intOrZeroHandlerV3(req.SelectedTracks.Subtitle.Index),
		}
		attrs = append(attrs, clientInfo.LogAttrs()...)
		slog.InfoContext(r.Context(), "playback replan answered unchanged", attrs...)
		markReplanDecisionLoggedV3(r.Context())
		return response, nil, true
	}
	// Remember the rejection even when this request also carries another
	// intent: the committed plan has still rejected this pick, so a later
	// request that repeats only it can be folded.
	return playback.DecisionResponseV3{}, func() {
		h.subtitleSelectionBreaker.record(record.SessionID, key, time.Now())
	}, false
}

// trackChangeCarriesOtherIntentV3 reports whether a track_change replan asks for
// anything besides the subtitle pick itself. The breaker may only fold a
// repeated unhonorable subtitle into the current plan when the request changes
// nothing else: an audio pick, a quality change, or a new output target still
// has to be planned.
func trackChangeCarriesOtherIntentV3(req playback.ReplanRequestV3, record *playback.AttemptRecordV3) bool {
	if record == nil {
		return true
	}
	if req.Failure.Classification == failureAudioTrackChangedV3 {
		return true
	}
	if req.SelectedTracks.Audio != nil {
		current := record.CurrentPlan.SelectedTracks.Audio
		if current == nil || req.SelectedTracks.Audio.ID != current.ID ||
			!optionalIntEqualV3(req.SelectedTracks.Audio.Index, current.Index) {
			return true
		}
	}
	if quality := strings.TrimSpace(req.QualityPreference); quality != "" {
		normalized, _ := playback.NormalizeQualityV3(quality)
		if normalized != record.NormalizedRequest.QualityPreference {
			return true
		}
	}
	if output := strings.TrimSpace(req.ClientPlaybackContext.Output.OutputContextID); output != "" &&
		output != record.NormalizedRequest.ClientPlaybackContext.Output.OutputContextID {
		return true
	}
	return false
}
