package handlers

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
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

	if got := remapFallbackAudioIndexV3(pinned, replacement, 1); got != 0 {
		t.Fatalf("remapped index = %d, want 0 (the English track on the replacement)", got)
	}
	if got := remapFallbackAudioIndexV3(pinned, nil, 1); got != -1 {
		t.Fatalf("remapped index without probed facts = %d, want -1 (container default)", got)
	}
	if got := remapFallbackAudioIndexV3(pinned, replacement, -1); got != -1 {
		t.Fatalf("remapped index for no selection = %d, want -1", got)
	}
}

// A successful cross-release carry names both sides of the remap so a
// same-language wrong-track carry is visible in the log instead of silent.
func TestCrossReleaseFallbackLogsRemappedAudioCarry(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	pinned := &models.MediaFile{ID: 42, AudioTracks: []models.AudioTrack{
		{Index: 0, Language: "ja", Codec: "aac", Channels: 2},
		{Index: 1, Language: "en", Codec: "aac", Channels: 2},
	}}
	replacement := &models.MediaFile{ID: 84, AudioTracks: []models.AudioTrack{
		{Index: 0, Language: "en", Codec: "eac3", Channels: 6},
		{Index: 1, Language: "ja", Codec: "eac3", Channels: 6},
	}}

	logCrossReleaseAudioCarry(context.Background(), pinned, replacement, 1, 0, "candidate-1")

	out := logs.String()
	for _, want := range []string{
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
}
