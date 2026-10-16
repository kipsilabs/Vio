package jellycompat

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

type thresholdFileResolver struct {
	file *models.MediaFile
}

func (r thresholdFileResolver) GetByID(context.Context, int) (*models.MediaFile, error) {
	return r.file, nil
}

type thresholdLookupStub struct {
	byFolder map[int]userstore.LibraryProgressThresholdOverrides
}

func (s thresholdLookupStub) ProgressThresholdOverrides(_ context.Context, folderID int) (userstore.LibraryProgressThresholdOverrides, error) {
	return s.byFolder[folderID], nil
}

func compatIntPtr(v int) *int { return &v }

func TestPlaybackThresholdsForSourceResolvesLibraryOverrides(t *testing.T) {
	settings := stubSettingsReader{values: map[string]string{
		"playback.watched_threshold":    "80",
		"playback.min_resume_threshold": "10",
	}}
	lookup := thresholdLookupStub{byFolder: map[int]userstore.LibraryProgressThresholdOverrides{
		7: {WatchedPct: compatIntPtr(70), MinResumePct: compatIntPtr(20)},
	}}
	h := &PlaybackHandler{SettingsRepo: settings, ThresholdLookup: lookup}

	// The source's file resolves to a library with overrides.
	h.fileResolver = thresholdFileResolver{file: &models.MediaFile{ID: 5, MediaFolderID: 7}}
	if got := h.playbackThresholdsForSource(t.Context(), &PlaybackMediaSource{FileID: 5}); got != (userstore.ProgressThresholds{WatchedPct: 70, MinResumePct: 20}) {
		t.Fatalf("overridden source = %+v, want 70/20", got)
	}

	// A file with no library inherits the server settings.
	h.fileResolver = thresholdFileResolver{file: &models.MediaFile{ID: 6}}
	if got := h.playbackThresholdsForSource(t.Context(), &PlaybackMediaSource{FileID: 6}); got != (userstore.ProgressThresholds{WatchedPct: 80, MinResumePct: 10}) {
		t.Fatalf("unmatched source = %+v, want 80/10", got)
	}

	// A nil source falls back to the server settings.
	if got := h.playbackThresholdsForSource(t.Context(), nil); got != (userstore.ProgressThresholds{WatchedPct: 80, MinResumePct: 10}) {
		t.Fatalf("nil source = %+v, want 80/10", got)
	}
}
