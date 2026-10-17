package api

import (
	"context"
	"errors"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

type fakeCandidateLister struct {
	streams []virtuallibrary.PlaybackStream
	err     error
	paths   []string
}

func (f *fakeCandidateLister) ListStreams(_ context.Context, virtualPath string) ([]virtuallibrary.PlaybackStream, error) {
	f.paths = append(f.paths, virtualPath)
	return f.streams, f.err
}

func TestVirtualCandidateScoreSourceMapsScoreByFilePath(t *testing.T) {
	candidateURI := "virtual://movie/tt1?profile=4k&result=abc"
	lister := &fakeCandidateLister{streams: []virtuallibrary.PlaybackStream{
		{URI: candidateURI, QualityScore: 850, MatchedFormats: []string{"English", "Remux Tier 1"}},
		{URI: "virtual://movie/tt1?profile=4k&result=def", QualityScore: 0},
	}}
	source := virtualCandidateScoreSource(lister)

	files := []*models.MediaFile{
		{ID: 42, FilePath: candidateURI},
		{ID: 43, FilePath: "virtual://movie/tt1?profile=4k&result=def"},
		{ID: 44, FilePath: "/media/Movies/Heat.mkv"},
	}
	scores := source(context.Background(), "movie:heat", files)

	// The listing path keeps the profile selector and drops the per-file result.
	if len(lister.paths) != 1 || lister.paths[0] != "virtual://movie/tt1?profile=4k" {
		t.Fatalf("listed path = %v, want [virtual://movie/tt1?profile=4k]", lister.paths)
	}
	if len(scores) != 1 || scores[42].Score != 850 {
		t.Fatalf("scores = %v, want map[42:{850 [English Remux Tier 1]}]", scores)
	}
	if got := scores[42].MatchedFormats; len(got) != 2 || got[0] != "English" || got[1] != "Remux Tier 1" {
		t.Fatalf("matched formats = %v, want [English Remux Tier 1]", got)
	}
	if _, ok := scores[43]; ok {
		t.Fatalf("zero-scored candidate recorded a score: %v", scores)
	}
}

func TestVirtualCandidateScoreSourceSkipsNonVirtualFiles(t *testing.T) {
	lister := &fakeCandidateLister{}
	source := virtualCandidateScoreSource(lister)

	scores := source(context.Background(), "movie:heat", []*models.MediaFile{
		{ID: 1, FilePath: "/media/Movies/Heat.mkv"},
	})

	if scores != nil {
		t.Fatalf("scores = %v, want nil", scores)
	}
	if len(lister.paths) != 0 {
		t.Fatalf("provider listed for a non-virtual content: %v", lister.paths)
	}
}

func TestVirtualCandidateScoreSourceSwallowsProviderError(t *testing.T) {
	lister := &fakeCandidateLister{err: errors.New("provider down")}
	source := virtualCandidateScoreSource(lister)

	scores := source(context.Background(), "movie:heat", []*models.MediaFile{
		{ID: 1, FilePath: "virtual://movie/tt1?result=abc"},
	})

	if scores != nil {
		t.Fatalf("scores = %v, want nil on provider error", scores)
	}
}
