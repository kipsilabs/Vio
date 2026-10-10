package handlers

import (
	"context"
	"testing"
	"time"
)

// phaseAttrs indexes the timing line's flat key/value attribute slice.
func phaseAttrs(t *testing.T, timings *playbackStartTimingsV3) map[string]any {
	t.Helper()
	attrs := make(map[string]any, len(timings.attrs)/2)
	for i := 0; i+1 < len(timings.attrs); i += 2 {
		name, ok := timings.attrs[i].(string)
		if !ok {
			t.Fatalf("timing attr %d = %T, want string", i, timings.attrs[i])
		}
		attrs[name] = timings.attrs[i+1]
	}
	return attrs
}

// TestTransportPhaseTimingsSplitTheCommit pins that every transport sub-phase
// lands on the start timing line under its own name, that the sub-phases do not
// disturb the composite session_transport_commit delta, and that a negative
// duration is clamped rather than producing a nonsensical negative millisecond.
func TestTransportPhaseTimingsSplitTheCommit(t *testing.T) {
	ctx, recorder := withTransportPhaseTimingsV3(context.Background())
	if recorder == nil {
		t.Fatal("withTransportPhaseTimingsV3 returned a nil recorder")
	}

	recordTransportPhaseV3(ctx, "transport_anchor", time.Now().Add(-25*time.Millisecond))
	addTransportPhaseDurationV3(ctx, "transport_spawn", 40*time.Millisecond)
	addTransportPhaseDurationV3(ctx, "transport_readiness", -5*time.Millisecond)

	timings := newPlaybackStartTimingsV3()
	timings.mark("session_transport_commit")
	timings.observeTransportPhasesV3(recorder)

	attrs := phaseAttrs(t, timings)
	for _, name := range transportPhaseNamesV3 {
		if _, ok := attrs[name+"_ms"]; !ok {
			t.Fatalf("phase %s was not emitted; attrs=%v", name, attrs)
		}
	}
	if got := attrs["transport_anchor_ms"].(int64); got < 20 || got > 200 {
		t.Fatalf("transport_anchor_ms = %d, want roughly 25", got)
	}
	if got := attrs["transport_spawn_ms"].(int64); got != 40 {
		t.Fatalf("transport_spawn_ms = %d, want 40", got)
	}
	if got := attrs["transport_readiness_ms"].(int64); got != 0 {
		t.Fatalf("transport_readiness_ms = %d, want 0 (a negative duration is clamped)", got)
	}
	// observeTransportPhasesV3 must append through the attribute slice directly,
	// never through mark: a mark would move t.last and shrink the next delta.
	// The composite mark was taken before observe ran, so it must still be the
	// only session_transport_commit entry on the line.
	var commitCount int
	for i := 0; i < len(timings.attrs); i += 2 {
		if timings.attrs[i] == "session_transport_commit_ms" {
			commitCount++
		}
	}
	if commitCount != 1 {
		t.Fatalf("session_transport_commit_ms appears %d times, want 1", commitCount)
	}
}

// TestTransportPhaseTimingsRequireARecorder pins that recording without an
// installed recorder is a no-op, so an uninstalled path pays nothing and cannot
// panic.
func TestTransportPhaseTimingsRequireARecorder(t *testing.T) {
	ctx := context.Background()
	if transportPhaseTimingsFromV3(ctx) != nil {
		t.Fatal("a bare context unexpectedly carries a phase recorder")
	}
	if got := transportPhaseDurationV3(ctx, "transport_spawn"); got != 0 {
		t.Fatalf("transportPhaseDurationV3 without a recorder = %v, want 0", got)
	}
	recordTransportPhaseV3(ctx, "transport_spawn", time.Now().Add(-time.Second))
	addTransportPhaseDurationV3(ctx, "transport_spawn", time.Second)
	// A nil recorder must not panic; the calls above returning is the assertion.
}

// TestTransportPhaseTimingsReplaceOnReattach pins that attaching a second
// recorder to a derived context replaces rather than accumulates, so a nested
// or retried start measures its own commit.
func TestTransportPhaseTimingsReplaceOnReattach(t *testing.T) {
	outer, outerRecorder := withTransportPhaseTimingsV3(context.Background())
	addTransportPhaseDurationV3(outer, "transport_spawn", 10*time.Millisecond)

	inner, innerRecorder := withTransportPhaseTimingsV3(outer)
	addTransportPhaseDurationV3(inner, "transport_spawn", 20*time.Millisecond)

	if innerRecorder == outerRecorder {
		t.Fatal("re-attaching returned the outer recorder; a nested start would accumulate")
	}
	if got := transportPhaseDurationV3(inner, "transport_spawn"); got != 20*time.Millisecond {
		t.Fatalf("inner transport_spawn = %v, want 20ms", got)
	}
	if got := transportPhaseDurationV3(outer, "transport_spawn"); got != 10*time.Millisecond {
		t.Fatalf("outer transport_spawn = %v, want 10ms (untouched)", got)
	}
}
