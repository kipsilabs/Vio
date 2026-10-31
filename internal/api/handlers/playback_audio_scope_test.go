package handlers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/settingscontract"
	"github.com/Silo-Server/silo-server/internal/settingskeys"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

func seedPlaybackAudioLanguage(t *testing.T, store userstore.UserStore, contentID, language string) {
	t.Helper()
	encoded, err := json.Marshal(language)
	if err != nil {
		t.Fatalf("encoding audio language: %v", err)
	}
	if _, err := store.UpsertSettingValue(context.Background(), userstore.SettingIdentity{
		Key:       settingskeys.PlaybackAudioLanguage,
		Scope:     settingscontract.ScopeProfileSeries,
		ProfileID: "profile-1",
		SeriesID:  contentID,
	}, encoded); err != nil {
		t.Fatalf("seeding audio language: %v", err)
	}
}

// The detail picker scopes a content audio preference by the item's content id
// for a movie (see catalog effectiveAudioSelectionWith). The start path must
// resolve the same scope, or the marker names one track and the plan plays
// another. This is the playback half of the lock; the catalog half is
// TestEffectiveAudioTrackIndex_ScopesMovieByContentID.
func TestPreferredAudioTrackIndexScopesMovieByContentID(t *testing.T) {
	store := newPlaybackTestStore(t)
	seedPlaybackAudioLanguage(t, store, "movie-1", "fr")
	if err := store.SetAudioPreference(context.Background(), userstore.AudioPreference{
		ProfileID:       "profile-1",
		SeriesID:        "movie-1",
		AudioTrackIndex: 1,
		AudioLanguage:   "fr",
	}); err != nil {
		t.Fatalf("SetAudioPreference: %v", err)
	}
	handler := &PlaybackHandler{StoreProvider: testUserStoreProvider{store: store}}
	file := &models.MediaFile{
		ID:        42,
		ContentID: "movie-1",
		AudioTracks: []models.AudioTrack{
			{Index: 0, Language: "en", Codec: "aac", Channels: 2, Default: true},
			{Index: 1, Language: "fr", Codec: "aac", Channels: 2},
		},
	}

	index, _, err := handler.preferredAudioTrackIndexV3(context.Background(), 1, "profile-1", "device-1", file)
	if err != nil {
		t.Fatalf("preferredAudioTrackIndexV3: %v", err)
	}
	if index != 1 {
		t.Fatalf("preferred audio index = %d, want the content-scoped French track (1)", index)
	}
}

// MULTI/DUAL titles whose language lives only in the embedded title must be
// ranked the same way the catalog picker ranks them.
func TestPreferredAudioTrackIndexUsesBackfilledLanguages(t *testing.T) {
	store := newPlaybackTestStore(t)
	seedPlaybackAudioLanguage(t, store, "movie-2", "en")
	if err := store.SetAudioPreference(context.Background(), userstore.AudioPreference{
		ProfileID:       "profile-1",
		SeriesID:        "movie-2",
		AudioTrackIndex: 0,
		AudioLanguage:   "en",
	}); err != nil {
		t.Fatalf("SetAudioPreference: %v", err)
	}
	handler := &PlaybackHandler{StoreProvider: testUserStoreProvider{store: store}}
	file := &models.MediaFile{
		ID:        43,
		ContentID: "movie-2",
		AudioTracks: []models.AudioTrack{
			{Index: 0, Language: "und", EmbeddedTitle: "Japanese", Codec: "aac", Channels: 2, Default: true},
			{Index: 1, Language: "und", EmbeddedTitle: "English", Codec: "aac", Channels: 2},
		},
	}
	backfilled := playback.BackfillAudioTrackLanguages(append([]models.AudioTrack(nil), file.AudioTracks...))
	if backfilled[1].Language != "en" {
		t.Fatalf("backfilled language = %q, want en", backfilled[1].Language)
	}
	index, _, err := handler.preferredAudioTrackIndexV3(context.Background(), 1, "profile-1", "", file)
	if err != nil {
		t.Fatalf("preferredAudioTrackIndexV3: %v", err)
	}
	if index != 1 {
		t.Fatalf("preferred audio index = %d, want the English track identified from the embedded title", index)
	}
}
