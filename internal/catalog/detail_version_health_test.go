package catalog

import (
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// The detail projection carries the durable liveness state and the injected
// wrong-source verdict per version, next to the audio/subtitle inventory a
// client needs to pick a row without a second request. The projection only
// reads the verdict source: it never invents a failure stamp.
func TestBuildPlaybackInfoProjectsVersionHealthAndInventory(t *testing.T) {
	probedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	failedAt := time.Date(2026, 3, 4, 6, 0, 0, 0, time.UTC)
	files := []*models.MediaFile{
		{
			ID: 1, ContentID: "movie:health", MediaFolderID: 1,
			FilePath: "virtual://movie/tt-health?result=a", Container: "virtual", FileSize: 100,
			ProbeUpdatedAt: &probedAt,
			AudioTracks:    []models.AudioTrack{{Language: "en", Codec: "truehd", Channels: 8}},
			SubtitleTracks: []models.SubtitleTrack{{Language: "en", Codec: "srt"}},
		},
		{
			ID: 2, ContentID: "movie:health", MediaFolderID: 1,
			FilePath: "virtual://movie/tt-health?result=b", Container: "virtual", FileSize: 200,
			FailedAt:    &failedAt,
			AudioTracks: []models.AudioTrack{{Language: "fr", Codec: "eac3"}},
		},
	}

	svc := &DetailService{}
	svc.SetVersionSourceMismatchSource(func(file *models.MediaFile) *VersionSourceMismatch {
		if file.ID != 1 {
			return nil
		}
		return &VersionSourceMismatch{ServedBytes: 50, DeclaredBytes: 100}
	})

	versions, _, _, _, _, _, _ := svc.buildPlaybackInfo(t.Context(), files, AccessFilter{}, "")
	if len(versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(versions))
	}
	byID := make(map[int]FileVersion, len(versions))
	for _, version := range versions {
		byID[version.FileID] = version
	}

	live := byID[1]
	if live.Failed {
		t.Fatalf("live row reported failed: %+v", live)
	}
	if live.Available != nil {
		t.Fatalf("live virtual row must omit available: %+v", live)
	}
	if live.ProbedAt == nil || !live.ProbedAt.Equal(probedAt) {
		t.Fatalf("probed_at = %v, want %v", live.ProbedAt, probedAt)
	}
	if live.SourceMismatch == nil || live.SourceMismatch.ServedBytes != 50 || live.SourceMismatch.DeclaredBytes != 100 {
		t.Fatalf("source_mismatch = %+v", live.SourceMismatch)
	}
	if len(live.AudioTracks) != 1 || live.AudioTracks[0].Codec != "truehd" || live.AudioTracks[0].Channels != 8 {
		t.Fatalf("audio_tracks = %+v", live.AudioTracks)
	}
	if len(live.SubtitleTracks) != 1 || live.SubtitleTracks[0].Codec != "srt" {
		t.Fatalf("subtitle_tracks = %+v", live.SubtitleTracks)
	}
	if live.EffectiveAudioTrackIndex == nil || live.EffectiveAudioLanguage == "" {
		t.Fatalf("effective audio not populated: %+v", live)
	}

	dead := byID[2]
	if !dead.Failed {
		t.Fatalf("dead row must report failed: %+v", dead)
	}
	if dead.Available == nil || *dead.Available {
		t.Fatalf("dead row must report available=false: %+v", dead)
	}
	if dead.SourceMismatch != nil {
		t.Fatalf("dead row must not carry the verdict of another row: %+v", dead.SourceMismatch)
	}
	if len(dead.AudioTracks) != 1 || dead.AudioTracks[0].Codec != "eac3" {
		t.Fatalf("dead row audio_tracks = %+v", dead.AudioTracks)
	}
}

// Without an injected verdict source every version stays source_mismatch-free
// rather than defaulting to a fabricated state.
func TestBuildPlaybackInfoOmitsSourceMismatchWithoutSource(t *testing.T) {
	versions, _, _, _, _, _, _ := (&DetailService{}).buildPlaybackInfo(t.Context(), []*models.MediaFile{{
		ID: 1, ContentID: "movie:health", MediaFolderID: 1,
		FilePath: "virtual://movie/tt-health?result=a", Container: "virtual", FileSize: 100,
	}}, AccessFilter{}, "")
	if len(versions) != 1 {
		t.Fatalf("versions = %d, want 1", len(versions))
	}
	if versions[0].SourceMismatch != nil {
		t.Fatalf("source_mismatch = %+v without a verdict source", versions[0].SourceMismatch)
	}
}
