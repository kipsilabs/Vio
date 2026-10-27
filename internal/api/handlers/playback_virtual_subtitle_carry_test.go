package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/subtitles"
)

// The viewer's English embedded track sat at ordinal 1 on the old candidate and
// at ordinal 0 on the rotated one. The carry must follow the identity, never
// reuse the old position.
func TestSubtitleCarryExactAcrossRotationDifferingOrdinals(t *testing.T) {
	oldEvidence := &models.MediaFile{ID: 200, FilePath: "virtual://movie/tt?result=a",
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Language: "ja", Codec: "subrip"},
			{Index: 1, Language: "en", Codec: "subrip"},
		},
	}
	target := &models.MediaFile{ID: 300, FilePath: "virtual://movie/tt?result=b",
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Language: "en", Codec: "subrip"},
			{Index: 1, Language: "ja", Codec: "subrip"},
		},
	}
	handler := &PlaybackHandler{}
	_, ordinal, exact, matched, err := handler.carrySubtitleSelectionAcrossRotationV3(
		context.Background(), nil, carriedInventorySourceV3{file: oldEvidence}, target, 1)
	if err != nil {
		t.Fatalf("carry: %v", err)
	}
	if !matched || !exact || ordinal != 0 {
		t.Fatalf("carry = ordinal %d exact %v matched %v, want 0/true/true", ordinal, exact, matched)
	}

	request := playback.StartRequestV3{SubtitleTrackIndex: new(1), SubtitleTrackID: playback.TrackIDV3(oldEvidence.ID, "subtitle", 1)}
	from, to, exact, matched, attempted, err := handler.reconcileRotationSubtitleSelectionV3(
		context.Background(), nil, carriedInventorySourceV3{file: oldEvidence}, target, &request)
	if err != nil || !attempted || !matched || !exact || from != 1 || to != 0 {
		t.Fatalf("reconcile = from %d to %d exact %v matched %v attempted %v err %v", from, to, exact, matched, attempted, err)
	}
	if request.SubtitleTrackIndex == nil || *request.SubtitleTrackIndex != 0 {
		t.Fatalf("request index = %v, want 0", request.SubtitleTrackIndex)
	}
	if want := playback.TrackIDV3(target.ID, "subtitle", 0); request.SubtitleTrackID != want {
		t.Fatalf("request id = %q, want %q", request.SubtitleTrackID, want)
	}

	// An exact carry is silent: no degradation warning is added.
	plan := &playback.PlanV3{PlanID: "plan-exact", Delivery: playback.DeliveryOriginalHTTPV3}
	session := &playback.Session{
		ID:                            "exact-carry-session",
		MediaFileID:                   target.ID,
		VirtualSourceURI:              target.FilePath,
		VirtualSubtitleEvidenceURI:    target.FilePath,
		VirtualSubtitleEvidenceFileID: target.ID,
		VirtualSubtitleEvidenceSet:    true,
		VirtualSubtitleTracks:         target.SubtitleTracks,
	}
	ctx := withSubtitleRotationOutcomeV3(context.Background(), subtitleRotationOutcomeV3{
		Recorded: true, FromIndex: 1, ToIndex: 0, Matched: true, Exact: true,
		ToFileID: target.ID, ToCandidateURI: target.FilePath,
	})
	state := handler.v3SessionStreamState(ctx, session, target, playback.PlannerResultV3{Plan: plan, SubtitleTrackIndex: 0}, preparedTransportV3{}, mediaAuthModeV3{})
	if !state.VirtualSubtitleCarrySet || state.VirtualSubtitleCarryFromIndex != 1 || state.VirtualSubtitleCarryToIndex != 0 {
		t.Fatalf("committed exact carry = %+v, want 1->0", state)
	}
	if got := countSubtitleCarryWarningsV3(plan.DegradationWarnings); got != 0 {
		t.Fatalf("exact carry warnings = %d, want none", got)
	}
}

// A unique survivor whose codec spelling differs is a carry, not a miss, but it
// must surface exactly one additive subtitle_carry_warning.
func TestSubtitleCarryFuzzyAddsExactlyOneWarning(t *testing.T) {
	oldEvidence := &models.MediaFile{ID: 200, FilePath: "virtual://movie/tt?result=a",
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "subrip", Title: "English"}},
	}
	target := &models.MediaFile{ID: 300, FilePath: "virtual://movie/tt?result=b",
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "mov_text", Title: "English"}},
	}
	handler := &PlaybackHandler{}
	_, ordinal, exact, matched, err := handler.carrySubtitleSelectionAcrossRotationV3(
		context.Background(), nil, carriedInventorySourceV3{file: oldEvidence}, target, 0)
	if err != nil || !matched || exact || ordinal != 0 {
		t.Fatalf("fuzzy carry = ordinal %d exact %v matched %v err %v, want 0/false/true", ordinal, exact, matched, err)
	}

	file := target
	session := &playback.Session{
		ID:                               "carry-session",
		MediaFileID:                      file.ID,
		VirtualSourceOwnerInstallationID: 3,
		VirtualSourceURI:                 file.FilePath,
		VirtualSubtitleEvidenceURI:       file.FilePath,
		VirtualSubtitleEvidenceFileID:    file.ID,
		VirtualSubtitleEvidenceSet:       true,
		VirtualSubtitleTracks:            target.SubtitleTracks,
	}
	plan := &playback.PlanV3{PlanID: "plan-fuzzy", Delivery: playback.DeliveryOriginalHTTPV3}
	result := playback.PlannerResultV3{Plan: plan, SubtitleTrackIndex: 0}
	ctx := withSubtitleRotationOutcomeV3(context.Background(), subtitleRotationOutcomeV3{
		Recorded: true, FromIndex: 0, ToIndex: 0, Matched: true, Exact: false,
		ToFileID: file.ID, ToCandidateURI: file.FilePath,
	})
	state := handler.v3SessionStreamState(ctx, session, file, result, preparedTransportV3{}, mediaAuthModeV3{})
	if !state.VirtualSubtitleCarrySet || state.VirtualSubtitleCarryFromIndex != 0 || state.VirtualSubtitleCarryToIndex != 0 {
		t.Fatalf("committed carry = %+v, want 0->0 recorded", state)
	}
	warnings := countSubtitleCarryWarningsV3(plan.DegradationWarnings)
	if warnings != 1 {
		t.Fatalf("carry warnings = %d, want exactly 1: %+v", warnings, plan.DegradationWarnings)
	}
	// Re-committing the same state must not add a second warning.
	_ = handler.v3SessionStreamState(ctx, session, file, result, preparedTransportV3{}, mediaAuthModeV3{})
	if got := countSubtitleCarryWarningsV3(plan.DegradationWarnings); got != 1 {
		t.Fatalf("carry warnings after recommit = %d, want 1", got)
	}
}

func countSubtitleCarryWarningsV3(warnings []playback.DegradationWarningV3) int {
	count := 0
	for _, warning := range warnings {
		if warning.Code == playback.DegradationWarningSubtitleCarryV3 {
			count++
		}
	}
	return count
}

// The subtitle URL extension was minted at plan time, so a text selection may
// not be carried onto a bitmap-only rotated release even though both are the
// same language.
func TestSubtitleCarryMuxerClassGate(t *testing.T) {
	oldEvidence := &models.MediaFile{ID: 200, FilePath: "virtual://movie/tt?result=a",
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "subrip"}},
	}
	target := &models.MediaFile{ID: 300, FilePath: "virtual://movie/tt?result=b",
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "hdmv_pgs_subtitle"}},
	}
	handler := &PlaybackHandler{}
	_, _, _, matched, err := handler.carrySubtitleSelectionAcrossRotationV3(
		context.Background(), nil, carriedInventorySourceV3{file: oldEvidence}, target, 0)
	if err != nil {
		t.Fatalf("carry: %v", err)
	}
	if matched {
		t.Fatal("a WebVTT selection was carried onto a PGS-only release")
	}
}

// Language is a hard gate: an English selection never lands on the French track
// that happens to sit at the same ordinal.
func TestSubtitleCarryLanguageGate(t *testing.T) {
	oldEvidence := &models.MediaFile{ID: 200, FilePath: "virtual://movie/tt?result=a",
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "subrip"}},
	}
	target := &models.MediaFile{ID: 300, FilePath: "virtual://movie/tt?result=b",
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Language: "fr", Codec: "subrip"}},
	}
	handler := &PlaybackHandler{}
	_, _, _, matched, err := handler.carrySubtitleSelectionAcrossRotationV3(
		context.Background(), nil, carriedInventorySourceV3{file: oldEvidence}, target, 0)
	if err != nil {
		t.Fatalf("carry: %v", err)
	}
	if matched {
		t.Fatal("an English selection was carried onto the French track")
	}
}

// Forced and hearing-impaired are hard gates too.
func TestSubtitleCarryForcedAndHearingImpairedGates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wanted models.SubtitleTrack
		target models.SubtitleTrack
	}{
		{
			name:   "forced",
			wanted: models.SubtitleTrack{Index: 0, Language: "en", Codec: "subrip", Forced: true},
			target: models.SubtitleTrack{Index: 0, Language: "en", Codec: "subrip", Forced: false},
		},
		{
			name:   "hearing impaired",
			wanted: models.SubtitleTrack{Index: 0, Language: "en", Codec: "subrip", HearingImpaired: true},
			target: models.SubtitleTrack{Index: 0, Language: "en", Codec: "subrip", HearingImpaired: false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldEvidence := &models.MediaFile{ID: 200, FilePath: "virtual://movie/tt?result=a", SubtitleTracks: []models.SubtitleTrack{tc.wanted}}
			target := &models.MediaFile{ID: 300, FilePath: "virtual://movie/tt?result=b", SubtitleTracks: []models.SubtitleTrack{tc.target}}
			handler := &PlaybackHandler{}
			_, _, _, matched, err := handler.carrySubtitleSelectionAcrossRotationV3(
				context.Background(), nil, carriedInventorySourceV3{file: oldEvidence}, target, 0)
			if err != nil {
				t.Fatalf("carry: %v", err)
			}
			if matched {
				t.Fatalf("%s gate admitted the wrong track", tc.name)
			}
		})
	}
}

// Two tracks that agree on every hard gate and both tiebreaks cannot be told
// apart; the carry fails closed instead of serving whichever sorts first.
func TestSubtitleCarryAmbiguousTracksFailClosed(t *testing.T) {
	oldEvidence := &models.MediaFile{ID: 200, FilePath: "virtual://movie/tt?result=a",
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "subrip", Title: "English"}},
	}
	target := &models.MediaFile{ID: 300, FilePath: "virtual://movie/tt?result=b",
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Language: "en", Codec: "subrip", Title: "English"},
			{Index: 1, Language: "en", Codec: "subrip", Title: "English"},
		},
	}
	handler := &PlaybackHandler{}
	_, _, _, matched, err := handler.carrySubtitleSelectionAcrossRotationV3(
		context.Background(), nil, carriedInventorySourceV3{file: oldEvidence}, target, 0)
	if err != nil {
		t.Fatalf("carry: %v", err)
	}
	if matched {
		t.Fatal("an ambiguous pair of identical tracks was matched")
	}
}

// Downloaded subtitles are file-bound: the stable row id is the identity, the
// language/format pair is the only fallback, and a missing language is a miss.
func TestSubtitleCarryDownloadedIdentity(t *testing.T) {
	const keptID, otherID = 71, 72
	oldEvidence := &models.MediaFile{ID: 200, FilePath: "virtual://movie/tt?result=a",
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "subrip"}},
	}
	target := &models.MediaFile{ID: 300, FilePath: "virtual://movie/tt?result=b",
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "subrip"}},
	}
	carry := carriedInventorySourceV3{
		file:            oldEvidence,
		downloadedKnown: true,
		downloaded: []playback.SubtitleInventoryItemV3{{
			TrackID:       "file:200:subtitle:1",
			CombinedIndex: 1,
			Source:        playback.SubtitleSourceDownloadedV3,
			Language:      "eng",
			Codec:         "srt",
			Label:         "English",
			SyncKey:       subtitles.StoredSyncKey(keptID),
		}},
	}

	t.Run("row id moved to a later offset", func(t *testing.T) {
		handler := &PlaybackHandler{SubtitleRepo: downloadedSubtitleRepoByFile{byFile: map[int][]subtitles.DownloadedSubtitle{
			target.ID: {
				{ID: otherID, MediaFileID: target.ID, Language: "jpn", Format: subtitles.FormatSRT},
				{ID: keptID, MediaFileID: target.ID, Language: "eng", Format: subtitles.FormatSRT},
			},
		}}}
		_, ordinal, exact, matched, err := handler.carrySubtitleSelectionAcrossRotationV3(
			context.Background(), nil, carry, target, 1)
		if err != nil || !matched || !exact {
			t.Fatalf("row-id carry = ordinal %d exact %v matched %v err %v", ordinal, exact, matched, err)
		}
		// One embedded track precedes the downloaded tail, so the second
		// downloaded row is published at ordinal 2.
		if ordinal != 2 {
			t.Fatalf("row-id carry ordinal = %d, want 2", ordinal)
		}
	})

	t.Run("row id dropped but language and format remain", func(t *testing.T) {
		handler := &PlaybackHandler{SubtitleRepo: downloadedSubtitleRepoByFile{byFile: map[int][]subtitles.DownloadedSubtitle{
			target.ID: {
				{ID: otherID, MediaFileID: target.ID, Language: "eng", Format: subtitles.FormatSRT},
			},
		}}}
		_, ordinal, exact, matched, err := handler.carrySubtitleSelectionAcrossRotationV3(
			context.Background(), nil, carry, target, 1)
		if err != nil || !matched || exact {
			t.Fatalf("language/format carry = ordinal %d exact %v matched %v err %v, want fuzzy", ordinal, exact, matched, err)
		}
		if ordinal != 1 {
			t.Fatalf("language/format carry ordinal = %d, want 1", ordinal)
		}
	})

	t.Run("no english row", func(t *testing.T) {
		handler := &PlaybackHandler{SubtitleRepo: downloadedSubtitleRepoByFile{byFile: map[int][]subtitles.DownloadedSubtitle{
			target.ID: {
				{ID: otherID, MediaFileID: target.ID, Language: "jpn", Format: subtitles.FormatSRT},
			},
		}}}
		_, _, _, matched, err := handler.carrySubtitleSelectionAcrossRotationV3(
			context.Background(), nil, carry, target, 1)
		if err != nil {
			t.Fatalf("carry: %v", err)
		}
		if matched {
			t.Fatal("a downloaded selection with no English row on the target was matched")
		}
	})
}

// Captured-empty evidence is authoritative: there is no old track to carry, so
// ordinal 0 must never be reinterpreted against the replacement.
func TestSubtitleCarryCapturedEmptyEvidenceNeverReinterpretsOrdinalZero(t *testing.T) {
	emptyEvidence := &models.MediaFile{ID: 200, FilePath: "virtual://movie/tt?result=a"}
	target := &models.MediaFile{ID: 300, FilePath: "virtual://movie/tt?result=b",
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "subrip"}},
	}
	carry := carriedInventorySourceV3{file: emptyEvidence, downloadedKnown: true, downloaded: nil}
	handler := &PlaybackHandler{}
	index := 0
	request := playback.StartRequestV3{SubtitleTrackIndex: &index, SubtitleTrackID: playback.TrackIDV3(emptyEvidence.ID, "subtitle", 0)}
	_, _, _, matched, attempted, err := handler.reconcileRotationSubtitleSelectionV3(
		context.Background(), nil, carry, target, &request)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if attempted || matched {
		t.Fatalf("captured-empty evidence attempted=%v matched=%v, want no carry", attempted, matched)
	}
	if request.SubtitleTrackIndex == nil || *request.SubtitleTrackIndex != 0 ||
		request.SubtitleTrackID != playback.TrackIDV3(emptyEvidence.ID, "subtitle", 0) {
		t.Fatalf("captured-empty evidence rewrote the selection: index=%v id=%q", request.SubtitleTrackIndex, request.SubtitleTrackID)
	}
}

// A subtitle fetch still naming an ordinal the rotation could not carry must
// answer the retryable source-changed 409 on the first request, without
// resolving the dead candidate or spawning ffmpeg. The identical request earns
// the same answer the second time.
func TestSubtitleCarryLostAnswers409WithoutSpawn(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "spawned")
	ffmpeg := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	file := &models.MediaFile{ID: 42, FilePath: "virtual://movie/tt?result=a",
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Language: "en", Codec: "subrip"},
			{Index: 1, Language: "en", Codec: "subrip"},
		},
	}
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatal(err)
	}
	const lostIndex = 1
	if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
		VirtualSourceSet:              true,
		VirtualSourceURI:              file.FilePath,
		VirtualSubtitleCarryLostSet:   true,
		VirtualSubtitleCarryLostIndex: lostIndex,
		SubtitleTrackIndex:            -1,
	}); err != nil {
		t.Fatal(err)
	}
	handler := NewStreamHandler(manager, mapPlaybackFileResolver{files: map[int]*models.MediaFile{file.ID: file}})
	handler.PlaybackConfig = func() config.PlaybackConfig { return config.PlaybackConfig{FFmpegPath: ffmpeg} }
	resolveCalls := 0
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		resolveCalls++
		return ResolvedVirtualMedia{}, os.ErrDeadlineExceeded
	})

	request := func() *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.HandleSubtitle(response, playbackTestRequest(http.MethodGet,
			"/stream/"+session.ID+"/subtitles/1.vtt?file_id=42", nil,
			map[string]string{"session_id": session.ID, "track": "1.vtt"}))
		return response
	}
	started := time.Now()
	first := request()
	second := request()
	elapsed := time.Since(started)
	if first.Code != http.StatusConflict || second.Code != http.StatusConflict {
		t.Fatalf("statuses = %d/%d, want 409/409; bodies = %q / %q",
			first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	for index, response := range []*httptest.ResponseRecorder{first, second} {
		if got := response.Header().Get("Retry-After"); got == "" {
			t.Fatalf("response %d has no Retry-After", index+1)
		}
	}
	if resolveCalls != 0 {
		t.Fatalf("dead candidate resolve calls = %d, want 0", resolveCalls)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("ffmpeg was spawned for a terminal rotation ordinal")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("two terminal fetches took %s, want bounded", elapsed)
	}
}

// A subtitle request issued before a rotation but resolved after it names the old
// ordinal. The committed carry rewrites it to the new one; it must never serve
// the rotated release at the old position.
func TestSubtitleCarryRotationMidSubfetchMapsCommittedOrdinal(t *testing.T) {
	file := &models.MediaFile{ID: 42, FilePath: "/media/movie.mkv",
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Language: "en", Codec: "subrip"},
			{Index: 1, Language: "fr", Codec: "subrip"},
		},
	}
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
		VirtualSourceSet:              true,
		VirtualSourceURI:              "virtual://movie/tt?result=b",
		VirtualSubtitleCarrySet:       true,
		VirtualSubtitleCarryFromIndex: 1,
		VirtualSubtitleCarryToIndex:   0,
		SubtitleTrackIndex:            -1,
	}); err != nil {
		t.Fatal(err)
	}
	handler := NewStreamHandler(manager, mapPlaybackFileResolver{files: map[int]*models.MediaFile{file.ID: file}})
	// The URL still names the pre-rotation ordinal (1). It must resolve to 0,
	// the English track the rotation carried; ordinal 1 is French on this row.
	resolved, index, err := handler.resolveSubtitleSourceRequest(context.Background(), file, session, 1, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved == nil || index != 0 {
		t.Fatalf("resolved index = %d, want the carried 0", index)
	}
	if got := resolved.SubtitleTracks[index].Language; got != "en" {
		t.Fatalf("served language = %q at the carried ordinal, want en", got)
	}
}

// The rotation-gated re-key uses the strict release matcher: a text selection is
// not carried onto the bitmap track the generic edition-switch remap would have
// accepted. The non-rotation caller keeps the historical cross-format remap.
func TestConstrainRekeyedSubtitleRotationUsesStrictMatcher(t *testing.T) {
	source := &models.MediaFile{ID: 200, FilePath: "virtual://movie/tt?result=a",
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "subrip"}},
	}
	target := &models.MediaFile{ID: 300, FilePath: "virtual://movie/tt?result=b",
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "hdmv_pgs_subtitle"}},
	}
	carry := carriedInventorySourceV3{file: source}
	handler := &PlaybackHandler{}
	index := 0
	request := playback.StartRequestV3{SubtitleTrackIndex: &index, SubtitleTrackID: playback.TrackIDV3(target.ID, "subtitle", 0)}
	unavailable, err := handler.constrainRekeyedSubtitleSelectionWithRotationV3(context.Background(), &request, carry, target, true)
	if err != nil {
		t.Fatalf("rotation constrain: %v", err)
	}
	if !unavailable {
		t.Fatal("a WebVTT selection was carried onto a PGS-only rotation")
	}
	if request.SubtitleTrackIndex != nil || request.SubtitleTrackID != "" {
		t.Fatalf("missed rotation selection was kept: index=%v id=%q", request.SubtitleTrackIndex, request.SubtitleTrackID)
	}

	legacyIndex := 0
	legacy := playback.StartRequestV3{SubtitleTrackIndex: &legacyIndex, SubtitleTrackID: playback.TrackIDV3(target.ID, "subtitle", 0)}
	unavailable, err = handler.constrainRekeyedSubtitleSelectionV3(context.Background(), &legacy, carry, target)
	if err != nil {
		t.Fatalf("legacy constrain: %v", err)
	}
	if unavailable {
		t.Fatal("the non-rotation edition-switch remap changed behavior")
	}
}

// A request for a different ordinal is not rewritten by the committed carry.
func TestSubtitleCarryLeavesUncarriedOrdinalsAlone(t *testing.T) {
	file := &models.MediaFile{ID: 42, FilePath: "/media/movie.mkv",
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Language: "en", Codec: "subrip"},
			{Index: 1, Language: "fr", Codec: "subrip"},
			{Index: 2, Language: "de", Codec: "subrip"},
		},
	}
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
		VirtualSourceSet:              true,
		VirtualSourceURI:              "virtual://movie/tt?result=b",
		VirtualSubtitleCarrySet:       true,
		VirtualSubtitleCarryFromIndex: 1,
		VirtualSubtitleCarryToIndex:   0,
		SubtitleTrackIndex:            -1,
	}); err != nil {
		t.Fatal(err)
	}
	handler := NewStreamHandler(manager, mapPlaybackFileResolver{files: map[int]*models.MediaFile{file.ID: file}})
	_, index, err := handler.resolveSubtitleSourceRequest(context.Background(), file, session, 2, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if index != 2 {
		t.Fatalf("uncarried ordinal = %d, want 2", index)
	}
}
