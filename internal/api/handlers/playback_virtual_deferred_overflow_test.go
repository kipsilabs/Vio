package handlers

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// deferredOverflowProbe builds a deferred probe bound to session for the given
// candidate, with the generation the worker will capture.
func deferredOverflowProbe(session *playback.Session, candidateURI string, generation uint64) *virtualDeferredProbeV3 {
	return &virtualDeferredProbeV3{
		stickyKey:            "sticky-overflow",
		file:                 &models.MediaFile{ID: session.MediaFileID, ContentID: "movie-overflow"},
		streamURL:            "http://provider.example/stream",
		probeTransient:       &models.MediaFile{ID: session.MediaFileID},
		cand:                 VirtualPlaybackStream{ID: "cand-1", URI: candidateURI},
		ownerID:              5,
		sessionID:            session.ID,
		bindingGeneration:    generation,
		hasBindingGeneration: true,
		serviceCtx:           context.Background(),
	}
}

// TestDeferredVirtualProbeOverflowMarksInventoryFailed is the point-1
// regression: when both the deferred-probe queue and its bounded retry set are
// full, the probe is discarded. The discard must not leave the session pending
// forever — the outcome is marked failed, which is exactly what the inventory
// poll reports, so a client stops loading instead of waiting for a probe no
// worker will run. The admission must also stay non-blocking.
func TestDeferredVirtualProbeOverflowMarksInventoryFailed(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 810, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	candidateURI := "virtual://movie/tt-overflow?result=cand-1"
	if err := manager.SetVirtualSource(session.ID, candidateURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	generation, err := manager.VirtualSourceGeneration(session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceGeneration: %v", err)
	}

	handler := NewPlaybackHandler(manager)
	// A canceled service context makes the lazily-started workers exit at once.
	// Wait for them to exit before filling the queue, so no worker can drain a
	// slot and the overflow branch is exercised deterministically.
	handler.ServiceContext = canceledContext()
	handler.startDeferredProbeWorkers()
	handler.deferredProbeWG.Wait()
	// Fill the worker queue and the retry set to their bounds with inert probes.
	for i := 0; i < virtualDeferredProbeQueueSize; i++ {
		handler.deferredProbeQueue <- &virtualDeferredProbeV3{}
		handler.deferredProbeRetry[deferredRetryTestKey(i)] = &virtualDeferredProbeV3{}
	}

	shed := deferredOverflowProbe(session, candidateURI, generation)

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
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
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

// TestDeferredProbeOutcomeNotWrittenAfterRotationDuringEvidenceWrite is the
// point-2 regression: the binding fence and the outcome write are separate
// operations, so a rotation that lands between probe completion and outcome
// storage could let the old probe overwrite the new binding's outcome. The
// saver callback performs exactly that rotation after the fence has passed; the
// generation compare-and-swap must reject the stale outcome so the replacement
// binding keeps its own (empty) lifecycle.
func TestDeferredProbeOutcomeNotWrittenAfterRotationDuringEvidenceWrite(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 811, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	oldURI := "virtual://movie/tt-rotate?result=old"
	if err := manager.SetVirtualSource(session.ID, oldURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	generation, err := manager.VirtualSourceGeneration(session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceGeneration: %v", err)
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

	deferred := deferredOverflowProbe(session, oldURI, generation)
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

// TestDeferredProbeDurableWriteRejectedMarksFailed is the point-3 regression:
// the deferred probe must not report verified before its evidence write is
// actually readable. When the durable write is rejected (a superseded CAS
// snapshot, so the row still shows the old menu), the probe's outcome is failed,
// which the inventory poll reports so the client leaves loading instead of
// stopping on a stale verified menu.
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
	generation, err := manager.VirtualSourceGeneration(session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceGeneration: %v", err)
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
	// probed evidence is not readable and must not be reported verified.
	handler.VirtualFileMetadataSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		return VirtualFileMetadataUpdateResult{MetadataUpdated: false, RowsAffected: 0}, nil
	}

	deferred := deferredOverflowProbe(session, candidateURI, generation)
	handler.runDeferredVirtualProbe(deferred)

	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != probeOutcomeFailed {
		t.Fatalf("rejected durable write outcome = %q, want failed", live.VirtualProbeOutcome)
	}
}

// TestDeferredProbeDurableWriteErrorMarksFailed covers the transient/terminal
// error arm of the durable write: a catalog error means the evidence is not
// readable, so the outcome is failed rather than a premature verified.
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
	generation, err := manager.VirtualSourceGeneration(session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceGeneration: %v", err)
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

	deferred := deferredOverflowProbe(session, candidateURI, generation)
	handler.runDeferredVirtualProbe(deferred)

	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != probeOutcomeFailed {
		t.Fatalf("durable write error outcome = %q, want failed", live.VirtualProbeOutcome)
	}
}
