package scanner

import "testing"

// TestLanguageSubtitleTracksDeduplicatesAliases proves the persisted virtual
// placeholder list keeps one row per base language even though the release
// name carried both a regional code and its bare base ("EN-US" and "ENG").
func TestLanguageSubtitleTracksDeduplicatesAliases(t *testing.T) {
	tracks := languageSubtitleTracks([]string{"EN-US", "ENG", "FRE", "FR-CA", "SPA", "ES-419"})
	if len(tracks) != 3 {
		t.Fatalf("languageSubtitleTracks = %#v, want 3 tracks (one per base language)", tracks)
	}
	got := map[string]bool{}
	for _, track := range tracks {
		got[track.Language] = true
	}
	for _, want := range []string{"EN-US", "FR-CA", "ES-419"} {
		if !got[want] {
			t.Fatalf("languageSubtitleTracks = %#v, missing %q", tracks, want)
		}
	}
	for _, dropped := range []string{"ENG", "FRE", "SPA"} {
		if got[dropped] {
			t.Fatalf("languageSubtitleTracks = %#v, kept redundant base code %q", tracks, dropped)
		}
	}
}

// TestLanguageSubtitleTracksEmptyStaysNonNil pins the JSONB shape: an empty
// language list must marshal as [] rather than null.
func TestLanguageSubtitleTracksEmptyStaysNonNil(t *testing.T) {
	tracks := languageSubtitleTracks(nil)
	if tracks == nil || len(tracks) != 0 {
		t.Fatalf("languageSubtitleTracks(nil) = %#v, want a non-nil empty slice", tracks)
	}
}
