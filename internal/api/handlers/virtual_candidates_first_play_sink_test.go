package handlers

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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
