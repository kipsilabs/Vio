package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// Guarded stale-pin recovery for identity-less rows.
//
// Background: a catalog row pinned to a concrete `?result=` id can go stale
// within minutes when the provider renumbers its per-listing result ids. When
// the row carries durable provider identity the resolver re-identifies the same
// release under its new id (the same-release rematch in
// internal/virtuallibrary/resolve.go). An identity-less row has nothing to rematch
// on, so a non-empty listing whose only change is the result id used to terminal
// the cold start even though the release was still listed.
//
// This recovery fires only in the cold-start version-fallback walk (never on the
// session-bound serve/replan path, which must keep refusing) when ALL of:
//   - the row is identity-less (no video hash / GUID / release name);
//   - the live provider listing is non-empty (an EMPTY listing is a transient
//     blackout, not a renumber, and keeps its existing terminal);
//   - the bound pin id is absent from that listing (otherwise the normal resolve
//     owns the candidate);
//   - a live candidate for the SAME content (same provider-neutral path, so never
//     across a different content_id) matches the row's stored evidence
//     fingerprint: size within virtualStalePinSizeTolerance, an equal video codec,
//     and a probed duration within virtualStalePinDurationTolerance when both are
//     known.
//
// When it fires it re-pins IN MEMORY ONLY (the durable catalog row's ?result= is
// never rewritten) and resolves the matched candidate once. If no candidate
// matches, the caller's terminal is preserved.
//
// Mismatch-rate concern: a fingerprint is evidence, not proof. A renumbered
// release whose size drifted past the tolerance, or two releases of one title
// with coincidentally equal size+codec+duration, could be confused. Adopting the
// wrong bytes is worse than a terminal, so the recovery is deliberately one-shot
// and kill-switchable. Every recovery logs at Info with the matched ids; if the
// observed mismatch rate exceeds ~1%, disable it with the kill switch below.
//
// Kill switch: set SILO_DISABLE_VIRTUAL_STALE_PIN_RECOVERY=1 at process start to
// disable the recovery entirely (fast, default-on). Tests override the var
// directly.
var virtualStalePinRecoveryEnabled = os.Getenv("SILO_DISABLE_VIRTUAL_STALE_PIN_RECOVERY") == ""

const (
	// virtualStalePinSizeTolerance is the maximum relative drift allowed between
	// the row's stored file size and the live candidate's declared size. Provider
	// display sizes are rounded, so an exact match is too strict; 5% absorbs the
	// rounding without admitting a different release.
	virtualStalePinSizeTolerance = 0.05
	// virtualStalePinDurationTolerance is the maximum relative drift allowed
	// between the row's stored duration and the candidate's probed duration. It is
	// tighter than the runtime tolerance used for probe plausibility because this
	// gate authorizes adopting different bytes.
	virtualStalePinDurationTolerance = 0.02
	// virtualStalePinListBudget bounds the one provider listing the recovery pays.
	// The walk's own decision budget still bounds the whole start.
	virtualStalePinListBudget = 5 * time.Second
)

// recoverStaleIdentityLessPinV3 attempts the guarded in-memory re-pin described
// above. It returns ok=true only when a fingerprint-matched live candidate was
// resolved; any other outcome leaves the caller's terminal in place. It performs
// exactly one candidate resolve (no loops) and never mutates the catalog pin.
func (h *PlaybackHandler) recoverStaleIdentityLessPinV3(
	r *http.Request,
	file *models.MediaFile,
	profileID string,
	req playback.StartRequestV3,
	bandwidthCapKbps int,
) (resolvedVirtualPlaybackSource, bool) {
	if !virtualStalePinRecoveryEnabled || h == nil || file == nil || r == nil {
		return resolvedVirtualPlaybackSource{}, false
	}
	if h.VirtualPlaybackStreamLister == nil || h.VirtualMediaDetailedResolver == nil {
		return resolvedVirtualPlaybackSource{}, false
	}
	// Cold start only, never a session-bound resolve. This function is called
	// from the fresh-start version-fallback walk, which is unbound by
	// construction, but the session binding is the one contract a stale-pin
	// re-pin must never touch: a session already serving a release keeps
	// refusing an absent pin so the serve layer's rotation policy owns it. The
	// guard makes that invariant explicit and testable rather than implicit in
	// the caller's wiring.
	if VirtualSessionBinding(r.Context()) {
		slog.DebugContext(r.Context(), "virtual stale-pin recovery skipped: session-bound resolve",
			"component", "api", "file_id", file.ID, "candidate_uri", file.FilePath)
		return resolvedVirtualPlaybackSource{}, false
	}
	pinnedID := virtualResultCandidateID(file.FilePath)
	if pinnedID == "" {
		// A neutral row has no pin to go stale.
		return resolvedVirtualPlaybackSource{}, false
	}
	// Identity-less only. A row with durable identity is already re-identified
	// by the resolver's same-release rematch; running here too would second-guess
	// a stronger signal with a weaker one.
	if _, hasIdentity := persistedVirtualIdentity(file); hasIdentity {
		return resolvedVirtualPlaybackSource{}, false
	}
	neutralKey := virtualPlaybackNeutralKey(file.FilePath)
	if neutralKey == "" || neutralKey == file.FilePath {
		return resolvedVirtualPlaybackSource{}, false
	}
	ctx := r.Context()
	userID := apimw.GetUserID(ctx)
	listCtx, cancel := context.WithTimeout(ctx, virtualStalePinListBudget)
	streams, err := h.VirtualPlaybackStreamLister.ListVirtualPlaybackStreams(
		listCtx, neutralKey, userID, profileID, file.VirtualOwnerInstallationID,
	)
	cancel()
	if err != nil || len(streams) == 0 {
		// An empty (or failed) listing is a transient outage, not a renumber.
		// Preserve the caller's terminal.
		return resolvedVirtualPlaybackSource{}, false
	}
	// The pin must be absent from the live listing. If it is still listed the
	// ordinary resolve owns it and a recovery would only mask a different fault.
	for _, stream := range streams {
		if stream.ID == pinnedID || stream.URI == file.FilePath {
			return resolvedVirtualPlaybackSource{}, false
		}
	}
	// Same-content candidates only: filterVirtualPlaybackStreams drops any
	// candidate whose scheme/host/path differs from the row's, so an adoption
	// can never cross a content_id (or a provider).
	candidates := filterVirtualPlaybackStreams(file, streams)
	if len(candidates) == 0 {
		return resolvedVirtualPlaybackSource{}, false
	}
	match, found := virtualStalePinBestStreamMatch(file, candidates)
	if !found {
		slog.DebugContext(ctx, "virtual stale-pin recovery found no fingerprint match",
			"component", "api", "file_id", file.ID, "old_candidate_id", pinnedID,
			"candidate_count", len(candidates))
		return resolvedVirtualPlaybackSource{}, false
	}
	// Re-pin in memory and resolve the matched candidate exactly once. The row's
	// durable ?result= is never rewritten; only process state moves.
	matchedID := virtualResultCandidateID(match.FilePath)
	recovered, resolveErr := h.resolveVirtualPlaybackSource(
		r, &match, profileID, false, nil, "", req.QualityPreference, bandwidthCapKbps, false,
		virtualResolveOptionsV3{sessionBound: false, bypassProviderFloor: true},
	)
	if resolveErr != nil || recovered.File == nil {
		return resolvedVirtualPlaybackSource{}, false
	}
	// Refuse a substitution: the recovery authorized serving the exact matched
	// candidate, so a resolve that returned a different result id (a sibling the
	// resolver preferred) must not be adopted under the re-pin. This keeps the
	// recovery's blast radius to the candidate the fingerprint actually matched.
	if got := virtualResultCandidateID(recovered.URI); got != "" && matchedID != "" && got != matchedID {
		slog.DebugContext(ctx, "virtual stale-pin recovery refused a substituted candidate",
			"component", "api", "file_id", file.ID, "old_candidate_id", pinnedID,
			"matched_candidate_id", matchedID, "resolved_candidate_id", got)
		return resolvedVirtualPlaybackSource{}, false
	}
	if !virtualStalePinDurationMatch(file.Duration, recovered.File.Duration) {
		slog.DebugContext(ctx, "virtual stale-pin recovery rejected a duration mismatch",
			"component", "api", "file_id", file.ID, "old_candidate_id", pinnedID,
			"new_candidate_id", virtualResultCandidateID(recovered.URI),
			"row_duration", file.Duration, "candidate_duration", recovered.File.Duration)
		return resolvedVirtualPlaybackSource{}, false
	}
	slog.InfoContext(ctx, "virtual stale-pin recovery re-pinned an identity-less candidate in memory",
		"component", "api", "file_id", file.ID, "old_candidate_id", pinnedID,
		"new_candidate_id", virtualResultCandidateID(recovered.URI),
		"virtual_uri", recovered.URI)
	return recovered, true
}

// virtualStalePinBestStreamMatch returns the best live candidate whose declared
// evidence matches the row's stored fingerprint, re-pinned as a copy of the row.
// "Best" is the first match in provider rank order: the listing is already
// ranked, so the first fingerprint match is the strongest one, and a single
// deterministic choice keeps the recovery one-shot. It reports ok=false when no
// candidate matches.
//
// The returned copy keeps the row's catalog id, its declared fingerprint fields
// (size, codec, resolution, duration) — those are what the match was made
// against — and the row's own lifecycle fields (verdict, delivery stamp). Every
// field that describes the OLD pinned bytes is cleared: the stored provider URL
// and its headers, the durable identity tiers, and the probe evidence. Without
// this the re-pinned candidate would inherit the old pin's URL (serving the old
// bytes under the new id) and its probe stamp (reporting the old inventory as
// verified for the new candidate). The cleared state makes the caller resolve
// and probe the matched candidate afresh, which is the only thing that may
// authorize serving its bytes or inventory.
func virtualStalePinBestStreamMatch(row *models.MediaFile, candidates []VirtualPlaybackStream) (models.MediaFile, bool) {
	for _, candidate := range candidates {
		if !virtualStalePinStreamFingerprintMatch(row, candidate) {
			continue
		}
		match := *row
		match.FilePath = candidate.URI
		// Keep the row's own identity for the in-memory re-pin: the live candidate
		// has no catalog row yet, and adopting its row id would fabricate one. Only
		// the served path changes.
		match.VirtualOwnerInstallationID = effectiveVirtualOwner(candidate.OwnerInstallationID, row.VirtualOwnerInstallationID)
		clearVirtualStalePinOldCandidateState(&match)
		return match, true
	}
	return models.MediaFile{}, false
}

// clearVirtualStalePinOldCandidateState drops the fields a re-pinned row must
// not carry from its previous pin. It is deliberately narrower than
// clearVirtualCandidateDeclaredMetadata: the row's declared fingerprint fields
// (size, codec, resolution, duration) are kept because they are the evidence the
// match was made against and drive candidate ranking, while the previous pin's
// transport and probe evidence are cleared so the matched candidate is resolved
// and probed on its own merits.
//
// The verdict (failed_at) and delivery (last_delivered_at) lifecycle fields are
// deliberately left in place: they describe the row's own lifecycle, not the
// bytes a candidate names, and the recovery must not alter verdict semantics.
// Clearing the stored URL is what matters most: without it the durable-resume
// fast path would serve the previous pin's URL under the new candidate's id.
func clearVirtualStalePinOldCandidateState(file *models.MediaFile) {
	if file == nil {
		return
	}
	// The stored URL and its headers name the previous pin's bytes.
	file.ResolvedURL = ""
	file.ResolvedURLExpiresAt = nil
	file.ProviderRequestHeaders = nil
	// Durable identity tiers (empty on an identity-less row, cleared for safety
	// so a partially-populated legacy row cannot leak them onto the new pin).
	file.ProviderVideoHash = ""
	file.ProviderGUID = ""
	file.ProviderReleaseName = ""
	file.ProviderReleaseSize = 0
	// Probe evidence describes the previous pin's bytes; serving it as verified
	// for the new candidate would be the premature-verified failure.
	file.ProbeUpdatedAt = nil
	file.ProbeSource = ""
	file.VideoTracks = nil
	file.AudioTracks = nil
	file.SubtitleTracks = nil
	file.ExternalSubtitles = nil
}

// virtualStalePinStreamFingerprintMatch is the cheap half of the fingerprint,
// evaluated on the live listing record before any resolve: the size must be
// known on both sides and agree within tolerance, and the video codec must be
// known on both sides and match. An audio codec mismatch is also a veto when both
// sides carry one; an unknown audio codec is neutral, since some providers omit
// it.
func virtualStalePinStreamFingerprintMatch(row *models.MediaFile, candidate VirtualPlaybackStream) bool {
	if row == nil {
		return false
	}
	if row.FileSize <= 0 || candidate.FileSize <= 0 ||
		!virtualStalePinWithinTolerance(row.FileSize, candidate.FileSize, virtualStalePinSizeTolerance) {
		return false
	}
	rowVideo := strings.TrimSpace(row.CodecVideo)
	candidateVideo := strings.TrimSpace(candidate.CodecVideo)
	if rowVideo == "" || candidateVideo == "" || !strings.EqualFold(rowVideo, candidateVideo) {
		return false
	}
	rowAudio := strings.TrimSpace(row.CodecAudio)
	candidateAudio := strings.TrimSpace(candidate.CodecAudio)
	if rowAudio != "" && candidateAudio != "" && !strings.EqualFold(rowAudio, candidateAudio) {
		return false
	}
	return true
}

// virtualStalePinDurationMatch applies the duration half of the fingerprint to a
// probed candidate. It is neutral when either duration is unknown (a probe that
// reported no runtime), so the size and codec tiers decide; when both are known
// the drift must stay inside the tolerance.
func virtualStalePinDurationMatch(rowDuration, candidateDuration int) bool {
	if rowDuration <= 0 || candidateDuration <= 0 {
		return true
	}
	return virtualStalePinWithinTolerance(int64(rowDuration), int64(candidateDuration), virtualStalePinDurationTolerance)
}

// virtualStalePinWithinTolerance reports whether two positive quantities agree
// within a relative tolerance, using the larger as the denominator so the test
// is symmetric.
func virtualStalePinWithinTolerance(a, b int64, tolerance float64) bool {
	if a <= 0 || b <= 0 {
		return false
	}
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	largest := a
	if b > largest {
		largest = b
	}
	return float64(diff)/float64(largest) <= tolerance
}
