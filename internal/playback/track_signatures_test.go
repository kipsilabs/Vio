package playback

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestRemapAudioSelectionIndexReidentifiesCommittedTrack pins the overlay-swap
// remap: the selected track's array position may move when the verified list
// reorders, so the remap must return the position that names the same stream.
func TestRemapAudioSelectionIndexReidentifiesCommittedTrack(t *testing.T) {
	plan := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
	}
	// pt's absolute container index is stable across the reorder, so it is
	// re-identified at its new array position.
	verified := []models.AudioTrack{
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
	}
	if got := remapAudioSelectionIndex(plan, 1, verified); got != 0 {
		t.Fatalf("remapped position = %d, want 0 (the reordered pt track)", got)
	}
}

// TestRemapAudioSelectionIndexRemovalReturnsInvalid pins the removed case: a
// committed stream that is absent from the verified inventory has no position to
// return, and the caller must see -1 rather than the old position naming another
// track.
func TestRemapAudioSelectionIndexRemovalReturnsInvalid(t *testing.T) {
	plan := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
	}
	verified := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
	}
	if got := remapAudioSelectionIndex(plan, 1, verified); got != -1 {
		t.Fatalf("remapped position = %d, want -1 for a removed committed track", got)
	}
}

// TestRemapAudioSelectionIndexWeakFallbackIsUniqueAndTransportMatched pins the
// language/codec fallback: it must not substitute a distinct track of the same
// language. A unique transport-fact match is accepted; an ambiguous set (two
// tracks sharing language, codec, channels, and layout) is invalid, and a track
// whose layout differs is never a substitute.
func TestRemapAudioSelectionIndexWeakFallbackIsUniqueAndTransportMatched(t *testing.T) {
	// The committed track's title differs from every verified track, so the
	// exact-signature match misses and the weak fallback decides.
	committed := models.AudioTrack{Index: 0, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo", Title: "Director"}

	t.Run("unique transport match", func(t *testing.T) {
		verified := []models.AudioTrack{
			{Index: 0, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo", Title: "Main"},
		}
		if got := remapAudioSelectionIndex([]models.AudioTrack{committed}, 0, verified); got != 0 {
			t.Fatalf("remapped position = %d, want the unique transport-matched track 0", got)
		}
	})

	t.Run("ambiguous same-language tracks are invalid", func(t *testing.T) {
		// Two tracks indistinguishable by language, codec, channels, and
		// layout: picking either could mux the wrong stream, so the identity is
		// invalid rather than a positional guess.
		verified := []models.AudioTrack{
			{Index: 0, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo", Title: "Commentary"},
			{Index: 0, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo", Title: "Descriptive"},
		}
		if got := remapAudioSelectionIndex([]models.AudioTrack{committed}, 0, verified); got != -1 {
			t.Fatalf("remapped position = %d, want -1 for an ambiguous same-language set", got)
		}
	})

	t.Run("different layout is not a substitute", func(t *testing.T) {
		verified := []models.AudioTrack{
			{Index: 0, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo", Title: "Main"},
		}
		committed51 := committed
		committed51.Layout = "5.1"
		committed51.Channels = 6
		if got := remapAudioSelectionIndex([]models.AudioTrack{committed51}, 0, verified); got != -1 {
			t.Fatalf("remapped position = %d, want -1; a different layout is a different stream", got)
		}
	})
}
