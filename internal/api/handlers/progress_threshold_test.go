package handlers

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

// thresholdCaptureStore records the resolved thresholds each progress write
// received, keyed by item.
type thresholdCaptureStore struct {
	userstore.UserStore
	byItem map[string]userstore.ProgressThresholds
}

func (s *thresholdCaptureStore) UpdateProgress(_ context.Context, _, item string, _, _ float64, thresholds userstore.ProgressThresholds) error {
	if s.byItem == nil {
		s.byItem = map[string]userstore.ProgressThresholds{}
	}
	s.byItem[item] = thresholds
	return nil
}

type thresholdStoreProvider struct {
	userstore.UserStoreProvider
	store userstore.UserStore
}

func (p *thresholdStoreProvider) ForUser(context.Context, int) (userstore.UserStore, error) {
	return p.store, nil
}

// fakeItemFolderLookup maps content IDs to a library folder.
type fakeItemFolderLookup struct {
	folders map[string]int
}

func (f *fakeItemFolderLookup) GetFolderIDForItem(_ context.Context, contentID string) (int, error) {
	return f.folders[contentID], nil
}

func (f *fakeItemFolderLookup) GetFolderIDsForItem(_ context.Context, contentID string) ([]int, error) {
	if folderID, ok := f.folders[contentID]; ok {
		return []int{folderID}, nil
	}
	return nil, nil
}

// fakeThresholdLookup answers stored per-library overrides and records which
// folders were asked about, so a test can prove caching and per-folder lookup.
type fakeThresholdLookup struct {
	byFolder map[int]userstore.LibraryProgressThresholdOverrides
	asked    []int
}

func (f *fakeThresholdLookup) ProgressThresholdOverrides(_ context.Context, folderID int) (userstore.LibraryProgressThresholdOverrides, error) {
	f.asked = append(f.asked, folderID)
	return f.byFolder[folderID], nil
}

func TestPlaybackThresholdsResolvePerLibrary(t *testing.T) {
	settings := testPlaybackSettingsRepo{values: map[string]string{
		"playback.watched_threshold":    "80",
		"playback.min_resume_threshold": "10",
	}}
	lookup := &fakeThresholdLookup{byFolder: map[int]userstore.LibraryProgressThresholdOverrides{
		7: {WatchedPct: intPtr(70), MinResumePct: intPtr(20)},
		8: {WatchedPct: intPtr(50), MinResumePct: intPtr(60)}, // inconsistent: resume falls back
	}}
	h := &PlaybackHandler{SettingsRepo: settings, ThresholdLookup: lookup}

	cases := []struct {
		name     string
		folderID int
		want     userstore.ProgressThresholds
	}{
		{"override wins", 7, userstore.ProgressThresholds{WatchedPct: 70, MinResumePct: 20}},
		{"inconsistent pair guarded", 8, userstore.ProgressThresholds{WatchedPct: 50, MinResumePct: 10}},
		{"no folder inherits global", 0, userstore.ProgressThresholds{WatchedPct: 80, MinResumePct: 10}},
		{"unknown folder inherits global", 99, userstore.ProgressThresholds{WatchedPct: 80, MinResumePct: 10}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := &models.MediaFile{ID: 1, MediaFolderID: tc.folderID}
			if got := h.playbackThresholds(t.Context(), file); got != tc.want {
				t.Fatalf("playbackThresholds(folder %d) = %+v, want %+v", tc.folderID, got, tc.want)
			}
		})
	}

	// A nil file and a nil lookup both resolve the server settings alone.
	if got := h.playbackThresholds(t.Context(), nil); got != (userstore.ProgressThresholds{WatchedPct: 80, MinResumePct: 10}) {
		t.Fatalf("nil file = %+v", got)
	}
	if got := (&PlaybackHandler{SettingsRepo: settings}).playbackThresholds(t.Context(), &models.MediaFile{MediaFolderID: 7}); got != (userstore.ProgressThresholds{WatchedPct: 80, MinResumePct: 10}) {
		t.Fatalf("nil lookup = %+v", got)
	}
}

func TestSyncProgressResolvesThresholdsPerItem(t *testing.T) {
	store := &thresholdCaptureStore{}
	h := NewProgressHandler(&thresholdStoreProvider{store: store})
	h.SettingsRepo = testPlaybackSettingsRepo{values: map[string]string{
		"playback.watched_threshold":    "80",
		"playback.min_resume_threshold": "10",
	}}
	h.FolderLookup = &fakeItemFolderLookup{folders: map[string]int{"a": 7, "b": 8}}
	lookup := &fakeThresholdLookup{byFolder: map[int]userstore.LibraryProgressThresholdOverrides{
		7: {WatchedPct: intPtr(70)},
		8: {MinResumePct: intPtr(25)},
	}}
	h.ThresholdLookup = lookup

	out, err := h.SyncProgress(t.Context(), 3, "child", []ProgressSyncUpdate{
		{MediaItemID: "a", Position: 30, Duration: 100},
		{MediaItemID: "b", Position: 30, Duration: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Status != "ok" || out[1].Status != "ok" {
		t.Fatalf("results = %+v", out)
	}
	want := map[string]userstore.ProgressThresholds{
		"a": {WatchedPct: 70, MinResumePct: 10},
		"b": {WatchedPct: 80, MinResumePct: 25},
	}
	for item, thresholds := range want {
		if got := store.byItem[item]; got != thresholds {
			t.Fatalf("%s thresholds = %+v, want %+v", item, got, thresholds)
		}
	}
	// Each folder is asked about once per batch, however many items it holds.
	if len(lookup.asked) != 2 {
		t.Fatalf("threshold lookups = %v, want one per folder", lookup.asked)
	}
}
