package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// virtualProbeGateCandidateHandler wires the minimum seams to drive
// resolveVirtualPlaybackSource down the candidate-declared upgrade path: a
// listed candidate that declares codecs/resolution/container, a stored row
// seeded by VirtualFileLookup, and a stub prober/saver.
func virtualProbeGateCandidateHandler(stored *models.MediaFile, lister VirtualPlaybackStreamLister, prober VirtualPlaybackSourceProber, saver VirtualFileSaver) *PlaybackHandler {
	return &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			return "http://127.0.0.1:8080/stream?path=" + path, nil
		}),
		VirtualPlaybackStreamLister: lister,
		VirtualFileLookup: func(_ context.Context, _ string) (*models.MediaFile, error) {
			return stored, nil
		},
		VirtualPlaybackSourceProber: prober,
		VirtualFileSaver:            saver,
	}
}

func virtualProbeGateLister() VirtualPlaybackStreamListerFunc {
	return func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID:         "cand-1",
			URI:        "virtual://movie/tt-probe-gate?result=cand-1",
			Resolution: "1080p",
			CodecVideo: "h264",
			CodecAudio: "aac",
			Container:  "mkv",
			// A provider-declared language that the synthesis path folds in
			// before the probe. It must not survive as a duplicate track once
			// the real probed inventory replaces the synthesized one.
			AudioLanguages: []string{"deu"},
		}}, nil
	}
}

// A virtual row with no probe stamp (NULL tracks, NULL probe_updated_at) whose
// candidate declares enough metadata to synthesize complete-looking evidence
// must still run the real probe and persist its true inventory. The immediate
// plan keeps the synthesized tracks; persistence happens in the background.
func TestResolveProbesUnprobedVirtualRowDespiteCandidateDeclarations(t *testing.T) {
	stored := &models.MediaFile{
		ID:                         100,
		ContentID:                  "movie-1",
		FilePath:                   "virtual://movie/tt-probe-gate?result=cand-1",
		Container:                  "virtual",
		ProbeUpdatedAt:             nil,
		VirtualOwnerInstallationID: 5,
	}

	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	saverDone := make(chan struct{})
	var savedVideo, savedAudio, savedSubs []byte

	h := virtualProbeGateCandidateHandler(stored, virtualProbeGateLister(),
		func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			close(probeStarted)
			<-releaseProbe
			// A real ffprobe inventory: different video codec and a track
			// inventory the candidate declarations cannot express.
			f.VideoTracks = []models.VideoTrack{{Codec: "hevc", Width: 3840, Height: 2160, FrameRate: "23.976"}}
			f.AudioTracks = []models.AudioTrack{
				{Codec: "eac3", Channels: 6, Language: "eng", Default: true},
				{Codec: "aac", Channels: 2, Language: "jpn"},
			}
			f.SubtitleTracks = []models.SubtitleTrack{{Codec: "srt", Language: "eng"}}
			f.CodecVideo = "hevc"
			f.CodecAudio = "eac3"
			f.Resolution = "2160p"
			f.Container = "mkv"
			return f, nil
		},
		func(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
			savedVideo = args.VideoTracks
			savedAudio = args.AudioTracks
			savedSubs = args.SubtitleTracks
			close(saverDone)
			return 1, nil
		})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := &models.MediaFile{
		ID:                         100,
		ContentID:                  "movie-1",
		FilePath:                   "virtual://movie/tt-probe-gate?result=cand-1",
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}

	// The immediate plan is not blocked on the probe and still carries the
	// candidate-synthesized evidence so playback can start right away.
	if resolved.Provenance != ProbeProvenancePending || resolved.ProbeSucceeded {
		t.Fatalf("immediate result provenance=%q succeeded=%v, want pending/false", resolved.Provenance, resolved.ProbeSucceeded)
	}
	if resolved.File == nil || len(resolved.File.VideoTracks) != 1 || resolved.File.VideoTracks[0].Codec != "h264" {
		t.Fatalf("immediate plan video tracks = %#v, want synthesized h264", resolved.File)
	}
	if len(resolved.File.AudioTracks) != 1 || resolved.File.AudioTracks[0].Codec != "aac" {
		t.Fatalf("immediate plan audio tracks = %#v, want synthesized aac", resolved.File.AudioTracks)
	}
	if len(resolved.File.SubtitleTracks) != 0 {
		t.Fatalf("immediate plan subtitle tracks = %#v, want none", resolved.File.SubtitleTracks)
	}

	select {
	case <-probeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("prober was not invoked for an unprobed virtual row")
	}
	select {
	case <-saverDone:
		t.Fatal("metadata persisted before the probe finished")
	default:
	}

	close(releaseProbe)
	select {
	case <-saverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("probed inventory was not persisted")
	}

	// The persisted inventory must be the probe result, not the synthesized
	// candidate declaration.
	if !strings.Contains(string(savedVideo), "hevc") || strings.Contains(string(savedVideo), "h264") {
		t.Fatalf("persisted video tracks = %s, want probed hevc", savedVideo)
	}
	if !strings.Contains(string(savedAudio), "jpn") || !strings.Contains(string(savedAudio), "eac3") {
		t.Fatalf("persisted audio tracks = %s, want probed multi-track inventory", savedAudio)
	}
	// The candidate-declared German language was synthesized into the immediate
	// plan, but a real probe inventory must replace it rather than re-append it.
	if strings.Contains(string(savedAudio), "deu") {
		t.Fatalf("persisted audio tracks = %s, probe inventory must replace the synthesized track", savedAudio)
	}
	if !strings.Contains(string(savedSubs), "srt") {
		t.Fatalf("persisted subtitle tracks = %s, want probed srt", savedSubs)
	}
}

// A row that already carries a probe stamp and complete evidence keeps the fast
// skipProbe path: no provider round-trip and no re-probe on every play.
func TestResolveSkipsProbeForAlreadyProbedVirtualRow(t *testing.T) {
	probedAt := time.Now().Add(-time.Hour)
	stored := &models.MediaFile{
		ID:                         200,
		ContentID:                  "movie-1",
		FilePath:                   "virtual://movie/tt-probe-gate-b?result=cand-1",
		Container:                  "mkv",
		CodecVideo:                 "h264",
		CodecAudio:                 "aac",
		Resolution:                 "1080p",
		Bitrate:                    10_000,
		ProbeUpdatedAt:             &probedAt,
		VirtualOwnerInstallationID: 5,
		VideoTracks:                []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, FrameRate: "24", BitDepth: 8, Bitrate: 10_000}},
		AudioTracks:                []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng", Default: true}},
	}

	probeCalls := 0
	h := virtualProbeGateCandidateHandler(stored, virtualProbeGateLister(),
		func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			probeCalls++
			return f, nil
		}, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := *stored
	file.FilePath = stored.FilePath
	resolved, err := h.resolveVirtualPlaybackSource(req, &file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if probeCalls != 0 {
		t.Fatalf("prober called %d times for an already-probed row, want 0", probeCalls)
	}
	if resolved.Provenance != ProbeProvenanceVerified || !resolved.ProbeSucceeded {
		t.Fatalf("provenance=%q succeeded=%v, want verified/true fast path", resolved.Provenance, resolved.ProbeSucceeded)
	}
}

// The probe gate can only converge if persisting a probe also stamps the row:
// without probe_updated_at a row that was just probed still looks unprobed on
// the next start and re-probes forever. probe_source stays 'virtual_collection'
// on collection-owned rows so the collection materializer keeps recognizing
// them, but a real playback probe stamps probe_updated_at on any row —
// including collection rows — so they converge to probed evidence instead of
// re-probing on every start.
func TestVirtualFileMetadataUpdatePersistsProbeStamp(t *testing.T) {
	sql := VirtualFileMetadataUpdateSQL
	if !strings.Contains(sql, "probe_updated_at") {
		t.Fatalf("metadata update does not stamp probe_updated_at: %s", sql)
	}
	if !strings.Contains(sql, "WHEN probe_source = 'virtual_collection' THEN probe_source") {
		t.Fatalf("metadata update does not preserve virtual_collection probe_source: %s", sql)
	}
	if !strings.Contains(sql, "ELSE GREATEST(clock_timestamp(), probe_updated_at") {
		t.Fatalf("metadata update does not stamp probe_updated_at on any real probe: %s", sql)
	}
	if !strings.Contains(sql, "ELSE 'virtual'") {
		t.Fatalf("metadata update does not default probe_source to virtual: %s", sql)
	}
	// Declared (unprobed) persists must not stamp the row as probed: the
	// stamp is gated on the $13 stampProbe flag.
	if !strings.Contains(sql, "WHEN NOT $13::boolean THEN probe_source") {
		t.Fatalf("metadata update does not gate probe stamp on stampProbe flag: %s", sql)
	}
	// A clear branch must exist for a release swap: the row must stop looking
	// probed so the next start re-probes the adopted bytes.
	if !strings.Contains(sql, "WHEN $31::boolean THEN NULL") {
		t.Fatalf("metadata update does not clear the probe stamp on a clear request: %s", sql)
	}
	// A stale background probe must not overwrite evidence committed since
	// its snapshot: the CAS fence binds the row to the caller's snapshot.
	if !strings.Contains(sql, "AND updated_at     = $14") {
		t.Fatalf("metadata update does not fence on updated_at snapshot: %s", sql)
	}
	if !strings.Contains(sql, "AND probe_updated_at IS NOT DISTINCT FROM $15::timestamptz") {
		t.Fatalf("metadata update does not fence on probe_updated_at snapshot: %s", sql)
	}
	// Path adoption must be refused on collection-owned rows. IS DISTINCT FROM
	// makes rows with a NULL probe_source (never stamped) adopt like any other
	// non-collection source instead of the guard evaluating to NULL. The only
	// exception is the explicit collection-variant reconcile verdict ($32),
	// which a caller sets only after proving the pinned release vanished.
	if !strings.Contains(sql, "WHEN $18 != '' AND (probe_source IS DISTINCT FROM 'virtual_collection' OR $32::boolean)") {
		t.Fatalf("metadata update does not guard path adoption: %s", sql)
	}
	// Adoption must also be refused when a sibling row (same virtual owner and
	// library) already owns the target path: adopting it anyway violates
	// media_files_virtual_file_owner_key and drops the probe evidence.
	if !strings.Contains(sql, "NOT EXISTS (") ||
		!strings.Contains(sql, "FROM media_files sibling") ||
		!strings.Contains(sql, "sibling.id <> media_files.id") ||
		!strings.Contains(sql, "sibling.file_path = $18") ||
		!strings.Contains(sql, "sibling.virtual_owner_installation_id IS NOT DISTINCT FROM $16") ||
		!strings.Contains(sql, "sibling.media_folder_id IS NOT DISTINCT FROM $17") {
		t.Fatalf("metadata update does not guard path adoption against an existing sibling owner: %s", sql)
	}
	// The metadata update must still adopt the path when no sibling owns it.
	if !strings.Contains(sql, "THEN $18\n    ELSE file_path\n  END,") {
		t.Fatalf("metadata update does not adopt $18 when no sibling owns it: %s", sql)
	}
}

// A resolution-less stored row whose lister returns a resolution-less
// candidate must still synthesize the 1080p
// baseline through the candidate-merge gate: the immediate plan keeps the
// synthesized tracks and defers the real probe to the background.
func TestResolveResolutionlessMergesIntoBaseline(t *testing.T) {
	uri := "virtual://movie/tt-merge-baseline?result=cand-1"
	stored := &models.MediaFile{
		ID:                         303,
		ContentID:                  "movie-1",
		FilePath:                   uri,
		Container:                  "mkv",
		CodecVideo:                 "h264",
		VirtualOwnerInstallationID: 5,
	}
	lister := VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "cand-merge", URI: uri, CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
			AudioLanguages: []string{"eng"},
		}}, nil
	})
	probeStarted := make(chan struct{})
	var probeCalls int32
	h := virtualProbeGateCandidateHandler(stored, lister,
		func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			atomic.AddInt32(&probeCalls, 1)
			close(probeStarted)
			<-time.After(50 * time.Millisecond)
			f.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, FrameRate: "24"}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}}
			f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = "h264", "aac", "1080p", "mkv"
			return f, nil
		}, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := *stored
	resolved, err := h.resolveVirtualPlaybackSource(req, &file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.Provenance != ProbeProvenancePending {
		t.Fatalf("provenance=%q, want pending deferred probe", resolved.Provenance)
	}
	if resolved.File == nil || resolved.File.Resolution != "1080p" {
		t.Fatalf("resolution=%v, want 1080p baseline", resolved.File)
	}
	if !completeVirtualVideoEvidenceV3(resolved.File) ||
		!completeVirtualAudioEvidenceV3(resolved.File) ||
		!completeVirtualContainerEvidenceV3(resolved.File) {
		t.Fatalf("baseline must carry complete evidence: %#v", resolved.File)
	}

	select {
	case <-probeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("real probe was not launched in the background")
	}
	if probeCalls := atomic.LoadInt32(&probeCalls); probeCalls != 1 {
		t.Fatalf("prober called %d times, want 1 background probe", probeCalls)
	}
}

// A resolution-less merge gate must not erase candidate-declared tracks: the
// probe failure damper path degrades to the same 1080p baseline. The first
// call consumes the probe budget; the second replan skips the prober via the
// damper but keeps the complete baseline evidence.
func TestResolveVirtualProbeFailureBaselineForResolutionless(t *testing.T) {
	uri := "virtual://movie/tt-negative-cache-baseline?result=cand-1"
	key := virtualProbeFailureKey(uri, 5)
	virtualProbeFailures.clear(key)
	t.Cleanup(func() { virtualProbeFailures.clear(key) })

	stored := &models.MediaFile{
		ID:                         302,
		ContentID:                  "movie-1",
		FilePath:                   uri,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	lister := VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "cand-baseline", URI: uri, CodecAudio: "aac", Container: "mkv",
			AudioLanguages: []string{"eng"},
		}}, nil
	})
	probeCalls := 0
	h := virtualProbeGateCandidateHandler(stored, lister,
		func(_ context.Context, _ string, _ *models.MediaFile) (*models.MediaFile, error) {
			probeCalls++
			return nil, errors.New("probe failed")
		}, nil)

	for call := 0; call < 2; call++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
		file := *stored
		resolved, err := h.resolveVirtualPlaybackSource(req, &file, "profile-1", false, nil, "", "", 0, false)
		if err != nil {
			t.Fatalf("call %d: resolveVirtualPlaybackSource error: %v", call, err)
		}
		if resolved.Provenance != ProbeProvenanceFailed {
			t.Fatalf("call %d: provenance=%q, want failed declared fallback", call, resolved.Provenance)
		}
		if resolved.File == nil || resolved.File.Resolution != "1080p" {
			t.Fatalf("call %d: resolution=%v, want 1080p baseline", call, resolved.File)
		}
		if !completeVirtualVideoEvidenceV3(resolved.File) {
			t.Fatalf("call %d: baseline must carry complete video evidence: %#v", call, resolved.File)
		}
		if !completeVirtualAudioEvidenceV3(resolved.File) {
			t.Fatalf("call %d: baseline must carry complete audio evidence: %#v", call, resolved.File)
		}
		if !completeVirtualContainerEvidenceV3(resolved.File) {
			t.Fatalf("call %d: baseline must carry complete container evidence: %#v", call, resolved.File)
		}
	}
	if probeCalls != 1 {
		t.Fatalf("prober called %d times across two replans, want 1 after the failure damper engages", probeCalls)
	}
}

// Merge idempotency: calling mergeVirtualCandidateTracks once vs twice with
// the same inputs must produce identical results. The baseline fix previously
// created empty-codec tracks on the first merge and filled them on the second.
func TestMergeVirtualCandidateTracksIdempotency(t *testing.T) {
	candidate := VirtualPlaybackStream{
		Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
		AudioLanguages: []string{"eng"},
	}
	// Single merge.
	once := &models.MediaFile{}
	mergeVirtualCandidateTracks(once, candidate)
	onceTracks := once.VideoTracks
	onceAudio := once.AudioTracks

	// Double merge: second call must not change anything.
	twice := &models.MediaFile{}
	mergeVirtualCandidateTracks(twice, candidate)
	mergeVirtualCandidateTracks(twice, candidate)

	if twice.CodecVideo != once.CodecVideo {
		t.Errorf("CodecVideo single=%q double=%q", once.CodecVideo, twice.CodecVideo)
	}
	if twice.CodecAudio != once.CodecAudio {
		t.Errorf("CodecAudio single=%q double=%q", once.CodecAudio, twice.CodecAudio)
	}
	if twice.Container != once.Container {
		t.Errorf("Container single=%q double=%q", once.Container, twice.Container)
	}
	if twice.Resolution != once.Resolution {
		t.Errorf("Resolution single=%q double=%q", once.Resolution, twice.Resolution)
	}
	if len(twice.VideoTracks) != len(onceTracks) {
		t.Fatalf("VideoTracks single=%d double=%d", len(onceTracks), len(twice.VideoTracks))
	}
	for i := range onceTracks {
		if twice.VideoTracks[i].Codec != onceTracks[i].Codec {
			t.Errorf("VideoTracks[%d].Codec single=%q double=%q", i, onceTracks[i].Codec, twice.VideoTracks[i].Codec)
		}
	}
	if len(twice.AudioTracks) != len(onceAudio) {
		t.Fatalf("AudioTracks single=%d double=%d", len(onceAudio), len(twice.AudioTracks))
	}
}

// Fully empty metadata (no resolution, no codecs, no container) must not
// synthesize tracks or codecs when resolution is absent. The merge should
// leave the file evidence-incomplete so the planner produces a terminal.
func TestMergeVirtualCandidateTracksEmptyMetadataStaysIncomplete(t *testing.T) {
	file := &models.MediaFile{}
	candidate := VirtualPlaybackStream{}

	mergeVirtualCandidateTracks(file, candidate)

	if file.Resolution != "" {
		t.Errorf("Resolution = %q, want empty", file.Resolution)
	}
	if file.CodecVideo != "" {
		t.Errorf("CodecVideo = %q, want empty", file.CodecVideo)
	}
	if file.CodecAudio != "" {
		t.Errorf("CodecAudio = %q, want empty", file.CodecAudio)
	}
	if file.Container != "" {
		t.Errorf("Container = %q, want empty", file.Container)
	}
	if len(file.VideoTracks) != 0 {
		t.Errorf("VideoTracks = %d, want 0", len(file.VideoTracks))
	}
	if len(file.AudioTracks) != 0 {
		t.Errorf("AudioTracks = %d, want 0", len(file.AudioTracks))
	}
	if completeVirtualVideoEvidenceV3(file) || completeVirtualAudioEvidenceV3(file) || completeVirtualContainerEvidenceV3(file) {
		t.Error("empty metadata must not produce complete evidence")
	}
}

// ResolutionAssumed flag is set only at baseline fallback sites, not on
// verified probe paths.
func TestResolutionAssumedFlagOnlyOnBaseline(t *testing.T) {
	uri := "virtual://movie/tt-resassumed-verify?result=cand-1"
	stored := &models.MediaFile{
		ID:                         304,
		ContentID:                  "movie-1",
		FilePath:                   uri,
		Container:                  "mkv",
		CodecVideo:                 "h264",
		Resolution:                 "1080p",
		Bitrate:                    10_000,
		ProbeUpdatedAt:             timePtr(time.Now().Add(-time.Hour)),
		VirtualOwnerInstallationID: 5,
		VideoTracks:                []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, FrameRate: "24", BitDepth: 8, Bitrate: 10_000}},
		AudioTracks:                []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}},
	}
	lister := VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "cand-ok", URI: uri, Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
		}}, nil
	})
	h := virtualProbeGateCandidateHandler(stored, lister, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := *stored
	resolved, err := h.resolveVirtualPlaybackSource(req, &file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.ResolutionAssumed {
		t.Error("verified fast-path must not set ResolutionAssumed")
	}
}

// No-prober fallback with assumed resolution must not persist the fabricated
// metadata as probed evidence.
func TestNoProberBaselineDoesNotPersistAssumedMetadata(t *testing.T) {
	uri := "virtual://movie/tt-noprober-persist?result=cand-1"
	stored := &models.MediaFile{
		ID:                         305,
		ContentID:                  "movie-1",
		FilePath:                   uri,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	lister := VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "cand-np", URI: uri, CodecAudio: "aac", Container: "mkv",
		}}, nil
	})
	var persisted bool
	saver := func(_ context.Context, _ models.VirtualFilePersistArgs) (int64, error) {
		persisted = true
		return 1, nil
	}
	h := virtualProbeGateCandidateHandler(stored, lister, nil, saver)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := *stored
	resolved, err := h.resolveVirtualPlaybackSource(req, &file, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if !resolved.ResolutionAssumed {
		t.Error("no-prober baseline must set ResolutionAssumed")
	}
	if resolved.File == nil || resolved.File.Resolution != "1080p" {
		t.Fatalf("resolution=%v, want 1080p baseline", resolved.File)
	}
	if persisted {
		t.Error("no-prober baseline must not trigger metadata persistence")
	}
}

func timePtr(t time.Time) *time.Time { return &t }

// A failed probe consumed the whole probe budget; the next replan must not pay
// it again for the same candidate. The second call skips the prober and falls
// back to the candidate-declared metadata.
func TestResolveVirtualProbeFailureDamperSkipsRepeatProbe(t *testing.T) {
	uri := "virtual://movie/tt-negative-cache?result=cand-1"
	key := virtualProbeFailureKey(uri, 5)
	virtualProbeFailures.clear(key)
	t.Cleanup(func() { virtualProbeFailures.clear(key) })

	stored := &models.MediaFile{
		ID:                         300,
		ContentID:                  "movie-1",
		FilePath:                   uri,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	lister := VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "cand-neg", URI: uri, Resolution: "1080p",
			CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
		}}, nil
	})
	probeCalls := 0
	h := virtualProbeGateCandidateHandler(stored, lister,
		func(_ context.Context, _ string, _ *models.MediaFile) (*models.MediaFile, error) {
			probeCalls++
			return nil, errors.New("probe failed")
		}, nil)

	for call := 0; call < 2; call++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
		file := *stored
		resolved, err := h.resolveVirtualPlaybackSource(req, &file, "profile-1", false, nil, "", "", 0, false)
		if err != nil {
			t.Fatalf("call %d: resolveVirtualPlaybackSource error: %v", call, err)
		}
		if resolved.Provenance != ProbeProvenanceFailed {
			t.Fatalf("call %d: provenance=%q, want failed declared fallback", call, resolved.Provenance)
		}
	}
	if probeCalls != 1 {
		t.Fatalf("prober called %d times across two replans, want 1 after the failure damper engages", probeCalls)
	}
}

// A successful probe must leave no failure marker behind, so a later start is
// not damped by stale state.
func TestResolveVirtualProbeSuccessClearsFailureMarker(t *testing.T) {
	uri := "virtual://movie/tt-negative-cache-ok?result=cand-1"
	key := virtualProbeFailureKey(uri, 5)
	virtualProbeFailures.clear(key)
	t.Cleanup(func() { virtualProbeFailures.clear(key) })

	stored := &models.MediaFile{
		ID:                         301,
		ContentID:                  "movie-1",
		FilePath:                   uri,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	lister := VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "cand-neg-ok", URI: uri, Resolution: "1080p",
			CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
		}}, nil
	})
	probeCalls := 0
	h := virtualProbeGateCandidateHandler(stored, lister,
		func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			probeCalls++
			f.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}}
			f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = "h264", "aac", "1080p", "mkv"
			return f, nil
		}, nil)

	// Seed a stale failure so the clear path is exercised after the probe.
	virtualProbeFailures.mu.Lock()
	virtualProbeFailures.marks[key] = virtualProbeFailureMark{
		ttl:       virtualProbeFailureTTL,
		expiresAt: time.Now().Add(-time.Minute),
	}
	virtualProbeFailures.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := *stored
	resolved, err := h.resolveVirtualPlaybackSource(req, &file, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.Provenance != ProbeProvenanceVerified {
		t.Fatalf("provenance=%q, want verified", resolved.Provenance)
	}
	if probeCalls != 1 {
		t.Fatalf("prober called %d times, want 1", probeCalls)
	}
	if virtualProbeFailures.recent(key) {
		t.Fatal("failure marker survived a successful probe")
	}
}

func TestVirtualProbeFailureCacheClearsMarker(t *testing.T) {
	key := "virtual://movie/tt-clear-marker"
	virtualProbeFailures.clear(key)
	t.Cleanup(func() { virtualProbeFailures.clear(key) })

	virtualProbeFailures.mark(key)
	if !virtualProbeFailures.recent(key) {
		t.Fatal("fresh failure marker is not recent")
	}
	virtualProbeFailures.clear(key)
	if virtualProbeFailures.recent(key) {
		t.Fatal("cleared failure marker is still recent")
	}
}

// virtualRepeatPlayFile is a virtual row that already carries a probe stamp and
// complete probed evidence: the state the repeat-play fast path recognizes.
func virtualRepeatPlayFile(path string) *models.MediaFile {
	probedAt := time.Now().Add(-time.Hour)
	return &models.MediaFile{
		ID:                         700,
		ContentID:                  "movie-repeat",
		FilePath:                   path,
		Container:                  "mkv",
		CodecVideo:                 "h264",
		CodecAudio:                 "aac",
		Resolution:                 "1080p",
		Bitrate:                    10_000,
		ProbeUpdatedAt:             &probedAt,
		VirtualOwnerInstallationID: 5,
		VideoTracks:                []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, FrameRate: "24", BitDepth: 8, Bitrate: 10_000}},
		AudioTracks:                []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}},
	}
}

// virtualRepeatPlayHandler wires detailed- and legacy-resolver spies. detailed
// counts every ResolveVirtualMediaDetailed invocation; the repeat-play fast
// path must leave it at zero on a replay. The legacy resolver is present only
// because resolveVirtualPlaybackSource requires one to be configured; the
// detailed resolver always takes precedence when both are set.
func virtualRepeatPlayHandler(detailedCalls, legacyCalls *int) *PlaybackHandler {
	return &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
			func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
				*legacyCalls++
				return "http://provider.example/legacy?path=" + path, nil
			}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(
			func(_ context.Context, virtualURI string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
				*detailedCalls++
				return ResolvedVirtualMedia{URL: "http://provider.example/stream.mkv", URI: virtualURI}, nil
			}),
	}
}

// A replay of a virtual row that already owns an adopted result= candidate,
// complete evidence, and a probe stamp must not call the provider resolver:
// the serve relay re-resolves and owns failover, so the start path only needs
// the persisted URI.
func TestResolveVirtualRepeatPlaySkipsProviderResolve(t *testing.T) {
	detailedCalls, legacyCalls := 0, 0
	h := virtualRepeatPlayHandler(&detailedCalls, &legacyCalls)
	file := virtualRepeatPlayFile("virtual://movie/tt-repeat?result=cand-1")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if detailedCalls != 0 || legacyCalls != 0 {
		t.Fatalf("resolvers called detailed=%d legacy=%d, want 0/0 on a repeat play", detailedCalls, legacyCalls)
	}
	if resolved.URL != "" {
		t.Fatalf("resolved URL = %q, want empty (serve relay owns resolution)", resolved.URL)
	}
	if resolved.URI != file.FilePath {
		t.Fatalf("resolved URI = %q, want persisted %q", resolved.URI, file.FilePath)
	}
	if resolved.Provenance != ProbeProvenancePending || resolved.ProbeSucceeded {
		t.Fatalf("provenance=%q succeeded=%v, want pending/false", resolved.Provenance, resolved.ProbeSucceeded)
	}
	if resolved.File == nil || resolved.File.FilePath != file.FilePath {
		t.Fatalf("resolved file = %#v, want persisted candidate", resolved.File)
	}
}

// A sticky pin plus a best-result cache hit clears noResult; the repeat-play
// fast path must then skip the resolver while returning the pinned URI.
func TestResolveVirtualRepeatPlaySkipsProviderResolveWithStickyPin(t *testing.T) {
	detailedCalls, legacyCalls := 0, 0
	h := virtualRepeatPlayHandler(&detailedCalls, &legacyCalls)
	h.BestResultCache = NewVirtualBestResultCache(time.Hour, 16)

	file := virtualRepeatPlayFile("virtual://movie/tt-repeat-pin")
	pinned := "virtual://movie/tt-repeat-pin?result=cand-9"
	neutral := virtualPlaybackNeutralKey(file.FilePath)
	stickyKey := bestResultCacheKey(file.ContentID, neutral, file.VirtualOwnerInstallationID, "")
	h.pinVirtualSticky(stickyKey, pinned)
	h.BestResultCache.set(bestResultCacheKey(file.ContentID, neutral, file.VirtualOwnerInstallationID, ""), []VirtualPlaybackStream{{
		URI: pinned, Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
	}}, time.Now())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if detailedCalls != 0 || legacyCalls != 0 {
		t.Fatalf("resolvers called detailed=%d legacy=%d, want 0/0 for a pinned candidate", detailedCalls, legacyCalls)
	}
	if resolved.URI != pinned {
		t.Fatalf("resolved URI = %q, want pinned %q", resolved.URI, pinned)
	}
}

// The fast path is start-only. forceRelist (explicit re-selection), noResult
// (neutral row with no adopted pick), incomplete evidence, and a missing probe
// stamp must all keep resolving synchronously.
func TestResolveVirtualRepeatPlayStillResolvesWhenRequired(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		deferProbe  bool
		forceRelist bool
		mutate      func(*models.MediaFile)
		wantCalls   int
	}{
		{
			name: "forceRelist", path: "virtual://movie/tt-repeat?result=cand-1",
			deferProbe: true, forceRelist: true, wantCalls: 1,
		},
		{
			name: "noResult", path: "virtual://movie/tt-repeat",
			deferProbe: true, wantCalls: 1,
		},
		{
			name: "evidenceIncomplete", path: "virtual://movie/tt-repeat?result=cand-1",
			deferProbe: true, mutate: func(f *models.MediaFile) { f.AudioTracks = nil }, wantCalls: 1,
		},
		{
			name: "probeMissing", path: "virtual://movie/tt-repeat?result=cand-1",
			deferProbe: true, mutate: func(f *models.MediaFile) { f.ProbeUpdatedAt = nil }, wantCalls: 1,
		},
		{
			name: "synchronousDeferProbeFalse", path: "virtual://movie/tt-repeat?result=cand-1",
			deferProbe: false, wantCalls: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detailedCalls, legacyCalls := 0, 0
			h := virtualRepeatPlayHandler(&detailedCalls, &legacyCalls)
			file := virtualRepeatPlayFile(tc.path)
			if tc.mutate != nil {
				tc.mutate(file)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
			if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", tc.deferProbe, nil, "", "", 0, tc.forceRelist); err != nil {
				t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
			}
			if detailedCalls != tc.wantCalls {
				t.Fatalf("detailed resolver called %d times, want %d", detailedCalls, tc.wantCalls)
			}
			if legacyCalls != 0 {
				t.Fatalf("legacy resolver called %d times, want 0", legacyCalls)
			}
		})
	}
}

// The damper backs off exponentially: each consecutive failure for the same
// key doubles the window, capped at virtualProbeFailureMaxTTL. The stored
// expiry must be derived from the injected clock, not the wall clock.
func TestVirtualProbeFailureCacheBackoffGrowsAndCaps(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cache := &virtualProbeFailureCache{
		marks: make(map[string]virtualProbeFailureMark),
		now:   func() time.Time { return now },
	}
	key := "virtual://movie/tt-backoff"

	cache.mark(key)
	first := cache.marks[key]
	if first.ttl != virtualProbeFailureTTL {
		t.Fatalf("first failure ttl=%s, want %s", first.ttl, virtualProbeFailureTTL)
	}
	if want := now.Add(virtualProbeFailureTTL); !first.expiresAt.Equal(want) {
		t.Fatalf("first failure expiresAt=%s, want %s", first.expiresAt, want)
	}

	// A second failure, after the first window lapses but while the marker is
	// still retained, extends the stored expiry to 10 minutes.
	now = now.Add(virtualProbeFailureTTL + time.Minute)
	cache.mark(key)
	second := cache.marks[key]
	if second.ttl != 2*virtualProbeFailureTTL {
		t.Fatalf("second failure ttl=%s, want %s", second.ttl, 2*virtualProbeFailureTTL)
	}
	if !second.expiresAt.After(first.expiresAt) {
		t.Fatalf("second failure expiresAt=%s did not grow past first %s", second.expiresAt, first.expiresAt)
	}

	// Keep failing: the window doubles until it hits the cap and stays there.
	for i := 0; i < 6; i++ {
		now = now.Add(cache.marks[key].ttl)
		cache.mark(key)
	}
	if got := cache.marks[key].ttl; got != virtualProbeFailureMaxTTL {
		t.Fatalf("capped ttl=%s, want %s", got, virtualProbeFailureMaxTTL)
	}
}

// recent honors the per-key backoff window: it is true inside the window and
// false once the stored expiry passes, dropping the marker on read.
func TestVirtualProbeFailureCacheRecentHonorsBackoff(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cache := &virtualProbeFailureCache{
		marks: make(map[string]virtualProbeFailureMark),
		now:   func() time.Time { return now },
	}
	key := "virtual://movie/tt-backoff-recent"

	cache.mark(key)
	if !cache.recent(key) {
		t.Fatal("fresh marker is not recent")
	}
	now = now.Add(virtualProbeFailureTTL - time.Second)
	if !cache.recent(key) {
		t.Fatal("marker inside its window is not recent")
	}
	now = now.Add(2 * time.Second)
	if cache.recent(key) {
		t.Fatal("marker past its window is still recent")
	}
	if _, ok := cache.marks[key]; ok {
		t.Fatal("expired marker was not pruned on read")
	}
}

// virtualOptimisticFile is a virtual row that has never been probed (no probe
// stamp, no stored tracks) but may carry recent delivery evidence: the state
// the optimistic-start gate recognizes.
func virtualOptimisticFile(path string, deliveredAt *time.Time) *models.MediaFile {
	return &models.MediaFile{
		ID:                         901,
		ContentID:                  "movie-optimistic",
		FilePath:                   path,
		Container:                  "virtual",
		LastDeliveredAt:            deliveredAt,
		VirtualOwnerInstallationID: 5,
		// The optimistic fast path now requires planner-grade video evidence;
		// the delivery grace alone only proves the bytes flowed once.
		CodecVideo: "h264",
		Resolution: "1080p",
		Bitrate:    10_000,
		VideoTracks: []models.VideoTrack{{
			Codec: "h264", Width: 1920, Height: 1080, FrameRate: "24", BitDepth: 8, Bitrate: 10_000,
		}},
	}
}

// virtualOptimisticGateHandler wires a non-blocking resolver spy and a
// permissive prober so the synchronous-resolve fallback is observable by call
// count.
func virtualOptimisticGateHandler(detailedCalls *int32) *PlaybackHandler {
	return &PlaybackHandler{
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(
			func(_ context.Context, virtualURI string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
				atomic.AddInt32(detailedCalls, 1)
				return ResolvedVirtualMedia{URL: "http://provider.example/stream.mkv", URI: virtualURI}, nil
			}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
			func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
				return "http://provider.example/legacy?path=" + path, nil
			}),
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			f.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}}
			f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = "h264", "aac", "1080p", "mkv"
			return f, nil
		},
	}
}

// A row that delivered bytes recently starts optimistically even when the
// probe stamp is missing: the start path returns the persisted URI without
// waiting on the provider, and the background chain resolves and probes so the
// next start takes the P0 fast path. The resolver is held open, so the start
// can only return if it never called it synchronously.
func TestResolveVirtualOptimisticStartWithinDeliveryGrace(t *testing.T) {
	uri := "virtual://movie/tt-optimistic?result=cand-1"
	deliveredAt := time.Now().Add(-time.Hour)
	stored := virtualOptimisticFile(uri, &deliveredAt)

	saverDone := make(chan string, 1)
	resolverStarted := make(chan struct{}, 1)
	releaseResolver := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseResolver) }) }
	t.Cleanup(release)

	var detailedCalls int32
	h := &PlaybackHandler{
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(
			func(_ context.Context, virtualURI string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
				atomic.AddInt32(&detailedCalls, 1)
				resolverStarted <- struct{}{}
				<-releaseResolver
				return ResolvedVirtualMedia{URL: "http://provider.example/stream.mkv", URI: virtualURI}, nil
			}),
		// resolveVirtualPlaybackSource requires the legacy resolver to be
		// configured even when the detailed resolver takes precedence.
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
			func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
				return "http://provider.example/legacy?path=" + path, nil
			}),
		VirtualFileLookup: func(_ context.Context, _ string) (*models.MediaFile, error) {
			return stored, nil
		},
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			f.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}}
			f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = "h264", "aac", "1080p", "mkv"
			return f, nil
		},
		VirtualFileSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
			saverDone <- args.ExpectedFilePath
			return 1, nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := *stored
	type startResult struct {
		src resolvedVirtualPlaybackSource
		err error
	}
	started := make(chan startResult, 1)
	go func() {
		src, err := h.resolveVirtualPlaybackSource(req, &file, "profile-1", true, nil, "", "", 0, false)
		started <- startResult{src, err}
	}()

	var resolved resolvedVirtualPlaybackSource
	select {
	case r := <-started:
		if r.err != nil {
			t.Fatalf("resolveVirtualPlaybackSource error: %v", r.err)
		}
		resolved = r.src
	case <-time.After(2 * time.Second):
		t.Fatal("start path blocked on the synchronous resolver despite recent delivery")
	}

	if resolved.URL != "" {
		t.Fatalf("resolved URL = %q, want empty optimistic start", resolved.URL)
	}
	if resolved.URI != uri {
		t.Fatalf("resolved URI = %q, want persisted %q", resolved.URI, uri)
	}
	if resolved.Provenance != ProbeProvenancePending || resolved.ProbeSucceeded {
		t.Fatalf("provenance=%q succeeded=%v, want pending/false", resolved.Provenance, resolved.ProbeSucceeded)
	}
	if resolved.File == nil || resolved.File.FilePath != uri {
		t.Fatalf("resolved file = %#v, want persisted candidate %q", resolved.File, uri)
	}

	// The optimistic return must still kick the background resolve+probe chain.
	select {
	case <-resolverStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("optimistic start did not kick the background resolver")
	}
	release()
	select {
	case got := <-saverDone:
		if got != uri {
			t.Fatalf("background persist path = %q, want %q", got, uri)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background revalidation did not persist probed metadata")
	}
}

// No adopted result= and no sticky pin means no persisted candidate: the
// optimistic gate must not apply even inside the delivery grace, so the start
// still resolves synchronously.
func TestResolveVirtualOptimisticRequiresPinnedCandidate(t *testing.T) {
	deliveredAt := time.Now().Add(-time.Hour)
	file := virtualOptimisticFile("virtual://movie/tt-no-pin", &deliveredAt)
	var detailedCalls int32
	h := virtualOptimisticGateHandler(&detailedCalls)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false); err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if got := atomic.LoadInt32(&detailedCalls); got != 1 {
		t.Fatalf("detailed resolver called %d times, want 1 without a pinned/adopted candidate", got)
	}
}

// Delivery older than the grace window is not evidence the candidate is still
// good, so the start must resolve synchronously instead of starting blind.
func TestResolveVirtualOptimisticRequiresDeliveryGrace(t *testing.T) {
	stale := time.Now().Add(-8 * 24 * time.Hour)
	file := virtualOptimisticFile("virtual://movie/tt-stale-delivery?result=cand-1", &stale)
	var detailedCalls int32
	h := virtualOptimisticGateHandler(&detailedCalls)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false); err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if got := atomic.LoadInt32(&detailedCalls); got != 1 {
		t.Fatalf("detailed resolver called %d times, want 1 once delivery grace lapsed", got)
	}
}

// The optimistic gate is start-only. A synchronous replan/alternate caller
// (deferProbe=false) still resolves even with recent delivery evidence.
func TestResolveVirtualOptimisticOnlyOnDeferredStart(t *testing.T) {
	deliveredAt := time.Now().Add(-time.Hour)
	file := virtualOptimisticFile("virtual://movie/tt-sync-start?result=cand-1", &deliveredAt)
	var detailedCalls int32
	h := virtualOptimisticGateHandler(&detailedCalls)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false); err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if got := atomic.LoadInt32(&detailedCalls); got != 1 {
		t.Fatalf("detailed resolver called %d times, want 1 for a synchronous caller", got)
	}
}

// The delivery grace proves the evidence owner's bytes flowed — never a
// freshly ranked sibling's. A candidate list that re-ranks a different release
// to the iterated position must not inherit the row's probed inventory through
// the optimistic gate; it resolves synchronously instead.
func TestResolveVirtualOptimisticRequiresExactEvidenceOwner(t *testing.T) {
	deliveredAt := time.Now().Add(-time.Hour)
	uri := "virtual://movie/tt-optimistic?result=cand-1"
	file := virtualOptimisticFile(uri, &deliveredAt)
	// A neutral-row start (no ?result= on the row) whose sticky pin names a
	// different release than the row evidence would otherwise let the gate
	// bind cand-2 with cand-1's inventory. The gate must refuse: the grace
	// proves cand-1's bytes flowed, not cand-2's.
	neutral := *file
	neutral.FilePath = "virtual://movie/tt-optimistic"
	neutral.LastDeliveredAt = &deliveredAt
	var detailedCalls int32
	h := virtualOptimisticGateHandler(&detailedCalls)
	h.pinVirtualSticky(
		bestResultCacheKey(neutral.ContentID, virtualPlaybackNeutralKey(neutral.FilePath), neutral.VirtualOwnerInstallationID, ""),
		"virtual://movie/tt-optimistic?result=cand-2",
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	if _, err := h.resolveVirtualPlaybackSource(req, &neutral, "profile-1", true, nil, "", "", 0, false); err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if got := atomic.LoadInt32(&detailedCalls); got != 1 {
		t.Fatalf("detailed resolver called %d times, want 1 synchronous resolve for a non-owner candidate", got)
	}
}

// resolveVirtualDamperFile builds a virtual row pointing at one provider
// candidate under a shared neutral path.
func resolveVirtualDamperFile(owner int, id int, uri string) *models.MediaFile {
	return &models.MediaFile{
		ID:                         id,
		ContentID:                  "movie-damper",
		FilePath:                   uri,
		Container:                  "virtual",
		VirtualOwnerInstallationID: owner,
	}
}

func resolveVirtualDamperLister(uri string) VirtualPlaybackStreamLister {
	return VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "cand", URI: uri, Resolution: "1080p",
			CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
		}}, nil
	})
}

// A probe failure recorded for one provider candidate must not damp a different
// candidate that happens to share the same neutral (result-less) virtual path:
// the provider rotates result= hashes for the same release, and a dead hash
// must not hide a healthy sibling.
func TestResolveVirtualProbeDamperIsolatesCandidates(t *testing.T) {
	const owner = 5
	uriA := "virtual://movie/tt-damper-cand-isolation?result=cand-a"
	uriB := "virtual://movie/tt-damper-cand-isolation?result=cand-b"

	resolve := func(h *PlaybackHandler, file *models.MediaFile) resolvedVirtualPlaybackSource {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
		resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false)
		if err != nil {
			t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
		}
		return resolved
	}

	// Candidate A fails its probe; the failure is remembered for A only.
	probeACalls := 0
	hA := virtualProbeGateCandidateHandler(resolveVirtualDamperFile(owner, 910, uriA), resolveVirtualDamperLister(uriA),
		func(_ context.Context, _ string, _ *models.MediaFile) (*models.MediaFile, error) {
			probeACalls++
			return nil, errors.New("probe A failed")
		}, nil)
	resolvedA := resolve(hA, resolveVirtualDamperFile(owner, 910, uriA))
	if probeACalls != 1 || resolvedA.Provenance != ProbeProvenanceFailed {
		t.Fatalf("candidate A: probeCalls=%d provenance=%q, want 1/failed", probeACalls, resolvedA.Provenance)
	}

	// Candidate B under the same neutral path must still be probed.
	probeBCalls := 0
	hB := virtualProbeGateCandidateHandler(resolveVirtualDamperFile(owner, 911, uriB), resolveVirtualDamperLister(uriB),
		func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			probeBCalls++
			f.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}}
			f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = "h264", "aac", "1080p", "mkv"
			return f, nil
		}, nil)
	resolvedB := resolve(hB, resolveVirtualDamperFile(owner, 911, uriB))
	if probeBCalls != 1 {
		t.Fatalf("candidate B probeCalls=%d, want 1: a failure for candidate A damped candidate B", probeBCalls)
	}
	if resolvedB.Provenance != ProbeProvenanceVerified {
		t.Fatalf("candidate B provenance=%q, want verified", resolvedB.Provenance)
	}
}

// The damper key must include installation ownership: the same candidate URI
// resolved for two owners is two independent probes.
func TestResolveVirtualProbeDamperIsolatesOwners(t *testing.T) {
	uri := "virtual://movie/tt-damper-owner-isolation?result=cand-1"

	resolve := func(h *PlaybackHandler, owner int) resolvedVirtualPlaybackSource {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
		resolved, err := h.resolveVirtualPlaybackSource(req, resolveVirtualDamperFile(owner, 920, uri), "profile-1", false, nil, "", "", 0, false)
		if err != nil {
			t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
		}
		return resolved
	}

	// Owner 5 exhausts its probe budget for this candidate.
	probeCalls5 := 0
	h5 := virtualProbeGateCandidateHandler(resolveVirtualDamperFile(5, 920, uri), resolveVirtualDamperLister(uri),
		func(_ context.Context, _ string, _ *models.MediaFile) (*models.MediaFile, error) {
			probeCalls5++
			return nil, errors.New("probe owner 5 failed")
		}, nil)
	if resolved := resolve(h5, 5); probeCalls5 != 1 || resolved.Provenance != ProbeProvenanceFailed {
		t.Fatalf("owner 5: probeCalls=%d provenance=%q, want 1/failed", probeCalls5, resolved.Provenance)
	}

	// Owner 6 must probe the same candidate independently.
	probeCalls6 := 0
	h6 := virtualProbeGateCandidateHandler(resolveVirtualDamperFile(6, 921, uri), resolveVirtualDamperLister(uri),
		func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			probeCalls6++
			f.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}}
			f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = "h264", "aac", "1080p", "mkv"
			return f, nil
		}, nil)
	resolved6 := resolve(h6, 6)
	if probeCalls6 != 1 {
		t.Fatalf("owner 6 probeCalls=%d, want 1: owner 5's failure damped owner 6", probeCalls6)
	}
	if resolved6.Provenance != ProbeProvenanceVerified {
		t.Fatalf("owner 6 provenance=%q, want verified", resolved6.Provenance)
	}
}

// The repeat-play fast path may only bind the candidate the row actually points
// at. A BestResultCache hit can flip noResult=false and rank a different
// release to index 0; without an identity check the fast path pins that
// different candidate and copies the row's probed inventory onto it.
func TestResolveVirtualRepeatPlayFastPathRequiresCandidateIdentity(t *testing.T) {
	t.Run("promotedCandidateMismatch", func(t *testing.T) {
		detailedCalls, legacyCalls := 0, 0
		h := virtualRepeatPlayHandler(&detailedCalls, &legacyCalls)
		h.BestResultCache = NewVirtualBestResultCache(time.Hour, 16)

		file := virtualRepeatPlayFile("virtual://movie/tt-best-mismatch")
		neutral := virtualPlaybackNeutralKey(file.FilePath)
		owner := file.VirtualOwnerInstallationID
		stickyKey := bestResultCacheKey(file.ContentID, neutral, owner, "")
		h.pinVirtualSticky(stickyKey, neutral+"?result=cand-pinned")
		promoted := neutral + "?result=cand-promoted"
		h.BestResultCache.set(bestResultCacheKey(file.ContentID, neutral, owner, ""), []VirtualPlaybackStream{{
			URI: promoted, Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
		}}, time.Now())

		req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
		resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
		if err != nil {
			t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
		}
		if detailedCalls != 1 {
			t.Fatalf("detailed resolver called %d times, want 1: fast path bound a candidate the row does not point at", detailedCalls)
		}
		if resolved.URI != promoted {
			t.Fatalf("resolved URI = %q, want resolved promoted candidate %q", resolved.URI, promoted)
		}
	})

	t.Run("persistedMatch", func(t *testing.T) {
		detailedCalls, legacyCalls := 0, 0
		h := virtualRepeatPlayHandler(&detailedCalls, &legacyCalls)
		file := virtualRepeatPlayFile("virtual://movie/tt-best-match?result=cand-1")

		req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
		resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
		if err != nil {
			t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
		}
		if detailedCalls != 0 {
			t.Fatalf("detailed resolver called %d times, want 0 for a persisted match", detailedCalls)
		}
		if resolved.URI != file.FilePath {
			t.Fatalf("resolved URI = %q, want persisted %q", resolved.URI, file.FilePath)
		}
	})
}

// A candidate-declared 2160p must survive the resolution precedence gate: a
// stored row with empty Resolution adopts the candidate label, and only a
// truly-empty (stored + candidate) resolution falls back to the 1080p
// baseline. No-prober branch (prober nil, deferProbe false).
// A fresh auto start prefers a probed catalog row over an unprobed stub:
// the probed release is known-good, the stub is speculative. The preference
// only breaks ties inside each accepted/rejected group — a rejected probed
// row never jumps a healthy accepted stub. Explicit picks are untouched.
func TestPreferProbedCandidateOverUnprobedStub(t *testing.T) {
	probedAt := time.Now().Add(-time.Hour)
	probedURI := "virtual://movie/tt-probe-gate?result=cand-probed"
	stubURI := "virtual://movie/tt-probe-gate?result=cand-stub"
	probedRow := &models.MediaFile{
		ID:                         501,
		ContentID:                  "movie-1",
		FilePath:                   probedURI,
		Container:                  "mkv",
		CodecVideo:                 "hevc",
		CodecAudio:                 "dts",
		Resolution:                 "2160p",
		Bitrate:                    40_000,
		ProbeUpdatedAt:             &probedAt,
		VirtualOwnerInstallationID: 5,
		VideoTracks:                []models.VideoTrack{{Codec: "hevc", Width: 3840, Height: 2160, FrameRate: "23.976", BitDepth: 10, Bitrate: 40_000}},
		AudioTracks:                []models.AudioTrack{{Codec: "dts", Channels: 6, Language: "en", Default: true}},
	}
	stubRow := &models.MediaFile{
		ID:                         502,
		ContentID:                  "movie-1",
		FilePath:                   stubURI,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	byURI := map[string]*models.MediaFile{probedURI: probedRow, stubURI: stubRow}
	lister := VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{
			{ID: "cand-stub", URI: stubURI},
			{ID: "cand-probed", URI: probedURI, Resolution: "2160p", CodecVideo: "hevc", CodecAudio: "dts", Container: "mkv"},
		}, nil
	})
	h := virtualProbeGateCandidateHandler(stubRow, lister, nil, nil)
	h.VirtualFileLookup = func(_ context.Context, uri string) (*models.MediaFile, error) {
		if row, ok := byURI[uri]; ok {
			return row, nil
		}
		return nil, ErrVirtualCandidateNotFound
	}
	h.VirtualCandidateFileLookup = func(_ context.Context, _ string, _ string, _ string, _ int) (*models.MediaFile, error) {
		return nil, ErrVirtualCandidateNotFound
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := &models.MediaFile{
		ID:                         500,
		ContentID:                  "movie-1",
		FilePath:                   "virtual://movie/tt-probe-gate",
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.URI != probedURI {
		t.Fatalf("resolved URI = %q, want probed %q", resolved.URI, probedURI)
	}
	if resolved.CandidateRank != 0 {
		t.Fatalf("candidate rank = %d, want 0 for the preferred probed release", resolved.CandidateRank)
	}
}

func TestPreferProbedNeverJumpsRejectedGroup(t *testing.T) {
	probedAt := time.Now().Add(-time.Hour)
	probedURI := "virtual://movie/tt-probe-gate?result=cand-probed"
	stubURI := "virtual://movie/tt-probe-gate?result=cand-stub"
	probedRow := &models.MediaFile{
		ID:                         511,
		ContentID:                  "movie-1",
		FilePath:                   probedURI,
		Container:                  "mkv",
		CodecVideo:                 "hevc",
		CodecAudio:                 "dts",
		Resolution:                 "2160p",
		Bitrate:                    40_000,
		ProbeUpdatedAt:             &probedAt,
		VirtualOwnerInstallationID: 5,
		VideoTracks:                []models.VideoTrack{{Codec: "hevc", Width: 3840, Height: 2160, FrameRate: "23.976", BitDepth: 10, Bitrate: 40_000}},
		AudioTracks:                []models.AudioTrack{{Codec: "dts", Channels: 6, Language: "en", Default: true}},
	}
	stubRow := &models.MediaFile{
		ID:                         512,
		ContentID:                  "movie-1",
		FilePath:                   stubURI,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	byURI := map[string]*models.MediaFile{probedURI: probedRow, stubURI: stubRow}
	lister := VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{
			{ID: "cand-stub", URI: stubURI},
			{ID: "cand-probed", URI: probedURI, Resolution: "2160p", CodecVideo: "hevc", CodecAudio: "dts", Container: "mkv", Rejected: true},
		}, nil
	})
	h := virtualProbeGateCandidateHandler(stubRow, lister, nil, nil)
	h.VirtualFileLookup = func(_ context.Context, uri string) (*models.MediaFile, error) {
		if row, ok := byURI[uri]; ok {
			return row, nil
		}
		return nil, ErrVirtualCandidateNotFound
	}
	h.VirtualCandidateFileLookup = func(_ context.Context, _ string, _ string, _ string, _ int) (*models.MediaFile, error) {
		return nil, ErrVirtualCandidateNotFound
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := &models.MediaFile{
		ID:                         510,
		ContentID:                  "movie-1",
		FilePath:                   "virtual://movie/tt-probe-gate",
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.URI != stubURI {
		t.Fatalf("resolved URI = %q, want accepted stub %q (rejected probed must not jump groups)", resolved.URI, stubURI)
	}
}

func TestCandidateResolutionPreferredOverBaseline(t *testing.T) {
	uri := "virtual://movie/tt-cand-res-pref?result=cand-2160p"
	stored := &models.MediaFile{
		ID:                         401,
		ContentID:                  "movie-1",
		FilePath:                   uri,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	lister := VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "cand-2160p", URI: uri, Resolution: "2160p",
			CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
		}}, nil
	})
	h := virtualProbeGateCandidateHandler(stored, lister, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := *stored
	resolved, err := h.resolveVirtualPlaybackSource(req, &file, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.File == nil || resolved.File.Resolution != "2160p" {
		t.Fatalf("resolution=%v, want declared 2160p (must not be clobbered to 1080p baseline)", resolved.File)
	}
	if resolved.ResolutionAssumed {
		t.Error("candidate-declared 2160p must not set ResolutionAssumed")
	}
	if resolved.Provenance != ProbeProvenanceDeclared {
		t.Errorf("provenance=%q, want declared", resolved.Provenance)
	}
}

// Track evidence wins over candidate blobs: existing hevc/eac3 tracks must
// not be overwritten by candidate h264/aac declarations, and a second merge
// must be a no-op.
func TestTrackCodecPreferredOverCandidate(t *testing.T) {
	file := &models.MediaFile{
		Resolution:  "1080p",
		VideoTracks: []models.VideoTrack{{Codec: "hevc", Width: 3840, Height: 2160}},
		AudioTracks: []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "eng"}},
	}
	candidate := VirtualPlaybackStream{
		Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
	}

	mergeVirtualCandidateTracks(file, candidate)
	if file.CodecVideo != "hevc" {
		t.Fatalf("CodecVideo=%q, want hevc from track evidence", file.CodecVideo)
	}
	if file.CodecAudio != "eac3" {
		t.Fatalf("CodecAudio=%q, want eac3 from track evidence", file.CodecAudio)
	}

	beforeVideo, beforeAudio := file.CodecVideo, file.CodecAudio
	beforeVideoTracks, beforeAudioTracks := len(file.VideoTracks), len(file.AudioTracks)
	beforeVideoCodec, beforeAudioCodec := file.VideoTracks[0].Codec, file.AudioTracks[0].Codec

	mergeVirtualCandidateTracks(file, candidate)
	if file.CodecVideo != beforeVideo || file.CodecAudio != beforeAudio {
		t.Fatalf("second merge changed codecs to %q/%q, want %q/%q", file.CodecVideo, file.CodecAudio, beforeVideo, beforeAudio)
	}
	if len(file.VideoTracks) != beforeVideoTracks || len(file.AudioTracks) != beforeAudioTracks {
		t.Fatalf("second merge changed track counts to %d/%d, want %d/%d",
			len(file.VideoTracks), len(file.AudioTracks), beforeVideoTracks, beforeAudioTracks)
	}
	if file.VideoTracks[0].Codec != beforeVideoCodec || file.AudioTracks[0].Codec != beforeAudioCodec {
		t.Fatalf("second merge changed track codecs to %q/%q, want %q/%q",
			file.VideoTracks[0].Codec, file.AudioTracks[0].Codec, beforeVideoCodec, beforeAudioCodec)
	}
}

// Repeated merges with the same candidate must converge: fields and track
// slices after the 1st vs 3rd call must be identical.
func TestMergeIdempotentAcrossRepeatedCandidates(t *testing.T) {
	candidate := VirtualPlaybackStream{
		Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
		AudioLanguages: []string{"eng"},
	}
	file := &models.MediaFile{}

	mergeVirtualCandidateTracks(file, candidate)
	if len(file.VideoTracks) == 0 || len(file.AudioTracks) == 0 {
		t.Fatalf("first merge produced no tracks: %#v", file)
	}
	snapVideo, snapAudio := file.CodecVideo, file.CodecAudio
	snapContainer, snapResolution := file.Container, file.Resolution
	snapVideoLen, snapAudioLen := len(file.VideoTracks), len(file.AudioTracks)
	snapVideoCodec, snapAudioCodec := file.VideoTracks[0].Codec, file.AudioTracks[0].Codec

	mergeVirtualCandidateTracks(file, candidate)
	mergeVirtualCandidateTracks(file, candidate)

	if file.CodecVideo != snapVideo {
		t.Errorf("CodecVideo=%q after 3 merges, want %q after 1", file.CodecVideo, snapVideo)
	}
	if file.CodecAudio != snapAudio {
		t.Errorf("CodecAudio=%q after 3 merges, want %q after 1", file.CodecAudio, snapAudio)
	}
	if file.Container != snapContainer {
		t.Errorf("Container=%q after 3 merges, want %q after 1", file.Container, snapContainer)
	}
	if file.Resolution != snapResolution {
		t.Errorf("Resolution=%q after 3 merges, want %q after 1", file.Resolution, snapResolution)
	}
	if len(file.VideoTracks) != snapVideoLen {
		t.Fatalf("VideoTracks=%d after 3 merges, want %d after 1", len(file.VideoTracks), snapVideoLen)
	}
	if len(file.AudioTracks) != snapAudioLen {
		t.Fatalf("AudioTracks=%d after 3 merges, want %d after 1", len(file.AudioTracks), snapAudioLen)
	}
	if file.VideoTracks[0].Codec != snapVideoCodec {
		t.Errorf("VideoTracks[0].Codec=%q after 3 merges, want %q after 1", file.VideoTracks[0].Codec, snapVideoCodec)
	}
	if file.AudioTracks[0].Codec != snapAudioCodec {
		t.Errorf("AudioTracks[0].Codec=%q after 3 merges, want %q after 1", file.AudioTracks[0].Codec, snapAudioCodec)
	}
}

// No-prober declared path persists DECLARED (non-assumed) metadata: the
// persistence gate in playback_virtual.go (resolve loop, `!ResolutionAssumed
// && prober == nil`) persists declared candidates, unlike
// the assumed-baseline gate which skips persistence. The saver runs in a
// background goroutine via persistVirtualMetadataBounded, so the test waits
// for the async call.
func TestDeclaredNoProberCandidateResolutionPersists(t *testing.T) {
	uri := "virtual://movie/tt-noprober-declared-persist?result=cand-2160p"
	stored := &models.MediaFile{
		ID:                         402,
		ContentID:                  "movie-1",
		FilePath:                   uri,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	lister := VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "cand-2160p", URI: uri, Resolution: "2160p",
			CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
		}}, nil
	})
	var saverCalls int32
	saverDone := make(chan struct{}, 1)
	saver := func(_ context.Context, _ models.VirtualFilePersistArgs) (int64, error) {
		atomic.AddInt32(&saverCalls, 1)
		select {
		case saverDone <- struct{}{}:
		default:
		}
		return 1, nil
	}
	h := virtualProbeGateCandidateHandler(stored, lister, nil, saver)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := *stored
	resolved, err := h.resolveVirtualPlaybackSource(req, &file, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.File == nil || resolved.File.Resolution != "2160p" {
		t.Fatalf("resolution=%v, want declared 2160p", resolved.File)
	}
	if resolved.ResolutionAssumed {
		t.Error("candidate-declared 2160p must not set ResolutionAssumed")
	}
	select {
	case <-saverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("declared non-assumed metadata was not persisted via saver")
	}
	if got := atomic.LoadInt32(&saverCalls); got != 1 {
		t.Fatalf("saver called %d times, want 1 for declared metadata", got)
	}
}

// Table-driven test for mergeVirtualCandidateTracks codec precedence:
// existing top-level wins; if empty, first track; then candidate; then default.
func TestMergeVirtualCandidateTracksCodecPrecedence(t *testing.T) {
	tests := []struct {
		name           string
		topLevelVideo  string // initial CodecVideo
		topLevelAudio  string // initial CodecAudio
		trackVideo     string // first video track codec (empty = no track)
		trackAudio     string // first audio track codec (empty = no track)
		candVideo      string
		candAudio      string
		resolution     string
		wantVideo      string
		wantAudio      string
		wantTrackCount int // expected len(VideoTracks) after merge
	}{
		{
			name:           "existing top-level wins over contradictory track and candidate",
			topLevelVideo:  "hevc",
			topLevelAudio:  "eac3",
			trackVideo:     "h264",
			trackAudio:     "aac",
			candVideo:      "av1",
			candAudio:      "opus",
			resolution:     "2160p",
			wantVideo:      "hevc",
			wantAudio:      "eac3",
			wantTrackCount: 1, // existing track preserved, no synthesized track
		},
		{
			name:           "empty top-level uses first track, ignores candidate",
			topLevelVideo:  "",
			topLevelAudio:  "",
			trackVideo:     "hevc",
			trackAudio:     "eac3",
			candVideo:      "h264",
			candAudio:      "aac",
			resolution:     "2160p",
			wantVideo:      "hevc",
			wantAudio:      "eac3",
			wantTrackCount: 1,
		},
		{
			name:           "empty top-level and track uses candidate",
			topLevelVideo:  "",
			topLevelAudio:  "",
			trackVideo:     "",
			trackAudio:     "",
			candVideo:      "h264",
			candAudio:      "aac",
			resolution:     "1080p",
			wantVideo:      "h264",
			wantAudio:      "aac",
			wantTrackCount: 1, // synthesized video track
		},
		{
			name:           "empty everything with resolution defaults",
			topLevelVideo:  "",
			topLevelAudio:  "",
			trackVideo:     "",
			trackAudio:     "",
			candVideo:      "",
			candAudio:      "",
			resolution:     "1080p",
			wantVideo:      "h264",
			wantAudio:      "aac",
			wantTrackCount: 1,
		},
		{
			name:           "empty everything without resolution stays empty",
			topLevelVideo:  "",
			topLevelAudio:  "",
			trackVideo:     "",
			trackAudio:     "",
			candVideo:      "",
			candAudio:      "",
			resolution:     "",
			wantVideo:      "",
			wantAudio:      "",
			wantTrackCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := &models.MediaFile{Resolution: tt.resolution}
			if tt.topLevelVideo != "" {
				file.CodecVideo = tt.topLevelVideo
			}
			if tt.topLevelAudio != "" {
				file.CodecAudio = tt.topLevelAudio
			}
			if tt.trackVideo != "" {
				file.VideoTracks = []models.VideoTrack{{Codec: tt.trackVideo}}
			}
			if tt.trackAudio != "" {
				file.AudioTracks = []models.AudioTrack{{Codec: tt.trackAudio}}
			}

			candidate := VirtualPlaybackStream{
				Resolution: tt.resolution,
				CodecVideo: tt.candVideo,
				CodecAudio: tt.candAudio,
				Container:  "mkv",
			}

			mergeVirtualCandidateTracks(file, candidate)

			if file.CodecVideo != tt.wantVideo {
				t.Errorf("CodecVideo = %q, want %q", file.CodecVideo, tt.wantVideo)
			}
			if file.CodecAudio != tt.wantAudio {
				t.Errorf("CodecAudio = %q, want %q", file.CodecAudio, tt.wantAudio)
			}
			if len(file.VideoTracks) != tt.wantTrackCount {
				t.Errorf("VideoTracks = %d, want %d", len(file.VideoTracks), tt.wantTrackCount)
			}

			// Idempotency: merge again, result must not change
			mergeVirtualCandidateTracks(file, candidate)
			if file.CodecVideo != tt.wantVideo {
				t.Errorf("after second merge: CodecVideo = %q, want %q", file.CodecVideo, tt.wantVideo)
			}
			if file.CodecAudio != tt.wantAudio {
				t.Errorf("after second merge: CodecAudio = %q, want %q", file.CodecAudio, tt.wantAudio)
			}
			if len(file.VideoTracks) != tt.wantTrackCount {
				t.Errorf("after second merge: VideoTracks = %d, want %d", len(file.VideoTracks), tt.wantTrackCount)
			}
		})
	}
}
