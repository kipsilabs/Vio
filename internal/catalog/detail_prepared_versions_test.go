package catalog

import (
	"reflect"
	"testing"
	"time"
)

func versionIDs(versions []FileVersion) []int {
	ids := make([]int, len(versions))
	for i := range versions {
		ids[i] = versions[i].FileID
	}
	return ids
}

func TestVersionPreparedPartitionPreservesWithinGroupOrder(t *testing.T) {
	probed := time.Now()
	input := []FileVersion{
		{FileID: 1},
		{FileID: 2, ProbedAt: &probed},
		{FileID: 3},
		{FileID: 4, ProbedAt: &probed},
		{FileID: 5, Failed: true, ProbedAt: &probed},
		{FileID: 6},
	}
	inputCopy := append([]FileVersion(nil), input...)

	got := partitionPreparedVersions(input)
	if want := []int{2, 4, 1, 3, 5, 6}; !reflect.DeepEqual(versionIDs(got), want) {
		t.Fatalf("version order = %v, want %v", versionIDs(got), want)
	}
	if !reflect.DeepEqual(input, inputCopy) {
		t.Fatal("partition mutated the caller's version slice")
	}
}

func TestVersionPreparedRowJumpsFront(t *testing.T) {
	probed := time.Now()
	got := partitionPreparedVersions([]FileVersion{
		{FileID: 1},
		{FileID: 2, ProbedAt: &probed},
	})
	if want := []int{2, 1}; !reflect.DeepEqual(versionIDs(got), want) {
		t.Fatalf("version order = %v, want %v", versionIDs(got), want)
	}
}

func TestVersionPreparedPartitionNoopWithoutMix(t *testing.T) {
	probed := time.Now()
	unavailable := false
	tests := map[string][]FileVersion{
		"all unverified": {
			{FileID: 1},
			{FileID: 2, Available: &unavailable},
			{FileID: 3},
		},
		"all verified": {
			{FileID: 1, ProbedAt: &probed},
			{FileID: 2, ProbedAt: &probed},
			{FileID: 3, ProbedAt: &probed},
		},
		"all failed": {
			{FileID: 1, Failed: true, ProbedAt: &probed},
			{FileID: 2, Failed: true, ProbedAt: &probed},
			{FileID: 3, Failed: true, ProbedAt: &probed},
		},
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			inputCopy := append([]FileVersion(nil), input...)
			got := partitionPreparedVersions(input)
			if !reflect.DeepEqual(got, inputCopy) {
				t.Fatalf("version order = %v, want the original order %v", versionIDs(got), versionIDs(inputCopy))
			}
		})
	}
}

// A sibling row adopted by the cross-indexer fallback carries the verified
// inventory while the listing's default row stays unprobed. The same partition
// rule must promote the adopted row ahead of that default.
func TestVersionPreparedAdoptedSiblingSortsFirst(t *testing.T) {
	probed := time.Now()
	got := partitionPreparedVersions([]FileVersion{
		{FileID: 41, FilePath: "virtual://movie/tt1?result=default"},
		{FileID: 42, FilePath: "virtual://movie/tt1?result=adopted-sibling", ProbedAt: &probed},
		{FileID: 43, FilePath: "virtual://movie/tt1?result=unprobed"},
	})
	if want := []int{42, 41, 43}; !reflect.DeepEqual(versionIDs(got), want) {
		t.Fatalf("version order = %v, want adopted sibling first %v", versionIDs(got), want)
	}
}

func TestVersionPreparedOrderServesVerifiedRowFirst(t *testing.T) {
	f := newVersionsFixture(t)
	files := f.files.files[f.ids["movie"]]
	probed := time.Now()
	files[1].ProbeUpdatedAt = &probed

	versions, err := f.svc.GetItemVersions(t.Context(), f.ids["movie"], AccessFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{files[1].ID, files[0].ID}; !reflect.DeepEqual(versionIDs(versions), want) {
		t.Fatalf("version order = %v, want verified row first %v", versionIDs(versions), want)
	}
}

func TestVersionPreparedOrderNoopWithoutMix(t *testing.T) {
	f := newVersionsFixture(t)
	files := f.files.files[f.ids["movie"]]

	cases := []struct {
		name    string
		prepare func()
	}{
		{
			name: "all unverified",
			prepare: func() {
				for _, file := range files {
					file.ProbeUpdatedAt = nil
				}
			},
		},
		{
			name: "all verified",
			prepare: func() {
				probed := time.Now()
				for _, file := range files {
					file.ProbeUpdatedAt = &probed
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.prepare()
			versions, err := f.svc.GetItemVersions(t.Context(), f.ids["movie"], AccessFilter{})
			if err != nil {
				t.Fatal(err)
			}
			if want := []int{files[0].ID, files[1].ID}; !reflect.DeepEqual(versionIDs(versions), want) {
				t.Fatalf("version order = %v, want unchanged %v", versionIDs(versions), want)
			}
		})
	}
}
