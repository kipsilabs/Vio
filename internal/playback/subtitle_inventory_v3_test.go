package playback

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/subtitles"
)

func TestBuildSubtitleInventoryV3_OrdinalsAreDenseAcrossAllThreeRanges(t *testing.T) {
	file := &models.MediaFile{
		ID: 42,
		ExternalSubtitles: []models.ExternalSubtitle{
			{Path: "/media/movie.en.srt", Language: "en", Format: "srt"},
			{Path: "/media/movie.fr.srt", Language: "fr", Format: "srt"},
		},
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Language: "en", Codec: "subrip"},
			{Index: 1, Language: "ja", Codec: "hdmv_pgs_subtitle"},
			// A bitmap track with no sidecar shape. It is the reason this test
			// exists: it must still occupy its ordinal.
			{Index: 2, Language: "de", Codec: "dvd_subtitle"},
		},
	}
	additional := []SubtitleInventoryEntryV3{
		{CombinedIndex: 5, Codec: "srt", Source: SubtitleSourceDownloadedV3, Language: "es"},
	}

	items := BuildSubtitleInventoryV3(file, additional)

	if len(items) != 6 {
		t.Fatalf("expected 6 inventory items (2 external + 3 embedded + 1 downloaded), got %d: %+v", len(items), items)
	}
	for i, item := range items {
		if item.CombinedIndex != i {
			t.Errorf("item %d has combined_index %d; the ordinal space must be dense", i, item.CombinedIndex)
		}
		if want := TrackIDV3(file.ID, "subtitle", i); item.TrackID != want {
			t.Errorf("item %d track_id = %q, want %q", i, item.TrackID, want)
		}
	}

	wantSources := []string{
		SubtitleSourceExternalV3, SubtitleSourceExternalV3,
		SubtitleSourceEmbeddedV3, SubtitleSourceEmbeddedV3, SubtitleSourceEmbeddedV3,
		SubtitleSourceDownloadedV3,
	}
	for i, want := range wantSources {
		if items[i].Source != want {
			t.Errorf("item %d source = %q, want %q", i, items[i].Source, want)
		}
	}

	// The downloaded track's ordinal must follow every embedded track,
	// including the burn-in-only one.
	if got := items[5]; got.CombinedIndex != 5 || got.Language != "es" {
		t.Errorf("downloaded track landed at the wrong ordinal: %+v", got)
	}
}

func TestBuildSubtitleInventoryV3_ClassifiesDelivery(t *testing.T) {
	file := &models.MediaFile{
		ID: 7,
		ExternalSubtitles: []models.ExternalSubtitle{
			{Path: "/media/movie.en.srt", Language: "en", Format: "srt"},
			// An external bitmap file has no sidecar route either.
			{Path: "/media/movie.en.sup", Language: "en", Format: "pgs"},
		},
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Codec: "ass"},
			{Index: 1, Codec: "hdmv_pgs_subtitle"},
			{Index: 2, Codec: "dvd_subtitle"},
			{Index: 3, Codec: "dvb_subtitle"},
		},
	}

	items := BuildSubtitleInventoryV3(file, nil)

	want := []string{
		SubtitleDeliverySidecarV3,    // external srt
		SubtitleDeliveryBurnInOnlyV3, // external pgs: no extraction route
		SubtitleDeliverySidecarV3,    // embedded ass
		SubtitleDeliverySidecarV3,    // embedded pgs extracts to .sup
		SubtitleDeliveryBurnInOnlyV3, // embedded dvd
		SubtitleDeliveryBurnInOnlyV3, // embedded dvb
	}
	if len(items) != len(want) {
		t.Fatalf("expected %d items, got %d", len(want), len(items))
	}
	for i, expected := range want {
		if items[i].Delivery != expected {
			t.Errorf("item %d (%s/%s) delivery = %q, want %q", i, items[i].Source, items[i].Codec, items[i].Delivery, expected)
		}
	}
}

func TestSubtitleInventoryV3_AttachesURLsOnlyToSidecarTracks(t *testing.T) {
	file := &models.MediaFile{
		ID: 44,
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Codec: "subrip"},
			{Index: 1, Codec: "ass"},
			{Index: 2, Codec: "hdmv_pgs_subtitle"},
			{Index: 3, Codec: "dvd_subtitle"},
		},
	}

	items := SubtitleInventoryV3("sess-1", file, nil)

	cases := []struct {
		index         int
		url           string
		fontBundleURL string
	}{
		{0, "/stream/sess-1/subtitles/0.vtt?file_id=44&embedded_stream_index=0", ""},
		{1, "/stream/sess-1/subtitles/1.ass?file_id=44&embedded_stream_index=1", "/stream/sess-1/subtitles/1/fonts?file_id=44&embedded_stream_index=1"},
		{2, "/stream/sess-1/subtitles/2.sup?file_id=44&embedded_stream_index=2", ""},
		{3, "", ""},
	}
	for _, tc := range cases {
		got := items[tc.index]
		if got.URL != tc.url {
			t.Errorf("item %d url = %q, want %q", tc.index, got.URL, tc.url)
		}
		if got.FontBundleURL != tc.fontBundleURL {
			t.Errorf("item %d font_bundle_url = %q, want %q", tc.index, got.FontBundleURL, tc.fontBundleURL)
		}
	}
}

func TestScopeSubtitleInventoryV3PreservesPinnedIdentity(t *testing.T) {
	file := &models.MediaFile{ID: 42,
		ExternalSubtitles: []models.ExternalSubtitle{{Path: "/media/selected.srt", Format: "srt"}},
		SubtitleTracks:    []models.SubtitleTrack{{Index: 4, Codec: "ass"}},
	}
	inventory := SubtitleInventoryV3("original-session", file, nil)
	wantKey := ExternalSubtitlePathKeyV3(file.ExternalSubtitles[0].Path)
	if !strings.Contains(inventory[0].URL, "external_subtitle_key="+wantKey) || strings.Contains(inventory[0].URL, "selected.srt") {
		t.Fatalf("external URL must carry an opaque path identity: %q", inventory[0].URL)
	}
	// Restoring a frozen inventory must not rebind its pins after a scan.
	file.ExternalSubtitles = append([]models.ExternalSubtitle{{Path: "/media/new.srt", Format: "srt"}}, file.ExternalSubtitles...)
	scoped := ScopeSubtitleInventoryV3("new-session", file, inventory, nil)
	if !strings.Contains(scoped[0].URL, "external_subtitle_key="+wantKey) ||
		!strings.Contains(scoped[1].URL, "embedded_stream_index=4") ||
		!strings.Contains(scoped[1].FontBundleURL, "embedded_stream_index=4") {
		t.Fatalf("rescoping changed subtitle identities: %#v", scoped)
	}
}

func TestSubtitleInventoryV3_OmitsURLsWithoutASession(t *testing.T) {
	file := &models.MediaFile{ID: 3, SubtitleTracks: []models.SubtitleTrack{{Index: 0, Codec: "subrip"}}}

	items := SubtitleInventoryV3("", file, nil)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].URL != "" {
		t.Errorf("expected no URL without a session, got %q", items[0].URL)
	}
}

func TestScopeSubtitleInventoryV3_EncodesEmptyInventoryAsArray(t *testing.T) {
	items := ScopeSubtitleInventoryV3("sess-empty", &models.MediaFile{ID: 4}, []SubtitleInventoryItemV3{}, nil)
	if items == nil {
		t.Fatal("expected a non-nil empty inventory")
	}

	payload, err := json.Marshal(SubtitleDecisionV3{Mode: SubtitleOffV3, Inventory: items})
	if err != nil {
		t.Fatalf("marshal subtitle decision: %v", err)
	}
	if got := string(payload); !strings.Contains(got, `"inventory":[]`) {
		t.Fatalf("empty inventory encoded as %s, want inventory:[]", got)
	}
}

// TestBuildSubtitleInventoryV3_DeduplicatesIdenticalEmbeddedTracks is the
// regression for the duplicate subtitle list: a file carrying both a regional
// code and its bare base (the placeholder pair "EN-US" and "ENG") publishes one
// track, and the published ordinals stay dense even though a source ordinal was
// suppressed.
func TestBuildSubtitleInventoryV3_DeduplicatesIdenticalEmbeddedTracks(t *testing.T) {
	file := &models.MediaFile{
		ID: 50,
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Language: "EN-US", Codec: "subrip"},
			{Index: 0, Language: "ENG", Codec: "subrip"},
			{Index: 2, Language: "FR-CA", Codec: "subrip"},
			{Index: 2, Language: "FRE", Codec: "subrip"},
		},
	}

	items := BuildSubtitleInventoryV3(file, nil)

	if len(items) != 2 {
		t.Fatalf("items = %#v, want 2 (one per base language)", items)
	}
	if items[0].CombinedIndex != 0 || items[0].TrackID != TrackIDV3(file.ID, "subtitle", 0) || items[0].Language != "EN-US" {
		t.Fatalf("items[0] = %#v, want EN-US at published ordinal 0", items[0])
	}
	if items[1].CombinedIndex != 1 || items[1].TrackID != TrackIDV3(file.ID, "subtitle", 1) || items[1].Language != "FR-CA" {
		t.Fatalf("items[1] = %#v, want FR-CA renumbered to published ordinal 1", items[1])
	}
	if _, ok := SubtitleInventoryItemAtV3(items, 1); !ok {
		t.Fatal("the renumbered surviving track must resolve at its published ordinal")
	}
	if _, ok := SubtitleInventoryItemAtV3(items, 2); ok {
		t.Fatal("a suppressed duplicate must not leave a published ordinal")
	}
	// The suppression did not erase the underlying track: the served identity
	// still points at the FR-CA source track (offset 2).
	if source, ok := SubtitleInventoryOwnSourceIndexV3(file, 1); !ok || source != 2 {
		t.Fatalf("published ordinal 1 source index = (%d, %v), want the FR-CA source ordinal 2", source, ok)
	}
}

// TestBuildSubtitleInventoryV3_RenumbersDeduplicatedGapToDense is the Android
// TV regression: nine embedded ordinals where 1..7 describe track 0 a second
// time used to publish its survivors at 0 and 8, leaving the client-visible
// list with a gap. The fixture lists the same container stream twice per
// entry (same Index twice would be one track, so the second spelling repeats
// the first entry's stream index): the published list is now dense and the
// source mapping survives underneath.
func TestBuildSubtitleInventoryV3_RenumbersDeduplicatedGapToDense(t *testing.T) {
	tracks := make([]models.SubtitleTrack, 9)
	// Seven distinct English streams plus one repeated description of stream
	// 0 (the bare-base retelling of the regional spelling): 8 physical
	// tracks, 7 published English entries after the retelling collapses.
	for i := range 7 {
		tracks[i] = models.SubtitleTrack{Index: i, Language: "en", Codec: "subrip"}
	}
	tracks[7] = models.SubtitleTrack{Index: 0, Language: "ENG", Codec: "subrip"}
	tracks[8] = models.SubtitleTrack{Index: 12, Language: "fr", Codec: "subrip"}
	file := &models.MediaFile{ID: 60, SubtitleTracks: tracks}

	items := BuildSubtitleInventoryV3(file, nil)

	if len(items) != 8 {
		t.Fatalf("items = %#v, want 8 survivors (7 English + French)", items)
	}
	for i, item := range items {
		if item.CombinedIndex != i {
			t.Errorf("item %d combined_index = %d; the published space must be dense", i, item.CombinedIndex)
		}
		if want := TrackIDV3(file.ID, "subtitle", i); item.TrackID != want {
			t.Errorf("item %d track_id = %q, want %q", i, item.TrackID, want)
		}
	}
	if items[7].Language != "fr" {
		t.Fatalf("items[7] = %#v, want the surviving French track", items[7])
	}
	// The French track occupied source ordinal 8; only its published ordinal is
	// renumbered.
	if source, ok := SubtitleInventoryOwnSourceIndexV3(file, 7); !ok || source != 8 {
		t.Fatalf("published ordinal 7 source index = (%d, %v), want 8", source, ok)
	}
}

// TestSubtitleEntryAtCombinedIndexV3_ResolvesLastPublishedOrdinal is the
// selection-path half of the regression: the ordinal a client echoes from the
// dense inventory must resolve to the track that ordinal now names, not to the
// source-space slot that position used to hold.
func TestSubtitleEntryAtCombinedIndexV3_ResolvesLastPublishedOrdinal(t *testing.T) {
	tracks := make([]models.SubtitleTrack, 9)
	for i := range 7 {
		tracks[i] = models.SubtitleTrack{Index: i, Language: "en", Codec: "subrip"}
	}
	tracks[7] = models.SubtitleTrack{Index: 0, Language: "ENG", Codec: "subrip"}
	tracks[8] = models.SubtitleTrack{Index: 12, Language: "fr", Codec: "ass"}
	file := &models.MediaFile{ID: 61, SubtitleTracks: tracks}

	entry, ok := subtitleEntryAtCombinedIndexV3(file, 7, nil)
	if !ok {
		t.Fatal("the last published ordinal must resolve")
	}
	if entry.Source != SubtitleSourceEmbeddedV3 || entry.Codec != "ass" {
		t.Fatalf("published ordinal 7 resolved to %#v; want the embedded ASS track that ordinal now names", entry)
	}
	if entry.CombinedIndex != 8 {
		t.Fatalf("resolved entry source ordinal = %d, want 8", entry.CombinedIndex)
	}
	if _, ok := subtitleEntryAtCombinedIndexV3(file, 8, nil); ok {
		t.Fatal("an ordinal past the dense inventory must not resolve")
	}
}

// TestResolveSubtitlePolicyV3ResolvesRenumberedPublishedOrdinal closes the loop
// at the planner boundary: the client echoes the last published ordinal and the
// policy must report the published index while driving the embedded transport
// index from the source track it names.
func TestResolveSubtitlePolicyV3ResolvesRenumberedPublishedOrdinal(t *testing.T) {
	file := detailedFixtureFileV3()
	tracks := make([]models.SubtitleTrack, 9)
	for i := range 7 {
		tracks[i] = models.SubtitleTrack{Index: i, Language: "en", Codec: "subrip"}
	}
	tracks[7] = models.SubtitleTrack{Index: 0, Language: "ENG", Codec: "subrip"}
	tracks[8] = models.SubtitleTrack{Index: 12, Language: "fr", Codec: "subrip"}
	file.ExternalSubtitles = nil
	file.SubtitleTracks = tracks

	req := validStartRequestV3()
	index := 7
	req.SubtitleTrackIndex = &index
	req.SubtitleTrackID = TrackIDV3(file.ID, "subtitle", index)

	result := ResolveSubtitlePolicyV3(file, req, true, DeliveryClassOriginalHTTPV3, nil)
	if result.Terminal != nil {
		t.Fatalf("terminal = %#v, want the renumbered selection resolved", result.Terminal)
	}
	if result.SelectedIndex != 7 || result.TransportIndex != 8 || result.Source != SubtitleSourceEmbeddedV3 || result.Codec != "subrip" {
		t.Fatalf("result = %#v, want published ordinal 7 mapped to embedded transport 8", result)
	}
}

// TestScopeSubtitleInventoryV3_PinsSourceIdentityAfterDedup proves the
// published URL a client fetches stays resolvable after renumbering: the path
// carries the dense published ordinal while the pin still names the source
// track's container index, which is what the stream route resolves.
func TestScopeSubtitleInventoryV3_PinsSourceIdentityAfterDedup(t *testing.T) {
	tracks := make([]models.SubtitleTrack, 9)
	for i := range 7 {
		tracks[i] = models.SubtitleTrack{Index: i, Language: "en", Codec: "subrip"}
	}
	tracks[7] = models.SubtitleTrack{Index: 0, Language: "ENG", Codec: "subrip"}
	tracks[8] = models.SubtitleTrack{Index: 12, Language: "fr", Codec: "ass"}
	file := &models.MediaFile{ID: 62, SubtitleTracks: tracks}

	items := SubtitleInventoryV3("sess-dedup", file, nil)
	if len(items) != 8 {
		t.Fatalf("items = %#v, want 8 survivors (7 English + French)", items)
	}
	// The surviving French ASS track is published at ordinal 7 but its URL pins
	// the live container stream index 12, not the renumbered ordinal.
	if got, want := items[7].URL, "/stream/sess-dedup/subtitles/7.ass?file_id=62&embedded_stream_index=12"; got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
	if got, want := items[7].FontBundleURL, "/stream/sess-dedup/subtitles/7/fonts?file_id=62&embedded_stream_index=12"; got != want {
		t.Errorf("font_bundle_url = %q, want %q", got, want)
	}
}

// TestBuildSubtitleInventoryV3_DeduplicatesAcrossRanges proves the seen-set
// spans the concatenated ranges: a downloaded entry that repeats the same
// downloaded row (same stable row ID) collapses, while a different row in the
// same language is kept. (A bare cross-range alias cannot name the sidecar's
// track — it carries no path — so it publishes beside it; see
// TestBuildSubtitleInventoryV3_KeepsDownloadedBesideEmbeddedSameLanguage.)
func TestBuildSubtitleInventoryV3_DeduplicatesAcrossRanges(t *testing.T) {
	file := &models.MediaFile{
		ID:                51,
		ExternalSubtitles: []models.ExternalSubtitle{{Path: "/media/movie.en.srt", Language: "en", Format: "srt"}},
	}
	additional := []SubtitleInventoryEntryV3{
		{CombinedIndex: 1, Codec: "srt", Source: SubtitleSourceDownloadedV3, Language: "ENG", Label: "English", DownloadedSubtitleID: 77},
		{CombinedIndex: 2, Codec: "srt", Source: SubtitleSourceDownloadedV3, Language: "ENG", Label: "English", DownloadedSubtitleID: 77},
	}

	items := BuildSubtitleInventoryV3(file, additional)

	if len(items) != 2 {
		t.Fatalf("items = %#v, want the sidecar plus one collapsed download", items)
	}
	if items[0].Source != SubtitleSourceExternalV3 || items[0].CombinedIndex != 0 {
		t.Fatalf("items[0] = %#v, want the external sidecar at ordinal 0", items[0])
	}
}

// TestBuildSubtitleInventoryV3_KeepsDistinctLanguageCodecForcedCombinations
// proves de-duplication is keyed on the whole combination, not the language
// alone.
func TestBuildSubtitleInventoryV3_KeepsDistinctLanguageCodecForcedCombinations(t *testing.T) {
	file := &models.MediaFile{
		ID: 52,
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Language: "en", Codec: "subrip"},
			{Index: 1, Language: "en", Codec: "subrip", Forced: true},
			{Index: 2, Language: "en", Codec: "ass"},
			{Index: 3, Language: "en", Codec: "subrip", HearingImpaired: true},
			{Index: 4, Language: "de", Codec: "subrip"},
		},
	}

	items := BuildSubtitleInventoryV3(file, nil)

	if len(items) != 5 {
		t.Fatalf("items = %#v, want every distinct language/codec/forced combination", items)
	}
}

// TestBuildSubtitleInventoryV3_KeepsDownloadedBesideEmbeddedSameLanguage
// proves a downloaded/AI track is not silently dropped because the file has an
// embedded track in the same language.
func TestBuildSubtitleInventoryV3_KeepsDownloadedBesideEmbeddedSameLanguage(t *testing.T) {
	file := &models.MediaFile{
		ID:             53,
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "srt"}},
	}
	additional := []SubtitleInventoryEntryV3{
		{CombinedIndex: 1, Codec: "srt", Source: SubtitleSourceDownloadedV3, Language: "en", DownloadedSubtitleID: 77},
	}

	items := BuildSubtitleInventoryV3(file, additional)

	if len(items) != 2 {
		t.Fatalf("items = %#v, want both the embedded and the downloaded track", items)
	}
}

// TestBuildSubtitleInventoryV3_KeepsDistinctDownloadedRows proves two distinct
// downloaded rows in one language are preserved; only a genuinely repeated row
// is collapsed.
func TestBuildSubtitleInventoryV3_KeepsDistinctDownloadedRows(t *testing.T) {
	file := &models.MediaFile{ID: 54}
	additional := []SubtitleInventoryEntryV3{
		{Codec: "srt", Source: SubtitleSourceDownloadedV3, Language: "en", DownloadedSubtitleID: 77},
		{Codec: "srt", Source: SubtitleSourceDownloadedV3, Language: "ENG", DownloadedSubtitleID: 88},
	}

	items := BuildSubtitleInventoryV3(file, additional)

	if len(items) != 2 {
		t.Fatalf("items = %#v, want both downloaded rows kept", items)
	}
	repeated := []SubtitleInventoryEntryV3{
		{Codec: "srt", Source: SubtitleSourceDownloadedV3, Language: "en", DownloadedSubtitleID: 77},
		{Codec: "srt", Source: SubtitleSourceDownloadedV3, Language: "ENG", DownloadedSubtitleID: 77},
	}
	if deduped := BuildSubtitleInventoryV3(file, repeated); len(deduped) != 1 {
		t.Fatalf("deduped = %#v, want the repeated row collapsed", deduped)
	}
}

// TestBuildSubtitleInventoryV3_KeepsUntitledSameLanguageTracks is the #98
// regression: two untitled embedded tracks that share a base language but
// live on different container stream indexes are distinct physical streams
// (zh-Hans beside zh-Hant, or two same-language rips) and must both publish.
func TestBuildSubtitleInventoryV3_KeepsUntitledSameLanguageTracks(t *testing.T) {
	file := &models.MediaFile{
		ID: 66,
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 4, Language: "zh-Hans", Codec: "subrip"},
			{Index: 5, Language: "zh-Hant", Codec: "subrip"},
			{Index: 6, Language: "eng", Codec: "subrip"},
			{Index: 7, Language: "eng", Codec: "subrip"},
		},
	}

	items := BuildSubtitleInventoryV3(file, nil)

	if len(items) != 4 {
		t.Fatalf("items = %#v, want all four untitled tracks", items)
	}
}

// TestBuildSubtitleInventoryV3_KeepsSameTitledTracksOnDifferentIndexes is the
// oracle follow-up: two streams at different container indexes that share a
// title, codec, language, and flags are different physical tracks and must
// both publish — the index is part of the identity, never shadowed by it.
func TestBuildSubtitleInventoryV3_KeepsSameTitledTracksOnDifferentIndexes(t *testing.T) {
	file := &models.MediaFile{
		ID: 68,
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 4, Language: "eng", Codec: "subrip", EmbeddedTitle: "English"},
			{Index: 5, Language: "eng", Codec: "subrip", EmbeddedTitle: "English"},
		},
	}

	items := BuildSubtitleInventoryV3(file, nil)

	if len(items) != 2 {
		t.Fatalf("items = %#v, want both same-titled tracks on different indexes", items)
	}
}

// TestBuildSubtitleInventoryV3_KeepsSameLanguageSidecars is the #98 sidecar
// half: two sidecars in one language with different stable paths are distinct
// physical tracks even when their codec and flags coincide.
func TestBuildSubtitleInventoryV3_KeepsSameLanguageSidecars(t *testing.T) {
	file := &models.MediaFile{
		ID: 67,
		ExternalSubtitles: []models.ExternalSubtitle{
			{Path: "/media/movie.en.srt", Language: "en", Format: "srt"},
			{Path: "/media/movie.en.forced.srt", Language: "en", Format: "srt", Forced: true},
			{Path: "/media/movie.eng.srt", Language: "ENG", Format: "srt"},
		},
	}

	items := BuildSubtitleInventoryV3(file, nil)

	if len(items) != 3 {
		t.Fatalf("items = %#v, want all three same-language sidecars", items)
	}
}

// TestBuildSubtitleInventoryV3_KeepsUnknownLanguageEntriesWithSameCodec is the
// regression for the over-eager de-duplication: several tracks whose language
// is unknown and which share a codec must all survive, because they carry no
// positive identity proving they are the same track.
func TestBuildSubtitleInventoryV3_KeepsUnknownLanguageEntriesWithSameCodec(t *testing.T) {
	file := &models.MediaFile{
		ID: 55,
		ExternalSubtitles: []models.ExternalSubtitle{
			{Format: "srt"},
			{Format: "srt"},
			{Format: "srt"},
		},
		SubtitleTracks: []models.SubtitleTrack{{Index: 4, Codec: "hdmv_pgs_subtitle"}},
	}

	items := BuildSubtitleInventoryV3(file, nil)

	if len(items) != 4 {
		t.Fatalf("items = %#v, want all four unknown-language entries kept", items)
	}
	for i, item := range items {
		if item.CombinedIndex != i {
			t.Fatalf("items[%d].CombinedIndex = %d, want %d", i, item.CombinedIndex, i)
		}
	}
}

// TestBuildSubtitleInventoryV3_KeepsRegionalVariantsOfOneLanguage is the live
// regression: five embedded tracks that all canonicalize to base language "fr"
// (fr-FR forced, fr-FR, fr-CA forced, fr-CA, and an SDH track) must all
// publish. Before the fix the two fr-FR tracks and the two fr-CA tracks
// collapsed to one each, so French-Canadien was missing from the client's list.
func TestBuildSubtitleInventoryV3_KeepsRegionalVariantsOfOneLanguage(t *testing.T) {
	file := &models.MediaFile{
		ID: 63,
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 4, Language: "fr-FR", Codec: "subrip", Forced: true, EmbeddedTitle: "French (France) Forced"},
			{Index: 5, Language: "fr-FR", Codec: "subrip", EmbeddedTitle: "French (France)"},
			{Index: 6, Language: "fr-CA", Codec: "subrip", Forced: true, EmbeddedTitle: "French (Canada) Forced"},
			{Index: 7, Language: "fr-CA", Codec: "subrip", EmbeddedTitle: "French (Canada)"},
			{Index: 8, Language: "en", Codec: "subrip", HearingImpaired: true, EmbeddedTitle: "English (SDH)"},
		},
	}

	items := BuildSubtitleInventoryV3(file, nil)

	if len(items) != 5 {
		t.Fatalf("items = %#v, want all five embedded tracks", items)
	}
	wantLangs := []string{"fr-FR", "fr-FR", "fr-CA", "fr-CA", "en"}
	wantForced := []bool{true, false, true, false, false}
	wantHI := []bool{false, false, false, false, true}
	for i, item := range items {
		if item.CombinedIndex != i || item.TrackID != TrackIDV3(file.ID, "subtitle", i) {
			t.Errorf("item %d identity = (%d, %q), want dense ordinal %d", i, item.CombinedIndex, item.TrackID, i)
		}
		if item.Language != wantLangs[i] || item.Forced != wantForced[i] || item.HearingImpaired != wantHI[i] {
			t.Errorf("item %d = (%s forced=%v hi=%v), want (%s forced=%v hi=%v)",
				i, item.Language, item.Forced, item.HearingImpaired, wantLangs[i], wantForced[i], wantHI[i])
		}
		if source, ok := SubtitleInventoryOwnSourceIndexV3(file, i); !ok || source != i {
			t.Errorf("published ordinal %d source index = (%d, %v), want %d", i, source, ok, i)
		}
	}
}

// TestBuildSubtitleInventoryV3_CollapsesRepeatedEmbeddedTitle proves a genuine
// duplicate still collapses: the same stream described twice under two
// language spellings is one track, while a different title in the same base
// language is kept.
func TestBuildSubtitleInventoryV3_CollapsesRepeatedEmbeddedTitle(t *testing.T) {
	file := &models.MediaFile{
		ID: 64,
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 10, Language: "fr", Codec: "subrip", EmbeddedTitle: "French"},
			{Index: 10, Language: "fra", Codec: "subrip", EmbeddedTitle: "French"},
			{Index: 12, Language: "fr", Codec: "subrip", EmbeddedTitle: "French (Canada)"},
		},
	}

	items := BuildSubtitleInventoryV3(file, nil)

	if len(items) != 2 {
		t.Fatalf("items = %#v, want the repeated title collapsed and the distinct title kept", items)
	}
	if items[1].CombinedIndex != 1 {
		t.Fatalf("items[1] = %#v, want the distinct French track at published ordinal 1", items[1])
	}
}

// TestBuildSubtitleInventoryV3_EmbeddedTitleDistinguishesSameLanguage proves
// the authored title is the discriminator that keeps distinct same-language
// tracks apart even when the probe recorded no container stream index.
func TestBuildSubtitleInventoryV3_EmbeddedTitleDistinguishesSameLanguage(t *testing.T) {
	file := &models.MediaFile{
		ID: 65,
		SubtitleTracks: []models.SubtitleTrack{
			{Language: "fr", Codec: "subrip", EmbeddedTitle: "French (France)"},
			{Language: "fr", Codec: "subrip", EmbeddedTitle: "French (Canada)"},
		},
	}

	items := BuildSubtitleInventoryV3(file, nil)

	if len(items) != 2 {
		t.Fatalf("items = %#v, want both titled tracks kept", items)
	}
}

func TestSubtitleInventoryItemAtV3(t *testing.T) {
	file := &models.MediaFile{
		ID: 9,
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Codec: "subrip", Language: "en"},
			{Index: 1, Codec: "dvd_subtitle", Language: "de"},
		},
	}
	items := BuildSubtitleInventoryV3(file, nil)

	if item, ok := SubtitleInventoryItemAtV3(items, 1); !ok || item.Language != "de" {
		t.Errorf("SubtitleInventoryItemAtV3(1) = (%+v, %v), want the German bitmap track", item, ok)
	}
	if _, ok := SubtitleInventoryItemAtV3(items, 2); ok {
		t.Error("SubtitleInventoryItemAtV3 must not resolve an ordinal past the end of the inventory")
	}
	if _, ok := SubtitleInventoryItemAtV3(items, -1); ok {
		t.Error("SubtitleInventoryItemAtV3 must not resolve a negative ordinal")
	}
}

func TestBuildSubtitleInventoryV3_NilFile(t *testing.T) {
	if items := BuildSubtitleInventoryV3(nil, nil); items != nil {
		t.Errorf("expected nil inventory for a nil file, got %+v", items)
	}
}

func TestSubtitleURLExtV3(t *testing.T) {
	cases := map[string]string{
		"ass":               ".ass",
		"ssa":               ".ass",
		"pgs":               ".sup",
		"hdmv_pgs_subtitle": ".sup",
		"subrip":            ".vtt",
		"srt":               ".vtt",
		"":                  ".vtt",
	}
	for codec, want := range cases {
		if got := SubtitleURLExtV3(codec); got != want {
			t.Errorf("SubtitleURLExtV3(%q) = %q, want %q", codec, got, want)
		}
	}
}

func TestSubtitleSidecarExtV3OffersOriginalSubRipOnlyWhenNegotiated(t *testing.T) {
	negotiated := []string{FeatureSubripSidecarV3}
	for _, tc := range []struct {
		codec, source string
		features      []string
		want          string
	}{
		{"srt", SubtitleSourceExternalV3, negotiated, ".srt"},
		{"subrip", SubtitleSourceDownloadedV3, negotiated, ".srt"},
		{"srt", SubtitleSourceExternalV3, nil, ".vtt"},
		{"subrip", SubtitleSourceDownloadedV3, []string{FeatureEmbeddedSubtitlesV3}, ".vtt"},
		// Embedded tracks have no original SRT bytes to serve.
		{"subrip", SubtitleSourceEmbeddedV3, negotiated, ".vtt"},
		{"ass", SubtitleSourceExternalV3, negotiated, ".ass"},
		{"webvtt", SubtitleSourceExternalV3, negotiated, ".vtt"},
	} {
		if got := SubtitleSidecarExtV3(tc.codec, tc.source, tc.features); got != tc.want {
			t.Errorf("SubtitleSidecarExtV3(%q, %q, %v) = %q, want %q", tc.codec, tc.source, tc.features, got, tc.want)
		}
	}
}

func TestScopeSubtitleInventoryV3PublishesOriginalSubRipToOptedInClients(t *testing.T) {
	file := &models.MediaFile{
		ID:                21,
		ExternalSubtitles: []models.ExternalSubtitle{{Path: "/media/movie.ar.srt", Language: "ar", Format: "srt"}},
		SubtitleTracks:    []models.SubtitleTrack{{Index: 3, Codec: "subrip"}},
	}
	inventory := BuildSubtitleInventoryV3(file, []SubtitleInventoryEntryV3{{CombinedIndex: 2, Codec: "srt", Source: SubtitleSourceDownloadedV3, DownloadedSubtitleID: 9}})

	opted := ScopeSubtitleInventoryV3("sess", file, inventory, []string{FeatureSubripSidecarV3})
	original := SubtitleOriginalParamV3 + "=1"
	if !strings.Contains(opted[0].URL, "/subtitles/0.srt?") || !strings.Contains(opted[0].URL, original) ||
		!strings.Contains(opted[1].URL, "/subtitles/1.vtt?") || strings.Contains(opted[1].URL, original) ||
		!strings.Contains(opted[2].URL, "/subtitles/2.srt?") || !strings.Contains(opted[2].URL, original) ||
		!strings.Contains(opted[2].URL, DownloadedSubtitleIDParamV3+"=9") {
		t.Fatalf("opted-in inventory URLs: %q, %q, %q", opted[0].URL, opted[1].URL, opted[2].URL)
	}
	legacy := ScopeSubtitleInventoryV3("sess", file, inventory, nil)
	for _, item := range legacy {
		if !strings.Contains(item.URL, ".vtt?") || strings.Contains(item.URL, original) {
			t.Fatalf("a client without subrip_sidecar_v1 must keep WebVTT URLs: %q", item.URL)
		}
	}
}

// The combined ordinal a plan advertises must be the one the selection path
// resolves, or a client echoing an inventory entry addresses a different track
// than the one it picked.
func TestSubtitleEntryAtCombinedIndexV3_AgreesWithPublishedInventory(t *testing.T) {
	file := &models.MediaFile{
		ID: 11,
		ExternalSubtitles: []models.ExternalSubtitle{
			{Path: "/media/movie.en.srt", Language: "en", Format: "srt"},
		},
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Codec: "dvd_subtitle", Language: "de"},
			{Index: 1, Codec: "hdmv_pgs_subtitle", Language: "ja"},
		},
	}
	additional := []SubtitleInventoryEntryV3{
		{CombinedIndex: 3, Codec: "srt", Source: SubtitleSourceDownloadedV3, Language: "es"},
	}

	for _, item := range BuildSubtitleInventoryV3(file, additional) {
		entry, ok := subtitleEntryAtCombinedIndexV3(file, item.CombinedIndex, additional)
		if !ok {
			t.Fatalf("ordinal %d is published but does not resolve", item.CombinedIndex)
		}
		if entry.Source != item.Source {
			t.Errorf("ordinal %d resolves to source %q, inventory says %q", item.CombinedIndex, entry.Source, item.Source)
		}
		if entry.Codec != normalizeCodecV3(item.Codec) {
			t.Errorf("ordinal %d resolves to codec %q, inventory says %q", item.CombinedIndex, entry.Codec, item.Codec)
		}
	}
}

func TestSubtitleFeaturesForPlanV3FollowsThePublishedRepresentation(t *testing.T) {
	file := &models.MediaFile{ID: 8,
		ExternalSubtitles: []models.ExternalSubtitle{{Path: "/media/movie.srt", Format: "srt"}},
		SubtitleTracks:    []models.SubtitleTrack{{Index: 1, Codec: "subrip"}},
	}
	base := []string{FeatureEmbeddedSubtitlesV3}
	withFeature := []string{FeatureEmbeddedSubtitlesV3, FeatureSubripSidecarV3}
	vtt := ScopeSubtitleInventoryV3("sess", file, BuildSubtitleInventoryV3(file, nil), nil)
	srt := ScopeSubtitleInventoryV3("sess", file, BuildSubtitleInventoryV3(file, nil), withFeature)

	if got := SubtitleFeaturesForPlanV3(vtt, withFeature); HasFeatureV3(got, FeatureSubripSidecarV3) || !HasFeatureV3(got, FeatureEmbeddedSubtitlesV3) {
		t.Fatalf("a WebVTT inventory must drop only subrip_sidecar_v1: %v", got)
	}
	if got := SubtitleFeaturesForPlanV3(srt, base); !HasFeatureV3(got, FeatureSubripSidecarV3) {
		t.Fatalf("an original-SRT inventory must add subrip_sidecar_v1: %v", got)
	}
	// Only the embedded SRT track has a URL to judge by, and embedded tracks
	// never change representation, so the features stay as they were.
	embeddedOnly := ScopeSubtitleInventoryV3("sess", &models.MediaFile{ID: 8, SubtitleTracks: file.SubtitleTracks}, BuildSubtitleInventoryV3(&models.MediaFile{ID: 8, SubtitleTracks: file.SubtitleTracks}, nil), nil)
	if got := SubtitleFeaturesForPlanV3(embeddedOnly, withFeature); !slices.Equal(got, withFeature) {
		t.Fatalf("an inventory with no external or downloaded SRT must keep the features: %v", got)
	}
}

// Only tracks whose timing Silo can correct carry a sync key: external and
// downloaded text tracks in SRT, WebVTT, ASS, or SSA.
func TestBuildSubtitleInventoryV3_SyncKeys(t *testing.T) {
	file := &models.MediaFile{
		ID: 42,
		ExternalSubtitles: []models.ExternalSubtitle{
			{Path: "/media/movie.en.srt", Format: "srt"},
			{Path: "/media/movie.fr.sub", Format: "sub"},
		},
		SubtitleTracks: []models.SubtitleTrack{{Index: 2, Codec: "subrip"}},
	}
	items := BuildSubtitleInventoryV3(file, []SubtitleInventoryEntryV3{
		{Codec: "ass", DownloadedSubtitleID: 7},
		{Codec: "sub", DownloadedSubtitleID: 8},
	})
	want := []string{subtitles.ExternalSyncKey("/media/movie.en.srt"), "", "", "stored-7", ""}
	for i, item := range items {
		if item.SyncKey != want[i] {
			t.Errorf("item %d (%s %s) sync key %q, want %q", i, item.Source, item.Codec, item.SyncKey, want[i])
		}
	}
}
