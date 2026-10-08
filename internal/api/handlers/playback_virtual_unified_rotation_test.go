package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// TestExplicitVirtualPinV3ReflectsFileSelection pins the single predicate every
// rotation door reads (issue #244 item 2). An explicit user version pick is a
// pin; an auto selection is not. Both the decode-rotation demotion hold
// (virtualCandidateRotationPendingV3) and the rehydration retry
// (resolveRehydratedVirtualSourceV3) call this one function, so they cannot
// disagree about whether a release may rotate. A nil record is not a pin.
func TestExplicitVirtualPinV3ReflectsFileSelection(t *testing.T) {
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	auto := &playback.AttemptRecordV3{NormalizedRequest: playback.StartRequestV3{FileSelection: playback.FileSelectionAutoV3}}
	explicit := &playback.AttemptRecordV3{NormalizedRequest: playback.StartRequestV3{FileSelection: playback.FileSelectionExplicitV3}}

	if h.explicitVirtualPinV3(auto) {
		t.Fatal("auto selection reported as an explicit pin")
	}
	if !h.explicitVirtualPinV3(explicit) {
		t.Fatal("explicit version pick not reported as a pin")
	}
	if h.explicitVirtualPinV3(nil) {
		t.Fatal("nil record reported as an explicit pin")
	}
}

// TestRehydrationRefusesRotationForExplicitPin proves the failure-recovery
// rehydration door honors the same refusal the session-bound door declares. The
// production call site threads refuseRotation from explicitVirtualPinV3(record):
// an explicit pin must return the absent-pin cause without a second resolve
// (the session path would refuse to substitute it), while an auto selection
// keeps its documented absent/dead-pin rotation. Both cases drive the identical
// resolve so the only difference is the threaded intent.
func TestRehydrationRefusesRotationForExplicitPin(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-unified-rotation"
		pinnedURI  = neutralURI + "?result=pinned"
		siblingURI = neutralURI + "?result=sibling"
	)
	newRehydration := func() (*PlaybackHandler, *[]bool) {
		h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
		h.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
			return "http://127.0.0.1:9/unused", nil
		})
		rotates := &[]bool{}
		h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			*rotates = append(*rotates, VirtualCandidateRotationAllowed(ctx))
			if !VirtualCandidateRotationAllowed(ctx) {
				return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
			}
			return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/sibling", URI: siblingURI, CandidateID: "sibling", IdentityRematched: true, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024"}, nil
		})
		return h, rotates
	}
	file := &models.MediaFile{ID: 7, ContentID: "movie-unified-rotation", FilePath: pinnedURI, VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024"}

	// Explicit pin: the caller withholds rotation as intent, exactly as the
	// session-bound refusal does. The absent pin must not silently rotate.
	explicit := &playback.AttemptRecordV3{NormalizedRequest: playback.StartRequestV3{FileSelection: playback.FileSelectionExplicitV3}}
	explicitHandler, explicitRotates := newRehydration()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil).WithContext(newAuthorizedPlaybackContext())
	_, explicitErr := explicitHandler.resolveRehydratedVirtualSourceV3(r, file, "profile-1", nil, "pinned", "auto", 0,
		virtualResolveOptionsV3{refuseRotation: explicitHandler.explicitVirtualPinV3(explicit), sessionBound: true, sessionAnchorURI: pinnedURI, bypassProviderFloor: true})
	if !errors.Is(explicitErr, virtuallibrary.ErrSessionBoundCandidateAbsent) {
		t.Fatalf("explicit pin err = %v, want the absent-pin refusal", explicitErr)
	}
	if len(*explicitRotates) != 1 || (*explicitRotates)[0] {
		t.Fatalf("explicit pin rotation intents = %v, want exactly [false] (no retry)", *explicitRotates)
	}

	// Auto selection: same absent pin, no withheld intent, so the documented
	// absent/dead-pin retry runs with rotation declared.
	auto := &playback.AttemptRecordV3{NormalizedRequest: playback.StartRequestV3{FileSelection: playback.FileSelectionAutoV3}}
	autoHandler, autoRotates := newRehydration()
	resolved, autoErr := autoHandler.resolveRehydratedVirtualSourceV3(r, file, "profile-1", nil, "pinned", "auto", 0,
		virtualResolveOptionsV3{refuseRotation: autoHandler.explicitVirtualPinV3(auto), sessionBound: true, sessionAnchorURI: pinnedURI, bypassProviderFloor: true})
	if autoErr != nil {
		t.Fatalf("auto selection err = %v, want the rotated sibling", autoErr)
	}
	if got := virtualResultCandidateID(resolved.URI); got != "sibling" {
		t.Fatalf("auto selection resolved %q, want the rotated sibling", got)
	}
	if len(*autoRotates) != 2 || (*autoRotates)[0] || !(*autoRotates)[1] {
		t.Fatalf("auto selection rotation intents = %v, want exactly [false true]", *autoRotates)
	}
}

// TestVirtualCandidateRotationPendingHonorsExplicitPin proves the other
// rotation door — the failure-recovery demotion hold
// (virtualCandidateRotationPendingV3) that keeps a server-transcode delivery
// eligible for a candidate rotation — reads the same explicit-pin predicate. On
// one live rejected generation, an auto selection holds the delivery for
// rotation while an explicit pin does not: recovery must not rotate where the
// session path refuses. Before the unification the demotion hold keyed on the
// raw FileSelection inline, which could drift from the rehydration path.
func TestVirtualCandidateRotationPendingHonorsExplicitPin(t *testing.T) {
	f := newDecodeRotationFixture(t, decodeRotationOptions{
		candidateIDs:        []string{"A", "B"},
		decodeFailCalls:     1,
		rejectAfterManifest: true,
	})
	start := f.request()

	code, started := f.start(t, start)
	if code != http.StatusCreated || started.PlaybackPlan == nil {
		t.Fatalf("start status=%d response=%+v", code, started)
	}
	defer f.handler.tm.CloseTranscodeSession(started.SessionID, "")
	if got := f.sessionVirtualURI(t, started.SessionID); got != decodeRotationCandidateURI("A") {
		t.Fatalf("start candidate = %q, want A", got)
	}
	// The rejection must land on the committed generation before the hold can
	// observe the live verdict.
	f.waitForSourceRejected(t, started.SessionID)

	record, err := f.handler.PlanStoreV3.GetAttempt(t.Context(), started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	req := playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3,
		Operation:       playback.ReplanOperationFailureRecoveryV3,
		Failure:         playback.FailureV3{Classification: "decode_error"},
	}
	if !f.handler.virtualCandidateRotationPendingV3(record, req) {
		t.Fatal("auto selection with a live rejected generation did not hold the delivery for rotation")
	}

	explicit := *record
	explicit.NormalizedRequest.FileSelection = playback.FileSelectionExplicitV3
	if f.handler.virtualCandidateRotationPendingV3(&explicit, req) {
		t.Fatal("explicit pin held the delivery for a rotation the session-bound door would refuse")
	}
}
