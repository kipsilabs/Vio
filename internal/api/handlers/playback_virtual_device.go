package handlers

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/plugins"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

// virtualFallbackSiblingListBudget bounds the listing a cross-release fallback
// spends looking for a probed sibling. It runs only on an attempt that already
// indicted the pinned release, and a provider answer is normally served from
// the listing cache the resolve just populated, so the bound exists to keep a
// slow provider from spending the startup budget it would otherwise leave for
// the fallback itself.
const virtualFallbackSiblingListBudget = 3 * time.Second

// DeviceCapabilityProfileSource resolves the persisted device capabilities used
// for candidate ranking. It is an interface so tests can inject a deterministic
// profile; production wires providerDeviceCapabilitySource.
type DeviceCapabilityProfileSource interface {
	DeviceCapabilitiesFor(ctx context.Context, profileID, deviceID string) (plugins.DeviceCapabilities, bool)
}

func (h *PlaybackHandler) requestDeviceCapabilities(r *http.Request) (plugins.DeviceCapabilities, bool) {
	if h == nil || h.DeviceCapabilitySource == nil || r == nil {
		return plugins.DeviceCapabilities{}, false
	}
	return h.DeviceCapabilitySource.DeviceCapabilitiesFor(r.Context(), apimw.GetProfileID(r.Context()), requestDeviceID(r))
}

// finalizeVirtualCandidateOrder applies the handler-side candidate ordering:
// device rank, then the explicit quality-rung reorder, then a stable
// rejected-last partition. The partition runs last so a custom-format-rejected
// candidate can never sit at index 0 even when device fit would promote it
// (plugins.ScoreCandidate clamps QualityScore, so reject cannot be expressed as
// a low score alone). With a single candidate the input is returned unchanged:
// a lone rejected stream is a last-resort choice, not an ordering decision.
func (h *PlaybackHandler) finalizeVirtualCandidateOrder(r *http.Request, streams []VirtualPlaybackStream, qualityPreference string, bandwidthCapKbps int) []VirtualPlaybackStream {
	if len(streams) <= 1 || virtualProviderOrderPreserved(streams) {
		return streams
	}
	ranked, _ := h.rankVirtualCandidatesForDevice(r, streams)
	ranked = reorderVirtualCandidatesForQuality(ranked, qualityPreference, bandwidthCapKbps)
	return partitionVirtualStreamsAcceptedFirst(ranked)
}

// virtualProviderOrderPreserved also checks listed alternatives when the first
// candidate is a persisted pin without transient listing metadata.
func virtualProviderOrderPreserved(streams []VirtualPlaybackStream) bool {
	for _, stream := range streams {
		if stream.PreserveProviderOrder {
			return true
		}
	}
	return false
}

// preferProbedVirtualCandidates stably moves candidates whose catalog row
// carries planner-grade probed evidence (complete video, audio and container
// evidence plus a probe stamp) ahead of unprobed stubs, within each
// accepted/rejected group. Compatibility still wins: a rejected probed row
// never jumps a healthy accepted stub. Best-effort: rows that fail the
// verdict lookup keep their relative order rather than blocking the start.
func (h *PlaybackHandler) preferProbedVirtualCandidates(ctx context.Context, candidates []VirtualPlaybackStream, file *models.MediaFile, ownerID int) []VirtualPlaybackStream {
	if h == nil || len(candidates) < 2 || virtualProviderOrderPreserved(candidates) {
		return candidates
	}
	probed := make([]bool, len(candidates))
	anyProbed, anyUnprobed := false, false
	for i, cand := range candidates {
		probed[i] = h.virtualCandidateHasProbeEvidence(ctx, cand.URI, file, ownerID)
		if probed[i] {
			anyProbed = true
		} else {
			anyUnprobed = true
		}
	}
	if !anyProbed || !anyUnprobed {
		return candidates
	}
	ordered := make([]VirtualPlaybackStream, 0, len(candidates))
	for _, accepted := range []bool{true, false} {
		for i, cand := range candidates {
			if !cand.Rejected == accepted && probed[i] {
				ordered = append(ordered, cand)
			}
		}
		for i, cand := range candidates {
			if !cand.Rejected == accepted && !probed[i] {
				ordered = append(ordered, cand)
			}
		}
	}
	return ordered
}

// virtualCandidateHasProbeEvidence reports whether the catalog row owning a
// candidate carries planner-grade probe evidence: complete video, audio and
// container evidence plus a probe stamp. Unknown verdicts (lookup error or
// incomplete row) return false so the candidate keeps its listed position.
func (h *PlaybackHandler) virtualCandidateHasProbeEvidence(ctx context.Context, candidateURI string, file *models.MediaFile, ownerID int) bool {
	if h == nil || candidateURI == "" {
		return false
	}
	contentID, episodeID := "", ""
	if file != nil {
		contentID, episodeID = file.ContentID, file.EpisodeID
	}
	row, found, err := h.lookupVirtualCandidateRowDetailed(ctx, candidateURI, contentID, episodeID, ownerID)
	if err != nil || !found || row == nil || row.ProbeUpdatedAt == nil {
		return false
	}
	return completeVirtualVideoEvidenceV3(row) && completeVirtualAudioEvidenceV3(row) && completeVirtualContainerEvidenceV3(row)
}

// virtualCandidateHasProbedInventory reports whether the catalog row owning a
// candidate carries measured audio or subtitle inventory. It is deliberately
// narrower than virtualCandidateHasProbeEvidence: a cross-release fallback only
// needs to know which stream ordinals address real streams, not whether the row
// is fully planner-grade. A row with no probe stamp is UNKNOWN inventory, not
// empty inventory, so it never qualifies.
func (h *PlaybackHandler) virtualCandidateHasProbedInventory(ctx context.Context, candidateURI string, file *models.MediaFile, ownerID int) bool {
	if h == nil || candidateURI == "" {
		return false
	}
	contentID, episodeID := "", ""
	if file != nil {
		contentID, episodeID = file.ContentID, file.EpisodeID
	}
	row, found, err := h.lookupVirtualCandidateRowDetailed(ctx, candidateURI, contentID, episodeID, ownerID)
	if err != nil || !found || row == nil || row.ProbeUpdatedAt == nil {
		return false
	}
	return len(row.AudioTracks) > 0 || len(row.SubtitleTracks) > 0
}

// preferredProbedFallbackCandidate returns the id of the best remaining
// candidate whose catalog row carries measured inventory, so a cross-release
// fallback can bind a release it can actually carry the viewer's audio and
// subtitle selections onto instead of stepping through unprobed stubs one hop
// at a time. It returns "" when the current preference is already that probed
// candidate, when no probed sibling remains, or when the listing cannot answer:
// the caller then keeps the ordinary walk, and an unknown-inventory attempt no
// longer resets the selections. Best-effort by design — a listing failure must
// never fail the fallback.
func (h *PlaybackHandler) preferredProbedFallbackCandidate(ctx context.Context, file *models.MediaFile, excludedCandidateIDs []string, currentPreference string, userID int, profileID string) string {
	if h == nil || h.VirtualPlaybackStreamLister == nil || file == nil {
		return ""
	}
	listCtx, cancel := context.WithTimeout(ctx, virtualFallbackSiblingListBudget)
	defer cancel()
	streams, err := h.VirtualPlaybackStreamLister.ListVirtualPlaybackStreams(
		listCtx, file.FilePath, userID, profileID, file.VirtualOwnerInstallationID,
	)
	if err != nil || len(streams) == 0 {
		return ""
	}
	// Keep the same sibling set the start path's own candidate list uses: same
	// release identity, same profile, and no provider-order override. Trusting
	// the provider's order is an explicit operator choice, so the fallback must
	// not reorder it either.
	streams = visibleVirtualPlaybackStreams(streams)
	if virtualProviderOrderPreserved(streams) {
		return ""
	}
	streams = filterVirtualPlaybackStreams(file, streams)
	if len(streams) == 0 {
		return ""
	}
	excluded := make(map[string]struct{}, len(excludedCandidateIDs))
	for _, id := range excludedCandidateIDs {
		excluded[id] = struct{}{}
	}
	for _, cand := range streams {
		id := strings.TrimSpace(cand.ID)
		if id == "" || cand.Rejected {
			// A custom-format-rejected release never jumps an accepted one,
			// probed or not: compatibility still wins the top slot.
			continue
		}
		if _, skip := excluded[id]; skip {
			continue
		}
		if !h.virtualCandidateHasProbedInventory(listCtx, cand.URI, file, file.VirtualOwnerInstallationID) {
			continue
		}
		if id == currentPreference {
			// The resolver is already bound to this release; nothing to change.
			return ""
		}
		return id
	}
	return ""
}

// partitionVirtualStreamsAcceptedFirst stably moves rejected streams behind
// accepted ones, preserving the ranked order within each group. Accepted
// always sort before rejected; a rejected stream stays selectable as a
// last resort.
func partitionVirtualStreamsAcceptedFirst(streams []VirtualPlaybackStream) []VirtualPlaybackStream {
	rejected := 0
	for _, stream := range streams {
		if stream.Rejected {
			rejected++
		}
	}
	if rejected == 0 || rejected == len(streams) {
		return streams
	}
	ordered := make([]VirtualPlaybackStream, 0, len(streams))
	for _, stream := range streams {
		if !stream.Rejected {
			ordered = append(ordered, stream)
		}
	}
	for _, stream := range streams {
		if stream.Rejected {
			ordered = append(ordered, stream)
		}
	}
	return ordered
}

// rankVirtualCandidatesForDevice ranks the candidate list for the requesting
// device using its persisted capability profile. When no profile is known the
// provider returns candidates unchanged (device order), preserving the
// pre-feature resolution-label behavior.
func (h *PlaybackHandler) rankVirtualCandidatesForDevice(r *http.Request, streams []VirtualPlaybackStream) ([]VirtualPlaybackStream, plugins.DeviceCapabilities) {
	if len(streams) == 0 {
		return streams, plugins.DeviceCapabilities{}
	}
	capabilities, ok := h.requestDeviceCapabilities(r)
	if !ok {
		return streams, plugins.DeviceCapabilities{}
	}
	meta := make([]plugins.VirtualStreamMetadata, len(streams))
	for i := range streams {
		meta[i] = &streams[i]
	}
	ranked := plugins.RankVirtualStreamsForDevice(meta, capabilities)
	out := make([]VirtualPlaybackStream, len(ranked))
	for i := range ranked {
		stream, ok := ranked[i].(*VirtualPlaybackStream)
		if !ok {
			return streams, capabilities
		}
		out[i] = *stream
	}
	return out, capabilities
}

// requestDeviceID returns the durable device ID the web/app clients send on
// every request (X-Vio-Device-Id, legacy X-Silo-Device-Id accepted). Empty
// means "no device identity": ranking then falls back to provider order.
func requestDeviceID(r *http.Request) string {
	return clampHeaderValue(headerValue(r.Header, deviceIDHeader, legacyDeviceIDHeader), 128)
}

// providerDeviceCapabilitySource implements plugins.DeviceCapabilityProfileSource
// on top of the shared UserStoreProvider, with a short TTL cache so Postgres is
// never on the playback/pre-warm critical path. It reads the native user ID
// from the request context (which the UserStoreProvider is scoped by).
type providerDeviceCapabilitySource struct {
	provider userstore.UserStoreProvider
	ttl      time.Duration
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]providerCapabilityEntry
}

func (s *providerDeviceCapabilitySource) CapabilitiesForProfile(profileID string) plugins.DeviceCapabilities {
	if s == nil || s.provider == nil || profileID == "" {
		return plugins.DeviceCapabilities{}
	}
	// The playback handler's ranking hook has no request context. Keep the
	// profile-only fallback conservative; request-scoped callers use the full
	// DeviceCapabilitiesFor method below.
	return plugins.DeviceCapabilities{}
}

type providerCapabilityEntry struct {
	device    plugins.DeviceCapabilities
	found     bool
	expiresAt time.Time
}

// newProviderDeviceCapabilitySource creates a source with a 5-minute found TTL
// and 1-minute misses.
func NewProviderDeviceCapabilitySource(provider userstore.UserStoreProvider) *providerDeviceCapabilitySource {
	return &providerDeviceCapabilitySource{
		provider: provider,
		ttl:      5 * time.Minute,
		now:      time.Now,
		entries:  make(map[string]providerCapabilityEntry),
	}
}

// DeviceCapabilitiesFor returns the persisted capability profile for the
// (user, profile, device) encoded in ctx + profileID + deviceID.
func (s *providerDeviceCapabilitySource) DeviceCapabilitiesFor(ctx context.Context, profileID, deviceID string) (plugins.DeviceCapabilities, bool) {
	if s == nil || s.provider == nil || profileID == "" || deviceID == "" {
		return plugins.DeviceCapabilities{}, false
	}
	userID := apimw.GetUserID(ctx)
	if userID == 0 {
		return plugins.DeviceCapabilities{}, false
	}
	key := userDeviceKey(userID, profileID, deviceID)
	now := s.now()
	s.mu.Lock()
	entry, ok := s.entries[key]
	s.mu.Unlock()
	if ok && now.Before(entry.expiresAt) {
		return entry.device, entry.found
	}

	store, err := s.provider.ForUser(ctx, userID)
	if err != nil || store == nil {
		return plugins.DeviceCapabilities{}, false
	}
	registry, ok := store.(userstore.DeviceProfileRegistry)
	if !ok {
		return plugins.DeviceCapabilities{}, false
	}
	profile, err := registry.GetDeviceProfile(ctx, profileID, deviceID)
	if err != nil || profile == nil {
		return plugins.DeviceCapabilities{}, false
	}
	device := plugins.DeviceCapabilitiesFromProfile(*profile)
	ttl := s.ttl
	s.mu.Lock()
	// Opportunistic hygiene on write: expired entries leave only when their
	// key is re-read, so bound the map here to keep growth finite per process.
	for k, e := range s.entries {
		if now.After(e.expiresAt) && k != key {
			delete(s.entries, k)
		}
	}
	const maxDeviceCapabilityEntries = 8192
	for len(s.entries) >= maxDeviceCapabilityEntries {
		oldestKey := ""
		var oldest time.Time
		for k, e := range s.entries {
			if oldestKey == "" || e.expiresAt.Before(oldest) {
				oldestKey, oldest = k, e.expiresAt
			}
		}
		if oldestKey == "" {
			break
		}
		delete(s.entries, oldestKey)
	}
	s.entries[key] = providerCapabilityEntry{device: device, found: true, expiresAt: now.Add(ttl)}
	s.mu.Unlock()
	return device, true
}

func userDeviceKey(userID int, profileID, deviceID string) string {
	// A single string key is all the map needs; the  separators keep the
	// three components unambiguous.
	return strconv.Itoa(userID) + "\x00" + profileID + "\x00" + deviceID
}
