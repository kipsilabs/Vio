package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Silo-Server/silo-server/internal/access"
	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

const (
	// maxVersionCheckFiles caps the batch so a single request cannot fan out
	// unbounded provider work.
	maxVersionCheckFiles = 40
	// versionCheckConcurrency bounds simultaneous provider resolutions.
	versionCheckConcurrency = 4
	// versionCheckPerFileBudget bounds one file's provider resolution.
	versionCheckPerFileBudget = 6 * time.Second
	// versionCheckOverallBudget bounds the whole batch.
	versionCheckOverallBudget = 15 * time.Second
)

type versionCheckRequest struct {
	FileIDs []int `json:"file_ids"`
}

type versionCheckResult struct {
	FileID    int  `json:"file_id"`
	Available bool `json:"available"`
}

type versionCheckResponse struct {
	Results []versionCheckResult `json:"results"`
}

// versionCheckOutcome is one file's liveness verdict before stamps are
// applied. Resolution and classification run concurrently per file, but
// stamps apply only after the whole batch is judged (see
// gateVersionCheckStamps): one volatile listing must never mass-tag
// versions as dead.
type versionCheckOutcome struct {
	fileID int
	// file is the resolved row, carried so the deferred stamp can fence on
	// its identity and observed verdict. Nil when the file was not found.
	file *models.MediaFile
	// available is the per-file answer: durable state when the round is
	// ambiguous or gated, live resolution otherwise.
	available bool
	// stamp is true when this round found the pin dead.
	stamp bool
	// durableAlive mirrors the row's pre-round failed_at (true when the row
	// carried no verdict): the answer to report when stamping is gated off.
	durableAlive bool
	// checked is true when the file got a provider resolution attempt, the
	// only outcomes that speak to listing health. Denied, unknown, local,
	// and unwired files report without resolving and stay out of the gate
	// denominator.
	checked bool
}

const (
	// minVersionCheckStampQuorum is the minimum dead-pin count that can trip
	// the batch gate. Below it every stamp applies: a lone genuinely-dead
	// pin (the common single-file check) must still be recorded.
	minVersionCheckStampQuorum = 4
	// versionCheckStampDisagreementRate is the dead-pin fraction above which
	// the listing itself is distrusted instead of the pins. Past it the
	// round reports durable state and stamps nothing: a renumber storm or a
	// degraded partial listing disagrees with everything at once, and that
	// pattern means the observation is bad, not forty releases at once.
	// The comparison is strictly past the rate: an exact-half split still
	// stamps, deciding ties in favor of recording genuinely-dead pins
	// rather than protecting against a volatile listing.
	versionCheckStampDisagreementRate = 0.5
)

// gateVersionCheckStamps decides which outcomes may stamp. It returns the
// file IDs to stamp; availability in the returned outcomes already reflects
// the decision (durable state when gated, live resolution otherwise).
func gateVersionCheckStamps(outcomes []versionCheckOutcome) ([]versionCheckOutcome, []int) {
	dead, checked := 0, 0
	for _, outcome := range outcomes {
		if !outcome.checked {
			continue
		}
		checked++
		if outcome.stamp {
			dead++
		}
	}
	gated := checked > 0 && dead >= minVersionCheckStampQuorum &&
		float64(dead)/float64(checked) > versionCheckStampDisagreementRate
	final := make([]versionCheckOutcome, 0, len(outcomes))
	var toStamp []int
	for _, outcome := range outcomes {
		if gated && outcome.stamp {
			// Listing untrustworthy this round: report the durable signal
			// and stamp nothing.
			outcome.available = outcome.durableAlive
			outcome.stamp = false
		}
		if outcome.stamp {
			toStamp = append(toStamp, outcome.fileID)
		}
		final = append(final, outcome)
	}
	return final, toStamp
}

// HandleCheckVersions implements POST /catalog/versions/check: a batched
// liveness probe for the media page's version list. Each file is tested
// cheaply — virtual rows resolve their pinned ?result= candidate through the
// provider (no media transfer), local rows are read from missing_since — and
// the durable failed_at signal is stamped accordingly. Unknown or deleted
// file IDs are reported as available=false.
func (h *CatalogResourceHandler) HandleCheckVersions(w http.ResponseWriter, r *http.Request) {
	var req versionCheckRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.FileIDs) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "At least one file ID is required")
		return
	}
	if len(req.FileIDs) > maxVersionCheckFiles {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "At most 40 file IDs are allowed per request")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), versionCheckOverallBudget)
	defer cancel()

	outcomes := make([]versionCheckOutcome, 0, len(req.FileIDs))
	var mu sync.Mutex
	eg, egCtx := errgroup.WithContext(ctx)
	eg.SetLimit(versionCheckConcurrency)
	for _, id := range req.FileIDs {
		if id <= 0 {
			continue
		}
		id := id
		eg.Go(func() error {
			outcome := h.checkVersion(egCtx, id)
			mu.Lock()
			outcomes = append(outcomes, outcome)
			mu.Unlock()
			return nil
		})
	}
	// Per-file failures are folded into the availability verdict; the batch
	// itself never fails on one file.
	_ = eg.Wait()

	// Stamps apply only after the whole batch is judged: one volatile
	// listing disagreeing with everything must not mass-tag versions.
	final, toStamp := gateVersionCheckStamps(outcomes)
	stampByID := make(map[int]struct{}, len(toStamp))
	for _, id := range toStamp {
		stampByID[id] = struct{}{}
	}
	results := make([]versionCheckResult, 0, len(final))
	for _, outcome := range final {
		if _, ok := stampByID[outcome.fileID]; ok {
			h.stampVirtualCandidateFailed(ctx, outcome.fileID, outcome.file)
		}
		results = append(results, versionCheckResult{FileID: outcome.fileID, Available: outcome.available})
	}

	writeJSON(w, http.StatusOK, versionCheckResponse{Results: results})
}

// checkVersion tests one media file's liveness and returns whether it is
// available. The file is authorized for the requesting profile BEFORE any
// resolution: inaccessible IDs are indistinguishable from unknown IDs
// (available=false, no distinguishing error), so a restricted profile cannot
// probe arbitrary files or trigger provider work for them. Virtual rows
// resolve the pinned candidate through the provider and stamp failed_at on a
// confirmed dead pin; local rows are read from missing_since with no probe.
// Ambiguous provider errors (timeout, network, resolver not configured) leave
// the stamp unchanged and report the row's current computed availability, so
// a provider outage cannot mass-tag versions as dead.
func (h *CatalogResourceHandler) checkVersion(ctx context.Context, fileID int) versionCheckOutcome {
	outcome := versionCheckOutcome{fileID: fileID}
	if h == nil || h.FileResolver == nil {
		return outcome
	}
	file, err := h.FileResolver.GetByID(ctx, fileID)
	if err != nil || file == nil {
		return outcome
	}
	outcome.file = file
	outcome.durableAlive = file.FailedAt == nil
	if !h.fileAccessible(ctx, file) {
		// Denied by the profile's catalog/library access policy: report the
		// same shape as an unknown ID and never resolve or stamp.
		return outcome
	}
	if !isVirtualPlaybackFile(file) {
		outcome.available = file.MissingSince == nil
		return outcome
	}
	if h.VirtualResolver == nil {
		// No provider resolver wired (playback disabled): report the durable
		// stamp only, never stamp anything.
		outcome.available = outcome.durableAlive
		return outcome
	}

	perFileCtx, cancel := context.WithTimeout(ctx, versionCheckPerFileBudget)
	defer cancel()
	// A strict availability check of the requested pin, not a fresh selection:
	// declare it session-bound so a profile-removed pin reports ambiguous
	// (do not stamp) instead of being substituted by a different candidate.
	perFileCtx = withVirtualSessionBindingV3(perFileCtx, true)
	// Thread the row's durable identity so a provider that renumbers result ids
	// between listings re-identifies the same release instead of being read as
	// a dead pin. Without this a volatile listing stamped every pinned row
	// dead, which is the incident this check guards against. A legacy row with
	// no durable identity is left untouched and stays ambiguous.
	perFileCtx = virtualResolveContextWithPersistedIdentity(perFileCtx, file)
	requestedCandidateID := virtualResultCandidateID(file.FilePath)
	resolved, err := h.VirtualResolver.ResolveVirtualMediaDetailed(
		perFileCtx, file.FilePath, file.VirtualOwnerInstallationID,
		apimw.GetUserID(ctx), apimw.GetProfileID(ctx), true, nil, requestedCandidateID,
	)
	if err == nil {
		// Strict check semantics, not playback fallback semantics: with
		// forceRefresh=true the selection code does not substitute candidates[0]
		// for an absent pin. A successful resolution counts when it named the
		// requested candidate, or when the durable identity proves it is the
		// same release under a renumbered result id (IdentityRematched).
		outcome.checked = true
		if resolvedIdentityMatches(resolved, requestedCandidateID) || resolvedMatchesPersistedIdentity(resolved, file) {
			// A healthy liveness observation for the same identity clears a
			// stale failed_at so the auto-pick considers the release again.
			h.clearVirtualCandidateIfFailed(ctx, fileID, file)
			if resolved.IdentityRematched {
				// Adopt the new ?result= identity through the same CAS-fenced
				// Phase-1 write the playback path uses, so the row moves with
				// the provider's renumbering instead of being rematched on
				// every listing.
				adoptRematchedVirtualResolution(
					context.WithoutCancel(ctx), file, resolved,
					h.VirtualFileMetadataSaver, h.VirtualFileSaver,
				)
			}
			outcome.available = true
			return outcome
		}
		if requestedCandidateID == "" {
			// The row carries no concrete pin (profile-neutral row): any
			// resolution of its identity is listing evidence. Report live.
			h.clearVirtualCandidateIfFailed(ctx, fileID, file)
			outcome.available = true
			return outcome
		}
		// The provider answered with a different candidate and the row's
		// durable identity does not match it: the requested release is
		// genuinely gone. Record the stamp for the batch gate; a row with
		// no durable identity is indistinguishable from a renumbered
		// listing, so it is left alone (ambiguous) rather than mass-stamped.
		if _, hasIdentity := persistedVirtualIdentity(file); hasIdentity {
			outcome.stamp = true
		}
		outcome.available = false
		return outcome
	}
	if isVirtualProviderListingTemporaryError(err) {
		// Transport-temporary: the provider's listing request failed, timed
		// out, or answered 5xx. That is availability-shaped and says nothing
		// about the pinned release, so this must be classified BEFORE the
		// string-based confirmed-dead classifier and before any
		// identity-grounded absent-pin branch. A joined error can carry both a
		// transient listing cause and a "no matching candidate" fallback text
		// (or an absent-pin sentinel); letting either be read as a verdict is
		// what let a provider flap indict a pin the liveness check then
		// reported dead.
		outcome.checked = true
		outcome.available = outcome.durableAlive
		return outcome
	}
	if isVirtualCandidateDeadError(err) {
		// Confirmed dead pin: the provider listed but the pinned candidate is
		// gone or unusable. Record the stamp for the batch gate; the write
		// itself is fenced the same way, so a candidate rotated while
		// resolution was in flight is never mis-marked.
		outcome.checked = true
		outcome.stamp = true
		outcome.available = false
		return outcome
	}
	if _, hasIdentity := persistedVirtualIdentity(file); hasIdentity &&
		errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent) {
		// The provider listed, the row carries a durable identity, and the
		// resolver still refused because no listed candidate re-identified the
		// same release. That is a confirmed absence, not a renumbered listing:
		// record the stamp for the batch gate. Without identity the sentinel
		// is ambiguous and is left alone below.
		outcome.checked = true
		outcome.stamp = true
		outcome.available = false
		return outcome
	}
	// Ambiguous (provider down, timeout, identity-less absence): do not stamp.
	// Report the current durable signal so an outage cannot mass-tag versions.
	outcome.checked = true
	outcome.available = outcome.durableAlive
	return outcome
}

// stampVirtualCandidateFailed applies the fenced failed_at verdict the check
// computed. It is a no-op when no stamp callback is wired or the row already
// carries a verdict, and the fenced write (expectedFilePath + observedFailedAt)
// rejects a row that rotated or gained a newer verdict while resolution was in
// flight.
func (h *CatalogResourceHandler) stampVirtualCandidateFailed(ctx context.Context, fileID int, file *models.MediaFile) {
	if h == nil || h.MarkVirtualFailed == nil || file == nil || file.FailedAt != nil {
		return
	}
	_ = h.MarkVirtualFailed(context.WithoutCancel(ctx), fileID, file.FilePath, file.FailedAt)
}

// clearVirtualCandidateIfFailed applies the recovery half of a healthy liveness
// observation: a same-identity successful resolution clears a stale failed_at
// so the release is eligible again. The clear is fenced on the same identity
// and observed verdict as the stamp, so a concurrent rotation or newer failure
// survives. It is a no-op when the row carries no verdict or no clear callback
// is wired.
func (h *CatalogResourceHandler) clearVirtualCandidateIfFailed(ctx context.Context, fileID int, file *models.MediaFile) {
	if h == nil || h.ClearVirtualFailed == nil || file == nil || file.FailedAt == nil {
		return
	}
	_ = h.ClearVirtualFailed(context.WithoutCancel(ctx), fileID, file.FilePath, file.FailedAt)
}

// resolvedIdentityMatches reports whether the resolver's answer named the
// requested candidate: either the returned CandidateID is the requested
// result= value, or the returned URI carries it. Substituted candidates are
// never treated as recovery evidence for the requested pin.
func resolvedIdentityMatches(resolved ResolvedVirtualMedia, requestedCandidateID string) bool {
	if resolved.CandidateID == requestedCandidateID {
		return true
	}
	if parsed, err := url.Parse(resolved.URI); err == nil {
		if strings.TrimSpace(parsed.Query().Get("result")) == requestedCandidateID {
			return true
		}
	}
	return false
}

// fileAccessible applies the requesting profile's catalog/library access
// policy to a media file, mirroring the playback handler's loadAuthorizedFile
// authorization: episodes authorize through their parent series, extras
// through their parent item, and plain files through their own content ID,
// followed by the file-level library/quality predicate. Any failure — missing
// lookup dependencies, an inaccessible parent, or a file outside the allowed
// libraries — denies the file exactly like an unknown ID.
func (h *CatalogResourceHandler) fileAccessible(ctx context.Context, file *models.MediaFile) bool {
	if h == nil || h.ItemAccess == nil {
		return false
	}
	filter := catalog.AccessFilter{
		AllowedLibraryIDs:  accessScopeAllowedLibraryIDs(ctx),
		DisabledLibraryIDs: accessScopeDisabledLibraryIDs(ctx),
		MaxPlaybackQuality: accessScopeMaxPlaybackQuality(ctx),
		UserID:             apimw.GetUserID(ctx),
		ProfileID:          apimw.GetProfileID(ctx),
	}
	if scope, ok := access.GetScope(ctx); ok {
		filter.MaturityLimits = scope.MaturityLimits
	}
	switch {
	case file.EpisodeID != "":
		if h.EpisodeLookup == nil {
			return false
		}
		episode, err := h.EpisodeLookup.GetByID(ctx, file.EpisodeID)
		if err != nil || episode == nil {
			return false
		}
		if err := h.ItemAccess.EnsureAccessible(ctx, episode.SeriesID, filter); err != nil {
			return false
		}
	case file.ContentID != "":
		if err := h.ItemAccess.EnsureAccessible(ctx, file.ContentID, filter); err != nil {
			return false
		}
	case file.ExtraID != "":
		if h.ExtraLookup == nil {
			return false
		}
		extra, err := h.ExtraLookup.GetByID(ctx, file.ExtraID)
		if err != nil || extra == nil {
			return false
		}
		if err := h.ItemAccess.EnsureAccessible(ctx, extra.ParentID, filter); err != nil {
			return false
		}
	default:
		return false
	}
	return catalog.FileAllowedByAccess(file, filter)
}

// isVirtualProviderListingTemporaryError reports whether a resolution failure
// is transport-temporary / availability-shaped rather than a verdict about the
// pinned release: the provider listing could not be produced (request failed,
// timed out, or answered 5xx), the release is still being fetched, or the
// trusted persisted candidate was absent from a listing the resolver refused to
// substitute from. None of these may indict a pin; the caller reports durable
// state instead.
//
// This is deliberately checked BEFORE the string-based
// isVirtualCandidateDeadError: a joined error can carry both a transient
// listing cause and a "no matching candidate" fallback text, and the transient
// cause must win. Every typed sentinel from internal/virtuallibrary and
// internal/virtuallibrary/resolver is matched with errors.Is so a rename of the
// human text cannot silently re-open the indictment; the provider-neutral
// strings below cover a wrapped RPC failure the core path does not type.
func isVirtualProviderListingTemporaryError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, resolver.ErrProviderUnavailable) ||
		errors.Is(err, virtuallibrary.ErrProviderPending) ||
		errors.Is(err, virtuallibrary.ErrPersistedCandidateTrusted) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "request failed") ||
		strings.Contains(msg, "resolver is not installed") ||
		strings.Contains(msg, "load owning virtual stream provider") ||
		strings.Contains(msg, "no streams available") ||
		strings.Contains(msg, "provider unavailable") ||
		strings.Contains(msg, "failed recently") ||
		strings.Contains(msg, "bad gateway") ||
		strings.Contains(msg, "service unavailable") ||
		strings.Contains(msg, "temporarily unavailable") ||
		containsProvider5xxStatus(msg)
}

// containsProvider5xxStatus reports whether an error message names an HTTP 5xx
// provider status ("... returned status 503", "... HTTP 503"). A listing that
// failed with a raw 5xx is transport-temporary even when the surrounding text
// also carries a fallback "no matching candidate", so the dead-pin classifier
// must defer to it.
func containsProvider5xxStatus(msg string) bool {
	if strings.Contains(msg, " 5xx") {
		return true
	}
	for _, marker := range []string{"status 5", "http 5"} {
		idx := strings.Index(msg, marker)
		if idx < 0 {
			continue
		}
		rest := msg[idx+len(marker):]
		if len(rest) >= 2 && rest[0] >= '0' && rest[0] <= '9' && rest[1] >= '0' && rest[1] <= '9' {
			return true
		}
	}
	return false
}

// isVirtualCandidateDeadError classifies a resolution failure as a confirmed
// dead pin: the provider answered, listed candidates, and the pinned candidate
// is no longer among them or is unusable. Provider-down/timeout errors do not
// match and are treated as ambiguous. The strings are the provider-neutral
// error texts the plugin service produces (see
// internal/plugins/virtual_playback.go). A joined error that also carries a
// provider RPC failure ("request failed") is ambiguous even when a fallback
// provider reported no matching candidate: the owner provider that owns the pin
// may simply be down. A transport-temporary cause always wins over the
// dead-pin strings, so a listing failure joined with a "no matching candidate"
// fallback text is never read as a verdict (see
// isVirtualProviderListingTemporaryError).
//
// An EMPTY listing ("no streams available from provider") is deliberately NOT
// classified as dead. A zero-count answer is a provider hiccup that proves
// nothing about any specific release: the provider intermittently returns an
// empty listing for a title whose releases are all still offered. Stamping pins
// on it is how a 2.6 s burst marked 50 of 57 rows failed in the incident.
func isVirtualCandidateDeadError(err error) bool {
	if err == nil || isVirtualProviderListingTemporaryError(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no matching candidate") ||
		strings.Contains(msg, "no usable stream")
}

// The access-scope extractors below mirror requestAccessFilter's mapping of
// the resolved access scope onto a catalog.AccessFilter, but take a context
// instead of an *http.Request because the liveness check fans out per file
// through an errgroup. A missing scope yields the unrestricted zero values,
// exactly like requestAccessFilter.

func accessScopeAllowedLibraryIDs(ctx context.Context) []int {
	if scope, ok := access.GetScope(ctx); ok {
		return scope.AllowedLibraryIDs
	}
	return nil
}

func accessScopeDisabledLibraryIDs(ctx context.Context) []int {
	if scope, ok := access.GetScope(ctx); ok {
		return scope.DisabledLibraryIDs
	}
	return nil
}

func accessScopeMaxPlaybackQuality(ctx context.Context) string {
	if scope, ok := access.GetScope(ctx); ok {
		return scope.MaxPlaybackQuality
	}
	return ""
}
