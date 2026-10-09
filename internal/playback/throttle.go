// internal/playback/throttle.go
package playback

import (
	"context"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// throttleCheckInterval is how often the throttler checks the gap.
	throttleCheckInterval = 5 * time.Second

	// minThresholdSeconds is the minimum allowed throttle threshold.
	minThresholdSeconds = 60
)

// TranscodeThrottleSettings reads the server settings controlling how far
// FFmpeg may run ahead of a client.
type TranscodeThrottleSettings interface {
	Get(context.Context, string) (string, error)
}

// TranscodeThrottleStarter is implemented by TranscodeSession and kept small
// so every playback frontend can share the settings policy.
type TranscodeThrottleStarter interface {
	StartThrottler(int)
}

// ConfiguredTranscodeThrottleSeconds resolves the configured forward-buffer
// duration. Zero means throttling is disabled. The resolved value can cross a
// node boundary without giving the executor access to the API server's settings
// store.
//
// Throttling is enabled by default: only an explicit "false" turns it off, and
// an absent row (the state of a fresh install, since none is seeded) resolves to
// the default rather than to off, or the advertised default would never take
// effect at runtime — the reader sees the raw store value, not the admin UI's
// effective default.
//
// A settings read failure disables throttling instead of enabling it. Treating a
// failed read as "unset" would turn a transient store outage into an unrequested
// behavior change; failing closed keeps playback at its prior, unbounded
// lookahead until the store is reachable again.
func ConfiguredTranscodeThrottleSeconds(ctx context.Context, settings TranscodeThrottleSettings) int {
	if settings == nil {
		return 0
	}
	enabled, err := settings.Get(ctx, "enable_transcode_throttle")
	if err != nil {
		return 0
	}
	if strings.EqualFold(strings.TrimSpace(enabled), "false") {
		return 0
	}
	raw, err := settings.Get(ctx, "transcode_throttle_seconds")
	if err != nil {
		// The threshold read failed, so the intended forward-buffer duration is
		// unknown. Fail closed like an enable-read failure rather than silently
		// arming the 300-second default.
		return 0
	}
	threshold := 300
	if raw != "" {
		if configured, parseErr := strconv.Atoi(raw); parseErr == nil && configured > 0 {
			threshold = max(configured, minThresholdSeconds)
		}
	}
	return threshold
}

// StartConfiguredTranscodeThrottler starts throttling when enabled, using the
// configured forward-buffer duration or the 300-second default.
func StartConfiguredTranscodeThrottler(ctx context.Context, settings TranscodeThrottleSettings, starter TranscodeThrottleStarter) {
	if starter == nil {
		return
	}
	if threshold := ConfiguredTranscodeThrottleSeconds(ctx, settings); threshold > 0 {
		starter.StartThrottler(threshold)
	}
}

// TranscodeThrottler pauses and resumes an FFmpeg process by sending
// interactive commands to its stdin. It monitors the gap
// between the transcode position (segments produced) and the client's
// download position (highest segment fetched).
type TranscodeThrottler struct {
	session          *TranscodeSession
	stdinPipe        io.WriteCloser
	thresholdSeconds int
	segmentDuration  int
	paused           bool
	stopCh           chan struct{}
	mu               sync.Mutex
}

// NewTranscodeThrottler creates a throttler. thresholdSeconds is clamped
// to a minimum of 60.
func NewTranscodeThrottler(session *TranscodeSession, stdinPipe io.WriteCloser, thresholdSeconds, segmentDuration int) *TranscodeThrottler {
	if thresholdSeconds < minThresholdSeconds {
		thresholdSeconds = minThresholdSeconds
	}
	return &TranscodeThrottler{
		session:          session,
		stdinPipe:        stdinPipe,
		thresholdSeconds: thresholdSeconds,
		segmentDuration:  segmentDuration,
		stopCh:           make(chan struct{}),
	}
}

// Start launches the background check goroutine.
func (t *TranscodeThrottler) Start() {
	go t.run()
}

// Stop signals the check goroutine to exit. If FFmpeg is currently paused,
// it sends a resume command before stopping.
func (t *TranscodeThrottler) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()

	select {
	case <-t.stopCh:
		return // already stopped
	default:
	}

	if t.paused {
		t.sendResume()
	}
	close(t.stopCh)
}

func (t *TranscodeThrottler) run() {
	ticker := time.NewTicker(throttleCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			if !t.session.IsRunning() {
				return
			}
			t.CheckOnce()
		}
	}
}

// CheckOnce performs a single throttle check. Exported for testing.
func (t *TranscodeThrottler) CheckOnce() {
	progress := t.session.SegmentProgress(time.Now())
	// Measure the forward buffer against media the CURRENT generation actually
	// produced, not against every segment the shared output directory holds. On
	// a restart that keeps an identical recipe's segments, ProducedHead includes
	// the retained window ahead of the replacement process; pausing on it would
	// stop a stable play before the new process wrote anything, and the manifest
	// would never refresh, deadlocking the stream until the user seeks. The
	// per-segment mtime fence (see SegmentProgress.ProducedHeadCurrentGeneration)
	// is generation-aware even when the manifest's own mtime looks fresh.
	head := progress.ProducedHead
	if !progress.GenerationStartedAt.IsZero() {
		head = progress.ProducedHeadCurrentGeneration
	}
	if head < progress.StartSegmentNumber {
		// This generation has produced nothing yet. Resume if an earlier check
		// paused on a prior generation's output, then wait for real progress.
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.paused {
			log.Printf("playback: throttler resuming ffmpeg (produced output predates current generation)")
			t.sendResume()
			t.paused = false
		}
		return
	}

	gapSegments := head - progress.LastRequestedSegment
	segmentDuration := progress.SegmentDuration
	if segmentDuration <= 0 {
		segmentDuration = t.segmentDuration
	}
	gap := gapSegments * segmentDuration

	t.mu.Lock()
	defer t.mu.Unlock()

	if gap >= t.thresholdSeconds && !t.paused {
		log.Printf("playback: throttler pausing ffmpeg (gap=%ds, threshold=%ds)", gap, t.thresholdSeconds)
		t.sendPause()
		t.paused = true
	} else if gap < t.thresholdSeconds && t.paused {
		log.Printf("playback: throttler resuming ffmpeg (gap=%ds, threshold=%ds)", gap, t.thresholdSeconds)
		t.sendResume()
		t.paused = false
	}
}

// progressPredatesGeneration reports whether the produced head was read from a
// manifest written before the current ffmpeg process started, i.e. by an
// earlier generation of this session or by a previous session that shared the
// output directory. A session with no generation timestamp (never started a
// process, as in tests) carries no staleness information and is treated as
// current.
func progressPredatesGeneration(progress SegmentProgress) bool {
	if progress.GenerationStartedAt.IsZero() || !progress.HasManifest {
		return false
	}
	return progress.ProducedHeadCurrentGeneration < progress.StartSegmentNumber
}

func (t *TranscodeThrottler) sendPause() {
	t.sendCommand("p")
}

func (t *TranscodeThrottler) sendResume() {
	t.sendCommand("u")
}

// sendCommand writes an interactive FFmpeg command to stdin. Errors are logged
// but not returned, which handles dead pipes from externally killed FFmpeg.
func (t *TranscodeThrottler) sendCommand(command string) {
	if _, err := t.stdinPipe.Write([]byte(command)); err != nil {
		log.Printf("playback: throttler stdin write error (ffmpeg may have exited): %v", err)
	}
}
