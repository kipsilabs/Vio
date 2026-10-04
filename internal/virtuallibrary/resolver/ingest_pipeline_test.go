package resolver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

// newProviderResolver builds a counting provider that answers with the given
// streams and a resolver pointed at it.
func newProviderResolver(t *testing.T, streams []StreamCandidate) (*Resolver, *countingProvider) {
	t.Helper()
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeStreams(w, streams)
	})
	return New(testConfig(p)), p
}

// fetchRaw drives the provider ingestion stage directly so tests can inspect
// the candidates before dedup/classification/truncation.
func fetchRaw(t *testing.T, r *Resolver) []StreamCandidate {
	t.Helper()
	got, err := r.fetchProviderCandidates(context.Background(), r.config, 0, "movie|tt1", "movie", "tt1")
	if err != nil {
		t.Fatalf("fetchProviderCandidates: %v", err)
	}
	return got
}

func TestFetchProviderCandidatesOnlyAbsoluteHTTPURLsSurvive(t *testing.T) {
	streams := []StreamCandidate{
		{URL: "https://cdn.example/a.mkv", Name: "absolute https"},
		{URL: "http://cdn.example/b.mkv", Name: "absolute http"},
		{URL: "ftp://cdn.example/c.mkv", Name: "wrong scheme"},
		{URL: "file:///d.mkv", Name: "file scheme"},
		{URL: "/relative/e.mkv", Name: "relative path"},
		{URL: "", Name: "empty"},
	}
	r, _ := newProviderResolver(t, streams)
	got := fetchRaw(t, r)

	if len(got) != 2 {
		t.Fatalf("surviving candidates = %d, want 2: %+v", len(got), got)
	}
	if got[0].URL != "https://cdn.example/a.mkv" || got[1].URL != "http://cdn.example/b.mkv" {
		t.Fatalf("surviving URLs = [%q, %q], want the absolute http/https pair", got[0].URL, got[1].URL)
	}
	// OriginalIndex points back into the provider payload, so a dropped
	// candidate stays diagnosable.
	if got[0].OriginalIndex != 0 || got[1].OriginalIndex != 1 {
		t.Fatalf("OriginalIndex = [%d, %d], want [0, 1]", got[0].OriginalIndex, got[1].OriginalIndex)
	}
}

func TestFetchProviderCandidatesDropsPlaceholderStubs(t *testing.T) {
	streams := []StreamCandidate{
		{URL: "https://cdn.example/a.mkv", Name: "No streams available"},
		{URL: "https://cdn.example/b.mkv", Title: "Nothing found"},
		{URL: "https://cdn.example/c.mkv", Description: "provider returned no results"},
		{URL: "https://cdn.example/real.mkv", Name: "Real.Release.2024.1080p.WEB-DL"},
	}
	r, _ := newProviderResolver(t, streams)
	got := fetchRaw(t, r)

	if len(got) != 1 || got[0].URL != "https://cdn.example/real.mkv" {
		t.Fatalf("surviving candidates = %+v, want only the real release", got)
	}
}

// TestFetchProviderCandidatesParseBound proves the provider payload is parsed
// against maxProviderCandidates, which is deliberately far above the
// selectable cap so dedup can run first.
func TestFetchProviderCandidatesParseBound(t *testing.T) {
	total := maxProviderCandidates + 25
	streams := make([]StreamCandidate, 0, total)
	for i := 0; i < total; i++ {
		streams = append(streams, StreamCandidate{
			URL:  fmt.Sprintf("https://cdn.example/file-%d.mkv", i),
			Name: fmt.Sprintf("Release %d", i),
		})
	}
	r, _ := newProviderResolver(t, streams)
	got := fetchRaw(t, r)

	if len(got) != maxProviderCandidates {
		t.Fatalf("parsed candidates = %d, want %d", len(got), maxProviderCandidates)
	}
	if got[len(got)-1].OriginalIndex != maxProviderCandidates-1 {
		t.Fatalf("last parsed OriginalIndex = %d, want %d", got[len(got)-1].OriginalIndex, maxProviderCandidates-1)
	}
}

// TestMultiFileReleaseDoesNotCrowdOutOtherReleases is the regression for the
// cap-before-dedup bug: a provider that offers sixty per-file variants of one
// release must not consume the selectable slots before the release collapses,
// or a distinct release offered last would never surface.
func TestMultiFileReleaseDoesNotCrowdOutOtherReleases(t *testing.T) {
	streams := make([]StreamCandidate, 0, 61)
	for i := 0; i < 60; i++ {
		candidate := StreamCandidate{
			URL:  fmt.Sprintf("https://cdn.example/big-%d.mkv", i),
			Name: fmt.Sprintf("Big.Release.2024-%d", i),
		}
		candidate.BehaviorHints.VideoHash = "same-content-hash"
		streams = append(streams, candidate)
	}
	other := StreamCandidate{URL: "https://cdn.example/other.mkv", Name: "Other.Release.2024.1080p"}
	other.BehaviorHints.VideoHash = "other-content-hash"
	streams = append(streams, other)

	r, _ := newProviderResolver(t, streams)
	got, _, _, _, err := r.GetCandidatesWithKeepers(context.Background(), "virtual://movie/tt1")
	if err != nil {
		t.Fatalf("GetCandidatesWithKeepers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want the collapsed big release plus the other release: %+v", len(got), got)
	}
	foundOther := false
	for _, candidate := range got {
		if candidate.URL == other.URL {
			foundOther = true
		}
	}
	if !foundOther {
		t.Fatalf("distinct release was crowded out by multi-file variants: %+v", got)
	}
}

// TestCandidateDedupKeyTiers pins the identity tiers: a shared content hash or
// release GUID collapses regardless of display name/size, and only the weakest
// tier falls back to name+size.
func TestCandidateDedupKeyTiers(t *testing.T) {
	withHash := func(hash, name string, size int64) StreamCandidate {
		c := StreamCandidate{Name: name, FileSize: size}
		c.BehaviorHints.VideoHash = hash
		return c
	}
	withGUID := func(guid, name string, size int64) StreamCandidate {
		return StreamCandidate{SourceGUID: guid, Name: name, FileSize: size}
	}
	withName := func(name string, size int64) StreamCandidate {
		return StreamCandidate{Name: name, FileSize: size}
	}

	cases := []struct {
		name     string
		a        StreamCandidate
		b        StreamCandidate
		wantSame bool
	}{
		{"video hash wins over name and size", withHash("HASH", "A", 1), withHash("hash", "B", 2), true},
		{"distinct video hashes differ", withHash("h1", "A", 1), withHash("h2", "A", 1), false},
		{"guid collapses differing name and size", withGUID("g", "A", 1), withGUID("g", "B", 2), true},
		{"distinct guids differ", withGUID("g1", "A", 1), withGUID("g2", "A", 1), false},
		{"weak tier matches name and size", withName("Release.Name", 100), withName("release name", 100), true},
		{"weak tier differs on size", withName("Release.Name", 100), withName("Release.Name", 200), false},
		{"weak tier differs on name", withName("Release.One", 100), withName("Release.Two", 100), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keyA := candidateDedupKey(tc.a)
			keyB := candidateDedupKey(tc.b)
			if keyA == "" || keyB == "" {
				t.Fatalf("setup: expected non-empty keys, got %q and %q", keyA, keyB)
			}
			if gotSame := keyA == keyB; gotSame != tc.wantSame {
				t.Fatalf("keys equal = %v, want %v (%q vs %q)", gotSame, tc.wantSame, keyA, keyB)
			}
		})
	}

	if key := candidateDedupKey(StreamCandidate{}); key != "" {
		t.Fatalf("candidateDedupKey(zero candidate) = %q, want empty (always kept)", key)
	}
}

// TestCandidateDedupNameStripsPerFileIndex proves the per-file result ids a
// multi-file provider appends (`-43`) do not keep two files of one release
// apart when only the filename identifies the release.
func TestCandidateDedupNameStripsPerFileIndex(t *testing.T) {
	a := StreamCandidate{FileSize: 7}
	a.BehaviorHints.Filename = "Some.Release-43.mkv"
	b := StreamCandidate{FileSize: 7}
	b.BehaviorHints.Filename = "Some.Release-44.mkv"

	if candidateDedupKey(a) != candidateDedupKey(b) {
		t.Fatalf("per-file filenames of one release did not collapse: %q vs %q",
			candidateDedupKey(a), candidateDedupKey(b))
	}
}

func TestPreferConfirmedCandidatesDropsFailedAndPartitionsConfirmed(t *testing.T) {
	r := New(Config{})
	r.SetCandidateClassifier(fakeClassifier{
		confirm: func(c StreamCandidate) bool { return c.URL == "u2" },
		fail:    func(c StreamCandidate) bool { return c.URL == "u3" },
	})
	in := []StreamCandidate{
		{URL: "u1", Name: "one"},
		{URL: "u2", Name: "two"},
		{URL: "u3", Name: "three"},
		{URL: "u4", Name: "four"},
	}

	got, dropped := r.preferConfirmedCandidates(context.Background(), in)
	want := []string{"u2", "u1", "u4"}
	if len(got) != len(want) {
		t.Fatalf("candidates = %+v, want the failed one dropped (%v)", got, want)
	}
	for i := range want {
		if got[i].URL != want[i] {
			t.Fatalf("candidate[%d] = %q, want %q (full order %+v)", i, got[i].URL, want[i], got)
		}
	}
	if dropped != nil {
		t.Fatalf("dropped map = %v, want nil when nothing collapsed", dropped)
	}
}

// TestFailedVariantDoesNotShadowLiveDuplicate pins that failed classification
// runs before dedup: a dead variant of a release must not become the surviving
// keeper of a live duplicate.
func TestFailedVariantDoesNotShadowLiveDuplicate(t *testing.T) {
	failed := StreamCandidate{URL: "u1", Name: "Release"}
	failed.BehaviorHints.VideoHash = "h"
	live := StreamCandidate{URL: "u2", Name: "Release"}
	live.BehaviorHints.VideoHash = "h"

	r := New(Config{})
	r.SetCandidateClassifier(fakeClassifier{fail: func(c StreamCandidate) bool { return c.URL == "u1" }})

	got, _ := r.preferConfirmedCandidates(context.Background(), []StreamCandidate{failed, live})
	if len(got) != 1 || got[0].URL != "u2" {
		t.Fatalf("candidates = %+v, want only the live duplicate", got)
	}
}

func TestDedupeCandidatesConfirmedVariantIsKeeper(t *testing.T) {
	a := StreamCandidate{URL: "https://cdn.example/a-1.mkv", Name: "A"}
	a.SourceGUID = "guid-1"
	b := StreamCandidate{URL: "https://cdn.example/a-2.mkv", Name: "A", SourceConfirmed: true}
	b.SourceGUID = "guid-1"

	kept, dropped := dedupeCandidates([]StreamCandidate{a, b})
	if len(kept) != 1 {
		t.Fatalf("kept = %d, want 1", len(kept))
	}
	if kept[0].URL != b.URL || !kept[0].SourceConfirmed {
		t.Fatalf("keeper = %+v, want the confirmed variant %q", kept[0], b.URL)
	}
	if got := dropped[stream.CandidateVariantID(a)]; got != stream.CandidateVariantID(b) {
		t.Fatalf("dropped[a] = %q, want the confirmed keeper %q", got, stream.CandidateVariantID(b))
	}
}

// TestProcessCandidatesKeeperMapFilteredByCap proves the dropped->keeper map
// only names keepers that survived truncation: a pin on a variant whose keeper
// ranked past the selectable cap has no representative and must be treated as
// dead rather than translated to an absent candidate.
func TestProcessCandidatesKeeperMapFilteredByCap(t *testing.T) {
	const groups = maxVirtualCandidates + 1
	in := make([]StreamCandidate, 0, groups*2)
	for g := 0; g < groups; g++ {
		keeper := StreamCandidate{
			URL:  fmt.Sprintf("https://cdn.example/g%d-keeper.mkv", g),
			Name: fmt.Sprintf("Release %d", g),
		}
		keeper.SourceGUID = fmt.Sprintf("guid-%d", g)
		variant := StreamCandidate{
			URL:  fmt.Sprintf("https://cdn.example/g%d-variant.mkv", g),
			Name: fmt.Sprintf("Release %d", g),
		}
		variant.SourceGUID = fmt.Sprintf("guid-%d", g)
		in = append(in, keeper, variant)
	}

	r := New(Config{})
	got, dropped := r.processCandidates(context.Background(), in)
	if len(got) != maxVirtualCandidates {
		t.Fatalf("candidates = %d, want the selectable cap %d", len(got), maxVirtualCandidates)
	}

	firstDroppedID := stream.CandidateVariantID(in[1])
	firstKeeperID := stream.CandidateVariantID(in[0])
	if mapped := dropped[firstDroppedID]; mapped != firstKeeperID {
		t.Fatalf("dropped[first variant] = %q, want surviving keeper %q", mapped, firstKeeperID)
	}

	// The last group's keeper ranked beyond the cap, so its variant must not
	// map to an absent keeper.
	lastVariantID := stream.CandidateVariantID(in[len(in)-1])
	lastKeeperID := stream.CandidateVariantID(in[len(in)-2])
	if mapped, ok := dropped[lastVariantID]; ok {
		t.Fatalf("dropped[last variant] = %q, want the entry removed (keeper %q was cut)", mapped, lastKeeperID)
	}
}

func TestCandidateDedupNameFallsBackToDescription(t *testing.T) {
	// A listing entry with no usable name, title, filename, or URL base —
	// exactly the shape that persisted identity-less rows in prod — still
	// derives identity from the description's first line when it names a
	// release.
	bare := StreamCandidate{
		Description: "Frieren.Beyond.Journeys.End.S01E03.1080p.CR.WEB-DL.H264-ToonsHub\n1.6 GB",
	}
	if got := candidateDedupName(bare); got == "" || !strings.Contains(got, "toonshub") {
		t.Fatalf("description-derived name = %q, want the ToonsHub release identity", got)
	}
	// A bare spec line must never become identity: it would collide across
	// unrelated releases sharing a resolution.
	specLine := StreamCandidate{Description: "1080p"}
	if got := candidateDedupName(specLine); got != "" {
		t.Fatalf("spec-line description produced identity %q, want empty", got)
	}
	// Existing tiers keep precedence: a real Name wins over Description.
	realName := StreamCandidate{
		Name:        "Frieren.Beyond.Journeys.End.S01E03.2160p.WEB-DL.H265-CSWEB",
		Description: "Something.Else.2024.1080p.WEB-DL.x264-OTHER",
	}
	if got, want := candidateDedupName(realName), candidateDedupName(StreamCandidate{Name: "Frieren.Beyond.Journeys.End.S01E03.2160p.WEB-DL.H265-CSWEB"}); got != want {
		t.Fatalf("name-tier precedence broken: %q vs %q", got, want)
	}
}
