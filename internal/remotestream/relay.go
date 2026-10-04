package remotestream

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Canonical header names reused by the relay's range cache and header
// filtering. Each literal appears once, here.
const (
	headerAccept        = "Accept"
	headerAcceptRanges  = "Accept-Ranges"
	headerAge           = "Age"
	headerCacheControl  = "Cache-Control"
	headerContentLength = "Content-Length"
	headerContentRange  = "Content-Range"
	headerContentType   = "Content-Type"
	headerDate          = "Date"
	headerETag          = "ETag"
	headerExpires       = "Expires"
	headerLastModified  = "Last-Modified"
	headerOrigin        = "Origin"
	headerReferer       = "Referer"
	headerUserAgent     = "User-Agent"
	headerVary          = "Vary"
)

// Cache-Control directive names the range cache inspects.
// relayCacheControlDirectives lowercases names, so these are lowercase too.
const (
	cacheControlNoStore = "no-store"
	cacheControlPrivate = "private"
	cacheControlNoCache = "no-cache"
	cacheControlMaxAge  = "max-age"
	cacheControlSMaxAge = "s-maxage"
)

const (
	relayEntryLifetime        = 24 * time.Hour
	relayMaxEntries           = 512
	maxPlaylistBytes          = 4 << 20
	maxRewrittenPlaylistBytes = 8 << 20
	maxPlaylistRefs           = 8192
	remoteFirstByteTimeout    = 30 * time.Second
	// remoteBodyIdleTimeout bounds how long a read from the upstream remote
	// body may pause before the relay gives up on it. Set generously enough
	// to tolerate transient Usenet provider latency and Altmount reader timeouts.
	remoteBodyIdleTimeout = 90 * time.Second
	remoteBodyChunkSize   = 256 << 10
	// remoteBodyBufferChunks bounds the per-connection read-ahead between the
	// upstream pump and the client writer. The channel already provides full
	// backpressure (the producer blocks when the client stalls, closing the
	// upstream TCP window), so this buffer only absorbs throughput jitter.
	// 16 chunks of 256 KiB ≈ 4 MiB per active stream, quartered from 64
	// (≈16 MiB): 1000 stalled streams hold ~4 GiB of queued payload instead
	// of ~16 GiB. Jitter coverage at this depth depends on bitrate (≈8s at
	// 4 Mbit/s); aggregate use still scales with concurrency, and the figure
	// counts queued payload only. This is a first step, not the aggregate
	// budget (#103 tracks validating slow-consumer throughput and total
	// memory, and a possible cross-stream cap).
	remoteBodyBufferChunks = 16

	// relayRangeCache* bound the in-memory cache of complete, small upstream
	// range responses. FFmpeg re-reads a container's index/seek tables from the
	// same byte offsets on every open+seek (a fresh process per seek), and on a
	// remote provider each of those reads is a full round trip. Caching only
	// complete, byte-bounded 2xx range responses keeps a hit byte-exact and
	// never has to synthesize a truncated body.
	relayRangeCacheTTL          = 2 * time.Minute
	relayRangeCacheMaxEntrySize = 512 << 10
	relayRangeCacheMaxEntries   = 64
	relayRangeCacheMaxTotalSize = 16 << 20

	// relayMaxFreshness is the widest origin-declared freshness the relay will
	// even try to represent: 30 days. It is a sanity bound on arithmetic, not
	// the relay's cache residency. An Age, max-age or s-maxage above it is
	// treated as malformed and refused; every derived duration (apparent age,
	// response delay, an Expires delta, lifetime minus corrected age) saturates
	// to it so a delta-seconds conversion or a pair of additions cannot wrap
	// int64 nanoseconds. It is deliberately far larger than
	// relayRangeCacheTTL: an origin may legitimately declare hours or days of
	// freshness and still yield a usable response. Local residency is bounded
	// separately, by clamping the remaining freshness to relayRangeCacheTTL
	// before the expiry is computed.
	relayMaxFreshness = 30 * 24 * time.Hour
)

// relayMaxFreshnessSeconds is relayMaxFreshness in whole delta-seconds, the
// largest value relayParseBoundedSeconds accepts.
const relayMaxFreshnessSeconds = int64(relayMaxFreshness / time.Second)

// relayRangeCacheEntry is one complete upstream range response. body is
// immutable once stored; header holds only the media headers the relay
// forwards, so a hit reproduces the original response exactly.
type relayRangeCacheEntry struct {
	status    int
	header    http.Header
	body      []byte
	expiresAt time.Time
}

// relayRangeCache is a bounded TTL cache keyed by upstream URL and exact Range
// header. now is injectable for tests.
type relayRangeCache struct {
	mu         sync.Mutex
	entries    map[string]relayRangeCacheEntry
	totalBytes int
	now        func() time.Time
}

func newRelayRangeCache() *relayRangeCache {
	return &relayRangeCache{entries: make(map[string]relayRangeCacheEntry)}
}

func (c *relayRangeCache) clock() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *relayRangeCache) get(key string) (relayRangeCacheEntry, bool) {
	if c == nil || key == "" {
		return relayRangeCacheEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return relayRangeCacheEntry{}, false
	}
	if !c.clock().Before(entry.expiresAt) {
		c.totalBytes -= len(entry.body)
		delete(c.entries, key)
		return relayRangeCacheEntry{}, false
	}
	return relayRangeCacheEntry{
		status:    entry.status,
		header:    entry.header.Clone(),
		body:      entry.body,
		expiresAt: entry.expiresAt,
	}, true
}

func (c *relayRangeCache) put(key string, entry relayRangeCacheEntry) {
	if c == nil || key == "" || len(entry.body) == 0 || len(entry.body) > relayRangeCacheMaxEntrySize {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]relayRangeCacheEntry)
	}
	now := c.clock()
	// Callers that measured origin freshness supply expiresAt; a zero value
	// (direct unit use) falls back to the bounded default. An entry that is
	// already stale by the time the full body has streamed is never stored, so
	// a concurrent reader can only ever see a complete, fresh entry.
	if entry.expiresAt.IsZero() {
		entry.expiresAt = now.Add(relayRangeCacheTTL)
	}
	if !now.Before(entry.expiresAt) {
		return
	}
	for k, existing := range c.entries {
		if !now.Before(existing.expiresAt) {
			c.totalBytes -= len(existing.body)
			delete(c.entries, k)
		}
	}
	if existing, ok := c.entries[key]; ok {
		c.totalBytes -= len(existing.body)
		delete(c.entries, key)
	}
	for len(c.entries) >= relayRangeCacheMaxEntries || c.totalBytes+len(entry.body) > relayRangeCacheMaxTotalSize {
		oldestKey := ""
		var oldest time.Time
		for k, existing := range c.entries {
			if oldestKey == "" || existing.expiresAt.Before(oldest) {
				oldestKey, oldest = k, existing.expiresAt
			}
		}
		if oldestKey == "" {
			break
		}
		c.totalBytes -= len(c.entries[oldestKey].body)
		delete(c.entries, oldestKey)
	}
	c.entries[key] = entry
	c.totalBytes += len(entry.body)
}

// relayRangeCacheKey names one complete upstream range response for a source.
// The exact Range header, the effective outbound-header identity, and the relay
// content scope are part of the key: a cache hit must answer the same request
// the upstream answered, under the same source URL (which carries provider
// credentials) and the same forwarded headers. The scope is content-addressed
// when the registration carries a content key, so a seek after a token rotation
// for the same content still hits; a registration without one (unregistered
// proxy traffic, or an entry seeded directly by a test) falls back to its token,
// preserving the generation fence. The content key is a credential-free HMAC, so
// the key never carries a credential.
func relayRangeCacheKey(target *url.URL, rangeHeader, headerIdentity, scope string) string {
	if target == nil || scope == "" {
		return ""
	}
	rangeHeader = strings.TrimSpace(rangeHeader)
	if rangeHeader == "" {
		return ""
	}
	return scope + "\x00" + target.String() + "\x00" + rangeHeader + "\x00" + headerIdentity
}

// relayContentKey derives a stable, credential-free identity for the bytes
// behind a registration from the provider URL, the forwarded headers the relay
// actually sends upstream, and the insecure flag. It is keyed with the relay's
// per-process seal key, so the result never carries a provider credential, and
// it is stable across token rotation for the same content. An empty result (a
// nil source) disables content addressing so the caller falls back to the token.
func (r *Relay) relayContentKey(source *url.URL, headers map[string]string, insecure bool) string {
	if source == nil {
		return ""
	}
	mac := hmac.New(sha256.New, r.sealKey[:])
	mac.Write([]byte(source.String()))
	mac.Write([]byte{0})
	mac.Write([]byte(relayRegistrationHeaderIdentity(headers)))
	mac.Write([]byte{0})
	if insecure {
		mac.Write([]byte{1})
	} else {
		mac.Write([]byte{0})
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// relayRegistrationHeaderIdentity hashes the registration headers the relay
// forwards upstream and that can change the response: Referer, Origin and
// User-Agent. Irrelevant headers (cookies, authorization) are excluded so two
// registrations that differ only in a header the relay discards still share one
// content key.
func relayRegistrationHeaderIdentity(headers map[string]string) string {
	picked := make([]string, 0, 6)
	for _, name := range []string{headerOrigin, headerReferer, headerUserAgent} {
		lowerName := strings.ToLower(name)
		value := ""
		for key, candidate := range headers {
			if strings.ToLower(key) == lowerName {
				value = candidate
				break
			}
		}
		picked = append(picked, lowerName, value)
	}
	sum := sha256.Sum256([]byte(strings.Join(picked, "\x1f")))
	return hex.EncodeToString(sum[:])
}

// relayRangeCacheHeaderIdentity hashes the effective outbound request headers
// that the relay forwards upstream and that can change the response: Accept,
// User-Agent, Referer and Origin. It is computed from the built upstream
// request, after the relay's defaults and the per-registration overrides are
// applied, so two requests that differ in any of these cannot share a cache
// entry even when they target the same URL and Range. The exact Range is hashed
// separately by relayRangeCacheKey; conditional headers (If-Range, If-None-Match,
// If-Modified-Since) disable the cache entirely. Headers the relay drops —
// cookies, authorization — cannot change the upstream response and are excluded.
// Values are hashed, not embedded, so a cache key never carries a credential.
func relayRangeCacheHeaderIdentity(headers http.Header) string {
	parts := make([]string, 0, 4)
	for _, name := range []string{headerAccept, headerUserAgent, headerReferer, headerOrigin} {
		parts = append(parts, strings.ToLower(name)+"\x00"+headers.Get(name))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:])
}

// relayRangeResponseCacheability reports whether a complete, byte-bounded range
// response may be cached, its declared body length, and the instant its
// freshness ends. A response is reusable only when the origin did not mark it
// non-reusable and enough of its origin-declared freshness survives. Reusable
// requires HTTP 200/206, a positive Content-Length within the entry bound, no
// no-store/private/no-cache directive, no max-age=0 or s-maxage=0, no Vary: *,
// and a positive remaining freshness.
//
// Remaining freshness is freshnessLifetime - correctedInitialAge:
//
//   - freshnessLifetime is s-maxage when present, else max-age, else
//     Expires minus Date (or minus responseReceivedAt when Date is absent),
//     else relayRangeCacheTTL when the origin sent no freshness directive.
//   - correctedInitialAge is the larger of the Age header plus the response
//     delay and the apparent age (responseReceivedAt minus Date), per RFC 9111
//     §4.2.3. See relayCorrectedInitialAge.
//
// When the origin sends no freshness directive the relay still caches for the
// bounded relayRangeCacheTTL, never longer: HTTP permits heuristic freshness,
// and FFmpeg repeats the same byte range within seconds, so a short bounded
// window cannot serve meaningfully stale bytes while preserving the seek
// optimization. A malformed Age, Date, Expires or max-age is treated as
// non-reusable because freshness cannot be established. An Age, max-age or
// s-maxage above relayMaxFreshness (the 30-day sanity ceiling, not the cache
// lifetime) is likewise non-reusable, so an oversized delta-seconds value is
// never converted (or wrapped) into a duration; the derived ages and lifetimes
// that remain saturate at that ceiling. An origin freshness longer than
// relayRangeCacheTTL is accepted and then clamped: the remaining freshness is
// min(remaining, relayRangeCacheTTL), so the returned expiry is never later
// than responseReceivedAt plus the relay's own cache lifetime.
func relayRangeResponseCacheability(response *http.Response, requestSentAt, responseReceivedAt time.Time) (int, time.Time, bool) {
	if response == nil {
		return 0, time.Time{}, false
	}
	if response.StatusCode != http.StatusPartialContent && response.StatusCode != http.StatusOK {
		return 0, time.Time{}, false
	}
	rawLength := strings.TrimSpace(response.Header.Get(headerContentLength))
	if rawLength == "" {
		return 0, time.Time{}, false
	}
	length, err := strconv.Atoi(rawLength)
	if err != nil || length <= 0 || length > relayRangeCacheMaxEntrySize {
		return 0, time.Time{}, false
	}
	directives := relayCacheControlDirectives(response.Header.Values(headerCacheControl))
	for _, blocked := range []string{cacheControlNoStore, cacheControlPrivate, cacheControlNoCache} {
		if _, ok := directives[blocked]; ok {
			return 0, time.Time{}, false
		}
	}
	if relayHasConflictingLifetime(response.Header.Values(headerCacheControl)) {
		return 0, time.Time{}, false
	}
	// Reject a malformed or zero max-age/s-maxage. A positive value longer than
	// relayRangeCacheTTL is not rejected here: it is accepted and clamped to the
	// cache lifetime when the expiry is computed.
	for _, bound := range []string{cacheControlSMaxAge, cacheControlMaxAge} {
		if value, ok := directives[bound]; ok {
			seconds, ok := relayParseBoundedSeconds(value)
			if !ok || seconds <= 0 {
				return 0, time.Time{}, false
			}
		}
	}
	if relayVaryDisablesCaching(response.Header.Values(headerVary)) {
		return 0, time.Time{}, false
	}
	lifetime, ok := relayFreshnessLifetime(directives, response.Header, responseReceivedAt)
	if !ok || lifetime <= 0 {
		return 0, time.Time{}, false
	}
	age, ok := relayCorrectedInitialAge(response.Header, requestSentAt, responseReceivedAt)
	if !ok {
		return 0, time.Time{}, false
	}
	// lifetime and age are both bounded by relayMaxFreshness, so the difference
	// cannot underflow; saturating it keeps the arithmetic bounded even if a
	// future change relaxes one of those bounds.
	remaining := relaySaturateFreshness(lifetime - age)
	if remaining <= 0 {
		return 0, time.Time{}, false
	}
	// Clamp residency, not validity: an origin may declare hours or days of
	// freshness, which is perfectly reusable but must not pin an entry longer
	// than the relay promises its clients. Cap the remaining freshness at the
	// cache lifetime instead of refusing the response.
	if remaining > relayRangeCacheTTL {
		remaining = relayRangeCacheTTL
	}
	return length, responseReceivedAt.Add(remaining), true
}

// relayParseBoundedSeconds parses a non-negative integer delta-seconds value.
// It reports ok=false for a malformed, negative, or over-ceiling value. The
// ceiling is relayMaxFreshness (30 days), a sanity bound on arithmetic rather
// than the relay's cache residency: a huge Age or max-age/s-maxage is refused
// rather than converted, because time.Duration(seconds) * time.Second would
// otherwise wrap to an arbitrary, possibly negative duration. A value inside
// the ceiling but longer than relayRangeCacheTTL is accepted and the resulting
// remaining freshness is later clamped to that cache lifetime.
func relayParseBoundedSeconds(raw string) (int64, bool) {
	seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || seconds < 0 || seconds > relayMaxFreshnessSeconds {
		return 0, false
	}
	return seconds, true
}

// relaySaturateFreshness clamps a derived duration into [0, relayMaxFreshness].
// A negative value (a future Date, or a clock that ran backwards) becomes zero;
// an above-ceiling value saturates, so two of them can be added without
// overflowing int64 nanoseconds. This bounds arithmetic only; the caller clamps
// the resulting remaining freshness to relayRangeCacheTTL.
func relaySaturateFreshness(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > relayMaxFreshness {
		return relayMaxFreshness
	}
	return d
}

// relayFreshnessLifetime returns how long a response stays fresh according to
// the origin. s-maxage wins for this shared cache; otherwise max-age; otherwise
// Expires relative to Date; otherwise the relay's own bounded default. An
// over-ceiling max-age/s-maxage is rejected, and an Expires delta beyond the
// ceiling saturates to relayMaxFreshness rather than yielding a far-future
// expiry. Either way the caller clamps the resulting remaining freshness to
// relayRangeCacheTTL.
func relayFreshnessLifetime(directives map[string]string, header http.Header, receivedAt time.Time) (time.Duration, bool) {
	for _, name := range []string{"s-maxage", "max-age"} {
		if value, ok := directives[name]; ok {
			seconds, ok := relayParseBoundedSeconds(value)
			if !ok {
				return 0, false
			}
			return time.Duration(seconds) * time.Second, true
		}
	}
	if raw := strings.TrimSpace(header.Get(headerExpires)); raw != "" {
		expires, err := relayHTTPTime(raw)
		if err != nil {
			return 0, false
		}
		base := receivedAt
		if rawDate := strings.TrimSpace(header.Get(headerDate)); rawDate != "" {
			date, err := relayHTTPTime(rawDate)
			if err != nil {
				return 0, false
			}
			base = date
		}
		lifetime := relaySaturateFreshness(expires.Sub(base))
		if lifetime <= 0 {
			return 0, false
		}
		return lifetime, true
	}
	return relayRangeCacheTTL, true
}

// relayCorrectedInitialAge returns how much of the response's freshness had
// already elapsed when the relay received it, using the RFC 9111 §4.2.3
// corrected-age rule. It is the larger of:
//
//   - apparent age: responseReceivedAt minus the origin's Date, clamped at
//     zero and treated as zero when Date is absent. This catches an origin
//     that backdates Date so a small Age understates how long the response has
//     existed.
//   - corrected Age: the Age header (zero when absent) plus the response delay
//     (responseReceivedAt minus requestSentAt). Age alone omits the time the
//     response spent in transit, so a response whose Age is just under its
//     freshness lifetime is not treated as fresh for another full lifetime.
//
// Taking the maximum means an old Date can only make a response look staler,
// never fresher, than Age claims. A malformed, negative, or over-ceiling Age
// cannot establish freshness and reports ok=false. Apparent age and response
// delay saturate at relayMaxFreshness before they are added, so a huge Date
// skew or a huge clock delta cannot overflow int64 nanoseconds or be mistaken
// for a small corrected age.
func relayCorrectedInitialAge(header http.Header, requestSentAt, responseReceivedAt time.Time) (time.Duration, bool) {
	apparentAge := time.Duration(0)
	if raw := strings.TrimSpace(header.Get(headerDate)); raw != "" {
		date, err := relayHTTPTime(raw)
		if err != nil {
			return 0, false
		}
		apparentAge = relaySaturateFreshness(responseReceivedAt.Sub(date))
	}
	ageValue := time.Duration(0)
	for _, raw := range header.Values(headerAge) {
		seconds, ok := relayParseBoundedSeconds(raw)
		if !ok {
			return 0, false
		}
		candidate := time.Duration(seconds) * time.Second
		if candidate > ageValue {
			ageValue = candidate
		}
	}
	responseDelay := relaySaturateFreshness(responseReceivedAt.Sub(requestSentAt))
	correctedAge := relaySaturateFreshness(ageValue + responseDelay)
	if apparentAge > correctedAge {
		return apparentAge, true
	}
	return correctedAge, true
}

// relayHTTPTime parses an HTTP-date tolerantly. http.ParseTime handles the
// canonical GMT forms first; the RFC 1123 fallback also accepts other zone
// abbreviations such as UTC, which http.ParseTime rejects. The freshness
// lifetime (Expires versus Date) and the corrected-age apparent age both parse
// every Date through this one function, so a Date one half accepts can never be
// rejected by the other.
func relayHTTPTime(raw string) (time.Time, error) {
	if parsed, err := http.ParseTime(raw); err == nil {
		return parsed, nil
	}
	return time.Parse(time.RFC1123, raw)
}

func relayHasConflictingLifetime(values []string) bool {
	seen := map[string]string{}
	for _, value := range values {
		for _, part := range splitCacheControlDirectives(value) {
			name, raw, _ := strings.Cut(part, "=")
			name = strings.ToLower(strings.TrimSpace(name))
			if name != "max-age" && name != "s-maxage" {
				continue
			}
			raw = strings.Trim(strings.TrimSpace(raw), `"`)
			if prior, ok := seen[name]; ok && prior != raw {
				return true
			}
			seen[name] = raw
		}
	}
	return false
}

// relayCacheControlDirectives parses Cache-Control header values into a map of
// lowercased directive names to values. Commas inside quoted directive values
// do not split the list. A bare directive maps to "".
func relayCacheControlDirectives(values []string) map[string]string {
	directives := make(map[string]string, len(values))
	for _, value := range values {
		for _, part := range splitCacheControlDirectives(value) {
			name, rawValue, _ := strings.Cut(part, "=")
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				continue
			}
			directives[name] = strings.Trim(strings.TrimSpace(rawValue), `"`)
		}
	}
	return directives
}

func splitCacheControlDirectives(value string) []string {
	parts := make([]string, 0, 4)
	var current strings.Builder
	inQuotes := false
	for _, char := range value {
		switch {
		case char == '"':
			inQuotes = !inQuotes
			current.WriteRune(char)
		case char == ',' && !inQuotes:
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteRune(char)
		}
	}
	parts = append(parts, current.String())
	return parts
}

// relayVaryDisablesCaching reports whether any Vary header lists the "*" token,
// which forbids reusing a stored response for any other request.
func relayVaryDisablesCaching(values []string) bool {
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			if strings.TrimSpace(token) == "*" {
				return true
			}
		}
	}
	return false
}

func relayCachedHeaders(response *http.Response) http.Header {
	cached := make(http.Header, 8)
	for _, header := range []string{
		headerAcceptRanges, headerContentLength, headerContentRange, headerContentType,
		headerETag, headerLastModified, headerCacheControl,
	} {
		if value := response.Header.Get(header); value != "" {
			cached.Set(header, value)
		}
	}
	return cached
}

// Relay exposes validated remote streams only on loopback. It gives FFmpeg a
// credential-free input URL while retaining Range support and applying the
// same SSRF policy to the initial request and every redirect.
type Relay struct {
	once     sync.Once
	mu       sync.Mutex
	server   *http.Server
	baseURL  string
	startErr error
	closed   bool
	entries  map[string]*relayEntry
	// content maps a credential-free content key to the live entry that holds
	// it, so a re-registration of the same content shares one entry instead of
	// minting a parallel upstream. See relayEntry.contentKey.
	content        map[string]*relayEntry
	client         *http.Client
	insecureClient *http.Client
	sealKey        [32]byte
	rangeCache     *relayRangeCache
	// readAhead bounds the aggregate read-ahead bytes queued across every
	// active stream on this relay. See relayReadAheadBudgetBytes.
	readAhead *readAheadBudget
}

type relayEntry struct {
	source   *url.URL
	baseName string
	// token is the random opaque handle in the relay URL. It is recorded so a
	// content-key reuse can return the same handle instead of minting a
	// parallel one.
	token string
	// createdAt is the registration time. It bounds an entry that never had its
	// expiresAt initialized (a directly seeded test entry) and breaks ties when
	// eviction picks the least-recently-used entry.
	createdAt time.Time
	// lastAccess is the most recent presentation of this token on the serve
	// path. Capacity eviction drops the least-recently-used entry, so a burst
	// of new registrations cannot evict a session that is still streaming.
	lastAccess time.Time
	// expiresAt is the sliding lifetime bound. A live presentation slides it
	// forward, so an actively used session does not expire mid-play. A zero
	// value falls back to createdAt + relayEntryLifetime.
	expiresAt time.Time
	// contentKey is a credential-free identity shared by registrations of the
	// same provider URL, forwarded headers, and secure flag. The range cache is
	// scoped by it and register reuses the live entry behind it, so a token
	// rotation for the same content neither orphans cached bytes nor mints a
	// parallel upstream. Empty means the token is the scope.
	contentKey string
	// refs counts the live registrations sharing this entry. release decrements
	// it and the entry is dropped at zero; a fresh registration starts at one.
	refs     int
	insecure bool // private/local destinations explicitly allowed by admin
	headers  map[string]string
	// upstreamAuthRejected records that the upstream answered a proxied request
	// with 401 or 403. The signed provider URL behind this registration is no
	// longer authorized, so a restart must renew the provider listing instead of
	// reusing the registration. It is monotonic for the entry's lifetime: a
	// renewal registers a fresh entry with a fresh token.
	upstreamAuthRejected bool
}

// expiry reports the instant this entry's lifetime ends. A registration sets
// expiresAt and slides it on use; an entry seeded without one (tests, or a
// future non-sliding path) uses the fixed createdAt bound.
func (e *relayEntry) expiry() time.Time {
	if !e.expiresAt.IsZero() {
		return e.expiresAt
	}
	return e.createdAt.Add(relayEntryLifetime)
}

// lastUsed reports the instant for least-recently-used ordering: the most
// recent serve-path presentation, or createdAt for an untouched entry.
func (e *relayEntry) lastUsed() time.Time {
	if !e.lastAccess.IsZero() {
		return e.lastAccess
	}
	return e.createdAt
}

// ProxyError reports whether an upstream failure happened before any response
// bytes were committed. Callers may safely try another provider candidate only
// when Started is false.
type ProxyError struct {
	Started bool
	Err     error
}

func (e *ProxyError) Error() string { return e.Err.Error() }
func (e *ProxyError) Unwrap() error { return e.Err }

func RetryableBeforeResponse(err error) bool {
	var proxyErr *ProxyError
	return errors.As(err, &proxyErr) && !proxyErr.Started
}

// NewRelay creates a lazy loopback relay. The listener is opened on the first
// Register call so installations that never play virtual media consume no
// socket or goroutine.
func NewRelay() *Relay {
	transport := NewSafeTransport()
	relay := &Relay{
		entries:    make(map[string]*relayEntry),
		content:    make(map[string]*relayEntry),
		rangeCache: newRelayRangeCache(),
		readAhead:  newReadAheadBudget(relayReadAheadBudgetBytes),
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: checkRedirect,
		},
	}
	if _, err := rand.Read(relay.sealKey[:]); err != nil {
		relay.startErr = errors.New("initialize remote stream relay key")
	}
	return relay
}

func (r *Relay) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		r.startErr = errors.New("remote stream relay is closed")
		return
	}
	if r.startErr != nil {
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		r.startErr = fmt.Errorf("start remote stream relay: %w", err)
		return
	}
	r.baseURL = "http://" + listener.Addr().String()
	mux := http.NewServeMux()
	mux.HandleFunc("/source/", r.handle)
	r.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	go func() {
		_ = r.server.Serve(listener)
	}()
}

// Close revokes every registered source and gracefully stops the loopback
// listener. It is safe to call more than once.
func (r *Relay) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	clear(r.entries)
	clear(r.content)
	server := r.server
	client := r.client
	insecureClient := r.insecureClient
	r.mu.Unlock()

	for _, candidate := range []*http.Client{client, insecureClient} {
		if candidate != nil {
			if transport, ok := candidate.Transport.(interface{ CloseIdleConnections() }); ok {
				transport.CloseIdleConnections()
			}
		}
	}
	if server == nil {
		return nil
	}
	return server.Shutdown(ctx)
}

// Register validates source and returns a short-lived loopback URL plus an
// idempotent release function. The provider URL never appears in the returned
// value, transcode recipe, or FFmpeg command line.
func (r *Relay) Register(ctx context.Context, source string) (string, func(), error) {
	return r.register(ctx, source, false, nil, true)
}

// RegisterInsecure registers a structurally valid source while allowing the
// owning plugin's explicit private-host opt-in. FFmpeg still receives only a
// loopback relay URL; the insecure transport is isolated to this entry.
func (r *Relay) RegisterInsecure(ctx context.Context, source string) (string, func(), error) {
	return r.register(ctx, source, true, nil, true)
}

// RegisterWithHeaders registers a source with optional upstream request headers.
func (r *Relay) RegisterWithHeaders(ctx context.Context, source string, headers map[string]string) (string, func(), error) {
	return r.register(ctx, source, false, headers, true)
}

// RegisterInsecureWithHeaders registers an insecure source with optional upstream request headers.
func (r *Relay) RegisterInsecureWithHeaders(ctx context.Context, source string, headers map[string]string) (string, func(), error) {
	return r.register(ctx, source, true, headers, true)
}

// RegisterWithHeadersFresh registers a source with optional upstream request
// headers, minting a fresh entry even when an equivalent live registration
// exists. A bounded retry uses it to present new bytes to the upstream instead
// of replaying the token the first attempt already used. Every other caller
// keeps the content-key reuse that shares one upstream and one range-cache
// scope.
func (r *Relay) RegisterWithHeadersFresh(ctx context.Context, source string, headers map[string]string) (string, func(), error) {
	return r.register(ctx, source, false, headers, false)
}

// RegisterInsecureWithHeadersFresh is RegisterWithHeadersFresh for a
// private-host source.
func (r *Relay) RegisterInsecureWithHeadersFresh(ctx context.Context, source string, headers map[string]string) (string, func(), error) {
	return r.register(ctx, source, true, headers, false)
}

func cloneHeaderMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (r *Relay) register(ctx context.Context, source string, insecure bool, headers map[string]string, reuse bool) (string, func(), error) {
	if r == nil {
		return "", nil, errors.New("remote stream relay is not configured")
	}
	var sourceURL *url.URL
	var err error
	if insecure {
		sourceURL, err = ValidateURLSyntaxAllowNonPublic(source)
	} else {
		var validated *ValidatedURL
		validated, err = ValidateURL(ctx, source)
		if validated != nil {
			sourceURL = validated.URL()
		}
	}
	if err != nil {
		return "", nil, err
	}
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return "", nil, errors.New("remote stream relay is closed")
	}
	r.once.Do(r.start)
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", nil, fmt.Errorf("create remote stream relay token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	baseName := safeRelayBaseName(sourceURL)

	contentKey := r.relayContentKey(sourceURL, headers, insecure)
	now := time.Now()
	r.mu.Lock()
	if r.startErr != nil {
		r.mu.Unlock()
		return "", nil, r.startErr
	}
	if r.closed {
		r.mu.Unlock()
		return "", nil, errors.New("remote stream relay is closed")
	}
	r.evictLocked(now)
	// The same content re-registered while the first registration is still live
	// reuses that entry instead of minting a parallel upstream: a re-resolve for
	// a seek or a reconnect shares one source, one range-cache scope, and one
	// refcounted lifetime. Only a live, non-rejected entry is reused; a rejected
	// or expired holder is superseded below. A force-fresh caller (a bounded
	// retry that must present new bytes) skips this lookup and mints a new
	// entry, superseding the old content mapping below.
	if reuse && contentKey != "" {
		if existing, ok := r.content[contentKey]; ok && existing != nil &&
			r.entries[existing.token] == existing && !existing.upstreamAuthRejected &&
			now.Before(existing.expiry()) {
			existing.refs++
			existing.lastAccess = now
			existing.expiresAt = now.Add(relayEntryLifetime)
			reuseURL := r.baseURL + "/source/" + existing.token + "/" + url.PathEscape(existing.baseName)
			existingToken := existing.token
			r.mu.Unlock()
			return reuseURL, r.releaseFunc(existingToken), nil
		}
	}
	// Bound the table by evicting the least-recently-used registrations instead
	// of refusing new ones: a hard capacity error would make every new virtual
	// playback fail once the bound is reached. An evicted entry's stream gets a
	// 404 on its next request and the client re-plans; a released entry is
	// already gone, so the coldest remaining entries are the long-lived or
	// leaked ones.
	r.evictOldestLocked(relayMaxEntries - 1)
	entry := &relayEntry{
		source: sourceURL, baseName: baseName, token: token,
		createdAt: now, lastAccess: now, expiresAt: now.Add(relayEntryLifetime),
		contentKey: contentKey, refs: 1, insecure: insecure, headers: cloneHeaderMap(headers),
	}
	r.entries[token] = entry
	if contentKey != "" {
		if r.content == nil {
			r.content = make(map[string]*relayEntry)
		}
		// A prior holder of this content key (rejected or expired but not yet
		// swept) must not later delete the new mapping when it is released.
		if prior, ok := r.content[contentKey]; ok && prior != entry {
			prior.contentKey = ""
		}
		r.content[contentKey] = entry
	}
	baseURL := r.baseURL
	r.mu.Unlock()

	return baseURL + "/source/" + token + "/" + url.PathEscape(baseName), r.releaseFunc(token), nil
}

// releaseFunc returns an idempotent release for one registration. Releasing
// drops this registration's reference; the entry itself is removed only when
// the last reference goes, so a shared content entry survives one caller's
// early release.
func (r *Relay) releaseFunc(token string) func() {
	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(func() {
			r.mu.Lock()
			r.releaseEntryLocked(token)
			r.mu.Unlock()
		})
	}
}

func (r *Relay) evictLocked(now time.Time) {
	for token, entry := range r.entries {
		if !now.Before(entry.expiry()) {
			r.deleteEntryLocked(token)
		}
	}
}

// evictOldestLocked removes the least-recently-used entries until at most limit
// remain. register calls it with relayMaxEntries-1 so a new registration always
// fits. Recency, not creation order, decides: a session that keeps requesting
// stays resident while idle registrations are dropped. Caller holds r.mu.
func (r *Relay) evictOldestLocked(limit int) {
	for len(r.entries) > limit {
		oldestToken := ""
		var oldest time.Time
		for token, entry := range r.entries {
			if oldestToken == "" || entry.lastUsed().Before(oldest) {
				oldestToken, oldest = token, entry.lastUsed()
			}
		}
		if oldestToken == "" {
			return
		}
		r.deleteEntryLocked(oldestToken)
	}
}

// entryForRequestLocked returns the live entry for a presented token. An entry
// whose lifetime has elapsed is dropped and reported as absent, so expiry is
// enforced when the token is presented rather than only when an unrelated
// registration happens to run eviction. A live presentation refreshes liveness:
// the entry becomes the most recently used and, when it carries a sliding
// expiresAt, its lifetime is extended so an actively played stream is not
// dropped mid-play. An entry seeded without an explicit expiresAt (tests) keeps
// its fixed createdAt bound. Caller holds r.mu.
func (r *Relay) entryForRequestLocked(token string, now time.Time) (*relayEntry, bool) {
	entry, ok := r.entries[token]
	if !ok {
		return nil, false
	}
	if !now.Before(entry.expiry()) {
		r.deleteEntryLocked(token)
		return nil, false
	}
	entry.lastAccess = now
	if !entry.expiresAt.IsZero() {
		entry.expiresAt = now.Add(relayEntryLifetime)
	}
	return entry, true
}

func (r *Relay) deleteEntryLocked(token string) {
	entry, ok := r.entries[token]
	if !ok {
		return
	}
	delete(r.entries, token)
	if entry.contentKey != "" && r.content[entry.contentKey] == entry {
		delete(r.content, entry.contentKey)
	}
}

// releaseEntryLocked drops one registration's reference to an entry, removing
// the entry only when the last reference is gone. A shared content entry is
// therefore removed after its final holder releases, not the first. Caller
// holds r.mu.
func (r *Relay) releaseEntryLocked(token string) {
	entry, ok := r.entries[token]
	if !ok {
		return
	}
	if entry.refs > 1 {
		entry.refs--
		return
	}
	r.deleteEntryLocked(token)
}

// RegistrationStatus is the live state of one relay registration, as reported
// by Relay.RegistrationStatus.
type RegistrationStatus int

const (
	// RegistrationAbsent means no live registration matches the URL: the relay
	// is nil or closed, the token is unknown, or the entry's lifetime elapsed.
	RegistrationAbsent RegistrationStatus = iota
	// RegistrationLive means the registration exists and its upstream has not
	// rejected credentials.
	RegistrationLive
	// RegistrationAuthRejected means the registration exists but its upstream
	// answered a proxied request with 401 or 403, so the signed URL behind it is
	// no longer authorized and must be renewed.
	RegistrationAuthRejected
)

// RegistrationStatus reports whether rawURL still names a live relay
// registration. It lets a caller holding a previously registered relay URL
// decide between reuse, renewal, and a fresh resolve without a provider call:
// an expired or evicted entry reports RegistrationAbsent (and is dropped), a
// closed relay reports RegistrationAbsent, and an entry whose upstream rejected
// credentials reports RegistrationAuthRejected. A URL that is not a relay
// source path reports RegistrationAbsent. The check is read-only apart from
// dropping an expired entry, which the presentation path would drop anyway.
func (r *Relay) RegistrationStatus(rawURL string) RegistrationStatus {
	if r == nil {
		return RegistrationAbsent
	}
	token, ok := relayTokenFromURL(rawURL)
	if !ok {
		return RegistrationAbsent
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return RegistrationAbsent
	}
	entry, ok := r.entryForRequestLocked(token, time.Now())
	if !ok {
		return RegistrationAbsent
	}
	if entry.upstreamAuthRejected {
		return RegistrationAuthRejected
	}
	return RegistrationLive
}

// relayTokenFromURL extracts the registration token from a relay source URL of
// the form <base>/source/<token>/<name>. It is deliberately structural rather
// than compared against r.baseURL so it works for a URL captured before a
// restart handed the relay a new base.
func relayTokenFromURL(rawURL string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", false
	}
	rest, found := strings.CutPrefix(parsed.Path, "/source/")
	if !found {
		return "", false
	}
	token, _, found := strings.Cut(rest, "/")
	if !found || token == "" {
		return "", false
	}
	return token, true
}

// markUpstreamAuthRejected records that the registration's upstream answered
// 401 or 403. Caller must not hold r.mu.
func (r *Relay) markUpstreamAuthRejected(token string) {
	if r == nil || token == "" {
		return
	}
	r.mu.Lock()
	if entry, ok := r.entries[token]; ok {
		entry.upstreamAuthRejected = true
	}
	r.mu.Unlock()
}

// upstreamStatusFromError extracts the HTTP status embedded by proxyWithClient
// in an upstream-failure error, or 0 when the error carried none (for example a
// transport-level connect failure, which never reached a response).
func upstreamStatusFromError(err error) int {
	if err == nil {
		return 0
	}
	const prefix = "remote stream returned HTTP "
	message := err.Error()
	if !strings.HasPrefix(message, prefix) {
		return 0
	}
	status, parseErr := strconv.Atoi(message[len(prefix):])
	if parseErr != nil {
		return 0
	}
	return status
}

// relayTokenPrefix bounds a relay token to a short, non-sensitive prefix for
// logs. The token is a random loopback handle, but only enough of it to
// correlate a log line with a registration is needed.
func relayTokenPrefix(token string) string {
	const prefixLen = 8
	if len(token) <= prefixLen {
		return token
	}
	return token[:prefixLen]
}

// logProxyFailure records why the loopback relay answered 502. Neither the
// upstream nor the source URL is safe to log in full: signed provider URLs can
// carry credentials in their query string. This logs only the destination host,
// the upstream status when the failure was an HTTP response (0 for transport
// failures), and a short relay-token prefix.
func (r *Relay) logProxyFailure(token string, target *url.URL, entry *relayEntry, proxyErr error) {
	host := ""
	if target != nil {
		host = target.Host
	}
	sourceHost := ""
	if entry != nil && entry.source != nil {
		sourceHost = entry.source.Host
	}
	slog.Warn("remote stream relay upstream failure",
		"relay_token", relayTokenPrefix(token),
		"host", host,
		"source_host", sourceHost,
		"upstream_status", upstreamStatusFromError(proxyErr),
		"error", proxyErr,
	)
}

func (r *Relay) handle(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(request.URL.Path, "/source/")
	token, suffix, found := strings.Cut(rest, "/")
	if !found || token == "" {
		http.NotFound(w, request)
		return
	}
	r.mu.Lock()
	entry, ok := r.entryForRequestLocked(token, time.Now())
	r.mu.Unlock()
	if !ok {
		http.NotFound(w, request)
		return
	}
	decodedSuffix, err := url.PathUnescape(suffix)
	if err != nil {
		http.Error(w, "invalid relay path", http.StatusBadRequest)
		return
	}
	var target *url.URL
	switch {
	case decodedSuffix == entry.baseName:
		target = entry.source
	case strings.HasPrefix(decodedSuffix, "resource/"):
		opaque, requestedName, found := strings.Cut(strings.TrimPrefix(decodedSuffix, "resource/"), "/")
		if !found || opaque == "" || requestedName == "" {
			http.NotFound(w, request)
			return
		}
		rawTarget, err := r.openReference(token, opaque, time.Now())
		if err != nil {
			http.NotFound(w, request)
			return
		}
		target, err = url.Parse(rawTarget)
		if err != nil || safeRelayBaseName(target) != requestedName {
			http.NotFound(w, request)
			return
		}
	default:
		http.NotFound(w, request)
		return
	}
	tracked := &relayResponseWriter{ResponseWriter: w}
	var proxyErr error
	if entry.insecure {
		proxyErr = r.proxyWithClient(tracked, request, target.String(), token, r.insecureHTTPClient(), entry.headers)
	} else {
		proxyErr = r.proxyWithClient(tracked, request, target.String(), token, r.client, entry.headers)
	}
	if proxyErr != nil {
		if !tracked.wroteHeader {
			r.logProxyFailure(token, target, entry, proxyErr)
			http.Error(w, "remote stream unavailable", http.StatusBadGateway)
			return
		}
		panic(http.ErrAbortHandler)
	}
}

// Proxy streams one validated provider response through Silo. Only media-safe
// request/response headers are forwarded; cookies and authorization headers
// from the Silo request are never sent upstream.
func (r *Relay) Proxy(w http.ResponseWriter, request *http.Request, source string) error {
	return r.ProxyWithHeaders(w, request, source, nil)
}

// ProxyWithHeaders streams one validated provider response with optional upstream request headers.
func (r *Relay) ProxyWithHeaders(w http.ResponseWriter, request *http.Request, source string, headers map[string]string) error {
	if r == nil {
		return errors.New("remote stream relay is not configured")
	}
	validated, err := ValidateURL(request.Context(), source)
	if err != nil {
		return err
	}
	tracked := &relayResponseWriter{ResponseWriter: w}
	if err := r.proxyWithClient(tracked, request, validated.String(), "", r.client, headers); err != nil {
		return &ProxyError{Started: tracked.wroteHeader, Err: err}
	}
	return nil
}

// ProxyInsecure is like Proxy but skips the public-address DNS validation.
// Use only when the admin has explicitly enabled allow_private_streams for
// core virtual streams — otherwise local and private IP stream URLs are
// rejected by Proxy.
func (r *Relay) ProxyInsecure(w http.ResponseWriter, request *http.Request, source string) error {
	return r.ProxyInsecureWithHeaders(w, request, source, nil)
}

// ProxyInsecureWithHeaders is like ProxyWithHeaders but allows private/local hosts when opted in.
func (r *Relay) ProxyInsecureWithHeaders(w http.ResponseWriter, request *http.Request, source string, headers map[string]string) error {
	if r == nil {
		return errors.New("remote stream relay is not configured")
	}
	parsed, err := ValidateURLSyntaxAllowNonPublic(source)
	if err != nil {
		return err
	}
	tracked := &relayResponseWriter{ResponseWriter: w}
	if err := r.proxyWithClient(tracked, request, parsed.String(), "", r.insecureHTTPClient(), headers); err != nil {
		return &ProxyError{Started: tracked.wroteHeader, Err: err}
	}
	return nil
}

// insecureHTTPClient lazily builds an HTTP client whose transport and redirect
// handling allow private/local hosts. It is used only by ProxyInsecure, which
// requires an explicit allow_private_streams opt-in.
func (r *Relay) insecureHTTPClient() *http.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.insecureClient == nil {
		r.insecureClient = &http.Client{
			Transport:     NewInsecureTransport(),
			CheckRedirect: checkRedirectAllowNonPublic,
		}
	}
	return r.insecureClient
}

func (r *Relay) proxyWithClient(w http.ResponseWriter, request *http.Request, source, relayToken string, client *http.Client, extraHeaders map[string]string) error {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		return errors.New("remote stream relay supports only GET and HEAD")
	}
	upstream, err := http.NewRequestWithContext(request.Context(), request.Method, source, nil)
	if err != nil {
		return errors.New("prepare remote stream request")
	}
	for _, header := range []string{"Range", "If-Range", "If-Modified-Since", "If-None-Match", "Accept", "User-Agent"} {
		if value := request.Header.Get(header); value != "" {
			upstream.Header.Set(header, value)
		}
	}
	if len(extraHeaders) > 0 {
		for k, v := range extraHeaders {
			kLower := strings.ToLower(k)
			if kLower == "referer" || kLower == "origin" || kLower == "user-agent" {
				upstream.Header.Set(k, v)
			}
		}
	}
	// A complete, byte-bounded range response for a registered source may
	// already be cached. Conditional requests and unregistered proxy traffic
	// never use the cache; a hit is byte-exact and returns before any upstream
	// round trip, which is what makes a fresh FFmpeg open+seek cheap. The cache
	// is scoped by the registration's content key when it has one, so a token
	// rotation for the same content still hits; otherwise the token scopes it.
	cacheScope := relayToken
	if relayToken != "" {
		r.mu.Lock()
		if entry, ok := r.entries[relayToken]; ok && entry.contentKey != "" {
			cacheScope = entry.contentKey
		}
		r.mu.Unlock()
	}
	cacheKey := ""
	if request.Method == http.MethodGet && relayToken != "" &&
		upstream.Header.Get("If-Range") == "" &&
		upstream.Header.Get("If-None-Match") == "" &&
		upstream.Header.Get("If-Modified-Since") == "" {
		cacheKey = relayRangeCacheKey(upstream.URL, upstream.Header.Get("Range"), relayRangeCacheHeaderIdentity(upstream.Header), cacheScope)
		if entry, ok := r.rangeCache.get(cacheKey); ok {
			for key, values := range entry.header {
				for _, value := range values {
					w.Header().Set(key, value)
				}
			}
			w.WriteHeader(entry.status)
			_, writeErr := w.Write(entry.body)
			return writeErr
		}
	}
	// Measure the request send time and the response receive time on the
	// cache's clock so the entry's corrected age, its expiry and every later
	// lookup use one time source (injectable in tests).
	requestSentAt := r.rangeCache.clock()
	response, err := client.Do(upstream)
	if err != nil {
		return errors.New("remote stream request failed")
	}
	responseReceivedAt := r.rangeCache.clock()
	defer func() { _ = response.Body.Close() }()
	// An upstream 401/403 means the provider URL behind this registration is no
	// longer authorized. Record it so a later restart renews the provider
	// listing instead of reusing a registration that will keep refusing.
	if relayToken != "" &&
		(response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
		r.markUpstreamAuthRejected(relayToken)
	}
	// Detect upstream sources that ignore Range headers: when we ask for a
	// byte range but get back 200 OK (full file), strip Accept-Ranges from
	// the response so clients don't assume range support and fail on seek.
	hadRange := upstream.Header.Get("Range") != ""
	if hadRange && response.StatusCode == http.StatusOK && relayToken != "" {
		response.Header.Del(headerAcceptRanges)
	}
	if response.StatusCode >= 400 && response.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		drainCtx, drainCancel := context.WithTimeout(request.Context(), 1*time.Second)
		defer drainCancel()
		stopTimer := context.AfterFunc(drainCtx, func() {
			_ = response.Body.Close()
		})
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		stopTimer()
		return fmt.Errorf("remote stream returned HTTP %d", response.StatusCode)
	}
	if isDASHManifestResponse(response, nil) {
		return errors.New("remote DASH manifests are not supported")
	}
	if request.Method == http.MethodHead || response.StatusCode == http.StatusNotModified ||
		response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusRequestedRangeNotSatisfiable ||
		strings.TrimSpace(response.Header.Get(headerContentLength)) == "0" {
		copyRemoteResponseHeaders(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		return nil
	}

	// The per-read idle timer is intentionally independent of the request
	// context. Give the pump its own cancellation scope so a timeout or an
	// early response-writing error always releases a worker that is waiting to
	// publish its next chunk, even when Proxy is called with a context whose
	// lifetime extends beyond this request.
	pumpCtx, cancelPump := context.WithCancel(request.Context())
	defer cancelPump()
	bodyChunks := pumpRemoteBody(pumpCtx, response.Body, r.readAhead)
	first, err := nextRemoteBodyChunk(request.Context(), bodyChunks, remoteFirstByteTimeout)
	if err != nil {
		return err
	}
	if len(first.data) == 0 {
		return errors.New("remote media stream returned no data")
	}
	if isDASHManifestResponse(response, first.data) {
		return errors.New("remote DASH manifests are not supported")
	}
	if isHLSPlaylistResponse(response) || looksLikeHLSPlaylist(first.data) {
		if response.StatusCode != http.StatusOK {
			return errors.New("remote HLS playlist returned an unsupported partial response")
		}
		if relayToken == "" {
			return errors.New("remote HLS playback requires a registered relay")
		}
		playlist := append([]byte(nil), first.data...)
		for first.err == nil {
			first, err = nextRemoteBodyChunk(request.Context(), bodyChunks, remoteBodyIdleTimeout)
			if err != nil {
				return err
			}
			if len(playlist)+len(first.data) > maxPlaylistBytes {
				return errors.New("remote HLS playlist exceeded size limit")
			}
			playlist = append(playlist, first.data...)
		}
		rewritten, err := r.rewritePlaylist(request.Context(), relayToken, response.Request.URL, playlist)
		if err != nil {
			return err
		}
		for _, header := range []string{headerContentType, headerCacheControl, headerLastModified} {
			if value := response.Header.Get(header); value != "" {
				w.Header().Set(header, value)
			}
		}
		w.Header().Set(headerContentLength, fmt.Sprintf("%d", len(rewritten)))
		w.WriteHeader(response.StatusCode)
		_, err = w.Write(rewritten)
		return err
	}
	// Cache only a complete, byte-bounded range response whose origin-declared
	// freshness still has time left after subtracting the response's own age.
	// The expiry computed here is carried into the entry, so a hit can never
	// outlive max-age because of when the relay happened to store the bytes.
	var cacheLength int
	var cacheExpiry time.Time
	cacheable := false
	if cacheKey != "" {
		cacheLength, cacheExpiry, cacheable = relayRangeResponseCacheability(response, requestSentAt, responseReceivedAt)
	}
	var cacheBody []byte
	if cacheable {
		cacheBody = make([]byte, 0, cacheLength)
	}
	copyRemoteResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	flusher, _ := w.(http.Flusher)
	if _, err := w.Write(first.data); err != nil {
		return err
	}
	if cacheable {
		cacheBody = append(cacheBody, first.data...)
	}
	if flusher != nil {
		flusher.Flush()
	}
	for first.err == nil {
		first, err = nextRemoteBodyChunk(request.Context(), bodyChunks, remoteBodyIdleTimeout)
		if err != nil {
			return err
		}
		if len(first.data) > 0 {
			if _, err := w.Write(first.data); err != nil {
				return err
			}
			if cacheable {
				if len(cacheBody)+len(first.data) <= cacheLength {
					cacheBody = append(cacheBody, first.data...)
				} else {
					cacheable = false
					cacheBody = nil
				}
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	if errors.Is(first.err, io.EOF) {
		// Cache only a response whose full declared body was read: a client
		// disconnect or upstream error must never leave a partial entry.
		if cacheable && len(cacheBody) == cacheLength {
			r.rangeCache.put(cacheKey, relayRangeCacheEntry{
				status:    response.StatusCode,
				header:    relayCachedHeaders(response),
				body:      cacheBody,
				expiresAt: cacheExpiry,
			})
		}
		return nil
	}
	return errors.New("read remote media stream")
}

type remoteBodyChunk struct {
	data []byte
	err  error
	// budget and reservedBytes record the aggregate read-ahead reservation this
	// chunk holds while it sits in the channel: the queue *is* the read-ahead,
	// so the pool must count a queued chunk until it is dequeued. The chunk is
	// received exactly once, by the consumer or by the pump's exit drain, and
	// that single receiver calls release. A released chunk is a no-op, so a
	// stray double call cannot corrupt the pool.
	budget        *readAheadBudget
	reservedBytes int
}

// release returns the chunk's read-ahead reservation to the relay pool. Called
// by the chunk's single receiver: the consumer on dequeue, or the pump's drain
// for a chunk no consumer ever received. Exactly one of the two runs per chunk.
func (c remoteBodyChunk) release() {
	if c.budget != nil && c.reservedBytes > 0 {
		c.budget.release(c.reservedBytes)
	}
}

func pumpRemoteBody(ctx context.Context, body io.Reader, budget *readAheadBudget) <-chan remoteBodyChunk {
	chunks := make(chan remoteBodyChunk, remoteBodyBufferChunks)
	go func() {
		// The queue is the read-ahead, so a chunk holds its reservation for as
		// long as it sits in the channel. Ownership of that reservation passes
		// with the chunk value: the consumer releases it on dequeue, or, for a
		// chunk no consumer ever receives, this drain releases it on exit. A
		// chunk value is received by exactly one receiver, so release stays
		// exactly-once and no path double-releases.
		//
		// Exit invariant: this goroutine leaves zero reservations behind on
		// every return path. The drain below is the leak fix — an abandoned
		// stream (client abort, first-byte timeout, HLS-branch return, upstream
		// read error) leaves its buffered chunks unreceived, and without the
		// drain those reservations would strand in the shared pool until every
		// acquire blocked. A closed buffered channel still yields its buffered
		// values, so ranging it releases every leftover.
		//
		// The drain waits for ctx.Done before ranging. On a graceful end the
		// consumer is still draining the buffered tail, and a plain range would
		// compete with it for channel values and steal chunks out of a healthy
		// stream. ctx is canceled by the caller once the consumer has stopped
		// (the relay cancels the pump scope on every handler return), so this
		// wait makes the drain observe only leftovers, never a live receive.
		defer func() {
			close(chunks)
			<-ctx.Done()
			for leftover := range chunks {
				leftover.release()
			}
		}()
		for {
			// Reserve a whole chunk before reading, so an exhausted aggregate
			// pool applies backpressure before the upstream read rather than
			// after a chunk is already occupying memory. A canceled acquire
			// returns without holding anything.
			if err := budget.acquire(ctx, remoteBodyChunkSize); err != nil {
				return
			}
			buffer := make([]byte, remoteBodyChunkSize)
			n, err := body.Read(buffer)
			if n == 0 && err == nil {
				// io.Reader permits (0, nil); return the whole reservation and
				// try again rather than publishing an empty chunk.
				budget.release(remoteBodyChunkSize)
				continue
			}
			// The chunk occupies only the bytes read; give the rest of the
			// reservation back before it is queued.
			if unused := remoteBodyChunkSize - n; unused > 0 {
				budget.release(unused)
			}
			chunk := remoteBodyChunk{data: buffer[:n], err: err, budget: budget, reservedBytes: n}
			select {
			case chunks <- chunk:
				// The chunk stays reserved while queued; its receiver releases
				// it. Ownership leaves the pump with the send.
			case <-ctx.Done():
				// The chunk was never delivered, so the pump still owns and
				// returns its reservation.
				chunk.release()
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return chunks
}

func nextRemoteBodyChunk(ctx context.Context, chunks <-chan remoteBodyChunk, timeout time.Duration) (remoteBodyChunk, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return remoteBodyChunk{}, ctx.Err()
	case <-timer.C:
		return remoteBodyChunk{}, errors.New("remote media stream stalled")
	case chunk, ok := <-chunks:
		if !ok {
			return remoteBodyChunk{err: io.EOF}, nil
		}
		// The consumer is the chunk's receiver, so it returns the queued
		// read-ahead reservation now that the bytes are no longer in the queue.
		// The timeout and cancellation paths above are pure receive attempts
		// that received nothing; they own no chunk and must not release. A
		// chunk the pump's exit drain receives instead is released there.
		chunk.release()
		if len(chunk.data) == 0 && chunk.err != nil && !errors.Is(chunk.err, io.EOF) {
			return remoteBodyChunk{}, errors.New("read remote media stream")
		}
		return chunk, nil
	}
}

func copyRemoteResponseHeaders(destination, source http.Header) {
	for _, header := range []string{
		headerAcceptRanges, headerContentLength, headerContentRange, headerContentType,
		headerETag, headerLastModified, headerCacheControl,
	} {
		if value := source.Get(header); value != "" {
			destination.Set(header, value)
		}
	}
}

func looksLikeHLSPlaylist(body []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(body), []byte("#EXTM3U"))
}

func isDASHManifestResponse(response *http.Response, body []byte) bool {
	if response != nil {
		contentType := strings.ToLower(response.Header.Get(headerContentType))
		if strings.Contains(contentType, "dash+xml") ||
			(response.Request != nil && response.Request.URL != nil && strings.HasSuffix(strings.ToLower(response.Request.URL.Path), ".mpd")) {
			return true
		}
	}
	probe := strings.ToLower(string(bytes.TrimSpace(body)))
	if len(probe) > 4096 {
		probe = probe[:4096]
	}
	return strings.Contains(probe, "<mpd") || strings.Contains(probe, "urn:mpeg:dash:schema:mpd")
}

func isHLSPlaylistResponse(response *http.Response) bool {
	if response == nil || response.Request == nil || response.Request.URL == nil {
		return false
	}
	contentType := strings.ToLower(response.Header.Get(headerContentType))
	if strings.Contains(contentType, "mpegurl") || strings.Contains(contentType, "m3u8") {
		return true
	}
	return strings.HasSuffix(strings.ToLower(response.Request.URL.Path), ".m3u8")
}

func (r *Relay) rewritePlaylist(ctx context.Context, parentToken string, base *url.URL, body []byte) ([]byte, error) {
	if base == nil {
		return nil, errors.New("remote HLS playlist has no base URL")
	}
	allowNonPublic := false
	if parentToken != "" {
		r.mu.Lock()
		if entry, ok := r.entries[parentToken]; ok {
			allowNonPublic = entry.insecure
		}
		r.mu.Unlock()
	}
	newline := "\n"
	if strings.Contains(string(body), "\r\n") {
		newline = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
	referenceCount := 0
	rewrittenSize := len(body)
	for index, line := range lines {
		originalLength := len(line)
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case strings.HasPrefix(trimmed, "#"):
			rewritten, err := r.rewritePlaylistAttributes(ctx, parentToken, base, line, &referenceCount, allowNonPublic)
			if err != nil {
				return nil, err
			}
			lines[index] = rewritten
		default:
			rewritten, err := r.relayPlaylistReference(ctx, parentToken, base, trimmed, &referenceCount, allowNonPublic)
			if err != nil {
				return nil, err
			}
			lines[index] = strings.Replace(line, trimmed, rewritten, 1)
		}
		rewrittenSize += len(lines[index]) - originalLength
		if rewrittenSize > maxRewrittenPlaylistBytes {
			return nil, errors.New("rewritten remote HLS playlist exceeded size limit")
		}
	}
	rewritten := []byte(strings.Join(lines, newline))
	if len(rewritten) > maxRewrittenPlaylistBytes {
		return nil, errors.New("rewritten remote HLS playlist exceeded size limit")
	}
	return rewritten, nil
}

func (r *Relay) rewritePlaylistAttributes(ctx context.Context, parentToken string, base *url.URL, line string, referenceCount *int, allowNonPublic bool) (string, error) {
	const marker = `URI="`
	offset := 0
	for {
		start := strings.Index(line[offset:], marker)
		if start < 0 {
			return line, nil
		}
		start += offset + len(marker)
		end := strings.IndexByte(line[start:], '"')
		if end < 0 {
			return "", errors.New("remote HLS playlist contains a malformed URI attribute")
		}
		end += start
		rewritten, err := r.relayPlaylistReference(ctx, parentToken, base, line[start:end], referenceCount, allowNonPublic)
		if err != nil {
			return "", err
		}
		line = line[:start] + rewritten + line[end:]
		offset = start + len(rewritten) + 1
	}
}

func (r *Relay) relayPlaylistReference(ctx context.Context, parentToken string, base *url.URL, reference string, referenceCount *int, allowNonPublic bool) (string, error) {
	if *referenceCount >= maxPlaylistRefs {
		return "", errors.New("remote HLS playlist contains too many references")
	}
	(*referenceCount)++
	parsed, err := url.Parse(strings.TrimSpace(reference))
	if err != nil {
		return "", errors.New("remote HLS playlist contains an invalid URI")
	}
	target := base.ResolveReference(parsed)
	target.Fragment = ""
	validated, err := validateURLSyntax(target.String(), allowNonPublic)
	if err != nil {
		return "", errors.New("remote HLS playlist contains an unsafe URI")
	}
	opaque, err := r.sealReference(parentToken, validated.String(), time.Now())
	if err != nil {
		return "", err
	}
	baseName := safeRelayBaseName(validated)
	return "/source/" + parentToken + "/resource/" + opaque + "/" + url.PathEscape(baseName), nil
}

func safeRelayBaseName(source *url.URL) string {
	if source == nil {
		return "stream"
	}
	extension := strings.ToLower(path.Ext(source.Path))
	if len(extension) < 2 || len(extension) > 10 {
		return "stream"
	}
	for _, char := range extension[1:] {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return "stream"
		}
	}
	switch extension {
	case ".m3u8", ".mp4", ".m4v", ".mkv", ".webm", ".ts", ".m2ts",
		".avi", ".mov", ".flv", ".wmv", ".mp3", ".m4a", ".aac", ".ac3",
		".eac3", ".flac", ".ogg", ".opus", ".wav", ".bin", ".key":
		return "stream" + extension
	default:
		return "stream"
	}
}

func (r *Relay) sealReference(parentToken, source string, now time.Time) (string, error) {
	block, err := aes.NewCipher(r.sealKey[:])
	if err != nil {
		return "", errors.New("initialize remote HLS reference encryption")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", errors.New("initialize remote HLS reference encryption")
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", errors.New("create remote HLS reference token")
	}
	plain := make([]byte, 8+len(source))
	binary.BigEndian.PutUint64(plain[:8], uint64(now.Add(relayEntryLifetime).Unix()))
	copy(plain[8:], source)
	sealed := aead.Seal(nil, nonce, plain, []byte(parentToken))
	return base64.RawURLEncoding.EncodeToString(append(nonce, sealed...)), nil
}

func (r *Relay) openReference(parentToken, opaque string, now time.Time) (string, error) {
	payload, err := base64.RawURLEncoding.DecodeString(opaque)
	if err != nil {
		return "", errors.New("invalid remote HLS reference token")
	}
	block, err := aes.NewCipher(r.sealKey[:])
	if err != nil {
		return "", errors.New("initialize remote HLS reference encryption")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil || len(payload) < aead.NonceSize() {
		return "", errors.New("invalid remote HLS reference token")
	}
	plain, err := aead.Open(nil, payload[:aead.NonceSize()], payload[aead.NonceSize():], []byte(parentToken))
	if err != nil || len(plain) < 9 {
		return "", errors.New("invalid remote HLS reference token")
	}
	expiresAt := int64(binary.BigEndian.Uint64(plain[:8]))
	if now.Unix() >= expiresAt && !r.parentLive(parentToken, now) {
		return "", errors.New("expired remote HLS reference token")
	}
	return string(plain[8:]), nil
}

// parentLive reports whether parentToken names a live registration whose
// session still holds it, and refreshes that entry's sliding lifetime as a side
// effect. A sealed child reference is AEAD-bound to its parent token, so while
// the parent session is live the child bytes stay authorized; accepting a
// child whose seal-time TTL has passed lets a long playback outlive the TTL it
// was sealed with instead of 404ing mid-segment. The parent token remains the
// true authorization bound: once the parent is released or expires, the child
// reference is refused.
func (r *Relay) parentLive(parentToken string, now time.Time) bool {
	if r == nil || parentToken == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.entryForRequestLocked(parentToken, now)
	return ok
}

type relayResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *relayResponseWriter) WriteHeader(status int) {
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *relayResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *relayResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *relayResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
