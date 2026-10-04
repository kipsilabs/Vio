package collectionutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MDBListRequestTimeout bounds one MDBList fetch when the base client sets no
// timeout of its own. http.DefaultClient never times out, so without it a
// stalled mdblist.com response would hold a sync worker indefinitely.
const MDBListRequestTimeout = 30 * time.Second

// SyncTimeout bounds one scheduled collection sync end to end, so a single
// slow source cannot hold a scheduler worker for the rest of the run.
const SyncTimeout = 15 * time.Minute

// ErrMDBListURL is returned when a caller-supplied list URL is not an
// MDBList list page. Sync fetches that URL with the server's HTTP client, so
// anything else is an SSRF primitive.
var ErrMDBListURL = errors.New("mdblist url must be an https://mdblist.com/lists/... list")

var allowedMDBListHosts = map[string]struct{}{
	"mdblist.com":     {},
	"www.mdblist.com": {},
}

// NormalizeMDBListURL accepts either an MDBList page URL or its JSON variant
// and returns the canonical JSON URL. Trailing slashes and accidental repeated
// /json suffixes are tolerated. It does not validate the host; call
// ValidateMDBListURL (or CanonicalMDBListURL) before fetching.
func NormalizeMDBListURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	raw = strings.TrimRight(raw, "/")
	for strings.HasSuffix(raw, "/json/json") {
		raw = strings.TrimSuffix(raw, "/json")
	}
	if !strings.HasSuffix(raw, "/json") {
		raw += "/json"
	}
	return raw
}

// CanonicalMDBListURL normalizes then allowlists the URL used for MDBList
// JSON fetches.
func CanonicalMDBListURL(raw string) (string, error) {
	normalized := NormalizeMDBListURL(raw)
	if err := ValidateMDBListURL(normalized); err != nil {
		return "", err
	}
	return normalized, nil
}

// ValidateMDBListURL rejects anything that is not an MDBList list page.
// Scheme may be http or https (mdblist redirects http→https); host must be
// mdblist.com or www.mdblist.com; path must be under /lists/.
func ValidateMDBListURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrMDBListURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ErrMDBListURL
	}
	if parsed.User != nil {
		return ErrMDBListURL
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if _, ok := allowedMDBListHosts[host]; !ok {
		return ErrMDBListURL
	}
	if port := parsed.Port(); port != "" && port != "80" && port != "443" {
		return ErrMDBListURL
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = parsed.Path
	}
	if !strings.HasPrefix(path, "/lists/") {
		return ErrMDBListURL
	}
	return nil
}

// ParseMDBListListURL extracts the user and list slug from a canonical MDBList
// list URL of the exact shape
// http(s)://mdblist.com|www.mdblist.com/lists/{user}/{list}[/json].
// It rejects anything else — numeric-only list ids, extra path segments,
// missing parts, query strings, or fragments — so callers never hand the
// authenticated API path an ambiguous identifier.
func ParseMDBListListURL(raw string) (user, list string, ok bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", "", false
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", false
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if _, allowed := allowedMDBListHosts[host]; !allowed {
		return "", "", false
	}
	if port := parsed.Port(); port != "" && port != "80" && port != "443" {
		return "", "", false
	}
	// Split the escaped path so an encoded %2F inside a slug is not treated as
	// a separator, then unescape each segment so callers receive raw slugs and
	// escape exactly once. Using EscapedPath directly would return encoded
	// segments that callers re-escape into double-encoding.
	path := strings.TrimSuffix(parsed.EscapedPath(), "/")
	path = strings.TrimSuffix(path, "/json")
	path = strings.TrimSuffix(path, "/")
	rawParts := strings.Split(path, "/")
	// ["", "lists", user, list]
	if len(rawParts) != 4 || rawParts[0] != "" || rawParts[1] != "lists" {
		return "", "", false
	}
	user, err = url.PathUnescape(rawParts[2])
	if err != nil {
		return "", "", false
	}
	list, err = url.PathUnescape(rawParts[3])
	if err != nil {
		return "", "", false
	}
	if user == "" || list == "" {
		return "", "", false
	}
	if strings.ContainsAny(user, " \t") || strings.ContainsAny(list, " \t") {
		return "", "", false
	}
	// A numeric-only list segment is an internal list id, not a slug; reject
	// it so the authenticated path never guesses at an ambiguous identifier.
	if isAllDigits(list) {
		return "", "", false
	}
	return user, list, true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// MDBListHTTPClient returns a clone of base whose redirects are re-checked
// against ValidateMDBListURL so an mdblist.com 3xx cannot bounce the fetch
// onto loopback or RFC1918. A nil base uses http.DefaultClient.
//
// The clone gets a default 30s timeout when the base has none, so a stalled
// MDBList socket cannot hang a collection sync forever. An explicit non-zero
// base timeout is preserved. http.DefaultClient itself is never mutated.
func MDBListHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	clone := *base
	if clone.Timeout == 0 {
		clone.Timeout = MDBListRequestTimeout
	}
	parentRedirect := base.CheckRedirect
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req == nil || req.URL == nil {
			return ErrMDBListURL
		}
		if err := ValidateMDBListURL(req.URL.String()); err != nil {
			return err
		}
		if parentRedirect != nil {
			return parentRedirect(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &clone
}

// MDBListJSONPageSize is the per-request limit used when paging the public
// MDBList /lists/{user}/{list}/json feed.
const MDBListJSONPageSize = 1000

// maxMDBListJSONResponseBytes bounds each page body read. The 4MiB ceiling is
// per response, not per fetch, so paging a large list still reads every page.
const maxMDBListJSONResponseBytes = 4 << 20

// FetchMDBListJSONPaged fetches MDBList's public JSON list feed, paging past
// the feed's default truncation.
//
// The /json feed returns at most 2000 entries by default. That default is not a
// hard cap: the endpoint accepts undocumented limit/offset query parameters
// (verified empirically against a 4,283-entry list — limit up to the full list
// size, offset to page; page/cursor are ignored). A bare GET, which the client
// used to send, therefore silently truncates any list larger than 2000. This
// helper pages with limit/offset until a page comes back shorter than the
// requested limit (feed exhausted), merging entries in feed order.
//
// hardCap > 0 bounds the total entries returned; otherwise MaxExplicitItemLimit
// applies. Each response's body is bounded by maxMDBListJSONResponseBytes, and
// the fetch stops at hardCap even if the feed ignores the page parameters.
//
// The raw URL is canonicalized with CanonicalMDBListURL, so callers may pass
// either the list page URL or its /json variant. Error semantics match the
// previous single-GET path: non-2xx yields the status, and malformed JSON or a
// read failure is wrapped.
func FetchMDBListJSONPaged[T any](ctx context.Context, client *http.Client, rawURL string, hardCap int) ([]T, error) {
	listURL, err := CanonicalMDBListURL(rawURL)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	if hardCap <= 0 || hardCap > MaxExplicitItemLimit {
		hardCap = MaxExplicitItemLimit
	}

	parsed, err := url.Parse(listURL)
	if err != nil {
		return nil, fmt.Errorf("parsing mdblist url: %w", err)
	}

	entries := make([]T, 0)
	offset := 0
	for len(entries) < hardCap {
		query := parsed.Query()
		query.Set("limit", strconv.Itoa(MDBListJSONPageSize))
		query.Set("offset", strconv.Itoa(offset))
		parsed.RawQuery = query.Encode()

		page, err := fetchMDBListJSONPage[T](ctx, client, parsed.String())
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		entries = append(entries, page...)
		if len(page) < MDBListJSONPageSize {
			break
		}
		offset += len(page)
	}

	if len(entries) > hardCap {
		entries = entries[:hardCap]
	}
	return entries, nil
}

// fetchMDBListJSONPage performs one bounded GET of a canonical /json URL and
// decodes a single page.
func fetchMDBListJSONPage[T any](ctx context.Context, client *http.Client, listURL string) ([]T, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating mdblist request: %w", err)
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching mdblist list: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("mdblist request failed with status %d", res.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, maxMDBListJSONResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("reading mdblist response: %w", err)
	}
	var page []T
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("parsing mdblist response: %w", err)
	}
	return page, nil
}

// FetchMDBListWithFallback tries each candidate URL in order and returns the
// entries from the first successful fetch, recovering when source_config and
// source_url drift. It returns the last error when every candidate fails.
// Callers should guard against an empty url list beforehand; an empty list
// yields a nil result and nil error.
func FetchMDBListWithFallback[T any](urls []string, fetch func(string) ([]T, error)) ([]T, error) {
	var entries []T
	var err error
	for _, url := range urls {
		entries, err = fetch(url)
		if err == nil {
			return entries, nil
		}
	}
	return entries, err
}

// MDBListURLCandidates returns unique canonical JSON URLs, preserving argument
// order. It lets syncers recover when source_config and source_url drift.
func MDBListURLCandidates(urls ...string) []string {
	candidates := make([]string, 0, len(urls))
	seen := make(map[string]struct{}, len(urls))
	for _, url := range urls {
		normalized := NormalizeMDBListURL(url)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		candidates = append(candidates, normalized)
	}
	return candidates
}
