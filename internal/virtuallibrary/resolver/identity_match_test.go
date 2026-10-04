package resolver

import "testing"

// candidateWithIdentity builds a listed candidate carrying the given durable
// identity fields. It exercises the same candidateDedupKey the deduplication
// chain uses, so the assertions below pin the re-match precedence.
func candidateWithIdentity(url, title, videoHash, guid string, size int64) StreamCandidate {
	candidate := StreamCandidate{
		URL:        url,
		Name:       title,
		Title:      title,
		FileSize:   size,
		SourceGUID: guid,
	}
	candidate.BehaviorHints.VideoHash = videoHash
	return candidate
}

func TestPersistedDedupKeyPrecedence(t *testing.T) {
	if got, want := PersistedDedupKey("ABC", "guid-1", "name", 100), "vidhash:abc"; got != want {
		t.Fatalf("hash tier = %q, want %q", got, want)
	}
	if got, want := PersistedDedupKey("", "guid-1", "name", 100), "guid:guid-1"; got != want {
		t.Fatalf("guid tier = %q, want %q", got, want)
	}
	nameKey := PersistedDedupKey("", "", "release.name", 100)
	if nameKey == "" || nameKey == PersistedDedupKey("", "", "release.name", 0) {
		t.Fatalf("name+size tier = %q; size must distinguish", nameKey)
	}
	if got := PersistedDedupKey("", "", "", 100); got != "" {
		t.Fatalf("identity with no tier = %q, want empty", got)
	}
}

// TestMatchCandidateByPersistedIdentitySharedTier pins the per-tier re-match: a
// stored tier matches any candidate that shares that tier, and a stronger
// stored tier is preferred over a weaker one.
func TestMatchCandidateByPersistedIdentitySharedTier(t *testing.T) {
	hashCandidate := candidateWithIdentity("https://p.example/a.mkv", "Movie.2024.1080p", "HASH1", "", 8_000_000_000)
	nameCandidate := candidateWithIdentity("https://p.example/b.mkv", "Other.Movie.2024.1080p", "", "", 8_000_000_000)
	candidates := []StreamCandidate{hashCandidate, nameCandidate}

	// A stored hash matches the hash candidate.
	matched, ok := MatchCandidateByPersistedIdentity(candidates, "hash1", "", "different.name", 1)
	if !ok || matched.URL != hashCandidate.URL {
		t.Fatalf("hash match = %v ok=%v, want the hash candidate", matched.URL, ok)
	}

	// A stored GUID matches a GUID-carrying candidate.
	guidCandidate := candidateWithIdentity("https://p.example/c.mkv", "Guid.Movie.2024.1080p", "", "GUID-1", 8_000_000_000)
	matched, ok = MatchCandidateByPersistedIdentity([]StreamCandidate{guidCandidate}, "", "GUID-1", "", 0)
	if !ok || matched.URL != guidCandidate.URL {
		t.Fatalf("guid match = %v ok=%v, want the guid candidate", matched.URL, ok)
	}

	// A stored name matches the name candidate even when the row has no
	// hash/GUID, and even when the candidate's size drifted within tolerance.
	matched, ok = MatchCandidateByPersistedIdentity(candidates, "", "", CandidateReleaseName(nameCandidate), nameCandidate.FileSize)
	if !ok || matched.URL != nameCandidate.URL {
		t.Fatalf("name+size match = %v ok=%v, want the name candidate", matched.URL, ok)
	}
}

// TestMatchCandidateByPersistedIdentityNameOnlyMatchesStrongerCandidate is the
// #145 acceptance case: a row that persisted only a release name must still
// match a listed candidate that carries a GUID or hash for the same release.
// Under the old precedence key the name-only row could never match such a
// candidate, which is why those rows were read as "release vanished".
func TestMatchCandidateByPersistedIdentityNameOnlyMatchesStrongerCandidate(t *testing.T) {
	strong := candidateWithIdentity("https://p.example/a.mkv", "Movie.2024.1080p", "HASH1", "GUID-1", 8_000_000_000)
	nameOnly := NewPersistedIdentityTiers("", "", CandidateReleaseName(strong), 0)
	if _, ok := nameOnly.SharedTier(NewPersistedIdentityTiers("HASH1", "", CandidateReleaseName(strong), 0)); !ok {
		t.Fatal("name-only tiers did not share the release-name tier with a hash-carrying identity")
	}

	matched, ok := MatchCandidateByPersistedIdentity([]StreamCandidate{strong}, "", "", CandidateReleaseName(strong), 0)
	if !ok || matched.URL != strong.URL {
		t.Fatalf("name-only row did not match a hash-carrying candidate: %v ok=%v", matched.URL, ok)
	}
}

// TestMatchCandidateByPersistedIdentitySizeDrift covers the name tier's size
// tolerance: a small relative drift still matches, a wildly different size is a
// miss, and an unknown size on either side is neutral.
func TestMatchCandidateByPersistedIdentitySizeDrift(t *testing.T) {
	candidate := candidateWithIdentity("https://p.example/a.mkv", "Movie.2024.1080p", "", "", 8_000_000_000)
	releaseName := CandidateReleaseName(candidate)

	if _, ok := MatchCandidateByPersistedIdentity([]StreamCandidate{candidate}, "", "", releaseName, 8_400_000_000); !ok {
		t.Fatal("a 5% size drift failed to match on the name tier")
	}
	if _, ok := MatchCandidateByPersistedIdentity([]StreamCandidate{candidate}, "", "", releaseName, 0); !ok {
		t.Fatal("an unknown stored size failed to match on the name tier")
	}
	if _, ok := MatchCandidateByPersistedIdentity([]StreamCandidate{candidate}, "", "", releaseName, 4_000_000_000); ok {
		t.Fatal("a half-size release matched on the name tier")
	}
}

// TestMatchCandidateByPersistedIdentityStrongerTierNotWeakened proves a stored
// hash that no candidate carries does NOT fall through to the name tier. The
// hash is decisive evidence of a release the fresh listing cannot corroborate,
// so a coincidental release name must not re-identify it.
func TestMatchCandidateByPersistedIdentityStrongerTierNotWeakened(t *testing.T) {
	candidates := []StreamCandidate{
		candidateWithIdentity("https://p.example/a.mkv", "Movie.2024.1080p", "HASH1", "", 8_000_000_000),
	}
	if _, ok := MatchCandidateByPersistedIdentity(candidates, "different-hash", "", CandidateReleaseName(candidates[0]), candidates[0].FileSize); ok {
		t.Fatal("a stored hash did not match, yet a name coincidence was accepted")
	}
}

// TestMatchCandidateByPersistedIdentityGUIDAsymmetry pins the asymmetry that
// makes #145 safe: a stored GUID the candidate cannot corroborate is a veto,
// while a candidate's stronger tier over a stored name-only row is not.
func TestMatchCandidateByPersistedIdentityGUIDAsymmetry(t *testing.T) {
	// Candidate carries only a name; the row carries a GUID. The listing cannot
	// prove the stored GUID release, so the name coincidence is refused.
	nameOnly := candidateWithIdentity("https://p.example/a.mkv", "Movie.2024.1080p", "", "", 8_000_000_000)
	releaseName := CandidateReleaseName(nameOnly)
	if _, ok := MatchCandidateByPersistedIdentity([]StreamCandidate{nameOnly}, "", "stored-guid", releaseName, nameOnly.FileSize); ok {
		t.Fatal("a stored GUID the candidate did not corroborate was satisfied by the name tier")
	}

	// Candidate carries a GUID and a name; the row carries only the name. The
	// candidate's stronger tier corroborates the same release, so it matches.
	withGUID := candidateWithIdentity("https://p.example/a.mkv", "Movie.2024.1080p", "", "candidate-guid", 8_000_000_000)
	if _, ok := MatchCandidateByPersistedIdentity([]StreamCandidate{withGUID}, "", "", CandidateReleaseName(withGUID), 0); !ok {
		t.Fatal("a name-only row failed to match a GUID-carrying candidate for the same release")
	}
}

// TestMatchCandidateByPersistedIdentityEmptyNeverMatches proves a legacy
// identity (all tiers empty) can never re-match and is reported as such.
func TestMatchCandidateByPersistedIdentityEmptyNeverMatches(t *testing.T) {
	candidates := []StreamCandidate{candidateWithIdentity("https://p.example/a.mkv", "Movie.2024.1080p", "", "", 0)}
	_, ok, report := MatchCandidateByPersistedIdentityReport(candidates, "", "", "", 0)
	if ok {
		t.Fatal("an empty persisted identity matched a candidate")
	}
	if report.HasIdentity() {
		t.Fatal("report claims the identity has a tier when every tier is empty")
	}
	if len(report.IdentityEmptyTiers) != 3 {
		t.Fatalf("identity empty tiers = %v, want all three", report.IdentityEmptyTiers)
	}
}

// TestMatchCandidateByPersistedIdentityReportDistinguishesMismatch proves the
// report tells a genuine tier mismatch apart from "the row carries nothing",
// which is what the refusal logs need.
func TestMatchCandidateByPersistedIdentityReportDistinguishesMismatch(t *testing.T) {
	candidates := []StreamCandidate{candidateWithIdentity("https://p.example/a.mkv", "Movie.2024.1080p", "", "", 8_000_000_000)}

	_, ok, report := MatchCandidateByPersistedIdentityReport(candidates, "", "", "Different.Release.2025", 8_000_000_000)
	if ok {
		t.Fatal("a different release name matched")
	}
	if !report.HasIdentity() {
		t.Fatal("the identity carried a name tier, so HasIdentity must be true")
	}
	if report.Matched {
		t.Fatal("report claims a match")
	}

	_, ok, report = MatchCandidateByPersistedIdentityReport(candidates, "", "", CandidateReleaseName(candidates[0]), candidates[0].FileSize)
	if !ok {
		t.Fatal("the same release name failed to match")
	}
	if report.Tier != "release_name" {
		t.Fatalf("matched tier = %q, want release_name", report.Tier)
	}
}

// TestPersistedIdentitiesMatchSymmetricTiers pins the symmetric predicate the
// duplicate-rotation guard uses: two persisted rows are the same release when a
// non-empty tier they both carry agrees, even when their strongest tiers differ.
// A strongest-tier key cannot see this, so a name-only row and a GUID row of one
// release would be read as distinct.
func TestPersistedIdentitiesMatchSymmetricTiers(t *testing.T) {
	nameOnly := NewPersistedIdentityTiers("", "", "Movie.2024.1080p", 8_000_000_000)
	guidRow := NewPersistedIdentityTiers("", "guid-1", "Movie.2024.1080p", 8_000_000_000)
	hashRow := NewPersistedIdentityTiers("hash-1", "", "Movie.2024.1080p", 8_000_000_000)

	if _, ok := PersistedIdentitiesMatch(nameOnly, guidRow); !ok {
		t.Fatal("name-only row did not share the release with a name+guid row")
	}
	if _, ok := PersistedIdentitiesMatch(nameOnly, hashRow); !ok {
		t.Fatal("name-only row did not share the release with a name+hash row")
	}

	// A tier both sides carry that disagrees is a veto.
	otherHash := NewPersistedIdentityTiers("hash-2", "", "Movie.2024.1080p", 8_000_000_000)
	if _, ok := PersistedIdentitiesMatch(hashRow, otherHash); ok {
		t.Fatal("two different hashes compared equal")
	}
	guidA := NewPersistedIdentityTiers("", "guid-a", "Movie.2024.1080p", 8_000_000_000)
	guidB := NewPersistedIdentityTiers("", "guid-b", "Movie.2024.1080p", 8_000_000_000)
	if _, ok := PersistedIdentitiesMatch(guidA, guidB); ok {
		t.Fatal("two different GUIDs merged on a matching name+size")
	}
	// A hash on one side and a GUID on the other is not proof either: the tiers
	// are disjoint (use bare rows with no shared name).
	bareHash := NewPersistedIdentityTiers("hash-9", "", "", 0)
	bareGUID := NewPersistedIdentityTiers("", "guid-9", "", 0)
	if _, ok := PersistedIdentitiesMatch(bareHash, bareGUID); ok {
		t.Fatal("a hash row and a GUID row compared equal with no shared tier")
	}

	// Disjoint tiers (a name-only row against a bare GUID row) are not proof.
	if _, ok := PersistedIdentitiesMatch(nameOnly, NewPersistedIdentityTiers("", "guid-2", "", 0)); ok {
		t.Fatal("disjoint tiers compared equal")
	}

	// An identity with no tier at all is never proof.
	if _, ok := PersistedIdentitiesMatch(NewPersistedIdentityTiers("", "", "", 0), nameOnly); ok {
		t.Fatal("an empty identity matched")
	}

	// The name tier still requires plausible size agreement.
	if _, ok := PersistedIdentitiesMatch(
		NewPersistedIdentityTiers("", "", "Movie.2024.1080p", 8_000_000_000),
		NewPersistedIdentityTiers("", "guid-1", "Movie.2024.1080p", 4_000_000_000),
	); ok {
		t.Fatal("a half-size release matched on the name tier")
	}
}
