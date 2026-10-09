package handlers

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/cache"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/notifications"
)

// errFirstPlaySink is a sentinel persist failure for the failure-path test.
var errFirstPlaySink = errors.New("first-play sink persist failed")

// firstPlaySinkFile is the neutral source row a first playback resolves from
// before any candidate has been persisted.
func firstPlaySinkFile(contentID string) *models.MediaFile {
	return &models.MediaFile{
		ID:                         41,
		ContentID:                  contentID,
		FilePath:                   "virtual://movie/" + contentID,
		Container:                  virtualURIScheme,
		VirtualOwnerInstallationID: 5,
	}
}

// waitForRecordingEvent polls the recording bus until an event payload contains
// every needle, or fails after a bounded wait. It waits on observable state
// rather than a fixed sleep because the sink's persist and publish run on a
// detached goroutine.
func waitForRecordingEvent(t *testing.T, bus *recordingEventBus, timeout time.Duration, needles ...string) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if payload := recordingEventPayload(bus, needles[0]); payload != "" {
			matched := true
			for _, needle := range needles {
				if !strings.Contains(payload, needle) {
					matched = false
					break
				}
			}
			if matched {
				return payload
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no event containing %v within %s; payloads=%v", needles, timeout, recordingEventPayloads(bus))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestFirstPlaybackCandidateSinkPublishesVersionsUpdated is the item 2 proof:
// the first-playback sink, having persisted a freshly listed candidate set,
// publishes the same catalog.item.changed/change=versions_updated event the
// Refresh List executor emits. That event is what makes an open detail or watch
// page invalidate itemKeys.watchDetail and stop showing the seeded placeholder.
func TestFirstPlaybackCandidateSinkPublishesVersionsUpdated(t *testing.T) {
	bus := &recordingEventBus{}
	hub := notifications.NewHub("first-play-sink", bus)

	var mu sync.Mutex
	var persistCalls int
	var persisted []VirtualPlaybackStream
	h := &PlaybackHandler{
		EventsHub: hub.EventsHub(),
		VirtualPlaybackStreamSink: func(_ context.Context, _ *models.MediaFile, streams []VirtualPlaybackStream) error {
			mu.Lock()
			persistCalls++
			persisted = append([]VirtualPlaybackStream(nil), streams...)
			mu.Unlock()
			return nil
		},
	}
	file := firstPlaySinkFile("movie-first-play-sink")
	streams := []VirtualPlaybackStream{{
		ID: "c1", URI: file.FilePath + "?result=c1", Label: "Heat 1995 1080p WEB-DL x264-GRP",
		Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", OwnerInstallationID: 5,
	}}

	h.spawnVirtualCandidateSink(context.Background(), file, streams)

	payload := waitForRecordingEvent(t, bus, 5*time.Second, "catalog.item.changed", "versions_updated", file.ContentID)
	if !strings.Contains(payload, file.ContentID) {
		t.Fatalf("event payload %q does not name the content", payload)
	}

	mu.Lock()
	calls, saved := persistCalls, persisted
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("sink persist calls = %d, want 1", calls)
	}
	if len(saved) != 1 || saved[0].URI != streams[0].URI {
		t.Fatalf("sink persisted %+v, want the listed candidate", saved)
	}
}

// TestFirstPlaybackCandidateSinkFailurePublishesNothing proves the event is
// gated on a successful persist, matching the executor's post-persist ordering:
// a failed write must not tell clients to reload a version list that was never
// saved.
func TestFirstPlaybackCandidateSinkFailurePublishesNothing(t *testing.T) {
	bus := &recordingEventBus{}
	hub := notifications.NewHub("first-play-sink-fail", bus)

	sinkDone := make(chan struct{})
	h := &PlaybackHandler{
		EventsHub: hub.EventsHub(),
		VirtualPlaybackStreamSink: func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error {
			close(sinkDone)
			return errFirstPlaySink
		},
	}
	file := firstPlaySinkFile("movie-first-play-sink-fail")
	h.spawnVirtualCandidateSink(context.Background(), file, []VirtualPlaybackStream{{
		URI: file.FilePath + "?result=c1", OwnerInstallationID: 5,
	}})

	select {
	case <-sinkDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the first-playback sink never ran")
	}
	// The publish would run after the failed persist on the same goroutine;
	// poll for the negative outcome instead of sleeping.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if payload := recordingEventPayload(bus, "versions_updated"); payload != "" {
			t.Fatalf("a failed persist still published %q", payload)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestFirstPlaybackCandidateSinkBudgetExhaustedSkipsQuietly proves the sink
// keeps its bounded, detached semantics: when the detached-work gate is
// saturated the sink is skipped with no persist and no event, rather than
// queueing or blocking the request path. The version list then reloads on a
// later first open, which is the documented degradation.
func TestFirstPlaybackCandidateSinkBudgetExhaustedSkipsQuietly(t *testing.T) {
	bus := &recordingEventBus{}
	hub := notifications.NewHub("first-play-sink-saturated", bus)

	sinkCalled := make(chan struct{}, 1)
	h := &PlaybackHandler{
		EventsHub: hub.EventsHub(),
		VirtualPlaybackStreamSink: func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error {
			sinkCalled <- struct{}{}
			return nil
		},
	}
	// Occupy the single detached slot so the next admission is shed.
	h.detachedWorkGate = newVirtualDetachedGate(1)
	if !h.detachedGate().tryAcquire() {
		t.Fatal("failed to occupy the detached gate")
	}

	h.spawnVirtualCandidateSink(context.Background(), firstPlaySinkFile("movie-first-play-sink-saturated"), []VirtualPlaybackStream{{
		URI: "virtual://movie/movie-first-play-sink-saturated?result=c1", OwnerInstallationID: 5,
	}})

	select {
	case <-sinkCalled:
		t.Fatal("a saturated detached gate still ran the sink")
	case <-time.After(100 * time.Millisecond):
	}
	if payload := recordingEventPayload(bus, "versions_updated"); payload != "" {
		t.Fatalf("a skipped sink still published %q", payload)
	}
	h.detachedGate().release()
}

// TestFirstPlaybackCandidateSinkPublishesOnlyAfterPersist is the sequencing
// proof for the publish budget split: while the persist is still blocked no
// event may be published, and releasing the persist lets the announce run. It
// guards the "publish after success" ordering independently of the budget.
func TestFirstPlaybackCandidateSinkPublishesOnlyAfterPersist(t *testing.T) {
	bus := &recordingEventBus{}
	hub := notifications.NewHub("first-play-sink-sequence", bus)

	persistStarted := make(chan struct{})
	releasePersist := make(chan struct{})
	h := &PlaybackHandler{
		EventsHub: hub.EventsHub(),
		VirtualPlaybackStreamSink: func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error {
			close(persistStarted)
			<-releasePersist
			return nil
		},
	}
	file := firstPlaySinkFile("movie-first-play-sink-sequence")

	h.spawnVirtualCandidateSink(context.Background(), file, []VirtualPlaybackStream{{
		URI: file.FilePath + "?result=c1", OwnerInstallationID: 5,
	}})

	select {
	case <-persistStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the first-playback sink never started its persist")
	}
	// The persist is blocked; the announce is gated on the write landing, so
	// nothing may be on the bus yet. Poll the negative outcome instead of
	// sleeping a fixed amount.
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		if payload := recordingEventPayload(bus, "versions_updated"); payload != "" {
			t.Fatalf("published %q before the persist returned", payload)
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(releasePersist)
	payload := waitForRecordingEvent(t, bus, 5*time.Second, "catalog.item.changed", "versions_updated", file.ContentID)
	if !strings.Contains(payload, file.ContentID) {
		t.Fatalf("event payload %q does not name the content", payload)
	}
}

// ctxCapturingEventBus records the liveness of the context each publish used.
// The hub fans out to local subscribers before the bus, so only the bus call
// proves the Redis (cross-node) publish actually ran on a live context.
type ctxCapturingEventBus struct {
	mu       sync.Mutex
	errs     []error
	payloads []string
	// waitForDone, when positive, makes Publish wait for ctx cancellation (up
	// to that bound) before recording. Shutdown propagation through
	// context.AfterFunc is asynchronous, so without this the assertion would be
	// racy; with it, a shutdown-bound context is deterministically observed as
	// canceled and a shutdown-detached one as live.
	waitForDone time.Duration
}

func (b *ctxCapturingEventBus) Publish(ctx context.Context, _ string, event cache.Event) error {
	if b.waitForDone > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(b.waitForDone):
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.errs = append(b.errs, ctx.Err())
	b.payloads = append(b.payloads, event.Payload)
	return nil
}

func (b *ctxCapturingEventBus) Subscribe(context.Context, string, cache.EventHandler) error {
	return nil
}
func (b *ctxCapturingEventBus) Close() error { return nil }

func (b *ctxCapturingEventBus) snapshot() ([]error, []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]error(nil), b.errs...), append([]string(nil), b.payloads...)
}

// waitForCapturedPublish polls the capturing bus until it recorded a publish,
// returning the context error it observed.
func waitForCapturedPublish(t *testing.T, bus *ctxCapturingEventBus, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		errs, payloads := bus.snapshot()
		if len(payloads) > 0 {
			return errs[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("no publish reached the bus within the wait window")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestFirstPlaybackCandidateSinkPublishSurvivesPersistDeadline is the
// deadline-boundary proof for the Major finding: a persist that consumes its
// whole sink budget but still succeeds must leave the follow-up publish a live
// context, so the Redis fan-out that refreshes the other API nodes is not
// skipped while the local node (fanned out first, inside the hub) refreshes.
func TestFirstPlaybackCandidateSinkPublishSurvivesPersistDeadline(t *testing.T) {
	previousBudget := virtualCandidateSinkBudget
	virtualCandidateSinkBudget = 40 * time.Millisecond
	t.Cleanup(func() { virtualCandidateSinkBudget = previousBudget })

	bus := &ctxCapturingEventBus{}
	hub := notifications.NewHub("first-play-sink-deadline", bus)

	persistDone := make(chan struct{})
	h := &PlaybackHandler{
		EventsHub: hub.EventsHub(),
		VirtualPlaybackStreamSink: func(ctx context.Context, _ *models.MediaFile, _ []VirtualPlaybackStream) error {
			// Drain the entire sink budget, then succeed. The sink context is
			// dead by the time this returns, which is exactly the boundary the
			// publish must not inherit.
			<-ctx.Done()
			close(persistDone)
			return nil
		},
	}
	file := firstPlaySinkFile("movie-first-play-sink-deadline")

	h.spawnVirtualCandidateSink(context.Background(), file, []VirtualPlaybackStream{{
		URI: file.FilePath + "?result=c1", OwnerInstallationID: 5,
	}})

	select {
	case <-persistDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the sink persist never reached its deadline")
	}

	if err := waitForCapturedPublish(t, bus, 5*time.Second); err != nil {
		t.Fatalf("versions_updated published on a dead context after a budget-consuming persist: %v", err)
	}
	_, payloads := bus.snapshot()
	if !strings.Contains(payloads[0], file.ContentID) {
		t.Fatalf("event payload %q does not name the content", payloads[0])
	}
}

// TestFirstPlaybackCandidateSinkPublishDetachesFromRequestCancel proves the
// publish context is detached from the request/call-site cancellation: a caller
// whose context is already canceled (browser gave up, client disconnected) must
// still let the committed version-list invalidation reach the bus, the way the
// pool reload survives request cancellation.
func TestFirstPlaybackCandidateSinkPublishDetachesFromRequestCancel(t *testing.T) {
	bus := &ctxCapturingEventBus{}
	hub := notifications.NewHub("first-play-sink-request-cancel", bus)

	h := &PlaybackHandler{
		EventsHub: hub.EventsHub(),
		VirtualPlaybackStreamSink: func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error {
			return nil
		},
	}
	file := firstPlaySinkFile("movie-first-play-sink-request-cancel")
	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()

	h.spawnVirtualCandidateSink(requestCtx, file, []VirtualPlaybackStream{{
		URI: file.FilePath + "?result=c1", OwnerInstallationID: 5,
	}})

	if err := waitForCapturedPublish(t, bus, 5*time.Second); err != nil {
		t.Fatalf("versions_updated did not survive request cancellation: %v", err)
	}
	_, payloads := bus.snapshot()
	if !strings.Contains(payloads[0], file.ContentID) {
		t.Fatalf("event payload %q does not name the content", payloads[0])
	}
}

// TestFirstPlaybackCandidateSinkPublishStopsAtShutdown covers the shutdown
// side of the fresh publish context: the publish must stay parented to service
// shutdown, so a service context canceled while the persist is in flight is
// observed as canceled by the bus call and the Redis fan-out aborts rather than
// outliving shutdown. The bus waits for cancellation to absorb the async
// context.AfterFunc propagation before recording.
func TestFirstPlaybackCandidateSinkPublishStopsAtShutdown(t *testing.T) {
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	bus := &ctxCapturingEventBus{waitForDone: 2 * time.Second}
	hub := notifications.NewHub("first-play-sink-shutdown", bus)

	persistStarted := make(chan struct{})
	releasePersist := make(chan struct{})
	h := &PlaybackHandler{
		ServiceContext: serviceCtx,
		EventsHub:      hub.EventsHub(),
		VirtualPlaybackStreamSink: func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error {
			close(persistStarted)
			<-releasePersist
			return nil
		},
	}
	file := firstPlaySinkFile("movie-first-play-sink-shutdown")

	h.spawnVirtualCandidateSink(context.Background(), file, []VirtualPlaybackStream{{
		URI: file.FilePath + "?result=c1", OwnerInstallationID: 5,
	}})

	select {
	case <-persistStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the first-playback sink never started its persist")
	}

	// Shut the service down while the persist is in flight, then let the
	// persist succeed. The follow-up publish is bound to shutdown, so its bus
	// call must observe a dead context.
	serviceCancel()
	close(releasePersist)

	if err := waitForCapturedPublish(t, bus, 5*time.Second); err == nil {
		t.Fatal("versions_updated reached the bus on a live context after service shutdown")
	}
}
