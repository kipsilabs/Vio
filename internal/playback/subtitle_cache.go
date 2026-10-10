package playback

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/httpstream"
	"golang.org/x/sync/singleflight"
)

// SubtitleCache stores complete SUP, VTT, and ASS subtitle extracts on disk
// so repeat selections don't re-run a whole-file ffmpeg demux. Even a small
// text subtitle track can require reading a multi-GB source to completion.
//
// Entries are keyed by the source path, subtitle ordinal, source mtime+size,
// and output format. When the source changes, the lookup key changes and the
// old entry becomes garbage that eviction reclaims. Entry recency for LRU is
// tracked by bumping the cache file's mtime on every hit (portable, unlike
// atime which is often disabled via noatime/relatime mounts).
//
// Concurrency: the first requester of an uncached track streams the extract
// progressively to its client while teeing bytes into a temp file that is
// atomically renamed into the cache on clean ffmpeg exit (and discarded on
// any error, so a partial entry is never served). A request that arrives while
// a fill is already in flight waits for that fill to settle (bounded by its
// own request context) and then serves the committed entry, so a plan-time
// warm is never duplicated by the first client fetch. A request that finds no
// in-flight fill streams independently as before, so a genuinely cold path
// never depends on another viewer's connection.
type SubtitleCache struct {
	// transcodeDir returns the current transcode directory; the cache lives
	// in a subtitle-cache subdirectory beneath it, created lazily. An empty
	// return disables the cache for that call.
	transcodeDir func() string
	// maxBytes is the total-size eviction budget for committed entries.
	maxBytes int64

	mu       sync.Mutex
	inflight map[string]*SubtitleCacheFill

	// warmMu guards warmGuard, the per-identity failed-warm cooldown. It is
	// separate from mu so admission never contends with in-flight fill
	// bookkeeping.
	warmMu    sync.Mutex
	warmGuard map[string]subtitleWarmGuard

	// warmSem bounds concurrent background warms server-wide (each warm
	// demuxes an entire source file — heavy sequential IO). Acquisition is
	// non-blocking: warms beyond the budget are dropped, not queued; the
	// next windowed miss for that track re-attempts the warm.
	warmSem chan struct{}

	// fontBundleFlight coalesces concurrent font-bundle extractions for the
	// same source so a font fetch stampede runs ffprobe+ffmpeg exactly once.
	fontBundleFlight singleflight.Group
}

const (
	subtitleFormatSRT        = "srt"
	subtitleCodecSubRip      = "subrip"
	subtitleCodecSSA         = "ssa"
	subtitleFormatSUP        = "sup"
	subtitleFormatASS        = "ass"
	subtitleMuxerWebVTT      = "webvtt"
	subtitleFormatFontBundle = "fontbundle"
	subtitleCacheDirName     = "subtitle-cache"
	// defaultSubtitleCacheMaxBytes caps the cache at 2 GiB — PGS tracks run
	// 15-80 MB, so this holds a few dozen tracks.
	// TODO: expose as a config knob following the download.artifact_max_bytes
	// pattern (internal/config/config.go DownloadConfig.ArtifactMaxBytes).
	defaultSubtitleCacheMaxBytes = 2 << 30
	// stalePartMaxAge is how long an orphaned .part temp file (leftover from
	// a crash mid-fill) survives before eviction sweeps remove it.
	stalePartMaxAge = time.Hour
	// subtitleCacheWarmSlots caps concurrent background warms server-wide.
	// Two lets a second household stream warm while the first is still
	// demuxing, without letting a burst of playbacks saturate disk IO.
	subtitleCacheWarmSlots = 2
	// subtitleCacheWarmTimeout bounds a single background warm. A full-file
	// demux of a large remux on network storage can take minutes; anything
	// beyond this is stuck and should release its slot.
	subtitleCacheWarmTimeout = 30 * time.Minute
	// subtitleCacheGenerationBucket bounds the staleness of identity-keyed
	// (remote / virtual / generated) cache entries. The identity already pins
	// the provider result candidate id, so a release that actually rotates
	// produces a different key; for subtitles the bucket is only residual
	// insurance against the same pinned id later serving changed bytes. It was
	// ten minutes, which discarded a warmed ASS/SUP artifact between plays and
	// forced a full ~100s relay demux on the next request. A day keeps a
	// warmed artifact across a viewing session while still bounding that
	// residual risk.
	subtitleCacheGenerationBucket = 24 * time.Hour
	// subtitleFontBundleExtractTimeout bounds one detached font-bundle
	// extraction. The extraction runs single-flighted and detached from the
	// request that triggered it, and its successful result is written through
	// to disk. A cold relay carrying a large anime font set (up to 47
	// attachments, each a network open) can take minutes, so the old 60s
	// ceiling discarded work before it could ever be stored — the reason every
	// retry repeated the same failure. Five minutes keeps a hard bound while
	// letting a cold extraction finish; the HTTP path degrades to an empty
	// bundle after its short client wait long before this fires.
	subtitleFontBundleExtractTimeout = 5 * time.Minute
	// subtitleFillWaitRetries bounds how many in-flight fills a single request
	// waits on before falling back to its own uncached stream. A fill that
	// fails without committing leaves no entry; the waiter re-attempts the
	// reservation, so one transient failure still yields a served response.
	// The cap stops a pathological run of failing fills from wedging a request
	// indefinitely when its context has no deadline.
	subtitleFillWaitRetries = 3
	// subtitleWarmFailureBaseCooldown is the first backoff after a warm for
	// one identity fails or is abandoned. Repeated windowed misses on a
	// source that cannot be warmed (provider error, broken relay,
	// unreadable file) would otherwise each spawn a full-track remote read —
	// the two warm slots bound concurrency, not total bytes or cluster load.
	// A failed identity is suppressed for this long before the next attempt.
	subtitleWarmFailureBaseCooldown = time.Minute
	// subtitleWarmFailureMaxCooldown caps the exponential backoff so a
	// permanently broken source is retried eventually instead of never (the
	// generation bucket, not the cooldown, is what re-keys it after a real
	// source rotation).
	subtitleWarmFailureMaxCooldown = 30 * time.Minute
	// subtitleWarmGuardMaxEntries caps the per-identity failed-warm cooldown
	// map. A live entry is an identity still inside its cooldown; without a cap
	// a long-lived server accumulates one entry for every identity that ever
	// failed to warm, including identities whose cooldown lapsed long ago.
	// 4096 is far above the number of distinct tracks that can fail inside the
	// 30m maximum cooldown, so eviction only fires under pathological churn.
	// It does not change when a retained entry suppresses a warm: an evicted
	// live entry simply lets the next attempt for that identity run sooner
	// than its backoff would have allowed.
	subtitleWarmGuardMaxEntries = 4096
)

// subtitleWarmGuard is the per-identity warm admission state: how many
// consecutive warms failed and the earliest time the next one may start.
type subtitleWarmGuard struct {
	failures int
	retryAt  time.Time
}

// SUPExtractFunc runs one ffmpeg subtitle extract described by opts, writing
// output to opts.Writer. Production callers pass StreamExtractSubtitle;
// tests substitute fakes. The cache invokes it with the caller's options
// rewritten as needed (tee writer for fills, cached-.sup input for windowed
// serves, cleared window for background warms).
type SUPExtractFunc func(ctx context.Context, opts StreamExtractOpts) error

// NewSubtitleCache builds a cache rooted under the transcode directory
// returned by transcodeDir at call time (so runtime config changes are
// honored). Pass nil to disable caching entirely.
func NewSubtitleCache(transcodeDir func() string) *SubtitleCache {
	return &SubtitleCache{
		transcodeDir: transcodeDir,
		maxBytes:     defaultSubtitleCacheMaxBytes,
		inflight:     make(map[string]*SubtitleCacheFill),
		warmGuard:    make(map[string]subtitleWarmGuard),
		warmSem:      make(chan struct{}, subtitleCacheWarmSlots),
	}
}

// ServeExtract serves an embedded subtitle using the cache for complete
// tracks. Explicit text windows stream without caching; SUP retains its
// progressive window and background-warming behavior in ServeSUPExtract.
// Only one request fills the cache; a request that arrives while a fill is in
// flight waits for it to commit and serves the cached entry (a failed fill
// falls back to a fresh attempt, then to an uncached stream).
//
// It is ServeExtractWithResult without the serve report. Callers that need to
// know whether a committed artifact was served — for example to decide whether
// track identity still needs validation — use ServeExtractWithResult.
func (c *SubtitleCache) ServeExtract(w http.ResponseWriter, r *http.Request, opts StreamExtractOpts, extract SUPExtractFunc) error {
	_, err := c.ServeExtractWithResult(w, r, opts, extract)
	return err
}

// ServeExtractResult reports how a serve was satisfied.
type ServeExtractResult struct {
	// ServedCommittedArtifact is true when the response body came from the
	// committed full-track cache artifact resolved for the request identity:
	// served directly for a whole-track request, or used as the input of a
	// windowed extract. False means the response was produced by a fresh
	// extract of the source or a live fill.
	//
	// A caller must not treat cache presence alone as proof of track
	// identity. It should resolve the artifact with ResolveCommittedTextEntry,
	// skip validation only when that returns a token, and pass the token back
	// through StreamExtractOpts.PinnedTextArtifact. If the pinned artifact is
	// gone by serve time this method returns ErrCommittedTextArtifactGone
	// instead of silently falling back to an unvalidated source extract.
	ServedCommittedArtifact bool
}

// ErrCommittedTextArtifactGone is returned by ServeExtractWithResult when the
// caller pinned a resolved committed text artifact (PinnedTextArtifact) but
// the artifact no longer exists by serve time. No response is written, so the
// caller can revalidate the live track identity and retry rather than stream
// an unvalidated source extract.
var ErrCommittedTextArtifactGone = errors.New("committed subtitle artifact no longer available")

func (c *SubtitleCache) ServeExtractWithResult(w http.ResponseWriter, r *http.Request, opts StreamExtractOpts, extract SUPExtractFunc) (ServeExtractResult, error) {
	if err := r.Context().Err(); err != nil {
		return ServeExtractResult{}, err
	}
	start := time.Now()
	// Full demuxes can outlive the API listener's absolute write timeout.
	// Reuse the media-stream deadline so progress continues while stalled
	// connections still have a bounded write lifetime.
	w = httpstream.NewRollingDeadlineWriter(w)
	format := subtitleCacheFormat(opts.SourceCodec, opts.TargetFormat)
	if format == subtitleFormatSUP {
		return ServeExtractResult{}, c.ServeSUPExtract(w, r, opts, extract)
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	if format == subtitleFormatASS {
		w.Header().Set("Content-Type", "text/x-ssa; charset=utf-8")
	}
	var result ServeExtractResult
	var fill *SubtitleCacheFill
	if !opts.windowIntent() {
		if c.serveCached(w, r, opts, format) {
			result.ServedCommittedArtifact = true
			return result, nil
		}
		var wait <-chan struct{}
		fill, wait = c.beginFillOrWait(opts.InputPath, opts.CacheIdentity, opts.TrackIndex, format)
		for attempt := 0; wait != nil && attempt < subtitleFillWaitRetries; attempt++ {
			// A warm (or another request) already holds the fill. Wait for it
			// to settle instead of running a second full demux, then serve the
			// entry it committed.
			if err := waitForSubtitleFill(r.Context(), wait); err != nil {
				return result, err
			}
			if c.serveCached(w, r, opts, format) {
				result.ServedCommittedArtifact = true
				return result, nil
			}
			fill, wait = c.beginFillOrWait(opts.InputPath, opts.CacheIdentity, opts.TrackIndex, format)
		}
		if fill != nil && c.serveCached(w, r, opts, format) {
			// A previous owner may have committed between lookup and reservation.
			fill.Discard()
			result.ServedCommittedArtifact = true
			return result, nil
		}
	} else {
		// Windowed text fetches are position-dependent slices, so they are
		// never cached themselves — but the cache still speeds them up,
		// exactly like serveWindowedSUP does for PGS: when a committed
		// full-track entry exists, the windowed extract's input is rewritten
		// to that small artifact (the -ss scan reads kilobytes instead of
		// re-demuxing a multi-GB remote source), and when it doesn't, a
		// detached warm is kicked off so later windows hit the fast path.
		// The committed-entry lookup always runs: virtual relay sources
		// disable only the background warm (a detached fill must not outlive
		// the request-scoped relay registration) while an already-populated
		// entry must still be served instead of re-demuxing the source.
		//
		// A pinned artifact (from ResolveCommittedTextEntry) is used by its
		// exact path rather than re-resolved, so a generation-bucket rollover
		// between the caller's identity check and this serve cannot turn a
		// validated artifact into a miss. If the pinned file has been
		// evicted, fail before the response is committed so the caller can
		// revalidate and retry.
		artifact, ok := c.resolveCommittedTextArtifact(opts)
		if ok {
			if _, statErr := os.Stat(artifact.path); statErr == nil {
				slog.DebugContext(r.Context(), "windowed text subtitle extract using cached full track",
					"input", opts.InputPath, "track", opts.TrackIndex, "cache_entry", artifact.path)
				opts.InputPath = artifact.path
				opts.InputIsExtractedText = artifact.format
				result.ServedCommittedArtifact = true
			} else if opts.PinnedTextArtifact != nil {
				// The caller validated identity against this exact artifact
				// and it is now gone. Falling back to the source here would
				// serve an unvalidated ordinal, so surface the miss instead.
				return ServeExtractResult{}, ErrCommittedTextArtifactGone
			}
		}
		if !result.ServedCommittedArtifact && !opts.DisableBackgroundWarm {
			c.WarmTrackInBackground(opts, extract, subtitleTraceRequestID(r.Context()))
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	counter := &subtitleCountingWriter{w: w}
	var writer io.Writer = counter
	if fill != nil {
		writer = fill.Tee(counter)
	}
	opts.Writer = writer
	err := extract(r.Context(), opts)
	outcome := subtitleTraceOutcome(r.Context(), err)
	if fill != nil {
		if err != nil {
			fill.Discard()
		} else if commitErr := fill.Commit(); commitErr != nil {
			outcome = SubtitleTraceOutcomeCommitFailed
			slog.Log(r.Context(), slog.LevelWarn, "subtitle cache commit failed",
				subtitleTraceAttrs(r.Context(), opts.SourceCodec, SubtitleTracePhaseServe, counter.bytes, outcome, -1,
					"track", opts.TrackIndex, "format", format, "error", commitErr)...)
		}
	}
	level := slog.LevelInfo
	if outcome != SubtitleTraceOutcomeSuccess {
		level = slog.LevelWarn
	}
	slog.Log(r.Context(), level, "subtitle extract served",
		subtitleTraceAttrs(r.Context(), opts.SourceCodec, subtitlePhaseForOutput(opts.SourceCodec, opts.TargetFormat),
			counter.bytes, outcome, time.Since(start),
			"track", opts.TrackIndex, "format", format)...)
	return result, err
}

// waitForSubtitleFill blocks until the in-flight fill identified by wait
// settles (its done channel closes on Commit or Discard) or the caller's
// request context is done. A canceled client returns promptly instead of
// holding a goroutine until the fill finishes.
func waitForSubtitleFill(ctx context.Context, wait <-chan struct{}) error {
	select {
	case <-wait:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *SubtitleCache) serveCached(w http.ResponseWriter, r *http.Request, opts StreamExtractOpts, format string) bool {
	cached, modTime, ok := c.lookup(opts.InputPath, opts.CacheIdentity, opts.TrackIndex, format)
	if !ok {
		return false
	}
	defer func() { _ = cached.Close() }()
	size := int64(SubtitleTraceBytesUnknown)
	if info, statErr := cached.Stat(); statErr == nil {
		size = info.Size()
	}
	slog.Log(r.Context(), slog.LevelDebug, "subtitle served from cache",
		subtitleTraceAttrs(r.Context(), opts.SourceCodec, SubtitleTracePhaseServe, size, SubtitleTraceOutcomeCacheHit, -1,
			"track", opts.TrackIndex, "format", format)...)
	w.Header().Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, "", modTime, cached)
	return true
}

// subtitleCacheFormat maps a codec/target pair to the cache format token,
// normalizing ffmpeg's "webvtt" muxer name to the cache's "vtt" token. It is
// the one mapping ServeExtract, WarmTrackInBackground, and committed-entry
// queries share, so a lookup can never disagree with a fill.
func subtitleCacheFormat(codec, targetFormat string) string {
	_, format := streamExtractOutput(codec, targetFormat)
	if format == subtitleMuxerWebVTT {
		return SubtitleFormatVTTV3
	}
	return format
}

// CommittedTextArtifact is a resolved reference to a committed full-track
// text (VTT/ASS) cache entry. ResolveCommittedTextEntry binds the exact cache
// path under the current invalidation coordinates; a caller can hand the token
// back through StreamExtractOpts.PinnedTextArtifact so the serve uses that
// same artifact rather than re-resolving it. Cache presence alone is not proof
// of track identity: the token is what makes the identity check and the read
// consistent, so only a token — never HasCommittedTextEntry's bool — may be
// used to skip validation.
type CommittedTextArtifact struct {
	path   string
	format string
}

// ResolveCommittedTextEntry resolves the committed full-track text artifact
// for the given source identity + track ordinal and returns a token that pins
// its exact path. codec and targetFormat are the same values passed to
// ServeExtract, and the format is derived through the same mapping, so the
// token is exactly what a windowed ServeExtract would read. ok is false for a
// bitmap (PGS) codec, an unkeyable source, a missing entry, or a nil cache.
//
// Unlike HasCommittedTextEntry, the returned token is bound to the resolved
// path: passing it back via StreamExtractOpts.PinnedTextArtifact guarantees
// ServeExtractWithResult serves that artifact (or reports
// ErrCommittedTextArtifactGone), so a generation-bucket rollover or an
// eviction between the identity check and the read cannot silently invalidate
// the decision. Callers that skip track-identity validation because an
// artifact exists must use this method and the pinned token.
func (c *SubtitleCache) ResolveCommittedTextEntry(inputPath, cacheIdentity string, trackIndex int, codec, targetFormat string) (CommittedTextArtifact, bool) {
	if c == nil {
		return CommittedTextArtifact{}, false
	}
	format := subtitleCacheFormat(codec, targetFormat)
	if format != SubtitleFormatVTTV3 && format != subtitleFormatASS {
		return CommittedTextArtifact{}, false
	}
	path, _, ok := c.cachedFormatEntryPath(inputPath, cacheIdentity, trackIndex, format)
	if !ok {
		return CommittedTextArtifact{}, false
	}
	return CommittedTextArtifact{path: path, format: format}, true
}

// resolveCommittedTextArtifact returns the artifact ServeExtract should use:
// the caller's pinned token when present (authoritative), otherwise a fresh
// resolution for an unpinned request.
func (c *SubtitleCache) resolveCommittedTextArtifact(opts StreamExtractOpts) (CommittedTextArtifact, bool) {
	if opts.PinnedTextArtifact != nil {
		return *opts.PinnedTextArtifact, true
	}
	return c.ResolveCommittedTextEntry(opts.InputPath, opts.CacheIdentity, opts.TrackIndex, opts.SourceCodec, opts.TargetFormat)
}

// HasCommittedTextEntry reports whether a committed full-track text (VTT/ASS)
// artifact exists for the given source identity + track ordinal. codec and
// targetFormat are the same values passed to ServeExtract, and the format is
// derived through the same mapping.
//
// This is a non-binding snapshot: because the check and the read are separate
// operations, a hit does not prove the track identity a later ServeExtract
// will read (a generation-bucket rollover or eviction can turn that read into
// a miss). Callers that intend to skip track-identity validation must resolve
// the artifact with ResolveCommittedTextEntry and pin it via
// StreamExtractOpts.PinnedTextArtifact instead. A bitmap (PGS) codec, an
// unkeyable source, or a nil cache reads as false.
func (c *SubtitleCache) HasCommittedTextEntry(inputPath, cacheIdentity string, trackIndex int, codec, targetFormat string) bool {
	if c == nil {
		return false
	}
	_, ok := c.ResolveCommittedTextEntry(inputPath, cacheIdentity, trackIndex, codec, targetFormat)
	return ok
}

// HasCommittedEntry reports whether a committed full-track artifact exists for
// the source identity + track ordinal in the format this codec/target maps to:
// VTT, ASS, or SUP. It generalizes HasCommittedTextEntry to every sidecar class
// so the implicit-window gate can serve an already-committed track whole
// (cheap) instead of slicing it, for text, ASS, and PGS alike.
//
// Like HasCommittedTextEntry this is a non-binding snapshot: it exists for
// admission decisions, not to prove the identity a later read will use. Callers
// that skip track-identity validation must still resolve and pin the artifact
// with ResolveCommittedTextEntry (text/ASS); PGS has no pinned-token path and
// keeps its mandatory drift probe. A codec with no sidecar class, an unkeyable
// source, or a nil cache reads as false.
func (c *SubtitleCache) HasCommittedEntry(inputPath, cacheIdentity string, trackIndex int, codec, targetFormat string) bool {
	if c == nil {
		return false
	}
	format := subtitleCacheFormat(codec, targetFormat)
	if format == "" {
		return false
	}
	_, _, ok := c.cachedFormatEntryPath(inputPath, cacheIdentity, trackIndex, format)
	return ok
}

// ServeSUPExtract serves the .sup extract for one source+track described by
// opts (opts.Writer is ignored; the cache supplies it). Full-track requests
// (no AllowWindow): a cache hit is served with http.ServeContent (Range
// support, Content-Length, Last-Modified from the source file's mtime,
// revalidatable instead of no-store); a miss invokes extract with a writer
// that streams to the client while teeing bytes into a temp file, atomically
// published as the cache entry on clean extract exit and discarded on any
// error (ffmpeg failure or client disconnect) — a partial entry is never
// served. A request that arrives while a fill is already in flight waits for
// that fill and serves its committed entry rather than running a second full
// demux; only when no fill can be owned does it fall back to an uncached
// stream. Windowed requests (opts.AllowWindow): the output covers only a
// slice of the track, so it is never cached; but when the full-track entry
// already exists, the windowed extract runs against the small cached .sup
// instead of re-demuxing the original file, and when it doesn't, a detached
// background warm is kicked off so subsequent windows get that fast path. A
// nil receiver disables caching and just streams.
//
// The caller sets any extra response headers (e.g. CORS) before calling.
// The returned error is the extract error; cache hits return nil.
func (c *SubtitleCache) ServeSUPExtract(w http.ResponseWriter, r *http.Request, opts StreamExtractOpts, extract SUPExtractFunc) error {
	if opts.AllowWindow {
		return c.serveWindowedSUP(w, r, opts, extract)
	}
	start := time.Now()

	if c.serveSUPEntry(w, r, opts) {
		return nil
	}

	// No committed entry. Try to own the fill; when another fill is already in
	// flight, wait for it and serve the entry it commits instead of running a
	// duplicate full demux. The wait happens before any header write so a
	// committed entry can still be served with http.ServeContent.
	fill, wait := c.beginFillOrWait(opts.InputPath, opts.CacheIdentity, opts.TrackIndex, subtitleFormatSUP)
	for attempt := 0; wait != nil && attempt < subtitleFillWaitRetries; attempt++ {
		if err := waitForSubtitleFill(r.Context(), wait); err != nil {
			return err
		}
		if c.serveSUPEntry(w, r, opts) {
			return nil
		}
		fill, wait = c.beginFillOrWait(opts.InputPath, opts.CacheIdentity, opts.TrackIndex, subtitleFormatSUP)
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	// BeginFill returns nil when no fill could be owned (cache unusable or a
	// run of failed fills); this request then streams its own uncached extract.
	counter := &subtitleCountingWriter{w: w}
	var writer io.Writer = counter
	if fill != nil {
		writer = fill.Tee(counter)
	}

	opts.Writer = writer
	err := extract(r.Context(), opts)
	outcome := subtitleTraceOutcome(r.Context(), err)
	if fill != nil {
		if err != nil {
			fill.Discard()
		} else if commitErr := fill.Commit(); commitErr != nil {
			outcome = SubtitleTraceOutcomeCommitFailed
			slog.Log(r.Context(), slog.LevelWarn, "subtitle cache commit failed",
				subtitleTraceAttrs(r.Context(), opts.SourceCodec, SubtitleTracePhaseServe, counter.bytes, outcome, -1,
					"track", opts.TrackIndex, "error", commitErr)...)
		}
	}
	level := slog.LevelInfo
	if outcome != SubtitleTraceOutcomeSuccess {
		level = slog.LevelWarn
	}
	slog.Log(r.Context(), level, "subtitle extract served",
		subtitleTraceAttrs(r.Context(), opts.SourceCodec, subtitlePhaseForOutput(opts.SourceCodec, opts.TargetFormat),
			counter.bytes, outcome, time.Since(start),
			"track", opts.TrackIndex, "format", subtitleFormatSUP)...)
	return err
}

// serveSUPEntry serves a committed full-track .sup entry, returning false on a
// miss (or when caching is disabled). On a hit it writes the octet-stream
// headers and delegates range/HEAD/content-length handling to
// http.ServeContent; lookup bumps the entry's mtime for LRU recency.
func (c *SubtitleCache) serveSUPEntry(w http.ResponseWriter, r *http.Request, opts StreamExtractOpts) bool {
	cached, modTime, ok := c.lookup(opts.InputPath, opts.CacheIdentity, opts.TrackIndex, subtitleFormatSUP)
	if !ok {
		return false
	}
	defer func() { _ = cached.Close() }()
	size := int64(SubtitleTraceBytesUnknown)
	if info, statErr := cached.Stat(); statErr == nil {
		size = info.Size()
	}
	slog.Log(r.Context(), slog.LevelDebug, "subtitle stream served from cache",
		subtitleTraceAttrs(r.Context(), opts.SourceCodec, SubtitleTracePhaseServe, size, SubtitleTraceOutcomeCacheHit, -1,
			"track", opts.TrackIndex, "format", subtitleFormatSUP)...)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, "", modTime, cached)
	return true
}

// serveWindowedSUP streams a windowed slice of the track. The output is a
// position-dependent slice so it is never cached itself, but the cache still
// speeds it up: with a committed full-track entry the extract's input is
// rewritten to the cached .sup (15-80 MB, so the -ss scan is near-instant
// versus re-demuxing a multi-GB source); without one, a background warm is
// started so later windows — the client re-fetches on every seek — hit the
// fast path.
func (c *SubtitleCache) serveWindowedSUP(w http.ResponseWriter, r *http.Request, opts StreamExtractOpts, extract SUPExtractFunc) error {
	if cachedPath, _, ok := c.cachedEntryPath(opts.InputPath, opts.CacheIdentity, opts.TrackIndex); ok {
		slog.DebugContext(r.Context(), "windowed subtitle extract using cached full track",
			"input", opts.InputPath, "track", opts.TrackIndex, "cache_entry", cachedPath)
		opts.InputPath = cachedPath
		opts.InputIsExtractedSup = true
	} else if !opts.DisableBackgroundWarm {
		c.WarmInBackground(opts, extract, subtitleTraceRequestID(r.Context()))
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	opts.Writer = w
	return extract(r.Context(), opts)
}

// formatEntryKey computes the in-flight/cache key for a source identity,
// track ordinal, and format without creating a fill. It mirrors
// beginFillOrWait's key derivation exactly, so warm admission and the fill it
// guards always agree. ok is false when the cache is disabled or the source
// cannot be keyed.
func (c *SubtitleCache) formatEntryKey(inputPath, cacheIdentity string, trackIndex int, format string) (string, bool) {
	if c == nil || c.dir() == "" {
		return "", false
	}
	identity, modTime, size, ok := subtitleCacheSource(inputPath, cacheIdentity, time.Now())
	if !ok {
		return "", false
	}
	return subtitleCacheFormatKey(identity, trackIndex, modTime, size, format), true
}

// warmAdmitted reports whether a background warm may start for the cache key,
// rate-limiting identities whose warm recently failed or was abandoned. It
// reads only; noteWarmOutcome records the result.
func (c *SubtitleCache) warmAdmitted(key string, now time.Time) bool {
	if c == nil {
		return false
	}
	c.warmMu.Lock()
	defer c.warmMu.Unlock()
	guard, ok := c.warmGuard[key]
	// At the cap, drop entries whose cooldown has lapsed. They no longer
	// suppress anything, so this keeps the map from retaining memory the
	// admission check will never consult.
	if len(c.warmGuard) >= subtitleWarmGuardMaxEntries {
		c.pruneExpiredWarmGuardLocked(now)
	}
	if !ok {
		return true
	}
	return !now.Before(guard.retryAt)
}

// pruneExpiredWarmGuardLocked drops entries whose cooldown has lapsed. Caller
// holds warmMu.
func (c *SubtitleCache) pruneExpiredWarmGuardLocked(now time.Time) {
	for k, guard := range c.warmGuard {
		if !now.Before(guard.retryAt) {
			delete(c.warmGuard, k)
		}
	}
}

// sweepWarmGuardLocked makes room when the map is at its cap. It first drops
// lapsed entries (behaviorally dead), then, if every remaining entry is still
// live, evicts the one closest to lapsing. The soonest-expiring entry would
// stop suppressing within the shortest time anyway. Caller holds warmMu.
func (c *SubtitleCache) sweepWarmGuardLocked(now time.Time) {
	if len(c.warmGuard) < subtitleWarmGuardMaxEntries {
		return
	}
	c.pruneExpiredWarmGuardLocked(now)
	if len(c.warmGuard) < subtitleWarmGuardMaxEntries {
		return
	}
	victim := ""
	var victimAt time.Time
	for k, guard := range c.warmGuard {
		if victim == "" || guard.retryAt.Before(victimAt) || (guard.retryAt.Equal(victimAt) && k < victim) {
			victim = k
			victimAt = guard.retryAt
		}
	}
	if victim != "" {
		delete(c.warmGuard, victim)
	}
}

// noteWarmOutcome records a warm result for the cache key. Success clears the
// identity's backoff; failure (or abandonment) increments it and sets an
// exponentially growing retry time capped at
// subtitleWarmFailureMaxCooldown, so repeated windowed misses on a source
// that cannot be warmed do not each spawn a full-track remote read.
func (c *SubtitleCache) noteWarmOutcome(key string, success bool, now time.Time) {
	if c == nil || key == "" {
		return
	}
	c.warmMu.Lock()
	defer c.warmMu.Unlock()
	if success {
		delete(c.warmGuard, key)
		return
	}
	if c.warmGuard == nil {
		c.warmGuard = make(map[string]subtitleWarmGuard)
	}
	// Make room before inserting a new entry. Refreshing an existing entry does
	// not grow the map, so it never needs the sweep and never evicts the entry
	// it is about to refresh.
	if _, exists := c.warmGuard[key]; !exists && len(c.warmGuard) >= subtitleWarmGuardMaxEntries {
		c.sweepWarmGuardLocked(now)
	}
	guard := c.warmGuard[key]
	guard.failures++
	backoff := subtitleWarmFailureBaseCooldown
	for i := 1; i < guard.failures && backoff < subtitleWarmFailureMaxCooldown; i++ {
		backoff *= 2
	}
	if backoff > subtitleWarmFailureMaxCooldown {
		backoff = subtitleWarmFailureMaxCooldown
	}
	guard.retryAt = now.Add(backoff)
	c.warmGuard[key] = guard
}

// WarmInBackground starts a detached full-track extract that fills the cache
// entry for opts' source+track, so future windowed requests can extract from
// the small cached .sup instead of the original file. The warm runs on a
// background context with a generous timeout — it must survive the request
// that triggered it. requestID is the triggering request's ID, captured at the
// boundary because the detached context no longer carries it; it is threaded
// into the warm's traces so they join that request. BeginFill's in-flight
// coalescing guarantees at most one fill per track (a concurrent client-driven
// fill wins and the warm is skipped), and warmSem bounds warms server-wide:
// beyond the budget the warm is dropped, not queued — the next windowed miss
// re-attempts it. A failed warm puts the identity in a cooldown so repeated
// misses do not each spawn a fresh full-track remote read. A nil receiver is a
// no-op.
func (c *SubtitleCache) WarmInBackground(opts StreamExtractOpts, extract SUPExtractFunc, requestID string) {
	if c == nil || extract == nil {
		return
	}
	key, keyOK := c.formatEntryKey(opts.InputPath, opts.CacheIdentity, opts.TrackIndex, subtitleFormatSUP)
	if !keyOK {
		return
	}
	if !c.warmAdmitted(key, time.Now()) {
		slog.Debug("subtitle cache warm skipped: failure cooldown",
			"input", opts.InputPath, "track", opts.TrackIndex)
		return
	}
	select {
	case c.warmSem <- struct{}{}:
	default:
		slog.Debug("subtitle cache warm skipped: all warm slots busy",
			"input", opts.InputPath, "track", opts.TrackIndex)
		return
	}
	fill := c.beginFill(opts.InputPath, opts.CacheIdentity, opts.TrackIndex, subtitleFormatSUP)
	if fill == nil {
		// Another fill (client-driven or a previous warm) is already in
		// flight, or the cache is unusable — either way, nothing to do.
		<-c.warmSem
		return
	}

	// Full-track options: the warm ignores the triggering request's window
	// and writes only to the cache temp file (no response writer).
	opts.SeekSeconds = 0
	opts.DurationSeconds = 0
	opts.WindowRequested = false
	opts.AllowWindow = false
	opts.InputIsExtractedSup = false
	counter := &subtitleCountingWriter{w: fill.Tee(io.Discard)}
	opts.Writer = counter

	go func() {
		defer func() { <-c.warmSem }()
		ctx, cancel := context.WithTimeout(context.Background(), subtitleCacheWarmTimeout)
		defer cancel()

		start := time.Now()
		slog.Info("subtitle cache warm started",
			"input", opts.InputPath, "track", opts.TrackIndex)
		if err := extract(ctx, opts); err != nil {
			fill.Discard()
			c.noteWarmOutcome(key, false, time.Now())
			slog.Log(ctx, slog.LevelWarn, "subtitle cache warm failed",
				subtitleTraceAttrsForRequest(requestID, opts.SourceCodec, SubtitleTracePhaseWarm, counter.bytes, subtitleTraceOutcome(ctx, err), time.Since(start),
					"input", opts.InputPath, "track", opts.TrackIndex, "error", err)...)
			return
		}
		if err := fill.Commit(); err != nil {
			c.noteWarmOutcome(key, false, time.Now())
			slog.Log(ctx, slog.LevelWarn, "subtitle cache warm commit failed",
				subtitleTraceAttrsForRequest(requestID, opts.SourceCodec, SubtitleTracePhaseWarm, counter.bytes, SubtitleTraceOutcomeCommitFailed, time.Since(start),
					"input", opts.InputPath, "track", opts.TrackIndex, "error", err)...)
			return
		}
		c.noteWarmOutcome(key, true, time.Now())
		slog.Log(ctx, slog.LevelInfo, "subtitle cache warm finished",
			subtitleTraceAttrsForRequest(requestID, opts.SourceCodec, SubtitleTracePhaseWarm, counter.bytes, SubtitleTraceOutcomeCommitted, time.Since(start),
				"input", opts.InputPath, "track", opts.TrackIndex)...)
	}()
}

// WarmTrackInBackground starts a detached full-track extract for one subtitle
// track in any output format (text VTT/ASS or bitmap .sup), so the first
// client fetch — full-track or windowed — hits the cache instead of paying a
// full remote demux. The returned channel closes exactly once on every path:
// when the warm finished (success or failure), and when it was skipped (warm
// slots busy, another fill in flight, cache disabled, already cached, or the
// identity is in its failure cooldown). Callers use it to release a
// request-scoped relay registration the warm held open. requestID is the
// triggering request's ID, captured at the boundary because the detached
// context no longer carries it; it is threaded into the warm's traces so they
// join that request.
//
// A warm whose extract fails is not retried immediately: noteWarmOutcome puts
// the identity in an exponentially growing cooldown, so repeated windowed
// misses do not each spawn a fresh full-track remote read (bandwidth
// amplification). The cooldown clears on the next successful commit.
//
// Staleness for identity-keyed (virtual) sources follows the same generation
// bucket as every other cache lookup: an entry committed under an
// earlier bucket is not found by later lookups, so a warm that loses its
// bucket race is simply re-kicked by the next windowed miss.
func (c *SubtitleCache) WarmTrackInBackground(opts StreamExtractOpts, extract SUPExtractFunc, requestID string) <-chan struct{} {
	done := make(chan struct{})
	format := subtitleCacheFormat(opts.SourceCodec, opts.TargetFormat)
	if c == nil || extract == nil || c.dir() == "" || format == "" {
		close(done)
		return done
	}
	// Already committed under the current bucket: nothing to warm.
	if _, _, ok := c.cachedFormatEntryPath(opts.InputPath, opts.CacheIdentity, opts.TrackIndex, format); ok {
		close(done)
		return done
	}
	key, keyOK := c.formatEntryKey(opts.InputPath, opts.CacheIdentity, opts.TrackIndex, format)
	if !keyOK {
		close(done)
		return done
	}
	if !c.warmAdmitted(key, time.Now()) {
		slog.Debug("subtitle cache warm skipped: failure cooldown",
			"input", opts.InputPath, "track", opts.TrackIndex, "format", format)
		close(done)
		return done
	}
	select {
	case c.warmSem <- struct{}{}:
	default:
		slog.Debug("subtitle cache warm skipped: all warm slots busy",
			"input", opts.InputPath, "track", opts.TrackIndex, "format", format)
		close(done)
		return done
	}
	fill := c.beginFill(opts.InputPath, opts.CacheIdentity, opts.TrackIndex, format)
	if fill == nil {
		// Another fill (client-driven or a previous warm) is already in
		// flight, or the cache is unusable — either way, nothing to do.
		<-c.warmSem
		close(done)
		return done
	}

	// Full-track options: the warm ignores the triggering request's window
	// and writes only to the cache temp file (no response writer).
	opts.SeekSeconds = 0
	opts.DurationSeconds = 0
	opts.WindowRequested = false
	opts.AllowWindow = false
	opts.InputIsExtractedSup = false
	opts.InputIsExtractedText = ""
	counter := &subtitleCountingWriter{w: fill.Tee(io.Discard)}
	opts.Writer = counter

	go func() {
		defer close(done)
		defer func() { <-c.warmSem }()
		ctx, cancel := context.WithTimeout(context.Background(), subtitleCacheWarmTimeout)
		defer cancel()

		start := time.Now()
		slog.Info("subtitle cache warm started",
			"input", opts.InputPath, "track", opts.TrackIndex, "format", format)
		if err := extract(ctx, opts); err != nil {
			fill.Discard()
			c.noteWarmOutcome(key, false, time.Now())
			slog.Log(ctx, slog.LevelWarn, "subtitle cache warm failed",
				subtitleTraceAttrsForRequest(requestID, opts.SourceCodec, SubtitleTracePhaseWarm, counter.bytes, subtitleTraceOutcome(ctx, err), time.Since(start),
					"input", opts.InputPath, "track", opts.TrackIndex, "format", format, "error", err)...)
			return
		}
		if err := fill.Commit(); err != nil {
			c.noteWarmOutcome(key, false, time.Now())
			slog.Log(ctx, slog.LevelWarn, "subtitle cache warm commit failed",
				subtitleTraceAttrsForRequest(requestID, opts.SourceCodec, SubtitleTracePhaseWarm, counter.bytes, SubtitleTraceOutcomeCommitFailed, time.Since(start),
					"input", opts.InputPath, "track", opts.TrackIndex, "format", format, "error", err)...)
			return
		}
		c.noteWarmOutcome(key, true, time.Now())
		slog.Log(ctx, slog.LevelInfo, "subtitle cache warm finished",
			subtitleTraceAttrsForRequest(requestID, opts.SourceCodec, SubtitleTracePhaseWarm, counter.bytes, SubtitleTraceOutcomeCommitted, time.Since(start),
				"input", opts.InputPath, "track", opts.TrackIndex, "format", format)...)
	}()
	return done
}

// dir resolves the cache directory, or "" when caching is disabled.
func (c *SubtitleCache) dir() string {
	if c == nil || c.transcodeDir == nil {
		return ""
	}
	base := c.transcodeDir()
	if base == "" {
		return ""
	}
	return filepath.Join(base, subtitleCacheDirName)
}

// subtitleCacheKeyPrefix identifies a source file + track ordinal regardless
// of source version; the full key appends mtime+size so a changed source
// yields a different filename.
func subtitleCacheKeyPrefix(inputPath string, trackIndex int) string {
	sum := sha256.Sum256([]byte(inputPath))
	return fmt.Sprintf("%x-s%d-", sum[:12], trackIndex)
}

func subtitleCacheFormatKey(inputPath string, trackIndex int, mtime time.Time, size int64, format string) string {
	return fmt.Sprintf("%s%d-%d.%s", subtitleCacheKeyPrefix(inputPath, trackIndex), mtime.UnixNano(), size, format)
}

// Lookup opens the cached full-track .sup extract for the given source file
// and subtitle stream ordinal. The source is stat'ed on every lookup: an
// mtime or size mismatch means the entry (if any) is stale and reads as a
// miss. On a hit the returned modTime is the *source* file's mtime — stable
// across hits, suitable for Last-Modified — while the cache file's own mtime
// is bumped to record recency for LRU eviction. The caller owns closing the
// returned file.
func (c *SubtitleCache) Lookup(inputPath string, trackIndex int) (f *os.File, modTime time.Time, ok bool) {
	return c.lookup(inputPath, "", trackIndex, subtitleFormatSUP)
}

func (c *SubtitleCache) lookup(inputPath, cacheIdentity string, trackIndex int, format string) (f *os.File, modTime time.Time, ok bool) {
	path, modTime, ok := c.cachedFormatEntryPath(inputPath, cacheIdentity, trackIndex, format)
	if !ok {
		return nil, time.Time{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, time.Time{}, false
	}
	return f, modTime, true
}

// LookupText returns a complete cached text subtitle artifact. The cache key
// includes the source path, mtime, size, stream ordinal, and output format.
func (c *SubtitleCache) LookupText(inputPath string, trackIndex int, format string) ([]byte, bool) {
	format = normalizeCachedTextSubtitleFormat(format)
	if format == "" {
		return nil, false
	}
	f, _, ok := c.lookup(inputPath, "", trackIndex, format)
	if !ok {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	return data, err == nil
}

// ExtractText reuses a complete cached text track or extracts it once for this
// request. The fill captures source identity before extraction, so a replaced
// source cannot publish an old extract under the new file's cache key.
func (c *SubtitleCache) ExtractText(ctx context.Context, inputPath string, trackIndex int, format string, extract func(context.Context) ([]byte, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	format = normalizeCachedTextSubtitleFormat(format)
	if format == "" {
		return nil, fmt.Errorf("unsupported cached subtitle format")
	}
	if data, ok := c.LookupText(inputPath, trackIndex, format); ok {
		slog.Log(ctx, slog.LevelDebug, "subtitle text served from cache",
			subtitleTraceAttrs(ctx, "", SubtitleTracePhaseServe, int64(len(data)), SubtitleTraceOutcomeCacheHit, -1,
				"track", trackIndex, "format", format)...)
		return data, nil
	}
	fill := c.beginFill(inputPath, "", trackIndex, format)
	data, err := extract(ctx)
	if err != nil {
		if fill != nil {
			fill.Discard()
		}
		return nil, err
	}
	if fill != nil {
		// Tee records cache write failures; its destination io.Discard cannot fail.
		_, _ = fill.Tee(io.Discard).Write(data)
		if err := fill.Commit(); err != nil {
			slog.Log(ctx, slog.LevelWarn, "subtitle cache commit failed",
				subtitleTraceAttrs(ctx, "", SubtitleTracePhaseWarm, int64(len(data)), SubtitleTraceOutcomeCommitFailed, -1,
					"track", trackIndex, "format", format, "error", err)...)
		}
	}
	return data, nil
}

func normalizeCachedTextSubtitleFormat(format string) string {
	switch strings.ToLower(strings.TrimPrefix(strings.TrimSpace(format), ".")) {
	case subtitleFormatSRT, subtitleCodecSubRip:
		return subtitleFormatSRT
	case subtitleFormatASS, subtitleCodecSSA:
		return subtitleFormatASS
	default:
		return ""
	}
}

// cachedEntryPath reports whether a committed entry exists for the given
// source+track and returns its path plus the source file's mtime. Like
// Lookup it stats the source on every call (a changed source reads as a
// miss) and bumps the entry's mtime to record recency for LRU eviction.
// Callers that hand the path to an external reader (ffmpeg) rather than
// opening it themselves use this instead of Lookup.
func (c *SubtitleCache) cachedEntryPath(inputPath, cacheIdentity string, trackIndex int) (path string, srcModTime time.Time, ok bool) {
	return c.cachedFormatEntryPath(inputPath, cacheIdentity, trackIndex, subtitleFormatSUP)
}

func (c *SubtitleCache) cachedFormatEntryPath(inputPath, cacheIdentity string, trackIndex int, format string) (path string, srcModTime time.Time, ok bool) {
	dir := c.dir()
	if dir == "" {
		return "", time.Time{}, false
	}
	identity, modTime, size, ok := subtitleCacheSource(inputPath, cacheIdentity, time.Now())
	if !ok {
		return "", time.Time{}, false
	}
	path = filepath.Join(dir, subtitleCacheFormatKey(identity, trackIndex, modTime, size, format))
	if _, err := os.Stat(path); err != nil {
		return "", time.Time{}, false
	}
	// Recency bump for LRU. Best-effort: a failure (e.g. read-only remount)
	// only degrades eviction ordering, not correctness.
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		slog.Debug("subtitle cache recency bump failed", "path", path, "error", err)
	}
	return path, modTime, true
}

// SubtitleCacheFill is an in-progress cache population for one track. Bytes
// are written to a temp file via the writer returned by Tee; Commit renames
// it into place atomically, Discard throws it away. Exactly one of Commit or
// Discard must be called.
type SubtitleCacheFill struct {
	c             *SubtitleCache
	key           string
	inputPath     string
	identity      string
	cacheIdentity string
	trackIndex    int
	srcMtime      time.Time
	srcSize       int64
	tmp           *os.File
	// done closes exactly once when the fill settles (Commit or Discard),
	// releasing any request that is waiting to serve the committed entry.
	done chan struct{}
	// failed flips when a temp-file write errors (e.g. disk full); the tee
	// keeps serving the client and Commit refuses to publish the entry.
	failed bool
}

// BeginFill reserves the in-flight slot for the given track and creates the
// temp file the tee will write into. Returns nil — meaning "stream without
// caching" — when caching is disabled, the source can't be stat'ed, the
// cache directory can't be created, or another fill for the same track is
// already in flight. Callers that want to wait for a competing fill and serve
// its committed entry use beginFillOrWait.
func (c *SubtitleCache) BeginFill(inputPath string, trackIndex int) *SubtitleCacheFill {
	return c.beginFill(inputPath, "", trackIndex, subtitleFormatSUP)
}

// beginFill is beginFillOrWait for callers that only want to lead a fill and
// skip when another is already in flight (background warms).
func (c *SubtitleCache) beginFill(inputPath, cacheIdentity string, trackIndex int, format string) *SubtitleCacheFill {
	fill, _ := c.beginFillOrWait(inputPath, cacheIdentity, trackIndex, format)
	return fill
}

// beginFillOrWait reserves the in-flight slot for the given track or reports
// the fill that already holds it. Exactly one of the return values is
// meaningful:
//
//   - fill != nil: the caller owns the reservation and must Commit or Discard it.
//   - fill == nil && wait != nil: another fill for the same key is in flight.
//     wait closes when that fill commits or is discarded; waiting on it and
//     then re-reading the cache lets a request serve the committed entry
//     instead of running a duplicate full demux.
//   - fill == nil && wait == nil: caching is unavailable (disabled, source
//     cannot be stat'ed, or the cache directory cannot be created). The caller
//     streams uncached.
func (c *SubtitleCache) beginFillOrWait(inputPath, cacheIdentity string, trackIndex int, format string) (*SubtitleCacheFill, <-chan struct{}) {
	if c == nil {
		return nil, nil
	}
	dir := c.dir()
	if dir == "" {
		return nil, nil
	}
	identity, modTime, size, ok := subtitleCacheSource(inputPath, cacheIdentity, time.Now())
	if !ok {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("subtitle cache dir create failed", "dir", dir, "error", err)
		return nil, nil
	}
	key := subtitleCacheFormatKey(identity, trackIndex, modTime, size, format)

	c.mu.Lock()
	if existing, busy := c.inflight[key]; busy {
		wait := existing.done
		c.mu.Unlock()
		return nil, wait
	}
	fill := &SubtitleCacheFill{
		c:             c,
		key:           key,
		inputPath:     inputPath,
		identity:      identity,
		cacheIdentity: cacheIdentity,
		trackIndex:    trackIndex,
		srcMtime:      modTime,
		srcSize:       size,
		done:          make(chan struct{}),
	}
	c.inflight[key] = fill
	c.mu.Unlock()

	tmp, err := os.CreateTemp(dir, key+".part-*")
	if err != nil {
		c.release(fill)
		slog.Warn("subtitle cache temp create failed", "dir", dir, "error", err)
		return nil, nil
	}
	fill.tmp = tmp
	return fill, nil
}

func subtitleCacheSource(inputPath, cacheIdentity string, now time.Time) (string, time.Time, int64, bool) {
	if identity := strings.TrimSpace(cacheIdentity); identity != "" {
		bucket := now.Unix() / int64(subtitleCacheGenerationBucket/time.Second)
		return identity, time.Unix(bucket*int64(subtitleCacheGenerationBucket/time.Second), 0), 0, true
	}
	src, err := os.Stat(inputPath)
	if err != nil {
		return "", time.Time{}, 0, false
	}
	return inputPath, src.ModTime(), src.Size(), true
}

// FontBundleKey is a stable identity for font-bundle caching. Virtual rows
// key on the pinned result id — resolved relay URLs rotate per registration
// and would defeat the cache (mirror of the DV RPU memo key). Local rows key
// on the file row's size and mtime so a re-probed or replaced file reads as a
// miss.
type FontBundleKey struct {
	FileID        int
	PinnedResult  string // "" for local files
	Size          int64
	MtimeUnixNano int64
	FFmpegPath    string
}

// source resolves the stable cache identity and invalidation coordinates for
// the key, mirroring subtitleCacheSource's two modes. Local rows use the file
// row's mtime/size verbatim (no generation bucket); virtual rows key on the
// pinned result id plus a generation bucket so a rotated source can
// never be served past the bucket boundary. ok is false for rows that cannot
// be keyed reliably (a local row without a usable mtime/size), which callers
// treat as "do not cache" — the same fallback the DV RPU memo uses.
func (k FontBundleKey) source(now time.Time) (identity string, modTime time.Time, size int64, ok bool) {
	if k.PinnedResult != "" {
		bucket := now.Unix() / int64(subtitleCacheGenerationBucket/time.Second)
		return fontBundleIdentity(k.FileID, k.PinnedResult, k.FFmpegPath, bucket),
			time.Unix(bucket*int64(subtitleCacheGenerationBucket/time.Second), 0), 0, true
	}
	if k.MtimeUnixNano == 0 || k.Size <= 0 {
		return "", time.Time{}, 0, false
	}
	return fontBundleIdentity(k.FileID, "", k.FFmpegPath, 0), time.Unix(0, k.MtimeUnixNano), k.Size, true
}

// fontBundleIdentity hashes the stable components of a font-bundle cache key.
// The ffmpeg path is included so a binary relocation (which can change
// extraction output) starts a fresh cache.
func fontBundleIdentity(fileID int, pinnedResult, ffmpegPath string, bucket int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s|%d", fileID, pinnedResult, ffmpegPath, bucket)))
	return fmt.Sprintf("%x", sum[:12])
}

func fontBundleCacheFileName(identity string, modTime time.Time, size int64) string {
	return fmt.Sprintf("fb-%s-%d-%d.%s", identity, modTime.UnixNano(), size, subtitleFormatFontBundle)
}

// VirtualSubtitleCacheIdentity is the credential-free, stable cache identity
// for a virtual subtitle extract. Relay URLs rotate per registration and would
// defeat the cache, so the identity is built from the pinned provider-neutral
// URI's "result=" candidate id plus the effective ffmpeg track ordinal (the
// ordinal the extraction will actually map, after any drift remap). Staleness
// is bounded by the same generation bucket subtitleCacheSource
// applies to identity-keyed entries.
//
// The hash is deliberately ordinal-based: folding in a container track id
// would change every key and orphan entries committed by earlier releases.
// Across a drift remap the start-path warm, which keys on the plan ordinal,
// can therefore miss the post-remap serve identity; the serve-path window warm
// is the authoritative populator and re-keys to the post-remap ordinal, so an
// orphaned plan-ordinal entry self-heals on the next window.
func VirtualSubtitleCacheIdentity(fileID int, virtualSourceURI string, trackIndex int) string {
	pinned := ""
	if parsed, err := url.Parse(strings.TrimSpace(virtualSourceURI)); err == nil {
		pinned = strings.TrimSpace(parsed.Query().Get("result"))
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%d", fileID, pinned, trackIndex)))
	return fmt.Sprintf("%x", sum[:12])
}

// LookupFontBundle returns the cached encoded font-bundle JSON for the key, or
// false on a miss. A nil receiver reads as a miss so cache-less handlers keep
// their uncached path.
func (c *SubtitleCache) LookupFontBundle(key FontBundleKey) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	path, ok := c.fontBundleEntryPath(key, time.Now())
	if !ok {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return data, true
}

// startFontBundleFlight registers or joins the single-flight extraction for
// key and returns the caller's result channel. The extraction runs on its own
// goroutine (singleflight.DoChan) on a context detached from every caller and
// bounded by subtitleFontBundleExtractTimeout, so a canceled waiter never
// cancels the shared work. The bool is false when the key cannot be resolved
// to a stable cache identity, in which case the caller extracts uncached.
func (c *SubtitleCache) startFontBundleFlight(ctx context.Context, key FontBundleKey, extract func(context.Context) ([]byte, error)) (<-chan singleflight.Result, bool) {
	identity, modTime, size, ok := key.source(time.Now())
	if !ok {
		return nil, false
	}
	// The flight key carries the invalidation coordinates too, so concurrent
	// requests for a changed source version (different mtime/size) do not
	// coalesce onto the stale version's extraction.
	flightKey := fmt.Sprintf("fontbundle:%s-%d-%d", identity, modTime.UnixNano(), size)
	ch := c.fontBundleFlight.DoChan(flightKey, func() (any, error) {
		// Another caller may have committed the entry while we waited to lead.
		if data, ok := c.LookupFontBundle(key); ok {
			return data, nil
		}
		extractCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), subtitleFontBundleExtractTimeout)
		defer cancel()
		data, extractErr := extract(extractCtx)
		if extractErr != nil {
			return nil, extractErr
		}
		if storeErr := c.storeFontBundle(key, data); storeErr != nil {
			slog.WarnContext(ctx, "subtitle font bundle cache store failed", "error", storeErr)
		}
		return data, nil
	})
	return ch, true
}

// fontBundleFlightOutcome converts a singleflight result into the cache API's
// (data, ready, err) shape. A timeout or cancellation is "pending", not an
// error: a client-facing caller degrades to an empty bundle while the
// detached flight keeps running. A definitive extractor failure (no usable
// attachment, malformed input) is surfaced so the handler can return 500.
func fontBundleFlightOutcome(res singleflight.Result) ([]byte, bool, error) {
	if res.Err != nil {
		if errors.Is(res.Err, context.DeadlineExceeded) || errors.Is(res.Err, context.Canceled) {
			return nil, false, nil
		}
		return nil, false, res.Err
	}
	data, _ := res.Val.([]byte)
	return data, true, nil
}

// ExtractFontBundle returns the encoded font-bundle JSON for the key, reusing
// a committed disk entry when present and otherwise running extract once under
// single-flight and writing the result through to disk for every waiter. The
// extraction runs detached from the leading request (bounded to
// subtitleFontBundleExtractTimeout), so a canceled leader does not fail the
// shared work other callers are waiting on. Callers that must not block on a
// cold extraction use ExtractFontBundleWithin instead.
func (c *SubtitleCache) ExtractFontBundle(ctx context.Context, key FontBundleKey, extract func(context.Context) ([]byte, error)) ([]byte, error) {
	if data, ok := c.LookupFontBundle(key); ok {
		return data, nil
	}
	if c == nil {
		return extract(ctx)
	}
	resultCh, ok := c.startFontBundleFlight(ctx, key, extract)
	if !ok {
		// Not reliably keyable (e.g. a local row without mtime/size): extract
		// uncached, matching the DV RPU memo's fallback.
		return extract(ctx)
	}
	data, ready, err := fontBundleFlightOutcome(<-resultCh)
	if err != nil {
		return nil, err
	}
	if !ready {
		// The shared flight timed out; the blocking API has no pending state.
		return nil, context.DeadlineExceeded
	}
	return data, nil
}

// ExtractFontBundleWithin serves a cached font bundle for key, or starts (or
// joins) the detached single-flight extraction and waits up to wait for it.
// It returns (data, true, nil) when a bundle is available, (nil, false, nil)
// when the extraction is still running — the caller should serve an empty
// bundle and let the flight land in the disk cache — or (nil, false, err) on
// a definitive extractor failure. A wait <= 0 still registers the flight but
// returns immediately, so concurrent requests coalesce instead of each
// starting their own extraction. A nil cache or an unkeyable local row falls
// back to a synchronous extract.
func (c *SubtitleCache) ExtractFontBundleWithin(ctx context.Context, key FontBundleKey, wait time.Duration, extract func(context.Context) ([]byte, error)) ([]byte, bool, error) {
	if data, ok := c.LookupFontBundle(key); ok {
		return data, true, nil
	}
	if c == nil {
		data, err := extract(ctx)
		return data, err == nil, err
	}
	resultCh, ok := c.startFontBundleFlight(ctx, key, extract)
	if !ok {
		data, err := extract(ctx)
		return data, err == nil, err
	}
	if wait <= 0 {
		return nil, false, nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case res := <-resultCh:
		return fontBundleFlightOutcome(res)
	case <-timer.C:
		return nil, false, nil
	case <-ctx.Done():
		// The caller gave up; the detached flight keeps running for the cache.
		return nil, false, nil
	}
}

// WarmFontBundleInBackground starts a detached, single-flighted font-bundle
// extraction for key so a later HTTP fetch hits the disk cache instead of
// paying the cold relay cost. It shares fontBundleFlight, the disk cache, and
// the extraction budget with ExtractFontBundleWithin, so a warm racing an HTTP
// request coalesces onto a single ffmpeg run. The returned channel closes when
// the warm settled (ran, failed, was skipped, or another fill held a warm
// slot); callers use it to release a request-scoped relay registration. A nil
// receiver or disabled cache is a no-op.
func (c *SubtitleCache) WarmFontBundleInBackground(key FontBundleKey, extract func(context.Context) ([]byte, error)) <-chan struct{} {
	done := make(chan struct{})
	if c == nil || extract == nil || c.dir() == "" {
		close(done)
		return done
	}
	if _, ok := c.LookupFontBundle(key); ok {
		close(done)
		return done
	}
	select {
	case c.warmSem <- struct{}{}:
	default:
		slog.Debug("font bundle warm skipped: all warm slots busy", "file_id", key.FileID)
		close(done)
		return done
	}
	resultCh, ok := c.startFontBundleFlight(context.Background(), key, extract)
	if !ok {
		<-c.warmSem
		close(done)
		return done
	}
	// Wait for the shared flight even when this warm joined one led by another
	// caller: the caller's relay registration must outlive the extraction that
	// may be using it.
	go func() {
		defer close(done)
		defer func() { <-c.warmSem }()
		start := time.Now()
		_, _, err := fontBundleFlightOutcome(<-resultCh)
		if err != nil {
			slog.Warn("font bundle warm failed",
				"file_id", key.FileID, "elapsed_ms", time.Since(start).Milliseconds(), "error", err)
			return
		}
		slog.Info("font bundle warm finished",
			"file_id", key.FileID, "elapsed_ms", time.Since(start).Milliseconds())
	}()
	return done
}

// fontBundleEntryPath resolves the committed cache-entry path for the key and
// stats it, bumping its mtime for LRU eviction. ok is false when the cache is
// disabled, the key is not reliably keyable, or no entry exists.
func (c *SubtitleCache) fontBundleEntryPath(key FontBundleKey, now time.Time) (string, bool) {
	dir := c.dir()
	if dir == "" {
		return "", false
	}
	identity, modTime, size, ok := key.source(now)
	if !ok {
		return "", false
	}
	path := filepath.Join(dir, fontBundleCacheFileName(identity, modTime, size))
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	// Recency bump for LRU eviction.
	now = time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		slog.Debug("subtitle cache recency bump failed", "path", path, "error", err)
	}
	return path, true
}

// storeFontBundle publishes the encoded font-bundle bytes with an atomic
// temp-file + rename commit, following the payload cache's Commit pattern. The
// temp file carries the ".part-" marker so the stale-sibling sweep reclaims it
// after a crash.
func (c *SubtitleCache) storeFontBundle(key FontBundleKey, data []byte) error {
	dir := c.dir()
	if dir == "" {
		return nil
	}
	identity, modTime, size, ok := key.source(time.Now())
	if !ok {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	final := filepath.Join(dir, fontBundleCacheFileName(identity, modTime, size))
	tmp, err := os.CreateTemp(dir, filepath.Base(final)+".part-*")
	if err != nil {
		return fmt.Errorf("subtitle font bundle temp create: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("subtitle font bundle publish: %w", err)
	}
	// Evict stale font-bundle entries so they don't accumulate past the budget.
	// The recognizer in evict already handles .fontbundle files.
	c.evict(dir)
	return nil
}

func (c *SubtitleCache) release(f *SubtitleCacheFill) {
	c.mu.Lock()
	if current, ok := c.inflight[f.key]; ok && current == f {
		delete(c.inflight, f.key)
		close(f.done)
	}
	c.mu.Unlock()
}

// Tee wraps the response writer so every chunk also lands in the fill's temp
// file. The returned writer implements http.Flusher (delegating to w when w
// does), so copyAndFlush keeps flushing cues to the client in real time. A
// temp-file write failure never fails the response — the fill is marked
// failed and the client keeps streaming.
func (f *SubtitleCacheFill) Tee(w io.Writer) io.Writer {
	flusher, _ := w.(http.Flusher)
	return &subtitleTeeWriter{w: w, flusher: flusher, fill: f}
}

type subtitleTeeWriter struct {
	w       io.Writer
	flusher http.Flusher
	fill    *SubtitleCacheFill
}

func (t *subtitleTeeWriter) Write(p []byte) (int, error) {
	if !t.fill.failed {
		if _, err := t.fill.tmp.Write(p); err != nil {
			t.fill.failed = true
			slog.Warn("subtitle cache tee write failed; continuing uncached",
				"track", t.fill.trackIndex, "error", err)
		}
	}
	return t.w.Write(p)
}

func (t *subtitleTeeWriter) Flush() {
	if t.flusher != nil {
		t.flusher.Flush()
	}
}

// Commit publishes the temp file as the cache entry: fsync, atomic rename,
// stale-sibling cleanup, then size-cap eviction. It refuses to publish (and
// discards instead) when a tee write failed or when the source file changed
// while the extract ran — a partial or mismatched entry must never be served.
func (f *SubtitleCacheFill) Commit() error {
	if f.failed {
		f.Discard()
		return errors.New("subtitle cache fill had write errors; discarded")
	}
	if identity, modTime, size, ok := subtitleCacheSource(f.inputPath, f.cacheIdentity, time.Now()); !ok ||
		identity != f.identity || !modTime.Equal(f.srcMtime) || size != f.srcSize {
		f.Discard()
		return errors.New("source file changed during extract; cache fill discarded")
	}
	defer f.c.release(f)

	tmpPath := f.tmp.Name()
	if err := f.tmp.Sync(); err != nil {
		f.closeAndRemoveTmp()
		return fmt.Errorf("sync subtitle cache temp: %w", err)
	}
	if err := f.tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close subtitle cache temp: %w", err)
	}
	dir := filepath.Dir(tmpPath)
	final := filepath.Join(dir, f.key)
	if err := os.Rename(tmpPath, final); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("publish subtitle cache entry: %w", err)
	}

	f.c.removeStaleSiblings(dir, f.identity, f.trackIndex, f.key)
	f.c.evict(dir)
	return nil
}

// Discard abandons the fill: the temp file is removed and the in-flight slot
// released. Safe to call after a failed Commit (idempotent enough — the temp
// file is already gone and re-removal is a no-op).
func (f *SubtitleCacheFill) Discard() {
	f.closeAndRemoveTmp()
	f.c.release(f)
}

func (f *SubtitleCacheFill) closeAndRemoveTmp() {
	_ = f.tmp.Close()
	if err := os.Remove(f.tmp.Name()); err != nil && !os.IsNotExist(err) {
		slog.Warn("subtitle cache temp remove failed", "path", f.tmp.Name(), "error", err)
	}
}

// removeStaleSiblings deletes committed entries for the same source+track
// with a different mtime/size suffix — the source was replaced, so those can
// never be served again.
func (c *SubtitleCache) removeStaleSiblings(dir, inputPath string, trackIndex int, keepKey string) {
	prefix := subtitleCacheKeyPrefix(inputPath, trackIndex)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if name == keepKey || !strings.HasPrefix(name, prefix) || filepath.Ext(name) != filepath.Ext(keepKey) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			slog.Warn("subtitle cache stale entry remove failed", "name", name, "error", err)
		}
	}
}

// evict is the scan-on-write LRU pass: when committed entries exceed the
// byte budget, the oldest-mtime entries are removed until the total fits.
// It also sweeps orphaned .part temp files older than stalePartMaxAge
// (crash leftovers). No background daemon — commits are rare enough that a
// directory scan per commit is cheap.
func (c *SubtitleCache) evict(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type cacheEnt struct {
		path  string
		size  int64
		mtime time.Time
	}
	var (
		ents  []cacheEnt
		total int64
	)
	now := time.Now()
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if strings.Contains(e.Name(), ".part-") {
			if now.Sub(info.ModTime()) > stalePartMaxAge {
				_ = os.Remove(path)
			}
			continue
		}
		if !slices.Contains([]string{SubtitleExtPGSV3, SubtitleExtVTTV3, SubtitleExtASSV3, ".srt", "." + subtitleFormatFontBundle}, filepath.Ext(e.Name())) {
			continue
		}
		ents = append(ents, cacheEnt{path: path, size: info.Size(), mtime: info.ModTime()})
		total += info.Size()
	}
	if total <= c.maxBytes {
		return
	}
	slices.SortFunc(ents, func(a, b cacheEnt) int { return a.mtime.Compare(b.mtime) })
	for _, e := range ents {
		if total <= c.maxBytes {
			break
		}
		if err := os.Remove(e.path); err != nil {
			if !os.IsNotExist(err) {
				slog.Warn("subtitle cache eviction remove failed", "path", e.path, "error", err)
			}
			continue
		}
		slog.Info("evicted cached subtitle track (LRU)", "path", e.path, "bytes", e.size)
		total -= e.size
	}
}
