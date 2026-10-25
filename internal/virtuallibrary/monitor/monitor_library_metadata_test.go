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
	"time"
)

// legacyProviderServer stands in for the public metadata providers, so the
// fallback path can be exercised without reaching the network.
func legacyProviderServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/lookup/shows":
			_ = json.NewEncoder(response).Encode(map[string]any{
				"id":   1,
				"name": "Series",
			})
		case "/shows/1/episodes":
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

// TestEvaluateFallsBackWhenLibraryMetadataHasNoEpisodes covers when the
// library metadata resolver succeeds but returns zero episodes for a series:
// the public fallback must supply the episodes.
func TestEvaluateFallsBackWhenLibraryMetadataHasNoEpisodes(t *testing.T) {
	withLegacyProviders(t, legacyProviderServer(t))
	m := newMediaMonitor(nil, slog.New(slog.DiscardHandler))
	called := 0
	m.metadataResolver = func(_ context.Context, item MonitoredMedia) (MonitoredMedia, error) {
		called++
		item.Title = "Library Title"
		item.Episodes = nil
		return item, nil
	}
	item := monitoredMedia{Key: "series:imdb:tt1", MediaType: "series", IMDbID: "tt1"}

	updated, _, err := m.evaluate(context.Background(), item)
	if called != 1 {
		t.Fatalf("metadata resolver calls = %d, want 1", called)
	}
	if err != nil {
		t.Fatalf("evaluate error = %v", err)
	}
	if updated.Title != "Library Title" {
		t.Fatalf("title = %q, want library title preserved", updated.Title)
	}
	if len(updated.Episodes) == 0 {
		t.Fatal("fallback did not supply episodes when library had none")
	}
}

// TestEvaluateKeepsSameSourcePartialProgressOnRetryableFailure covers a
// provider timeout during a frozen-source traversal: the episodes and cursor
// already accumulated from that same source must survive the deferred retry,
// while the fallback public providers stay out of a frozen request.
func TestEvaluateKeepsSameSourcePartialProgressOnRetryableFailure(t *testing.T) {
	m := newMediaMonitor(nil, slog.New(slog.DiscardHandler))
	m.metadataResolver = func(_ context.Context, item MonitoredMedia) (MonitoredMedia, error) {
		item.Episodes = []VirtualEpisode{{Season: 1, Episode: 1, Title: "Pilot"}}
		item.MetadataNextSeason = 1
		return item, errors.New("tvdb timeout")
	}
	item := monitoredMedia{
		Key: "series:tvdb:1", MediaType: "series", TVDBID: "1", MetadataSource: "tvdb",
	}

	updated, message, err := m.evaluate(context.Background(), item)
	if err == nil {
		t.Fatal("a frozen source failure must defer the item, not fall back")
	}
	if message != selectedSourceUnavailableMessage {
		t.Fatalf("message = %q, want the retry message", message)
	}
	if len(updated.Episodes) != 1 || updated.Episodes[0].Title != "Pilot" {
		t.Fatalf("episodes = %+v, want the same-source partial progress preserved", updated.Episodes)
	}
	if updated.MetadataNextSeason != 1 {
		t.Fatalf("metadata next season = %d, want the preserved cursor 1", updated.MetadataNextSeason)
	}
}

// TestFulfillEvaluatesAgainstTheConnectionLibrary covers the destination
// library: the connection names the library the media is registered in, so
// the metadata lookup has to run against that library and not against the
// service-wide default.
func TestFulfillEvaluatesAgainstTheConnectionLibrary(t *testing.T) {
	withLegacyProviders(t, legacyProviderServer(t))
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
		item.Title = "Movie"
		return item, nil
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

// TestCheckStatusToleratesTransientEvaluationError covers ordinary evaluation
// errors in CheckStatus: they leave the target queued without failing the RPC.
func TestCheckStatusToleratesTransientEvaluationError(t *testing.T) {
	monitor := New(nil, slog.New(slog.DiscardHandler), nil)
	if err := monitor.Configure(Config{
		File:              filepath.Join(t.TempDir(), "queue.json"),
		ProwlarrIndexFile: filepath.Join(t.TempDir(), "index.json"),
	}); err != nil {
		t.Fatalf("configure monitor: %v", err)
	}
	monitor.SetMetadataResolver(func(context.Context, MonitoredMedia) (MonitoredMedia, error) {
		return MonitoredMedia{}, errors.New("transient provider error")
	})

	resp, err := monitor.CheckStatus(context.Background(), &CheckStatusRequest{
		Request: &RequestDescriptor{
			MediaType:   "movie",
			Title:       "Test Movie",
			ExternalIds: map[string]string{"imdb": "tt12345"},
		},
		Targets: []*TargetRef{{
			Quality:      "original",
			ConnectionId: "conn-1",
		}},
		Connections: []*RouterConnection{{
			ID:     "conn-1",
			Config: map[string]any{"media_folder_id": "1"},
		}},
	})
	if err != nil {
		t.Fatalf("CheckStatus failed: %v", err)
	}
	if len(resp.Statuses) != 1 {
		t.Fatalf("statuses count = %d, want 1", len(resp.Statuses))
	}
	if resp.Statuses[0].Status != "queued" {
		t.Fatalf("status = %q, want queued", resp.Statuses[0].Status)
	}
}

// TestCheckStatusPropagatesDeadlineExpiry covers when the evaluation context
// actually expires: that deadline error is propagated.
func TestCheckStatusPropagatesDeadlineExpiry(t *testing.T) {
	monitor := New(nil, slog.New(slog.DiscardHandler), nil)
	if err := monitor.Configure(Config{
		File:              filepath.Join(t.TempDir(), "queue.json"),
		ProwlarrIndexFile: filepath.Join(t.TempDir(), "index.json"),
	}); err != nil {
		t.Fatalf("configure monitor: %v", err)
	}
	monitor.SetMetadataResolver(func(ctx context.Context, _ MonitoredMedia) (MonitoredMedia, error) {
		<-ctx.Done()
		return MonitoredMedia{}, ctx.Err()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := monitor.CheckStatus(ctx, &CheckStatusRequest{
		Request: &RequestDescriptor{
			MediaType:   "movie",
			Title:       "Test Movie",
			ExternalIds: map[string]string{"imdb": "tt12345"},
		},
		Targets: []*TargetRef{{
			Quality:      "original",
			ConnectionId: "conn-1",
		}},
		Connections: []*RouterConnection{{
			ID:     "conn-1",
			Config: map[string]any{"media_folder_id": "1"},
		}},
	})
	if err == nil {
		t.Fatal("expected deadline expiry error, got nil")
	}
}
