package handlers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// The serve bind reads one carried list for both the transport map and the
// inventory endpoint. When the bound row is probe-stamped in place, the bind
// must keep serving that carried list: swapping in the row's tracks would
// publish a selection ordinal space the transport was never muxed against and
// silence a fast start. Verified truth reaches the session through the
// deferred-probe propagation below, not by rewriting the bind on every read.
func TestBindVirtualSourceKeepsOverlayWhenRowStampedAndSessionUpdatedElsewhere(t *testing.T) {
	uri := "virtual://movie/tt-freeze?result=cand-1"
	probedAt := time.Now().UTC()
	row := &models.MediaFile{
		ID:       5272900,
		FilePath: uri,
		AudioTracks: []models.AudioTrack{
			{Index: 1, Codec: "eac3", Channels: 6, Language: "eng", Default: true},
			{Index: 2, Codec: "aac", Channels: 2, Language: "eng"},
		},
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 4, Codec: "subrip", Language: "eng"},
			{Index: 7, Codec: "subrip", Language: "fra"},
		},
		ProbeUpdatedAt: &probedAt,
	}
	// The session's carried overlay was updated elsewhere (the deferred-probe
	// propagation) and now holds the verified 1 audio / 1 subtitle list the
	// transport is muxing against. The row's own tracks must not displace it.
	session := &playback.Session{
		ID:                            "sess-freeze",
		MediaFileID:                   row.ID,
		VirtualSourceURI:              uri,
		VirtualSubtitleEvidenceSet:    true,
		VirtualSubtitleEvidenceURI:    uri,
		VirtualSubtitleEvidenceFileID: row.ID,
		VirtualAudioTracks:            []models.AudioTrack{{Index: 1, Codec: "eac3", Channels: 6, Language: "jpn"}},
		VirtualSubtitleTracks:         []models.SubtitleTrack{{Index: 4, Codec: "subrip", Language: "jpn"}},
	}

	bound := bindSessionVirtualSourceWithTracks(context.Background(), row, session, nil)
	if len(bound.AudioTracks) != 1 || bound.AudioTracks[0].Language != "jpn" {
		t.Fatalf("bound audio = %+v, want the carried jpn overlay", bound.AudioTracks)
	}
	if len(bound.SubtitleTracks) != 1 || bound.SubtitleTracks[0].Language != "jpn" {
		t.Fatalf("bound subtitles = %+v, want the carried jpn overlay", bound.SubtitleTracks)
	}
}

// While the bound row is still unverified, the session's plan-time overlay stays
// authoritative: the declared menu the plan promised must still be served.
func TestBindVirtualSourceKeepsOverlayWhileRowUnverified(t *testing.T) {
	uri := "virtual://movie/tt-freeze-unverified?result=cand-1"
	row := &models.MediaFile{ID: 5272901, FilePath: uri}
	session := &playback.Session{
		ID:                            "sess-freeze-unverified",
		MediaFileID:                   row.ID,
		VirtualSourceURI:              uri,
		VirtualSubtitleEvidenceSet:    true,
		VirtualSubtitleEvidenceURI:    uri,
		VirtualSubtitleEvidenceFileID: row.ID,
		VirtualAudioTracks:            []models.AudioTrack{{Index: 1, Codec: "eac3", Channels: 6, Language: "jpn"}},
		VirtualSubtitleTracks:         []models.SubtitleTrack{{Index: 1, Codec: "subrip", Language: "fra"}},
	}

	bound := bindSessionVirtualSourceWithTracks(context.Background(), row, session, nil)
	if len(bound.AudioTracks) != 1 || bound.AudioTracks[0].Language != "jpn" {
		t.Fatalf("unverified overlay audio = %+v, want the carried jpn track", bound.AudioTracks)
	}
	if len(bound.SubtitleTracks) != 1 || bound.SubtitleTracks[0].Language != "fra" {
		t.Fatalf("unverified overlay subtitles = %+v, want the carried fra track", bound.SubtitleTracks)
	}
}

// The end-to-end acceptance: a deferred probe that commits the release's real
// tracks must reach the same session through its carried overlay, not by
// rewriting the serve bind to read the row directly. The session's own inventory
// then serves the verified 2-audio / N-subtitle list with a changed revision, and
// the bind still resolves to that same carried list so the transport map stays
// coherent. A durable write that does not commit (superseded or refused) must not
// empty the menu: the carried overlay is served with status failed.
func TestDeferredProbeCommitsViaSessionOverlayNotRowDirect(t *testing.T) {
	const fileID = 5272902
	const candidateURI = "virtual://movie/tt-freeze-commit?result=cand-1"
	planTime := []models.AudioTrack{{Index: 1, Codec: "eac3", Channels: 6, Language: "jpn"}}
	verifiedAudio := []models.AudioTrack{
		{Index: 1, Codec: "eac3", Channels: 6, Language: "jpn", Default: true},
		{Index: 2, Codec: "eac3", Channels: 6, Language: "eng"},
	}
	verifiedSubs := []models.SubtitleTrack{
		{Index: 4, Codec: "subrip", Language: "eng"},
		{Index: 7, Codec: "subrip", Language: "fra"},
		{Index: 9, Codec: "subrip", Language: "deu"},
	}
	provisionalAt := time.Now().UTC().Add(-time.Minute)

	type harness struct {
		manager  *playback.SessionManager
		session  *playback.Session
		resolver *syncPlaybackFileResolver
		handler  *PlaybackHandler
	}
	setup := func(t *testing.T) *harness {
		t.Helper()
		row := models.MediaFile{
			ID:                         fileID,
			ContentID:                  "movie-freeze-commit",
			FilePath:                   candidateURI,
			Container:                  "virtual",
			CodecVideo:                 "h264",
			Resolution:                 "1080p",
			Duration:                   3600,
			VirtualOwnerInstallationID: 5,
			VideoTracks:                []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8}},
			AudioTracks:                planTime,
			// The plan captured a provisional probe stamp, so the session keeps a
			// carried overlay (evidence-set) that the deferred probe must refresh.
			ProbeUpdatedAt: &provisionalAt,
		}
		manager := playback.NewSessionManager(0, 0)
		session, err := manager.StartSession(1, "profile-1", fileID, playback.PlayDirect, false)
		if err != nil {
			t.Fatalf("StartSession: %v", err)
		}
		if err := manager.SetVirtualSource(session.ID, candidateURI, 5); err != nil {
			t.Fatalf("SetVirtualSource: %v", err)
		}
		if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
			VirtualSourceURI:              candidateURI,
			VirtualSourceSet:              true,
			VirtualSourceOwnershipSet:     true,
			VirtualSourceRevision:         "planrev",
			VirtualSubtitleEvidenceSet:    true,
			VirtualAudioTracks:            planTime,
			VirtualSubtitleEvidenceURI:    candidateURI,
			VirtualSubtitleEvidenceFileID: fileID,
		}); err != nil {
			t.Fatalf("UpdateStreamState: %v", err)
		}
		resolver := &syncPlaybackFileResolver{file: row}
		handler := NewPlaybackHandler(manager, resolver)
		handler.InstallationID = "test-install"
		serviceCtx, serviceCancel := context.WithCancel(context.Background())
		handler.ServiceContext = serviceCtx
		t.Cleanup(serviceCancel)
		if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
			PlaybackAttemptID: "attempt-freeze-commit", SessionID: session.ID, UserID: 1, ProfileID: "profile-1",
			RequestedMediaFileID: fileID, EffectiveMediaFileID: fileID, ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("SaveAttempt: %v", err)
		}
		handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			f.AudioTracks = verifiedAudio
			f.SubtitleTracks = verifiedSubs
			f.CodecAudio = "eac3"
			return f, nil
		}
		return &harness{manager: manager, session: session, resolver: resolver, handler: handler}
	}
	caller := PlaybackCaller{UserID: 1, ProfileID: "profile-1", InstallationID: "test-install"}

	t.Run("commit propagates through the session overlay", func(t *testing.T) {
		fx := setup(t)
		probedAt := time.Now().UTC()
		fx.handler.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			var audio []models.AudioTrack
			if err := json.Unmarshal(args.AudioTracks, &audio); err != nil {
				return VirtualFileMetadataUpdateResult{}, err
			}
			var subs []models.SubtitleTrack
			if len(args.SubtitleTracks) > 0 {
				if err := json.Unmarshal(args.SubtitleTracks, &subs); err != nil {
					return VirtualFileMetadataUpdateResult{}, err
				}
			}
			fx.resolver.update(func(f *models.MediaFile) {
				f.AudioTracks = audio
				f.SubtitleTracks = subs
				f.CodecAudio = args.CodecAudio
				f.ProbeUpdatedAt = &probedAt
			})
			return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
		}

		before, err := fx.handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(), caller, fx.session.ID)
		if err != nil {
			t.Fatalf("GetPlaybackInventoryV2 before: %v", err)
		}
		if len(before.AudioTracks) != 1 || len(before.SubtitleInventory) != 0 {
			t.Fatalf("provisional inventory = %d audio / %d subtitles, want the captured 1/0 overlay", len(before.AudioTracks), len(before.SubtitleInventory))
		}

		deferred := readmitDeferredProbe(t, fx.handler, deferredOverflowProbe(fx.session, candidateURI))
		fx.handler.runDeferredVirtualProbe(deferred)

		after, err := fx.handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(), caller, fx.session.ID)
		if err != nil {
			t.Fatalf("GetPlaybackInventoryV2 after: %v", err)
		}
		if after.InventoryStatus != string(ProbeProvenanceVerified) {
			t.Fatalf("inventory status = %q, want verified", after.InventoryStatus)
		}
		if len(after.AudioTracks) != 2 {
			t.Fatalf("verified poll audio = %+v, want the verified 2 tracks", after.AudioTracks)
		}
		if len(after.SubtitleInventory) != 3 {
			t.Fatalf("verified poll subtitles = %+v, want the verified 3 tracks", after.SubtitleInventory)
		}
		if after.InventoryRevision == before.InventoryRevision {
			t.Fatalf("inventory revision did not change when the verified tracks landed: %q", after.InventoryRevision)
		}

		// The verified truth landed on the session itself. The bind both the
		// transport and the inventory read must resolve to that same carried
		// list, never the row's tracks taken directly.
		live, err := fx.manager.GetSession(fx.session.ID)
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if len(live.VirtualAudioTracks) != 2 {
			t.Fatalf("session carried audio = %+v, want the propagated verified 2 tracks", live.VirtualAudioTracks)
		}
		row, err := fx.resolver.GetByID(context.Background(), fileID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		bound := bindSessionVirtualSourceWithTracks(context.Background(), row, live, fx.resolver)
		if len(bound.AudioTracks) != len(live.VirtualAudioTracks) || bound.AudioTracks[0].Index != live.VirtualAudioTracks[0].Index {
			t.Fatalf("serve bind audio = %+v, want the session's propagated overlay %+v", bound.AudioTracks, live.VirtualAudioTracks)
		}
	})

	t.Run("superseded write keeps the carried overlay and fails", func(t *testing.T) {
		fx := setup(t)
		// The durable write is superseded/refused: the saver reports no commit,
		// and the refused evidence leaves the row unstamped with no tracks. The
		// session must still serve its own carried inventory with status failed,
		// never an empty menu.
		fx.handler.VirtualFileMetadataSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			fx.resolver.update(func(f *models.MediaFile) {
				f.AudioTracks = nil
				f.SubtitleTracks = nil
				f.ProbeUpdatedAt = nil
			})
			return VirtualFileMetadataUpdateResult{MetadataUpdated: false, RowsAffected: 0}, nil
		}

		deferred := readmitDeferredProbe(t, fx.handler, deferredOverflowProbe(fx.session, candidateURI))
		fx.handler.runDeferredVirtualProbe(deferred)

		after, err := fx.handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(), caller, fx.session.ID)
		if err != nil {
			t.Fatalf("GetPlaybackInventoryV2 after: %v", err)
		}
		if after.InventoryStatus != string(ProbeProvenanceFailed) {
			t.Fatalf("inventory status = %q, want failed", after.InventoryStatus)
		}
		if len(after.AudioTracks) != 1 || after.AudioTracks[0].Language != "jpn" {
			t.Fatalf("polled audio after a superseded write = %+v, want the carried jpn track", after.AudioTracks)
		}
		raw, err := json.Marshal(after)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		if got := string(decoded["audio_tracks"]); got == "null" {
			t.Fatal("inventory serialized audio_tracks null after a superseded write")
		}
	})
}
