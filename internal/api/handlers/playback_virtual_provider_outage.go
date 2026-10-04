package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// errVirtualProviderUnavailable marks a resolve that failed because the
// provider's listing was transiently unavailable (empty answer, request
// failure, or 5XX) for a session-bound candidate the catalog still trusts. It
// is deliberately a retryable dependency condition, not a verdict about the
// release: the serve layer answers 503 provider_unavailable so hls.js keeps
// retrying the same pinned release instead of rotating to a sibling.
var errVirtualProviderUnavailable = errors.New("virtual provider listing temporarily unavailable")

// providerUnavailableReasonV3 is the retryable transport/terminal reason a
// classified provider outage surfaces under. It matches the serve layer's
// machine-readable code (stream.go writes the same string as a 503), so a
// v3 start and a byte serve describe the same transient dependence on the
// provider's listing.
const providerUnavailableReasonV3 = "provider_unavailable"

// virtualProviderOutageBackoff is the bounded wait schedule for a transient
// provider-listing failure on a session-bound candidate the catalog still
// trusts. Two retries add at most ~3s of latency, well under the serve and
// transport-startup budgets, and every wait observes request cancellation.
var virtualProviderOutageBackoff = []time.Duration{1 * time.Second, 2 * time.Second}

// virtualProviderListingOutage reports whether a virtual resolve failure is a
// transient provider-listing outage rather than a verdict about the release.
// Only the causes a live provider outage produces are named:
//   - resolver.ErrProviderUnavailable: the provider request failed or answered
//     5XX;
//   - ErrPersistedCandidateTrusted: the trusted persisted same-identity
//     candidate was absent from an empty/partial answer (the resolver refuses
//     to substitute, which is exactly the case that should be retried, not
//     rotated);
//   - ErrSessionBoundCandidateAbsent: a pinned candidate absent from the
//     current listing. Providers renumber their per-listing result ids, so an
//     absence can be a listing artifact rather than a dead release; a bounded
//     forced relist can re-identify the same release under its new id;
//   - the "no streams available from provider" message: an empty answer that
//     carried no pinned-candidate refusal (e.g. a neutral re-list).
//
// A genuine different-release substitution or an untrusted dead pin never
// reaches here because callers gate the retry on the trust predicate below. A
// genuine rotation verdict (an excluded, indicted pin) surfaces as a different
// error because substitution was allowed and the resolver substituted a
// sibling, so it is never turned into a retry loop.
func virtualProviderListingOutage(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, resolver.ErrProviderUnavailable) {
		return true
	}
	if errors.Is(err, virtuallibrary.ErrPersistedCandidateTrusted) {
		return true
	}
	if errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent) {
		return true
	}
	return strings.Contains(err.Error(), "no streams available from provider")
}

// virtualProviderOutageRetries returns how many bounded forced relists a
// retryable provider-listing failure gets. A pinned id that is merely absent
// from the listing gets exactly one: providers renumber result ids per listing,
// so a single forced relist can re-identify the same release under its new id,
// while waiting out the full schedule would only delay a genuinely dead pin. A
// transient provider outage (5XX, empty listing) keeps the full backoff
// schedule.
func virtualProviderOutageRetries(err error) int {
	if errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent) {
		return min(1, len(virtualProviderOutageBackoff))
	}
	return len(virtualProviderOutageBackoff)
}

// virtualCandidateTrustedForOutageRetry reports whether a catalog row is the
// session-bound candidate a transient provider outage should be retried for:
// it carries durable provider identity plus transport evidence (a stored
// resolved URL or a recorded delivery) and is inside the candidate store
// window. A row with no evidence, no identity, or a lapsed window keeps the
// pre-existing behavior: the outage is reported, not retried.
func (h *PlaybackHandler) virtualCandidateTrustedForOutageRetry(row *models.MediaFile) bool {
	if h == nil {
		return false
	}
	return virtualCandidateTrustedForOutageRetry(row, h.virtualCandidateTrustWindow())
}

// virtualCandidateTrustedForOutageRetry is the StreamHandler counterpart of the
// playback handler's trust predicate. The window is read from the handler so
// both share one implementation.
func (h *StreamHandler) virtualCandidateTrustedForOutageRetry(row *models.MediaFile) bool {
	if h == nil {
		return false
	}
	return virtualCandidateTrustedForOutageRetry(row, h.virtualStoredURLTrustWindow())
}

func virtualCandidateTrustedForOutageRetry(row *models.MediaFile, window time.Duration) bool {
	if row == nil || window <= 0 {
		return false
	}
	if _, ok := persistedVirtualIdentity(row); !ok {
		return false
	}
	if strings.TrimSpace(row.ResolvedURL) == "" && row.LastDeliveredAt == nil {
		return false
	}
	return virtualCandidateWithinStoreWindow(row, time.Now(), window)
}

// virtualPendingHoldCap bounds one wait for an in-flight AltMount import
// (SABnzbd queue, not history). It matches the observed 1–10s fetch window:
// long enough for a progressing download to flip to completed, short enough
// that a stuck slot cannot pin playback.
const virtualPendingHoldCap = 10 * time.Second

// virtualPendingHoldReserve is the room the hold leaves for the retry
// resolve after the pause: one probe-class attempt plus scheduling margin.
// A caller with less than reserve plus a minimal pause degrades immediately
// instead of waiting away the retry budget.
const virtualPendingHoldReserve = 15 * time.Second

// virtualPendingLoopPause spaces startup-loop retries of a pending release
// so a lapsed input hold (low remaining budget) cannot spin fresh provider
// listings with no sleep.
const virtualPendingLoopPause = 1 * time.Second

// waitVirtualPendingHold pauses for an in-flight import, bounded by the hold
// cap and the caller's remaining budget. It reports whether the caller should
// re-list: false when the pause would consume the room for the retry resolve,
// in which case the caller degrades to the pending error immediately.
func waitVirtualPendingHold(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return sleepWithContext(ctx, virtualPendingHoldCap)
	}
	remaining := time.Until(deadline)
	wait := min(virtualPendingHoldCap, remaining-virtualPendingHoldReserve)
	if wait <= 0 {
		return false
	}
	return sleepWithContext(ctx, wait)
}

// retryVirtualProviderOutageResolve runs resolve, and while it fails with a
// transient provider-listing outage for a trusted session-bound row, retries
// with the bounded schedule. A canceled request stops immediately. Retry
// attempts are passed relist=true so the resolve closure forces a genuine
// re-list past the negative cache the outage just wrote, and marks the service
// resolve as an outage re-list. The stored-first resolve stays the first
// attempt, so a healthy persisted URL still wins with zero provider calls and a
// transient outage never swaps the viewer's release.
//
// The retry budget is error-shaped (see virtualProviderOutageRetries): a pinned
// id merely absent from the listing gets a single forced relist (a provider
// renumber is a listing artifact), while a transient provider outage keeps the
// full backoff schedule. A genuine rotation verdict never reaches this loop
// because the resolver substitutes a sibling instead of reporting the pin
// absent.
func retryVirtualProviderOutageResolve(
	ctx context.Context,
	trusted bool,
	resolve func(context.Context, bool) (ResolvedVirtualMedia, error),
) (ResolvedVirtualMedia, error) {
	resolved, err := resolve(ctx, false)
	if !trusted {
		return resolved, err
	}
	for attempt := 0; err != nil && virtualProviderListingOutage(err) && attempt < virtualProviderOutageRetries(err); attempt++ {
		wait := virtualProviderOutageBackoff[attempt]
		if !sleepWithContext(ctx, wait) {
			return resolved, err
		}
		slog.InfoContext(ctx, "virtual provider listing unavailable; retrying the session-bound candidate",
			"component", "api", "attempt", attempt+1, "retry_in_ms", wait.Milliseconds(), "error", err)
		resolved, err = resolve(ctx, true)
	}
	return resolved, err
}

// classifyVirtualProviderOutage wraps a final provider outage so the serve
// layer can answer 503 provider_unavailable instead of a permanent resolve
// error. It is a no-op for a non-outage error or an untrusted row.
func classifyVirtualProviderOutage(err error, trusted bool) error {
	if err == nil || !trusted || !virtualProviderListingOutage(err) {
		return err
	}
	return fmt.Errorf("%w: %w", errVirtualProviderUnavailable, err)
}

// transportStartFailureV3 classifies a local transport start failure. A
// virtual provider-listing outage on a release the catalog still trusts is a
// transient dependency failure, not a transcode fault: the transport start
// itself never ran FFmpeg, so reporting transcode_start_failed would tell the
// client the start is broken rather than that the provider should be retried.
// It maps to the retryable provider_unavailable reason the serve layer already
// answers 503 with, so a start and a byte serve agree. Every other cause keeps
// the caller's reason and message.
func transportStartFailureV3(cause error, fallback *transportErrorV3) *transportErrorV3 {
	if errors.Is(cause, errVirtualProviderUnavailable) {
		return &transportErrorV3{
			reason:    providerUnavailableReasonV3,
			message:   "The virtual source provider is temporarily unavailable.",
			retryable: true,
			cause:     cause,
		}
	}
	return fallback
}

// virtualStartUnresolvedTerminalV3 builds the honest terminal for a virtual
// start whose listing failed on every version the fallback walk could try. A
// provider request failure (the edge answered 5xx or the request timed out) is
// the retryable provider_unavailable dependency condition; an empty provider
// listing stays the retryable virtual_source_unavailable the start path already
// used, with a message that names the empty answer; every other cause keeps the
// generic resolve message so no verdict is fabricated from a failure the
// classifier cannot name.
func virtualStartUnresolvedTerminalV3(resolveErr error) playback.DecisionResponseV3 {
	switch {
	case errors.Is(resolveErr, resolver.ErrProviderUnavailable):
		return playback.NewTerminalResponseV3(providerUnavailableReasonV3, "The virtual source provider is temporarily unavailable.", true)
	case strings.Contains(strings.ToLower(fmt.Sprint(resolveErr)), "no streams available"):
		return playback.NewTerminalResponseV3("virtual_source_unavailable", "The provider listed no streams for this title.", true)
	default:
		return playback.NewTerminalResponseV3("virtual_source_unavailable", "The virtual source could not be resolved for playback.", true)
	}
}

// virtualSubstitutionReasonV3 classifies why a fresh start resolved to a
// release other than the one the viewer requested, into the additive
// substitution_reason wire value. A transient provider-listing outage and a
// confirmed-dead pin are both listing failures; every other cause is left
// unknown rather than guessed from a message.
func virtualSubstitutionReasonV3(resolveErr error) string {
	switch {
	case errors.Is(resolveErr, ErrVirtualCandidateMarkedFailed):
		// The requested release is confirmed dead, not merely unreachable.
		return substitutionReasonDeadReleaseV3
	case virtualProviderListingOutage(resolveErr):
		return substitutionReasonListingFailedV3
	default:
		return substitutionReasonUnknownV3
	}
}

// sleepWithContext waits for d or returns false when ctx is canceled first.
func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
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
