package playback

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// readyRetainedSession writes a real manifest + segment into dir and returns a
// session over it, so the retention tests exercise the same GetManifest /
// OpenSegment read paths the serve route uses.
func readyRetainedSession(t *testing.T, dir string) *TranscodeSession {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"seg_00000.ts", "seg_00001.ts"} {
		segment := filepath.Join(dir, name)
		if err := os.WriteFile(segment, []byte("retained-segment-bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manifest := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:2.000000,\nseg_00000.ts\n#EXTINF:2.000000,\nseg_00001.ts\n"
	if err := os.WriteFile(filepath.Join(dir, "stream.m3u8"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	session := NewTranscodeSessionForTest(dir)
	session.opts = TranscodeOpts{FastStart: true, HWAccel: transcodeHWQSV}
	return session
}

// TestRetireTranscodeSessionPredecessorKeepsSegmentServable proves the core of
// the switchover overlap: a displaced generation's already-produced bytes stay
// readable from the retained directory after its process is stopped.
func TestRetireTranscodeSessionPredecessorKeepsSegmentServable(t *testing.T) {
	m := NewTranscodeManager()
	dir := filepath.Join(t.TempDir(), "gen-old")
	old := readyRetainedSession(t, dir)

	m.RetireTranscodeSessionPredecessor("s1", old, RetainedGenerationRetention)

	retained := m.GetRetainedTranscodeSession("s1")
	if retained == nil {
		t.Fatal("displaced generation was not retained")
	}
	lease, err := retained.OpenSegment("seg_00000.ts")
	if err != nil {
		t.Fatalf("retained generation cannot serve its segment: %v", err)
	}
	_ = lease.Close()
	if _, err := retained.GetManifest(); err != nil {
		t.Fatalf("retained generation cannot serve its manifest: %v", err)
	}
}

// TestRetireTranscodeSessionPredecessorPreservesGenerationToken proves the
// retained predecessor keeps the exact sgen token its in-flight playlist was
// minted with. Retiring stops the process, and if that stop advanced the
// generation the published token would no longer match: every one-behind
// segment request would be refused with 412 and the client would loop on a dead
// URL instead of being served the predecessor's already-produced bytes. This is
// the switchover regression the retained overlap exists to prevent.
func TestRetireTranscodeSessionPredecessorPreservesGenerationToken(t *testing.T) {
	m := NewTranscodeManager()
	dir := filepath.Join(t.TempDir(), "gen-old")
	old := readyRetainedSession(t, dir)
	// The token a manifest builder put on the client's in-flight segment URLs
	// before the switch.
	publishedToken := old.GenerationToken()
	if publishedToken == "" {
		t.Fatal("setup: predecessor has no generation token")
	}

	m.RetireTranscodeSessionPredecessor("s1", old, RetainedGenerationRetention)

	retained := m.GetRetainedTranscodeSession("s1")
	if retained != old {
		t.Fatalf("retained generation = %v, want the displaced predecessor", retained)
	}
	if !retained.MatchesGenerationToken(publishedToken) {
		t.Fatalf("retained predecessor no longer matches the published token %q; a tokened one-behind request would 412", publishedToken)
	}
	// A tokened read of the predecessor's own segment succeeds, so the segment
	// and manifest fences agree on what the retained generation owns.
	lease, err := retained.OpenSegmentForGeneration("seg_00000.ts", publishedToken)
	if err != nil {
		t.Fatalf("retained predecessor refused its own published token: %v", err)
	}
	_ = lease.Close()
}

// TestRetireTranscodeSessionPredecessorRetiresTokenAfterReplacement proves a
// second switch does not leave the first, now-closed predecessor answerable:
// only the most recently displaced bytes stay servable, and its token is the
// one the retained generation matches.
func TestRetireTranscodeSessionPredecessorRetiresTokenAfterReplacement(t *testing.T) {
	m := NewTranscodeManager()
	first := readyRetainedSession(t, filepath.Join(t.TempDir(), "gen-first"))
	firstToken := first.GenerationToken()
	second := readyRetainedSession(t, filepath.Join(t.TempDir(), "gen-second"))

	m.RetireTranscodeSessionPredecessor("s1", first, RetainedGenerationRetention)
	m.RetireTranscodeSessionPredecessor("s1", second, RetainedGenerationRetention)

	retained := m.GetRetainedTranscodeSession("s1")
	if retained != second {
		t.Fatalf("retained generation = %v, want the newest predecessor", retained)
	}
	if retained.MatchesGenerationToken(firstToken) {
		t.Fatal("the replaced predecessor's token still matches the retained generation")
	}
}

// TestRetainedGenerationExpires proves the overlap is bounded: once the
// retention window lapses, the retained entry is reaped (and its directory
// removed) on read, so a stale generation cannot be served forever.
func TestRetainedGenerationExpires(t *testing.T) {
	m := NewTranscodeManager()
	dir := filepath.Join(t.TempDir(), "gen-old")
	old := readyRetainedSession(t, dir)

	m.RetireTranscodeSessionPredecessor("s1", old, 10*time.Millisecond)
	time.Sleep(30 * time.Millisecond)

	if retained := m.GetRetainedTranscodeSession("s1"); retained != nil {
		t.Fatal("an expired retained generation must not be served")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expired retained generation directory still exists: %v", err)
	}
	if got := m.GetRetainedTranscodeSession("s1"); got != nil {
		t.Fatal("expired retained generation reappeared")
	}
}

// TestRetireTranscodeSessionPredecessorReplacesPrior proves a second switch
// does not leak the first displaced directory: only the newest predecessor is
// retained and the previous one is closed (its directory removed).
func TestRetireTranscodeSessionPredecessorReplacesPrior(t *testing.T) {
	m := NewTranscodeManager()
	firstDir := filepath.Join(t.TempDir(), "gen-first")
	first := readyRetainedSession(t, firstDir)
	secondDir := filepath.Join(t.TempDir(), "gen-second")
	second := readyRetainedSession(t, secondDir)

	m.RetireTranscodeSessionPredecessor("s1", first, RetainedGenerationRetention)
	m.RetireTranscodeSessionPredecessor("s1", second, RetainedGenerationRetention)

	retained := m.GetRetainedTranscodeSession("s1")
	if retained != second {
		t.Fatal("the newest displaced generation must be the retained one")
	}
	if _, err := os.Stat(firstDir); !os.IsNotExist(err) {
		t.Fatalf("the replaced retained generation directory still exists: %v", err)
	}
}

// TestRetireTranscodeSessionPredecessorZeroRetentionCloses proves the
// non-positive-retention path restores the pre-overlap behavior: the
// predecessor is closed outright and its directory removed.
func TestRetireTranscodeSessionPredecessorZeroRetentionCloses(t *testing.T) {
	m := NewTranscodeManager()
	dir := filepath.Join(t.TempDir(), "gen-old")
	old := readyRetainedSession(t, dir)

	m.RetireTranscodeSessionPredecessor("s1", old, 0)

	if retained := m.GetRetainedTranscodeSession("s1"); retained != nil {
		t.Fatal("zero retention must not retain the predecessor")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("zero-retention predecessor directory still exists: %v", err)
	}
}

// TestShutdownCleanupClosesRetainedGenerations proves a retained directory does
// not outlive the manager: shutdown closes the live set and every retained
// generation.
func TestShutdownCleanupClosesRetainedGenerations(t *testing.T) {
	m := NewTranscodeManager()
	liveDir := filepath.Join(t.TempDir(), "gen-live")
	m.RegisterTranscodeSession("s1", readyRetainedSession(t, liveDir))
	retiredDir := filepath.Join(t.TempDir(), "gen-retired")
	m.RetireTranscodeSessionPredecessor("s1", readyRetainedSession(t, retiredDir), RetainedGenerationRetention)

	ctx, cancel := context.WithCancel(context.Background())
	done := m.StartShutdownCleanup(ctx)
	cancel()
	<-done

	if _, err := os.Stat(retiredDir); !os.IsNotExist(err) {
		t.Fatalf("retained generation survived shutdown: %v", err)
	}
}

// TestNilRetainedGenerationRegistryIsInert proves the serve-route lookup is
// nil-tolerant: a manager with no retained registry (the pre-feature shape)
// returns zero results and never errors, so the route falls through to its
// pre-existing behavior instead of a 500.
func TestNilRetainedGenerationRegistryIsInert(t *testing.T) {
	var nilManager *TranscodeManager
	if got := nilManager.GetRetainedTranscodeSession("s1"); got != nil {
		t.Fatal("a nil manager must report no retained generation")
	}
	// A manager built as a bare struct literal has a nil retired map; the
	// lookup must not panic or error on it.
	bare := &TranscodeManager{}
	if got := bare.GetRetainedTranscodeSession("s1"); got != nil {
		t.Fatal("a nil retained map must report no retained generation")
	}
	bare.RetireTranscodeSessionPredecessor("s1", nil, RetainedGenerationRetention)
	if got := bare.GetRetainedTranscodeSession("s1"); got != nil {
		t.Fatal("retiring a nil predecessor must not register anything")
	}
}
