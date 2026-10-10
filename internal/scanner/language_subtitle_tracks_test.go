package scanner

import "testing"

// TestRegistrationSubtitleTracksDropsDeclaredLanguages pins the registration
// invariant: declared subtitle languages are provider hints, not observed
// streams, so a virtual registration or candidate listing persists zero
// embedded subtitle tracks. Persisting them would fabricate language-only rows
// (Codec="", Index=0) that map to a phantom ffmpeg 0:s:N and suppress the
// subtitle search the hint is meant to drive.
func TestRegistrationSubtitleTracksDropsDeclaredLanguages(t *testing.T) {
	tracks := registrationSubtitleTracks([]string{"EN-US", "ENG", "FRE", "FR-CA", "SPA", "ES-419"})
	if len(tracks) != 0 {
		t.Fatalf("registrationSubtitleTracks = %#v, want no embedded subtitle tracks", tracks)
	}
}

// TestRegistrationSubtitleTracksEmptyStaysNonNil pins the JSONB shape: the
// persisted inventory must marshal as [] rather than null.
func TestRegistrationSubtitleTracksEmptyStaysNonNil(t *testing.T) {
	tracks := registrationSubtitleTracks(nil)
	if tracks == nil || len(tracks) != 0 {
		t.Fatalf("registrationSubtitleTracks(nil) = %#v, want a non-nil empty slice", tracks)
	}
}
