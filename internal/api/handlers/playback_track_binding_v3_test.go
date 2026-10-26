package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/subtitles"
)

func TestRekeyStartTrackIdentitiesAcceptsPlanEffectiveRowOnRestart(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	// The predecessor plan bound the concrete effective row 200 while the
	// client keeps re-sending the requested provider-neutral row 100.
	session, err := manager.StartSessionWithFilesContext(context.Background(), 1, "profile-1", 200, 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSessionWithFilesContext: %v", err)
	}
	if session.MediaFileID != 200 || session.RequestedMediaFileID != 100 {
		t.Fatalf("fixture session = effective %d requested %d, want 200/100", session.MediaFileID, session.RequestedMediaFileID)
	}
	requested := &models.MediaFile{ID: 100, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?profile=hd"}
	effective := &models.MediaFile{ID: 200, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?profile=hd&result=abc"}
	foreign := &models.MediaFile{ID: 300, ContentID: "movie-2", Container: "virtual", FilePath: "virtual://movie/other?profile=hd&result=zzz"}
	handler := &PlaybackHandler{
		sessionMgr:   manager,
		fileResolver: mapPlaybackFileResolver{files: map[int]*models.MediaFile{100: requested, 200: effective, 300: foreign}},
	}

	req := playback.StartRequestV3{
		FileID:          100,
		AudioTrackID:    playback.TrackIDV3(200, "audio", 1),
		SubtitleTrackID: playback.TrackIDV3(200, "subtitle", 2),
	}
	handler.rekeyStartTrackIdentitiesV3(context.Background(), &req, 1, "profile-1", "", "")

	if want := playback.TrackIDV3(100, "audio", 1); req.AudioTrackID != want {
		t.Fatalf("audio id = %q, want %q (requested row, plan ordinal)", req.AudioTrackID, want)
	}
	if want := playback.TrackIDV3(100, "subtitle", 2); req.SubtitleTrackID != want {
		t.Fatalf("subtitle id = %q, want %q (requested row, plan ordinal)", req.SubtitleTrackID, want)
	}
	if playback.StaleTrackIdentityV3("audio", req.FileID, req.AudioTrackID) {
		t.Fatal("the re-keyed audio identity is still stale against the requested row")
	}

	// A row naming a different release is not a restart echo and must stay
	// foreign so the stale-identity guard drops it.
	foreignReq := playback.StartRequestV3{FileID: 100, AudioTrackID: playback.TrackIDV3(300, "audio", 0)}
	handler.rekeyStartTrackIdentitiesV3(context.Background(), &foreignReq, 1, "profile-1", "", "")
	if foreignReq.AudioTrackID != playback.TrackIDV3(300, "audio", 0) {
		t.Fatalf("foreign audio id = %q, want it left untouched", foreignReq.AudioTrackID)
	}
	if !playback.StaleTrackIdentityV3("audio", foreignReq.FileID, foreignReq.AudioTrackID) {
		t.Fatal("a foreign-release identity was accepted as a restart echo")
	}
}

func TestRekeyReplanTrackIdentitiesAcceptsSessionEffectiveRow(t *testing.T) {
	effective := &models.MediaFile{ID: 200, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?result=abc"}
	duplicate := &models.MediaFile{ID: 201, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?result=abc"}
	foreign := &models.MediaFile{ID: 300, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?result=zzz"}
	handler := &PlaybackHandler{
		fileResolver: mapPlaybackFileResolver{files: map[int]*models.MediaFile{200: effective, 201: duplicate, 300: foreign}},
	}
	start := playback.StartRequestV3{
		FileID:       200,
		AudioTrackID: playback.TrackIDV3(201, "audio", 1),
	}
	if _, _, err := handler.rekeyReplanTrackIdentitiesV3(context.Background(), &start, effective, &playback.Session{MediaFileID: 201, RequestedMediaFileID: 100}); err != nil {
		t.Fatalf("rekeyReplanTrackIdentitiesV3: %v", err)
	}

	if want := playback.TrackIDV3(200, "audio", 1); start.AudioTrackID != want {
		t.Fatalf("replan audio id = %q, want %q", start.AudioTrackID, want)
	}

	start.AudioTrackID = playback.TrackIDV3(300, "audio", 0)
	if _, _, err := handler.rekeyReplanTrackIdentitiesV3(context.Background(), &start, effective, &playback.Session{MediaFileID: 201}); err != nil {
		t.Fatalf("rekeyReplanTrackIdentitiesV3: %v", err)
	}
	if start.AudioTrackID != playback.TrackIDV3(300, "audio", 0) {
		t.Fatalf("foreign replan audio id = %q, want it left untouched", start.AudioTrackID)
	}
}

// Enrichment can re-probe a catalog row in place (same id, different release).
// A carried audio id that only names that row must be remapped from the
// evidence generation the selection was minted against, not reused by row-id
// equality against the rewritten layout.
func TestResolveCarriedAudioTrackRemapsSameRowAfterInPlaceReprobe(t *testing.T) {
	target := &models.MediaFile{ID: 77, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?result=abc", AudioTracks: []models.AudioTrack{
		{Index: 0, Language: "en", Codec: "aac", Channels: 2, Default: true},
		{Index: 1, Language: "ja", Codec: "aac", Channels: 2},
	}}
	source := &models.MediaFile{ID: 77, AudioTracks: []models.AudioTrack{
		{Index: 0, Language: "ja", Codec: "aac", Channels: 2},
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Default: true},
	}}
	handler := &PlaybackHandler{}

	got, ok := handler.resolveCarriedAudioTrackV3(context.Background(), playback.TrackIDV3(77, "audio", 1), target, source)
	if !ok {
		t.Fatal("carried audio selection was dropped")
	}
	if got != 0 {
		t.Fatalf("carried audio index = %d, want 0 (the English track on the re-probed layout)", got)
	}

	same := &models.MediaFile{ID: 77, AudioTracks: append([]models.AudioTrack(nil), source.AudioTracks...)}
	got, ok = handler.resolveCarriedAudioTrackV3(context.Background(), playback.TrackIDV3(77, "audio", 1), same, source)
	if !ok || got != 1 {
		t.Fatalf("unchanged evidence: index=%d ok=%v, want the original ordinal 1", got, ok)
	}
}

// A restart re-keys the echoed identity onto the requested row, but virtual
// resolution can then serve another candidate that orders its tracks
// differently. The carried ordinal must be re-identified against the served
// inventory: English at ordinal 1 on the old effective row is English at
// ordinal 0 on the resolved row, never the Japanese track that now sits at 1.
func TestRekeyStartRemapsCarriedOrdinalOntoResolvedCandidate(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	if _, err := manager.StartSessionWithFilesContext(context.Background(), 1, "profile-1", 200, 100, playback.PlayDirect, false); err != nil {
		t.Fatalf("StartSessionWithFilesContext: %v", err)
	}
	requested := &models.MediaFile{ID: 100, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?profile=hd"}
	oldEffective := &models.MediaFile{ID: 200, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?profile=hd&result=abc",
		AudioTracks: []models.AudioTrack{
			{Index: 0, Language: "ja", Codec: "aac", Channels: 2},
			{Index: 1, Language: "en", Codec: "aac", Channels: 2, Default: true},
		},
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Language: "ja", Codec: "subrip"},
			{Index: 1, Language: "en", Codec: "subrip"},
		},
	}
	resolvedRow := &models.MediaFile{ID: 300, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?profile=hd&result=xyz",
		AudioTracks: []models.AudioTrack{
			{Index: 0, Language: "en", Codec: "aac", Channels: 2, Default: true},
			{Index: 1, Language: "ja", Codec: "aac", Channels: 2},
		},
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Language: "en", Codec: "subrip"},
			{Index: 1, Language: "ja", Codec: "subrip"},
		},
	}
	handler := &PlaybackHandler{
		sessionMgr:   manager,
		fileResolver: mapPlaybackFileResolver{files: map[int]*models.MediaFile{100: requested, 200: oldEffective, 300: resolvedRow}},
	}
	req := v3HandlerStartRequest()
	req.FileID = 100
	req.AudioTrackID = playback.TrackIDV3(200, "audio", 1)
	req.AudioTrackIndex = new(1)
	req.SubtitleTrackID = playback.TrackIDV3(200, "subtitle", 1)
	req.SubtitleTrackIndex = new(1)
	// Run the production start validation sequence: structural check, re-key,
	// stale-identity drop, then the pair check inside NormalizeAndValidate.
	if err := req.ValidateSelectedTrackStructureV3(); err != nil {
		t.Fatalf("ValidateSelectedTrackStructureV3: %v", err)
	}
	rekeys := handler.rekeyStartTrackIdentitiesV3(context.Background(), &req, 1, "profile-1", "", "")
	dropStaleRequestTrackIdentitiesV3(context.Background(), &req)
	if _, err := req.NormalizeAndValidate(); err != nil {
		t.Fatalf("NormalizeAndValidate after re-key: %v", err)
	}

	// Resolution serves row 300 under the requested row id (the start path keeps
	// req.FileID for the attempt record but installs the served tracks).
	served := *resolvedRow
	served.ID = req.FileID
	audioUnavailable, subtitleUnavailable, err := handler.constrainRekeyedStartTracksV3(context.Background(), &req, &served, rekeys)
	if err != nil {
		t.Fatalf("constrainRekeyedStartTracksV3: %v", err)
	}
	if audioUnavailable || subtitleUnavailable {
		t.Fatalf("selection reported unavailable: audio=%v subtitle=%v", audioUnavailable, subtitleUnavailable)
	}
	if req.AudioTrackIndex == nil || *req.AudioTrackIndex != 0 {
		t.Fatalf("audio index = %v, want the remapped ordinal 0", req.AudioTrackIndex)
	}
	if _, err := req.NormalizeAndValidate(); err != nil {
		t.Fatalf("remapped audio id/index pair failed validation: %v", err)
	}
	if req.SubtitleTrackIndex == nil || *req.SubtitleTrackIndex != 0 {
		t.Fatalf("subtitle index = %v, want the remapped ordinal 0", req.SubtitleTrackIndex)
	}
	if _, err := req.NormalizeAndValidate(); err != nil {
		t.Fatalf("remapped subtitle id/index pair failed validation: %v", err)
	}

	index, degraded, err := resolveV3AudioIndex(&served, req.AudioTrackID, req.AudioTrackIndex)
	if err != nil {
		t.Fatalf("resolveV3AudioIndex: %v", err)
	}
	if degraded {
		t.Fatal("audio selection degraded to the default instead of being remapped")
	}
	if index != 0 || served.AudioTracks[index].Language != "en" {
		t.Fatalf("audio index = %d language = %q, want 0/en (English, not the Japanese track at ordinal 1)",
			index, served.AudioTracks[index].Language)
	}

	_, kind, ordinal, ok := playback.ParseTrackIDV3(req.SubtitleTrackID)
	if !ok || kind != "subtitle" {
		t.Fatalf("subtitle id = %q, want a subtitle identity", req.SubtitleTrackID)
	}
	if ordinal != 0 {
		t.Fatalf("subtitle ordinal = %d, want 0 (English on the resolved inventory)", ordinal)
	}
}

// When the resolved candidate has no equivalent of the carried selection, the
// start path must clear it and report the selection unavailable rather than
// reusing the stale ordinal against a different language.
func TestRekeyStartReportsUnavailableWhenResolvedCandidateLacksSelection(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	if _, err := manager.StartSessionWithFilesContext(context.Background(), 1, "profile-1", 200, 100, playback.PlayDirect, false); err != nil {
		t.Fatalf("StartSessionWithFilesContext: %v", err)
	}
	requested := &models.MediaFile{ID: 100, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?profile=hd"}
	oldEffective := &models.MediaFile{ID: 200, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?profile=hd&result=abc",
		AudioTracks: []models.AudioTrack{
			{Index: 0, Language: "ja", Codec: "aac", Channels: 2},
			{Index: 1, Language: "en", Codec: "aac", Channels: 2, Default: true},
		},
	}
	resolvedRow := &models.MediaFile{ID: 300, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?profile=hd&result=xyz",
		AudioTracks: []models.AudioTrack{{Index: 0, Language: "ja", Codec: "aac", Channels: 2, Default: true}},
	}
	handler := &PlaybackHandler{
		sessionMgr:   manager,
		fileResolver: mapPlaybackFileResolver{files: map[int]*models.MediaFile{100: requested, 200: oldEffective, 300: resolvedRow}},
	}
	req := playback.StartRequestV3{FileID: 100, AudioTrackID: playback.TrackIDV3(200, "audio", 1)}
	rekeys := handler.rekeyStartTrackIdentitiesV3(context.Background(), &req, 1, "profile-1", "", "")

	served := *resolvedRow
	served.ID = req.FileID
	audioUnavailable, _, err := handler.constrainRekeyedStartTracksV3(context.Background(), &req, &served, rekeys)
	if err != nil {
		t.Fatalf("constrainRekeyedStartTracksV3: %v", err)
	}
	if !audioUnavailable {
		t.Fatal("a selection with no equivalent on the resolved candidate was not reported unavailable")
	}
	if req.AudioTrackID != "" || req.AudioTrackIndex != nil {
		t.Fatalf("unavailable audio selection was kept: id=%q index=%v", req.AudioTrackID, req.AudioTrackIndex)
	}
}

// The replan counterpart accepts a committed session row, but the carried
// ordinal still belongs to that row's inventory. A rotation to a candidate that
// reverses the track order must remap English@1 to English@0 instead of
// re-keying the ordinal onto the new row's Japanese@1.
func TestRekeyReplanRemapsCarriedOrdinalOntoEffectiveCandidate(t *testing.T) {
	oldEffective := &models.MediaFile{ID: 200, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?result=abc",
		AudioTracks: []models.AudioTrack{
			{Index: 0, Language: "ja", Codec: "aac", Channels: 2},
			{Index: 1, Language: "en", Codec: "aac", Channels: 2, Default: true},
		},
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Language: "ja", Codec: "subrip"},
			{Index: 1, Language: "en", Codec: "subrip"},
		},
	}
	effective := &models.MediaFile{ID: 300, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?result=xyz",
		AudioTracks: []models.AudioTrack{
			{Index: 0, Language: "en", Codec: "aac", Channels: 2, Default: true},
			{Index: 1, Language: "ja", Codec: "aac", Channels: 2},
		},
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Language: "en", Codec: "subrip"},
			{Index: 1, Language: "ja", Codec: "subrip"},
		},
	}
	handler := &PlaybackHandler{
		fileResolver: mapPlaybackFileResolver{files: map[int]*models.MediaFile{200: oldEffective, 300: effective}},
	}
	start := v3HandlerStartRequest()
	start.FileID = 300
	start.AudioTrackID = playback.TrackIDV3(200, "audio", 1)
	start.AudioTrackIndex = new(1)
	start.SubtitleTrackID = playback.TrackIDV3(200, "subtitle", 1)
	start.SubtitleTrackIndex = new(1)
	session := &playback.Session{MediaFileID: 200, RequestedMediaFileID: 100}
	audioUnavailable, subtitleUnavailable, err := handler.rekeyReplanTrackIdentitiesV3(context.Background(), &start, effective, session)
	if err != nil {
		t.Fatalf("rekeyReplanTrackIdentitiesV3: %v", err)
	}
	if audioUnavailable || subtitleUnavailable {
		t.Fatalf("selection reported unavailable: audio=%v subtitle=%v", audioUnavailable, subtitleUnavailable)
	}
	// NormalizeAndValidate is the next step in the replan path; it rejects an
	// id/index pair that names two different tracks.
	if _, err := start.NormalizeAndValidate(); err != nil {
		t.Fatalf("NormalizeAndValidate after re-key: %v", err)
	}
	if start.AudioTrackIndex == nil || *start.AudioTrackIndex != 0 {
		t.Fatalf("audio index = %v, want the remapped ordinal 0", start.AudioTrackIndex)
	}
	if _, err := start.NormalizeAndValidate(); err != nil {
		t.Fatalf("remapped audio id/index pair failed validation: %v", err)
	}
	if start.SubtitleTrackIndex == nil || *start.SubtitleTrackIndex != 0 {
		t.Fatalf("subtitle index = %v, want the remapped ordinal 0", start.SubtitleTrackIndex)
	}
	if _, err := start.NormalizeAndValidate(); err != nil {
		t.Fatalf("remapped subtitle id/index pair failed validation: %v", err)
	}

	index, degraded, err := resolveV3AudioIndex(effective, start.AudioTrackID, start.AudioTrackIndex)
	if err != nil {
		t.Fatalf("resolveV3AudioIndex: %v", err)
	}
	if degraded {
		t.Fatal("replan audio selection degraded to the default instead of being remapped")
	}
	if index != 0 || effective.AudioTracks[index].Language != "en" {
		t.Fatalf("replan audio index = %d language = %q, want 0/en", index, effective.AudioTracks[index].Language)
	}

	_, kind, ordinal, ok := playback.ParseTrackIDV3(start.SubtitleTrackID)
	if !ok || kind != "subtitle" {
		t.Fatalf("replan subtitle id = %q, want a subtitle identity", start.SubtitleTrackID)
	}
	if ordinal != 0 {
		t.Fatalf("replan subtitle ordinal = %d, want 0 (English on the effective inventory)", ordinal)
	}
}

// A same-row rotation that swaps two tracks agreeing on language, codec,
// channels, layout, and index is only visible in their titles. The fast path
// must not treat the row id as proof that the carried ordinal still names the
// selected stream; it has to remap onto the title the viewer chose.
func TestRemapAudioSelectionSameRowTitleOnlyReorder(t *testing.T) {
	mainTrack := func(index int) models.AudioTrack {
		return models.AudioTrack{Index: index, Codec: "aac", Language: "eng", Layout: "stereo", Channels: 2, Title: "Main"}
	}
	commentary := func(index int) models.AudioTrack {
		return models.AudioTrack{Index: index, Codec: "aac", Language: "eng", Layout: "stereo", Channels: 2, Title: "Commentary"}
	}
	source := &models.MediaFile{ID: 7, AudioTracks: []models.AudioTrack{mainTrack(0), commentary(1)}}
	target := &models.MediaFile{ID: 7, AudioTracks: []models.AudioTrack{commentary(0), mainTrack(1)}}

	request := playback.StartRequestV3{AudioTrackID: playback.TrackIDV3(7, "audio", 1)}
	if err := remapAudioSelectionV3(source, target, &request); err != nil {
		t.Fatalf("remapAudioSelectionV3: %v", err)
	}
	if request.AudioTrackIndex == nil || *request.AudioTrackIndex != 0 {
		t.Fatalf("remapped audio index = %v, want 0 (the commentary track after the swap)", request.AudioTrackIndex)
	}
	_, kind, ordinal, ok := playback.ParseTrackIDV3(request.AudioTrackID)
	if !ok || kind != "audio" || ordinal != 0 {
		t.Fatalf("remapped audio id = %q, want ordinal 0", request.AudioTrackID)
	}
}

// A re-keyed subtitle ordinal is not proof that the served candidate has the
// same track. When the target retains the ordinal but holds a different
// language there, the confirmed miss must clear both fields and report the
// degradation instead of leaving the old ordinal to select that language.
func TestRekeyStartClearsSubtitleMissingFromResolvedCandidate(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	if _, err := manager.StartSessionWithFilesContext(context.Background(), 1, "profile-1", 200, 100, playback.PlayDirect, false); err != nil {
		t.Fatalf("StartSessionWithFilesContext: %v", err)
	}
	requested := &models.MediaFile{ID: 100, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?profile=hd"}
	oldEffective := &models.MediaFile{ID: 200, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?profile=hd&result=abc",
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Language: "ja", Codec: "subrip"},
			{Index: 1, Language: "en", Codec: "subrip"},
		},
	}
	resolvedRow := &models.MediaFile{ID: 300, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?profile=hd&result=xyz",
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Language: "fr", Codec: "subrip"},
			{Index: 1, Language: "ja", Codec: "subrip"},
		},
	}
	handler := &PlaybackHandler{
		sessionMgr:   manager,
		fileResolver: mapPlaybackFileResolver{files: map[int]*models.MediaFile{100: requested, 200: oldEffective, 300: resolvedRow}},
	}
	req := v3HandlerStartRequest()
	req.FileID = 100
	req.SubtitleTrackID = playback.TrackIDV3(200, "subtitle", 1)
	req.SubtitleTrackIndex = new(1)
	rekeys := handler.rekeyStartTrackIdentitiesV3(context.Background(), &req, 1, "profile-1", "", "")
	served := *resolvedRow
	served.ID = req.FileID

	audioUnavailable, subtitleUnavailable, err := handler.constrainRekeyedStartTracksV3(context.Background(), &req, &served, rekeys)
	if err != nil {
		t.Fatalf("constrainRekeyedStartTracksV3: %v", err)
	}
	if audioUnavailable {
		t.Fatal("audio selection reported unavailable")
	}
	if !subtitleUnavailable {
		t.Fatal("a subtitle with no equivalent on the resolved candidate was not reported unavailable")
	}
	if req.SubtitleTrackID != "" || req.SubtitleTrackIndex != nil {
		t.Fatalf("missing subtitle selection was kept: id=%q index=%v", req.SubtitleTrackID, req.SubtitleTrackIndex)
	}
}

// The replan counterpart must apply the same fail-closed rule: a target that
// keeps the ordinal but not the selected language is a miss, not a success.
func TestRekeyReplanClearsSubtitleMissingFromEffectiveCandidate(t *testing.T) {
	oldEffective := &models.MediaFile{ID: 200, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?result=abc",
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Language: "ja", Codec: "subrip"},
			{Index: 1, Language: "en", Codec: "subrip"},
		},
	}
	effective := &models.MediaFile{ID: 300, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?result=xyz",
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Language: "fr", Codec: "subrip"},
			{Index: 1, Language: "ja", Codec: "subrip"},
		},
	}
	handler := &PlaybackHandler{
		fileResolver: mapPlaybackFileResolver{files: map[int]*models.MediaFile{200: oldEffective, 300: effective}},
	}
	start := v3HandlerStartRequest()
	start.FileID = 300
	start.SubtitleTrackID = playback.TrackIDV3(200, "subtitle", 1)
	start.SubtitleTrackIndex = new(1)

	audioUnavailable, subtitleUnavailable, err := handler.rekeyReplanTrackIdentitiesV3(context.Background(), &start, effective, &playback.Session{MediaFileID: 200, RequestedMediaFileID: 100})
	if err != nil {
		t.Fatalf("rekeyReplanTrackIdentitiesV3: %v", err)
	}
	if audioUnavailable {
		t.Fatal("audio selection reported unavailable")
	}
	if !subtitleUnavailable {
		t.Fatal("a subtitle with no equivalent on the effective row was not reported unavailable")
	}
	if start.SubtitleTrackID != "" || start.SubtitleTrackIndex != nil {
		t.Fatalf("missing subtitle selection was kept: id=%q index=%v", start.SubtitleTrackID, start.SubtitleTrackIndex)
	}
}

type failingSubtitleListRepoV3 struct {
	subtitles.Repository
	err error
}

func (r failingSubtitleListRepoV3) ListDownloadedSubtitles(context.Context, int) ([]subtitles.DownloadedSubtitle, error) {
	return nil, r.err
}

// A failed downloaded-subtitle lookup says nothing about whether the selected
// track exists. The re-key must surface the dependency failure rather than
// treat the unread source as an empty inventory and reinterpret or clear the
// ordinal against the target.
func TestRekeyStartPropagatesDownloadedSubtitleLookupFailure(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	if _, err := manager.StartSessionWithFilesContext(context.Background(), 1, "profile-1", 200, 100, playback.PlayDirect, false); err != nil {
		t.Fatalf("StartSessionWithFilesContext: %v", err)
	}
	requested := &models.MediaFile{ID: 100, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?profile=hd"}
	oldEffective := &models.MediaFile{ID: 200, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?profile=hd&result=abc"}
	resolvedRow := &models.MediaFile{ID: 300, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?profile=hd&result=xyz",
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "subrip"}},
	}
	storeErr := errors.New("subtitle database unavailable")
	handler := &PlaybackHandler{
		sessionMgr:   manager,
		fileResolver: mapPlaybackFileResolver{files: map[int]*models.MediaFile{100: requested, 200: oldEffective, 300: resolvedRow}},
		SubtitleRepo: failingSubtitleListRepoV3{err: storeErr},
	}
	req := v3HandlerStartRequest()
	req.FileID = 100
	req.SubtitleTrackID = playback.TrackIDV3(200, "subtitle", 0)
	req.SubtitleTrackIndex = new(0)
	rekeys := handler.rekeyStartTrackIdentitiesV3(context.Background(), &req, 1, "profile-1", "", "")
	served := *resolvedRow
	served.ID = req.FileID

	_, _, err := handler.constrainRekeyedStartTracksV3(context.Background(), &req, &served, rekeys)
	if err == nil {
		t.Fatal("a failed downloaded-subtitle lookup was treated as a track miss")
	}
	if !errors.Is(err, errSubtitleStoreUnavailableV3) {
		t.Fatalf("error = %v, want a wrapped subtitle store failure", err)
	}
}

func TestRekeyReplanPropagatesDownloadedSubtitleLookupFailure(t *testing.T) {
	oldEffective := &models.MediaFile{ID: 200, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?result=abc"}
	effective := &models.MediaFile{ID: 300, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?result=xyz",
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "subrip"}},
	}
	storeErr := errors.New("subtitle database unavailable")
	handler := &PlaybackHandler{
		fileResolver: mapPlaybackFileResolver{files: map[int]*models.MediaFile{200: oldEffective, 300: effective}},
		SubtitleRepo: failingSubtitleListRepoV3{err: storeErr},
	}
	start := v3HandlerStartRequest()
	start.FileID = 300
	start.SubtitleTrackID = playback.TrackIDV3(200, "subtitle", 0)
	start.SubtitleTrackIndex = new(0)

	_, _, err := handler.rekeyReplanTrackIdentitiesV3(context.Background(), &start, effective, &playback.Session{MediaFileID: 200, RequestedMediaFileID: 100})
	if err == nil {
		t.Fatal("a failed downloaded-subtitle lookup was treated as a track miss")
	}
	if !errors.Is(err, errSubtitleStoreUnavailableV3) {
		t.Fatalf("error = %v, want a wrapped subtitle store failure", err)
	}
}

// Enrichment can re-probe the same catalog row in place between attempts. A
// restart that names the predecessor attempt must interpret the echoed ordinal
// against that attempt's captured plan inventory, not the rewritten row, or
// English at ordinal 1 becomes whatever now sits at ordinal 1.
func TestRekeyStartUsesPredecessorPlanInventoryAfterSameRowReprobe(t *testing.T) {
	prior := &models.MediaFile{ID: 100, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?result=abc",
		AudioTracks: []models.AudioTrack{
			{Index: 0, Language: "ja", Codec: "aac", Channels: 2},
			{Index: 1, Language: "en", Codec: "aac", Channels: 2, Default: true},
		},
	}
	current := &models.MediaFile{ID: 100, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?result=abc",
		AudioTracks: []models.AudioTrack{
			{Index: 0, Language: "en", Codec: "aac", Channels: 2, Default: true},
			{Index: 1, Language: "ja", Codec: "aac", Channels: 2},
		},
	}
	store := playback.NewMemoryPlanStoreV3()
	record := playback.AttemptRecordV3{
		PlaybackAttemptID:    "attempt-predecessor-reprobe",
		SessionID:            "predecessor-session",
		UserID:               1,
		ProfileID:            "profile-1",
		RequestedMediaFileID: prior.ID,
		EffectiveMediaFileID: prior.ID,
		CurrentPlan: playback.PlanV3{
			AudioTracks: playback.AudioInventoryV3(prior),
			Subtitle: playback.SubtitleDecisionV3{
				Inventory: playback.BuildSubtitleInventoryV3(prior, nil),
			},
		},
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := store.SaveAttempt(context.Background(), record); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}
	handler := &PlaybackHandler{
		fileResolver: mapPlaybackFileResolver{files: map[int]*models.MediaFile{100: current}},
		PlanStoreV3:  store,
	}
	req := v3HandlerStartRequest()
	req.FileID = 100
	req.AudioTrackID = playback.TrackIDV3(100, "audio", 1)
	req.PreviousPlaybackAttemptID = record.PlaybackAttemptID

	rekeys := handler.rekeyStartTrackIdentitiesV3(context.Background(), &req, 1, "profile-1", "", "")
	audioUnavailable, _, err := handler.constrainRekeyedStartTracksV3(context.Background(), &req, current, rekeys)
	if err != nil {
		t.Fatalf("constrainRekeyedStartTracksV3: %v", err)
	}
	if audioUnavailable {
		t.Fatal("the predecessor's English selection was reported unavailable")
	}
	_, kind, ordinal, ok := playback.ParseTrackIDV3(req.AudioTrackID)
	if !ok || kind != "audio" || ordinal != 0 {
		t.Fatalf("audio id = %q, want ordinal 0 (English on the re-probed row)", req.AudioTrackID)
	}
	if req.AudioTrackIndex != nil && *req.AudioTrackIndex != 0 {
		t.Fatalf("audio index = %v, want 0", req.AudioTrackIndex)
	}
}
