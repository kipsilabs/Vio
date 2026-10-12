package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// noCandidateIdentity is the resolver identity for a test that does not model a
// resolver result. An empty identity proves nothing about the served bytes, so
// the release-continuity checks fail closed to the cross-release path exactly as
// they do when the resolver could not corroborate the row's stronger tier.
var noCandidateIdentity = resolver.PersistedIdentityTiers{}

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

// TestHandoffVirtualSessionToCachedRefusesEmptyResolvedCandidate proves the
// handoff guard rejects an empty resolved candidate explicitly, before carrying
// the pin, even when the resolver's other evidence looks like a same-release
// re-match. An empty candidate names no release at all, so neither
// IdentityRematched nor a durable-identity match may be read as proof that the
// session can stay on its pin and adopt the cached bytes.
func TestHandoffVirtualSessionToCachedRefusesEmptyResolvedCandidate(t *testing.T) {
	cases := []struct {
		name     string
		resolved ResolvedVirtualMedia
	}{
		{
			name: "identity rematched",
			resolved: ResolvedVirtualMedia{
				URL:               "http://127.0.0.1:9/cached",
				CandidateID:       "",
				OwnerID:           5,
				IdentityRematched: true,
			},
		},
		{
			name: "durable identity match",
			resolved: ResolvedVirtualMedia{
				URL:                 "http://127.0.0.1:9/cached",
				CandidateID:         "",
				OwnerID:             5,
				ProviderReleaseName: "Movie.2024.1080p.WEB-DL",
				ProviderReleaseSize: 8_000_000_000,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, _, session, file := newCacheHandoffFixture(t, "Movie.2024.1080p.WEB-DL")
			file.ProviderReleaseSize = 8_000_000_000
			handler.VirtualReleaseCacheStatus = func(context.Context, string, int) (bool, bool) { return true, true }
			handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
				return tc.resolved, nil
			})

			if _, _, applied, err := handler.handoffVirtualSessionToCached(context.Background(), session, file, false); err == nil || applied {
				t.Fatalf("empty-candidate handoff applied=%v err=%v, want refused", applied, err)
			}
			current, _ := handler.sessionMgr.GetSession(session.ID)
			if current.VirtualSourceURI != file.FilePath {
				t.Fatalf("binding moved to %q on a refused empty-candidate handoff", current.VirtualSourceURI)
			}
		})
	}
}

// TestVirtualProbeEvidenceChurnedListingKeepsPinnedRow is the evidence half of
// the burn-in incident: the PGS burn-in probe resolved the pinned release under
// a fresh listing token, and the CAS-fenced cross-release adoption matched no
// row. The resolver reported the same durable identity under the new token, so
// the candidate is the pinned row's release and must be written metadata-only
// against the row's pinned path instead of requiring an adoption of the churned
// token. The identity is the resolver's result, not the probed transient: the
// probed file here carries no identity at all, so a check that read it would have
// refused this write.
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
	// The probed transient deliberately carries no identity: only the resolver's
	// result may prove the release, so this test fails if the classification
	// reads the served file instead.
	probed := &models.MediaFile{
		VideoTracks: []models.VideoTrack{{Codec: "hevc", Width: 1920, Height: 1080}},
	}
	resolved := virtualProbeIdentity{
		ReleaseName: "Movie.2024.1080p.WEB-DL",
		ReleaseSize: 8_000_000_000,
	}

	args, ok := (&PlaybackHandler{}).virtualProbeEvidenceArgsForRow(context.Background(), row, churned, probed, resolved.tiers(), true)
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
// direction is unchanged: a resolver result that shares no durable identity tier
// with the row is a different release and keeps the fenced cross-release
// adoption, so its evidence rotates onto the candidate path rather than being
// folded onto the pinned row. The release is never swapped silently.
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
		VideoTracks: []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}},
	}
	resolved := virtualProbeIdentity{
		ReleaseName: "Another.Release.2020",
		ReleaseSize: 4_000_000_000,
	}

	args, ok := (&PlaybackHandler{}).virtualProbeEvidenceArgsForRow(context.Background(), row, sibling, probed, resolved.tiers(), true)
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

// TestVirtualProbeEvidenceForeignSubstitutionStaysFenced is the substitution
// direction of the identity fix. The row carries a populated durable identity
// and the resolver's result cannot corroborate a stronger tier the row holds:
// the resolved hash is missing (a listing that stopped declaring it) or conflicts
// (different bytes under the same release name). Under the asymmetric
// SharedTier the row is authoritative, so a missing stronger tier is a veto, not
// a neutral, and a size-only claim on the name tier never matches. Each case must
// stay on the fenced cross-release path; if the classification read the
// inherited transient it would echo the row's own hash and silently drop the
// fence.
func TestVirtualProbeEvidenceForeignSubstitutionStaysFenced(t *testing.T) {
	const (
		neutral   = "virtual://movie/tt-foreign-substitution"
		pinned    = neutral + "?result=pinned"
		candidate = neutral + "?result=substitute"
	)
	row := &models.MediaFile{
		ID:                         21,
		ContentID:                  "movie-foreign-substitution",
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		MediaFolderID:              13,
		ProviderVideoHash:          "row-hash",
		ProviderReleaseName:        "Movie.2024.1080p.WEB-DL",
		ProviderReleaseSize:        8_000_000_000,
	}
	probed := &models.MediaFile{
		VideoTracks: []models.VideoTrack{{Codec: "hevc", Width: 1920, Height: 1080}},
	}

	cases := []struct {
		name     string
		resolved virtualProbeIdentity
	}{
		{
			// The listing cannot corroborate the row's stronger hash: the same
			// release name is not proof, so it must not count neutral.
			name: "missing stronger tier is a veto",
			resolved: virtualProbeIdentity{
				ReleaseName: "Movie.2024.1080p.WEB-DL",
				ReleaseSize: 8_000_000_000,
			},
		},
		{
			// A different hash is proof of different bytes even under the same
			// release name.
			name: "conflicting hash is a veto",
			resolved: virtualProbeIdentity{
				VideoHash:   "other-hash",
				ReleaseName: "Movie.2024.1080p.WEB-DL",
				ReleaseSize: 8_000_000_000,
			},
		},
		{
			// Size alone is never a release match: the name tier requires both
			// sides to carry the same normalized name.
			name: "size-only never matches",
			resolved: virtualProbeIdentity{
				ReleaseSize: 8_000_000_000,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, ok := (&PlaybackHandler{}).virtualProbeEvidenceArgsForRow(context.Background(), row, candidate, probed, tc.resolved.tiers(), true)
			if !ok {
				t.Fatal("a foreign substitution must stay adoptable (fenced), not be dropped")
			}
			if !args.RequireAdopt {
				t.Fatalf("foreign substitution was not fenced on adoption: %+v", args)
			}
			if args.AdoptPath != candidate {
				t.Fatalf("AdoptPath = %q, want the foreign candidate %q", args.AdoptPath, candidate)
			}
			if args.ExpectedFilePath != pinned {
				t.Fatalf("ExpectedFilePath = %q, want the pinned path %q", args.ExpectedFilePath, pinned)
			}
		})
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

// TestForeignSubstitutionProbeWriteStaysFenced is the SQL half of the
// foreign-substitution probe test. The row carries a populated durable identity
// and the resolver's result cannot corroborate a stronger tier it holds, so the
// evidence is classified cross-release and the write carries RequireAdopt. The
// SQL then proves the fence holds: a sibling row that owns the substitute path
// makes the required adoption match no row, and the original row is left
// byte-identical. A regression that classified the write from the inherited
// transient would call it metadata-only, land the substitute's inventory on the
// pinned row under the row's own identity, and change the row bytes.
func TestForeignSubstitutionProbeWriteStaysFenced(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994353, 7043
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	const (
		neutral       = "virtual://movie/tt-db-foreign-sub"
		pinnedPath    = neutral + "?result=pinned"
		candidatePath = neutral + "?result=substitute"
	)
	// A sibling row already owns the substitute's concrete path, so a required
	// adoption onto it can never land.
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source, video_tracks, resolution)
		VALUES('movie-db-foreign-sub', $1, $2, 'virtual', $3, 'virtual',
			'[{"codec":"h264","width":1920,"height":1080}]'::jsonb, '1080p')`,
		folderID, candidatePath, ownerID); err != nil {
		t.Fatalf("insert sibling row: %v", err)
	}
	var rowID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source,
			video_tracks, resolution, provider_video_hash, provider_release_name, provider_release_size)
		VALUES('movie-db-foreign-sub', $1, $2, 'virtual', $3, 'virtual',
			'[{"codec":"h264","width":1920,"height":1080}]'::jsonb, '1080p', 'row-hash', 'Movie.2024.1080p.WEB-DL', 8000000000)
		RETURNING id, updated_at, probe_updated_at`, folderID, pinnedPath, ownerID,
	).Scan(&rowID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert pinned row: %v", err)
	}
	row := &models.MediaFile{
		ID: rowID, ContentID: "movie-db-foreign-sub", FilePath: pinnedPath,
		MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID, ProbeSource: "virtual",
		ProviderVideoHash: "row-hash", ProviderReleaseName: "Movie.2024.1080p.WEB-DL", ProviderReleaseSize: 8_000_000_000,
		UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
	}
	probed := &models.MediaFile{VideoTracks: []models.VideoTrack{{Codec: "hevc", Width: 1920, Height: 1080}}}
	before := readVirtualEvidenceRowJSON(t, pool, rowID)

	cases := []struct {
		name     string
		resolved virtualProbeIdentity
	}{
		// The listing stopped declaring the row's stronger hash: the shared name
		// is not proof, so the substitution stays fenced.
		{"missing stronger tier", virtualProbeIdentity{ReleaseName: "Movie.2024.1080p.WEB-DL", ReleaseSize: 8_000_000_000}},
		// Different bytes under the same release name: the conflicting hash is
		// proof of a different release.
		{"conflicting hash", virtualProbeIdentity{VideoHash: "other-hash", ReleaseName: "Movie.2024.1080p.WEB-DL", ReleaseSize: 8_000_000_000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, ok := (&PlaybackHandler{}).virtualProbeEvidenceArgsForRow(ctx, row, candidatePath, probed, tc.resolved.tiers(), true)
			if !ok {
				t.Fatal("foreign substitution was dropped instead of fenced")
			}
			if !args.RequireAdopt {
				t.Fatalf("foreign substitution was not fenced: %+v", args)
			}
			if args.AdoptPath != candidatePath {
				t.Fatalf("AdoptPath = %q, want the substitute %q", args.AdoptPath, candidatePath)
			}
			result, err := ExecVirtualFileMetadataUpdateResult(ctx, pool, args)
			if !errors.Is(err, errVirtualAdoptIdentityNotPersisted) {
				t.Fatalf("error = %v, want errVirtualAdoptIdentityNotPersisted", err)
			}
			if result.RowsAffected != 0 || result.MetadataUpdated || result.IdentityAdopted {
				t.Fatalf("result = %+v, want a fully refused write", result)
			}
			if after := readVirtualEvidenceRowJSON(t, pool, rowID); after != before {
				t.Fatalf("pinned row changed on a fenced foreign-substitution write:\nbefore=%s\nafter =%s", before, after)
			}
		})
	}
}
