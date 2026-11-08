package handlers

import (
	"testing"
)

// A track change does not move the playhead, so the replan must resume where the
// session is, not where the client's snapshot claimed it was. The client's
// position is sampled before its replan round trip and a subtitle switch that
// reloads the media element can report the route's start position, which used
// to restart a mid-movie replay at the resume point.
func TestTrackChangeReplanUsesLiveSessionPosition(t *testing.T) {
	file := subtitleReplanFixtureFile(t, 3)
	handler := subtitleReplanHandler(t, file)
	sessionID, started, start := startSubtitleReplanSessionV3(t, handler)

	const livePosition = 187.5
	if err := handler.sessionMgr.UpdateProgress(sessionID, livePosition, false); err != nil {
		t.Fatalf("update progress: %v", err)
	}

	request := subtitleReplanTrackChangeV3(started, start, file.ID, 1, "position-live-0001")
	// The stale echo: the client rebuilt its element and reported the plan's
	// start instead of where the viewer is.
	request.PositionSeconds = 12
	response := postPlaybackReplanV3(t, handler, sessionID, request)
	if response.PlaybackPlan == nil {
		t.Fatalf("replan returned no plan: %#v", response.Terminal)
	}
	if got := response.PlaybackPlan.Timeline.SourceStartSeconds; got != livePosition {
		t.Fatalf("replan source position = %v, want the live session position %v (client echo was %v)", got, livePosition, request.PositionSeconds)
	}
	if got := response.PlaybackPlan.Timeline.PlayerStartSeconds; got != livePosition {
		t.Fatalf("replan player position = %v, want the live session position %v", got, livePosition)
	}
}
