package playback

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// SubtitlePrefetchWindow names one bounded source-time window that
// PrefetchWindowsInBackground may extract and keep as a last-good window.
type SubtitlePrefetchWindow struct {
	StartSeconds    float64
	DurationSeconds float64
}

// subtitlePrefetchMaxWindows bounds the work one prefetch kick may do. A
// prefetch exists to cover the viewer's next page or two; anything wider is
// the full-track warm's job (or the client's own window requests).
const subtitlePrefetchMaxWindows = 2

// PrefetchWindowsInBackground fills the bounded window cache for the windows a
// mid-file playhead is about to need, using the ordinary windowed extract path.
//
// It is the safe alternative to starting a full-track warm mid-file. A
// full-track warm always extracts from source zero and commits under the
// whole-track artifact key; starting one at a playhead would publish a tail
// slice as the complete track, with offsets that do not match the source
// timeline and without the cues before the seek. A bounded prefetch never
// commits an artifact: each window goes through ServeExtractWithResult, which
// stores the slice under windowResultKey (or its covering-window family) and
// lets fix 2's covering lookup serve the next request without another extract.
//
// The prefetch is deliberately opportunistic, like every other detached warm:
//
//   - It skips while the request path is shedding for the source, so it never
//
// adds a demux to a relay pipe that is already refusing client reads.
//
//   - It draws from the detached admission budget and shares warmSem with the
//     full-track warms, so a burst of kicks and an idle warm cannot stack
//     background demuxes server-wide. No capacity means the kick is dropped; the
//     next window miss re-attempts it.
//
//   - At most subtitlePrefetchMaxWindows windows run per kick, and identical
//     concurrent windows coalesce through the cache's window flight.
//
// windows beyond the cap are ignored. The returned channel closes exactly once
// when the prefetch settles or is skipped.
func (c *SubtitleCache) PrefetchWindowsInBackground(opts StreamExtractOpts, windows []SubtitlePrefetchWindow, extract SUPExtractFunc, requestID string) <-chan struct{} {
	done := make(chan struct{})
	format := subtitleCacheFormat(opts.SourceCodec, opts.TargetFormat)
	if c == nil || extract == nil || c.dir() == "" || format == "" || len(windows) == 0 {
		close(done)
		return done
	}
	if len(windows) > subtitlePrefetchMaxWindows {
		windows = windows[:subtitlePrefetchMaxWindows]
	}
	if relayDemuxRequestPathShedding(opts.InputPath, subtitleWarmRequestShedQuietWindow) {
		slog.Debug("subtitle window prefetch skipped: request-path admission is shedding",
			"input", opts.InputPath, "track", opts.TrackIndex, "format", format)
		close(done)
		return done
	}
	select {
	case c.warmSem <- struct{}{}:
	default:
		slog.Debug("subtitle window prefetch skipped: all warm slots busy",
			"input", opts.InputPath, "track", opts.TrackIndex, "format", format)
		close(done)
		return done
	}

	// The prefetch is always detached and never starts a full-track fill of its
	// own: the window slice is the only output, and a mid-file fill under the
	// artifact key is exactly what this path must not create.
	opts.DisableBackgroundWarm = true
	opts.DetachedRelayDemux = true
	opts.PinnedTextArtifact = nil
	opts.Writer = nil
	opts.SeekSeconds = 0
	opts.DurationSeconds = 0
	opts.WindowRequested = false
	opts.AllowWindow = false
	opts.InputIsExtractedSup = false
	opts.InputIsExtractedText = ""
	windows = append([]SubtitlePrefetchWindow(nil), windows...)

	go func() {
		defer close(done)
		defer func() { <-c.warmSem }()
		for _, window := range windows {
			if window.DurationSeconds <= 0 {
				continue
			}
			if relayDemuxRequestPathShedding(opts.InputPath, subtitleWarmRequestShedQuietWindow) {
				slog.Debug("subtitle window prefetch stopped: request-path admission is shedding",
					"input", opts.InputPath, "track", opts.TrackIndex, "format", format)
				return
			}
			windowOpts := opts
			windowOpts.SeekSeconds = window.StartSeconds
			windowOpts.DurationSeconds = window.DurationSeconds
			windowOpts.WindowRequested = true
			// PGS windows only when AllowWindow is set; text and ASS honor
			// WindowRequested. The cache still stores the slice under
			// windowResultKey, never under the whole-track artifact key.
			windowOpts.AllowWindow = true
			key, ok := c.windowResultKey(windowOpts, format)
			if !ok {
				continue
			}
			if _, cached := c.lookupWindowResult(key); cached {
				continue
			}
			if _, covering := c.lookupCoveringWindowResult(windowResultKeyBase(key), windowOpts); covering {
				continue
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
			if err != nil {
				return
			}
			start := time.Now()
			if _, err := c.ServeExtractWithResult(&subtitlePrefetchWriter{header: make(http.Header)}, req, windowOpts, extract); err != nil {
				// One failure ends the kick: a rekey, a shed detached slot, or
				// an upstream failure all mean the next window miss is a better
				// trigger than another demux here.
				slog.Debug("subtitle window prefetch stopped",
					"input", opts.InputPath, "track", opts.TrackIndex, "format", format,
					"start", window.StartSeconds, "duration", window.DurationSeconds,
					"request_id", requestID, "error", err)
				return
			}
			slog.Debug("subtitle window prefetched",
				"input", opts.InputPath, "track", opts.TrackIndex, "format", format,
				"start", window.StartSeconds, "duration", window.DurationSeconds,
				"elapsed", time.Since(start), "request_id", requestID)
		}
	}()
	return done
}

// subtitlePrefetchWriter is a sink for a prefetched window. The window capture
// in ServeExtractWithResult keeps the bytes; nothing is delivered to a client,
// so Write only reports success.
type subtitlePrefetchWriter struct {
	header http.Header
}

func (w *subtitlePrefetchWriter) Header() http.Header { return w.header }

func (w *subtitlePrefetchWriter) Write(p []byte) (int, error) { return len(p), nil }

func (w *subtitlePrefetchWriter) WriteHeader(int) {}
