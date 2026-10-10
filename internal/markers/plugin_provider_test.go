package markers

import (
	"context"
	"errors"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-server/internal/models"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakePluginMarkerClient struct {
	fetchResp *pluginv1.FetchMarkersResponse
	fetchReq  *pluginv1.FetchMarkersRequest
	submitReq *pluginv1.SubmitMarkerRequest
	submitErr error
}

func (f *fakePluginMarkerClient) FetchMarkers(_ context.Context, req *pluginv1.FetchMarkersRequest) (*pluginv1.FetchMarkersResponse, error) {
	f.fetchReq = req
	return f.fetchResp, nil
}

func (f *fakePluginMarkerClient) SubmitMarker(_ context.Context, req *pluginv1.SubmitMarkerRequest) (*pluginv1.SubmitMarkerResponse, error) {
	f.submitReq = req
	if f.submitErr != nil {
		return nil, f.submitErr
	}
	return &pluginv1.SubmitMarkerResponse{SubmissionId: "sub1", Status: SubmissionStatusPending, Weight: 2}, nil
}

func (f *fakePluginMarkerClient) GetMarkerProviderStats(context.Context, *pluginv1.GetMarkerProviderStatsRequest) (*pluginv1.MarkerProviderStatsResponse, error) {
	return &pluginv1.MarkerProviderStatsResponse{Total: 3, Accepted: 2, Pending: 1, AcceptanceRate: 0.66}, nil
}

func TestPluginProviderFetchMapsAllSegments(t *testing.T) {
	start10, end60 := 10.0, 60.0
	negativeStart, endPastDuration := -1.0, 1801.0
	creditsStart := 1700.0
	previewStart := 1750.0
	client := &fakePluginMarkerClient{fetchResp: &pluginv1.FetchMarkersResponse{Markers: []*pluginv1.MarkerSegment{
		{Segment: "intro", StartSeconds: &start10, EndSeconds: &end60, Confidence: 0.8, SubmissionCount: 2, Algorithm: "intro:v1"},
		{Segment: "credits", StartSeconds: &creditsStart, Confidence: 0.9, SubmissionCount: 3},
		{Segment: "recap", EndSeconds: &start10, Confidence: 0.7},
		{Segment: "preview", StartSeconds: &previewStart, Confidence: 0.6},
		{Segment: "intro", StartSeconds: &negativeStart},
		{Segment: "intro", StartSeconds: &start10, EndSeconds: &endPastDuration},
	}}}
	provider, err := NewPluginProviderWithClientFactory(PluginProviderOptions{
		InstallationID: 12,
		CapabilityID:   "markers",
		DisplayName:    "Markers",
		PluginID:       "silo.markers",
		CacheRevision:  "config-revision",
	}, func(context.Context, int, string) (pluginMarkerClient, error) {
		return client, nil
	})
	if err != nil {
		t.Fatalf("NewPluginProviderWithClientFactory: %v", err)
	}
	if provider.CacheRevision() != "config-revision" {
		t.Fatalf("cache revision = %q, want configured revision", provider.CacheRevision())
	}

	res, err := provider.FetchMarkers(context.Background(), Request{
		Kind:          ItemKindEpisode,
		ExternalIDs:   map[string]string{ExternalIDKeyTVDB: "777"},
		SeasonNumber:  1,
		EpisodeNumber: 2,
		Duration:      1800 * time.Second,
	})
	if err != nil {
		t.Fatalf("FetchMarkers: %v", err)
	}
	if client.fetchReq.GetItemType() != "episode" || client.fetchReq.GetExternalIds().GetTvdbId() != "777" {
		t.Fatalf("fetch request = %+v", client.fetchReq)
	}
	if res.SourceClass != models.MarkerSourcePlugin || res.ProviderID != "plugin:12:markers" {
		t.Fatalf("result provenance = source %q provider %q", res.SourceClass, res.ProviderID)
	}
	byKind := map[MarkerKind]Marker{}
	for _, marker := range res.Markers {
		byKind[marker.Kind] = marker
		if marker.SourceClass != models.MarkerSourcePlugin || marker.ProviderID != "plugin:12:markers" {
			t.Fatalf("marker provenance = %+v", marker)
		}
	}
	if len(res.Markers) != 4 || len(byKind) != 4 {
		t.Fatalf("mapped %d markers, want 4: %+v", len(byKind), res.Markers)
	}
	if got := byKind[MarkerKindCredits]; got.End != 1800*time.Second {
		t.Fatalf("credits end = %s, want duration default", got.End)
	}
	if got := byKind[MarkerKindRecap]; got.Start != 0 {
		t.Fatalf("recap start = %s, want zero default", got.Start)
	}
}

// TheIntroDB answers an out-of-range season with HTTP 400. Filename parsing can
// overrun its accepted range (absolute-numbered anime, yearly season folders),
// so the provider boundary must clamp before the call. Related issue: #148.
func TestPluginProviderClampsEpisodeCoordinates(t *testing.T) {
	for _, tt := range []struct {
		name            string
		season, episode int
		wantSeason      int
		wantEpisode     int
	}{
		{name: "season above provider ceiling", season: 2009, episode: 3, wantSeason: MaxProviderSeason, wantEpisode: 3},
		{name: "season far above provider ceiling", season: 100000, episode: 1, wantSeason: MaxProviderSeason, wantEpisode: 1},
		{name: "season at ceiling is unchanged", season: MaxProviderSeason, episode: 1, wantSeason: MaxProviderSeason, wantEpisode: 1},
		{name: "episode above provider ceiling", season: 1, episode: 20000, wantSeason: 1, wantEpisode: MaxProviderEpisode},
		{name: "ordinary season is unchanged", season: 3, episode: 12, wantSeason: 3, wantEpisode: 12},
		{name: "specials season zero is unchanged", season: 0, episode: 2, wantSeason: 0, wantEpisode: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakePluginMarkerClient{}
			provider, err := NewPluginProviderWithClientFactory(PluginProviderOptions{
				InstallationID: 12,
				CapabilityID:   "introdb",
			}, func(context.Context, int, string) (pluginMarkerClient, error) {
				return client, nil
			})
			if err != nil {
				t.Fatalf("NewPluginProviderWithClientFactory: %v", err)
			}

			if _, err := provider.FetchMarkers(context.Background(), Request{
				Kind:          ItemKindEpisode,
				ExternalIDs:   map[string]string{ExternalIDKeyIMDB: "tt0434665"},
				SeasonNumber:  tt.season,
				EpisodeNumber: tt.episode,
				Duration:      time.Hour,
			}); err != nil {
				t.Fatalf("FetchMarkers: %v", err)
			}
			if client.fetchReq == nil {
				t.Fatal("provider was not queried")
			}
			if got := client.fetchReq.GetSeasonNumber(); got != int32(tt.wantSeason) {
				t.Errorf("season = %d, want %d", got, tt.wantSeason)
			}
			if got := client.fetchReq.GetEpisodeNumber(); got != int32(tt.wantEpisode) {
				t.Errorf("episode = %d, want %d", got, tt.wantEpisode)
			}

			start := 5 * time.Second
			end := 30 * time.Second
			if _, err := provider.SubmitMarker(context.Background(), SubmissionRequest{
				Kind:          ItemKindEpisode,
				ExternalIDs:   map[string]string{ExternalIDKeyIMDB: "tt0434665"},
				SeasonNumber:  tt.season,
				EpisodeNumber: tt.episode,
				Segment:       MarkerKindIntro,
				Start:         &start,
				End:           &end,
				Duration:      time.Hour,
			}); err != nil {
				t.Fatalf("SubmitMarker: %v", err)
			}
			if client.submitReq == nil {
				t.Fatal("submit was not sent")
			}
			if got := client.submitReq.GetSeasonNumber(); got != int32(tt.wantSeason) {
				t.Errorf("submit season = %d, want %d", got, tt.wantSeason)
			}
			if got := client.submitReq.GetEpisodeNumber(); got != int32(tt.wantEpisode) {
				t.Errorf("submit episode = %d, want %d", got, tt.wantEpisode)
			}
		})
	}
}

// An unknown season cannot address an episode-indexed provider. Fetch must
// degrade to an empty miss without calling the provider or failing the pass, so
// other providers' markers still reach the file.
func TestPluginProviderSkipsUnknownSeason(t *testing.T) {
	client := &fakePluginMarkerClient{}
	provider, err := NewPluginProviderWithClientFactory(PluginProviderOptions{
		InstallationID: 12,
		CapabilityID:   "introdb",
	}, func(context.Context, int, string) (pluginMarkerClient, error) {
		return client, nil
	})
	if err != nil {
		t.Fatalf("NewPluginProviderWithClientFactory: %v", err)
	}

	result, err := provider.FetchMarkers(context.Background(), Request{
		Kind:          ItemKindEpisode,
		ExternalIDs:   map[string]string{ExternalIDKeyIMDB: "tt0434665"},
		SeasonNumber:  -1,
		EpisodeNumber: 4,
		Duration:      time.Hour,
	})
	if err != nil {
		t.Fatalf("FetchMarkers on unknown season: %v", err)
	}
	if len(result.Markers) != 0 {
		t.Fatalf("markers = %+v, want none", result.Markers)
	}
	if client.fetchReq != nil {
		t.Fatal("provider was queried with an unknown season")
	}

	if _, err := provider.SubmitMarker(context.Background(), SubmissionRequest{
		Kind:          ItemKindEpisode,
		ExternalIDs:   map[string]string{ExternalIDKeyIMDB: "tt0434665"},
		SeasonNumber:  -1,
		EpisodeNumber: 4,
		Segment:       MarkerKindIntro,
	}); err == nil {
		t.Fatal("SubmitMarker on unknown season succeeded, want an error")
	}
	if client.submitReq != nil {
		t.Fatal("submit was sent with an unknown season")
	}
}

// Movies carry no season coordinate and must pass through unchanged, including
// zero-valued fields, so movie marker lookups keep working.
func TestPluginProviderLeavesMovieCoordinatesAlone(t *testing.T) {
	client := &fakePluginMarkerClient{}
	provider, err := NewPluginProviderWithClientFactory(PluginProviderOptions{
		InstallationID: 12,
		CapabilityID:   "introdb",
	}, func(context.Context, int, string) (pluginMarkerClient, error) {
		return client, nil
	})
	if err != nil {
		t.Fatalf("NewPluginProviderWithClientFactory: %v", err)
	}
	if _, err := provider.FetchMarkers(context.Background(), Request{
		Kind:        ItemKindMovie,
		ExternalIDs: map[string]string{ExternalIDKeyIMDB: "tt1"},
		Duration:    time.Hour,
	}); err != nil {
		t.Fatalf("FetchMarkers: %v", err)
	}
	if client.fetchReq == nil || client.fetchReq.GetSeasonNumber() != 0 || client.fetchReq.GetEpisodeNumber() != 0 {
		t.Fatalf("movie request = %+v", client.fetchReq)
	}
}

func TestClampProviderCoordinate(t *testing.T) {
	for _, tt := range []struct {
		name       string
		value, lim int
		want       int
	}{
		{name: "negative clamps to zero", value: -5, lim: 1000, want: 0},
		{name: "in range unchanged", value: 42, lim: 1000, want: 42},
		{name: "at limit unchanged", value: 1000, lim: 1000, want: 1000},
		{name: "above limit clamps", value: 1001, lim: 1000, want: 1000},
		{name: "zero unchanged", value: 0, lim: 1000, want: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampProviderCoordinate(tt.value, tt.lim); got != tt.want {
				t.Fatalf("clampProviderCoordinate(%d, %d) = %d, want %d", tt.value, tt.lim, got, tt.want)
			}
		})
	}
}

func TestPluginProviderRejectsOutOfBoundsSegments(t *testing.T) {
	negativeStart := -1.0
	start10, end61 := 10.0, 61.0
	validStart := 50.0
	duration := time.Minute

	tests := []struct {
		name    string
		segment *pluginv1.MarkerSegment
		wantOK  bool
		wantEnd time.Duration
	}{
		{
			name:    "negative start",
			segment: &pluginv1.MarkerSegment{Segment: "intro", StartSeconds: &negativeStart},
		},
		{
			name:    "end past duration",
			segment: &pluginv1.MarkerSegment{Segment: "intro", StartSeconds: &start10, EndSeconds: &end61},
		},
		{
			name:    "default end uses duration",
			segment: &pluginv1.MarkerSegment{Segment: "credits", StartSeconds: &validStart},
			wantOK:  true,
			wantEnd: duration,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			marker, ok := markerFromPluginSegment(tt.segment, duration)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantOK && marker.End != tt.wantEnd {
				t.Fatalf("end = %s, want %s", marker.End, tt.wantEnd)
			}
		})
	}
}

func TestPluginProviderSubmitMapsRequest(t *testing.T) {
	client := &fakePluginMarkerClient{}
	provider, err := NewPluginProviderWithClientFactory(PluginProviderOptions{
		InstallationID: 12,
		CapabilityID:   "markers",
	}, func(context.Context, int, string) (pluginMarkerClient, error) {
		return client, nil
	})
	if err != nil {
		t.Fatalf("NewPluginProviderWithClientFactory: %v", err)
	}
	start, end := 5*time.Second, 30*time.Second
	result, err := provider.SubmitMarker(context.Background(), SubmissionRequest{
		Kind:        ItemKindMovie,
		ExternalIDs: map[string]string{ExternalIDKeyIMDB: "tt1"},
		Segment:     MarkerKindIntro,
		Start:       &start,
		End:         &end,
		Duration:    90 * time.Minute,
	})
	if err != nil {
		t.Fatalf("SubmitMarker: %v", err)
	}
	if result.ID != "sub1" || result.Status != SubmissionStatusPending || result.Weight != 2 {
		t.Fatalf("submit result = %+v", result)
	}
	if client.submitReq.GetItemType() != "movie" || client.submitReq.GetExternalIds().GetImdbId() != "tt1" {
		t.Fatalf("submit request identity = %+v", client.submitReq)
	}
	if client.submitReq.GetSegment() != "intro" || client.submitReq.GetStartSeconds() != 5 || client.submitReq.GetEndSeconds() != 30 {
		t.Fatalf("submit request segment = %+v", client.submitReq)
	}
}

func TestPluginProviderSubmitMapsConflicts(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "structured already exists",
			err:  status.Error(codes.AlreadyExists, "submission already exists"),
		},
		{
			name: "legacy HTTP 409",
			err:  status.Error(codes.Unknown, `introdb: submit HTTP 409: {"error":"already submitted"}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakePluginMarkerClient{submitErr: tt.err}
			provider, err := NewPluginProviderWithClientFactory(PluginProviderOptions{
				InstallationID: 12,
				CapabilityID:   "markers",
			}, func(context.Context, int, string) (pluginMarkerClient, error) {
				return client, nil
			})
			if err != nil {
				t.Fatalf("NewPluginProviderWithClientFactory: %v", err)
			}

			_, err = provider.SubmitMarker(context.Background(), SubmissionRequest{
				Kind:        ItemKindMovie,
				ExternalIDs: map[string]string{ExternalIDKeyTMDB: "123"},
				Segment:     MarkerKindIntro,
			})
			var conflict *SubmissionConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("error = %T %v, want SubmissionConflictError", err, err)
			}
			if conflict.Provider != "plugin:12:markers" || conflict.HTTPStatus != 409 {
				t.Fatalf("conflict = %+v", conflict)
			}
		})
	}
}

func TestPluginProviderSubmitMapsPermanentRefusals(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{
			name:       "legacy HTTP 400",
			err:        status.Error(codes.Unknown, `introdb: submit HTTP 400: {"error":"Season 2 does not exist for this show on TMDB"}`),
			wantStatus: 400,
		},
		{
			name:       "legacy HTTP 422",
			err:        status.Error(codes.Unknown, `introdb: submit HTTP 422: unprocessable`),
			wantStatus: 422,
		},
		{
			name: "structured invalid argument",
			err:  status.Error(codes.InvalidArgument, "season does not exist"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := submitWithPluginError(t, tt.err)
			var invalid *SubmissionInvalidError
			if !errors.As(err, &invalid) {
				t.Fatalf("error = %T %v, want SubmissionInvalidError", err, err)
			}
			if invalid.Provider != "plugin:12:markers" || invalid.HTTPStatus != tt.wantStatus {
				t.Fatalf("invalid = %+v, want HTTP status %d", invalid, tt.wantStatus)
			}
		})
	}
}

func TestPluginProviderSubmitKeepsOtherErrorsRetryable(t *testing.T) {
	for _, pluginErr := range []error{
		status.Error(codes.Unknown, "introdb: submit HTTP 500: unavailable"),
		status.Error(codes.Unknown, "introdb: submit HTTP 401: invalid api key"),
		status.Error(codes.Unknown, "introdb: submit HTTP 408: timeout"),
		status.Error(codes.Unavailable, "plugin unavailable"),
		status.Error(codes.FailedPrecondition, "api key not configured"),
	} {
		_, err := submitWithPluginError(t, pluginErr)
		var conflict *SubmissionConflictError
		var invalid *SubmissionInvalidError
		if errors.As(err, &conflict) || errors.As(err, &invalid) {
			t.Fatalf("error for %v = %T, want retryable provider error", pluginErr, err)
		}
	}
}

func submitWithPluginError(t *testing.T, pluginErr error) (SubmissionResult, error) {
	t.Helper()
	client := &fakePluginMarkerClient{submitErr: pluginErr}
	provider, err := NewPluginProviderWithClientFactory(PluginProviderOptions{
		InstallationID: 12,
		CapabilityID:   "markers",
	}, func(context.Context, int, string) (pluginMarkerClient, error) {
		return client, nil
	})
	if err != nil {
		t.Fatalf("NewPluginProviderWithClientFactory: %v", err)
	}
	return provider.SubmitMarker(context.Background(), SubmissionRequest{
		Kind:        ItemKindMovie,
		ExternalIDs: map[string]string{ExternalIDKeyTMDB: "123"},
		Segment:     MarkerKindIntro,
	})
}
