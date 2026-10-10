package handlers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestColdPlanCarriesDeclaredLanguagesOnLanguageLessTracks pins the cold-plan
// audio inventory: a multi-stream release whose probe left every track without
// a language tag (HLS/DASH relays omit them) must carry the provider's declared
// languages so the existing selection policy can pick French, not fall back to
// the language-less index 0. The declaration labels the observed streams in
// place; it never fabricates a stream beyond what was observed.
func TestColdPlanCarriesDeclaredLanguagesOnLanguageLessTracks(t *testing.T) {
	probed := &models.MediaFile{
		Resolution: "1080p", CodecVideo: "h264",
		AudioTracks: []models.AudioTrack{
			{Index: 1, Codec: "eac3", Channels: 6},
			{Index: 2, Codec: "aac", Channels: 2},
		},
	}
	candidate := VirtualPlaybackStream{
		URI:            "virtual://movie/tt-cold-audio?result=one",
		CodecAudio:     "eac3",
		AudioLanguages: []string{"ENG", "FRA"},
	}

	mergeVirtualCandidateTracks(probed, candidate)

	if len(probed.AudioTracks) != 2 {
		t.Fatalf("audio tracks = %#v, want the 2 observed streams with no fabricated extras", probed.AudioTracks)
	}
	if got := probed.AudioTracks[0].Language; got != "ENG" {
		t.Fatalf("audio[0].language = %q, want ENG (declaration order)", got)
	}
	if got := probed.AudioTracks[1].Language; got != "FRA" {
		t.Fatalf("audio[1].language = %q, want FRA (declaration order)", got)
	}
	if got := playback.SelectAudioTrack(probed.AudioTracks, "fr", nil); got != 1 {
		t.Fatalf("French selection = %d, want 1 (the labeled second stream), not the language-less index 0", got)
	}
}

// TestColdPlanDoesNotSynthesizeLanguageLessDefaultTrack pins the removal of the
// anonymous fallback: when the declaration names no real language, the cold
// plan advertises an empty audio inventory and reports pending. A language-less
// Default:true track made a multi-audio release look like exactly one stream,
// so the plan could only ever select index 0 and the deferred probe's real
// inventory could not re-select it (inventory adoption never replans).
func TestColdPlanDoesNotSynthesizeLanguageLessDefaultTrack(t *testing.T) {
	probed := &models.MediaFile{Resolution: "1080p", CodecVideo: "h264"}
	candidate := VirtualPlaybackStream{
		URI:            "virtual://movie/tt-cold-noaudio?result=one",
		CodecAudio:     "aac",
		AudioLanguages: []string{"MULTI", "DUAL"},
	}

	mergeVirtualCandidateTracks(probed, candidate)

	if len(probed.AudioTracks) != 0 {
		t.Fatalf("audio tracks = %#v, want none (no language-less Default:true synthesis)", probed.AudioTracks)
	}
}

// TestColdPlanSingleDeclaredLanguageUnchanged pins that a single declared
// language still produces exactly one playable default track: the single-track
// case worked vacuously before and must keep working.
func TestColdPlanSingleDeclaredLanguageUnchanged(t *testing.T) {
	probed := &models.MediaFile{Resolution: "1080p", CodecVideo: "h264"}
	candidate := VirtualPlaybackStream{
		URI:            "virtual://movie/tt-cold-single?result=one",
		CodecAudio:     "eac3",
		AudioLanguages: []string{"FRA"},
	}

	mergeVirtualCandidateTracks(probed, candidate)

	if len(probed.AudioTracks) != 1 {
		t.Fatalf("audio tracks = %#v, want exactly one", probed.AudioTracks)
	}
	if !probed.AudioTracks[0].Default || probed.AudioTracks[0].Language != "FRA" {
		t.Fatalf("single declared track = %#v, want FRA marked default", probed.AudioTracks[0])
	}
	if got := playback.SelectAudioTrack(probed.AudioTracks, "fr", nil); got != 0 {
		t.Fatalf("single-track French selection = %d, want 0", got)
	}
}

// TestColdPlanFillsOnlyLanguageLessTracksWithoutDuplicating pins the fill
// policy: an observed language is never overwritten, and a declared language
// already present on another track is not duplicated onto an empty one. The
// track count stays equal to the observed stream count.
func TestColdPlanFillsOnlyLanguageLessTracksWithoutDuplicating(t *testing.T) {
	probed := &models.MediaFile{
		Resolution: "1080p", CodecVideo: "h264",
		AudioTracks: []models.AudioTrack{
			{Index: 1, Language: "en", Codec: "aac", Channels: 2},
			{Index: 2, Codec: "eac3", Channels: 6},
		},
	}
	candidate := VirtualPlaybackStream{
		URI:            "virtual://movie/tt-cold-fill?result=one",
		AudioLanguages: []string{"ENG", "FRA"},
	}

	mergeVirtualCandidateTracks(probed, candidate)

	if len(probed.AudioTracks) != 2 {
		t.Fatalf("audio tracks = %#v, want the 2 observed streams", probed.AudioTracks)
	}
	if got := probed.AudioTracks[0].Language; got != "en" {
		t.Fatalf("observed language overwritten: got %q, want en", got)
	}
	if got := probed.AudioTracks[1].Language; got != "FRA" {
		t.Fatalf("empty track not filled with the distinct declared language: got %q, want FRA", got)
	}
}

// TestColdPlanKeepsDeclaredSubtitleHintsOffEmbeddedInventory pins the
// registration invariant end to end: declared subtitle languages never become
// embedded subtitle tracks (Codec="", Index=0 maps to a phantom ffmpeg 0:s:N),
// the hint stays on the candidate, and an empty embedded inventory is what
// admits the background subtitle search the hint drives. Phantom rows made the
// row look subtitled and silently blocked that search.
func TestColdPlanKeepsDeclaredSubtitleHintsOffEmbeddedInventory(t *testing.T) {
	probed := &models.MediaFile{Resolution: "1080p", CodecVideo: "h264"}
	candidate := VirtualPlaybackStream{
		URI:               "virtual://movie/tt-cold-subs?result=one",
		SubtitleLanguages: []string{"eng", "fra"},
	}

	mergeVirtualCandidateTracks(probed, candidate)

	if len(probed.SubtitleTracks) != 0 {
		t.Fatalf("embedded subtitle tracks = %#v, want none (no phantom rows)", probed.SubtitleTracks)
	}
	if len(candidate.SubtitleLanguages) != 2 {
		t.Fatalf("declared subtitle hint = %#v, want it preserved on the candidate", candidate.SubtitleLanguages)
	}

	searched := make(chan []string, 1)
	h := &PlaybackHandler{
		SubtitleSearchInFlight: &sync.Map{},
		VirtualSubtitleSearcher: func(_ context.Context, _, _, _ string, _, _, _, _ int, subtitleLanguages []string) {
			searched <- subtitleLanguages
		},
	}
	h.maybeTriggerSubtitleSearch(context.Background(), probed, candidate)
	select {
	case got := <-searched:
		if len(got) != 2 || got[0] != "eng" || got[1] != "fra" {
			t.Fatalf("search received hint %#v, want [eng fra]", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subtitle search blocked; the declared hint never reached the searcher")
	}
}
