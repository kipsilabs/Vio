package requests

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/metadata/tmdb"
)

func TestCreateRequestDefersRoutingFactsAndKeepsClientTitle(t *testing.T) {
	store := newFakeStore()
	tmdbClient := &fakeTMDBClient{detail: &tmdb.MediaDetail{
		MediaType: "movie", ID: 129, Title: "Spirited Away", Year: 2001,
		GenreIDs: []int{16, 14}, KeywordIDs: []int{210024}, OriginalLanguage: "ja",
		OriginCountries: []string{"JP"}, CompanyIDs: []int{10342},
	}}
	svc := newTestServiceWithTMDB(store, tmdbClient)
	enrichment := &deferredEnrichment{}
	svc.enrichAsync = enrichment.schedule

	req, err := svc.CreateRequest(context.Background(), testViewer(1), CreateRequestInput{
		MediaType: MediaTypeMovie, TMDBID: 129, Title: "spirited away (client copy)",
	})
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	// The 201 carries the client's title and no TMDB facts: the read is deferred.
	if req.Title != "spirited away (client copy)" || store.created[0].Facts.Captured() {
		t.Fatalf("title = %q facts = %+v, want the client's title and uncaptured facts", req.Title, store.created[0].Facts)
	}
	if enrichment.pending() != 1 {
		t.Fatalf("deferred enrichment pending = %d, want 1", enrichment.pending())
	}

	enrichment.drain()

	facts := store.factsSet[req.ID]
	if !facts.Captured() || !facts.Anime || facts.OriginalLanguage != "ja" || facts.Year != 2001 ||
		!slices.Equal(facts.GenreIDs, []int{16, 14}) || !slices.Equal(facts.CompanyIDs, []int{10342}) {
		t.Fatalf("facts = %+v, want the TMDB snapshot", facts)
	}
	got := store.requests[req.ID]
	if !got.IsAnime || got.Year == nil || *got.Year != 2001 {
		t.Fatalf("stored request = %+v, want the fetched anime flag and year", got)
	}
}

func TestCreateRequestWithoutTMDBDetailLeavesFactsUncaptured(t *testing.T) {
	store := newFakeStore()
	svc := newTestServiceWithTMDB(store, &fakeTMDBClient{})

	req, err := svc.CreateRequest(context.Background(), testViewer(1), CreateRequestInput{
		MediaType: MediaTypeMovie, TMDBID: 550, Title: "Fight Club",
	})
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if req.Title != "Fight Club" || store.created[0].Facts.Captured() {
		t.Fatalf("title = %q facts = %+v, want the client's title and uncaptured facts", req.Title, store.created[0].Facts)
	}
}

func TestRoutingFactsDatabase(t *testing.T) {
	repo, _ := lifecycleTestRepository(t)
	ctx := t.Context()
	at := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	facts := RoutingFacts{GenreIDs: []int{16}, KeywordIDs: []int{210024}, OriginalLanguage: "ja", Year: 2001, Anime: true, CapturedAt: &at}
	if _, err := repo.CreateRequest(ctx, CreateRequestRecord{
		ID: "facts", Input: CreateRequestInput{MediaType: MediaTypeMovie, TMDBID: 129, Title: "Spirited Away"},
		Status: StatusPending, Outcome: OutcomeActive, Requester: Viewer{UserID: 1}, Facts: facts,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetRequest(ctx, "facts")
	if err != nil {
		t.Fatal(err)
	}
	if !got.RoutingFacts.Captured() || !got.RoutingFacts.CapturedAt.Equal(at) || !got.RoutingFacts.Anime ||
		!slices.Equal(got.RoutingFacts.GenreIDs, []int{16}) || got.RoutingFacts.OriginalLanguage != "ja" {
		t.Fatalf("facts = %+v, want the stored snapshot", got.RoutingFacts)
	}
}
