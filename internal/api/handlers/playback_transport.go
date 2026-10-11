package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/Silo-Server/silo-server/internal/telemetry"

	"github.com/Silo-Server/silo-server/internal/logredact"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/nodepool"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
	"github.com/Silo-Server/silo-server/internal/tonemap"
	"github.com/Silo-Server/silo-server/internal/transcodenode"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// startLocalPlaybackTransport is the shared local ffmpeg launch primitive for
// legacy and protocol-v3 orchestration. Callers retain ownership of lifecycle
// locking and decide whether registration is immediate or transactionally
// staged.
//
// It starts exactly one FFmpeg process and returns without waiting for its
// first manifest: manifest readiness belongs to the caller
// (startReadyLocalPlaybackTransportV3) or to the hw_accel=auto pipeline loop
// (runTranscodeStartup), which advances to a safer path only when the attempt
// it started exits before its first manifest. Waiting here would convert a
// manifest failure into a start error, and the pipeline treats start errors
// as non-hardware failures it must not retry.
func (h *PlaybackHandler) startLocalPlaybackTransport(ctx context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
	// Tone-map hardware/software selection and retries are owned by the v3
	// planner (softwareToneMapRetryOptsV3) and the compat recipe resolver; this
	// primitive starts exactly the executor it was given.
	return h.startLocalPlaybackTransportOnce(ctx, opts)
}

func (h *PlaybackHandler) startTranscodeSession(ctx context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
	if h != nil && h.StartTranscodeFunc != nil {
		return h.StartTranscodeFunc(ctx, opts)
	}
	return playback.StartTranscode(ctx, opts)
}

// virtualSourceRotationContextKeyV3 scopes an explicit replacement candidate to
// a single transport preparation.
type virtualSourceRotationContextKeyV3 struct{}

type virtualSourceRotationV3 struct {
	URI   string
	Owner int
}

// withVirtualSourceRotationV3 threads the replacement virtual candidate for a
// decode-driven rotation through one transport preparation. The live session is
// still bound to the rejected candidate until the durable session replacement
// commits, so the transport must be told which candidate this generation serves
// without mutating session state early.
func withVirtualSourceRotationV3(ctx context.Context, uri string, owner int) context.Context {
	if ctx == nil || strings.TrimSpace(uri) == "" {
		return ctx
	}
	return context.WithValue(ctx, virtualSourceRotationContextKeyV3{}, virtualSourceRotationV3{URI: uri, Owner: owner})
}

func virtualSourceRotationFromContextV3(ctx context.Context) (string, int, bool) {
	if ctx == nil {
		return "", 0, false
	}
	rotation, ok := ctx.Value(virtualSourceRotationContextKeyV3{}).(virtualSourceRotationV3)
	if !ok || strings.TrimSpace(rotation.URI) == "" {
		return "", 0, false
	}
	return rotation.URI, rotation.Owner, true
}

// pinnedRelayRegistration is the relay a session generation currently serves.
// version increases on every replacement (and on a successful clear) so a stale
// failure that observed an older generation clears only that generation and
// never wipes a replacement another restart installed in the meantime (#158).
type pinnedRelayRegistration struct {
	mu       sync.Mutex
	version  uint64
	url      string
	identity string
}

// set replaces the pin with a new generation.
func (p *pinnedRelayRegistration) set(url, identity string) {
	p.mu.Lock()
	p.version++
	p.url = url
	p.identity = identity
	p.mu.Unlock()
}

// snapshot returns the current pin and the version it belongs to.
func (p *pinnedRelayRegistration) snapshot() (url, identity string, version uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.url, p.identity, p.version
}

// clearIfVersion empties the pin only when it still belongs to version. It
// reports whether the pin was cleared; a stale caller whose observed generation
// was already replaced leaves the replacement intact.
func (p *pinnedRelayRegistration) clearIfVersion(version uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.version != version {
		return false
	}
	p.url = ""
	p.identity = ""
	p.version++
	return true
}

// pinnedRelayStatus reports the live state of a pinned relay registration. It
// prefers the injected RelayRegistrationStatus seam so tests can exercise
// absent, expired, and upstream-rejected pins, and otherwise queries the
// configured relay.
func (h *PlaybackHandler) pinnedRelayStatus(relayURL string) remotestream.RegistrationStatus {
	if h == nil {
		return remotestream.RegistrationAbsent
	}
	if h.RelayRegistrationStatus != nil {
		return h.RelayRegistrationStatus(relayURL)
	}
	if h.RemoteStreamRelay == nil {
		return remotestream.RegistrationAbsent
	}
	return h.RemoteStreamRelay.RegistrationStatus(relayURL)
}

func (h *PlaybackHandler) startLocalPlaybackTransportOnce(ctx context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
	// Repeated input demux failures stamp the virtual candidate known-bad; the
	// manager owns the callback so every local start (fresh or reconstructed)
	// reaches the same marker without the transcode package importing handlers.
	if opts.OnDemuxFailure == nil && h.tm != nil {
		opts.OnDemuxFailure = h.tm.OnDemuxFailure
	}
	if opts.OnSourceRejected == nil && h.tm != nil {
		opts.OnSourceRejected = h.tm.OnSourceRejected
	}
	if !strings.HasPrefix(strings.ToLower(opts.InputPath), virtualPlaybackPrefix) {
		return h.startTranscodeSession(context.WithoutCancel(ctx), opts)
	}
	// The catalog candidate can be replaced while playback is starting. Do not
	// make a just-selected virtual row a second hard dependency; the canonical
	// URI and owner carried in opts are sufficient to resolve the provider URL.
	file := &models.MediaFile{ID: opts.MediaFileID, FilePath: opts.InputPath, VirtualOwnerInstallationID: opts.VirtualSourceOwnerInstallationID}
	if h.fileResolver != nil && opts.MediaFileID > 0 {
		if catalogFile, lookupErr := h.fileResolver.GetByID(ctx, opts.MediaFileID); lookupErr == nil && catalogFile != nil {
			origPath := file.FilePath
			file = catalogFile
			if isUnplayableVirtualURI(file.FilePath) && !isUnplayableVirtualURI(origPath) {
				file.FilePath = origPath
			}
		}
	}
	userID, profileID := 0, ""
	ownerInstallationID := file.VirtualOwnerInstallationID
	sessionVirtualURI := ""
	if rotationURI, rotationOwner, rotationSet := virtualSourceRotationFromContextV3(ctx); rotationSet {
		// A decode-driven candidate rotation deliberately serves the plan's
		// replacement release. Prefer it over the session's still-rejected
		// binding; the durable session replacement commits the same binding.
		copy := *file
		copy.FilePath = rotationURI
		if rotationOwner > 0 {
			copy.VirtualOwnerInstallationID = rotationOwner
		}
		file = &copy
		ownerInstallationID = file.VirtualOwnerInstallationID
		opts.InputPath = file.FilePath
		sessionVirtualURI = rotationURI
		if session, sessionErr := h.sessionMgr.GetSession(opts.SessionID); sessionErr == nil && session != nil {
			userID, profileID = session.UserID, session.ProfileID
		}
	} else if session, sessionErr := h.sessionMgr.GetSession(opts.SessionID); sessionErr == nil && session != nil {
		userID, profileID = session.UserID, session.ProfileID
		sessionVirtualURI = session.VirtualSourceURI
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(session.VirtualSourceURI)), virtualPlaybackPrefix) && (!isUnplayableVirtualURI(session.VirtualSourceURI) || isUnplayableVirtualURI(file.FilePath)) {
			copy := *file
			copy.FilePath = session.VirtualSourceURI
			if session.VirtualSourceOwnerInstallationID > 0 {
				copy.VirtualOwnerInstallationID = session.VirtualSourceOwnerInstallationID
			}
			file = &copy
			ownerInstallationID = file.VirtualOwnerInstallationID
			opts.InputPath = file.FilePath
		}
	}
	canonicalPath := opts.InputPath
	neutralPath := virtualPlaybackNeutralKey(file.FilePath)
	if neutralPath == "" || strings.HasSuffix(neutralPath, "://") {
		neutralPath = virtualPlaybackNeutralKey(opts.InputPath)
	}
	initialPinnedPath := ""
	initialPinnedResultID := ""
	if file != nil && strings.Contains(file.FilePath, "?result=") {
		initialPinnedPath = file.FilePath
		if parsed, err := url.Parse(file.FilePath); err == nil {
			initialPinnedResultID = parsed.Query().Get("result")
		}
	}
	opts.CanonicalInputPath = canonicalPath
	opts.VirtualSourceOwnerInstallationID = ownerInstallationID
	// pin is the concrete relay URL the session's generation currently serves.
	// An in-place segment restart reuses it without a provider call: the URL
	// names the exact candidate the session is bound to, and a live relay entry
	// avoids provider latency on every random seek (#158). Reuse is gated on the
	// registration still being live — not expired, not evicted, relay not
	// closed, upstream not rejecting credentials — because a stale pin would
	// otherwise wedge the restart on a dead transport. The fast path is scoped
	// to a configured relay: without one there is no pinned transport to reuse
	// and the stored-URL/rotation path below is unchanged, so the no-relay tests
	// and callers keep their behavior. The pin is versioned so a stale failure
	// clears only the generation it observed, never a concurrent replacement.
	pin := &pinnedRelayRegistration{}
	relayConfigured := h.RemoteStreamRelay != nil
	setPinnedRelay := func(res ResolvedVirtualMedia) {
		if !relayConfigured {
			return
		}
		pin.set(res.URL, res.URI)
	}
	opts.RefreshInput = func(refreshCtx context.Context) (string, func(), error) {
		// Reuse the pinned relay only while its registration is live. A pin can
		// go stale without this session noticing yet: the relay drops entries
		// after 24h, evicts the oldest past its bound, and refuses every entry
		// once closed, while an upstream 401/403 marks the registration
		// rejected because the signed URL behind it is no longer authorized.
		// Reusing any of those would pin the restart to a dead transport. On a
		// miss, clear only the generation observed here and fall through: an
		// auth-rejected pin forces a fresh provider listing to renew the token,
		// an absent pin keeps the stored-first path.
		pinnedURL, pinnedURI, pinnedVersion := pin.snapshot()
		forceRefresh := false
		if trimmed := strings.TrimSpace(pinnedURL); trimmed != "" {
			switch h.pinnedRelayStatus(trimmed) {
			case remotestream.RegistrationLive:
				slog.InfoContext(refreshCtx, "virtual transport restart reused the pinned relay without a provider call",
					"component", "api", "session_anchor", canonicalPath,
					"status", "reused", "virtual_uri", pinnedURI)
				return trimmed, nil, nil
			case remotestream.RegistrationAuthRejected:
				// The pin's upstream rejected its credentials (401/403). The
				// stored URL behind it is the same dead URL, so skip the
				// stored-first shortcut and force a provider relist to renew.
				pin.clearIfVersion(pinnedVersion)
				forceRefresh = true
				slog.InfoContext(refreshCtx, "virtual transport restart renewing a relay whose upstream rejected credentials",
					"component", "api", "session_anchor", canonicalPath,
					"status", "renewing", "virtual_uri", pinnedURI)
			default:
				pin.clearIfVersion(pinnedVersion)
				slog.InfoContext(refreshCtx, "virtual transport restart found the pinned relay registration gone; resolving stored-first",
					"component", "api", "session_anchor", canonicalPath,
					"status", "pin_lost", "virtual_uri", pinnedURI)
			}
		}
		// A restart renews the exact candidate pinned to this session, never a
		// provider-neutral re-selection: re-resolving through the neutral path
		// can silently swap to a differently-ranked candidate mid-stream. The
		// canonical path still carries the ?result= identity the session bound
		// to during planning.
		//
		// First attempt with forceRefresh=false (or true after an auth
		// rejection): the session's own persisted provider URL is a perfectly
		// good restart input, and the stored-URL shortcut serves it from the
		// catalog row with zero provider calls. The historical force here was
		// meant to bypass cached transport, not to discard the stored URL;
		// forcing it made every mid-session restart re-list the provider, so an
		// altmount ?result= renumbering (ids are per-listing) turned a healthy
		// persisted URL into a fatal resolve error and every segment 500'd until
		// hls.js gave up. Identity is still threaded (see
		// resolveVirtualInputURI), so a harmless renumbering re-matches the same
		// release. An observed 401/403 is the one case that legitimately needs
		// the forced relist: the stored URL is equally unauthorized.
		res, cleanup, err := h.resolveVirtualInputURI(
			refreshCtx, canonicalPath, ownerInstallationID,
			userID, profileID, forceRefresh, nil, "",
		)
		if err == nil {
			setPinnedRelay(res)
			return res.URL, cleanup, nil
		}
		// The stored path failed. Retry once with a fresh relist and rotation
		// declared, excluding the pinned candidate, and thread the row's durable
		// identity so a renumbered same-release candidate is re-identified rather
		// than swapped. Mirror resolveVirtualAnchorURIWithRotationV3: the retry
		// must never silently anchor this already-planned session on sibling
		// bytes, so accept it only when the same release re-matched.
		pinnedID := virtualResultCandidateID(canonicalPath)
		var excluded []string
		if pinnedID != "" {
			excluded = []string{pinnedID}
		}
		retryCtx := virtualResolveContextWithPersistedIdentity(refreshCtx, file)
		rotated, rotatedCleanup, rotateErr := h.resolveVirtualInputURI(
			retryCtx, canonicalPath, ownerInstallationID,
			userID, profileID, true, excluded, "", true,
		)
		if rotateErr != nil {
			pin.clearIfVersion(pinnedVersion)
			return rotated.URL, rotatedCleanup, rotateErr
		}
		if !rotated.IdentityRematched && !resolvedMatchesPersistedIdentity(rotated, file) {
			if rotatedCleanup != nil {
				rotatedCleanup()
			}
			slog.WarnContext(refreshCtx, "virtual transport restart rotation resolved a different release; refusing a silent restart swap",
				"component", "api", "session_anchor", canonicalPath,
				"status", "rotation_refused", "old_candidate_id", pinnedID,
				"new_candidate_id", virtualResultCandidateID(rotated.URI))
			pin.clearIfVersion(pinnedVersion)
			return "", nil, err
		}
		slog.InfoContext(refreshCtx, "virtual transport restart rotated an absent session-bound candidate",
			"component", "api", "session_anchor", canonicalPath,
			"status", "rotated", "old_candidate_id", pinnedID,
			"new_candidate_id", virtualResultCandidateID(rotated.URI), "virtual_uri", rotated.URI)
		setPinnedRelay(rotated)
		return rotated.URL, rotatedCleanup, nil
	}
	var lastErr error
	// One deadline owns the whole transport startup: provider resolution,
	// transcode session start, and manifest readiness all observe the
	// remainder of this single budget. A slow resolve must not hand a fresh
	// timeout to the manifest wait, or the advertised cold-start budget stops
	// being an end-to-end bound. startupCtx derives from the caller's ctx (not
	// a detached background context) so cancellation still propagates.
	startupCtx, startupCancel := context.WithTimeout(ctx, virtualStartupBudget)
	defer startupCancel()
	maxAttempts := h.maxVirtualFailoverAttempts(ctx)
	if sessionVirtualURI != "" {
		maxAttempts = min(2, maxAttempts)
	}
	// A fresh start has no session binding yet, so it declares the resolve
	// not-session-bound: a provider that renumbered its per-listing result ids
	// falls through to a live sibling instead of refusing with the
	// session-bound absent-pin sentinel (ErrSessionBoundCandidateAbsent) and
	// failing the start. A start that already owns a session binding (a
	// restart or replan against a live session) keeps the session-bound
	// default and refuses a genuine release swap.
	resolveStartupCtx := startupCtx
	if sessionVirtualURI == "" {
		resolveStartupCtx = withVirtualSessionBindingV3(startupCtx, false)
	}
	failedCandidateIDs := make([]string, 0)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		targetURI := canonicalPath
		if attempt > 0 {
			targetURI = neutralPath
		}
		preferredID := initialPinnedResultID
		for _, failed := range failedCandidateIDs {
			if preferredID == failed {
				preferredID = ""
				break
			}
		}
		// An exclusion is only a substitution verdict when this startup loop
		// just indicted that candidate (attempt > 0 with a failed id). Declare
		// it explicitly so the resolver may serve a sibling; a neutral first
		// attempt, or a failure that identified no candidate, keeps refusing.
		resolvedMedia, cleanup, resolveErr := h.resolveVirtualInputURI(
			resolveStartupCtx, targetURI, ownerInstallationID, userID, profileID, attempt > 0, failedCandidateIDs, preferredID, attempt > 0 && len(failedCandidateIDs) > 0,
		)
		if resolveErr != nil {
			lastErr = resolveErr
			// A pending release (AltMount actively fetching) is never
			// indicted: it is neither dead nor ready, so excluding it and
			// scrubbing it from the best-result cache would be the skip this
			// hold exists to prevent. Retry it fresh instead; the loop's own
			// attempt bound still terminates a release that never completes.
			if errors.Is(resolveErr, virtuallibrary.ErrProviderPending) {
				if targetURI == canonicalPath {
					canonicalPath = neutralPath
					if file != nil {
						file.FilePath = neutralPath
					}
				}
				// Pause before the fresh retry: the input resolver already
				// held once, but a lapsed hold (low remaining budget) would
				// otherwise spin this loop with no sleep, hammering the
				// provider with fresh listings. One second per attempt is
				// negligible against the hold cap and bounds the spin.
				if !sleepWithContext(resolveStartupCtx, virtualPendingLoopPause) {
					break
				}
				continue
			}
			failedID := resolvedMedia.CandidateID
			if failedID == "" {
				if parsed, err := url.Parse(targetURI); err == nil {
					failedID = parsed.Query().Get("result")
				}
			}
			if failedID != "" {
				failedCandidateIDs = append(failedCandidateIDs, failedID)
			}
			if h.BestResultCache != nil && file != nil {
				neutralURI := virtualPlaybackNeutralKey(canonicalPath)
				failedURI := targetURI
				if failedID != "" {
					failedURI = withVirtualResultKey(neutralURI, failedID)
				}
				h.BestResultCache.RemoveCandidate(bestResultCacheKey(file.ContentID, neutralURI, ownerInstallationID), failedURI)
				h.BestResultCache.RemoveCandidateForContent(file.ContentID, neutralURI, ownerInstallationID, failedURI)
			}
			if targetURI == canonicalPath {
				canonicalPath = neutralPath
				if file != nil {
					file.FilePath = neutralPath
				}
			}
			continue
		}
		// The session's process lifetime stays independent of the startup
		// deadline: transcodeCtx derives from a detached context so the
		// deferred startupCancel below cannot kill a ready session's FFmpeg.
		// Startup bounding happens in two places that never touch process
		// lifetime: resolveVirtualInputURI runs under startupCtx, and the
		// manifest wait below observes startupCtx as an owner deadline while
		// the session itself runs on transcodeCtx.
		if attempt > 0 && sessionVirtualURI != "" && file != nil {
			// A fallback attempt declares substitution so the resolver may
			// serve a sibling when this loop just indicted the pin. An existing
			// session is bound to the release the viewer picked, though, so it
			// must never silently swap to different bytes: accept the fallback
			// only when the same release re-identified (IdentityRematched) or
			// its durable identity matches the row's. A legacy row with no
			// durable identity keeps the pre-existing failover behavior, and a
			// fresh start (no session binding) is unaffected.
			if _, hasIdentity := persistedVirtualIdentity(file); hasIdentity &&
				!resolvedMedia.IdentityRematched && !resolvedMatchesPersistedIdentity(resolvedMedia, file) {
				if cleanup != nil {
					cleanup()
				}
				lastErr = fmt.Errorf("virtual transport fallback resolved a different release; refusing a silent release swap")
				if resolvedMedia.CandidateID != "" {
					failedCandidateIDs = append(failedCandidateIDs, resolvedMedia.CandidateID)
				}
				continue
			}
		}
		transcodeCtx, transcodeCancel := context.WithCancel(context.WithoutCancel(ctx))
		timer := time.AfterFunc(4*time.Hour, transcodeCancel)
		cleanupWithCancel := func() {
			timer.Stop()
			transcodeCancel()
			if cleanup != nil {
				cleanup()
			}
		}
		attemptOpts := opts
		attemptOpts.InputPath = resolvedMedia.URL
		attemptOpts.InputCleanup = cleanupWithCancel
		// A cross-release fallback serves different bytes than the plan was
		// built for: the plan's audio/subtitle ordinals name the pinned
		// release's inventory, not the sibling's. Probe-then-start: the
		// replacement is probed synchronously under the remaining startup
		// budget, and the session is built on the probed facts — source
		// codecs, audio layout/channels, HDR range — instead of the pinned
		// release's. Selections reset to the container default (audio and
		// subtitle -1, no burn-in): the client starts from a valid track on
		// the replacement rather than an ordinal that means something else
		// (or nothing) there. The first attempt (the pinned release itself)
		// keeps the plan's selections untouched. A probe failure does not
		// fail the fallback: the attempt proceeds on reset-to-default opts
		// (the pre-#118 behavior) so a slow prober cannot wedge startup.
		if attempt > 0 {
			attemptOpts.AudioTrackIndex = -1
			attemptOpts.SubtitleTrackIndex = -1
			attemptOpts.SubtitleBurnIn = false
			attemptOpts.SourceVideoCodec = ""
			attemptOpts.SourceVideoProfile = ""
			attemptOpts.SourceVideoBitDepth = 0
			attemptOpts.SourceAudioChannels = 0
			// A lapsed stored URL is renewed in the background before the probe
			// runs, so the probe is not spent on a dead token; probe failures
			// already fall through to declared metadata, so this stays
			// best-effort and never fails the fallback.
			h.fireExpiredVirtualRefresh(startupCtx, file, ownerInstallationID, userID, profileID, resolvedMedia.ExpiresAt)
			if probedFacts := h.probeVirtualFallbackSource(startupCtx, resolvedMedia, file); probedFacts != nil {
				attemptOpts.SourceVideoCodec = probedFacts.CodecVideo
				attemptOpts.SourceVideoProfile = virtualFallbackVideoProfile(probedFacts)
				attemptOpts.SourceVideoBitDepth = virtualFallbackVideoBitDepth(probedFacts)
				attemptOpts.SourceAudioChannels = virtualFallbackAudioChannels(probedFacts)
			}
		}
		session, startErr := h.startTranscodeSession(transcodeCtx, attemptOpts)
		if startErr == nil {
			// The manifest wait observes the startup deadline as an owner
			// bound: it ends at whichever comes first — the historical
			// per-wait timeout or whatever the resolve stages left of the
			// single startup budget. The session keeps running on
			// transcodeCtx regardless; only this wait is bounded.
			if _, readyErr := session.WaitForManifestContext(startupCtx, playback.ManifestStartupTimeout); readyErr == nil {
				winningURI := resolvedMedia.URI
				if winningURI == "" || winningURI == neutralPath {
					if resolvedMedia.CandidateID != "" {
						winningURI = withVirtualResultKey(neutralPath, resolvedMedia.CandidateID)
					} else {
						winningURI = targetURI
					}
				}
				canonicalPath = winningURI
				if file != nil {
					file.FilePath = winningURI
				}
				// Transport successfully ready. Record the winning relay as the
				// session's pinned input so an in-place restart reuses it without
				// re-resolving the provider. On a fallback the winning candidate
				// is the one the attempts above accepted, so the pin follows it.
				setPinnedRelay(ResolvedVirtualMedia{URL: resolvedMedia.URL, URI: winningURI})
				// If this was a fallback attempt from a dead pin,
				// update the persisted pin compare-and-swap so future sessions use the live source.
				if replacer, ok := h.fileResolver.(interface {
					ReplaceVirtualResultPin(context.Context, int, string, string) (bool, error)
				}); ok && file != nil && file.ID > 0 && initialPinnedPath != "" {
					if winningURI != "" && winningURI != initialPinnedPath {
						unpinCtx, unpinCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
						if _, err := replacer.ReplaceVirtualResultPin(unpinCtx, file.ID, initialPinnedPath, winningURI); err != nil {
							slog.WarnContext(ctx, "failed to update virtual result pin after fallback success", "component", "api", "file_id", file.ID, "error", err)
						}
						unpinCancel()
					}
				}
				return session, nil
			} else {
				startErr = readyErr
				// A decoder-rejected candidate belongs to the start/replan
				// rotation loops, not this transport fallback: they bound
				// attempts, accumulate exclusions, and persist the terminal.
				// Advancing here as well would double-rotate (and could
				// exhaust the poisoned-start budget the tests pin down),
				// so surface the verdict immediately.
				if errors.Is(readyErr, playback.ErrSourceDecodeRejected) {
					_ = session.Close()
					return nil, readyErr
				}
			}
			_ = session.Close()
		} else if cleanup != nil {
			cleanupWithCancel()
		}
		if resolvedMedia.CandidateID != "" {
			failedCandidateIDs = append(failedCandidateIDs, resolvedMedia.CandidateID)
		}
		if h.BestResultCache != nil && file != nil {
			neutralURI := virtualPlaybackNeutralKey(canonicalPath)
			failedURI := targetURI
			if resolvedMedia.CandidateID != "" {
				failedURI = withVirtualResultKey(neutralURI, resolvedMedia.CandidateID)
			}
			h.BestResultCache.RemoveCandidate(bestResultCacheKey(file.ContentID, neutralURI, ownerInstallationID), failedURI)
			h.BestResultCache.RemoveCandidateForContent(file.ContentID, neutralURI, ownerInstallationID, failedURI)
		}
		if targetURI == canonicalPath {
			canonicalPath = neutralPath
			if file != nil {
				file.FilePath = neutralPath
			}
		}
		lastErr = startErr
	}
	if lastErr == nil {
		lastErr = errors.New("virtual transcode provider returned no usable stream")
	}
	// All attempts failed: conditionally clear the initial dead pin so future plays re-list
	if file != nil && file.ID > 0 {
		unpinCtx, unpinCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		if replacer, ok := h.fileResolver.(interface {
			ReplaceVirtualResultPin(context.Context, int, string, string) (bool, error)
		}); ok && initialPinnedPath != "" {
			_, _ = replacer.ReplaceVirtualResultPin(unpinCtx, file.ID, initialPinnedPath, neutralPath)
		} else if cleaner, ok := h.fileResolver.(interface {
			ClearVirtualResultPin(context.Context, int) error
		}); ok {
			_ = cleaner.ClearVirtualResultPin(unpinCtx, file.ID)
		}
		unpinCancel()
	}
	return nil, lastErr
}

// resolveVirtualInputURI resolves a virtual input for a transport start. The
// probeVirtualFallbackSource probes the replacement candidate of a
// cross-release fallback synchronously under the remaining startup budget and
// returns its observed facts, merged with (never overwritten by) the
// candidate's declared metadata. Nil means "proceed on reset-to-default
// opts": no prober wired, budget exhausted, or probe failed — none of which
// may wedge the fallback. The caller copies source codec/profile/depth and
// audio channels from the result; track selections stay at the container
// default (the replacement's own inventory arrives with its session/plan,
// and adopting the pinned release's ordinals would misaddress it).
func (h *PlaybackHandler) probeVirtualFallbackSource(ctx context.Context, resolved ResolvedVirtualMedia, file *models.MediaFile) *models.MediaFile {
	if h == nil || (h.VirtualPlaybackSourceProber == nil && h.VirtualPlaybackSourceProberWithHeaders == nil) {
		return nil
	}
	if strings.TrimSpace(resolved.URL) == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, virtualProbeBudget)
	defer cancel()
	probeFile := models.MediaFile{Container: virtualURIScheme}
	if file != nil {
		probeFile.ContentID = file.ContentID
		probeFile.MediaFolderID = file.MediaFolderID
		probeFile.VirtualOwnerInstallationID = file.VirtualOwnerInstallationID
	}
	probed, err := h.probeVirtualSource(probeCtx, resolved.URL, &probeFile, resolved.RequestHeaders)
	if err != nil || probed == nil {
		return nil
	}
	probeCand := VirtualPlaybackStream{
		URI:               resolved.URI,
		CodecAudio:        resolved.CodecAudio,
		AudioLanguages:    resolved.AudioLanguages,
		SubtitleLanguages: resolved.SubtitleLanguages,
	}
	mergeVirtualCandidateTracks(probed, probeCand)
	return probed
}

// fireExpiredVirtualRefresh renews a lapsed virtual candidate's stored URL in
// the background so a probe or seek running next does not have to fail on a
// dead token, and the renewed URL is persisted for the next request. The work
// is detached from the caller (a client disconnect must not cancel it) and
// bounded to virtualProbeBudget; it is best-effort and never fails the caller.
// A non-forced resolution is deliberate: it re-lists and records the renewal
// through the existing Phase-1 saver, and it never serves the expired URL.
func (h *PlaybackHandler) fireExpiredVirtualRefresh(ctx context.Context, file *models.MediaFile, ownerInstallationID, userID int, profileID string, expiresAt time.Time) {
	if h == nil || file == nil || !virtualResolvedURLExpired(expiresAt, time.Now()) {
		return
	}
	bgCtx, cancel := h.virtualDetachedContext(ctx, virtualProbeBudget)
	go func() {
		defer cancel()
		res, cleanup, err := h.resolveVirtualInputURI(bgCtx, file.FilePath, ownerInstallationID, userID, profileID, false, nil, "")
		if cleanup != nil {
			cleanup()
		}
		if err != nil {
			slog.WarnContext(bgCtx, "virtual stored-url refresh failed",
				"component", "api",
				"file_id", file.ID,
				"virtual_uri", file.FilePath,
				"error", logredact.SanitizeURLError(err))
			return
		}
		slog.InfoContext(bgCtx, "virtual stored-url refreshed ahead of playback",
			"component", "api",
			"file_id", file.ID,
			"virtual_uri", file.FilePath,
			"candidate_id", res.CandidateID)
	}()
}

// virtualProviderStaleRefresher is an optional capability a virtual resolve
// port may expose. It lets a play refresh provider-side cached state — the
// AltMount completed/failed snapshot or the Prowlarr RSS index — that a
// long-lived process has let go stale, before resolving against it.
type virtualProviderStaleRefresher interface {
	Stale() bool
	RefreshIfStale(ctx context.Context) error
}

// refreshStaleVirtualProvider fires a single virtualProbeBudget-bounded
// RefreshIfStale on any configured virtual resolve port that reports stale
// provider state. It reports whether a refresh ran, so the caller can bypass
// its own candidate caches, which would otherwise answer from the stale
// listing the refresh just superseded. Ports that do not implement the
// capability are unaffected.
//
// One deadline bounds every refresh in the pass: each port used to get its own
// virtualProbeBudget, so four stale ports could consume four budgets (60s) of
// the caller's startup or seek window before the stored-URL shortcut ran. One
// adapter may also implement several of the port interfaces below; it is
// refreshed once per pass, deduplicated by adapter identity, not once per
// interface.
func (h *PlaybackHandler) refreshStaleVirtualProvider(ctx context.Context) bool {
	if h == nil {
		return false
	}
	refreshCtx, cancel := context.WithTimeout(ctx, virtualProbeBudget)
	defer cancel()
	refreshed := false
	observed := make(map[any]struct{}, 4)
	for _, port := range []any{
		h.VirtualPlaybackResolver,
		h.VirtualPlaybackStreamLister,
		h.VirtualMediaDetailedResolver,
		h.VirtualMediaResolver,
	} {
		refresher, ok := port.(virtualProviderStaleRefresher)
		if !ok || refresher == nil {
			continue
		}
		if key, comparable := virtualRefresherDedupKey(port); comparable {
			if _, seen := observed[key]; seen {
				continue
			}
			observed[key] = struct{}{}
		}
		if !refresher.Stale() {
			continue
		}
		if err := refresher.RefreshIfStale(refreshCtx); err != nil {
			slog.WarnContext(ctx, "virtual provider stale refresh failed",
				"component", "api", "error", logredact.SanitizeURLError(err))
			continue
		}
		refreshed = true
	}
	return refreshed
}

// virtualRefresherDedupKey returns a dedup key for a virtual resolve port. The
// same adapter may implement several of the interfaces refreshStaleVirtualProvider
// scans; a comparable dynamic value (the usual pointer adapter) dedups by value,
// so two interfaces backed by the same adapter collapse to one refresh.
// Non-comparable implementations return comparable=false and are never wrongly
// collapsed.
func virtualRefresherDedupKey(port any) (key any, comparable bool) {
	t := reflect.TypeOf(port)
	if t == nil || !t.Comparable() {
		return nil, false
	}
	return port, true
}

// virtualRelayFreshRegistrationContextKey marks a virtual resolve that must
// register a new relay entry even when an equivalent live registration exists.
// A bounded anchor retry sets it so its second probe presents a fresh token
// instead of replaying the token the provider just refused; every other caller
// keeps the content-key reuse that shares one upstream and one range-cache
// scope.
type virtualRelayFreshRegistrationContextKey struct{}

func withVirtualRelayFreshRegistration(ctx context.Context) context.Context {
	if ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, virtualRelayFreshRegistrationContextKey{}, true)
}

func virtualRelayFreshRegistrationRequested(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	requested, _ := ctx.Value(virtualRelayFreshRegistrationContextKey{}).(bool)
	return requested
}

// virtualFallbackVideoProfile returns the probed primary video profile for
// fallback session facts, or "" when the probe recorded none.
func virtualFallbackVideoProfile(probed *models.MediaFile) string {
	if probed == nil || len(probed.VideoTracks) == 0 {
		return ""
	}
	return probed.VideoTracks[0].Profile
}

// virtualFallbackVideoBitDepth returns the probed primary video bit depth,
// or 0 when unknown.
func virtualFallbackVideoBitDepth(probed *models.MediaFile) int {
	if probed == nil || len(probed.VideoTracks) == 0 {
		return 0
	}
	return probed.VideoTracks[0].BitDepth
}

// virtualFallbackAudioChannels returns the probed first audio track's channel
// count, or 0 when the probe recorded none.
func virtualFallbackAudioChannels(probed *models.MediaFile) int {
	if probed == nil || len(probed.AudioTracks) == 0 {
		return 0
	}
	return probed.AudioTracks[0].Channels
}

// ResolveVirtualTransportInput resolves the session-bound virtual source for a
// reconstructed local transport. It is the exported front door for the same
// stored-URL-first + trust-window + provider-outage-retry resolution the
// transport startup uses: a rebuilt session must first try the row's persisted
// URL (zero provider calls, no release swap), fall through to a provider
// resolve with the row's durable identity threaded, and retry a transient
// provider listing before giving up. When the pinned candidate is genuinely
// absent from the provider list it retries once with rotation declared,
// excluding the absent pin, and accepts the retry only when the same release
// re-matched — mirroring RefreshInput and resolveVirtualAnchorURIWithRotationV3,
// so a rebuilt session never silently anchors on sibling bytes. It returns the
// relay-registered input and its cleanup.
func (h *PlaybackHandler) ResolveVirtualTransportInput(ctx context.Context, virtualURI string, ownerInstallationID, userID int, profileID string) (ResolvedVirtualMedia, func(), error) {
	resolved, cleanup, err := h.resolveVirtualInputURI(ctx, virtualURI, ownerInstallationID, userID, profileID, false, nil, "")
	if err == nil || (!errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent) && !errors.Is(err, ErrVirtualCandidateMarkedFailed)) {
		return resolved, cleanup, err
	}
	file, lookupErr := h.VirtualFileLookup(ctx, virtualURI)
	if lookupErr != nil || file == nil {
		return resolved, cleanup, err
	}
	pinnedID := virtualResultCandidateID(virtualURI)
	var excluded []string
	if pinnedID != "" {
		excluded = []string{pinnedID}
	}
	retryCtx := virtualResolveContextWithPersistedIdentity(ctx, file)
	rotated, rotatedCleanup, rotateErr := h.resolveVirtualInputURI(
		retryCtx, virtualURI, ownerInstallationID, userID, profileID, true, excluded, "", true,
	)
	if rotateErr != nil {
		return rotated, rotatedCleanup, rotateErr
	}
	if !rotated.IdentityRematched && !resolvedMatchesPersistedIdentity(rotated, file) {
		if rotatedCleanup != nil {
			rotatedCleanup()
		}
		slog.WarnContext(ctx, "virtual transport rebuild rotation resolved a different release; refusing a silent rebuild swap",
			"component", "api", "session_anchor", virtualURI,
			"status", "rotation_refused", "old_candidate_id", pinnedID,
			"new_candidate_id", virtualResultCandidateID(rotated.URI))
		return resolved, cleanup, err
	}
	if cleanup != nil {
		cleanup()
	}
	slog.InfoContext(ctx, "virtual transport rebuild rotated an absent session-bound candidate",
		"component", "api", "session_anchor", virtualURI,
		"status", "rotated", "old_candidate_id", pinnedID,
		"new_candidate_id", virtualResultCandidateID(rotated.URI), "virtual_uri", rotated.URI)
	return rotated, rotatedCleanup, nil
}

// ResolveVirtualDownloadInput adapts the playback virtual-transport resolution
// to the downloads artifact preparer. It shares the exact stored-URL-first,
// trust-window, provider-outage-retrying, same-release-rematching resolve seam
// and relay registration as playback — never a second rotation or trust
// implementation. The canonical row is read from file; the returned cleanup
// releases the relay registration once the executor has finished reading.
//
// Preparation identity is the persisted catalog row, not the requester. The
// row's virtual owner installation scopes the provider resolve and its durable
// provider identity drives the same-release re-match, so every requester of the
// same file resolves to the same input and shares one artifact. There is no
// requesting session for a background preparation, so the session-bound
// account/profile are the zero value; per-requester authorization is enforced
// on the download row and its /file route, never here.
func (h *PlaybackHandler) ResolveVirtualDownloadInput(ctx context.Context, file *models.MediaFile) (string, func(), error) {
	if h == nil || file == nil {
		return "", nil, errors.New("virtual download input resolver is unavailable")
	}
	resolveCtx := virtualResolveContextWithPersistedIdentity(ctx, file)
	res, cleanup, err := h.ResolveVirtualTransportInput(resolveCtx, file.FilePath, file.VirtualOwnerInstallationID, 0, "")
	if err != nil {
		return "", nil, err
	}
	return res.URL, cleanup, nil
}

// final rotateCandidates argument is the caller's explicit declaration that
// excluding a candidate is a verdict against that release, which authorizes
// serving a sibling. It defaults to false, so an exclusion on its own never
// authorizes a silent release swap (see resolveVirtualInputURIExcluding on the
// stream handler for the same contract). The variadic form keeps the many
// non-excluding callers unchanged; only a serve-layer failover that just
// indicted the candidate passes true.
func (h *PlaybackHandler) resolveVirtualInputURI(
	ctx context.Context,
	virtualURI string,
	ownerInstallationID int,
	userID int,
	profileID string,
	forceRefresh bool,
	excludedCandidateIDs []string,
	preferredCandidateID string,
	rotateCandidates ...bool,
) (ResolvedVirtualMedia, func(), error) {
	rotationRequested := false
	if len(rotateCandidates) > 0 {
		rotationRequested = rotateCandidates[0]
	}
	// Opportunistic provider-staleness refresh: when the resolve port reports
	// stale provider state, refresh it under a bounded context and bypass the
	// stored-URL shortcut so this resolve cannot answer from the stale listing
	// the refresh just superseded. A port without the capability is unaffected.
	if h.refreshStaleVirtualProvider(ctx) {
		forceRefresh = true
	}
	var res ResolvedVirtualMedia
	var err error
	// Stored-URL shortcut. A session-bound re-resolve of a pinned candidate —
	// the remux seek anchor, a transport restart, the subtitle/font warm — may
	// serve the row's own persisted provider URL instead of listing the
	// provider again. The URL belongs to the exact candidate being resolved
	// (see evaluateStoredVirtualURLCandidate for the same-row check), so it is
	// the pinned release itself and never a substitution. Narrow by design:
	// an explicit forceRefresh (a failover retry after this candidate failed)
	// or an exclusion list always takes the list-and-resolve path.
	var storedRow *models.MediaFile
	// Read the row once. It is both the source of the stored-URL shortcut
	// and the durable identity the same-release re-match needs when the
	// provider renumbers its result ids. On a forced refresh the stored URL
	// is never served (the shortcut below requires !forceRefresh), but the
	// durable identity is still threaded: force-refresh bypasses cached
	// transport, not identity, so a harmless provider renumbering still
	// re-matches the same release instead of failing same-release recovery.
	if h.VirtualFileLookup != nil {
		if row, lookupErr := h.VirtualFileLookup(ctx, virtualURI); lookupErr == nil && row != nil {
			storedRow = row
			ctx = virtualResolveContextWithPersistedTrust(ctx, row, time.Now(), h.virtualCandidateTrustWindow())
		}
	}
	var storedExpiredRow *models.MediaFile
	storedUsable := false
	if storedRow != nil && !forceRefresh && len(excludedCandidateIDs) == 0 {
		usable, state := evaluateStoredVirtualURLCandidate(
			ctx, virtualURI, storedRow,
			h.storedVirtualURLAllowInsecure(storedRow, ownerInstallationID), time.Now(),
			h.virtualCandidateTrustWindow(),
		)
		switch state {
		case virtualStoredURLUsable:
			res = usable
			storedUsable = true
		case virtualStoredURLExpiredWithinWindow:
			// The signed URL lapsed but the row is still trusted: resolve the
			// same candidate afresh and refresh the stored value below. Never
			// serve the expired URL itself.
			storedExpiredRow = storedRow
		case virtualStoredURLExpired:
			// The row owns this candidate but its URL lapsed. Resolve afresh
			// below, then refresh the stored value through the existing
			// Phase-1 saver.
			storedExpiredRow = storedRow
		}
	}
	if !storedUsable {
		if h.VirtualMediaDetailedResolver != nil {
			// The intent travels with the context so the resolver can distinguish a
			// serve-layer indictment from a display-driven same-file re-plan.
			ctx = withVirtualCandidateRotationV3(ctx, rotationRequested)
			// The session-binding intent is declared by the caller and travels
			// on the context (see withVirtualSessionBindingV3): absent means
			// session-bound, the conservative default that refuses a
			// profile-removed or absent pin instead of silently swapping the
			// release. A fresh transport start declares false (no session
			// binding yet) so a provider renumber falls through to a live
			// sibling; the restart, anchor and reconstructed-transport callers
			// declare or default to true so a genuine release swap still
			// refuses.
			// A transient provider-listing blackout for the session's own
			// trusted candidate must not be read as an indictment of the
			// release. Retry it with a short bounded backoff before giving up,
			// and classify the final failure as a retryable provider outage so
			// the client keeps retrying the release it picked. The retry keeps
			// the caller's exact parameters (exclusions, forceRefresh), so an
			// intentional rotation still rotates on the first successful
			// relist; only a failed listing is retried.
			outageTrusted := h.virtualCandidateTrustedForOutageRetry(storedRow)
			res, err = retryVirtualProviderOutageResolve(ctx, outageTrusted, func(retryCtx context.Context, relist bool) (ResolvedVirtualMedia, error) {
				if relist {
					retryCtx = virtuallibrary.WithProviderOutageRelist(retryCtx)
				}
				return h.VirtualMediaDetailedResolver.ResolveVirtualMediaDetailed(
					retryCtx, virtualURI, ownerInstallationID, userID, profileID, forceRefresh || relist, excludedCandidateIDs, preferredCandidateID,
				)
			})
			// A release AltMount is actively fetching is worth waiting for,
			// not skipping: hold briefly for the import, then re-list once
			// (forced, so a flip to completed is picked up) and take whatever
			// that answer is. The retry carries the caller's exclusions and
			// rotation intent unchanged (no outage marker: a progressing
			// download is not an outage, so outage budgets must not move).
			// A lapsed hold degrades to the pending error, which callers
			// handle like any unresolvable release — except the startup
			// loop, which must not indict it (see below).
			if errors.Is(err, virtuallibrary.ErrProviderPending) && waitVirtualPendingHold(ctx) {
				res, err = h.VirtualMediaDetailedResolver.ResolveVirtualMediaDetailed(
					ctx, virtualURI, ownerInstallationID, userID, profileID, true, excludedCandidateIDs, preferredCandidateID,
				)
			}
			if err == nil && res.IdentityRematched && storedRow != nil {
				// The pinned id was absent but the same release re-identified
				// under a new id. Adopt it through the Phase-1 CAS/fence write
				// so the row's ?result= and durable identity move with it.
				// storedRow is always loaded (see above), so this covers a
				// forced refresh too — not only the stored-URL shortcut path.
				adoptRematchedVirtualResolution(ctx, storedRow, res, h.VirtualFileMetadataSaver, h.VirtualFileSaver)
			} else if err == nil && storedExpiredRow != nil {
				// Reuse the Phase-1 write path; only the requested candidate's
				// own successful resolution is recorded (a substituted sibling
				// is skipped inside). The result is intentionally ignored here:
				// the serve path already holds the resolved URL, so persistence
				// is best-effort cache hygiene, not the request's outcome.
				_, _ = refreshStoredVirtualResolution(ctx, storedExpiredRow, res, h.VirtualFileMetadataSaver, h.VirtualFileSaver)
			} else if err != nil {
				err = classifyVirtualProviderOutage(err, outageTrusted)
			}
		} else if forceRefresh && h.VirtualMediaRefreshResolver != nil {
			var inputPath string
			inputPath, err = h.VirtualMediaRefreshResolver.RefreshVirtualMedia(
				ctx, virtualURI, ownerInstallationID, userID, profileID,
			)
			res = ResolvedVirtualMedia{URL: inputPath, URI: virtualURI}
		} else {
			var inputPath string
			inputPath, err = resolveVirtualMediaPath(
				ctx, h.VirtualMediaResolver, virtualURI,
				ownerInstallationID, userID, profileID,
			)
			res = ResolvedVirtualMedia{URL: inputPath, URI: virtualURI}
		}
	}
	if err != nil {
		// A resolve refusal is hard to correlate to its request without the
		// edge request id, and has_identity distinguishes a legacy row with no
		// durable provider identity (which cannot same-release re-match) from a
		// renumber. The identity presence is read from the resolve context the
		// caller already threaded, so the virtuallibrary resolver never needs
		// the edge middleware.
		_, hasIdentity := virtuallibrary.PersistedCandidateIdentityFromContext(ctx)
		slog.WarnContext(ctx, "virtual stream resolve failed",
			"component", "api",
			requestIDLogKeyV3, chimw.GetReqID(ctx),
			"owner_installation_id", ownerInstallationID,
			"virtual_uri", virtualURI,
			"has_identity", hasIdentity,
			"error", logredact.SanitizeURLError(err),
		)
		return res, nil, fmt.Errorf("resolve virtual input: %w", err)
	}
	if h.RemoteStreamRelay == nil {
		return res, nil, nil
	}
	var relayURL string
	var cleanup func()
	effectiveOwner := effectiveVirtualOwner(res.OwnerID, ownerInstallationID)
	insecure := h.AllowPrivateStreams != nil && h.AllowPrivateStreams(effectiveOwner)
	fresh := virtualRelayFreshRegistrationRequested(ctx)
	switch {
	case insecure && fresh:
		relayURL, cleanup, err = h.RemoteStreamRelay.RegisterInsecureWithHeadersFresh(ctx, res.URL, res.RequestHeaders)
	case insecure:
		relayURL, cleanup, err = h.RemoteStreamRelay.RegisterInsecureWithHeaders(ctx, res.URL, res.RequestHeaders)
	case fresh:
		relayURL, cleanup, err = h.RemoteStreamRelay.RegisterWithHeadersFresh(ctx, res.URL, res.RequestHeaders)
	default:
		relayURL, cleanup, err = h.RemoteStreamRelay.RegisterWithHeaders(ctx, res.URL, res.RequestHeaders)
	}
	if err != nil {
		slog.WarnContext(ctx, "virtual stream relay registration failed",
			"component", "api",
			"owner_installation_id", effectiveOwner,
			"virtual_uri", virtualURI,
			"error", logredact.SanitizeURLError(err),
		)
		return ResolvedVirtualMedia{}, nil, err
	}
	res.URL = relayURL
	return res, cleanup, nil
}

// startRemotePlaybackTransport is the shared remote-node launch primitive.
// It returns the node's HTTP status separately so legacy and v3 can preserve
// their existing public error envelopes while executing identical transport
// startup and response parsing.
func (h *PlaybackHandler) startRemotePlaybackTransport(ctx context.Context, nodeURL string, request transcodenode.TranscodeStartRequest) (transcodenode.TranscodeStartResponse, int, error) {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(request.InputPath)), virtualPlaybackPrefix) {
		return transcodenode.TranscodeStartResponse{}, 0, errors.New("unresolved virtual sources require an integrated transcode transport")
	}
	body, err := json.Marshal(request)
	if err != nil {
		return transcodenode.TranscodeStartResponse{}, 0, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, h.remotePlaybackTransportTimeout(nodeURL, request))
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, nodepool.NodeEndpoint(nodeURL, "/transcode/start"), bytes.NewReader(body))
	if err != nil {
		return transcodenode.TranscodeStartResponse{}, 0, logredact.SanitizeURLError(err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+h.JWTSecret)
	response, err := telemetry.DoTrustedNode(http.DefaultClient, httpRequest, "transcode_start")
	if err != nil {
		return transcodenode.TranscodeStartResponse{}, 0, logredact.SanitizeURLError(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusAccepted {
		// Drain the (small) error body so the transport can reuse the
		// connection instead of tearing it down on every failed start.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if request.ToneMapMode != "" {
			if validationErr := transcodenode.ToneMapExecutionErrorForResponse(
				response.StatusCode,
				response.Header.Get(transcodenode.ToneMapExecutionErrorHeader),
			); validationErr != nil {
				return transcodenode.TranscodeStartResponse{}, response.StatusCode, validationErr
			}
		}
		return transcodenode.TranscodeStartResponse{}, response.StatusCode, nil
	}
	var result transcodenode.TranscodeStartResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		// Older nodes returned an empty 202 response; accept that for ordinary
		// transcodes while treating any other malformed 202 body as a failed
		// start instead of fabricating a success from a zero-value response.
		if errors.Is(err, io.EOF) && request.ToneMapMode == "" {
			return transcodenode.TranscodeStartResponse{}, response.StatusCode, nil
		}
		slog.WarnContext(ctx, "remote transcode start response decode failed", "component", "api", "node", logredact.SanitizeURL(nodeURL), "error", err)
		return transcodenode.TranscodeStartResponse{}, response.StatusCode, fmt.Errorf("decode remote transcode start response: %w", err)
	}
	return result, response.StatusCode, nil
}

func (h *PlaybackHandler) remotePlaybackTransportTimeout(nodeURL string, request transcodenode.TranscodeStartRequest) time.Duration {
	// A burn-in node waits longer for its first segment, so the caller's HTTP
	// budget must cover the same budget or it would abort a slow-but-healthy
	// subtitle composite before the node answers. Non-burn-in plans keep the
	// historical numbers (TranscodeStartReadinessTimeout == ManifestStartupTimeout).
	readinessBudget := playback.ManifestStartupTimeoutFor(playback.TranscodeOpts{
		SubtitleBurnIn:     request.SubtitleBurnIn,
		SubtitleTrackIndex: request.SubtitleTrackIndex,
	})
	if request.ToneMapMode == "" {
		if request.RequireReady && remoteAutoFallbackPossibleV3(request) {
			// The node answers only after its first manifest, which under
			// hw_accel=auto can follow an early exit on each safer path.
			return transcodenode.TranscodeStartReadyMaxDuration + 5*time.Second
		}
		return readinessBudget + 5*time.Second
	}
	timeout := h.remoteToneMapProbeTimeoutV3(nodeURL) + readinessBudget
	if request.ToneMapPreflightRequired {
		timeout += tonemap.SourcePreflightTimeout(request.TotalDuration)
	}
	if request.RequireReady {
		timeout += readinessBudget
	}
	return timeout
}

// remoteAutoFallbackPossibleV3 reports whether a node may walk the hw_accel=auto
// fallback for this start: a video transcode dispatched as auto. The node
// resolves auto against its live hardware, so the budget follows the request
// rather than a stored capability report that may be missing or stale.
func remoteAutoFallbackPossibleV3(request transcodenode.TranscodeStartRequest) bool {
	return strings.EqualFold(strings.TrimSpace(request.HWAccel), "auto") &&
		!strings.EqualFold(strings.TrimSpace(request.TargetCodecVideo), "copy")
}

func fetchRemoteTranscodeCapabilities(ctx context.Context, nodeURL, jwtSecret string) (playback.HWAccelInfo, error) {
	info, status, err := transcodenode.FetchHWCapabilities(ctx, http.DefaultClient, nodeURL, jwtSecret)
	if err != nil {
		return playback.HWAccelInfo{}, err
	}
	if status != http.StatusOK {
		return playback.HWAccelInfo{}, fmt.Errorf("node returned %d", status)
	}
	info.Source = "transcode_node"
	info.NodeURL = nodeURL
	return info, nil
}
