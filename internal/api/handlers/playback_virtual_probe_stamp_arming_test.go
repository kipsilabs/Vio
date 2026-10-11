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
)

// TestVirtualProbeEvidenceArgsStampCoupledToCodecInventory pins the args-level
// half of the stamp coupling: a write carrying a codec-bearing audio or
// subtitle entry forces StampProbe even when the caller asked for a
// metadata-only write, while a language-only hint inventory leaves the flag
// untouched (so a NULL stamp is preserved).
func TestVirtualProbeEvidenceArgsStampCoupledToCodecInventory(t *testing.T) {
	const candidateURI = "virtual://movie/tt-coupling?result=cand"
	row := &models.MediaFile{
		ID:                         41,
		ContentID:                  "movie-tt-coupling",
		FilePath:                   candidateURI,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	h := &PlaybackHandler{}

	cases := []struct {
		name   string
		probed *models.MediaFile
		want   bool
	}{
		{
			name:   "codec-bearing audio forces the stamp",
			probed: &models.MediaFile{AudioTracks: []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}}},
			want:   true,
		},
		{
			name:   "codec-bearing subtitle forces the stamp",
			probed: &models.MediaFile{SubtitleTracks: []models.SubtitleTrack{{Codec: "subrip", Language: "eng"}}},
			want:   true,
		},
		{
			name: "language-only hint inventory preserves the unstamped state",
			probed: &models.MediaFile{
				AudioTracks:    []models.AudioTrack{{Language: "eng"}},
				SubtitleTracks: []models.SubtitleTrack{{Language: "eng"}},
			},
			want: false,
		},
		{
			name:   "empty inventory preserves the unstamped state",
			probed: &models.MediaFile{},
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, ok := h.virtualProbeEvidenceArgsForRow(context.Background(), row, candidateURI, tc.probed, false)
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
