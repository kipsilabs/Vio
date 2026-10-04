package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// virtualDupRow builds a virtual catalog row for the duplicate-collapse tests.
// probed controls whether the row carries probe evidence and an audio track
// whose language identifies it in the projected version.
func virtualDupRow(id int, size int64, path, language string, probed bool) *models.MediaFile {
	file := &models.MediaFile{
		ID:             id,
		ContentID:      "movie-dup",
		FilePath:       path,
		Container:      "virtual",
		FileSize:       size,
		Duration:       3600,
		Resolution:     "1080p",
		CodecVideo:     "h264",
		CodecAudio:     "aac",
		AudioTracks:    []models.AudioTrack{{Codec: "aac", Language: language}},
		SubtitleTracks: []models.SubtitleTrack{},
	}
	if probed {
		probedAt := time.Now()
		file.ProbeUpdatedAt = &probedAt
	}
	return file
}

// buildDupVersions projects the supplied rows through the shared version builder
// with no repositories, which is sufficient for the pure collapse logic.
func buildDupVersions(t *testing.T, files []*models.MediaFile) []FileVersion {
	t.Helper()
	svc := &DetailService{}
	versions, _, _, _, _, _, _ := svc.buildPlaybackInfo(context.Background(), files, AccessFilter{}, "movie-dup")
	return versions
}

// TestBuildPlaybackInfoCollapsesByteIdenticalDuplicateRows pins the primary
// dedup: two catalog rows for one release that share content, size and concrete
// candidate URI must list once. Distinct rows of one release otherwise appear as
// duplicate versions and let a later serve pick the wrong sibling.
func TestBuildPlaybackInfoCollapsesByteIdenticalDuplicateRows(t *testing.T) {
	uri := "virtual://movie/dup?result=a"
	files := []*models.MediaFile{
		virtualDupRow(11, 1000, uri, "eng", true),
		virtualDupRow(12, 1000, uri, "eng", true),
	}
	versions := buildDupVersions(t, files)
	if len(versions) != 1 {
		t.Fatalf("byte-identical duplicates projected %d versions, want 1", len(versions))
	}
	if versions[0].FileID != 11 {
		t.Fatalf("collapsed to file %d, want the first occurrence 11", versions[0].FileID)
	}
}

// TestBuildPlaybackInfoCollapsesUnprobedPlaceholderAgainstProbedCopy pins the
// second rule: an unprobed virtual placeholder for a release whose probed copy
// is present must not be listed. The probed copy is authoritative.
func TestBuildPlaybackInfoCollapsesUnprobedPlaceholderAgainstProbedCopy(t *testing.T) {
	// Same release, different concrete result= ids: the placeholder row and the
	// probed row share the provider-neutral key.
	probed := virtualDupRow(21, 1000, "virtual://movie/dup?result=probed", "eng", true)
	placeholder := virtualDupRow(22, 1000, "virtual://movie/dup?result=placeholder", "", false)
	if versions := buildDupVersions(t, []*models.MediaFile{probed, placeholder}); len(versions) != 1 {
		t.Fatalf("unprobed placeholder beside a probed copy projected %d versions, want 1", len(versions))
	}
	if versions := buildDupVersions(t, []*models.MediaFile{placeholder, probed}); len(versions) != 1 {
		t.Fatalf("placeholder-first ordering projected %d versions, want 1", len(versions))
	}

	// A collection placeholder is stored with size 0 while the probed copy of
	// the same release carries the real size; the placeholder must still collapse.
	sizeZeroPlaceholder := virtualDupRow(23, 0, "virtual://movie/dup?result=placeholder", "", false)
	if versions := buildDupVersions(t, []*models.MediaFile{probed, sizeZeroPlaceholder}); len(versions) != 1 {
		t.Fatalf("size-0 placeholder beside a probed copy projected %d versions, want 1", len(versions))
	}
}

// TestBuildPlaybackInfoKeepsUnprobedPlaceholderWithoutProbedCopy proves the
// collapse does not hide a release that has not been probed yet: a lone
// placeholder (or several placeholders of distinct releases) stays listed.
func TestBuildPlaybackInfoKeepsUnprobedPlaceholderWithoutProbedCopy(t *testing.T) {
	only := virtualDupRow(31, 1000, "virtual://movie/dup?result=only", "", false)
	if versions := buildDupVersions(t, []*models.MediaFile{only}); len(versions) != 1 {
		t.Fatalf("lone unprobed placeholder projected %d versions, want 1", len(versions))
	}

	// Distinct releases (different size) are not duplicates and must both stay.
	small := virtualDupRow(41, 1000, "virtual://movie/dup?result=small", "eng", true)
	large := virtualDupRow(42, 2000, "virtual://movie/dup?result=large", "eng", true)
	if versions := buildDupVersions(t, []*models.MediaFile{small, large}); len(versions) != 2 {
		t.Fatalf("distinct-size releases projected %d versions, want 2", len(versions))
	}

	// Same size and content but different neutral paths are distinct releases.
	other := virtualDupRow(51, 1000, "virtual://movie/other?result=a", "eng", true)
	sameRelease := virtualDupRow(52, 1000, "virtual://movie/dup?result=a", "eng", true)
	if versions := buildDupVersions(t, []*models.MediaFile{other, sameRelease}); len(versions) != 2 {
		t.Fatalf("distinct releases projected %d versions, want 2", len(versions))
	}
}

// TestBuildPlaybackInfoDoesNotCollapseLocalFiles proves the collapse is scoped
// to virtual rows: two local rows with the same content/size but distinct paths
// are still listed (their paths are their identity).
func TestBuildPlaybackInfoDoesNotCollapseLocalFiles(t *testing.T) {
	a := &models.MediaFile{ID: 61, ContentID: "movie-dup", FilePath: "/media/a.mkv", Container: "mkv", FileSize: 1000, Duration: 3600}
	b := &models.MediaFile{ID: 62, ContentID: "movie-dup", FilePath: "/media/b.mkv", Container: "mkv", FileSize: 1000, Duration: 3600}
	if versions := buildDupVersions(t, []*models.MediaFile{a, b}); len(versions) != 2 {
		t.Fatalf("local files projected %d versions, want 2", len(versions))
	}
}

// TestBuildPlaybackInfoKeepsDistinctUnprobedCandidateBeneathProbedCopy pins the
// placeholder-identification requirement: an unprobed row that is a concrete
// alternate release — a distinct size or a distinct durable provider identity —
// must stay selectable beside the probed copy even when it shares the probed
// row's provider-neutral URI. Only a genuine placeholder (unknown size, no
// conflicting identity) collapses.
func TestBuildPlaybackInfoKeepsDistinctUnprobedCandidateBeneathProbedCopy(t *testing.T) {
	probed := virtualDupRow(71, 1000, "virtual://movie/dup?result=probed", "eng", true)
	probed.ProviderReleaseName = "Movie.2024.1080p"
	probed.ProviderVideoHash = "hash-probed"

	// A concrete alternate with a different advertised size must not vanish,
	// even though no provider identity tier conflicts (the alternate declares
	// none).
	distinctSize := virtualDupRow(72, 2000, "virtual://movie/dup?result=alt-size", "", false)
	if versions := buildDupVersions(t, []*models.MediaFile{probed, distinctSize}); len(versions) != 2 {
		t.Fatalf("distinct-size unprobed alternate beside a probed copy projected %d versions, want 2", len(versions))
	}

	// Same unknown stored size, but a provider-declared size that disagrees:
	// the durable size tier is ProviderReleaseSize, so a row whose size lives
	// only there is not a size-unknown placeholder.
	sizedProbed := virtualDupRow(75, 0, "virtual://movie/dup?result=probed-size", "", true)
	sizedProbed.ProviderReleaseSize = 1000
	distinctProviderSize := virtualDupRow(76, 0, "virtual://movie/dup?result=alt-provider-size", "", false)
	distinctProviderSize.ProviderReleaseSize = 2000
	if versions := buildDupVersions(t, []*models.MediaFile{sizedProbed, distinctProviderSize}); len(versions) != 2 {
		t.Fatalf("distinct provider-size unprobed alternate beside a probed copy projected %d versions, want 2", len(versions))
	}

	// Same unknown size, but a durable provider identity that disagrees.
	distinctHash := virtualDupRow(73, 1000, "virtual://movie/dup?result=alt-hash", "", false)
	distinctHash.ProviderVideoHash = "hash-other"
	if versions := buildDupVersions(t, []*models.MediaFile{probed, distinctHash}); len(versions) != 2 {
		t.Fatalf("distinct-identity unprobed alternate beside a probed copy projected %d versions, want 2", len(versions))
	}

	// A genuine placeholder (size 0, no identity) still collapses.
	sizeZeroPlaceholder := virtualDupRow(74, 0, "virtual://movie/dup?result=placeholder", "", false)
	if versions := buildDupVersions(t, []*models.MediaFile{probed, sizeZeroPlaceholder}); len(versions) != 1 {
		t.Fatalf("size-0 placeholder beside a probed copy projected %d versions, want 1", len(versions))
	}
}
