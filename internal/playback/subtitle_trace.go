package playback

import (
	"context"
	"io"
	"net/http"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"
)

// Subtitle delivery has two distinct costs that a single end-to-end timer
// cannot separate: the provider fetch (or container demux) that produces the
// bytes, and the convert into the delivered format (for example subrip → VTT).
// A trace names the phase so a slow or failed subtitle is attributable to one
// of them instead of to a bare duration. Serve, warm and notify cover the
// non-extract paths that move the same bytes.
const (
	SubtitleTracePhaseFetch   = "fetch"
	SubtitleTracePhaseConvert = "convert"
	SubtitleTracePhaseServe   = "serve"
	SubtitleTracePhaseWarm    = "warm"
	SubtitleTracePhaseNotify  = "notify"
)

// Subtitle trace outcomes form a closed set so a consumer can count them
// without parsing prose. Committed/commit_failed describe the cache write that
// follows a successful extract; unsupported names a codec with no delivery
// route; skipped names a warm or event that was deliberately not attempted.
const (
	SubtitleTraceOutcomeSuccess      = "success"
	SubtitleTraceOutcomeFailure      = "failure"
	SubtitleTraceOutcomeCanceled     = "canceled"
	SubtitleTraceOutcomeCacheHit     = "cache_hit"
	SubtitleTraceOutcomeCacheMiss    = "cache_miss"
	SubtitleTraceOutcomeSkipped      = "skipped"
	SubtitleTraceOutcomeUnsupported  = "unsupported"
	SubtitleTraceOutcomeCommitted    = "committed"
	SubtitleTraceOutcomeCommitFailed = "commit_failed"
	// The notifier's delivery traces share the same field set even though no
	// bytes move: an encode or a send that failed is still an outcome.
	SubtitleTraceOutcomeEncodeFailed   = "encode_failed"
	SubtitleTraceOutcomeDeliveryFailed = "delivery_failed"
)

// SubtitleTraceBytesUnknown marks a trace whose byte count was not measured, so
// the field is omitted rather than reported as a real zero.
const SubtitleTraceBytesUnknown = -1

// subtitleTraceRequestID returns the API request ID the middleware stored, so a
// subtitle trace joins the playback start or stream line that caused it. It is
// empty for a detached warm and in tests, both of which have no request.
func subtitleTraceRequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	return chimw.GetReqID(ctx)
}

// subtitleTraceFields builds the stable key/value trace every subtitle phase
// shares: request ID (where known), codec (where known), phase, measured bytes,
// outcome, and elapsed time. A field is omitted rather than emitted empty, so a
// consumer never has to distinguish "unknown" from a real value.
func subtitleTraceFields(ctx context.Context, codec, phase string, bytes int64, outcome string, elapsed time.Duration) []any {
	attrs := make([]any, 0, 14)
	if id := subtitleTraceRequestID(ctx); id != "" {
		attrs = append(attrs, "request_id", id)
	}
	if codec != "" {
		attrs = append(attrs, "codec", codec)
	}
	if phase != "" {
		attrs = append(attrs, "phase", phase)
	}
	if bytes >= 0 {
		attrs = append(attrs, "bytes", bytes)
	}
	if outcome != "" {
		attrs = append(attrs, "outcome", outcome)
	}
	if elapsed >= 0 {
		attrs = append(attrs, "elapsed_ms", elapsed.Milliseconds())
	}
	return attrs
}

// subtitleTraceAttrs is the common field set plus any phase-specific fields.
// Call sites pass it to a slog call whose message is a literal, so the fixed
// field set cannot drift between the cache, the extract and the notifier while
// sloglint keeps the message constant.
func subtitleTraceAttrs(ctx context.Context, codec, phase string, bytes int64, outcome string, elapsed time.Duration, extra ...any) []any {
	attrs := subtitleTraceFields(ctx, codec, phase, bytes, outcome, elapsed)
	return append(attrs, extra...)
}

// subtitlePhaseForOutput names the phase an extract of codec runs under for the
// requested target. A copy output is the fetch — the demux/remux that produces
// the source bytes with no re-encode — while anything else is a convert. It
// reads streamExtractOutput, the same function that builds the ffmpeg command,
// so the trace can never disagree with what actually ran.
func subtitlePhaseForOutput(codec, targetFormat string) string {
	outCodec, _ := streamExtractOutput(codec, targetFormat)
	if outCodec == codecCopyV3 {
		return SubtitleTracePhaseFetch
	}
	return SubtitleTracePhaseConvert
}

// subtitleTraceOutcome classifies a completed extraction. A canceled request is
// normal client behavior, not a failure; a write or process error is a failure.
func subtitleTraceOutcome(ctx context.Context, err error) string {
	if ctx != nil && ctx.Err() != nil {
		return SubtitleTraceOutcomeCanceled
	}
	if err != nil {
		return SubtitleTraceOutcomeFailure
	}
	return SubtitleTraceOutcomeSuccess
}

// subtitleCountingWriter counts the bytes written through a subtitle response
// or cache fill. It forwards Flush when the wrapped writer flushes, so wrapping
// the stream extract's per-chunk flushing writer keeps delivering cues in real
// time instead of buffering them until the process exits.
type subtitleCountingWriter struct {
	w     io.Writer
	bytes int64
}

func (c *subtitleCountingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.bytes += int64(n)
	return n, err
}

func (c *subtitleCountingWriter) Flush() {
	if flusher, ok := c.w.(http.Flusher); ok {
		flusher.Flush()
	}
}
