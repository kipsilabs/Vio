package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		{
			name: "committed selection absent from a verified-empty inventory",
			plan: []models.AudioTrack{
				{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
				{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
			},
			committed:   1,
			verified:    nil,
			wantCarried: 0,
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

// TestVirtualAudioSelectionInvalidRecoveryServesMedia closes the fail-closed
// chain at the transport, which the marker-only recovery tests never reach: a
// virtual remux whose committed audio selection is absent from the verified
// inventory answers the retryable 409, and once the client's recovery replan
// commits an acceptable replacement through the same stream-state apply the v3
// replan performs (replaceSession -> UpdateStreamState), the same transport
// serves media instead of the error. Two variants are covered. A removal with a
// surviving verified track recovers onto that track. A verified-empty inventory
// — the probe committed that the file carries no audio at all — recovers to
// video-only playback: the recovery commits exactly the verified empty inventory
// with no audio selection, and the transport serves the source without a
// fabricated track. The empty inventory is the only honest replacement, so the
// test asserts the derived audio-only ordinal is the no-audio sentinel rather
// than naming a stream the probe never saw.
func TestVirtualAudioSelectionInvalidRecoveryServesMedia(t *testing.T) {
	cases := []struct {
		name        string
		verified    []models.AudioTrack
		recovered   []models.AudioTrack
		wantIndex   int
		wantOrdinal int
	}{
		{
			name:        "removed committed track",
			verified:    []models.AudioTrack{{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"}},
			recovered:   []models.AudioTrack{{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"}},
			wantIndex:   0,
			wantOrdinal: 0,
		},
		{
			name:        "verified-empty inventory recovers to video-only",
			verified:    nil,
			recovered:   nil,
			wantIndex:   -1,
			wantOrdinal: -1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("media-bytes"))
			}))
			defer upstream.Close()

			pinned := "virtual://movie/tt-recovery-serve?result=cand-a"
			file := &models.MediaFile{
				ID:                         4312,
				ContentID:                  "movie-recovery-serve",
				FilePath:                   pinned,
				CodecVideo:                 "h264",
				VideoTracks:                []models.VideoTrack{{Codec: "h264"}},
				VirtualOwnerInstallationID: 5,
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
				PlayMethod:                    playback.PlayRemux,
				BasePlayMethod:                playback.PlayRemux,
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
			if applied, err := manager.SetVirtualProbeVerifiedInventoryIfBinding(session.ID, binding, "rev", tc.verified, nil, nil); err != nil || !applied {
				t.Fatalf("SetVirtualProbeVerifiedInventoryIfBinding = %v, %v; want applied", applied, err)
			}
			invalid, err := manager.GetSession(session.ID)
			if err != nil {
				t.Fatalf("GetSession: %v", err)
			}
			if !invalid.VirtualAudioSelectionInvalid || invalid.AudioTrackIndex != -1 {
				t.Fatalf("fixture not invalid: index=%d invalid=%v", invalid.AudioTrackIndex, invalid.VirtualAudioSelectionInvalid)
			}

			// The transport fails closed before serving anything.
			assertStreamAudioSelectionRejected(t, manager, testPlaybackFileResolver{file: file}, session.ID, pinned)

			// The client's recovery replan commits an acceptable replacement. The
			// v3 replan applies the fresh plan's stream state through this same
			// manager seam, which installs the selection and clears the marker.
			// The recovered inventory is the case's verified replacement, never
			// a track the probe did not report.
			if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
				PlayMethod:                    playback.PlayRemux,
				BasePlayMethod:                playback.PlayRemux,
				AudioTrackIndex:               tc.wantIndex,
				VirtualSourceOwnershipSet:     true,
				VirtualSourceURI:              pinned,
				VirtualSubtitleEvidenceSet:    true,
				VirtualAudioTracks:            tc.recovered,
				VirtualSubtitleEvidenceURI:    pinned,
				VirtualSubtitleEvidenceFileID: file.ID,
			}); err != nil {
				t.Fatalf("recovery commit: %v", err)
			}
			live, err := manager.GetSession(session.ID)
			if err != nil {
				t.Fatalf("GetSession after recovery: %v", err)
			}
			if live.VirtualAudioSelectionInvalid || live.AudioTrackIndex != tc.wantIndex {
				t.Fatalf("recovery did not install a valid selection: index=%d invalid=%v", live.AudioTrackIndex, live.VirtualAudioSelectionInvalid)
			}
			if len(live.VirtualAudioTracks) != len(tc.recovered) {
				t.Fatalf("recovery installed %d audio tracks, want the committed %d: %+v", len(live.VirtualAudioTracks), len(tc.recovered), live.VirtualAudioTracks)
			}
			// The transport derives its audio-only ordinal from this list. The
			// verified-empty recovery must name no audio stream: a fabricated
			// index would mux a track the probe said does not exist.
			if got := playback.AudioStreamOrdinal(live.VirtualAudioTracks, live.AudioTrackIndex); got != tc.wantOrdinal {
				t.Fatalf("recovered audio ordinal = %d, want %d", got, tc.wantOrdinal)
			}

			// The same transport now serves media instead of the 409.
			serveHandler := NewStreamHandler(manager, testPlaybackFileResolver{file: file})
			serveHandler.AllowPrivateStreams = func(int) bool { return true }
			serveHandler.VirtualCandidateTrustWindow = func() time.Duration { return 720 * time.Hour }
			ffmpeg := filepath.Join(t.TempDir(), "ffmpeg")
			if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nprintf served-media\n"), 0o700); err != nil {
				t.Fatalf("write fake ffmpeg: %v", err)
			}
			serveHandler.PlaybackConfig = func() config.PlaybackConfig {
				return config.PlaybackConfig{FFmpegPath: ffmpeg}
			}
			serveHandler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
				return ResolvedVirtualMedia{URL: upstream.URL, URI: pinned, CandidateID: "cand-a"}, nil
			})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil)
			req = req.WithContext(newAuthorizedPlaybackContext())
			req = withPlaybackRouteParam(req, "session_id", session.ID)
			serveHandler.HandleStream(rec, req)
			if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
				t.Fatalf("post-recovery delivery = %d %q, want 200 with media bytes", rec.Code, rec.Body.String())
			}
		})
	}
}

// virtualRemuxSelectionRecoveryFixture starts a real v3 virtual session on the
// remux route so the fail-closed audio gate applies: the client advertises HLS
// only, so the planner copies the H.264 video into HLS (PlayRemux) instead of
// taking the direct route the gate would skip. The catalog row already carries
// the concrete pinned candidate and probed evidence, so the start binds to that
// candidate with its plan-time inventory captured.
type virtualRemuxSelectionRecoveryFixture struct {
	handler   *PlaybackHandler
	manager   *playback.SessionManager
	source    *models.MediaFile
	start     playback.StartRequestV3
	started   playback.DecisionResponseV3
	pinnedURI string
}

func newVirtualRemuxSelectionRecoveryFixture(t *testing.T) *virtualRemuxSelectionRecoveryFixture {
	t.Helper()
	source := v3HandlerFixtureFile(t)
	source.ID = 741
	source.ContentID = "movie-selection-recovery"
	source.FilePath = "virtual://movie/tt-selection-recovery"
	source.VirtualOwnerInstallationID = 5
	source.AudioTracks = []models.AudioTrack{
		{Index: 0, Codec: "aac", Channels: 2, Layout: "stereo", Language: "eng", Default: true},
		{Index: 1, Codec: "aac", Channels: 2, Layout: "stereo", Language: "spa"},
	}
	probedAt := time.Now().Add(-time.Hour)
	source.ProbeUpdatedAt = &probedAt
	pinnedURI := withVirtualResultKey(source.FilePath, "pinned")
	source.FilePath = pinnedURI

	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager, mapPlaybackFileResolver{files: map[int]*models.MediaFile{source.ID: source}})
	stubCopySeekAnchorV3(handler)
	handler.PlaybackConfig = playbackTestConfig(writePlaybackTestFFmpeg(t), t.TempDir())
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{
		"allow_4k_transcode":                     "true",
		"playback.max_virtual_failover_attempts": "3",
	}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, virtualURI string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/stream?path=" + virtualURI, URI: virtualURI, CandidateID: virtualResultCandidateID(virtualURI)}, nil
	})
	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:9/stream?path=" + path, nil
	})
	handler.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "pinned", URI: pinnedURI,
			Resolution: source.Resolution, CodecVideo: source.CodecVideo, CodecAudio: source.CodecAudio, Container: source.Container,
		}}, nil
	})
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.VideoTracks = source.VideoTracks
		f.AudioTracks = source.AudioTracks
		f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = source.CodecVideo, source.CodecAudio, source.Resolution, source.Container
		return f, nil
	}

	start := v3HandlerStartRequest()
	start.FileID = source.ID
	start.QualityPreference = "auto"
	start.ClientPlaybackContext.Deliveries = map[string]playback.DeliveryCapabilityV3{
		playback.DeliveryClassHLSV3: {Enabled: true, SupportedOnDevice: true, Containers: []string{"hls"}, VideoCodecs: []string{"h264"}, AudioDecodeCodecs: []string{"aac"}},
	}
	fx := &virtualRemuxSelectionRecoveryFixture{
		handler: handler, manager: manager, source: source, start: start, pinnedURI: pinnedURI,
	}
	fx.started = startV3PlaybackForHandlerTest(t, handler, start)
	t.Cleanup(func() { _ = manager.StopSession(fx.started.SessionID) })
	return fx
}

// TestVirtualAudioSelectionInvalidRecoveryReplanCommitsValidTransport drives the
// whole fail-closed chain through the real replan entry point, which the
// seam-only recovery tests never reached: a real v3 virtual remux session whose
// committed audio selection is removed by the verified overlay answers the
// retryable 409, and the client's recovery replan runs through
// HandleReplanPlaybackV3 (durable attempt, lease, transport commit) to a valid,
// in-inventory selection whose derived remux ordinal names a real stream. It
// proves the recovery is production behavior, not an artifact of calling the
// session mutators directly.
func TestVirtualAudioSelectionInvalidRecoveryReplanCommitsValidTransport(t *testing.T) {
	fx := newVirtualRemuxSelectionRecoveryFixture(t)
	sessionID := fx.started.SessionID

	// The committed selection moves to the non-default Spanish track, then the
	// verified overlay reports it gone. remapAudioSelectionIndex cannot
	// re-identify it, so the session records the fail-closed sentinel.
	if err := fx.manager.UpdateAudioTrack(sessionID, 1, playback.PlayRemux); err != nil {
		t.Fatalf("UpdateAudioTrack: %v", err)
	}
	binding, err := fx.manager.VirtualSourceBinding(sessionID)
	if err != nil {
		t.Fatalf("VirtualSourceBinding: %v", err)
	}
	verified := []models.AudioTrack{{Index: 0, Codec: "aac", Channels: 2, Layout: "stereo", Language: "eng", Default: true}}
	if applied, err := fx.manager.SetVirtualProbeVerifiedInventoryIfBinding(sessionID, binding, "rev", verified, nil, nil); err != nil || !applied {
		t.Fatalf("SetVirtualProbeVerifiedInventoryIfBinding = %v, %v; want applied", applied, err)
	}
	invalid, err := fx.manager.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !invalid.VirtualAudioSelectionInvalid || invalid.AudioTrackIndex != -1 {
		t.Fatalf("fixture not invalid: index=%d invalid=%v", invalid.AudioTrackIndex, invalid.VirtualAudioSelectionInvalid)
	}
	if invalid.PlayMethod != playback.PlayRemux {
		t.Fatalf("session play method = %s, want PlayRemux so the audio gate applies", invalid.PlayMethod)
	}

	// The real transport fails closed before muxing the wrong stream.
	assertStreamAudioSelectionRejected(t, fx.manager, mapPlaybackFileResolver{files: map[int]*models.MediaFile{fx.source.ID: fx.source}}, sessionID, fx.pinnedURI)

	// The client's recovery replan selects the surviving English track.
	english := 0
	recovery := playback.ReplanRequestV3{
		ProtocolVersion:   playback.ProtocolV3,
		Operation:         playback.ReplanOperationTrackChangeV3,
		PlaybackAttemptID: fx.start.PlaybackAttemptID,
		ReplanRequestID:   "selection-recovery-0001",
		FailedPlanID:      fx.started.PlaybackPlan.PlanID,
		PlanAttemptID:     "selection-recovery-0001-plan",
		PlanAttemptKey:    fx.started.PlaybackPlan.PlanAttemptKey,
		AttemptCount:      1,
		PositionSeconds:   10,
		QualityPreference: "auto",
		SelectedTracks: playback.SelectedTracksV3{
			Audio: &playback.TrackIdentityV3{ID: playback.TrackIDV3(fx.source.ID, "audio", english), Index: &english},
		},
		Capabilities:          fx.start.Capabilities,
		ClientPlaybackContext: fx.start.ClientPlaybackContext,
	}
	response := postPlaybackReplanV3(t, fx.handler, sessionID, recovery)
	if response.Terminal != nil || response.PlaybackPlan == nil {
		t.Fatalf("recovery replan = terminal %#v plan %#v, want a committed plan", response.Terminal, response.PlaybackPlan)
	}
	if response.PlaybackPlan.SelectedTracks.Audio == nil || response.PlaybackPlan.SelectedTracks.Audio.Index == nil ||
		*response.PlaybackPlan.SelectedTracks.Audio.Index != english {
		t.Fatalf("recovery plan audio selection = %#v, want index %d", response.PlaybackPlan.SelectedTracks.Audio, english)
	}

	// The committed session carries the fresh selection and no marker, the
	// binding did not move, and the derived remux ordinal names the English
	// stream rather than the sentinel or the removed Spanish track.
	recovered, err := fx.manager.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession after recovery: %v", err)
	}
	if recovered.VirtualAudioSelectionInvalid || recovered.AudioTrackIndex != english {
		t.Fatalf("recovery did not commit a valid selection: index=%d invalid=%v", recovered.AudioTrackIndex, recovered.VirtualAudioSelectionInvalid)
	}
	if recovered.VirtualSourceURI != fx.pinnedURI {
		t.Fatalf("recovery moved the binding to %q, want the unchanged pin %q", recovered.VirtualSourceURI, fx.pinnedURI)
	}
	bound := bindSessionVirtualSourceWithTracks(context.Background(), fx.source, recovered, fx.handler.fileResolver)
	if got := audioStreamOrdinalV3(bound, recovered.AudioTrackIndex); got != english {
		t.Fatalf("recovered remux ordinal = %d, want %d (the English stream)", got, english)
	}
}
