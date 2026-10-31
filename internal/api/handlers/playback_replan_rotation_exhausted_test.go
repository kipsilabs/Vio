package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

const (
	rotationExhaustedSourceID    = 370
	rotationExhaustedAlternateID = 371
)

// rotationExhaustedReplanFixture starts a virtual session whose provider lists
// one concrete ?result= candidate (so a replan rehydrates instead of skipping
// on an unchanged candidate). After the start, the provider is made to fail:
// pinnedErr is returned for the session's release and alternateErr for the
// sibling edition. The returned counter records every sibling-edition resolve,
// which only the alternate-version walk performs.
func rotationExhaustedReplanFixture(t *testing.T, pinnedErr, alternateErr error) (*PlaybackHandler, playback.DecisionResponseV3, playback.StartRequestV3, *atomic.Int32) {
	t.Helper()
	source := v3HandlerFixtureFile(t)
	source.ID = rotationExhaustedSourceID
	source.FilePath = fmt.Sprintf("virtual://movie/source-%d", rotationExhaustedSourceID)
	source.VirtualOwnerInstallationID = 5

	alternate := *source
	alternate.ID = rotationExhaustedAlternateID
	alternate.FilePath = fmt.Sprintf("virtual://movie/alt-%d", rotationExhaustedAlternateID)

	manager := playback.NewSessionManager(0, 0)
	files := map[int]*models.MediaFile{source.ID: source, alternate.ID: &alternate}
	handler := NewPlaybackHandler(manager, mapPlaybackFileResolver{files: files})
	handler.FileVersionFetcher = testPlaybackFileVersionFetcher{byContent: map[string][]*models.MediaFile{
		source.ContentID: {source, &alternate},
	}}
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{}}
	handler.PlaybackConfig = playbackTestConfig("", "")
	handler.ItemAccess = allowAllPlaybackItemAccess{}

	var providerDown atomic.Bool
	alternateResolves := &atomic.Int32{}
	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		if !providerDown.Load() {
			return "http://127.0.0.1:8080/stream?path=" + path, nil
		}
		if strings.Contains(path, fmt.Sprintf("alt-%d", rotationExhaustedAlternateID)) {
			alternateResolves.Add(1)
			if alternateErr != nil {
				return "", alternateErr
			}
		} else if pinnedErr != nil {
			return "", pinnedErr
		}
		return "http://127.0.0.1:8080/stream?path=" + path, nil
	})
	// The provider lists a result=-suffixed candidate so the session binds to a
	// URI that differs from the catalog row, keeping the replan on the
	// fall-through rehydration path instead of the unchanged-candidate skip.
	handler.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(func(_ context.Context, path string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			URI: path + "?result=pinned", Resolution: source.Resolution, CodecVideo: source.CodecVideo, CodecAudio: source.CodecAudio, Container: "mp4",
		}}, nil
	})

	startRequest := v3HandlerStartRequest()
	startRequest.FileID = source.ID
	started := startV3PlaybackForHandlerTest(t, handler, startRequest)
	providerDown.Store(true)
	return handler, started, startRequest, alternateResolves
}

func rotationExhaustedReplanRequest(started playback.DecisionResponseV3, startRequest playback.StartRequestV3, replanRequestID string) playback.ReplanRequestV3 {
	currentKey := playback.PlanAttemptKeyV3(*started.PlaybackPlan, startRequest.ClientPlaybackContext.Output.OutputContextID, nil)
	return playback.ReplanRequestV3{
		ProtocolVersion:       playback.ProtocolV3,
		Operation:             playback.ReplanOperationFailureRecoveryV3,
		PlaybackAttemptID:     startRequest.PlaybackAttemptID,
		ReplanRequestID:       replanRequestID,
		FailedPlanID:          started.PlaybackPlan.PlanID,
		PlanAttemptID:         replanRequestID,
		PlanAttemptKey:        currentKey,
		AttemptedPlanKeys:     []string{currentKey},
		AttemptCount:          1,
		QualityPreference:     "auto",
		SelectedTracks:        started.PlaybackPlan.SelectedTracks,
		Failure:               playback.FailureV3{Classification: "playback_error"},
		Capabilities:          startRequest.Capabilities,
		ClientPlaybackContext: startRequest.ClientPlaybackContext,
	}
}

// assertExhaustedRotationFailsFast drives the replan and asserts the response
// is the same exhausted-rotation terminal the full sibling walk would reach,
// without resolving any sibling edition. The counter is the deterministic
// budget-skip evidence: the sibling walk is the only path that resolves an
// alternate file.
func assertExhaustedRotationFailsFast(t *testing.T, handler *PlaybackHandler, started playback.DecisionResponseV3, startRequest playback.StartRequestV3, replanRequestID string, alternateResolves *atomic.Int32) {
	t.Helper()
	replanReq := rotationExhaustedReplanRequest(started, startRequest, replanRequestID)

	begin := time.Now()
	response := postPlaybackReplanV3(t, handler, started.SessionID, replanReq)
	elapsed := time.Since(begin)

	if response.Outcome != playback.OutcomeAdaptationUnavailableV3 {
		t.Fatalf("outcome = %q, want %q; response = %#v", response.Outcome, playback.OutcomeAdaptationUnavailableV3, response)
	}
	if response.PlaybackPlan != nil {
		t.Fatalf("replan returned a plan (%#v), want the exhausted-rotation terminal", response.PlaybackPlan)
	}
	if response.Terminal == nil || response.Terminal.Reason != "virtual_source_unavailable" || !response.Terminal.Retryable {
		t.Fatalf("terminal = %#v, want retryable virtual_source_unavailable", response.Terminal)
	}
	if got := alternateResolves.Load(); got != 0 {
		t.Fatalf("sibling editions resolved %d times, want 0: an exhausted rotation must skip the substituted-sibling probe", got)
	}
	if elapsed > time.Second {
		t.Fatalf("exhausted-rotation replan took %v; it probed substituted siblings instead of failing fast", elapsed)
	}
}

// A dead provider makes the replan rehydration fail with a provider-wide
// outage. The sibling-edition walk would resolve each substitute through the
// same provider and burn the probe plus planning budgets before reaching this
// same terminal, so the rotation must fail fast.
func TestHandleReplanPlaybackV3ProviderOutageRotationExhaustedSkipsSiblingProbe(t *testing.T) {
	providerDown := fmt.Errorf("%w: streaming provider returned status 503", resolver.ErrProviderUnavailable)
	handler, started, startRequest, alternateResolves := rotationExhaustedReplanFixture(t, providerDown, providerDown)
	assertExhaustedRotationFailsFast(t, handler, started, startRequest, "virtual-provider-outage-0001", alternateResolves)
}

// Control for the test above: the same fixture with a release-specific pinned
// failure still walks the sibling edition. This proves the fast path is gated
// on the exhaustion verdict, not on the fixture never reaching the walk.
func TestHandleReplanPlaybackV3ReleaseSpecificFailureStillWalksSibling(t *testing.T) {
	handler, started, startRequest, alternateResolves := rotationExhaustedReplanFixture(t, errors.New("source offline"), nil)
	replanReq := rotationExhaustedReplanRequest(started, startRequest, "virtual-release-specific-0001")

	response := postPlaybackReplanV3(t, handler, started.SessionID, replanReq)

	if response.Terminal != nil || response.PlaybackPlan == nil {
		t.Fatalf("response = %#v, want a plan on the sibling edition", response)
	}
	if got := alternateResolves.Load(); got != 1 {
		t.Fatalf("sibling editions resolved %d times, want 1: a release-specific failure must keep the sibling walk", got)
	}
}

// A session-bound pin that the provider no longer lists triggers the rotation
// retry. When that declared rotation finds no viable candidate either, the
// replan already has its answer and must not walk sibling editions.
func TestHandleReplanPlaybackV3FailedRotationSkipsSiblingProbe(t *testing.T) {
	absent := fmt.Errorf("session-bound candidate is no longer listed: %w", virtuallibrary.ErrSessionBoundCandidateAbsent)
	handler, started, startRequest, alternateResolves := rotationExhaustedReplanFixture(t, absent, absent)
	assertExhaustedRotationFailsFast(t, handler, started, startRequest, "virtual-failed-rotation-0001", alternateResolves)
}
