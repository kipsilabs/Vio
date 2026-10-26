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
