package catalog

import "testing"

// TestRegistrationSubtitleLanguagesPersistNoEmbeddedTracks pins the
// registration invariant: declared subtitle languages are provider hints, not
// observed streams. Registration must persist none of them as embedded
// subtitle tracks; a language-only row (Codec="", Index=0) maps to a phantom
// ffmpeg 0:s:N and suppresses the subtitle search the hint is meant to drive.
func TestRegistrationSubtitleLanguagesPersistNoEmbeddedTracks(t *testing.T) {
	if got := registrationSubtitleLanguages([]string{"ENG", "FRA", "MULTI", "DUAL"}); len(got) != 0 {
		t.Fatalf("registrationSubtitleLanguages persisted %#v, want no embedded subtitle rows", got)
	}
	if got := registrationSubtitleLanguages(nil); len(got) != 0 {
		t.Fatalf("registrationSubtitleLanguages(nil) persisted %#v, want no embedded subtitle rows", got)
	}
}
