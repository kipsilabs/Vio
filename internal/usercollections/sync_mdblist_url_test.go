package usercollections

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Silo-Server/silo-server/internal/collectionutil"
)

type countingRoundTripper struct {
	hits atomic.Int32
}

func (t *countingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	t.hits.Add(1)
	return nil, errors.New("HTTP client must not be used for a rejected MDBList URL")
}

// roundTripFunc adapts a function to http.RoundTripper for the paging test.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestFetchMDBListEntriesDoesNotDialPrivateHosts(t *testing.T) {
	t.Parallel()

	transport := &countingRoundTripper{}
	svc := NewService(nil, nil, nil, nil, &http.Client{Transport: transport}, slog.New(slog.DiscardHandler))

	_, err := svc.fetchMDBListEntries(context.Background(), "http://127.0.0.1:8096/", 0)
	if !errors.Is(err, collectionutil.ErrMDBListURL) {
		t.Fatalf("fetchMDBListEntries(loopback) = %v, want ErrMDBListURL", err)
	}
	if transport.hits.Load() != 0 {
		t.Fatalf("HTTP client was used %d times for a private URL", transport.hits.Load())
	}

	_, err = svc.fetchMDBListEntries(context.Background(), "http://169.254.169.254/latest/meta-data/", 0)
	if !errors.Is(err, collectionutil.ErrMDBListURL) {
		t.Fatalf("fetchMDBListEntries(link-local) = %v, want ErrMDBListURL", err)
	}
	if transport.hits.Load() != 0 {
		t.Fatalf("HTTP client was used %d times for a private URL", transport.hits.Load())
	}
}

func TestCanonicalMDBListURLRejectsPrivateHosts(t *testing.T) {
	t.Parallel()

	if _, err := CanonicalMDBListURL("http://10.0.0.1/lists/x/y"); !errors.Is(err, collectionutil.ErrMDBListURL) {
		t.Fatalf("CanonicalMDBListURL(rfc1918) = %v, want ErrMDBListURL", err)
	}
	got, err := CanonicalMDBListURL("https://mdblist.com/lists/example-user/watchlist")
	if err != nil {
		t.Fatalf("CanonicalMDBListURL(valid) = %v", err)
	}
	if !strings.HasSuffix(got, "/json") {
		t.Fatalf("canonical URL = %q, want /json suffix", got)
	}
}

// TestFetchMDBListEntriesPagesPastDefaultTruncation proves the user-collection
// sync path also pages the public /json feed instead of truncating at 2000.
func TestFetchMDBListEntriesPagesPastDefaultTruncation(t *testing.T) {
	t.Parallel()

	const total = 2500
	var offsets []string
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		query := req.URL.Query()
		offsets = append(offsets, query.Get("offset"))
		offset, _ := strconv.Atoi(query.Get("offset"))
		limit, _ := strconv.Atoi(query.Get("limit"))
		end := offset + limit
		if end > total {
			end = total
		}
		var b strings.Builder
		b.WriteByte('[')
		for i := offset; i < end; i++ {
			if i > offset {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"id":%d,"title":"Item %d","mediatype":"movie","release_year":2000}`, i, i)
		}
		b.WriteByte(']')
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(b.String())),
			Header:     make(http.Header),
		}, nil
	})

	svc := NewService(nil, nil, nil, nil, &http.Client{Transport: transport}, slog.New(slog.DiscardHandler))
	entries, err := svc.fetchMDBListEntries(context.Background(), "https://mdblist.com/lists/alice/large/json", 0)
	if err != nil {
		t.Fatalf("fetchMDBListEntries: %v", err)
	}
	if len(entries) != total {
		t.Fatalf("entries = %d, want %d (must page past 2000)", len(entries), total)
	}
	if got := strings.Join(offsets, ","); got != "0,1000,2000" {
		t.Fatalf("offsets = %s, want 0,1000,2000", got)
	}
}
