package scanner

import (
	"reflect"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestMergeEpisodeFilesListsDirectFilesBeforeAliasFiles(t *testing.T) {
	direct := []*models.MediaFile{
		{ID: 10, EpisodeID: "canonical"},
		nil,
		{ID: 20, EpisodeID: "canonical"},
	}
	alias := []*models.MediaFile{
		{ID: 2, EpisodeID: "alias"},
		{ID: 10, EpisodeID: "canonical"}, // duplicate of a direct file
		{ID: 15, EpisodeID: "alias"},
	}

	ids := func(files []*models.MediaFile) []int {
		out := make([]int, 0, len(files))
		for _, file := range files {
			out = append(out, file.ID)
		}
		return out
	}

	merged := mergeEpisodeFiles(direct, alias)
	if got, want := ids(merged), []int{10, 20, 2, 15}; !reflect.DeepEqual(got, want) {
		t.Fatalf("merged file ids = %v, want direct files then alias files %v", got, want)
	}
	for _, file := range merged[2:] {
		if file.EpisodeID != "alias" {
			t.Fatalf("merged alias file = %+v, want the stored alias episode", file)
		}
	}

	if got := mergeEpisodeFiles(direct, nil); !reflect.DeepEqual(got, direct) {
		t.Fatalf("merge without alias files = %+v, want direct files unchanged", got)
	}
	if got, want := ids(mergeEpisodeFiles(nil, alias)), []int{2, 10, 15}; !reflect.DeepEqual(got, want) {
		t.Fatalf("merge without direct files = %v, want alias files in id order %v", got, want)
	}
}
