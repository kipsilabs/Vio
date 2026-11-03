package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// markCompletedDurationVerified turns the fixture's pinned row into the shape the
// stickiness gate recognizes: a completed release whose probe measured the
// item's runtime and filled planner-grade video, audio and container evidence.
func markCompletedDurationVerified(file *models.MediaFile) {
	stamp := time.Now()
	file.ProbeUpdatedAt = &stamp
	file.VirtualExpectedRuntimeMinutes = 90
	file.Duration = 90 * 60
	file.Container = "mkv"
	file.Bitrate = 8_000
	file.VideoTracks = []models.VideoTrack{{
		Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8, FrameRate: "24000/1001", Bitrate: 8_000,
	}}
}

// A transport hiccup on a completed, duration-verified pinned release must retry
// that release (fresh relay registration) before the cross-release walk. The
// retry keeps the viewer's audio/subtitle ordinals because it never leaves the
// planned release, and the walk is never started.
func TestTransportRetriesCompletedPinnedReleaseOnTransientHiccup(t *testing.T) {
	f := newFallbackInventoryFixture(t, "session-sticky-retry")
	markCompletedDurationVerified(f.file)
	f.withSiblings(t, nil)

	var calls int
	var pinCallHadExclusions bool
	var pinRetryFresh bool
	f.handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(
		func(ctx context.Context, uri string, _ int, _ int, _ string, forceRefresh bool, excluded []string, _ string) (ResolvedVirtualMedia, error) {
			calls++
			if calls == 1 {
				pinCallHadExclusions = len(excluded) > 0
				return ResolvedVirtualMedia{}, fmt.Errorf("%w: upstream answered 503", resolver.ErrProviderUnavailable)
			}
			pinRetryFresh = virtualRelayFreshRegistrationRequested(ctx)
			if len(excluded) > 0 {
				t.Fatalf("the same-release retry excluded its own pin: %v", excluded)
			}
			return ResolvedVirtualMedia{
				URL:         "http://localhost:8080/pinned-live.mp4",
				URI:         f.pinnedURI,
				CandidateID: "dead",
			}, nil
		})
	f.withTranscode(t)

	plan := f.run(t, "session-sticky-retry")

	if calls != 2 {
		t.Fatalf("resolver calls = %d, want the failed resolve plus one same-release retry", calls)
	}
	if pinCallHadExclusions {
		t.Fatal("the first resolve was called as a rotation instead of the pinned attempt")
	}
	if !pinRetryFresh {
		t.Fatal("the same-release retry did not request a fresh relay registration")
	}
	if len(f.started) != 1 {
		t.Fatalf("started = %d transports, want the retried pinned release", len(f.started))
	}
	if got := f.started[0].InputPath; !strings.Contains(got, "pinned-live.mp4") {
		t.Fatalf("started input = %q, want the retried pinned release", got)
	}
	// The transport stays on the planned release, so the selections are not
	// rewritten by the cross-release carry/degrade path.
	if f.started[0].AudioTrackIndex != 1 {
		t.Fatalf("audio index = %d, want the planned 1", f.started[0].AudioTrackIndex)
	}
	if f.started[0].SubtitleTrackIndex != 1 || !f.started[0].SubtitleBurnIn {
		t.Fatalf("subtitle selection = index %d burn-in %v, want the planned index 1 burn-in true",
			f.started[0].SubtitleTrackIndex, f.started[0].SubtitleBurnIn)
	}
	if len(f.preferredIDs) != 0 {
		t.Fatalf("alternate walk preferred candidates %v, want none", f.preferredIDs)
	}
	if len(plan.DegradationWarnings) != 0 {
		t.Fatalf("degradation warnings = %+v, want none on a same-release retry", plan.DegradationWarnings)
	}
}

// A confirmed-dead release is not a transport hiccup: the same-release retry must
// not fire, and the existing bounded alternate walk rotates exactly as before.
func TestTransportConfirmedDeadPinnedReleaseStillRotates(t *testing.T) {
	f := newFallbackInventoryFixture(t, "session-dead-rotate")
	markCompletedDurationVerified(f.file)
	f.withSiblings(t, nil)

	var calls int
	var retriedPinned bool
	f.handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(
		func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, excluded []string, _ string) (ResolvedVirtualMedia, error) {
			calls++
			if len(excluded) == 0 {
				if calls > 1 {
					// A same-release retry would appear here.
					retriedPinned = true
				}
				return ResolvedVirtualMedia{}, fmt.Errorf("%w: pinned candidate is gone", ErrVirtualCandidateMarkedFailed)
			}
			return ResolvedVirtualMedia{
				URL:         "http://localhost:8080/alternate-live.mp4",
				URI:         f.unprobedURI,
				CandidateID: "unprobed",
			}, nil
		})
	f.withTranscode(t)

	_ = f.run(t, "session-dead-rotate")

	if retriedPinned || calls != 2 {
		t.Fatalf("resolver calls = %d (retried pinned: %v), want only the dead attempt and one alternate", calls, retriedPinned)
	}
	if len(f.started) != 1 {
		t.Fatalf("started = %d transports, want the alternate", len(f.started))
	}
	if got := f.started[0].InputPath; !strings.Contains(got, "alternate-live.mp4") {
		t.Fatalf("started input = %q, want the alternate release", got)
	}
}

// A transient failure on a release that is not completed/duration-verified keeps
// the old walk: the gate must not add 20 s of retry to speculative unprobed rows.
func TestTransportTransientHiccupWithoutCompletedReleaseKeepsWalk(t *testing.T) {
	f := newFallbackInventoryFixture(t, "session-unprobed-hiccup")
	f.withSiblings(t, nil)

	var calls int
	f.handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(
		func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, excluded []string, _ string) (ResolvedVirtualMedia, error) {
			calls++
			if len(excluded) == 0 {
				return ResolvedVirtualMedia{}, fmt.Errorf("%w: upstream answered 503", resolver.ErrProviderUnavailable)
			}
			return ResolvedVirtualMedia{
				URL:         "http://localhost:8080/alternate-live.mp4",
				URI:         f.unprobedURI,
				CandidateID: "unprobed",
			}, nil
		})
	f.withTranscode(t)

	_ = f.run(t, "session-unprobed-hiccup")

	if calls != 2 {
		t.Fatalf("resolver calls = %d, want the failed attempt and one alternate with no extra retry", calls)
	}
	if len(f.started) != 1 || !strings.Contains(f.started[0].InputPath, "alternate-live.mp4") {
		t.Fatalf("started = %+v, want the alternate", f.started)
	}
}

// An absent pin is a release verdict even when the row carries complete
// evidence: providers renumber ids, but the start path treats the absence as
// rotation, so the same-release retry stays off.
func TestTransportAbsentPinRotatesWithoutSameReleaseRetry(t *testing.T) {
	f := newFallbackInventoryFixture(t, "session-absent-pin")
	markCompletedDurationVerified(f.file)
	f.withSiblings(t, nil)

	var attemptsWithoutExclusions int
	f.handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(
		func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, excluded []string, _ string) (ResolvedVirtualMedia, error) {
			if len(excluded) == 0 {
				attemptsWithoutExclusions++
				return ResolvedVirtualMedia{}, errors.New("pinned candidate absent")
			}
			return ResolvedVirtualMedia{
				URL:         "http://localhost:8080/alternate-live.mp4",
				URI:         f.unprobedURI,
				CandidateID: "unprobed",
			}, nil
		})
	f.withTranscode(t)

	_ = f.run(t, "session-absent-pin")

	if attemptsWithoutExclusions != 1 {
		t.Fatalf("pinned attempts = %d, want exactly one (no same-release retry for an absent pin)", attemptsWithoutExclusions)
	}
	if len(f.started) != 1 || !strings.Contains(f.started[0].InputPath, "alternate-live.mp4") {
		t.Fatalf("started = %+v, want the alternate", f.started)
	}
}
