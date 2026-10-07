package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
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
//   - EXACTLY ONE live candidate for the SAME content (same provider-neutral
//     path, so never across a different content_id) matches the row's stored
//     evidence fingerprint: size within virtualStalePinSizeTolerance, an equal
//     video codec, no conflicting known resolution or audio-language signal, and
//     a probed duration within virtualStalePinDurationTolerance when both are
//     known.
//
// When it fires it re-pins IN MEMORY ONLY: the durable catalog row's ?result= is
// never rewritten. Replacement evidence is persisted only against a row that
// already verifiably owns the matched candidate's concrete path (an alternate
// owner), and then as metadata only; when no such row exists the recovery leaves
// no durable trace. It resolves the matched candidate once. If no candidate
// matches, or more than one does, the caller's terminal is preserved.
//
// Mismatch-rate concern: a fingerprint is evidence, not proof. A single matching
// candidate is a heuristic, not a proof that it is the same release: same-title
// releases share codec, size and runtime, so size+codec cannot separate language
// or edition variants. When more than one candidate identity matches, the
// recovery refuses rather than rank-picking, because adopting the wrong bytes is
// worse than a terminal. The recovery is deliberately one-shot and
// kill-switchable. Every recovery logs at Info with the matched ids; if the
// observed mismatch rate exceeds ~1%, disable it with the kill switch below.
//
// Kill switch: set SILO_DISABLE_VIRTUAL_STALE_PIN_RECOVERY to any non-empty
// value (conventionally 1) at process start to disable the recovery entirely
// (fast, default-on). Tests override the var directly.
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
)

// virtualStalePinRecoveryBudget bounds the entire stale-pin recovery: its one
// provider listing plus the single exact-candidate resolve and probe. It is a
// small fraction of the version-fallback walk's decision budget so a slow
// recovery yields the remaining time to the healthy alternate rows the walk
// still has to try; a recovery that cannot finish inside this window is
// abandoned and the walk proceeds to the alternates. Without this sub-budget a
// synchronous recovery could consume the whole parent budget on one listing and
// starve every alternate. It is a var so tests can pin it.
var virtualStalePinRecoveryBudget = 500 * time.Millisecond

// stalePinCandidateResolutionV3 is the outcome of the recovery's single
// exact-candidate resolve+probe: the probed file (its Duration is the value the
// probe measured, never a copied or stale value), the canonical candidate URI,
// and the transport facts the caller needs to pin and persist it.
type stalePinCandidateResolutionV3 struct {
	file                *models.MediaFile
	uri                 string
	url                 string
	headers             map[string]string
	providerVideoHash   string
	providerGUID        string
	providerReleaseName string
	providerReleaseSize int64
}

// recoverStaleIdentityLessPinV3 attempts the guarded in-memory re-pin described
// above. It returns ok=true only when a fingerprint-matched live candidate was
// resolved; any other outcome leaves the caller's terminal in place.
//
// The recovery obeys three safety contracts:
//   - a single attempt: it resolves the exact re-pinned candidate through a
//     bespoke one-candidate resolve (no sibling iteration, no stale-source
//     fallback), never the general resolver whose candidate loop and fallback
//     would break the promised retry bound;
//   - validation before side effects: identity and duration are checked before
//     anything is persisted or pinned, so a rejected recovery publishes no
//     evidence and leaves no new sticky selection;
//   - measured duration: the duration comparison uses the probe result's own
//     measured runtime, and a no-prober or failed probe is a no-match rather
//     than a comparison against the copied row's stale value.
//
// It never mutates the durable catalog pin.
func (h *PlaybackHandler) recoverStaleIdentityLessPinV3(
	r *http.Request,
	file *models.MediaFile,
	profileID string,
) (resolvedVirtualPlaybackSource, bool) {
	if !virtualStalePinRecoveryEnabled || h == nil || file == nil || r == nil {
		return resolvedVirtualPlaybackSource{}, false
	}
	if h.VirtualPlaybackStreamLister == nil || h.VirtualMediaDetailedResolver == nil {
		return resolvedVirtualPlaybackSource{}, false
	}
	// Cold start only, never a session-bound resolve. The binding intent is the
	// caller's, preserved by the walk: a session already serving a release keeps
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
	// Capture ONE write generation before the recovery does any work, exactly as
	// the general resolver does. Allocating it at publication time would let an
	// earlier-started, later-finishing recovery (or a selection racing it) carry
	// a newer generation than a newer selection that actually finished first, and
	// so overwrite that newer selection's pin and evidence. Allocating here makes
	// the generation order the start order, and every write below carries this
	// token.
	generation := nextVirtualCacheGeneration()
	// The recovery is a declared re-list: it bypasses the provider floor, so it
	// must draw on the same per-listing budget as the sibling fallback and the
	// other floor-bypassing recovery resolves. Without this bound a burst of
	// dead-pin cold starts would re-list a failing provider without the shared
	// backpressure the rest of recovery honors. Acquire before listing; a
	// listing that answers with candidates clears the budget below, matching the
	// fallback's accounting. An exhausted budget preserves the caller's terminal
	// so the walk falls through to the same alternate rows it would otherwise.
	recoveryKey := virtualRecoveryRelistKey(neutralKey, file.VirtualOwnerInstallationID)
	if !virtualRecoveryRelists.allow(recoveryKey) {
		slog.WarnContext(ctx, "virtual stale-pin recovery re-list budget exhausted; preserving the terminal",
			"component", "api", "file_id", file.ID, "neutral_key", neutralKey)
		return resolvedVirtualPlaybackSource{}, false
	}
	// Bound the whole recovery well under the walk's decision budget so the
	// alternates the walk still has to try keep their time.
	recoveryCtx, cancel := context.WithTimeout(ctx, virtualStalePinRecoveryBudget)
	defer cancel()
	streams, err := h.VirtualPlaybackStreamLister.ListVirtualPlaybackStreams(
		recoveryCtx, neutralKey, userID, profileID, file.VirtualOwnerInstallationID,
	)
	if err != nil || len(streams) == 0 {
		// An empty (or failed) listing is a transient outage, not a renumber.
		// Preserve the caller's terminal. It deliberately does not clear the
		// budget, so a provider that keeps answering [] still accumulates
		// toward the bound instead of being re-listed on every press.
		return resolvedVirtualPlaybackSource{}, false
	}
	// The provider answered with candidates, so this re-list is no longer
	// defeating a provider fail-fast: clear the budget so a later failure starts
	// from a full window. Cleared before the downstream gates below, exactly as
	// the sibling fallback clears once a listing answers.
	virtualRecoveryRelists.clear(recoveryKey)
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
	match, matchedStream, candidatesMatched := virtualStalePinMatchStream(file, candidates)
	if candidatesMatched == 0 {
		slog.DebugContext(ctx, "virtual stale-pin recovery found no fingerprint match",
			"component", "api", "file_id", file.ID, "old_candidate_id", pinnedID,
			"candidate_count", len(candidates))
		return resolvedVirtualPlaybackSource{}, false
	}
	if candidatesMatched != 1 {
		// More than one live candidate identity carries the row's fingerprint.
		// The fingerprint cannot separate same-title releases (shared codec,
		// size and runtime), so every one of them remains a candidate for the
		// pin, and rank-picking would authorize a coin flip. Refuse.
		slog.InfoContext(ctx, "virtual stale-pin recovery refused: ambiguous candidate identities share the row's fingerprint",
			"component", "api", "file_id", file.ID, "old_candidate_id", pinnedID,
			"candidate_count", len(candidates), "matching_identities", candidatesMatched)
		return resolvedVirtualPlaybackSource{}, false
	}
	// Resolve and probe the single matched candidate in one attempt.
	ownerID := effectiveVirtualOwner(match.VirtualOwnerInstallationID, file.VirtualOwnerInstallationID)
	matchedID := virtualResultCandidateID(match.FilePath)
	resolution, resolveErr := h.resolveStalePinCandidateOnceV3(recoveryCtx, &match, matchedID, matchedStream, ownerID, userID, profileID)
	if resolveErr != nil {
		slog.DebugContext(ctx, "virtual stale-pin recovery did not resolve the matched candidate",
			"component", "api", "file_id", file.ID, "old_candidate_id", pinnedID,
			"matched_candidate_id", matchedID, "error", resolveErr)
		return resolvedVirtualPlaybackSource{}, false
	}
	if !virtualStalePinDurationMatch(file.Duration, resolution.file.Duration) {
		slog.DebugContext(ctx, "virtual stale-pin recovery rejected a duration mismatch",
			"component", "api", "file_id", file.ID, "old_candidate_id", pinnedID,
			"new_candidate_id", virtualResultCandidateID(resolution.uri),
			"row_duration", file.Duration, "candidate_duration", resolution.file.Duration)
		return resolvedVirtualPlaybackSource{}, false
	}
	// Validation passed; only now may the recovery touch shared state. Pin the
	// matched candidate for later starts with the generation captured before the
	// listing.
	fingerprint := ""
	if caps, ok := h.requestDeviceCapabilities(r); ok {
		fingerprint = caps.Fingerprint()
	}
	stickyKey := bestResultCacheKey(file.ContentID, neutralKey, file.VirtualOwnerInstallationID, fingerprint)
	h.pinVirtualStickyAt(stickyKey, resolution.uri, generation)
	// Persist the replacement evidence ONLY against a row that already
	// verifiably owns the matched candidate's concrete path. The requested row
	// does not: that is precisely why this recovery runs, and handing the
	// adoption-capable writer the original identity-less row with the
	// replacement URI would take its path-adoption branch and rewrite the
	// durable pin. The recovery is transient: it must never rewrite the
	// requested row's ?result=, so when no alternate owner exists it persists
	// nothing rather than adopt the path.
	if ownerRow, ownerErr := h.virtualPathOwnerRow(recoveryCtx, file, resolution.uri, ownerID, file.MediaFolderID); ownerErr == nil &&
		ownerRow != nil && ownerRow.ID != file.ID {
		// ownerRow already owns this exact path, so a metadata-only write
		// (no AdoptPath) stamps the verified inventory in place without moving
		// any file_path. Pass the owner row so the shared builder cannot fall
		// back to adopting onto a different row.
		h.persistVirtualProbeEvidence(ctx, ownerRow, resolution.uri, resolution.file, true, true)
	}
	slog.InfoContext(ctx, "virtual stale-pin recovery re-pinned an identity-less candidate in memory",
		"component", "api", "file_id", file.ID, "old_candidate_id", pinnedID,
		"new_candidate_id", virtualResultCandidateID(resolution.uri),
		"virtual_uri", resolution.uri)
	return resolvedVirtualPlaybackSource{
		URL:                 resolution.url,
		URI:                 resolution.uri,
		OwnerID:             ownerID,
		File:                resolution.file,
		ProbeSucceeded:      true,
		Provenance:          ProbeProvenanceVerified,
		ResolvedURL:         resolution.url,
		ProviderVideoHash:   resolution.providerVideoHash,
		ProviderGUID:        resolution.providerGUID,
		ProviderReleaseName: resolution.providerReleaseName,
		ProviderReleaseSize: resolution.providerReleaseSize,
		RequestHeaders:      cloneHeaderMap(resolution.headers),
	}, true
}

// resolveStalePinCandidateOnceV3 resolves and probes exactly one candidate for
// the stale-pin recovery. It performs one provider resolve and at most one
// probe, and it has no persistence or sticky-pin side effect: the general
// resolver would iterate siblings and invoke the stale-source fallback before
// returning, and would persist evidence and pin the candidate before the
// recovery's own identity and duration checks ran. The caller validates the
// returned identity and measured duration and only then persists and pins.
func (h *PlaybackHandler) resolveStalePinCandidateOnceV3(
	ctx context.Context,
	match *models.MediaFile,
	matchedID string,
	matchedStream VirtualPlaybackStream,
	ownerID int,
	userID int,
	profileID string,
) (stalePinCandidateResolutionV3, error) {
	// The recovery is a declared re-list: it must see the provider past the
	// fresh-serve floor and the short provider-failure fail-fast, exactly as the
	// general resolver's bypassProviderFloor did, without which a recent
	// background failure would hide the renumbered candidate.
	ctx = virtuallibrary.WithProviderOutageRelist(ctx)
	res, err := h.VirtualMediaDetailedResolver.ResolveVirtualMediaDetailed(
		ctx, match.FilePath, ownerID, userID, profileID, true, nil, "",
	)
	if err != nil {
		return stalePinCandidateResolutionV3{}, err
	}
	if strings.TrimSpace(res.URL) == "" {
		return stalePinCandidateResolutionV3{}, errors.New("stale-pin candidate resolved without a stream URL")
	}
	resolvedURI := strings.TrimSpace(res.URI)
	if resolvedURI == "" {
		return stalePinCandidateResolutionV3{}, errors.New("stale-pin candidate resolved without a concrete URI")
	}
	// The resolver must have returned the exact matched candidate. A returned
	// URI that carries no concrete pick is not the matched identity, and a
	// sibling it preferred (a different pick under the same neutral key, or a
	// candidate under a different release/content) is not the bytes the
	// fingerprint authorized. The match is made on the candidate's concrete
	// ?result= identity, so a concrete, matching identity is required on every
	// returned surface; there is no neutral or same-id-on-other-content escape.
	returnedID := virtualResultCandidateID(resolvedURI)
	if returnedID == "" {
		return stalePinCandidateResolutionV3{}, fmt.Errorf("resolver returned a provider-neutral URI %q for matched candidate %q", resolvedURI, matchedID)
	}
	if trimmedCandidateID := strings.TrimSpace(res.CandidateID); trimmedCandidateID != "" && trimmedCandidateID != returnedID {
		return stalePinCandidateResolutionV3{}, fmt.Errorf("resolver candidate id %q is inconsistent with returned URI %q", res.CandidateID, resolvedURI)
	}
	if returnedID != matchedID {
		return stalePinCandidateResolutionV3{}, fmt.Errorf("resolver substituted candidate %q for matched %q", returnedID, matchedID)
	}
	if virtualPlaybackNeutralKey(resolvedURI) != virtualPlaybackNeutralKey(match.FilePath) {
		return stalePinCandidateResolutionV3{}, fmt.Errorf("resolver returned candidate %q under a different release than matched %q", resolvedURI, match.FilePath)
	}
	if ownerID > 0 && res.OwnerID > 0 && res.OwnerID != ownerID {
		return stalePinCandidateResolutionV3{}, fmt.Errorf("resolver returned an owner installation %d outside the matched release scope %d", res.OwnerID, ownerID)
	}
	probeTransient := cloneVirtualProbeTransient(*match)
	probeTransient.FilePath = resolvedURI
	probeTransient.VirtualOwnerInstallationID = ownerID
	// Zero the copied row's duration so the fingerprint's duration tier is
	// measured by the probe, never inherited from the previous pin. A prober
	// that reports no runtime then leaves Duration at zero and the caller treats
	// the recovery as a no-match rather than comparing a stale value.
	probeTransient.Duration = 0
	probed, probeErr := h.probeVirtualSource(ctx, res.URL, &probeTransient, cloneHeaderMap(res.RequestHeaders))
	if probeErr != nil || probed == nil {
		// No probe result means no measured duration: the recovery must not fall
		// back to the copied row's stale runtime.
		return stalePinCandidateResolutionV3{}, fmt.Errorf("stale-pin candidate probe failed: %w", probeErr)
	}
	if probed.Duration <= 0 {
		// A probe that measured no runtime cannot confirm the fingerprint's
		// duration tier, so the recovery is a no-match.
		return stalePinCandidateResolutionV3{}, errors.New("stale-pin candidate probe measured no duration")
	}
	// A candidate the serve layer already indicted must never be re-served by the
	// recovery. Check the identity the resolver actually returned, exactly as the
	// general resolver's verdict gate does.
	if err := h.virtualCandidateVerdictError(ctx, resolvedURI, match, ownerID, false); err != nil {
		return stalePinCandidateResolutionV3{}, err
	}
	probed.FilePath = resolvedURI
	if match.ID > 0 {
		probed.ID = match.ID
		probed.MediaFolderID = match.MediaFolderID
	}
	// Fill declared gaps the same way the general resolver does, so the served
	// file carries the candidate's language/codec hints where the probe left
	// them empty. This never overwrites probed evidence.
	mergeVirtualCandidateTracks(probed, matchedStream)
	return stalePinCandidateResolutionV3{
		file:                probed,
		uri:                 resolvedURI,
		url:                 res.URL,
		headers:             res.RequestHeaders,
		providerVideoHash:   res.ProviderVideoHash,
		providerGUID:        res.ProviderGUID,
		providerReleaseName: res.ProviderReleaseName,
		providerReleaseSize: res.ProviderReleaseSize,
	}, nil
}

// virtualStalePinMatchStream scans the live candidates and returns the single
// fingerprint match as the row re-pinned as a copy, alongside the matched stream
// itself (whose declared tracks the caller may merge into the probe result).
//
// A fingerprint is evidence, not proof: same-title releases share video codec,
// declared size and runtime, so more than one distinct candidate identity can
// match. In that case there is no way to tell which one the row pinned, and
// rank-picking would authorize a coin flip. matched is the number of matching
// candidate identities: 0 means no candidate matches, 1 means match and stream
// are the unique match, and >1 means the caller must refuse and ignore match and
// stream.
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
func virtualStalePinMatchStream(row *models.MediaFile, candidates []VirtualPlaybackStream) (models.MediaFile, VirtualPlaybackStream, int) {
	matched := 0
	var (
		match  models.MediaFile
		stream VirtualPlaybackStream
	)
	for _, candidate := range candidates {
		if !virtualStalePinStreamFingerprintMatch(row, candidate) {
			continue
		}
		matched++
		if matched > 1 {
			// The decision is already refusal; stop scanning the rest of a
			// listing that could be arbitrarily long.
			return models.MediaFile{}, VirtualPlaybackStream{}, matched
		}
		match = *row
		match.FilePath = candidate.URI
		// Keep the row's own identity for the in-memory re-pin: the live candidate
		// has no catalog row yet, and adopting its row id would fabricate one. Only
		// the served path changes.
		match.VirtualOwnerInstallationID = effectiveVirtualOwner(candidate.OwnerInstallationID, row.VirtualOwnerInstallationID)
		clearVirtualStalePinOldCandidateState(&match)
		stream = candidate
	}
	return match, stream, matched
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
// it. A known resolution or track-language conflict is a veto too: those are the
// few declared signals that can separate a language/edition variant from a
// same-title release whose codec, size and runtime coincide. Every unknown side
// is neutral.
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
	rowRes := strings.TrimSpace(row.Resolution)
	candidateRes := strings.TrimSpace(candidate.Resolution)
	if rowRes != "" && candidateRes != "" &&
		!strings.EqualFold(stream.NormalizeResolution(rowRes), stream.NormalizeResolution(candidateRes)) {
		return false
	}
	if virtualStalePinKnownAudioLanguageConflict(row, candidate) {
		return false
	}
	return true
}

// virtualStalePinKnownAudioLanguageConflict reports whether both the row and the
// candidate carry a known audio-language set and the candidate shares no language
// with the row. An empty or unparseable-only side is neutral: the provider or the
// probe may simply not have recorded a language, and treating absence as a
// conflict would veto every unknown candidate. Both sides are canonicalized to
// ISO base subtags so "ENG" and "en" agree.
func virtualStalePinKnownAudioLanguageConflict(row *models.MediaFile, candidate VirtualPlaybackStream) bool {
	rowLangs := virtualStalePinRowAudioLanguages(row)
	if len(rowLangs) == 0 {
		return false
	}
	candidateLangs := virtualStalePinCanonicalLanguages(candidate.AudioLanguages)
	if len(candidateLangs) == 0 {
		return false
	}
	for lang := range candidateLangs {
		if rowLangs[lang] {
			return false
		}
	}
	return true
}

// virtualStalePinRowAudioLanguages collects the row's known audio languages from
// its probed tracks, canonicalized to ISO base subtags. A row whose tracks carry
// no parseable language is unknown and yields an empty set, which the caller
// treats as neutral.
func virtualStalePinRowAudioLanguages(row *models.MediaFile) map[string]bool {
	if row == nil {
		return nil
	}
	out := make(map[string]bool)
	for _, track := range row.AudioTracks {
		values := append([]string{track.Language}, track.Languages...)
		for _, raw := range values {
			if lang := virtualStalePinCanonicalLanguage(raw); lang != "" {
				out[lang] = true
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// virtualStalePinCanonicalLanguages canonicalizes a provider-declared language
// list, dropping release markers (MULTI/DUAL) that are not real languages. An
// all-marker list yields an empty set, which is neutral rather than a conflict.
func virtualStalePinCanonicalLanguages(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, raw := range values {
		if lang := virtualStalePinCanonicalLanguage(raw); lang != "" {
			out[lang] = true
		}
	}
	return out
}

// virtualStalePinCanonicalLanguage canonicalizes one language token to its ISO
// base subtag, or "" when it is not a real language tag.
func virtualStalePinCanonicalLanguage(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || !isRealVirtualLanguageTag(trimmed) {
		return ""
	}
	return virtualLanguageBaseSubtag(trimmed)
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
