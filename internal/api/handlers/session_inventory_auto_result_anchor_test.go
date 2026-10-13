package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// The auto-selection result-row anchor: an auto start whose requested row is
// the release's provider-neutral collection row, and whose resolver picks a
// concrete ?result= candidate that another catalog row owns. Explicitly picking
// that concrete row serves it directly; an auto start that resolves the same
// candidate must land on the same row, so the plan's effective identity, the
// session's evidence anchor, and the deferred probe's durable write all name the
// concrete result row. Otherwise the probe stamps the neutral sibling, the
// generation-fenced overlay propagation refuses it as a different release, and
// the menus stay frozen on the provisional plan-time inventory for the session's
// whole life while the transport streams the concrete candidate.
const (
	autoCollectionRowID = 4912228
	autoResultRowID     = 5316823
	autoCollectionURI   = "virtual://movie/tt-auto/2/10?episode=10&season=2"
	autoResultURI       = autoCollectionURI + "&result=8f847c6"
)

func autoResultIdentity() virtualProbeIdentity {
	return virtualProbeIdentity{
		VideoHash:   "hash-auto-result",
		ReleaseName: "Movie.Auto.2024.2160p.WEB-DL",
		ReleaseSize: 9_000_000_000,
	}
}

// autoResultRows builds the neutral collection row the auto start requested and
// the concrete result-pinned sibling the resolver selects. The collection row
// carries no durable identity, so the release can only be proven by the result
// row's own identity.
func autoResultRows() (models.MediaFile, models.MediaFile) {
	identity := autoResultIdentity()
	collection := models.MediaFile{
		ID:                         autoCollectionRowID,
		ContentID:                  "movie-tt-auto",
		FilePath:                   autoCollectionURI,
		Container:                  "virtual",
		ProbeSource:                virtualCollectionProbeSource,
		VirtualOwnerInstallationID: 5,
		MediaFolderID:              3,
		AudioTracks:                []models.AudioTrack{{Index: 0, Codec: "eac3", Channels: 6, Language: "eng", Default: true}},
	}
	result := models.MediaFile{
		ID:                         autoResultRowID,
		ContentID:                  "movie-tt-auto",
		FilePath:                   autoResultURI,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
		MediaFolderID:              3,
		ProviderVideoHash:          identity.VideoHash,
		ProviderReleaseName:        identity.ReleaseName,
		ProviderReleaseSize:        identity.ReleaseSize,
		AudioTracks:                []models.AudioTrack{{Index: 0, Codec: "eac3", Channels: 6, Language: "eng", Default: true}},
	}
	return collection, result
}

type autoResultFixture struct {
	handler       *PlaybackHandler
	manager       *playback.SessionManager
	session       *playback.Session
	resolver      *canonicalCollectionResolver
	resolved      resolvedVirtualPlaybackSource
	planAudio     []models.AudioTrack
	committedRows []int
}

// setupAutoResultFixture runs the real auto resolve on the neutral collection
// row and commits a session bound to the resolved row, mirroring what the start
// path does with resolved.File. exactLookup controls whether the handler exposes
// the exact-path lookup (VirtualFileLookup); when it is unset the resolve must
// still reach the concrete owner row through the same exact-path resolution the
// deferred-probe rotation uses.
func setupAutoResultFixture(t *testing.T, exactLookup bool) *autoResultFixture {
	t.Helper()
	collection, result := autoResultRows()
	// The carried plan-time overlay is a different language from the resolved
	// row's declared track, so a test can tell which inventory is served: the
	// frozen plan-time overlay, the bound row's declared list, or the verified
	// probe result.
	planAudio := []models.AudioTrack{{Index: 0, Codec: "eac3", Channels: 6, Language: "jpn", Default: true}}

	rows := newCanonicalCollectionResolver(collection, result)
	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager, rows)
	handler.InstallationID = "test-install"
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	handler.ServiceContext = serviceCtx
	t.Cleanup(serviceCancel)

	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:8080/stream?path=" + path, nil
	})
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.AudioTracks = verifiedCanonicalAudio()
		f.SubtitleTracks = verifiedCanonicalSubtitles()
		f.CodecAudio = "eac3"
		return f, nil
	}
	handler.VirtualCandidateFileLookup = func(ctx context.Context, path, _ string, _ string, _ int) (*models.MediaFile, error) {
		// The provider-neutral fallback the auto resolve consults: it strips
		// result= and returns the lowest-id row sharing the neutral key, which
		// is the collection row.
		return rows.GetByPath(ctx, path)
	}
	if exactLookup {
		handler.VirtualFileLookup = func(ctx context.Context, path string) (*models.MediaFile, error) {
			return rows.GetByPath(ctx, path)
		}
	}
	handler.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "8f847c6", URI: autoResultURI, Resolution: "2160p",
			CodecVideo: "h264", CodecAudio: "eac3", Container: "mkv",
		}}, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	resolved, err := handler.resolveVirtualPlaybackSource(req, &collection, "profile-1", true, nil, "", "", 0, false,
		virtualResolveOptionsV3{deferProbePastCommit: true})
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource: %v", err)
	}
	if resolved.File == nil {
		t.Fatal("resolve returned no file")
	}
	if resolved.DeferredProbe == nil {
		t.Fatal("resolve returned no deferred probe")
	}

	session, err := manager.StartSession(1, "profile-1", resolved.File.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, resolved.URI, resolved.OwnerID); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
		VirtualSourceSet:              true,
		VirtualSourceOwnershipSet:     true,
		VirtualSourceURI:              resolved.URI,
		VirtualSourceRevision:         "planrev",
		VirtualSubtitleEvidenceSet:    true,
		VirtualSubtitleEvidenceURI:    resolved.URI,
		VirtualSubtitleEvidenceFileID: resolved.File.ID,
		VirtualAudioTracks:            planAudio,
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		PlaybackAttemptID: "attempt-auto", SessionID: session.ID, UserID: 1, ProfileID: "profile-1",
		RequestedMediaFileID: collection.ID, EffectiveMediaFileID: resolved.File.ID, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}
	resolved.DeferredProbe.sessionID = session.ID
	return &autoResultFixture{
		handler:   handler,
		manager:   manager,
		session:   session,
		resolver:  rows,
		resolved:  resolved,
		planAudio: planAudio,
	}
}

// TestAutoStartAnchorsEvidenceAtResultRow pins the auto/explicit parity at the
// resolve boundary: an auto start on the release's neutral collection row binds
// its effective file, its evidence anchor, and its deferred probe to the
// concrete result row the resolver selected, not to the neutral sibling.
func TestAutoStartAnchorsEvidenceAtResultRow(t *testing.T) {
	for _, exactLookup := range []bool{true, false} {
		name := "exact lookup wired"
		if !exactLookup {
			name = "owner row resolved by path"
		}
		t.Run(name, func(t *testing.T) {
			fx := setupAutoResultFixture(t, exactLookup)
			if fx.resolved.File.ID != autoResultRowID {
				t.Fatalf("resolved effective row = %d, want the concrete result row %d", fx.resolved.File.ID, autoResultRowID)
			}
			if fx.resolved.DeferredProbe.file.ID != autoResultRowID {
				t.Fatalf("deferred probe catalog row = %d, want the session's concrete result row %d",
					fx.resolved.DeferredProbe.file.ID, autoResultRowID)
			}
			if fx.resolved.DeferredProbe.probeTransient.ID != autoResultRowID {
				t.Fatalf("deferred probe transient row = %d, want the concrete result row %d",
					fx.resolved.DeferredProbe.probeTransient.ID, autoResultRowID)
			}
			if fx.session.MediaFileID != autoResultRowID {
				t.Fatalf("session effective row = %d, want the concrete result row %d", fx.session.MediaFileID, autoResultRowID)
			}
		})
	}
}

// runAutoDeferredProbe wires the durable evidence write and runs the deferred
// probe the resolve handed back, recording the row the write targeted.
func runAutoDeferredProbe(t *testing.T, fx *autoResultFixture) {
	t.Helper()
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
		fx.committedRows = append(fx.committedRows, args.FileID)
		fx.resolver.update(args.FileID, func(f *models.MediaFile) {
			f.AudioTracks = audio
			f.SubtitleTracks = subs
			f.CodecAudio = args.CodecAudio
			f.ProbeUpdatedAt = &probedAt
		})
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}
	deferred := readmitDeferredProbe(t, fx.handler, fx.resolved.DeferredProbe)
	fx.handler.runDeferredVirtualProbe(deferred)
}

// TestServe_AutoExactUriEvidenceMatchesWhenAnchoredAtResult pins the serve-layer
// outcome: after the auto start's deferred probe verifies the release, the
// session's own carried overlay holds the verified inventory, the serve bind
// serves it, and no different-candidate refusal is logged. Before the anchor
// fix the probe stamped the neutral sibling, the overlay propagation refused it,
// and the session served the frozen plan-time audio.
func TestServe_AutoExactUriEvidenceMatchesWhenAnchoredAtResult(t *testing.T) {
	var logs strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	fx := setupAutoResultFixture(t, true)
	runAutoDeferredProbe(t, fx)

	if len(fx.committedRows) != 1 || fx.committedRows[0] != autoResultRowID {
		t.Fatalf("durable evidence landed on rows %v, want the concrete result row %d", fx.committedRows, autoResultRowID)
	}
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if len(live.VirtualAudioTracks) != 2 {
		t.Fatalf("session carried audio = %+v, want the propagated verified 2 tracks", live.VirtualAudioTracks)
	}
	resultRow, err := fx.resolver.GetByID(context.Background(), autoResultRowID)
	if err != nil || resultRow == nil {
		t.Fatalf("GetByID(result): %v", err)
	}
	bound := bindSessionVirtualSourceWithTracks(context.Background(), resultRow, live, fx.resolver)
	if bound.ID != autoResultRowID {
		t.Fatalf("serve bind row = %d, want the concrete result row %d", bound.ID, autoResultRowID)
	}
	if len(bound.AudioTracks) != 2 {
		t.Fatalf("serve bind audio = %+v, want the verified 2 tracks", bound.AudioTracks)
	}
	if strings.Contains(logs.String(), "belongs to a different candidate") {
		t.Fatalf("serve bind refused the auto result-row evidence as a different candidate: %s", logs.String())
	}
}

// TestRemuxOrdinal_AutoUsesVerifiedTwoAudio pins that the session's carried
// selection space is the verified inventory the probe committed to the result
// row, so a remux ordinal the client selects against the two-audio stream names
// a real stream instead of a phantom second track over the thin plan-time list.
// The plan-time declared selection is not re-identifiable in the verified list,
// so the session records a server-enforced invalid selection instead of a stale
// position that would name whichever verified track now occupies slot 0.
func TestRemuxOrdinal_AutoUsesVerifiedTwoAudio(t *testing.T) {
	fx := setupAutoResultFixture(t, true)

	runAutoDeferredProbe(t, fx)

	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if len(live.VirtualAudioTracks) != 2 {
		t.Fatalf("session carried audio = %+v, want the verified 2-track list the stream really has", live.VirtualAudioTracks)
	}
	if !live.VirtualAudioSelectionInvalid || live.AudioTrackIndex != -1 {
		t.Fatalf("session audio index = %d (invalid=%v), want the invalid sentinel -1: the committed plan-time track is not in the verified inventory",
			live.AudioTrackIndex, live.VirtualAudioSelectionInvalid)
	}
	// The verified list is what the inventory endpoint serves, so a client that
	// selects the second audio stream (0:a:1) names the real stream.
	inventory, err := fx.handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(),
		PlaybackCaller{UserID: 1, ProfileID: "profile-1", InstallationID: "test-install"}, fx.session.ID)
	if err != nil {
		t.Fatalf("GetPlaybackInventoryV2: %v", err)
	}
	if len(inventory.AudioTracks) != 2 {
		t.Fatalf("served inventory audio = %+v, want the verified 2 tracks", inventory.AudioTracks)
	}
	if inventory.InventoryStatus != string(ProbeProvenanceVerified) {
		t.Fatalf("inventory status = %q, want verified", inventory.InventoryStatus)
	}
}

// TestAutoStartAdoptsResultRowThatAppearsAfterResolve reproduces the production
// first-ever-play shape that the resolve-time anchor fix could not cover: the
// concrete result row does not exist yet when the auto resolve runs, so the
// session is pinned to the provider-neutral collection row and the deferred
// probe stamps that row. The result row materializes later in the same session
// (the collection sync). The serve/inventory bind resolves it by exact path, but
// the session's effective row and evidence anchor still name the neutral
// sibling, so before the fix the verified overlay is rejected as a different
// candidate and the menu serves the owner row's thin declared list. The late
// adoption must move the session onto the owner row and serve the verified
// inventory.
func TestAutoStartAdoptsResultRowThatAppearsAfterResolve(t *testing.T) {
	collection, result := autoResultRows()
	planAudio := []models.AudioTrack{{Index: 0, Codec: "eac3", Channels: 6, Language: "jpn", Default: true}}
	verifiedAudio, verifiedSubs := verifiedCanonicalAudio(), verifiedCanonicalSubtitles()

	// Only the neutral collection row exists at resolve time.
	rows := newCanonicalCollectionResolver(collection)
	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager, rows)
	handler.InstallationID = "test-install"
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	handler.ServiceContext = serviceCtx
	t.Cleanup(serviceCancel)

	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:8080/stream?path=" + path, nil
	})
	handler.VirtualCandidateFileLookup = func(ctx context.Context, path, _ string, _ string, _ int) (*models.MediaFile, error) {
		return rows.GetByPath(ctx, path)
	}
	handler.VirtualFileLookup = func(ctx context.Context, path string) (*models.MediaFile, error) {
		return rows.GetByPath(ctx, path)
	}
	handler.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "8f847c6", URI: autoResultURI, Resolution: "2160p",
			CodecVideo: "h264", CodecAudio: "eac3", Container: "mkv",
		}}, nil
	})
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.AudioTracks = verifiedAudio
		f.SubtitleTracks = verifiedSubs
		f.CodecAudio = "eac3"
		return f, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	resolved, err := handler.resolveVirtualPlaybackSource(req, &collection, "profile-1", true, nil, "", "", 0, false,
		virtualResolveOptionsV3{deferProbePastCommit: true})
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource: %v", err)
	}
	if resolved.File == nil || resolved.File.ID != collection.ID {
		t.Fatalf("resolve effective row = %v, want the neutral collection row %d before the owner exists", resolved.File, collection.ID)
	}
	if resolved.DeferredProbe == nil {
		t.Fatal("resolve returned no deferred probe")
	}

	session, err := manager.StartSession(1, "profile-1", resolved.File.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, resolved.URI, resolved.OwnerID); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
		VirtualSourceSet:              true,
		VirtualSourceOwnershipSet:     true,
		VirtualSourceURI:              resolved.URI,
		VirtualSourceRevision:         "planrev",
		VirtualSubtitleEvidenceSet:    true,
		VirtualSubtitleEvidenceURI:    resolved.File.FilePath,
		VirtualSubtitleEvidenceFileID: resolved.File.ID,
		VirtualAudioTracks:            planAudio,
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		PlaybackAttemptID: "attempt-late-owner", SessionID: session.ID, UserID: 1, ProfileID: "profile-1",
		RequestedMediaFileID: collection.ID, EffectiveMediaFileID: resolved.File.ID, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}
	resolved.DeferredProbe.sessionID = session.ID

	probedAt := time.Now().UTC()
	handler.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
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
		rows.update(args.FileID, func(f *models.MediaFile) {
			f.AudioTracks = audio
			f.SubtitleTracks = subs
			f.CodecAudio = args.CodecAudio
			f.ProbeUpdatedAt = &probedAt
		})
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}

	deferred := readmitDeferredProbe(t, handler, resolved.DeferredProbe)
	handler.runDeferredVirtualProbe(deferred)

	// The collection sync materializes the concrete result row after the probe
	// committed to the neutral sibling.
	rows.mu.Lock()
	rows.rows[result.ID] = result
	rows.mu.Unlock()

	inventory, err := handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(),
		PlaybackCaller{UserID: 1, ProfileID: "profile-1", InstallationID: "test-install"}, session.ID)
	if err != nil {
		t.Fatalf("GetPlaybackInventoryV2: %v", err)
	}
	if inventory.EffectiveMediaFileID != result.ID {
		t.Fatalf("effective media file id = %d, want the materialized result row %d", inventory.EffectiveMediaFileID, result.ID)
	}
	if len(inventory.AudioTracks) != len(verifiedAudio) {
		t.Fatalf("served audio = %+v, want the committed verified %d tracks, not the owner row's thin declared list", inventory.AudioTracks, len(verifiedAudio))
	}
	if inventory.InventoryStatus != string(ProbeProvenanceVerified) {
		t.Fatalf("inventory status = %q, want verified", inventory.InventoryStatus)
	}

	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.MediaFileID != result.ID || live.VirtualSubtitleEvidenceFileID != result.ID {
		t.Fatalf("session anchor = effective %d / evidence %d, want the result row %d for both", live.MediaFileID, live.VirtualSubtitleEvidenceFileID, result.ID)
	}

	// The transport bind resolves the same verified stream after the adoption.
	boundRow, err := rows.GetByID(context.Background(), live.MediaFileID)
	if err != nil || boundRow == nil {
		t.Fatalf("GetByID(bound): %v", err)
	}
	bound := bindSessionVirtualSourceWithTracks(context.Background(), boundRow, live, rows)
	if len(bound.AudioTracks) != len(verifiedAudio) {
		t.Fatalf("transport bind audio = %+v, want the verified overlay", bound.AudioTracks)
	}
}

// TestNeutralCollectionEvidenceVersusDifferentResultRowStillRefused keeps the
// fail-closed half of the identity gate: a neutral collection row with no
// durable identity must not be accepted as the same release as a different
// result row of the same episode. The anchor fix makes the auto start target the
// concrete row, but it does not weaken the cross-release refusal for genuine
// sibling or collection evidence.
func TestNeutralCollectionEvidenceVersusDifferentResultRowStillRefused(t *testing.T) {
	neutral := &models.MediaFile{
		ID: autoCollectionRowID, ContentID: "movie-tt-auto", FilePath: autoCollectionURI,
		Container: "virtual", ProbeSource: virtualCollectionProbeSource, VirtualOwnerInstallationID: 5, MediaFolderID: 3,
	}
	other := &models.MediaFile{
		ID: autoResultRowID, ContentID: "movie-tt-auto",
		FilePath:  autoCollectionURI + "&result=aaaaaaaa",
		Container: "virtual", VirtualOwnerInstallationID: 5, MediaFolderID: 3,
		ProviderVideoHash: "hash-other", ProviderReleaseName: "Movie.Other.2024.2160p", ProviderReleaseSize: 9_000_000_000,
	}
	if boundRowSharesRelease(other, neutral) {
		t.Fatal("an identity-less neutral collection row was accepted as the same release as a different result row")
	}
	// And the canonical-collection predicate keeps failing closed for the same
	// pairing, so no cross-release adoption is admitted.
	session := &playback.Session{VirtualSubtitleEvidenceFileID: neutral.ID, VirtualSubtitleEvidenceURI: neutral.FilePath}
	resolver := newCanonicalCollectionResolver(*neutral, *other)
	if canonicalCollectionEvidenceMatchesBoundRow(context.Background(), other, session, resolver, neutral.FilePath) {
		t.Fatal("identity-less neutral evidence matched a different result row's release")
	}
}
