package altmount

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// concurrentAltmountServer answers history and queue requests while counting
// each, and can hold the queue response to model a slow queue endpoint. The
// first queue request closes firstQueue so a test can observe the moment history
// has already been published.
type concurrentAltmountServer struct {
	historyCalls atomic.Int32
	queueCalls   atomic.Int32
	queueDelay   time.Duration
	firstQueue   chan struct{}
	firstQueueOn sync.Once
}

func newConcurrentAltmountServer(queueDelay time.Duration) *concurrentAltmountServer {
	return &concurrentAltmountServer{queueDelay: queueDelay, firstQueue: make(chan struct{})}
}

func (m *concurrentAltmountServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Query().Get("mode") {
	case "history":
		m.historyCalls.Add(1)
		_, _ = io.WriteString(w, historyPayload(`{"name":"Concurrent.Release.2024","nzb_name":"Concurrent.Release.2024.nzb","status":"Completed","storage":"/downloads/Concurrent.Release.2024","bytes":1000,"completetime":`+fmt.Sprint(time.Now().Unix())+`}`))
	case "queue":
		m.queueCalls.Add(1)
		m.firstQueueOn.Do(func() { close(m.firstQueue) })
		if m.queueDelay > 0 {
			select {
			case <-time.After(m.queueDelay):
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, altmountQueuePayload(`{"filename":"In.Flight.Release.2024.nzb","status":"Downloading","mbleft":"100","timeleft":"0:05:00"}`))
	default:
		http.Error(w, "unknown mode", http.StatusBadRequest)
	}
}

// TestRefreshConcurrentSlowQueuePublishesHistoryAndStaysBounded is the bounded
// concurrency evidence for Blocker 4's slow-queue path. Several refreshes run at
// once against an AltMount whose queue endpoint pauses for a quarter of the
// queue budget. It proves the history verdict is published before the queue is
// even requested, that every refresh still completes inside the queue budget,
// and that the concurrency adds no failure.
//
// The exercise is small and time-bounded: it must finish in well under a second,
// so it demonstrates bounded behavior under concurrency, not throughput.
func TestRefreshConcurrentSlowQueuePublishesHistoryAndStaysBounded(t *testing.T) {
	const (
		refreshers    = 8
		queueDelay    = 250 * time.Millisecond
		requestSlack  = 2 * time.Second
		perRefreshCap = altmountQueueBudget + requestSlack
	)
	release := "Concurrent.Release.2024"

	server := newConcurrentAltmountServer(queueDelay)
	srv := httptest.NewServer(server)
	defer srv.Close()

	client := New(srv.Client())
	client.Configure(srv.URL, "", 15)

	start := make(chan struct{})
	errs := make([]error, refreshers)
	elapsed := make([]time.Duration, refreshers)
	var wg sync.WaitGroup
	wallStart := time.Now()
	for i := 0; i < refreshers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			refreshStart := time.Now()
			errs[i] = client.Refresh(context.Background())
			elapsed[i] = time.Since(refreshStart)
		}(i)
	}
	close(start)

	// Once the queue endpoint has been reached, at least one refresh has already
	// committed the merged history snapshot: history is published before the
	// queue round-trip, so a slow queue cannot delay completion visibility.
	select {
	case <-server.firstQueue:
	case <-time.After(5 * time.Second):
		t.Fatal("queue endpoint was never requested")
	}
	if completed, known := client.ReleaseCompleted(release); !known || !completed {
		t.Fatalf("history not published before the queue fetch under concurrent refresh: ReleaseCompleted = (%v, %v)", completed, known)
	}

	wg.Wait()
	wall := time.Since(wallStart)

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent refresh %d: %v", i, err)
		}
	}
	if completed, known := client.ReleaseCompleted(release); !known || !completed {
		t.Fatalf("ReleaseCompleted after refreshes = (%v, %v), want (true, true)", completed, known)
	}
	// One history fetch and one queue fetch per refresh, no more and no less.
	if got := server.historyCalls.Load(); got != int32(refreshers) {
		t.Fatalf("history calls = %d, want %d", got, refreshers)
	}
	if got := server.queueCalls.Load(); got != int32(refreshers) {
		t.Fatalf("queue calls = %d, want %d (one bounded queue fetch per refresh)", got, refreshers)
	}
	// Each refresh waits the queue delay once and is capped by the queue budget;
	// the slow queue must never extend a refresh past that bound.
	for i, d := range elapsed {
		if d > perRefreshCap {
			t.Fatalf("refresh %d took %v, want <= %v (queue budget %v + %v)", i, d, perRefreshCap, altmountQueueBudget, requestSlack)
		}
	}
	if wall > perRefreshCap {
		t.Fatalf("concurrent refresh wall time %v, want <= %v", wall, perRefreshCap)
	}
	t.Logf("altmount bounded-concurrency: refreshers=%d history_calls=%d queue_calls=%d queue_delay=%v wall=%v",
		refreshers, server.historyCalls.Load(), server.queueCalls.Load(), queueDelay, wall.Round(time.Millisecond))
}
