package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// fallbackInventoryFixture is the shared shape of a cross-release fallback: the
// pinned release is dead, the provider still lists siblings under the same
// neutral path, and the fallback loop has to decide which sibling to bind and
// what to do with the viewer's audio/subtitle selections.
type fallbackInventoryFixture struct {
	neutral          string
	pinnedURI        string
	unprobedURI      string
	probedURI        string
	file             *models.MediaFile
	handler          *PlaybackHandler
	started          []playback.TranscodeOpts
	preferredIDs     []string
	probedLookupSeen int
}

func newFallbackInventoryFixture(t *testing.T, sessionID string) *fallbackInventoryFixture {
	t.Helper()
	const neutral = "virtual://series/tt-fallback-inventory/1/1"
	f := &fallbackInventoryFixture{
		neutral:     neutral,
		pinnedURI:   neutral + "?result=dead",
		unprobedURI: neutral + "?result=unprobed",
		probedURI:   neutral + "?result=probed",
	}
	f.file = &models.MediaFile{
		ID:                         41,
		ContentID:                  "series-fallback-inventory",
		EpisodeID:                  "episode-fallback-inventory",
		FilePath:                   f.pinnedURI,
		VirtualOwnerInstallationID: 5,
		AudioTracks: []models.AudioTrack{
			{Index: 0, Language: "ja", Codec: "aac", Channels: 2},
			{Index: 1, Language: "en", Codec: "aac", Channels: 2},
		},
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "subrip"}},
	}
	tempDir := t.TempDir()
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.fileResolver = &fakePinFileResolver{file: f.file}
	h.PlaybackConfig = func() config.PlaybackConfig {
		return config.PlaybackConfig{TranscodeDir: tempDir, TranscodeEnabled: true, HWAccel: playback.HWAccelNone}
	}
	f.handler = h
	return f
}

// withSiblings wires the provider listing and the catalog rows the fallback
// consults. probedRow nil means no sibling carries measured inventory.
func (f *fallbackInventoryFixture) withSiblings(t *testing.T, probedRow *models.MediaFile) {
	t.Helper()
	f.handler.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(
		func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{
				{ID: "unprobed", URI: f.unprobedURI, Container: "mkv"},
				{ID: "probed", URI: f.probedURI, Container: "mkv"},
			}, nil
		})
	f.handler.VirtualFileLookup = func(_ context.Context, path string) (*models.MediaFile, error) {
		if probedRow == nil || path != f.probedURI {
			return nil, nil
		}
		f.probedLookupSeen++
		row := *probedRow
		return &row, nil
	}
}

// withResolver serves the dead pin on the first attempt and the listed sibling
// on the fallback attempt, honoring the probed-candidate preference exactly
// like the real resolver does.
func (f *fallbackInventoryFixture) withResolver(t *testing.T) {
	t.Helper()
	f.handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(
		func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, excluded []string, preferred string) (ResolvedVirtualMedia, error) {
			if len(excluded) == 0 {
				return ResolvedVirtualMedia{URL: "http://localhost:8080/broken.mp4", URI: uri, CandidateID: "dead"}, nil
			}
			f.preferredIDs = append(f.preferredIDs, preferred)
			switch preferred {
			case "probed":
				return ResolvedVirtualMedia{URL: "http://localhost:8080/live.mp4", URI: f.probedURI, CandidateID: "probed"}, nil
			default:
				return ResolvedVirtualMedia{URL: "http://localhost:8080/unprobed.mp4", URI: f.unprobedURI, CandidateID: "unprobed"}, nil
			}
		})
}

func (f *fallbackInventoryFixture) withTranscode(t *testing.T) {
	t.Helper()
	f.handler.StartTranscodeFunc = func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
		if strings.Contains(opts.InputPath, "broken.mp4") {
			return nil, errors.New("manifest startup failed")
		}
		f.started = append(f.started, opts)
		return playback.NewReadyTranscodeSessionForTesting(t.TempDir(), opts)
	}
}

func (f *fallbackInventoryFixture) run(t *testing.T, sessionID string) *playback.PlanV3 {
	t.Helper()
	plan := &playback.PlanV3{Delivery: playback.DeliveryTranscodeHLSV3}
	result := playback.PlannerResultV3{
		Plan: plan, PlayMethod: playback.PlayTranscode,
		TargetVideoCodec: "h264", TargetAudioCodec: "aac", TargetResolution: "1080p",
		SubtitleTrackIndex: 1, SubtitleTransportTrackIndex: 1, SubtitleBurnIn: true,
	}
	transport, transportErr := f.handler.prepareLocalTransportV3(
		httptest.NewRequest(http.MethodPost, "/", nil),
		&playback.Session{ID: sessionID, UserID: 1, ProfileID: "profile-1", AudioTrackIndex: 1},
		f.file, result, preparedTimelineV3{}, mediaAuthModeV3{},
	)
	if transportErr != nil {
		t.Fatalf("prepareLocalTransportV3: %v (cause: %v)", transportErr, transportErr.cause)
	}
	transport.rollback()
	return plan
}

func hasAudioUnavailableWarning(plan *playback.PlanV3) bool {
	for _, warning := range plan.DegradationWarnings {
		if warning.Code == playback.DegradationWarningAudioTrackUnavailableV3 {
			return true
		}
	}
	return false
}

// A transport hiccup that falls back across releases must not walk unprobed
// alternates while a release with measured inventory is available: the unprobed
// hop cannot carry the viewer's selections, so binding it guarantees the very
// reset this fallback exists to avoid.
func TestCrossReleaseFallbackPrefersProbedSiblingOverUnprobedHop(t *testing.T) {
	f := newFallbackInventoryFixture(t, "fallback-prefer-probed")
	stamp := time.Now().Add(-time.Hour)
	f.withSiblings(t, &models.MediaFile{
		ID: 92, ContentID: f.file.ContentID, EpisodeID: f.file.EpisodeID,
		FilePath: f.probedURI, VirtualOwnerInstallationID: 5, ProbeUpdatedAt: &stamp,
		AudioTracks:    []models.AudioTrack{{Index: 0, Language: "ja"}, {Index: 1, Language: "en"}},
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "subrip"}},
	})
	f.withResolver(t)
	f.withTranscode(t)
	f.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, probeFile *models.MediaFile) (*models.MediaFile, error) {
		probeFile.AudioTracks = []models.AudioTrack{{Index: 0, Language: "ja"}, {Index: 1, Language: "en"}}
		probeFile.SubtitleTracks = []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "subrip"}}
		return probeFile, nil
	}

	plan := f.run(t, "fallback-prefer-probed")

	if len(f.preferredIDs) != 1 || f.preferredIDs[0] != "probed" {
		t.Fatalf("fallback resolver preferences = %v, want the probed candidate", f.preferredIDs)
	}
	if f.probedLookupSeen == 0 {
		t.Fatal("the fallback never consulted the probed sibling's catalog row")
	}
	if len(f.started) != 1 {
		t.Fatalf("started attempts = %d, want exactly the probed fallback", len(f.started))
	}
	if !strings.Contains(f.started[0].InputPath, "live.mp4") {
		t.Fatalf("fallback started %q, want the probed release's bytes", f.started[0].InputPath)
	}
	if strings.Contains(f.started[0].InputPath, "unprobed.mp4") {
		t.Fatal("the fallback started the unprobed candidate")
	}
	// The probed replacement carries the selected audio family and a known
	// subtitle inventory, so the existing known-inventory behavior applies.
	if f.started[0].AudioTrackIndex != 1 {
		t.Fatalf("carried audio index = %d, want 1", f.started[0].AudioTrackIndex)
	}
	if f.started[0].SubtitleTrackIndex != -1 {
		t.Fatalf("subtitle index = %d, want off on a known replacement", f.started[0].SubtitleTrackIndex)
	}
	if hasAudioUnavailableWarning(plan) {
		t.Fatal("a carried audio selection reported the unavailable-audio warning")
	}
}

// When only unprobed candidates remain the walk still tries them, but unknown
// inventory is not evidence that the viewer's selection is absent: the
// requested ordinals stay pending instead of being reset to the defaults.
func TestCrossReleaseFallbackKeepsSelectionsPendingOnUnprobedReplacement(t *testing.T) {
	f := newFallbackInventoryFixture(t, "fallback-unprobed-pending")
	f.withSiblings(t, nil)
	f.withResolver(t)
	f.withTranscode(t)
	f.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, _ *models.MediaFile) (*models.MediaFile, error) {
		return nil, errors.New("probe budget spent")
	}

	plan := f.run(t, "fallback-unprobed-pending")

	if len(f.preferredIDs) != 1 || f.preferredIDs[0] != "" {
		t.Fatalf("fallback resolver preferences = %v, want no probed preference", f.preferredIDs)
	}
	if len(f.started) != 1 {
		t.Fatalf("started attempts = %d, want the unprobed fallback to be tried", len(f.started))
	}
	if !strings.Contains(f.started[0].InputPath, "unprobed.mp4") {
		t.Fatalf("fallback started %q, want the unprobed release's bytes", f.started[0].InputPath)
	}
	if f.started[0].AudioTrackIndex != 1 {
		t.Fatalf("audio index = %d, want the requested 1 kept pending", f.started[0].AudioTrackIndex)
	}
	if f.started[0].SubtitleTrackIndex != 1 {
		t.Fatalf("subtitle index = %d, want the requested 1 kept pending", f.started[0].SubtitleTrackIndex)
	}
	if f.started[0].SubtitleBurnIn {
		t.Fatal("burn-in stayed on for an unverified subtitle ordinal")
	}
	if hasAudioUnavailableWarning(plan) {
		t.Fatal("unknown inventory claimed the audio selection was unavailable")
	}
}

// A probed replacement that genuinely lacks the selected track family is a
// known mismatch and keeps the existing behavior: remap to a playable track and
// report the degradation.
func TestCrossReleaseFallbackKnownMismatchStillDegrades(t *testing.T) {
	f := newFallbackInventoryFixture(t, "fallback-known-mismatch")
	f.withSiblings(t, nil)
	f.withResolver(t)
	f.withTranscode(t)
	f.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, probeFile *models.MediaFile) (*models.MediaFile, error) {
		probeFile.AudioTracks = []models.AudioTrack{{Index: 0, Language: "fr", Codec: "eac3"}}
		probeFile.SubtitleTracks = nil
		return probeFile, nil
	}

	plan := f.run(t, "fallback-known-mismatch")

	if len(f.started) != 1 {
		t.Fatalf("started attempts = %d, want the fallback to start", len(f.started))
	}
	if f.started[0].AudioTrackIndex != 0 {
		t.Fatalf("audio index = %d, want the replacement's playable track (a known mismatch)", f.started[0].AudioTrackIndex)
	}
	if f.started[0].SubtitleTrackIndex != -1 {
		t.Fatalf("subtitle index = %d, want off on a known replacement", f.started[0].SubtitleTrackIndex)
	}
	if !hasAudioUnavailableWarning(plan) {
		t.Fatal("a known audio mismatch did not report the unavailable-audio warning")
	}
}
