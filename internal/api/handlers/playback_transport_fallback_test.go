package handlers

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

func TestRemapFallbackAudioIndexCarriesSelectionByIdentity(t *testing.T) {
	pinned := &models.MediaFile{AudioTracks: []models.AudioTrack{
		{Index: 0, Language: "ja", Codec: "aac", Channels: 2},
		{Index: 1, Language: "en", Codec: "aac", Channels: 2},
	}}
	// The replacement lists the same two track families in the opposite order.
	replacement := &models.MediaFile{AudioTracks: []models.AudioTrack{
		{Index: 0, Language: "en", Codec: "aac", Channels: 2},
		{Index: 1, Language: "ja", Codec: "aac", Channels: 2},
	}}

	if got, carry := remapFallbackAudioIndexV3(pinned, replacement, 1); got != 0 || carry != fallbackAudioCarryMatched {
		t.Fatalf("remapped index = %d carry = %v, want 0/matched (the English track on the replacement)", got, carry)
	}
	if got, carry := remapFallbackAudioIndexV3(pinned, nil, 1); got != -1 || carry != fallbackAudioCarryDegraded {
		t.Fatalf("remapped index without probed facts = %d carry = %v, want -1/degraded (container default)", got, carry)
	}
	if got, carry := remapFallbackAudioIndexV3(pinned, replacement, -1); got != -1 || carry != fallbackAudioCarryNone {
		t.Fatalf("remapped index for no selection = %d carry = %v, want -1/none", got, carry)
	}
	// A selection with no equivalent on the replacement is a degradation, not
	// a successful carry, even though both files have audio. The matcher still
	// returns the replacement's default track for playability.
	otherLanguage := &models.MediaFile{AudioTracks: []models.AudioTrack{
		{Index: 0, Language: "fr", Codec: "aac", Channels: 2},
	}}
	if got, carry := remapFallbackAudioIndexV3(pinned, otherLanguage, 1); got != 0 || carry != fallbackAudioCarryDegraded {
		t.Fatalf("remapped index with no equivalent = %d carry = %v, want 0/degraded (default track, warned)", got, carry)
	}
	// An ordinal past the pinned inventory names no stream; it must not index
	// the slice or claim a carry.
	if got, carry := remapFallbackAudioIndexV3(pinned, replacement, 2); got != -1 || carry != fallbackAudioCarryDegraded {
		t.Fatalf("remapped index past the pinned inventory = %d carry = %v, want -1/degraded", got, carry)
	}
}

// A successful cross-release carry names both sides of the remap so a
// same-language wrong-track carry is visible in the log instead of silent. A
// selection the replacement cannot honor must warn instead of claiming a carry.
func TestCrossReleaseFallbackLogsRemappedAudioCarry(t *testing.T) {
	newLogger := func() (*bytes.Buffer, func()) {
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		return &logs, func() { slog.SetDefault(previous) }
	}

	pinned := &models.MediaFile{ID: 42, AudioTracks: []models.AudioTrack{
		{Index: 0, Language: "ja", Codec: "aac", Channels: 2},
		{Index: 1, Language: "en", Codec: "aac", Channels: 2},
	}}
	replacement := &models.MediaFile{ID: 84, AudioTracks: []models.AudioTrack{
		{Index: 0, Language: "en", Codec: "eac3", Channels: 6},
		{Index: 1, Language: "ja", Codec: "eac3", Channels: 6},
	}}

	logs, restore := newLogger()
	logCrossReleaseAudioCarry(context.Background(), pinned, replacement, 1, 0, fallbackAudioCarryMatched, "candidate-1")
	restore()
	out := logs.String()
	for _, want := range []string{
		"level=INFO",
		"cross-release virtual fallback carried the audio selection",
		"from_audio_index=1",
		"from_audio_language=en",
		"to_audio_index=0",
		"to_audio_language=en",
		"candidate_id=candidate-1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("carry log missing %q:\n%s", want, out)
		}
	}

	logs, restore = newLogger()
	logCrossReleaseAudioCarry(context.Background(), pinned, nil, 1, -1, fallbackAudioCarryDegraded, "candidate-2")
	restore()
	out = logs.String()
	for _, want := range []string{
		"level=WARN",
		"cross-release virtual fallback could not carry the audio selection",
		"using its default audio track",
		"requested_audio_index=1",
		"probed_audio_tracks=0",
		"candidate_id=candidate-2",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("degradation log missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "carried the audio selection") {
		t.Fatalf("degradation was logged as a successful carry:\n%s", out)
	}
}

// A cross-release fallback that cannot carry the viewer's audio selection plays
// the replacement's default, while the plan keeps naming the requested track.
// That divergence must reach the client as the documented audio-unavailable
// degradation warning; an exact inventory carry must stay quiet.
func TestVirtualFallbackReportsAudioUnavailableOnPlan(t *testing.T) {
	tests := []struct {
		name        string
		replacement []models.AudioTrack
		wantWarning bool
	}{
		{
			name:        "replacement keeps the selected inventory",
			replacement: []models.AudioTrack{{Language: "ja", Codec: "aac"}, {Language: "en", Codec: "aac"}},
		},
		{
			name:        "replacement has no equivalent track",
			replacement: []models.AudioTrack{{Language: "fr", Codec: "aac"}},
			wantWarning: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			file := &models.MediaFile{
				ID:                         10,
				ContentID:                  "content-fallback-audio-warning",
				FilePath:                   "virtual://series/tt1/1/1?result=broken",
				VirtualOwnerInstallationID: 5,
				AudioTracks: []models.AudioTrack{
					{Language: "ja", Codec: "aac"},
					{Language: "en", Codec: "aac"},
				},
			}
			handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
			handler.fileResolver = &fakePinFileResolver{file: file}
			handler.PlaybackConfig = func() config.PlaybackConfig {
				return config.PlaybackConfig{TranscodeDir: tempDir, TranscodeEnabled: true, HWAccel: playback.HWAccelNone}
			}
			handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, excluded []string, _ string) (ResolvedVirtualMedia, error) {
				if len(excluded) == 0 {
					return ResolvedVirtualMedia{URL: "http://localhost:8080/broken.mp4", URI: uri, CandidateID: "broken"}, nil
				}
				return ResolvedVirtualMedia{URL: "http://localhost:8080/live.mp4", URI: "virtual://series/tt1/1/1?result=live", CandidateID: "live"}, nil
			})
			handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
				f.AudioTracks = tc.replacement
				return f, nil
			}
			handler.StartTranscodeFunc = func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
				if strings.Contains(opts.InputPath, "broken.mp4") {
					return nil, errors.New("manifest startup failed")
				}
				return playback.NewReadyTranscodeSessionForTesting(tempDir, opts)
			}
			plan := &playback.PlanV3{Delivery: playback.DeliveryTranscodeHLSV3}
			result := playback.PlannerResultV3{
				Plan: plan, PlayMethod: playback.PlayTranscode,
				TargetVideoCodec: "h264", TargetAudioCodec: "aac", TargetResolution: "1080p",
				SubtitleTrackIndex: -1, SubtitleTransportTrackIndex: -1,
			}
			transport, transportErr := handler.prepareLocalTransportV3(
				httptest.NewRequest(http.MethodPost, "/", nil),
				&playback.Session{ID: "fallback-audio-warning-" + tc.name, UserID: 1, ProfileID: "profile-1", AudioTrackIndex: 1},
				file, result, preparedTimelineV3{}, mediaAuthModeV3{},
			)
			if transportErr != nil {
				t.Fatalf("prepareLocalTransportV3: %v (cause: %v)", transportErr, transportErr.cause)
			}
			transport.rollback()

			found := false
			for _, warning := range plan.DegradationWarnings {
				if warning.Code == playback.DegradationWarningAudioTrackUnavailableV3 {
					found = true
				}
			}
			if found != tc.wantWarning {
				t.Fatalf("audio-unavailable warning present = %v, want %v (warnings: %+v)", found, tc.wantWarning, plan.DegradationWarnings)
			}
		})
	}
}
