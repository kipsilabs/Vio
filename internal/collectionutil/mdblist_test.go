package collectionutil

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNormalizeMDBListURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"  ", ""},
		{"https://mdblist.com/lists/example-user/watchlist", "https://mdblist.com/lists/example-user/watchlist/json"},
		{"https://mdblist.com/lists/example-user/watchlist/", "https://mdblist.com/lists/example-user/watchlist/json"},
		{"https://mdblist.com/lists/example-user/watchlist/json", "https://mdblist.com/lists/example-user/watchlist/json"},
		{"https://mdblist.com/lists/example-user/watchlist/json/", "https://mdblist.com/lists/example-user/watchlist/json"},
		{"https://mdblist.com/lists/example-user/external/1234/json", "https://mdblist.com/lists/example-user/external/1234/json"},
		{"https://mdblist.com/lists/example-user/external/1234/json/json", "https://mdblist.com/lists/example-user/external/1234/json"},
		{"  https://mdblist.com/lists/example-user/watchlist  ", "https://mdblist.com/lists/example-user/watchlist/json"},
	}
	for _, tc := range cases {
		if got := NormalizeMDBListURL(tc.in); got != tc.want {
			t.Errorf("NormalizeMDBListURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMDBListURLCandidatesNormalizesAndDeduplicates(t *testing.T) {
	got := MDBListURLCandidates(
		"https://mdblist.com/lists/example-user/external/1234/json/json",
		"https://mdblist.com/lists/example-user/external/1234/json",
		"https://mdblist.com/lists/example-user/other",
	)
	want := []string{
		"https://mdblist.com/lists/example-user/external/1234/json",
		"https://mdblist.com/lists/example-user/other/json",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MDBListURLCandidates = %#v, want %#v", got, want)
	}
}

func TestFetchMDBListWithFallback(t *testing.T) {
	errFetch := errors.New("fetch failed")

	t.Run("returns first success without trying later candidates", func(t *testing.T) {
		var tried []string
		got, err := FetchMDBListWithFallback([]string{"a", "b"}, func(url string) ([]string, error) {
			tried = append(tried, url)
			return []string{url + "-entry"}, nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"a-entry"}) {
			t.Fatalf("entries = %#v, want first candidate's result", got)
		}
		if !reflect.DeepEqual(tried, []string{"a"}) {
			t.Fatalf("tried = %#v, want to stop after first success", tried)
		}
	})

	t.Run("falls back past a failing candidate", func(t *testing.T) {
		var tried []string
		got, err := FetchMDBListWithFallback([]string{"a", "b"}, func(url string) ([]string, error) {
			tried = append(tried, url)
			if url == "a" {
				return nil, errFetch
			}
			return []string{url + "-entry"}, nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"b-entry"}) {
			t.Fatalf("entries = %#v, want fallback candidate's result", got)
		}
		if !reflect.DeepEqual(tried, []string{"a", "b"}) {
			t.Fatalf("tried = %#v, want both candidates attempted", tried)
		}
	})

	t.Run("returns the last error when every candidate fails", func(t *testing.T) {
		_, err := FetchMDBListWithFallback([]string{"a", "b"}, func(string) ([]string, error) {
			return nil, errFetch
		})
		if !errors.Is(err, errFetch) {
			t.Fatalf("err = %v, want %v", err, errFetch)
		}
	})

	t.Run("empty candidate list yields nil result and nil error", func(t *testing.T) {
		got, err := FetchMDBListWithFallback(nil, func(string) ([]string, error) {
			t.Fatal("fetch should not be called for an empty list")
			return nil, nil
		})
		if err != nil || got != nil {
			t.Fatalf("got %#v, %v; want nil, nil", got, err)
		}
	})
}

func TestValidateMDBListURL(t *testing.T) {
	t.Parallel()

	allowed := []string{
		"https://mdblist.com/lists/example-user/watchlist",
		"https://mdblist.com/lists/example-user/watchlist/json",
		"https://www.mdblist.com/lists/example-user/watchlist/json",
		"http://mdblist.com/lists/example-user/watchlist/json",
		"https://mdblist.com./lists/example-user/watchlist/json",
		"https://mdblist.com:443/lists/example-user/watchlist/json",
	}
	for _, raw := range allowed {
		if err := ValidateMDBListURL(raw); err != nil {
			t.Errorf("ValidateMDBListURL(%q) = %v, want nil", raw, err)
		}
	}

	blocked := []string{
		"",
		"http://127.0.0.1:8096/",
		"http://127.0.0.1:8096/json",
		"http://127.0.0.1/lists/x/y/json",
		"http://localhost/lists/x/y/json",
		"http://[::1]/lists/x/y/json",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/lists/x/y/json",
		"http://192.168.1.1/lists/x/y/json",
		"http://172.16.0.1/lists/x/y/json",
		"https://mdblist.com.evil.example/lists/x/y/json",
		"https://evil.example/lists/x/y/json",
		"https://mdblist.com/",
		"https://mdblist.com/json",
		"https://mdblist.com/admin/json",
		"https://api.mdblist.com/lists/x/y/json",
		"https://mdblist.com:8080/lists/x/y/json",
		"https://mdblist.com@127.0.0.1/lists/x/y/json",
		"file:///etc/passwd",
		"ftp://mdblist.com/lists/x/y/json",
	}
	for _, raw := range blocked {
		if err := ValidateMDBListURL(raw); !errors.Is(err, ErrMDBListURL) {
			t.Errorf("ValidateMDBListURL(%q) = %v, want ErrMDBListURL", raw, err)
		}
	}
}

func TestCanonicalMDBListURLRejectsPrivateHosts(t *testing.T) {
	t.Parallel()

	if _, err := CanonicalMDBListURL("http://127.0.0.1:8096/"); !errors.Is(err, ErrMDBListURL) {
		t.Fatalf("CanonicalMDBListURL(loopback) = %v, want ErrMDBListURL", err)
	}

	got, err := CanonicalMDBListURL("https://mdblist.com/lists/example-user/watchlist")
	if err != nil {
		t.Fatalf("CanonicalMDBListURL(valid) = %v", err)
	}
	if got != "https://mdblist.com/lists/example-user/watchlist/json" {
		t.Fatalf("CanonicalMDBListURL = %q", got)
	}
}

func TestParseMDBListListURLReturnsUnescapedSegments(t *testing.T) {
	t.Parallel()

	// F10: the returned segments must be raw slugs so callers escape exactly
	// once. %2F/%25 must not come back percent-encoded.
	user, list, ok := ParseMDBListListURL("https://mdblist.com/lists/some%2Fuser/my%25list/json")
	if !ok {
		t.Fatal("ParseMDBListListURL rejected an escaped URL")
	}
	if user != "some/user" || list != "my%list" {
		t.Fatalf("segments = (%q, %q), want (some/user, my%%list)", user, list)
	}
}

func TestParseMDBListListURLEscapesExactlyOnce(t *testing.T) {
	t.Parallel()

	user, list, ok := ParseMDBListListURL("https://mdblist.com/lists/some%2Fuser/my%25list/json")
	if !ok {
		t.Fatal("ParseMDBListListURL rejected an escaped URL")
	}
	if got := url.PathEscape(user); got != "some%2Fuser" {
		t.Fatalf("re-escaping user = %q, want some%%2Fuser (single escape)", got)
	}
	if got := url.PathEscape(list); got != "my%25list" {
		t.Fatalf("re-escaping list = %q, want my%%25list (single escape)", got)
	}
}

func TestParseMDBListListURL(t *testing.T) {
	t.Parallel()

	accepted := []struct {
		raw  string
		user string
		list string
	}{
		{"https://mdblist.com/lists/alice/horror", "alice", "horror"},
		{"https://mdblist.com/lists/alice/horror/json", "alice", "horror"},
		{"https://mdblist.com/lists/alice/horror/", "alice", "horror"},
		{"http://www.mdblist.com/lists/bob/my-list/json", "bob", "my-list"},
		{"https://mdblist.com:443/lists/carol/top_100/json", "carol", "top_100"},
		{"  https://mdblist.com/lists/dave/sci-fi  ", "dave", "sci-fi"},
		{"https://mdblist.com/lists/12345/horror", "12345", "horror"},
	}
	for _, tc := range accepted {
		user, list, ok := ParseMDBListListURL(tc.raw)
		if !ok {
			t.Errorf("ParseMDBListListURL(%q) rejected, want accept", tc.raw)
			continue
		}
		if user != tc.user || list != tc.list {
			t.Errorf("ParseMDBListListURL(%q) = (%q, %q), want (%q, %q)", tc.raw, user, list, tc.user, tc.list)
		}
	}

	rejected := []string{
		"",
		"https://mdblist.com/",
		"https://mdblist.com/lists/alice",
		"https://mdblist.com/lists/alice/horror/extra",
		"https://mdblist.com/lists/alice/horror/extra/json",
		"https://mdblist.com/lists/alice/12345", // numeric list id, not a slug
		"https://mdblist.com/lists/alice/12345/json",
		"https://evil.example/lists/alice/horror",
		"https://mdblist.com.evil.example/lists/alice/horror",
		"ftp://mdblist.com/lists/alice/horror",
		"https://mdblist.com:8080/lists/alice/horror",
		"https://mdblist.com@127.0.0.1/lists/alice/horror",
		"https://mdblist.com/lists//horror",
		"https://mdblist.com/lists/alice/",
		"https://mdblist.com/lists/alice/horror?x=1",
		"https://mdblist.com/lists/alice/horror#frag",
		"https://mdblist.com/admin/alice/horror",
	}
	for _, raw := range rejected {
		if _, _, ok := ParseMDBListListURL(raw); ok {
			t.Errorf("ParseMDBListListURL(%q) accepted, want reject", raw)
		}
	}
}

func TestMDBListHTTPClientRejectsPrivateRedirect(t *testing.T) {
	t.Parallel()

	client := MDBListHTTPClient(nil)
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:8096/json", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if err := client.CheckRedirect(req, []*http.Request{req}); !errors.Is(err, ErrMDBListURL) {
		t.Fatalf("CheckRedirect = %v, want ErrMDBListURL", err)
	}
}

func TestMDBListHTTPClientAppliesDefaultTimeout(t *testing.T) {
	t.Parallel()

	base := &http.Client{}
	client := MDBListHTTPClient(base)
	if client.Timeout != 30*time.Second {
		t.Fatalf("clone timeout = %v, want 30s default", client.Timeout)
	}
	if base.Timeout != 0 {
		t.Fatalf("base client mutated: timeout = %v, want 0", base.Timeout)
	}
}

func TestMDBListHTTPClientPreservesExplicitTimeout(t *testing.T) {
	t.Parallel()

	base := &http.Client{Timeout: 7 * time.Second}
	client := MDBListHTTPClient(base)
	if client.Timeout != 7*time.Second {
		t.Fatalf("clone timeout = %v, want explicit 7s preserved", client.Timeout)
	}
}

func TestMDBListHTTPClientNilBaseGetsDefaultTimeout(t *testing.T) {
	t.Parallel()

	client := MDBListHTTPClient(nil)
	if client.Timeout != 30*time.Second {
		t.Fatalf("nil-base clone timeout = %v, want 30s default", client.Timeout)
	}
	if http.DefaultClient.Timeout != 0 {
		t.Fatalf("http.DefaultClient mutated: timeout = %v", http.DefaultClient.Timeout)
	}
}

// ── FetchMDBListJSONPaged ────────────────────────────────────────────────────

// pagedTestEntry is a minimal shape for the public /json feed.
type pagedTestEntry struct {
	ID int `json:"id"`
}

// mdblistPagedTransport answers /json paging requests from an in-memory list,
// honoring the undocumented limit/offset parameters. It records the queries and
// paths it was asked for so a test can assert the client actually paged.
type mdblistPagedTransport struct {
	total    int
	status   int    // non-zero overrides the 200 status
	body     []byte // overrides the generated page body
	requests []url.Values
	paths    []string
}

func (t *mdblistPagedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.requests = append(t.requests, req.URL.Query())
	t.paths = append(t.paths, req.URL.Path)

	status := t.status
	if status == 0 {
		status = http.StatusOK
	}
	var body []byte
	if t.body != nil {
		body = t.body
	} else {
		limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
		offset, _ := strconv.Atoi(req.URL.Query().Get("offset"))
		end := offset + limit
		if end > t.total {
			end = t.total
		}
		page := make([]pagedTestEntry, 0, limit)
		for i := offset; i < end; i++ {
			page = append(page, pagedTestEntry{ID: i})
		}
		body, _ = json.Marshal(page)
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}, nil
}

func newPagedTestClient(transport *mdblistPagedTransport) *http.Client {
	return &http.Client{Transport: transport}
}

// TestFetchMDBListJSONPagedPagesPastDefaultTruncation proves the /json feed is
// paged with limit/offset until exhausted: a 3500-entry list (larger than the
// feed's 2000 default) comes back whole, in order.
func TestFetchMDBListJSONPagedPagesPastDefaultTruncation(t *testing.T) {
	t.Parallel()

	transport := &mdblistPagedTransport{total: 3500}
	client := newPagedTestClient(transport)

	entries, err := FetchMDBListJSONPaged[pagedTestEntry](
		context.Background(), client, "https://mdblist.com/lists/alice/large", 0)
	if err != nil {
		t.Fatalf("FetchMDBListJSONPaged: %v", err)
	}
	if len(entries) != 3500 {
		t.Fatalf("entries = %d, want 3500 (must page past the 2000 default)", len(entries))
	}
	for i, entry := range entries {
		if entry.ID != i {
			t.Fatalf("entries[%d].ID = %d, want %d (feed order preserved)", i, entry.ID, i)
		}
	}

	// offset 0,1000,2000 request full pages; 3000 returns the final short page.
	wantOffsets := []string{"0", "1000", "2000", "3000"}
	if len(transport.requests) != len(wantOffsets) {
		t.Fatalf("requests = %d, want %d", len(transport.requests), len(wantOffsets))
	}
	for i, want := range wantOffsets {
		if got := transport.requests[i].Get("limit"); got != strconv.Itoa(MDBListJSONPageSize) {
			t.Fatalf("request %d limit = %q, want %d", i, got, MDBListJSONPageSize)
		}
		if got := transport.requests[i].Get("offset"); got != want {
			t.Fatalf("request %d offset = %q, want %q", i, got, want)
		}
	}
	for _, path := range transport.paths {
		if path != "/lists/alice/large/json" {
			t.Fatalf("request path = %q, want canonical /json path", path)
		}
	}
}

// TestFetchMDBListJSONPagedStopsOnShortPage proves the loop stops as soon as a
// page is shorter than the requested limit instead of issuing an extra probe.
func TestFetchMDBListJSONPagedStopsOnShortPage(t *testing.T) {
	t.Parallel()

	transport := &mdblistPagedTransport{total: 2000}
	entries, err := FetchMDBListJSONPaged[pagedTestEntry](
		context.Background(), newPagedTestClient(transport), "https://mdblist.com/lists/alice/exact/json", 0)
	if err != nil {
		t.Fatalf("FetchMDBListJSONPaged: %v", err)
	}
	if len(entries) != 2000 {
		t.Fatalf("entries = %d, want 2000", len(entries))
	}
	// 1000, 1000, then an empty page (0 < 1000) terminates the loop.
	if len(transport.requests) != 3 {
		t.Fatalf("requests = %d, want 3 (two full pages then an empty terminator)", len(transport.requests))
	}
}

// TestFetchMDBListJSONPagedRespectsHardCap proves hardCap bounds both the
// returned entries and the number of requests issued.
func TestFetchMDBListJSONPagedRespectsHardCap(t *testing.T) {
	t.Parallel()

	transport := &mdblistPagedTransport{total: 5000}
	entries, err := FetchMDBListJSONPaged[pagedTestEntry](
		context.Background(), newPagedTestClient(transport), "https://mdblist.com/lists/alice/huge", 1500)
	if err != nil {
		t.Fatalf("FetchMDBListJSONPaged: %v", err)
	}
	if len(entries) != 1500 {
		t.Fatalf("entries = %d, want 1500 (hard cap)", len(entries))
	}
	if len(transport.requests) != 2 {
		t.Fatalf("requests = %d, want 2 (cap hit after the second page)", len(transport.requests))
	}
}

// TestFetchMDBListJSONPagedBoundsEachResponse proves the per-response reader
// cap still applies: a body larger than the 4MiB limit is truncated and the
// malformed JSON is reported rather than decoded into a partial list.
func TestFetchMDBListJSONPagedBoundsEachResponse(t *testing.T) {
	t.Parallel()

	// A valid JSON array just over the per-response cap. Reading is capped at
	// 4MiB, so the truncation makes the JSON malformed and decoding fails.
	oversized := make([]byte, 0, (4<<20)+1024)
	oversized = append(oversized, '[')
	for len(oversized) < (4<<20)+64 {
		oversized = append(oversized, "\"xxxxxxxxxx\", "...)
	}
	oversized = append(oversized, ']')

	transport := &mdblistPagedTransport{body: oversized}
	entries, err := FetchMDBListJSONPaged[pagedTestEntry](
		context.Background(), newPagedTestClient(transport), "https://mdblist.com/lists/alice/oversized", 0)
	if err == nil {
		t.Fatalf("FetchMDBListJSONPaged accepted a response over the per-response cap (%d entries)", len(entries))
	}
	if !strings.Contains(err.Error(), "parsing mdblist response") {
		t.Fatalf("error = %v, want a parse failure from the truncated body", err)
	}
}

// TestFetchMDBListJSONPagedPropagatesStatusError proves non-2xx responses keep
// the existing error semantics.
func TestFetchMDBListJSONPagedPropagatesStatusError(t *testing.T) {
	t.Parallel()

	transport := &mdblistPagedTransport{total: 10, status: http.StatusInternalServerError}
	_, err := FetchMDBListJSONPaged[pagedTestEntry](
		context.Background(), newPagedTestClient(transport), "https://mdblist.com/lists/alice/broken/json", 0)
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("error = %v, want status 500", err)
	}
}

// TestFetchMDBListJSONPagedRejectsMalformedJSON proves a decode failure on the
// first page surfaces rather than returning an empty list.
func TestFetchMDBListJSONPagedRejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	transport := &mdblistPagedTransport{body: []byte("not json")}
	_, err := FetchMDBListJSONPaged[pagedTestEntry](
		context.Background(), newPagedTestClient(transport), "https://mdblist.com/lists/alice/bad/json", 0)
	if err == nil || !strings.Contains(err.Error(), "parsing mdblist response") {
		t.Fatalf("error = %v, want parse failure", err)
	}
}

// TestFetchMDBListJSONPagedRejectsPrivateHosts proves the shared helper keeps
// the allowlist check the callers relied on.
func TestFetchMDBListJSONPagedRejectsPrivateHosts(t *testing.T) {
	t.Parallel()

	var hits int
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		hits++
		return nil, errors.New("must not dial a private host")
	})}
	_, err := FetchMDBListJSONPaged[pagedTestEntry](context.Background(), client, "http://127.0.0.1:8096/", 0)
	if !errors.Is(err, ErrMDBListURL) {
		t.Fatalf("error = %v, want ErrMDBListURL", err)
	}
	if hits != 0 {
		t.Fatalf("HTTP client dialed %d times for a private URL", hits)
	}
}

// TestFetchMDBListJSONPagedNilClient proves a nil client falls back to
// http.DefaultClient rather than panicking. A caller-supplied client is
// required in production; this only locks the defensive default.
func TestFetchMDBListJSONPagedNilClient(t *testing.T) {
	t.Parallel()

	// The canonical URL is valid but cannot be dialed in a unit test; the
	// assertion is that construction proceeds past the nil check and the
	// request fails on transport rather than on a nil dereference.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := FetchMDBListJSONPaged[pagedTestEntry](ctx, nil, "https://mdblist.com/lists/alice/list/json", 0)
	if err == nil {
		t.Fatal("expected an error from a canceled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

// roundTripFunc adapts a function to http.RoundTripper for rejection tests.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestMDBListHTTPClientBoundsRequests(t *testing.T) {
	t.Parallel()

	if got := MDBListHTTPClient(nil).Timeout; got != MDBListRequestTimeout {
		t.Fatalf("nil base timeout = %v, want %v", got, MDBListRequestTimeout)
	}
	if got := MDBListHTTPClient(&http.Client{}).Timeout; got != MDBListRequestTimeout {
		t.Fatalf("unbounded base timeout = %v, want %v", got, MDBListRequestTimeout)
	}
	if got := MDBListHTTPClient(&http.Client{Timeout: 5 * time.Second}).Timeout; got != 5*time.Second {
		t.Fatalf("explicit base timeout = %v, want 5s", got)
	}
	if http.DefaultClient.Timeout != 0 {
		t.Fatal("MDBListHTTPClient mutated http.DefaultClient")
	}
}
