package prowlarr

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// slowServer answers only after delay, so a short caller deadline fires in
// client.Do and the transport error reaches the caller.
func slowServer(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "[]")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// isTimeoutError reports a request timeout either via errors.Is on the wrapped
// context deadline or the net.Error Timeout method.
func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr interface{ Timeout() bool }
	return errors.As(err, &netErr) && netErr.Timeout()
}

// TestConfiguredTimeoutBoundsSearch proves a 5s configured timeout aborts a
// Prowlarr search that has not answered, without any caller deadline.
func TestConfiguredTimeoutBoundsSearch(t *testing.T) {
	srv := slowServer(t, 25*time.Second)
	c := NewSearchClient(nil)
	c.Configure(srv.URL, "", 15, 5)

	start := time.Now()
	_, err := c.search(context.Background(), monitoredMedia{Title: "The Show"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("search: expected a timeout error")
	}
	if !isTimeoutError(err) {
		t.Errorf("search error %v is not a timeout", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("search took %v; the 5s configured timeout did not bound it", elapsed)
	}
}

// TestConfiguredTimeoutAllowsSlowSuccess proves a raised timeout lets a slow
// aggregation succeed: a 30s timeout accepts a server that takes 1s.
func TestConfiguredTimeoutAllowsSlowSuccess(t *testing.T) {
	srv := slowServer(t, time.Second)
	c := NewSearchClient(nil)
	c.Configure(srv.URL, "", 15, 30)

	releases, err := c.search(context.Background(), monitoredMedia{Title: "The Show"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if releases == nil {
		t.Fatal("search returned nil releases for an empty [] body")
	}
}

// TestConfigureClampsTimeout locks the fallback for out-of-range values to the
// same bounds the admin-setting validator enforces.
func TestConfigureClampsTimeout(t *testing.T) {
	cases := []struct {
		in   int
		want time.Duration
	}{
		{5, 5 * time.Second},
		{900, 900 * time.Second},
		{4, defaultSearchTimeoutSeconds * time.Second},
		{901, defaultSearchTimeoutSeconds * time.Second},
		{0, defaultSearchTimeoutSeconds * time.Second},
	}
	for _, tc := range cases {
		c := NewSearchClient(nil)
		c.Configure("http://prowlarr:9696", "key", 15, tc.in)
		c.mu.Lock()
		got := c.timeout
		c.mu.Unlock()
		if got != tc.want {
			t.Errorf("Configure(timeout=%d) timeout = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSearchTransportErrorPreservesCause(t *testing.T) {
	srv := slowServer(t, 500*time.Millisecond)
	c := NewSearchClient(newRestrictedRedirectHTTPClient(0))
	c.Configure(srv.URL, "", 15, defaultSearchTimeoutSeconds)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.search(ctx, monitoredMedia{Title: "The Show"})
	if err == nil {
		t.Fatal("search: expected a transport error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("search error %v does not wrap context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "Prowlarr search request failed") {
		t.Errorf("search error %q lost the generic summary", err)
	}
}

func TestRefreshTransportErrorPreservesCause(t *testing.T) {
	srv := slowServer(t, 500*time.Millisecond)
	c := NewSearchClient(newRestrictedRedirectHTTPClient(0))
	c.Configure(srv.URL, "", 15, defaultSearchTimeoutSeconds)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := c.refresh(ctx)
	if err == nil {
		t.Fatal("refresh: expected a transport error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("refresh error %v does not wrap context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "Prowlarr search request failed") {
		t.Errorf("refresh error %q lost the generic summary", err)
	}
	c.mu.Lock()
	lastErr := c.lastErr
	c.mu.Unlock()
	if !errors.Is(lastErr, context.DeadlineExceeded) {
		t.Errorf("recorded lastErr %v does not wrap context.DeadlineExceeded", lastErr)
	}
}

func configureTestClient(t *testing.T, baseURL, apiKey string) *prowlarrSearchClient {
	t.Helper()
	c := NewSearchClient(nil)
	c.Configure(baseURL, apiKey, 15, defaultSearchTimeoutSeconds)
	return c
}

// errorBodyServer answers every request with the given status and body.
func errorBodyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSearchErrorIncludesStatusBodyAndURL(t *testing.T) {
	srv := errorBodyServer(t, http.StatusBadRequest, `{"message":"No such function (t=movie)"}`)
	c := configureTestClient(t, srv.URL, "an-api-key")

	_, err := c.search(context.Background(), monitoredMedia{Title: "The Show"})
	if err == nil {
		t.Fatal("search: expected an error for HTTP 400")
	}
	msg := err.Error()
	for _, want := range []string{"400", "No such function", "/api/v1/search", "limit=1000", "query=The+Show"} {
		if !strings.Contains(msg, want) {
			t.Errorf("search error %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "an-api-key") {
		t.Errorf("search error leaked the API key: %q", msg)
	}
}

func TestRefreshErrorIncludesStatusBodyAndURL(t *testing.T) {
	srv := errorBodyServer(t, http.StatusBadRequest, `{"message":"No such function (t=tvsearch)"}`)
	c := configureTestClient(t, srv.URL, "an-api-key")

	err := c.refresh(context.Background())
	if err == nil {
		t.Fatal("refresh: expected an error for HTTP 400")
	}
	msg := err.Error()
	for _, want := range []string{"400", "No such function", "/api/v1/search"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refresh error %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "an-api-key") {
		t.Errorf("refresh error leaked the API key: %q", msg)
	}
	c.mu.Lock()
	lastErr := c.lastErr
	c.mu.Unlock()
	if lastErr == nil {
		t.Error("refresh did not record lastErr")
	}
}

func TestValidateErrorIncludesStatusBodyAndURL(t *testing.T) {
	srv := errorBodyServer(t, http.StatusBadRequest, `{"message":"No such function (t=movie)"}`)
	c := configureTestClient(t, srv.URL, "an-api-key")

	_, err := c.Validate(context.Background())
	if err == nil {
		t.Fatal("Validate: expected an error for HTTP 400")
	}
	msg := err.Error()
	for _, want := range []string{"400", "No such function", "/api/v1/search"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Validate error %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "an-api-key") {
		t.Errorf("Validate error leaked the API key: %q", msg)
	}
}

func TestErrorBodyIsTruncatedSafely(t *testing.T) {
	srv := errorBodyServer(t, http.StatusBadRequest, strings.Repeat("A", maxErrorBodyBytes*4)+"TAIL-MARKER")
	c := configureTestClient(t, srv.URL, "")

	_, err := c.search(context.Background(), monitoredMedia{Title: "The Show"})
	if err == nil {
		t.Fatal("search: expected an error for HTTP 400")
	}
	msg := err.Error()
	if !strings.Contains(msg, "(truncated)") {
		t.Errorf("error %q missing truncation marker", msg)
	}
	if strings.Contains(msg, "TAIL-MARKER") {
		t.Errorf("error quoted body past the %d-byte cap", maxErrorBodyBytes)
	}
	if len(msg) > maxErrorBodyBytes+512 {
		t.Errorf("error length %d exceeds the bounded snippet", len(msg))
	}
}

func TestErrorBodyRedactsAPIKey(t *testing.T) {
	const apiKey = "sup3r-secret-key"
	srv := errorBodyServer(t, http.StatusBadRequest, `{"message":"rejected key `+apiKey+`"}`)
	c := configureTestClient(t, srv.URL, apiKey)

	_, err := c.search(context.Background(), monitoredMedia{Title: "The Show"})
	if err == nil {
		t.Fatal("search: expected an error for HTTP 400")
	}
	msg := err.Error()
	if strings.Contains(msg, apiKey) {
		t.Errorf("error leaked the API key: %q", msg)
	}
	if !strings.Contains(msg, "[redacted]") {
		t.Errorf("error %q missing redaction marker", msg)
	}
}

func TestUnauthorizedStaysDistinguishable(t *testing.T) {
	srv := errorBodyServer(t, http.StatusUnauthorized, `{"message":"unauthorized"}`)
	c := configureTestClient(t, srv.URL, "bad-key")

	_, err := c.search(context.Background(), monitoredMedia{Title: "The Show"})
	if err == nil {
		t.Fatal("search: expected an error for HTTP 401")
	}
	msg := err.Error()
	if !strings.Contains(msg, "HTTP 401") {
		t.Errorf("error %q does not name HTTP 401", msg)
	}
	if strings.Contains(msg, "HTTP 400") {
		t.Errorf("error %q is indistinguishable from a 400", msg)
	}
	if !strings.Contains(msg, "unauthorized") {
		t.Errorf("error %q missing the response body", msg)
	}
}

func TestSearchURLKeepsBaseAndAppendsEndpoint(t *testing.T) {
	c := configureTestClient(t, "http://prowlarr:9696", "key")

	got, err := c.searchURLForQuery("")
	if err != nil {
		t.Fatalf("searchURLForQuery: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.Path != "/api/v1/search" {
		t.Errorf("path = %q, want /api/v1/search", u.Path)
	}
	if q := u.Query(); q.Get("t") != "" || q.Get("query") != "" {
		t.Errorf("unexpected query parameters: %q", u.RawQuery)
	}
	if cats := u.Query()["categories"]; len(cats) != 2 || cats[0] != "2000" || cats[1] != "5000" {
		t.Errorf("categories = %v, want [2000 5000]", cats)
	}

	got, err = c.searchURLForQuery("The Show")
	if err != nil {
		t.Fatalf("searchURLForQuery(query): %v", err)
	}
	u, err = url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.Query().Get("query") != "The Show" {
		t.Errorf("query = %q, want The Show", u.Query().Get("query"))
	}
}

func TestSearchURLKeepsReverseProxyBasePath(t *testing.T) {
	c := configureTestClient(t, "http://host/prowlarr", "key")

	got, err := c.searchURLForQuery("")
	if err != nil {
		t.Fatalf("searchURLForQuery: %v", err)
	}
	if !strings.HasPrefix(got, "http://host/prowlarr/api/v1/search?") {
		t.Errorf("URL = %q, want a /prowlarr/api/v1/search path", got)
	}
}

func TestSearchURLRejectsIndexerPathOrQuery(t *testing.T) {
	cases := map[string]string{
		"hint URL":          "http://prowlarr:9696/1/api/v1/search?t=movie",
		"bare indexer id":   "http://prowlarr:9696/1",
		"trailing slash id": "http://prowlarr:9696/1/",
		"search path":       "http://prowlarr:9696/api/v1/search",
		"newznab path":      "http://prowlarr:9696/newznab",
		"query only":        "http://prowlarr:9696?t=movie",
	}
	for name, base := range cases {
		t.Run(name, func(t *testing.T) {
			c := configureTestClient(t, base, "key")
			_, err := c.searchURLForQuery("")
			if err == nil {
				t.Fatalf("searchURLForQuery(%q): expected a configuration error", base)
			}
			if !strings.Contains(err.Error(), "base URL") {
				t.Errorf("error %q does not name the expected base URL shape", err)
			}
		})
	}
}

func TestSearchURLRejectsMissingScheme(t *testing.T) {
	for _, base := range []string{"prowlarr:9696", "localhost:9696", "//prowlarr:9696"} {
		t.Run(base, func(t *testing.T) {
			c := configureTestClient(t, base, "key")
			_, err := c.searchURLForQuery("")
			if err == nil {
				t.Fatalf("searchURLForQuery(%q): expected a configuration error", base)
			}
			if !strings.Contains(err.Error(), "scheme") {
				t.Errorf("error %q does not name the missing scheme", err)
			}
		})
	}
}

// TestNormalizeBaseURL proves the config-time canonicalization: a scheme-less
// host like "one.vio" becomes an http URL the search client can use, a working
// URL is trimmed, and a still-invalid shape is rejected so the caller can leave
// Prowlarr unconfigured instead of failing every staleness refresh.
func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "scheme-less host", raw: "one.vio", want: "http://one.vio"},
		{name: "scheme-less host and port", raw: "prowlarr:9696", want: "http://prowlarr:9696"},
		{name: "already schemed", raw: "https://prowlarr.example.com", want: "https://prowlarr.example.com"},
		{name: "trailing slash", raw: "http://prowlarr:9696/", want: "http://prowlarr:9696"},
		{name: "reverse proxy base path", raw: "http://host/prowlarr", want: "http://host/prowlarr"},
		{name: "empty", raw: "", want: ""},
		{name: "indexer path", raw: "one.vio/1", wantErr: true},
		{name: "search path", raw: "one.vio/api/v1/search", wantErr: true},
		{name: "query string", raw: "one.vio?t=movie", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeBaseURL(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NormalizeBaseURL(%q) = %q, want an error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeBaseURL(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeBaseURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
