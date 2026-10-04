package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/database/pglock"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/altmount"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/prowlarr"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/quality"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/release"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

// --- Core port compatibility shims ---
// routing.go lived in package main alongside its collaborators; in core the
// prowlarr/altmount/quality/release collaborators live in their own packages.
// The aliases below reunite the identities so the ported logic stays verbatim.

const maxResponseBytes = 4 << 20

type prowlarrSearchClient = prowlarr.SearchClient
type altmountStateClient = altmount.Client

// VirtualMediaVariant mirrors the SDK runtimehost variant shape without
// importing the plugin SDK runtime. The ported monitor never invokes the
// resolver (resolution lives in the resolver package); it is kept so the
// construction signature stays verbatim.
type VirtualMediaVariant struct {
	VirtualURI     string
	Label          string
	Resolution     string
	CodecVideo     string
	CodecAudio     string
	HDR            string
	Bitrate        int
	RuntimeMinutes int
}

type streamResolver interface {
	Resolve(context.Context, string) (string, error)
}

// MediaRegistrar registers fulfilled virtual media with the host catalog.
// It replaces the plugin's virtualMediaRegistrar (library.go) binding.
type MediaRegistrar interface {
	Register(ctx context.Context, item MonitoredMedia) error
}

// ReconcileEvidence attests that a source's keep set came from a pass that
// enumerated the entire queue from the front. FullCycle is false for resumed
// or deadline-cut passes. The monitor reconciles only after a full cycle, and
// the catalog refuses destructive reconciliation without complete evidence; a
// keep set that shrinks implausibly is refused there as well.
type ReconcileEvidence struct {
	FullCycle   bool
	SourceCount int
	QueueCount  int
}

// ErrReconcileRefused marks a reconciliation the catalog rejected because the
// completeness evidence was missing or the keep set shrank implausibly. It is
// reported at Error rather than Debug because live media was about to be
// deleted.
var ErrReconcileRefused = errors.New("virtual reconciliation refused")

type mediaReconciler interface {
	Reconcile(ctx context.Context, source string, keepIDs []string, libraryIDs []int, evidence ReconcileEvidence) error
}

// mediaPresence is the optional capability a registrar may expose so the
// monitor can evict queue entries whose catalog content was genuinely removed.
// A registrar without it simply never prunes on absence.
type mediaPresence interface {
	MissingVirtualMedia(ctx context.Context, contentIDs []string) (map[string]struct{}, error)
}

type virtualMediaRegistrar = MediaRegistrar

// ProviderValidator is the provider health surface used by TestConnection
// (satisfied by *resolver.Resolver).
type ProviderValidator interface {
	ValidateConnection(context.Context) error
}

type connectionValidator = ProviderValidator

// HostRegistrar is the host callback used by the monitor. routing.go only
// ever called ListLibraries on the host (via sdkruntime.Host());
// UpsertVirtualMedia-style registration flows through MediaRegistrar and
// reconciliation through virtualMediaLister instead.
type HostRegistrar interface {
	ListLibraries(ctx context.Context, userID string) ([]*Library, error)
}

// Monitor is the ported request-router / fulfillment / monitoring-queue
// service. It wraps the verbatim mediaMonitor queue with an injectable
// provider validator and host registrar (replacing sdkruntime.Host()).
const monitorAdvisoryLock int64 = 0x76696f5f766c6d6e

type Monitor struct {
	resolver connectionValidator
	monitor  *mediaMonitor
	host     HostRegistrar
	pool     *pgxpool.Pool
}

// New builds a Monitor. A nil logger selects slog.Default().
func New(resolver connectionValidator, logger *slog.Logger, host HostRegistrar, pools ...*pgxpool.Pool) *Monitor {
	var pool *pgxpool.Pool
	if len(pools) > 0 {
		pool = pools[0]
	}
	return &Monitor{resolver: resolver, monitor: newMediaMonitor(nil, logger), host: host, pool: pool}
}

// SetHost installs the host registrar used by ListConfigOptions.
func (s *Monitor) SetHost(host HostRegistrar) { s.host = host }

// SetPool installs the pool used for the cross-replica monitor lease.
func (s *Monitor) SetPool(pool *pgxpool.Pool) { s.pool = pool }

// SetResolver installs the provider validator used by TestConnection.
func (s *Monitor) SetResolver(resolver connectionValidator) { s.resolver = resolver }

// SetRegistrar installs the catalog registrar used by Fulfill/CheckStatus/Run.
func (s *Monitor) SetRegistrar(registrar MediaRegistrar) { s.monitor.setRegistrar(registrar) }

// ReleaseStore returns the in-memory release schedule cache the monitor
// populates while evaluating items. A nil result means the monitor is
// unconfigured. The store implements the resolver's ReleaseGate: it fails open
// on unknown or partial schedule data and blocks only a concrete future air
// date.
func (s *Monitor) ReleaseStore() *release.ReleaseStore {
	if s == nil || s.monitor == nil {
		return nil
	}
	return s.monitor.releaseStore
}

// ClassifyCandidates applies the configured completion state to a candidate
// list for the resolver. AltMount's authoritative completed/failed state runs
// first; Prowlarr's cached confirmation runs second and only adds
// SourceConfirmed/SourceGUID, so it can never clear an AltMount SourceFailed
// verdict.
func (s *Monitor) ClassifyCandidates(candidates []stream.StreamCandidate) {
	if s == nil || s.monitor == nil {
		return
	}
	s.monitor.classifyCandidates(candidates)
}

func (m *mediaMonitor) classifyCandidates(candidates []stream.StreamCandidate) {
	if m == nil || len(candidates) == 0 {
		return
	}
	// Read the configured clients directly: the altmountClient/prowlarrClient
	// accessors allocate an empty client on a miss, and this runs on every
	// serve. An unconfigured source is simply skipped.
	m.mu.Lock()
	altmountClient := m.altmount
	prowlarrClient := m.prowlarr
	m.mu.Unlock()
	if altmountClient != nil {
		altmountClient.ClassifyCandidates(candidates)
	}
	if prowlarrClient != nil {
		prowlarrClient.ClassifyCandidates(candidates)
	}
}

// Configure loads/persists the monitored queue (delegates to mediaMonitor).
func (s *Monitor) Configure(c Config) error { return s.monitor.Configure(c) }

// ConfigureProwlarr sets up the Prowlarr search client (delegates).
func (s *Monitor) ConfigureProwlarr(urls, apiKey string, intervalMinutes, timeoutSeconds int, indexFile string) error {
	return s.monitor.configureProwlarr(urls, apiKey, intervalMinutes, timeoutSeconds, indexFile)
}

// ConfigureAltmount sets up the AltMount state client (delegates).
func (s *Monitor) ConfigureAltmount(baseURL, apiKey string, intervalMinutes int, indexFile string) error {
	return s.monitor.configureAltmount(baseURL, apiKey, intervalMinutes, indexFile)
}

// SetReleaseConfirmedObserver installs a callback invoked once per AltMount
// release key when AltMount first reports that release completed. It is the
// push signal a cache handoff listens for so an in-session handoff can react to
// a fill that finished during playback without polling the provider. A nil
// callback clears it.
//
// The observer is also recorded internally so ReleaseCached can answer a
// synchronous "is this pinned release already cached?" probe (the mid-play heal
// path) from the same completion signal, without a provider round-trip.
func (s *Monitor) SetReleaseConfirmedObserver(fn altmount.ReleaseConfirmationObserver) {
	if s == nil || s.monitor == nil {
		return
	}
	m := s.monitor
	var installed altmount.ReleaseConfirmationObserver
	if fn != nil {
		installed = func(releaseKey string) {
			if key := altmount.ReleaseKey(releaseKey); key != "" {
				m.mu.Lock()
				if m.confirmedReleases == nil {
					m.confirmedReleases = make(map[string]struct{})
				}
				m.confirmedReleases[key] = struct{}{}
				m.mu.Unlock()
			}
			// Fire the caller's listener outside the monitor lock: a handoff it
			// triggers resolves and rebinds, and must never block completion
			// bookkeeping or risk re-entering the observer under the lock.
			fn(releaseKey)
		}
	}
	m.mu.Lock()
	m.confirmObserver = installed
	client := m.altmount
	m.mu.Unlock()
	if client != nil {
		client.SetConfirmObserver(installed)
	}
}

// ReleaseCached reports whether AltMount has reported the named release
// completed. known is false when no AltMount client is configured, so an
// unconfigured provider is never mistaken for "not cached". It answers from the
// recorded completion signal, so it is cheap and side-effect free.
func (s *Monitor) ReleaseCached(releaseName string) (cached bool, known bool) {
	if s == nil || s.monitor == nil {
		return false, false
	}
	m := s.monitor
	client := m.configuredAltmount()
	if client == nil || client.URL() == "" {
		return false, false
	}
	key := altmount.ReleaseKey(releaseName)
	if key == "" {
		return false, false
	}
	// Check the authoritative configured AltMount snapshot first (e.g. loaded
	// from disk on restart or refreshed from AltMount).
	if completed, known := client.ReleaseCompleted(key); known && completed {
		return true, true
	}
	m.mu.Lock()
	_, cached = m.confirmedReleases[key]
	m.mu.Unlock()
	return cached, true
}

// ReleaseFailed reports whether AltMount has reported the named release failed.
// known is false when no AltMount client is configured, so an unconfigured
// provider is never mistaken for "not failed". It is the symmetric read to
// ReleaseCached: a caller holding a persisted candidate row, rather than a
// freshly listed candidate, uses it to ask whether the source of truth has
// branded that row's release dead without re-listing the provider. It answers
// from the cached snapshot, so it is cheap and side-effect free.
func (s *Monitor) ReleaseFailed(releaseName string) (failed bool, known bool) {
	if s == nil || s.monitor == nil {
		return false, false
	}
	client := s.monitor.configuredAltmount()
	if client == nil || client.URL() == "" {
		return false, false
	}
	key := altmount.ReleaseKey(releaseName)
	if key == "" {
		return false, false
	}
	return client.ReleaseFailed(key)
}

// ReleaseDownloading reports whether AltMount has reported the named release
// as actively fetching (SABnzbd queue, not history). known is false when no
// AltMount client is configured. A downloading release is pending: neither
// dead (so failed-drops and the pruner must ignore it) nor ready (so the
// resolver may hold for it). It answers from the cached snapshot, so it is
// cheap and side-effect free.
func (s *Monitor) ReleaseDownloading(releaseName string) (downloading bool, known bool) {
	if s == nil || s.monitor == nil {
		return false, false
	}
	client := s.monitor.configuredAltmount()
	if client == nil || client.URL() == "" {
		return false, false
	}
	key := altmount.ReleaseKey(releaseName)
	if key == "" {
		return false, false
	}
	return client.ReleaseDownloading(key)
}

// ProviderStale reports whether any configured virtual provider's cached state
// has aged past its refresh interval. It is the read half of the stale-provider
// seam the playback layer probes before a resolve, so a long-lived process does
// not resolve against a stale AltMount completed/failed snapshot or Prowlarr
// index.
func (s *Monitor) ProviderStale() bool {
	if s == nil || s.monitor == nil {
		return false
	}
	m := s.monitor
	m.mu.Lock()
	altmountClient := m.altmount
	prowlarrClient := m.prowlarr
	m.mu.Unlock()
	if altmountClient != nil && altmountClient.URL() != "" && altmountClient.Stale() {
		return true
	}
	if prowlarrClient != nil && prowlarrClient.URL() != "" && prowlarrClient.Stale() {
		return true
	}
	return false
}

// RefreshStaleProvider refreshes every configured virtual provider whose cached
// state is stale, under the caller's budget. One unreachable provider never
// blocks the other: both are attempted and their errors are joined.
func (s *Monitor) RefreshStaleProvider(ctx context.Context) error {
	if s == nil || s.monitor == nil {
		return nil
	}
	m := s.monitor
	m.mu.Lock()
	altmountClient := m.altmount
	prowlarrClient := m.prowlarr
	m.mu.Unlock()
	var errs []error
	if altmountClient != nil && altmountClient.URL() != "" && altmountClient.Stale() {
		if err := altmountClient.RefreshIfStale(ctx); err != nil {
			errs = append(errs, fmt.Errorf("refresh AltMount state: %w", err))
		}
	}
	if prowlarrClient != nil && prowlarrClient.URL() != "" && prowlarrClient.Stale() {
		if err := prowlarrClient.RefreshIfStale(ctx); err != nil {
			errs = append(errs, fmt.Errorf("refresh Prowlarr search: %w", err))
		}
	}
	return errors.Join(errs...)
}

// RefreshProviderState forces a fresh classification snapshot from every
// configured virtual provider before a caller classifies candidates. Unlike
// RefreshStaleProvider it does not honor the refresh interval: an explicit
// "Refresh List" must classify against the provider's current completed/failed
// state, not a snapshot up to one interval old. It reuses the existing AltMount
// history fetch and Prowlarr RSS refresh and adds no new snapshot machinery. One
// unreachable provider never blocks the other; both are attempted and their
// errors are joined.
func (s *Monitor) RefreshProviderState(ctx context.Context) error {
	if s == nil || s.monitor == nil {
		return nil
	}
	m := s.monitor
	m.mu.Lock()
	altmountClient := m.altmount
	prowlarrClient := m.prowlarr
	m.mu.Unlock()
	var errs []error
	if altmountClient != nil && altmountClient.URL() != "" {
		if err := altmountClient.Refresh(ctx); err != nil {
			errs = append(errs, fmt.Errorf("refresh AltMount state: %w", err))
		}
	}
	if prowlarrClient != nil && prowlarrClient.URL() != "" {
		if err := prowlarrClient.RefreshIfStale(ctx); err != nil {
			errs = append(errs, fmt.Errorf("refresh Prowlarr search: %w", err))
		}
	}
	return errors.Join(errs...)
}

// SearchMonitoredReleases performs an on-demand Prowlarr search for one
// monitored title. It returns an error when Prowlarr is unwired so the caller
// can degrade to an altmount-only result instead of treating it as no match.
func (s *Monitor) SearchMonitoredReleases(ctx context.Context, item MonitoredMedia, episode *VirtualEpisode, qc quality.QualityConfig) ([]prowlarr.SearchItem, error) {
	if s == nil || s.monitor == nil {
		return nil, errors.New("virtual library monitor is not configured")
	}
	client := s.monitor.configuredProwlarr()
	if client == nil || client.URL() == "" {
		return nil, errors.New("prowlarr is not configured")
	}
	return client.SearchMonitoredReleases(ctx, item, episode, qc)
}

// EnqueueRelease hands one release's download URL to the configured AltMount
// client. A nil or unconfigured client fails closed.
func (s *Monitor) EnqueueRelease(ctx context.Context, downloadURL, name string) (string, error) {
	if s == nil || s.monitor == nil {
		return "", errors.New("virtual library monitor is not configured")
	}
	return s.monitor.configuredAltmount().Enqueue(ctx, downloadURL, name)
}

// ProwlarrConfigured reports whether a Prowlarr search client is wired with a
// base URL.
func (s *Monitor) ProwlarrConfigured() bool {
	if s == nil || s.monitor == nil {
		return false
	}
	client := s.monitor.configuredProwlarr()
	return client != nil && client.URL() != ""
}

// AltmountConfigured reports whether an AltMount client is wired with a base
// URL.
func (s *Monitor) AltmountConfigured() bool {
	if s == nil || s.monitor == nil {
		return false
	}
	client := s.monitor.configuredAltmount()
	return client != nil && client.URL() != ""
}

var (
	tmdbBaseURL      = "https://api.themoviedb.org/3"
	cinemetaBaseURL  = "https://v3-cinemeta.strem.io"
	tvmazeBaseURL    = "https://api.tvmaze.com"
	metadataClient   = newRestrictedRedirectHTTPClient(20 * time.Second)
	errNoHomeRelease = errors.New("TMDB metadata has no digital or physical release date")
)

const (
	maxMonitoredItems    = 10000
	maxMonitorStateBytes = 32 << 20
	maxMonitoredEpisodes = 10000
	maxMonitorKeyBytes   = 512
	// monitorPerItemTimeout bounds a single item's evaluate/register work (and
	// a single source reconciliation) so one hung provider or poison item
	// cannot consume the whole pass budget. It is set above the 20s metadata
	// client timeout so an ordinary multi-call series evaluation still fits,
	// while remaining below the ~2 minute pass deadline so a hung provider
	// always leaves budget for later items. A per-item timeout is a deferral,
	// not a pass failure: the item is retried on a later pass.
	monitorPerItemTimeout = 45 * time.Second

	// mediaTypeMovie is the movie media-type value shared by the queue's
	// movie/series branches.
	mediaTypeMovie = "movie"
)

// monitorState is the on-disk monitor queue. Older releases wrote a bare JSON
// array of items; loadMonitorConfig still accepts that legacy shape (with an
// empty cursor), while saves always write the object below. cursor is the key
// of the last item whose evaluation completed in the previous partial pass;
// it is cleared after a pass that reaches the end of the sorted queue.
type monitorState struct {
	Cursor string           `json:"cursor,omitempty"`
	Items  []monitoredMedia `json:"items"`
}

type Config struct {
	TMDBAPIKey        string
	File              string
	ProwlarrIndexFile string
	FilterProwlarr    bool
	Quality           quality.QualityConfig
	LibraryIDs        []int
}

// MonitoredMedia aliases the prowlarr package monitored shape so the monitor
// queue and the indexer matcher share one identity (see prowlarr package).
type MonitoredMedia = prowlarr.MonitoredMedia

// VirtualEpisode aliases the prowlarr package episode shape.
type VirtualEpisode = prowlarr.VirtualEpisode

type monitoredMedia = prowlarr.MonitoredMedia
type virtualEpisode = prowlarr.VirtualEpisode
type mediaMonitor struct {
	mu           sync.Mutex
	runMu        sync.Mutex
	resolver     streamResolver
	logger       *slog.Logger
	config       Config
	items        map[string]monitoredMedia
	registrar    virtualMediaRegistrar
	prowlarr     *prowlarrSearchClient
	altmount     *altmountStateClient
	registered   map[string]struct{}
	releaseStore *release.ReleaseStore
	// confirmObserver forwards AltMount's once-per-release completion signal to
	// a cache-handoff listener. It is kept here so a later reconfigure re-applies
	// it to the (re)created client.
	confirmObserver altmount.ReleaseConfirmationObserver
	// confirmedReleases records the release keys AltMount has reported completed
	// (the same normalized identity altmount.ReleaseKey produces). It is
	// populated from the confirm observer the monitor installs, so a cache
	// handoff probe can answer synchronously without a provider round-trip or a
	// classifier side effect.
	confirmedReleases map[string]struct{}
	// cursor is the key of the last item whose evaluation completed in a
	// partial pass. It is persisted with the queue so the next pass resumes
	// instead of restarting from the front.
	cursor string
	// itemTimeout bounds evaluate/register and per-source reconciliation.
	// Zero disables the extra bound (caller deadline only).
	itemTimeout time.Duration
	// evaluateFn is the per-item evaluation step; tests replace it to make
	// pass budgeting deterministic without provider network calls.
	evaluateFn func(context.Context, monitoredMedia) (monitoredMedia, string, error)
}

type virtualMediaLister interface {
	ListVirtual(context.Context) ([]monitoredMedia, error)
}

func (m *mediaMonitor) setRegistrar(registrar virtualMediaRegistrar) {
	m.mu.Lock()
	m.registrar = registrar
	m.mu.Unlock()
}

// prowlarrMatch returns true when any configured RSS feed contains a
// release matching the monitored item.
func (m *mediaMonitor) prowlarrMatch(item monitoredMedia) bool {
	m.mu.Lock()
	c := m.prowlarr
	m.mu.Unlock()
	if c == nil {
		return false
	}
	if c.URL() == "" {
		return false
	}
	m.mu.Lock()
	quality := m.config.Quality
	m.mu.Unlock()
	return c.MatchWithQuality(item, quality)
}

// configureProwlarr sets up the Prowlarr search client with the first
// non-empty URL from the list. Multiple URLs / per-indexer discovery are
// no longer needed — /api/v1/search covers all indexers in one request.
//
// The first URL is normalized before it is applied: a scheme-less value such as
// "one.vio" becomes http://one.vio. An invalid URL (indexer path, query string,
// malformed host) leaves Prowlarr unconfigured instead of storing a value that
// would fail every staleness refresh, and the error is returned so the caller
// logs it once at configuration time.
func (m *mediaMonitor) configureProwlarr(urls, apiKey string, intervalMinutes, timeoutSeconds int, indexFile string) error {
	firstURL := ""
	for _, u := range strings.FieldsFunc(urls, func(r rune) bool { return r == '\n' || r == ',' }) {
		u = strings.TrimSpace(u)
		if u != "" {
			firstURL = u
			break
		}
	}
	normalizedURL, normalizeErr := prowlarr.NormalizeBaseURL(firstURL)
	if normalizeErr != nil {
		normalizedURL = ""
		m.logger.Warn("virtual library Prowlarr URL is invalid; Prowlarr stays unconfigured", "error", normalizeErr)
	}
	m.mu.Lock()
	if m.prowlarr == nil {
		m.prowlarr = prowlarr.NewSearchClient(nil)
	}
	m.mu.Unlock()
	m.prowlarr.Configure(normalizedURL, apiKey, intervalMinutes, timeoutSeconds)
	if indexErr := m.prowlarr.ConfigureIndexFile(indexFile); indexErr != nil {
		return indexErr
	}
	return normalizeErr
}

// configureAltmount sets up the AltMount completed/failed state client. The
// URL may be blank, in which case the client stays inert and Prowlarr remains
// the known-good fallback.
func (m *mediaMonitor) configureAltmount(baseURL, apiKey string, intervalMinutes int, indexFile string) error {
	m.mu.Lock()
	if m.altmount == nil {
		m.altmount = altmount.New(nil)
	}
	// Invalidate the monitor's completion set on configuration change so
	// state from another provider instance cannot be served as current.
	m.confirmedReleases = make(map[string]struct{})
	observer := m.confirmObserver
	client := m.altmount
	m.mu.Unlock()
	client.Configure(baseURL, apiKey, intervalMinutes)
	// Re-apply a listener installed before the client existed; loading the
	// persisted state must not drop it.
	client.SetConfirmObserver(observer)
	return client.ConfigureIndexFile(indexFile)
}

// altmountClient returns the AltMount state client, or a new empty one.
func (m *mediaMonitor) altmountClient() *altmountStateClient {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.altmount != nil {
		return m.altmount
	}
	return altmount.New(nil)
}

// prowlarrClient returns the Prowlarr search client, or a new empty one for Validate.
func (m *mediaMonitor) prowlarrClient() *prowlarrSearchClient {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.prowlarr != nil {
		return m.prowlarr
	}
	return prowlarr.NewSearchClient(nil)
}

// configuredProwlarr returns the configured client, or nil when none is wired.
// Unlike prowlarrClient it does not allocate an empty placeholder, so a caller
// that must degrade on "not configured" can tell the difference.
func (m *mediaMonitor) configuredProwlarr() *prowlarrSearchClient {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.prowlarr
}

// configuredAltmount returns the configured client, or nil when none is wired.
func (m *mediaMonitor) configuredAltmount() *altmountStateClient {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.altmount
}

func (m *mediaMonitor) markRegistered(key string) {
	m.mu.Lock()
	m.registered[key] = struct{}{}
	m.mu.Unlock()
}

func (m *mediaMonitor) isRegistered(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.registered[key]
	return ok
}

func (m *mediaMonitor) register(ctx context.Context, item monitoredMedia) error {
	m.mu.Lock()
	registrar := m.registrar
	m.mu.Unlock()
	if registrar == nil {
		return errors.New("Vio virtual catalog service is not configured")
	}
	return registrar.Register(ctx, item)
}

func newMediaMonitor(resolver streamResolver, logger *slog.Logger) *mediaMonitor {
	if logger == nil {
		logger = slog.Default()
	}
	m := &mediaMonitor{
		resolver:     resolver,
		logger:       logger,
		config:       Config{File: ".vio-virtual-library-monitored.json", ProwlarrIndexFile: ".vio-virtual-library-prowlarr-index.json"},
		items:        map[string]monitoredMedia{},
		prowlarr:     nil,
		registered:   map[string]struct{}{},
		releaseStore: release.NewReleaseStore(),
		itemTimeout:  monitorPerItemTimeout,
	}
	m.evaluateFn = m.evaluate
	return m
}

func (m *mediaMonitor) Configure(c Config) error {
	configured, loaded, cursor, err := loadMonitorConfig(c)
	if err != nil {
		return err
	}
	m.applyConfiguration(configured, loaded, nil, false)
	m.mu.Lock()
	m.cursor = cursor
	m.mu.Unlock()
	return nil
}

// currentCursor returns the persisted resume position, empty when the next
// pass should start from the front.
func (m *mediaMonitor) currentCursor() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cursor
}

// setCursor persists the resume position. A failure is best-effort: the queue
// items are unchanged, so the worst case is a restart from the previous
// cursor, never data loss.
func (m *mediaMonitor) setCursor(cursor string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cursor = cursor
	return m.saveLocked()
}

func (m *mediaMonitor) reconcileLibraryIDs() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.config.LibraryIDs...)
}

func loadMonitorConfig(c Config) (Config, map[string]monitoredMedia, string, error) {
	if c.File == "" {
		c.File = ".vio-virtual-library-monitored.json"
	}
	if c.ProwlarrIndexFile == "" {
		c.ProwlarrIndexFile = ".vio-virtual-library-prowlarr-index.json"
	}
	if !filepath.IsAbs(c.File) {
		if _, err := os.Stat(c.File); errors.Is(err, os.ErrNotExist) {
			for _, fb := range []string{
				filepath.Join("/var/lib/silo/plugins/com.drondeseries.vio-virtual-library", c.File),
				filepath.Join("/var/lib/silo/plugins", c.File),
			} {
				if _, sErr := os.Stat(fb); sErr == nil {
					c.File = fb
					break
				}
			}
		}
	}
	if !filepath.IsAbs(c.ProwlarrIndexFile) {
		if _, err := os.Stat(c.ProwlarrIndexFile); errors.Is(err, os.ErrNotExist) {
			for _, fb := range []string{
				filepath.Join("/var/lib/silo/plugins/com.drondeseries.vio-virtual-library", c.ProwlarrIndexFile),
				filepath.Join("/var/lib/silo/plugins", c.ProwlarrIndexFile),
			} {
				if _, sErr := os.Stat(fb); sErr == nil {
					c.ProwlarrIndexFile = fb
					break
				}
			}
		}
	}
	loaded := make(map[string]monitoredMedia)
	cursor := ""
	file, err := os.Open(c.File)
	if err == nil {
		defer file.Close()
		data, readErr := io.ReadAll(io.LimitReader(file, maxMonitorStateBytes+1))
		if readErr != nil {
			return Config{}, nil, "", fmt.Errorf("read monitored queue: %w", readErr)
		}
		if len(data) > maxMonitorStateBytes {
			return Config{}, nil, "", fmt.Errorf("monitored queue exceeds %d bytes", maxMonitorStateBytes)
		}
		var items []monitoredMedia
		if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '[' {
			// Legacy shape: a bare array of items, written before the cursor
			// existed. It simply has no resume position.
			if err := json.Unmarshal(data, &items); err != nil {
				return Config{}, nil, "", fmt.Errorf("decode monitored queue: %w", err)
			}
		} else {
			var state monitorState
			if err := json.Unmarshal(data, &state); err != nil {
				return Config{}, nil, "", fmt.Errorf("decode monitored queue: %w", err)
			}
			items = state.Items
			cursor = state.Cursor
		}
		if len(items) > maxMonitoredItems {
			return Config{}, nil, "", fmt.Errorf("monitored queue exceeds %d items", maxMonitoredItems)
		}
		duplicateKeys := make(map[string]struct{}, len(items))
		for _, item := range items {
			if _, exists := duplicateKeys[item.Key]; exists {
				return Config{}, nil, "", fmt.Errorf("monitored queue contains duplicate key %q", item.Key)
			}
			duplicateKeys[item.Key] = struct{}{}
		}
		for _, item := range items {
			if err := validateMonitoredMedia(item); err != nil {
				return Config{}, nil, "", fmt.Errorf("invalid monitored queue item: %w", err)
			}
			loaded[item.Key] = item
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, nil, "", fmt.Errorf("open monitored queue: %w", err)
	}
	return c, loaded, cursor, nil
}

func (m *mediaMonitor) applyConfiguration(c Config, items map[string]monitoredMedia, registrar virtualMediaRegistrar, replaceRegistrar bool) {
	m.mu.Lock()
	m.config = c
	m.items = items
	if replaceRegistrar {
		m.registrar = registrar
	}
	m.mu.Unlock()
}
func (m *mediaMonitor) remember(item monitoredMedia) error {
	if err := validateMonitoredMedia(item); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	previous, existed := m.items[item.Key]
	if !existed && len(m.items) >= maxMonitoredItems {
		// Reclaim space from finished items before refusing the request so a
		// full queue does not silently drop a new one. A queue full of
		// unfinished work still refuses, preserving the bound.
		if pruned, err := m.pruneCompletedMoviesLocked(); err == nil && pruned > 0 {
			m.logger.Info("pruned completed virtual media to admit a new queue item",
				"pruned", pruned, "bound", maxMonitoredItems, "remaining", len(m.items))
		}
	}
	if !existed && len(m.items) >= maxMonitoredItems {
		return fmt.Errorf("monitored queue is at its %d item limit", maxMonitoredItems)
	}
	m.items[item.Key] = item
	if err := m.saveLocked(); err != nil {
		if existed {
			m.items[item.Key] = previous
		} else {
			delete(m.items, item.Key)
		}
		return err
	}
	return nil
}

// rememberSeriesEpisodes merges freshly evaluated episodes into the persisted
// series item without dropping entries that are already registered, so the
// monitor queue can keep tracking upcoming episodes for ongoing series.
func (m *mediaMonitor) rememberSeriesEpisodes(key string, episodes []virtualEpisode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.items[key]
	if !ok {
		return
	}
	item.Episodes = mergeEpisodes(item.Episodes, episodes)
	m.items[key] = item
}

func mergeEpisodes(existing, fresh []virtualEpisode) []virtualEpisode {
	byKey := make(map[string]int, len(existing))
	for i, episode := range existing {
		byKey[episodeKey(episode)] = i
	}
	for _, episode := range fresh {
		key := episodeKey(episode)
		if idx, ok := byKey[key]; ok {
			if !existing[idx].Available && episode.Available {
				existing[idx].Available = true
			}
			if existing[idx].Title == "" {
				existing[idx].Title = episode.Title
			}
			if existing[idx].Released.IsZero() {
				existing[idx].Released = episode.Released
			}
			continue
		}
		byKey[key] = len(existing)
		existing = append(existing, episode)
	}
	return existing
}

func episodeKey(episode virtualEpisode) string {
	return fmt.Sprintf("%d:%d", episode.Season, episode.Episode)
}

func (m *mediaMonitor) forget(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.forgetLocked(key)
}

// forgetLocked evicts one queue item and its registration marker. Dropping the
// marker means a re-request re-registers through the idempotent upsert instead
// of being silently skipped as already-registered.
func (m *mediaMonitor) forgetLocked(key string) error {
	previous, existed := m.items[key]
	_, wasRegistered := m.registered[key]
	delete(m.items, key)
	delete(m.registered, key)
	if err := m.saveLocked(); err != nil {
		if existed {
			m.items[key] = previous
		}
		if wasRegistered {
			m.registered[key] = struct{}{}
		}
		return err
	}
	return nil
}

// forgetMany evicts several queue items and their registration markers in one
// persisted write, restoring all of them if the save fails.
func (m *mediaMonitor) forgetMany(keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	previous := make(map[string]monitoredMedia, len(keys))
	previousRegistered := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if item, ok := m.items[key]; ok {
			previous[key] = item
		}
		if _, ok := m.registered[key]; ok {
			previousRegistered[key] = struct{}{}
		}
		delete(m.items, key)
		delete(m.registered, key)
	}
	if err := m.saveLocked(); err != nil {
		for key, item := range previous {
			m.items[key] = item
		}
		for key := range previousRegistered {
			m.registered[key] = struct{}{}
		}
		return err
	}
	return nil
}

// evictMissing drops queue entries whose content ID no longer has a catalog
// item. Only previously-registered items are probed, so a request that has not
// been registered yet is never mistaken for removed media. A probe failure
// returns an error and prunes nothing.
func (m *mediaMonitor) evictMissing(ctx context.Context, presence mediaPresence, items []monitoredMedia) (int, error) {
	m.mu.Lock()
	byContent := make(map[string][]string)
	for _, item := range items {
		if _, registered := m.registered[item.Key]; !registered {
			continue
		}
		contentID := virtualContentID(item)
		if contentID == "" {
			continue
		}
		byContent[contentID] = append(byContent[contentID], item.Key)
	}
	m.mu.Unlock()
	if len(byContent) == 0 {
		return 0, nil
	}
	contentIDs := make([]string, 0, len(byContent))
	for contentID := range byContent {
		contentIDs = append(contentIDs, contentID)
	}
	missing, err := presence.MissingVirtualMedia(context.WithoutCancel(ctx), contentIDs)
	if err != nil {
		return 0, err
	}
	if len(missing) == 0 {
		return 0, nil
	}
	keys := make([]string, 0, len(missing))
	for contentID := range missing {
		keys = append(keys, byContent[contentID]...)
	}
	if err := m.forgetMany(keys); err != nil {
		return 0, err
	}
	return len(keys), nil
}

// pruneCompletedMovies bounds the queue by evicting movies whose monitoring
// work is finished: they evaluated ready and were registered in the catalog.
// A completed movie needs no further passes, so keeping it only consumes the
// queue bound. Deferred or failed items are never Ready and are left for retry,
// and a source is evicted only when every one of its items is complete, so a
// source never loses part of its keep set while it remains in reconciliation.
func (m *mediaMonitor) pruneCompletedMovies() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pruneCompletedMoviesLocked()
}

func (m *mediaMonitor) pruneCompletedMoviesLocked() (int, error) {
	bySource := make(map[string][]string)
	complete := make(map[string]bool)
	for key, item := range m.items {
		source := monitorItemSource(item)
		bySource[source] = append(bySource[source], key)
		if _, registered := m.registered[key]; !registered || item.MediaType != mediaTypeMovie || !item.Ready {
			complete[source] = false
			continue
		}
		if _, decided := complete[source]; !decided {
			complete[source] = true
		}
	}
	prune := make(map[string]struct{})
	for source, keys := range bySource {
		if !complete[source] {
			continue
		}
		for _, key := range keys {
			prune[key] = struct{}{}
		}
	}
	if len(prune) == 0 {
		return 0, nil
	}

	previous := make(map[string]monitoredMedia, len(prune))
	previousRegistered := make(map[string]struct{}, len(prune))
	for key := range prune {
		previous[key] = m.items[key]
		if _, ok := m.registered[key]; ok {
			previousRegistered[key] = struct{}{}
		}
		delete(m.items, key)
		delete(m.registered, key)
	}
	if err := m.saveLocked(); err != nil {
		for key, item := range previous {
			m.items[key] = item
		}
		for key := range previousRegistered {
			m.registered[key] = struct{}{}
		}
		return 0, err
	}
	return len(prune), nil
}

func (m *mediaMonitor) itemCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}

func (m *mediaMonitor) item(key string) (monitoredMedia, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[key]
	return v, ok
}
func (m *mediaMonitor) saveLocked() error {
	items := make([]monitoredMedia, 0, len(m.items))
	for _, item := range m.items {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Key < items[j].Key })
	data, err := json.MarshalIndent(monitorState{Cursor: m.cursor, Items: items}, "", "  ")
	if err != nil {
		return err
	}
	if len(data)+1 > maxMonitorStateBytes {
		return fmt.Errorf("monitored queue exceeds %d bytes", maxMonitorStateBytes)
	}
	dir := filepath.Dir(m.config.File)
	tmp, err := os.CreateTemp(dir, ".silo-monitor-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(append(data, '\n')); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, m.config.File)
}

func validateMonitoredMedia(item monitoredMedia) error {
	if item.Key == "" || len(item.Key) > maxMonitorKeyBytes || strings.TrimSpace(item.Key) != item.Key {
		return errors.New("monitored media key is empty, invalid, or too long")
	}
	if len(item.Episodes) > maxMonitoredEpisodes {
		return fmt.Errorf("monitored media exceeds %d episodes", maxMonitoredEpisodes)
	}
	if item.MediaType != "movie" && item.MediaType != "series" {
		return errors.New("monitored media type must be movie or series")
	}
	return nil
}

func mediaFromRequest(r *RequestDescriptor) (monitoredMedia, error) {
	if r == nil {
		return monitoredMedia{}, errors.New("request descriptor is required")
	}
	typ := strings.ToLower(strings.TrimSpace(r.GetMediaType()))
	if typ != "movie" && typ != "series" {
		return monitoredMedia{}, errors.New("media type must be movie or series")
	}
	ids := r.GetExternalIds()
	imdb, tmdb, tvdb := strings.TrimSpace(ids["imdb"]), strings.TrimSpace(ids["tmdb"]), strings.TrimSpace(ids["tvdb"])
	streamID := imdb
	if streamID == "" && typ == "series" && tvdb != "" {
		streamID = "tvdb:" + tvdb
	} else if streamID == "" && tmdb != "" {
		streamID = "tmdb:" + tmdb
	}
	if streamID == "" {
		return monitoredMedia{}, errors.New("IMDb, TVDB, or TMDB ID is required")
	}
	return monitoredMedia{Key: typ + ":" + streamID, MediaType: typ, Title: strings.TrimSpace(r.GetTitle()), Year: r.GetYear(), StreamID: streamID, IMDbID: imdb, TMDBID: tmdb, TVDBID: tvdb, SourceKey: "request:" + typ + ":" + streamID}, nil
}

func virtualContentID(item monitoredMedia) string {
	if item.MediaType == "series" && item.TVDBID != "" {
		return "series-tvdb-" + item.TVDBID
	}
	if item.TMDBID != "" {
		return item.MediaType + "-tmdb-" + item.TMDBID
	}
	if item.IMDbID != "" {
		return item.MediaType + "-imdb-" + item.IMDbID
	}
	return ""
}

func appendUniqueString(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func (m *mediaMonitor) evaluate(ctx context.Context, item monitoredMedia) (monitoredMedia, string, error) {
	now := time.Now()
	var metadataErr error
	if enriched, err := m.fetchCinemeta(ctx, item); err == nil && (item.MediaType != "series" || episodeMetadataComplete(enriched.Episodes)) {
		item = enriched
		if item.MediaType == "series" && episodeRuntimeMissing(item.Episodes) {
			if supplemented, supplementErr := m.fetchTVMaze(ctx, item); supplementErr == nil {
				item = supplemented
			}
		}
	} else {
		if item.MediaType == "series" {
			if fallback, fallbackErr := m.fetchTVMaze(ctx, item); fallbackErr == nil {
				item = fallback
			} else {
				m.logger.Warn("fetch series metadata", "key", item.Key, "cinemeta_error", err, "tvmaze_error", fallbackErr)
				metadataErr = errors.Join(err, fallbackErr)
			}
		} else {
			m.logger.Warn("fetch Cinemeta metadata", "key", item.Key, "error", err)
		}
	}
	if item.MediaType == "movie" {
		if runtime, err := m.fetchTMDBMovieRuntime(ctx, item); err == nil && runtime > 0 {
			item.Runtime = runtime
		}
		release, err := m.movieRelease(ctx, item)
		if err != nil {
			if item.Force && (errors.Is(err, errNoHomeRelease) || item.Title != "") {
				item.Ready = true
				return item, "Movie force-added by administrator", nil
			}
			if errors.Is(err, errNoHomeRelease) && m.prowlarrMatch(item) {
				item.Ready = true
				return item, "Movie release confirmed by indexer RSS feed", nil
			}
			item.Ready = false
			if errors.Is(err, errNoHomeRelease) {
				return item, "Movie is theatrical-only; monitoring indexer RSS feed", nil
			}
			return item, "Release metadata unavailable; monitoring will retry", err
		}
		item.Release = release
		if m.releaseStore != nil && item.IMDbID != "" && !release.IsZero() {
			m.releaseStore.SetMovie(item.IMDbID, release)
		}
		if item.Force {
			item.Ready = true
			return item, "Movie force-added by administrator", nil
		}
		if release.After(now) {
			if m.prowlarrMatch(item) {
				item.Ready = true
				return item, "Movie release confirmed by indexer search", nil
			}
			item.Ready = false
			return item, "Movie is not released for home media yet; monitoring indexer search", nil
		}
	}
	if item.MediaType == "series" {
		episodes := episodeList(item.Episodes)
		if item.Force {
			for i := range episodes {
				if episodes[i].Season > 0 && episodes[i].Episode > 0 {
					episodes[i].Available = true
				}
			}
		} else {
			available := appendUniqueEpisodes(airedEpisodes(episodes, now), m.prowlarrMatchedEpisodes(item, episodes))
			for i := range episodes {
				for _, match := range available {
					if episodes[i].Season == match.Season && episodes[i].Episode == match.Episode {
						episodes[i].Available = true
						break
					}
				}
			}
		}
		item.Episodes = episodes
		if m.releaseStore != nil && item.IMDbID != "" {
			epMap := make(map[string]release.EpisodeInfo, len(episodes))
			var nextAir *time.Time
			for _, ep := range episodes {
				key := fmt.Sprintf("%d:%d", ep.Season, ep.Episode)
				epMap[key] = release.EpisodeInfo{
					Season:  ep.Season,
					Episode: ep.Episode,
					AirDate: ep.Released,
					Title:   ep.Title,
				}
				if !ep.Released.IsZero() && ep.Released.After(now) {
					if nextAir == nil || ep.Released.Before(*nextAir) {
						t := ep.Released
						nextAir = &t
					}
				}
			}
			// Seed the store only when the scheduler has not yet written an
			// authoritative TVmaze schedule for this show; never overwrite a
			// real status (e.g. "Ended") with monitor-local guesses.
			m.releaseStore.SetShowIfAbsent(item.IMDbID, &release.ShowSchedule{
				IMDBID:      item.IMDbID,
				Title:       item.Title,
				Status:      "Monitoring",
				NextAirDate: nextAir,
				Episodes:    epMap,
			})
		}
		available := 0
		for _, episode := range episodes {
			if episode.Available {
				available++
			}
		}
		item.Ready = available > 0
		if !item.Ready {
			return item, "Series registered; monitoring for released episodes", metadataErr
		}
		return item, fmt.Sprintf("%d episodes registered for on-demand playback", available), metadataErr
	}
	item.Ready = true
	return item, "Movie is available for home media", nil
}

func (m *mediaMonitor) prowlarrMatchedEpisodes(item monitoredMedia, episodes []virtualEpisode) []virtualEpisode {
	m.mu.Lock()
	c := m.prowlarr
	quality := m.config.Quality
	m.mu.Unlock()
	if c == nil {
		return nil
	}
	matched := make([]virtualEpisode, 0, len(episodes))
	for _, episode := range episodes {
		if episode.Season <= 0 || episode.Episode <= 0 {
			continue
		}
		if c.MatchEpisodeWithQuality(item, episode, quality) {
			matched = append(matched, episode)
		}
	}
	return matched
}

func appendUniqueEpisodes(existing, additions []virtualEpisode) []virtualEpisode {
	seen := make(map[string]struct{}, len(existing)+len(additions))
	for _, episode := range existing {
		seen[fmt.Sprintf("%d:%d", episode.Season, episode.Episode)] = struct{}{}
	}
	for _, episode := range additions {
		key := fmt.Sprintf("%d:%d", episode.Season, episode.Episode)
		if _, ok := seen[key]; !ok {
			existing = append(existing, episode)
			seen[key] = struct{}{}
		}
	}
	return existing
}

func episodeList(episodes []virtualEpisode) []virtualEpisode {
	if episodes == nil {
		return []virtualEpisode{}
	}
	return episodes
}

func airedEpisodes(episodes []virtualEpisode, now time.Time) []virtualEpisode {
	aired := make([]virtualEpisode, 0, len(episodes))
	for _, episode := range episodeList(episodes) {
		if episode.Season <= 0 || episode.Episode <= 0 || episode.Released.IsZero() || episode.Released.After(now) {
			continue
		}
		aired = append(aired, episode)
	}
	return aired
}

func missingEpisodes(episodes []virtualEpisode, _ time.Time) []virtualEpisode {
	missing := make([]virtualEpisode, 0, len(episodes))
	for _, episode := range episodeList(episodes) {
		if episode.Season > 0 && episode.Episode > 0 && !episode.Released.IsZero() && !episode.Available {
			missing = append(missing, episode)
		}
	}
	return missing
}

func episodeMetadataComplete(episodes []virtualEpisode) bool {
	if len(episodes) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(episodes))
	for _, episode := range episodes {
		if episode.Season <= 0 || episode.Episode <= 0 || strings.TrimSpace(episode.Title) == "" {
			return false
		}
		key := episodeKey(episode)
		if _, exists := seen[key]; exists {
			return false
		}
		seen[key] = struct{}{}
	}
	return true
}

func mergeSeriesEpisodeMetadata(existing, fresh []virtualEpisode) []virtualEpisode {
	byKey := make(map[string]virtualEpisode, len(existing)+len(fresh))
	order := make([]string, 0, len(existing)+len(fresh))
	add := func(episode virtualEpisode) {
		if episode.Season <= 0 || episode.Episode <= 0 {
			return
		}
		key := episodeKey(episode)
		if _, exists := byKey[key]; !exists {
			order = append(order, key)
			byKey[key] = episode
			return
		}
		current := byKey[key]
		if current.Title == "" {
			current.Title = episode.Title
		}
		if current.Overview == "" {
			current.Overview = episode.Overview
		}
		if current.Thumbnail == "" {
			current.Thumbnail = episode.Thumbnail
		}
		if current.Runtime <= 0 {
			current.Runtime = episode.Runtime
		}
		if current.Released.IsZero() {
			current.Released = episode.Released
		}
		byKey[key] = current
	}
	for _, episode := range existing {
		add(episode)
	}
	for _, episode := range fresh {
		add(episode)
	}
	result := make([]virtualEpisode, 0, len(order))
	for _, key := range order {
		result = append(result, byKey[key])
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Season != result[j].Season {
			return result[i].Season < result[j].Season
		}
		return result[i].Episode < result[j].Episode
	})
	return result
}

func episodeRuntimeMissing(episodes []virtualEpisode) bool {
	for _, episode := range episodes {
		if episode.Season > 0 && episode.Episode > 0 && episode.Runtime <= 0 {
			return true
		}
	}
	return false
}

func (s *Monitor) Fulfill(ctx context.Context, req *FulfillRequest) (resp *FulfillResponse, err error) {
	if s == nil || s.monitor == nil {
		return nil, errors.New("virtual library monitor is unavailable")
	}
	// Serialize queue mutation and immediate registration with scheduled Run.
	// Run also acquires the cross-replica advisory lock; Fulfill uses the same
	// process mutex here and catalog transaction locks for cross-replica safety.
	s.monitor.runMu.Lock()
	defer s.monitor.runMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			s.monitor.logger.Error("panic in Fulfill", "error", r)
			err = fmt.Errorf("plugin fulfill panic: %v", r)
		}
	}()
	item, err := mediaFromRequest(req.GetRequest())
	if err != nil {
		return nil, err
	}
	item, message, _ := s.monitor.evaluate(ctx, item)
	if len(req.GetConnections()) > 0 {
		folderID, folderErr := configuredFolderID(req.GetConnections()[0].GetConfig()["media_folder_id"])
		if folderErr != nil {
			return nil, folderErr
		}
		item.MediaFolderID = folderID
	}
	// If media is available but still has no title or is a series with no
	// episodes (Silo did not include episodes in the request descriptor and
	// metadata lookup hasn't resolved them yet), keep it queued so the
	// scheduled monitor can retry once episode metadata arrives.
	if item.Ready && (strings.TrimSpace(item.Title) == "" || (item.MediaType == "series" && len(item.Episodes) == 0)) {
		item.Ready = false
		if item.MediaType == "series" && len(item.Episodes) == 0 {
			message = "Queued; waiting for episode metadata"
		} else {
			message = "Queued; waiting for metadata before registering"
		}
	}
	if item.Ready {
		if err := s.monitor.register(ctx, item); err != nil {
			return nil, fmt.Errorf("register virtual media: %w", err)
		}
		s.monitor.markRegistered(item.Key)
		message = "Virtual media registered in Vio library"
		if item.MediaType == "series" {
			s.monitor.rememberSeriesEpisodes(item.Key, item.Episodes)
		}
	}
	if err := s.monitor.remember(item); err != nil {
		return nil, fmt.Errorf("persist monitored media: %w", err)
	}
	status, external := "queued", "monitored"
	if item.Ready {
		status, external = "completed", "registered"
	}
	targets := make([]*FulfillmentTarget, 0, len(req.GetQualities()))
	conn := ""
	if len(req.GetConnections()) > 0 {
		conn = req.GetConnections()[0].GetId()
	}
	for _, q := range req.GetQualities() {
		targets = append(targets, &FulfillmentTarget{Quality: q.GetId(), ConnectionId: conn, ExternalId: item.Key, Status: status, ExternalStatus: external, Message: message})
	}
	return &FulfillResponse{Targets: targets, Message: message}, nil
}

func (s *Monitor) CheckStatus(ctx context.Context, req *CheckStatusRequest) (*CheckStatusResponse, error) {
	base, err := mediaFromRequest(req.GetRequest())
	if err != nil {
		return nil, err
	}
	statuses := make([]*TargetStatus, 0, len(req.GetTargets()))
	for _, target := range req.GetTargets() {
		item, ok := s.monitor.item(target.GetExternalId())
		if !ok {
			item = base
		}
		item, message, _ := s.monitor.evaluate(ctx, item)
		// If media is available but still has no title or is a series with no
		// episodes, keep it queued rather than failing the RPC with an SDK validation error.
		if item.Ready && (strings.TrimSpace(item.Title) == "" || (item.MediaType == "series" && len(item.Episodes) == 0)) {
			item.Ready = false
			if item.MediaType == "series" && len(item.Episodes) == 0 {
				message = "Queued; waiting for episode metadata"
			} else {
				message = "Queued; waiting for metadata before registering"
			}
		}
		if item.Ready {
			if err := s.monitor.register(ctx, item); err != nil {
				return nil, fmt.Errorf("register virtual media: %w", err)
			}
			s.monitor.markRegistered(item.Key)
			message = "Virtual media registered in Vio library"
			if item.MediaType == "series" {
				s.monitor.rememberSeriesEpisodes(item.Key, item.Episodes)
			}
		}
		if err := s.monitor.remember(item); err != nil {
			return nil, fmt.Errorf("persist monitored media: %w", err)
		}
		status, external := "queued", "monitored"
		if item.Ready {
			status, external = "completed", "registered"
		}
		statuses = append(statuses, &TargetStatus{Quality: target.GetQuality(), ConnectionId: target.GetConnectionId(), Status: status, ExternalStatus: external, Message: message})
	}
	return &CheckStatusResponse{Statuses: statuses}, nil
}
func (s *Monitor) ListConfigOptions(ctx context.Context, _ *ListConfigOptionsRequest) (*ListConfigOptionsResponse, error) {
	if s.host == nil {
		return &ListConfigOptionsResponse{}, nil
	}
	libs, err := s.host.ListLibraries(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list host libraries for config options: %w", err)
	}
	s.monitor.logger.Info("ListConfigOptions: host returned libraries", "count", len(libs))
	movies := make([]*ConfigOption, 0)
	series := make([]*ConfigOption, 0)
	for _, lib := range libs {
		if lib == nil {
			continue
		}
		mediaType := strings.ToLower(strings.TrimSpace(lib.GetMediaType()))
		s.monitor.logger.Info("ListConfigOptions: library", "id", lib.GetId(), "name", lib.GetName(), "media_type", mediaType)
		switch mediaType {
		case "movie", "movies":
			movies = append(movies, &ConfigOption{Value: lib.GetId(), Label: lib.GetName()})
		case "tv", "show", "shows", "series":
			series = append(series, &ConfigOption{Value: lib.GetId(), Label: lib.GetName()})
		case "mixed":
			movies = append(movies, &ConfigOption{Value: lib.GetId(), Label: lib.GetName()})
			series = append(series, &ConfigOption{Value: lib.GetId(), Label: lib.GetName()})
		default:
			s.monitor.logger.Warn("ListConfigOptions: unrecognized library media_type", "id", lib.GetId(), "name", lib.GetName(), "media_type", mediaType)
		}
	}
	optionsByField := map[string]*ConfigOptionList{}
	if len(movies) > 0 {
		optionsByField["movie_library_id"] = &ConfigOptionList{Options: movies}
	}
	if len(series) > 0 {
		optionsByField["series_library_id"] = &ConfigOptionList{Options: series}
	}
	return &ListConfigOptionsResponse{OptionsByField: optionsByField}, nil
}
func (s *Monitor) Validate(context.Context, *ValidateRequest) (*ValidateResponse, error) {
	return &ValidateResponse{FieldErrors: map[string]string{}}, nil
}
func (s *Monitor) TestConnection(ctx context.Context, _ *TestConnectionRequest) (*TestConnectionResponse, error) {
	// Wrap everything in a hard 8s deadline so the SDK gRPC context
	// never races against slow provider or indexer responses.
	testCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	totalStart := time.Now()
	start := totalStart
	err := s.resolver.ValidateConnection(testCtx)
	s.monitor.logger.Info("TestConnection phase", "phase", "provider", "duration_ms", time.Since(start).Milliseconds())
	if err != nil {
		return &TestConnectionResponse{Ok: false, Message: err.Error()}, nil
	}
	msg := "Connected to streaming provider"
	client := s.monitor.prowlarrClient()
	prowlarrURL := client.URL()
	s.monitor.logger.Info("TestConnection phase", "phase", "prowlarr-check", "url_configured", prowlarrURL != "")
	if prowlarrURL != "" {
		start = time.Now()
		searchMsg, searchErr := client.Validate(testCtx)
		s.monitor.logger.Info("TestConnection phase", "phase", "prowlarr", "duration_ms", time.Since(start).Milliseconds())
		if searchErr != nil {
			msg += fmt.Sprintf("\nProwlarr search: %s", searchErr.Error())
		} else {
			msg += fmt.Sprintf("\n%s", searchMsg)
		}
	}
	if altmount := s.monitor.altmountClient(); altmount.URL() != "" {
		start = time.Now()
		stateMsg, stateErr := altmount.Validate(testCtx)
		s.monitor.logger.Info("TestConnection phase", "phase", "altmount", "duration_ms", time.Since(start).Milliseconds())
		if stateErr != nil {
			msg += fmt.Sprintf("\nAltMount state: %s", stateErr.Error())
		} else {
			msg += fmt.Sprintf("\n%s", stateMsg)
		}
	}
	s.monitor.logger.Info("TestConnection phase", "phase", "done", "total_ms", time.Since(totalStart).Milliseconds())
	return &TestConnectionResponse{Ok: true, Message: msg}, nil
}
func (s *Monitor) Run(ctx context.Context, req *RunScheduledTaskRequest) (*RunScheduledTaskResponse, error) {
	if req.GetTaskKey() != "" && req.GetTaskKey() != "monitor-media" {
		return nil, fmt.Errorf("unknown task key %q", req.GetTaskKey())
	}
	if s == nil || s.monitor == nil {
		return nil, errors.New("virtual library monitor is unavailable")
	}
	s.monitor.runMu.Lock()
	defer s.monitor.runMu.Unlock()
	if s.pool != nil {
		lock, acquired, err := pglock.TryAcquire(ctx, s.pool, monitorAdvisoryLock)
		if err != nil {
			return nil, fmt.Errorf("acquire virtual library monitor lock: %w", err)
		}
		if !acquired {
			return &RunScheduledTaskResponse{Output: map[string]any{"skipped": true, "reason": "monitor pass already running on another replica"}}, nil
		}
		defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()
	}

	s.monitor.mu.Lock()
	itemsByKey := make(map[string]monitoredMedia, len(s.monitor.items))
	for _, v := range s.monitor.items {
		itemsByKey[v.Key] = v
	}
	registrar := s.monitor.registrar
	s.monitor.mu.Unlock()
	if lister, ok := registrar.(virtualMediaLister); ok {
		existing, err := lister.ListVirtual(ctx)
		if err != nil {
			return nil, fmt.Errorf("list existing virtual media: %w", err)
		}
		for _, item := range existing {
			itemsByKey[item.Key] = item
			s.monitor.markRegistered(item.Key)
		}
	}
	items := make([]monitoredMedia, 0, len(itemsByKey))
	for _, item := range itemsByKey {
		items = append(items, item)
	}
	// Deterministic order is what makes the persisted cursor meaningful: the
	// same key set always sorts the same way, so a pass resumes exactly where
	// the previous one stopped instead of at a random map position.
	sort.Slice(items, func(i, j int) bool { return items[i].Key < items[j].Key })

	s.monitor.mu.Lock()
	client := s.monitor.prowlarr
	altmount := s.monitor.altmount
	s.monitor.mu.Unlock()
	if client != nil {
		if err := client.RefreshIfStale(ctx); err != nil {
			s.monitor.logger.Warn("refresh Prowlarr search", "error", err)
		}
	}
	if altmount != nil && altmount.URL() != "" {
		if err := altmount.RefreshIfStale(ctx); err != nil {
			s.monitor.logger.Warn("refresh AltMount state", "error", err)
		}
	}
	return s.monitor.runPass(ctx, items, registrar)
}

// itemOutcome classifies the bounded work done for one queue item.
type itemOutcome int

const (
	// itemReady: evaluated and registered (or already registered).
	itemReady itemOutcome = iota
	// itemPending: evaluated successfully but not ready yet, or queued for
	// more metadata; the updated item is persisted.
	itemPending
	// itemDeferred: the per-item bound (or an unattributable context error)
	// fired while the pass still had budget. The item is skipped for this
	// pass and its source is barred from reconciliation.
	itemDeferred
	// itemFailed: a non-context evaluation/registration error. The item is
	// skipped for this pass.
	itemFailed
	// itemBudgetExhausted: the pass deadline fired; stop the whole pass.
	itemBudgetExhausted
)

// runPass performs one bounded monitoring pass over a deterministic snapshot.
//
// Resumability: items are sorted by key and the pass starts at the first key
// after the persisted cursor, wrapping to the front. When the pass stops early
// the cursor is advanced to the last item whose evaluation completed, so the
// next pass continues from there; a complete pass clears the cursor so the
// normal refresh cycle restarts from the front. Every item is attempted at
// most once per pass and the cursor only ever moves forward through the sorted
// keys, so later items are never starved and a poison item cannot trap the pass
// in a loop.
//
// Budget: the item loop honors the caller's deadline. A per-item timeout bounds
// evaluate/register so one hung provider cannot consume the whole budget; a
// per-item timeout is a deferral, never a pass failure. Exhausting the pass
// deadline is normal operation: the pass records progress and returns a
// successful response with the remainder left for the next pass.
func (m *mediaMonitor) runPass(ctx context.Context, items []monitoredMedia, registrar virtualMediaRegistrar) (*RunScheduledTaskResponse, error) {
	sourceTotals := make(map[string]int, len(items))
	for _, item := range items {
		sourceTotals[monitorItemSource(item)]++
	}
	keepBySource := make(map[string][]string)
	reconcileSafeBySource := make(map[string]bool)
	sourceProgress := make(map[string]int)

	start := m.passStartIndex(items)
	advance := ""
	processed, ready, pending, deferred := 0, 0, 0, 0
	budgetExhausted := false
	n := len(items)
	// A pass that starts at the front and is not cut short by the deadline has
	// enumerated every queue item exactly once. Only such a full cycle can
	// attest that a source's keep set is complete; a resumed or deadline-cut
	// pass observed only a suffix and must never authorize deletion.
	fullCycle := start == 0 && n > 0
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			budgetExhausted = true
			break
		}
		item := items[(start+i)%n]
		source := monitorItemSource(item)
		if _, exists := keepBySource[source]; !exists {
			keepBySource[source] = nil
			reconcileSafeBySource[source] = true
		}
		// Keep the previously known content unless an authoritative successful
		// pass proves it should be absent. Metadata and host failures must never
		// turn an empty keep-set into destructive reconciliation.
		if contentID := virtualContentID(item); contentID != "" {
			keepBySource[source] = appendUniqueString(keepBySource[source], contentID)
		}
		sourceProgress[source]++

		updated, _, outcome := m.processItem(ctx, item)
		switch outcome {
		case itemBudgetExhausted:
			// Do not advance the cursor past an item that never finished; the
			// next pass retries it first.
			budgetExhausted = true
		case itemDeferred:
			pending++
			deferred++
			reconcileSafeBySource[source] = false
			advance = item.Key
		case itemFailed:
			pending++
			reconcileSafeBySource[source] = false
			advance = item.Key
		case itemReady:
			advance = item.Key
			if err := m.remember(updated); err != nil {
				return nil, err
			}
			ready++
		case itemPending:
			advance = item.Key
			if err := m.remember(updated); err != nil {
				return nil, err
			}
			pending++
		}
		// Evaluation can enrich the item with a content identity it did not
		// have before (for example resolving an IMDb ID from TMDB); add the
		// post-evaluation identity to the keep set as the original pass did.
		if contentID := virtualContentID(updated); contentID != "" {
			keepBySource[source] = appendUniqueString(keepBySource[source], contentID)
		}
		if budgetExhausted {
			break
		}
		processed++
	}

	// Persist the resume position. A failed save is not fatal: the queue is
	// untouched, so the next pass merely redoes work from the previous cursor.
	if budgetExhausted {
		if advance != "" {
			if err := m.setCursor(advance); err != nil {
				m.logger.WarnContext(ctx, "persist virtual library monitor cursor", "error", err)
			}
		}
	} else if processed > 0 {
		if err := m.setCursor(""); err != nil {
			m.logger.WarnContext(ctx, "persist virtual library monitor cursor", "error", err)
		}
	}
	if budgetExhausted {
		// Deadline exhaustion is normal operation, not a failed pass: the
		// cursor decides where the next pass resumes.
		m.logger.InfoContext(ctx, "virtual library monitor pass reached its deadline",
			"processed", processed, "total", n, "resume_after", advance)
	}

	if fullCycle && !budgetExhausted && ctx.Err() == nil {
		if reconciler, ok := registrar.(mediaReconciler); ok {
			libraryIDs := m.reconcileLibraryIDs()
			for _, source := range sortedSourceKeys(keepBySource) {
				if ctx.Err() != nil {
					break
				}
				if !reconcileSafeBySource[source] {
					continue
				}
				// Reconcile a source only when every item that belongs to it
				// was evaluated this pass. A partial keep set could look like
				// a withdrawal and delete still-live media.
				if sourceProgress[source] != sourceTotals[source] {
					continue
				}
				evidence := ReconcileEvidence{
					FullCycle:   fullCycle,
					SourceCount: sourceTotals[source],
					QueueCount:  n,
				}
				if err := m.reconcileSource(ctx, reconciler, source, keepBySource[source], libraryIDs, evidence); err != nil {
					// One bad source must not fail the pass; it is retried
					// next pass. Context errors are the expected bounded-pass
					// outcome and are logged quietly. A refusal is different:
					// it means live media was about to be deleted, so it is
					// logged loudly with the source and counts.
					switch {
					case errors.Is(err, ErrReconcileRefused):
						m.logger.ErrorContext(ctx, "virtual library reconciliation refused; live media preserved",
							"source", source, "source_items", sourceTotals[source],
							"keep_ids", len(keepBySource[source]), "queue_items", n, "error", err)
					case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
						m.logger.DebugContext(ctx, "reconcile virtual source deferred", "source", source, "error", err)
					default:
						m.logger.WarnContext(ctx, "reconcile virtual source", "source", source, "error", err)
					}
					continue
				}
			}
		}
	}

	// Bound the queue by evicting monitoring work that is finished. This runs
	// after reconciliation so a completed item still contributed to its
	// source's keep set for this pass; once a source is fully evicted it drops
	// out of future keep sets and is no longer reconciled, leaving its catalog
	// rows in place.
	pruned := 0
	if !budgetExhausted && ctx.Err() == nil {
		count, err := m.pruneCompletedMovies()
		if err != nil {
			m.logger.WarnContext(ctx, "prune completed virtual media", "error", err)
		} else {
			pruned = count
			if count > 0 {
				m.logger.InfoContext(ctx, "pruned completed virtual media from the monitor queue",
					"pruned", count, "bound", maxMonitoredItems, "remaining", m.itemCount())
			}
		}
		// A registered item whose catalog row has disappeared was removed
		// elsewhere; keeping it in the queue cannot protect or restore it, so
		// evict it. This is the only pruning signal that positively means
		// "gone"; deferred and failing items are never probed.
		if presence, ok := registrar.(mediaPresence); ok {
			missing, err := m.evictMissing(ctx, presence, items)
			if err != nil {
				m.logger.WarnContext(ctx, "prune missing virtual media", "error", err)
			} else if missing > 0 {
				pruned += missing
				m.logger.InfoContext(ctx, "evicted missing virtual media from the monitor queue",
					"evicted", missing, "bound", maxMonitoredItems, "remaining", m.itemCount())
			}
		}
	}

	output := map[string]any{
		"media_checked":    len(items),
		"processed":        processed,
		"remaining":        n - processed,
		"ready":            ready,
		"pending":          pending,
		"budget_exhausted": budgetExhausted,
		"cursor":           m.currentCursor(),
		"queue_bound":      maxMonitoredItems,
	}
	if deferred > 0 {
		output["deferred"] = deferred
	}
	if pruned > 0 {
		output["pruned"] = pruned
	}
	return &RunScheduledTaskResponse{Output: output}, nil
}

// passStartIndex returns the index in the sorted snapshot where the pass
// resumes, wrapping to the front once the cursor is at or past the last key.
func (m *mediaMonitor) passStartIndex(items []monitoredMedia) int {
	cursor := m.currentCursor()
	if cursor == "" || len(items) == 0 {
		return 0
	}
	i := sort.Search(len(items), func(i int) bool { return items[i].Key > cursor })
	if i >= len(items) {
		return 0
	}
	return i
}

// processItem runs bounded evaluate/register work for a single queue item and
// classifies the result. The pass deadline wins over the per-item bound so an
// expired pass stops cleanly; a per-item timeout (or an unattributable context
// error) is a deferral, and any other error is a per-item failure.
func (m *mediaMonitor) processItem(ctx context.Context, item monitoredMedia) (monitoredMedia, string, itemOutcome) {
	itemCtx := ctx
	if m.itemTimeout > 0 {
		var cancel context.CancelFunc
		itemCtx, cancel = context.WithTimeout(ctx, m.itemTimeout)
		defer cancel()
	}
	evaluate := m.evaluateFn
	if evaluate == nil {
		evaluate = m.evaluate
	}
	updated, message, evaluationErr := evaluate(itemCtx, item)
	if evaluationErr != nil {
		switch {
		case ctx.Err() != nil:
			return item, "", itemBudgetExhausted
		case itemCtx.Err() != nil, errors.Is(evaluationErr, context.Canceled), errors.Is(evaluationErr, context.DeadlineExceeded):
			m.logger.DebugContext(itemCtx, "defer virtual media item; per-item budget elapsed", "key", item.Key)
			return item, "", itemDeferred
		default:
			m.logger.WarnContext(itemCtx, "evaluate virtual media", "key", item.Key, "error", evaluationErr)
			return item, "", itemFailed
		}
	}
	if updated.Ready && (strings.TrimSpace(updated.Title) == "" || (updated.MediaType == "series" && len(updated.Episodes) == 0)) {
		updated.Ready = false
		return updated, message, itemPending
	}
	if updated.Ready {
		if updated.MediaType == "movie" && m.isRegistered(updated.Key) {
			return updated, message, itemPending
		}
		if err := m.register(itemCtx, updated); err != nil {
			switch {
			case ctx.Err() != nil:
				return item, "", itemBudgetExhausted
			case itemCtx.Err() != nil:
				m.logger.DebugContext(itemCtx, "defer virtual media registration; per-item budget elapsed", "key", updated.Key)
				return updated, "", itemDeferred
			default:
				m.logger.ErrorContext(itemCtx, "register virtual media", "key", updated.Key, "error", err)
				return updated, "", itemFailed
			}
		}
		m.markRegistered(updated.Key)
		if updated.MediaType == "series" {
			m.rememberSeriesEpisodes(updated.Key, updated.Episodes)
		}
		return updated, message, itemReady
	}
	return updated, message, itemPending
}

// reconcileSource bounds one source's reconciliation by the per-item timeout so
// a single slow sweep cannot run past the pass budget.
func (m *mediaMonitor) reconcileSource(ctx context.Context, reconciler mediaReconciler, source string, keepIDs []string, libraryIDs []int, evidence ReconcileEvidence) error {
	recCtx := ctx
	if m.itemTimeout > 0 {
		var cancel context.CancelFunc
		recCtx, cancel = context.WithTimeout(ctx, m.itemTimeout)
		defer cancel()
	}
	return reconciler.Reconcile(recCtx, source, keepIDs, libraryIDs, evidence)
}

// monitorItemSource names a queue item's source key, defaulting to the shared
// "monitor" bucket when it carries none.
func monitorItemSource(item monitoredMedia) string {
	if item.SourceKey != "" {
		return item.SourceKey
	}
	return "monitor"
}

// sortedSourceKeys returns map keys in deterministic order.
func sortedSourceKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (m *mediaMonitor) fetchCinemeta(ctx context.Context, item monitoredMedia) (monitoredMedia, error) {
	if item.IMDbID == "" {
		// Try to resolve IMDb ID from TMDB when available.
		if item.TMDBID != "" && m.config.TMDBAPIKey != "" {
			if ids, err := fetchTMDBExternalIDs(ctx, item.MediaType, item.TMDBID, m.config.TMDBAPIKey); err == nil && ids.IMDbID != "" {
				item.IMDbID = ids.IMDbID
			}
		}
	}
	if item.IMDbID == "" {
		return item, errors.New("IMDb ID required for Cinemeta metadata")
	}
	endpoint := strings.TrimRight(cinemetaBaseURL, "/") + "/meta/" + item.MediaType + "/" + url.PathEscape(item.IMDbID) + ".json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return item, err
	}
	resp, err := metadataClient.Do(req)
	if err != nil {
		return item, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return item, fmt.Errorf("Cinemeta HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Meta struct {
			Name, Description, Poster, Background string
			Runtime                               string `json:"runtime"`
			Genres                                []string
			Videos                                []struct {
				ID, Title, Overview, Thumbnail string
				Season, Episode                int
				Released                       time.Time
			} `json:"videos"`
		} `json:"meta"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return item, err
	}
	if payload.Meta.Name != "" {
		item.Title = payload.Meta.Name
	}
	item.Overview, item.Poster, item.Backdrop, item.Genres = payload.Meta.Description, payload.Meta.Poster, payload.Meta.Background, payload.Meta.Genres
	if runtime := parseRuntimeMinutes(payload.Meta.Runtime); runtime > 0 {
		item.Runtime = runtime
	}
	freshEpisodes := make([]virtualEpisode, 0, len(payload.Meta.Videos))
	for _, video := range payload.Meta.Videos {
		freshEpisodes = append(freshEpisodes, virtualEpisode{Season: video.Season, Episode: video.Episode, Title: video.Title, Overview: video.Overview, Thumbnail: video.Thumbnail, Released: video.Released})
	}
	item.Episodes = mergeSeriesEpisodeMetadata(item.Episodes, freshEpisodes)
	return item, nil
}

var htmlTagPattern = regexp.MustCompile(`<[^>]*>`)
var runtimeHourPattern = regexp.MustCompile(`(?i)(\d+)\s*h`)
var runtimeMinutePattern = regexp.MustCompile(`(?i)(\d+)\s*m`)
var runtimeNumberPattern = regexp.MustCompile(`\d+`)

func parseRuntimeMinutes(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	hours, minutes := 0, 0
	if match := runtimeHourPattern.FindStringSubmatch(value); len(match) == 2 {
		hours, _ = strconv.Atoi(match[1])
	}
	if match := runtimeMinutePattern.FindStringSubmatch(value); len(match) == 2 {
		minutes, _ = strconv.Atoi(match[1])
	}
	if hours > 0 || minutes > 0 {
		return hours*60 + minutes
	}
	if match := runtimeNumberPattern.FindString(value); match != "" {
		minutes, _ = strconv.Atoi(match)
	}
	return minutes
}

func cleanTVMazeSummary(value string) string {
	return strings.TrimSpace(html.UnescapeString(htmlTagPattern.ReplaceAllString(value, "")))
}

func (m *mediaMonitor) fetchTVMaze(ctx context.Context, item monitoredMedia) (monitoredMedia, error) {
	if item.IMDbID == "" && item.TVDBID == "" && item.TMDBID != "" {
		m.mu.Lock()
		apiKey := m.config.TMDBAPIKey
		m.mu.Unlock()
		if apiKey != "" {
			if externalIDs, err := fetchTMDBExternalIDs(ctx, item.MediaType, item.TMDBID, apiKey); err == nil {
				if externalIDs.IMDbID != "" {
					item.IMDbID = externalIDs.IMDbID
				}
				if externalIDs.TVDBID > 0 {
					item.TVDBID = strconv.Itoa(externalIDs.TVDBID)
				}
			}
		}
	}
	lookup, err := url.Parse(strings.TrimRight(tvmazeBaseURL, "/") + "/lookup/shows")
	if err != nil {
		return item, err
	}
	query := lookup.Query()
	if item.IMDbID != "" {
		query.Set("imdb", item.IMDbID)
	} else if item.TVDBID != "" {
		query.Set("thetvdb", item.TVDBID)
	} else if item.Title != "" {
		lookup, _ = url.Parse(strings.TrimRight(tvmazeBaseURL, "/") + "/singlesearch/shows")
		query = lookup.Query()
		query.Set("q", item.Title)
	} else {
		return item, errors.New("IMDb, TVDB, or Title required for TVMaze")
	}
	lookup.RawQuery = query.Encode()
	client := metadataClient
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, lookup.String(), nil)
	resp, err := client.Do(req)
	if err != nil {
		return item, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return item, fmt.Errorf("TVMaze lookup HTTP %d", resp.StatusCode)
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return item, err
	}
	var show struct {
		ID            int `json:"id"`
		Name, Summary string
		Genres        []string
		Premiered     string
		Image         struct{ Medium, Original string }
	}
	if err := json.Unmarshal(bodyBytes, &show); err != nil || show.ID <= 0 {
		var wrapped struct {
			Show struct {
				ID            int `json:"id"`
				Name, Summary string
				Genres        []string
				Premiered     string
				Image         struct{ Medium, Original string }
			} `json:"show"`
		}
		if err := json.Unmarshal(bodyBytes, &wrapped); err == nil {
			show = wrapped.Show
		}
	}
	if show.ID <= 0 {
		return item, errors.New("TVMaze returned no show ID")
	}
	episodesURL := fmt.Sprintf("%s/shows/%d/episodes", strings.TrimRight(tvmazeBaseURL, "/"), show.ID)
	episodeReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, episodesURL, nil)
	episodeResp, err := client.Do(episodeReq)
	if err != nil {
		return item, err
	}
	defer episodeResp.Body.Close()
	if episodeResp.StatusCode != http.StatusOK {
		return item, fmt.Errorf("TVMaze episodes HTTP %d", episodeResp.StatusCode)
	}
	var episodes []struct {
		Name, Summary, Airdate, Airstamp string
		Season, Number                   int
		Runtime                          int
		Image                            struct{ Medium, Original string }
	}
	if err := json.NewDecoder(io.LimitReader(episodeResp.Body, maxResponseBytes)).Decode(&episodes); err != nil {
		return item, err
	}
	if show.Name != "" {
		item.Title = show.Name
	}
	item.Overview, item.Genres = cleanTVMazeSummary(show.Summary), show.Genres
	if show.Image.Original != "" {
		item.Poster = show.Image.Original
	} else {
		item.Poster = show.Image.Medium
	}
	freshEpisodes := make([]virtualEpisode, 0, len(episodes))
	for _, episode := range episodes {
		released, parseErr := time.Parse(time.RFC3339, episode.Airstamp)
		if parseErr != nil && episode.Airdate != "" {
			released, _ = time.Parse("2006-01-02", episode.Airdate)
		}
		thumbnail := episode.Image.Original
		if thumbnail == "" {
			thumbnail = episode.Image.Medium
		}
		freshEpisodes = append(freshEpisodes, virtualEpisode{Season: episode.Season, Episode: episode.Number, Runtime: episode.Runtime, Title: episode.Name, Overview: cleanTVMazeSummary(episode.Summary), Thumbnail: thumbnail, Released: released})
	}
	item.Episodes = mergeSeriesEpisodeMetadata(item.Episodes, freshEpisodes)
	return item, nil
}

type tmdbReleaseDates struct {
	Results []struct {
		Country string `json:"iso_3166_1"`
		Dates   []struct {
			Date time.Time `json:"release_date"`
			Type int       `json:"type"`
		} `json:"release_dates"`
	} `json:"results"`
}

func (m *mediaMonitor) movieRelease(ctx context.Context, item monitoredMedia) (time.Time, error) {
	m.mu.Lock()
	cfg := m.config
	m.mu.Unlock()
	if cfg.TMDBAPIKey != "" && item.TMDBID != "" {
		release, err := fetchTMDBRelease(ctx, item.TMDBID, cfg.TMDBAPIKey)
		if err == nil {
			return release, nil
		}
	}
	if item.IMDbID == "" && item.TMDBID != "" && cfg.TMDBAPIKey != "" {
		if ext, err := fetchTMDBExternalIDs(ctx, item.MediaType, item.TMDBID, cfg.TMDBAPIKey); err == nil && ext.IMDbID != "" {
			item.IMDbID = ext.IMDbID
		}
	}
	if item.IMDbID == "" {
		return time.Time{}, errors.New("IMDb ID required for Cinemeta fallback")
	}
	endpoint := strings.TrimRight(cinemetaBaseURL, "/") + "/meta/movie/" + url.PathEscape(item.IMDbID) + ".json"
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	client := metadataClient
	resp, err := client.Do(request)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return time.Time{}, fmt.Errorf("Cinemeta HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Meta struct {
			Released    time.Time `json:"released"`
			ReleaseInfo string    `json:"releaseInfo"`
			Year        string    `json:"year"`
		} `json:"meta"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return time.Time{}, err
	}
	if !payload.Meta.Released.IsZero() {
		// Cinemeta exposes the theatrical/premiere date, not a verified home
		// release. Do not let a newly opened theatrical title bypass the gate.
		// For older catalog titles, a conservative 90-day window keeps the
		// no-TMDB fallback useful without claiming day-one home availability.
		presumedHomeRelease := payload.Meta.Released.AddDate(0, 0, 90)
		if presumedHomeRelease.After(time.Now()) {
			return time.Time{}, errNoHomeRelease
		}
		return presumedHomeRelease, nil
	}
	for _, v := range []string{payload.Meta.ReleaseInfo, payload.Meta.Year} {
		if len(strings.TrimSpace(v)) >= 4 {
			if y, e := strconv.Atoi(strings.TrimSpace(v)[:4]); e == nil {
				if y < time.Now().Year() {
					// Previous catalog years can be presumed released on Jan 1 of that past year.
					return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC), nil
				}
				// Current-year or future-year titles without an explicit home release date
				// must remain gated as theatrical/unreleased until verified.
				return time.Time{}, errNoHomeRelease
			}
		}
	}
	if item.Year > 0 && int(item.Year) < time.Now().Year() {
		return time.Date(int(item.Year), 1, 1, 0, 0, 0, 0, time.UTC), nil
	}
	return time.Time{}, errNoHomeRelease
}

func (m *mediaMonitor) fetchTMDBMovieRuntime(ctx context.Context, item monitoredMedia) (int, error) {
	m.mu.Lock()
	key := m.config.TMDBAPIKey
	m.mu.Unlock()
	if key == "" || item.TMDBID == "" {
		return 0, nil
	}
	endpoint := strings.TrimRight(tmdbBaseURL, "/") + "/movie/" + url.PathEscape(item.TMDBID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if strings.Count(key, ".") == 2 {
		req.Header.Set("Authorization", "Bearer "+key)
	} else {
		query := req.URL.Query()
		query.Set("api_key", key)
		req.URL.RawQuery = query.Encode()
	}
	resp, err := metadataClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("TMDB movie details HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Runtime int `json:"runtime"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return 0, err
	}
	return payload.Runtime, nil
}
func fetchTMDBRelease(ctx context.Context, id, key string) (time.Time, error) {
	endpoint := strings.TrimRight(tmdbBaseURL, "/") + "/movie/" + url.PathEscape(id) + "/release_dates"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if strings.Count(key, ".") == 2 {
		req.Header.Set("Authorization", "Bearer "+key)
	} else {
		q := req.URL.Query()
		q.Set("api_key", key)
		req.URL.RawQuery = q.Encode()
	}
	resp, err := metadataClient.Do(req)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return time.Time{}, fmt.Errorf("TMDB HTTP %d", resp.StatusCode)
	}
	var data tmdbReleaseDates
	if err = json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&data); err != nil {
		return time.Time{}, err
	}
	results := data.Results
	// TMDB release types:
	// Type 4: Digital (VOD, iTunes)
	// Type 5: Physical (Blu-ray, DVD)
	// Type 6: TV / Streaming Originals (Apple TV+, Netflix, Disney+)
	for _, typ := range []int{4, 5, 6} {
		var earliest time.Time
		for _, r := range results {
			for _, d := range r.Dates {
				if d.Type == typ && !d.Date.IsZero() && (earliest.IsZero() || d.Date.Before(earliest)) {
					earliest = d.Date
				}
			}
		}
		if !earliest.IsZero() {
			return earliest, nil
		}
	}
	// No Digital/Physical/TV date on record means the movie is still
	// theatrical-only: keep it queued. The earlier "presume home release 90
	// days after the earliest date" fallback admitted premiere and theatrical
	// dates into that calculation, registering titles that were still only in
	// theaters. Indexer RSS / Prowlarr confirmation remains the escape hatch
	// for titles whose home release is real but not yet on TMDB.
	return time.Time{}, errNoHomeRelease
}

type tmdbExternalIDs struct {
	IMDbID string `json:"imdb_id"`
	TVDBID int    `json:"tvdb_id"`
}

func fetchTMDBExternalIDs(ctx context.Context, mediaType, tmdbID, key string) (tmdbExternalIDs, error) {
	kind := "tv"
	if mediaType == "movie" {
		kind = "movie"
	}
	endpoint := strings.TrimRight(tmdbBaseURL, "/") + "/" + kind + "/" + url.PathEscape(tmdbID) + "/external_ids"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if strings.Count(key, ".") == 2 {
		req.Header.Set("Authorization", "Bearer "+key)
	} else {
		q := req.URL.Query()
		q.Set("api_key", key)
		req.URL.RawQuery = q.Encode()
	}
	resp, err := metadataClient.Do(req)
	if err != nil {
		return tmdbExternalIDs{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return tmdbExternalIDs{}, fmt.Errorf("TMDB external_ids HTTP %d", resp.StatusCode)
	}
	var out tmdbExternalIDs
	if err = json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return tmdbExternalIDs{}, err
	}
	return out, nil
}

// configuredFolderID coerces a media_folder_id connection config value to a
// positive library ID (copied verbatim from the plugin library.go).
func configuredFolderID(value any) (int, error) {
	switch v := value.(type) {
	case float64:
		if v > 0 && v == float64(int(v)) {
			return int(v), nil
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n, nil
		}
	case nil:
		return 0, nil
	}
	return 0, errors.New("library ID must be a positive integer")
}

// sameParentDomain reports whether host a and host b share at least two
// rightmost domain labels (copied verbatim from the plugin main.go).
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

// newRestrictedRedirectHTTPClient bounds redirects to same-host or
// same-registered-domain targets (copied verbatim from the plugin main.go).
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
