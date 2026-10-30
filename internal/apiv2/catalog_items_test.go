package apiv2

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
	catalogpkg "github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

// fakeCatalog backs the catalog-items operations: it records the request
// the seam received and answers a fixed page.
type fakeCatalog struct {
	sourceOrder        bool
	unreadableEpisodes bool
	err                error
	lastReq            catalogpkg.CatalogRequest
	lastViewer         handlers.ItemViewer
	lastGroup          bool
	lastGroups         catalogpkg.AudiobookGroupsQuery
}

func (f *fakeCatalog) ContextAccessFilter(ctx context.Context, opts handlers.AccessFilterOptions) (catalogpkg.AccessFilter, error) {
	return catalogpkg.AccessFilter{UserID: claimsFrom(ctx).UserID, ProfileID: "p-owner", AllowedLibraryIDs: []int{1, 2}, PresentationLibraryID: opts.PresentationLibraryID, ScopeFilesToLibrary: opts.ScopeFilesToLibrary, SelectedFileID: opts.SelectedFileID, DeviceID: opts.DeviceID}, nil
}

func fakeListingCard(id string) handlers.CollectionItemView {
	return handlers.CollectionItemView{ContentID: id, Type: "movie", Title: "Heat", Year: 1995, Genres: []string{"Crime"}, Status: "matched"}
}

func (f *fakeCatalog) Browse(_ context.Context, v handlers.ItemViewer, req catalogpkg.CatalogRequest, grouped bool) (handlers.CatalogBrowseView, error) {
	if f.err != nil {
		return handlers.CatalogBrowseView{}, f.err
	}
	f.lastReq, f.lastViewer, f.lastGroup = req, v, grouped
	view := handlers.CatalogBrowseView{Total: 3, TotalExact: true, Snapshot: "2026-01-02T03:04:05.678Z",
		EffectiveSort: &handlers.EffectiveSortView{Field: "title", Order: "asc"}}
	if f.sourceOrder {
		view.EffectiveSort = nil
		view.ResolvedSort = new(catalogpkg.QuerySort)
	}
	ids := []string{"movie:heat-1995", "movie:alien-1979", "movie:blade-runner-1982"}
	for i := req.Offset; i < len(ids) && len(view.Items) < req.Limit; i++ {
		view.Items = append(view.Items, fakeListingCard(ids[i]))
	}
	view.HasMore = req.Offset+len(view.Items) < len(ids)
	if req.SearchQuery != "" {
		view.SearchDiagnostics = &handlers.SearchDiagnosticsView{Provider: "postgres", Mode: "keyword"}
	}
	return view, nil
}

func (f *fakeCatalog) Filters(_ context.Context, _ handlers.ItemViewer, req catalogpkg.CatalogRequest, technical bool) (handlers.CatalogFiltersView, error) {
	if f.err != nil {
		return handlers.CatalogFiltersView{}, f.err
	}
	f.lastReq = req
	view := handlers.CatalogFiltersView{Genres: []string{"Crime"}, Authors: []string{"Frank Herbert"}}
	if technical {
		res := []string{"2160p"}
		view.Resolutions, view.AudioLanguages, view.SubtitleLanguages = &res, &[]string{}, &[]string{"en"}
	}
	return view, nil
}

func (f *fakeCatalog) SearchFacet(_ context.Context, _ handlers.ItemViewer, req catalogpkg.CatalogRequest, facet, q string, limit int) (handlers.CatalogFacetSearchView, error) {
	if f.err != nil {
		return handlers.CatalogFacetSearchView{}, f.err
	}
	f.lastReq = req
	if facet == "author" && strings.HasPrefix("frank herbert", q) { //nolint:gocritic // the fake matches a typed prefix of the name
		return handlers.CatalogFacetSearchView{
			Matches: []string{"Frank Herbert"}, HasMore: limit == 1,
			Values: []catalogpkg.FacetValue{{Value: "Frank Herbert", Count: 6}}, ValuesHasMore: limit == 1,
		}, nil
	}
	if facet == "genre" && q == "" {
		return handlers.CatalogFacetSearchView{Values: []catalogpkg.FacetValue{{Value: "Drama", Count: 9}}, ValuesHasMore: true}, nil
	}
	return handlers.CatalogFacetSearchView{}, nil
}

func (f *fakeCatalog) LetterIndex(_ context.Context, v handlers.ItemViewer, req catalogpkg.CatalogRequest) (catalogpkg.LetterIndex, error) {
	if f.err != nil {
		return catalogpkg.LetterIndex{}, f.err
	}
	f.lastReq, f.lastViewer = req, v
	// A digit title, three H titles, and a non-Latin one the collation sorts
	// after them, so # has two runs.
	return catalogpkg.LetterIndex{
		Letters: []catalogpkg.LetterBucket{{Letter: "#", Count: 2, Position: 0}, {Letter: "H", Count: 3, Position: 1}},
		Runs:    []catalogpkg.LetterRun{{Letter: "#", Position: 0}, {Letter: "H", Position: 1}, {Letter: "#", Position: 4}},
	}, nil
}

func (f *fakeCatalog) AudiobookGroups(_ context.Context, v handlers.ItemViewer, q catalogpkg.AudiobookGroupsQuery) (handlers.AudiobookGroupsView, error) {
	if f.err != nil {
		return handlers.AudiobookGroupsView{}, f.err
	}
	f.lastGroups, f.lastViewer = q, v
	offset := 0
	if q.After != nil {
		offset = 1
	}
	view := handlers.AudiobookGroupsView{Total: 2, TotalExact: q.IncludeTotal, HasMore: offset == 0 && q.Limit == 1}
	names := []string{"Frank Herbert", "Ursula K. Le Guin"}
	for i := offset; i < len(names) && len(view.Groups) < q.Limit; i++ {
		view.Groups = append(view.Groups, handlers.AudiobookGroupView{Name: names[i], ItemCount: 3, TotalDurationSeconds: 7200, PosterURLs: nil})
	}
	if view.HasMore {
		view.Next = &catalogpkg.AudiobookGroupCursor{GroupKey: "frank herbert", Value: 3}
	}
	return view, nil
}

// fakeDeviceAudioOverride is the device whose device-scoped audio preference
// is French; every other caller resolves the profile's English.
const fakeDeviceAudioOverride = "phone-1"

// fakeHeatVersion answers Heat's one version with the effective audio the
// viewer's device resolves, as the detail service does.
func fakeHeatVersion(v handlers.ItemViewer) catalogpkg.FileVersion {
	version := catalogpkg.FileVersion{FileID: 120, FilePath: "/media/movies/Heat.mkv", Resolution: "2160p", AddedAt: fixedTime(),
		EffectiveAudioTrackIndex: new(1), EffectiveAudioLanguage: "en"}
	if v.Access.DeviceID == fakeDeviceAudioOverride {
		version.EffectiveAudioTrackIndex, version.EffectiveAudioLanguage = new(2), "fr"
	}
	return version
}

func notFoundItem() error {
	return &handlers.APIError{Status: http.StatusNotFound, Code: "not_found", Message: "Item not found"}
}

func (f *fakeCatalog) ItemDetail(_ context.Context, v handlers.ItemViewer, id string) (*catalogpkg.ItemDetail, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.lastViewer = v
	if id == "episode:severance-s01e01" {
		return &catalogpkg.ItemDetail{ContentID: id, Type: "episode", Title: "Good News About Hell", SeriesID: "series:severance",
			Genres: []string{}, Cast: []catalogpkg.CastCredit{}, Crew: []catalogpkg.CrewCredit{}, Versions: []catalogpkg.FileVersion{}, Subtitles: []catalogpkg.SubtitleInfo{}}, nil
	}
	if id == "series:severance-S01" {
		return &catalogpkg.ItemDetail{ContentID: id, Type: "season", Title: "Season 1", SeriesID: "series:severance",
			Genres: []string{}, Cast: []catalogpkg.CastCredit{}, Crew: []catalogpkg.CrewCredit{}, Versions: []catalogpkg.FileVersion{}, Subtitles: []catalogpkg.SubtitleInfo{}}, nil
	}
	if id == "series:severance" {
		return &catalogpkg.ItemDetail{ContentID: id, Type: "series", Title: "Severance",
			Genres: []string{}, Cast: []catalogpkg.CastCredit{}, Crew: []catalogpkg.CrewCredit{}, Versions: []catalogpkg.FileVersion{}, Subtitles: []catalogpkg.SubtitleInfo{}}, nil
	}
	if id != "movie:heat-1995" {
		return nil, notFoundItem()
	}
	fileID := 120
	imdbVotes := int64(673852)
	return &catalogpkg.ItemDetail{ContentID: id, Type: "movie", Title: "Heat", Year: 1995, Genres: []string{"Crime"}, Tagline: "A Los Angeles crime saga",
		RatingSources: []catalogpkg.ItemRatingSourceInfo{
			{Source: models.RatingSourceIMDB, Score: 83, Votes: &imdbVotes},
			{Source: "mdblist", Score: 86},
		},
		Cast:           []catalogpkg.CastCredit{{Name: "Al Pacino", Character: "Vincent Hanna", PersonID: "7"}},
		SeasonUserData: &catalogpkg.SeasonUserData{Played: true, WatchedCount: 1, LastFileID: &fileID},
		UserState:      &catalogpkg.ItemUserState{Played: true, IsFavorite: true},
		Versions:       []catalogpkg.FileVersion{fakeHeatVersion(v)},
		Subtitles:      []catalogpkg.SubtitleInfo{}, OverlaySummary: &models.OverlaySummary{Resolution: "4K"},
		WorkFormats: []catalogpkg.WorkFormatSummary{{Type: "ebook", ContentID: "ebook:heat", LibraryID: 2}}}, nil
}

func (f *fakeCatalog) ItemVersions(_ context.Context, v handlers.ItemViewer, id string) ([]catalogpkg.FileVersion, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.lastViewer = v
	if id != "movie:heat-1995" {
		return nil, notFoundItem()
	}
	version := fakeHeatVersion(v)
	version.FilePath = ""
	return []catalogpkg.FileVersion{version}, nil
}

func (f *fakeCatalog) MangaFiles(_ context.Context, _ handlers.ItemViewer, id string) (*catalogpkg.MangaSeriesFiles, error) {
	if f.err != nil {
		return nil, f.err
	}
	if id != "series:berserk" {
		return nil, notFoundItem()
	}
	return &catalogpkg.MangaSeriesFiles{FolderPaths: []string{"/media/manga/Berserk"}, Files: []catalogpkg.MangaChapterFile{{ContentID: "manga:berserk-c001", Title: "Chapter 1", FileName: "c001.cbz", FileSize: 42}}}, nil
}

func fakeEpisode(n int) handlers.EpisodeView {
	ep := handlers.EpisodeView{ContentID: "episode:severance-s01e0" + string(rune('0'+n)), SeasonNumber: 1, EpisodeNumber: n, Title: "Good News About Hell", AirDate: "2022-02-18", Runtime: 57,
		Files: []handlers.EpisodeFileView{{FileID: 200 + n, Resolution: "1080p", FileSize: 1}}, UserData: &catalogpkg.SeasonUserData{Played: n == 1, WatchedCount: 1}}
	if n == 1 {
		ep.RatingIMDB, ep.RatingTMDB = new(8.7), new(8.25)
	}
	return ep
}

func (f *fakeCatalog) ItemEpisodes(_ context.Context, _ handlers.ItemViewer, id string) ([]handlers.EpisodeView, error) {
	if f.err != nil {
		return nil, f.err
	}
	if id != "series:severance-S01" {
		return nil, notFoundItem()
	}
	episodes := []handlers.EpisodeView{fakeEpisode(1), fakeEpisode(2)}
	if f.unreadableEpisodes {
		episodes[0].Files[0].Unreadable = true
		episodes[0].Files = append(episodes[0].Files, handlers.EpisodeFileView{FileID: 203, Resolution: "1080p"})
	}
	return episodes, nil
}

func fakeSeason() handlers.SeasonView {
	return handlers.SeasonView{ContentID: "series:severance-S01", PlayContentID: "episode:severance-s01e02", SeasonNumber: 1, Title: "Season 1", AirDate: "2022-02-18", EpisodeCount: 9,
		UserData: &catalogpkg.SeasonUserData{WatchedCount: 1, UnplayedCount: 8}}
}

func (f *fakeCatalog) SeriesSeasons(_ context.Context, _ handlers.ItemViewer, id string, includeArtwork bool) ([]handlers.SeasonView, error) {
	if f.err != nil {
		return nil, f.err
	}
	if id != "series:severance" {
		return nil, notFoundItem()
	}
	season := fakeSeason()
	if includeArtwork {
		season.PosterURL = "https://images.example/season.webp"
		season.PosterThumbhash = "placeholder"
	}
	return []handlers.SeasonView{season}, nil
}

func (f *fakeCatalog) SeriesSeason(_ context.Context, _ handlers.ItemViewer, id string, num int) (handlers.SeasonView, error) {
	if f.err != nil {
		return handlers.SeasonView{}, f.err
	}
	if id != "series:severance" || num != 1 {
		return handlers.SeasonView{}, &handlers.APIError{Status: http.StatusNotFound, Code: "not_found", Message: "Season not found"}
	}
	return fakeSeason(), nil
}

func (f *fakeCatalog) SeasonEpisodes(_ context.Context, _ handlers.ItemViewer, id string, num int) ([]handlers.EpisodeView, error) {
	if f.err != nil {
		return nil, f.err
	}
	if id != "series:severance" || num != 1 {
		return nil, &handlers.APIError{Status: http.StatusNotFound, Code: "not_found", Message: "Season not found"}
	}
	return []handlers.EpisodeView{fakeEpisode(1), fakeEpisode(2)}, nil
}

func catalogDeps(t *testing.T) (Dependencies, *fakeCatalog) {
	t.Helper()
	deps := libraryViewDeps(t)
	deps.CursorSecret = []byte("catalog-cursor-key")
	fake := &fakeCatalog{}
	deps.CatalogAccess, deps.CatalogBrowse, deps.CatalogItems = fake, fake, fake
	return deps, fake
}

type fakePreplayCatalog struct {
	*fakeCatalog
	mu            sync.Mutex
	detail        catalogpkg.ItemDetail
	reads         int
	discoveries   int
	discoveryErr  error
	reloadErr     error
	discoveryView handlers.ItemViewer
	discoveryUser int
	discoveryID   string
	discoveryMode handlers.VirtualDiscoveryMode
	bounded       bool
	// hasVirtual is the eligibility answer from the stored rows (the table's
	// unredacted path), independent of the file paths the response returns:
	// a viewer's redaction must not gate discovery.
	hasVirtual bool
	// discoveryGate, when non-nil, holds a listing open until it is closed and
	// discoveryStarted receives once that listing is running. They let a test
	// observe that catalog detail answered while discovery was still in flight.
	discoveryGate    <-chan struct{}
	discoveryStarted chan<- struct{}
	completions      int
}

// preplaySnapshot copies the fake's mutable observation state. Catalog detail
// starts discovery on a detached goroutine, so the test must read it under the
// same lock the fake writes it with.
type preplaySnapshot struct {
	reads         int
	discoveries   int
	discoveryView handlers.ItemViewer
	discoveryUser int
	discoveryID   string
	discoveryMode handlers.VirtualDiscoveryMode
	bounded       bool
}

func (fake *fakePreplayCatalog) snapshot() preplaySnapshot {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return preplaySnapshot{
		reads: fake.reads, discoveries: fake.discoveries,
		discoveryView: fake.discoveryView, discoveryUser: fake.discoveryUser,
		discoveryID: fake.discoveryID, discoveryMode: fake.discoveryMode,
		bounded: fake.bounded,
	}
}

// waitForDiscoveries polls until the detached listing for the item has run.
func (fake *fakePreplayCatalog) waitForDiscoveries(t *testing.T, want int, timeout time.Duration) preplaySnapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		snap := fake.snapshot()
		if snap.discoveries >= want {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("discoveries = %d after %s, want %d", snap.discoveries, timeout, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (fake *fakePreplayCatalog) HasVirtualSource(_ context.Context, _ string) (bool, error) {
	return fake.hasVirtual, nil
}

func (fake *fakePreplayCatalog) ItemDetail(_ context.Context, viewer handlers.ItemViewer, _ string) (*catalogpkg.ItemDetail, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.reads++
	if fake.err != nil {
		return nil, fake.err
	}
	if fake.reads > 1 && fake.reloadErr != nil {
		return nil, fake.reloadErr
	}
	fake.lastViewer = viewer
	detail := fake.detail
	return &detail, nil
}

func (fake *fakePreplayCatalog) DiscoverVirtualCandidates(ctx context.Context, userID int, profileID, contentID string, access catalogpkg.AccessFilter, mode handlers.VirtualDiscoveryMode) ([]catalogpkg.FileVersion, error) {
	started := fake.discoveryStarted
	gate := fake.discoveryGate
	fake.mu.Lock()
	fake.discoveries++
	fake.discoveryMode = mode
	fake.discoveryView = handlers.ItemViewer{ProfileID: profileID, Access: access}
	fake.discoveryUser, fake.discoveryID = userID, contentID
	_, fake.bounded = ctx.Deadline()
	discoveryErr := fake.discoveryErr
	fake.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if discoveryErr != nil {
		return nil, discoveryErr
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.completions++
	fake.detail.Versions = []catalogpkg.FileVersion{{FileID: 121, FilePath: "virtual://movie/tt0113277?result=candidate", Resolution: "2160p", AddedAt: fixedTime()}}
	return fake.detail.Versions, nil
}

// discoveryDoneCount reports how many listings returned successfully.
func (fake *fakePreplayCatalog) discoveryDoneCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.completions
}

type fakePreplayWatch struct {
	*fakeWatch
	catalog *fakePreplayCatalog
}

func (fake *fakePreplayWatch) ContextAccessFilter(ctx context.Context, opts handlers.AccessFilterOptions) (catalogpkg.AccessFilter, error) {
	return fake.catalog.ContextAccessFilter(ctx, opts)
}

func (fake *fakePreplayWatch) WatchDetail(ctx context.Context, _ int, profileID, contentID string, access catalogpkg.AccessFilter) (*catalogpkg.WatchDetail, error) {
	detail, err := fake.catalog.ItemDetail(ctx, handlers.ItemViewer{ProfileID: profileID, Access: access}, contentID)
	if err != nil {
		return nil, err
	}
	return &catalogpkg.WatchDetail{ContentID: detail.ContentID, Type: detail.Type, Versions: detail.Versions}, nil
}

func TestDetailPreplayDiscovery(t *testing.T) {
	for _, endpoint := range []string{"catalog/items", "watch"} {
		for _, test := range []struct {
			name        string
			mediaType   string
			path        string
			disabled    bool
			unwired     bool
			denied      bool
			redactPath  bool
			discoverErr error
			reloadErr   error
			wantCalls   int
			wantReads   int
			wantFileID  string
		}{
			{name: "movie", mediaType: "movie", path: "virtual://movie/tt0113277", wantCalls: 1, wantReads: 2, wantFileID: "121"},
			{name: "episode", mediaType: "episode", path: "virtual://series/tvdb/393159/1/2", wantCalls: 1, wantReads: 2, wantFileID: "121"},
			{name: "redacted for viewer", mediaType: "movie", path: "virtual://movie/tt0113277", redactPath: true, wantCalls: 1, wantReads: 2, wantFileID: "121"},
			{name: "disabled", mediaType: "movie", path: "virtual://movie/tt0113277", disabled: true, wantReads: 1, wantFileID: "120"},
			{name: "unwired", mediaType: "movie", path: "virtual://movie/tt0113277", unwired: true, wantReads: 1, wantFileID: "120"},
			{name: "local", mediaType: "movie", path: "/media/movie.mkv", wantReads: 1, wantFileID: "120"},
			{name: "series", mediaType: "series", path: "virtual://series/tt123", wantReads: 1, wantFileID: "120"},
			{name: "season", mediaType: "season", path: "virtual://series/tt123/1", wantReads: 1, wantFileID: "120"},
			{name: "failure", mediaType: "movie", path: "virtual://movie/tt0113277", discoverErr: errors.New("provider failed"), wantCalls: 1, wantReads: 1, wantFileID: "120"},
			{name: "timeout", mediaType: "episode", path: "virtual://series/tt123/1/2", discoverErr: context.DeadlineExceeded, wantCalls: 1, wantReads: 1, wantFileID: "120"},
			{name: "reload failure", mediaType: "movie", path: "virtual://movie/tt0113277", reloadErr: errors.New("reload failed"), wantCalls: 1, wantReads: 2, wantFileID: "120"},
			{name: "denied", mediaType: "movie", path: "virtual://movie/tt0113277", denied: true, wantReads: 1},
		} {
			t.Run(endpoint+"/"+test.name, func(t *testing.T) {
				deps, base := catalogDeps(t)
				fake := &fakePreplayCatalog{
					fakeCatalog: base, discoveryErr: test.discoverErr, reloadErr: test.reloadErr,
					detail: catalogpkg.ItemDetail{ContentID: "movie:heat-1995", Type: test.mediaType,
						Versions: []catalogpkg.FileVersion{{FileID: 120, FilePath: test.path, AddedAt: fixedTime()}}},
					hasVirtual: strings.HasPrefix(test.path, "virtual://"),
				}
				if test.redactPath {
					// A non-curator viewer's answer strips the stored path; the
					// eligibility lookup reads the real rows instead.
					fake.detail.Versions[0].FilePath = ""
				}
				if test.denied {
					fake.err = notFoundItem()
				}
				deps.CatalogItems = fake
				deps.Watch = &fakePreplayWatch{fakeWatch: &fakeWatch{}, catalog: fake}
				if !test.unwired {
					deps.VirtualCandidatesDiscovery = fake
				}
				deps.PreplayDiscoveryEnabled = func(context.Context) bool { return !test.disabled }
				rec := do(t, newTestHandler(t, deps), http.MethodGet,
					"/api/v2/"+endpoint+"/movie:heat-1995?library_id=2&file_id=120", "", viewerHeaders())
				if test.denied {
					requireProblem(t, rec, TypeNotFound)
				} else {
					if rec.Code != http.StatusOK {
						t.Fatal(rec.Body.String())
					}
					var body struct {
						Versions []struct {
							FileID string `json:"file_id"`
						} `json:"versions"`
						HasVirtualSource *bool `json:"has_virtual_source"`
					}
					decodeJSON(t, rec.Body, &body)
					wantFileID := test.wantFileID
					if endpoint == "catalog/items" {
						// Catalog detail answers from the persisted list: the
						// listing this read starts is detached, so its result
						// reaches clients through versions_updated, never this
						// response.
						wantFileID = "120"
					}
					if len(body.Versions) != 1 || body.Versions[0].FileID != wantFileID {
						t.Fatalf("versions = %+v", body.Versions)
					}
					// The hint is present for eligible virtual leaves (regardless
					// of whether discovery is enabled or succeeded), and absent
					// for non-leaf media, local items, and unwired services.
					wantVirtualHint := (test.mediaType == "movie" || test.mediaType == "episode") &&
						strings.HasPrefix(test.path, "virtual://") && !test.unwired
					if wantVirtualHint {
						if body.HasVirtualSource == nil || !*body.HasVirtualSource {
							t.Fatalf("has_virtual_source = %v, want true", body.HasVirtualSource)
						}
					} else if body.HasVirtualSource != nil {
						t.Fatalf("has_virtual_source = %v, want absent", *body.HasVirtualSource)
					}
				}
				wantReads := test.wantReads
				if endpoint == "catalog/items" && test.wantCalls > 0 {
					// No in-request reload: the detached run is what refreshes
					// an open page.
					wantReads = 1
				}
				snap := fake.snapshot()
				if endpoint == "catalog/items" && test.wantCalls > 0 {
					// The listing runs detached; wait for it instead of racing
					// the goroutine.
					snap = fake.waitForDiscoveries(t, test.wantCalls, 5*time.Second)
				}
				if snap.discoveries != test.wantCalls || snap.reads != wantReads {
					t.Fatalf("discoveries = %d, reads = %d", snap.discoveries, snap.reads)
				}
				if test.wantCalls > 0 && snap.discoveryMode != handlers.VirtualDiscoveryDetail {
					t.Fatalf("discovery mode = %q, want a detail-page read to prepare", snap.discoveryMode)
				}
				if test.wantCalls > 0 && (!snap.bounded || !reflect.DeepEqual(snap.discoveryView, fake.lastViewer) ||
					snap.discoveryUser != fake.lastViewer.Access.UserID || snap.discoveryID != fake.detail.ContentID) {
					t.Fatalf("discovery viewer = %+v, detail viewer = %+v, bounded = %v", snap.discoveryView, fake.lastViewer, snap.bounded)
				}
				if test.wantCalls > 0 && snap.discoveryView.Access.SelectedFileID != 120 {
					t.Fatalf("discovery selected file = %d, want the route's file_id 120", snap.discoveryView.Access.SelectedFileID)
				}
			})
		}
	}
}

type fakeSelectionWarm struct {
	probes []int
	warms  []int
}

func (f *fakeSelectionWarm) ScheduleSelectedFileProbe(_ string, fileID int, _ catalogpkg.AccessFilter) {
	f.probes = append(f.probes, fileID)
}

func (f *fakeSelectionWarm) ScheduleSelectedFileWarm(_ string, fileID int, _ catalogpkg.AccessFilter) {
	f.warms = append(f.warms, fileID)
}

// A catalog detail read is the only server-visible signal a thin client sends
// before it picks a version. It must warm the virtual rows it surfaces: an
// unprobed row gets the probe and the fetch warm-up, a probed row only the
// warm-up, the cap holds, local and failed rows are skipped, and a repeated
// read inside the memo window schedules nothing again.
func TestCatalogDetailWarmsVirtualVersions(t *testing.T) {
	deps, base := catalogDeps(t)
	probedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	detail := catalogpkg.ItemDetail{ContentID: "movie:heat-1995", Type: "movie",
		Versions: []catalogpkg.FileVersion{
			{FileID: 1, FilePath: "virtual://movie/tt0113277?result=a", AddedAt: fixedTime()},
			{FileID: 2, FilePath: "virtual://movie/tt0113277?result=b", AddedAt: fixedTime(), ProbedAt: &probedAt},
			{FileID: 3, FilePath: "virtual://movie/tt0113277?result=c", AddedAt: fixedTime()},
			{FileID: 4, FilePath: "/media/movie/heat.mkv", AddedAt: fixedTime()},
			{FileID: 5, FilePath: "virtual://movie/tt0113277?result=dead", AddedAt: fixedTime(), Failed: true},
		}}
	fake := &fakePreplayCatalog{fakeCatalog: base, detail: detail}
	deps.CatalogItems = fake
	warm := &fakeSelectionWarm{}
	deps.VirtualSelectionWarm = warm
	h := newTestHandler(t, deps)

	for range 2 {
		rec := do(t, h, http.MethodGet, "/api/v2/catalog/items/movie:heat-1995", "", viewerHeaders())
		if rec.Code != http.StatusOK {
			t.Fatal(rec.Body.String())
		}
	}
	if want := []int{1}; !reflect.DeepEqual(warm.probes, want) {
		t.Fatalf("probes = %v, want %v (two-version cap, then deduped)", warm.probes, want)
	}
	if want := []int{1, 2}; !reflect.DeepEqual(warm.warms, want) {
		t.Fatalf("warms = %v, want %v (two-version cap, then deduped)", warm.warms, want)
	}
}

// An unwired warm seam leaves the detail read untouched rather than failing it.
func TestCatalogDetailWarmIsNoOpWhenUnwired(t *testing.T) {
	deps, base := catalogDeps(t)
	fake := &fakePreplayCatalog{fakeCatalog: base,
		detail: catalogpkg.ItemDetail{ContentID: "movie:heat-1995", Type: "movie",
			Versions: []catalogpkg.FileVersion{{FileID: 1, FilePath: "virtual://movie/tt0113277?result=a", AddedAt: fixedTime()}}}}
	deps.CatalogItems = fake
	rec := do(t, newTestHandler(t, deps), http.MethodGet, "/api/v2/catalog/items/movie:heat-1995", "", viewerHeaders())
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
}

// The detail's version rows carry the per-version liveness state the client
// needs to choose without a second check request: durable failed/available,
// the process-local source mismatch, the probe stamp, and the audio/subtitle
// inventory that makes a direct pick possible.
func TestCatalogDetailVersionsCarryLivenessAndTrackInventory(t *testing.T) {
	deps, base := catalogDeps(t)
	probedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	unavailable := false
	zeroIndex := 0
	detail := catalogpkg.ItemDetail{ContentID: "movie:heat-1995", Type: "movie",
		Versions: []catalogpkg.FileVersion{
			{
				FileID: 42, FilePath: "virtual://movie/tt0113277?result=a", AddedAt: fixedTime(),
				Failed: true, Available: &unavailable, ProbedAt: &probedAt,
				SourceMismatch:           &catalogpkg.VersionSourceMismatch{ServedBytes: 7_427_636_945, DeclaredBytes: 8_449_999_999},
				AudioTracks:              []models.AudioTrack{{Language: "en", Codec: "truehd", Channels: 8}},
				SubtitleTracks:           []catalogpkg.VersionSubtitleTrack{{Language: "en", Codec: "srt"}},
				EffectiveAudioTrackIndex: &zeroIndex, EffectiveAudioLanguage: "en",
			},
			{FileID: 43, FilePath: "/media/movie/heat.mkv", AddedAt: fixedTime()},
		}}
	deps.CatalogItems = &fakePreplayCatalog{fakeCatalog: base, detail: detail}

	rec := do(t, newTestHandler(t, deps), http.MethodGet, "/api/v2/catalog/items/movie:heat-1995", "", viewerHeaders())
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	var body struct {
		Versions []struct {
			FileID         string  `json:"file_id"`
			Failed         bool    `json:"failed"`
			Available      *bool   `json:"available"`
			ProbedAt       *string `json:"probed_at"`
			SourceMismatch *struct {
				ServedBytes   int64 `json:"served_bytes"`
				DeclaredBytes int64 `json:"declared_bytes"`
			} `json:"source_mismatch"`
			AudioTracks    []models.AudioTrack               `json:"audio_tracks"`
			SubtitleTracks []catalogpkg.VersionSubtitleTrack `json:"subtitle_tracks"`
			EffectiveAudio string                            `json:"effective_audio_language"`
		} `json:"versions"`
	}
	decodeJSON(t, rec.Body, &body)
	if len(body.Versions) != 2 {
		t.Fatalf("versions = %+v", body.Versions)
	}
	first := body.Versions[0]
	if first.FileID != "42" || !first.Failed || first.Available == nil || *first.Available {
		t.Fatalf("virtual version liveness = %+v", first)
	}
	if first.ProbedAt == nil || *first.ProbedAt != "2026-03-04T05:06:07.000Z" {
		t.Fatalf("probed_at = %v", first.ProbedAt)
	}
	if first.SourceMismatch == nil || first.SourceMismatch.ServedBytes != 7_427_636_945 || first.SourceMismatch.DeclaredBytes != 8_449_999_999 {
		t.Fatalf("source_mismatch = %+v", first.SourceMismatch)
	}
	if len(first.AudioTracks) != 1 || first.AudioTracks[0].Codec != "truehd" || first.AudioTracks[0].Channels != 8 {
		t.Fatalf("audio_tracks = %+v", first.AudioTracks)
	}
	if len(first.SubtitleTracks) != 1 || first.SubtitleTracks[0].Codec != "srt" {
		t.Fatalf("subtitle_tracks = %+v", first.SubtitleTracks)
	}
	if first.EffectiveAudio != "en" {
		t.Fatalf("effective_audio_language = %q", first.EffectiveAudio)
	}
	second := body.Versions[1]
	if second.Failed || second.Available != nil || second.ProbedAt != nil || second.SourceMismatch != nil {
		t.Fatalf("local version must omit virtual-only liveness = %+v", second)
	}
}

// Catalog detail must not price the pre-play discovery budget into the answer.
// It answers from the persisted version list while the listing it started is
// still in flight, and a second read during that window joins the work instead
// of starting another provider round-trip. The completion is what publishes
// versions_updated (covered by the handlers test), so nothing about the fresh
// list depends on this response waiting.
func TestCatalogDetailAnswersWithoutWaitingForPreplayDiscovery(t *testing.T) {
	deps, base := catalogDeps(t)
	gate := make(chan struct{})
	started := make(chan struct{}, 4)
	fake := &fakePreplayCatalog{
		fakeCatalog: base,
		detail: catalogpkg.ItemDetail{ContentID: "movie:heat-1995", Type: "movie",
			Versions: []catalogpkg.FileVersion{{FileID: 120, FilePath: "virtual://movie/tt0113277", AddedAt: fixedTime()}}},
		hasVirtual:       true,
		discoveryGate:    gate,
		discoveryStarted: started,
	}
	deps.CatalogItems = fake
	deps.VirtualCandidatesDiscovery = fake
	deps.PreplayDiscoveryEnabled = func(context.Context) bool { return true }
	// The deployment setting that produced the observed 10 s detail reads.
	deps.PreplayDiscoveryBudgetMs = func(context.Context) int { return 10000 }
	handler := newTestHandler(t, deps)

	answer := func(wantFileID string) time.Duration {
		begin := time.Now()
		rec := do(t, handler, http.MethodGet, "/api/v2/catalog/items/movie:heat-1995?library_id=2&file_id=120", "", viewerHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Versions []struct {
				FileID string `json:"file_id"`
			} `json:"versions"`
		}
		decodeJSON(t, rec.Body, &body)
		if len(body.Versions) != 1 || body.Versions[0].FileID != wantFileID {
			t.Fatalf("versions = %+v, want %s", body.Versions, wantFileID)
		}
		return time.Since(begin)
	}

	elapsed := answer("120")
	// Generous compared with the 10 s budget the provider would have consumed,
	// small enough to fail if the handler waited on the listing at all.
	if elapsed > 2*time.Second {
		t.Fatalf("catalog detail took %v while discovery was still in flight", elapsed)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("catalog detail did not start the detached discovery")
	}
	if snap := fake.snapshot(); snap.discoveries != 1 {
		t.Fatalf("discoveries = %d after the first read, want 1", snap.discoveries)
	}

	// A second read during the same listing must answer as well and must not
	// start a competing provider round-trip.
	if elapsed := answer("120"); elapsed > 2*time.Second {
		t.Fatalf("second catalog detail took %v", elapsed)
	}
	select {
	case <-started:
		t.Fatal("a second detail read started a second discovery while one was running")
	case <-time.After(50 * time.Millisecond):
	}
	if snap := fake.snapshot(); snap.discoveries != 1 {
		t.Fatalf("discoveries = %d after the second read, want 1", snap.discoveries)
	}

	close(gate)
	// The detached run completes and records its 30 s suppression window, so a
	// later read in that window neither waits nor re-lists.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if fake.discoveryDoneCount() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("detached discovery never completed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The detached run persisted the fresh candidate, so the next read serves
	// it from the stored rows without another provider round-trip.
	if elapsed := answer("121"); elapsed > 2*time.Second {
		t.Fatalf("catalog detail after completion took %v", elapsed)
	}
	if snap := fake.snapshot(); snap.discoveries != 1 {
		t.Fatalf("discoveries = %d after the memo window, want 1", snap.discoveries)
	}
}

func TestCatalogVersionsRouteDoesNotDiscover(t *testing.T) {
	deps, base := catalogDeps(t)
	fake := &fakePreplayCatalog{fakeCatalog: base,
		detail: catalogpkg.ItemDetail{ContentID: "movie:heat-1995", Type: "movie",
			Versions: []catalogpkg.FileVersion{{FileID: 120, FilePath: "virtual://movie/tt0113277", AddedAt: fixedTime()}}},
		hasVirtual: true}
	deps.CatalogItems = fake
	deps.VirtualCandidatesDiscovery = fake
	deps.PreplayDiscoveryEnabled = func(context.Context) bool { return true }
	rec := do(t, newTestHandler(t, deps), http.MethodGet, "/api/v2/catalog/items/movie:heat-1995/versions", "", viewerHeaders())
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if fake.discoveries != 0 {
		t.Fatal("versions route initiated pre-play discovery")
	}
}

func TestListCatalogItems(t *testing.T) {
	deps, fake := catalogDeps(t)
	h := newTestHandler(t, deps)
	rec := do(t, h, http.MethodGet, "/api/v2/catalog?limit=2&sort=-release_date&content_rating=R&content_rating=PG-13&year_min=1990&q=heat&library_id=1&image_size=small", "", viewerHeaders())
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var body struct {
		Items []map[string]json.RawMessage `json:"items"`
		Page  PageInfo                     `json:"page"`
		Total int                          `json:"total"`
		Diag  json.RawMessage              `json:"search_diagnostics"`
		Sort  json.RawMessage              `json:"effective_sort"`
	}
	decodeJSON(t, rec.Body, &body)
	if len(body.Items) != 2 || body.Total != 3 || !body.Page.HasMore || body.Page.NextCursor == "" || string(body.Sort) != `{"field":"title","order":"asc"}` || !strings.Contains(string(body.Diag), `"provider":"postgres"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if string(body.Items[0]["content_id"]) != `"movie:heat-1995"` || string(body.Items[0]["genres"]) != `["Crime"]` || string(body.Items[0]["keywords"]) != `[]` {
		t.Errorf("card = %v", body.Items[0])
	}
	req := fake.lastReq
	if req.Limit != 2 || req.Offset != 0 || req.SearchQuery != "heat" || req.Query.Sort.Field != "release_date" || req.Query.Sort.Order != "desc" || len(req.Query.LibraryIDs) != 1 || req.Query.LibraryIDs[0] != 1 {
		t.Fatalf("seam request = %+v", req)
	}
	ratings := 0
	for _, g := range req.Query.Groups {
		for _, r := range g.Rules {
			if r.Field == "content_rating" {
				ratings++
			}
		}
	}
	if ratings != 2 || fake.lastViewer.Access.ImageSize != "small" || fake.lastViewer.ProfileID != "p-owner" {
		t.Fatalf("ratings = %d viewer = %+v", ratings, fake.lastViewer)
	}
	// The next page resumes at the offset and pins the first page's snapshot.
	rec = do(t, h, http.MethodGet, "/api/v2/catalog?limit=2&sort=-release_date&content_rating=R&content_rating=PG-13&year_min=1990&q=heat&library_id=1&image_size=small&cursor="+body.Page.NextCursor, "", viewerHeaders())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"has_more":false`) || fake.lastReq.Offset != 2 || fake.lastReq.SnapshotAt == nil {
		t.Fatalf("second page: %d %s offset=%d snapshot=%v", rec.Code, rec.Body.String(), fake.lastReq.Offset, fake.lastReq.SnapshotAt)
	}
	// The cursor is bound to the filters.
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog?limit=2&cursor="+body.Page.NextCursor, "", viewerHeaders()), TypeInvalidCursor)
	// group=work reaches the seam as the grouped flag.
	rec = do(t, h, http.MethodGet, "/api/v2/catalog?group=work", "", viewerHeaders())
	if rec.Code != 200 || !fake.lastGroup {
		t.Fatalf("group=work: %d grouped=%v", rec.Code, fake.lastGroup)
	}
	// Validation: an unknown sort field, a source missing its required id, a
	// comma list where repeated keys are required, and a bad library id.
	p := requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog?sort=bogus", "", viewerHeaders()), TypeValidationFailed)
	if len(p.Errors) != 1 || p.Errors[0].Location != "query.sort" {
		t.Fatalf("errors = %+v", p.Errors)
	}
	p = requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog?source=section", "", viewerHeaders()), TypeValidationFailed)
	if len(p.Errors) != 1 || p.Errors[0].Location != "query.section_id" {
		t.Fatalf("errors = %+v", p.Errors)
	}
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog?sort=title,year", "", viewerHeaders()), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog?limit=2&limit=3", "", viewerHeaders()), TypeMalformedRequest)
	// Class denial: no profile.
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog", "", bearer(memberToken)), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog", "", nil), TypeAuthenticationRequired)
	// Seam failures keep the v1 decision.
	fake.err = &handlers.APIError{Status: http.StatusNotFound, Code: "not_found", Message: "Catalog source not found"}
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog?source=user_collection&collection_id=x", "", viewerHeaders()), TypeNotFound)
	fake.err = &handlers.APIError{Status: http.StatusGatewayTimeout, Code: "search_timeout", Message: "Search took too long and was stopped"}
	rec = do(t, h, http.MethodGet, "/api/v2/catalog?q=slow", "", viewerHeaders())
	requireProblem(t, rec, TypeDependencyUnavailable)
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("timeout without Retry-After")
	}
	// Unwired service fails closed.
	deps.CatalogBrowse = nil
	requireProblem(t, do(t, newTestHandler(t, deps), http.MethodGet, "/api/v2/catalog", "", viewerHeaders()), TypeDependencyUnavailable)
}

func TestCatalogStructuredWindowScope(t *testing.T) {
	deps, fake := catalogDeps(t)
	h := newTestHandler(t, deps)
	request := map[string]any{
		"source": "query", "library_id": "1", "limit": 2, "sort": "title", "query_limit": 3,
		"groups": []any{map[string]any{"match": "all", "rules": []any{map[string]any{"field": "year", "op": "gte", "value": 1990}}}},
	}
	send := func() *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		return do(t, h, http.MethodPost, "/api/v2/catalog/query", string(body), viewerHeaders())
	}
	rec := send()
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var first struct {
		Window string   `json:"window_cursor"`
		Page   PageInfo `json:"page"`
	}
	decodeJSON(t, rec.Body, &first)
	if first.Window == "" || first.Page.NextCursor == "" || !fake.lastReq.CursorPaging || fake.lastReq.Query.Limit == nil || *fake.lastReq.Query.Limit != 3 {
		t.Fatalf("window or query cap missing: %s %+v", rec.Body.String(), fake.lastReq)
	}
	request["cursor"], request["skip_total"], request["seek"] = first.Window, true, 2
	if rec = send(); rec.Code != 200 || fake.lastReq.Seek == nil || *fake.lastReq.Seek != 2 || !fake.lastReq.SkipTotal {
		t.Fatalf("window jump: %d %s %+v", rec.Code, rec.Body.String(), fake.lastReq)
	}
	request["seek"] = 0
	if rec = send(); rec.Code != 200 || fake.lastReq.Offset != 0 || fake.lastReq.After != nil {
		t.Fatalf("return to first window: %d %s %+v", rec.Code, rec.Body.String(), fake.lastReq)
	}
	request["limit"] = 1
	requireProblem(t, send(), TypeInvalidCursor)
	request["limit"], request["library_id"] = 2, "2"
	requireProblem(t, send(), TypeInvalidCursor)
	request["library_id"], request["query_limit"] = "1", 4
	requireProblem(t, send(), TypeInvalidCursor)
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog?limit=2&cursor="+url.QueryEscape(first.Window), "", viewerHeaders()), TypeInvalidCursor)
	// JSON filters on GET receive the same field/operator validation as POST.
	bad := `[{"match":"all","rules":[{"field":"not_a_field","op":"is","value":true}]}]`
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog?groups="+url.QueryEscape(bad), "", viewerHeaders()), TypeValidationFailed)
	delete(request, "cursor")
	request["groups"] = json.RawMessage(bad)
	requireProblem(t, send(), TypeValidationFailed)
}

func TestCatalogUnsupportedStorageProblem(t *testing.T) {
	deps, fake := catalogDeps(t)
	h := newTestHandler(t, deps)
	for _, err := range []error{catalogpkg.ErrCatalogStorageUnsupported, &handlers.APIError{Status: http.StatusServiceUnavailable, Code: "catalog_storage_unsupported"}} {
		fake.err = err
		requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog", "", viewerHeaders()), TypeCapabilityUnsupported)
	}
}

func TestListAudiobookGroups(t *testing.T) {
	deps, fake := catalogDeps(t)
	h := newTestHandler(t, deps)
	rec := do(t, h, http.MethodGet, "/api/v2/catalog/audiobook-groups?library_id=3&group_by=author&limit=1&sort=count&q=fr", "", viewerHeaders())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"Frank Herbert"`) || !strings.Contains(rec.Body.String(), `"poster_urls":[]`) || !strings.Contains(rec.Body.String(), `"has_more":true`) {
		t.Fatal(rec.Code, rec.Body.String())
	}
	q := fake.lastGroups
	if !q.CursorPaging || q.LibraryID != 3 || q.GroupBy != catalogpkg.AudiobookGroupByAuthor || q.Limit != 1 || q.Sort != "count" || q.SearchPrefix != "fr" || !q.IncludeTotal {
		t.Fatalf("query = %+v", q)
	}
	var body struct {
		Page PageInfo `json:"page"`
	}
	decodeJSON(t, rec.Body, &body)
	rec = do(t, h, http.MethodGet, "/api/v2/catalog/audiobook-groups?library_id=3&group_by=author&limit=1&sort=count&q=fr&cursor="+body.Page.NextCursor, "", viewerHeaders())
	if rec.Code != 200 || (fake.lastGroups.After == nil || fake.lastGroups.After.GroupKey != "frank herbert" || fake.lastGroups.After.Value != 3) || !strings.Contains(rec.Body.String(), `"Ursula K. Le Guin"`) {
		t.Fatal(rec.Code, rec.Body.String())
	}
	for _, changed := range []string{
		"library_id=4&group_by=author&limit=1&sort=count&q=fr",
		"library_id=3&group_by=narrator&limit=1&sort=count&q=fr",
		"library_id=3&group_by=author&limit=2&sort=count&q=fr",
		"library_id=3&group_by=author&limit=1&sort=name&q=fr",
		"library_id=3&group_by=author&limit=1&sort=count&q=xx",
	} {
		requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/audiobook-groups?"+changed+"&cursor="+body.Page.NextCursor, "", viewerHeaders()), TypeInvalidCursor)
	}
	rec = do(t, h, http.MethodGet, "/api/v2/catalog/audiobook-groups?library_id=3&group_by=author&limit=1&sort=count&q=fr&skip_total=true&cursor="+body.Page.NextCursor, "", viewerHeaders())
	if rec.Code != 200 || fake.lastGroups.IncludeTotal {
		t.Fatal(rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodGet, "/api/v2/catalog/audiobook-groups?library_id=3&group_by=author&skip_total=true", "", viewerHeaders())
	if rec.Code != 200 || fake.lastGroups.IncludeTotal || !strings.Contains(rec.Body.String(), `"total_exact":false`) {
		t.Fatal(rec.Code, rec.Body.String())
	}
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/audiobook-groups?group_by=author", "", viewerHeaders()), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/audiobook-groups?library_id=3&group_by=publisher", "", viewerHeaders()), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/audiobook-groups?library_id=x&group_by=author", "", viewerHeaders()), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/audiobook-groups?library_id=3&group_by=author", "", bearer(memberToken)), TypeValidationFailed)
}

func TestCatalogFiltersAndFacetSearch(t *testing.T) {
	deps, fake := catalogDeps(t)
	h := newTestHandler(t, deps)
	rec := do(t, h, http.MethodGet, "/api/v2/catalog/filters?library_id=1&type=audiobook", "", viewerHeaders())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"technical":{"resolutions":["2160p"],"audio_languages":[],"subtitle_languages":["en"]}`) || !strings.Contains(rec.Body.String(), `"studios":[]`) {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if fake.lastReq.Query.MediaScope != "audiobook" || len(fake.lastReq.Query.LibraryIDs) != 1 {
		t.Fatalf("seam request = %+v", fake.lastReq)
	}
	rec = do(t, h, http.MethodGet, "/api/v2/catalog/filters?skip_technical=true", "", viewerHeaders())
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"technical"`) {
		t.Fatal(rec.Code, rec.Body.String())
	}
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/filters?skip_technical=1", "", viewerHeaders()), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/filters?source=person", "", viewerHeaders()), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/filters", "", bearer(memberToken)), TypeValidationFailed)

	rec = do(t, h, http.MethodGet, "/api/v2/catalog/filters/search?facet=author&q=fra&limit=1", "", viewerHeaders())
	if rec.Code != 200 || rec.Body.String() != `{"matches":["Frank Herbert"],"has_more":true,"values":[{"value":"Frank Herbert","count":6}],"values_has_more":true}`+"\n" {
		t.Fatal(rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodGet, "/api/v2/catalog/filters/search?facet=narrator&q=zz", "", viewerHeaders())
	if rec.Code != 200 || rec.Body.String() != `{"matches":[],"has_more":false,"values":[],"values_has_more":false}`+"\n" {
		t.Fatal(rec.Code, rec.Body.String())
	}
	// An empty q: no prefix matches, while values ranks the common ones.
	rec = do(t, h, http.MethodGet, "/api/v2/catalog/filters/search?facet=genre", "", viewerHeaders())
	if rec.Code != 200 || rec.Body.String() != `{"matches":[],"has_more":false,"values":[{"value":"Drama","count":9}],"values_has_more":true}`+"\n" {
		t.Fatal(rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodGet, "/api/v2/catalog/filters/search?facet=studio&library_id=3&library_ids=1&library_ids=3&library_ids=2&type=series", "", viewerHeaders())
	if rec.Code != 200 || !reflect.DeepEqual(fake.lastReq.Query.LibraryIDs, []int{1, 2, 3}) || fake.lastReq.Query.MediaScope != "series" {
		t.Fatalf("library_ids: %d %s seam request %+v", rec.Code, rec.Body.String(), fake.lastReq.Query)
	}
	rec = do(t, h, http.MethodGet, "/api/v2/catalog/filters?library_ids=2", "", viewerHeaders())
	if rec.Code != 200 || !reflect.DeepEqual(fake.lastReq.Query.LibraryIDs, []int{2}) {
		t.Fatalf("filters library_ids: %d seam request %+v", rec.Code, fake.lastReq.Query)
	}
	for _, query := range []string{"library_ids=0", "library_ids=x", "library_ids=01", "source=section&section_id=s1&library_id=1&library_ids=2"} {
		p := requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/filters/search?facet=studio&"+query, "", viewerHeaders()), TypeValidationFailed)
		if len(p.Errors) != 1 || p.Errors[0].Location != "query.library_ids" {
			t.Fatalf("%s: errors = %+v", query, p.Errors)
		}
	}
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/filters/search?q=fra", "", viewerHeaders()), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/filters/search?facet=color", "", viewerHeaders()), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/filters/search?facet=author&limit=500", "", viewerHeaders()), TypeValidationFailed)
	fake.err = &handlers.APIError{Status: http.StatusBadRequest, Code: "bad_request", Message: "unsupported facet"}
	p := requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/filters/search?facet=author", "", viewerHeaders()), TypeValidationFailed)
	if len(p.Errors) != 1 || p.Errors[0].Location != "query.facet" {
		t.Fatalf("errors = %+v", p.Errors)
	}
}

func TestQueryCatalogItems(t *testing.T) {
	deps, fake := catalogDeps(t)
	h := newTestHandler(t, deps)
	body := `{"match":"all","groups":[{"match":"any","rules":[{"field":"genre","op":"contains","value":"Crime"}]}],"sort":"title","order":"asc","library_id":"1","limit":10}`
	rec := do(t, h, http.MethodPost, "/api/v2/catalog/query?image_size=large", body, viewerHeaders())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"content_id":"movie:heat-1995"`) || !strings.Contains(rec.Body.String(), `"page":{"has_more":false}`) || !strings.Contains(rec.Body.String(), `"total":3`) {
		t.Fatal(rec.Code, rec.Body.String())
	}
	q := fake.lastReq
	if len(q.Query.LibraryIDs) != 1 || q.Query.LibraryIDs[0] != 1 || q.Limit != 10 || q.Query.Sort.Field != "title" || len(q.Query.Groups) != 1 || q.Query.Groups[0].Rules[0].Value != "Crime" || fake.lastViewer.Access.ImageSize != "large" {
		t.Fatalf("query = %+v viewer = %+v", q, fake.lastViewer)
	}
	rec = do(t, h, http.MethodPost, "/api/v2/catalog/query", `{}`, viewerHeaders())
	if rec.Code != 200 || fake.lastReq.Limit != 50 {
		t.Fatalf("defaults: %d limit=%d", rec.Code, fake.lastReq.Limit)
	}
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/catalog/query", `{"limit":500}`, viewerHeaders()), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/catalog/query", `{"library_id":"0"}`, viewerHeaders()), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/catalog/query", `{"unknown":1}`, viewerHeaders()), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/catalog/query", `{}`, bearer(memberToken)), TypeValidationFailed)
	fake.err = &handlers.APIError{Status: http.StatusBadRequest, Code: "bad_request", Message: "Invalid filter: unknown field"}
	p := requireProblem(t, do(t, h, http.MethodPost, "/api/v2/catalog/query", `{}`, viewerHeaders()), TypeValidationFailed)
	if len(p.Errors) != 1 || p.Errors[0].Location != "body.source" {
		t.Fatalf("errors = %+v", p.Errors)
	}
}

func TestGetCatalogItem(t *testing.T) {
	deps, fake := catalogDeps(t)
	fake.unreadableEpisodes = true
	h := newTestHandler(t, deps)
	rec := do(t, h, http.MethodGet, "/api/v2/catalog/items/movie:heat-1995?library_id=2&file_id=120", "", viewerHeaders())
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var body map[string]json.RawMessage
	decodeJSON(t, rec.Body, &body)
	for k, want := range map[string]string{
		"content_id": `"movie:heat-1995"`, "tagline": `"A Los Angeles crime saga"`, "keywords": `[]`, "crew": `[]`, "subtitles": `[]`,
		"user_state": `{"played":true,"is_favorite":true,"in_watchlist":false,"not_interested":false}`, "overlay_summary": `{"resolution":"4K"}`,
		"work_formats": `[{"type":"ebook","content_id":"ebook:heat","library_id":"2"}]`,
	} {
		if string(body[k]) != want {
			t.Errorf("%s = %s, want %s", k, body[k], want)
		}
	}
	if !strings.Contains(string(body["user_data"]), `"last_file_id":"120"`) || !strings.Contains(string(body["versions"]), `"file_id":"120"`) || !strings.Contains(string(body["versions"]), `"added_at":"2026-01-02T03:04:05.678Z"`) {
		t.Errorf("user_data = %s versions = %s", body["user_data"], body["versions"])
	}
	if string(body["status"]) != `""` {
		t.Errorf("status = %s; the detail service does not load the match state", body["status"])
	}
	if fake.lastViewer.Access.PresentationLibraryID == nil || *fake.lastViewer.Access.PresentationLibraryID != 2 || fake.lastViewer.Access.SelectedFileID != 120 || fake.lastViewer.Access.ScopeFilesToLibrary {
		t.Fatalf("viewer = %+v", fake.lastViewer.Access)
	}
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/items/movie:nope", "", viewerHeaders()), TypeNotFound)
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/items/movie:heat-1995?file_id=abc", "", viewerHeaders()), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/items/movie:heat-1995", "", bearer(memberToken)), TypeValidationFailed)

	rec = do(t, h, http.MethodGet, "/api/v2/catalog/items/movie:heat-1995/versions", "", viewerHeaders())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"items":[{"file_id":"120"`) {
		t.Fatal(rec.Code, rec.Body.String())
	}
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/items/movie:nope/versions", "", viewerHeaders()), TypeNotFound)

	rec = do(t, h, http.MethodGet, "/api/v2/catalog/items/series:berserk/manga-files", "", viewerHeaders())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"folder_paths":["/media/manga/Berserk"]`) || !strings.Contains(rec.Body.String(), `"file_name":"c001.cbz"`) {
		t.Fatal(rec.Code, rec.Body.String())
	}
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/items/movie:heat-1995/manga-files", "", viewerHeaders()), TypeNotFound)

	rec = do(t, h, http.MethodGet, "/api/v2/catalog/items/series:severance-S01/episodes", "", viewerHeaders())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"air_date":"2022-02-18"`) || !strings.Contains(rec.Body.String(), `"files":[{"file_id":"201"`) || !strings.Contains(rec.Body.String(), `"user_data":{"watched_count":1,"unplayed_count":0,"in_progress_count":0,"played":true}`) {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var episodePage struct {
		Items []struct {
			Files []map[string]json.RawMessage `json:"files"`
		} `json:"items"`
	}
	decodeJSON(t, rec.Body, &episodePage)
	if len(episodePage.Items) != 2 || len(episodePage.Items[0].Files) != 2 ||
		string(episodePage.Items[0].Files[0]["unreadable"]) != "true" || episodePage.Items[0].Files[1]["unreadable"] != nil {
		t.Fatalf("episode file readability = %+v", episodePage)
	}
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/items/series:nope/episodes", "", viewerHeaders()), TypeNotFound)
}

// TestGetCatalogItemResolvesDeviceAudio covers the device-scoped audio
// override on item detail and its versions: the declared X-Silo-Device-Id
// reaches the detail service, so the device's French override wins, and a
// request without the header answers the profile's English.
func TestGetCatalogItemResolvesDeviceAudio(t *testing.T) {
	deps, fake := catalogDeps(t)
	h := newTestHandler(t, deps)
	type version struct {
		EffectiveAudioTrackIndex *int   `json:"effective_audio_track_index"`
		EffectiveAudioLanguage   string `json:"effective_audio_language"`
	}
	for _, tc := range []struct {
		name, header, wantDevice, wantLang string
		wantIndex                          int
	}{
		{"device override", fakeDeviceAudioOverride, fakeDeviceAudioOverride, "fr", 2},
		{"device id trimmed", " " + fakeDeviceAudioOverride + " ", fakeDeviceAudioOverride, "fr", 2},
		{"other device", "tv-1", "tv-1", "en", 1},
		{"no header", "", "", "en", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := viewerHeaders()
			if tc.header != "" {
				headers = with(headers, deviceIDHeader, tc.header)
			}
			check := func(path string, versions func([]byte) []version) {
				t.Helper()
				rec := do(t, h, http.MethodGet, path, "", headers)
				if rec.Code != http.StatusOK {
					t.Fatal(rec.Code, rec.Body.String())
				}
				if fake.lastViewer.Access.DeviceID != tc.wantDevice {
					t.Fatalf("%s device = %q, want %q", path, fake.lastViewer.Access.DeviceID, tc.wantDevice)
				}
				got := versions(rec.Body.Bytes())
				if len(got) != 1 || got[0].EffectiveAudioTrackIndex == nil || *got[0].EffectiveAudioTrackIndex != tc.wantIndex || got[0].EffectiveAudioLanguage != tc.wantLang {
					t.Fatalf("%s effective audio = %+v, want %s #%d", path, got, tc.wantLang, tc.wantIndex)
				}
			}
			check("/api/v2/catalog/items/movie:heat-1995", func(b []byte) []version {
				var body struct {
					Versions []version `json:"versions"`
				}
				if err := json.Unmarshal(b, &body); err != nil {
					t.Fatal(err)
				}
				return body.Versions
			})
			check("/api/v2/catalog/items/movie:heat-1995/versions", func(b []byte) []version {
				var body struct {
					Items []version `json:"items"`
				}
				if err := json.Unmarshal(b, &body); err != nil {
					t.Fatal(err)
				}
				return body.Items
			})
		})
	}
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/items/movie:heat-1995", "", with(viewerHeaders(), deviceIDHeader, "phone-1,phone-1")), TypeValidationFailed)
}

func TestSeriesSeasons(t *testing.T) {
	deps, _ := catalogDeps(t)
	h := newTestHandler(t, deps)
	rec := do(t, h, http.MethodGet, "/api/v2/catalog/series/series:severance/seasons", "", viewerHeaders())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"items":[{"content_id":"series:severance-S01","play_content_id":"episode:severance-s01e02"`) || !strings.Contains(rec.Body.String(), `"air_date":"2022-02-18"`) {
		t.Fatal(rec.Code, rec.Body.String())
	}
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/series/series:nope/seasons", "", viewerHeaders()), TypeNotFound)
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/series/series:severance/seasons", "", bearer(memberToken)), TypeValidationFailed)

	rec = do(t, h, http.MethodGet, "/api/v2/catalog/series/series:severance/seasons/1", "", viewerHeaders())
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), `{"content_id":"series:severance-S01"`) || !strings.Contains(rec.Body.String(), `"episode_count":9`) {
		t.Fatal(rec.Code, rec.Body.String())
	}
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/series/series:severance/seasons/2", "", viewerHeaders()), TypeNotFound)
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/series/series:severance/seasons/one", "", viewerHeaders()), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/series/series:severance/seasons/-1", "", viewerHeaders()), TypeValidationFailed)
}

func TestListCatalogItemsPreservesResolvedSourceOrderCursors(t *testing.T) {
	deps, fake := catalogDeps(t)
	fake.sourceOrder = true
	h := newTestHandler(t, deps)
	path := "/api/v2/catalog?source=library_collection&collection_id=collection&limit=1"
	first := do(t, h, http.MethodGet, path, "", viewerHeaders())
	if first.Code != 200 {
		t.Fatal(first.Code, first.Body.String())
	}
	var body struct {
		Page         PageInfo `json:"page"`
		WindowCursor string   `json:"window_cursor"`
	}
	decodeJSON(t, first.Body, &body)
	for _, query := range []string{"&cursor=" + body.Page.NextCursor, "&cursor=" + body.WindowCursor + "&seek=1"} {
		response := do(t, h, http.MethodGet, path+query, "", viewerHeaders())
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
		if fake.lastReq.ResolvedSort == nil || *fake.lastReq.ResolvedSort != (catalogpkg.QuerySort{}) {
			t.Fatalf("source-order sentinel lost: %+v", fake.lastReq.ResolvedSort)
		}
	}
}

func TestSeriesSeasonsArtwork(t *testing.T) {
	deps, _ := catalogDeps(t)
	h := newTestHandler(t, deps)
	capability := do(t, h, http.MethodGet, Prefix+"/images/capabilities", "", viewerHeaders())
	var discovery ImageCapabilities
	decodeJSON(t, capability.Body, &discovery)
	if capability.Code != http.StatusOK || discovery.SeasonListArtworkParam != "include_artwork" {
		t.Fatal(capability.Code, discovery)
	}
	var baseline Season
	for _, value := range []string{"", "true", "false"} {
		t.Run("include_artwork="+value, func(t *testing.T) {
			path := Prefix + "/catalog/series/series:severance/seasons"
			if value != "" {
				path += "?" + discovery.SeasonListArtworkParam + "=" + value
			}
			rec := do(t, h, http.MethodGet, path, "", viewerHeaders())
			if rec.Code != http.StatusOK {
				t.Fatal(rec.Code, rec.Body.String())
			}
			var body SeasonCollection
			decodeJSON(t, rec.Body, &body)
			if len(body.Items) != 1 {
				t.Fatal(body)
			}
			season := body.Items[0]
			if value == "false" {
				if strings.Contains(rec.Body.String(), `"poster_url"`) || strings.Contains(rec.Body.String(), `"poster_thumbhash"`) {
					t.Fatal("artwork fields present", rec.Body.String())
				}
			} else if season.PosterURL == "" || season.PosterThumbhash == "" {
				t.Fatal("artwork absent", season)
			}
			season.PosterURL = ""
			season.PosterThumbhash = ""
			if value == "" {
				baseline = season
			} else if !reflect.DeepEqual(baseline, season) {
				t.Fatal("non-artwork metadata changed", season)
			}
		})
	}
	requireProblem(t, do(t, h, http.MethodGet, Prefix+"/catalog/series/series:severance/seasons?include_artwork=invalid", "", viewerHeaders()), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodGet, Prefix+"/catalog/series/series:severance/seasons/1?include_artwork=false", "", viewerHeaders()), TypeValidationFailed)
}

func TestCatalogStructuredSearchRelevance(t *testing.T) {
	for _, groups := range []string{`[]`, `[{"match":"all","rules":[{"field":"year","op":"gte","value":2000}]}]`} {
		t.Run(groups, func(t *testing.T) {
			deps, fake := catalogDeps(t)
			h := newTestHandler(t, deps)
			rec := do(t, h, http.MethodPost, "/api/v2/catalog/query", `{"source":"query","q":"heat","sort":"relevance","order":"desc","groups":`+groups+`}`, viewerHeaders())
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if fake.lastReq.Query.Sort.Field != "relevance" || fake.lastReq.SearchQuery != "heat" {
				t.Fatalf("lost search semantics: %+v", fake.lastReq)
			}
		})
	}
	deps, _ := catalogDeps(t)
	h := newTestHandler(t, deps)
	for _, body := range []string{
		`{"q":"heat","groups":[{"match":"all","rules":[{"field":"not-a-field","op":"eq","value":1}]}]}`,
		`{"source":"query","sort":"relevance","groups":[]}`,
		`{"source":"favorites","q":"heat","sort":"relevance","groups":[]}`,
	} {
		requireProblem(t, do(t, h, http.MethodPost, "/api/v2/catalog/query", body, viewerHeaders()), TypeValidationFailed)
	}
}

type fakeCatalogSettings map[string]string

func (f fakeCatalogSettings) Get(_ context.Context, key string) (string, error) { return f[key], nil }

// The library scope is opt-in through catalog.scope_versions_to_library and
// only reaches a read that named a library.
func TestGetCatalogItemScopesVersionsToLibraryWhenEnabled(t *testing.T) {
	deps, fake := catalogDeps(t)
	deps.CatalogSettings = fakeCatalogSettings{"catalog.scope_versions_to_library": "true"}
	h := newTestHandler(t, deps)

	if rec := do(t, h, http.MethodGet, "/api/v2/catalog/items/movie:heat-1995?library_id=2", "", viewerHeaders()); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if !fake.lastViewer.Access.ScopeFilesToLibrary || fake.lastViewer.Access.PresentationLibraryID == nil {
		t.Fatalf("viewer = %+v", fake.lastViewer.Access)
	}

	if rec := do(t, h, http.MethodGet, "/api/v2/catalog/items/movie:heat-1995", "", viewerHeaders()); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if fake.lastViewer.Access.ScopeFilesToLibrary || fake.lastViewer.Access.PresentationLibraryID != nil {
		t.Fatalf("viewer = %+v", fake.lastViewer.Access)
	}

	deps.CatalogSettings = nil
	h = newTestHandler(t, deps)
	if rec := do(t, h, http.MethodGet, "/api/v2/catalog/items/movie:heat-1995?library_id=2", "", viewerHeaders()); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if fake.lastViewer.Access.ScopeFilesToLibrary {
		t.Fatalf("viewer = %+v", fake.lastViewer.Access)
	}
}

// TestGetCatalogItemCarriesCollections pins the additive item detail
// "collections" row: a movie's detail fetches the reverse lookup with its own
// content id under the viewer's resolved access filter, an episode and a
// season resolve to their parent series, the chips carry poster and count, an
// item in none answers an empty array, never null, and a lookup failure leaves
// the detail a success with an empty row.
func TestGetCatalogItemCarriesCollections(t *testing.T) {
	deps, _ := catalogDeps(t)
	fake := deps.LibraryCollections.(*fakeLibraryViews)
	fake.collections = []handlers.ItemCollectionView{{ID: "alpha", Title: "Dune Saga", PosterURL: "https://cdn.example.test/alpha.jpg", ItemCount: 3}}
	h := newTestHandler(t, deps)

	rec := do(t, h, http.MethodGet, "/api/v2/catalog/items/movie:heat-1995", "", viewerHeaders())
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var movie CatalogItemDetail
	decodeJSON(t, rec.Body, &movie)
	if fake.lastCollections != "movie:heat-1995" {
		t.Fatalf("lookup = %q, want the movie id", fake.lastCollections)
	}
	// The viewer's resolved access filter reaches the lookup; the fake catalog
	// resolves allowed libraries {1, 2}.
	if got := fake.lastCollectionsAccess.AllowedLibraryIDs; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("lookup access = %+v, want allowed libraries [1 2]", fake.lastCollectionsAccess)
	}
	if len(movie.Collections) != 1 || movie.Collections[0].ID != "alpha" || movie.Collections[0].Title != "Dune Saga" ||
		movie.Collections[0].ItemCount != 3 || movie.Collections[0].PosterURL != "https://cdn.example.test/alpha.jpg" {
		t.Fatalf("movie collections = %+v", movie.Collections)
	}

	rec = do(t, h, http.MethodGet, "/api/v2/catalog/items/episode:severance-s01e01", "", viewerHeaders())
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var episode CatalogItemDetail
	decodeJSON(t, rec.Body, &episode)
	if fake.lastCollections != "series:severance" {
		t.Fatalf("episode lookup = %q, want the parent series id", fake.lastCollections)
	}
	if len(episode.Collections) != 1 || episode.Collections[0].ID != "alpha" {
		t.Fatalf("episode collections = %+v", episode.Collections)
	}

	// A season answers its series' collections the same way.
	rec = do(t, h, http.MethodGet, "/api/v2/catalog/items/series:severance-S01", "", viewerHeaders())
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var season CatalogItemDetail
	decodeJSON(t, rec.Body, &season)
	if fake.lastCollections != "series:severance" {
		t.Fatalf("season lookup = %q, want the parent series id", fake.lastCollections)
	}
	if len(season.Collections) != 1 || season.Collections[0].ID != "alpha" {
		t.Fatalf("season collections = %+v", season.Collections)
	}

	// A series answers its own collections by its own id.
	rec = do(t, h, http.MethodGet, "/api/v2/catalog/items/series:severance", "", viewerHeaders())
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var series CatalogItemDetail
	decodeJSON(t, rec.Body, &series)
	if fake.lastCollections != "series:severance" {
		t.Fatalf("series lookup = %q, want the series id itself", fake.lastCollections)
	}
	if len(series.Collections) != 1 || series.Collections[0].ID != "alpha" {
		t.Fatalf("series collections = %+v", series.Collections)
	}

	// An item in no collection answers an empty array, not null.
	fake.collections = nil
	rec = do(t, h, http.MethodGet, "/api/v2/catalog/items/movie:heat-1995", "", viewerHeaders())
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"collections":[]`) {
		t.Fatalf("empty collections = %s", rec.Body.String())
	}

	// A lookup failure is best-effort: the detail succeeds with an empty row.
	fake.err = errors.New("boom")
	rec = do(t, h, http.MethodGet, "/api/v2/catalog/items/movie:heat-1995", "", viewerHeaders())
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"collections":[]`) {
		t.Fatalf("failed lookup collections = %d %s", rec.Code, rec.Body.String())
	}
}

// TestListCatalogItemsVideoWithEpisodesScope pins the search-only scope on
// the v2 browse grammar: the query source records it for text search, the
// filters read narrows it to video, and a source without text search refuses
// it at the type parameter (body.type on the structured form).
func TestListCatalogItemsVideoWithEpisodesScope(t *testing.T) {
	deps, fake := catalogDeps(t)
	h := newTestHandler(t, deps)
	rec := do(t, h, http.MethodGet, "/api/v2/catalog?q=heat&type=video_with_episodes", "", viewerHeaders())
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if fake.lastReq.SearchMediaScope != catalogpkg.MediaScopeVideoWithEpisodes || fake.lastReq.Query.MediaScope != catalogpkg.MediaScopeVideo {
		t.Fatalf("seam scopes = search %q query %q", fake.lastReq.SearchMediaScope, fake.lastReq.Query.MediaScope)
	}
	rec = do(t, h, http.MethodGet, "/api/v2/catalog/filters?type=video_with_episodes", "", viewerHeaders())
	if rec.Code != 200 || fake.lastReq.Query.MediaScope != catalogpkg.MediaScopeVideo {
		t.Fatalf("filters: %d %s scope %q", rec.Code, rec.Body.String(), fake.lastReq.Query.MediaScope)
	}
	p := requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog?source=favorites&type=video_with_episodes", "", viewerHeaders()), TypeValidationFailed)
	if len(p.Errors) != 1 || p.Errors[0].Location != "query.type" {
		t.Fatalf("errors = %+v", p.Errors)
	}
	p = requireProblem(t, do(t, h, http.MethodPost, "/api/v2/catalog/query", `{"source":"watchlist","type":"video_with_episodes"}`, viewerHeaders()), TypeValidationFailed)
	if len(p.Errors) != 1 || p.Errors[0].Location != "body.type" {
		t.Fatalf("errors = %+v", p.Errors)
	}
	p = requireProblem(t, do(t, h, http.MethodPost, "/api/v2/catalog/query", `{"source":"section","scope":"home","section_id":"s","type":"video_with_episodes"}`, viewerHeaders()), TypeValidationFailed)
	if len(p.Errors) != 1 || p.Errors[0].Location != "body.type" {
		t.Fatalf("section errors = %+v", p.Errors)
	}
}

// An episode states its ratings as a title page does: on a 0-100 score with
// the source's own display, IMDb before TMDB. A stored 0 (unrated) or a value
// off the 0-10 scale is dropped, and an episode with no rating omits the field.
func TestEpisodesOfStatesRatings(t *testing.T) {
	episodes := episodesOf([]handlers.EpisodeView{
		{ContentID: "episode:1", RatingIMDB: new(8.7), RatingTMDB: new(8.25)},
		{ContentID: "episode:2", RatingIMDB: new(0.0), RatingTMDB: new(42.0)},
		{ContentID: "episode:3"},
	})
	want := []CatalogRating{
		{Source: "imdb", Name: "IMDb", Score: 87, Display: "8.7"},
		{Source: "tmdb", Name: "TMDB", Score: 82.5, Display: "8.3"},
	}
	if !reflect.DeepEqual(episodes[0].Ratings, want) {
		t.Fatalf("ratings = %+v, want %+v", episodes[0].Ratings, want)
	}
	for _, ep := range episodes[1:] {
		body, err := json.Marshal(ep)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["ratings"]; ok {
			t.Fatalf("%s carries ratings: %s", ep.ContentID, body)
		}
	}
}

// The episode's rating columns stay off the frozen /api/v1 wire.
func TestEpisodeViewKeepsRatingsOffV1(t *testing.T) {
	body, err := json.Marshal(handlers.EpisodeView{ContentID: "episode:1", RatingIMDB: new(8.7), RatingTMDB: new(8.2)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "rating") {
		t.Fatalf("v1 episode = %s", body)
	}
}
