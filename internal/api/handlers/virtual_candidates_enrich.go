package handlers

import (
	"context"
	"log/slog"
	"strings"

	"github.com/Silo-Server/silo-server/internal/models"
)

// EnrichVirtualCandidates probes freshly persisted provider candidates so their
// subtitle and audio tracks are present in the catalog before the refresh job
// completes. It resolves each candidate to its provider URL and runs the
// existing probe path, then persists the evidence through the same CAS-fenced
// write the playback path uses.
//
// It is best-effort and bounded: a probe failure is warned and skipped, and
// every candidate is attempted before the deadline fires. The int result is how
// many candidates were successfully probed and persisted.
func (h *PlaybackHandler) EnrichVirtualCandidates(ctx context.Context, contentID, episodeID string, userID int, profileID string, streams []VirtualPlaybackStream) int {
	if h == nil || (h.VirtualPlaybackSourceProber == nil && h.VirtualPlaybackSourceProberWithHeaders == nil) {
		return 0
	}
	if ctx == nil {
		ctx = context.Background()
	}
	enrichCtx, cancel := context.WithTimeout(ctx, virtualEnrichmentBudget)
	defer cancel()

	enriched := 0
	for i := range streams {
		if enrichCtx.Err() != nil {
			break
		}
		stream := streams[i]
		if strings.TrimSpace(stream.ProviderURL) == "" {
			continue
		}
		row := h.virtualRowForCandidate(enrichCtx, stream, contentID, episodeID)
		if row == nil || row.ID <= 0 {
			// Without a persisted row there is nowhere to store the evidence;
			// the probe would only burn provider time.
			continue
		}
		probeTransient := cloneVirtualProbeTransient(*row)
		probeTransient.FilePath = stream.URI
		probeTransient.Container = virtualURIScheme
		probed, err := h.probeVirtualSource(enrichCtx, stream.ProviderURL, &probeTransient, stream.RequestHeaders)
		if err != nil || probed == nil {
			slog.WarnContext(enrichCtx, "virtual candidates refresh: probe failed; leaving candidate unenriched",
				"component", "api", "content_id", contentID, "candidate_uri", stream.URI, "error", err)
			continue
		}
		if probeTransient.ID > 0 {
			probed.ID = probeTransient.ID
			probed.MediaFolderID = probeTransient.MediaFolderID
		}
		if probeTransient.Duration > 0 && probed.Duration <= 0 {
			probed.Duration = probeTransient.Duration
		}
		mergeVirtualCandidateTracks(probed, stream)
		args, _, ok := h.virtualProbeEvidenceArgs(enrichCtx, row, stream.URI, probed, true)
		if !ok {
			continue
		}
		if !h.persistVirtualEvidenceDirect(enrichCtx, args) {
			slog.WarnContext(enrichCtx, "virtual candidates refresh: probe evidence did not persist",
				"component", "api", "content_id", contentID, "candidate_uri", stream.URI)
			continue
		}
		enriched++
	}
	return enriched
}

// virtualRowForCandidate resolves the persisted row for a listed candidate.
// The exact URI is tried first (the row the sink just wrote), then the
// provider-neutral path scoped by content, episode and owner.
func (h *PlaybackHandler) virtualRowForCandidate(ctx context.Context, stream VirtualPlaybackStream, contentID, episodeID string) *models.MediaFile {
	owner := stream.OwnerInstallationID
	if h.VirtualFileLookup != nil {
		if row, err := h.VirtualFileLookup(ctx, stream.URI); err == nil && row != nil {
			return row
		}
	}
	if h.VirtualCandidateFileLookup != nil {
		neutral := virtualPlaybackNeutralKey(stream.URI)
		if row, err := h.VirtualCandidateFileLookup(ctx, neutral, contentID, episodeID, owner); err == nil && row != nil {
			return row
		}
	}
	return nil
}
