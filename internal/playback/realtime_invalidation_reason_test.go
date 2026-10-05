package playback

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// clientReasonSourceV3 is the web client's mirror of this constant. The browser
// cannot import Go, so the two halves of the wire are held together by a test in
// each repository rather than by the compiler:
// internal/playback/realtime_invalidation_reason_test.go here, and
// web/src/player/defaultAudioReconciliationReason.test.ts on the client side.
//
// A drift is invisible at runtime and degrading rather than breaking: the client
// falls back to treating a healthy cold-start session's audio correction as a
// route failure, which excludes the route that is playing from its own
// replacement plan. See docs/architecture/playback-protocol-v3.md §6.1.1.
var clientReasonSourceV3 = filepath.Join(
	"..", "..", "web", "src", "player", "realtime-protocol.ts",
)

func TestPlanInvalidatedDefaultAudioReconciliationReasonMatchesClient(t *testing.T) {
	source, err := os.ReadFile(clientReasonSourceV3)
	if err != nil {
		t.Fatalf("read %s: %v", clientReasonSourceV3, err)
	}
	match := regexp.MustCompile(
		`PLAN_INVALIDATED_DEFAULT_AUDIO_RECONCILIATION\s*=\s*"([a-z_]+)"`,
	).FindSubmatch(source)
	if match == nil {
		t.Fatalf("no PLAN_INVALIDATED_DEFAULT_AUDIO_RECONCILIATION string in %s", clientReasonSourceV3)
	}
	if got, want := string(match[1]), PlanInvalidatedDefaultAudioReconciliation; got != want {
		t.Errorf("web client reason = %q, server constant = %q", got, want)
	}
}

// The two reasons share one command and take different replan paths on the
// client: the copy-safety verdict excludes the route it withdraws, the audio
// reconciliation does not. They must not collapse into one another.
func TestPlanInvalidationReasonsAreDistinct(t *testing.T) {
	if PlanInvalidatedVideoCopyUnsafe == PlanInvalidatedDefaultAudioReconciliation {
		t.Errorf("both plan_invalidated reasons are %q", PlanInvalidatedVideoCopyUnsafe)
	}
}
