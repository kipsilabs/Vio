package requests

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Silo-Server/silo-server/internal/metadata/tmdb"
)

// severanceDetail is a series with two aired seasons, one announced, and
// specials. The test clock (newTestService) is 2026-05-24.
func severanceDetail() *tmdb.MediaDetail {
	return &tmdb.MediaDetail{MediaType: "series", ID: 95396, Title: "Severance", Year: 2022, Seasons: []tmdb.SeasonSummary{
		{Number: 0, Name: "Specials", EpisodeCount: 3, AirDate: "2022-01-01"},
		{Number: 1, Name: "Season 1", EpisodeCount: 9, AirDate: "2022-02-18"},
		{Number: 2, Name: "Season 2", EpisodeCount: 10, AirDate: "2025-01-17"},
		{Number: 3, Name: "Season 3", EpisodeCount: 0, AirDate: ""},
	}}
}

// severanceInLibrary puts the series in the library with season 1 complete
// and season 2 partly present.
func severanceInLibrary() *fakePresence {
	return &fakePresence{
		available: map[MediaType]map[int]bool{MediaTypeSeries: {95396: true}},
		seasons: map[string]map[int]SeasonCounts{
			fakePresenceContentID(MediaTypeSeries, 95396): {1: {Aired: 9, Have: 9}, 2: {Aired: 10, Have: 4}},
		},
	}
}

func seasonService(store *fakeStore, presence *fakePresence) *Service {
	svc := NewService(store, &fakeTMDBClient{detail: severanceDetail()}, presence)
	svc.Now = newTestService(store).Now
	svc.SetUserRepository(requestUserRepo{})
	return svc
}

func TestSeasonCountsComplete(t *testing.T) {
	for _, tc := range []struct {
		c    SeasonCounts
		want bool
	}{
		{SeasonCounts{Aired: 9, Have: 9}, true},
		{SeasonCounts{Aired: 9, Have: 8}, false},
		{SeasonCounts{Aired: 0, Have: 2}, true},              // no air dates yet: any episode counts
		{SeasonCounts{Upcoming: 8, Have: 1}, false},          // dated, not aired: an early episode is not the season
		{SeasonCounts{Aired: 2, Upcoming: 6, Have: 2}, true}, // airing: every aired episode is in
		{SeasonCounts{}, false},
	} {
		if got := tc.c.Complete(); got != tc.want {
			t.Errorf("%+v.Complete() = %v, want %v", tc.c, got, tc.want)
		}
	}
}

func TestCreateSeriesRequestAsksForTheMissingSeasons(t *testing.T) {
	store := newFakeStore()
	svc := seasonService(store, severanceInLibrary())

	req, err := svc.CreateRequest(context.Background(), testViewer(1), CreateRequestInput{MediaType: MediaTypeSeries, TMDBID: 95396, Title: "Severance"})
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	// Season 1 is complete; season 2 is aired but partial; season 3 has not
	// aired; specials are never requested by default.
	if !slices.Equal(req.Seasons, []int{2}) {
		t.Fatalf("seasons = %v, want [2]", req.Seasons)
	}
}

func TestCreateSeriesRequestForNamedSeasons(t *testing.T) {
	store := newFakeStore()
	svc := seasonService(store, &fakePresence{})

	req, err := svc.CreateRequest(context.Background(), testViewer(1), CreateRequestInput{MediaType: MediaTypeSeries, TMDBID: 95396, Title: "Severance", Seasons: []int{3, 1, 1}})
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if !slices.Equal(req.Seasons, []int{1, 3}) {
		t.Fatalf("seasons = %v, want [1 3]", req.Seasons)
	}

	if _, err := svc.CreateRequest(context.Background(), testViewer(2), CreateRequestInput{MediaType: MediaTypeSeries, TMDBID: 95396, Title: "Severance", Seasons: []int{9}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown season: err = %v, want ErrInvalidInput", err)
	}
	if _, err := svc.CreateRequest(context.Background(), testViewer(2), CreateRequestInput{MediaType: MediaTypeMovie, TMDBID: 550, Title: "Fight Club", Seasons: []int{1}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("movie with seasons: err = %v, want ErrInvalidInput", err)
	}
	// A season below 1 is refused on the seasons field, never dropped into a
	// request for every missing season.
	for _, seasons := range [][]int{{0}, {-1}, {2, 0}} {
		_, err := svc.CreateRequest(context.Background(), testViewer(2), CreateRequestInput{MediaType: MediaTypeSeries, TMDBID: 95396, Title: "Severance", Seasons: seasons})
		verr, ok := errors.AsType[*ValidationError](err)
		if !ok || verr.FieldErrors["seasons"] == "" {
			t.Fatalf("seasons %v: err = %v, want a validation error on seasons", seasons, err)
		}
	}
}

func TestCreateSeriesRequestRefusesACompleteSeries(t *testing.T) {
	store := newFakeStore()
	presence := severanceInLibrary()
	presence.seasons[fakePresenceContentID(MediaTypeSeries, 95396)][2] = SeasonCounts{Aired: 10, Have: 10}
	svc := seasonService(store, presence)

	if _, err := svc.CreateRequest(context.Background(), testViewer(1), CreateRequestInput{MediaType: MediaTypeSeries, TMDBID: 95396, Title: "Severance"}); !errors.Is(err, ErrAlreadyAvailable) {
		t.Fatalf("complete series: err = %v, want ErrAlreadyAvailable", err)
	}
	if _, err := svc.CreateRequest(context.Background(), testViewer(1), CreateRequestInput{MediaType: MediaTypeSeries, TMDBID: 95396, Title: "Severance", Seasons: []int{1}}); !errors.Is(err, ErrAlreadyAvailable) {
		t.Fatalf("complete named season: err = %v, want ErrAlreadyAvailable", err)
	}
}

// A season request completes when its seasons are complete, not when the
// first episode of the series is scanned in.
func TestReconcileCompletesASeasonRequestOnlyWhenItsSeasonsAre(t *testing.T) {
	store := newFakeStore()
	presence := severanceInLibrary()
	store.waiting = []*Request{{ID: "r1", MediaType: MediaTypeSeries, TMDBID: 95396, Status: StatusPending, Outcome: OutcomeActive, Seasons: []int{2}}}
	svc := seasonService(store, presence)

	if _, err := svc.ReconcileRequests(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if got := store.requests["r1"]; got != nil && got.Status == StatusCompleted {
		t.Fatalf("request = %+v, want still pending: season 2 is partial", got)
	}

	presence.seasons[fakePresenceContentID(MediaTypeSeries, 95396)][2] = SeasonCounts{Aired: 10, Have: 10}
	if _, err := svc.ReconcileRequests(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if got := store.requests["r1"]; got == nil || got.Status != StatusCompleted {
		t.Fatalf("request = %+v, want completed once season 2 is", got)
	}
}

func TestSeasonRequestState(t *testing.T) {
	partial := Request{MediaType: MediaTypeSeries, Status: StatusQueued, Outcome: OutcomeActive, Seasons: []int{1, 2},
		SeasonProgress: []SeasonProgress{{Season: 1, SeasonCounts: SeasonCounts{Aired: 9, Have: 9}}, {Season: 2, SeasonCounts: SeasonCounts{Aired: 10}}}}
	if got := partial.State(); got != StatePartiallyAvailable {
		t.Fatalf("partial = %s, want partially_available", got)
	}
	partial.SeasonProgress[1].Have = 10
	if got := partial.State(); got != StateAvailable {
		t.Fatalf("complete = %s, want available", got)
	}
	waiting := Request{MediaType: MediaTypeSeries, Status: StatusCompleted, Outcome: OutcomeActive, Seasons: []int{2}, LibraryContentID: "series-1",
		SeasonProgress: []SeasonProgress{{Season: 2, SeasonCounts: SeasonCounts{Aired: 10}}}}
	if got := waiting.State(); got != StateProcessing {
		t.Fatalf("downloaded, not scanned = %s, want processing", got)
	}
}

func TestSeriesDetailListsSeasonsAndStaysRequestable(t *testing.T) {
	store := newFakeStore()
	svc := seasonService(store, severanceInLibrary())

	detail, err := svc.GetDetail(context.Background(), testViewer(1), MediaTypeSeries, 95396)
	if err != nil {
		t.Fatalf("GetDetail: %v", err)
	}
	if !detail.Request.Requestable {
		t.Fatalf("request state = %+v, want requestable: season 2 is incomplete", detail.Request)
	}
	byNumber := map[int]RequestSeason{}
	for _, season := range detail.Seasons {
		byNumber[season.Number] = season
	}
	if len(detail.Seasons) != 3 || byNumber[1].Availability != AvailabilityAvailable ||
		byNumber[2].Availability != AvailabilityPartial || byNumber[3].Availability != AvailabilityMissing {
		t.Fatalf("seasons = %+v", detail.Seasons)
	}
}

func TestSeasonDeliveredRelaxesOnceTheServerIsDone(t *testing.T) {
	for _, tc := range []struct {
		name       string
		c          SeasonCounts
		serverDone bool
		want       bool
	}{
		{"complete", SeasonCounts{Aired: 9, Have: 9}, false, true},
		{"partial while downloading", SeasonCounts{Aired: 9, Have: 4}, false, false},
		{"partial once done: an episode the server could not find", SeasonCounts{Aired: 9, Have: 4}, true, true},
		{"missing once done", SeasonCounts{Aired: 9}, true, false},
		{"nothing in the library once done", SeasonCounts{}, true, false},
	} {
		if got := seasonDelivered(tc.c, tc.serverDone); got != tc.want {
			t.Errorf("%s: seasonDelivered(%+v, %v) = %v, want %v", tc.name, tc.c, tc.serverDone, got, tc.want)
		}
	}
}

// A season request the download server finished is notified once each of its
// seasons has an episode in the library, even with one the server could not
// find; one still waiting on the library is stamped so it rotates behind
// newer completions.
func TestNotifyFulfilledSeasonRequests(t *testing.T) {
	store := newFakeStore()
	presence := severanceInLibrary()
	store.requests["partial"] = &Request{ID: "partial", MediaType: MediaTypeSeries, TMDBID: 95396,
		Status: StatusCompleted, Outcome: OutcomeActive, Seasons: []int{2}}
	store.requests["waiting"] = &Request{ID: "waiting", MediaType: MediaTypeSeries, TMDBID: 95396,
		Status: StatusCompleted, Outcome: OutcomeActive, Seasons: []int{2, 3}}
	store.unnotified = []string{"partial", "waiting"}
	notifier := &fakeNotifier{}
	svc := seasonService(store, presence)
	svc.SetFulfillmentNotifier(notifier)

	svc.notifyFulfilledPending(context.Background())

	if !slices.Equal(notifier.requestIDs, []string{"partial"}) {
		t.Fatalf("notified = %v, want only the partial request", notifier.requestIDs)
	}
	// Season 3 has not reached the library at all: the server's word alone
	// does not make it available.
	if !slices.Equal(store.reconciled, []string{"waiting"}) {
		t.Fatalf("stamped = %v, want the waiting request stamped", store.reconciled)
	}
}

func TestRequestListsReadSeasonCountsOnce(t *testing.T) {
	store := newFakeStore()
	presence := severanceInLibrary()
	presence.available[MediaTypeSeries][1399] = true
	presence.seasons[fakePresenceContentID(MediaTypeSeries, 1399)] = map[int]SeasonCounts{1: {Aired: 10, Have: 3}}
	svc := seasonService(store, presence)
	reqs := []*Request{
		{ID: "a", MediaType: MediaTypeSeries, TMDBID: 95396, Seasons: []int{2}},
		{ID: "b", MediaType: MediaTypeSeries, TMDBID: 1399, Seasons: []int{1}},
		{ID: "c", MediaType: MediaTypeSeries, TMDBID: 95396, Seasons: []int{1, 2}},
	}

	if err := svc.attachLibraryContent(context.Background(), reqs...); err != nil {
		t.Fatal(err)
	}
	if presence.seasonLookups != 1 {
		t.Fatalf("season lookups = %d, want one for the whole page", presence.seasonLookups)
	}
	if got := reqs[1].SeasonProgress; len(got) != 1 || got[0].Have != 3 {
		t.Fatalf("progress = %+v", got)
	}
}

// A router plugin without supports_seasons takes a whole series, so with a
// download server for series on one, a series already in the library is not
// requestable for its missing seasons.
func TestMissingSeasonsNeedALibraryOnlySetup(t *testing.T) {
	store := newFakeStore()
	store.integrations = []Integration{routerInst("sonarr")}
	svc := seasonService(store, severanceInLibrary())
	svc.SetRouterProvider(&fakeRouterProvider{})

	if _, err := svc.CreateRequest(context.Background(), testViewer(1), CreateRequestInput{MediaType: MediaTypeSeries, TMDBID: 95396, Title: "Severance"}); !errors.Is(err, ErrAlreadyAvailable) {
		t.Fatalf("err = %v, want ErrAlreadyAvailable", err)
	}
	detail, err := svc.GetDetail(context.Background(), testViewer(1), MediaTypeSeries, 95396)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Request.Requestable || detail.Request.Reason != "already_available" {
		t.Fatalf("request state = %+v, want already_available", detail.Request)
	}
	if len(detail.Seasons) != 3 {
		t.Fatalf("seasons = %+v, want them listed regardless", detail.Seasons)
	}
	status, err := svc.GetFeatureStatus(context.Background(), testViewer(1))
	if err != nil {
		t.Fatal(err)
	}
	if status.MissingSeasonsRequestable {
		t.Fatal("status advertises missing seasons as requestable with a download server for series")
	}

	libraryOnly, err := seasonService(newFakeStore(), severanceInLibrary()).GetFeatureStatus(context.Background(), testViewer(1))
	if err != nil {
		t.Fatal(err)
	}
	if !libraryOnly.MissingSeasonsRequestable {
		t.Fatal("status hides missing seasons without a download server")
	}
}

// v1 keeps the whole-series rule: refused once the series is in the library,
// and a request for the whole series otherwise.
func TestWholeSeriesRequests(t *testing.T) {
	store := newFakeStore()
	svc := seasonService(store, severanceInLibrary())
	if _, err := svc.CreateRequest(context.Background(), testViewer(1), CreateRequestInput{MediaType: MediaTypeSeries, TMDBID: 95396, Title: "Severance", WholeSeries: true}); !errors.Is(err, ErrAlreadyAvailable) {
		t.Fatalf("in the library: err = %v, want ErrAlreadyAvailable", err)
	}

	svc = seasonService(newFakeStore(), &fakePresence{})
	req, err := svc.CreateRequest(context.Background(), testViewer(1), CreateRequestInput{MediaType: MediaTypeSeries, TMDBID: 95396, Title: "Severance", WholeSeries: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Seasons) != 0 {
		t.Fatalf("seasons = %v, want none: the whole series", req.Seasons)
	}
}

// A series whose aired seasons are all in the library stays requestable for
// a season that has not aired; one with nothing left to air is available.
func TestLibrarySeriesCanRequestAnUpcomingSeason(t *testing.T) {
	store := newFakeStore()
	presence := severanceInLibrary()
	presence.seasons[fakePresenceContentID(MediaTypeSeries, 95396)][2] = SeasonCounts{Aired: 10, Have: 10}
	svc := seasonService(store, presence)

	detail, err := svc.GetDetail(context.Background(), testViewer(1), MediaTypeSeries, 95396)
	if err != nil {
		t.Fatal(err)
	}
	if !detail.Request.Requestable {
		t.Fatalf("request state = %+v, want requestable for the upcoming season 3", detail.Request)
	}
	req, err := svc.CreateRequest(context.Background(), testViewer(1), CreateRequestInput{MediaType: MediaTypeSeries, TMDBID: 95396, Title: "Severance", Seasons: []int{3}})
	if err != nil || !slices.Equal(req.Seasons, []int{3}) {
		t.Fatalf("request for season 3 = %+v, %v", req, err)
	}

	ended := severanceDetail()
	ended.Seasons = ended.Seasons[:3] // no season 3 announced
	svc = NewService(newFakeStore(), &fakeTMDBClient{detail: ended}, presence)
	svc.Now = newTestService(store).Now
	svc.SetUserRepository(requestUserRepo{})
	if detail, err = svc.GetDetail(context.Background(), testViewer(1), MediaTypeSeries, 95396); err != nil {
		t.Fatal(err)
	}
	if detail.Request.Requestable || detail.Request.Reason != "already_available" {
		t.Fatalf("request state = %+v, want already_available: every season is in the library", detail.Request)
	}
}

// An episode of an upcoming season that reached the library ahead of its air
// date does not make the season complete: it can still be requested.
func TestAnEarlyEpisodeDoesNotCompleteAnUpcomingSeason(t *testing.T) {
	presence := severanceInLibrary()
	presence.seasons[fakePresenceContentID(MediaTypeSeries, 95396)][3] = SeasonCounts{Upcoming: 8, Have: 1}
	svc := seasonService(newFakeStore(), presence)

	req, err := svc.CreateRequest(context.Background(), testViewer(1), CreateRequestInput{MediaType: MediaTypeSeries, TMDBID: 95396, Title: "Severance", Seasons: []int{3}})
	if err != nil || !slices.Equal(req.Seasons, []int{3}) {
		t.Fatalf("request for season 3 = %+v, %v", req, err)
	}
}

// The detail page reads an active season request's state from the series'
// season counts, as the request lists do.
func TestSeriesDetailShowsSeasonRequestProgress(t *testing.T) {
	store := newFakeStore()
	store.active[MediaTypeSeries] = map[int]*Request{95396: {ID: "r1", MediaType: MediaTypeSeries, TMDBID: 95396,
		Status: StatusQueued, Outcome: OutcomeActive, RequestedByUserID: 1, Seasons: []int{2}}}
	svc := seasonService(store, severanceInLibrary())

	detail, err := svc.GetDetail(context.Background(), testViewer(1), MediaTypeSeries, 95396)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Request.State != StatePartiallyAvailable {
		t.Fatalf("state = %q, want partially_available: 4 of season 2's 10 episodes are in", detail.Request.State)
	}
}

// Reconciliation reads the season counts of a whole batch in one lookup.
func TestPresentRequestsReadSeasonCountsOnce(t *testing.T) {
	presence := severanceInLibrary()
	presence.available[MediaTypeSeries][1399] = true
	presence.seasons[fakePresenceContentID(MediaTypeSeries, 1399)] = map[int]SeasonCounts{1: {Aired: 10, Have: 10}}
	svc := seasonService(newFakeStore(), presence)
	reqs := []*Request{
		{ID: "partial", MediaType: MediaTypeSeries, TMDBID: 95396, Status: StatusApproved, Seasons: []int{2}},
		{ID: "complete", MediaType: MediaTypeSeries, TMDBID: 1399, Status: StatusApproved, Seasons: []int{1}},
		{ID: "done-partial", MediaType: MediaTypeSeries, TMDBID: 95396, Status: StatusCompleted, Seasons: []int{1, 2}},
		{ID: "whole", MediaType: MediaTypeSeries, TMDBID: 95396, Status: StatusApproved},
		{ID: "absent", MediaType: MediaTypeSeries, TMDBID: 7, Status: StatusApproved, Seasons: []int{1}},
	}

	present, err := svc.presentRequests(context.Background(), reqs)
	if err != nil {
		t.Fatal(err)
	}
	if presence.seasonLookups != 1 {
		t.Fatalf("season lookups = %d, want one for the whole batch", presence.seasonLookups)
	}
	want := map[string]bool{"partial": false, "complete": true, "done-partial": true, "whole": true, "absent": false}
	for id, w := range want {
		if present[id] != w {
			t.Errorf("present[%s] = %v, want %v", id, present[id], w)
		}
	}
}

// A season request made for a series in the library while no download server
// took series is not sent to one set up later on a plugin without
// supports_seasons: it would add the whole series. A season request for a
// series outside the library still goes.
func TestSeasonRequestForALibrarySeriesSkipsALaterRouter(t *testing.T) {
	store := newFakeStore()
	store.integrations = []Integration{routerInst("sonarr")}
	for _, req := range []*Request{
		{ID: "in-library", MediaType: MediaTypeSeries, TMDBID: 95396, Status: StatusApproved, Outcome: OutcomeActive, Seasons: []int{2}},
		{ID: "absent", MediaType: MediaTypeSeries, TMDBID: 1399, Status: StatusApproved, Outcome: OutcomeActive, Seasons: []int{1}},
	} {
		store.candidates = append(store.candidates, req)
		store.requests[req.ID] = req
	}
	router := &fakeRouterProvider{}
	svc := seasonService(store, severanceInLibrary())
	svc.SetRouterProvider(router)

	if _, err := svc.ReconcileRequests(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if router.fulfillCalls != 1 {
		t.Fatalf("router calls = %d, want one, for the series outside the library", router.fulfillCalls)
	}
	if got := store.requests["in-library"]; got.Status != StatusApproved || got.SubmitAttempts != 0 {
		t.Fatalf("in-library request = %+v, want approved and unsent, waiting for the library", got)
	}
	if got := store.requests["absent"]; got.Status == StatusApproved {
		t.Fatalf("absent request = %+v, want submitted", got)
	}
}

// fakeSeriesSeasonProvider stands in for the installed TVDB provider.
type fakeSeriesSeasonProvider struct {
	results     []SeriesSeason
	err         error
	gotTVDBID   int
	gotLanguage string
	calls       int
}

func (f *fakeSeriesSeasonProvider) FetchSeriesTVDBSeasons(_ context.Context, tvdbID int, language string) ([]SeriesSeason, error) {
	f.calls++
	f.gotTVDBID = tvdbID
	f.gotLanguage = language
	return f.results, f.err
}

// longAnimeDetail is TMDB's grouping of a long series: two wide seasons. TVDB
// lists the same show as three seasons.
func longAnimeDetail() *tmdb.MediaDetail {
	return &tmdb.MediaDetail{
		MediaType: "series", ID: 700001, Title: "Long Anime", TVDBID: 900001,
		NumberOfSeasons: 2, NumberOfEpisodes: 416,
		Seasons: []tmdb.SeasonSummary{
			{Number: 1, Name: "Season 1", EpisodeCount: 366, AirDate: "2000-01-01"},
			{Number: 2, Name: "Season 2", EpisodeCount: 50, AirDate: "2001-01-01"},
		},
	}
}

func longAnimeTVDBSeasons() []SeriesSeason {
	season := func(number int, name, airDate string, episodeCount int) SeriesSeason {
		return SeriesSeason{Number: number, Name: name, EpisodeCount: episodeCount, AirDate: airDate}
	}
	return []SeriesSeason{
		season(1, "TVDB Season 1", "2000-01-01", 3),
		season(2, "TVDB Season 2", "2001-01-01", 2),
		season(3, "TVDB Season 3", "2002-01-01", 1),
		{Number: 0, Name: "Specials", AirDate: "1999-01-01"},
	}
}

func longAnimeService(store *fakeStore, provider SeriesSeasonProvider) *Service {
	svc := NewService(store, &fakeTMDBClient{detail: longAnimeDetail()}, &fakePresence{})
	svc.Now = newTestService(store).Now
	svc.SetUserRepository(requestUserRepo{})
	if provider != nil {
		svc.SetSeriesSeasonProvider(provider)
	}
	return svc
}

func TestGetDetailPrefersTVDBSeasons(t *testing.T) {
	provider := &fakeSeriesSeasonProvider{results: longAnimeTVDBSeasons()}
	svc := longAnimeService(newFakeStore(), provider)

	detail, err := svc.GetDetail(context.Background(), testViewer(1), MediaTypeSeries, 700001)
	if err != nil {
		t.Fatalf("GetDetail: %v", err)
	}
	if provider.gotTVDBID != 900001 || provider.gotLanguage != "en" {
		t.Fatalf("provider got tvdb=%d language=%q, want 900001 and en", provider.gotTVDBID, provider.gotLanguage)
	}
	if len(detail.Seasons) != 3 {
		t.Fatalf("seasons = %+v, want the three TVDB regular seasons", detail.Seasons)
	}
	want := []SeriesSeason{
		{Number: 1, Name: "TVDB Season 1", EpisodeCount: 3, AirDate: "2000-01-01"},
		{Number: 2, Name: "TVDB Season 2", EpisodeCount: 2, AirDate: "2001-01-01"},
		{Number: 3, Name: "TVDB Season 3", EpisodeCount: 1, AirDate: "2002-01-01"},
	}
	for i, season := range detail.Seasons {
		w := want[i]
		if season.Number != w.Number || season.Name != w.Name || season.EpisodeCount != w.EpisodeCount ||
			season.AirDate != w.AirDate || season.PosterPath != "" || season.Availability != AvailabilityMissing {
			t.Fatalf("season %d = %+v, want %+v", i, season, w)
		}
	}
	if detail.NumberOfSeasons != 3 || detail.NumberOfEpisodes != 6 {
		t.Fatalf("totals = %d seasons/%d episodes, want the TVDB totals 3/6", detail.NumberOfSeasons, detail.NumberOfEpisodes)
	}
	if detail.TVDBID == nil || *detail.TVDBID != 900001 {
		t.Fatalf("tvdb_id = %v, want 900001", detail.TVDBID)
	}
}

func TestGetDetailFallsBackToTMDBSeasons(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider *fakeSeriesSeasonProvider
	}{
		{"provider error", &fakeSeriesSeasonProvider{err: errors.New("tvdb unavailable")}},
		{"provider empty", &fakeSeriesSeasonProvider{}},
		{"specials only", &fakeSeriesSeasonProvider{results: []SeriesSeason{{Number: 0, Name: "Specials"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := longAnimeService(newFakeStore(), tc.provider)

			detail, err := svc.GetDetail(context.Background(), testViewer(1), MediaTypeSeries, 700001)
			if err != nil {
				t.Fatalf("GetDetail: %v", err)
			}
			if len(detail.Seasons) != 2 || detail.Seasons[0].Name != "Season 1" || detail.Seasons[0].EpisodeCount != 366 ||
				detail.Seasons[1].Name != "Season 2" || detail.Seasons[1].EpisodeCount != 50 {
				t.Fatalf("seasons = %+v, want TMDB's two seasons", detail.Seasons)
			}
			if detail.NumberOfSeasons != 2 || detail.NumberOfEpisodes != 416 {
				t.Fatalf("totals = %d seasons/%d episodes, want TMDB's 2/416", detail.NumberOfSeasons, detail.NumberOfEpisodes)
			}
			if detail.TVDBID == nil || *detail.TVDBID != 900001 {
				t.Fatalf("tvdb_id = %v, want the TMDB cross-reference 900001", detail.TVDBID)
			}
		})
	}
}

func TestSeriesSeasonSourceResolvesAMissingTVDBID(t *testing.T) {
	store := newFakeStore()
	detail := longAnimeDetail()
	detail.TVDBID = 0
	provider := &fakeSeriesSeasonProvider{results: longAnimeTVDBSeasons()}
	svc := NewService(store, &fakeTMDBClient{detail: detail}, &fakePresence{})
	svc.Now = newTestService(store).Now
	svc.SetUserRepository(requestUserRepo{})
	svc.SetSeriesSeasonProvider(provider)
	resolver := &fakeTVDBResolver{tvdbID: 900001}
	svc.SetTVDBIDResolver(resolver)

	seasons, resolvedTVDBID, fromTVDB, available := svc.seriesSeasonSource(context.Background(), detail, 0)
	if resolver.gotTMDBID != 700001 || resolver.gotIMDbID != "" {
		t.Fatalf("resolver got tmdb=%d imdb=%q, want 700001 and no IMDb id", resolver.gotTMDBID, resolver.gotIMDbID)
	}
	if !fromTVDB || !available || resolvedTVDBID != 900001 || len(seasons) != 3 {
		t.Fatalf("source = %+v, tvdb=%d, fromTVDB=%v, available=%v; want TVDB's three seasons", seasons, resolvedTVDBID, fromTVDB, available)
	}
	if provider.gotTVDBID != 900001 {
		t.Fatalf("provider got tvdb=%d, want the resolved 900001", provider.gotTVDBID)
	}
}

func TestCreateSeriesRequestAcceptsATVDBOnlySeason(t *testing.T) {
	store := newFakeStore()
	svc := longAnimeService(store, &fakeSeriesSeasonProvider{results: longAnimeTVDBSeasons()})

	req, err := svc.CreateRequest(context.Background(), testViewer(1), CreateRequestInput{
		MediaType: MediaTypeSeries, TMDBID: 700001, Title: "Long Anime", Seasons: []int{3},
	})
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if !slices.Equal(req.Seasons, []int{3}) {
		t.Fatalf("seasons = %v, want [3] from TVDB", req.Seasons)
	}
	if len(store.created) != 1 || !slices.Equal(store.created[0].Input.Seasons, []int{3}) {
		t.Fatalf("created = %+v, want the TVDB-only season stored", store.created)
	}

	// Without a provider, the same season is not in TMDB's list.
	tmdbOnly := longAnimeService(newFakeStore(), nil)
	if _, err := tmdbOnly.CreateRequest(context.Background(), testViewer(1), CreateRequestInput{
		MediaType: MediaTypeSeries, TMDBID: 700001, Title: "Long Anime", Seasons: []int{3},
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("without TVDB: err = %v, want ErrInvalidInput for season 3", err)
	}
}

func TestSeriesSeasonsUseTMDBWithoutAProvider(t *testing.T) {
	detail := severanceDetail()
	detail.NumberOfSeasons = 3
	detail.NumberOfEpisodes = 22
	store := newFakeStore()
	svc := NewService(store, &fakeTMDBClient{detail: detail}, &fakePresence{})
	svc.Now = newTestService(store).Now
	svc.SetUserRepository(requestUserRepo{})

	got, err := svc.GetDetail(context.Background(), testViewer(1), MediaTypeSeries, 95396)
	if err != nil {
		t.Fatalf("GetDetail: %v", err)
	}
	if len(got.Seasons) != 3 || got.Seasons[0].Name != "Season 1" || got.Seasons[2].Name != "Season 3" {
		t.Fatalf("seasons = %+v, want the TMDB regular seasons", got.Seasons)
	}
	if got.NumberOfSeasons != 3 || got.NumberOfEpisodes != 22 {
		t.Fatalf("totals = %d/%d, want TMDB's 3/22", got.NumberOfSeasons, got.NumberOfEpisodes)
	}
}

// A mutation returns the request with its season progress, so its state
// matches what a detail or list read reports.
func TestSeasonRequestMutationsReturnSeasonProgress(t *testing.T) {
	contentID := fakePresenceContentID(MediaTypeSeries, 95396)
	input := CreateRequestInput{MediaType: MediaTypeSeries, TMDBID: 95396, Title: "Severance", Seasons: []int{2}}

	store := newFakeStore()
	store.settings.GlobalAutoApprovalEnabled = true
	svc := seasonService(store, severanceInLibrary())
	created, err := svc.CreateRequest(context.Background(), testViewer(1), input)
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != StatusApproved || created.LibraryContentID != contentID || created.State() != StatePartiallyAvailable {
		t.Fatalf("auto-approved create = status %s, content %q, state %s; want approved, %q, partially_available",
			created.Status, created.LibraryContentID, created.State(), contentID)
	}

	store = newFakeStore()
	svc = seasonService(store, severanceInLibrary())
	pending, err := svc.CreateRequest(context.Background(), testViewer(1), input)
	if err != nil {
		t.Fatal(err)
	}
	if pending.LibraryContentID != contentID {
		t.Fatalf("pending create content = %q, want %q", pending.LibraryContentID, contentID)
	}
	approved, err := svc.Approve(context.Background(), Viewer{UserID: 99, ProfileID: "admin", IsAdmin: true}, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if approved.LibraryContentID != contentID || approved.State() != StatePartiallyAvailable {
		t.Fatalf("approve = content %q, state %s; want %q, partially_available", approved.LibraryContentID, approved.State(), contentID)
	}
}
