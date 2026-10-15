package handlers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/settingscontract"
	"github.com/Silo-Server/silo-server/internal/settingskeys"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

// subtitleBurnInFixtureFileV3 is a probed file whose default audio track is
// English but whose preferred audio language is French, carrying an embedded
// PGS subtitle track that only server-side burn-in can deliver. This is the
// production shape that used to plan AV1 with a default audio selection and
// then restart FFmpeg twice to fix the audio and the subtitle.
func subtitleBurnInFixtureFileV3(t *testing.T) *models.MediaFile {
	t.Helper()
	file := v3HandlerFixtureFile(t)
	file.AudioTracks = []models.AudioTrack{
		{Index: 1, Language: "eng", Codec: "truehd", Channels: 6, Layout: "5.1", Default: true},
		{Index: 2, Language: "fra", Codec: "truehd", Channels: 6, Layout: "5.1"},
	}
	file.SubtitleTracks = []models.SubtitleTrack{
		{Index: 3, Language: "eng", Codec: "hdmv_pgs_subtitle"},
	}
	return file
}

// TestStartResolvesBurnInAndPreferredAudioOnFirstPlan proves the fast-start
// plan fix: when the profile's stored subtitle mode is "always" with a language
// that only a burn-in can deliver, and a preferred audio language is stored, the
// first plan already carries the burn-in requirement (so the burn-in codec
// H.264 is chosen instead of the AV1-first ordinary order) and the preferred
// audio track. No audio or subtitle reconciliation replan is needed: the plan
// the client receives is already the corrected one.
func TestStartResolvesBurnInAndPreferredAudioOnFirstPlan(t *testing.T) {
	store := newPlaybackTestStore(t)
	seed := func(key string, value any) {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.UpsertSettingValue(context.Background(), userstore.SettingIdentity{
			Key: key, Scope: settingscontract.ScopeProfile, ProfileID: "profile-1",
		}, encoded); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	seed(settingskeys.PlaybackAudioLanguage, "fra")
	seed(settingskeys.PlaybackSubtitleLanguage, "eng")
	seed(settingskeys.PlaybackSubtitleMode, "always")

	file := subtitleBurnInFixtureFileV3(t)
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), testPlaybackFileResolver{file: file})
	handler.StoreProvider = testUserStoreProvider{store: store}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{
		config.Allow4KTranscodeSettingKey:          "true",
		config.PlaybackAllowHEVCEncodingSettingKey: "true",
	}}
	handler.PlaybackConfig = func() config.PlaybackConfig {
		return config.PlaybackConfig{
			FFmpegPath:       writePlaybackTestFFmpeg(t),
			TranscodeDir:     t.TempDir(),
			TranscodeEnabled: true,
			HWAccel:          "qsv",
		}
	}
	presetLocalRegistryV3(handler, playback.NewTransformationRegistryV3([]playback.TransformationSpecV3{
		{Name: playback.TransformationAudioToAACV3, RecipeVersion: playback.TransformationAudioToAACRecipeVersionV3, Available: true},
		{Name: playback.TransformationVideoToH264V3, RecipeVersion: playback.TransformationVideoToH264RecipeVersionV3, Available: true},
		{Name: playback.TransformationVideoToHEVCV3, RecipeVersion: playback.TransformationVideoToHEVCRecipeVersionV3, Available: true},
		{Name: playback.TransformationVideoToAV1V3, RecipeVersion: playback.TransformationVideoToAV1RecipeVersionV3, Available: true},
	}))

	start := v3HandlerStartRequest()
	caps := start.Capabilities
	caps.CodecsVideo = []string{"h264", "hevc", "av1"}
	caps.CodecsVideoHardware = []string{"h264", "hevc", "av1"}
	caps.Containers = []string{"mp4", "mkv"}
	start.Capabilities = caps
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassOriginalHTTPV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true, VideoCodecs: []string{"h264"},
		Subtitles: playback.DeliverySubtitleCapabilitiesV3{EmbeddedText: true, SidecarText: true},
	}
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true,
		Containers:        []string{"hls"},
		VideoCodecs:       []string{"h264", "hevc", "av1"},
		AudioDecodeCodecs: []string{"aac"},
		Subtitles:         playback.DeliverySubtitleCapabilitiesV3{EmbeddedText: true, SidecarText: true},
	}

	started := startV3PlaybackV2ForHandlerTest(t, handler, start)
	plan := started.PlaybackPlan
	if plan == nil {
		t.Fatalf("no plan: terminal=%#v", started.Terminal)
	}

	if plan.DecisionReason != "subtitle_burn_in_required" {
		t.Fatalf("decision reason = %q, want the burn-in requirement on the first plan", plan.DecisionReason)
	}
	if plan.Subtitle.Mode != playback.SubtitleBurnInV3 {
		t.Fatalf("subtitle mode = %q, want %q on the first plan", plan.Subtitle.Mode, playback.SubtitleBurnInV3)
	}
	if plan.EffectiveRecipe.VideoCodec != "h264" {
		t.Fatalf("video codec = %q, want the burn-in floor h264 (not AV1-first)", plan.EffectiveRecipe.VideoCodec)
	}
	if plan.Delivery != playback.DeliveryTranscodeHLSV3 {
		t.Fatalf("delivery = %q, want %q", plan.Delivery, playback.DeliveryTranscodeHLSV3)
	}
	if plan.SelectedTracks.Audio == nil || plan.SelectedTracks.Audio.Index == nil {
		t.Fatalf("plan carries no selected audio track: %#v", plan.SelectedTracks.Audio)
	}
	if got := *plan.SelectedTracks.Audio.Index; got != 1 {
		t.Fatalf("selected audio index = %d, want the preferred French track at array index 1", got)
	}
	if plan.SelectedTracks.Subtitle == nil || plan.SelectedTracks.Subtitle.Index == nil {
		t.Fatalf("plan carries no selected subtitle track: %#v", plan.SelectedTracks.Subtitle)
	}
	if got := *plan.SelectedTracks.Subtitle.Index; got != 0 {
		t.Fatalf("selected subtitle index = %d, want the embedded burn-in track at combined index 0", got)
	}
}

// TestSelectPreferredSubtitleV3 pins the selection rules: a stored track
// signature wins over language matching, an empty preference selects nothing,
// and a forced track is only taken when the preference opts in or no non-forced
// track matches.
func TestSelectPreferredSubtitleV3(t *testing.T) {
	inventory := []playback.SubtitleInventoryItemV3{
		{TrackID: "subtitle:0", CombinedIndex: 0, Source: "external", Codec: "srt", Language: "eng"},
		{TrackID: "subtitle:1", CombinedIndex: 1, Source: "embedded", Codec: "hdmv_pgs_subtitle", Language: "eng"},
		{TrackID: "subtitle:2", CombinedIndex: 2, Source: "embedded", Codec: "subrip", Language: "fra", Forced: true},
	}

	t.Run("no language selects nothing", func(t *testing.T) {
		if _, ok := selectPreferredSubtitleV3(inventory, playbackSubtitlePreferenceV3{}, nil); ok {
			t.Fatal("an empty language preference must not select a track")
		}
	})

	t.Run("signature beats language and reaches the bitmap track", func(t *testing.T) {
		sig := &userstore.SubtitleTrackSignature{Source: "embedded", Codec: "hdmv_pgs_subtitle", Language: "eng"}
		item, ok := selectPreferredSubtitleV3(inventory, playbackSubtitlePreferenceV3{Language: "eng", HasLanguage: true}, sig)
		if !ok || item.CombinedIndex != 1 {
			t.Fatalf("signature selection = (%+v, %v), want the embedded bitmap track", item, ok)
		}
	})

	t.Run("best language match wins", func(t *testing.T) {
		item, ok := selectPreferredSubtitleV3(inventory, playbackSubtitlePreferenceV3{Language: "eng", HasLanguage: true}, nil)
		if !ok || item.CombinedIndex != 0 {
			t.Fatalf("language selection = (%+v, %v), want the first English track", item, ok)
		}
	})

	t.Run("forced track needs the opt-in", func(t *testing.T) {
		forcedOnly := []playback.SubtitleInventoryItemV3{{CombinedIndex: 0, Source: "embedded", Codec: "subrip", Language: "fra", Forced: true}}
		if _, ok := selectPreferredSubtitleV3(forcedOnly, playbackSubtitlePreferenceV3{Language: "fra", HasLanguage: true}, nil); ok {
			t.Fatal("a forced track must not be selected without the forced opt-in")
		}
		item, ok := selectPreferredSubtitleV3(forcedOnly, playbackSubtitlePreferenceV3{Language: "fra", HasLanguage: true, ShowForced: true, HasShowForced: true}, nil)
		if !ok || item.CombinedIndex != 0 {
			t.Fatalf("forced opt-in selection = (%+v, %v), want the forced track", item, ok)
		}
	})
}

// TestSelectPreferredSubtitleV3FallsBackToNonForcedMatch pins the forced
// fallback: a forced track can outrank an available non-forced one (an exact
// forced tag beats a regional non-forced variant), and the selection must land
// on the non-forced match rather than be abandoned.
func TestSelectPreferredSubtitleV3FallsBackToNonForcedMatch(t *testing.T) {
	inventory := []playback.SubtitleInventoryItemV3{
		{CombinedIndex: 0, Source: "embedded", Codec: "subrip", Language: "fr-CA"},
		{CombinedIndex: 1, Source: "embedded", Codec: "hdmv_pgs_subtitle", Language: "fr", Forced: true},
	}
	item, ok := selectPreferredSubtitleV3(inventory, playbackSubtitlePreferenceV3{Language: "fr", HasLanguage: true}, nil)
	if !ok || item.CombinedIndex != 0 {
		t.Fatalf("selection = (%+v, %v), want the non-forced regional match at combined index 0", item, ok)
	}
}

func seedPlaybackSubtitlePreferenceV3(t *testing.T, store userstore.UserStore, key string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertSettingValue(context.Background(), userstore.SettingIdentity{
		Key: key, Scope: settingscontract.ScopeProfile, ProfileID: "profile-1",
	}, encoded); err != nil {
		t.Fatalf("seed %s: %v", key, err)
	}
}

func subtitleResolverHandlerV3(t *testing.T, store userstore.UserStore, file *models.MediaFile) *PlaybackHandler {
	t.Helper()
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), testPlaybackFileResolver{file: file})
	handler.StoreProvider = testUserStoreProvider{store: store}
	return handler
}

// TestResolvePreferredSubtitleSelectionV3StoredOffSelectsNothing pins that a
// stored off mode leaves an omitted selection to the client: turning subtitles
// on is scoped to the "always" mode.
func TestResolvePreferredSubtitleSelectionV3StoredOffSelectsNothing(t *testing.T) {
	store := newPlaybackTestStore(t)
	seedPlaybackSubtitlePreferenceV3(t, store, settingskeys.PlaybackSubtitleLanguage, "eng")
	seedPlaybackSubtitlePreferenceV3(t, store, settingskeys.PlaybackSubtitleMode, "off")
	file := subtitleBurnInFixtureFileV3(t)

	if _, ok := subtitleResolverHandlerV3(t, store, file).resolvePreferredSubtitleSelectionV3(context.Background(), 1, "profile-1", "", file, nil); ok {
		t.Fatal("a stored off mode must not resolve a subtitle selection on omission")
	}
}

// TestResolvePreferredSubtitleSelectionV3StoredAlwaysSelectsTrack pins the
// intended behavior change: an omitted selection resolves to the stored track
// when the profile's mode is "always".
func TestResolvePreferredSubtitleSelectionV3StoredAlwaysSelectsTrack(t *testing.T) {
	store := newPlaybackTestStore(t)
	seedPlaybackSubtitlePreferenceV3(t, store, settingskeys.PlaybackSubtitleLanguage, "eng")
	seedPlaybackSubtitlePreferenceV3(t, store, settingskeys.PlaybackSubtitleMode, "always")
	file := subtitleBurnInFixtureFileV3(t)

	index, ok := subtitleResolverHandlerV3(t, store, file).resolvePreferredSubtitleSelectionV3(context.Background(), 1, "profile-1", "", file, nil)
	if !ok || index != 0 {
		t.Fatalf("resolved selection = (%d, %v), want the embedded track at combined index 0", index, ok)
	}
}

// TestStartExplicitSubtitleIndexWinsOverStoredAlways pins that a named client
// selection is never overridden by the stored "always" mode. The stored mode
// would tie-break to the first English track (the text track at index 0); the
// explicit index selects the bitmap PGS track at index 1, which forces burn-in.
func TestStartExplicitSubtitleIndexWinsOverStoredAlways(t *testing.T) {
	store := newPlaybackTestStore(t)
	seedPlaybackSubtitlePreferenceV3(t, store, settingskeys.PlaybackSubtitleLanguage, "eng")
	seedPlaybackSubtitlePreferenceV3(t, store, settingskeys.PlaybackSubtitleMode, "always")

	file := subtitleBurnInFixtureFileV3(t)
	file.SubtitleTracks = []models.SubtitleTrack{
		{Index: 3, Language: "eng", Codec: "subrip"},
		{Index: 4, Language: "eng", Codec: "hdmv_pgs_subtitle"},
	}

	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), testPlaybackFileResolver{file: file})
	handler.StoreProvider = testUserStoreProvider{store: store}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{
		config.Allow4KTranscodeSettingKey:          "true",
		config.PlaybackAllowHEVCEncodingSettingKey: "true",
	}}
	handler.PlaybackConfig = func() config.PlaybackConfig {
		return config.PlaybackConfig{
			FFmpegPath:       writePlaybackTestFFmpeg(t),
			TranscodeDir:     t.TempDir(),
			TranscodeEnabled: true,
			HWAccel:          "qsv",
		}
	}
	presetLocalRegistryV3(handler, playback.NewTransformationRegistryV3([]playback.TransformationSpecV3{
		{Name: playback.TransformationAudioToAACV3, RecipeVersion: playback.TransformationAudioToAACRecipeVersionV3, Available: true},
		{Name: playback.TransformationVideoToH264V3, RecipeVersion: playback.TransformationVideoToH264RecipeVersionV3, Available: true},
		{Name: playback.TransformationVideoToHEVCV3, RecipeVersion: playback.TransformationVideoToHEVCRecipeVersionV3, Available: true},
	}))

	start := v3HandlerStartRequest()
	caps := start.Capabilities
	caps.CodecsVideo = []string{"h264", "hevc"}
	caps.CodecsVideoHardware = []string{"h264", "hevc"}
	caps.Containers = []string{"mp4", "mkv"}
	start.Capabilities = caps
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassOriginalHTTPV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true, VideoCodecs: []string{"h264"},
		Subtitles: playback.DeliverySubtitleCapabilitiesV3{EmbeddedText: true, SidecarText: true},
	}
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true,
		Containers:        []string{"hls"},
		VideoCodecs:       []string{"h264", "hevc"},
		AudioDecodeCodecs: []string{"aac"},
		Subtitles:         playback.DeliverySubtitleCapabilitiesV3{EmbeddedText: true, SidecarText: true},
	}
	const explicit = 1
	start.SubtitleTrackIndex = intPtrPlaybackV3(explicit)
	start.SubtitleTrackID = playback.TrackIDV3(file.ID, "subtitle", explicit)

	started := startV3PlaybackV2ForHandlerTest(t, handler, start)
	plan := started.PlaybackPlan
	if plan == nil {
		t.Fatalf("no plan: terminal=%#v", started.Terminal)
	}
	if plan.SelectedTracks.Subtitle == nil || plan.SelectedTracks.Subtitle.Index == nil || *plan.SelectedTracks.Subtitle.Index != explicit {
		t.Fatalf("selected subtitle = %#v, want the explicit index %d", plan.SelectedTracks.Subtitle, explicit)
	}
	if plan.Subtitle.Mode != playback.SubtitleBurnInV3 {
		t.Fatalf("subtitle mode = %q, want burn-in for the explicitly selected bitmap track", plan.Subtitle.Mode)
	}
}

func intPtrPlaybackV3(v int) *int { return &v }
