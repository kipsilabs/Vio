package altmount

import "testing"

// TestReleaseFailedReportsCachedVerdict proves the symmetric read to
// ReleaseCompleted: a release in the failed snapshot is reported failed and
// known, an unknown release is known-but-not-failed, and an unconfigured client
// reports unknown so a caller never prunes on its silence.
func TestReleaseFailedReportsCachedVerdict(t *testing.T) {
	client := newAltmountStateClient(nil)
	client.url = "https://altmount.example"
	client.state = altmountStateSnapshot{
		Completed: map[string]altmountReleaseRecord{},
		Failed: map[string]altmountReleaseRecord{
			"deadrelease2024": {CompletedAt: 1700000000},
		},
	}

	failed, known := client.ReleaseFailed("Dead.Release.2024")
	if !known || !failed {
		t.Fatalf("ReleaseFailed(dead) = (%v, %v), want (true, true)", failed, known)
	}

	failed, known = client.ReleaseFailed("Another.Release.2024")
	if !known || failed {
		t.Fatalf("ReleaseFailed(unknown) = (%v, %v), want (false, true)", failed, known)
	}

	// A key that normalizes to empty is never a failed verdict.
	if _, known := client.ReleaseFailed("   "); known {
		t.Fatal("empty release key reported known")
	}

	unconfigured := newAltmountStateClient(nil)
	if _, known := unconfigured.ReleaseFailed("Dead.Release.2024"); known {
		t.Fatal("unconfigured client reported a known failed verdict")
	}
}
