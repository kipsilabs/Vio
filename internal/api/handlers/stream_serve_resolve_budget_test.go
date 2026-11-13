package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestHandleStreamServeResolveBudgetAnswersRetryable503 is the incident replay
// for the two 60s getPlaybackMedia requests: the media element asked for bytes
// while the session's virtual candidate could not re-list, and the serve spent
// the client's whole patience in the provider outage retry. The serve must now
// stop at its own budget and answer the retryable provider_unavailable 503 with
// a Retry-After, so the player can fall back or retry instead of hanging until
// it aborts and the request is recorded as a 408 timeout.
func TestHandleStreamServeResolveBudgetAnswersRetryable503(t *testing.T) {
	restore := virtualServeResolveBudget
	virtualServeResolveBudget = 40 * time.Millisecond
	defer func() { virtualServeResolveBudget = restore }()

	const pinned = "virtual://movie/tt-serve-budget?result=cand-a"
	file := &models.MediaFile{
		ID:                         42,
		ContentID:                  "movie-serve-budget",
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 7,
	}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, pinned, file.VirtualOwnerInstallationID); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(
		func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			<-ctx.Done()
			return ResolvedVirtualMedia{}, ctx.Err()
		})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil).WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	rec := httptest.NewRecorder()

	started := time.Now()
	handler.HandleStream(rec, req)
	elapsed := time.Since(started)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 provider_unavailable; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want 5", got)
	}
	if !strings.Contains(rec.Body.String(), "provider_unavailable") {
		t.Fatalf("body = %q, want the provider_unavailable code", rec.Body.String())
	}
	if elapsed > time.Second {
		t.Fatalf("serve held the request for %s, want the %s budget", elapsed, virtualServeResolveBudget)
	}
}
