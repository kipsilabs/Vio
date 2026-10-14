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
// committed selection is absent from the verified inventory, or the identity is
// ambiguous (duplicate signatures, or a same-transport track with a conflicting
// known title), the session must record a server-enforced invalid selection and
// the transport must answer a retryable 409 - never mux whichever track now
// occupies the old array position.
func TestDeferredProbeOverlayRemovedOrAmbiguousCommittedAudioFailsClosed(t *testing.T) {
	cases := []struct {
		name        string
		plan        []models.AudioTrack
		committed   int
		verified    []models.AudioTrack
		wantCarried int
	}{
		{
			name: "committed track removed by absent probed index",
			plan: []models.AudioTrack{
				{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
				{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
			},
			committed:   1,
			verified:    []models.AudioTrack{{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"}},
			wantCarried: 1,
		},
		{
			name: "removed commentary with retained main of identical transport facts",
			plan: []models.AudioTrack{
				{Index: 0, Language: "en", Codec: "eac3", Channels: 6, Layout: "5.1", Title: "Commentary"},
			},
			committed: 0,
			// Same language, codec, channels, and layout as the selected
			// commentary, but a conflicting known title: a different stream.
			verified:    []models.AudioTrack{{Index: 0, Language: "en", Codec: "eac3", Channels: 6, Layout: "5.1", Title: "Main"}},
			wantCarried: 1,
		},
		{
			name: "duplicate signatures are ambiguous",
			plan: []models.AudioTrack{
				{Index: 0, Language: "en", Codec: "eac3", Channels: 6, Layout: "5.1"},
			},
			committed:   0,
			verified:    []models.AudioTrack{{Index: 0, Language: "en", Codec: "eac3", Channels: 6, Layout: "5.1"}, {Index: 0, Language: "en", Codec: "eac3", Channels: 6, Layout: "5.1"}},
			wantCarried: 2,
		},
		{
			name: "duplicate-language distinct-title tracks are ambiguous",
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
		{
			name: "canonically-equal distinct language tags are ambiguous",
			plan: []models.AudioTrack{
				{Index: 0, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
			},
			committed: 0,
			// "en" and "eng" canonicalize to the same language, so the full
			// signature ties exactly; the raw-string fallback would admit
			// only "en" and silently hide the tie.
			verified: []models.AudioTrack{
				{Index: 0, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
				{Index: 0, Language: "eng", Codec: "aac", Channels: 2, Layout: "stereo"},
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

			// The real transport gate fails closed instead of muxing the wrong
			// stream.
			assertStreamAudioSelectionRejected(t, manager, resolver, session.ID, candidateURI)
		})
	}
}

// assertStreamAudioSelectionRejected drives the real HandleStream entry point
// and asserts the invalid audio selection answers a retryable 409 with the
// audio_selection_invalid code.
func assertStreamAudioSelectionRejected(t *testing.T, manager *playback.SessionManager, resolver FilePathResolver, sessionID, candidateURI string) {
	t.Helper()
	handler := NewStreamHandler(manager, resolver)
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.VirtualCandidateTrustWindow = func() time.Duration { return 720 * time.Hour }
	handler.PlaybackConfig = func() config.PlaybackConfig {
		return config.PlaybackConfig{FFmpegPath: "/does/not/matter"}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+sessionID, nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", sessionID)

	handler.HandleStream(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for an invalid audio selection (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "audio_selection_invalid") {
		t.Fatalf("body = %s, want the audio_selection_invalid problem code", rec.Body.String())
	}
}

// TestHandleStream_VirtualRemuxInvalidAudioSelectionFailsClosed drives the real
// HandleStream entry point through the overlay write directly: a virtual remux
// session whose committed audio selection vanished must answer a retryable 409
// and never spawn a remux that would mux whichever stream occupies the old array
// position.
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

	assertStreamAudioSelectionRejected(t, manager, testPlaybackFileResolver{file: file}, session.ID, pinned)
}

// TestVirtualAudioSelectionInvalidClearsOnClientRecovery is the recovery half:
// the retryable 409 is only useful if the client's next commit recovers. The
// v3 replan commits the replanned selection through UpdateAudioTrack (the same
// seam HandleReplanPlaybackV3 uses), which must clear the invalid-selection
// marker and leave a valid, in-range selection whose derived remux ordinal names
// a real stream.
func TestVirtualAudioSelectionInvalidClearsOnClientRecovery(t *testing.T) {
	candidateURI := "virtual://movie/tt-recovery?result=cand-1"
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 5273001, playback.PlayRemux, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, candidateURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	plan := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
	}
	if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
		AudioTrackIndex:               1, // pt
		VirtualSourceSet:              true,
		VirtualSourceOwnershipSet:     true,
		VirtualSourceURI:              candidateURI,
		VirtualSubtitleEvidenceSet:    true,
		VirtualAudioTracks:            plan,
		VirtualSubtitleEvidenceURI:    candidateURI,
		VirtualSubtitleEvidenceFileID: session.MediaFileID,
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}
	binding, err := manager.VirtualSourceBinding(session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceBinding: %v", err)
	}
	// The verified overlay drops pt, so the selection is invalid and the
	// transport gate would answer 409.
	verified := []models.AudioTrack{{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"}}
	if applied, err := manager.SetVirtualProbeVerifiedInventoryIfBinding(session.ID, binding, "rev", verified, nil, nil); err != nil || !applied {
		t.Fatalf("SetVirtualProbeVerifiedInventoryIfBinding = %v, %v; want applied", applied, err)
	}
	invalid, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !invalid.VirtualAudioSelectionInvalid || invalid.AudioTrackIndex != -1 {
		t.Fatalf("fixture not invalid: index=%d invalid=%v", invalid.AudioTrackIndex, invalid.VirtualAudioSelectionInvalid)
	}

	// The client's recovery replan commits a fresh, valid selection.
	if err := manager.UpdateAudioTrack(session.ID, 0, playback.PlayRemux); err != nil {
		t.Fatalf("UpdateAudioTrack: %v", err)
	}
	recovered, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession after recovery: %v", err)
	}
	if recovered.VirtualAudioSelectionInvalid {
		t.Fatal("a recovery commit did not clear the invalid-selection marker")
	}
	if recovered.AudioTrackIndex != 0 {
		t.Fatalf("recovered audio index = %d, want 0", recovered.AudioTrackIndex)
	}
	if got := playback.AudioStreamOrdinal(recovered.VirtualAudioTracks, recovered.AudioTrackIndex); got != 0 {
		t.Fatalf("recovered remux ordinal = %d, want 0 (the en stream)", got)
	}
}

// TestVirtualAudioSelectionInvalidClearsOnStreamStateCommit covers the other
// replan commit seam: a full route commit (applySessionStreamStateLocked, the
// v3SessionStreamState path) also clears the invalid-selection marker and
// installs the committed selection.
func TestVirtualAudioSelectionInvalidClearsOnStreamStateCommit(t *testing.T) {
	candidateURI := "virtual://movie/tt-recovery-state?result=cand-1"
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 5273002, playback.PlayRemux, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, candidateURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	plan := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
	}
	if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
		AudioTrackIndex:               1,
		VirtualSourceSet:              true,
		VirtualSourceOwnershipSet:     true,
		VirtualSourceURI:              candidateURI,
		VirtualSubtitleEvidenceSet:    true,
		VirtualAudioTracks:            plan,
		VirtualSubtitleEvidenceURI:    candidateURI,
		VirtualSubtitleEvidenceFileID: session.MediaFileID,
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
	if invalid, _ := manager.GetSession(session.ID); !invalid.VirtualAudioSelectionInvalid {
		t.Fatal("fixture not invalid")
	}

	// The full route commit (the v3 replan's stream-state apply) clears it.
	if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
		AudioTrackIndex:               0,
		VirtualSourceOwnershipSet:     true,
		VirtualSourceURI:              candidateURI,
		VirtualSubtitleEvidenceSet:    true,
		VirtualAudioTracks:            verified,
		VirtualSubtitleEvidenceURI:    candidateURI,
		VirtualSubtitleEvidenceFileID: session.MediaFileID,
	}); err != nil {
		t.Fatalf("UpdateStreamState recovery: %v", err)
	}
	recovered, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession after recovery: %v", err)
	}
	if recovered.VirtualAudioSelectionInvalid {
		t.Fatal("a route commit did not clear the invalid-selection marker")
	}
	if recovered.AudioTrackIndex != 0 {
		t.Fatalf("recovered audio index = %d, want 0", recovered.AudioTrackIndex)
	}
}
