package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/nodepool"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/transcodenode"
)

// audioOrdinalFixtureFileV3 builds a probed file whose audio tracks carry
// ABSOLUTE container stream indices (the domain the scanner stores), with
// subtitles interleaved between the audio streams — the shape that breaks a
// naive array-position or absolute-index mapping.
func audioOrdinalFixtureFileV3(t *testing.T) *models.MediaFile {
	t.Helper()
	file := v3HandlerFixtureFile(t)
	file.AudioTracks = []models.AudioTrack{
		{Index: 1, Language: "eng", Codec: "aac", Channels: 2, Layout: "stereo", Default: true},
		{Index: 3, Language: "fra", Codec: "aac", Channels: 2, Layout: "stereo"},
	}
	file.SubtitleTracks = []models.SubtitleTrack{{Index: 2, Language: "eng", Codec: "srt"}}
	return file
}

// audioOrdinalAudioFirstFixtureFileV3 builds a probed file whose audio track
// sits at absolute stream 0 (audio muxed before video, or an audio-only
// container): stream index 0 is a real container position, not a missing
// index.
func audioOrdinalAudioFirstFixtureFileV3(t *testing.T) *models.MediaFile {
	t.Helper()
	file := v3HandlerFixtureFile(t)
	file.AudioTracks = []models.AudioTrack{
		{Index: 0, Language: "eng", Codec: "aac", Channels: 2, Layout: "stereo", Default: true},
	}
	return file
}

// audioOrdinalAudioOnlyZeroBasedFixtureFileV3 builds a probed audio-only file
// whose two audio tracks carry indexes [0, 1] — the shape the old `Index > 0`
// ranking mis-ranked: selecting the second track (Index 1) must yield the
// audio-only ordinal 1, not the array-position fallback.
func audioOrdinalAudioOnlyZeroBasedFixtureFileV3(t *testing.T) *models.MediaFile {
	t.Helper()
	file := v3HandlerFixtureFile(t)
	file.AudioTracks = []models.AudioTrack{
		{Index: 0, Language: "eng", Codec: "aac", Channels: 2, Layout: "stereo", Default: true},
		{Index: 1, Language: "fra", Codec: "aac", Channels: 2, Layout: "stereo"},
	}
	return file
}

// audioOrdinalSynthesizedZeroIndexFixtureFileV3 builds the synthesized
// virtual-source shape: every track carries Index 0 (the zero value of an
// unset field), so the array position is the ordinal for all tracks.
func audioOrdinalSynthesizedZeroIndexFixtureFileV3(t *testing.T) *models.MediaFile {
	t.Helper()
	file := v3HandlerFixtureFile(t)
	file.AudioTracks = []models.AudioTrack{
		{Index: 0, Language: "eng", Codec: "aac", Channels: 2, Layout: "stereo", Default: true},
		{Index: 0, Language: "fra", Codec: "aac", Channels: 2, Layout: "stereo"},
	}
	return file
}

// audioOrdinalPlanV3 is a transcode plan whose SelectedTracks.Audio.Index is
// the array position of the selected audio track (the protocol domain).
func audioOrdinalPlanV3(selectedAudioIndex int) *playback.PlanV3 {
	index := selectedAudioIndex
	return &playback.PlanV3{
		PlanID:   "plan:audio-ordinal",
		Delivery: playback.DeliveryTranscodeHLSV3,
		SelectedTracks: playback.SelectedTracksV3{
			Audio: &playback.TrackIdentityV3{ID: playback.TrackIDV3(42, "audio", selectedAudioIndex), Index: &index},
		},
	}
}

func audioOrdinalResultV3(selectedAudioIndex int) playback.PlannerResultV3 {
	return playback.PlannerResultV3{
		Plan: audioOrdinalPlanV3(selectedAudioIndex), PlayMethod: playback.PlayTranscode,
		TargetVideoCodec: "h264", TargetAudioCodec: "aac", TargetResolution: "1080p",
		SubtitleTrackIndex: -1, SubtitleTransportTrackIndex: -1,
	}
}

// TestAudioStreamOrdinalV3MapsAbsoluteIndexToAudioOnlyOrdinal verifies the
// conversion at the unit level: the selected track's array position is mapped
// to its rank among the file's audio tracks ordered by absolute stream Index,
// and tracks without a usable Index fall back to the array position.
func TestAudioStreamOrdinalV3MapsAbsoluteIndexToAudioOnlyOrdinal(t *testing.T) {
	file := audioOrdinalFixtureFileV3(t)
	tests := []struct {
		name          string
		selectedIndex int
		want          int
	}{
		{name: "first audio at stream 1 maps to ordinal 0", selectedIndex: 0, want: 0},
		{name: "second audio at stream 3 maps to ordinal 1", selectedIndex: 1, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := audioStreamOrdinalV3(file, tt.selectedIndex); got != tt.want {
				t.Fatalf("audioStreamOrdinalV3(file, %d) = %d, want %d", tt.selectedIndex, got, tt.want)
			}
		})
	}
}

// TestAudioStreamOrdinalV3AudioFirstAtStreamZero verifies a real probed audio
// track at absolute stream 0 (audio muxed before video, or an audio-only
// container) maps to ordinal 0: stream index 0 is a real container position.
func TestAudioStreamOrdinalV3AudioFirstAtStreamZero(t *testing.T) {
	file := audioOrdinalAudioFirstFixtureFileV3(t)
	if got := audioStreamOrdinalV3(file, 0); got != 0 {
		t.Fatalf("audioStreamOrdinalV3(file, 0) = %d, want 0", got)
	}
}

// TestAudioStreamOrdinalV3MultiTrackAudioOnlyZeroBasedIndexes verifies a
// probed audio-only inventory with indexes [0, 1] ranks by absolute stream
// index: selecting the second track (Index 1) yields ordinal 1, not the
// array-position fallback.
func TestAudioStreamOrdinalV3MultiTrackAudioOnlyZeroBasedIndexes(t *testing.T) {
	file := audioOrdinalAudioOnlyZeroBasedFixtureFileV3(t)
	tests := []struct {
		name          string
		selectedIndex int
		want          int
	}{
		{name: "first audio at stream 0 maps to ordinal 0", selectedIndex: 0, want: 0},
		{name: "second audio at stream 1 maps to ordinal 1", selectedIndex: 1, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := audioStreamOrdinalV3(file, tt.selectedIndex); got != tt.want {
				t.Fatalf("audioStreamOrdinalV3(file, %d) = %d, want %d", tt.selectedIndex, got, tt.want)
			}
		})
	}
}

// TestAudioStreamOrdinalV3SynthesizedZeroIndexInventoryFallsBack verifies the
// synthesized virtual-source shape (every track Index 0) keeps the array
// position as the ordinal for all tracks, even though Index 0 alone would be
// a valid rankable position in a probed inventory.
func TestAudioStreamOrdinalV3SynthesizedZeroIndexInventoryFallsBack(t *testing.T) {
	file := audioOrdinalSynthesizedZeroIndexFixtureFileV3(t)
	for _, tt := range []struct {
		selectedIndex int
		want          int
	}{
		{selectedIndex: 0, want: 0},
		{selectedIndex: 1, want: 1},
	} {
		if got := audioStreamOrdinalV3(file, tt.selectedIndex); got != tt.want {
			t.Fatalf("audioStreamOrdinalV3(file, %d) = %d, want %d", tt.selectedIndex, got, tt.want)
		}
	}
}

// TestAudioStreamOrdinalV3FallsBackToArrayPositionForSynthesizedTracks verifies
// the legacy synthesized case: virtual-source tracks carry no container stream
// index, so the array position is the ordinal.
func TestAudioStreamOrdinalV3FallsBackToArrayPositionForSynthesizedTracks(t *testing.T) {
	file := v3HandlerFixtureFile(t)
	file.AudioTracks = []models.AudioTrack{
		{Language: "eng", Codec: "aac", Channels: 2, Layout: "stereo", Default: true},
		{Language: "fra", Codec: "aac", Channels: 2, Layout: "stereo"},
	}
	for _, tt := range []struct {
		selectedIndex int
		want          int
	}{
		{selectedIndex: 0, want: 0},
		{selectedIndex: 1, want: 1},
	} {
		if got := audioStreamOrdinalV3(file, tt.selectedIndex); got != tt.want {
			t.Fatalf("audioStreamOrdinalV3(file, %d) = %d, want %d", tt.selectedIndex, got, tt.want)
		}
	}
}

// TestPrepareLocalTransportV3AudioTrackIndexIsAudioOnlyOrdinal drives the
// local start path (playback_v3.go:3667) and asserts the FFmpeg args emitted
// for the selected track use the audio-only ordinal ffmpeg's `0:a:N` expects.
func TestPrepareLocalTransportV3AudioTrackIndexIsAudioOnlyOrdinal(t *testing.T) {
	tests := []struct {
		name          string
		selectedIndex int
		wantMap       string
	}{
		{name: "first audio at stream 1", selectedIndex: 0, wantMap: "0:a:0?"},
		{name: "second audio at stream 3", selectedIndex: 1, wantMap: "0:a:1?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := audioOrdinalFixtureFileV3(t)
			transcodeDir := t.TempDir()
			argsPath := filepath.Join(t.TempDir(), tt.name+"-args.txt")
			ffmpegPath := writePlaybackArgsRecordingFFmpegV3(t, argsPath)
			handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
			handler.PlaybackConfig = func() config.PlaybackConfig {
				return config.PlaybackConfig{FFmpegPath: ffmpegPath, TranscodeDir: transcodeDir, TranscodeEnabled: true, HWAccel: playback.HWAccelNone}
			}
			result := audioOrdinalResultV3(tt.selectedIndex)
			transport, transportErr := handler.prepareLocalTransportV3(
				httptest.NewRequest(http.MethodPost, "/", nil),
				&playback.Session{ID: "session-audio-ordinal-local", UserID: 7, ProfileID: "profile-1"},
				file, result, preparedTimelineV3{}, mediaAuthModeV3{},
			)
			if transportErr != nil {
				t.Fatalf("prepare local transport: %v (cause: %v)", transportErr, transportErr.cause)
			}
			transport.rollback()
			args, readErr := os.ReadFile(argsPath)
			if readErr != nil {
				t.Fatalf("read local FFmpeg args: %v", readErr)
			}
			// The recording script writes one arg per line, so the map specifier
			// appears as its own line following the -map flag.
			if !strings.Contains(string(args), "-map\n"+tt.wantMap+"\n") {
				t.Fatalf("local FFmpeg args missing -map %s: %s", tt.wantMap, string(args))
			}
		})
	}
}

// TestPrepareLocalTransportV3AudioFirstAtStreamZeroEmitsOrdinalZero drives the
// local start path with a real probed audio track at absolute stream 0 (audio
// muxed before video, or an audio-only container) and asserts the emitted map
// is `0:a:0?` — stream index 0 is a real container position, not a missing
// index.
func TestPrepareLocalTransportV3AudioFirstAtStreamZeroEmitsOrdinalZero(t *testing.T) {
	file := audioOrdinalAudioFirstFixtureFileV3(t)
	transcodeDir := t.TempDir()
	argsPath := filepath.Join(t.TempDir(), "audio-first-args.txt")
	ffmpegPath := writePlaybackArgsRecordingFFmpegV3(t, argsPath)
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.PlaybackConfig = func() config.PlaybackConfig {
		return config.PlaybackConfig{FFmpegPath: ffmpegPath, TranscodeDir: transcodeDir, TranscodeEnabled: true, HWAccel: playback.HWAccelNone}
	}
	transport, transportErr := handler.prepareLocalTransportV3(
		httptest.NewRequest(http.MethodPost, "/", nil),
		&playback.Session{ID: "session-audio-ordinal-audio-first", UserID: 7, ProfileID: "profile-1"},
		file, audioOrdinalResultV3(0), preparedTimelineV3{}, mediaAuthModeV3{},
	)
	if transportErr != nil {
		t.Fatalf("prepare local transport: %v (cause: %v)", transportErr, transportErr.cause)
	}
	transport.rollback()
	args, readErr := os.ReadFile(argsPath)
	if readErr != nil {
		t.Fatalf("read local FFmpeg args: %v", readErr)
	}
	if !strings.Contains(string(args), "-map\n0:a:0?\n") {
		t.Fatalf("local FFmpeg args missing -map 0:a:0? for the stream-0 audio track: %s", string(args))
	}
}

// TestPrepareLocalTransportV3AudioOnlyZeroBasedIndexesEmitsOrdinals drives the
// local start path with a probed audio-only inventory [0, 1] and asserts the
// emitted maps: selecting the second track (Index 1) must emit `0:a:1?`, not
// the array-position fallback.
func TestPrepareLocalTransportV3AudioOnlyZeroBasedIndexesEmitsOrdinals(t *testing.T) {
	tests := []struct {
		name          string
		selectedIndex int
		wantMap       string
	}{
		{name: "first audio at stream 0", selectedIndex: 0, wantMap: "0:a:0?"},
		{name: "second audio at stream 1", selectedIndex: 1, wantMap: "0:a:1?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := audioOrdinalAudioOnlyZeroBasedFixtureFileV3(t)
			transcodeDir := t.TempDir()
			argsPath := filepath.Join(t.TempDir(), tt.name+"-args.txt")
			ffmpegPath := writePlaybackArgsRecordingFFmpegV3(t, argsPath)
			handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
			handler.PlaybackConfig = func() config.PlaybackConfig {
				return config.PlaybackConfig{FFmpegPath: ffmpegPath, TranscodeDir: transcodeDir, TranscodeEnabled: true, HWAccel: playback.HWAccelNone}
			}
			transport, transportErr := handler.prepareLocalTransportV3(
				httptest.NewRequest(http.MethodPost, "/", nil),
				&playback.Session{ID: "session-audio-ordinal-audio-only", UserID: 7, ProfileID: "profile-1"},
				file, audioOrdinalResultV3(tt.selectedIndex), preparedTimelineV3{}, mediaAuthModeV3{},
			)
			if transportErr != nil {
				t.Fatalf("prepare local transport: %v (cause: %v)", transportErr, transportErr.cause)
			}
			transport.rollback()
			args, readErr := os.ReadFile(argsPath)
			if readErr != nil {
				t.Fatalf("read local FFmpeg args: %v", readErr)
			}
			if !strings.Contains(string(args), "-map\n"+tt.wantMap+"\n") {
				t.Fatalf("local FFmpeg args missing -map %s: %s", tt.wantMap, string(args))
			}
		})
	}
}

// TestPrepareLocalTransportV3SynthesizedZeroIndexInventoryKeepsArrayPosition
// drives the local start path with the synthesized virtual-source shape (every
// track Index 0) and asserts the array-position fallback is preserved: the
// second track still emits `0:a:1?`.
func TestPrepareLocalTransportV3SynthesizedZeroIndexInventoryKeepsArrayPosition(t *testing.T) {
	file := audioOrdinalSynthesizedZeroIndexFixtureFileV3(t)
	transcodeDir := t.TempDir()
	argsPath := filepath.Join(t.TempDir(), "synthesized-args.txt")
	ffmpegPath := writePlaybackArgsRecordingFFmpegV3(t, argsPath)
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.PlaybackConfig = func() config.PlaybackConfig {
		return config.PlaybackConfig{FFmpegPath: ffmpegPath, TranscodeDir: transcodeDir, TranscodeEnabled: true, HWAccel: playback.HWAccelNone}
	}
	transport, transportErr := handler.prepareLocalTransportV3(
		httptest.NewRequest(http.MethodPost, "/", nil),
		&playback.Session{ID: "session-audio-ordinal-synthesized", UserID: 7, ProfileID: "profile-1"},
		file, audioOrdinalResultV3(1), preparedTimelineV3{}, mediaAuthModeV3{},
	)
	if transportErr != nil {
		t.Fatalf("prepare local transport: %v (cause: %v)", transportErr, transportErr.cause)
	}
	transport.rollback()
	args, readErr := os.ReadFile(argsPath)
	if readErr != nil {
		t.Fatalf("read local FFmpeg args: %v", readErr)
	}
	if !strings.Contains(string(args), "-map\n0:a:1?\n") {
		t.Fatalf("local FFmpeg args missing -map 0:a:1? for the synthesized second track: %s", string(args))
	}
}

// TestPrepareRemoteTransportV3AudioTrackIndexIsAudioOnlyOrdinal drives the
// remote start path (playback_v3.go:3888) and asserts the TranscodeStartRequest
// sent to the node carries the audio-only ordinal.
func TestPrepareRemoteTransportV3AudioTrackIndexIsAudioOnlyOrdinal(t *testing.T) {
	tests := []struct {
		name          string
		selectedIndex int
		wantOrdinal   int
	}{
		{name: "first audio at stream 1", selectedIndex: 0, wantOrdinal: 0},
		{name: "second audio at stream 3", selectedIndex: 1, wantOrdinal: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got transcodenode.TranscodeStartRequest
			node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/transcode/start":
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Errorf("decode remote start: %v", err)
					}
					writeJSON(w, http.StatusAccepted, transcodenode.TranscodeStartResponse{SessionID: got.SessionID, Status: "started"})
				default:
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer node.Close()

			handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
			handler.JWTSecret = "test-secret"
			file := audioOrdinalFixtureFileV3(t)
			result := audioOrdinalResultV3(tt.selectedIndex)
			transport, transportErr := handler.prepareRemoteTransportV3(
				httptest.NewRequest(http.MethodPost, "/", nil),
				&playback.Session{ID: "session-audio-ordinal-remote", UserID: 7, ProfileID: "profile-1"},
				file, result,
				nodepool.Plan{TranscodeNode: &nodepool.Node{URL: node.URL}}, preparedTimelineV3{}, mediaAuthModeV3{},
			)
			if transportErr != nil {
				t.Fatalf("prepare remote transport: %v (cause: %v)", transportErr, transportErr.cause)
			}
			defer transport.rollback()
			if got.AudioTrackIndex != tt.wantOrdinal {
				t.Fatalf("remote AudioTrackIndex = %d, want audio-only ordinal %d", got.AudioTrackIndex, tt.wantOrdinal)
			}
		})
	}
}

// TestAudioStreamOrdinalV3PlanRoundTripKeepsProtocolDomain verifies the plan's
// SelectedTracks.Audio.Index round-trip stays in the array-position domain:
// plannedAudioTrackIndexV3 returns the plan's stored index verbatim, and only
// audioStreamOrdinalV3 converts it to the ffmpeg ordinal at the transport edge.
func TestAudioStreamOrdinalV3PlanRoundTripKeepsProtocolDomain(t *testing.T) {
	file := audioOrdinalFixtureFileV3(t)
	result := audioOrdinalResultV3(1) // plan stores array position 1 (the French track)
	if planned := plannedAudioTrackIndexV3(result, 0); planned != 1 {
		t.Fatalf("plannedAudioTrackIndexV3 = %d, want 1 (array position preserved)", planned)
	}
	if ordinal := audioStreamOrdinalV3(file, plannedAudioTrackIndexV3(result, 0)); ordinal != 1 {
		t.Fatalf("audioStreamOrdinalV3 = %d, want 1 (audio-only ordinal for stream 3)", ordinal)
	}
	if !strings.Contains(result.Plan.SelectedTracks.Audio.ID, "audio:") {
		t.Fatalf("plan audio identity malformed: %q", result.Plan.SelectedTracks.Audio.ID)
	}
}

// TestPlannedAudioTrackIndexV3PrefersCommittedPlanOverFailClosedSession pins the
// transcode start's index resolution after a verified absence. The session
// records the fail-closed sentinel -1 when its committed selection vanishes, but
// the transcode path resolves the audio index through plannedAudioTrackIndexV3,
// which uses the committed plan's selected array position and only falls back to
// the session index when the plan carries none. The -1 sentinel therefore never
// reaches the transport while the plan still names a valid stream.
func TestPlannedAudioTrackIndexV3PrefersCommittedPlanOverFailClosedSession(t *testing.T) {
	result := audioOrdinalResultV3(1)
	if got := plannedAudioTrackIndexV3(result, -1); got != 1 {
		t.Fatalf("plannedAudioTrackIndexV3(result, -1) = %d, want the committed plan index 1", got)
	}
	if got := plannedAudioTrackIndexV3(playback.PlannerResultV3{}, -1); got != -1 {
		t.Fatalf("plannedAudioTrackIndexV3(empty result, -1) = %d, want the -1 fallback", got)
	}
}

// Recovery evidence discipline (round-5 review finding 2): written response
// headers are not media delivery. The relay explicitly forwards header-only
// 204/304/416/zero-length 200 responses, so direct-play recovery must require
// a 200/206 status AND positive body bytes.
func TestVirtualCandidateDeliveryEvidence(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		bytes      int64
		want       bool
	}{
		{"200 with media bytes", http.StatusOK, 1500, true},
		{"206 with media bytes", http.StatusPartialContent, 1500, true},
		{"empty 200 (zero-length body)", http.StatusOK, 0, false},
		{"204 no content", http.StatusNoContent, 0, false},
		{"304 not modified", http.StatusNotModified, 0, false},
		{"416 range not satisfiable", http.StatusRequestedRangeNotSatisfiable, 0, false},
		{"error status with bytes", http.StatusInternalServerError, 1500, false},
		{"bytes without committed status", 0, 1500, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := virtualCandidateDeliveryEvidence(tc.statusCode, tc.bytes); got != tc.want {
				t.Fatalf("deliveryEvidence(%d, %d) = %v, want %v", tc.statusCode, tc.bytes, got, tc.want)
			}
		})
	}
}
