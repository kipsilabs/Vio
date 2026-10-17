package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// deferredLifecycleFixture builds a handler whose workers are lazily startable
// and a bound session, so each lifecycle test can drive the exact admission,
// worker, and shed entry points it covers without a full start request. The
// probe gate lets a test hold the worker mid-probe; the saver models the
// committed catalog write the poll and the push re-read.
//
// The service context is LIVE by default and canceled only at test cleanup:
// the worker's gate wait selects on it, so a pre-canceled context makes the
// gate acquisition nondeterministic (the select can take either arm). Tests
// that need the worker pool drained to keep an enqueued job observable set a
// canceled context explicitly and say why.
type deferredLifecycleFixture struct {
	handler  *PlaybackHandler
	manager  *playback.SessionManager
	session  *playback.Session
	resolver *syncPlaybackFileResolver
	source   *models.MediaFile
	probedAt time.Time

	probeStarted chan struct{}
	releaseProbe chan struct{}
	probeOnce    sync.Once
	probedAudio  []models.AudioTrack
	probedSubs   []models.SubtitleTrack
	// saverOverride replaces the saver body when set; saverCalls counts every
	// invocation so a fenced probe that must never reach persistence can be
	// asserted directly instead of inferred.
	saverOverride func() (VirtualFileMetadataUpdateResult, error)
	saverCalls    int
	saverMu       sync.Mutex
}

func newDeferredLifecycleFixture(t *testing.T, fileID int, candidateURI string) *deferredLifecycleFixture {
	t.Helper()
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", fileID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, candidateURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	source := &models.MediaFile{
		ID:                         fileID,
		ContentID:                  "movie-lifecycle",
		FilePath:                   candidateURI,
		Container:                  "virtual",
		CodecVideo:                 "h264",
		Resolution:                 "1080p",
		Duration:                   3600,
		VirtualOwnerInstallationID: 5,
		VideoTracks:                []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8}},
		AudioTracks:                []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng", Default: true}},
	}
	resolver := &syncPlaybackFileResolver{file: *source}
	handler := NewPlaybackHandler(manager, resolver)
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.PlaybackConfig = playbackTestConfig("", "")
	handler.RealtimeHub = playback.NewRealtimeHub()
	handler.InstallationID = "test-install"
	stubCopySeekAnchorV3(handler)
	// The worker's gate wait and the publish worker both select on the service
	// context; keep it alive for the test body and cancel only at cleanup so
	// neither path is nondeterministic.
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	handler.ServiceContext = serviceCtx
	t.Cleanup(serviceCancel)

	fx := &deferredLifecycleFixture{
		handler:      handler,
		manager:      manager,
		session:      session,
		resolver:     resolver,
		source:       source,
		probedAt:     time.Now().UTC(),
		probeStarted: make(chan struct{}),
		releaseProbe: make(chan struct{}),
		probedAudio: []models.AudioTrack{
			{Codec: "aac", Channels: 2, Language: "eng", Default: true},
			{Codec: "eac3", Channels: 6, Language: "deu"},
		},
		probedSubs: []models.SubtitleTrack{{Codec: "subrip", Language: "eng"}},
	}
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		fx.probeOnce.Do(func() { close(fx.probeStarted) })
		<-fx.releaseProbe
		f.AudioTracks = fx.probedAudio
		f.SubtitleTracks = fx.probedSubs
		f.CodecAudio = "eac3"
		return f, nil
	}
	handler.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		fx.saverMu.Lock()
		fx.saverCalls++
		override := fx.saverOverride
		fx.saverMu.Unlock()
		if override != nil {
			return override()
		}
		var audio []models.AudioTrack
		if err := json.Unmarshal(args.AudioTracks, &audio); err != nil {
			return VirtualFileMetadataUpdateResult{}, err
		}
		var subs []models.SubtitleTrack
		if err := json.Unmarshal(args.SubtitleTracks, &subs); err != nil {
			return VirtualFileMetadataUpdateResult{}, err
		}
		resolver.update(func(f *models.MediaFile) {
			f.AudioTracks = audio
			f.SubtitleTracks = subs
			f.CodecAudio = "eac3"
			f.ProbeUpdatedAt = &fx.probedAt
		})
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}
	return fx
}

// saverCallCount reports how many durable evidence writes were attempted.
func (fx *deferredLifecycleFixture) saverCallCount() int {
	fx.saverMu.Lock()
	defer fx.saverMu.Unlock()
	return fx.saverCalls
}

// setSaverOverride installs the saver body under the fixture lock, so a test
// that arms it while a worker may already run stays race-clean.
func (fx *deferredLifecycleFixture) setSaverOverride(override func() (VirtualFileMetadataUpdateResult, error)) {
	fx.saverMu.Lock()
	defer fx.saverMu.Unlock()
	fx.saverOverride = override
}

// job builds a deferred probe for the fixture's session and candidate, with
// the probe's gate channels wired so the test controls the exact lifecycle
// step it covers. The job's file row is a shallow copy of the fixture source
// with the session's file ID, so tests that create extra sessions can hand
// each job the file ID its own session is bound to (the terminal push re-reads
// the inventory by that ID).
func (fx *deferredLifecycleFixture) job(candidateURI string) *virtualDeferredProbeV3 {
	file := *fx.source
	file.ID = fx.session.MediaFileID
	return &virtualDeferredProbeV3{
		stickyKey:      "sticky-lifecycle",
		file:           &file,
		streamURL:      "http://provider.example/stream",
		probeTransient: &models.MediaFile{ID: fx.session.MediaFileID},
		cand:           VirtualPlaybackStream{ID: "cand-1", URI: candidateURI},
		ownerID:        5,
		sessionID:      fx.session.ID,
	}
}

// registerPush attaches a realtime connection and hub registration to the
// fixture session so a published inventory_updated lands on the returned
// channel. The registration is removed at cleanup.
func (fx *deferredLifecycleFixture) registerPush(t *testing.T) *deferredInventoryPush {
	t.Helper()
	if err := fx.manager.SetRealtimeConnection(fx.session.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}
	conn := newDeferredInventoryPush()
	registration := fx.handler.RealtimeHub.Register(fx.session.ID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	t.Cleanup(func() { fx.handler.RealtimeHub.Unregister(registration) })
	return conn
}

// registerGatingPush attaches a gatingInventoryPush to the fixture session so a
// test can hold a publish worker mid-fan-out at a deterministic point. The
// registration is removed at cleanup.
func (fx *deferredLifecycleFixture) registerGatingPush(t *testing.T) *gatingInventoryPush {
	t.Helper()
	if err := fx.manager.SetRealtimeConnection(fx.session.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}
	conn := newGatingInventoryPush()
	registration := fx.handler.RealtimeHub.Register(fx.session.ID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	t.Cleanup(func() { conn.release(); fx.handler.RealtimeHub.Unregister(registration) })
	return conn
}

// countingInventoryPush records every inventory_updated event delivered to a
// session, unlike deferredInventoryPush (which drops everything past the
// first), so exactly-once assertions can count deliveries instead of sampling.
type countingInventoryPush struct {
	mu     sync.Mutex
	events []playback.InventoryUpdatedPayload
}

func (c *countingInventoryPush) WriteJSON(v any) error {
	event, ok := v.(playback.EventEnvelope)
	if !ok || event.Name != playback.RealtimeEventInventoryUpdated {
		return nil
	}
	var payload playback.InventoryUpdatedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return err
	}
	c.mu.Lock()
	c.events = append(c.events, payload)
	c.mu.Unlock()
	return nil
}

func (c *countingInventoryPush) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

// last returns the most recently delivered payload, or nil when none arrived.
func (c *countingInventoryPush) last() *playback.InventoryUpdatedPayload {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) == 0 {
		return nil
	}
	return &c.events[len(c.events)-1]
}

// countAfter returns once at least want events were delivered or the timeout
// expires, and reports the final count. A terminal-notification push is the
// dispatcher's own delivery, so the bound is a receive deadline on that wake,
// not an ordering sleep.
func (c *countingInventoryPush) countAfter(want int, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	for {
		if n := c.count(); n >= want {
			return n
		}
		if time.Now().After(deadline) {
			return c.count()
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// gatingInventoryPush is a realtime connection whose WriteJSON blocks until
// released, so a test can hold a publish worker mid-fan-out at a deterministic
// point. It records every payload it was asked to write (the write is counted
// when WriteJSON is ENTERED, i.e. the worker committed to delivering it) and
// exposes started/released channels so the test orchestrates the block without
// sleeps. The hub's Send calls WriteJSON synchronously, so holding the gate
// pins the publish worker between building the event and acking it.
type gatingInventoryPush struct {
	mu       sync.Mutex
	payloads []playback.InventoryUpdatedPayload
	// ch receives every payload as it is written, so a test can do negative
	// receives ("no second push") against a bounded timeout. entered is closed
	// when the first WriteJSON begins; releaseWrite is closed to let the
	// blocked write return. block controls whether writes wait; once false,
	// writes pass through immediately.
	ch           chan playback.InventoryUpdatedPayload
	block        bool
	entered      chan struct{}
	releaseWrite chan struct{}
	enteredOnce  sync.Once
}

func newGatingInventoryPush() *gatingInventoryPush {
	return &gatingInventoryPush{
		ch:           make(chan playback.InventoryUpdatedPayload, 8),
		block:        true,
		entered:      make(chan struct{}),
		releaseWrite: make(chan struct{}),
	}
}

func (g *gatingInventoryPush) WriteJSON(v any) error {
	event, ok := v.(playback.EventEnvelope)
	if ok && event.Name == playback.RealtimeEventInventoryUpdated {
		var payload playback.InventoryUpdatedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		g.mu.Lock()
		g.payloads = append(g.payloads, payload)
		shouldBlock := g.block
		g.mu.Unlock()
		g.enteredOnce.Do(func() { close(g.entered) })
		if shouldBlock {
			// Hold the payload out of ch until the write is released, so a
			// negative receive ("no second push") is not satisfied by the
			// in-flight first payload still sitting in the buffer.
			<-g.releaseWrite
		}
		select {
		case g.ch <- payload:
		default:
		}
	}
	return nil
}

// release unblocks any in-flight WriteJSON and stops blocking future writes.
func (g *gatingInventoryPush) release() {
	g.mu.Lock()
	g.block = false
	g.mu.Unlock()
	select {
	case <-g.releaseWrite:
	default:
		close(g.releaseWrite)
	}
}

func (g *gatingInventoryPush) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.payloads)
}

// holdDetachedGate saturates the shared detached gate so a publish parks and a
// worker blocks until the test releases slots. The returned closure restores
// every slot it held. A test-side release does NOT wake the terminal-notification
// dispatcher (it wakes on its park-time signal or its 1s backstop ticker, not
// on gate.release); recovery tests rely on the ticker and use a bounded receive
// timeout rather than injecting a signal.
func holdDetachedGate(t *testing.T, handler *PlaybackHandler) func() {
	t.Helper()
	gate := handler.detachedGate()
	held := gate.capacity()
	for i := 0; i < held; i++ {
		if !gate.tryAcquire() {
			t.Fatalf("could not hold slot %d of %d", i, held)
		}
	}
	return func() {
		for i := 0; i < held; i++ {
			gate.release()
		}
	}
}

// saveAttemptWithoutNegotiation registers a session's attempt row without the
// deferred_track_inventory_v1 negotiation on either side, modeling a session
// that was never in the deferred lifecycle (a frozen v1 start, or a v2 client
// that did not advertise the feature).
func (fx *deferredLifecycleFixture) saveAttemptWithoutNegotiation(t *testing.T) {
	t.Helper()
	if err := fx.handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		PlaybackAttemptID:    "attempt-lifecycle",
		SessionID:            fx.session.ID,
		UserID:               1,
		ProfileID:            "profile-1",
		RequestedMediaFileID: fx.source.ID,
		EffectiveMediaFileID: fx.source.ID,
		ExpiresAt:            time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}
}

// saveAttempt registers the session's attempt row so the inventory poll can
// resolve it, matching the state a real start commits. The row carries the
// deferred_track_inventory_v1 negotiation on both sides, because a session in
// the deferred lifecycle can only have been started by a client that negotiated
// it (the start gate); the delivery-side guard reads exactly this record to
// decide whether a failed inventory is meaningful to the session.
func (fx *deferredLifecycleFixture) saveAttempt(t *testing.T) {
	t.Helper()
	if err := fx.handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		PlaybackAttemptID:    "attempt-lifecycle",
		SessionID:            fx.session.ID,
		UserID:               1,
		ProfileID:            "profile-1",
		RequestedMediaFileID: fx.source.ID,
		EffectiveMediaFileID: fx.source.ID,
		NormalizedRequest: playback.StartRequestV3{
			ClientFeatures: []string{playback.FeatureDeferredTrackInventoryV3},
		},
		StartResponse: playback.DecisionResponseV3{
			ServerFeatures: []string{playback.FeatureDeferredTrackInventoryV3},
		},
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}
}

// pollInventory reads the inventory poll surface once for the fixture session.
func (fx *deferredLifecycleFixture) pollInventory(t *testing.T) playback.PlaybackInventoryV3 {
	t.Helper()
	inventory, err := fx.handler.GetPlaybackInventoryV2(
		newAuthorizedPlaybackContext(),
		PlaybackCaller{UserID: 1, ProfileID: "profile-1", InstallationID: "test-install"},
		fx.session.ID,
	)
	if err != nil {
		t.Fatalf("GetPlaybackInventoryV2: %v", err)
	}
	return inventory
}

// pollInventoryStatus polls the inventory endpoint until it reports want or
// the deadline passes. The session outcome is written synchronously before any
// publish, so the first read usually already observes it; the short poll only
// covers surfaces whose status derives from the committed row.
func (fx *deferredLifecycleFixture) pollInventoryStatus(t *testing.T, want string) playback.PlaybackInventoryV3 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		inventory := fx.pollInventory(t)
		if inventory.InventoryStatus == want {
			return inventory
		}
		if time.Now().After(deadline) {
			t.Fatalf("inventory poll status = %q, want %q", inventory.InventoryStatus, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// assertProbedMenu checks the concrete probed track menu — two audio tracks
// (aac stereo eng default, eac3 5.1 deu) and one subrip eng subtitle — on one
// observed inventory, from either surface. Counts alone would pass against a
// menu with the wrong codecs or languages.
func assertProbedMenu(t *testing.T, surface string, audio []playback.AudioInventoryItemV3, subs []playback.SubtitleInventoryItemV3) {
	t.Helper()
	if len(audio) != 2 {
		t.Fatalf("%s audio tracks = %#v, want the two probed tracks", surface, audio)
	}
	if audio[0].Codec != "aac" || audio[0].Channels != 2 || audio[0].Language != "eng" || !audio[0].Default {
		t.Fatalf("%s audio[0] = %#v, want aac/2ch/eng default", surface, audio[0])
	}
	if audio[1].Codec != "eac3" || audio[1].Channels != 6 || audio[1].Language != "deu" || audio[1].Default {
		t.Fatalf("%s audio[1] = %#v, want eac3/6ch/deu non-default", surface, audio[1])
	}
	if len(subs) != 1 {
		t.Fatalf("%s subtitle inventory = %#v, want the probed subtitle", surface, subs)
	}
	if subs[0].Codec != "subrip" || subs[0].Language != "eng" {
		t.Fatalf("%s subtitle[0] = %#v, want subrip/eng", surface, subs[0])
	}
}

// TestDeferredProbeOverflowShedsTerminally is the point-1 regression: when the
// deferred probe queue and the bounded retry set are both full, one more
// admission must shed the job terminally — the session outcome is failed
// synchronously, and the failure is published off the request path so a client
// leaves its loading state through either surface. Both the push and the poll
// must observe the failure: a shed that only updated the session row would
// leave a push-driven client loading forever.
func TestDeferredProbeOverflowShedsTerminally(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 900, "virtual://movie/tt-overflow?result=cand-1")
	fx.saveAttempt(t)
	conn := fx.registerPush(t)
	// A canceled service context makes the lazily-started workers exit at once,
	// so nothing drains the queue: the overflow is deterministic. The shed
	// itself does not consult the service context, and the detached publish
	// worker only needs a free gate slot, which it has.
	fx.handler.ServiceContext = canceledContext()
	fx.handler.startDeferredProbeWorkers()
	fx.handler.deferredProbeWG.Wait()

	// Fill the queue with jobs for distinct sessions so the shed job is the
	// only one that can target the session under test.
	for i := 0; i < virtualDeferredProbeQueueSize; i++ {
		session, err := fx.manager.StartSession(1, "profile-1", 900+i+1, playback.PlayDirect, false)
		if err != nil {
			t.Fatalf("StartSession fill %d: %v", i, err)
		}
		if err := fx.manager.SetVirtualSource(session.ID, fmt.Sprintf("virtual://movie/tt-overflow?result=fill-%d", i), 5); err != nil {
			t.Fatalf("SetVirtualSource fill %d: %v", i, err)
		}
		fill := fx.job(fmt.Sprintf("virtual://movie/tt-overflow?result=fill-%d", i))
		fill.sessionID = session.ID
		if !fx.handler.armDeferredProbePending(context.Background(), fill) {
			t.Fatalf("admission rejected fill job %d as stale", i)
		}
		fx.handler.deferredProbeQueue <- fill
	}
	// Park entries in the retry set so its bound is hit.
	fx.handler.deferredProbeMu.Lock()
	for i := 0; i < virtualDeferredProbeQueueSize; i++ {
		fx.handler.deferredProbeRetry[fmt.Sprintf("retry-%d", i)] = fx.job("virtual://movie/tt-overflow?result=cand-1")
	}
	fx.handler.deferredProbeMu.Unlock()

	// The next admission sheds: the synchronous outcome write must land before
	// the publish is attempted, and the publish must not block the caller.
	overflow := fx.job("virtual://movie/tt-overflow?result=cand-1")
	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.handler.enqueueDeferredVirtualProbeV3(context.Background(), overflow)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("overflow admission blocked the caller")
	}
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != probeOutcomeFailed {
		t.Fatalf("overflow outcome = %q, want failed", live.VirtualProbeOutcome)
	}

	// Push surface: the gate is free, so the shed publishes the terminal
	// failure and the push-driven client leaves loading.
	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceFailed) {
			t.Fatalf("pushed inventory status = %q, want failed", event.payload.InventoryStatus)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no inventory_updated failure push delivered after the overflow shed")
	}

	// Poll surface: the same terminal status is readable from the inventory
	// endpoint for a client that never holds the push connection.
	fx.pollInventoryStatus(t, string(ProbeProvenanceFailed))
}

// TestDeferredProbeGateWaitExpiryShedsTerminally covers the point-1 discard
// path: a worker whose gate wait expires while the retry set is full must shed
// the job terminally — failed outcome synchronously, then the publish goes out
// once the gate has room, so both surfaces report the failure.
func TestDeferredProbeGateWaitExpiryShedsTerminally(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 901, "virtual://movie/tt-gatewait?result=cand-1")
	fx.saveAttempt(t)
	conn := fx.registerPush(t)
	// Start the workers so the retry map is initialized; the queue stays empty
	// so the idle pool does not race the test for the job under test.
	fx.handler.startDeferredProbeWorkers()

	deferred := fx.job("virtual://movie/tt-gatewait?result=cand-1")
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the gate-wait job as stale")
	}
	// Saturate the gate and the retry set so the worker's gate wait expires
	// and the re-park cannot fit.
	releaseGate := holdDetachedGate(t, fx.handler)
	fx.handler.deferredProbeMu.Lock()
	for i := 0; i < virtualDeferredProbeQueueSize; i++ {
		fx.handler.deferredProbeRetry[fmt.Sprintf("retry-%d", i)] = deferred
	}
	fx.handler.deferredProbeMu.Unlock()

	// Shorten the gate-wait budget so the worker's acquire returns false
	// promptly without a sleep. Use the handler-level override rather than
	// mutating virtualBackgroundProbeBudget: the lazily-started convergence
	// watchdog reads that package global from its own goroutine, so a test that
	// writes it here races the watchdog at teardown.
	fx.handler.probeGateWaitOverride = 50 * time.Millisecond

	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.handler.runDeferredVirtualProbe(deferred)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("gate-wait expiry did not return promptly")
	}
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != probeOutcomeFailed {
		t.Fatalf("gate-wait expiry outcome = %q, want failed", live.VirtualProbeOutcome)
	}

	// The shed could not publish while the gate was held: it parked the
	// terminal notification. Releasing the gate lets the dispatcher deliver the
	// parked push on its own wake path — no signal is injected, so the delivery
	// exercises the backstop ticker. The timeout is a receive bound on the
	// ticker-driven wake, not an ordering sleep.
	releaseGate()
	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceFailed) {
			t.Fatalf("pushed inventory status = %q, want failed", event.payload.InventoryStatus)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no inventory_updated failure push delivered after the gate freed")
	}

	// Poll surface: the same terminal status is readable from the inventory
	// endpoint.
	fx.pollInventoryStatus(t, string(ProbeProvenanceFailed))
}

// TestDeferredProbePublishParkedUntilGateFrees is the core dispatcher
// regression: while every detached gate slot is held, a shed must still return
// promptly with the failed outcome synchronously stored, and the push must NOT
// fire — the notification is parked, not dropped. Releasing the slots must
// deliver the parked inventory_updated through the dispatcher's own wake path
// (the 1s backstop ticker), with no signal injected by the test: a wakeup that
// only fires because the test poked the dispatcher proves nothing about the
// production wakeup path.
func TestDeferredProbePublishParkedUntilGateFrees(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 911, "virtual://movie/tt-publish-park?result=cand-1")
	fx.saveAttempt(t)
	conn := fx.registerPush(t)

	deferred := fx.job("virtual://movie/tt-publish-park?result=cand-1")
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the publish-park job as stale")
	}

	releaseGate := holdDetachedGate(t, fx.handler)
	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.handler.shedDeferredVirtualProbe(deferred)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shed blocked on the saturated publish gate")
	}
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != probeOutcomeFailed {
		t.Fatalf("parked-publish outcome = %q, want failed", live.VirtualProbeOutcome)
	}
	// The gate is full, so the publish worker could not start: no event may
	// arrive while the notification is parked.
	select {
	case event := <-conn.ch:
		t.Fatalf("push fired while the gate was saturated (status %q); the notification should be parked", event.payload.InventoryStatus)
	case <-time.After(200 * time.Millisecond):
	}
	fx.handler.deferredPublishMu.Lock()
	parked := len(fx.handler.deferredPublishPending)
	fx.handler.deferredPublishMu.Unlock()
	if parked != 1 {
		t.Fatalf("parked publish entries = %d, want exactly the shed session's notification", parked)
	}

	// Releasing the held slots must deliver the parked notification through
	// the dispatcher's own wake path (the backstop ticker) with no new playback
	// request and no injected signal. The timeout bounds the ticker wake; it is
	// a receive deadline, not an ordering sleep.
	releaseGate()
	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceFailed) {
			t.Fatalf("drained push inventory status = %q, want failed", event.payload.InventoryStatus)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parked inventory_updated was not delivered after the gate freed")
	}
	fx.handler.deferredPublishMu.Lock()
	parked = len(fx.handler.deferredPublishPending)
	fx.handler.deferredPublishMu.Unlock()
	if parked != 0 {
		t.Fatalf("parked publish entries after drain = %d, want 0", parked)
	}
}

// TestDeferredProbePublishParkThenReleaseSurvives covers the park-then-release
// interleaving: the terminal outcome is written (and the wake signal sent)
// while the gate is still saturated, so the publish attempt cannot acquire a
// slot and the notification is parked; the slots free only afterwards. This is
// the lost-wakeup shape a naive park-then-wait design strands — the "slot
// free" moment never produces a new signal. The dispatcher survives it
// structurally: the pending map is the record, the signal is only a wake hint,
// and the backstop ticker re-reads the map, so the parked entry is delivered
// with no new signal. (The name is honest about the ordering: the park lands
// BEFORE the release; the dispatcher's map-plus-ticker is what makes the
// ordering safe.)
func TestDeferredProbePublishParkThenReleaseSurvives(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 913, "virtual://movie/tt-release-before-park?result=cand-1")
	fx.saveAttempt(t)
	conn := fx.registerPush(t)

	deferred := fx.job("virtual://movie/tt-release-before-park?result=cand-1")
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the park-then-release job as stale")
	}

	releaseGate := holdDetachedGate(t, fx.handler)
	// The shed writes the terminal outcome and signals the dispatcher while the
	// gate is still saturated, so its publish attempt cannot acquire a slot and
	// the notification is parked. Only then does the last slot free: the
	// delivery must come from the dispatcher re-reading its map on the backstop
	// ticker, not from a signal that could have been coalesced away.
	fx.handler.shedDeferredVirtualProbe(deferred)
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != probeOutcomeFailed {
		t.Fatalf("park-then-release outcome = %q, want failed", live.VirtualProbeOutcome)
	}
	releaseGate()

	// Exactly-once delivery through the dispatcher's own wake path: the first
	// receive is the ticker-driven publish; a second event would mean a
	// duplicate (the map is keyed by session and the entry is deleted before
	// the publish worker runs, so a re-delivery would be a machinery bug).
	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceFailed) {
			t.Fatalf("park-then-release push status = %q, want failed", event.payload.InventoryStatus)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("notification stranded: the dispatcher never delivered after the slot freed")
	}
	select {
	case event := <-conn.ch:
		t.Fatalf("duplicate inventory_updated delivered (status %q); want exactly-once", event.payload.InventoryStatus)
	case <-time.After(3 * time.Second):
	}
}

// TestDeferredProbePublishCoalescesStaleFileID proves a parked terminal
// notification always publishes the LATEST intent, never a stale one: an entry
// parked for file A is overwritten by a newer terminal update for file B while
// the gate is saturated, and the single delivered push must carry file B's
// inventory — not file A's. The map is keyed by session and the batch re-reads
// the CURRENT value after acquiring its slot, so the overwrite is what gets
// published; a stale-fileID push would mean the client renders the wrong
// release's menu.
func TestDeferredProbePublishCoalescesStaleFileID(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	// File B is what the session's binding ultimately points at, so the
	// dispatcher's push (keyed by the parked intent's file) resolves to this
	// session only when the intent is the NEWER file B. File A is a distinct,
	// older menu that must NOT appear in the delivered push.
	fileA := models.MediaFile{
		ID: 5001, ContentID: "movie-coalesce", FilePath: "virtual://movie/tt-coalesce?result=cand-0", Container: "virtual", CodecVideo: "h264", Resolution: "1080p", Duration: 3600,
		VirtualOwnerInstallationID: 5,
		AudioTracks:                []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng", Default: true}},
	}
	fileB := models.MediaFile{
		ID: 5002, ContentID: "movie-coalesce", FilePath: "virtual://movie/tt-coalesce?result=cand-1", Container: "virtual", CodecVideo: "h264", Resolution: "1080p", Duration: 3600,
		VirtualOwnerInstallationID: 5,
		VideoTracks:                []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8}},
		AudioTracks:                []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng", Default: true}, {Codec: "eac3", Channels: 6, Language: "deu"}},
		SubtitleTracks:             []models.SubtitleTrack{{Codec: "subrip", Language: "eng"}},
	}
	// The session is bound to file B: StartSession on A, then the binding move
	// re-binds to B, so GetSessionsByMediaFileID(B) matches this session and
	// GetSessionsByMediaFileID(A) does not.
	session, err := manager.StartSession(1, "profile-1", fileA.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, fileB.FilePath, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	if err := manager.SetEffectiveMediaFileID(session.ID, fileB.ID); err != nil {
		t.Fatalf("SetEffectiveMediaFileID: %v", err)
	}
	// The publish worker only sends a push whose inventory status is terminal
	// (verified or failed); a pending session's declared inventory is not new
	// information and is suppressed. The newer terminal update is what parks
	// the intent, so mark the session terminally probed against file B.
	if err := manager.SetVirtualProbeOutcome(session.ID, probeOutcomeVerified); err != nil {
		t.Fatalf("SetVirtualProbeOutcome: %v", err)
	}
	resolver := &syncPlaybackFileResolver{file: fileB}
	handler := NewPlaybackHandler(manager, resolver)
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.PlaybackConfig = playbackTestConfig("", "")
	handler.RealtimeHub = playback.NewRealtimeHub()
	handler.InstallationID = "test-install"
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	handler.ServiceContext = serviceCtx
	t.Cleanup(serviceCancel)
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		PlaybackAttemptID: "attempt-coalesce", SessionID: session.ID, UserID: 1, ProfileID: "profile-1",
		RequestedMediaFileID: fileB.ID, EffectiveMediaFileID: fileB.ID, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}
	conn := &countingInventoryPush{}
	if err := manager.SetRealtimeConnection(session.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}
	registration := handler.RealtimeHub.Register(session.ID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	t.Cleanup(func() { handler.RealtimeHub.Unregister(registration) })
	binding, err := manager.VirtualSourceGeneration(session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceGeneration: %v", err)
	}

	releaseGate := holdDetachedGate(t, handler)
	// Park the STALE intent (file A) first, then the newer terminal update
	// overwrites it with file B while the gate is still full.
	// parkDeferredProbePublish is the machinery's own entry point, so this is
	// the honest coalescing path. The single delivered push must be for file B.
	handler.parkDeferredProbePublish(session.ID, fileA.ID, binding)
	handler.parkDeferredProbePublish(session.ID, fileB.ID, binding)
	handler.deferredPublishMu.Lock()
	parked := len(handler.deferredPublishPending)
	intent := handler.deferredPublishPending[session.ID]
	handler.deferredPublishMu.Unlock()
	if parked != 1 {
		t.Fatalf("parked entries = %d, want 1 (coalesced by session)", parked)
	}
	if intent.fileID != fileB.ID {
		t.Fatalf("parked intent fileID = %d, want the newer file B (%d)", intent.fileID, fileB.ID)
	}

	// Release the gate: the dispatcher delivers the single coalesced
	// notification, and it must be file B's menu (the probed one), not file A's
	// lone declared track.
	releaseGate()
	if got := conn.countAfter(1, 5*time.Second); got != 1 {
		t.Fatalf("coalesced pushes = %d, want exactly 1", got)
	}
	conn.mu.Lock()
	payload := conn.events[0]
	conn.mu.Unlock()
	if payload.InventoryStatus != string(ProbeProvenanceVerified) && payload.InventoryStatus != string(ProbeProvenanceFailed) {
		t.Fatalf("coalesced push status = %q, want a terminal status", payload.InventoryStatus)
	}
	assertProbedMenu(t, "coalesced push", payload.AudioTracks, payload.SubtitleInventory)
}

// TestDeferredProbePublishOverflowRescansAllSessions proves the bounded
// pending map never loses terminal intent: with the gate saturated, more
// sessions reach a terminal deferred outcome than the map can hold, so the
// overflow sets the rescan flag instead of dropping them. Once the gate frees,
// the dispatcher must deliver a terminal push for EVERY session — the parked
// ones from the map and the overflow ones re-parked from the session manager's
// outstanding-notification scan — with no session silently left loading.
func TestDeferredProbePublishOverflowRescansAllSessions(t *testing.T) {
	const total = virtualDeferredPublishPendingCap + 3
	manager := playback.NewSessionManager(0, 0)
	resolver := &syncPlaybackFileResolver{file: models.MediaFile{ID: 1, Container: "virtual", CodecVideo: "h264", Resolution: "1080p", Duration: 3600}}
	handler := NewPlaybackHandler(manager, resolver)
	handler.RealtimeHub = playback.NewRealtimeHub()
	handler.InstallationID = "test-install"
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	handler.ServiceContext = serviceCtx
	t.Cleanup(serviceCancel)

	type parkedSession struct {
		session *playback.Session
		uri     string
		conn    *countingInventoryPush
	}
	parked := make([]parkedSession, 0, total)
	for i := 0; i < total; i++ {
		session, err := manager.StartSession(1, "profile-1", 3000+i, playback.PlayDirect, false)
		if err != nil {
			t.Fatalf("StartSession %d: %v", i, err)
		}
		uri := fmt.Sprintf("virtual://movie/tt-overflow-push?result=cand-%d", i)
		if err := manager.SetVirtualSource(session.ID, uri, 5); err != nil {
			t.Fatalf("SetVirtualSource %d: %v", i, err)
		}
		// These sessions model deferred-lifecycle clients, which by
		// construction negotiated deferred_track_inventory_v1; the failed-push
		// guard reads the attempt row, so seed the negotiation it would see.
		if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
			PlaybackAttemptID:    fmt.Sprintf("attempt-overflow-%d", i),
			SessionID:            session.ID,
			UserID:               1,
			ProfileID:            "profile-1",
			RequestedMediaFileID: session.MediaFileID,
			EffectiveMediaFileID: session.MediaFileID,
			NormalizedRequest: playback.StartRequestV3{
				ClientFeatures: []string{playback.FeatureDeferredTrackInventoryV3},
			},
			StartResponse: playback.DecisionResponseV3{
				ServerFeatures: []string{playback.FeatureDeferredTrackInventoryV3},
			},
			ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("SaveAttempt %d: %v", i, err)
		}
		if err := manager.SetRealtimeConnection(session.ID, true); err != nil {
			t.Fatalf("SetRealtimeConnection %d: %v", i, err)
		}
		conn := &countingInventoryPush{}
		registration := handler.RealtimeHub.Register(session.ID, conn)
		if registration == nil {
			t.Fatalf("expected a realtime registration for session %d", i)
		}
		t.Cleanup(func() { handler.RealtimeHub.Unregister(registration) })
		parked = append(parked, parkedSession{session: session, uri: uri, conn: conn})
	}

	releaseGate := holdDetachedGate(t, handler)
	for _, ps := range parked {
		fileID := ps.session.MediaFileID
		deferred := &virtualDeferredProbeV3{
			stickyKey:      "sticky-overflow-rescan",
			file:           &models.MediaFile{ID: fileID},
			streamURL:      "http://provider.example/stream",
			probeTransient: &models.MediaFile{ID: fileID},
			cand:           VirtualPlaybackStream{ID: "cand-1", URI: ps.uri},
			ownerID:        5,
			sessionID:      ps.session.ID,
		}
		if !handler.armDeferredProbePending(context.Background(), deferred) {
			t.Fatalf("admission rejected the overflow job for session %s as stale", ps.session.ID)
		}
		handler.shedDeferredVirtualProbe(deferred)
	}

	// The map caps at its bound; the rescan flag carries the overflow intent.
	handler.deferredPublishMu.Lock()
	parkedCount := len(handler.deferredPublishPending)
	rescan := handler.deferredPublishRescan
	handler.deferredPublishMu.Unlock()
	if parkedCount != virtualDeferredPublishPendingCap {
		t.Fatalf("parked entries = %d, want the map bound %d", parkedCount, virtualDeferredPublishPendingCap)
	}
	if !rescan {
		t.Fatal("rescan flag not set: terminal notifications past the map bound would be lost")
	}

	// Every session reaches the terminal failed state synchronously; the
	// pushes are what the dispatcher owes once the gate frees.
	for _, ps := range parked {
		live, err := manager.GetSession(ps.session.ID)
		if err != nil {
			t.Fatalf("GetSession %s: %v", ps.session.ID, err)
		}
		if live.VirtualProbeOutcome != probeOutcomeFailed {
			t.Fatalf("session %s outcome = %q, want failed", ps.session.ID, live.VirtualProbeOutcome)
		}
	}

	// Free the gate: the dispatcher must deliver every session's terminal push
	// through its own wake path — the parked entries from the map and the
	// overflow entries recovered by the outstanding-notification rescan. No
	// signal is injected; the backstop ticker drives the whole recovery. Each
	// push is counted, so exactly-once is asserted per session rather than
	// sampled.
	releaseGate()
	for i, ps := range parked {
		if got := ps.conn.countAfter(1, 30*time.Second); got != 1 {
			t.Fatalf("session %d (%s) received %d terminal pushes, want exactly 1", i, ps.session.ID, got)
		}
	}

	// Convergence, not a snapshot: the dispatcher settles only when every
	// terminal outcome's generation is acked delivered. Assert the ack
	// watermark caught up to each session's binding generation — the rescan
	// skips anything at-or-below the watermark, so this is what guarantees no
	// re-publish.
	for i, ps := range parked {
		binding, err := manager.VirtualSourceGeneration(ps.session.ID)
		if err != nil {
			t.Fatalf("VirtualSourceGeneration %d: %v", i, err)
		}
		notified, err := manager.VirtualProbeNotifiedGeneration(ps.session.ID)
		if err != nil {
			t.Fatalf("VirtualProbeNotifiedGeneration %d: %v", i, err)
		}
		if notified != binding {
			t.Fatalf("session %d ack watermark = %d, want the binding generation %d (the delivered terminal outcome is unacknowledged)", i, notified, binding)
		}
	}

	// The system must STAY settled: an empty map or a clear flag seen during a
	// rescan refill is transient. Wait on the observable settled state — the
	// pending map empty, no in-flight worker, the rescan flag clear, and the
	// outstanding-notification accessor reporting nothing left — instead of
	// sleeping a fixed number of backstop ticks. Once every one of those is true
	// nothing schedules another push, so the per-session counts below are final.
	waitDeferredCondition(t, func() bool {
		handler.deferredPublishMu.Lock()
		settled := len(handler.deferredPublishPending) == 0 && !handler.deferredPublishRescan && len(handler.deferredPublishInFlight) == 0
		handler.deferredPublishMu.Unlock()
		return settled && len(manager.VirtualProbeOutstandingNotifications(virtualDeferredPublishPendingCap+total)) == 0
	}, "dispatcher settled: empty map, no in-flight worker, clear rescan flag, no outstanding notifications")
	handler.deferredPublishMu.Lock()
	remaining := len(handler.deferredPublishPending)
	rescan = handler.deferredPublishRescan
	handler.deferredPublishMu.Unlock()
	if remaining != 0 || rescan {
		t.Fatalf("dispatcher not settled: parked entries = %d, rescan = %v; want empty map and clear flag", remaining, rescan)
	}
	if outstanding := manager.VirtualProbeOutstandingNotifications(virtualDeferredPublishPendingCap + total); len(outstanding) != 0 {
		t.Fatalf("outstanding terminal notifications after settle = %d, want 0", len(outstanding))
	}
	for i, ps := range parked {
		if got := ps.conn.count(); got != 1 {
			t.Fatalf("session %d received %d terminal pushes after settle, want exactly 1 (duplicate delivery)", i, got)
		}
	}
}

// TestDeferredProbePublishRescanVsAckRace proves the rescan never re-parks a
// session whose publish is mid-flight, even when the rescan's outstanding
// snapshot was taken before the worker's ack. The machinery has no seam to
// pause the rescan mid-validate, so the test drives the invariant directly:
// with a worker held mid-publish (in-flight), force an overflow rescan and
// assert the mid-publish session is never re-parked and gets exactly one push;
// then, after the worker acks, force another rescan and assert the session is
// no longer outstanding and gets no second push. Exactly-once per session
// across the whole test.
func TestDeferredProbePublishRescanVsAckRace(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 921, "virtual://movie/tt-rescan-ack?result=cand-1")
	fx.saveAttempt(t)
	conn := fx.registerGatingPush(t)

	deferred := fx.job("virtual://movie/tt-rescan-ack?result=cand-1")
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the rescan-ack job as stale")
	}

	// Saturate the gate so the shed parks the terminal notification, then
	// release it so the dispatcher claims a slot and starts the worker. The
	// gating connection holds the worker mid-fan-out: it has committed to the
	// push (WriteJSON entered) but has not acked, so the session is in-flight.
	releaseGate := holdDetachedGate(t, fx.handler)
	fx.handler.shedDeferredVirtualProbe(deferred)
	releaseGate()

	// Wait for the worker to reach the gating write: it is now mid-publish,
	// in-flight, unacked. The dispatcher is quiet (the entry is in-flight).
	select {
	case <-conn.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publish worker never reached the gating write")
	}

	// Force an overflow rescan while the worker is mid-publish. The rescan
	// must skip the in-flight session: it is not re-parked, and no second push
	// is scheduled. The rescan flag is set so rescanDeferredPublishOverflow
	// actually runs.
	fx.handler.deferredPublishMu.Lock()
	fx.handler.deferredPublishRescan = true
	fx.handler.deferredPublishMu.Unlock()
	fx.handler.rescanDeferredPublishOverflow()
	fx.handler.deferredPublishMu.Lock()
	_, parked := fx.handler.deferredPublishPending[fx.session.ID]
	_, inFlight := fx.handler.deferredPublishInFlight[fx.session.ID]
	fx.handler.deferredPublishMu.Unlock()
	if parked {
		t.Fatal("rescan re-parked a mid-publish (in-flight) session; it must be skipped")
	}
	if !inFlight {
		t.Fatal("mid-publish session is not marked in-flight; the rescan-vs-ack guard is not engaged")
	}
	// No second push may arrive while the worker is blocked.
	select {
	case <-conn.ch:
		t.Fatalf("second push delivered while the first was still mid-publish")
	default:
	}

	// Release the worker: it completes the push, acks, clears in-flight, and
	// signals done. Exactly one push total.
	conn.release()
	waitDeferredPublishIdle(t, fx.handler, fx.manager, fx.session.ID, deferred.bindingGeneration)
	// After the ack, the session is no longer outstanding and a forced
	// rescan re-parks nothing and schedules no second push.
	fx.handler.deferredPublishMu.Lock()
	fx.handler.deferredPublishRescan = true
	fx.handler.deferredPublishMu.Unlock()
	fx.handler.rescanDeferredPublishOverflow()
	fx.handler.deferredPublishMu.Lock()
	_, parked = fx.handler.deferredPublishPending[fx.session.ID]
	fx.handler.deferredPublishMu.Unlock()
	if parked {
		t.Fatal("rescan re-parked an already-acked session; the watermark must cover it")
	}
	if outstanding := fx.manager.VirtualProbeOutstandingNotifications(10); len(outstanding) != 0 {
		t.Fatalf("outstanding notifications after ack = %d, want 0", len(outstanding))
	}
	// No additional push after the ack. Drain the single completed payload
	// first, then assert the channel stays empty for a bounded window.
drain:
	for {
		select {
		case <-conn.ch:
		default:
			break drain
		}
	}
	select {
	case <-conn.ch:
		t.Fatalf("duplicate push delivered after the ack")
	case <-time.After(500 * time.Millisecond):
	}
	if got := conn.count(); got != 1 {
		t.Fatalf("total pushes = %d, want exactly 1", got)
	}
}

// TestDeferredProbePublishInFlightNoChurn proves the dispatcher goes quiet
// while a publish worker is mid-fan-out instead of spinning: a second intent
// parked for the same in-flight session must not schedule a second push, and
// the gate must not be repeatedly acquired/released. While the worker is
// blocked, the newer intent coalesces onto the parked entry; when the worker
// completes, exactly one coalesced push (the newer intent) arrives via the
// done-signal path — never a duplicate of the first.
func TestDeferredProbePublishInFlightNoChurn(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 922, "virtual://movie/tt-inflight-churn?result=cand-1")
	fx.saveAttempt(t)
	conn := fx.registerGatingPush(t)

	deferred := fx.job("virtual://movie/tt-inflight-churn?result=cand-1")
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the in-flight-churn job as stale")
	}

	// Saturate the gate so the shed parks, then release so the dispatcher
	// claims a slot and starts the worker, which the gating connection holds
	// mid-fan-out.
	releaseGate := holdDetachedGate(t, fx.handler)
	fx.handler.shedDeferredVirtualProbe(deferred)
	releaseGate()
	select {
	case <-conn.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publish worker never reached the gating write")
	}

	// While the worker is mid-publish, simulate a NEWER terminal update for the
	// same session: bump the binding generation via SetVirtualSource (so the
	// intent carries gen 2, which the first worker's gen-1 ack cannot cover),
	// and park intent for it. The dispatcher must treat the session as in-flight
	// and go quiet: no second push, no gate churn. The newer intent coalesces
	// onto the parked entry.
	if err := fx.manager.SetVirtualSource(fx.session.ID, "virtual://movie/tt-inflight-churn?result=cand-2", 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	// A binding move clears the outcome; the newer terminal update that parks
	// this intent also records its outcome, so re-mark terminal for gen 2.
	if err := fx.manager.SetVirtualProbeOutcome(fx.session.ID, probeOutcomeVerified); err != nil {
		t.Fatalf("SetVirtualProbeOutcome: %v", err)
	}
	newerGen, err := fx.manager.VirtualSourceGeneration(fx.session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceGeneration: %v", err)
	}
	fx.handler.parkDeferredProbePublish(fx.session.ID, fx.source.ID, newerGen)
	// Drive one batch while the write is blocked. A false return is the
	// dispatcher's sleep decision; selecting an in-flight entry used to return
	// more-work and spin. No timing sample of gate occupancy can prove that.
	if more := fx.handler.drainDeferredPublishBatch(fx.handler.ServiceContext); more {
		t.Fatal("batch requested another immediate pass with only in-flight work")
	}
	if got := conn.count(); got != 1 {
		t.Fatalf("writes while blocked = %d, want only the first write", got)
	}
	if held := len(fx.handler.detachedGate().slots); held != 1 {
		t.Fatalf("held gate slots = %d, want the blocked worker's one slot", held)
	}

	// Release the worker: it completes, acks, clears in-flight, signals done.
	// The coalesced newer intent then dispatches exactly once.
	conn.release()
	// Exactly one MORE push (the coalesced newer intent) arrives after the
	// first — total 2 writes but they are two distinct generations' pushes for
	// the same session, which is correct (the first was the older intent's
	// push that was already in flight). Assert no THIRD push and no duplicate
	// of the coalesced one.
	// Consume the older write and then the coalesced newer write. No manual
	// wake is sent: worker completion and the dispatcher own progress.
	for i := 0; i < 2; i++ {
		select {
		case <-conn.ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("write %d did not complete", i+1)
		}
	}
	waitDeferredPublishIdle(t, fx.handler, fx.manager, fx.session.ID, newerGen)
	// No additional push beyond the two generations.
drain:
	for {
		select {
		case <-conn.ch:
		default:
			break drain
		}
	}
	select {
	case <-conn.ch:
		t.Fatalf("extra push delivered after the coalesced intent; want no duplicate")
	case <-time.After(500 * time.Millisecond):
	}
	if got := conn.count(); got != 2 {
		t.Fatalf("total pushes = %d, want exactly 2", got)
	}
}

// TestDeferredProbePublishSameFileSessionsTargeted proves the publish worker's
// delivery is strictly session-targeted (PublishInventoryUpdatedToSession),
// not the file-wide fan-out: two distinct sessions playing the SAME file must
// each receive exactly its own inventory_updated event, exactly once — never a
// cross-delivery. The old file-wide fan-out would push every live session on
// the file whenever ANY session completed, delivering duplicates to sibling
// sessions that never acked them and breaking exactly-once.
func TestDeferredProbePublishSameFileSessionsTargeted(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	const sharedFileID = 6001
	file := models.MediaFile{
		ID: sharedFileID, ContentID: "movie-same-file", FilePath: "virtual://movie/tt-same-file?result=cand-1", Container: "virtual", CodecVideo: "h264", Resolution: "1080p", Duration: 3600,
		VirtualOwnerInstallationID: 5,
		VideoTracks:                []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8}},
		AudioTracks:                []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng", Default: true}, {Codec: "eac3", Channels: 6, Language: "deu"}},
		SubtitleTracks:             []models.SubtitleTrack{{Codec: "subrip", Language: "eng"}},
	}
	resolver := &syncPlaybackFileResolver{file: file}
	handler := NewPlaybackHandler(manager, resolver)
	handler.RealtimeHub = playback.NewRealtimeHub()
	handler.InstallationID = "test-install"
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	handler.ServiceContext = serviceCtx
	t.Cleanup(serviceCancel)

	// Two sessions bound to the SAME fileID.
	session1, err := manager.StartSession(1, "profile-1", sharedFileID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession 1: %v", err)
	}
	if err := manager.SetVirtualSource(session1.ID, file.FilePath, 5); err != nil {
		t.Fatalf("SetVirtualSource 1: %v", err)
	}
	if err := manager.SetRealtimeConnection(session1.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection 1: %v", err)
	}
	conn1 := &countingInventoryPush{}
	reg1 := handler.RealtimeHub.Register(session1.ID, conn1)
	t.Cleanup(func() { handler.RealtimeHub.Unregister(reg1) })
	if err := manager.SetVirtualProbeOutcome(session1.ID, probeOutcomeVerified); err != nil {
		t.Fatalf("SetVirtualProbeOutcome 1: %v", err)
	}
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		PlaybackAttemptID: "attempt-1", SessionID: session1.ID, UserID: 1, ProfileID: "profile-1",
		RequestedMediaFileID: sharedFileID, EffectiveMediaFileID: sharedFileID, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveAttempt 1: %v", err)
	}

	session2, err := manager.StartSession(1, "profile-2", sharedFileID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession 2: %v", err)
	}
	if err := manager.SetVirtualSource(session2.ID, file.FilePath, 5); err != nil {
		t.Fatalf("SetVirtualSource 2: %v", err)
	}
	if err := manager.SetRealtimeConnection(session2.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection 2: %v", err)
	}
	conn2 := &countingInventoryPush{}
	reg2 := handler.RealtimeHub.Register(session2.ID, conn2)
	t.Cleanup(func() { handler.RealtimeHub.Unregister(reg2) })
	if err := manager.SetVirtualProbeOutcome(session2.ID, probeOutcomeVerified); err != nil {
		t.Fatalf("SetVirtualProbeOutcome 2: %v", err)
	}
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		PlaybackAttemptID: "attempt-2", SessionID: session2.ID, UserID: 1, ProfileID: "profile-2",
		RequestedMediaFileID: sharedFileID, EffectiveMediaFileID: sharedFileID, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveAttempt 2: %v", err)
	}

	gen1, _ := manager.VirtualSourceGeneration(session1.ID)
	gen2, _ := manager.VirtualSourceGeneration(session2.ID)

	// Park intent for session 1 only, with gate saturated, then release.
	// Only session 1 must receive a push; session 2 must receive NOTHING.
	releaseGate := holdDetachedGate(t, handler)
	handler.parkDeferredProbePublish(session1.ID, sharedFileID, gen1)
	releaseGate()
	if got := conn1.countAfter(1, 5*time.Second); got != 1 {
		t.Fatalf("session 1 pushes = %d, want exactly 1", got)
	}
	// Ack and in-flight clear fence the entire first publish attempt.
	waitDeferredPublishIdle(t, handler, manager, session1.ID, gen1)
	if got := conn2.count(); got != 0 {
		t.Fatalf("session 2 received %d pushes from session 1's notification (cross-delivery bug)", got)
	}

	// Now park intent for session 2 only, then release.
	releaseGate = holdDetachedGate(t, handler)
	handler.parkDeferredProbePublish(session2.ID, sharedFileID, gen2)
	releaseGate()
	if got := conn2.countAfter(1, 5*time.Second); got != 1 {
		t.Fatalf("session 2 pushes = %d, want exactly 1", got)
	}
	// Ack and in-flight clear fence the entire second publish attempt.
	waitDeferredPublishIdle(t, handler, manager, session2.ID, gen2)
	if got := conn1.count(); got != 1 {
		t.Fatalf("session 1 received %d total pushes after session 2's notification (cross-delivery bug)", got)
	}

	// Each connection received only its own session ID.
	conn1.mu.Lock()
	s1Payload := conn1.events[0]
	conn1.mu.Unlock()
	conn2.mu.Lock()
	s2Payload := conn2.events[0]
	conn2.mu.Unlock()
	if s1Payload.SessionID != session1.ID {
		t.Fatalf("session 1 received payload for session %q", s1Payload.SessionID)
	}
	if s2Payload.SessionID != session2.ID {
		t.Fatalf("session 2 received payload for session %q", s2Payload.SessionID)
	}
}

// TestDeferredProbePublishReverseGenerationParkingCoalesces proves parking is
// monotonic by binding generation: a delayed older worker attempting to park
// generation N must not replace a newer generation N+1 already parked, and a
// park whose generation the notified watermark already covers is dropped
// outright. When the gate frees, exactly one push delivers carrying the
// generation N+1 intent, and the watermark acknowledges that generation.
func TestDeferredProbePublishReverseGenerationParkingCoalesces(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 923, "virtual://movie/tt-rev-gen?result=cand-1")
	fx.saveAttempt(t)
	conn := &countingInventoryPush{}
	if err := fx.manager.SetRealtimeConnection(fx.session.ID, true); err != nil {
		t.Fatal(err)
	}
	reg := fx.handler.RealtimeHub.Register(fx.session.ID, conn)
	t.Cleanup(func() { fx.handler.RealtimeHub.Unregister(reg) })
	oldGen, err := fx.manager.VirtualSourceGeneration(fx.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Rebinding even the same candidate starts a newer lifecycle.
	if err := fx.manager.SetVirtualSource(fx.session.ID, fx.source.FilePath, 5); err != nil {
		t.Fatal(err)
	}
	newGen, err := fx.manager.VirtualSourceGeneration(fx.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.manager.SetVirtualProbeOutcome(fx.session.ID, probeOutcomeVerified); err != nil {
		t.Fatal(err)
	}
	release := holdDetachedGate(t, fx.handler)
	fx.handler.parkDeferredProbePublish(fx.session.ID, fx.source.ID, newGen)
	fx.handler.parkDeferredProbePublish(fx.session.ID, 9999, oldGen)
	// Selection can temporarily remove an entry even while the gate is full.
	// A synchronous batch completes that nonblocking claim/re-park cycle.
	waitDeferredCondition(t, func() bool {
		fx.handler.deferredPublishMu.Lock()
		defer fx.handler.deferredPublishMu.Unlock()
		intent, ok := fx.handler.deferredPublishPending[fx.session.ID]
		return ok && intent.generation == newGen && intent.fileID == fx.source.ID
	}, "newer intent preserved")
	if !fx.manager.MarkVirtualProbeNotified(fx.session.ID, oldGen) {
		t.Fatal("older watermark did not advance")
	}
	fx.handler.parkDeferredProbePublish(fx.session.ID, 9998, oldGen)
	release()
	if n := conn.countAfter(1, 5*time.Second); n != 1 {
		t.Fatalf("pushes = %d, want 1", n)
	}
	waitDeferredPublishIdle(t, fx.handler, fx.manager, fx.session.ID, newGen)
	payload := conn.last()
	if payload.SessionID != fx.session.ID || payload.EffectiveMediaFileID != fx.source.ID || payload.InventoryStatus != probeOutcomeVerified {
		t.Fatalf("wrong newer intent payload: %#v", payload)
	}
	// Once the newer generation is acked, neither it nor an older one can
	// re-enter the queue. This checks the covered-watermark rule on an empty map.
	fx.handler.parkDeferredProbePublish(fx.session.ID, fx.source.ID, newGen)
	fx.handler.parkDeferredProbePublish(fx.session.ID, 9999, oldGen)
	fx.handler.deferredPublishMu.Lock()
	remaining := len(fx.handler.deferredPublishPending)
	fx.handler.deferredPublishMu.Unlock()
	if remaining != 0 || conn.count() != 1 {
		t.Fatalf("covered park: pending=%d pushes=%d", remaining, conn.count())
	}
}

// Wait on observable worker state, not a guessed scheduling delay. The ticker
// only samples state; the deadline fails the test if the worker stops progressing.
func waitDeferredCondition(t *testing.T, ready func() bool, what string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !ready() {
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}

func waitDeferredPublishIdle(t *testing.T, h *PlaybackHandler, m *playback.SessionManager, id string, generation uint64) {
	t.Helper()
	waitDeferredCondition(t, func() bool {
		h.deferredPublishMu.Lock()
		defer h.deferredPublishMu.Unlock()
		_, active := h.deferredPublishInFlight[id]
		_, parked := h.deferredPublishPending[id]
		notified, err := m.VirtualProbeNotifiedGeneration(id)
		return err == nil && notified == generation && !active && !parked && len(h.detachedGate().slots) == 0
	}, "publish ack, in-flight clear and gate release")
}

// TestDeferredProbePublishDeliveredAfterOtherConsumerReleases proves the
// dispatcher does not care which subsystem freed the gate: the parked
// notification is a function of the shared gate, not of whoever held it. A
// different consumer (a direct acquire on the shared detached gate, the same
// primitive a detached probe or subtitle worker uses) saturates every slot;
// the shed parks; the other consumer's release lets the dispatcher deliver on
// its backstop ticker with no signal.
func TestDeferredProbePublishDeliveredAfterOtherConsumerReleases(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 914, "virtual://movie/tt-other-consumer?result=cand-1")
	fx.saveAttempt(t)
	conn := fx.registerPush(t)

	deferred := fx.job("virtual://movie/tt-other-consumer?result=cand-1")
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the other-consumer job as stale")
	}

	// A different gate consumer: acquire every slot directly, the same call
	// any detached worker makes, so the publish worker cannot start.
	gate := fx.handler.detachedGate()
	held := gate.capacity()
	for i := 0; i < held; i++ {
		if !gate.tryAcquire() {
			t.Fatalf("other consumer could not hold slot %d of %d", i, held)
		}
	}
	fx.handler.shedDeferredVirtualProbe(deferred)
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != probeOutcomeFailed {
		t.Fatalf("other-consumer outcome = %q, want failed", live.VirtualProbeOutcome)
	}
	select {
	case event := <-conn.ch:
		t.Fatalf("push fired while another consumer held the gate (status %q); the notification should be parked", event.payload.InventoryStatus)
	case <-time.After(200 * time.Millisecond):
	}

	// The other consumer releases its slots; the dispatcher's backstop ticker
	// must deliver the parked notification without any signal from the test.
	for i := 0; i < held; i++ {
		gate.release()
	}
	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceFailed) {
			t.Fatalf("other-consumer push status = %q, want failed", event.payload.InventoryStatus)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no inventory_updated delivered after the other consumer released the gate")
	}
}

// TestDeferredProbeRotationBeforeAdmissionDropped is the point-2a regression:
// a job armed against generation N must be dropped at admission when the
// session has already rotated to generation N+1, so no pending is written and
// no probe runs for a candidate nobody serves.
func TestDeferredProbeRotationBeforeAdmissionDropped(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 902, "virtual://movie/tt-rotate-pre?result=old")
	oldJob := fx.job("virtual://movie/tt-rotate-pre?result=old")
	if !fx.handler.armDeferredProbePending(context.Background(), oldJob) {
		t.Fatal("admission rejected the pre-rotation job as stale")
	}
	// Rotate before the job reaches the queue: the binding generation moves.
	if err := fx.manager.SetVirtualSource(fx.session.ID, "virtual://movie/tt-rotate-pre?result=new", 5); err != nil {
		t.Fatalf("SetVirtualSource rotate: %v", err)
	}

	// A canceled service context drains the lazily-started pool, so nothing
	// can take an admitted job off the queue: an admission would be observable.
	fx.handler.ServiceContext = canceledContext()
	fx.handler.startDeferredProbeWorkers()
	fx.handler.deferredProbeWG.Wait()

	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.handler.enqueueDeferredVirtualProbeV3(context.Background(), oldJob)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("rotation-before-admission blocked the caller")
	}
	select {
	case <-fx.handler.deferredProbeQueue:
		t.Fatal("a probe for the superseded candidate was admitted to the worker pool")
	default:
	}
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != "" {
		t.Fatalf("stale job wrote outcome %q onto the replacement binding; want empty", live.VirtualProbeOutcome)
	}
}

// TestDeferredProbeRotationWhileQueuedSheds is the point-2b regression: a job
// admitted at generation N but still queued when the session rotates to N+1
// must be shed by the worker's pre-probe fence — no probe runs, and the new
// binding's outcome is not overwritten.
func TestDeferredProbeRotationWhileQueuedSheds(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 903, "virtual://movie/tt-rotate-queued?result=old")
	deferred := fx.job("virtual://movie/tt-rotate-queued?result=old")
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the queued job as stale")
	}
	// Rotate after admission but before the worker picks the job up.
	if err := fx.manager.SetVirtualSource(fx.session.ID, "virtual://movie/tt-rotate-queued?result=new", 5); err != nil {
		t.Fatalf("SetVirtualSource rotate: %v", err)
	}

	// Drive the worker entry directly with the live service context so the
	// gate wait cannot flake: the gate is free, so the worker reaches the
	// pre-probe fence.
	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.handler.runDeferredVirtualProbe(deferred)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("queued-job shed did not return promptly")
	}
	select {
	case <-fx.probeStarted:
		t.Fatal("probe ran for a superseded candidate")
	case <-time.After(100 * time.Millisecond):
	}
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != "" {
		t.Fatalf("queued stale job wrote outcome %q onto the replacement binding; want empty", live.VirtualProbeOutcome)
	}
}

// TestDeferredProbeRotationDuringPersistenceFenced is the point-2c regression:
// the probe completes, the persistence write is in flight, the session
// rotates, and the write releases — the terminal outcome CAS must reject the
// stale verdict so the new binding's lifecycle stays empty. The gating is
// strictly ordered by channels: the probe must have started (and been
// released) before the write can start, so waiting on saveStarted cannot race
// the worker's scheduling.
func TestDeferredProbeRotationDuringPersistenceFenced(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 904, "virtual://movie/tt-rotate-persist?result=old")
	deferred := fx.job("virtual://movie/tt-rotate-persist?result=old")
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the persistence job as stale")
	}

	// The saver blocks until the test releases it; saveStarted proves the
	// worker reached the durable write. The probe is released only after the
	// worker goroutine is running, and the probe gate sits before the saver in
	// the worker's call order, so saveStarted implies the probe already ran.
	saveStarted := make(chan struct{})
	releaseSave := make(chan struct{})
	var saveOnce sync.Once
	fx.setSaverOverride(func() (VirtualFileMetadataUpdateResult, error) {
		saveOnce.Do(func() { close(saveStarted) })
		<-releaseSave
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.handler.runDeferredVirtualProbe(deferred)
	}()
	close(fx.releaseProbe)
	select {
	case <-saveStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("durable write was not attempted")
	}
	// Rotate while the write is blocked: the pending mark the admission wrote
	// belongs to the old binding and is cleared by the rotation.
	if err := fx.manager.SetVirtualSource(fx.session.ID, "virtual://movie/tt-rotate-persist?result=new", 5); err != nil {
		t.Fatalf("SetVirtualSource rotate: %v", err)
	}
	close(releaseSave)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish after the write released")
	}
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != "" {
		t.Fatalf("stale probe wrote outcome %q onto the replacement binding; want empty", live.VirtualProbeOutcome)
	}
}

// TestDeferredProbeABAStaleGenerationFenced is the point-2d regression: a
// session that rotates A→B→A yields a different generation for the second A
// binding, so a job armed against the first A must still be treated as stale —
// the worker's pre-probe fence drops it before the remote enumeration and it
// writes nothing.
//
// As with the mid-flight A→B→A case, the pinned invariant is that the superseded
// job runs nothing and writes nothing; it is not that the second A binding stays
// un-enumerated. The drop admits a FRESH replacement for the current A binding
// (see scheduleDroppedProbeReplacement), and that replacement legitimately probes
// and records verified against the second A generation. Only one probe runs — the
// superseded first-A job never reaches the enumeration — and exactly one durable
// write lands, against the current binding.
func TestDeferredProbeABAStaleGenerationFenced(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 905, "virtual://movie/tt-aba?result=a")
	fx.saveAttempt(t)
	conn := fx.registerPush(t)

	var probeMu sync.Mutex
	probeCalls := 0
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		probeMu.Lock()
		probeCalls++
		probeMu.Unlock()
		f.AudioTracks = fx.probedAudio
		f.SubtitleTracks = fx.probedSubs
		f.CodecAudio = "eac3"
		return f, nil
	}

	first := fx.job("virtual://movie/tt-aba?result=a")
	if !fx.handler.armDeferredProbePending(context.Background(), first) {
		t.Fatal("admission rejected the first A job as stale")
	}
	firstGen := first.bindingGeneration
	if err := fx.manager.SetVirtualSource(fx.session.ID, "virtual://movie/tt-aba?result=b", 5); err != nil {
		t.Fatalf("SetVirtualSource B: %v", err)
	}
	if err := fx.manager.SetVirtualSource(fx.session.ID, "virtual://movie/tt-aba?result=a", 5); err != nil {
		t.Fatalf("SetVirtualSource A: %v", err)
	}
	secondGen, err := fx.manager.VirtualSourceGeneration(fx.session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceGeneration: %v", err)
	}
	if secondGen == firstGen {
		t.Fatalf("A→B→A did not change the binding generation: first=%d second=%d", firstGen, secondGen)
	}
	// The first job's retained generation is stale: the worker's pre-probe fence
	// drops it before the remote enumeration and hands the current binding a
	// replacement job with its own lifecycle.
	fx.handler.runDeferredVirtualProbe(first)

	deadline := time.Now().Add(2 * time.Second)
	for {
		live, err := fx.manager.GetSession(fx.session.ID)
		if err == nil && live.VirtualProbeOutcome == playback.VirtualProbeOutcomeVerified {
			break
		}
		if time.Now().After(deadline) {
			got := ""
			if live2, err := fx.manager.GetSession(fx.session.ID); err == nil {
				got = live2.VirtualProbeOutcome
			}
			t.Fatalf("session outcome = %q, want verified from the pre-probe replacement", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The superseded first-A job never wrote: exactly one durable write landed,
	// from the replacement, against the current A generation.
	if calls := fx.saverCallCount(); calls != 1 {
		t.Fatalf("durable evidence writes = %d, want 1 (only the replacement persists; the superseded job writes nothing)", calls)
	}
	probeMu.Lock()
	calls := probeCalls
	probeMu.Unlock()
	if calls != 1 {
		t.Fatalf("probe invocations = %d, want 1 (the superseded first-A job must not probe)", calls)
	}
	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceVerified) {
			t.Fatalf("published inventory status = %q, want verified from the replacement", event.payload.InventoryStatus)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no inventory_updated push for the replacement's verified binding")
	}
}

// TestDeferredProbeABADuringProbingFenced is the mid-flight half of the A→B→A
// regression: the worker has already entered the remote enumeration when the
// binding rotates A→B→A, so the URI-only fence would pass — the second A
// binding re-uses the first's candidate URI. The generation-aware post-probe
// fence must drop the superseded job's result: it must never persist, record an
// outcome, or publish its verdict against the new binding.
//
// The pinned invariant is that a probe may only write against the binding it was
// scheduled for. It is not that a superseded binding must stay un-enumerated: a
// different, still-live session has recovered the dropped probe by admitting a
// FRESH replacement for its current binding (see scheduleDroppedProbeReplacement),
// and that replacement has its own lifecycle and legitimately persists and
// records verified against the current generation. The two probes therefore
// return distinct menus — the superseded job "stale", the replacement "live" — so
// every write and publish can be attributed to exactly one of them. Only the
// replacement's menu may ever land, and the dropped job's own lifecycle stays
// untouched.
func TestDeferredProbeABADuringProbingFenced(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 912, "virtual://movie/tt-aba-mid?result=a")
	fx.saveAttempt(t)
	conn := fx.registerPush(t)

	staleMenu := []models.AudioTrack{{Codec: "stale", Channels: 1, Language: "eng", Default: true}}
	liveMenu := []models.AudioTrack{{Codec: "live", Channels: 2, Language: "eng", Default: true}}
	var probeMu sync.Mutex
	probeCalls := 0
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		probeMu.Lock()
		probeCalls++
		call := probeCalls
		probeMu.Unlock()
		fx.probeOnce.Do(func() { close(fx.probeStarted) })
		<-fx.releaseProbe
		if call == 1 {
			f.AudioTracks = staleMenu
			f.CodecAudio = "stale"
		} else {
			f.AudioTracks = liveMenu
			f.CodecAudio = "live"
		}
		return f, nil
	}

	first := fx.job("virtual://movie/tt-aba-mid?result=a")
	if !fx.handler.armDeferredProbePending(context.Background(), first) {
		t.Fatal("admission rejected the first A job as stale")
	}
	firstGen := first.bindingGeneration

	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.handler.runDeferredVirtualProbe(first)
	}()
	// The worker is inside the probe; rotate A→B→A while it is in flight.
	select {
	case <-fx.probeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("deferred post-commit probe was not scheduled")
	}
	if err := fx.manager.SetVirtualSource(fx.session.ID, "virtual://movie/tt-aba-mid?result=b", 5); err != nil {
		t.Fatalf("SetVirtualSource B: %v", err)
	}
	if err := fx.manager.SetVirtualSource(fx.session.ID, "virtual://movie/tt-aba-mid?result=a", 5); err != nil {
		t.Fatalf("SetVirtualSource A: %v", err)
	}
	currentGen, err := fx.manager.VirtualSourceGeneration(fx.session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceGeneration: %v", err)
	}
	if currentGen == firstGen {
		t.Fatalf("A→B→A did not change the binding generation: first=%d current=%d", firstGen, currentGen)
	}

	close(fx.releaseProbe)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish after the probe released")
	}

	// Wait for the replacement's own fenced lifecycle write to land on the
	// current binding; its row stamp and its session outcome are separate writes.
	deadline := time.Now().Add(2 * time.Second)
	for {
		live, err := fx.manager.GetSession(fx.session.ID)
		if err == nil && live.VirtualProbeOutcome == playback.VirtualProbeOutcomeVerified {
			break
		}
		if time.Now().After(deadline) {
			got := ""
			if live2, err := fx.manager.GetSession(fx.session.ID); err == nil {
				got = live2.VirtualProbeOutcome
			}
			t.Fatalf("session outcome = %q, want verified from the replacement", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The superseded job never wrote: the durable evidence is the replacement's
	// live menu, not the dropped job's stale one.
	inventory := fx.pollInventory(t)
	if len(inventory.AudioTracks) != 1 || inventory.AudioTracks[0].Codec != "live" {
		t.Fatalf("inventory audio = %#v, want the replacement's live menu (the superseded job must not have written)", inventory.AudioTracks)
	}
	if calls := fx.saverCallCount(); calls != 1 {
		t.Fatalf("durable evidence writes = %d, want 1 (only the replacement persists)", calls)
	}
	probeMu.Lock()
	calls := probeCalls
	probeMu.Unlock()
	if calls != 2 {
		t.Fatalf("probe invocations = %d, want 2 (the dropped job and its replacement)", calls)
	}
	// The publish that follows is the replacement's verdict for the current
	// binding, never the superseded job's stale one.
	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceVerified) {
			t.Fatalf("published inventory status = %q, want verified from the replacement", event.payload.InventoryStatus)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no inventory_updated push for the replacement's verified binding")
	}
}

// TestDeferredProbeReplacementAdmittedForMovedBinding is the dropped-probe
// recovery regression: a live session whose binding moves mid-probe loses the
// superseded job's verdict to the fence, and the current binding — which carries
// no probe of its own — must get a FRESH replacement so it is enumerated instead
// of being served its provisional declared inventory until a later start or the
// convergence watchdog. The replacement is a new job with its own lifecycle: it
// writes only against the binding it was admitted for, and the dropped job writes
// nothing. Distinct per-call menus prove which job produced the persisted verdict.
func TestDeferredProbeReplacementAdmittedForMovedBinding(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 913, "virtual://movie/tt-moved?result=a")
	fx.saveAttempt(t)
	conn := fx.registerPush(t)

	staleMenu := []models.AudioTrack{{Codec: "stale", Channels: 1, Language: "eng", Default: true}}
	liveMenu := []models.AudioTrack{{Codec: "live", Channels: 2, Language: "eng", Default: true}}
	var probeMu sync.Mutex
	probeCalls := 0
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		probeMu.Lock()
		probeCalls++
		call := probeCalls
		probeMu.Unlock()
		fx.probeOnce.Do(func() { close(fx.probeStarted) })
		<-fx.releaseProbe
		if call == 1 {
			f.AudioTracks = staleMenu
			f.CodecAudio = "stale"
		} else {
			f.AudioTracks = liveMenu
			f.CodecAudio = "live"
		}
		return f, nil
	}

	first := fx.job("virtual://movie/tt-moved?result=a")
	if !fx.handler.armDeferredProbePending(context.Background(), first) {
		t.Fatal("admission rejected the first job as stale")
	}
	firstGen := first.bindingGeneration

	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.handler.runDeferredVirtualProbe(first)
	}()
	select {
	case <-fx.probeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("deferred post-commit probe was not scheduled")
	}
	// Move the binding mid-probe. Re-binding to the same candidate still advances
	// the generation, superseding the in-flight job while leaving the current
	// binding with no probe of its own.
	if err := fx.manager.SetVirtualSource(fx.session.ID, "virtual://movie/tt-moved?result=a", 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	movedGen, err := fx.manager.VirtualSourceGeneration(fx.session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceGeneration: %v", err)
	}
	if movedGen == firstGen {
		t.Fatalf("re-bind did not advance the generation: first=%d moved=%d", firstGen, movedGen)
	}

	close(fx.releaseProbe)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish after the probe released")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		live, err := fx.manager.GetSession(fx.session.ID)
		if err == nil && live.VirtualProbeOutcome == playback.VirtualProbeOutcomeVerified {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the moved binding never received a replacement probe's verified outcome")
		}
		time.Sleep(5 * time.Millisecond)
	}
	inventory := fx.pollInventory(t)
	if len(inventory.AudioTracks) != 1 || inventory.AudioTracks[0].Codec != "live" {
		t.Fatalf("inventory audio = %#v, want the replacement's live menu (the dropped job must not have written)", inventory.AudioTracks)
	}
	if calls := fx.saverCallCount(); calls != 1 {
		t.Fatalf("durable evidence writes = %d, want 1 (only the replacement persists)", calls)
	}
	probeMu.Lock()
	calls := probeCalls
	probeMu.Unlock()
	if calls != 2 {
		t.Fatalf("probe invocations = %d, want 2 (the dropped job and its replacement)", calls)
	}
	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceVerified) {
			t.Fatalf("published inventory status = %q, want verified from the replacement", event.payload.InventoryStatus)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no inventory_updated push for the replacement's verified binding")
	}
}

// TestDeferredProbeMidFlightDifferentCandidateNoReplacement is the negative half
// of the dropped-probe recovery: when the session rotates mid-probe to a
// DIFFERENT candidate, the dropped job's input (stream URL and headers) describes
// the superseded release, so no replacement may be admitted for the new one. The
// fresh resolve that produced the rotated binding owns its URL and headers; a
// replacement built from the dropped input would probe the wrong bytes and stamp
// them onto the new binding. The dropped job writes nothing, no replacement runs,
// and the rotated binding is left to the lifecycle the rotation itself owns (empty
// outcome here, never a fabricated verified/failed verdict).
func TestDeferredProbeMidFlightDifferentCandidateNoReplacement(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 915, "virtual://movie/tt-mid-other?result=a")
	fx.saveAttempt(t)

	var probeMu sync.Mutex
	probeCalls := 0
	probeTwo := make(chan struct{})
	var twoOnce sync.Once
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		probeMu.Lock()
		probeCalls++
		call := probeCalls
		probeMu.Unlock()
		if call == 2 {
			twoOnce.Do(func() { close(probeTwo) })
		}
		fx.probeOnce.Do(func() { close(fx.probeStarted) })
		<-fx.releaseProbe
		f.AudioTracks = fx.probedAudio
		f.CodecAudio = "eac3"
		return f, nil
	}

	first := fx.job("virtual://movie/tt-mid-other?result=a")
	if !fx.handler.armDeferredProbePending(context.Background(), first) {
		t.Fatal("admission rejected the first job as stale")
	}
	firstGen := first.bindingGeneration

	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.handler.runDeferredVirtualProbe(first)
	}()
	select {
	case <-fx.probeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("deferred post-commit probe was not scheduled")
	}
	// Rotate to a genuinely different candidate while the probe is in flight.
	if err := fx.manager.SetVirtualSource(fx.session.ID, "virtual://movie/tt-mid-other?result=b", 5); err != nil {
		t.Fatalf("SetVirtualSource B: %v", err)
	}
	rotatedGen, err := fx.manager.VirtualSourceGeneration(fx.session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceGeneration: %v", err)
	}
	if rotatedGen == firstGen {
		t.Fatalf("rotation did not advance the generation: first=%d rotated=%d", firstGen, rotatedGen)
	}

	close(fx.releaseProbe)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish after the probe released")
	}

	// A replacement, if wrongly admitted, would run a second probe shortly after
	// the first returns. Give it that window before asserting the refusal.
	select {
	case <-probeTwo:
		t.Fatal("a replacement probe ran for a different candidate URI")
	case <-time.After(200 * time.Millisecond):
	}

	if calls := fx.saverCallCount(); calls != 0 {
		t.Fatalf("durable evidence writes = %d, want 0 (the dropped job and any replacement must not write)", calls)
	}
	probeMu.Lock()
	calls := probeCalls
	probeMu.Unlock()
	if calls != 1 {
		t.Fatalf("probe invocations = %d, want 1 (no replacement for a different candidate)", calls)
	}
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != "" {
		t.Fatalf("session outcome = %q, want empty (no lifecycle admitted for the rotated candidate)", live.VirtualProbeOutcome)
	}
}

// TestDeferredProbeMidFlightSameURIDifferentOwnerNoReplacement covers the owner
// half of the replacement fence: the candidate URI is unchanged, but the session
// is now bound to a different owner installation. The dropped job's probe input
// (stream URL and headers) was resolved for the old owner, so a replacement built
// from it would probe another source's bytes and stamp them onto the new binding.
// It must be refused: no second probe, no durable write, and no fabricated
// outcome on the rotated binding.
func TestDeferredProbeMidFlightSameURIDifferentOwnerNoReplacement(t *testing.T) {
	const uri = "virtual://movie/tt-mid-owner?result=a"
	fx := newDeferredLifecycleFixture(t, 916, uri)
	fx.saveAttempt(t)

	var probeMu sync.Mutex
	probeCalls := 0
	probeTwo := make(chan struct{})
	var twoOnce sync.Once
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		probeMu.Lock()
		probeCalls++
		call := probeCalls
		probeMu.Unlock()
		if call == 2 {
			twoOnce.Do(func() { close(probeTwo) })
		}
		fx.probeOnce.Do(func() { close(fx.probeStarted) })
		<-fx.releaseProbe
		f.AudioTracks = fx.probedAudio
		f.CodecAudio = "eac3"
		return f, nil
	}

	first := fx.job(uri)
	if !fx.handler.armDeferredProbePending(context.Background(), first) {
		t.Fatal("admission rejected the first job as stale")
	}
	firstGen := first.bindingGeneration

	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.handler.runDeferredVirtualProbe(first)
	}()
	select {
	case <-fx.probeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("deferred post-commit probe was not scheduled")
	}
	// Same candidate, different owner installation: the generation advances and
	// the owner moves, so the dropped input no longer describes the binding.
	if err := fx.manager.SetVirtualSource(fx.session.ID, uri, 9); err != nil {
		t.Fatalf("SetVirtualSource owner 9: %v", err)
	}
	moved, err := fx.manager.VirtualSourceBinding(fx.session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceBinding: %v", err)
	}
	if moved.Generation == firstGen || moved.OwnerInstallationID != 9 {
		t.Fatalf("owner move binding = %#v, want a new generation and owner 9", moved)
	}

	close(fx.releaseProbe)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish after the probe released")
	}
	select {
	case <-probeTwo:
		t.Fatal("a replacement probe ran after a same-URI owner move")
	case <-time.After(200 * time.Millisecond):
	}
	if calls := fx.saverCallCount(); calls != 0 {
		t.Fatalf("durable evidence writes = %d, want 0 (no replacement for a different owner)", calls)
	}
	probeMu.Lock()
	calls := probeCalls
	probeMu.Unlock()
	if calls != 1 {
		t.Fatalf("probe invocations = %d, want 1 (no replacement after an owner move)", calls)
	}
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != "" {
		t.Fatalf("session outcome = %q, want empty (no lifecycle admitted for the different owner)", live.VirtualProbeOutcome)
	}
}

// TestDeferredProbeQueuedBindingMovedBeforeProbeAdmitsReplacement covers the
// pre-probe shed path: a job that was armed and then queued behind the pool finds
// the binding moved when it finally reaches the pre-probe fence, so it is dropped
// without writing. The current binding carries no probe of its own (the move
// cleared the outcome), so the shed must hand it a fresh replacement exactly as the
// post-probe fence does; otherwise the session serves its provisional declared
// inventory until a later start or the convergence watchdog. The re-bind keeps the
// same candidate and owner (a generation-only move), so the replacement may reuse
// the dropped input and its own lifecycle must drive the session to verified.
func TestDeferredProbeQueuedBindingMovedBeforeProbeAdmitsReplacement(t *testing.T) {
	const uri = "virtual://movie/tt-queued-moved?result=a"
	fx := newDeferredLifecycleFixture(t, 917, uri)
	fx.saveAttempt(t)
	conn := fx.registerPush(t)

	var probeMu sync.Mutex
	probeCalls := 0
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		probeMu.Lock()
		probeCalls++
		probeMu.Unlock()
		f.AudioTracks = fx.probedAudio
		f.SubtitleTracks = fx.probedSubs
		f.CodecAudio = "eac3"
		return f, nil
	}

	// Arm the job, then move the binding (same candidate and owner) before the
	// job reaches its pre-probe fence; the move clears the pending mark.
	deferred := fx.job(uri)
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the queued job as stale")
	}
	firstGen := deferred.bindingGeneration
	if err := fx.manager.SetVirtualSource(fx.session.ID, uri, 5); err != nil {
		t.Fatalf("SetVirtualSource re-bind: %v", err)
	}
	movedGen, err := fx.manager.VirtualSourceGeneration(fx.session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceGeneration: %v", err)
	}
	if movedGen == firstGen {
		t.Fatalf("re-bind did not advance the generation: first=%d moved=%d", firstGen, movedGen)
	}

	// The worker reaches the pre-probe fence, sheds the superseded job, and must
	// admit a replacement for the current binding.
	fx.handler.runDeferredVirtualProbe(deferred)

	deadline := time.Now().Add(2 * time.Second)
	for {
		live, err := fx.manager.GetSession(fx.session.ID)
		if err == nil && live.VirtualProbeOutcome == playback.VirtualProbeOutcomeVerified {
			break
		}
		if time.Now().After(deadline) {
			got := ""
			if live2, err := fx.manager.GetSession(fx.session.ID); err == nil {
				got = live2.VirtualProbeOutcome
			}
			t.Fatalf("session outcome = %q, want verified from the pre-probe replacement", got)
		}
		time.Sleep(5 * time.Millisecond)
	}

	inventory := fx.pollInventory(t)
	if len(inventory.AudioTracks) != 2 || inventory.AudioTracks[1].Codec != "eac3" {
		t.Fatalf("inventory audio = %#v, want the replacement's probed menu", inventory.AudioTracks)
	}
	if calls := fx.saverCallCount(); calls != 1 {
		t.Fatalf("durable evidence writes = %d, want 1 (the replacement persists)", calls)
	}
	probeMu.Lock()
	calls := probeCalls
	probeMu.Unlock()
	if calls != 1 {
		t.Fatalf("probe invocations = %d, want 1 (only the pre-probe replacement runs)", calls)
	}
	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceVerified) {
			t.Fatalf("published inventory status = %q, want verified from the replacement", event.payload.InventoryStatus)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no inventory_updated push for the pre-probe replacement's verified binding")
	}
}

// TestDeferredProbeQueuedTeardownNoWork is the point-3 regression: a queued
// job whose session is torn down before the worker picks it up must exit
// without remote work and without writes. The teardown is the manager's
// StopSession — not a service-context cancel — so the worker reaches the
// pre-probe fence on a free gate and the fence rejects the job because the
// binding is unreadable.
func TestDeferredProbeQueuedTeardownNoWork(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 906, "virtual://movie/tt-teardown?result=cand-1")
	deferred := fx.job("virtual://movie/tt-teardown?result=cand-1")
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the teardown job as stale")
	}
	// Tear the session down before the worker runs.
	if err := fx.manager.StopSession(fx.session.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.handler.runDeferredVirtualProbe(deferred)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("teardown job did not return promptly")
	}
	select {
	case <-fx.probeStarted:
		t.Fatal("probe ran for a torn-down session")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestDeferredProbeBlockedPersistenceKeepsPending is the point-4a regression:
// while the durable evidence write is blocked, the inventory poll must still
// report pending — not verified, not failed — so the client keeps waiting
// instead of acting on an unfinished probe.
func TestDeferredProbeBlockedPersistenceKeepsPending(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 907, "virtual://movie/tt-blocked?result=cand-1")
	fx.saveAttempt(t)
	deferred := fx.job("virtual://movie/tt-blocked?result=cand-1")
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the blocked-write job as stale")
	}

	saveStarted := make(chan struct{})
	releaseSave := make(chan struct{})
	var saveOnce sync.Once
	fx.setSaverOverride(func() (VirtualFileMetadataUpdateResult, error) {
		saveOnce.Do(func() { close(saveStarted) })
		<-releaseSave
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.handler.runDeferredVirtualProbe(deferred)
	}()
	close(fx.releaseProbe)
	select {
	case <-saveStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("durable write was not attempted")
	}

	// The write is blocked, so the poll must still report pending.
	inventory := fx.pollInventory(t)
	if inventory.InventoryStatus != string(ProbeProvenancePending) {
		t.Fatalf("blocked-write poll status = %q, want pending", inventory.InventoryStatus)
	}
	close(releaseSave)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish after the write released")
	}
}

// TestDeferredProbePersistenceErrorEndsLoading is the point-4b regression: a
// persistence error is a terminal failed outcome, visible on both the push and
// the poll, so the client leaves loading instead of waiting forever.
func TestDeferredProbePersistenceErrorEndsLoading(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 908, "virtual://movie/tt-writeerr?result=cand-1")
	fx.saveAttempt(t)
	deferred := fx.job("virtual://movie/tt-writeerr?result=cand-1")
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the write-error job as stale")
	}
	fx.setSaverOverride(func() (VirtualFileMetadataUpdateResult, error) {
		return VirtualFileMetadataUpdateResult{}, errors.New("catalog unavailable")
	})
	conn := fx.registerPush(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.handler.runDeferredVirtualProbe(deferred)
	}()
	close(fx.releaseProbe)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish after the write error")
	}

	// Push surface: the failure must be delivered so a push-driven client
	// exits the loading state.
	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceFailed) {
			t.Fatalf("pushed inventory status = %q, want failed", event.payload.InventoryStatus)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no inventory_updated failure push delivered after the write error")
	}

	// Poll surface: a client that never holds the push connection must observe
	// the same terminal status.
	fx.pollInventoryStatus(t, string(ProbeProvenanceFailed))
}

// TestDeferredProbeSuccessPublishesProbedTracks is the point-4c regression:
// after the durable write commits, both the push and the poll must show the
// actual enumerated tracks — the probed audio/subtitle menu — not the
// declared/provisional one. The assertions check the concrete menu fields
// (codec, channels, language, default), because a count-only check would pass
// against a menu with the wrong tracks.
func TestDeferredProbeSuccessPublishesProbedTracks(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 909, "virtual://movie/tt-success?result=cand-1")
	fx.saveAttempt(t)
	deferred := fx.job("virtual://movie/tt-success?result=cand-1")
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the success job as stale")
	}
	conn := fx.registerPush(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.handler.runDeferredVirtualProbe(deferred)
	}()
	close(fx.releaseProbe)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish after the write committed")
	}

	// Push surface: the delivered menu must be the probed one.
	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceVerified) {
			t.Fatalf("pushed inventory status = %q, want verified", event.payload.InventoryStatus)
		}
		assertProbedMenu(t, "push", event.payload.AudioTracks, event.payload.SubtitleInventory)
	case <-time.After(2 * time.Second):
		t.Fatal("no inventory_updated push delivered after the probe succeeded")
	}

	// Poll surface: the same probed menu must be readable from the inventory
	// endpoint.
	inventory := fx.pollInventoryStatus(t, string(ProbeProvenanceVerified))
	assertProbedMenu(t, "poll", inventory.AudioTracks, inventory.SubtitleInventory)
}

// TestDeferredProbePublishBackpressureSkipsPush is the point-5 regression: a
// shed whose publish cannot take a gate slot must still return promptly with
// the failed outcome synchronously stored; the notification is parked for the
// next gate release rather than blocking the shed caller.
func TestDeferredProbePublishBackpressureSkipsPush(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 910, "virtual://movie/tt-publish-gate?result=cand-1")
	release := holdDetachedGate(t, fx.handler)
	defer release()

	deferred := fx.job("virtual://movie/tt-publish-gate?result=cand-1")
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the publish-gate job as stale")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.handler.shedDeferredVirtualProbe(deferred)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shed blocked on the saturated publish gate")
	}
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != probeOutcomeFailed {
		t.Fatalf("publish-gate outcome = %q, want failed", live.VirtualProbeOutcome)
	}
	// The publish parked behind the full gate: the pending set carries it.
	fx.handler.deferredPublishMu.Lock()
	parked := len(fx.handler.deferredPublishPending)
	fx.handler.deferredPublishMu.Unlock()
	if parked != 1 {
		t.Fatalf("parked publish entries = %d, want the shed session's notification parked", parked)
	}
}

// TestDeferredProbeFailedPushScopedToNegotiatedSessions is the point-2
// delivery-side guard: a terminal "failed" inventory is the deferred
// lifecycle's signal, meaningful only to a session whose attempt negotiated
// deferred_track_inventory_v1. A session that never negotiated it must not
// receive the failed push (it was never promised a provisional menu), while a
// negotiated session still does. The attempt record is the discriminator, so
// the test drives both sides of it through the real publish path.
func TestDeferredProbeFailedPushScopedToNegotiatedSessions(t *testing.T) {
	// Unnegotiated session: the attempt row carries neither the client token
	// nor the server feature, so a failed inventory is not for it.
	plain := newDeferredLifecycleFixture(t, 930, "virtual://movie/tt-guard-plain?result=cand-1")
	plain.saveAttemptWithoutNegotiation(t)
	plainConn := plain.registerPush(t)
	if err := plain.manager.SetVirtualProbeOutcome(plain.session.ID, probeOutcomeFailed); err != nil {
		t.Fatalf("SetVirtualProbeOutcome: %v", err)
	}
	plain.handler.PublishInventoryUpdatedToSession(context.Background(), plain.session.ID, 0)
	select {
	case event := <-plainConn.ch:
		t.Fatalf("unnegotiated session received a failed inventory push (status %q)", event.payload.InventoryStatus)
	case <-time.After(300 * time.Millisecond):
	}

	// Negotiated session: the attempt row carries both sides, so the failed
	// push is delivered and the client can leave loading.
	negotiated := newDeferredLifecycleFixture(t, 931, "virtual://movie/tt-guard-negotiated?result=cand-1")
	negotiated.saveAttempt(t)
	negotiatedConn := negotiated.registerPush(t)
	if err := negotiated.manager.SetVirtualProbeOutcome(negotiated.session.ID, probeOutcomeFailed); err != nil {
		t.Fatalf("SetVirtualProbeOutcome: %v", err)
	}
	negotiated.handler.PublishInventoryUpdatedToSession(context.Background(), negotiated.session.ID, 0)
	select {
	case event := <-negotiatedConn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceFailed) {
			t.Fatalf("negotiated session failed push status = %q, want failed", event.payload.InventoryStatus)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("negotiated session did not receive the failed inventory push")
	}
}

// failingGetAttemptPlanStoreV3 models a plan-store outage on the publish path:
// every attempt read fails, so publishInventoryUpdatedToSession cannot load the
// record that carries the deferred negotiation and builds its event with a nil
// record.
type failingGetAttemptPlanStoreV3 struct {
	playback.PlanStoreV3
	err error
}

func (s *failingGetAttemptPlanStoreV3) GetAttempt(context.Context, string) (*playback.AttemptRecordV3, error) {
	return nil, s.err
}

// TestDeferredProbeFailedPushFailsClosedOnStoreError pins the nil-record arm of
// the delivery-side guard. When the attempt row cannot be read, the negotiation
// cannot be established, so a terminal "failed" inventory is suppressed rather
// than delivered to a client that may never have negotiated the lifecycle. The
// suppression is delivery-side only: once the store recovers, the session's own
// poll still reports the terminal status, which is what leaves the client's
// loading state.
func TestDeferredProbeFailedPushFailsClosedOnStoreError(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 932, "virtual://movie/tt-guard-store-error?result=cand-1")
	fx.saveAttempt(t)
	conn := fx.registerPush(t)
	if err := fx.manager.SetVirtualProbeOutcome(fx.session.ID, probeOutcomeFailed); err != nil {
		t.Fatalf("SetVirtualProbeOutcome: %v", err)
	}

	healthy := fx.handler.PlanStoreV3
	fx.handler.PlanStoreV3 = &failingGetAttemptPlanStoreV3{PlanStoreV3: healthy, err: errors.New("plan store unavailable")}
	t.Cleanup(func() { fx.handler.PlanStoreV3 = healthy })
	fx.handler.PublishInventoryUpdatedToSession(context.Background(), fx.session.ID, 0)
	select {
	case event := <-conn.ch:
		t.Fatalf("store-error publish delivered a failed inventory push (status %q); the guard must fail closed", event.payload.InventoryStatus)
	case <-time.After(300 * time.Millisecond):
	}

	// The failed push is suppressed, not the terminal state: with the store
	// recovered the poll carries it.
	fx.handler.PlanStoreV3 = healthy
	fx.pollInventoryStatus(t, string(ProbeProvenanceFailed))
}

// idKeyedPlaybackFileResolver resolves rows by id and reports an error for any
// id it does not hold, modeling a catalog read that cannot produce the row a
// binding now points at.
type idKeyedPlaybackFileResolver struct {
	rows map[int]*models.MediaFile
}

func (r idKeyedPlaybackFileResolver) GetByID(_ context.Context, id int) (*models.MediaFile, error) {
	if row, ok := r.rows[id]; ok && row != nil {
		return row, nil
	}
	return nil, errors.New("catalog row not found")
}

// TestDeferredProbeReplacementShedsWhenEffectiveRowUnresolved is the stale-row
// regression: when the replacement's current effective row cannot be resolved,
// the dropped job's superseded row must NOT be used as a fallback. The
// replacement fences against the new binding, so writing through the old file
// would stamp evidence onto a row the session no longer serves while leaving the
// served row un-stamped. The replacement is shed instead: no probe runs, no
// evidence is written, and the session never reports a false verified status.
func TestDeferredProbeReplacementShedsWhenEffectiveRowUnresolved(t *testing.T) {
	const uri = "virtual://movie/tt-shed-unresolved?result=a"
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 700, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, uri, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	// A row-only move leaves the generation and candidate unchanged but points
	// the session at a row the resolver cannot produce.
	if err := manager.SetEffectiveMediaFileID(session.ID, 701); err != nil {
		t.Fatalf("SetEffectiveMediaFileID: %v", err)
	}

	rowA := &models.MediaFile{
		ID: 700, ContentID: "movie-shed", FilePath: uri, VirtualOwnerInstallationID: 5,
		AudioTracks: []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng", Default: true}},
	}
	handler := NewPlaybackHandler(manager, idKeyedPlaybackFileResolver{rows: map[int]*models.MediaFile{700: rowA}})
	handler.RealtimeHub = playback.NewRealtimeHub()
	handler.InstallationID = "test-install"
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	handler.ServiceContext = serviceCtx
	t.Cleanup(serviceCancel)

	var mu sync.Mutex
	probeCalls, saverCalls := 0, 0
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		mu.Lock()
		probeCalls++
		mu.Unlock()
		return f, nil
	}
	handler.VirtualFileMetadataSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		mu.Lock()
		saverCalls++
		mu.Unlock()
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}

	dropped := &virtualDeferredProbeV3{
		stickyKey:      "sticky-shed",
		file:           &models.MediaFile{ID: 700},
		streamURL:      "http://provider.example/stream",
		probeTransient: &models.MediaFile{ID: 700},
		cand:           VirtualPlaybackStream{ID: "cand-1", URI: uri},
		ownerID:        5,
		sessionID:      session.ID,
	}
	handler.scheduleDroppedProbeReplacement(context.Background(), dropped)

	mu.Lock()
	gotProbes, gotSavers := probeCalls, saverCalls
	mu.Unlock()
	if gotProbes != 0 || gotSavers != 0 {
		t.Fatalf("probe/saver calls = %d/%d, want 0/0: an unresolved effective row must not be probed or stamped", gotProbes, gotSavers)
	}
	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome == probeOutcomeVerified {
		t.Fatal("outcome = verified on an unresolved effective row; want no false verified status")
	}
	if live.VirtualProbeOutcome != probeOutcomeFailed {
		t.Fatalf("outcome = %q, want the shed terminal failure", live.VirtualProbeOutcome)
	}
}

// rowMoveAfterBindingReadManager reproduces the admission interleaving where
// SetEffectiveMediaFileID moves the effective row between the admission binding
// read and the pending arm, without advancing the generation. The read returns
// the pre-move binding, so admission must record THAT binding as the pending
// mark's owner rather than the row the move left behind.
type rowMoveAfterBindingReadManager struct {
	*playback.SessionManager
	moveTo int
	once   sync.Once
}

func (m *rowMoveAfterBindingReadManager) VirtualSourceBinding(sessionID string) (playback.VirtualSourceBindingSnapshot, error) {
	binding, err := m.SessionManager.VirtualSourceBinding(sessionID)
	m.once.Do(func() { _ = m.SetEffectiveMediaFileID(sessionID, m.moveTo) })
	return binding, err
}

// TestDeferredProbeAdmissionRecordsScheduledBindingAcrossRowMove is the
// unowned-pending regression: a row-only move between the admission read and the
// arm must not leave the pending mark looking live for a row no worker will land
// on. Recording the scheduled binding keeps the mark stale for the moved row, so
// recovery can arm the row the session now serves; recording the moved row would
// refuse the replacement and strand the client until the convergence watchdog.
func TestDeferredProbeAdmissionRecordsScheduledBindingAcrossRowMove(t *testing.T) {
	const uri = "virtual://movie/tt-admit-row-move?result=a"
	fx := newDeferredLifecycleFixture(t, 730, uri)
	barrier := &rowMoveAfterBindingReadManager{SessionManager: fx.manager, moveTo: 731}
	fx.handler.sessionMgr = barrier

	deferred := fx.job(uri)
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the pre-move job as stale")
	}
	if deferred.binding.EffectiveMediaFileID != 730 {
		t.Fatalf("admitted binding row = %d, want the scheduled row 730", deferred.binding.EffectiveMediaFileID)
	}
	// The moved row carries no live lifecycle of its own, so recovery must be able
	// to own it. A pending mark recorded against the moved row would look live and
	// refuse this arm.
	binding, applied, err := fx.manager.ArmVirtualProbeReplacementForCurrentBinding(fx.session.ID, uri, 5)
	if err != nil || !applied {
		t.Fatalf("recovery arm for the moved row = (_, %v, %v), want applied", applied, err)
	}
	if binding.EffectiveMediaFileID != 731 {
		t.Fatalf("replacement armed row %d, want the moved row 731", binding.EffectiveMediaFileID)
	}
}

// TestDeferredProbeRetryFoldKeepsServedReplacementOverStalePark is the
// equal-generation retry-set barrier: the retry set is keyed by the full binding
// (session, candidate, owner, effective row), so a stale job for the row a
// row-only move superseded and a legitimate replacement for the row the session
// now serves are distinct entries. A row-only move does not advance the binding
// generation, so keying by session and candidate alone would let one evict the
// other; neither may be discarded, because the session can return to either row.
func TestDeferredProbeRetryFoldKeepsServedReplacementOverStalePark(t *testing.T) {
	const uri = "virtual://movie/tt-fold-equal-gen?result=a"
	fx := newDeferredLifecycleFixture(t, 730, uri)
	// A row-only move points the session at row 731 at the unchanged generation.
	if err := fx.manager.SetEffectiveMediaFileID(fx.session.ID, 731); err != nil {
		t.Fatalf("SetEffectiveMediaFileID: %v", err)
	}
	binding, err := fx.manager.VirtualSourceBinding(fx.session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceBinding: %v", err)
	}
	if binding.Generation == 0 || binding.EffectiveMediaFileID != 731 {
		t.Fatalf("binding = %#v, want the moved row 731 at a nonzero generation", binding)
	}

	// served is the legitimate replacement armed for the row the session serves;
	// stale is the shed job for the row the move superseded. Both carry the same
	// generation but a different effective row, so they key apart.
	served := &virtualDeferredProbeV3{
		sessionID:         fx.session.ID,
		file:              &models.MediaFile{ID: binding.EffectiveMediaFileID},
		cand:              VirtualPlaybackStream{ID: "cand-1", URI: uri},
		ownerID:           binding.OwnerInstallationID,
		bindingGeneration: binding.Generation,
		binding:           binding,
	}
	staleBinding := binding
	staleBinding.EffectiveMediaFileID = 730
	stale := *served
	stale.file = &models.MediaFile{ID: 730}
	stale.binding = staleBinding
	if stale.bindingGeneration != served.bindingGeneration {
		t.Fatalf("test setup: generations differ (%d vs %d); the barrier is about equal generations",
			stale.bindingGeneration, served.bindingGeneration)
	}
	if deferredProbeKey(served) == deferredProbeKey(&stale) {
		t.Fatal("test setup: the two rows share a retry key; the key must include the effective row")
	}

	// Both jobs must survive in either arrival order: the served replacement is
	// reachable under its own key, and the stale job is not discarded either.
	fx.handler.deferredProbeRetry = make(map[string]*virtualDeferredProbeV3)
	fx.handler.deferredProbeMu.Lock()
	fx.handler.parkDeferredProbeRetryLocked(served)
	fx.handler.parkDeferredProbeRetryLocked(&stale)
	keptServed := fx.handler.deferredProbeRetry[deferredProbeKey(served)]
	keptStale := fx.handler.deferredProbeRetry[deferredProbeKey(&stale)]
	fx.handler.deferredProbeMu.Unlock()
	if keptServed != served {
		t.Fatalf("retry set lost the replacement bound to row %d", binding.EffectiveMediaFileID)
	}
	if keptStale != &stale {
		t.Fatal("retry set discarded the job for the superseded row; the session can return to it")
	}

	// The reverse arrival order must keep both entries too.
	fx.handler.deferredProbeRetry = make(map[string]*virtualDeferredProbeV3)
	fx.handler.deferredProbeMu.Lock()
	fx.handler.parkDeferredProbeRetryLocked(&stale)
	fx.handler.parkDeferredProbeRetryLocked(served)
	keptServed = fx.handler.deferredProbeRetry[deferredProbeKey(served)]
	keptStale = fx.handler.deferredProbeRetry[deferredProbeKey(&stale)]
	fx.handler.deferredProbeMu.Unlock()
	if keptServed != served || keptStale != &stale {
		t.Fatal("retry set dropped a job when the served replacement arrived after the stale park")
	}
}

// TestDeferredProbeAdmissionDropsOwnerMovedBeforeCapture is the admission input
// re-check: the job's stream URL and headers were resolved for one owner
// installation, so a same-candidate move to a different owner before the binding
// is captured must drop it. Probing the old installation's source and stamping its
// verdict onto the new owner's binding is exactly what the candidate-URI-only
// check could not see.
func TestDeferredProbeAdmissionDropsOwnerMovedBeforeCapture(t *testing.T) {
	const uri = "virtual://movie/tt-admit-owner-move?result=a"
	fx := newDeferredLifecycleFixture(t, 735, uri)

	deferred := fx.job(uri)
	// The fixture's session is bound to owner 5; the job describes owner 9.
	deferred.ownerID = 9
	if fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission accepted a job resolved for a different owner installation")
	}
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != "" {
		t.Fatalf("outcome = %q, want empty: a dropped job must not leave a pending mark", live.VirtualProbeOutcome)
	}
}

// TestDeferredProbeRowMoveDuringPersistenceAdmitsReplacement is the
// move-after-the-post-probe-check regression for the failed-terminal-CAS window:
// a row-only move lands while the durable write is blocked, so the write commits
// the old row and the terminal CAS against the admitted binding fails. The moved
// row must get its own replacement probe instead of being served un-enumerated.
func TestDeferredProbeRowMoveDuringPersistenceAdmitsReplacement(t *testing.T) {
	const uri = "virtual://movie/tt-row-move-persist?result=a"
	fx := newDeferredLifecycleFixture(t, 740, uri)
	fx.saveAttempt(t)
	fx.handler.fileResolver = idKeyedPlaybackFileResolver{rows: map[int]*models.MediaFile{
		740: {ID: 740, ContentID: "movie-row-move", FilePath: uri, VirtualOwnerInstallationID: 5},
		741: {ID: 741, ContentID: "movie-row-move", FilePath: uri, VirtualOwnerInstallationID: 5},
	}}

	var probeMu sync.Mutex
	probeCalls := 0
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		probeMu.Lock()
		probeCalls++
		probeMu.Unlock()
		f.AudioTracks = fx.probedAudio
		f.SubtitleTracks = fx.probedSubs
		f.CodecAudio = "eac3"
		return f, nil
	}

	saveStarted := make(chan struct{})
	releaseSave := make(chan struct{})
	var saveOnce sync.Once
	fx.setSaverOverride(func() (VirtualFileMetadataUpdateResult, error) {
		saveOnce.Do(func() { close(saveStarted) })
		<-releaseSave
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	})

	deferred := fx.job(uri)
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the persistence job as stale")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.handler.runDeferredVirtualProbe(deferred)
	}()
	close(fx.releaseProbe)
	select {
	case <-saveStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("durable write was not attempted")
	}
	// Row-only move while the durable write is blocked: the post-probe fence has
	// already passed (generation and candidate unchanged), so the committed row is
	// the old one and the terminal CAS against the admitted binding fails.
	if err := fx.manager.SetEffectiveMediaFileID(fx.session.ID, 741); err != nil {
		t.Fatalf("SetEffectiveMediaFileID: %v", err)
	}
	close(releaseSave)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish after the write released")
	}
	// The moved row must receive a replacement probe. Without recovery the
	// superseded job writes nothing to the moved row and only one probe ever runs.
	waitDeferredCondition(t, func() bool {
		probeMu.Lock()
		defer probeMu.Unlock()
		return probeCalls >= 2
	}, "replacement probe for the row moved during persistence")
}

// bindingMoveDuringReconcileManager lands a same-candidate generation move on the
// reconciliation step's session lookup, modeling a binding that moves after the
// durable write commits but before the verdict is propagated.
type bindingMoveDuringReconcileManager struct {
	*playback.SessionManager
	move func()
	once sync.Once
}

func (m *bindingMoveDuringReconcileManager) GetSessionsByMediaFileID(fileID int) []*playback.Session {
	m.once.Do(func() {
		if m.move != nil {
			m.move()
		}
	})
	return m.SessionManager.GetSessionsByMediaFileID(fileID)
}

// TestDeferredProbeBindingMoveDuringReconciliationAdmitsReplacement is the
// move-after-the-post-probe-check regression for the superseded window: the
// binding moves after the durable write commits, so the propagation CAS rejects
// the verdict as superseded. The moved generation must still get its own
// replacement probe rather than being left un-enumerated.
func TestDeferredProbeBindingMoveDuringReconciliationAdmitsReplacement(t *testing.T) {
	const uri = "virtual://movie/tt-move-reconcile?result=a"
	fx := newDeferredLifecycleFixture(t, 750, uri)
	fx.saveAttempt(t)

	var probeMu sync.Mutex
	probeCalls := 0
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		probeMu.Lock()
		probeCalls++
		probeMu.Unlock()
		f.AudioTracks = fx.probedAudio
		f.SubtitleTracks = fx.probedSubs
		f.CodecAudio = "eac3"
		return f, nil
	}
	// Same candidate, new generation: the committed verdict passes the
	// post-probe fence but is then superseded by the terminal CAS.
	fx.handler.sessionMgr = &bindingMoveDuringReconcileManager{
		SessionManager: fx.manager,
		move:           func() { _ = fx.manager.SetVirtualSource(fx.session.ID, uri, 5) },
	}

	deferred := fx.job(uri)
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the reconciliation job as stale")
	}
	close(fx.releaseProbe)
	fx.handler.runDeferredVirtualProbe(deferred)

	waitDeferredCondition(t, func() bool {
		probeMu.Lock()
		defer probeMu.Unlock()
		return probeCalls >= 2
	}, "replacement probe for the binding moved during reconciliation")
}

// moveOnLookupThenFailResolver resolves rows by id, except that the first lookup
// of moveFrom lands a row-only binding move to moveTo and then reports no row. It
// models the replacement's effective-row lookup racing a move that leaves the
// lookup with nothing to return — the exact window in which a rejected shed used
// to strand the moved binding.
type moveOnLookupThenFailResolver struct {
	rows     map[int]*models.MediaFile
	moveFrom int
	move     func()
	once     sync.Once
}

func (r *moveOnLookupThenFailResolver) GetByID(_ context.Context, id int) (*models.MediaFile, error) {
	if id == r.moveFrom {
		r.once.Do(func() {
			if r.move != nil {
				r.move()
			}
		})
		return nil, errors.New("catalog row not found")
	}
	if row, ok := r.rows[id]; ok && row != nil {
		return row, nil
	}
	return nil, errors.New("catalog row not found")
}

// TestDeferredProbeShedRowLookupRecoversMovedBinding is the rejected-shed
// row-move regression: the replacement arms the session's effective row B, its
// lookup of B lands a row-only move to C and then fails, so the replacement is
// shed. The shed's terminal CAS is fenced on B and is rejected. Recovery must
// hand the stale job back to the bounded retry queue instead of returning: a
// worker drops it at the full-binding fence and admits a fresh probe for the row
// the session now serves (C), so evidence is written only onto C. Without the
// hand-off C kept a stale pending mark until the convergence watchdog; without
// the full-binding fence the stale job would have probed and persisted through B.
func TestDeferredProbeShedRowLookupRecoversMovedBinding(t *testing.T) {
	const uri = "virtual://movie/tt-shed-row-lookup?result=a"
	fx := newDeferredLifecycleFixture(t, 860, uri)
	fx.saveAttempt(t)

	resolver := &moveOnLookupThenFailResolver{
		moveFrom: 860,
		move: func() {
			if err := fx.manager.SetEffectiveMediaFileID(fx.session.ID, 861); err != nil {
				t.Errorf("SetEffectiveMediaFileID: %v", err)
			}
		},
		rows: map[int]*models.MediaFile{
			861: {
				ID: 861, ContentID: "movie-shed-row-lookup", FilePath: uri, VirtualOwnerInstallationID: 5,
				AudioTracks: []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng", Default: true}},
			},
		},
	}
	fx.handler.fileResolver = resolver

	var mu sync.Mutex
	probeCalls := 0
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		mu.Lock()
		probeCalls++
		mu.Unlock()
		f.AudioTracks = fx.probedAudio
		f.SubtitleTracks = fx.probedSubs
		f.CodecAudio = "eac3"
		return f, nil
	}
	var saveIDs []int
	fx.handler.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		mu.Lock()
		saveIDs = append(saveIDs, args.FileID)
		mu.Unlock()
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}

	// The dropped job's input describes a superseded binding; the session serves
	// row 860 and the lookup of it is what lands the move to 861 and then fails.
	dropped := &virtualDeferredProbeV3{
		stickyKey:      "sticky-shed-row-lookup",
		file:           &models.MediaFile{ID: 700},
		streamURL:      "http://provider.example/stream",
		probeTransient: &models.MediaFile{ID: 700},
		cand:           VirtualPlaybackStream{ID: "cand-1", URI: uri},
		ownerID:        5,
		sessionID:      fx.session.ID,
	}
	fx.handler.scheduleDroppedProbeReplacement(context.Background(), dropped)

	// Recovery must probe and persist exactly the moved row, once, and never the
	// superseded row the shed was armed against.
	waitDeferredCondition(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(saveIDs) == 1
	}, "evidence write onto the moved binding's row")
	mu.Lock()
	gotSaves := append([]int(nil), saveIDs...)
	gotProbes := probeCalls
	mu.Unlock()
	if len(gotSaves) != 1 || gotSaves[0] != 861 {
		t.Fatalf("durable evidence writes = %v, want exactly [861] (the moved row, never the superseded row)", gotSaves)
	}
	if gotProbes != 1 {
		t.Fatalf("probe invocations = %d, want 1 (the superseded job must be dropped by the fence, never probed)", gotProbes)
	}
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.MediaFileID != 861 {
		t.Fatalf("effective row = %d, want the moved row 861", live.MediaFileID)
	}
	if live.VirtualProbeOutcome != probeOutcomeVerified {
		t.Fatalf("outcome = %q, want verified on the moved binding", live.VirtualProbeOutcome)
	}
}

// TestDeferredProbeSaturatedAdmissionRecoversMovedBinding is the saturated-shed
// half: admission overflows a full queue and a full retry set, and a row-only
// move between the arm and the shed makes the shed's terminal CAS reject. With
// the bounded retry queue already saturated there is nowhere to park a recovery
// job, so the current binding is failed directly instead of being left pending
// until the convergence watchdog. No probe runs, so no evidence is written onto
// the superseded row, and the bounded retry set is not grown.
func TestDeferredProbeSaturatedAdmissionRecoversMovedBinding(t *testing.T) {
	const uri = "virtual://movie/tt-shed-saturated?result=a"
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 850, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, uri, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	// A row-only move leaves the generation and candidate unchanged but points
	// the session at row 851.
	if err := manager.SetEffectiveMediaFileID(session.ID, 851); err != nil {
		t.Fatalf("SetEffectiveMediaFileID: %v", err)
	}

	handler := NewPlaybackHandler(manager, idKeyedPlaybackFileResolver{rows: map[int]*models.MediaFile{
		850: {ID: 850, ContentID: "movie-shed-saturated", FilePath: uri, VirtualOwnerInstallationID: 5},
		851: {ID: 851, ContentID: "movie-shed-saturated", FilePath: uri, VirtualOwnerInstallationID: 5},
		852: {ID: 852, ContentID: "movie-shed-saturated", FilePath: uri, VirtualOwnerInstallationID: 5},
	}})
	handler.RealtimeHub = playback.NewRealtimeHub()
	// A canceled service context lets the lazily-started workers exit at once, so
	// nothing drains the queue or the retry set: the saturation is deterministic.
	handler.ServiceContext = canceledContext()
	handler.startDeferredProbeWorkers()
	handler.deferredProbeWG.Wait()

	probeCalls, saverCalls := 0, 0
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		probeCalls++
		return f, nil
	}
	handler.VirtualFileMetadataSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		saverCalls++
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}

	// Fill the probe queue and the bounded retry set to their bounds.
	for i := 0; i < virtualDeferredProbeQueueSize; i++ {
		handler.deferredProbeQueue <- &virtualDeferredProbeV3{sessionID: fmt.Sprintf("fill-%d", i)}
	}
	handler.deferredProbeMu.Lock()
	for i := 0; i < virtualDeferredProbeQueueSize; i++ {
		handler.deferredProbeRetry[fmt.Sprintf("retry-%d", i)] = &virtualDeferredProbeV3{sessionID: fmt.Sprintf("fill-%d", i)}
	}
	handler.deferredProbeMu.Unlock()

	deferred := &virtualDeferredProbeV3{
		stickyKey:      "sticky-shed-saturated",
		file:           &models.MediaFile{ID: 851},
		streamURL:      "http://provider.example/stream",
		probeTransient: &models.MediaFile{ID: 851},
		cand:           VirtualPlaybackStream{ID: "cand-1", URI: uri},
		ownerID:        5,
		sessionID:      session.ID,
	}
	// Admission arms the pending mark for row 851; the move to 852 lands after,
	// so the shed's CAS is fenced on 851 and rejects.
	if !handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the saturated job as stale")
	}
	if err := manager.SetEffectiveMediaFileID(session.ID, 852); err != nil {
		t.Fatalf("SetEffectiveMediaFileID: %v", err)
	}

	handler.admitArmedDeferredProbe(context.Background(), deferred)

	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != probeOutcomeFailed {
		t.Fatalf("outcome = %q, want the terminal failure that keeps the moved binding from stranding", live.VirtualProbeOutcome)
	}
	if probeCalls != 0 || saverCalls != 0 {
		t.Fatalf("probe/saver calls = %d/%d, want 0/0: a saturated shed must not probe or write onto a superseded row", probeCalls, saverCalls)
	}
	handler.deferredProbeMu.Lock()
	retryLen := len(handler.deferredProbeRetry)
	handler.deferredProbeMu.Unlock()
	if retryLen != virtualDeferredProbeQueueSize {
		t.Fatalf("bounded retry set size = %d, want it unchanged at %d", retryLen, virtualDeferredProbeQueueSize)
	}
}

// TestDeferredProbeRetryFoldKeepsReturnToRowJob is the finding-1 regression: a
// row-only move does not advance the binding generation, so the job parked for
// the row the session left and the job for the row it now serves carry the same
// generation. If the retry set keyed only by session and candidate, one would
// evict the other; when the session returned to the evicted row, its mark (armed
// for that row and still live) would refuse a replacement arm while the only job
// that could land it was gone, leaving convergence to the watchdog. Keying the
// retry set by the full binding keeps the job for every row, so the mark
// converges without the convergence watchdog.
func TestDeferredProbeRetryFoldKeepsReturnToRowJob(t *testing.T) {
	const uri = "virtual://movie/tt-fold-return?result=a"
	fx := newDeferredLifecycleFixture(t, 730, uri)
	// A synchronous prober so the retained job can drive the session to verified
	// once the retry set is drained, without the fixture's gating probe.
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.AudioTracks = fx.probedAudio
		f.SubtitleTracks = fx.probedSubs
		f.CodecAudio = "eac3"
		return f, nil
	}
	fx.handler.deferredProbeRetry = make(map[string]*virtualDeferredProbeV3)

	// Row 730 is armed and its job parked.
	j730 := fx.job(uri)
	if !fx.handler.armDeferredProbePending(context.Background(), j730) {
		t.Fatal("admission rejected the row-730 job as stale")
	}
	if j730.binding.EffectiveMediaFileID != 730 {
		t.Fatalf("armed binding row = %d, want 730", j730.binding.EffectiveMediaFileID)
	}
	fx.handler.deferredProbeMu.Lock()
	fx.handler.parkDeferredProbeRetryLocked(j730)
	fx.handler.deferredProbeMu.Unlock()

	// Row-only move away: the same generation, a different effective row.
	if err := fx.manager.SetEffectiveMediaFileID(fx.session.ID, 731); err != nil {
		t.Fatalf("SetEffectiveMediaFileID 731: %v", err)
	}
	// A colliding job for the row the session now serves.
	binding, err := fx.manager.VirtualSourceBinding(fx.session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceBinding: %v", err)
	}
	j731 := *j730
	j731.binding = binding
	j731.bindingGeneration = binding.Generation
	fx.handler.deferredProbeMu.Lock()
	fx.handler.parkDeferredProbeRetryLocked(&j731)
	kept730 := fx.handler.deferredProbeRetry[deferredProbeKey(j730)]
	fx.handler.deferredProbeMu.Unlock()
	if kept730 != j730 {
		t.Fatal("the retry set discarded the job for row 730; the session can return to that row and its live mark would be orphaned")
	}

	// Row returns to 730. The mark is live (armed for 730), so a replacement arm
	// is refused; convergence must come from the retained job, not the watchdog.
	if err := fx.manager.SetEffectiveMediaFileID(fx.session.ID, 730); err != nil {
		t.Fatalf("SetEffectiveMediaFileID 730: %v", err)
	}
	if _, applied, err := fx.manager.ArmVirtualProbeReplacementForCurrentBinding(fx.session.ID, uri, 5); err != nil || applied {
		t.Fatalf("replacement arm = (_, %v, %v), want refused: the returned-to mark reads live", applied, err)
	}

	// Drain the retry set: the retained row-730 job lands the mark.
	for i := 0; i < virtualDeferredProbeQueueSize+1; i++ {
		next := fx.handler.takeDeferredRetry()
		if next == nil {
			break
		}
		fx.handler.runDeferredVirtualProbe(next)
	}
	waitDeferredCondition(t, func() bool {
		live, err := fx.manager.GetSession(fx.session.ID)
		return err == nil && live.VirtualProbeOutcome == playback.VirtualProbeOutcomeVerified
	}, "the returned-to row converging from its retained job without the watchdog")
}

// blockingUnresolvedRowResolver resolves rows by id except that its first lookup
// of blockID blocks on release and then reports no row, modeling the
// replacement's effective-row lookup racing a row-only move that leaves it
// nothing to return. Later lookups of the same id do not block.
type blockingUnresolvedRowResolver struct {
	rows    map[int]*models.MediaFile
	blockID int
	started chan struct{}
	release chan struct{}
	once    *sync.Once
}

func (r *blockingUnresolvedRowResolver) GetByID(_ context.Context, id int) (*models.MediaFile, error) {
	if id == r.blockID {
		r.once.Do(func() {
			close(r.started)
			<-r.release
		})
		return nil, errors.New("catalog row not found")
	}
	if row, ok := r.rows[id]; ok && row != nil {
		return row, nil
	}
	return nil, errors.New("catalog row not found")
}

// TestDeferredProbeLateUnresolvedShedKeepsNewerLifecycle is the finding-2
// regression: the replacement path arms a lifecycle, blocks in the effective-row
// lookup, and sheds when the lookup fails. A row-only B -> C -> B move can return
// the session to a binding byte-for-byte equal to the one that first arm captured,
// while a second recovery arms a newer lifecycle for the same binding. The late
// shed must fence on the lifecycle token, not binding equality, so it does not
// stamp its failure onto the newer pending lifecycle. The newer lifecycle must
// survive and still verify.
func TestDeferredProbeLateUnresolvedShedKeepsNewerLifecycle(t *testing.T) {
	const uri = "virtual://movie/tt-late-shed?result=a"
	fx := newDeferredLifecycleFixture(t, 730, uri)
	fx.saveAttempt(t)
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.AudioTracks = fx.probedAudio
		f.SubtitleTracks = fx.probedSubs
		f.CodecAudio = "eac3"
		return f, nil
	}

	lookupStarted := make(chan struct{})
	releaseLookup := make(chan struct{})
	var once sync.Once
	fx.handler.fileResolver = &blockingUnresolvedRowResolver{
		blockID: 730,
		rows: map[int]*models.MediaFile{
			731: {ID: 731, ContentID: "movie-late-shed", FilePath: uri, VirtualOwnerInstallationID: 5},
		},
		started: lookupStarted,
		release: releaseLookup,
		once:    &once,
	}

	// R1: arm row 730, then block in the effective-row lookup. The dropped job's
	// row differs from the effective row so the lookup is actually taken.
	dropped := &virtualDeferredProbeV3{
		stickyKey:      "sticky-late-shed",
		file:           &models.MediaFile{ID: 700},
		streamURL:      "http://provider.example/stream",
		probeTransient: &models.MediaFile{ID: 700},
		cand:           VirtualPlaybackStream{ID: "cand-1", URI: uri},
		ownerID:        5,
		sessionID:      fx.session.ID,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.handler.scheduleDroppedProbeReplacement(context.Background(), dropped)
	}()
	select {
	case <-lookupStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("R1 never reached the effective-row lookup")
	}

	// Move B -> C, arm C, move C -> B, then arm B again: a newer lifecycle for the
	// returned-to B, byte-for-byte equal to R1's arm snapshot.
	if err := fx.manager.SetEffectiveMediaFileID(fx.session.ID, 731); err != nil {
		t.Fatalf("SetEffectiveMediaFileID 731: %v", err)
	}
	if _, applied, err := fx.manager.ArmVirtualProbeReplacementForCurrentBinding(fx.session.ID, uri, 5); err != nil || !applied {
		t.Fatalf("R2 arm C = (_, %v, %v), want applied", applied, err)
	}
	if err := fx.manager.SetEffectiveMediaFileID(fx.session.ID, 730); err != nil {
		t.Fatalf("SetEffectiveMediaFileID 730: %v", err)
	}
	r2Binding, applied, err := fx.manager.ArmVirtualProbeReplacementForCurrentBinding(fx.session.ID, uri, 5)
	if err != nil || !applied {
		t.Fatalf("R2 arm B = (_, %v, %v), want applied", applied, err)
	}

	// R1's lookup fails and it sheds. It must not write over R2's lifecycle.
	close(releaseLookup)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("R1's unresolved shed did not return")
	}
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != probeOutcomePending {
		t.Fatalf("outcome = %q, want R2's pending lifecycle untouched by R1's stale shed", live.VirtualProbeOutcome)
	}

	// R2 can still verify: a job bound to the lifecycle R2 armed lands verified.
	job := fx.job(uri)
	job.binding = r2Binding
	job.bindingGeneration = r2Binding.Generation
	fx.handler.runDeferredVirtualProbe(job)
	waitDeferredCondition(t, func() bool {
		live, err := fx.manager.GetSession(fx.session.ID)
		return err == nil && live.VirtualProbeOutcome == playback.VirtualProbeOutcomeVerified
	}, "R2's lifecycle reaching verified after R1's stale shed")
}
