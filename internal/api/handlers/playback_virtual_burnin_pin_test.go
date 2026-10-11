package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

// TestHandoffVirtualSessionToCachedKeepsPinOnChurnedListing is the burn-in
// incident: a session is pinned to candidate A of a release (here the confirmed
// release whose cached copy is ready), and the provider's fresh listing
// renumbers its per-listing result ids so the re-resolve names candidate B for
// bytes that never changed. Before the fix the handoff refused the churned token
// as an unverifiable release swap (handoff_failed) even though the row's durable
// identity proves it is the same release. The handoff must carry the session pin
// forward: it applies, and the session stays bound to candidate A.
func TestHandoffVirtualSessionToCachedKeepsPinOnChurnedListing(t *testing.T) {
	handler, _, session, file := newCacheHandoffFixture(t, "Movie.2024.1080p.WEB-DL")
	file.ProviderReleaseSize = 8_000_000_000
	handler.VirtualReleaseCacheStatus = func(context.Context, string, int) (bool, bool) { return true, true }
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, forceRefresh bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		if !forceRefresh {
			t.Error("cache handoff must list afresh")
		}
		return ResolvedVirtualMedia{
			URL:                 "http://127.0.0.1:9/cached",
			URI:                 "virtual://movie/tt1?result=churned",
			CandidateID:         "churned",
			OwnerID:             5,
			ProviderReleaseName: file.ProviderReleaseName,
			ProviderReleaseSize: file.ProviderReleaseSize,
		}, nil
	})

	_, cleanup, applied, err := handler.handoffVirtualSessionToCached(context.Background(), session, file, false)
	if err != nil {
		t.Fatalf("handoff refused a churned same-release listing: %v", err)
	}
	if !applied {
		t.Fatal("handoff did not apply for a churned same-release listing")
	}
	if cleanup != nil {
		cleanup()
	}
	current, err := handler.sessionMgr.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if current.VirtualSourceURI != file.FilePath {
		t.Fatalf("binding = %q, want the carried pinned candidate %q", current.VirtualSourceURI, file.FilePath)
	}
}

// TestHandoffVirtualSessionToCachedRefusesForeignReleaseIdentity proves the
// carry-forward path never silently switches releases: a re-resolve that shares
// no durable identity tier with the pin is still refused, and the session
// binding stays on the pin.
func TestHandoffVirtualSessionToCachedRefusesForeignReleaseIdentity(t *testing.T) {
	handler, _, session, file := newCacheHandoffFixture(t, "Movie.2024.1080p.WEB-DL")
	file.ProviderReleaseSize = 8_000_000_000
	handler.VirtualReleaseCacheStatus = func(context.Context, string, int) (bool, bool) { return true, true }
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{
			URL:                 "http://127.0.0.1:9/other",
			URI:                 "virtual://movie/tt1?result=churned",
			CandidateID:         "churned",
			OwnerID:             5,
			ProviderReleaseName: "Another.Release.2020",
			ProviderReleaseSize: 4_000_000_000,
		}, nil
	})

	if _, _, applied, err := handler.handoffVirtualSessionToCached(context.Background(), session, file, false); err == nil || applied {
		t.Fatalf("foreign-release handoff applied=%v err=%v, want refused", applied, err)
	}
	current, _ := handler.sessionMgr.GetSession(session.ID)
	if current.VirtualSourceURI != file.FilePath {
		t.Fatalf("binding moved to %q on a refused foreign-release handoff", current.VirtualSourceURI)
	}
}

// TestVirtualProbeEvidenceChurnedListingKeepsPinnedRow is the evidence half of
// the burn-in incident: the PGS burn-in probe resolved the pinned release under
// a fresh listing token, and the CAS-fenced cross-release adoption matched no
// row. A candidate whose durable identity proves it is the pinned row's release
// is the row's own evidence, so it must be written metadata-only against the
// row's pinned path instead of requiring an adoption of the churned token.
func TestVirtualProbeEvidenceChurnedListingKeepsPinnedRow(t *testing.T) {
	const neutral = "virtual://movie/tt-burnin-evidence"
	pinned := neutral + "?result=pinned"
	churned := neutral + "?result=churned"
	row := &models.MediaFile{
		ID:                         7,
		ContentID:                  "movie-burnin-evidence",
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		MediaFolderID:              11,
		ProviderReleaseName:        "Movie.2024.1080p.WEB-DL",
		ProviderReleaseSize:        8_000_000_000,
	}
	probed := &models.MediaFile{
		ProviderReleaseName: "Movie.2024.1080p.WEB-DL",
		ProviderReleaseSize: 8_000_000_000,
		VideoTracks:         []models.VideoTrack{{Codec: "hevc", Width: 1920, Height: 1080}},
	}

	args, ok := (&PlaybackHandler{}).virtualProbeEvidenceArgsForRow(context.Background(), row, churned, probed, true)
	if !ok {
		t.Fatal("churned same-release evidence was refused instead of written against the pin")
	}
	if args.RequireAdopt {
		t.Fatalf("churned same-release evidence required a cross-release adoption: %+v", args)
	}
	if args.AdoptPath != "" {
		t.Fatalf("AdoptPath = %q, want empty so the pinned row keeps its path", args.AdoptPath)
	}
	if args.ExpectedFilePath != pinned {
		t.Fatalf("ExpectedFilePath = %q, want the pinned path %q", args.ExpectedFilePath, pinned)
	}
}

// TestVirtualProbeEvidenceForeignReleaseStillFenced proves the dead-pin
// direction is unchanged: a candidate that shares no durable identity tier with
// the row is a different release and keeps the fenced cross-release adoption, so
// its evidence rotates onto the candidate path rather than being folded onto the
// pinned row. The release is never swapped silently.
func TestVirtualProbeEvidenceForeignReleaseStillFenced(t *testing.T) {
	const neutral = "virtual://movie/tt-dead-pin-evidence"
	pinned := neutral + "?result=pinned"
	sibling := neutral + "?result=sibling"
	row := &models.MediaFile{
		ID:                         8,
		ContentID:                  "movie-dead-pin-evidence",
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		MediaFolderID:              12,
		ProviderReleaseName:        "Movie.2024.1080p.WEB-DL",
		ProviderReleaseSize:        8_000_000_000,
	}
	probed := &models.MediaFile{
		ProviderReleaseName: "Another.Release.2020",
		ProviderReleaseSize: 4_000_000_000,
		VideoTracks:         []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}},
	}

	args, ok := (&PlaybackHandler{}).virtualProbeEvidenceArgsForRow(context.Background(), row, sibling, probed, true)
	if !ok {
		t.Fatal("a foreign release must stay adoptable (rotates with evidence), not be dropped")
	}
	if !args.RequireAdopt {
		t.Fatalf("foreign-release evidence was not fenced on adoption: %+v", args)
	}
	if args.AdoptPath != sibling {
		t.Fatalf("AdoptPath = %q, want the foreign candidate %q", args.AdoptPath, sibling)
	}
}

// TestVirtualTranscodeStartUsesPinnedRelayForBurnIn proves the burn-in transcode
// starts against the session's pinned relay: a PGS burn-in transcode whose
// catalog row has drifted to a churned token still resolves the pinned candidate
// (never the drifted row path) and registers its relay as the transport input.
func TestVirtualTranscodeStartUsesPinnedRelayForBurnIn(t *testing.T) {
	tempDir := t.TempDir()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("pinned-media"))
	}))
	defer upstream.Close()

	neutral := "virtual://movie/tt-burnin-transcode"
	pinned := neutral + "?result=pinned"
	churned := neutral + "?result=churned"
	row := &models.MediaFile{
		ID:                         99,
		ContentID:                  "movie-burnin-transcode",
		FilePath:                   churned,
		VirtualOwnerInstallationID: 5,
		ProviderReleaseName:        "Movie.2024.1080p.WEB-DL",
	}

	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", row.ID, playback.PlayTranscode, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, pinned, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	var resolved []string
	h := &PlaybackHandler{
		fileResolver: &fakePinFileResolver{file: row},
		sessionMgr:   manager,
		RemoteStreamRelay: func() *remotestream.Relay {
			r := remotestream.NewRelay()
			t.Cleanup(func() { _ = r.Close(context.Background()) })
			return r
		}(),
		AllowPrivateStreams: func(int) bool { return true },
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			resolved = append(resolved, uri)
			return ResolvedVirtualMedia{URL: upstream.URL + "/provider/pinned.mp4", URI: uri, CandidateID: "pinned"}, nil
		}),
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			opts.OutputDir = tempDir
			return playback.NewReadyTranscodeSessionForTesting(tempDir, opts)
		},
	}
	opts := playback.TranscodeOpts{
		MediaFileID:                      row.ID,
		InputPath:                        churned,
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        session.ID,
		OutputDir:                        tempDir,
		SubtitleBurnIn:                   true,
		SubtitleTrackIndex:               0,
		SubtitleCodec:                    "pgs",
	}
	started, err := h.startLocalPlaybackTransportOnce(context.Background(), opts)
	if err != nil {
		t.Fatalf("startLocalPlaybackTransportOnce: %v", err)
	}
	defer func() { _ = started.Close() }()

	if len(resolved) == 0 || !strings.Contains(resolved[0], "result=pinned") {
		t.Fatalf("transcode resolved %v, want the pinned candidate", resolved)
	}
	if !strings.Contains(started.Opts().InputPath, "/source/") {
		t.Fatalf("transcode input = %q, want the pinned relay", started.Opts().InputPath)
	}
}
