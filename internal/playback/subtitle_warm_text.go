package playback

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// In-progress full-track ASS warm progress.
//
// A windowed ASS request is a fresh ffmpeg read of the relay source. On a
// large remote release each read costs tens of seconds and, while the
// full-track warm is still filling the cache, the client keeps asking for new
// windows. The warm already produces the complete script in source-time order;
// keeping the part it has written so far lets a later window whose range the
// warm has passed be answered from memory instead of opening another relay
// read. The retained artifact is the same few tens of kilobytes as the text
// track, bounded below.
//
// Correctness rests on source-time ordering: the ASS muxer emits events in
// order of their start time, so once the highest emitted event end has passed
// a window's end, every event that can overlap that window has already been
// written. The window is then reconstructed as the script header plus the
// events that overlap the requested range, which is exactly what a windowed
// extract from the committed full-track artifact would produce.

const (
	// subtitleWarmTextMaxBytes bounds the retained script. A feature-length
	// ASS track is a few tens of kilobytes; the cap only stops a pathological
	// (or malicious) source from pinning a large buffer for the warm's life.
	subtitleWarmTextMaxBytes = 8 << 20
	// subtitleWarmTextMaxEvents bounds the retained event lines independently
	// of bytes, so a source of tiny lines cannot multiply the slice header.
	subtitleWarmTextMaxEvents = 50_000
	// subtitleWarmTextMaxLineBytes bounds one unterminated line. A longer line
	// means the muxer is not emitting the expected line-oriented script.
	subtitleWarmTextMaxLineBytes = 64 << 10
	// subtitleASSFormatStartColumn and subtitleASSFormatEndColumn name the
	// [Events] Format fields that carry a line's source-time range.
	subtitleASSFormatStartColumn = "start"
	subtitleASSFormatEndColumn   = "end"
	// subtitleASSEventDialogue and subtitleASSEventComment are the ASS/SSA
	// event line keywords. The spelling is the format's, not English.
	subtitleASSEventDialogue = "Dialogue:" //nolint:misspell // ASS/SSA event keyword
	subtitleASSEventComment  = "Comment:"
)

// subtitleWarmTextDisabledSignal is the operator-facing signal emitted when a
// warm's in-memory ASS prefix disables itself. The layer is fail-closed: a
// later window uses the existing extract path. Counting it lets production
// measure how often sources are malformed, unordered, or larger than the
// retained bounds rather than guessing from missing cache hits.
const subtitleWarmTextDisabledSignal = "warm_prefix_disabled_unordered"

// subtitleWarmTextDisables counts warm-prefix layers disabled for any reason.
// Disabling is once per warm, so the counter is also the number of warms that
// fell back to the extract path.
var subtitleWarmTextDisables atomic.Int64

// subtitleWarmTextEvent is one retained event line with the source time range
// parsed from it.
type subtitleWarmTextEvent struct {
	line  string
	start float64
	end   float64
}

// subtitleWarmTextProgress accumulates the script header and event lines of a
// full-track ASS warm as it streams. A nil or disabled progress never serves.
type subtitleWarmTextProgress struct {
	mu sync.Mutex

	header strings.Builder
	events []subtitleWarmTextEvent
	// retainedBytes counts the header and event line bytes currently held, so
	// the memory bound covers the actual strings rather than a per-event
	// estimate.
	retainedBytes int
	// watermark is the latest event start emitted so far. Events arrive in
	// start-time order, so once the watermark has reached a window's end every
	// event that can start inside that window has already been written.
	watermark float64
	// seenEvents is true once any event line was retained, so an empty prefix
	// is not mistaken for a covered range.
	seenEvents bool
	disabled   bool

	// partialLine carries an unterminated line between chunks.
	partialLine []byte
	// startIndex/endIndex are the event field positions from the [Events]
	// Format line; the common ASS layout is used until one is seen.
	startIndex int
	endIndex   int
	formatSeen bool
	section    string
}

func newSubtitleWarmTextProgress() *subtitleWarmTextProgress {
	return &subtitleWarmTextProgress{startIndex: 1, endIndex: 2}
}

// subtitleWarmTextTap forwards every chunk to the warm's real writer and
// observes it for the in-memory window layer.
type subtitleWarmTextTap struct {
	dst      io.Writer
	progress *subtitleWarmTextProgress
}

func (t *subtitleWarmTextTap) Write(p []byte) (int, error) {
	t.progress.observe(p)
	return t.dst.Write(p)
}

func (t *subtitleWarmTextTap) Flush() {
	if flusher, ok := t.dst.(http.Flusher); ok {
		flusher.Flush()
	}
}

// observe parses a chunk into complete lines. A malformed event (unparseable
// timestamps, a field count that cannot carry Start/End) disables the layer:
// serving a window from a prefix whose ordering cannot be trusted is worse
// than falling back to the existing extract path.
func (p *subtitleWarmTextProgress) observe(chunk []byte) {
	if p == nil || len(chunk) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disabled {
		return
	}
	if p.retainedBytes+len(p.partialLine)+len(chunk) > subtitleWarmTextMaxBytes {
		p.disableLocked("bundle_byte_budget")
		return
	}
	p.partialLine = append(p.partialLine, chunk...)
	if len(p.partialLine) > subtitleWarmTextMaxLineBytes && bytes.IndexByte(p.partialLine, '\n') < 0 {
		p.disableLocked("line_byte_budget")
		return
	}
	for {
		newline := bytes.IndexByte(p.partialLine, '\n')
		if newline < 0 {
			return
		}
		line := string(p.partialLine[:newline])
		p.partialLine = p.partialLine[newline+1:]
		if !p.consumeLineLocked(strings.TrimSuffix(line, "\r")) {
			return
		}
	}
}

// consumeLineLocked classifies one complete line. It returns false when the
// layer was disabled.
func (p *subtitleWarmTextProgress) consumeLineLocked(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		p.writeHeaderLocked(line)
		return true
	}
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		p.section = strings.ToLower(trimmed)
		p.writeHeaderLocked(line)
		return true
	}
	lower := strings.ToLower(trimmed)
	switch {
	case strings.HasPrefix(lower, "format:"):
		if p.section == "[events]" {
			p.parseFormatLocked(trimmed)
		}
		p.writeHeaderLocked(line)
		return true
	case strings.HasPrefix(lower, strings.ToLower(subtitleASSEventDialogue)),
		strings.HasPrefix(lower, strings.ToLower(subtitleASSEventComment)):
		start, end, ok := p.parseEventTimesLocked(trimmed)
		if !ok {
			p.disableLocked("malformed_event")
			return false
		}
		if len(p.events) >= subtitleWarmTextMaxEvents {
			p.disableLocked("event_budget")
			return false
		}
		p.events = append(p.events, subtitleWarmTextEvent{line: line, start: start, end: end})
		p.retainedBytes += len(line) + 1
		p.seenEvents = true
		if start > p.watermark {
			p.watermark = start
		}
		return true
	default:
		p.writeHeaderLocked(line)
		return true
	}
}

func (p *subtitleWarmTextProgress) writeHeaderLocked(line string) {
	p.header.WriteString(line)
	p.header.WriteByte('\n')
	p.retainedBytes += len(line) + 1
}

func (p *subtitleWarmTextProgress) parseFormatLocked(line string) {
	_, value, ok := strings.Cut(line, ":")
	if !ok {
		return
	}
	fields := strings.Split(value, ",")
	startIndex, endIndex := -1, -1
	for i, field := range fields {
		switch strings.ToLower(strings.TrimSpace(field)) {
		case subtitleASSFormatStartColumn:
			startIndex = i
		case subtitleASSFormatEndColumn:
			endIndex = i
		}
	}
	if startIndex < 0 || endIndex < 0 {
		return
	}
	p.startIndex = startIndex
	p.endIndex = endIndex
	p.formatSeen = true
}

func (p *subtitleWarmTextProgress) parseEventTimesLocked(line string) (start, end float64, ok bool) {
	_, value, found := strings.Cut(line, ":")
	if !found {
		return 0, 0, false
	}
	fields := strings.Split(value, ",")
	highest := p.startIndex
	if p.endIndex > highest {
		highest = p.endIndex
	}
	if len(fields) <= highest {
		return 0, 0, false
	}
	start, ok = parseASSTimestamp(fields[p.startIndex])
	if !ok {
		return 0, 0, false
	}
	end, ok = parseASSTimestamp(fields[p.endIndex])
	if !ok {
		return 0, 0, false
	}
	if end < start {
		return 0, 0, false
	}
	return start, end, true
}

func (p *subtitleWarmTextProgress) disableLocked(reason string) {
	if p.disabled {
		return
	}
	p.disabled = true
	events, retainedBytes, watermark := len(p.events), p.retainedBytes, p.watermark
	p.header.Reset()
	p.events = nil
	p.partialLine = nil
	p.retainedBytes = 0
	subtitleWarmTextDisables.Add(1)
	// Once per warm: the disabled guard above makes every later observe a
	// no-op, so a malformed source cannot flood the log with one line per
	// chunk. Serving never changes; this is the only signal that the
	// in-memory prefix fell back to the extract path.
	slog.Warn("subtitle warm text prefix layer disabled; serving windows from the extract path",
		"signal", subtitleWarmTextDisabledSignal,
		"reason", reason,
		"retained_events", events,
		"retained_bytes", retainedBytes,
		"watermark_seconds", watermark)
}

// render returns a self-contained ASS window for [start,end] when the warm has
// already emitted every event that can overlap it. The caller must not serve
// the bytes under a wider coverage than it asked for.
func (p *subtitleWarmTextProgress) render(start, end float64) ([]byte, bool) {
	if p == nil {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disabled || !p.seenEvents || p.header.Len() == 0 {
		return nil, false
	}
	if p.watermark < end {
		return nil, false
	}
	if start < 0 {
		start = 0
	}
	var out bytes.Buffer
	out.Grow(p.header.Len() + (len(p.events)+1)*64)
	out.WriteString(p.header.String())
	for _, event := range p.events {
		if event.end > start && event.start < end {
			out.WriteString(event.line)
			out.WriteByte('\n')
		}
	}
	return out.Bytes(), true
}

// parseASSTimestamp parses an ASS `H:MM:SS.cc` timestamp (and tolerates a
// decimal point, comma, or a missing fraction) into seconds.
func parseASSTimestamp(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return 0, false
	}
	hours, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || hours < 0 {
		return 0, false
	}
	minutes, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || minutes < 0 || minutes >= 60 {
		return 0, false
	}
	secondsText := strings.Replace(strings.TrimSpace(parts[2]), ",", ".", 1)
	seconds, err := strconv.ParseFloat(secondsText, 64)
	if err != nil || seconds < 0 || seconds >= 60 {
		return 0, false
	}
	return float64(hours*3600+minutes*60) + seconds, true
}

// beginWarmTextProgress registers an in-progress ASS warm under its cache
// entry key. A nil cache or key is a no-op.
func (c *SubtitleCache) beginWarmTextProgress(key string) *subtitleWarmTextProgress {
	if c == nil || key == "" {
		return nil
	}
	progress := newSubtitleWarmTextProgress()
	c.warmTextMu.Lock()
	if c.warmText == nil {
		c.warmText = make(map[string]*subtitleWarmTextProgress)
	}
	c.warmText[key] = progress
	c.warmTextMu.Unlock()
	return progress
}

// endWarmTextProgress removes a warm's progress entry once it settles. The
// committed artifact (or a failed warm) supersedes it.
func (c *SubtitleCache) endWarmTextProgress(key string, progress *subtitleWarmTextProgress) {
	if c == nil || key == "" || progress == nil {
		return
	}
	c.warmTextMu.Lock()
	if c.warmText[key] == progress {
		delete(c.warmText, key)
	}
	c.warmTextMu.Unlock()
}

// warmTextWindow renders a windowed ASS request from an in-progress warm when
// that warm has already passed the requested range.
func (c *SubtitleCache) warmTextWindow(key string, start, end float64) ([]byte, bool) {
	if c == nil || key == "" {
		return nil, false
	}
	c.warmTextMu.Lock()
	progress := c.warmText[key]
	c.warmTextMu.Unlock()
	return progress.render(start, end)
}

// logWarmTextServe records a window answered from the in-progress warm.
func logWarmTextServe(r *http.Request, opts StreamExtractOpts, bytes int, start, end float64) {
	slog.Log(r.Context(), slog.LevelDebug, "windowed subtitle served from in-progress warm",
		subtitleTraceAttrs(r.Context(), opts.SourceCodec, SubtitleTracePhaseServe, int64(bytes), SubtitleTraceOutcomeCacheHit, -1,
			"track", opts.TrackIndex, "format", subtitleFormatASS,
			"window_start", start, "window_end", end)...)
}
