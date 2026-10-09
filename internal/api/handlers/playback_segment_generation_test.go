package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// A generation-scoped segment request that a matching retained generation
// cannot answer is terminal: the live generation is not the stream the URL
// named, so neither waiting on it nor restarting it can produce servable bytes.
// The handler must reject with the stale-generation verdict instead of running
// the missing-segment recovery against the live generation.
func TestTranscodeSegmentStaleRetainedGenerationIsTerminal(t *testing.T) {
	const sessionID = "stale-retained-session"

	liveDir := t.TempDir()
	live := playback.NewTranscodeSessionForTest(liveDir)
	if err := os.WriteFile(filepath.Join(liveDir, "seg_0.m4s"), []byte("new-gen-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The displaced generation holds seg_5 but never produced the seg_9 the
	// stale playlist asks for.
	oldDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(oldDir, "seg_5.m4s"), []byte("old-gen-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := playback.NewTranscodeSessionForTest(oldDir)

	sessions := playback.NewSessionManager(0, 0)
	sessions.RegisterReconstructed(&playback.Session{
		ID:          sessionID,
		UserID:      1,
		MediaFileID: 91,
		PlayMethod:  playback.PlayTranscode,
	})
	handler := NewPlaybackHandler(sessions)
	handler.TranscodeManager().RegisterTranscodeSession(sessionID, live)
	handler.TranscodeManager().RetireTranscodeSessionPredecessor(sessionID, old, playback.RetainedGenerationRetention)

	// The URL names the retained generation, which does not own the segment.
	oldToken := old.GenerationToken()
	if live.MatchesGenerationToken(oldToken) {
		t.Fatal("test setup: live generation unexpectedly matches the retained token")
	}

	const segmentName = "seg_9.m4s"
	rec := httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(rec, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+sessionID+"/segment/"+segmentName+"?sgen="+url.QueryEscape(oldToken),
		nil,
		map[string]string{"session_id": sessionID, "name": segmentName},
	))
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want %d (stale generation); body = %s", rec.Code, http.StatusPreconditionFailed, rec.Body.String())
	}
}

// TestRetainedPredecessorServesItsPublishedTokenAfterTwoRapidReplacements
// replays the two-seek prod sequence (incarnation :08aaa866 -> :e7f731d0 ->
// :7854dd0b) where the client is permanently one behind: the token it holds
// names the generation the previous replan displaced. Retiring that predecessor
// must keep its published token answerable for the bounded overlap, so the
// one-behind frag request is served from its already-produced bytes instead of
// 412-ing. If the retire stop advanced the generation, the token would never
// match and the client would loop until failure_recovery terminally killed a
// healthy session.
func TestRetainedPredecessorServesItsPublishedTokenAfterTwoRapidReplacements(t *testing.T) {
	const sessionID = "retained-two-deep-session"

	// Generations in publication order: gen1 is the original live, gen2 is the
	// first replacement, gen3 is the second. Each writes a distinct segment so
	// the served bytes identify which generation answered.
	gen1Dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(gen1Dir, "seg_9.m4s"), []byte("gen1-bytes"), 0o644)
	gen1 := playback.NewTranscodeSessionForTest(gen1Dir)

	gen2Dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(gen2Dir, "seg_9.m4s"), []byte("gen2-bytes"), 0o644)
	gen2 := playback.NewTranscodeSessionForTest(gen2Dir)

	gen3Dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(gen3Dir, "seg_9.m4s"), []byte("gen3-bytes"), 0o644)
	gen3 := playback.NewTranscodeSessionForTest(gen3Dir)

	sessions := playback.NewSessionManager(0, 0)
	sessions.RegisterReconstructed(&playback.Session{
		ID:          sessionID,
		UserID:      1,
		MediaFileID: 91,
		PlayMethod:  playback.PlayTranscode,
	})
	handler := NewPlaybackHandler(sessions)

	// First replacement: gen1 is displaced and retained. Capture the token the
	// client's in-flight playlist was minted with, before the retire stops it.
	handler.TranscodeManager().RegisterTranscodeSession(sessionID, gen1)
	gen1Token := gen1.GenerationToken()
	handler.TranscodeManager().SwapTranscodeSession(sessionID, gen2)
	handler.TranscodeManager().RetireTranscodeSessionPredecessor(sessionID, gen1, playback.RetainedGenerationRetention)
	if !handler.TranscodeManager().GetRetainedTranscodeSession(sessionID).MatchesGenerationToken(gen1Token) {
		t.Fatal("test fixture: retained gen1 does not match the token its playlist published")
	}

	// Second replacement before the client adopts: gen2 displaces gen1 in the
	// retained slot (only the newest predecessor is retained); gen3 is live.
	handler.TranscodeManager().SwapTranscodeSession(sessionID, gen3)
	handler.TranscodeManager().RetireTranscodeSessionPredecessor(sessionID, gen2, playback.RetainedGenerationRetention)

	// The client's one-behind token names gen1, which the second retirement
	// replaced. The retained slot now holds gen2, so this is a defined stale
	// verdict rather than a wrong-generation serve: only the most recent
	// predecessor is retained and gen1's bytes are gone.
	const segmentName = "seg_9.m4s"
	rec := httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(rec, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+sessionID+"/segment/"+segmentName+"?sgen="+url.QueryEscape(gen1Token),
		nil,
		map[string]string{"session_id": sessionID, "name": segmentName},
	))
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("gen1 (two-deep) token = %d %q, want the defined %d stale verdict",
			rec.Code, rec.Body.String(), http.StatusPreconditionFailed)
	}

	// The client adopts gen2 next; gen2's published token is still servable from
	// the retained predecessor immediately after the replacement, which is the
	// one-behind recovery the fix protects.
	gen2PublishedToken := gen2.GenerationToken()
	rec = httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(rec, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+sessionID+"/segment/"+segmentName+"?sgen="+url.QueryEscape(gen2PublishedToken),
		nil,
		map[string]string{"session_id": sessionID, "name": segmentName},
	))
	if rec.Code != http.StatusOK || rec.Body.String() != "gen2-bytes" {
		t.Fatalf("retained gen2 token = %d %q, want 200 gen2-bytes from the retained predecessor",
			rec.Code, rec.Body.String())
	}

	// The live generation serves its own window under its own token.
	gen3Token := gen3.GenerationToken()
	rec = httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(rec, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+sessionID+"/segment/"+segmentName+"?sgen="+url.QueryEscape(gen3Token),
		nil,
		map[string]string{"session_id": sessionID, "name": segmentName},
	))
	if rec.Code != http.StatusOK || rec.Body.String() != "gen3-bytes" {
		t.Fatalf("live gen3 token = %d %q, want 200 gen3-bytes", rec.Code, rec.Body.String())
	}
}

// TestRetainedPredecessorNeverServesAcrossSegmentsItDidNotProduce proves the
// retained fallback stays tight: a tokened request for a segment the retained
// predecessor never wrote is refused with the stale verdict rather than served
// from the live generation or a wrong file.
func TestRetainedPredecessorNeverServesAcrossSegmentsItDidNotProduce(t *testing.T) {
	const sessionID = "retained-segment-fence"

	liveDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(liveDir, "seg_5.m4s"), []byte("live-bytes"), 0o644)
	live := playback.NewTranscodeSessionForTest(liveDir)

	oldDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(oldDir, "seg_3.m4s"), []byte("old-bytes"), 0o644)
	old := playback.NewTranscodeSessionForTest(oldDir)

	sessions := playback.NewSessionManager(0, 0)
	sessions.RegisterReconstructed(&playback.Session{
		ID:          sessionID,
		UserID:      1,
		MediaFileID: 91,
		PlayMethod:  playback.PlayTranscode,
	})
	handler := NewPlaybackHandler(sessions)
	handler.TranscodeManager().RegisterTranscodeSession(sessionID, live)
	oldToken := old.GenerationToken()
	handler.TranscodeManager().RetireTranscodeSessionPredecessor(sessionID, old, playback.RetainedGenerationRetention)

	// The retained predecessor holds seg_3, not the seg_5 the stale playlist
	// asks for: the request must not borrow the live generation's seg_5 bytes.
	const segmentName = "seg_5.m4s"
	rec := httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(rec, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+sessionID+"/segment/"+segmentName+"?sgen="+url.QueryEscape(oldToken),
		nil,
		map[string]string{"session_id": sessionID, "name": segmentName},
	))
	if rec.Code == http.StatusOK {
		t.Fatalf("stale token borrowed another generation's segment: %d %q", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale token for an unproduced segment = %d %q, want the stale verdict",
			rec.Code, rec.Body.String())
	}
}
