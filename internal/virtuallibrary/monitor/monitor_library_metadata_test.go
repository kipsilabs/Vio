package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// legacyProviderServer stands in for the public metadata providers, so the
// fallback path can be exercised without reaching the network.
func legacyProviderServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/lookup/shows":
			_ = json.NewEncoder(response).Encode(map[string]any{
				"id":   1,
				"name": "Series",
			})
		case request.URL.Path == "/shows/1/episodes":
			_ = json.NewEncoder(response).Encode([]map[string]any{{
				"id":       1,
				"name":     "Pilot",
				"season":   1,
				"number":   1,
				"airstamp": "2000-01-01T00:00:00Z",
			}})
		default:
			_ = json.NewEncoder(response).Encode(map[string]any{
				"meta": map[string]any{
					"name":        "Series",
					"description": "Overview",
					"runtime":     "30 min",
					"videos": []map[string]any{{
						"title":    "Pilot",
						"season":   1,
						"episode":  1,
						"released": "2000-01-01T00:00:00Z",
					}},
				},
			})
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func withLegacyProviders(t *testing.T, server *httptest.Server) {
	t.Helper()
	previousCinemeta, previousTVMaze := cinemetaBaseURL, tvmazeBaseURL
	cinemetaBaseURL, tvmazeBaseURL = server.URL, server.URL
	t.Cleanup(func() {
		cinemetaBaseURL, tvmazeBaseURL = previousCinemeta, previousTVMaze
	})
}

// TestEvaluateFallsBackWhenLibraryMetadataFails covers the degraded policy:
// the library metadata path is preferred, but a request that it cannot answer
// still goes through the public providers instead of failing outright.
func TestEvaluateFallsBackWhenLibraryMetadataFails(t *testing.T) {
	withLegacyProviders(t, legacyProviderServer(t))
	m := newMediaMonitor(nil, slog.New(slog.DiscardHandler))
	called := 0
	m.metadataResolver = func(context.Context, MonitoredMedia) (MonitoredMedia, error) {
		called++
		return MonitoredMedia{}, errors.New("provider chain is unavailable")
	}
	item := monitoredMedia{Key: "series:imdb:tt1", MediaType: "series", Title: "Series", IMDbID: "tt1"}

	updated, message, err := m.evaluate(context.Background(), item)

	if called != 1 {
		t.Fatalf("metadata resolver calls = %d, want 1", called)
	}
	if err != nil {
		t.Fatalf("evaluate error = %v, want the fallback to carry the item", err)
	}
	if message == "Library metadata unavailable; monitoring will retry" {
		t.Fatal("the library metadata failure aborted the evaluation")
	}
	// The public providers answered, so the series carries their episodes.
	if len(updated.Episodes) == 0 {
		t.Fatal("the fallback did not fetch the series episodes")
	}
	if updated.Title != "Series" {
		t.Fatalf("title = %q, want the fallback provider's title", updated.Title)
	}
}

// TestEvaluateReportsUnconfiguredLibraryMetadata covers the configuration
// failure: the monitor still falls back, so a request that used to work keeps
// working, but the failure is not reported as a refused request.
func TestEvaluateReportsUnconfiguredLibraryMetadata(t *testing.T) {
	withLegacyProviders(t, legacyProviderServer(t))
	m := newMediaMonitor(nil, slog.New(slog.DiscardHandler))
	m.metadataResolver = func(context.Context, MonitoredMedia) (MonitoredMedia, error) {
		return MonitoredMedia{}, ErrMetadataNotConfigured
	}
	item := monitoredMedia{Key: "series:imdb:tt1", MediaType: "series", Title: "Series", IMDbID: "tt1"}

	updated, _, err := m.evaluate(context.Background(), item)

	if errors.Is(err, ErrMetadataNotConfigured) {
		t.Fatal("the configuration failure aborted the evaluation")
	}
	if len(updated.Episodes) == 0 {
		t.Fatal("the fallback did not fetch the series episodes")
	}
}

// TestFulfillEvaluatesAgainstTheConnectionLibrary covers the destination
// library: the connection names the library the media is registered in, so
// the metadata lookup has to run against that library and not against the
// service-wide default.
func TestFulfillEvaluatesAgainstTheConnectionLibrary(t *testing.T) {
	monitor := New(nil, slog.New(slog.DiscardHandler), nil)
	if err := monitor.Configure(Config{
		File:              filepath.Join(t.TempDir(), "queue.json"),
		ProwlarrIndexFile: filepath.Join(t.TempDir(), "index.json"),
	}); err != nil {
		t.Fatalf("configure monitor: %v", err)
	}
	var seenFolder int
	monitor.SetMetadataResolver(func(_ context.Context, item MonitoredMedia) (MonitoredMedia, error) {
		seenFolder = item.MediaFolderID
		return MonitoredMedia{
			Key: item.Key, MediaType: item.MediaType, Title: item.Title,
			MediaFolderID: item.MediaFolderID, Ready: false,
		}, nil
	})

	response, err := monitor.Fulfill(context.Background(), &FulfillRequest{
		Request: &RequestDescriptor{
			MediaType:   "movie",
			Title:       "Movie",
			ExternalIds: map[string]string{"imdb": "tt1"},
		},
		Qualities: []*RequestedQuality{{ID: "original"}},
		Connections: []*RouterConnection{{
			ID:     "conn-1",
			Config: map[string]any{"media_folder_id": "42"},
		}},
	})
	if err != nil {
		t.Fatalf("fulfill: %v", err)
	}
	if seenFolder != 42 {
		t.Fatalf("metadata lookup library = %d, want 42", seenFolder)
	}
	if response == nil || len(response.Targets) == 0 {
		t.Fatal("fulfill returned no targets")
	}
}

// TestFulfillRejectsAnInvalidConnectionLibrary covers a connection that names
// no usable library: the request is refused before any metadata lookup runs.
func TestFulfillRejectsAnInvalidConnectionLibrary(t *testing.T) {
	monitor := New(nil, slog.New(slog.DiscardHandler), nil)
	called := 0
	monitor.SetMetadataResolver(func(context.Context, MonitoredMedia) (MonitoredMedia, error) {
		called++
		return MonitoredMedia{}, nil
	})

	_, err := monitor.Fulfill(context.Background(), &FulfillRequest{
		Request: &RequestDescriptor{
			MediaType:   "movie",
			Title:       "Movie",
			ExternalIds: map[string]string{"imdb": "tt1"},
		},
		Qualities: []*RequestedQuality{{ID: "original"}},
		Connections: []*RouterConnection{{
			ID:     "conn-1",
			Config: map[string]any{"media_folder_id": "not-a-number"},
		}},
	})
	if err == nil {
		t.Fatal("an invalid library id must be refused")
	}
	if called != 0 {
		t.Fatalf("metadata resolver calls = %d, want 0", called)
	}
}
