package playback

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

type inventoryRevisionEnvelopeV3 struct {
	Status                string                    `json:"status"`
	AudioTracks           []AudioInventoryItemV3    `json:"audio_tracks"`
	Subtitles             []SubtitleInventoryItemV3 `json:"subtitles"`
	EffectiveMediaFileID  int                       `json:"effective_media_file_id,omitempty"`
	EffectiveVirtualURI   string                    `json:"effective_virtual_uri,omitempty"`
	VirtualSourceRevision string                    `json:"virtual_source_revision,omitempty"`
}
type InventorySourceIdentityV3 struct {
	EffectiveMediaFileID  int
	EffectiveVirtualURI   string
	VirtualSourceRevision string
}

func ComputeInventoryRevisionV3(status string, audio []AudioInventoryItemV3, subs []SubtitleInventoryItemV3, source ...InventorySourceIdentityV3) string {
	var i InventorySourceIdentityV3
	if len(source) > 0 {
		i = source[0]
	}
	b, e := json.Marshal(inventoryRevisionEnvelopeV3{status, audio, subs, i.EffectiveMediaFileID, i.EffectiveVirtualURI, i.VirtualSourceRevision})
	if e != nil {
		h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d", status, len(audio), len(subs))))
		return fmt.Sprintf("inv:%x", h[:16])
	}
	h := sha256.Sum256(b)
	return fmt.Sprintf("inv:%x", h[:16])
}
