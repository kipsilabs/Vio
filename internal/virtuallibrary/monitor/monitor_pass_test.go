package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// trackingRegistrar records reconciliation calls and their evidence, and can
// inject a failure.
type trackingRegistrar struct {
	reconcileCalls    []string
	reconcileEvidence []ReconcileEvidence
	reconcileErr      error
}

func (r *trackingRegistrar) Register(context.Context, MonitoredMedia) error { return nil }

func (r *trackingRegistrar) Reconcile(_ context.Context, source string, _ []string, _ []int, evidence ReconcileEvidence) error {
	r.reconcileCalls = append(r.reconcileCalls, source)
	r.reconcileEvidence = append(r.reconcileEvidence, evidence)
	return r.reconcileErr
}

func newPassTestMonitor(t *testing.T) *mediaMonitor {
	t.Helper()
	m := newMediaMonitor(nil, slog.New(slog.DiscardHandler))
	dir := t.TempDir()
	m.config.File = filepath.Join(dir, "queue.json")
	m.config.ProwlarrIndexFile = filepath.Join(dir, "index.json")
	return m
}

func movieQueueItem(key string) monitoredMedia {
	return monitoredMedia{
		Key:       key,
		MediaType: "movie",
		SourceKey: "request:" + key,
		Title:     key,
	}
}

func identifiedQueueItem(key, tmdbID string) monitoredMedia {
	return monitoredMedia{
		Key:       key,
		MediaType: "movie",
		SourceKey: "request:" + key,
		Title:     key,
		TMDBID:    tmdbID,
	}
}

// fakePresence reports catalog content the test declares removed.
type fakePresence struct {
	missing map[string]struct{}
	err     error
}

func (f *fakePresence) MissingVirtualMedia(context.Context, []string) (map[string]struct{}, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.missing, nil
}

func readMonitorCursor(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}
	var state monitorState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode queue file: %v", err)
	}
	return state.Cursor
}

// TestRunPassResumesAfterBudgetExhaustion proves that a pass cut off by its
// deadline reports success, persists the position of the last completed item,
// and that the next pass continues from there instead of restarting.
func TestRunPassResumesAfterBudgetExhaustion(t *testing.T) {
	m := newPassTestMonitor(t)
	// Make the per-item bound irrelevant: the test exercises the pass deadline.
	m.itemTimeout = time.Hour

	items := []monitoredMedia{
		movieQueueItem("a"),
		movieQueueItem("b"),
		movieQueueItem("c"),
		movieQueueItem("d"),
		movieQueueItem("e"),
	}

	blocked := true
	var attemptedFirst, attemptedSecond []string
	m.evaluateFn = func(ctx context.Context, item monitoredMedia) (monitoredMedia, string, error) {
		if item.Key == "c" && blocked {
			<-ctx.Done()
			return item, "", ctx.Err()
		}
		return item, "", nil
	}

	// Wrap to record attempts per pass.
	inner := m.evaluateFn
	m.evaluateFn = func(ctx context.Context, item monitoredMedia) (monitoredMedia, string, error) {
		attemptedFirst = append(attemptedFirst, item.Key)
		return inner(ctx, item)
	}

	firstRegistrar := &trackingRegistrar{}
	passCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	resp, err := m.runPass(passCtx, items, firstRegistrar)
	cancel()
	if err != nil {
		t.Fatalf("budget-exhausted pass returned error: %v", err)
	}
	if len(firstRegistrar.reconcileCalls) != 0 {
		t.Fatalf("budget-exhausted pass reconciled %v; a partial keep set must not sweep", firstRegistrar.reconcileCalls)
	}
	if got := resp.Output["budget_exhausted"]; got != true {
		t.Fatalf("budget_exhausted = %v, want true", got)
	}
	if got := resp.Output["processed"]; got != 2 {
		t.Fatalf("processed = %v, want 2 (a,b before the deadline)", got)
	}
	if cursor := m.currentCursor(); cursor != "b" {
		t.Fatalf("cursor = %q, want %q", cursor, "b")
	}
	if cursor := readMonitorCursor(t, m.config.File); cursor != "b" {
		t.Fatalf("persisted cursor = %q, want %q", cursor, "b")
	}
	for _, key := range attemptedFirst {
		if key == "d" || key == "e" {
			t.Fatalf("item %q attempted in the budget-exhausted pass", key)
		}
	}

	// Second pass: the poison condition is gone, so the pass completes.
	blocked = false
	m.evaluateFn = func(ctx context.Context, item monitoredMedia) (monitoredMedia, string, error) {
		attemptedSecond = append(attemptedSecond, item.Key)
		return inner(ctx, item)
	}
	resp, err = m.runPass(context.Background(), items, &trackingRegistrar{})
	if err != nil {
		t.Fatalf("resumed pass returned error: %v", err)
	}
	if got := resp.Output["budget_exhausted"]; got != false {
		t.Fatalf("resumed budget_exhausted = %v, want false", got)
	}
	if got := resp.Output["processed"]; got != 5 {
		t.Fatalf("resumed processed = %v, want 5", got)
	}
	if cursor := m.currentCursor(); cursor != "" {
		t.Fatalf("cursor after complete pass = %q, want empty", cursor)
	}
	// The resume must start at c and reach the items the first pass never got
	// to, then wrap for a full cycle.
	joined := ""
	for _, key := range attemptedSecond {
		joined += key
	}
	if joined != "cdeab" {
		t.Fatalf("second pass order = %q, want %q", joined, "cdeab")
	}
}

// TestRunPassDefersPoisonItem proves that a single item whose per-item bound
// expires is skipped rather than aborting the pass, and that items behind it
// still run.
func TestRunPassDefersPoisonItem(t *testing.T) {
	m := newPassTestMonitor(t)
	m.itemTimeout = 20 * time.Millisecond

	items := []monitoredMedia{
		movieQueueItem("a"),
		movieQueueItem("b"),
		movieQueueItem("c"),
	}
	m.evaluateFn = func(ctx context.Context, item monitoredMedia) (monitoredMedia, string, error) {
		if item.Key == "b" {
			<-ctx.Done()
			return item, "", ctx.Err()
		}
		return item, "", nil
	}

	resp, err := m.runPass(context.Background(), items, &trackingRegistrar{})
	if err != nil {
		t.Fatalf("pass with deferred item returned error: %v", err)
	}
	if got := resp.Output["budget_exhausted"]; got != false {
		t.Fatalf("budget_exhausted = %v, want false", got)
	}
	if got := resp.Output["processed"]; got != 3 {
		t.Fatalf("processed = %v, want 3 (c runs after b's timeout)", got)
	}
	if got := resp.Output["deferred"]; got != 1 {
		t.Fatalf("deferred = %v, want 1", got)
	}
	if got := resp.Output["pending"]; got != 3 {
		t.Fatalf("pending = %v, want 3", got)
	}
}

// TestRunPassPersistsSameSourceProgressOnDeferredItem proves a frozen-source
// traversal that times out keeps the episodes and season cursor it accumulated,
// so the retry resumes instead of restarting from the first season.
func TestRunPassPersistsSameSourceProgressOnDeferredItem(t *testing.T) {
	m := newPassTestMonitor(t)
	item := monitoredMedia{
		Key: "series:tvdb:1", MediaType: "series", TVDBID: "1", MetadataSource: "tvdb",
	}
	m.evaluateFn = func(_ context.Context, item monitoredMedia) (monitoredMedia, string, error) {
		item.Episodes = []virtualEpisode{{Season: 1, Episode: 1, Title: "Pilot"}}
		item.MetadataNextSeason = 1
		return item, "Selected metadata source unavailable; monitoring will retry", context.DeadlineExceeded
	}

	resp, err := m.runPass(context.Background(), []monitoredMedia{item}, &trackingRegistrar{})
	if err != nil {
		t.Fatalf("pass with deferred item returned error: %v", err)
	}
	if got := resp.Output["deferred"]; got != 1 {
		t.Fatalf("deferred = %v, want 1", got)
	}
	persisted, ok := m.item(item.Key)
	if !ok {
		t.Fatal("deferred item was not persisted")
	}
	if len(persisted.Episodes) != 1 || persisted.Episodes[0].Title != "Pilot" {
		t.Fatalf("episodes = %+v, want the same-source partial progress", persisted.Episodes)
	}
	if persisted.MetadataNextSeason != 1 {
		t.Fatalf("metadata next season = %d, want the preserved cursor 1", persisted.MetadataNextSeason)
	}
}

// TestRunPassReconcileFailureDoesNotFailPass proves a failing source
// reconciliation is logged and retried later, not returned as a pass error.
func TestRunPassReconcileFailureDoesNotFailPass(t *testing.T) {
	m := newPassTestMonitor(t)
	items := []monitoredMedia{movieQueueItem("a"), movieQueueItem("b")}
	m.evaluateFn = func(_ context.Context, item monitoredMedia) (monitoredMedia, string, error) {
		return item, "", nil
	}
	registrar := &trackingRegistrar{reconcileErr: errors.New("sweep failed")}

	resp, err := m.runPass(context.Background(), items, registrar)
	if err != nil {
		t.Fatalf("runPass returned error for a per-source reconcile failure: %v", err)
	}
	if got := resp.Output["processed"]; got != 2 {
		t.Fatalf("processed = %v, want 2", got)
	}
	if len(registrar.reconcileCalls) != 2 {
		t.Fatalf("reconcile calls = %v, want both sources", registrar.reconcileCalls)
	}
}

// TestRunPassResumedCompletionBarsReconciliation proves that a pass which
// resumes from the cursor and reaches the end does not reconcile: it observed
// only the suffix, so its keep set is a partial enumeration and cannot
// authorize a delete.
func TestRunPassResumedCompletionBarsReconciliation(t *testing.T) {
	m := newPassTestMonitor(t)
	m.itemTimeout = time.Hour

	items := []monitoredMedia{
		movieQueueItem("a"),
		movieQueueItem("b"),
		movieQueueItem("c"),
	}
	blocked := true
	m.evaluateFn = func(ctx context.Context, item monitoredMedia) (monitoredMedia, string, error) {
		if item.Key == "c" && blocked {
			<-ctx.Done()
			return item, "", ctx.Err()
		}
		return item, "", nil
	}

	firstRegistrar := &trackingRegistrar{}
	passCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	_, err := m.runPass(passCtx, items, firstRegistrar)
	cancel()
	if err != nil {
		t.Fatalf("first pass returned error: %v", err)
	}
	if len(firstRegistrar.reconcileCalls) != 0 {
		t.Fatalf("budget-exhausted pass reconciled %v", firstRegistrar.reconcileCalls)
	}
	if cursor := m.currentCursor(); cursor != "b" {
		t.Fatalf("cursor = %q, want %q", cursor, "b")
	}

	// The resumed pass processes c, wraps through a and b, and clears the
	// cursor. It must still refuse to reconcile because it started mid-queue.
	blocked = false
	resumedRegistrar := &trackingRegistrar{}
	if _, err := m.runPass(context.Background(), items, resumedRegistrar); err != nil {
		t.Fatalf("resumed pass returned error: %v", err)
	}
	if len(resumedRegistrar.reconcileCalls) != 0 {
		t.Fatalf("resumed pass reconciled %v; a partial enumeration must not sweep", resumedRegistrar.reconcileCalls)
	}
	if cursor := m.currentCursor(); cursor != "" {
		t.Fatalf("cursor after resumed completion = %q, want empty", cursor)
	}
}

// TestRunPassFullCycleCarriesCompleteEvidence proves a front-to-back pass hands
// the reconciler positive completeness evidence for every source.
func TestRunPassFullCycleCarriesCompleteEvidence(t *testing.T) {
	m := newPassTestMonitor(t)
	items := []monitoredMedia{movieQueueItem("a"), movieQueueItem("b")}
	m.evaluateFn = func(_ context.Context, item monitoredMedia) (monitoredMedia, string, error) {
		return item, "", nil
	}

	registrar := &trackingRegistrar{}
	if _, err := m.runPass(context.Background(), items, registrar); err != nil {
		t.Fatalf("pass returned error: %v", err)
	}
	if len(registrar.reconcileEvidence) != 2 {
		t.Fatalf("evidence count = %d, want 2 (one per source)", len(registrar.reconcileEvidence))
	}
	for i, evidence := range registrar.reconcileEvidence {
		if !evidence.FullCycle {
			t.Fatalf("evidence[%d].FullCycle = false, want true", i)
		}
		if evidence.SourceCount != 1 || evidence.QueueCount != 2 {
			t.Fatalf("evidence[%d] = %+v, want source 1 / queue 2", i, evidence)
		}
	}
}

// TestRunPassReconcileRefusalLoggedLoud proves a catalog refusal does not fail
// the pass and is logged at Error with the source, and that the queue bound is
// reported.
func TestRunPassReconcileRefusalLoggedLoud(t *testing.T) {
	m := newPassTestMonitor(t)
	var logs bytes.Buffer
	m.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	items := []monitoredMedia{movieQueueItem("a")}
	m.evaluateFn = func(_ context.Context, item monitoredMedia) (monitoredMedia, string, error) {
		return item, "", nil
	}
	registrar := &trackingRegistrar{reconcileErr: fmt.Errorf("%w: truncated keep set", ErrReconcileRefused)}

	resp, err := m.runPass(context.Background(), items, registrar)
	if err != nil {
		t.Fatalf("refusal must not fail the pass: %v", err)
	}
	if len(registrar.reconcileCalls) != 1 {
		t.Fatalf("reconcile calls = %v, want 1", registrar.reconcileCalls)
	}
	logged := logs.String()
	if !strings.Contains(logged, "level=ERROR") || !strings.Contains(logged, "refused") || !strings.Contains(logged, "request:a") {
		t.Fatalf("refusal was not logged loudly with the source:\n%s", logged)
	}
	if got := resp.Output["queue_bound"]; got != maxMonitoredItems {
		t.Fatalf("queue_bound = %v, want %d", got, maxMonitoredItems)
	}
}

// TestPruneCompletedMoviesEvictsOnlyCompleteSources proves the queue bound is
// enforced by evicting finished movies while leaving deferred/failing and
// series items for the next pass.
func TestPruneCompletedMoviesEvictsOnlyCompleteSources(t *testing.T) {
	m := newPassTestMonitor(t)

	completed := movieQueueItem("completed")
	completed.Ready = true
	unregistered := movieQueueItem("unregistered")
	unregistered.Ready = true
	pending := movieQueueItem("pending")
	series := movieQueueItem("series")
	series.MediaType = "series"
	series.Ready = true

	for _, item := range []monitoredMedia{completed, unregistered, pending, series} {
		if err := m.remember(item); err != nil {
			t.Fatalf("remember %s: %v", item.Key, err)
		}
	}
	m.markRegistered("completed")
	m.markRegistered("pending")
	m.markRegistered("series")

	pruned, err := m.pruneCompletedMovies()
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1 (only the completed movie)", pruned)
	}
	if _, ok := m.item("completed"); ok {
		t.Fatal("completed movie survived pruning")
	}
	for _, key := range []string{"unregistered", "pending", "series"} {
		if _, ok := m.item(key); !ok {
			t.Fatalf("pruning dropped %q", key)
		}
	}

	// The eviction is durable: a reload must not resurrect the item.
	reloaded := newMediaMonitor(nil, nil)
	if err := reloaded.Configure(Config{File: m.config.File, ProwlarrIndexFile: m.config.ProwlarrIndexFile}); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := reloaded.item("completed"); ok {
		t.Fatal("pruned item returned after reload")
	}
}

// TestEvictMissingPrunesOnlyAbsentRegisteredItems proves the existence probe
// evicts registered media that is genuinely gone, never an unregistered
// request, and prunes nothing when the probe fails.
func TestEvictMissingPrunesOnlyAbsentRegisteredItems(t *testing.T) {
	m := newPassTestMonitor(t)
	keep := identifiedQueueItem("keep", "10")
	gone := identifiedQueueItem("gone", "20")
	pending := identifiedQueueItem("pending", "30")
	items := []monitoredMedia{keep, gone, pending}
	for _, item := range items {
		if err := m.remember(item); err != nil {
			t.Fatalf("remember %s: %v", item.Key, err)
		}
	}
	m.markRegistered("keep")
	m.markRegistered("gone")

	presence := &fakePresence{missing: map[string]struct{}{"movie-tmdb-20": {}}}
	evicted, err := m.evictMissing(context.Background(), presence, items)
	if err != nil {
		t.Fatalf("evictMissing: %v", err)
	}
	if evicted != 1 {
		t.Fatalf("evicted = %d, want 1", evicted)
	}
	if _, ok := m.item("gone"); ok {
		t.Fatal("genuinely-missing registered item survived")
	}
	for _, key := range []string{"keep", "pending"} {
		if _, ok := m.item(key); !ok {
			t.Fatalf("evictMissing dropped %q", key)
		}
	}

	// A probe failure must not prune anything.
	m2 := newPassTestMonitor(t)
	failing := identifiedQueueItem("fail", "40")
	if err := m2.remember(failing); err != nil {
		t.Fatalf("remember: %v", err)
	}
	m2.markRegistered("fail")
	if _, err := m2.evictMissing(context.Background(), &fakePresence{err: errors.New("database unavailable")}, []monitoredMedia{failing}); err == nil {
		t.Fatal("expected probe failure")
	}
	if _, ok := m2.item("fail"); !ok {
		t.Fatal("probe failure pruned an item")
	}

	// A registered item whose content was never probed as missing stays.
	m3 := newPassTestMonitor(t)
	untouched := identifiedQueueItem("untouched", "50")
	if err := m3.remember(untouched); err != nil {
		t.Fatalf("remember: %v", err)
	}
	m3.markRegistered(untouched.Key)
	if evicted, err := m3.evictMissing(context.Background(), &fakePresence{}, []monitoredMedia{untouched}); err != nil || evicted != 0 {
		t.Fatalf("empty missing set: evicted=%d err=%v, want 0/nil", evicted, err)
	}
	if _, ok := m3.item(untouched.Key); !ok {
		t.Fatal("item was pruned with no missing evidence")
	}
}

// TestForgetClearsRegistrationMarker proves forgetting an item also drops its
// registration marker so a re-request re-registers instead of being skipped.
func TestForgetClearsRegistrationMarker(t *testing.T) {
	m := newPassTestMonitor(t)
	item := movieQueueItem("done")
	if err := m.remember(item); err != nil {
		t.Fatalf("remember: %v", err)
	}
	m.markRegistered(item.Key)
	if !m.isRegistered(item.Key) {
		t.Fatal("registration marker not set")
	}
	if err := m.forget(item.Key); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if _, ok := m.item(item.Key); ok {
		t.Fatal("forgotten item still queued")
	}
	if m.isRegistered(item.Key) {
		t.Fatal("forget left a stale registration marker")
	}
}

// TestMonitorConfigRoundTripsCursorAndLegacyArray verifies the persisted cursor
// survives a reload and that a queue file written by an older release (a bare
// JSON array) still loads, with no resume position.
func TestMonitorConfigRoundTripsCursorAndLegacyArray(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queue.json")

	m := newPassTestMonitor(t)
	m.config.File = path
	if err := m.remember(movieQueueItem("a")); err != nil {
		t.Fatalf("remember a: %v", err)
	}
	if err := m.remember(movieQueueItem("b")); err != nil {
		t.Fatalf("remember b: %v", err)
	}
	if err := m.setCursor("b"); err != nil {
		t.Fatalf("setCursor: %v", err)
	}

	reloaded := newMediaMonitor(nil, nil)
	if err := reloaded.Configure(Config{File: path, ProwlarrIndexFile: filepath.Join(dir, "index.json")}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if got := reloaded.currentCursor(); got != "b" {
		t.Fatalf("reloaded cursor = %q, want %q", got, "b")
	}
	if _, ok := reloaded.item("a"); !ok {
		t.Fatal("reloaded queue lost item a")
	}
	if _, ok := reloaded.item("b"); !ok {
		t.Fatal("reloaded queue lost item b")
	}

	// Legacy shape: a bare array has no cursor and must still load.
	legacy := filepath.Join(dir, "legacy.json")
	legacyItems := []monitoredMedia{movieQueueItem("old")}
	data, err := json.Marshal(legacyItems)
	if err != nil {
		t.Fatalf("marshal legacy queue: %v", err)
	}
	if err := os.WriteFile(legacy, data, 0o600); err != nil {
		t.Fatalf("write legacy queue: %v", err)
	}
	fromLegacy := newMediaMonitor(nil, nil)
	if err := fromLegacy.Configure(Config{File: legacy, ProwlarrIndexFile: filepath.Join(dir, "index.json")}); err != nil {
		t.Fatalf("Configure legacy: %v", err)
	}
	if got := fromLegacy.currentCursor(); got != "" {
		t.Fatalf("legacy cursor = %q, want empty", got)
	}
	if _, ok := fromLegacy.item("old"); !ok {
		t.Fatal("legacy queue lost item old")
	}
}
