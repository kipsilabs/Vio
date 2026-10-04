package altmount

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// serialAltmountServer answers history and queue requests from per-ordinal
// response lists, so two refreshes against one server observe different
// provider answers. It can gate one queue request to hold a refresh inside its
// queue-enrichment window.
type serialAltmountServer struct {
	mu         sync.Mutex
	histories  []string
	queues     []string
	histCalls  int
	queueCalls int

	gateQueueOrdinal int
	queueGate        chan struct{}
	queueEntered     chan struct{}
	queueEnteredOnce sync.Once
}

func (s *serialAltmountServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	body := ""
	gated := false
	switch r.URL.Query().Get("mode") {
	case "history":
		body = pickOrdinal(s.histories, s.histCalls)
		s.histCalls++
	case "queue":
		body = pickOrdinal(s.queues, s.queueCalls)
		s.queueCalls++
		gated = s.gateQueueOrdinal > 0 && s.queueCalls == s.gateQueueOrdinal
	}
	s.mu.Unlock()

	if gated {
		s.queueEnteredOnce.Do(func() { close(s.queueEntered) })
		select {
		case <-s.queueGate:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// pickOrdinal returns the body for a 0-based request ordinal, clamping to the
// last entry so an unexpected extra request cannot index out of range.
func pickOrdinal(bodies []string, ordinal int) string {
	if len(bodies) == 0 {
		return ""
	}
	if ordinal >= len(bodies) {
		ordinal = len(bodies) - 1
	}
	return bodies[ordinal]
}

// TestRefreshSerializesConcurrentCyclesWithStateFile is the Blocker 2 evidence
// for the in-memory publish window. Two refreshes run against one AltMount that
// answers with DIFFERENT history, and a state file is configured so the winner's
// snapshot is observable on disk. The first refresh is held between building its
// merged snapshot and publishing it; the second is launched while the first is
// held. On the fixed code the second refresh waits on the refresh cycle lock, so
// after the first persists, the second folds its release against the first's and
// the file holds both. Without serialization the second would build from the
// pre-publish state, finish, and then be overwritten by the first's stale
// snapshot, losing its release from both memory and disk.
func TestRefreshSerializesConcurrentCyclesWithStateFile(t *testing.T) {
	alpha := "Alpha.Release.2024"
	beta := "Beta.Release.2024"
	server := &serialAltmountServer{
		histories: []string{
			historyPayload(`{"name":"` + alpha + `","nzb_name":"` + alpha + `.nzb","status":"Completed","storage":"/downloads/` + alpha + `","bytes":1000,"completetime":` + unixAgoString(time.Hour) + `}`),
			historyPayload(`{"name":"` + beta + `","nzb_name":"` + beta + `.nzb","status":"Completed","storage":"/downloads/` + beta + `","bytes":2000,"completetime":` + unixAgoString(time.Minute) + `}`),
		},
		queues: []string{altmountQueuePayload(""), altmountQueuePayload("")},
	}
	srv := httptest.NewServer(server)
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "altmount-state.json")
	client := New(srv.Client())
	client.Configure(srv.URL, "", 15)
	if err := client.ConfigureIndexFile(path); err != nil {
		t.Fatalf("ConfigureIndexFile: %v", err)
	}

	// The first refresh to reach the build hook is held before it publishes;
	// the second must not be able to build at all until the first finishes.
	var built atomic.Int32
	aBuilt := make(chan struct{})
	aRelease := make(chan struct{})
	client.refreshHistoryBuiltHook = func() {
		if built.Add(1) == 1 {
			close(aBuilt)
			<-aRelease
		}
	}

	aDone := make(chan error, 1)
	go func() { aDone <- client.Refresh(context.Background()) }()
	select {
	case <-aBuilt:
	case <-time.After(5 * time.Second):
		t.Fatal("first refresh never reached its build window")
	}

	bStarted := make(chan struct{})
	bDone := make(chan error, 1)
	bFinished := make(chan struct{})
	go func() {
		close(bStarted)
		err := client.Refresh(context.Background())
		bDone <- err
		close(bFinished)
	}()
	<-bStarted

	// Give the second refresh a bounded chance to overtake the held first. On
	// the fixed code it is blocked on the cycle lock and cannot; the bound is
	// only a failure guard, the release below is what drives the test.
	select {
	case <-bFinished:
		t.Log("second refresh completed while the first was held in its build window")
	case <-time.After(1 * time.Second):
	}
	close(aRelease)

	if err := <-aDone; err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if err := <-bDone; err != nil {
		t.Fatalf("second refresh: %v", err)
	}

	client.mu.Lock()
	state := client.state
	client.mu.Unlock()
	onDisk := readAltmountStateFile(t, path)

	for _, tc := range []struct {
		name     string
		snapshot altmountStateSnapshot
	}{
		{"in-memory", state},
		{"on-disk", onDisk},
	} {
		for _, release := range []string{alpha, beta} {
			if _, ok := tc.snapshot.Completed[ReleaseKey(release)]; !ok {
				t.Fatalf("%s snapshot lost %q under concurrent refresh: completed=%v", tc.name, release, keysOf(tc.snapshot.Completed))
			}
		}
	}
}

// TestRefreshSerializesQueueEnrichmentAndPersistence is the Blocker 2 evidence
// for the queue-finalization window: the first refresh is held inside its
// best-effort queue fetch (after publishing history but before persisting), the
// second is launched with a DIFFERENT history and queue, and a state file is
// configured. The fixed code serializes the entire cycle, so the second's build
// and persist land strictly after the first's persist. The recorded cycle-event
// order is [build, persist, build, persist]; without the cycle lock the second
// would build and persist while the first was still in its queue fetch, giving
// [build, build, persist, persist] and letting the first's later persist write
// an older snapshot over the newer one.
func TestRefreshSerializesQueueEnrichmentAndPersistence(t *testing.T) {
	alpha := "Alpha.Release.2024"
	beta := "Beta.Release.2024"
	server := &serialAltmountServer{
		histories: []string{
			historyPayload(`{"name":"` + alpha + `","nzb_name":"` + alpha + `.nzb","status":"Completed","storage":"/downloads/` + alpha + `","bytes":1000,"completetime":` + unixAgoString(time.Hour) + `}`),
			historyPayload(`{"name":"` + beta + `","nzb_name":"` + beta + `.nzb","status":"Completed","storage":"/downloads/` + beta + `","bytes":2000,"completetime":` + unixAgoString(time.Minute) + `}`),
		},
		queues: []string{
			altmountQueuePayload(`{"filename":"In.Flight.Alpha.2024.nzb","status":"Downloading","mbleft":"100","timeleft":"0:05:00"}`),
			altmountQueuePayload(`{"filename":"In.Flight.Beta.2024.nzb","status":"Downloading","mbleft":"50","timeleft":"0:02:00"}`),
		},
		gateQueueOrdinal: 1,
		queueGate:        make(chan struct{}),
		queueEntered:     make(chan struct{}),
	}
	srv := httptest.NewServer(server)
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "altmount-state.json")
	client := New(srv.Client())
	client.Configure(srv.URL, "", 15)
	if err := client.ConfigureIndexFile(path); err != nil {
		t.Fatalf("ConfigureIndexFile: %v", err)
	}

	var eventMu sync.Mutex
	var events []string
	record := func(event string) {
		eventMu.Lock()
		events = append(events, event)
		eventMu.Unlock()
	}
	client.refreshHistoryBuiltHook = func() { record("build") }
	client.refreshPublishedHook = func() { record("persist") }

	aDone := make(chan error, 1)
	go func() { aDone <- client.Refresh(context.Background()) }()
	select {
	case <-server.queueEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first refresh never reached its gated queue fetch")
	}

	bStarted := make(chan struct{})
	bDone := make(chan error, 1)
	bFinished := make(chan struct{})
	go func() {
		close(bStarted)
		err := client.Refresh(context.Background())
		bDone <- err
		close(bFinished)
	}()
	<-bStarted

	// The gated first refresh holds the cycle lock, so on the fixed code the
	// second cannot complete while the gate is closed. The bound is a failure
	// guard; closing the gate below is what releases the first refresh.
	select {
	case <-bFinished:
		t.Log("second refresh completed while the first was held in its queue fetch")
	case <-time.After(1 * time.Second):
	}
	close(server.queueGate)

	if err := <-aDone; err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if err := <-bDone; err != nil {
		t.Fatalf("second refresh: %v", err)
	}

	eventMu.Lock()
	gotEvents := append([]string(nil), events...)
	eventMu.Unlock()
	wantEvents := []string{"build", "persist", "build", "persist"}
	if len(gotEvents) != len(wantEvents) {
		t.Fatalf("refresh cycle events = %v, want %v", gotEvents, wantEvents)
	}
	for i := range wantEvents {
		if gotEvents[i] != wantEvents[i] {
			t.Fatalf("refresh cycle events = %v, want %v (concurrent cycles interleaved)", gotEvents, wantEvents)
		}
	}

	client.mu.Lock()
	state := client.state
	client.mu.Unlock()
	onDisk := readAltmountStateFile(t, path)
	for _, tc := range []struct {
		name     string
		snapshot altmountStateSnapshot
	}{
		{"in-memory", state},
		{"on-disk", onDisk},
	} {
		for _, release := range []string{alpha, beta} {
			if _, ok := tc.snapshot.Completed[ReleaseKey(release)]; !ok {
				t.Fatalf("%s snapshot lost %q after serialized queue enrichment: completed=%v", tc.name, release, keysOf(tc.snapshot.Completed))
			}
		}
	}
}

func unixAgoString(d time.Duration) string {
	return strconv.FormatInt(time.Now().Add(-d).Unix(), 10)
}

func readAltmountStateFile(t *testing.T, path string) altmountStateSnapshot {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var snapshot altmountStateSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatalf("decode state file: %v", err)
	}
	return snapshot
}

func keysOf(records map[string]altmountReleaseRecord) []string {
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	return keys
}
