package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// The canonical collection case: a result-pinned session serves the concrete
// ?result= sibling of a release while its carried track evidence is anchored at
// that release's provider-neutral collection path. The deferred probe commits
// verified evidence onto the neutral collection row (the same base path, no
// result pick) and that truth has to reach the same session through its carried
// overlay; the serve path must recognize the neutral anchor as the bound
// release rather than rejecting it as a different candidate.
//
// The neutral query is written in the order virtualPlaybackNeutralKey re-encodes
// it, so the two anchors compare equal once the result id is stripped — exactly
// the production shape where the collection URI and the pinned URI share a base.
const (
	canonicalCollectionRowID = 4912227
	canonicalPinnedRowID     = 5273000
	canonicalCollectionURI   = "virtual://movie/tt-canonical/2/10?episode=10&season=2"
	canonicalPinnedURI       = canonicalCollectionURI + "&result=0803bd5a"
)

// canonicalCollectionResolver serves the two catalog rows the case needs: the
// neutral collection row the deferred probe commits to and the result-pinned
// sibling the session is bound to. Rows are copied on read and write, so a probe
// worker's commit cannot race the inventory read (production's reads and writes
// go through the database).
type canonicalCollectionResolver struct {
	mu   sync.Mutex
	rows map[int]models.MediaFile
}

func newCanonicalCollectionResolver(rows ...models.MediaFile) *canonicalCollectionResolver {
	m := make(map[int]models.MediaFile, len(rows))
	for _, row := range rows {
		m[row.ID] = row
	}
	return &canonicalCollectionResolver{rows: m}
}

func (r *canonicalCollectionResolver) GetByID(_ context.Context, id int) (*models.MediaFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[id]
	if !ok {
		return nil, nil
	}
	return &row, nil
}

func (r *canonicalCollectionResolver) GetByPath(_ context.Context, path string) (*models.MediaFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, row := range r.rows {
		if strings.TrimSpace(row.FilePath) == strings.TrimSpace(path) {
			cp := r.rows[id]
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *canonicalCollectionResolver) update(id int, fn func(*models.MediaFile)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row := r.rows[id]
	fn(&row)
	r.rows[id] = row
}

func canonicalCollectionIdentity() virtualProbeIdentity {
	return virtualProbeIdentity{
		VideoHash:   "hash-shared-release",
		ReleaseName: "Movie.2024.1080p.WEB-DL.x264",
		ReleaseSize: 8_000_000_000,
	}
}

// canonicalCollectionRows builds the neutral collection row and its
// result-pinned sibling with the identities supplied by the caller, so each
// subtest controls whether the two rows provably name one release.
func canonicalCollectionRows(canonicalIdentity, pinnedIdentity virtualProbeIdentity) (models.MediaFile, models.MediaFile) {
	canonical := models.MediaFile{
		ID:                         canonicalCollectionRowID,
		ContentID:                  "movie-tt-canonical",
		FilePath:                   canonicalCollectionURI,
		Container:                  "virtual",
		ProbeSource:                virtualCollectionProbeSource,
		VirtualOwnerInstallationID: 5,
		MediaFolderID:              3,
		ProviderVideoHash:          canonicalIdentity.VideoHash,
		ProviderGUID:               canonicalIdentity.GUID,
		ProviderReleaseName:        canonicalIdentity.ReleaseName,
		ProviderReleaseSize:        canonicalIdentity.ReleaseSize,
		AudioTracks:                []models.AudioTrack{{Index: 0, Codec: "eac3", Channels: 6, Language: "eng", Default: true}},
	}
	pinned := models.MediaFile{
		ID:                         canonicalPinnedRowID,
		ContentID:                  "movie-tt-canonical",
		FilePath:                   canonicalPinnedURI,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
		MediaFolderID:              3,
		ProviderVideoHash:          pinnedIdentity.VideoHash,
		ProviderGUID:               pinnedIdentity.GUID,
		ProviderReleaseName:        pinnedIdentity.ReleaseName,
		ProviderReleaseSize:        pinnedIdentity.ReleaseSize,
		AudioTracks:                []models.AudioTrack{{Index: 0, Codec: "eac3", Channels: 6, Language: "eng", Default: true}},
	}
	return canonical, pinned
}

type canonicalCollectionFixture struct {
	handler       *PlaybackHandler
	manager       *playback.SessionManager
	session       *playback.Session
	resolver      *canonicalCollectionResolver
	canonicalRow  *models.MediaFile
	planAudio     []models.AudioTrack
	committedRows []int
}

func setupCanonicalCollectionFixture(t *testing.T, canonicalIdentity, pinnedIdentity virtualProbeIdentity) *canonicalCollectionFixture {
	t.Helper()
	canonical, pinned := canonicalCollectionRows(canonicalIdentity, pinnedIdentity)
	// The carried plan-time overlay is deliberately a different language from
	// the bound row's declared track, so a test can tell which inventory a
	// refusal serves: the carried overlay or the row's own declared list.
	planAudio := []models.AudioTrack{{Index: 0, Codec: "eac3", Channels: 6, Language: "jpn", Default: true}}

	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", canonicalPinnedRowID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, canonicalPinnedURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	// The plan captured the release's neutral collection anchor while the
	// binding names the concrete ?result= sibling. The evidence row id stays at
	// the pinned row, so only the URI distinguishes the anchors.
	if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
		VirtualSourceSet:              true,
		VirtualSourceOwnershipSet:     true,
		VirtualSourceURI:              canonicalPinnedURI,
		VirtualSourceRevision:         "planrev",
		VirtualSubtitleEvidenceSet:    true,
		VirtualSubtitleEvidenceURI:    canonicalCollectionURI,
		VirtualSubtitleEvidenceFileID: canonicalPinnedRowID,
		VirtualAudioTracks:            planAudio,
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}

	rows := newCanonicalCollectionResolver(canonical, pinned)
	handler := NewPlaybackHandler(manager, rows)
	handler.InstallationID = "test-install"
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	handler.ServiceContext = serviceCtx
	t.Cleanup(serviceCancel)
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		PlaybackAttemptID: "attempt-canonical", SessionID: session.ID, UserID: 1, ProfileID: "profile-1",
		RequestedMediaFileID: canonicalPinnedRowID, EffectiveMediaFileID: canonicalPinnedRowID, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}
	return &canonicalCollectionFixture{
		handler:      handler,
		manager:      manager,
		session:      session,
		resolver:     rows,
		canonicalRow: &canonical,
		planAudio:    planAudio,
	}
}

func canonicalCollectionProbe(session *playback.Session, canonical models.MediaFile, identity virtualProbeIdentity) *virtualDeferredProbeV3 {
	probeFile := canonical
	return &virtualDeferredProbeV3{
		stickyKey:      "sticky-canonical",
		file:           &probeFile,
		streamURL:      "http://provider.example/stream",
		probeTransient: &models.MediaFile{ID: canonical.ID, FilePath: canonical.FilePath},
		cand:           VirtualPlaybackStream{ID: "cand-pinned", URI: canonicalPinnedURI},
		candidate:      identity,
		ownerID:        5,
		sessionID:      session.ID,
	}
}

func verifiedCanonicalAudio() []models.AudioTrack {
	return []models.AudioTrack{
		{Index: 0, Codec: "eac3", Channels: 6, Language: "eng", Default: true},
		{Index: 1, Codec: "aac", Channels: 2, Language: "jpn"},
	}
}

func verifiedCanonicalSubtitles() []models.SubtitleTrack {
	subs := make([]models.SubtitleTrack, 0, 9)
	for i := 0; i < 9; i++ {
		subs = append(subs, models.SubtitleTrack{Index: i + 2, Codec: "subrip", Language: "eng"})
	}
	return subs
}

// TestPinnedSessionReceivesCanonicalCollectionEvidenceViaOverlay is the pin: a
// result-pinned session whose evidence is anchored at the same release's neutral
// collection path must receive the collection row's verified tracks through its
// carried overlay after the deferred probe commits them. The same session's
// inventory then serves the verified 2 audio / 9 subtitle list with a changed
// revision, the transport keeps its pinned row and ordinal, and no
// different-candidate warning is emitted.
func TestPinnedSessionReceivesCanonicalCollectionEvidenceViaOverlay(t *testing.T) {
	var logs strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	identity := canonicalCollectionIdentity()
	fx := setupCanonicalCollectionFixture(t, identity, identity)
	caller := PlaybackCaller{UserID: 1, ProfileID: "profile-1", InstallationID: "test-install"}

	verifiedAudio, verifiedSubs := verifiedCanonicalAudio(), verifiedCanonicalSubtitles()
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
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.AudioTracks = verifiedAudio
		f.SubtitleTracks = verifiedSubs
		f.CodecAudio = "eac3"
		return f, nil
	}

	before, err := fx.handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(), caller, fx.session.ID)
	if err != nil {
		t.Fatalf("GetPlaybackInventoryV2 before: %v", err)
	}
	if len(before.AudioTracks) != 1 || len(before.SubtitleInventory) != 0 {
		t.Fatalf("provisional inventory = %d audio / %d subtitles, want the captured 1/0 overlay", len(before.AudioTracks), len(before.SubtitleInventory))
	}

	deferred := readmitDeferredProbe(t, fx.handler, canonicalCollectionProbe(fx.session, *fx.canonicalRow, identity))
	fx.handler.runDeferredVirtualProbe(deferred)

	if len(fx.committedRows) != 1 || fx.committedRows[0] != canonicalCollectionRowID {
		t.Fatalf("durable evidence landed on rows %v, want the canonical collection row %d", fx.committedRows, canonicalCollectionRowID)
	}

	after, err := fx.handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(), caller, fx.session.ID)
	if err != nil {
		t.Fatalf("GetPlaybackInventoryV2 after: %v", err)
	}
	if after.InventoryStatus != string(ProbeProvenanceVerified) {
		t.Fatalf("inventory status = %q, want verified", after.InventoryStatus)
	}
	if len(after.AudioTracks) != 2 {
		t.Fatalf("verified poll audio = %+v, want the collection row's 2 tracks", after.AudioTracks)
	}
	if len(after.SubtitleInventory) != 9 {
		t.Fatalf("verified poll subtitles = %d tracks, want the collection row's 9", len(after.SubtitleInventory))
	}
	if after.InventoryRevision == before.InventoryRevision {
		t.Fatalf("inventory revision did not change when the verified tracks landed: %q", after.InventoryRevision)
	}
	if after.EffectiveMediaFileID != canonicalPinnedRowID {
		t.Fatalf("effective media file id = %d, want the pinned transport row %d", after.EffectiveMediaFileID, canonicalPinnedRowID)
	}

	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if len(live.VirtualAudioTracks) != 2 {
		t.Fatalf("session carried audio = %+v, want the propagated verified 2 tracks", live.VirtualAudioTracks)
	}
	// The plan-time declared track is not re-identifiable in the verified
	// inventory, so the session records a server-enforced invalid selection:
	// no position survives to name whichever verified track now occupies the
	// old slot.
	if !live.VirtualAudioSelectionInvalid || live.AudioTrackIndex != -1 {
		t.Fatalf("session audio index = %d (invalid=%v), want the invalid sentinel -1", live.AudioTrackIndex, live.VirtualAudioSelectionInvalid)
	}
	pinnedRow, err := fx.resolver.GetByID(context.Background(), canonicalPinnedRowID)
	if err != nil || pinnedRow == nil {
		t.Fatalf("GetByID(pinned): %v", err)
	}
	bound := bindSessionVirtualSourceWithTracks(context.Background(), pinnedRow, live, fx.resolver)
	if bound.ID != canonicalPinnedRowID || bound.FilePath != canonicalPinnedURI {
		t.Fatalf("serve bind moved to row %d path %q; want the pinned transport row", bound.ID, bound.FilePath)
	}
	if len(bound.AudioTracks) != len(live.VirtualAudioTracks) {
		t.Fatalf("serve bind audio = %+v, want the session's propagated overlay %+v", bound.AudioTracks, live.VirtualAudioTracks)
	}
	if logs.String() != "" {
		t.Fatalf("canonical collection evidence logged a warning: %s", logs.String())
	}
}

// TestPinnedSessionRefusesCanonicalCollectionEvidenceWithoutSharedRelease keeps
// the fail-closed half: when the neutral collection row cannot prove a shared
// release with the bound row (identity-less or conflicting tiers), the committed
// evidence must not reach the session, the serve path must keep refusing it, and
// the carried audio must never be emptied.
func TestPinnedSessionRefusesCanonicalCollectionEvidenceWithoutSharedRelease(t *testing.T) {
	cases := []struct {
		name              string
		canonicalIdentity virtualProbeIdentity
		pinnedIdentity    virtualProbeIdentity
	}{
		{
			name:              "identity-less rows fail closed",
			canonicalIdentity: virtualProbeIdentity{},
			pinnedIdentity:    virtualProbeIdentity{},
		},
		{
			name:              "conflicting tiers fail closed",
			canonicalIdentity: virtualProbeIdentity{VideoHash: "hash-canonical", ReleaseName: "Movie.2024.1080p.WEB-DL.x264", ReleaseSize: 8_000_000_000},
			pinnedIdentity:    virtualProbeIdentity{VideoHash: "hash-pinned", ReleaseName: "Movie.2024.1080p.WEB-DL.x264", ReleaseSize: 8_000_000_000},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs strings.Builder
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(previous) })

			fx := setupCanonicalCollectionFixture(t, tc.canonicalIdentity, tc.pinnedIdentity)
			caller := PlaybackCaller{UserID: 1, ProfileID: "profile-1", InstallationID: "test-install"}

			verifiedAudio, verifiedSubs := verifiedCanonicalAudio(), verifiedCanonicalSubtitles()
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
				fx.resolver.update(args.FileID, func(f *models.MediaFile) {
					f.AudioTracks = audio
					f.SubtitleTracks = subs
					f.ProbeUpdatedAt = &probedAt
				})
				return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
			}
			fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
				f.AudioTracks = verifiedAudio
				f.SubtitleTracks = verifiedSubs
				f.CodecAudio = "eac3"
				return f, nil
			}

			deferred := readmitDeferredProbe(t, fx.handler, canonicalCollectionProbe(fx.session, *fx.canonicalRow, tc.canonicalIdentity))
			fx.handler.runDeferredVirtualProbe(deferred)

			after, err := fx.handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(), caller, fx.session.ID)
			if err != nil {
				t.Fatalf("GetPlaybackInventoryV2 after: %v", err)
			}
			if len(after.AudioTracks) != 1 || after.AudioTracks[0].Language != "eng" {
				t.Fatalf("refused probe served audio = %+v, want the bound row's own 1-track declared inventory, never the collection row's 2", after.AudioTracks)
			}
			live, err := fx.manager.GetSession(fx.session.ID)
			if err != nil {
				t.Fatalf("GetSession: %v", err)
			}
			if len(live.VirtualAudioTracks) != 1 || live.VirtualAudioTracks[0].Language != "jpn" {
				t.Fatalf("session carried audio = %+v, want the untouched plan-time jpn overlay", live.VirtualAudioTracks)
			}
			raw, err := json.Marshal(after)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			if string(decoded["audio_tracks"]) == "null" {
				t.Fatal("inventory serialized audio_tracks null after a refused canonical collection commit")
			}
			if !strings.Contains(logs.String(), "belongs to a different candidate") {
				t.Fatalf("the refusal was not logged as a different-candidate mismatch: %s", logs.String())
			}
		})
	}
}

// TestPinnedSessionKeepsCarriedAudioWhenCanonicalCollectionCommitRefused pins
// the refused-write half for the canonical case: a durable write that does not
// commit reports failed and the carried plan-time audio is served intact, never
// emptied.
func TestPinnedSessionKeepsCarriedAudioWhenCanonicalCollectionCommitRefused(t *testing.T) {
	identity := canonicalCollectionIdentity()
	fx := setupCanonicalCollectionFixture(t, identity, identity)
	caller := PlaybackCaller{UserID: 1, ProfileID: "profile-1", InstallationID: "test-install"}

	fx.handler.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		fx.resolver.update(args.FileID, func(f *models.MediaFile) {
			f.AudioTracks = nil
			f.SubtitleTracks = nil
			f.ProbeUpdatedAt = nil
		})
		return VirtualFileMetadataUpdateResult{MetadataUpdated: false, RowsAffected: 0}, nil
	}
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.AudioTracks = verifiedCanonicalAudio()
		f.SubtitleTracks = verifiedCanonicalSubtitles()
		f.CodecAudio = "eac3"
		return f, nil
	}

	deferred := readmitDeferredProbe(t, fx.handler, canonicalCollectionProbe(fx.session, *fx.canonicalRow, identity))
	fx.handler.runDeferredVirtualProbe(deferred)

	after, err := fx.handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(), caller, fx.session.ID)
	if err != nil {
		t.Fatalf("GetPlaybackInventoryV2 after: %v", err)
	}
	if after.InventoryStatus != string(ProbeProvenanceFailed) {
		t.Fatalf("inventory status = %q, want failed", after.InventoryStatus)
	}
	if len(after.AudioTracks) != 1 || after.AudioTracks[0].Language != "jpn" {
		t.Fatalf("polled audio after a refused durable write = %+v, want the carried 1-track jpn overlay", after.AudioTracks)
	}
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if len(live.VirtualAudioTracks) != 1 || live.VirtualAudioTracks[0].Language != "jpn" {
		t.Fatalf("session carried audio = %+v, want the untouched plan-time jpn overlay", live.VirtualAudioTracks)
	}
}
