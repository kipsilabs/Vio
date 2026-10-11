package playback

import (
	"encoding/json"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestAudioInventoryV3NeverNil pins the wire contract for the audio inventory:
// an effective source with no audio tracks must produce an empty, non-nil
// inventory so the field serializes as [] rather than null. A null audio_tracks
// is indistinguishable from "the source was never enumerated", so a verified
// inventory that emitted null left clients showing an empty menu forever.
func TestAudioInventoryV3NeverNil(t *testing.T) {
	cases := map[string]*models.MediaFile{
		"nil file":   nil,
		"empty file": {},
		"no tracks":  {AudioTracks: nil},
	}
	for name, file := range cases {
		t.Run(name, func(t *testing.T) {
			items := AudioInventoryV3(file)
			if items == nil {
				t.Fatal("AudioInventoryV3 returned nil; want an empty slice")
			}
			raw, err := json.Marshal(items)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			if string(raw) != "[]" {
				t.Fatalf("audio inventory JSON = %s, want []", raw)
			}
		})
	}
}

// TestPlaybackInventoryV3NeverSerializesNullAudio pins the endpoint contract:
// PlaybackInventoryV3.AudioTracks is tagged without omitempty, so an empty
// inventory must marshal to [] and never to null. GET /inventory returned the
// struct directly, so this is the field that served audio_tracks:null.
func TestPlaybackInventoryV3NeverSerializesNullAudio(t *testing.T) {
	inv := PlaybackInventoryV3{
		SessionID:         "sess",
		InventoryStatus:   "verified",
		AudioTracks:       AudioInventoryV3(nil),
		SubtitleInventory: []SubtitleInventoryItemV3{},
	}
	raw, err := json.Marshal(inv)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got := string(decoded["audio_tracks"]); got != "[]" {
		t.Fatalf("audio_tracks = %s, want []", got)
	}
}
