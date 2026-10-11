package playback

import (
	"context"
	"testing"
	"time"
)

// TestWaitForManifestContextPollsReportsReadyCount pins that a manifest already
// on disk is reported after a single poll, and the manifest bytes are identical
// to WaitForManifestContext's.
func TestWaitForManifestContextPollsReportsReadyCount(t *testing.T) {
	dir := t.TempDir()
	opts := TranscodeOpts{
		OutputDir:          dir,
		SessionID:          "polls-ready",
		SegmentDuration:    2,
		TargetCodecVideo:   "h264",
		TargetCodecAudio:   "aac",
		AudioTrackIndex:    -1,
		SubtitleTrackIndex: -1,
	}
	s, err := NewReadyTranscodeSessionForTesting(dir, opts)
	if err != nil {
		t.Fatalf("ready fixture: %v", err)
	}

	manifest, polls, err := s.WaitForManifestContextPolls(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("WaitForManifestContextPolls: %v", err)
	}
	if len(manifest) == 0 {
		t.Fatal("WaitForManifestContextPolls returned an empty manifest")
	}
	if polls != 1 {
		t.Fatalf("polls = %d, want 1 for a manifest already on disk", polls)
	}

	plain, plainErr := s.WaitForManifestContext(context.Background(), 5*time.Second)
	if plainErr != nil {
		t.Fatalf("WaitForManifestContext: %v", plainErr)
	}
	if string(plain) != string(manifest) {
		t.Fatal("the poll-count variant changed the returned manifest")
	}
}

// TestWaitForManifestContextPollsCountsWhileWaiting pins that the count grows
// across the 100ms ticks while the manifest is missing, and the wait still ends
// at the timeout as before.
func TestWaitForManifestContextPollsCountsWhileWaiting(t *testing.T) {
	s := &TranscodeSession{outputDir: t.TempDir(), running: true}
	start := time.Now()
	_, polls, err := s.WaitForManifestContextPolls(context.Background(), 250*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("WaitForManifestContextPolls returned no error with no manifest")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("wait ran %v, want the 250ms timeout", elapsed)
	}
	if polls < 2 {
		t.Fatalf("polls = %d, want at least 2 across a 250ms wait", polls)
	}
}
