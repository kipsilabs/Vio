package prowlarr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/quality"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

// StreamCandidate aliases the shared stream package type so the Prowlarr
// classifier satisfies the resolver's CandidateClassifier interface.
type StreamCandidate = stream.StreamCandidate

// QualityConfig, QualityProfile and CustomFormat alias the quality package
// types so Prowlarr matching honors the same operator configuration.
type QualityConfig = quality.QualityConfig
type QualityProfile = quality.QualityProfile
type CustomFormat = quality.CustomFormat

// SearchClient is the exported Prowlarr search client.
type SearchClient = prowlarrSearchClient

// SearchItem is one result from Prowlarr's /api/v1/search endpoint.
type SearchItem = prowlarrRelease

// QualityScore returns the custom-format score the sort assigned the release.
// Callers that got the release from SearchMonitoredReleases read the score the
// sort already computed; an unprepared release parses locally so the value is
// never silently zero on a direct caller.
func (r prowlarrRelease) QualityScore() int {
	if r.parsedCandidate == nil {
		candidate := StreamCandidate{Name: r.Title, Title: r.Title, URL: r.DownloadURL}
		parseStreamDetails(&candidate)
		return candidate.QualityScore
	}
	return r.parsedCandidate.QualityScore
}

// DisplayResolution returns the parsed resolution of the release title.
func (r prowlarrRelease) DisplayResolution() string {
	if r.parsedCandidate == nil {
		candidate := StreamCandidate{Name: r.Title, Title: r.Title, URL: r.DownloadURL}
		parseStreamDetails(&candidate)
		return candidate.Resolution
	}
	return r.parsedCandidate.Resolution
}

// HDR returns the parsed HDR label of the release title, empty when none.
func (r prowlarrRelease) HDR() string {
	if r.parsedCandidate == nil {
		candidate := StreamCandidate{Name: r.Title, Title: r.Title, URL: r.DownloadURL}
		parseStreamDetails(&candidate)
		return candidate.HDR
	}
	return r.parsedCandidate.HDR
}

// CodecVideo returns the parsed video codec of the release title.
func (r prowlarrRelease) CodecVideo() string {
	if r.parsedCandidate == nil {
		candidate := StreamCandidate{Name: r.Title, Title: r.Title, URL: r.DownloadURL}
		parseStreamDetails(&candidate)
		return candidate.CodecVideo
	}
	return r.parsedCandidate.CodecVideo
}

// CodecAudio returns the parsed audio codec of the release title.
func (r prowlarrRelease) CodecAudio() string {
	if r.parsedCandidate == nil {
		candidate := StreamCandidate{Name: r.Title, Title: r.Title, URL: r.DownloadURL}
		parseStreamDetails(&candidate)
		return candidate.CodecAudio
	}
	return r.parsedCandidate.CodecAudio
}

// NormalizedTitle returns the title reduced to a comparable identity.
func (r prowlarrRelease) NormalizedTitle() string {
	if r.normalizedTitle != "" {
		return r.normalizedTitle
	}
	title, _ := normalizeReleaseTitle(r.Title)
	return title
}

// MonitoredMedia aliases the monitored catalog item matched against Prowlarr.
type MonitoredMedia = monitoredMedia

// VirtualEpisode aliases a series episode for episode-level matching.
type VirtualEpisode = virtualEpisode

// monitoredMedia mirrors the virtual-library monitored item shape.
type monitoredMedia struct {
	Key       string `json:"key"`
	MediaType string `json:"media_type"`
	Title     string `json:"title"`
	StreamID  string `json:"stream_id,omitempty"`
	IMDbID    string `json:"imdb_id,omitempty"`
	TMDBID    string `json:"tmdb_id,omitempty"`
	TVDBID    string `json:"tvdb_id,omitempty"`
	SourceKey string `json:"source_key,omitempty"`
	// MetadataSource is the per-request metadata provider choice ("tvdb" or
	// "tmdb"); empty follows the library provider chain.
	MetadataSource     string           `json:"metadata_source,omitempty"`
	MediaFolderID      int              `json:"media_folder_id,omitempty"`
	Year               int32            `json:"year,omitempty"`
	Runtime            int              `json:"runtime,omitempty"`
	Release            time.Time        `json:"release,omitempty"`
	Ready              bool             `json:"ready"`
	Force              bool             `json:"force"`
	Overview           string           `json:"overview,omitempty"`
	Poster             string           `json:"poster,omitempty"`
	PosterSourcePath   string           `json:"poster_source_path,omitempty"`
	Backdrop           string           `json:"backdrop,omitempty"`
	BackdropSourcePath string           `json:"backdrop_source_path,omitempty"`
	Genres             []string         `json:"genres,omitempty"`
	Episodes           []virtualEpisode `json:"episodes,omitempty"`
	// CatalogContentID is the catalog identity resolved from provider aliases.
	// Once set, enrichment may add provider IDs but must never change this ID.
	CatalogContentID string `json:"catalog_content_id,omitempty"`
	// MetadataNextSeason is the resumable series metadata cursor. Zero means
	// the next lookup starts from the first season.
	MetadataNextSeason int `json:"metadata_next_season,omitempty"`
	// MetadataComplete records that the last lookup reached the end of the
	// season list. It never authorizes deleting episodes.
	MetadataComplete bool `json:"metadata_complete,omitempty"`
}

// virtualEpisode mirrors a series episode for episode-level matching.
type virtualEpisode struct {
	Season    int       `json:"season"`
	Episode   int       `json:"episode"`
	Runtime   int       `json:"runtime,omitempty"`
	Title     string    `json:"title"`
	Overview  string    `json:"overview,omitempty"`
	Thumbnail string    `json:"thumbnail,omitempty"`
	Released  time.Time `json:"released,omitempty"`
	Available bool      `json:"available,omitempty"`
	// Season artwork is repeated on each episode because the registration
	// payload is episode-based; the catalog upsert keeps one row per season.
	SeasonPosterPath       string `json:"season_poster_path,omitempty"`
	SeasonPosterSourcePath string `json:"season_poster_source_path,omitempty"`
	SeasonPosterThumbhash  string `json:"season_poster_thumbhash,omitempty"`
}

// NewSearchClient builds a SearchClient with an injectable HTTP client. A nil
// client selects a restricted-redirect client whose timeout Configure sets
// from the operator's indexer search timeout.
func NewSearchClient(client *http.Client) *SearchClient {
	return newProwlarrSearchClient(client)
}

func parseStreamDetails(s *StreamCandidate) {
	stream.ParseStreamDetails(s)
}

func matchProfile(c StreamCandidate, p QualityProfile) bool {
	return quality.MatchProfile(c, p)
}

func customFormatScore(candidate StreamCandidate, formats []CustomFormat) (int, bool) {
	return quality.CustomFormatScore(candidate, formats)
}

func resolutionScore(res string) int {
	return stream.ResolutionScore(res)
}

func sourceScore(src string) int {
	return stream.SourceScore(src)
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
			if sameParentDomain(origin.Host, target.Host) {
				return nil
			}
			return errors.New("cross-origin redirects are not allowed")
		},
	}
}

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

const (
	defaultSearchCheckMinutes = 15
	minSearchCheckMinutes     = 15
	maxSearchCheckMinutes     = 10080
	// defaultSearchTimeoutSeconds matches
	// virtual_library.indexer_search_timeout_seconds' default and bounds.
	defaultSearchTimeoutSeconds = 20
	minSearchTimeoutSeconds     = 5
	maxSearchTimeoutSeconds     = 900
	maxSearchBodyBytes          = 8 << 20
	maxErrorBodyBytes           = 4 << 10
	maxProwlarrIndexBytes       = 64 << 20
	maxProwlarrIndexReleases    = 20000
	prowlarrIndexRetention      = 14 * 24 * time.Hour
)

// prowlarrRelease is one result from Prowlarr's /api/v1/search endpoint.
type prowlarrRelease struct {
	GUID        string `json:"guid"`
	Title       string `json:"title"`
	Size        int64  `json:"size"`
	Indexer     string `json:"indexer"`
	IndexerID   int    `json:"indexerId"`
	IMDbID      int64  `json:"imdbId"`
	TMDBID      int64  `json:"tmdbId"`
	TVDBID      int64  `json:"tvdbId"`
	PublishDate string `json:"publishDate"`
	DownloadURL string `json:"downloadUrl"`
	// Protocol is Prowlarr's release protocol: "usenet" or "torrent". Prowlarr
	// serializes it under the JSON key "protocol". Callers that can only enqueue
	// usenet releases filter on this so a torrent result is never offered.
	Protocol        string              `json:"protocol"`
	parsedCandidate *StreamCandidate    `json:"-"`
	episodeKeys     []episodeReleaseKey `json:"-"`
	normalizedTitle string              `json:"-"`
	// normalizedYear is the year normalizeReleaseTitle extracted from Title.
	// Persisted alongside normalizedTitle so the classifier's fallback never
	// re-runs the normalization regexes per candidate/release pair.
	normalizedYear int `json:"-"`
	// nameKey is releaseNameKey(Title): the extension-stripped, lowercased,
	// non-alphanumeric-free identity the exact-name confirmation matches on.
	nameKey string `json:"-"`
}

func prepareProwlarrRelease(release *prowlarrRelease) {
	if release == nil {
		return
	}
	candidate := StreamCandidate{Name: release.Title, Title: release.Title, URL: release.DownloadURL}
	parseStreamDetails(&candidate)
	release.parsedCandidate = &candidate
	release.episodeKeys = episodeReleaseKeys(release.Title)
	release.normalizedTitle, release.normalizedYear = normalizeReleaseTitle(release.Title)
	release.nameKey = releaseNameKey(release.Title)
}

// prowlarrSearchClient holds a cached snapshot of Prowlarr's recent releases
// search and refreshes it on a configurable interval (min 15 minutes). One
// request covers every enabled indexer.
type prowlarrSearchClient struct {
	mu       sync.Mutex
	url      string
	apiKey   string
	interval time.Duration
	// timeout bounds a single search request. It is the operator's
	// virtual_library.indexer_search_timeout_seconds and applies only to the
	// restricted-redirect client this package constructs.
	timeout time.Duration
	// injectedClient marks a caller-supplied HTTP client. Configure leaves
	// that client untouched; only the package-built client follows the
	// configured timeout.
	injectedClient bool
	client         *http.Client
	lastFetch      time.Time
	lastErr        error
	releases       []prowlarrRelease
	// releasesGeneration advances every time releases is replaced. It keys the
	// cached classification index so repeated serves reuse one build and a
	// refresh invalidates it without comparing slice backing arrays.
	releasesGeneration uint64
	classifyIndex      *prowlarrReleaseIndex
	// classifyIndexGeneration is the releasesGeneration the cached index
	// describes; a mismatch means it must be rebuilt.
	classifyIndexGeneration uint64
	indexFile               string
}

// prowlarrReleaseIndex is the read-only lookup the candidate classifier uses to
// avoid the previous candidates x releases scan. Releases are grouped by their
// exact release-name identity and by normalized title, so each candidate costs
// an O(1) exact lookup plus a bounded fallback scan over releases that already
// share its normalized title. identities mirrors the snapshot so the pair
// predicate never re-runs a normalization regex.
type prowlarrReleaseIndex struct {
	// byNameKey maps a release identity to the smallest snapshot index carrying
	// it, preserving the linear scan's first-match ordering.
	byNameKey map[string]int
	// byTitle lists snapshot indices sharing a normalized title, ascending, so
	// the weak fallback can stop at its first size-corroborated match.
	byTitle    map[string][]int
	identities []releaseIdentity
}

// releaseIdentity is the reduced form both confirmation predicates compare:
// the exact release-name key, the normalized title and year, and the size.
type releaseIdentity struct {
	nameKey string
	title   string
	year    int
	size    int64
}

// releaseIdentityOf reduces a release to its comparison identity. It reuses the
// fields prepareProwlarrRelease precomputes and only derives them for a release
// built directly in a test, so a prepared snapshot never pays the regexes here.
func releaseIdentityOf(release *prowlarrRelease) releaseIdentity {
	if release == nil {
		return releaseIdentity{}
	}
	identity := releaseIdentity{
		nameKey: release.nameKey,
		title:   release.normalizedTitle,
		year:    release.normalizedYear,
		size:    release.Size,
	}
	if identity.nameKey == "" && identity.title == "" && strings.TrimSpace(release.Title) != "" {
		identity.title, identity.year = normalizeReleaseTitle(release.Title)
		identity.nameKey = releaseNameKey(release.Title)
	}
	return identity
}

// candidateIdentity reduces a provider candidate the same way. The candidate
// carries no precomputed fields, so this runs the regexes once per candidate
// instead of once per candidate/release pair.
func candidateIdentity(candidate StreamCandidate) releaseIdentity {
	nameKey := candidateReleaseName(candidate)
	if nameKey == "" {
		return releaseIdentity{}
	}
	title, year := normalizeReleaseTitle(candidate.Title + " " + candidate.Name)
	return releaseIdentity{nameKey: nameKey, title: title, year: year, size: candidate.FileSize}
}

// releaseIdentityMatchesCandidate is the pair predicate: exact release-name
// identity first, then a normalized title+year match corroborated by compatible
// sizes so a different quality tier is not treated as confirmed.
func releaseIdentityMatchesCandidate(release, candidate releaseIdentity) bool {
	if candidate.nameKey == "" {
		return false
	}
	if release.nameKey != "" && release.nameKey == candidate.nameKey {
		return true
	}
	if release.title == "" || release.title != candidate.title {
		return false
	}
	if release.year != 0 && candidate.year != 0 && release.year != candidate.year {
		return false
	}
	// A title-only fallback is too weak on its own: Prowlarr returns many
	// quality tiers of the same movie. Require both sizes to corroborate the
	// match rather than confirming whichever tier the provider happened to
	// return. (AltMount's own classification may treat unknown sizes as
	// neutral; here a false confirmation would reorder playback.)
	if release.size <= 0 || candidate.size <= 0 {
		return false
	}
	return releaseSizesMatch(release.size, candidate.size)
}

// buildProwlarrReleaseIndex reads the identity fields prepareProwlarrRelease
// precomputes on each release, falling back to deriving them for an unprepared
// release without mutating the shared snapshot.
func buildProwlarrReleaseIndex(releases []prowlarrRelease) *prowlarrReleaseIndex {
	index := &prowlarrReleaseIndex{
		byNameKey:  make(map[string]int, len(releases)),
		byTitle:    make(map[string][]int, len(releases)),
		identities: make([]releaseIdentity, len(releases)),
	}
	for i := range releases {
		identity := releaseIdentityOf(&releases[i])
		index.identities[i] = identity
		if identity.nameKey != "" {
			if _, ok := index.byNameKey[identity.nameKey]; !ok {
				index.byNameKey[identity.nameKey] = i
			}
		}
		if identity.title != "" {
			index.byTitle[identity.title] = append(index.byTitle[identity.title], i)
		}
	}
	return index
}

func newProwlarrSearchClient(client *http.Client) *prowlarrSearchClient {
	c := &prowlarrSearchClient{timeout: defaultSearchTimeoutSeconds * time.Second}
	if client != nil {
		c.client = client
		c.injectedClient = true
	}
	return c
}

// httpClient returns the HTTP client for this configuration. The package-built
// client honors the configured timeout; a caller-injected client is returned
// as-is so its own transport semantics win.
func (c *prowlarrSearchClient) httpClient() *http.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		c.client = newRestrictedRedirectHTTPClient(c.timeout)
	}
	return c.client
}

// URL returns the configured Prowlarr base URL, or empty string.
func (c *prowlarrSearchClient) URL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.url
}

// Configure sets the Prowlarr base URL, API key, check interval, and per-search
// timeout. A timeout outside [minSearchTimeoutSeconds, maxSearchTimeoutSeconds]
// falls back to the default, matching the admin-setting validator's bounds.
func (c *prowlarrSearchClient) Configure(baseURL, apiKey string, intervalMinutes, timeoutSeconds int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	newURL := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	newKey := strings.TrimSpace(apiKey)
	if intervalMinutes < minSearchCheckMinutes {
		intervalMinutes = defaultSearchCheckMinutes
	}
	if intervalMinutes > maxSearchCheckMinutes {
		intervalMinutes = maxSearchCheckMinutes
	}
	if timeoutSeconds < minSearchTimeoutSeconds || timeoutSeconds > maxSearchTimeoutSeconds {
		timeoutSeconds = defaultSearchTimeoutSeconds
	}
	newInterval := time.Duration(intervalMinutes) * time.Minute
	newTimeout := time.Duration(timeoutSeconds) * time.Second
	if newURL != c.url || newKey != c.apiKey || newInterval != c.interval {
		c.releases = nil
		c.releasesGeneration++
		c.lastFetch = time.Time{}
		c.lastErr = nil
	}
	// Rebuild only the package-built client; a caller-injected client owns its
	// own timeout. newRestrictedRedirectHTTPClient keeps the redirect policy.
	if !c.injectedClient && newTimeout != c.timeout {
		c.client = newRestrictedRedirectHTTPClient(newTimeout)
	}
	c.url = newURL
	c.apiKey = newKey
	c.interval = newInterval
	c.timeout = newTimeout
}

func (c *prowlarrSearchClient) ConfigureIndexFile(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	path = strings.TrimSpace(path)
	if path == "" {
		path = ".vio-virtual-library-prowlarr-index.json"
	}
	c.indexFile = path
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open Prowlarr index: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxProwlarrIndexBytes+1))
	if err != nil {
		return fmt.Errorf("read Prowlarr index: %w", err)
	}
	if len(data) > maxProwlarrIndexBytes {
		return fmt.Errorf("Prowlarr index exceeds %d bytes", maxProwlarrIndexBytes)
	}
	var releases []prowlarrRelease
	if err := json.Unmarshal(data, &releases); err != nil {
		return fmt.Errorf("decode Prowlarr index: %w", err)
	}
	for i := range releases {
		prepareProwlarrRelease(&releases[i])
	}
	c.releases = pruneProwlarrReleases(releases, time.Now())
	c.releasesGeneration++
	return nil
}

func (c *prowlarrSearchClient) searchURL() (string, error) {
	return c.searchURLForQuery("")
}

// validateProwlarrBaseURL rejects a configured URL that cannot have
// "/api/v1/search" appended cleanly. The setting takes Prowlarr's server base
// URL; an indexer-scoped path (for example .../1 or .../1/api/v1/search) or a
// query string (for example ?t=movie) makes the appended path match Prowlarr's
// newznab proxy route, which answers 400. The URL is rejected rather than
// silently rewritten because a reverse-proxy deployment may legitimately use a
// path base such as http://host/prowlarr, so stripping the path would break a
// working configuration.
func validateProwlarrBaseURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return errors.New("the Prowlarr URL is not configured")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("invalid Prowlarr URL %q: %w", trimmed, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("invalid Prowlarr URL %q: expected the server base URL with a scheme, for example http://prowlarr:9696", trimmed)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || hasIndexerPath(parsed.Path) {
		return fmt.Errorf("invalid Prowlarr URL %q: expected the Prowlarr server base URL (for example http://prowlarr:9696) with no indexer id, /api/v1/search path, or query string", trimmed)
	}
	return nil
}

// NormalizeBaseURL canonicalizes a configured Prowlarr server base URL. A
// scheme-less value such as "one.vio" is a common admin input; prepending
// http:// turns it into an address the search client can actually reach
// instead of letting every staleness refresh fail with "expected the server
// base URL with a scheme". Anything still invalid after the scheme is
// prepended (an indexer path, a query string, a malformed host) is rejected so
// the caller can leave Prowlarr unconfigured rather than fail resolves. An
// empty value stays empty (Prowlarr unwired).
func NormalizeBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "http://" + trimmed
	}
	trimmed = strings.TrimRight(trimmed, "/")
	if err := validateProwlarrBaseURL(trimmed); err != nil {
		return "", err
	}
	return trimmed, nil
}

// hasIndexerPath reports whether the URL path is an indexer-scoped or newznab
// proxy path rather than a plain base (or a reverse-proxy subpath). A trailing
// numeric segment is Prowlarr's indexer id; "api", "newznab" and "torznab"
// segments are the search/download routes the client would duplicate.
func hasIndexerPath(p string) bool {
	segments := strings.Split(strings.Trim(p, "/"), "/")
	for i, segment := range segments {
		lower := strings.ToLower(segment)
		if lower == "" {
			continue
		}
		if lower == "api" || lower == "newznab" || lower == "torznab" {
			return true
		}
		if i == len(segments)-1 && isDigits(segment) {
			return true
		}
	}
	return false
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// searchURLForQuery builds the Prowlarr search endpoint URL without any
// credentials in it. The API key travels in the X-Api-Key request header so
// it can never leak through error strings or logs that embed URLs.
func (c *prowlarrSearchClient) searchURLForQuery(query string) (string, error) {
	c.mu.Lock()
	raw := strings.TrimSpace(c.url)
	c.mu.Unlock()
	if err := validateProwlarrBaseURL(raw); err != nil {
		return "", err
	}
	u, err := url.Parse(raw + "/api/v1/search")
	if err != nil {
		return "", fmt.Errorf("invalid Prowlarr URL: %w", err)
	}
	q := u.Query()
	q.Set("limit", "1000")
	q.Del("categories")
	q.Add("categories", "2000")
	q.Add("categories", "5000")
	if strings.TrimSpace(query) != "" {
		q.Set("query", query)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (c *prowlarrSearchClient) newSearchRequest(ctx context.Context, searchURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	c.mu.Lock()
	key := c.apiKey
	c.mu.Unlock()
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	return req, nil
}

// errorSnippet reads at most maxErrorBodyBytes of a response body and marks
// truncation. Prowlarr's 400 responses carry the reason (for example
// "No such function (...)"), so the snippet is the point of the error.
func errorSnippet(body io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes+1))
	truncated := len(data) > maxErrorBodyBytes
	if truncated {
		data = data[:maxErrorBodyBytes]
	}
	snippet := strings.TrimSpace(string(data))
	if snippet == "" {
		snippet = "(empty response body)"
	}
	if truncated {
		snippet += " ... (truncated)"
	}
	return snippet
}

// httpError describes a non-2xx Prowlarr response: status, effective request
// URL, and a bounded body snippet. The API key travels in the X-Api-Key
// header and is never part of requestURL, but it is redacted from both the URL
// and the snippet defensively.
func (c *prowlarrSearchClient) httpError(resp *http.Response, requestURL string) error {
	snippet := errorSnippet(resp.Body)
	c.mu.Lock()
	key := c.apiKey
	c.mu.Unlock()
	if key != "" {
		snippet = strings.ReplaceAll(snippet, key, "[redacted]")
		requestURL = strings.ReplaceAll(requestURL, key, "[redacted]")
	}
	return fmt.Errorf("prowlarr returned HTTP %d for %s: %s", resp.StatusCode, requestURL, snippet)
}

func (c *prowlarrSearchClient) search(ctx context.Context, item monitoredMedia) ([]prowlarrRelease, error) {
	searchURL, err := c.searchURLForQuery(item.Title)
	if err != nil {
		return nil, err
	}
	req, err := c.newSearchRequest(ctx, searchURL)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("Prowlarr search request failed: %w", err) //nolint:staticcheck // Prowlarr is a proper product name.
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.httpError(resp, searchURL)
	}
	releases, err := parseProwlarrSearch(io.LimitReader(resp.Body, maxSearchBodyBytes+1))
	if err != nil {
		return nil, err
	}
	for i := range releases {
		prepareProwlarrRelease(&releases[i])
	}
	return releases, nil
}

func (c *prowlarrSearchClient) Stale() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.url == "" || c.lastFetch.IsZero() || time.Since(c.lastFetch) >= c.interval
}

func (c *prowlarrSearchClient) refreshIfStale(ctx context.Context) error {
	if !c.Stale() {
		return nil
	}
	return c.refresh(ctx)
}

func (c *prowlarrSearchClient) refresh(ctx context.Context) error {
	searchURL, err := c.searchURL()
	if err != nil {
		c.mu.Lock()
		c.lastErr = err
		c.mu.Unlock()
		return err
	}
	req, err := c.newSearchRequest(ctx, searchURL)
	if err != nil {
		return err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		err = fmt.Errorf("Prowlarr search request failed: %w", err) //nolint:staticcheck // Prowlarr is a proper product name.
		c.mu.Lock()
		c.lastErr = err
		c.mu.Unlock()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := c.httpError(resp, searchURL)
		c.mu.Lock()
		c.lastErr = err
		c.mu.Unlock()
		return err
	}
	releases, err := parseProwlarrSearch(io.LimitReader(resp.Body, maxSearchBodyBytes+1))
	if err != nil {
		c.mu.Lock()
		c.lastErr = err
		c.mu.Unlock()
		return err
	}
	c.mu.Lock()
	merged := mergeProwlarrReleases(c.releases, releases, time.Now())
	c.releases = merged
	c.releasesGeneration++
	c.lastFetch = time.Now()
	c.lastErr = nil
	indexFile := c.indexFile
	c.mu.Unlock()
	if indexFile != "" {
		if err := saveProwlarrIndex(indexFile, merged); err != nil {
			return err
		}
	}
	return nil
}

func releaseKey(release prowlarrRelease) string {
	if release.GUID != "" {
		return "guid:" + release.GUID
	}
	if release.DownloadURL != "" {
		return "download:" + release.DownloadURL
	}
	return release.Indexer + "\x00" + release.Title + "\x00" + release.PublishDate
}

func mergeProwlarrReleases(existing, incoming []prowlarrRelease, now time.Time) []prowlarrRelease {
	byKey := make(map[string]prowlarrRelease, len(existing)+len(incoming))
	for _, release := range append(append([]prowlarrRelease(nil), existing...), incoming...) {
		prepareProwlarrRelease(&release)
		byKey[releaseKey(release)] = release
	}
	merged := make([]prowlarrRelease, 0, len(byKey))
	for _, release := range byKey {
		if release.PublishDate != "" {
			if published, err := time.Parse(time.RFC3339, release.PublishDate); err == nil && now.Sub(published) > prowlarrIndexRetention {
				continue
			}
		}
		merged = append(merged, release)
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].PublishDate > merged[j].PublishDate })
	if len(merged) > maxProwlarrIndexReleases {
		merged = merged[:maxProwlarrIndexReleases]
	}
	return merged
}

func pruneProwlarrReleases(releases []prowlarrRelease, now time.Time) []prowlarrRelease {
	return mergeProwlarrReleases(nil, releases, now)
}

func saveProwlarrIndex(path string, releases []prowlarrRelease) error {
	data, kept := marshalProwlarrIndex(releases)
	if len(data) > maxProwlarrIndexBytes {
		return fmt.Errorf("Prowlarr index exceeds %d bytes after retaining %d releases", maxProwlarrIndexBytes, kept)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".silo-prowlarr-index-*.tmp")
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

func marshalProwlarrIndex(releases []prowlarrRelease) ([]byte, int) {
	for len(releases) > 0 {
		data, err := json.Marshal(releases)
		if err != nil {
			return nil, len(releases)
		}
		data = append(data, '\n')
		if len(data) <= maxProwlarrIndexBytes {
			return data, len(releases)
		}
		releases = releases[:len(releases)-1]
	}
	return []byte("[]\n"), 0
}

func parseProwlarrSearch(r io.Reader) ([]prowlarrRelease, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxSearchBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Prowlarr search: %w", err)
	}
	if int64(len(data)) > maxSearchBodyBytes {
		return nil, fmt.Errorf("Prowlarr search response exceeds %d bytes", maxSearchBodyBytes)
	}
	var releases []prowlarrRelease
	if err := json.Unmarshal(data, &releases); err != nil {
		return nil, fmt.Errorf("decode Prowlarr search: %w", err)
	}
	return releases, nil
}

// --- Title normalization for fallback matching ---

var prowlarrSepPattern = regexp.MustCompile(`[._\-/()\[\]{}]`)
var prowlarrTagPattern = regexp.MustCompile(`(?i)\b(2160p|1080p|720p|480p|4k|uhd|hdr|dolby|dolby.?vision|dv|hdr10|hdr10\+|ddp|dd|dts|dts-?hd|truehd|flac|aac|ac3|atmos|x264|x265|hevc|avc|av1|web-?dl|web-?rip|web|blu-?ray|brrip|bdrip|hdtv|dvdrip|remux|proper|repack|extended|imax|directors.?cut|unrated|multi|dual|10bit|8bit|10-?bit|h\.?264|h\.?265|s[0-9]{2}e[0-9]{2}|season[0-9]+|episode[0-9]+|amzn|amazon|nf|netflix|dsnp|disney|hbo|max|appletv|itunes|vudu|prime|red|group|post|subbed|dubbed|dual-?audio|hc|hdcam|cam|ts|telesync|telecine|screener|scr|complete|1080|720|2160)\b.*`)
var prowlarrCleanPattern = regexp.MustCompile(`[^a-z0-9]+`)
var prowlarrYearPattern = regexp.MustCompile(`\b(19|20)\d{2}\b`)

var episodeReleasePattern = regexp.MustCompile(`(?i)(?:s([0-9]{1,3})e([0-9]{1,3})|([0-9]{1,3})x([0-9]{1,3})|season[ ._-]*([0-9]{1,3})[ ._-]*episode[ ._-]*([0-9]{1,3}))`)

type episodeReleaseKey struct {
	season  int
	episode int
}

func episodeReleaseKeys(title string) []episodeReleaseKey {
	matches := episodeReleasePattern.FindAllStringSubmatch(title, -1)
	keys := make([]episodeReleaseKey, 0, len(matches))
	for _, match := range matches {
		values := [][2]int{{1, 2}, {3, 4}, {5, 6}}
		for _, pair := range values {
			if match[pair[0]] == "" || match[pair[1]] == "" {
				continue
			}
			season, seasonErr := strconv.Atoi(match[pair[0]])
			episode, episodeErr := strconv.Atoi(match[pair[1]])
			if seasonErr == nil && episodeErr == nil && season > 0 && episode > 0 {
				keys = append(keys, episodeReleaseKey{season: season, episode: episode})
			}
		}
	}
	return keys
}

func normalizeReleaseTitle(raw string) (title string, year int) {
	s := prowlarrSepPattern.ReplaceAllString(raw, " ")
	s = prowlarrTagPattern.ReplaceAllString(s, "")
	parts := strings.Fields(strings.ToLower(s))
	var kept []string
	for _, p := range parts {
		if m := prowlarrYearPattern.FindString(p); m != "" {
			if y, err := strconv.Atoi(m); err == nil {
				year = y
			}
			continue
		}
		if len(p) > 1 {
			kept = append(kept, p)
		}
	}
	cleaned := prowlarrCleanPattern.ReplaceAllString(strings.Join(kept, " "), " ")
	return strings.TrimSpace(cleaned), year
}

func normalizedReleaseTitle(raw string) string {
	title, _ := normalizeReleaseTitle(raw)
	return title
}

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
	for _, ext := range []string{".mkv", ".mp4", ".avi", ".ts", ".m2ts", ".mov", ".webm"} {
		value = strings.TrimSuffix(value, ext)
	}
	return prowlarrCleanPattern.ReplaceAllString(value, "")
}

// candidateReleaseName returns the most release-like identity carried by a
// provider candidate. AltMount-style providers put the release name on the
// first line of the stream title (the remaining lines carry size and indexer
// badges), so only the first line is considered.
func candidateReleaseName(candidate StreamCandidate) string {
	for _, value := range []string{
		candidate.BehaviorHints.Filename,
		firstReleaseLine(candidate.Title),
		firstReleaseLine(candidate.Name),
	} {
		if key := releaseNameKey(value); key != "" {
			return key
		}
	}
	if parsed, err := url.Parse(strings.TrimSpace(candidate.URL)); err == nil {
		if key := releaseNameKey(path.Base(parsed.Path)); key != "" {
			return key
		}
	}
	return ""
}

func firstReleaseLine(value string) string {
	if idx := strings.IndexAny(value, "\r\n"); idx >= 0 {
		return value[:idx]
	}
	return value
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

// prowlarrReleaseConfirmsCandidate reports whether a provider candidate is the
// same release that Prowlarr already indexed. Exact release-name identity is
// the primary signal; a normalized title+year match additionally requires
// compatible sizes so a different quality tier is not treated as confirmed.
//
// It is the linear reference for releaseIdentityMatchesCandidate, which
// ClassifyCandidates uses through its inverted index.
func prowlarrReleaseConfirmsCandidate(release prowlarrRelease, candidate StreamCandidate) bool {
	return releaseIdentityMatchesCandidate(releaseIdentityOf(&release), candidateIdentity(candidate))
}

// IndexerReleaseMatchesProvider reports whether a provider release (as named
// and sized by AltMount) is the same posting as a Prowlarr search result. It is
// the strict dedup used by the virtual-library release listing, which must not
// offer a release the provider already has.
//
// Deliberately stricter than prowlarrReleaseConfirmsCandidate: only exact
// release-name identity counts, corroborated by a size match when both sizes
// are known. The looser title+year fallback is intentionally excluded, because
// it exists to reorder playback candidates (where a wrong confirmation only
// changes preference), not to hide releases from a user (where a wrong match
// silently drops a release they could have requested). When the names cannot be
// reduced to an exact key, the releases are treated as different.
func IndexerReleaseMatchesProvider(release SearchItem, providerReleaseName string, providerSize int64) bool {
	releaseKeyValue := releaseNameKey(release.Title)
	providerKey := releaseNameKey(providerReleaseName)
	if releaseKeyValue == "" || providerKey == "" {
		return false
	}
	if releaseKeyValue != providerKey {
		return false
	}
	// Exact name identity is the signal; size is corroboration. An unknown size
	// on either side does not reject (releaseSizesMatch treats <=0 as unknown),
	// so a provider that omits sizes still dedups by name.
	return releaseSizesMatch(release.Size, providerSize)
}

// maxFallbackReleaseScan bounds the weak title fallback the classifier runs per
// candidate. Exact release-name identity is resolved by map lookup and is never
// capped; only a snapshot with an implausible number of same-titled releases can
// reach this bound. The early exit is preserved: the scan stops at the first
// size-corroborated match, or as soon as it passes an already-found exact match.
const maxFallbackReleaseScan = 512

// ClassifyCandidates sets SourceConfirmed on each candidate whose release the
// cached Prowlarr snapshot already carries. This is the fallback signal used
// when AltMount is not configured. The snapshot is a durable, operator-visible
// record (persisted to the index file and refreshed by the scheduled monitor),
// so this is not a live lookup and adds no provider round-trip to playback.
//
// The classifier runs on every list and resolve of the same cached listing, so
// it must not be quadratic: candidates x releases with per-pair regexes made a
// 500 x 20000 snapshot cost ~14s per call. It now snapshots the release slice
// (the backing array is immutable once published, so no copy is needed), reuses
// the identity fields prepareProwlarrRelease precomputes on every release, and
// resolves each candidate through an inverted index. Exact release-name identity
// is an O(1) map lookup; the weak title fallback scans only the releases sharing
// the candidate's normalized title, bounded by maxFallbackReleaseScan.
//
// First-match ordering is preserved exactly: the match is the smallest snapshot
// index satisfying either the exact-name or the weak-title predicate, and the
// GUID is filled from that release only when the candidate carries none. A
// candidate's existing (for example AltMount-namespaced) GUID is never
// overwritten.
func (c *prowlarrSearchClient) ClassifyCandidates(candidates []StreamCandidate) {
	if c == nil || len(candidates) == 0 {
		return
	}
	c.mu.Lock()
	// A shallow snapshot is safe: refresh and index load replace c.releases with
	// a new slice and never mutate a published one in place.
	releases := c.releases
	index := c.classifyIndex
	if index == nil || c.classifyIndexGeneration != c.releasesGeneration {
		index = buildProwlarrReleaseIndex(releases)
		c.classifyIndex = index
		c.classifyIndexGeneration = c.releasesGeneration
	}
	c.mu.Unlock()
	if len(releases) == 0 || index == nil {
		return
	}
	for i := range candidates {
		identity := candidateIdentity(candidates[i])
		if identity.nameKey == "" {
			continue
		}
		// Find the earliest release satisfying either confirmation predicate,
		// mirroring the linear first-match scan this replaced.
		matchIndex := -1
		if j, ok := index.byNameKey[identity.nameKey]; ok {
			matchIndex = j
		}
		if matchIndex != 0 && identity.title != "" {
			scanned := 0
			for _, j := range index.byTitle[identity.title] {
				// A weak match can only change the result when it precedes the
				// exact match already found.
				if matchIndex >= 0 && j >= matchIndex {
					break
				}
				if scanned >= maxFallbackReleaseScan {
					break
				}
				scanned++
				if releaseIdentityMatchesCandidate(index.identities[j], identity) {
					matchIndex = j
					break
				}
			}
		}
		if matchIndex < 0 {
			continue
		}
		candidates[i].SourceConfirmed = true
		// AltMount's release-scoped identity (namespaced) is authoritative for a
		// candidate it already confirmed: Prowlarr corroborates the same release,
		// it does not re-identify it. Overwriting would move the row's GUID to a
		// different provider key and break re-matching. Only fill an empty GUID.
		if strings.TrimSpace(candidates[i].SourceGUID) == "" && releases[matchIndex].GUID != "" {
			candidates[i].SourceGUID = releases[matchIndex].GUID
		}
	}
}

// Match returns true when any release in the cached search results
// corresponds to the monitored media. Prefers IMDb/TMDB/TVDB IDs,
// falling back to normalized title+year comparison.
func (c *prowlarrSearchClient) Match(item monitoredMedia) bool {
	return c.MatchWithQuality(item, QualityConfig{})
}

func (c *prowlarrSearchClient) MatchWithQuality(item monitoredMedia, quality QualityConfig) bool {
	c.mu.Lock()
	releases := append([]prowlarrRelease(nil), c.releases...)
	c.mu.Unlock()
	if len(releases) == 0 {
		return false
	}
	return matchProwlarrReleasesWithQuality(releases, item, quality)
}

func (c *prowlarrSearchClient) SearchItem(ctx context.Context, item monitoredMedia) ([]prowlarrRelease, error) {
	return c.search(ctx, item)
}

// SearchMonitoredReleases performs an on-demand Prowlarr search for one
// monitored title and returns only the releases that match it, normalized and
// sorted for display. It is the per-title lookup the virtual-library "Refresh
// List" uses; unlike RefreshIfStale it hits Prowlarr every call and is not
// governed by the cached RSS interval.
//
// A non-nil episode narrows the match to that episode's release keys, so a
// series search does not offer another episode's releases. Quality filtering
// follows the same helpers as the cached Match paths so an operator's profile
// settings mean the same thing here. The caller owns the request context; the
// client's configured indexer search timeout still bounds a single request.
func (c *prowlarrSearchClient) SearchMonitoredReleases(ctx context.Context, item monitoredMedia, episode *virtualEpisode, quality QualityConfig) ([]prowlarrRelease, error) {
	releases, err := c.search(ctx, item)
	if err != nil {
		return nil, err
	}
	matched := make([]prowlarrRelease, 0, len(releases))
	for i := range releases {
		release := releases[i]
		var ok bool
		if episode != nil {
			ok = releaseMatchesEpisodeWithQuality(&release, item, *episode, quality)
		} else {
			ok = releaseMatchesMonitoredWithQuality(&release, item, quality)
		}
		if ok {
			matched = append(matched, release)
		}
	}
	sortProwlarrReleases(matched, quality.CustomFormats)
	return matched, nil
}

func (c *prowlarrSearchClient) MatchEpisodeWithQuality(item monitoredMedia, episode virtualEpisode, quality QualityConfig) bool {
	c.mu.Lock()
	if c.url == "" {
		c.mu.Unlock()
		return false
	}
	releases := append([]prowlarrRelease(nil), c.releases...)
	c.mu.Unlock()
	for i := range releases {
		if releaseMatchesEpisodeWithQuality(&releases[i], item, episode, quality) {
			return true
		}
	}
	return false
}

// releaseMatchesEpisodeWithQuality reports whether one Prowlarr release is the
// given episode of the monitored series, honoring the operator's quality
// profile. It is the per-release form shared by the cached MatchEpisodeWithQuality
// and the on-demand SearchMonitoredReleases, so both apply identical rules.
func releaseMatchesEpisodeWithQuality(release *prowlarrRelease, item monitoredMedia, episode virtualEpisode, quality QualityConfig) bool {
	if release == nil {
		return false
	}
	wantTitle, _ := normalizeReleaseTitle(item.Title)
	if wantTitle == "" {
		return false
	}
	wantIMDb, _ := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(item.IMDbID), "tt"), 10, 64)
	wantTMDB, _ := strconv.ParseInt(item.TMDBID, 10, 64)
	wantTVDB, _ := strconv.ParseInt(item.TVDBID, 10, 64)
	if quality.EnableProfiles && !releaseMatchesQuality(release, quality.Profiles) {
		return false
	}
	if len(quality.CustomFormats) > 0 {
		if release.parsedCandidate == nil {
			prepareProwlarrRelease(release)
		}
		if release.parsedCandidate != nil {
			if _, rejected := customFormatScore(*release.parsedCandidate, quality.CustomFormats); rejected {
				return false
			}
		}
	}
	if wantIMDb > 0 && release.IMDbID > 0 && wantIMDb != release.IMDbID || wantTMDB > 0 && release.TMDBID > 0 && wantTMDB != release.TMDBID || wantTVDB > 0 && release.TVDBID > 0 && wantTVDB != release.TVDBID {
		return false
	}
	if release.normalizedTitle == "" {
		prepareProwlarrRelease(release)
	}
	return (strings.Contains(release.normalizedTitle, wantTitle) || strings.Contains(wantTitle, release.normalizedTitle)) &&
		hasEpisodeReleaseKey(release.episodeKeys, episode.Season, episode.Episode)
}

// releaseMatchesMonitoredWithQuality is the per-release form of
// matchProwlarrReleasesWithQuality, used to filter an on-demand search to the
// monitored title. Keeping both on the same predicate means the on-demand path
// accepts exactly the releases the cached Match path would.
func releaseMatchesMonitoredWithQuality(release *prowlarrRelease, item monitoredMedia, quality QualityConfig) bool {
	if release == nil {
		return false
	}
	return matchProwlarrReleasesWithQuality([]prowlarrRelease{*release}, item, quality)
}

func containsEpisodeReleaseKey(title string, season, episode int) bool {
	return hasEpisodeReleaseKey(episodeReleaseKeys(title), season, episode)
}

func hasEpisodeReleaseKey(keys []episodeReleaseKey, season, episode int) bool {
	for _, key := range keys {
		if key.season == season && key.episode == episode {
			return true
		}
	}
	return false
}

func matchProwlarrReleases(releases []prowlarrRelease, item monitoredMedia) bool {
	return matchProwlarrReleasesWithQuality(releases, item, QualityConfig{})
}

func matchProwlarrReleasesWithQuality(releases []prowlarrRelease, item monitoredMedia, quality QualityConfig) bool {
	wantTitle, titleYear := normalizeReleaseTitle(item.Title)
	wantYear := item.Year
	if wantYear == 0 && titleYear != 0 {
		wantYear = int32(titleYear)
	}
	if wantTitle == "" {
		return false
	}
	wantIMDb, _ := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(item.IMDbID), "tt"), 10, 64)
	wantTMDB, _ := strconv.ParseInt(item.TMDBID, 10, 64)
	wantTVDB, _ := strconv.ParseInt(item.TVDBID, 10, 64)

	for i := range releases {
		r := &releases[i]
		if quality.EnableProfiles && !releaseMatchesQuality(r, quality.Profiles) {
			continue
		}
		if len(quality.CustomFormats) > 0 {
			if r.parsedCandidate == nil {
				prepareProwlarrRelease(r)
			}
			if r.parsedCandidate != nil {
				if _, rejected := customFormatScore(*r.parsedCandidate, quality.CustomFormats); rejected {
					continue
				}
			}
		}
		// Fast path: direct ID match.
		if wantIMDb > 0 && r.IMDbID > 0 && wantIMDb == r.IMDbID {
			return true
		}
		if wantTMDB > 0 && r.TMDBID > 0 && wantTMDB == r.TMDBID {
			return true
		}
		if wantTVDB > 0 && r.TVDBID > 0 && wantTVDB == r.TVDBID {
			return true
		}

		// Fallback: title+year matching.
		if r.normalizedTitle == "" {
			prepareProwlarrRelease(r)
		}
		gotTitle, gotYear := r.normalizedTitle, 0
		if r.normalizedTitle != "" {
			_, gotYear = normalizeReleaseTitle(r.Title)
		}
		if gotTitle == "" {
			continue
		}
		titleMatch := strings.Contains(gotTitle, wantTitle) || strings.Contains(wantTitle, gotTitle)
		if !titleMatch {
			continue
		}
		if wantYear != 0 && gotYear != 0 && int(wantYear) != gotYear {
			continue
		}
		if wantYear == 0 && len(strings.Fields(wantTitle)) < 2 {
			continue
		}
		return true
	}
	return false
}

func releaseMatchesQuality(release *prowlarrRelease, profiles []QualityProfile) bool {
	if release == nil || len(profiles) == 0 {
		return false
	}
	candidate := release.parsedCandidate
	if candidate == nil {
		prepareProwlarrRelease(release)
		candidate = release.parsedCandidate
	}
	if candidate == nil {
		return false
	}
	return slicesContainsFunc(profiles, func(profile QualityProfile) bool {
		return matchProfile(*candidate, profile)
	})
}

func slicesContainsFunc[T any](values []T, predicate func(T) bool) bool {
	for _, value := range values {
		if predicate(value) {
			return true
		}
	}
	return false
}

type scoredProwlarrRelease struct {
	release     prowlarrRelease
	score       int
	rejected    bool
	index       int
	publishTime time.Time
	hasTime     bool
}

func sortProwlarrReleases(releases []prowlarrRelease, formats []CustomFormat) {
	if len(releases) == 0 {
		return
	}
	if len(releases) == 1 {
		if releases[0].parsedCandidate == nil {
			prepareProwlarrRelease(&releases[0])
		}
		if releases[0].parsedCandidate != nil {
			releases[0].parsedCandidate.QualityScore, _ = customFormatScore(*releases[0].parsedCandidate, formats)
		}
		return
	}
	scored := make([]scoredProwlarrRelease, len(releases))
	for i := range releases {
		if releases[i].parsedCandidate == nil {
			prepareProwlarrRelease(&releases[i])
		}
		score, rejected := 0, false
		if releases[i].parsedCandidate != nil {
			score, rejected = customFormatScore(*releases[i].parsedCandidate, formats)
			releases[i].parsedCandidate.QualityScore = score
		}
		pubStr := strings.TrimSpace(releases[i].PublishDate)
		var pubTime time.Time
		var hasTime bool
		if t, err := time.Parse(time.RFC3339, pubStr); err == nil {
			pubTime = t
			hasTime = true
		} else if t, err := time.Parse(time.RFC3339Nano, pubStr); err == nil {
			pubTime = t
			hasTime = true
		}
		scored[i] = scoredProwlarrRelease{
			release:     releases[i],
			score:       score,
			rejected:    rejected,
			index:       i,
			publishTime: pubTime,
			hasTime:     hasTime,
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		s1, s2 := scored[i], scored[j]
		if s1.rejected != s2.rejected {
			return !s1.rejected
		}
		if s1.score != s2.score {
			return s1.score > s2.score
		}
		c1, c2 := s1.release.parsedCandidate, s2.release.parsedCandidate
		if c1 != nil && c2 != nil {
			if r1, r2 := resolutionScore(c1.Resolution), resolutionScore(c2.Resolution); r1 != r2 {
				return r1 > r2
			}
			if src1, src2 := sourceScore(c1.SourceType), sourceScore(c2.SourceType); src1 != src2 {
				return src1 > src2
			}
		} else if (c1 != nil) != (c2 != nil) {
			return c1 != nil
		}
		if s1.hasTime && s2.hasTime {
			if !s1.publishTime.Equal(s2.publishTime) {
				return s1.publishTime.After(s2.publishTime)
			}
		} else if s1.hasTime != s2.hasTime {
			return s1.hasTime
		} else if s1.release.PublishDate != s2.release.PublishDate {
			return s1.release.PublishDate > s2.release.PublishDate
		}
		return s1.index < s2.index
	})
	for i := range scored {
		releases[i] = scored[i].release
	}
}

// Validate performs a one-shot search and returns a human-readable status.
func (c *prowlarrSearchClient) Validate(ctx context.Context) (string, error) {
	searchURL, err := c.searchURL()
	if err != nil {
		return "", err
	}
	req, err := c.newSearchRequest(ctx, searchURL)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	// Dedicated short-timeout client so a stale Prowlarr config can't exhaust
	// the SDK's gRPC deadline during TestConnection.
	validateClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := validateClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("connect to Prowlarr failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", c.httpError(resp, searchURL)
	}
	releases, err := parseProwlarrSearch(io.LimitReader(resp.Body, maxSearchBodyBytes+1))
	if err != nil {
		return "", fmt.Errorf("parse search: %w", err)
	}
	return fmt.Sprintf("Prowlarr search OK: %d recent releases across all indexers", len(releases)), nil
}

// RefreshIfStale refreshes the RSS index when the configured interval has
// elapsed (exported wrapper for the monitor package; logic unchanged).
func (c *prowlarrSearchClient) RefreshIfStale(ctx context.Context) error {
	return c.refreshIfStale(ctx)
}
