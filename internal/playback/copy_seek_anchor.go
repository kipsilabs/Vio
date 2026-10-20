package playback

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	maxConcurrentCopySeekProbes = 4
	// CopySeekProbeTimeout is the per-attempt cap for one copy-video seek-anchor
	// probe. It is exported so the replan retry can decide whether another
	// attempt fits the caller's remaining budget; the probe itself is capped at
	// the smaller of this and the caller's remaining deadline.
	CopySeekProbeTimeout = 15 * time.Second

	// copySeekAnchorCacheTTL bounds how long a resolved anchor is reused. The
	// anchor is a property of the source bytes at a seek position, not of the
	// transport URL used to reach them: a virtual source resolves to the same
	// underlying release for the lifetime of a session's candidate. The probe
	// costs roughly 1.9s of session startup, so a short in-process TTL removes
	// that cost from resume starts, replans, seeks into an already-probed GOP,
	// and concurrent starts without risking a stale anchor.
	copySeekAnchorCacheTTL = 10 * time.Minute
	// copySeekAnchorCacheMax bounds the cache. Entries are tiny and the live
	// working set is the distinct (source, GOP) observations being resumed.
	copySeekAnchorCacheMax = 256
)

// CopySeekProbeRetryBudget is the wall-clock budget a caller must reserve
// before the first copy-video seek-anchor probe for one retry to fit: two full
// probes. A caller with less than this plus its own scheduling margin must skip
// the retry rather than start a second probe the deadline will truncate; the
// caller adds the margin because process startup, probe-slot waits, and result
// handling sit outside the probe timeout.
func CopySeekProbeRetryBudget() time.Duration {
	return 2 * CopySeekProbeTimeout
}

var (
	copySeekProbeGroup singleflight.Group
	copySeekProbeSlots = make(chan struct{}, maxConcurrentCopySeekProbes)
	// copySeekAnchorCallerHook, when non-nil, runs on every caller of
	// ResolveCopySeekAnchorForSource after it has registered with the probe
	// singleflight and before it waits for the shared result. It exists so a
	// test can hold a flight open until concurrent callers have coalesced onto
	// it rather than racing the first probe to completion. Production leaves it
	// nil; the hook never changes coalescing or probe behavior.
	copySeekAnchorCallerHook func()
	// copySeekAnchors is the process-wide resolved-anchor cache. Tests replace
	// it to inject a clock and probe runner.
	copySeekAnchors = &copySeekAnchorCache{
		entries:    make(map[string]copySeekAnchorCacheEntry),
		ttl:        copySeekAnchorCacheTTL,
		maxEntries: copySeekAnchorCacheMax,
		probe:      resolveCopySeekAnchor,
	}
)

type copySeekAnchor struct {
	seconds float64
	segment int
}

// ErrTransientProvider marks an ffmpeg failure whose cause is the upstream
// provider answering an HTTP 5xx, as opposed to the source bytes being
// undecodable or the request being canceled. Callers retry it with a bounded
// backoff instead of treating the candidate as dead or hammering the provider.
var ErrTransientProvider = errors.New("transient provider error")

// IsTransientProviderError reports whether err is (or wraps) a transient
// provider failure, so a retry path can back off before re-probing the same
// candidate.
func IsTransientProviderError(err error) bool {
	return errors.Is(err, ErrTransientProvider)
}

// upstream5xxMarkers are the ffmpeg stderr fragments that mean the upstream
// answered 5xx. ffmpeg's HTTP protocol logs "Server returned 5XX Server Error
// reply" (and the concrete 5xx codes) before "Error opening input file"; the
// status-class phrase is the stable part. Only 5xx counts here: 4xx is a
// configuration/authentication problem the provider will keep refusing, so a
// retry would not help and it is left as an ordinary failure.
var upstream5xxMarkers = []string{
	"Server returned 5XX",
	"Server returned 500",
	"Server returned 501",
	"Server returned 502",
	"Server returned 503",
	"Server returned 504",
	"Server returned 505",
}

// upstream5xxMarker returns the first marker proving the probe's stderr tail
// names an upstream 5xx, or the empty string when none does.
func upstream5xxMarker(stderrTail string) string {
	for _, marker := range upstream5xxMarkers {
		if strings.Contains(stderrTail, marker) {
			return marker
		}
	}
	return ""
}

// transientProviderCause wraps the command error with ErrTransientProvider when
// the probe's stderr tail names an upstream 5xx. The original error is kept so
// the exit status and message still reach the caller.
func transientProviderCause(err error, stderrTail string) error {
	if upstream5xxMarker(stderrTail) != "" {
		return fmt.Errorf("%w: %w", ErrTransientProvider, err)
	}
	return err
}

// externalProbeKillSignals are the signals that mean the probe process was
// terminated from outside instead of exiting with a status of its own. SIGKILL
// is the OOM killer or an operator's kill -9; SIGTERM is an orderly external
// stop. Either one says nothing about the source bytes, so the probe is retried
// once. Other signals (for example a repeatable FFmpeg crash) stay ordinary
// failures, because a second probe would die the same way.
var externalProbeKillSignals = []syscall.Signal{syscall.SIGKILL, syscall.SIGTERM}

// copySeekProbeKill reports the external termination signal that ended the probe
// process, if any. A decoder rejection, an invalid configuration, or a 4xx
// refusal exits with a status instead, so only a signal match is treated as an
// external kill rather than a verdict about the release.
func copySeekProbeKill(err error) (syscall.Signal, bool) {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ProcessState == nil {
		return 0, false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return 0, false
	}
	sig := status.Signal()
	for _, external := range externalProbeKillSignals {
		if sig == external {
			return sig, true
		}
	}
	return 0, false
}

// classifyCopySeekProbeFailure turns one FFmpeg probe failure into the error a
// caller classifies for retry, and logs why. The probe context is checked first:
// context cancellation, including the probe's own budget expiring, kills the
// process with SIGKILL, and that kill is our own doing rather than evidence
// about the release, so it is not transient (a caller cancellation must never be
// retried and a timeout keeps its existing single immediate retry). An external
// SIGKILL/SIGTERM with the probe context still live is also not evidence about
// the release, so it is transient and the caller's bounded retry applies. An
// upstream 5xx marker is transient too, but the caller backs off before
// re-probing a provider that is already failing. Everything else, such as a
// decoder rejection or a 4xx configuration refusal, stays an ordinary failure.
func classifyCopySeekProbeFailure(ctx context.Context, err error, stderr string) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		slog.WarnContext(ctx, "copy seek anchor probe stopped by its own context; not a provider verdict",
			"component", "playback",
			"cause", ctxErr,
			"error", err,
		)
		return fmt.Errorf("resolve copy seek anchor: probe interrupted: %w", ctxErr)
	}
	if sig, ok := copySeekProbeKill(err); ok {
		slog.WarnContext(ctx, "copy seek anchor probe killed by an external signal; treating the probe as transient",
			"component", "playback",
			"signal", sig.String(),
			"error", err,
		)
		return fmt.Errorf("%w: %w", ErrTransientProvider, err)
	}
	if marker := upstream5xxMarker(stderr); marker != "" {
		slog.WarnContext(ctx, "copy seek anchor probe hit a transient upstream provider response",
			"component", "playback",
			"marker", marker,
			"error", err,
		)
		return fmt.Errorf("%w: %w", ErrTransientProvider, err)
	}
	return err
}

// anchorProbeRunner runs the FFmpeg observation that produces an anchor. It is
// a seam so tests can exercise caching without spawning a process.
type anchorProbeRunner func(
	ctx context.Context,
	ffmpegPath string,
	inputPath string,
	requestedSeekSeconds float64,
	segmentDuration int,
) (float64, int, error)

type copySeekAnchorCacheEntry struct {
	anchor copySeekAnchor
	// maxRequested is the highest requested position a probe covered for
	// anchor, kept as an inclusive upper bound: every position in
	// (anchor, maxRequested] resolves to anchor. The probed request
	// position itself was observed by FFmpeg to yield anchor, so an exact
	// repeat is proven — unlike a request exactly on the anchor keyframe,
	// which stays unproven (Matroska pre-roll) and always re-probes. A
	// probe returns one packet and cannot prove the GOP continues past it.
	maxRequested float64
	expiresAt    time.Time
}

// copySeekAnchorCache memoizes anchors by stable source identity, segment
// duration, and FFmpeg build, keyed by the GOP (resolved keyframe) each anchor
// starts. A resolved keyframe serves positions in its GOP, so a repeat or
// random seek into an already-probed GOP does not pay for a fresh FFmpeg probe.
// now and probe are injectable for tests and default to time.Now and the real
// probe.
type copySeekAnchorCache struct {
	mu         sync.Mutex
	entries    map[string]copySeekAnchorCacheEntry
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
	probe      anchorProbeRunner
}

func (c *copySeekAnchorCache) clock() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

// lookup returns the cached anchor for a requested position. It finds the
// latest observed keyframe at or before requested and serves it only when the
// request lies inside the interval a probe has proven: requested is greater
// than anchor and at most maxRequested. Reuse never extends past that proven
// interval, because an unobserved keyframe between the anchor and a later
// request would otherwise be served the wrong anchor. The lower bound remains
// exclusive: FFmpeg can emit the preceding packet for an exact keyframe seek
// (Matroska pre-roll), so a request on the anchor keyframe re-probes.
func (c *copySeekAnchorCache) lookup(sourceKey string, requested float64) (copySeekAnchor, bool) {
	if c == nil || sourceKey == "" {
		return copySeekAnchor{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	prefix := sourceKey + "\x00kf="
	var best copySeekAnchorCacheEntry
	found := false
	for key, entry := range c.entries {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if !now.Before(entry.expiresAt) {
			delete(c.entries, key)
			continue
		}
		if entry.anchor.seconds > requested {
			continue
		}
		if !found || entry.anchor.seconds > best.anchor.seconds {
			best, found = entry, true
		}
	}
	if !found {
		return copySeekAnchor{}, false
	}
	if requested <= best.anchor.seconds {
		// The request sits on the keyframe boundary; the probe may resolve the
		// preceding packet, so this entry cannot answer it.
		return copySeekAnchor{}, false
	}
	if requested <= best.maxRequested {
		return best.anchor, true
	}
	// Past the interval this probe proved, a later keyframe may already have
	// started the next GOP; re-probe rather than assume the GOP continues.
	return copySeekAnchor{}, false
}

// store records a resolved anchor under its GOP key. A repeat observation at a
// higher position extends the proven interval for that keyframe; reuse is
// bounded to that interval, because one probe cannot prove where the GOP ends.
// The stored position is an inclusive upper bound: the exact probed request
// was observed to yield anchor, so it reuses (see lookup). Only a request
// exactly on the anchor keyframe stays unproven (Matroska pre-roll).
func (c *copySeekAnchorCache) store(sourceKey string, requested float64, anchor copySeekAnchor) {
	if c == nil || sourceKey == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]copySeekAnchorCacheEntry)
	}
	now := c.clock()
	// Drop expired entries first so a bounded cache is not crowded by entries
	// that will never be served again.
	for k, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, k)
		}
	}
	key := copySeekAnchorGOPKey(sourceKey, anchor.seconds)
	entry, ok := c.entries[key]
	if ok {
		if requested > entry.maxRequested {
			entry.maxRequested = requested
		}
		entry.expiresAt = now.Add(c.ttl)
	} else {
		entry = copySeekAnchorCacheEntry{anchor: anchor, maxRequested: requested, expiresAt: now.Add(c.ttl)}
	}
	c.entries[key] = entry
	for len(c.entries) > c.maxEntries {
		oldestKey := ""
		var oldest time.Time
		for k, e := range c.entries {
			if oldestKey == "" || e.expiresAt.Before(oldest) {
				oldestKey, oldest = k, e.expiresAt
			}
		}
		if oldestKey == "" {
			break
		}
		delete(c.entries, oldestKey)
	}
}

// copySeekAnchorSourceKey identifies one (source, segment duration, FFmpeg
// build) observation space. sourceIdentity — not inputPath — is used so
// concurrent resume starts that resolve to different pinned relay URLs still
// share one probe. The resolved FFmpeg path and the segment duration stay in the
// key because both change the resolved anchor or the segment index derived from
// it, so anchors are never reused across sources or segment durations.
func copySeekAnchorSourceKey(ffmpegPath, sourceIdentity string, segmentDuration int) string {
	return strings.Join([]string{
		ffmpegPath,
		sourceIdentity,
		strconv.Itoa(segmentDuration),
	}, "\x00")
}

// copySeekAnchorGOPKey identifies one observed GOP by the keyframe that starts
// it. Anchors are stored under this key rather than the requested position so a
// probe serves the rest of its GOP.
func copySeekAnchorGOPKey(sourceKey string, keyframe float64) string {
	return sourceKey + "\x00kf=" + strconv.FormatFloat(keyframe, 'g', -1, 64)
}

// copySeekAnchorFlightKey identifies one in-flight probe for singleflight
// coalescing. It keeps the exact requested position because two positions in
// one GOP cannot be known to share a keyframe until the probe returns; only an
// identical concurrent request joins the flight.
func copySeekAnchorFlightKey(sourceKey string, requestedSeekSeconds float64) string {
	return sourceKey + "\x00ss=" + strconv.FormatFloat(requestedSeekSeconds, 'g', -1, 64)
}

// copySeekAnchorProbe returns the configured probe runner, falling back to the
// real FFmpeg probe when a test has not installed one.
func (c *copySeekAnchorCache) probeRunner() anchorProbeRunner {
	if c != nil && c.probe != nil {
		return c.probe
	}
	return resolveCopySeekAnchor
}

// ResolveCopySeekAnchor returns the keyframe timestamp FFmpeg's input seek will
// actually use for a copy-video restart. FFmpeg cannot discard the pre-roll
// between that keyframe and requestedSeekSeconds while preserving -c:v copy,
// so callers need both timestamps: requestedSeekSeconds remains the -ss input,
// while the returned anchor defines the stream's real timeline origin.
//
// The one-packet framecrc output is intentional. ffprobe read intervals discard
// Matroska pre-roll at an exact keyframe boundary, while FFmpeg stream copy
// emits that preceding packet. Running FFmpeg with the transport's real seek
// and timestamp policy observes the packet the HLS muxer will actually receive
// without decoding or writing media output. start_at_zero matches the transport
// and makes the returned origin relative to the source start, not raw input PTS.
func ResolveCopySeekAnchor(
	ctx context.Context,
	ffmpegPath string,
	inputPath string,
	requestedSeekSeconds float64,
	segmentDuration int,
) (float64, int, error) {
	return ResolveCopySeekAnchorForSource(
		ctx, ffmpegPath, inputPath, inputPath, requestedSeekSeconds, segmentDuration,
	)
}

// ResolveCopySeekAnchorForSource is ResolveCopySeekAnchor with a stable source
// identity for the cache and singleflight key. inputPath is the concrete probe
// target: for a virtual source it is the per-call pinned-IP relay URL.
// sourceIdentity is the provider-neutral URI or on-disk path that names the
// same source across calls, so concurrent resume starts that resolve to
// different relay URLs still share one probe. The anchor is a property of the
// source bytes, and the caller's candidate points at one underlying release for
// the session, so a short-TTL cache is safe.
//
// Anchors are cached by the GOP (resolved keyframe) they start, not by the
// exact requested position: a probe at T records T's keyframe and proves no
// keyframe lies between them, so a nearby seek into the interval the probe
// covered reuses the anchor — including an exact repeat of T, which FFmpeg
// observed to yield that keyframe — instead of spawning another ~1.9s FFmpeg
// probe. Only a request exactly on the anchor keyframe re-probes (Matroska
// pre-roll can emit the preceding packet). Reuse is bounded to that proven
// interval; a seek beyond it re-probes, because a single packet does not
// reveal where the GOP ends. The source identity and segment duration remain
// part of the key, so an anchor is never reused across sources or segment
// durations.
func ResolveCopySeekAnchorForSource(
	ctx context.Context,
	ffmpegPath string,
	sourceIdentity string,
	inputPath string,
	requestedSeekSeconds float64,
	segmentDuration int,
) (float64, int, error) {
	if requestedSeekSeconds <= 0 {
		return 0, 0, nil
	}
	if strings.TrimSpace(inputPath) == "" {
		return 0, 0, fmt.Errorf("resolve copy seek anchor: empty input path")
	}
	if segmentDuration <= 0 {
		segmentDuration = DefaultSegmentDuration
	}

	resolvedFFmpegPath := ResolveFFmpegPath(ffmpegPath)
	identity := strings.TrimSpace(sourceIdentity)
	if identity == "" {
		identity = inputPath
	}
	sourceKey := copySeekAnchorSourceKey(resolvedFFmpegPath, identity, segmentDuration)

	// Fast path: a previous probe already proved this position resolves to its
	// keyframe. This is what turns a repeat or nearby seek into a zero-probe
	// start.
	if anchor, ok := copySeekAnchors.lookup(sourceKey, requestedSeekSeconds); ok {
		return anchor.seconds, anchor.segment, nil
	}

	flightKey := copySeekAnchorFlightKey(sourceKey, requestedSeekSeconds)
	resultCh := copySeekProbeGroup.DoChan(flightKey, func() (any, error) {
		// Another caller may have populated the cache between the fast-path
		// check and this closure starting.
		if anchor, ok := copySeekAnchors.lookup(sourceKey, requestedSeekSeconds); ok {
			return anchor, nil
		}
		budget, shortened := copySeekProbeTimeoutFor(ctx)
		if budget <= 0 {
			return copySeekAnchor{}, context.DeadlineExceeded
		}
		if shortened {
			slog.InfoContext(ctx, "copy seek anchor probe shortened to the caller's remaining budget",
				"component", "playback",
				"budget", budget,
				"cap", CopySeekProbeTimeout,
			)
		}
		// WithoutCancel keeps the probe alive across a client disconnect so a
		// completed observation can still populate the cache, but the derived
		// deadline is the caller's remaining budget: the probe can no longer
		// outlive the request by a full probe timeout.
		probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
		defer cancel()
		select {
		case copySeekProbeSlots <- struct{}{}:
			defer func() { <-copySeekProbeSlots }()
		case <-probeCtx.Done():
			slog.WarnContext(ctx, "copy seek anchor probe skipped: caller budget expired before a probe slot",
				"component", "playback",
				"budget", budget,
			)
			return copySeekAnchor{}, probeCtx.Err()
		}
		seconds, segment, err := copySeekAnchors.probeRunner()(probeCtx, resolvedFFmpegPath, inputPath, requestedSeekSeconds, segmentDuration)
		if err != nil {
			// Probe failures are not cached: a transient upstream timeout must
			// be retried by the next caller rather than pinned for the TTL.
			return copySeekAnchor{}, err
		}
		anchor := copySeekAnchor{seconds: seconds, segment: segment}
		copySeekAnchors.store(sourceKey, requestedSeekSeconds, anchor)
		return anchor, nil
	})

	// DoChan returns as soon as the caller is registered on the flight, for the
	// goroutine that leads the probe and for any that coalesce onto it. Reaching
	// this point therefore means the peer has joined this flight; a test uses the
	// hook to hold the probe open until both callers are here.
	if hook := copySeekAnchorCallerHook; hook != nil {
		hook()
	}

	select {
	case <-ctx.Done():
		return 0, 0, ctx.Err()
	case result := <-resultCh:
		if result.Err != nil {
			return 0, 0, result.Err
		}
		anchor := result.Val.(copySeekAnchor) //nolint:errcheck,forcetypeassert // copySeekProbeGroup's only DoChan closure returns a copySeekAnchor
		return anchor.seconds, anchor.segment, nil
	}
}

// LookupCachedCopySeekAnchor answers a copy-video seek from the shared anchor
// cache without resolving a probe input. It applies exactly the same
// proven-interval rules as ResolveCopySeekAnchorForSource's fast path: it
// returns true only when requestedSeekSeconds lies inside an interval a prior
// probe already covered (anchor < requested <= maxRequested); an exact repeat
// of the probed request reuses, while a request on the anchor keyframe or any
// unproven position still probes.
//
// It exists so a caller that would otherwise resolve a pinned relay URL before
// probing can skip that reopen entirely when the anchor is already proven: a
// cache hit needs no bytes to read. sourceIdentity is the same stable identity
// ResolveCopySeekAnchorForSource keys on; an empty identity cannot be answered
// without probing and returns false, so the caller falls through to a probe. The
// anchor is never resolved across a different source identity or segment
// duration.
func LookupCachedCopySeekAnchor(
	ffmpegPath string,
	sourceIdentity string,
	requestedSeekSeconds float64,
	segmentDuration int,
) (float64, int, bool) {
	if requestedSeekSeconds <= 0 {
		return 0, 0, false
	}
	identity := strings.TrimSpace(sourceIdentity)
	if identity == "" {
		return 0, 0, false
	}
	if segmentDuration <= 0 {
		segmentDuration = DefaultSegmentDuration
	}
	sourceKey := copySeekAnchorSourceKey(ResolveFFmpegPath(ffmpegPath), identity, segmentDuration)
	anchor, ok := copySeekAnchors.lookup(sourceKey, requestedSeekSeconds)
	if !ok {
		return 0, 0, false
	}
	return anchor.seconds, anchor.segment, true
}

// copySeekProbeTimeoutFor derives one probe's timeout from the caller's
// remaining deadline. The probe must not outlive the request by a full probe
// timeout: a resumed session's provider re-list can consume most of the replan
// budget, so a fixed 15s probe that ignored the caller's deadline was killed by
// our own timeout while the caller's budget was already gone. The returned
// budget is the smaller of CopySeekProbeTimeout and the remaining deadline;
// shortened reports that the caller's deadline, not the probe cap, set it. A
// non-positive budget means the caller had no time left and the probe must not
// run. A caller with no deadline keeps the full probe cap.
func copySeekProbeTimeoutFor(ctx context.Context) (time.Duration, bool) {
	if ctx == nil {
		return CopySeekProbeTimeout, false
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return CopySeekProbeTimeout, false
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, true
	}
	if remaining < CopySeekProbeTimeout {
		return remaining, true
	}
	return CopySeekProbeTimeout, false
}

func resolveCopySeekAnchor(
	ctx context.Context,
	ffmpegPath string,
	inputPath string,
	requestedSeekSeconds float64,
	segmentDuration int,
) (float64, int, error) {

	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-v", "error",
		"-fflags", "+genpts+fastseek",
		// The probe must observe the packet the remux transport will actually
		// copy, so its demuxer caps match the transport's (see
		// buildRemuxArgsWithLeadingPictureDropV3: probesize 5000000,
		// analyzeduration 3000000). A probe-scoped reduction was considered as a
		// #279 latency lever and rejected: a smaller probe can select a
		// different stream or seek point than the muxer, so the returned anchor
		// would no longer be the copied packet's timestamp and copy-seek
		// frame-exactness would be lost.
		"-analyzeduration", "3000000",
		"-probesize", "5000000",
		"-ss", fmt.Sprintf("%.3f", requestedSeekSeconds),
		"-i", inputPath,
		// Match the remux transport's 0:V:0 map. Uppercase V excludes attached
		// pictures, thumbnails, and cover art from the video stream ordinal.
		"-map", "0:V:0",
		"-c:v", "copy",
		"-copyts",
		"-start_at_zero",
		"-avoid_negative_ts", "disabled",
		"-frames:v", "1",
		"-f", "framecrc",
		"-",
	)
	var stdout bytes.Buffer
	stderr := newBoundedTailBuffer(stderrTailMaxBytes)
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		tail := truncateStderr(stderr.String())
		// Classify the full tail, not the truncated message: an upstream 5xx is
		// a transient provider failure a caller should back off and retry, while
		// a decoder/stream error is not. An external SIGKILL/SIGTERM is also
		// transient: the kill says nothing about the release, so the caller's
		// bounded retry covers it once. The probe's own context cancellation
		// stays distinct so it is never retried as a provider verdict.
		cause := classifyCopySeekProbeFailure(ctx, err, stderr.String())
		if tail != "" {
			return 0, 0, fmt.Errorf("resolve copy seek anchor: ffmpeg failed: %w (stderr: %s)", cause, tail)
		}
		return 0, 0, fmt.Errorf("resolve copy seek anchor: ffmpeg failed: %w", cause)
	}

	timeBaseNumerator, timeBaseDenominator := int64(0), int64(0)
	for line := range bytes.SplitSeq(stdout.Bytes(), []byte("\n")) {
		trimmed := strings.TrimSpace(string(line))
		if strings.HasPrefix(trimmed, "#tb 0:") {
			parts := strings.Split(strings.TrimSpace(strings.TrimPrefix(trimmed, "#tb 0:")), "/")
			if len(parts) != 2 {
				continue
			}
			timeBaseNumerator, _ = strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
			timeBaseDenominator, _ = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || timeBaseNumerator <= 0 || timeBaseDenominator <= 0 {
			continue
		}
		fields := strings.Split(trimmed, ",")
		if len(fields) < 3 || strings.TrimSpace(fields[0]) != "0" {
			continue
		}
		timestamp := strings.TrimSpace(fields[2])
		if timestamp == "" || strings.EqualFold(timestamp, "N/A") {
			timestamp = strings.TrimSpace(fields[1])
		}
		timestampTicks, err := strconv.ParseInt(timestamp, 10, 64)
		anchor := float64(timestampTicks) * float64(timeBaseNumerator) / float64(timeBaseDenominator)
		if err != nil || math.IsNaN(anchor) || math.IsInf(anchor, 0) {
			continue
		}
		anchor = math.Max(0, anchor)
		return anchor, int(anchor / float64(segmentDuration)), nil
	}

	return 0, 0, fmt.Errorf("resolve copy seek anchor: ffmpeg returned no video packet timestamp")
}
