package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// TestVirtualProbeEvidenceArgsStampFollowsExplicitProvenance pins that
// StampProbe is the explicit successful-probe flag, never a function of the
// inventory shape. A metadata-only write that carries probe-observed-looking
// tracks still preserves the row's stamp (stampProbe false), so the row is not
// advanced from tracks the write merely copied, and an ffprobe track numbered 0
// is not special-cased. A write from a probe that succeeded stamps regardless
// of the inventory it carries. This is the corrected design: index or codec
// positivity no longer controls the stamp.
func TestVirtualProbeEvidenceArgsStampFollowsExplicitProvenance(t *testing.T) {
	const candidateURI = "virtual://movie/tt-provenance?result=cand"
	row := &models.MediaFile{
		ID:                         41,
		ContentID:                  "movie-tt-provenance",
		FilePath:                   candidateURI,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	h := &PlaybackHandler{}

	cases := []struct {
		name   string
		probed *models.MediaFile
		stamp  bool
		want   bool
	}{
		{
			name:   "metadata-only write of probe-observed audio preserves the stamp",
			probed: &models.MediaFile{AudioTracks: []models.AudioTrack{{Index: 1, Codec: "aac", Channels: 2, Language: "eng"}}},
			stamp:  false,
			want:   false,
		},
		{
			// ffprobe numbers some streams 0; a metadata-only write must not
			// treat a valid index-0 track as probe evidence either.
			name:   "metadata-only write of a valid index-0 track preserves the stamp",
			probed: &models.MediaFile{AudioTracks: []models.AudioTrack{{Index: 0, Codec: "aac", Channels: 2, Language: "eng"}}},
			stamp:  false,
			want:   false,
		},
		{
			name:   "metadata-only write of synthesized declared audio preserves the stamp",
			probed: &models.MediaFile{AudioTracks: declaredVirtualAudioTracks("aac", []string{"eng", "deu"}, "")},
			stamp:  false,
			want:   false,
		},
		{
			name: "metadata-only write of hint-only inventory preserves the stamp",
			probed: &models.MediaFile{
				AudioTracks:    []models.AudioTrack{{Language: "eng"}},
				SubtitleTracks: []models.SubtitleTrack{{Language: "eng"}},
			},
			stamp: false,
			want:  false,
		},
		{
			name:   "successful probe stamps probe-observed audio",
			probed: &models.MediaFile{AudioTracks: []models.AudioTrack{{Index: 1, Codec: "aac", Channels: 2, Language: "eng"}}},
			stamp:  true,
			want:   true,
		},
		{
			// A successful probe that enumerated no tracks still commits its
			// verdict; the stamp follows provenance, not inventory shape.
			name:   "successful probe stamps an empty inventory too",
			probed: &models.MediaFile{},
			stamp:  true,
			want:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, ok := h.virtualProbeEvidenceArgsForRow(context.Background(), row, candidateURI, tc.probed, resolver.PersistedIdentityTiers{}, tc.stamp)
			if !ok {
				t.Fatal("args builder refused the write")
			}
			if args.StampProbe != tc.want {
				t.Fatalf("StampProbe = %v, want %v", args.StampProbe, tc.want)
			}
		})
	}
}

// TestDurableResumeArmsDeferredProbeForUnstampedRow is the direct-play cold
// start end-to-end: a row with planner-grade video evidence, a persisted
// provider URL, and no probe stamp takes the durable-resume fast path, which
// must hand back a deferred probe. Admitting that probe after commit arms the
// session pending and a worker resolves it to verified with inventory_updated
// published in the same session.
func TestDurableResumeArmsDeferredProbeForUnstampedRow(t *testing.T) {
	const candidateURI = "virtual://movie/tt-stamp-arm?result=cand-arm"
	fx := newDeferredLifecycleFixture(t, 771, candidateURI)
	fx.saveAttempt(t)
	fx.handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(
		func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			t.Fatal("the durable-resume fast path must not fall back to the provider resolver")
			return "", nil
		})

	expiresAt := time.Now().Add(3 * time.Hour)
	file := withVirtualResumeVideoEvidence(&models.MediaFile{
		ID:                         771,
		ContentID:                  "movie-tt-stamp-arm",
		FilePath:                   candidateURI,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
		ResolvedURL:                "https://93.184.216.34/stream/token=stored",
		ResolvedURLExpiresAt:       &expiresAt,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)
	resolved, err := fx.handler.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.Provenance != ProbeProvenancePending {
		t.Fatalf("provenance = %q, want pending (the fast path still returns immediately)", resolved.Provenance)
	}
	deferred := resolved.DeferredProbe
	if deferred == nil {
		t.Fatal("unstamped direct-play start armed no deferred probe; pending would never resolve")
	}
	if deferred.streamURL != "https://93.184.216.34/stream/token=stored" {
		t.Fatalf("deferred probe URL = %q, want the row's persisted URL", deferred.streamURL)
	}

	conn := fx.registerPush(t)
	deferred.sessionID = fx.session.ID
	fx.handler.enqueueDeferredVirtualProbeV3(context.Background(), deferred)
	close(fx.releaseProbe)

	deadline := time.Now().Add(3 * time.Second)
	for {
		live, getErr := fx.manager.GetSession(fx.session.ID)
		if getErr == nil && live.VirtualProbeOutcome == probeOutcomeVerified {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session outcome never reached verified (last: %v)", live)
		}
		time.Sleep(5 * time.Millisecond)
	}

	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceVerified) {
			t.Fatalf("pushed inventory status = %q, want verified", event.payload.InventoryStatus)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no inventory_updated push delivered after the deferred probe verified")
	}
}

// TestDurableResumeArmsDeferredProbeThatCanFail is the failure half of the same
// wiring: an unstamped direct-play start still arms a worker, and a probe that
// cannot enumerate terminally fails the session instead of leaving it pending
// for the convergence watchdog.
func TestDurableResumeArmsDeferredProbeThatCanFail(t *testing.T) {
	const candidateURI = "virtual://movie/tt-stamp-arm-fail?result=cand-arm"
	fx := newDeferredLifecycleFixture(t, 772, candidateURI)
	fx.saveAttempt(t)
	fx.handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(
		func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			t.Fatal("the durable-resume fast path must not fall back to the provider resolver")
			return "", nil
		})
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, _ *models.MediaFile) (*models.MediaFile, error) {
		return nil, errors.New("provider refused the enumeration")
	}

	expiresAt := time.Now().Add(3 * time.Hour)
	file := withVirtualResumeVideoEvidence(&models.MediaFile{
		ID:                         772,
		ContentID:                  "movie-tt-stamp-arm-fail",
		FilePath:                   candidateURI,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
		ResolvedURL:                "https://93.184.216.34/stream/token=stored",
		ResolvedURLExpiresAt:       &expiresAt,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)
	resolved, err := fx.handler.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	deferred := resolved.DeferredProbe
	if deferred == nil {
		t.Fatal("unstamped direct-play start armed no deferred probe")
	}
	deferred.sessionID = fx.session.ID
	fx.handler.enqueueDeferredVirtualProbeV3(context.Background(), deferred)

	deadline := time.Now().Add(3 * time.Second)
	for {
		live, getErr := fx.manager.GetSession(fx.session.ID)
		if getErr == nil && live.VirtualProbeOutcome == probeOutcomeFailed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session outcome never reached failed (last: %v)", live)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAlreadyStampedDirectPlayStartDoesNotArmDeferredProbe pins the no-double
// probe half: a row with complete evidence and a probe stamp takes the P0 fast
// path and is never re-probed, so it arms no deferred worker.
func TestAlreadyStampedDirectPlayStartDoesNotArmDeferredProbe(t *testing.T) {
	const candidateURI = "virtual://movie/tt-stamp-p0?result=cand-p0"
	fx := newDeferredLifecycleFixture(t, 773, candidateURI)
	fx.handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(
		func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			t.Fatal("already-stamped P0 fast path must not resolve")
			return "", nil
		})
	fx.handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(
		func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			t.Fatal("already-stamped P0 fast path must not resolve")
			return ResolvedVirtualMedia{}, nil
		})

	stampedAt := time.Now()
	file := withVirtualResumeVideoEvidence(&models.MediaFile{
		ID:                         773,
		ContentID:                  "movie-tt-stamp-p0",
		FilePath:                   candidateURI,
		Container:                  "mkv",
		CodecAudio:                 "aac",
		AudioTracks:                []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}},
		VirtualOwnerInstallationID: 5,
		ProbeUpdatedAt:             &stampedAt,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)
	resolved, err := fx.handler.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.DeferredProbe != nil {
		t.Fatal("already-stamped row armed a deferred probe; it must not be re-probed")
	}
}

// TestNonNegotiatingStartStillArmsDeferredProbe is the all-clients acceptance: a
// fresh start from a client that never negotiated deferred_track_inventory_v1
// and does not consume realtime or the inventory poll still arms the deferred
// probe and stamps the row, so its next play reads verified inventory. This
// covers the Silo clients and any non-web consumer that never opted into the
// deferred lifecycle.
func TestNonNegotiatingStartStillArmsDeferredProbe(t *testing.T) {
	const candidateURI = "virtual://movie/tt-stamp-nonneg?result=cand-nn"
	fx := newDeferredLifecycleFixture(t, 774, candidateURI)
	// The attempt negotiated neither the client token nor the server feature.
	fx.saveAttemptWithoutNegotiation(t)
	fx.handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(
		func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			t.Fatal("the durable-resume fast path must not fall back to the provider resolver")
			return "", nil
		})
	fx.handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(
		func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			t.Fatal("a stamped row must take the P0 fast path on the next play")
			return ResolvedVirtualMedia{}, nil
		})

	// The saver models the real statement: the write carries probed tracks and
	// StampProbe, so the row ends this session stamped and the next resolve sees
	// complete evidence plus a stamp.
	var saved models.VirtualFilePersistArgs
	var savedMu sync.Mutex

	expiresAt := time.Now().Add(3 * time.Hour)
	file := withVirtualResumeVideoEvidence(&models.MediaFile{
		ID:                         774,
		ContentID:                  "movie-tt-stamp-nonneg",
		FilePath:                   candidateURI,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
		ResolvedURL:                "https://93.184.216.34/stream/token=stored",
		ResolvedURLExpiresAt:       &expiresAt,
	})

	fx.handler.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		savedMu.Lock()
		saved = args
		savedMu.Unlock()
		var audio []models.AudioTrack
		if err := json.Unmarshal(args.AudioTracks, &audio); err != nil {
			return VirtualFileMetadataUpdateResult{}, err
		}
		// Apply the committed write the way the real statement does: probed
		// inventory plus a stamp, so this row is verified for the next play.
		stamp := time.Now()
		file.AudioTracks = audio
		file.CodecAudio = "eac3"
		file.Container = "mkv"
		file.ProbeUpdatedAt = &stamp
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)
	resolved, err := fx.handler.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	deferred := resolved.DeferredProbe
	if deferred == nil {
		t.Fatal("a non-negotiating start armed no deferred probe; the row would never be stamped")
	}
	deferred.sessionID = fx.session.ID
	fx.handler.enqueueDeferredVirtualProbeV3(context.Background(), deferred)
	close(fx.releaseProbe)

	deadline := time.Now().Add(3 * time.Second)
	for {
		live, getErr := fx.manager.GetSession(fx.session.ID)
		if getErr == nil && live.VirtualProbeOutcome == probeOutcomeVerified {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("non-negotiating session outcome never reached verified (last: %v)", live)
		}
		time.Sleep(5 * time.Millisecond)
	}

	savedMu.Lock()
	got := saved
	savedMu.Unlock()
	if !got.StampProbe {
		t.Fatal("the deferred write for a non-negotiating start did not request the stamp")
	}
	if file.ProbeUpdatedAt == nil {
		t.Fatal("the row was not stamped, so the next play would still report pending")
	}

	// Next play: the stamped, complete row must take the P0 fast path and arm no
	// worker. A second deferred probe here would mean the non-negotiating start
	// failed to converge.
	next, err := fx.handler.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("second resolve error: %v", err)
	}
	if next.DeferredProbe != nil {
		t.Fatal("the next play on a stamped row re-armed a probe")
	}
}

// TestDurableResumeDamperDeclinesWorkerAndServesDeclared is the damper half of
// the arming wiring: an unstamped durable resume whose candidate already carries
// a recent probe failure does not admit a worker (the budget was already spent)
// and reports declared, not pending, so no session waits on a resolution nothing
// will deliver. The cache-only recovery still runs; a cold cache leaves the row
// for a later attempt once the damper lapses.
func TestDurableResumeDamperDeclinesWorkerAndServesDeclared(t *testing.T) {
	const candidateURI = "virtual://movie/tt-stamp-damper?result=cand-damper"
	fx := newDeferredLifecycleFixture(t, 775, candidateURI)
	fx.saveAttempt(t)
	fx.handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(
		func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			t.Fatal("the durable-resume fast path must not fall back to the provider resolver")
			return "", nil
		})
	probeCalled := false
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		probeCalled = true
		return f, nil
	}
	// Arm the damper for this candidate before the start; the durable resume
	// must share the declared fast path's admission decision.
	key := virtualProbeFailureKey(candidateURI, 5)
	virtualProbeFailures.mark(key)
	t.Cleanup(func() { virtualProbeFailures.clear(key) })

	expiresAt := time.Now().Add(3 * time.Hour)
	file := withVirtualResumeVideoEvidence(&models.MediaFile{
		ID:                         775,
		ContentID:                  "movie-tt-stamp-damper",
		FilePath:                   candidateURI,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
		ResolvedURL:                "https://93.184.216.34/stream/token=stored",
		ResolvedURLExpiresAt:       &expiresAt,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)
	resolved, err := fx.handler.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.Provenance != ProbeProvenanceDeclared {
		t.Fatalf("provenance = %q, want declared (no worker will resolve a pending state)", resolved.Provenance)
	}
	if resolved.DeferredProbe != nil {
		t.Fatal("damper-armed durable resume armed a worker; the probe budget was already spent")
	}
	if probeCalled {
		t.Fatal("damper-armed durable resume probed despite the failure backoff")
	}
}

// TestDurableResumeDamperRecoversWarmCachedProbe is the warm-cache half of the
// damper wiring that TestDurableResumeDamperDeclinesWorkerAndServesDeclared
// covers cold: when the damper is armed but a completed inner probe has already
// landed in the probe cache, the cache-only recovery persists that evidence and
// stamps the row. The session still reports declared (no worker is admitted
// while the budget is spent), but the row converges to verified inventory
// instead of waiting for the backoff to lapse, so the next play takes the P0
// fast path.
func TestDurableResumeDamperRecoversWarmCachedProbe(t *testing.T) {
	const candidateURI = "virtual://movie/tt-stamp-damper-warm?result=cand-dw"
	fx := newDeferredLifecycleFixture(t, 776, candidateURI)
	fx.saveAttempt(t)
	fx.handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(
		func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			t.Fatal("the durable-resume fast path must not fall back to the provider resolver")
			return "", nil
		})
	// The worker must not be admitted while the damper is armed; the recovery
	// has to come from the cache alone.
	probeCalled := false
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		probeCalled = true
		return f, nil
	}
	// A completed inner probe for the persisted URL is available in the cache.
	var cacheLookups int
	fx.handler.VirtualProbeCacheLookup = func(_ string, file *models.MediaFile) *models.MediaFile {
		cacheLookups++
		return &models.MediaFile{
			FilePath:                   file.FilePath,
			VirtualOwnerInstallationID: file.VirtualOwnerInstallationID,
			CodecAudio:                 "eac3",
			Container:                  "mkv",
			AudioTracks:                []models.AudioTrack{{Index: 1, Codec: "eac3", Channels: 6, Language: "eng"}},
		}
	}
	// The recovered evidence must be persisted as a successful-probe write: the
	// cached probe measured the bytes, so it is real inventory and must carry the
	// stamp. VirtualFileSaver admits the write into the bounded evidence buffer;
	// VirtualFileMetadataSaver is the write the worker runs.
	var saved models.VirtualFilePersistArgs
	var savedMu sync.Mutex
	fx.handler.VirtualFileSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (int64, error) {
		return 1, nil
	}
	fx.handler.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		savedMu.Lock()
		saved = args
		savedMu.Unlock()
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}
	// Arm the damper for this candidate before the start; the durable resume
	// must share the declared fast path's admission decision.
	key := virtualProbeFailureKey(candidateURI, 5)
	virtualProbeFailures.mark(key)
	t.Cleanup(func() { virtualProbeFailures.clear(key) })

	expiresAt := time.Now().Add(3 * time.Hour)
	file := withVirtualResumeVideoEvidence(&models.MediaFile{
		ID:                         776,
		ContentID:                  "movie-tt-stamp-damper-warm",
		FilePath:                   candidateURI,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
		ResolvedURL:                "https://93.184.216.34/stream/token=stored",
		ResolvedURLExpiresAt:       &expiresAt,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)
	resolved, err := fx.handler.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.Provenance != ProbeProvenanceDeclared {
		t.Fatalf("provenance = %q, want declared (no worker will resolve a pending state)", resolved.Provenance)
	}
	if resolved.DeferredProbe != nil {
		t.Fatal("damper-armed durable resume armed a worker; the probe budget was already spent")
	}
	if probeCalled {
		t.Fatal("damper-armed durable resume probed despite the failure backoff")
	}
	if cacheLookups != 1 {
		t.Fatalf("cache lookups = %d, want the recovery to consult the warm cache once", cacheLookups)
	}
	if virtualProbeFailures.count(key) != 0 {
		t.Fatal("warm-cache recovery did not clear the failure damper")
	}

	// The recovery enqueues into the bounded evidence buffer; wait on the write
	// rather than sleeping a fixed duration.
	deadline := time.Now().Add(3 * time.Second)
	for {
		savedMu.Lock()
		got := saved
		savedMu.Unlock()
		if got.FileID != 0 {
			if !got.StampProbe {
				t.Fatal("recovered cache evidence was persisted without requesting the probe stamp")
			}
			var audio []models.AudioTrack
			if err := json.Unmarshal(got.AudioTracks, &audio); err != nil {
				t.Fatalf("saved audio tracks: %v", err)
			}
			if len(audio) != 1 || audio[0].Codec != "eac3" {
				t.Fatalf("saved audio tracks = %#v, want the cached eac3 track", audio)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("warm-cache recovery never persisted the cached probe evidence")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestUnstampedStoredURLStartWithoutAWorkerServesDeclared pins that pending is
// only promised when a deferred worker is actually admitted. The durable-resume
// fast path can run for an unstamped row whose stored URL cannot back a probe:
// no prober is configured, or the trusted-but-expired stored URL yields an
// empty value. In both cases no worker exists to resolve a pending state, so
// the start must serve declared metadata with no pending lifecycle instead of
// leaving the session waiting on a resolution nothing will ever deliver.
func TestUnstampedStoredURLStartWithoutAWorkerServesDeclared(t *testing.T) {
	const candidateURI = "virtual://movie/tt-stamp-noworker?result=cand-nw"

	cases := []struct {
		name string
		// configure adjusts the fixture and the row before the start. Both
		// cases leave the durable-resume fast path intact, so the resolve must
		// not fall back to the provider: the fatal resolver below proves it.
		configure func(fx *deferredLifecycleFixture, file *models.MediaFile)
	}{
		{
			name: "no prober is configured",
			configure: func(fx *deferredLifecycleFixture, _ *models.MediaFile) {
				fx.handler.VirtualPlaybackSourceProber = nil
				fx.handler.VirtualPlaybackSourceProberWithHeaders = nil
			},
		},
		{
			name: "trusted but expired stored URL yields an empty probe input",
			configure: func(fx *deferredLifecycleFixture, file *models.MediaFile) {
				// Expired inside the trust window: the row still owns its
				// candidate and the durable-resume fast path binds it, but the
				// stored URL is refused and no URL reaches the probe.
				expiredAt := time.Now().Add(-time.Hour)
				file.ResolvedURLExpiresAt = &expiredAt
				file.UpdatedAt = time.Now()
				fx.handler.VirtualCandidateTrustWindow = func() time.Duration { return 24 * time.Hour }
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newDeferredLifecycleFixture(t, 777, candidateURI)
			fx.saveAttempt(t)
			fx.handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(
				func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
					t.Fatal("the durable-resume fast path must not fall back to the provider resolver")
					return "", nil
				})
			probeCalled := false
			fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
				probeCalled = true
				return f, nil
			}

			expiresAt := time.Now().Add(3 * time.Hour)
			file := withVirtualResumeVideoEvidence(&models.MediaFile{
				ID:                         777,
				ContentID:                  "movie-tt-stamp-noworker",
				FilePath:                   candidateURI,
				Container:                  "virtual",
				VirtualOwnerInstallationID: 5,
				ResolvedURL:                "https://93.184.216.34/stream/token=stored",
				ResolvedURLExpiresAt:       &expiresAt,
			})
			tc.configure(fx, file)

			req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)
			resolved, err := fx.handler.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
			if err != nil {
				t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
			}
			if resolved.Provenance != ProbeProvenanceDeclared {
				t.Fatalf("provenance = %q, want declared (no worker can resolve a pending state)", resolved.Provenance)
			}
			if resolved.DeferredProbe != nil {
				t.Fatal("the start armed a deferred probe although no worker could be admitted")
			}
			if probeCalled {
				t.Fatal("the start probed although no deferred worker was admitted")
			}
		})
	}
}
