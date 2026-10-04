package altmount

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

// altmountQueuePayload builds a SABnzbd-compatible queue JSON body.
func altmountQueuePayload(slots string) string {
	return `{"queue":{"slots":[` + slots + `]}}`
}

// TestDownloadingFrozenProgressExpires proves a slot the queue keeps reporting
// with unchanged progress is retired by the no-progress bound, while a slot
// whose progress advances is not. Without this, refresh bumping the record
// timestamp on every observation pins a frozen slot forever.
func TestDownloadingFrozenProgressExpires(t *testing.T) {
	now := time.Now()
	frozen := altmountReleaseRecord{
		CompletedAt:    now.Unix(),
		FirstSeenAt:    now.Add(-time.Hour).Unix(),
		LastProgressAt: now.Add(-downloadingNoProgressRetention - time.Minute).Unix(),
		SizeLeft:       100,
	}
	if !altmountDownloadingExpired(frozen, now) {
		t.Fatal("a slot with frozen progress past the no-progress bound must be expired")
	}

	progressing := frozen
	progressing.FirstSeenAt = now.Add(-time.Minute).Unix()
	progressing.LastProgressAt = now.Unix()
	if altmountDownloadingExpired(progressing, now) {
		t.Fatal("a slot whose progress advanced must stay pending")
	}

	// A slot that reports no progress fields at all cannot demonstrate
	// progress, so it is bounded by the absolute lifetime, not the frozen bound.
	opaque := altmountReleaseRecord{
		CompletedAt: now.Unix(),
		FirstSeenAt: now.Add(-time.Minute).Unix(),
		SizeLeft:    0,
		ETASeconds:  0,
	}
	if altmountDownloadingExpired(opaque, now) {
		t.Fatal("a young slot with no progress fields must stay pending")
	}
	opaque.FirstSeenAt = now.Add(-downloadingRetention - time.Minute).Unix()
	if !altmountDownloadingExpired(opaque, now) {
		t.Fatal("a slot past its absolute lifetime must expire even with no progress fields")
	}
}

// TestReleaseDownloadingEnforcesExpiryAtReadTime proves the read path expires a
// stale record even though no refresh succeeded to prune it: the caller learns
// the release is not downloading (and known), so a stuck slot releases the hold.
func TestReleaseDownloadingEnforcesExpiryAtReadTime(t *testing.T) {
	client := New(nil)
	client.Configure("https://altmount.example", "", 15)
	key := ReleaseKey("Stuck.Release.2024")
	now := time.Now()
	client.state = altmountStateSnapshot{
		Completed: map[string]altmountReleaseRecord{},
		Failed:    map[string]altmountReleaseRecord{},
		Downloading: map[string]altmountReleaseRecord{
			key: {
				CompletedAt:    now.Add(-downloadingRetention - time.Minute).Unix(),
				FirstSeenAt:    now.Add(-2 * downloadingRetention).Unix(),
				LastProgressAt: now.Add(-downloadingRetention - time.Minute).Unix(),
				SizeLeft:       10,
			},
		},
	}
	if downloading, known := client.ReleaseDownloading("Stuck.Release.2024"); !known || downloading {
		t.Fatalf("ReleaseDownloading(stale) = (%v, %v), want (false, true)", downloading, known)
	}
}

// TestDisappearingSlotExpires proves a slot that stops appearing in the queue
// goes stale downloadingRetention after its last observation, both at read time
// and when pruned.
func TestDisappearingSlotExpires(t *testing.T) {
	now := time.Now()
	gone := altmountReleaseRecord{
		CompletedAt: now.Add(-downloadingRetention - time.Second).Unix(),
		FirstSeenAt: now.Add(-2 * downloadingRetention).Unix(),
	}
	if !altmountDownloadingExpired(gone, now) {
		t.Fatal("a slot whose last observation is past retention must be expired")
	}
	pruned := pruneAltmountSnapshot(altmountStateSnapshot{
		Completed:   map[string]altmountReleaseRecord{},
		Failed:      map[string]altmountReleaseRecord{},
		Downloading: map[string]altmountReleaseRecord{"gone": gone},
	}, now)
	if len(pruned.Downloading) != 0 {
		t.Fatalf("pruned still holds the disappeared slot: %+v", pruned.Downloading)
	}
}

// TestRefreshOutageDoesNotPinStuckSlot proves that when a refresh fails, an
// already-stale downloading record is not re-stamped by the merge and is read
// as not downloading. This is the failure mode that let a permanently
// Downloading slot outlive the retention.
func TestRefreshOutageDoesNotPinStuckSlot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "outage", http.StatusBadGateway)
	}))
	defer srv.Close()

	client := New(nil)
	client.Configure(srv.URL, "", 15)
	key := ReleaseKey("Stuck.Release.2024")
	client.mu.Lock()
	client.state = altmountStateSnapshot{
		Completed: map[string]altmountReleaseRecord{},
		Failed:    map[string]altmountReleaseRecord{},
		Downloading: map[string]altmountReleaseRecord{
			key: {CompletedAt: time.Now().Add(-downloadingRetention - time.Minute).Unix(), FirstSeenAt: time.Now().Add(-2 * downloadingRetention).Unix()},
		},
	}
	client.mu.Unlock()

	// Refresh fails (history 502) and must leave the stale record untouched.
	if err := client.Refresh(context.Background()); err == nil {
		t.Fatal("expected the refresh to fail")
	}
	if downloading, known := client.ReleaseDownloading("Stuck.Release.2024"); !known || downloading {
		t.Fatalf("ReleaseDownloading after failed refresh = (%v, %v), want (false, true)", downloading, known)
	}
}

// TestDownloadingFirstSeenAndProgressSurviveRestart proves the new lifetime
// fields persist to disk and reload, and that reloading prunes a record whose
// lifetime already elapsed.
func TestDownloadingFirstSeenAndProgressSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "altmount-state.json")
	now := time.Now()
	key := releaseNameKey("Fresh.Release.2024")
	expiredKey := releaseNameKey("Expired.Release.2024")
	snapshot := altmountStateSnapshot{
		Completed: map[string]altmountReleaseRecord{},
		Failed:    map[string]altmountReleaseRecord{},
		Downloading: map[string]altmountReleaseRecord{
			key: {
				CompletedAt:    now.Unix(),
				FirstSeenAt:    now.Add(-time.Minute).Unix(),
				LastProgressAt: now.Add(-30 * time.Second).Unix(),
				SizeLeft:       42,
				ETASeconds:     60,
			},
			expiredKey: {
				CompletedAt: now.Add(-downloadingRetention - time.Minute).Unix(),
				FirstSeenAt: now.Add(-2 * downloadingRetention).Unix(),
			},
		},
	}
	if err := saveAltmountState(path, snapshot); err != nil {
		t.Fatalf("saveAltmountState: %v", err)
	}

	client := New(nil)
	client.Configure("https://altmount.example", "", 15)
	if err := client.ConfigureIndexFile(path); err != nil {
		t.Fatalf("ConfigureIndexFile: %v", err)
	}
	client.mu.Lock()
	_, freshOK := client.state.Downloading[key]
	_, expiredOK := client.state.Downloading[expiredKey]
	client.mu.Unlock()
	if !freshOK {
		t.Fatal("fresh downloading record did not survive the reload")
	}
	if expiredOK {
		t.Fatal("expired downloading record should be pruned on reload")
	}
	if downloading, _ := client.ReleaseDownloading("Fresh.Release.2024"); !downloading {
		t.Fatal("reloaded fresh record must still read as downloading")
	}
	client.mu.Lock()
	record := client.state.Downloading[key]
	client.mu.Unlock()
	if record.FirstSeenAt != snapshot.Downloading[key].FirstSeenAt {
		t.Fatalf("FirstSeenAt = %d, want %d", record.FirstSeenAt, snapshot.Downloading[key].FirstSeenAt)
	}
	if record.LastProgressAt != snapshot.Downloading[key].LastProgressAt {
		t.Fatalf("LastProgressAt = %d, want %d", record.LastProgressAt, snapshot.Downloading[key].LastProgressAt)
	}
}

// mutableAltmountServer answers history and queue mode requests from a
// swappable pair of bodies.
type mutableAltmountServer struct {
	mu       sync.Mutex
	history  string
	queue    string
	queueHit func()
}

func (m *mutableAltmountServer) set(history, queue string) {
	m.mu.Lock()
	m.history = history
	m.queue = queue
	m.mu.Unlock()
}

func (m *mutableAltmountServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	history, queue := m.history, m.queue
	m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Query().Get("mode") {
	case "history":
		_, _ = fmt.Fprint(w, history)
	case "queue":
		if m.queueHit != nil {
			m.queueHit()
		}
		_, _ = fmt.Fprint(w, queue)
	default:
		http.Error(w, "unknown mode", http.StatusBadRequest)
	}
}

// TestCompletedTransitionClearsDownloading proves a release that flips from the
// queue to history Completed stops being pending, is retained as completed, and
// notifies the confirmation observer once.
func TestCompletedTransitionClearsDownloading(t *testing.T) {
	release := "Movie.2024.1080p.WEB-DL.x264-GRP"
	server := &mutableAltmountServer{}
	server.set(
		historyPayload(""),
		altmountQueuePayload(`{"filename": "`+release+`.nzb", "status": "Downloading", "mbleft": "100", "timeleft": "0:05:00"}`),
	)
	srv := httptest.NewServer(server)
	defer srv.Close()

	client := New(nil)
	client.Configure(srv.URL, "", 15)
	observer := &recordingObserver{}
	client.SetConfirmObserver(observer.observe)

	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if downloading, _ := client.ReleaseDownloading(release); !downloading {
		t.Fatal("release should be pending after the first refresh")
	}

	// The next refresh reports the release completed and the queue empty.
	server.set(
		historyPayload(`{"name":"`+release+`","nzb_name":"`+release+`.nzb","status":"Completed","storage":"/downloads/`+release+`","bytes":1000,"completetime":`+fmt.Sprint(time.Now().Unix())+`}`),
		altmountQueuePayload(""),
	)
	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if downloading, _ := client.ReleaseDownloading(release); downloading {
		t.Fatal("completed release must no longer read as downloading")
	}
	if completed, known := client.ReleaseCompleted(release); !known || !completed {
		t.Fatalf("ReleaseCompleted = (%v, %v), want (true, true)", completed, known)
	}
	if keys := observer.snapshot(); !containsKey(keys, ReleaseKey(release)) {
		t.Fatalf("observer notifications = %v, want the completed release key %q", keys, ReleaseKey(release))
	}
}

// TestHistoryPublishedBeforeQueueFetch proves Blocker 4's ordering: the merged
// history snapshot is committed before the queue endpoint is even called, so a
// slow queue can never delay completion/failure visibility.
func TestHistoryPublishedBeforeQueueFetch(t *testing.T) {
	release := "Movie.2024.1080p.WEB-DL.x264-GRP"
	queueHit := make(chan struct{})
	var once sync.Once
	server := &mutableAltmountServer{
		queueHit: func() { once.Do(func() { close(queueHit) }) },
	}
	server.set(
		historyPayload(`{"name":"`+release+`","nzb_name":"`+release+`.nzb","status":"Completed","storage":"/downloads/`+release+`","bytes":1000,"completetime":`+fmt.Sprint(time.Now().Unix())+`}`),
		altmountQueuePayload(""),
	)
	// The queue handler blocks until the request context is canceled, so
	// Refresh cannot finish its queue step within the assertion window.
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("mode") == "queue" {
			server.queueHit()
			<-r.Context().Done()
			return
		}
		server.ServeHTTP(w, r)
	}))
	defer blocking.Close()

	client := New(nil)
	client.Configure(blocking.URL, "", 15)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Refresh(ctx) }()

	select {
	case <-queueHit:
	case <-time.After(5 * time.Second):
		t.Fatal("queue request never started")
	}
	// The history state is already published while the queue is still hung.
	if completed, known := client.ReleaseCompleted(release); !known || !completed {
		t.Fatalf("history not published before the queue fetch: ReleaseCompleted = (%v, %v)", completed, known)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("expected the canceled refresh to return an error")
	}
}

// TestObserveAltmountQueuePreservesProgress proves the fold keeps the original
// first-seen and last-progress timestamps across observations, and advances
// last-progress only when the reported progress changes.
func TestObserveAltmountQueuePreservesProgress(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	key := releaseNameKey("Movie.2024.1080p")
	first := observeAltmountQueue(nil, map[string]altmountReleaseRecord{
		key: {CompletedAt: t0.Unix(), SizeLeft: 500, ETASeconds: 100},
	}, t0)
	rec := first[key]
	if rec.FirstSeenAt != t0.Unix() || rec.LastProgressAt != t0.Unix() {
		t.Fatalf("first observation = %+v, want first-seen/progress at t0", rec)
	}

	t1 := t0.Add(5 * time.Minute)
	frozen := observeAltmountQueue(first, map[string]altmountReleaseRecord{
		key: {CompletedAt: t1.Unix(), SizeLeft: 500, ETASeconds: 100},
	}, t1)
	if frozen[key].FirstSeenAt != t0.Unix() {
		t.Fatalf("first-seen moved on a frozen observation: %d", frozen[key].FirstSeenAt)
	}
	if frozen[key].LastProgressAt != t0.Unix() {
		t.Fatalf("last-progress advanced without progress: %d", frozen[key].LastProgressAt)
	}

	advanced := observeAltmountQueue(frozen, map[string]altmountReleaseRecord{
		key: {CompletedAt: t1.Unix(), SizeLeft: 400, ETASeconds: 80},
	}, t1)
	if advanced[key].LastProgressAt != t1.Unix() {
		t.Fatalf("last-progress did not advance on real progress: %d", advanced[key].LastProgressAt)
	}
	if advanced[key].FirstSeenAt != t0.Unix() {
		t.Fatalf("first-seen moved on a progressing observation: %d", advanced[key].FirstSeenAt)
	}
}

// TestRefreshQueueFailureKeepsHistoryState proves the queue enrichment is
// best-effort: a queue failure does not lose the history verdict fetched in the
// same refresh.
func TestRefreshQueueFailureKeepsHistoryState(t *testing.T) {
	release := "Movie.2024.1080p.WEB-DL.x264-GRP"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("mode") == "queue" {
			http.Error(w, "no queue support", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, historyPayload(`{"name":"`+release+`","nzb_name":"`+release+`.nzb","status":"Completed","storage":"/downloads/`+release+`","bytes":1000,"completetime":`+fmt.Sprint(time.Now().Unix())+`}`))
	}))
	defer srv.Close()

	client := New(nil)
	client.Configure(srv.URL, "", 15)
	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if completed, known := client.ReleaseCompleted(release); !known || !completed {
		t.Fatalf("history verdict lost on queue failure: ReleaseCompleted = (%v, %v)", completed, known)
	}
}

// TestKeepsReportingStuckSlotStillExpires proves a slot the queue reports on
// every refresh is still retired once it exceeds its absolute lifetime. This is
// the exact failure Blocker 1 named: refresh re-stamping the record made a
// permanently Downloading slot immortal. The queue keeps answering here, so the
// only thing that can expire it is the preserved first-seen time.
func TestKeepsReportingStuckSlotStillExpires(t *testing.T) {
	release := "Stuck.Release.2024"
	server := &mutableAltmountServer{}
	server.set(
		historyPayload(""),
		altmountQueuePayload(`{"filename": "`+release+`.nzb", "status": "Downloading", "mbleft": "100", "timeleft": "0:05:00"}`),
	)
	srv := httptest.NewServer(server)
	defer srv.Close()

	client := New(nil)
	client.Configure(srv.URL, "", 15)
	key := ReleaseKey(release)
	client.mu.Lock()
	client.state = altmountStateSnapshot{
		Completed: map[string]altmountReleaseRecord{},
		Failed:    map[string]altmountReleaseRecord{},
		Downloading: map[string]altmountReleaseRecord{
			key: {
				CompletedAt: time.Now().Unix(),
				FirstSeenAt: time.Now().Add(-downloadingRetention - time.Minute).Unix(),
				SizeLeft:    100,
			},
		},
	}
	client.mu.Unlock()

	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if downloading, known := client.ReleaseDownloading(release); !known || downloading {
		t.Fatalf("keeps-being-reported slot past lifetime = (%v, %v), want (false, true)", downloading, known)
	}
}

// TestProgressingSlotSurvivesExpiryCheck proves the counterpart: a slot whose
// reported progress is recent is not expired by the no-progress bound, so the
// bound does not break a genuinely progressing download.
func TestProgressingSlotSurvivesExpiryCheck(t *testing.T) {
	release := "Long.Release.2024"
	server := &mutableAltmountServer{}
	server.set(
		historyPayload(""),
		altmountQueuePayload(`{"filename": "`+release+`.nzb", "status": "Downloading", "mbleft": "100", "timeleft": "0:05:00"}`),
	)
	srv := httptest.NewServer(server)
	defer srv.Close()

	client := New(nil)
	client.Configure(srv.URL, "", 15)
	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	// Past the no-progress bound but within the absolute lifetime, with
	// progress just observed: the observation fold must keep it pending. This
	// is the case a no-progress-only bound would wrongly retire.
	key := ReleaseKey(release)
	client.mu.Lock()
	record := client.state.Downloading[key]
	record.FirstSeenAt = time.Now().Add(-downloadingNoProgressRetention - time.Minute).Unix()
	record.LastProgressAt = time.Now().Unix()
	client.state.Downloading[key] = record
	client.mu.Unlock()

	if downloading, _ := client.ReleaseDownloading(release); !downloading {
		t.Fatal("a slot with recent progress inside its lifetime must stay pending")
	}
}

// TestExpiredDownloadingIsNotFailed proves the liveness invariant the version
// check depends on: retiring an in-flight record only clears its pending flag.
// It never becomes a failed verdict, so an exhausted pending release is
// reported not-pending (and not-failed) — the retryable/alternative treatment —
// rather than being stamped dead.
func TestExpiredDownloadingIsNotFailed(t *testing.T) {
	client := New(nil)
	client.Configure("https://altmount.example", "", 15)
	key := ReleaseKey("Stuck.Release.2024")
	now := time.Now()
	client.state = altmountStateSnapshot{
		Completed: map[string]altmountReleaseRecord{},
		Failed:    map[string]altmountReleaseRecord{},
		Downloading: map[string]altmountReleaseRecord{
			key: {
				CompletedAt:    now.Add(-downloadingRetention - time.Minute).Unix(),
				FirstSeenAt:    now.Add(-2 * downloadingRetention).Unix(),
				LastProgressAt: now.Add(-downloadingRetention - time.Minute).Unix(),
			},
		},
	}

	if downloading, known := client.ReleaseDownloading("Stuck.Release.2024"); !known || downloading {
		t.Fatalf("ReleaseDownloading(expired) = (%v, %v), want (false, true)", downloading, known)
	}
	if failed, known := client.ReleaseFailed("Stuck.Release.2024"); !known || failed {
		t.Fatalf("ReleaseFailed(expired pending) = (%v, %v), want (false, true): expiry must not be a death verdict", failed, known)
	}
	// Classification clears pending without flipping to failed, so the resolver
	// sees an ordinary not-pending candidate and can hold/alternate.
	candidates := []stream.StreamCandidate{{Name: "Stuck.Release.2024"}}
	client.ClassifyCandidates(candidates)
	if candidates[0].SourcePending {
		t.Fatal("expired record must not be classified pending")
	}
	if candidates[0].SourceFailed {
		t.Fatal("expired record must not be classified failed: pending exhaustion is not death")
	}
}

// TestConfigureIndexFileMissingIsNotError pins that a first run with no state
// file is not a failure.
func TestConfigureIndexFileMissingIsNotError(t *testing.T) {
	client := New(nil)
	if err := client.ConfigureIndexFile(filepath.Join(t.TempDir(), "missing.json")); err != nil {
		t.Fatalf("ConfigureIndexFile on a missing file: %v", err)
	}
}

// TestExpiredSlotStaysRetiredAcrossRefreshes proves Blocker 1-revive: once a
// stuck slot crosses its pending window, later successful refreshes that keep
// reporting it must not reset its FirstSeenAt and cycle it back to pending. The
// expired observation is retained (out of the effective pending verdict) so the
// fold has the original timestamps to compare against.
func TestExpiredSlotStaysRetiredAcrossRefreshes(t *testing.T) {
	release := "Stuck.Release.2024"
	server := &mutableAltmountServer{}
	server.set(
		historyPayload(""),
		altmountQueuePayload(`{"filename": "`+release+`.nzb", "status": "Downloading", "mbleft": "100", "timeleft": "0:05:00"}`),
	)
	srv := httptest.NewServer(server)
	defer srv.Close()

	client := New(nil)
	client.Configure(srv.URL, "", 15)
	key := ReleaseKey(release)
	firstSeen := time.Now().Add(-downloadingRetention - time.Minute).Unix()
	client.mu.Lock()
	client.state = altmountStateSnapshot{
		Completed: map[string]altmountReleaseRecord{},
		Failed:    map[string]altmountReleaseRecord{},
		Downloading: map[string]altmountReleaseRecord{
			key: {
				CompletedAt:    time.Now().Unix(),
				FirstSeenAt:    firstSeen,
				LastProgressAt: time.Now().Add(-downloadingNoProgressRetention - time.Minute).Unix(),
				SizeLeft:       100,
				ETASeconds:     300,
			},
		},
	}
	client.mu.Unlock()

	for i := 0; i < 3; i++ {
		if err := client.Refresh(context.Background()); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
		if downloading, known := client.ReleaseDownloading(release); !known || downloading {
			t.Fatalf("refresh %d: slot = (%v, %v), want expired (false, true)", i, downloading, known)
		}
		client.mu.Lock()
		_, pending := client.state.Downloading[key]
		retained, retainedOK := client.state.ExpiredDownloading[key]
		client.mu.Unlock()
		if pending {
			t.Fatalf("refresh %d: expired slot re-entered the pending map", i)
		}
		if !retainedOK {
			t.Fatalf("refresh %d: expired slot identity was dropped, so the next refresh can revive it", i)
		}
		if retained.FirstSeenAt != firstSeen {
			t.Fatalf("refresh %d: FirstSeenAt = %d, want the original %d", i, retained.FirstSeenAt, firstSeen)
		}
	}
}

// TestExpiredSlotRetainedAcrossRestart proves the retained expired observation
// is persisted and reloaded, so a restart followed by a still-present slot
// cannot restart the release's clocks and cycle it back to pending. This is the
// restart half of Blocker 1-revive.
func TestExpiredSlotRetainedAcrossRestart(t *testing.T) {
	release := "Stuck.Release.2024"
	key := ReleaseKey(release)
	dir := t.TempDir()
	path := filepath.Join(dir, "altmount-state.json")
	firstSeen := time.Now().Add(-downloadingRetention - time.Minute).Unix()
	snapshot := altmountStateSnapshot{
		Completed: map[string]altmountReleaseRecord{},
		Failed:    map[string]altmountReleaseRecord{},
		ExpiredDownloading: map[string]altmountReleaseRecord{
			key: {
				CompletedAt: time.Now().Unix(),
				FirstSeenAt: firstSeen,
				SizeLeft:    104857600,
				ETASeconds:  300,
			},
		},
	}
	if err := saveAltmountState(path, snapshot); err != nil {
		t.Fatalf("saveAltmountState: %v", err)
	}

	server := &mutableAltmountServer{}
	server.set(
		historyPayload(""),
		altmountQueuePayload(`{"filename": "`+release+`.nzb", "status": "Downloading", "mbleft": "100", "timeleft": "0:05:00"}`),
	)
	srv := httptest.NewServer(server)
	defer srv.Close()

	client := New(nil)
	client.Configure(srv.URL, "", 15)
	if err := client.ConfigureIndexFile(path); err != nil {
		t.Fatalf("ConfigureIndexFile: %v", err)
	}
	client.mu.Lock()
	_, reloaded := client.state.ExpiredDownloading[key]
	client.mu.Unlock()
	if !reloaded {
		t.Fatal("expired observation did not survive the restart")
	}

	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if downloading, known := client.ReleaseDownloading(release); !known || downloading {
		t.Fatalf("slot after restart = (%v, %v), want expired (false, true)", downloading, known)
	}
	client.mu.Lock()
	retained, retainedOK := client.state.ExpiredDownloading[key]
	_, pending := client.state.Downloading[key]
	client.mu.Unlock()
	if pending {
		t.Fatal("expired slot re-entered the pending map after restart")
	}
	if !retainedOK {
		t.Fatal("expired slot identity was dropped after restart")
	}
	if retained.FirstSeenAt != firstSeen {
		t.Fatalf("FirstSeenAt = %d, want the original %d", retained.FirstSeenAt, firstSeen)
	}
}

// TestExpiredObservationClearsOnDisappearance proves a retained expired
// observation ends when the queue stops reporting the slot: retention is not
// permanent, and a slot that truly leaves clears from both maps.
func TestExpiredObservationClearsOnDisappearance(t *testing.T) {
	release := "Stuck.Release.2024"
	server := &mutableAltmountServer{}
	server.set(historyPayload(""), altmountQueuePayload(""))
	srv := httptest.NewServer(server)
	defer srv.Close()

	client := New(nil)
	client.Configure(srv.URL, "", 15)
	key := ReleaseKey(release)
	client.mu.Lock()
	client.state = altmountStateSnapshot{
		Completed: map[string]altmountReleaseRecord{},
		Failed:    map[string]altmountReleaseRecord{},
		ExpiredDownloading: map[string]altmountReleaseRecord{
			key: {
				CompletedAt: time.Now().Add(-downloadingRetention - time.Minute).Unix(),
				FirstSeenAt: time.Now().Add(-2 * downloadingRetention).Unix(),
			},
		},
	}
	client.mu.Unlock()

	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	client.mu.Lock()
	_, pending := client.state.Downloading[key]
	_, retained := client.state.ExpiredDownloading[key]
	client.mu.Unlock()
	if pending || retained {
		t.Fatalf("disappeared slot still retained: pending=%v expired=%v", pending, retained)
	}
}

// TestExpiredObservationClearsOnTerminalReconcile proves a retained expired
// observation ends when the release reconciles to a terminal verdict, even
// while the queue still reports the slot. The terminal verdict is authoritative
// and the identity is no longer needed.
func TestExpiredObservationClearsOnTerminalReconcile(t *testing.T) {
	release := "Stuck.Release.2024"
	server := &mutableAltmountServer{}
	server.set(
		historyPayload(`{"name":"`+release+`","nzb_name":"`+release+`.nzb","status":"Completed","storage":"/downloads/`+release+`","bytes":1000,"completetime":`+fmt.Sprint(time.Now().Unix())+`}`),
		altmountQueuePayload(`{"filename": "`+release+`.nzb", "status": "Downloading", "mbleft": "100", "timeleft": "0:05:00"}`),
	)
	srv := httptest.NewServer(server)
	defer srv.Close()

	client := New(nil)
	client.Configure(srv.URL, "", 15)
	key := ReleaseKey(release)
	client.mu.Lock()
	client.state = altmountStateSnapshot{
		Completed: map[string]altmountReleaseRecord{},
		Failed:    map[string]altmountReleaseRecord{},
		ExpiredDownloading: map[string]altmountReleaseRecord{
			key: {
				CompletedAt: time.Now().Add(-downloadingRetention - time.Minute).Unix(),
				FirstSeenAt: time.Now().Add(-2 * downloadingRetention).Unix(),
			},
		},
	}
	client.mu.Unlock()

	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if completed, known := client.ReleaseCompleted(release); !known || !completed {
		t.Fatalf("ReleaseCompleted = (%v, %v), want (true, true)", completed, known)
	}
	client.mu.Lock()
	_, pending := client.state.Downloading[key]
	_, retained := client.state.ExpiredDownloading[key]
	client.mu.Unlock()
	if pending || retained {
		t.Fatalf("terminally reconciled slot still retained: pending=%v expired=%v", pending, retained)
	}
}
