package stream

import "testing"

func TestCandidateDisplayNameDoesNotAppendSize(t *testing.T) {
	first := StreamCandidate{Name: "AIO", Title: "Release 1.00 GB", URL: "https://provider.example/one.mkv"}
	second := StreamCandidate{Name: "AIO", Title: "Release 2.00 GB", URL: "https://provider.example/two.mkv"}
	for _, candidate := range []StreamCandidate{first, second} {
		ParseStreamDetails(&candidate)
		if got := CandidateDisplayName(candidate); got != "AIO" {
			t.Fatalf("display name = %q, want AIO", got)
		}
		if candidate.FileSize == 0 {
			t.Fatal("size metadata must still be parsed")
		}
	}
	if CandidateVariantID(first) == CandidateVariantID(second) {
		t.Fatal("equal display names must not collapse distinct versions")
	}
}

// TestCandidateVideoHashAcceptsInfoHashAndHint pins the fallback order for the
// provider-declared content hash: the explicit Stremio behaviorHints.videoHash
// wins, and a torrent infoHash is the durable alternative an addon like AltMount
// sends instead. Both identify the same bytes across a result renumbering.
func TestCandidateVideoHashAcceptsInfoHashAndHint(t *testing.T) {
	hashOnly := StreamCandidate{}
	hashOnly.BehaviorHints.VideoHash = "HINT-HASH"
	if got := CandidateVideoHash(hashOnly); got != "HINT-HASH" {
		t.Fatalf("videoHash tier = %q, want the hint", got)
	}

	infoOnly := StreamCandidate{InfoHash: "abc123"}
	if got := CandidateVideoHash(infoOnly); got != "abc123" {
		t.Fatalf("infoHash fallback = %q, want the infoHash", got)
	}

	both := StreamCandidate{InfoHash: "abc123"}
	both.BehaviorHints.VideoHash = "HINT-HASH"
	if got := CandidateVideoHash(both); got != "HINT-HASH" {
		t.Fatalf("both tiers = %q, want the explicit hint to win", got)
	}

	if got := CandidateVideoHash(StreamCandidate{}); got != "" {
		t.Fatalf("no hash = %q, want empty", got)
	}
}

// TestParseStreamMetadataUsesVideoSizeHint proves a provider that declares only
// behaviorHints.videoSize still yields a size tier, so its name+size identity is
// re-matchable without size text in the title.
func TestParseStreamMetadataUsesVideoSizeHint(t *testing.T) {
	candidate := StreamCandidate{Name: "My.Movie.2024.1080p", Title: "My.Movie.2024.1080p"}
	candidate.BehaviorHints.VideoSize = 8_500_000_000
	ParseStreamDetails(&candidate)
	if candidate.FileSize != 8_500_000_000 {
		t.Fatalf("FileSize = %d, want the videoSize hint", candidate.FileSize)
	}

	// A parseable size text wins over the hint, because the parsed value is the
	// one the dedup chain already compares against.
	text := StreamCandidate{Name: "My.Movie.2024.1080p 4.00 GB", Title: "My.Movie.2024.1080p 4.00 GB"}
	text.BehaviorHints.VideoSize = 8_500_000_000
	ParseStreamDetails(&text)
	if text.FileSize != 4_000_000_000 {
		t.Fatalf("FileSize = %d, want the parsed size text", text.FileSize)
	}
}

// TestCandidateDeclaredSizePrefersParsedSize pins the size accessor used by the
// durable identity tiers when the stream-level size is unset.
func TestCandidateDeclaredSizePrefersParsedSize(t *testing.T) {
	candidate := StreamCandidate{FileSize: 4_000_000_000}
	candidate.BehaviorHints.VideoSize = 8_500_000_000
	if got := CandidateDeclaredSize(candidate); got != 4_000_000_000 {
		t.Fatalf("declared size = %d, want the parsed stream size", got)
	}
	hintOnly := StreamCandidate{}
	hintOnly.BehaviorHints.VideoSize = 8_500_000_000
	if got := CandidateDeclaredSize(hintOnly); got != 8_500_000_000 {
		t.Fatalf("declared size = %d, want the videoSize hint", got)
	}
}

// TestCandidateVariantIDIncludesInfoHash proves the ?result= identity is not
// stable across two different releases distinguished only by infoHash.
func TestCandidateVariantIDIncludesInfoHash(t *testing.T) {
	base := StreamCandidate{Name: "Release", Title: "Release", URL: "https://p.example/a.mkv"}
	one := base
	one.InfoHash = "hash-one"
	two := base
	two.InfoHash = "hash-two"
	if CandidateVariantID(one) == CandidateVariantID(two) {
		t.Fatal("two candidates differing only by infoHash share a variant id")
	}
}
