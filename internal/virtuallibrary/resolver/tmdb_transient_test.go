package resolver

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// TestTMDBTransientLookupFailureIsNotNegativeCached pins that a TMDB outage is
// not remembered as "no IMDb id": the failure is reported as the retryable
// provider condition, and the next resolve retries the translation instead of
// failing for the negative TTL with a misleading message.
func TestTMDBTransientLookupFailureIsNotNegativeCached(t *testing.T) {
	healthy := false
	tmdb := newTMDBStub(t, func(w http.ResponseWriter, r *http.Request) {
		if !healthy {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"imdb_id":"tt83533"}`))
	})
	answer := []StreamCandidate{{URL: "https://cdn.example/one.mkv", Name: "One.Release.1080p"}}
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) { writeStreams(w, answer) })
	cfg := testConfig(p)
	cfg.TMDBAPIKey = "testkey"
	cfg.TMDBBaseURL = tmdb.server.URL
	r := New(cfg)

	_, _, _, err := r.GetCandidates(context.Background(), "virtual://movie/tmdb:83533")
	if !errors.Is(err, ErrTMDBExternalIDLookupFailed) {
		t.Fatalf("transient TMDB lookup error = %v, want ErrTMDBExternalIDLookupFailed", err)
	}
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("transient TMDB lookup error = %v, want it to wrap ErrProviderUnavailable", err)
	}

	healthy = true
	got, _, _, err := r.GetCandidatesFreshUnbounded(context.Background(), "virtual://movie/tmdb:83533")
	if err != nil || len(got) != 1 {
		t.Fatalf("resolve after TMDB recovery: count=%d err=%v, want 1 and no negative cache", len(got), err)
	}
	if requests := tmdb.requests(); requests != 2 {
		t.Fatalf("TMDB requests = %d, want the recovered resolve to retry the lookup", requests)
	}
}
