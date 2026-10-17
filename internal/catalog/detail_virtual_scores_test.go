package catalog

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestAttachVirtualCandidateScoresStampsMatchingVersions(t *testing.T) {
	var gotContentID string
	svc := &DetailService{virtualScoreSource: func(_ context.Context, contentID string, _ []*models.MediaFile) map[int]VirtualCandidateScore {
		gotContentID = contentID
		return map[int]VirtualCandidateScore{7: {Score: 850, MatchedFormats: []string{"English"}}}
	}}

	versions := []FileVersion{{FileID: 7}, {FileID: 8}}
	svc.attachVirtualCandidateScores(context.Background(), "movie:heat", versions, []*models.MediaFile{{ID: 7}})

	if gotContentID != "movie:heat" {
		t.Fatalf("scorer content id = %q, want movie:heat", gotContentID)
	}
	if versions[0].FormatScore == nil || *versions[0].FormatScore != 850 {
		t.Fatalf("scored version score = %v, want 850", versions[0].FormatScore)
	}
	if len(versions[0].MatchedFormats) != 1 || versions[0].MatchedFormats[0] != "English" {
		t.Fatalf("scored version formats = %v, want [English]", versions[0].MatchedFormats)
	}
	if versions[1].FormatScore != nil {
		t.Fatalf("unscored version score = %v, want nil", versions[1].FormatScore)
	}
	if versions[1].MatchedFormats != nil {
		t.Fatalf("unscored version formats = %v, want nil", versions[1].MatchedFormats)
	}
}

func TestAttachVirtualCandidateScoresWithoutSourceIsNoop(t *testing.T) {
	svc := &DetailService{}
	versions := []FileVersion{{FileID: 7}}

	svc.attachVirtualCandidateScores(context.Background(), "movie:heat", versions, nil)

	if versions[0].FormatScore != nil {
		t.Fatalf("score = %v, want nil when no source is wired", versions[0].FormatScore)
	}
}

// TestAttachVirtualCandidateScoresTimesOutFailOpen pins the watch-response
// guarantee: a score source that blocks (a cold virtual candidate cache waits
// on a provider round-trip) must not hold the response. The lookup is bounded
// by virtualScoreTimeout and the versions are served unannotated. The source
// here deliberately ignores context cancellation, proving the fail-open does
// not depend on a well-behaved scorer.
func TestAttachVirtualCandidateScoresTimesOutFailOpen(t *testing.T) {
	prev := virtualScoreTimeout
	virtualScoreTimeout = 20 * time.Millisecond
	t.Cleanup(func() { virtualScoreTimeout = prev })

	released := make(chan struct{})
	svc := &DetailService{virtualScoreSource: func(_ context.Context, _ string, _ []*models.MediaFile) map[int]VirtualCandidateScore {
		<-released
		return map[int]VirtualCandidateScore{7: {Score: 850}}
	}}

	versions := []FileVersion{{FileID: 7}}
	start := time.Now()
	svc.attachVirtualCandidateScores(context.Background(), "movie:heat", versions, []*models.MediaFile{{ID: 7}})
	elapsed := time.Since(start)
	close(released)

	if elapsed > time.Second {
		t.Fatalf("score lookup blocked for %v, want the short deadline to fail it open", elapsed)
	}
	if versions[0].FormatScore != nil {
		t.Fatalf("score = %v, want nil when the lookup times out", versions[0].FormatScore)
	}
}

// TestAttachVirtualCandidateScoresRecoversScorerPanic pins the other half of
// the fail-open guarantee: the scorer runs on a background goroutine now, so a
// panic there must be contained rather than crashing the process while the
// watch response serves unannotated.
func TestAttachVirtualCandidateScoresRecoversScorerPanic(t *testing.T) {
	svc := &DetailService{virtualScoreSource: func(context.Context, string, []*models.MediaFile) map[int]VirtualCandidateScore {
		panic("scorer blew up")
	}}

	versions := []FileVersion{{FileID: 7}}
	svc.attachVirtualCandidateScores(context.Background(), "movie:heat", versions, []*models.MediaFile{{ID: 7}})

	if versions[0].FormatScore != nil {
		t.Fatalf("score = %v, want nil after a scorer panic", versions[0].FormatScore)
	}
}

// TestAttachVirtualCandidateScoresBoundsConcurrentScorers pins that a scorer
// that ignores cancellation cannot strand an unbounded number of goroutines.
// Every slot is filled with a scorer that never returns; the next lookup must
// fail open without starting another scorer.
func TestAttachVirtualCandidateScoresBoundsConcurrentScorers(t *testing.T) {
	prev := virtualScoreTimeout
	virtualScoreTimeout = 20 * time.Millisecond
	t.Cleanup(func() { virtualScoreTimeout = prev })

	released := make(chan struct{})
	var started atomic.Int64
	svc := &DetailService{virtualScoreSource: func(_ context.Context, _ string, _ []*models.MediaFile) map[int]VirtualCandidateScore {
		started.Add(1)
		<-released
		return nil
	}}
	t.Cleanup(func() { close(released) })

	for i := 0; i < virtualScoreConcurrency; i++ {
		svc.attachVirtualCandidateScores(context.Background(), "movie:heat", []FileVersion{{FileID: 7}}, []*models.MediaFile{{ID: 7}})
	}
	deadline := time.Now().Add(2 * time.Second)
	for started.Load() < int64(virtualScoreConcurrency) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := started.Load(); got != int64(virtualScoreConcurrency) {
		t.Fatalf("scorers started = %d, want %d", got, virtualScoreConcurrency)
	}

	// Every slot is held, so this lookup must fail open without spawning.
	versions := []FileVersion{{FileID: 7}}
	svc.attachVirtualCandidateScores(context.Background(), "movie:heat", versions, []*models.MediaFile{{ID: 7}})
	if versions[0].FormatScore != nil {
		t.Fatalf("score = %v, want nil when every slot is held", versions[0].FormatScore)
	}
	if got := started.Load(); got != int64(virtualScoreConcurrency) {
		t.Fatalf("scorers started = %d after saturation, want %d (no unbounded spawn)", got, virtualScoreConcurrency)
	}
}
