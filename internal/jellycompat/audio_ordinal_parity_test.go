package jellycompat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// writeCompatArgsRecordingFFmpeg returns a fake ffmpeg that answers the
// -bsfs/-encoders probes the transcode path runs, records its full argument
// list to argsPath, then sleeps. Mirrors the v3 argument-recording helper so
// tests can assert the emitted `-map 0:a:N?` specifier.
func writeCompatArgsRecordingFFmpeg(t *testing.T, argsPath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ffmpeg.sh")
	script := "#!/bin/sh\n" +
		"case \"$2\" in\n" +
		"  -bsfs) exit 0;;\n" +
		"  -encoders) printf ' A....D aac AAC\\n'; exit 0;;\n" +
		"esac\n" +
		"printf '%s\\n' \"$@\" > \"" + argsPath + "\"\n" +
		"exec sleep 30\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write argument-recording fake ffmpeg: %v", err)
	}
	return path
}

// multiAudioOrdinalVersion builds a MULTi release whose catalog track-list
// order differs from the container order: absolute stream 1 is English and
// stream 2 is French, but the track list lists French first. A naive
// array-position mapping would emit `-map 0:a:1?` for the English track and
// select French; the parity fix must emit the audio-only ordinal 0.
func multiAudioOrdinalVersion() catalog.FileVersion {
	version := testCompatVersion()
	version.AudioTracks = []models.AudioTrack{
		{Index: 2, Language: "fra", Codec: "ac3", Default: true, Title: "French"},
		{Index: 1, Language: "eng", Codec: "aac", Title: "English"},
	}
	return version
}

// TestEnsureTranscodeSession_MultiAudioUsesContainerOrdinal drives the local
// transcode builder (streams.go:2730 → TranscodeOpts.AudioTrackIndex →
// buildFFmpegArgs) with a MULTi source and asserts the emitted `-map 0:a:N?`
// selects the English track by its audio-only ordinal, not its array position.
func TestEnsureTranscodeSession_MultiAudioUsesContainerOrdinal(t *testing.T) {
	version := multiAudioOrdinalVersion()
	codec := NewResourceIDCodec()
	source := testCompatSource(codec, version)
	// Client selects the English track: DTO stream index = len(VideoTracks)+1.
	source.SelectedAudioStreamIndex = intPtr(len(version.VideoTracks) + 1)

	filePath := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(filePath, []byte("video"), 0o644); err != nil {
		t.Fatalf("write media file: %v", err)
	}
	argsPath := filepath.Join(t.TempDir(), "args.txt")
	playbackStore := NewPlaybackSessionStore(time.Hour, nil)
	playbackStore.Put(PlaybackSession{ID: "play-1", MediaSources: []PlaybackMediaSource{source}})
	handler := &PlaybackHandler{
		playbackStore: playbackStore,
		sessionMgr: &testCompatSessionManager{sessions: map[string]*playback.Session{
			"upstream-1": {ID: "upstream-1", UserID: 7, ProfileID: "profile-1", MediaFileID: version.FileID, PlayMethod: playback.PlayTranscode},
		}},
		fileResolver: testCompatFileResolver{file: &models.MediaFile{ID: version.FileID, FilePath: filePath}},
		TranscodeDir: t.TempDir(),
		FFmpegPath:   writeCompatArgsRecordingFFmpeg(t, argsPath),
		HWAccel:      playback.HWAccelNone,
		tm:           playback.NewTranscodeManager(),
	}

	transcodeSession, err := handler.ensureTranscodeSession(context.Background(), "play-1", "upstream-1", source)
	if err != nil {
		t.Fatalf("ensureTranscodeSession: %v", err)
	}
	t.Cleanup(func() { _ = transcodeSession.Close() })

	// The English track is the FIRST audio stream in the container (absolute
	// stream 1), so the ffmpeg ordinal is 0 — not the array position 1.
	if got := transcodeSession.Opts().AudioTrackIndex; got != 0 {
		t.Fatalf("AudioTrackIndex = %d, want 0 (English is the first audio stream)", got)
	}

	// The fake ffmpeg creates argsPath by shell redirection before it writes the
	// arguments, so a single successful read can still observe an empty file.
	// Poll until the recorder has written content, not merely until the path
	// exists; otherwise a loaded runner races the truncate against the write and
	// fails with "args were not recorded" even though ffmpeg ran.
	deadline := time.Now().Add(5 * time.Second)
	var args []byte
	for time.Now().Before(deadline) {
		if data, readErr := os.ReadFile(argsPath); readErr == nil && len(data) > 0 {
			args = data
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(args) == 0 {
		t.Fatalf("ffmpeg args were not recorded to %s", argsPath)
	}
	// The negotiated pick is mandatory, so the map carries no optional marker:
	// a source missing it must fail rather than stream video without audio.
	if !strings.Contains(string(args), "-map\n0:a:0\n") {
		t.Fatalf("ffmpeg args missing -map 0:a:0 for the English track: %s", string(args))
	}
}

// TestUpstreamRemuxRecipeCard_MultiAudioUsesContainerOrdinal verifies the
// remux recipe card (streams.go:2416 → NewRemuxRecipeCard) carries the
// audio-only ordinal for a MULTi source.
func TestUpstreamRemuxRecipeCard_MultiAudioUsesContainerOrdinal(t *testing.T) {
	version := multiAudioOrdinalVersion()
	source := testCompatSource(NewResourceIDCodec(), version)
	source.SelectedAudioStreamIndex = intPtr(len(version.VideoTracks) + 1) // English

	card := (&PlaybackHandler{}).upstreamRecipeCard(
		&PlaybackSession{UpstreamSessionID: "upstream"},
		&Session{StreamAppUserID: 7, ProfileID: "profile-1"},
		source,
		"remux",
	)
	if card.AudioTrackIndex != 0 {
		t.Fatalf("remux recipe AudioTrackIndex = %d, want 0 (English is the first audio stream)", card.AudioTrackIndex)
	}
}

// TestCompatRecipeMatchesSource_MultiAudioOrdinalDomain verifies the
// streams.go:290 dedupe comparison stays consistent after the domain change:
// a recipe card built from the same source (both sides now audio-only
// ordinals) must match.
func TestCompatRecipeMatchesSource_MultiAudioOrdinalDomain(t *testing.T) {
	version := multiAudioOrdinalVersion()
	source := testCompatSource(NewResourceIDCodec(), version)
	source.SelectedAudioStreamIndex = intPtr(len(version.VideoTracks) + 1) // English

	card := playback.NewRemuxRecipeCard("upstream", 7, "profile-1", source.FileID, source.TranscodeAudio, compatAudioOrdinalOrDefault(source))
	if !compatRecipeMatchesSource(&card, source) {
		t.Fatalf("recipe built from the same source must dedupe: card %#v source %#v", card, source)
	}
}

// TestEnsureTranscodeSession_MultiAudioSameSourceDedupes verifies the live
// transcode dedupe (streams.go:2732 and :3002) still matches when the opts
// were built from the same MULTi source: both sides flow through the ordinal
// conversion, so a second ensure call adopts the live session instead of
// failing with errCompatRecipeSourceMismatch.
func TestEnsureTranscodeSession_MultiAudioSameSourceDedupes(t *testing.T) {
	version := multiAudioOrdinalVersion()
	source := testCompatSource(NewResourceIDCodec(), version)
	source.SelectedAudioStreamIndex = intPtr(len(version.VideoTracks) + 1) // English

	filePath := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(filePath, []byte("video"), 0o644); err != nil {
		t.Fatalf("write media file: %v", err)
	}
	playbackStore := NewPlaybackSessionStore(time.Hour, nil)
	playbackStore.Put(PlaybackSession{ID: "play-1", MediaSources: []PlaybackMediaSource{source}})
	handler := &PlaybackHandler{
		playbackStore: playbackStore,
		sessionMgr: &testCompatSessionManager{sessions: map[string]*playback.Session{
			"upstream-1": {ID: "upstream-1", UserID: 7, ProfileID: "profile-1", MediaFileID: version.FileID, PlayMethod: playback.PlayTranscode},
		}},
		fileResolver: testCompatFileResolver{file: &models.MediaFile{ID: version.FileID, FilePath: filePath}},
		TranscodeDir: t.TempDir(),
		FFmpegPath:   writeCompatTestFFmpeg(t),
		HWAccel:      playback.HWAccelNone,
		tm:           playback.NewTranscodeManager(),
	}

	first, err := handler.ensureTranscodeSession(t.Context(), "play-1", "upstream-1", source)
	if err != nil {
		t.Fatalf("first ensureTranscodeSession: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	second, err := handler.ensureTranscodeSession(t.Context(), "play-1", "upstream-1", source)
	if err != nil {
		t.Fatalf("second ensureTranscodeSession: %v", err)
	}
	if second != first {
		t.Fatalf("second ensure returned a different session, want the live one (dedupe failed)")
	}
}
