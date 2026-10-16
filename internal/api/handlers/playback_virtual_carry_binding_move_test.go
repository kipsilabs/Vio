package handlers

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// seekReanchorRequestV3 builds a seek reanchor request against the fixture's
// started session, matching the shape the other seek-reanchor tests use.
func seekReanchorRequestV3(fx *pinnedTrackChangeFixture, requestID string) playback.ReplanRequestV3 {
	return playback.ReplanRequestV3{
		ProtocolVersion:       playback.ProtocolV3,
		Operation:             playback.ReplanOperationSeekReanchorV3,
		PlaybackAttemptID:     fx.start.PlaybackAttemptID,
		ReplanRequestID:       requestID,
		FailedPlanID:          fx.started.PlaybackPlan.PlanID,
		PlanAttemptID:         requestID + "-plan",
		PlanAttemptKey:        fx.started.PlaybackPlan.PlanAttemptKey,
		AttemptCount:          1,
		PositionSeconds:       120,
		QualityPreference:     "auto",
		SelectedTracks:        fx.started.PlaybackPlan.SelectedTracks,
		Capabilities:          fx.start.Capabilities,
		ClientPlaybackContext: fx.start.ClientPlaybackContext,
	}
}

// A same-row binding move must not let a user-intent replan pair the running
// plan's descriptor with the newly pinned candidate. The session pin moves to
// B while the running plan still describes A, and the catalog row is rewritten
// in place to name B with no probed evidence; row-id equality cannot tell that
// apart from a same-release re-plan. Admission therefore rests on the session's
// retained provenance, which still names A, so the move forces a re-resolve
// instead of topping B up with A's bytes.
func TestReplanSameRowBindingMoveDoesNotPairStaleDescriptor(t *testing.T) {
	cases := []struct {
		slug string
		// wantMovedPin asserts the plan was rebuilt for the moved pin. A seek
		// reanchor instead reopens the frozen recipe, which still names the
		// previous effective URI; its proof is the re-resolve itself.
		wantMovedPin bool
		request      func(fx *pinnedTrackChangeFixture, requestID string) playback.ReplanRequestV3
	}{
		{slug: "track-change", wantMovedPin: true, request: func(fx *pinnedTrackChangeFixture, id string) playback.ReplanRequestV3 {
			return fx.trackChangeRequest(1, id)
		}},
		{slug: "seek-reanchor", request: seekReanchorRequestV3},
	}
	for _, tc := range cases {
		t.Run(tc.slug, func(t *testing.T) {
			fx := newPinnedTrackChangeFixture(t, "b")
			candidateB := withVirtualResultKey(virtualPlaybackNeutralKey(fx.source.FilePath), "b")
			if err := fx.manager.SetVirtualSource(fx.started.SessionID, candidateB, fx.source.VirtualOwnerInstallationID); err != nil {
				t.Fatalf("SetVirtualSource: %v", err)
			}
			// The catalog row is re-probed in place under the same id: it now
			// names B but carries no verified video evidence.
			fx.churnCatalogRowToStub("b")

			response := postPlaybackReplanV3(t, fx.handler, fx.started.SessionID, tc.request(fx, "binding-move-"+tc.slug))
			if response.Terminal != nil || response.PlaybackPlan == nil {
				t.Fatalf("%s = plan %#v terminal %#v, want a plan", tc.slug, response.PlaybackPlan, response.Terminal)
			}
			if tc.wantMovedPin && response.PlaybackPlan.EffectiveVirtualURI != candidateB {
				t.Fatalf("%s effective virtual URI = %q, want the moved pin %q", tc.slug, response.PlaybackPlan.EffectiveVirtualURI, candidateB)
			}
			if fx.detailCalls == 0 {
				t.Fatalf("%s paired the running plan's descriptor with the moved pin without re-resolving", tc.slug)
			}
		})
	}
}

// A same-row binding move that returns to the same candidate (A to B to A)
// leaves the session's retained provenance naming the returned pin: the running
// plan's descriptor is that candidate, so the carry is admitted and no
// re-resolve is needed. This pins the boundary of the provenance gate: it must
// not reject a plan whose descriptor still matches the pin.
func TestReplanSameRowAbABindingMoveKeepsDescriptorForReturnedPin(t *testing.T) {
	cases := []struct {
		slug    string
		request func(fx *pinnedTrackChangeFixture, requestID string) playback.ReplanRequestV3
	}{
		{slug: "track-change", request: func(fx *pinnedTrackChangeFixture, id string) playback.ReplanRequestV3 {
			return fx.trackChangeRequest(1, id)
		}},
		{slug: "seek-reanchor", request: seekReanchorRequestV3},
	}
	for _, tc := range cases {
		t.Run(tc.slug, func(t *testing.T) {
			fx := newPinnedTrackChangeFixture(t, "b")
			candidateB := withVirtualResultKey(virtualPlaybackNeutralKey(fx.source.FilePath), "b")
			if err := fx.manager.SetVirtualSource(fx.started.SessionID, candidateB, fx.source.VirtualOwnerInstallationID); err != nil {
				t.Fatalf("SetVirtualSource to B: %v", err)
			}
			if err := fx.manager.SetVirtualSource(fx.started.SessionID, fx.pinnedURI, fx.source.VirtualOwnerInstallationID); err != nil {
				t.Fatalf("SetVirtualSource back to A: %v", err)
			}
			fx.churnCatalogRowToStub("pinned")

			response := postPlaybackReplanV3(t, fx.handler, fx.started.SessionID, tc.request(fx, "aba-"+tc.slug))
			if response.Terminal != nil || response.PlaybackPlan == nil {
				t.Fatalf("A->B->A %s = plan %#v terminal %#v, want a plan", tc.slug, response.PlaybackPlan, response.Terminal)
			}
			if response.PlaybackPlan.EffectiveVirtualURI != fx.pinnedURI {
				t.Fatalf("A->B->A %s effective virtual URI = %q, want the returned pin %q", tc.slug, response.PlaybackPlan.EffectiveVirtualURI, fx.pinnedURI)
			}
			if response.PlaybackPlan.Source.BitDepth != fx.started.PlaybackPlan.Source.BitDepth ||
				response.PlaybackPlan.Source.BitrateKbps != fx.started.PlaybackPlan.Source.BitrateKbps {
				t.Fatalf("A->B->A %s source = {bit_depth:%d bitrate_kbps:%d}, want the returned pin's descriptor {bit_depth:%d bitrate_kbps:%d}",
					tc.slug, response.PlaybackPlan.Source.BitDepth, response.PlaybackPlan.Source.BitrateKbps,
					fx.started.PlaybackPlan.Source.BitDepth, fx.started.PlaybackPlan.Source.BitrateKbps)
			}
			if fx.detailCalls != 0 {
				t.Fatalf("A->B->A %s re-resolved although provenance names the returned pin: detail=%d", tc.slug, fx.detailCalls)
			}
		})
	}
}
