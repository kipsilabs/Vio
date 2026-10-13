package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestDeferredProbeOverlayRemovedOrAmbiguousCommittedAudioFailsClosed is the
// removed/ambiguous-audio regression for the real deferred-probe path: when the
// committed selection is absent from the verified inventory, or two verified
// tracks are indistinguishable by language, codec, channels, and layout, the
// session must record a server-enforced invalid selection whose derived remux
// ordinal is -1 - never a stale position that now names a different track.
func TestDeferredProbeOverlayRemovedOrAmbiguousCommittedAudioFailsClosed(t *testing.T) {
	cases := []struct {
		name        string
		plan        []models.AudioTrack
		committed   int
		verified    []models.AudioTrack
		wantCarried int
	}{
		{
			name: "committed track removed",
			plan: []models.AudioTrack{
				{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
				{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
			},
			committed:   1,
			verified:    []models.AudioTrack{{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"}},
			wantCarried: 1,
		},
		{
			name: "duplicate-language tracks are ambiguous",
			plan: []models.AudioTrack{
				{Index: 0, Language: "en", Codec: "eac3", Channels: 6, Layout: "5.1", Title: "Director"},
			},
			committed: 0,
			verified: []models.AudioTrack{
				{Index: 1, Language: "en", Codec: "eac3", Channels: 6, Layout: "5.1", Title: "Commentary"},
				{Index: 3, Language: "en", Codec: "eac3", Channels: 6, Layout: "5.1", Title: "Descriptive"},
			},
			wantCarried: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidateURI := "virtual://movie/tt-overlay-invalid?result=cand-1"
			manager := playback.NewSessionManager(0, 0)
			session, err := manager.StartSession(1, "profile-1", 5272999, playback.PlayRemux, false)
			if err != nil {
				t.Fatalf("StartSession: %v", err)
			}
			if err := manager.SetVirtualSource(session.ID, candidateURI, 5); err != nil {
				t.Fatalf("SetVirtualSource: %v", err)
			}
			if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
				AudioTrackIndex:               tc.committed,
				VirtualSourceSet:              true,
				VirtualSourceOwnershipSet:     true,
				VirtualSourceURI:              candidateURI,
				VirtualSubtitleEvidenceSet:    true,
				VirtualAudioTracks:            tc.plan,
				VirtualSubtitleEvidenceURI:    candidateURI,
				VirtualSubtitleEvidenceFileID: session.MediaFileID,
			}); err != nil {
				t.Fatalf("UpdateStreamState: %v", err)
			}

			// Drive the deferred probe through a handler wired to this manager.
			probedAt := time.Now().UTC()
			resolver := &syncPlaybackFileResolver{file: models.MediaFile{
				ID: session.MediaFileID, ContentID: "movie-overlay-invalid", FilePath: candidateURI,
				Container: "virtual", CodecVideo: "h264", Resolution: "1080p", Duration: 3600,
				VirtualOwnerInstallationID: 5,
				VideoTracks:                []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8}},
				AudioTracks:                tc.plan,
			}}
			handler := NewPlaybackHandler(manager, resolver)
			handler.InstallationID = "test-install"
			serviceCtx, serviceCancel := context.WithCancel(context.Background())
			handler.ServiceContext = serviceCtx
			t.Cleanup(serviceCancel)
			handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
				f.AudioTracks = tc.verified
				f.CodecAudio = "eac3"
				return f, nil
			}
			handler.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
				var audio []models.AudioTrack
				if err := json.Unmarshal(args.AudioTracks, &audio); err != nil {
					return VirtualFileMetadataUpdateResult{}, err
				}
				resolver.update(func(f *models.MediaFile) {
					f.AudioTracks = audio
					f.CodecAudio = args.CodecAudio
					f.ProbeUpdatedAt = &probedAt
				})
				return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
			}

			deferred := readmitDeferredProbe(t, handler, deferredOverflowProbe(session, candidateURI))
			handler.runDeferredVirtualProbe(deferred)

			live, err := manager.GetSession(session.ID)
			if err != nil {
				t.Fatalf("GetSession: %v", err)
			}
			if len(live.VirtualAudioTracks) != tc.wantCarried {
				t.Fatalf("carried audio = %+v, want the verified %d-track list", live.VirtualAudioTracks, tc.wantCarried)
			}
			if !live.VirtualAudioSelectionInvalid {
				t.Fatalf("committed selection index %d was not marked invalid; verified = %+v", live.AudioTrackIndex, live.VirtualAudioTracks)
			}
			if live.AudioTrackIndex != -1 {
				t.Fatalf("session audio index = %d, want the invalid sentinel -1", live.AudioTrackIndex)
			}
			// The production remux derivation must not name another stream.
			bound := bindSessionVirtualSourceWithTracks(context.Background(), &resolver.file, live, resolver)
			if got := audioStreamOrdinalV3(bound, live.AudioTrackIndex); got != -1 {
				t.Fatalf("remux ordinal after an invalid selection = %d, want -1 (no silent substitution)", got)
			}
		})
	}
}

// TestHandleStream_VirtualRemuxInvalidAudioSelectionFailsClosed drives the real
// HandleStream entry point: a virtual remux session whose committed audio
// selection vanished must answer a retryable 409 and never spawn a remux that
// would mux whichever stream occupies the old array position.
func TestHandleStream_VirtualRemuxInvalidAudioSelectionFailsClosed(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("video"))
	}))
	defer relay.Close()

	pinned := "virtual://movie/tt-invalid-selection?result=cand-a"
	delivered := time.Now().Add(-time.Minute)
	file := &models.MediaFile{
		ID:                         4310,
		ContentID:                  "movie-invalid-selection",
		FilePath:                   pinned,
		CodecVideo:                 "h264",
		VideoTracks:                []models.VideoTrack{{Codec: "h264"}},
		VirtualOwnerInstallationID: 5,
		ResolvedURL:                relay.URL + "/stream?token=stored",
		ResolvedURLExpiresAt:       new(time.Now().Add(2 * time.Hour)),
		LastDeliveredAt:            &delivered,
		UpdatedAt:                  time.Now(),
	}
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", file.ID, playback.PlayRemux, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, pinned, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	t.Cleanup(func() { _ = manager.StopSession(session.ID) })

	plan := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
	}
	if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
		AudioTrackIndex:               1, // pt
		VirtualSourceSet:              true,
		VirtualSourceOwnershipSet:     true,
		VirtualSourceURI:              pinned,
		VirtualSubtitleEvidenceSet:    true,
		VirtualAudioTracks:            plan,
		VirtualSubtitleEvidenceURI:    pinned,
		VirtualSubtitleEvidenceFileID: file.ID,
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}
	binding, err := manager.VirtualSourceBinding(session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceBinding: %v", err)
	}
	verified := []models.AudioTrack{{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"}}
	if applied, err := manager.SetVirtualProbeVerifiedInventoryIfBinding(session.ID, binding, "rev", verified, nil, nil); err != nil || !applied {
		t.Fatalf("SetVirtualProbeVerifiedInventoryIfBinding = %v, %v; want applied", applied, err)
	}
	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !live.VirtualAudioSelectionInvalid {
		t.Fatal("fixture did not mark the invalid selection")
	}

	handler := NewStreamHandler(manager, testPlaybackFileResolver{file: file})
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.VirtualCandidateTrustWindow = func() time.Duration { return 720 * time.Hour }
	handler.PlaybackConfig = func() config.PlaybackConfig {
		return config.PlaybackConfig{FFmpegPath: "/does/not/matter"}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)

	handler.HandleStream(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for an invalid audio selection (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "audio_selection_invalid") {
		t.Fatalf("body = %s, want the audio_selection_invalid problem code", rec.Body.String())
	}
}
