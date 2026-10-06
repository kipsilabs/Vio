package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// deferredOverflowProbe builds a deferred probe for the session's bound
// candidate. The caller binds it to the session (capturing the binding
// generation) before admission.
func deferredOverflowProbe(session *playback.Session, candidateURI string) *virtualDeferredProbeV3 {
	return &virtualDeferredProbeV3{
		stickyKey:      "sticky-overflow",
		file:           &models.MediaFile{ID: session.MediaFileID, ContentID: "movie-overflow"},
		streamURL:      "http://provider.example/stream",
		probeTransient: &models.MediaFile{ID: session.MediaFileID},
		cand:           VirtualPlaybackStream{ID: "cand-1", URI: candidateURI},
		ownerID:        5,
		sessionID:      session.ID,
	}
}

// bindOrFail binds a probe to its session and fails the test when the session
// does not own the probe candidate, so a test never runs an unbound probe.
func bindOrFail(t *testing.T, handler *PlaybackHandler, deferred *virtualDeferredProbeV3) {
	t.Helper()
	if !handler.bindDeferredProbeToSession(deferred) {
		t.Fatal("bindDeferredProbeToSession refused a probe for the session's own candidate")
	}
}

// startLiveDeferredProbePool fires the probe-pool once with no workers, so a
// later enqueue observes a queue/retry set that no worker can drain. It leaves
// the separate publish pool untouched, so shed publications still run in the
// background normally.
func startLiveDeferredProbePool(handler *PlaybackHandler) {
	handler.deferredProbeOnce.Do(func() {
		handler.deferredProbeQueue = make(chan *virtualDeferredProbeV3, virtualDeferredProbeQueueSize)
		handler.deferredProbeRetry = make(map[string]*virtualDeferredProbeV3)
		handler.deferredProbeSignal = make(chan struct{}, 1)
	})
}

// fillDeferredProbeBackpressure fills the worker queue and the re-park set to
// their bounds with inert probes, so the next admission hits the shed branch.
func fillDeferredProbeBackpressure(handler *PlaybackHandler) {
	for i := 0; i < virtualDeferredProbeQueueSize; i++ {
		handler.deferredProbeQueue <- &virtualDeferredProbeV3{}
		handler.deferredProbeRetry[deferredRetryTestKey(i)] = &virtualDeferredProbeV3{}
	}
}

func deferredRetryTestKey(i int) string {
	return "overflow-retry-" + strconv.Itoa(i)
}

// syncPlaybackFileResolver is a test file resolver whose row is guarded by a
// mutex. A probe worker's evidence write mutates the row while the inventory
// poll reads it, exactly as the catalog does between two connections; without
// the lock the test's shared pointer is a data race even though production's
// reads and writes go through the database.
type syncPlaybackFileResolver struct {
	mu   sync.Mutex
	file models.MediaFile
}

func (r *syncPlaybackFileResolver) GetByID(context.Context, int) (*models.MediaFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := r.file
	return &cp, nil
}

// update runs fn against the row under the lock, modeling a committed catalog
// write.
func (r *syncPlaybackFileResolver) update(fn func(*models.MediaFile)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.file)
}

// TestDeferredOverflowInitialShedsViaPushAndPoll is the initial-overflow
// regression: when both the deferred-probe queue and its bounded retry set are
// full, the probe is discarded. The discard must not leave the session pending
// forever — the outcome is marked failed immediately, which the inventory poll
// reports, and the terminal failure is delivered to a live push client from the
// bounded background publisher. Admission must stay non-blocking.
func TestDeferredOverflowInitialShedsViaPushAndPoll(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 810, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	candidateURI := "virtual://movie/tt-overflow?result=cand-1"
	if err := manager.SetVirtualSource(session.ID, candidateURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	if err := manager.SetRealtimeConnection(session.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}

	resolver := &syncPlaybackFileResolver{file: models.MediaFile{
		ID: 810, ContentID: "movie-overflow", FilePath: candidateURI, VirtualOwnerInstallationID: 5,
	}}
	handler := NewPlaybackHandler(manager, resolver)
	handler.InstallationID = "test-install"
	handler.RealtimeHub = playback.NewRealtimeHub()
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		return f, nil
	}
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		PlaybackAttemptID: "attempt-overflow-810", SessionID: session.ID,
		UserID: 1, ProfileID: "profile-1", RequestedMediaFileID: 810, EffectiveMediaFileID: 810,
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}
	startLiveDeferredProbePool(handler)
	fillDeferredProbeBackpressure(handler)

	conn := newDeferredInventoryPush()
	registration := handler.RealtimeHub.Register(session.ID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	defer handler.RealtimeHub.Unregister(registration)

	shed := deferredOverflowProbe(session, candidateURI)
	bindOrFail(t, handler, shed)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.enqueueDeferredVirtualProbeV3(context.Background(), shed)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("overflow admission blocked the caller")
	}

	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != probeOutcomeFailed {
		t.Fatalf("overflowed probe outcome = %q, want %q so the client leaves loading", live.VirtualProbeOutcome, probeOutcomeFailed)
	}

	// Poll surface: a client that never holds the push connection observes the
	// same terminal status.
	inventory, err := handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(), PlaybackCaller{UserID: 1, ProfileID: "profile-1", InstallationID: "test-install"}, session.ID)
	if err != nil {
		t.Fatalf("GetPlaybackInventoryV2: %v", err)
	}
	if inventory.InventoryStatus != string(ProbeProvenanceFailed) {
		t.Fatalf("overflow poll status = %q, want failed", inventory.InventoryStatus)
	}

	// Push surface: the background publisher delivers the terminal failure.
	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceFailed) {
			t.Fatalf("overflow push status = %q, want failed", event.payload.InventoryStatus)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no inventory_updated failure push delivered after the overflow shed")
	}
}

// TestDeferredOverflowGateExpirySheds is the gate-expiry variant: when the
// aggregate detached gate stays saturated past the probe's wait budget and the
// bounded re-park set is full, the waiter is shed terminally rather than dropped
// silently, so the session leaves the pending state via both the poll and the
// background push.
func TestDeferredOverflowGateExpirySheds(t *testing.T) {
	previousBudget := virtualBackgroundProbeBudget
	virtualBackgroundProbeBudget = 50 * time.Millisecond
	defer func() { virtualBackgroundProbeBudget = previousBudget }()

	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 814, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	candidateURI := "virtual://movie/tt-gate-expiry?result=cand-1"
	if err := manager.SetVirtualSource(session.ID, candidateURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	if err := manager.SetRealtimeConnection(session.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}

	resolver := &syncPlaybackFileResolver{file: models.MediaFile{
		ID: 814, ContentID: "movie-gate-expiry", FilePath: candidateURI, VirtualOwnerInstallationID: 5,
	}}
	handler := NewPlaybackHandler(manager, resolver)
	handler.InstallationID = "test-install"
	handler.RealtimeHub = playback.NewRealtimeHub()
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		return f, nil
	}
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		PlaybackAttemptID: "attempt-gate-expiry-814", SessionID: session.ID,
		UserID: 1, ProfileID: "profile-1", RequestedMediaFileID: 814, EffectiveMediaFileID: 814,
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}

	// Saturate the aggregate gate so the probe worker can never acquire a slot.
	gate := handler.detachedGate()
	held := gate.capacity()
	for i := 0; i < held; i++ {
		if !gate.tryAcquire() {
			t.Fatalf("could not hold slot %d of %d", i, held)
		}
	}
	defer func() {
		for i := 0; i < held; i++ {
			gate.release()
		}
	}()

	// Fill the re-park set so the waiter has nowhere to park after its wait
	// budget expires.
	startLiveDeferredProbePool(handler)
	for i := 0; i < virtualDeferredProbeQueueSize; i++ {
		handler.deferredProbeRetry[deferredRetryTestKey(i)] = &virtualDeferredProbeV3{}
	}

	conn := newDeferredInventoryPush()
	registration := handler.RealtimeHub.Register(session.ID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	defer handler.RealtimeHub.Unregister(registration)

	deferred := deferredOverflowProbe(session, candidateURI)
	bindOrFail(t, handler, deferred)
	handler.runDeferredVirtualProbe(deferred)

	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != probeOutcomeFailed {
		t.Fatalf("gate-expiry shed outcome = %q, want failed so the client leaves loading", live.VirtualProbeOutcome)
	}
	inventory, err := handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(), PlaybackCaller{UserID: 1, ProfileID: "profile-1", InstallationID: "test-install"}, session.ID)
	if err != nil {
		t.Fatalf("GetPlaybackInventoryV2: %v", err)
	}
	if inventory.InventoryStatus != string(ProbeProvenanceFailed) {
		t.Fatalf("gate-expiry poll status = %q, want failed", inventory.InventoryStatus)
	}
	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceFailed) {
			t.Fatalf("gate-expiry push status = %q, want failed", event.payload.InventoryStatus)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no inventory_updated failure push delivered after the gate-expiry shed")
	}
}

// blockingRealtimeConn blocks every WriteJSON until released, so a test can hold
// the background publisher inside the realtime fan-out.
type blockingRealtimeConn struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingRealtimeConn() *blockingRealtimeConn {
	return &blockingRealtimeConn{entered: make(chan struct{}), release: make(chan struct{})}
}

func (c *blockingRealtimeConn) WriteJSON(any) error {
	c.once.Do(func() { close(c.entered) })
	<-c.release
	return nil
}

// startStalledDeferredPublishPool fires the shed-publication pool once with no
// workers, so a scheduled publication is queued forever. It lets a test observe
// that the request path does not wait on publication.
func startStalledDeferredPublishPool(handler *PlaybackHandler) {
	handler.deferredPublishOnce.Do(func() {
		handler.deferredPublishQueue = make(chan *virtualDeferredProbeV3, virtualDeferredPublishQueueSize)
		handler.deferredPublishRetry = make(map[int]*virtualDeferredProbeV3)
		handler.deferredPublishSignal = make(chan struct{}, 1)
	})
}

// TestDeferredOverflowPublicationRunsOffTheRequestPath is the publication-path
// regression: shedding must update the session's failed state immediately and
// deliver the notification from bounded background work, so a blocked
// publication cannot delay the response. The publish pool is stalled (no worker),
// yet the start request still returns and stores the terminal outcome.
func TestDeferredOverflowPublicationRunsOffTheRequestPath(t *testing.T) {
	source := v3HandlerFixtureFile(t)
	source.ID = 815
	source.ContentID = "movie-overflow-publish-815"
	source.FilePath = "virtual://movie/tt-overflow-publish-815?result=cand-1"
	source.VirtualOwnerInstallationID = 5
	source.ProbeUpdatedAt = nil

	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: source})
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.PlaybackConfig = playbackTestConfig("", "")
	handler.RealtimeHub = playback.NewRealtimeHub()
	handler.InstallationID = "test-install"
	stubCopySeekAnchorV3(handler)
	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:8080/stream?path=" + path, nil
	})
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.AudioTracks = []models.AudioTrack{{Codec: "eac3", Channels: 6}}
		return f, nil
	}
	// The probe pool is pre-fired with no workers and its bounds are filled, so
	// the start's deferred probe is shed on the response path; the publish pool is
	// stalled, so its notification is queued but never executed.
	startLiveDeferredProbePool(handler)
	fillDeferredProbeBackpressure(handler)
	startStalledDeferredPublishPool(handler)

	startDone := make(chan playback.DecisionResponseV3, 1)
	go func() {
		startDone <- startV3PlaybackForHandlerTest(t, handler, func() playback.StartRequestV3 {
			request := v3HandlerStartRequest()
			request.FileID = source.ID
			request.QualityPreference = "original"
			return request
		}())
	}()

	var start playback.DecisionResponseV3
	select {
	case start = <-startDone:
	case <-time.After(3 * time.Second):
		t.Fatal("start blocked while the publication path was stalled")
	}
	if start.PlaybackPlan == nil {
		t.Fatal("start returned no playback plan")
	}
	t.Cleanup(func() { handler.tm.CloseTranscodeSession(start.SessionID, "") })

	live, err := manager.GetSession(start.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != probeOutcomeFailed {
		t.Fatalf("shed probe outcome = %q, want failed (the response must store it before publication)", live.VirtualProbeOutcome)
	}
	if len(handler.deferredPublishQueue) == 0 {
		t.Fatal("shed notification was not queued for background publication")
	}
}

// TestDeferredOverflowShedDoesNotBlockOnPublication proves a shed returns without
// waiting for the realtime fan-out: with the delivery blocked inside WriteJSON,
// the shed has already stored the failed outcome and released its caller.
func TestDeferredOverflowShedDoesNotBlockOnPublication(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 821, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	candidateURI := "virtual://movie/tt-shed-block?result=cand-1"
	if err := manager.SetVirtualSource(session.ID, candidateURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	if err := manager.SetRealtimeConnection(session.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}

	resolver := &syncPlaybackFileResolver{file: models.MediaFile{
		ID: 821, ContentID: "movie-shed-block", FilePath: candidateURI, VirtualOwnerInstallationID: 5,
	}}
	handler := NewPlaybackHandler(manager, resolver)
	handler.RealtimeHub = playback.NewRealtimeHub()
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		PlaybackAttemptID: "attempt-shed-block-821", SessionID: session.ID,
		UserID: 1, ProfileID: "profile-1", RequestedMediaFileID: 821, EffectiveMediaFileID: 821,
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}

	conn := newBlockingRealtimeConn()
	registration := handler.RealtimeHub.Register(session.ID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	defer handler.RealtimeHub.Unregister(registration)

	deferred := deferredOverflowProbe(session, candidateURI)
	bindOrFail(t, handler, deferred)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.shedDeferredVirtualProbe(deferred)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shed blocked on the publication path")
	}

	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != probeOutcomeFailed {
		t.Fatalf("shed probe outcome = %q, want failed", live.VirtualProbeOutcome)
	}

	select {
	case <-conn.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher never reached the realtime write")
	}
	close(conn.release)
}

// TestDeferredProbeRotationBeforeAdmissionDropsStaleWork proves a probe whose
// session has already rotated to a different candidate is dropped at admission:
// the retained generation would not fence the new binding, so the job must never
// start.
func TestDeferredProbeRotationBeforeAdmissionDropsStaleWork(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 816, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, "virtual://movie/tt-admission?result=old", 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	handler := NewPlaybackHandler(manager)
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		return f, nil
	}
	startLiveDeferredProbePool(handler)

	deferred := deferredOverflowProbe(session, "virtual://movie/tt-admission?result=old")
	bindOrFail(t, handler, deferred)

	// Rotate the session before admission.
	if err := manager.SetVirtualSource(session.ID, "virtual://movie/tt-admission?result=new", 5); err != nil {
		t.Fatalf("SetVirtualSource rotate: %v", err)
	}
	handler.enqueueDeferredVirtualProbeV3(context.Background(), deferred)

	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualSourceURI != "virtual://movie/tt-admission?result=new" {
		t.Fatalf("binding = %q, want the rotated candidate", live.VirtualSourceURI)
	}
	if live.VirtualProbeOutcome != "" {
		t.Fatalf("stale probe wrote outcome %q onto the replacement binding; want empty", live.VirtualProbeOutcome)
	}
}

// TestDeferredProbeOutcomeNotWrittenAfterRotationDuringPersistence is the
// rotation-during-persistence regression: the binding fence and the outcome
// write are separate operations, so a rotation that lands between probe
// completion and outcome storage could let the old probe overwrite the new
// binding's outcome. The saver callback performs exactly that rotation after the
// fence has passed; the generation compare-and-swap must reject the stale
// outcome so the replacement binding keeps its own (empty) lifecycle.
func TestDeferredProbeOutcomeNotWrittenAfterRotationDuringPersistence(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 811, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	oldURI := "virtual://movie/tt-rotate?result=old"
	if err := manager.SetVirtualSource(session.ID, oldURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	source := &models.MediaFile{
		ID:                         811,
		ContentID:                  "movie-rotate",
		FilePath:                   oldURI,
		VirtualOwnerInstallationID: 5,
		AudioTracks:                []models.AudioTrack{{Codec: "aac", Channels: 2}},
	}
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: source})
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.AudioTracks = []models.AudioTrack{{Codec: "eac3", Channels: 6}}
		f.CodecAudio = "eac3"
		return f, nil
	}
	handler.VirtualFileMetadataSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		if err := manager.SetVirtualSource(session.ID, "virtual://movie/tt-rotate?result=new", 5); err != nil {
			return VirtualFileMetadataUpdateResult{}, err
		}
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}

	deferred := deferredOverflowProbe(session, oldURI)
	bindOrFail(t, handler, deferred)
	handler.runDeferredVirtualProbe(deferred)

	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualSourceURI != "virtual://movie/tt-rotate?result=new" {
		t.Fatalf("binding = %q, want the rotated candidate", live.VirtualSourceURI)
	}
	if live.VirtualProbeOutcome != "" {
		t.Fatalf("stale probe wrote outcome %q onto the replacement binding; want empty", live.VirtualProbeOutcome)
	}
}

// TestDeferredProbeBindingSurvivesAToBToARotation proves the retained generation
// is a monotonic binding identity rather than a candidate identity: rotating
// A -> B -> A mints a new generation for the final A binding, so a probe admitted
// against the first A must not write its verdict onto the second A.
func TestDeferredProbeBindingSurvivesAToBToARotation(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 817, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	uriA := "virtual://movie/tt-aba?result=a"
	uriB := "virtual://movie/tt-aba?result=b"
	if err := manager.SetVirtualSource(session.ID, uriA, 5); err != nil {
		t.Fatalf("SetVirtualSource A: %v", err)
	}
	handler := NewPlaybackHandler(manager)
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.AudioTracks = []models.AudioTrack{{Codec: "eac3", Channels: 6}}
		return f, nil
	}
	handler.VirtualFileMetadataSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}

	deferred := deferredOverflowProbe(session, uriA)
	bindOrFail(t, handler, deferred)

	// A -> B -> A.
	if err := manager.SetVirtualSource(session.ID, uriB, 5); err != nil {
		t.Fatalf("SetVirtualSource B: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, uriA, 5); err != nil {
		t.Fatalf("SetVirtualSource A again: %v", err)
	}

	handler.runDeferredVirtualProbe(deferred)

	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualSourceURI != uriA {
		t.Fatalf("binding = %q, want the rotated-back A", live.VirtualSourceURI)
	}
	if live.VirtualProbeOutcome != "" {
		t.Fatalf("probe for the first A wrote %q onto the new A binding; want empty", live.VirtualProbeOutcome)
	}
}

// TestDeferredProbeQueuedRotationBeforeWorkerRunDropsStaleOutcome covers a probe
// parked in the retry set (queue pressure) when its binding rotates before any
// worker runs it. The worker must use the generation retained at admission, so
// the stale verdict is dropped and the replacement keeps its lifecycle.
func TestDeferredProbeQueuedRotationBeforeWorkerRunDropsStaleOutcome(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 818, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, "virtual://movie/tt-queued?result=old", 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	handler := NewPlaybackHandler(manager)
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.AudioTracks = []models.AudioTrack{{Codec: "eac3", Channels: 6}}
		return f, nil
	}
	handler.VirtualFileMetadataSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}
	startLiveDeferredProbePool(handler)

	deferred := deferredOverflowProbe(session, "virtual://movie/tt-queued?result=old")
	bindOrFail(t, handler, deferred)
	// Park the probe as a saturated queue would.
	handler.deferredProbeRetry[deferredProbeKey(deferred)] = deferred

	// Rotate before the parked probe runs.
	if err := manager.SetVirtualSource(session.ID, "virtual://movie/tt-queued?result=new", 5); err != nil {
		t.Fatalf("SetVirtualSource rotate: %v", err)
	}
	handler.runDeferredVirtualProbe(deferred)

	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != "" {
		t.Fatalf("parked probe wrote %q onto the replacement binding; want empty", live.VirtualProbeOutcome)
	}
	if live.VirtualSourceURI != "virtual://movie/tt-queued?result=new" {
		t.Fatalf("binding = %q, want the rotated candidate", live.VirtualSourceURI)
	}
}

// TestDeferredProbeQueuedTeardownDropsOutcome covers a queued probe whose session
// is torn down before the worker writes its verdict. The write is dropped, so a
// stopped session is never resurrected with a stale terminal outcome.
func TestDeferredProbeQueuedTeardownDropsOutcome(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 819, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, "virtual://movie/tt-teardown?result=cand-1", 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	handler := NewPlaybackHandler(manager)
	deferred := deferredOverflowProbe(session, "virtual://movie/tt-teardown?result=cand-1")
	bindOrFail(t, handler, deferred)

	if err := manager.StopSession(session.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	if handler.applyDeferredProbeOutcome(deferred, probeOutcomeVerified) {
		t.Fatal("outcome applied to a torn-down session")
	}
	if _, err := manager.GetSession(session.ID); !errors.Is(err, playback.ErrSessionNotFound) {
		t.Fatalf("GetSession error = %v, want ErrSessionNotFound", err)
	}
}

// TestDeferredProbeDurableWriteRejectedMarksFailed is the point-3 regression:
// the deferred probe must not report verified when its durable write is
// rejected by the CAS fence (a superseded snapshot). The probe's outcome is
// failed, which the inventory poll reports so the client leaves loading instead
// of stopping on a stale menu.
func TestDeferredProbeDurableWriteRejectedMarksFailed(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 812, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	candidateURI := "virtual://movie/tt-rejected?result=cand-1"
	if err := manager.SetVirtualSource(session.ID, candidateURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	source := &models.MediaFile{
		ID:                         812,
		ContentID:                  "movie-rejected",
		FilePath:                   candidateURI,
		VirtualOwnerInstallationID: 5,
		AudioTracks:                []models.AudioTrack{{Codec: "aac", Channels: 2}},
	}
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: source})
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.AudioTracks = []models.AudioTrack{{Codec: "eac3", Channels: 6}}
		f.CodecAudio = "eac3"
		return f, nil
	}
	// The write is rejected: a newer snapshot already committed the row, so the
	// probed evidence is superseded and must not be reported verified.
	handler.VirtualFileMetadataSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		return VirtualFileMetadataUpdateResult{MetadataUpdated: false, RowsAffected: 0}, nil
	}

	deferred := deferredOverflowProbe(session, candidateURI)
	bindOrFail(t, handler, deferred)
	handler.runDeferredVirtualProbe(deferred)

	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != probeOutcomeFailed {
		t.Fatalf("rejected durable write outcome = %q, want failed", live.VirtualProbeOutcome)
	}
}

// TestDeferredProbeDurableWriteErrorMarksFailed covers the error arm of the
// durable write: a catalog error means the write did not commit, so the outcome
// is failed rather than a premature verified.
func TestDeferredProbeDurableWriteErrorMarksFailed(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 813, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	candidateURI := "virtual://movie/tt-error?result=cand-1"
	if err := manager.SetVirtualSource(session.ID, candidateURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	source := &models.MediaFile{ID: 813, ContentID: "movie-error", FilePath: candidateURI, VirtualOwnerInstallationID: 5}
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: source})
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.CodecAudio = "eac3"
		return f, nil
	}
	handler.VirtualFileMetadataSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		return VirtualFileMetadataUpdateResult{}, errors.New("catalog unavailable")
	}

	deferred := deferredOverflowProbe(session, candidateURI)
	bindOrFail(t, handler, deferred)
	handler.runDeferredVirtualProbe(deferred)

	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != probeOutcomeFailed {
		t.Fatalf("durable write error outcome = %q, want failed", live.VirtualProbeOutcome)
	}
}

// TestDeferredProbePendingWhileWriteBlockedThenVerifiedWithActualTracks proves
// both halves of the durable-write contract: while the evidence write is still
// blocked the inventory poll reports pending, and once it commits the poll
// reports verified with the tracks the probe actually enumerated (not merely a
// session outcome). The saver is gated on channels, not sleeps, so the ordering
// is deterministic.
func TestDeferredProbePendingWhileWriteBlockedThenVerifiedWithActualTracks(t *testing.T) {
	source := v3HandlerFixtureFile(t)
	source.ID = 820
	source.ContentID = "movie-deferred-blocked-820"
	source.FilePath = "virtual://movie/tt-deferred-blocked-820?result=cand-1"
	source.VirtualOwnerInstallationID = 5
	source.ProbeUpdatedAt = nil

	manager := playback.NewSessionManager(0, 0)
	resolver := &syncPlaybackFileResolver{file: *source}
	handler := NewPlaybackHandler(manager, resolver)
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.PlaybackConfig = playbackTestConfig("", "")
	handler.RealtimeHub = playback.NewRealtimeHub()
	handler.InstallationID = "test-install"
	stubCopySeekAnchorV3(handler)
	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:8080/stream?path=" + path, nil
	})

	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	var writeOnce sync.Once
	probedAt := time.Now().UTC()
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		// A second audio track the candidate declaration could not express.
		f.AudioTracks = []models.AudioTrack{
			{Codec: "aac", Channels: 2, Language: "eng", Default: true},
			{Codec: "eac3", Channels: 6, Language: "deu"},
		}
		f.CodecAudio = "eac3"
		return f, nil
	}
	handler.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		var audio []models.AudioTrack
		if err := json.Unmarshal(args.AudioTracks, &audio); err != nil {
			return VirtualFileMetadataUpdateResult{}, err
		}
		writeOnce.Do(func() { close(writeStarted) })
		<-releaseWrite
		resolver.update(func(f *models.MediaFile) {
			f.AudioTracks = audio
			f.CodecAudio = args.CodecAudio
			f.ProbeUpdatedAt = &probedAt
		})
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}

	start := startV3PlaybackForHandlerTest(t, handler, func() playback.StartRequestV3 {
		request := v3HandlerStartRequest()
		request.FileID = source.ID
		request.QualityPreference = "original"
		return request
	}())
	if start.PlaybackPlan == nil {
		t.Fatal("start returned no playback plan")
	}
	t.Cleanup(func() { handler.tm.CloseTranscodeSession(start.SessionID, "") })

	select {
	case <-writeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("deferred probe did not reach the evidence write")
	}

	// While the write is blocked the poll must report pending, not declared.
	pending, err := handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(), PlaybackCaller{UserID: 1, ProfileID: "profile-1", InstallationID: "test-install"}, start.SessionID)
	if err != nil {
		t.Fatalf("GetPlaybackInventoryV2: %v", err)
	}
	if pending.InventoryStatus != string(ProbeProvenancePending) {
		t.Fatalf("in-flight poll status = %q, want pending", pending.InventoryStatus)
	}

	close(releaseWrite)
	deadline := time.Now().Add(2 * time.Second)
	for {
		verified, err := handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(), PlaybackCaller{UserID: 1, ProfileID: "profile-1", InstallationID: "test-install"}, start.SessionID)
		if err != nil {
			t.Fatalf("GetPlaybackInventoryV2: %v", err)
		}
		if verified.InventoryStatus == string(ProbeProvenanceVerified) {
			if len(verified.AudioTracks) != 2 {
				t.Fatalf("verified poll audio tracks = %#v, want the two enumerated tracks", verified.AudioTracks)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("post-write poll status = %q, want verified", verified.InventoryStatus)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
