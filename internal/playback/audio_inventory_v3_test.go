package playback

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestAudioInventoryV3CarriesCanonicalSelectionOrdinal pins the plan's audio
// inventory contract: each entry publishes its 0-based selection ordinal and
// matching track id while the raw container stream index (the absolute ffprobe
// stream index) is preserved untouched. A client must never have to send the
// raw index as a selection.
func TestAudioInventoryV3CarriesCanonicalSelectionOrdinal(t *testing.T) {
	file := &models.MediaFile{ID: 77, AudioTracks: []models.AudioTrack{
		{Index: 1, Language: "eng", Codec: "eac3", Channels: 6, Default: true},
		{Index: 3, Language: "fra", Codec: "aac", Channels: 2},
	}}

	items := audioInventoryV3(file)

	if len(items) != 2 {
		t.Fatalf("items = %#v, want 2", items)
	}
	for i, item := range items {
		if item.SelectionIndex != i {
			t.Errorf("items[%d].SelectionIndex = %d, want the array position %d", i, item.SelectionIndex, i)
		}
		if want := TrackIDV3(file.ID, "audio", i); item.TrackID != want {
			t.Errorf("items[%d].TrackID = %q, want %q", i, item.TrackID, want)
		}
	}
	// The raw stream index is a different domain and must survive verbatim.
	if items[0].Index != 1 || items[1].Index != 3 {
		t.Fatalf("raw stream indexes = %d,%d, want 1,3", items[0].Index, items[1].Index)
	}
	if items[0].SelectionIndex == items[0].Index {
		t.Fatal("selection index must not be the raw container stream index")
	}

	// The wire form carries both domains, the raw index and the canonical
	// selection pair, so a client can pick the latter without ambiguity.
	encoded, err := json.Marshal(items[1])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(encoded)
	for _, want := range []string{`"index":3`, `"selection_index":1`, `"track_id":"file:77:audio:1"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("encoded audio item missing %q: %s", want, got)
		}
	}

	// A nil or track-less file publishes an empty inventory, never nil: the
	// wire field has no omitempty, so nil would serialize as null and read as
	// "not enumerated". An empty slice keeps verified meaning enumerated-empty.
	if got := audioInventoryV3(nil); got == nil || len(got) != 0 {
		t.Fatalf("audioInventoryV3(nil) = %#v, want an empty non-nil slice", got)
	}
	if got := audioInventoryV3(&models.MediaFile{ID: 1}); got == nil || len(got) != 0 {
		t.Fatalf("audioInventoryV3(no tracks) = %#v, want an empty non-nil slice", got)
	}
}
