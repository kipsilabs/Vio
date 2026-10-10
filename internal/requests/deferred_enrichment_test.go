package requests

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Silo-Server/silo-server/internal/metadata/tmdb"
)

// A duplicate is refused from the local check before any TMDB read: a bulk
// client retrying after a client-side timeout pays no external round trips for
// its 409, and no enrichment is scheduled for a request that was never made.
func TestCreateRequestDuplicateRefusedBeforeTMDBRead(t *testing.T) {
	store := newFakeStore()
	tmdbClient := &fakeTMDBClient{}
	svc := newTestServiceWithTMDB(store, tmdbClient)
	enrichment := &deferredEnrichment{}
	svc.enrichAsync = enrichment.schedule
	store.active[MediaTypeMovie][550] = &Request{
		ID: "existing", MediaType: MediaTypeMovie, TMDBID: 550, Status: StatusQueued, Outcome: OutcomeActive,
	}

	_, err := svc.CreateRequest(context.Background(), testViewer(1), CreateRequestInput{
		MediaType: MediaTypeMovie, TMDBID: 550, Title: "Fight Club",
	})
	if !errors.Is(err, ErrAlreadyRequested) {
		t.Fatalf("err = %v, want ErrAlreadyRequested", err)
	}
	if got := tmdbClient.externalCalls(); got != 0 {
		t.Fatalf("external calls = %d, want none before the duplicate refusal", got)
	}
	if enrichment.pending() != 0 {
		t.Fatalf("deferred enrichment scheduled = %d, want none for a refused duplicate", enrichment.pending())
	}
}

// The 201 is returned before any TMDB read; the enrichment is scheduled to run
// after the response.
func TestCreateRequestReturns201WithoutExternalCalls(t *testing.T) {
	store := newFakeStore()
	tmdbClient := &fakeTMDBClient{externalIDs: &tmdb.ExternalIDs{IMDbID: "tt0137523"},
		detail: &tmdb.MediaDetail{MediaType: "movie", ID: 550, Title: "Fight Club", Year: 1999}}
	svc := newTestServiceWithTMDB(store, tmdbClient)
	enrichment := &deferredEnrichment{}
	svc.enrichAsync = enrichment.schedule

	req, err := svc.CreateRequest(context.Background(), testViewer(1), CreateRequestInput{
		MediaType: MediaTypeMovie, TMDBID: 550, Title: "Fight Club",
	})
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if req.Status != StatusPending {
		t.Fatalf("status = %s, want pending", req.Status)
	}
	if got := tmdbClient.externalCalls(); got != 0 {
		t.Fatalf("external calls = %d, want none before the 201", got)
	}
	if enrichment.pending() != 1 {
		t.Fatalf("deferred enrichment scheduled = %d, want 1", enrichment.pending())
	}
}

// The deferred step writes the title's external IDs and routing facts back to
// the committed row.
func TestDeferredEnrichmentWritesRoutingFacts(t *testing.T) {
	store := newFakeStore()
	tmdbClient := &fakeTMDBClient{
		externalIDs: &tmdb.ExternalIDs{IMDbID: "tt0137523"},
		detail: &tmdb.MediaDetail{
			MediaType: "movie", ID: 550, Title: "Fight Club", Year: 1999,
			GenreIDs: []int{18, 53}, KeywordIDs: []int{210024}, OriginalLanguage: "en", OriginCountries: []string{"US"},
		},
	}
	svc := newTestServiceWithTMDB(store, tmdbClient)
	enrichment := &deferredEnrichment{}
	svc.enrichAsync = enrichment.schedule

	req, err := svc.CreateRequest(context.Background(), testViewer(1), CreateRequestInput{
		MediaType: MediaTypeMovie, TMDBID: 550, Title: "Fight Club",
	})
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	enrichment.drain()

	facts := store.factsSet[req.ID]
	if !facts.Captured() || !slices.Equal(facts.GenreIDs, []int{18, 53}) || facts.OriginalLanguage != "en" ||
		!slices.Equal(facts.OriginCountries, []string{"US"}) {
		t.Fatalf("facts = %+v, want the TMDB snapshot", facts)
	}
	if got := store.requests[req.ID]; got.IMDbID != "tt0137523" || got.Year == nil || *got.Year != 1999 {
		t.Fatalf("stored request = %+v, want the deferred external id and year", got)
	}
}

// The retry a timed-out bulk client sends is refused from the active request
// the first attempt committed, with no further external calls.
func TestCreateRequestRetryAfterTimeoutReturnsConflictWithoutExternalCalls(t *testing.T) {
	store := newFakeStore()
	store.trackActive = true
	tmdbClient := &fakeTMDBClient{detail: &tmdb.MediaDetail{MediaType: "movie", ID: 550, Title: "Fight Club"}}
	svc := newTestServiceWithTMDB(store, tmdbClient)
	enrichment := &deferredEnrichment{}
	svc.enrichAsync = enrichment.schedule
	input := CreateRequestInput{MediaType: MediaTypeMovie, TMDBID: 550, Title: "Fight Club"}

	if _, err := svc.CreateRequest(context.Background(), testViewer(1), input); err != nil {
		t.Fatalf("first CreateRequest: %v", err)
	}
	enrichment.drain()
	externalBefore, detailBefore := len(tmdbClient.externalIDCalls), tmdbClient.detailCalls

	_, err := svc.CreateRequest(context.Background(), testViewer(1), input)
	if !errors.Is(err, ErrAlreadyRequested) {
		t.Fatalf("retry err = %v, want ErrAlreadyRequested", err)
	}
	if len(tmdbClient.externalIDCalls) != externalBefore || tmdbClient.detailCalls != detailBefore {
		t.Fatalf("retry external calls = %d/%d, want unchanged from %d/%d",
			len(tmdbClient.externalIDCalls), tmdbClient.detailCalls, externalBefore, detailBefore)
	}
}

// The committed row carries the client's display fields; the deferred
// enrichment never overwrites them.
func TestCreateRequestUsesClientDisplayFields(t *testing.T) {
	store := newFakeStore()
	tmdbClient := &fakeTMDBClient{detail: &tmdb.MediaDetail{
		MediaType: "movie", ID: 550, Title: "Server Title", Year: 2001,
		Overview: "server overview", PosterPath: "/server.jpg", BackdropPath: "/server-backdrop.jpg",
	}}
	svc := newTestServiceWithTMDB(store, tmdbClient)
	enrichment := &deferredEnrichment{}
	svc.enrichAsync = enrichment.schedule
	year := 1999

	req, err := svc.CreateRequest(context.Background(), testViewer(1), CreateRequestInput{
		MediaType: MediaTypeMovie, TMDBID: 550, Title: "Client Title", Year: &year,
		Overview: "client overview", PosterPath: "/client.jpg", BackdropPath: "/client-backdrop.jpg",
	})
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	in := store.created[0].Input
	if in.Title != "Client Title" || in.Year == nil || *in.Year != 1999 ||
		in.Overview != "client overview" || in.PosterPath != "/client.jpg" || in.BackdropPath != "/client-backdrop.jpg" {
		t.Fatalf("insert input = %+v, want the client's display fields", in)
	}
	if req.Title != "Client Title" {
		t.Fatalf("response title = %q, want the client's", req.Title)
	}

	enrichment.drain()
	got := store.requests[req.ID]
	if got.Title != "Client Title" || got.Overview != "client overview" ||
		got.PosterPath != "/client.jpg" || got.BackdropPath != "/client-backdrop.jpg" ||
		got.Year == nil || *got.Year != 1999 {
		t.Fatalf("stored request = %+v, want the client's display fields kept", got)
	}
}

// A failed deferred enrichment records nothing: the facts stay uncaptured and
// the external IDs unfilled, so submission and routing retry through their
// backfill paths instead of routing on a title with no facts. The failure is
// actionable, not silent.
func TestDeferredEnrichmentFailureLeavesRequestRetryable(t *testing.T) {
	store := newFakeStore()
	tmdbClient := &fakeTMDBClient{
		externalIDsErr: errors.New("tmdb: HTTP 429"),
		detailErr:      errors.New("tmdb: HTTP 429"),
	}
	svc := newTestServiceWithTMDB(store, tmdbClient)
	enrichment := &deferredEnrichment{}
	svc.enrichAsync = enrichment.schedule

	req, err := svc.CreateRequest(context.Background(), testViewer(1), CreateRequestInput{
		MediaType: MediaTypeMovie, TMDBID: 550, Title: "Fight Club",
	})
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	enrichment.drain()

	if _, ok := store.factsSet[req.ID]; ok {
		t.Fatalf("stored routing facts after a failed read, want the row left uncaptured")
	}
	if got := store.requests[req.ID]; got.RoutingFacts.Captured() || got.IMDbID != "" {
		t.Fatalf("stored request = %+v, want no captured facts and no external id", got)
	}
	// A conditional route refuses to route on the missing facts rather than
	// sending the title somewhere arbitrary.
	routes := []Route{{ID: "r", MediaType: MediaTypeMovie, Enabled: true, Position: 1,
		Conditions: RouteConditions{GenreIDs: []int{18}}, HD: RouteDestination{IntegrationID: "radarr"}}}
	if err := svc.ensureRoutingFacts(context.Background(), req, routes); err == nil {
		t.Fatal("ensureRoutingFacts routed on uncaptured facts, want an actionable error")
	}
}

// When every TVDB lookup fails rather than confirming no ID exists, the
// submission failure says so instead of telling the admin to add an ID that
// may exist: the lookup-failed flag the enrichment sets is not lost.
func TestSeriesTVDBLookupFailureIsNotSilent(t *testing.T) {
	store := newFakeStore()
	store.integrations = []Integration{routerInst("router-1")}
	store.requests["req-1"] = &Request{
		ID: "req-1", MediaType: MediaTypeSeries, TMDBID: 240001, Status: StatusQueued, Outcome: OutcomeFailed,
	}
	svc := newTestServiceWithTMDB(store, &fakeTMDBClient{externalIDsErr: errors.New("tmdb: HTTP 429")})
	svc.SetTVDBIDResolver(&fakeTVDBResolver{err: errors.New("tvdb: HTTP 503")})
	svc.SetRouterProvider(&fakeRouterProvider{targetsOverride: []RouterTarget{{
		Quality: Quality1080p, ConnectionID: "router-1", Status: StatusFailed, Message: "sonarr: tvdb_id is required",
	}}})

	if _, err := svc.Retry(context.Background(), Viewer{UserID: 1, IsAdmin: true}, "req-1"); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	var explained bool
	for _, target := range store.targets["req-1"] {
		if target.Quality == Quality1080p && target.LastError == tvdbLookupFailedMessage {
			explained = true
		}
	}
	if !explained {
		t.Fatalf("targets = %+v, want the lookup-failed explanation", store.targets["req-1"])
	}
}
