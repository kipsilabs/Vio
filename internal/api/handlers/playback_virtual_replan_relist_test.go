package handlers

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestVirtualReplanNeedsFreshProviderListing pins the forced-relist policy for a
// session-bound replan: only a cause that indicts the source re-lists, and a
// user-intent replan (including a track change reported as a failure recovery)
// reuses the fresh resolved source.
func TestVirtualReplanNeedsFreshProviderListing(t *testing.T) {
	for _, tc := range []struct {
		name           string
		operation      playback.ReplanOperationV3
		classification string
		decodeRotation bool
		want           bool
	}{
		{name: "decode rotation always relists", operation: playback.ReplanOperationTrackChangeV3, decodeRotation: true, want: true},
		{name: "track change reuses", operation: playback.ReplanOperationTrackChangeV3, want: false},
		{name: "quality change reuses", operation: playback.ReplanOperationQualityChangeV3, want: false},
		{name: "output change reuses", operation: playback.ReplanOperationOutputChangeV3, want: false},
		{name: "seek reanchor reuses", operation: playback.ReplanOperationSeekReanchorV3, want: false},
		{name: "subtitle track change as a recovery reuses", operation: playback.ReplanOperationFailureRecoveryV3, classification: failureSubtitleTrackChangedV3, want: false},
		{name: "audio track change as a recovery reuses", operation: playback.ReplanOperationFailureRecoveryV3, classification: failureAudioTrackChangedV3, want: false},
		{name: "source failure recovery relists", operation: playback.ReplanOperationFailureRecoveryV3, classification: "decode_failed", want: true},
		{name: "seek failure recovery relists", operation: playback.ReplanOperationSeekFailureRecoveryV3, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := virtualReplanNeedsFreshProviderListingV3(tc.operation, tc.classification, tc.decodeRotation); got != tc.want {
				t.Fatalf("needs fresh listing = %v, want %v", got, tc.want)
			}
		})
	}
}
