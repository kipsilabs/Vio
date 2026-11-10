package playback

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSubtitleProbeScript writes an executable ffprobe stand-in with the given
// shell body under a temp dir and returns its path.
func writeSubtitleProbeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write ffprobe script: %v", err)
	}
	return path
}

// TestProbeSubtitleLayoutWithinSurvivesAProviderSpike proves a layout probe
// that needs longer than the old fixed 5s wall succeeds when the caller grants
// an adaptive budget for the slow link. Production saw a 5.0s hard failure
// against provider first-byte spikes of 5.5-8.7s while the same probe normally
// reads a header in 0.7-1.9s.
func TestProbeSubtitleLayoutWithinSurvivesAProviderSpike(t *testing.T) {
	bin := writeSubtitleProbeScript(t, `sleep 5.2
printf '%s' '{"streams":[{"index":2,"codec_name":"subrip","codec_type":"subtitle","tags":{"language":"eng"}}]}'
`)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	tracks, err := ProbeSubtitleLayoutWithin(ctx, bin, "/media/movie.mkv", 9*time.Second)
	if err != nil {
		t.Fatalf("probe with an extended budget failed: %v", err)
	}
	if len(tracks) != 1 || tracks[0].Index != 2 || tracks[0].Language != "en" {
		t.Fatalf("tracks = %#v, want the one English subrip track the probe printed", tracks)
	}
}

// TestProbeSubtitleLayoutWithinFailsFastOnADeadSource proves the extended
// budget is not a fixed wait: a source that cannot be probed at all still
// returns its error immediately instead of holding the request to the bound.
func TestProbeSubtitleLayoutWithinFailsFastOnADeadSource(t *testing.T) {
	bin := writeSubtitleProbeScript(t, "exit 1\n")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	started := time.Now()
	if _, err := ProbeSubtitleLayoutWithin(ctx, bin, "/media/movie.mkv", 10*time.Second); err == nil {
		t.Fatal("a dead source probed successfully")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("dead source probe took %v, want a fail-fast error well inside the budget", elapsed)
	}
}

// TestProbeSubtitleLayoutWithinSkipsWhenNoBudgetIsLeft proves a caller with no
// remaining budget does not spawn ffprobe at all, so it fails fast with an
// honest "no budget" answer instead of a mid-read kill.
func TestProbeSubtitleLayoutWithinSkipsWhenNoBudgetIsLeft(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	bin := writeSubtitleProbeScript(t, "touch "+marker+"\n")
	if _, err := ProbeSubtitleLayoutWithin(context.Background(), bin, "/media/movie.mkv", 0); err == nil {
		t.Fatal("a zero-budget probe reported success")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("ffprobe was spawned with no probe budget")
	}
}
