package playback

import (
	"errors"
	"testing"
	"time"
)

// markRestartContinuationForTest mirrors the bookkeeping a successful in-place
// restart performs. Keeping it in one place lets the fencing tests exercise the
// predecessor->successor relationship without spawning ffmpeg.
func markRestartContinuationForTest(s *TranscodeSession, startSegment int, recipeStable bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.segmentIncarnation == "" {
		s.segmentIncarnation = "test-incarnation"
	}
	predecessorToken := s.segmentGenerationTokenLocked()
	s.segmentGeneration++
	s.restartPredecessorToken = predecessorToken
	s.restartStartSegment = startSegment
	s.restartRecipeStable = recipeStable
	return predecessorToken
}

func testContinuationSession() *TranscodeSession {
	return &TranscodeSession{
		opts:                TranscodeOpts{SessionID: "session-1"},
		segmentIncarnation:  "inc",
		segmentGeneration:   1,
		restartRecipeStable: false,
	}
}

// Only the immediate predecessor of the live generation may be continued. A
// second restart retires the first predecessor even though the requested
// segment numbers still look valid, because generation A and generation C have
// no proven codec/track/init compatibility.
func TestRestartContinuationAcceptsOnlyImmediatePredecessor(t *testing.T) {
	session := testContinuationSession()
	tokenA := session.GenerationToken()

	if predecessor := markRestartContinuationForTest(session, 10, true); predecessor != tokenA {
		t.Fatalf("recorded predecessor token = %q, want the displaced token %q", predecessor, tokenA)
	}
	tokenB := session.GenerationToken()
	if _, successor, ok := session.RestartContinuationSegment(tokenA, "seg_00010.ts"); !ok || successor != tokenB {
		t.Fatalf("the immediate predecessor was not accepted at the restart point (successor=%q, want %q)", successor, tokenB)
	}
	if _, _, ok := session.RestartContinuationSegment(tokenA, "seg_00009.ts"); ok {
		t.Fatal("a segment before the restart point was accepted")
	}
	if _, _, ok := session.RestartContinuationSegment(tokenA, "init.mp4"); !ok {
		t.Fatal("init.mp4 was not accepted for an unchanged recipe")
	}

	if predecessor := markRestartContinuationForTest(session, 20, true); predecessor != tokenB {
		t.Fatalf("second predecessor token = %q, want %q", predecessor, tokenB)
	}
	tokenC := session.GenerationToken()
	if _, _, ok := session.RestartContinuationSegment(tokenA, "seg_00020.ts"); ok {
		t.Fatal("generation A's token was accepted by generation C")
	}
	if _, _, ok := session.RestartContinuationSegment(tokenB, "seg_00020.ts"); !ok {
		t.Fatal("generation B (the immediate predecessor) was not accepted by generation C")
	}
	if tokenC == tokenB {
		t.Fatal("the generation token did not rotate across the second restart")
	}
}

// A restart that changes the emitted recipe cleans the old segments and starts
// a different stream. Absolute segment numbers are then not enough: the old
// token must be refused rather than served with the successor's init/track.
func TestRestartContinuationRejectsRecipeChange(t *testing.T) {
	session := testContinuationSession()
	tokenA := session.GenerationToken()
	markRestartContinuationForTest(session, 10, false)
	if _, _, ok := session.RestartContinuationSegment(tokenA, "seg_00010.ts"); ok {
		t.Fatal("a recipe-changing restart continued the displaced generation")
	}
}

// The token carries the session incarnation, so a token from another session
// object (a reconstruct or a replacement attempt) never matches the predecessor
// relationship even if the numeric generation is the same.
func TestRestartContinuationRejectsOtherIncarnation(t *testing.T) {
	session := testContinuationSession()
	markRestartContinuationForTest(session, 10, true)
	other := &TranscodeSession{
		opts:               TranscodeOpts{SessionID: "session-1"},
		segmentIncarnation: "other-incarnation",
		segmentGeneration:  1,
	}
	if _, _, ok := session.RestartContinuationSegment(other.GenerationToken(), "seg_00010.ts"); ok {
		t.Fatal("a token from another incarnation was accepted")
	}
}

// Stopping a session advances the generation but is not a restart: the prior
// playlist's timeline ended there, so its token must not inherit continuation
// rights from the last successful restart.
func TestRestartContinuationRetiredOnStop(t *testing.T) {
	session := testContinuationSession()
	tokenA := session.GenerationToken()
	markRestartContinuationForTest(session, 10, true)
	if err := session.shutdownProcess(false, true); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if _, _, ok := session.RestartContinuationSegment(tokenA, "seg_00010.ts"); ok {
		t.Fatal("a stopped generation still offered restart continuation")
	}
}

// A request that already spent the continuation budget must answer the stale
// verdict immediately; the detached restart keeps producing for the playlist
// reload. This is the bound that keeps a stalled-head wait plus a restart from
// composing to roughly twice the client's read timeout.
func TestWaitForRestartContinuationSpentBudgetFailsFast(t *testing.T) {
	session := testContinuationSession()
	start := time.Now()
	_, err := session.WaitForRestartContinuationWithin("seg_00010.ts", RestartContinuationRemaining(start.Add(-2*SeekRestartServeTimeout)))
	if !errors.Is(err, ErrStaleSegmentGeneration) {
		t.Fatalf("spent-budget continuation error = %v, want ErrStaleSegmentGeneration", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("spent-budget continuation waited %s, want an immediate verdict", elapsed)
	}
}

func TestRestartContinuationRemainingAndStallBudgets(t *testing.T) {
	if remaining := RestartContinuationRemaining(time.Now().Add(-5 * time.Second)); remaining <= 0 || remaining > time.Second {
		t.Fatalf("remaining after 5s = %s, want the last second of the 6s ceiling", remaining)
	}
	if remaining := RestartContinuationRemaining(time.Now().Add(-10 * time.Second)); remaining != 0 {
		t.Fatalf("remaining after 10s = %s, want 0", remaining)
	}
	if SeekRestartStallTimeout >= SeekRestartServeTimeout {
		t.Fatalf("stall watchdog %s must be below the %s continuation ceiling", SeekRestartStallTimeout, SeekRestartServeTimeout)
	}
}
