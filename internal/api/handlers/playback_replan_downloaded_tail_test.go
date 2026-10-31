package handlers

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/subtitles"
)

// A carried downloaded-subtitle ordinal with no captured tail cannot be proven
// against the live repository list: the same row id may now publish a different
// downloaded order. The replan must clear the selection and report it
// unavailable instead of leaving the durable ordinal to be reused positionally.
func TestRekeyReplanClearsDownloadedSubtitleWithoutCapturedTail(t *testing.T) {
	file := &models.MediaFile{
		ID:        200,
		ContentID: "movie-1",
		Container: "virtual",
		FilePath:  "virtual://movie/tt?profile=hd&result=abc",
		// No embedded or external tracks, so ordinal 0 is a downloaded row.
	}
	handler := &PlaybackHandler{
		fileResolver: mapPlaybackFileResolver{files: map[int]*models.MediaFile{200: file}},
	}
	start := playback.StartRequestV3{
		FileID:          200,
		SubtitleTrackID: playback.TrackIDV3(200, "subtitle", 0),
	}

	_, subtitleUnavailable, err := handler.rekeyReplanTrackIdentitiesV3(context.Background(), &start, file, nil, nil, false)
	if err != nil {
		t.Fatalf("rekeyReplanTrackIdentitiesV3: %v", err)
	}
	if !subtitleUnavailable {
		t.Fatal("a downloaded selection without a captured tail must be reported unavailable")
	}
	if start.SubtitleTrackID != "" || start.SubtitleTrackIndex != nil {
		t.Fatalf("unprovable downloaded selection survived: id=%q index=%v", start.SubtitleTrackID, start.SubtitleTrackIndex)
	}
}

// The same input with a captured tail that still names the ordinal is
// provable and must keep carrying: the fix must not clear every downloaded
// selection.
func TestRekeyReplanKeepsDownloadedSubtitleWithCapturedTail(t *testing.T) {
	file := &models.MediaFile{
		ID:        200,
		ContentID: "movie-1",
		Container: "virtual",
		FilePath:  "virtual://movie/tt?profile=hd&result=abc",
	}
	repo := newMockSubtitleRepoForHandler()
	repo.list = []subtitles.DownloadedSubtitle{{
		ID:          41,
		MediaFileID: 200,
		Format:      subtitles.FormatSRT,
		Language:    "eng",
	}}
	handler := &PlaybackHandler{
		fileResolver: mapPlaybackFileResolver{files: map[int]*models.MediaFile{200: file}},
		SubtitleRepo: repo,
	}
	start := playback.StartRequestV3{
		FileID:          200,
		SubtitleTrackID: playback.TrackIDV3(200, "subtitle", 0),
	}
	durable := map[int]predecessorPlanInventoryV3{
		200: {
			subtitleKnown: true,
			subtitleItems: []playback.SubtitleInventoryItemV3{{
				Source:  "downloaded",
				Label:   "English",
				SyncKey: "downloaded:41",
			}},
		},
	}

	_, subtitleUnavailable, err := handler.rekeyReplanTrackIdentitiesV3(context.Background(), &start, file, nil, durable, false)
	if err != nil {
		t.Fatalf("rekeyReplanTrackIdentitiesV3: %v", err)
	}
	if subtitleUnavailable {
		t.Fatal("a downloaded selection with its captured tail must remain provable")
	}
}
