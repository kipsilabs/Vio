package altmount

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

const (
	defaultAltmountCheckMinutes = 15
	minAltmountCheckMinutes     = 5
	maxAltmountCheckMinutes     = 10080
	maxAltmountBodyBytes        = 32 << 20
	maxAltmountStateBytes       = 16 << 20
	maxAltmountHistorySlots     = 50000
	// maxErrorBodyBytes bounds the response snippet attached to an enqueue
	// error, mirroring the Prowlarr client's error budget.
	maxErrorBodyBytes = 4 << 10
	// altmountStateRetention bounds how long a completed/failed release keeps
	// influencing playback preference. The underlying NZB and its storage are
	// normally gone well before this, so it is a safety cap, not a policy knob.
	altmountStateRetention = 30 * 24 * time.Hour
	// downloadingRetention is the absolute lifetime of an in-flight download
	// record and the staleness bound on its last observation. A record still
	// reported by every queue fetch expires once it is this old (first-seen),
	// and a record whose queue fetch stopped succeeding expires this long
	// after its last observation. Minutes, not days: a slot that stops
	// appearing finished, failed (history then carries the verdict), or
	// stalled, and none of those should pin a pending verdict forever.
	downloadingRetention = 30 * time.Minute
	// downloadingNoProgressRetention is how long a slot may report the same
	// progress before it stops being pending. The queue is polled on the
	// refresh cadence, so a frozen slot is observed but never advances; this
	// retires it without waiting for the absolute lifetime cap. A slot that
	// reports no progress fields at all cannot demonstrate progress, so it is
	// bounded by downloadingRetention instead.
	downloadingNoProgressRetention = 15 * time.Minute
	// altmountQueueBudget bounds the best-effort queue enrichment so a slow or
	// unsupported queue endpoint can never delay history publication. History
	// is published before the queue is fetched, but the refresh itself still
	// waits up to this budget for the queue response plus the final persistence
	// before it returns.
	altmountQueueBudget = 5 * time.Second
	// altmountCachedBadge is the prefix AltMount's own Stremio addon puts on
	// stream names for releases already imported and still fresh. It is a
	// zero-config completion signal available even when the API is not wired.
	altmountCachedBadge = "⚡ cached"
)

var altmountHTTPClient = newRestrictedRedirectHTTPClient(20 * time.Second)

// altmountReleaseRecord is one release's source-of-truth state as reported by
// AltMount's SABnzbd-compatible history API.
//
// Identity is AltMount's own stable name for the release (its NZB name when the
// slot reports one, else the storage/relative path), reduced to a comparable
// key. It is release-scoped, so it is a durable identity a candidate can adopt
// when the Stremio answer carries no hash and Prowlarr is unwired. Size is
// AltMount's exact imported byte count, which is stronger than a rounded
// display size and can fill a candidate's otherwise-unknown size tier.
type altmountReleaseRecord struct {
	Size        int64  `json:"size,omitempty"`
	CompletedAt int64  `json:"completed_at,omitempty"`
	Identity    string `json:"identity,omitempty"`
	// SizeLeft and ETASeconds describe an in-flight download; they are only
	// set on Downloading-map records parsed from the queue API. Completed and
	// failed records leave them zero. For a Downloading record, SizeLeft and
	// ETASeconds are the last observed progress: unchanged progress means a
	// frozen slot, not a healthy one.
	SizeLeft   int64 `json:"size_left,omitempty"`
	ETASeconds int64 `json:"eta_seconds,omitempty"`
	// FirstSeenAt is when a Downloading record was first observed. It bounds
	// the record's absolute lifetime so a slot that keeps being reported but
	// never finishes cannot pin a pending verdict on its observation alone.
	FirstSeenAt int64 `json:"first_seen_at,omitempty"`
	// LastProgressAt is when a Downloading record's progress fields (SizeLeft
	// or ETASeconds) last changed. A record whose progress never changes goes
	// stale even while the queue keeps reporting it. Zero means the slot
	// reported no progress fields, so it is bounded only by FirstSeenAt.
	LastProgressAt int64 `json:"last_progress_at,omitempty"`
}

type altmountStateSnapshot struct {
	Completed map[string]altmountReleaseRecord `json:"completed"`
	Failed    map[string]altmountReleaseRecord `json:"failed"`
	// Downloading holds releases AltMount is actively fetching (SABnzbd queue,
	// not history) that are still inside their pending window. A downloading
	// release is neither dead nor ready: it is the pending state between
	// listed and failed.
	Downloading map[string]altmountReleaseRecord `json:"downloading,omitempty"`
	// ExpiredDownloading retains the identity and timestamps of in-flight
	// observations that outlived their pending window while the queue kept
	// reporting the slot. They never make a release pending, but dropping them
	// would let the next successful refresh see the still-present slot as new
	// and restart its FirstSeenAt/LastProgressAt, so a permanently stuck slot
	// could cycle back to pending forever. Entries leave when the queue stops
	// reporting the slot (disappearance) or the release reconciles to a
	// terminal verdict, and the map is persisted so a restart keeps folding
	// against them instead of reopening the same hole.
	ExpiredDownloading map[string]altmountReleaseRecord `json:"expired_downloading,omitempty"`
}

// altmountStateClient caches which releases AltMount reports as completed
// (imported, with storage) versus failed. It is refreshed on the scheduled
// monitor task, never on the playback path, and persists to disk so a restart
// keeps the known-good signal.
type altmountStateClient struct {
	mu sync.Mutex
	// refreshMu serializes whole refresh cycles, including the best-effort
	// queue enrichment and the state-file write. Without it, two concurrent
	// refreshes can both merge against the same prior snapshot and then
	// publish in completion order, so a slower, older refresh overwrites a
	// newer completion; persistence, which sits outside any order fence,
	// then writes the stale snapshot to disk. mu still guards the published
	// snapshot, so readers (ReleaseCompleted/ReleaseFailed/Downloading and
	// ClassifyCandidates) keep seeing the early history publication and are
	// never blocked by a refresh in flight.
	refreshMu sync.Mutex
	url       string
	apiKey    string
	interval  time.Duration
	client    *http.Client
	lastFetch time.Time
	lastErr   error
	state     altmountStateSnapshot
	indexFile string
	// confirmObserver is notified once per release key when AltMount first
	// reports that release completed. It is the push signal a cache handoff
	// listens for so it does not have to poll the provider.
	confirmObserver ReleaseConfirmationObserver
	// confirmedOnce records the release keys already reported to
	// confirmObserver, so a steady completed state is announced once and not on
	// every classification or refresh.
	confirmedOnce map[string]struct{}
	// refreshHistoryBuiltHook, when non-nil, runs after a refresh has computed
	// its merged history snapshot and before it publishes it. It is nil in
	// production; a test uses it to hold one refresh in the build window so a
	// competing refresh can be proven to serialize behind it rather than
	// overwrite it. The callback must not call back into the client.
	refreshHistoryBuiltHook func()
	// refreshPublishedHook, when non-nil, runs after a refresh writes its final
	// snapshot to the state file. It is nil in production; a test uses it to
	// order the disk writes of two concurrent refreshes deterministically. The
	// callback must not call back into the client.
	refreshPublishedHook func()
}

// ReleaseConfirmationObserver is notified once per release key when AltMount
// first reports that release completed. The key is the same normalized release
// identity classification matches on (see ReleaseKey). The callback runs inline
// on the classification or refresh that discovered the transition, so it must
// be quick and must not block; a listener that needs to do work should hand it
// to a goroutine or a queue.
type ReleaseConfirmationObserver func(releaseKey string)

// ReleaseKey normalizes a release name to the comparable identity AltMount's
// classification and history matching use. It is exported so a caller can key
// a listener on the same identity without reimplementing the normalization.
func ReleaseKey(value string) string {
	return releaseNameKey(value)
}

// Client is the exported AltMount SABnzbd-history state client. It aliases
// the ported implementation verbatim so the resolver can consume it as a
// CandidateClassifier.
type Client = altmountStateClient

// StateClient is an alias of Client kept for call-site readability.
type StateClient = altmountStateClient

// candidateClassifier mirrors the resolver's CandidateClassifier interface so
// a compile-time assertion guards the port without importing the resolver.
type candidateClassifier interface {
	ClassifyCandidates(candidates []stream.StreamCandidate)
}

var _ candidateClassifier = (*Client)(nil)

func newAltmountStateClient(client *http.Client) *altmountStateClient {
	if client == nil {
		client = altmountHTTPClient
	}
	return &altmountStateClient{client: client}
}

// New creates an AltMount state client with the given HTTP client (nil selects
// the shared restricted-redirect client).
func New(client *http.Client) *Client {
	return newAltmountStateClient(client)
}

// NewStateClient is an alias of New kept for call-site readability.
func NewStateClient(client *http.Client) *StateClient {
	return newAltmountStateClient(client)
}

// NewClient is an alias of New kept for call-site readability.
func NewClient(client *http.Client) *Client {
	return newAltmountStateClient(client)
}

// URL returns the configured AltMount base URL, or empty string.
func (c *altmountStateClient) URL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.url
}

// Configure sets the AltMount base URL, SABnzbd API key, and refresh interval.
func (c *altmountStateClient) Configure(baseURL, apiKey string, intervalMinutes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	newURL := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	newKey := strings.TrimSpace(apiKey)
	if intervalMinutes < minAltmountCheckMinutes {
		intervalMinutes = defaultAltmountCheckMinutes
	}
	if intervalMinutes > maxAltmountCheckMinutes {
		intervalMinutes = maxAltmountCheckMinutes
	}
	newInterval := time.Duration(intervalMinutes) * time.Minute
	if newURL != c.url || newKey != c.apiKey || newInterval != c.interval {
		c.state = altmountStateSnapshot{}
		c.lastFetch = time.Time{}
		c.lastErr = nil
		c.confirmedOnce = nil
	}
	c.url = newURL
	c.apiKey = newKey
	c.interval = newInterval
}

func (c *altmountStateClient) ConfigureIndexFile(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	path = strings.TrimSpace(path)
	if path == "" {
		path = ".vio-virtual-library-altmount-state.json"
	}
	c.indexFile = path
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open AltMount state: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxAltmountStateBytes+1))
	if err != nil {
		return fmt.Errorf("read AltMount state: %w", err)
	}
	if len(data) > maxAltmountStateBytes {
		return fmt.Errorf("AltMount state exceeds %d bytes", maxAltmountStateBytes)
	}
	var snapshot altmountStateSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("decode AltMount state: %w", err)
	}
	c.state = pruneAltmountSnapshot(snapshot, time.Now())
	return nil
}

// ReleaseCompleted reports whether AltMount's authoritative snapshot records
// the release as completed. known is false when AltMount is unconfigured.
func (c *altmountStateClient) ReleaseCompleted(releaseKey string) (completed bool, known bool) {
	if c == nil {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.url == "" {
		return false, false
	}
	key := ReleaseKey(releaseKey)
	if key == "" {
		return false, false
	}
	_, ok := c.state.Completed[key]
	return ok, true
}

// ReleaseFailed reports whether AltMount's authoritative snapshot records the
// release as failed. known is false when AltMount is unconfigured or the key is
// empty, so an unconfigured provider is never mistaken for "not failed". It
// answers from the cached completed/failed snapshot without a network call, and
// mirrors ReleaseCompleted. It is the read half of the failed verdict that lets
// a caller holding a persisted candidate row (not just a freshly listed
// candidate) ask whether the source of truth has branded its release dead.
func (c *altmountStateClient) ReleaseFailed(releaseKey string) (failed bool, known bool) {
	if c == nil {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.url == "" {
		return false, false
	}
	key := ReleaseKey(releaseKey)
	if key == "" {
		return false, false
	}
	_, ok := c.state.Failed[key]
	return ok, true
}

// ReleaseDownloading reports whether AltMount's authoritative snapshot
// records the release as actively fetching (SABnzbd queue, not history).
// known is false when AltMount is unconfigured or the key is empty. A
// downloading release is pending: neither dead (so the pruner and the
// failed-drop must ignore it) nor ready (so the resolver may hold for it).
//
// Expiry is enforced here, at read time, not only when a refresh succeeds: a
// record whose queue fetch stopped succeeding goes stale downloadingRetention
// after its last observation, and one that keeps being reported but never
// progresses is retired once it exceeds its absolute lifetime or its
// progress-free observation budget. A stale record is reported as not
// downloading (and known), so a permanently stuck slot releases the hold
// exactly like a slot that disappeared. Reporting not-pending is deliberately
// distinct from reporting the release failed: this method never moves a record
// into the Failed map, so an exhausted pending release stays eligible for the
// resolver's retryable-pending / alternative treatment instead of being
// stamped dead.
func (c *altmountStateClient) ReleaseDownloading(releaseKey string) (downloading bool, known bool) {
	if c == nil {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.url == "" {
		return false, false
	}
	key := ReleaseKey(releaseKey)
	if key == "" {
		return false, false
	}
	record, ok := c.state.Downloading[key]
	if !ok {
		return false, true
	}
	if altmountDownloadingExpired(record, time.Now()) {
		return false, true
	}
	return true, true
}

// altmountDownloadingExpired reports whether an in-flight record has outlived
// its bounded pending window. It is the read-time half of the retention
// enforcement, so a slot whose refresh stopped arriving (or that never makes
// progress) cannot pin a pending verdict between refreshes.
//
// Expiring a record only clears its pending flag: it never converts it into a
// failed verdict and never removes it from the Completed or Failed maps. A
// caller learns "no longer pending", which is the same signal as a slot that
// disappeared, never the "confirmed dead" signal. That distinction is load
// bearing: the version liveness check treats a pending release as durable
// alive and only stamps a row dead on a confirmed-absent/confirmed-dead
// resolve, so an exhausted pending release must fall through to that
// retryable-pending / eligible-alternative treatment rather than being
// branded dead by this package.
func altmountDownloadingExpired(record altmountReleaseRecord, now time.Time) bool {
	nowUnix := now.Unix()
	// The queue fetch stopped succeeding: last observation is old. This fires
	// even when refresh has been failing, so a stuck release cannot be pinned
	// by the absence of new observations.
	if observedAt := record.LastSeen(); observedAt != 0 && nowUnix-observedAt >= int64(downloadingRetention/time.Second) {
		return true
	}
	// The queue keeps reporting the slot but its progress is frozen. A slot
	// that carries no progress fields cannot demonstrate progress, so it is
	// governed by the absolute lifetime cap above/below instead.
	if record.LastProgressAt != 0 && nowUnix-record.LastProgressAt >= int64(downloadingNoProgressRetention/time.Second) {
		return true
	}
	// Absolute lifetime from first observation, reported or not.
	if record.FirstSeenAt != 0 && nowUnix-record.FirstSeenAt >= int64(downloadingRetention/time.Second) {
		return true
	}
	return false
}

// LastSeen returns the last time a Downloading record was observed in a queue
// fetch. The Downloading map has always carried that time in CompletedAt
// (terminal records use it as the completion time), so this names the reuse
// rather than adding a redundant field.
func (r altmountReleaseRecord) LastSeen() int64 {
	return r.CompletedAt
}

func (c *altmountStateClient) Stale() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.url == "" || c.lastFetch.IsZero() || time.Since(c.lastFetch) >= c.interval
}

func (c *altmountStateClient) refreshIfStale(ctx context.Context) error {
	if !c.Stale() {
		return nil
	}
	return c.refresh(ctx)
}

// altmountSABnzbdBase normalizes a configured AltMount base URL to the
// SABnzbd-compatible API path. AltMount serves that API at /sabnzbd/api (the
// path Radarr/Sonarr reach after appending /api to their SABnzbd base URL), so
// an operator may enter either the host root or "/sabnzbd".
func altmountSABnzbdBase(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	lower := strings.ToLower(raw)
	for _, suffix := range []string{"/sabnzbd/api", "/sabnzbd"} {
		if strings.HasSuffix(lower, suffix) {
			raw = raw[:len(raw)-len(suffix)]
			break
		}
	}
	return strings.TrimRight(raw, "/") + "/sabnzbd/api"
}

// historyURL builds the SABnzbd-compatible history endpoint without embedding
// the API key in the URL, so it can never leak through an error or log line.
func (c *altmountStateClient) historyURL() (string, error) {
	c.mu.Lock()
	raw := c.url
	c.mu.Unlock()
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("AltMount URL is not configured")
	}
	u, err := url.Parse(altmountSABnzbdBase(raw))
	if err != nil {
		return "", fmt.Errorf("invalid AltMount URL: %w", err)
	}
	q := u.Query()
	q.Set("mode", "history")
	q.Set("output", "json")
	q.Set("limit", "10000")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// queueURL builds the SABnzbd-compatible queue endpoint the same way as
// historyURL. History carries terminal states only (Completed/Failed); the
// queue carries in-flight slots (Downloading/Queued/Fetching), which is the
// only source for the pending state between listed and failed.
func (c *altmountStateClient) queueURL() (string, error) {
	c.mu.Lock()
	raw := c.url
	c.mu.Unlock()
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("AltMount URL is not configured")
	}
	u, err := url.Parse(altmountSABnzbdBase(raw))
	if err != nil {
		return "", fmt.Errorf("invalid AltMount URL: %w", err)
	}
	q := u.Query()
	q.Set("mode", "queue")
	q.Set("output", "json")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Enqueue hands one release's download URL to AltMount's SABnzbd-compatible
// addurl endpoint and returns the nzo_id AltMount assigns. It is the package's
// only write: the state client is otherwise read-only.
//
// The URL and the SABnzbd API key are secrets and are never part of any
// returned error or log line. A status:false response or an empty nzo_ids list
// is a failure, reported with a redacted body snippet so the operator can see
// why the provider refused without the release URL leaking.
//
// The request runs under the caller's ctx bounded by an internal 15s cap, so a
// hung AltMount cannot outlive the request lifetime. It reuses the shared
// restricted-redirect client (cross-origin redirects are rejected) so a
// compromised provider cannot redirect the internal download URL elsewhere.
func (c *altmountStateClient) Enqueue(ctx context.Context, downloadURL, name string) (string, error) {
	downloadURL = strings.TrimSpace(downloadURL)
	if downloadURL == "" {
		return "", errors.New("AltMount enqueue requires a download URL")
	}
	c.mu.Lock()
	raw := c.url
	key := c.apiKey
	c.mu.Unlock()
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("AltMount URL is not configured")
	}
	u, err := url.Parse(altmountSABnzbdBase(raw))
	if err != nil {
		return "", fmt.Errorf("invalid AltMount URL: %w", err)
	}
	q := u.Query()
	q.Set("mode", "addurl")
	q.Set("name", downloadURL)
	if nzbName := strings.TrimSpace(name); nzbName != "" {
		q.Set("nzbname", nzbName)
	}
	q.Set("output", "json")
	if key != "" {
		q.Set("apikey", key)
	}
	u.RawQuery = q.Encode()

	enqueueCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(enqueueCtx, http.MethodPost, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		// The transport error may embed the request URL (which carries the
		// download URL and key); report only that the request failed.
		return "", errors.New("AltMount enqueue request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("AltMount enqueue returned status %d: %s", resp.StatusCode, c.redactedSnippet(resp.Body, downloadURL))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAltmountBodyBytes+1))
	if err != nil {
		return "", fmt.Errorf("read AltMount enqueue response: %w", err)
	}
	if int64(len(body)) > maxAltmountBodyBytes {
		return "", fmt.Errorf("AltMount enqueue response exceeds %d bytes", maxAltmountBodyBytes)
	}
	var payload struct {
		Status bool     `json:"status"`
		NzoIDs []string `json:"nzo_ids"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("decode AltMount enqueue response: %w", err)
	}
	if !payload.Status || len(payload.NzoIDs) == 0 || strings.TrimSpace(payload.NzoIDs[0]) == "" {
		return "", fmt.Errorf("AltMount rejected the enqueue: %s", c.redactEnqueueBody(body, downloadURL))
	}
	return strings.TrimSpace(payload.NzoIDs[0]), nil
}

// redactedSnippet reads a bounded response body for an error message and
// strips the configured API key and the submitted download URL. It never
// includes the request URL.
func (c *altmountStateClient) redactedSnippet(body io.Reader, downloadURL string) string {
	c.mu.Lock()
	key := c.apiKey
	c.mu.Unlock()
	data, _ := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes+1))
	if len(data) > maxErrorBodyBytes {
		data = data[:maxErrorBodyBytes]
	}
	snippet := strings.TrimSpace(string(data))
	if snippet == "" {
		snippet = "(empty response body)"
	}
	return redactAltmountSecret(snippet, key, downloadURL)
}

// redactEnqueueBody trims a SABnzbd JSON failure body and strips the API key
// and submitted download URL (a provider may echo the URL in its error).
func (c *altmountStateClient) redactEnqueueBody(body []byte, downloadURL string) string {
	c.mu.Lock()
	key := c.apiKey
	c.mu.Unlock()
	snippet := strings.TrimSpace(string(body))
	if snippet == "" {
		snippet = "(empty response body)"
	}
	return redactAltmountSecret(snippet, key, downloadURL)
}

func redactAltmountSecret(value, key, downloadURL string) string {
	if key != "" {
		value = strings.ReplaceAll(value, key, "[redacted]")
	}
	if downloadURL != "" {
		value = strings.ReplaceAll(value, downloadURL, "[redacted]")
	}
	return value
}

// Refresh performs a history fetch now and persists the merged snapshot.
// It is the exported form of the ported refresh step; HTTP logic is identical.
//
// Refreshes are serialized end to end, so a forced refresh never interleaves
// with a scheduled one and the in-memory publish, the queue enrichment, and
// the state-file write always happen in one order.
func (c *altmountStateClient) Refresh(ctx context.Context) error {
	return c.refresh(ctx)
}

func (c *altmountStateClient) refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	return c.refreshHistory(ctx)
}

// refreshHistory runs one refresh cycle with the serialization lock already
// held: fetch history, publish it in memory, enrich with the bounded queue
// fetch, then persist. Keeping the whole cycle under refreshMu is what stops
// concurrent refreshes from publishing in completion order; mu is released
// between the phases so readers are never blocked by the network work.
func (c *altmountStateClient) refreshHistory(ctx context.Context) error {
	historyURL, err := c.historyURL()
	if err != nil {
		c.mu.Lock()
		c.lastErr = err
		c.mu.Unlock()
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, historyURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	c.mu.Lock()
	key := c.apiKey
	c.mu.Unlock()
	if key != "" {
		req.Header.Set("X-Api-Key", key)
		// SABnzbd clients pass the key in the query; AltMount accepts both.
		q := req.URL.Query()
		q.Set("apikey", key)
		req.URL.RawQuery = q.Encode()
	}
	resp, err := c.client.Do(req)
	if err != nil {
		err = errors.New("AltMount history request failed")
		c.mu.Lock()
		c.lastErr = err
		c.mu.Unlock()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("AltMount history returned status %d", resp.StatusCode)
		c.mu.Lock()
		c.lastErr = err
		c.mu.Unlock()
		return err
	}
	incoming, err := parseAltmountHistory(io.LimitReader(resp.Body, maxAltmountBodyBytes+1), time.Now())
	if err != nil {
		c.mu.Lock()
		c.lastErr = err
		c.mu.Unlock()
		return err
	}

	c.mu.Lock()
	previousCompleted := c.state.Completed
	previousDownloading := c.state.Downloading
	previousExpired := c.state.ExpiredDownloading
	merged := mergeAltmountSnapshots(c.state, incoming, time.Now())
	// Carry the existing in-flight and expired-observation maps through the
	// history merge (which only covers terminal states); the bounded queue
	// enrichment below refreshes them. prune moves a slot that crossed its
	// pending window into ExpiredDownloading rather than dropping it, so the
	// identity survives the publish even when the queue fetch never arrives.
	merged.Downloading = previousDownloading
	merged.ExpiredDownloading = previousExpired
	builtHook := c.refreshHistoryBuiltHook
	c.mu.Unlock()
	if builtHook != nil {
		// Test-only seam: pause after the merged snapshot is built and before
		// it is published, so a competing refresh can be observed serializing
		// behind the whole cycle rather than racing this build.
		builtHook()
	}

	// Publish the terminal history state in memory before touching the queue.
	// Completion and failure visibility must not wait on the best-effort queue
	// endpoint: the merged snapshot becomes current here, and the queue only
	// refines the in-flight map afterward. History visibility still leads, but
	// the refresh itself does wait for the bounded queue step plus persistence.
	published := c.publishSnapshot(merged, previousCompleted)

	// In-flight slots are best-effort enrichment over the history state: an
	// AltMount without queue support (or a transient queue failure) must not
	// disturb the history state that classification already runs on. The queue
	// fetch is bounded by its own budget so a slow endpoint cannot hold the
	// refresh open beyond that budget.
	queueCtx, cancel := context.WithTimeout(ctx, altmountQueueBudget)
	queueSnapshot, queueErr := c.fetchQueueSnapshot(queueCtx)
	cancel()
	if queueErr != nil {
		slog.WarnContext(ctx, "AltMount queue request failed; keeping history state",
			"component", "altmount", "error", queueErr)
		// The history snapshot is already current in memory; persist it so the
		// verdict survives a restart, then report success unless this caller's
		// own context was canceled.
		if err := c.persistState(published); err != nil {
			return err
		}
		c.runRefreshPublishedHook()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return nil
	}
	c.mu.Lock()
	// Fold against the union of still-pending and already-expired observations:
	// the in-memory publish may have already moved an expired slot out of the
	// Downloading map, and folding only against the pending map would reset its
	// FirstSeenAt and revive it. The union still holds the original timestamps,
	// so a slot the queue keeps reporting but that is past its lifetime stays
	// retired. reconcile then drops both maps for any slot that disappeared
	// from the queue or reconciled to a terminal verdict.
	current := c.state
	prior := mergeAltmountDownloading(previousDownloading, previousExpired)
	observed := observeAltmountQueue(prior, queueSnapshot.Downloading, time.Now())
	current.Downloading, current.ExpiredDownloading = reconcileAltmountDownloading(observed, current.Completed, current.Failed, time.Now())
	c.mu.Unlock()
	// Re-publish with the refined in-flight map and write the final snapshot to
	// disk exactly once. The terminal map is unchanged, so passing it as its own
	// previous set reports no duplicate completion.
	final := c.publishSnapshot(current, current.Completed)
	if err := c.persistState(final); err != nil {
		return err
	}
	c.runRefreshPublishedHook()
	return nil
}

// runRefreshPublishedHook invokes the test-only post-persist seam, if set. The
// callback runs with refreshMu held and outside c.mu, so a test can order two
// refreshes' writes without deadlocking the reader lock.
func (c *altmountStateClient) runRefreshPublishedHook() {
	c.mu.Lock()
	hook := c.refreshPublishedHook
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
}

// publishSnapshot makes snapshot the client's current state and reports the
// uncached -> cached transitions versus previousCompleted. It prunes expired
// records first. Persistence is left to the caller so a refresh can publish
// history in memory before the queue round-trip and then write the final
// snapshot to disk exactly once.
func (c *altmountStateClient) publishSnapshot(snapshot altmountStateSnapshot, previousCompleted map[string]altmountReleaseRecord) altmountStateSnapshot {
	snapshot = pruneAltmountSnapshot(snapshot, time.Now())
	c.mu.Lock()
	c.state = snapshot
	c.lastFetch = time.Now()
	c.lastErr = nil
	c.mu.Unlock()
	// Report the refresh's own uncached -> cached transitions. A serve may not
	// happen for an already-playing session, so this is what lets a cache
	// handoff react to a fill that completed during playback.
	c.notifyConfirmed(newlyCompletedKeys(previousCompleted, snapshot.Completed)...)
	return snapshot
}

// persistState writes snapshot to the configured index file, if any.
func (c *altmountStateClient) persistState(snapshot altmountStateSnapshot) error {
	c.mu.Lock()
	indexFile := c.indexFile
	c.mu.Unlock()
	if indexFile == "" {
		return nil
	}
	return saveAltmountState(indexFile, snapshot)
}

// observeAltmountQueue folds a freshly fetched in-flight map into the previous
// one, preserving each slot's first-seen and last-progress timestamps. Progress
// is sampled once per refresh: a slot that keeps being reported with unchanged
// progress keeps its original LastProgressAt and eventually expires, instead of
// having its clock reset by every observation.
func observeAltmountQueue(previous, incoming map[string]altmountReleaseRecord, now time.Time) map[string]altmountReleaseRecord {
	observed := make(map[string]altmountReleaseRecord, len(incoming))
	for key, record := range incoming {
		prev, had := previous[key]
		if had && prev.FirstSeenAt != 0 {
			record.FirstSeenAt = prev.FirstSeenAt
		} else {
			record.FirstSeenAt = now.Unix()
		}
		hasProgress := record.SizeLeft != 0 || record.ETASeconds != 0
		sameProgress := had && prev.SizeLeft == record.SizeLeft && prev.ETASeconds == record.ETASeconds
		switch {
		case !hasProgress:
			// No progress fields to compare: the slot cannot demonstrate
			// progress, so it is bounded only by the absolute lifetime cap.
			record.LastProgressAt = 0
		case sameProgress && prev.LastProgressAt != 0:
			record.LastProgressAt = prev.LastProgressAt
		default:
			record.LastProgressAt = now.Unix()
		}
		observed[key] = record
	}
	return observed
}

// fetchQueueSnapshot fetches AltMount's in-flight queue slots. A failure is
// the caller's to tolerate: the queue is best-effort enrichment over the
// history state, so this returns the error for a warn-and-continue rather
// than recording it as the client's authoritative error.
func (c *altmountStateClient) fetchQueueSnapshot(ctx context.Context) (altmountStateSnapshot, error) {
	queueURL, err := c.queueURL()
	if err != nil {
		return altmountStateSnapshot{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, queueURL, nil)
	if err != nil {
		return altmountStateSnapshot{}, err
	}
	req.Header.Set("Accept", "application/json")
	c.mu.Lock()
	key := c.apiKey
	c.mu.Unlock()
	if key != "" {
		req.Header.Set("X-Api-Key", key)
		q := req.URL.Query()
		q.Set("apikey", key)
		req.URL.RawQuery = q.Encode()
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return altmountStateSnapshot{}, errors.New("AltMount queue request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return altmountStateSnapshot{}, fmt.Errorf("AltMount queue returned status %d", resp.StatusCode)
	}
	return parseAltmountQueue(io.LimitReader(resp.Body, maxAltmountBodyBytes+1), time.Now())
}

func parseAltmountHistory(r io.Reader, now time.Time) (altmountStateSnapshot, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxAltmountBodyBytes+1))
	if err != nil {
		return altmountStateSnapshot{}, fmt.Errorf("read AltMount history: %w", err)
	}
	if int64(len(data)) > maxAltmountBodyBytes {
		return altmountStateSnapshot{}, fmt.Errorf("AltMount history exceeds %d bytes", maxAltmountBodyBytes)
	}
	var payload struct {
		History struct {
			Slots []struct {
				Name         string `json:"name"`
				NzbName      string `json:"nzb_name"`
				Status       string `json:"status"`
				Storage      string `json:"storage"`
				Path         string `json:"path"`
				Bytes        int64  `json:"bytes"`
				Completetime int64  `json:"completetime"`
			} `json:"slots"`
		} `json:"history"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return altmountStateSnapshot{}, fmt.Errorf("decode AltMount history: %w", err)
	}
	snapshot := altmountStateSnapshot{
		Completed: map[string]altmountReleaseRecord{},
		Failed:    map[string]altmountReleaseRecord{},
	}
	if len(payload.History.Slots) > maxAltmountHistorySlots {
		return altmountStateSnapshot{}, fmt.Errorf("AltMount history exceeds %d slots", maxAltmountHistorySlots)
	}
	for _, slot := range payload.History.Slots {
		completedAt := slot.Completetime
		if completedAt <= 0 {
			completedAt = now.Unix()
		}
		record := altmountReleaseRecord{
			Size:        slot.Bytes,
			CompletedAt: completedAt,
			Identity:    altmountReleaseIdentity(slot.NzbName, slot.Name, slot.Storage, slot.Path),
		}
		keys := altmountSlotKeys(slot.Name, slot.NzbName, slot.Storage, slot.Path)
		switch {
		case strings.EqualFold(slot.Status, "Completed") && strings.TrimSpace(slot.Storage) != "":
			for _, key := range keys {
				snapshot.Completed[key] = record
			}
		case strings.EqualFold(slot.Status, "Failed"):
			for _, key := range keys {
				snapshot.Failed[key] = record
			}
		}
	}
	return snapshot, nil
}

// parseAltmountQueue decodes SABnzbd-compatible queue slots into the
// Downloading map. Only active-progress statuses are kept (Downloading,
// Fetching, Propagating, Queued); Paused slots are operator-held, not
// progressing, and are left out. All progress fields are optional: a slot
// with a usable name but no sizes still marks the release pending. Keys reuse
// altmountSlotKeys on the slot filename so queue entries match the history
// entries derived from the same NZB name.
func parseAltmountQueue(r io.Reader, now time.Time) (altmountStateSnapshot, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxAltmountBodyBytes+1))
	if err != nil {
		return altmountStateSnapshot{}, fmt.Errorf("read AltMount queue: %w", err)
	}
	if int64(len(data)) > maxAltmountBodyBytes {
		return altmountStateSnapshot{}, fmt.Errorf("AltMount queue exceeds %d bytes", maxAltmountBodyBytes)
	}
	var payload struct {
		Queue struct {
			Slots []struct {
				Filename string `json:"filename"`
				NzoID    string `json:"nzo_id"`
				Status   string `json:"status"`
				MB       string `json:"mb"`
				MBLeft   string `json:"mbleft"`
				Timeleft string `json:"timeleft"`
				ETA      string `json:"eta"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return altmountStateSnapshot{}, fmt.Errorf("decode AltMount queue: %w", err)
	}
	snapshot := altmountStateSnapshot{
		Downloading: map[string]altmountReleaseRecord{},
	}
	if len(payload.Queue.Slots) > maxAltmountHistorySlots {
		return altmountStateSnapshot{}, fmt.Errorf("AltMount queue exceeds %d slots", maxAltmountHistorySlots)
	}
	for _, slot := range payload.Queue.Slots {
		switch {
		case strings.EqualFold(slot.Status, "Downloading"),
			strings.EqualFold(slot.Status, "Fetching"),
			strings.EqualFold(slot.Status, "Propagating"),
			strings.EqualFold(slot.Status, "Queued"):
		default:
			continue
		}
		keys := altmountSlotKeys(slot.Filename, slot.NzoID, "", "")
		if len(keys) == 0 {
			continue
		}
		record := altmountReleaseRecord{
			CompletedAt: now.Unix(),
			Identity:    altmountReleaseIdentity(slot.Filename, slot.NzoID),
			SizeLeft:    parseAltmountMB(slot.MBLeft),
			ETASeconds:  parseAltmountDuration(slot.Timeleft),
		}
		for _, key := range keys {
			snapshot.Downloading[key] = record
		}
	}
	return snapshot, nil
}

// parseAltmountMB parses an SABnzbd megabyte field ("123.4") into bytes.
// Empty or unparsable values report zero; the slot still marks pending.
func parseAltmountMB(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	mb, err := strconv.ParseFloat(value, 64)
	if err != nil || mb < 0 {
		return 0
	}
	return int64(mb * 1024 * 1024)
}

// parseAltmountDuration parses an SABnzbd timeleft field ("H:MM:SS",
// "MM:SS" or bare seconds) into seconds. Empty or unparsable values
// report zero; the slot still marks pending.
func parseAltmountDuration(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		return seconds
	}
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0
	}
	var total int64
	for _, part := range parts {
		digits := strings.TrimSpace(part)
		n, err := strconv.ParseInt(digits, 10, 64)
		if err != nil || n < 0 {
			return 0
		}
		total = total*60 + n
	}
	return total
}

// altmountReleaseIdentity returns AltMount's most release-stable name for a
// history slot: the NZB name the operator/ indexer submitted when present,
// else the display name, else the storage or relative path base. The value is
// normalized to the same comparable key the slot map uses, so two postings of
// one release yield one identity. Empty means the slot exposes nothing usable.
func altmountReleaseIdentity(values ...string) string {
	for _, value := range values {
		if key := releaseNameKey(value); key != "" {
			return key
		}
	}
	return ""
}

// altmountSlotKeys returns every normalized identity a history slot exposes.
// AltMount's own cache predicate matches on nzb path, storage path, and
// relative path, so mirroring all of them avoids missing a match when the
// display name differs from the stored filename.
func altmountSlotKeys(values ...string) []string {
	seen := make(map[string]struct{}, len(values))
	keys := make([]string, 0, len(values))
	for _, value := range values {
		key := releaseNameKey(value)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}

// mergeAltmountSnapshots resolves per-key state by newest report, so a release
// that completed and later failed (or vice versa) reflects AltMount's latest
// answer. Equal timestamps prefer completed, since a completed import is the
// stronger, non-destructive signal.
func mergeAltmountSnapshots(existing, incoming altmountStateSnapshot, now time.Time) altmountStateSnapshot {
	type entry struct {
		record altmountReleaseRecord
		failed bool
	}
	all := map[string]entry{}
	consider := func(records map[string]altmountReleaseRecord, failed bool) {
		for key, record := range records {
			current, ok := all[key]
			if !ok || record.CompletedAt > current.record.CompletedAt ||
				(record.CompletedAt == current.record.CompletedAt && !failed && current.failed) {
				all[key] = entry{record: record, failed: failed}
			}
		}
	}
	consider(existing.Completed, false)
	consider(existing.Failed, true)
	consider(incoming.Completed, false)
	consider(incoming.Failed, true)
	merged := altmountStateSnapshot{
		Completed: map[string]altmountReleaseRecord{},
		Failed:    map[string]altmountReleaseRecord{},
	}
	for key, item := range all {
		if item.failed {
			merged.Failed[key] = item.record
		} else {
			merged.Completed[key] = item.record
		}
	}
	return pruneAltmountSnapshot(merged, now)
}

// pruneAltmountSnapshot drops terminal records past their retention and
// partitions the in-flight map by pending window. A Downloading record that has
// expired is not discarded: its identity and timestamps move to
// ExpiredDownloading so the next successful refresh folds the still-present
// slot against them instead of restarting its clocks. Expired observations are
// only cleared by reconcileAltmountDownloading (disappearance or a terminal
// verdict), never by pruning.
func pruneAltmountSnapshot(snapshot altmountStateSnapshot, now time.Time) altmountStateSnapshot {
	pruned := altmountStateSnapshot{
		Completed:          map[string]altmountReleaseRecord{},
		Failed:             map[string]altmountReleaseRecord{},
		Downloading:        map[string]altmountReleaseRecord{},
		ExpiredDownloading: map[string]altmountReleaseRecord{},
	}
	cutoff := now.Add(-altmountStateRetention).Unix()
	for key, record := range snapshot.Completed {
		if record.CompletedAt != 0 && record.CompletedAt < cutoff {
			continue
		}
		pruned.Completed[key] = record
	}
	for key, record := range snapshot.Failed {
		if record.CompletedAt != 0 && record.CompletedAt < cutoff {
			continue
		}
		pruned.Failed[key] = record
	}
	// Downloading records use CompletedAt as their last-observed time,
	// FirstSeenAt for absolute lifetime, and LastProgressAt for the frozen
	// progress bound. An expired record is retained as an observation, not a
	// pending verdict, so a permanently stuck slot stops holding playback while
	// still remembering where it began.
	for key, record := range snapshot.Downloading {
		if altmountDownloadingExpired(record, now) {
			pruned.ExpiredDownloading[key] = record
			continue
		}
		pruned.Downloading[key] = record
	}
	// Carry previously retained observations forward. A key with a pending
	// record is already accounted for and must not also appear as expired.
	for key, record := range snapshot.ExpiredDownloading {
		if _, ok := pruned.Downloading[key]; ok {
			continue
		}
		if _, ok := pruned.ExpiredDownloading[key]; ok {
			continue
		}
		pruned.ExpiredDownloading[key] = record
	}
	return pruned
}

// mergeAltmountDownloading unions the pending and retained-expired in-flight
// observations, preferring the pending entry for a key present in both. The
// fold in observeAltmountQueue needs the original timestamps from either map;
// without the expired half, a refresh that already published (and moved an
// expired slot) would treat the still-reported slot as new.
func mergeAltmountDownloading(pending, expired map[string]altmountReleaseRecord) map[string]altmountReleaseRecord {
	merged := make(map[string]altmountReleaseRecord, len(pending)+len(expired))
	for key, record := range expired {
		merged[key] = record
	}
	for key, record := range pending {
		merged[key] = record
	}
	return merged
}

// reconcileAltmountDownloading classifies the folded queue observations into
// pending and retained-expired maps. A slot that reconciled to a terminal
// verdict is dropped from both. Slots absent from observed have disappeared
// from the queue and are cleared by omission, which is how retention ends for a
// release that neither completes nor fails.
func reconcileAltmountDownloading(observed map[string]altmountReleaseRecord, completed, failed map[string]altmountReleaseRecord, now time.Time) (pending, expired map[string]altmountReleaseRecord) {
	pending = make(map[string]altmountReleaseRecord, len(observed))
	expired = make(map[string]altmountReleaseRecord)
	for key, record := range observed {
		if _, ok := completed[key]; ok {
			continue
		}
		if _, ok := failed[key]; ok {
			continue
		}
		if altmountDownloadingExpired(record, now) {
			expired[key] = record
			continue
		}
		pending[key] = record
	}
	return pending, expired
}

func saveAltmountState(path string, snapshot altmountStateSnapshot) error {
	if snapshot.Completed == nil {
		snapshot.Completed = map[string]altmountReleaseRecord{}
	}
	if snapshot.Failed == nil {
		snapshot.Failed = map[string]altmountReleaseRecord{}
	}
	if snapshot.Downloading == nil {
		snapshot.Downloading = map[string]altmountReleaseRecord{}
	}
	if snapshot.ExpiredDownloading == nil {
		snapshot.ExpiredDownloading = map[string]altmountReleaseRecord{}
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > maxAltmountStateBytes {
		return fmt.Errorf("AltMount state exceeds %d bytes", maxAltmountStateBytes)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".silo-altmount-state-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

// ClassifyCandidates applies AltMount's authoritative state: completed
// releases are marked SourceConfirmed, failed releases SourceFailed. A
// completed record always wins over a stale failed one.
//
// A confirmed candidate additionally adopts AltMount's durable identity when
// it lacks one: the record's release-scoped key becomes a namespaced
// SourceGUID (so it can never be confused with a Prowlarr GUID), and AltMount's
// exact imported size fills an unknown candidate size. Both are additive — a
// value the candidate already carries is never overwritten — so this only
// raises identity coverage without changing which release a candidate is.
func (c *altmountStateClient) ClassifyCandidates(candidates []stream.StreamCandidate) {
	if c == nil || len(candidates) == 0 {
		return
	}
	c.mu.Lock()
	completed := c.state.Completed
	failed := c.state.Failed
	downloading := c.state.Downloading
	watchConfirmations := c.confirmObserver != nil
	c.mu.Unlock()
	now := time.Now()
	confirmedKeys := make([]string, 0, len(candidates))
	// The AltMount Stremio addon's "⚡ cached" badge is a free completion signal
	// available even when the history API is unwired. The resolver applies it
	// before classification on every serve; when a cache-handoff listener is
	// installed, apply it here too so the badge-driven uncached -> cached
	// transition is observed even though this classifier otherwise owns only the
	// history state.
	if watchConfirmations {
		markAltmountBadgeCandidates(candidates)
		for i := range candidates {
			if candidates[i].SourceConfirmed {
				if key := candidateReleaseName(candidates[i]); key != "" {
					confirmedKeys = append(confirmedKeys, key)
				}
			}
		}
	}
	if len(completed) == 0 && len(failed) == 0 && len(downloading) == 0 {
		c.notifyConfirmed(confirmedKeys...)
		return
	}
	for i := range candidates {
		key := candidateReleaseName(candidates[i])
		if key == "" {
			continue
		}
		if record, ok := completed[key]; ok && releaseSizesMatch(record.Size, candidates[i].FileSize) {
			candidates[i].SourceConfirmed = true
			adoptAltmountIdentity(&candidates[i], record)
			confirmedKeys = append(confirmedKeys, key)
			continue
		}
		if _, ok := failed[key]; ok {
			candidates[i].SourceFailed = true
			continue
		}
		// Pending only applies when neither terminal verdict exists: a
		// completed record wins (above), a failed record brands dead even
		// while a retry downloads, and only then does an in-flight slot
		// mark the candidate as worth waiting for rather than skipping.
		// A slot past its pending window is not pending: enforcing expiry at
		// classification keeps a stuck release from being re-declared pending
		// on every serve even before the next refresh prunes it.
		if record, ok := downloading[key]; ok && !altmountDownloadingExpired(record, now) {
			candidates[i].SourcePending = true
		}
	}
	c.notifyConfirmed(confirmedKeys...)
}

// SetConfirmObserver installs (or clears, with nil) the callback invoked once
// per release key when AltMount first reports that release completed. Clearing
// the observer also resets the once-only bookkeeping so a later installation
// starts fresh.
func (c *altmountStateClient) SetConfirmObserver(fn ReleaseConfirmationObserver) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.confirmObserver = fn
	if fn == nil {
		c.confirmedOnce = nil
	}
	c.mu.Unlock()
}

// notifyConfirmed reports newly completed release keys to the observer exactly
// once each. It is called with the client lock released and re-acquires it only
// to update the once-only set, so a slow observer never blocks classification.
func (c *altmountStateClient) notifyConfirmed(keys ...string) {
	if c == nil || len(keys) == 0 {
		return
	}
	c.mu.Lock()
	observer := c.confirmObserver
	if observer == nil {
		c.mu.Unlock()
		return
	}
	if c.confirmedOnce == nil {
		c.confirmedOnce = make(map[string]struct{}, len(keys))
	}
	fresh := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, ok := c.confirmedOnce[key]; ok {
			continue
		}
		c.confirmedOnce[key] = struct{}{}
		fresh = append(fresh, key)
	}
	c.mu.Unlock()
	for _, key := range fresh {
		observer(key)
	}
}

// newlyCompletedKeys returns the release keys present in current but absent
// from previous, the uncached -> cached transition a refresh reports.
func newlyCompletedKeys(previous, current map[string]altmountReleaseRecord) []string {
	if len(current) == 0 {
		return nil
	}
	fresh := make([]string, 0, len(current))
	for key := range current {
		if _, ok := previous[key]; ok {
			continue
		}
		fresh = append(fresh, key)
	}
	return fresh
}

// altmountGUIDPrefix namespaces an AltMount release key used as a candidate's
// source GUID, so it can never collide with a Prowlarr GUID (which is an
// indexer-assigned release id, not a normalized name).
const altmountGUIDPrefix = "altmount:"

// adoptAltmountIdentity fills a candidate's durable identity from AltMount's
// completed record. A candidate that already carries a video hash, a source
// GUID or a size keeps it: AltMount is corroboration, not an override, and a
// re-list must not lose the stronger tier the provider already declared.
func adoptAltmountIdentity(candidate *stream.StreamCandidate, record altmountReleaseRecord) {
	if candidate == nil {
		return
	}
	if strings.TrimSpace(candidate.SourceGUID) == "" && strings.TrimSpace(record.Identity) != "" {
		candidate.SourceGUID = altmountGUIDPrefix + record.Identity
	}
	if candidate.FileSize <= 0 && record.Size > 0 {
		candidate.FileSize = record.Size
	}
}

// markAltmountBadgeCandidates honors the completion badge AltMount's Stremio
// addon already puts on imported, fresh releases. This needs no API
// configuration and lets the plugin respect AltMount's cached-first ordering
// instead of re-sorting it away.
func markAltmountBadgeCandidates(candidates []stream.StreamCandidate) {
	for i := range candidates {
		name := strings.ToLower(candidates[i].Name)
		if strings.Contains(name, altmountCachedBadge) {
			candidates[i].SourceConfirmed = true
		}
	}
}

// Validate performs a one-shot history fetch and returns a human-readable
// status for TestConnection.
func (c *altmountStateClient) Validate(ctx context.Context) (string, error) {
	historyURL, err := c.historyURL()
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, historyURL, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.mu.Lock()
	key := c.apiKey
	c.mu.Unlock()
	if key != "" {
		q := req.URL.Query()
		q.Set("apikey", key)
		req.URL.RawQuery = q.Encode()
	}
	validateClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := validateClient.Do(req)
	if err != nil {
		return "", errors.New("connect to AltMount failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("AltMount returned HTTP %d", resp.StatusCode)
	}
	snapshot, err := parseAltmountHistory(io.LimitReader(resp.Body, maxAltmountBodyBytes+1), time.Now())
	if err != nil {
		return "", fmt.Errorf("parse AltMount history: %w", err)
	}
	status := fmt.Sprintf("AltMount history OK: %d completed, %d failed releases", len(snapshot.Completed), len(snapshot.Failed))
	// Probe queue reachability best-effort: the pending verdict needs it, but
	// an AltMount without queue support (or a transient failure) must not
	// fail validation that history already passed.
	if queueStatus, queueErr := c.validateQueue(ctx, key); queueErr != nil {
		status += "; queue unavailable"
	} else {
		status += "; " + queueStatus
	}
	return status, nil
}

// validateQueue probes the SABnzbd queue endpoint for TestConnection. It
// reports reachability only and never fails validation: history already
// passed, and older AltMount builds may not serve the queue mode.
func (c *altmountStateClient) validateQueue(ctx context.Context, apiKey string) (string, error) {
	queueURL, err := c.queueURL()
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, queueURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		q := req.URL.Query()
		q.Set("apikey", apiKey)
		req.URL.RawQuery = q.Encode()
	}
	validateClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := validateClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("AltMount queue returned HTTP %d", resp.StatusCode)
	}
	snapshot, err := parseAltmountQueue(io.LimitReader(resp.Body, maxAltmountBodyBytes+1), time.Now())
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("queue OK: %d downloading", len(snapshot.Downloading)), nil
}

// --- Release-identity and HTTP helpers ported verbatim with the AltMount
// state client. They mirror the plugin's prowlarr_search.go and main.go so
// the package compiles standalone without importing unexported resolver
// helpers. HTTP logic is identical. ---

// prowlarrCleanPattern reduces a release name to a comparable identity:
// lowercase, extension stripped, all non-alphanumerics removed.
var prowlarrCleanPattern = regexp.MustCompile(`[^a-z0-9]+`)

// releaseNameKey reduces a release or provider filename to a comparable
// identity: lowercase, extension stripped, all non-alphanumerics removed. Two
// postings of the same scene release normalize to the same key even when one
// uses dots and the other spaces.
func releaseNameKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	if idx := strings.IndexAny(value, "?#"); idx != -1 {
		value = value[:idx]
	}
	for _, ext := range []string{".mkv", ".mp4", ".avi", ".ts", ".m2ts", ".mov", ".webm", ".nzb"} {
		value = strings.TrimSuffix(value, ext)
	}
	return prowlarrCleanPattern.ReplaceAllString(value, "")
}

func firstReleaseLine(value string) string {
	if idx := strings.IndexAny(value, "\r\n"); idx >= 0 {
		return value[:idx]
	}
	return value
}

// candidateReleaseName returns the most release-like identity carried by a
// provider candidate. AltMount-style providers put the release name on the
// first line of the stream title (the remaining lines carry size and indexer
// badges), so only the first line is considered.
func candidateReleaseName(candidate stream.StreamCandidate) string {
	for _, value := range []string{
		candidate.BehaviorHints.Filename,
		firstReleaseLine(candidate.Title),
		firstReleaseLine(candidate.Name),
	} {
		if key := releaseNameKey(stripAltmountCachedBadge(value)); key != "" {
			return key
		}
	}
	if parsed, err := url.Parse(strings.TrimSpace(candidate.URL)); err == nil {
		if key := releaseNameKey(stripAltmountCachedBadge(path.Base(parsed.Path))); key != "" {
			return key
		}
	}
	return ""
}

// altmountBadgeStripPattern matches the AltMount completion badge in any case,
// with its surrounding whitespace, so it can be removed before release-key
// derivation.
var altmountBadgeStripPattern = regexp.MustCompile(`(?i)\s*⚡\s*cached\s*`)

// stripAltmountCachedBadge removes AltMount's completion badge from a display
// value. The badge decorates the stream name with cache state; it is not part of
// the release identity. Leaving it in the derived key makes a badge-only
// confirmation miss the unbadged identity the row persisted, so the waiter is
// never reached.
func stripAltmountCachedBadge(value string) string {
	if !strings.Contains(strings.ToLower(value), "cached") {
		return value
	}
	return strings.TrimSpace(altmountBadgeStripPattern.ReplaceAllString(value, " "))
}

// releaseSizesMatch reports whether two byte sizes plausibly describe the same
// file. Provider display sizes are rounded (and parsed as decimal GB), so the
// tolerance is wider than a byte-exact comparison. Unknown sizes never reject a
// match.
func releaseSizesMatch(a, b int64) bool {
	if a <= 0 || b <= 0 {
		return true
	}
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	largest := a
	if b > largest {
		largest = b
	}
	return float64(diff)/float64(largest) <= 0.10
}

// sameParentDomain reports whether host a and host b share at least two
// rightmost domain labels (e.g. "v3-cinemeta.strem.io" and
// "cinemeta-live.strem.io" both end with ".strem.io").
func sameParentDomain(a, b string) bool {
	aParts := strings.Split(strings.TrimSuffix(a, "."), ".")
	bParts := strings.Split(strings.TrimSuffix(b, "."), ".")
	if len(aParts) < 3 || len(bParts) < 3 {
		return false
	}
	aLast := strings.ToLower(strings.Join(aParts[len(aParts)-2:], "."))
	bLast := strings.ToLower(strings.Join(bParts[len(bParts)-2:], "."))
	return aLast == bLast
}

func newRestrictedRedirectHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if len(via) == 0 {
				return nil
			}
			origin := via[0].URL
			target := request.URL
			if target.User != nil {
				return errors.New("redirect with userinfo is not allowed")
			}
			if !strings.EqualFold(origin.Scheme, target.Scheme) {
				return errors.New("cross-scheme redirects are not allowed")
			}
			if strings.EqualFold(origin.Host, target.Host) {
				return nil
			}
			// Allow same-registered-domain redirects so well-known
			// metadata providers redirect within their own domain.
			if sameParentDomain(origin.Host, target.Host) {
				return nil
			}
			return errors.New("cross-origin redirects are not allowed")
		},
	}
}

// RefreshIfStale refreshes the completed/failed state when the configured
// interval has elapsed (exported wrapper for the monitor package; logic
// unchanged).
func (c *altmountStateClient) RefreshIfStale(ctx context.Context) error {
	return c.refreshIfStale(ctx)
}
