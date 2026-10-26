package handlers

import (
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
