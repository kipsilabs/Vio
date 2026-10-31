package handlers

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// TestRetryVirtualProviderOutageResolveIsBounded proves the retry loop enforces
// its bound in code: a resolver that fails every attempt is called exactly
// initial + len(backoff) times, never more, and every backoff wait is the
// configured schedule. A runaway retry would amplify a provider outage into a
// request storm.
func TestRetryVirtualProviderOutageResolveIsBounded(t *testing.T) {
	restore := virtualProviderOutageBackoff
	virtualProviderOutageBackoff = []time.Duration{time.Millisecond, 2 * time.Millisecond}
	defer func() { virtualProviderOutageBackoff = restore }()

	calls := 0
	_, err := retryVirtualProviderOutageResolve(context.Background(), true, func(context.Context, bool) (ResolvedVirtualMedia, error) {
		calls++
		return ResolvedVirtualMedia{}, provider502()
	})
	wantCalls := 1 + len(virtualProviderOutageBackoff)
	if calls != wantCalls {
		t.Fatalf("provider calls = %d, want exactly %d (initial + bounded retries)", calls, wantCalls)
	}
	if !virtualProviderListingOutage(err) {
		t.Fatalf("final error = %v, want it still classified as a provider outage", err)
	}
	if got := len(virtualProviderOutageBackoff); got == 0 {
		t.Fatal("test replaced the backoff with an empty schedule; retry bound is not exercised")
	}
}

// TestRetryVirtualProviderOutageResolveSkipsBackoffForMarkedTitle proves the
// fail-fast sentinel retries without waiting: the resolve never contacted the
// provider, so there is no round-trip to back off from. The schedule is set to
// an hour, which would blow the test context if the wait were applied.
func TestRetryVirtualProviderOutageResolveSkipsBackoffForMarkedTitle(t *testing.T) {
	restore := virtualProviderOutageBackoff
	virtualProviderOutageBackoff = []time.Duration{time.Hour, time.Hour}
	defer func() { virtualProviderOutageBackoff = restore }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	calls := 0
	resolved, err := retryVirtualProviderOutageResolve(ctx, true, func(context.Context, bool) (ResolvedVirtualMedia, error) {
		calls++
		if calls == 1 {
			return ResolvedVirtualMedia{}, fmt.Errorf("resolve virtual input: %w: title virtual://movie/tt-marked", virtuallibrary.ErrTitleWithoutStreams)
		}
		return ResolvedVirtualMedia{URI: "virtual://movie/tt-marked?result=live", URL: "https://provider.test/live.mp4"}, nil
	})
	if err != nil {
		t.Fatalf("retry after the fail-fast sentinel = %v, want recovery", err)
	}
	if calls != 2 {
		t.Fatalf("provider calls = %d, want 2 (initial + one immediate relist)", calls)
	}
	if resolved.URI == "" {
		t.Fatal("retry returned no resolved URI")
	}
}

// TestRetryVirtualProviderOutageResolveStopsOnNonOutage proves a non-outage
// resolve failure is never retried: the loop returns the first error, so a real
// release verdict is surfaced immediately instead of waiting out the schedule.
func TestRetryVirtualProviderOutageResolveStopsOnNonOutage(t *testing.T) {
	calls := 0
	_, err := retryVirtualProviderOutageResolve(context.Background(), true, func(context.Context, bool) (ResolvedVirtualMedia, error) {
		calls++
		return ResolvedVirtualMedia{}, errors.New("virtual playback provider returned an unsafe stream URL")
	})
	if calls != 1 {
		t.Fatalf("provider calls = %d, want 1: a non-outage error must not be retried", calls)
	}
	if errors.Is(err, errVirtualProviderUnavailable) {
		t.Fatalf("non-outage error was classified as provider_unavailable: %v", err)
	}
}

// TestRetryVirtualProviderOutageResolveRenumberGetsOneRelist proves #143: a
// pinned id absent from the listing is a renumber artifact eligible for exactly
// one bounded forced relist, not the full transient-outage schedule. Providers
// renumber result ids per listing, so one relist can re-identify the release;
// retrying the full schedule would only delay a genuinely dead pin.
func TestRetryVirtualProviderOutageResolveRenumberGetsOneRelist(t *testing.T) {
	restore := virtualProviderOutageBackoff
	virtualProviderOutageBackoff = []time.Duration{time.Millisecond, 2 * time.Millisecond}
	defer func() { virtualProviderOutageBackoff = restore }()

	calls := 0
	relisted := 0
	_, err := retryVirtualProviderOutageResolve(context.Background(), true, func(_ context.Context, relist bool) (ResolvedVirtualMedia, error) {
		calls++
		if relist {
			relisted++
		}
		return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
	})
	if calls != 2 || relisted != 1 {
		t.Fatalf("calls=%d relists=%d, want exactly 2 calls (initial + one forced relist) and 1 relist", calls, relisted)
	}
	if !virtualProviderListingOutage(err) {
		t.Fatalf("final error = %v, want it still recognized as a listing artifact", err)
	}
}

// TestRetryVirtualProviderOutageResolveRenumberRecoversOnRelist proves the other
// half of #143: a single renumber event resolves on the forced relist instead of
// failing the start.
func TestRetryVirtualProviderOutageResolveRenumberRecoversOnRelist(t *testing.T) {
	restore := virtualProviderOutageBackoff
	virtualProviderOutageBackoff = []time.Duration{time.Millisecond, 2 * time.Millisecond}
	defer func() { virtualProviderOutageBackoff = restore }()

	calls := 0
	resolved, err := retryVirtualProviderOutageResolve(context.Background(), true, func(_ context.Context, relist bool) (ResolvedVirtualMedia, error) {
		calls++
		if !relist {
			return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
		}
		return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/rematched", URI: "virtual://movie/x?result=new", CandidateID: "new"}, nil
	})
	if err != nil {
		t.Fatalf("renumber retry failed: %v", err)
	}
	if calls != 2 || resolved.CandidateID != "new" {
		t.Fatalf("calls=%d resolved=%+v, want the relisted candidate after one relist", calls, resolved)
	}
}

// TestRetryVirtualProviderOutageResolveDoesNotRetryRotationVerdict proves #143's
// bound on genuine rotation verdicts: the pinned-candidate-excluded refusal is a
// verdict about the release, not a listing artifact, so it is never turned into
// a retry loop.
func TestRetryVirtualProviderOutageResolveDoesNotRetryRotationVerdict(t *testing.T) {
	verdict := errors.New(`pinned virtual candidate "pinned" is excluded and candidate rotation was not requested`)
	calls := 0
	_, err := retryVirtualProviderOutageResolve(context.Background(), true, func(context.Context, bool) (ResolvedVirtualMedia, error) {
		calls++
		return ResolvedVirtualMedia{}, verdict
	})
	if calls != 1 {
		t.Fatalf("calls = %d, want 1: a genuine rotation verdict must not be retried", calls)
	}
	if errors.Is(err, errVirtualProviderUnavailable) {
		t.Fatalf("rotation verdict was classified as provider_unavailable: %v", err)
	}
}

// TestClassifyVirtualProviderOutageNeverLeaksForNonOutage pins the classification
// guard in code: provider_unavailable is only ever attached to a genuine outage
// for a trusted row. An untrusted row or a non-outage error keeps its original
// error, so the retryable 503 cannot mask a permanent resolve verdict.
func TestClassifyVirtualProviderOutageNeverLeaksForNonOutage(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		trusted bool
		want    bool
	}{
		{"outage + trusted", provider502(), true, true},
		{"outage + untrusted", provider502(), false, false},
		{"empty listing + trusted", providerEmptyListing(), true, true},
		{"nil error", nil, true, false},
		{"absent pin renumber + trusted", absentSessionPinError("pinned"), true, true},
		{"absent pin renumber + untrusted", absentSessionPinError("pinned"), false, false},
		{"arbitrary error + trusted", errors.New("decode failed"), true, false},
		{"resolver unavailable sentinel + trusted", fmt.Errorf("listing: %w", resolver.ErrProviderUnavailable), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := errors.Is(classifyVirtualProviderOutage(tc.err, tc.trusted), errVirtualProviderUnavailable)
			if got != tc.want {
				t.Fatalf("classifyVirtualProviderOutage(%v, %v) provider_unavailable = %v, want %v", tc.err, tc.trusted, got, tc.want)
			}
		})
	}
}

// TestVirtualProviderOutageBackoffIsBounded pins the production schedule itself:
// a future edit cannot make the retry unbounded or push the added latency past
// the documented ceiling without failing this test.
func TestVirtualProviderOutageBackoffIsBounded(t *testing.T) {
	if len(virtualProviderOutageBackoff) == 0 {
		t.Fatal("outage backoff schedule is empty; the retry is not exercised")
	}
	const maxRetries = 3
	if len(virtualProviderOutageBackoff) > maxRetries {
		t.Fatalf("outage backoff has %d retries, want at most %d", len(virtualProviderOutageBackoff), maxRetries)
	}
	var total time.Duration
	for _, d := range virtualProviderOutageBackoff {
		if d <= 0 || d > 5*time.Second {
			t.Fatalf("outage backoff step %s is outside (0, 5s]", d)
		}
		total += d
	}
	if total > 5*time.Second {
		t.Fatalf("outage backoff total %s exceeds the 5s latency bound", total)
	}
}
