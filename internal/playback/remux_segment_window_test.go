package playback

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A backward seek inside the retention window that reuses an identical copy
// (remux) recipe must keep the already-produced segments on disk so the read is
// a file serve instead of a fresh FFmpeg spawn.
func TestRemuxWindowHitSeekKeepsRetainedSegments(t *testing.T) {
	dir := t.TempDir()
	opts := TranscodeOpts{
		SessionID:               "remux-window",
		TargetCodecVideo:        "copy",
		TargetCodecAudio:        "aac",
		AudioTrackIndex:         1,
		SegmentDuration:         2,
		SegmentRetentionSeconds: 600,
	}
	writeManifestRange(t, dir, 5, 9, hlsSegmentExtension(opts))
	for i := 5; i <= 9; i++ {
		writeSegmentFile(t, dir, segmentFilename(i, opts), []byte("window bytes"), time.Now())
	}

	session := &TranscodeSession{outputDir: dir, opts: opts, runningRecipe: emittedRecipeOf(opts)}

	// A seek back to segment 7 re-emits the identical recipe.
	if session.cleanStaleOutputForRestart(emittedRecipeOf(opts), 9) {
		t.Fatal("an identical-recipe remux restart discarded retained segments")
	}
	if _, err := os.Stat(filepath.Join(dir, "stream.m3u8")); err != nil {
		t.Fatalf("retained manifest was removed: %v", err)
	}
	for i := 5; i <= 9; i++ {
		name := segmentFilename(i, opts)
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("retained segment %s was removed: %v", name, err)
		}
	}

	// The in-window segment is served from the retained file.
	lease, err := session.OpenSegment(segmentFilename(7, opts))
	if err != nil {
		t.Fatalf("window-hit segment was not served from disk: %v", err)
	}
	if got := lease.Info.Size(); got != int64(len("window bytes")) {
		t.Fatalf("served segment size = %d, want %d", got, len("window bytes"))
	}
	_ = lease.Close()
}

// A recipe change invalidates retained remux segments: they were produced by a
// different byte recipe, so serving them would mix generations.
func TestRemuxRecipeChangeDiscardsRetainedSegments(t *testing.T) {
	dir := t.TempDir()
	previous := TranscodeOpts{
		SessionID:               "remux-recipe",
		TargetCodecVideo:        "copy",
		TargetCodecAudio:        "aac",
		AudioTrackIndex:         1,
		SegmentDuration:         2,
		SegmentRetentionSeconds: 600,
	}
	writeManifestRange(t, dir, 5, 9, hlsSegmentExtension(previous))
	for i := 5; i <= 9; i++ {
		writeSegmentFile(t, dir, segmentFilename(i, previous), []byte("old recipe"), time.Now())
	}

	session := &TranscodeSession{outputDir: dir, opts: previous, runningRecipe: emittedRecipeOf(previous)}

	next := previous
	next.AudioTrackIndex = 2 // a different track re-renders every segment

	if !session.cleanStaleOutputForRestart(emittedRecipeOf(next), 9) {
		t.Fatal("a changed remux recipe kept stale segments")
	}
	if _, err := os.Stat(filepath.Join(dir, "stream.m3u8")); err == nil {
		t.Fatal("the previous generation's manifest survived a recipe change")
	}
	for i := 9; i <= 9; i++ {
		if _, err := os.Stat(filepath.Join(dir, segmentFilename(i, previous))); err == nil {
			t.Fatalf("segment %d survived a recipe change", i)
		}
	}
	// Retained segments below the restart point still survive the clean.
	if _, err := os.Stat(filepath.Join(dir, segmentFilename(5, previous))); err != nil {
		t.Fatalf("pre-restart segment was removed by the clean: %v", err)
	}
}

// A planned (keyframe VOD) stream keeps its prefix for backward seeking only
// while the replacement emits the same recipe. An audio switch changes the
// emitted audio, so a restart that lands at segment 7 must discard segments
// 0..6 as well: they still hold the pre-switch track under numbers the
// replacement generation serves, and a backward seek would deliver them as
// current media.
func TestPlannedRestartDiscardsPrefixOnRecipeChange(t *testing.T) {
	tests := []struct {
		name        string
		switchAudio bool
		wantKept    int
	}{
		{name: "unchanged recipe keeps the prefix", wantKept: 7},
		{name: "audio switch discards the prefix", switchAudio: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			truePath := trueBinaryPath(t)
			dir := t.TempDir()
			opts := TranscodeOpts{
				OutputDir:           dir,
				TargetCodecVideo:    "copy",
				TargetCodecAudio:    "aac",
				AudioTrackIndex:     1,
				SourceAudioChannels: 6,
				SegmentDuration:     2,
				FFmpegPath:          truePath,
			}
			plan := &copySegmentPlan{durations: make([]float64, 10)}
			for i := range plan.durations {
				plan.durations[i] = 2
			}
			for i := range plan.durations {
				writeSegmentFile(t, dir, segmentFilename(i, opts), []byte("old track"), time.Now())
			}
			writeManifestRange(t, dir, 0, len(plan.durations)-1, hlsSegmentExtension(opts))

			session := &TranscodeSession{
				outputDir:     dir,
				opts:          opts,
				runningRecipe: emittedRecipeOf(opts),
				copyPlan:      plan,
			}
			if tc.switchAudio {
				session.SetAudioTrackIndex(2)
				session.SetSourceAudioChannels(2)
			}
			if err := session.Restart(context.Background(), 14, 7); err != nil {
				t.Fatalf("Restart: %v", err)
			}
			t.Cleanup(func() { _ = session.Close() })

			for i := range plan.durations {
				_, err := os.Stat(filepath.Join(dir, segmentFilename(i, opts)))
				if i < tc.wantKept {
					if err != nil {
						t.Fatalf("segment %d below the restart point was removed: %v", i, err)
					}
					continue
				}
				if !os.IsNotExist(err) {
					t.Fatalf("segment %d survived the restart", i)
				}
			}
		})
	}
}

// An audio selection changed before Restart mutates s.opts, but the segments on
// disk were emitted by the running process's earlier selection. Comparing the
// replacement against s.opts would see no change and retain old-generation
// audio; the decision must use the running process's frozen recipe.
func TestRemuxAudioSelectionChangeBeforeRestartDiscardsRetainedSegments(t *testing.T) {
	dir := t.TempDir()
	running := TranscodeOpts{
		SessionID:               "remux-audio-switch",
		TargetCodecVideo:        "copy",
		TargetCodecAudio:        "aac",
		AudioTrackIndex:         1,
		SourceAudioChannels:     6,
		SegmentDuration:         2,
		SegmentRetentionSeconds: 600,
	}
	writeManifestRange(t, dir, 5, 9, hlsSegmentExtension(running))
	for i := 5; i <= 9; i++ {
		writeSegmentFile(t, dir, segmentFilename(i, running), []byte("old audio"), time.Now())
	}

	session := &TranscodeSession{outputDir: dir, opts: running, runningRecipe: emittedRecipeOf(running)}

	// The pre-restart mutation path an audio switch uses: it edits s.opts, not
	// the running recipe.
	session.SetAudioTrackIndex(2)
	session.SetSourceAudioChannels(2)

	// The replacement recipe mirrors the mutated opts exactly as restart
	// computes it; retention must still compare against the running recipe.
	if !session.cleanStaleOutputForRestart(emittedRecipeOf(session.opts), 9) {
		t.Fatal("an audio selection change before restart retained old-generation segments")
	}
}

// The remux recipe comparison covers packaging and audio choices that change
// the emitted bytes even when the video codec string is unchanged.
func TestRemuxRecipeComparisonCoversPackagingAndAudio(t *testing.T) {
	base := TranscodeOpts{TargetCodecVideo: "copy", TargetCodecAudio: "aac", AudioTrackIndex: 1, TargetAudioChannels: 2}

	same := base
	same.SeekSeconds = 42
	same.StartSegmentNumber = 7
	same.FastStart = true
	if emittedRecipeOf(base) != emittedRecipeOf(same) {
		t.Fatal("a restart that only moves the seek position changed the emitted recipe")
	}

	for name, mutate := range map[string]func(*TranscodeOpts){
		"audio track":       func(o *TranscodeOpts) { o.AudioTrackIndex = 2 },
		"audio codec":       func(o *TranscodeOpts) { o.TargetCodecAudio = "copy" },
		"audio channels":    func(o *TranscodeOpts) { o.TargetAudioChannels = 6 },
		"source channels":   func(o *TranscodeOpts) { o.SourceAudioChannels = 6 },
		"mpegts packaging":  func(o *TranscodeOpts) { o.CopyVideoMPEGTS = true },
		"sample entry":      func(o *TranscodeOpts) { o.VideoSampleEntry = VideoSampleEntryHVC1 },
		"copy recipe":       func(o *TranscodeOpts) { o.CopyFMP4RecipeVersion = CopyFMP4RecipeVersion },
		"subtitle burn-in":  func(o *TranscodeOpts) { o.SubtitleBurnIn = true },
		"segment duration":  func(o *TranscodeOpts) { o.SegmentDuration = 6 },
		"resolution":        func(o *TranscodeOpts) { o.TargetResolution = "720p" },
		"video bitrate cap": func(o *TranscodeOpts) { o.TargetBitrateKbps = 4000 },
	} {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			if emittedRecipeOf(base) == emittedRecipeOf(changed) {
				t.Fatalf("changing %s did not change the emitted recipe comparison", name)
			}
		})
	}
}
