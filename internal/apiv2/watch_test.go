package apiv2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
	catalogpkg "github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

// fakeWatch is the watch seam: one playable movie, one series (not directly
// playable), everything else unknown.
type fakeWatch struct {
	filters   []catalogpkg.AccessFilter
	marks     []fakeMark
	clears    []fakeMark
	err       error
	trickplay *catalogpkg.TrickplayGrid
}

type fakeMark struct {
	userID        int
	profileID     string
	contentID     string
	played        bool
	recordRewatch bool
}

func (f *fakeWatch) ContextAccessFilter(_ context.Context, opts handlers.AccessFilterOptions) (catalogpkg.AccessFilter, error) {
	filter := catalogpkg.AccessFilter{SelectedFileID: opts.SelectedFileID, PresentationLibraryID: opts.PresentationLibraryID, ImageSize: opts.ImageSize, DeviceID: opts.DeviceID}
	f.filters = append(f.filters, filter)
	return filter, nil
}

func (f *fakeWatch) WatchDetail(_ context.Context, userID int, profileID, contentID string, _ catalogpkg.AccessFilter) (*catalogpkg.WatchDetail, error) {
	if f.err != nil {
		return nil, f.err
	}
	switch contentID {
	case "series:heat":
		return nil, &handlers.APIError{Status: http.StatusBadRequest, Code: "invalid_watch_target", Message: "Content is not directly playable"}
	case "movie:heat-1995":
		three := 3
		ranking := &catalogpkg.VirtualRanking{
			ProfileLabel: "4K HDR",
			Source:       catalogpkg.VirtualRankingSourceProfile,
			Criteria: []catalogpkg.VirtualRankingCriterion{
				{Attribute: "score", Direction: "desc"},
				{Attribute: "resolution", Direction: "desc"},
			},
		}
		detail := &catalogpkg.WatchDetail{
			ContentID: contentID, Type: "movie", Title: "Heat", Year: 1995,
			EffectiveSubtitleLanguage: "eng", HasEffectiveSubtitleLang: true,
			VirtualRanking: ranking,
			PosterURL:      "/api/images/poster/heat.jpg?size=medium", PosterThumbhash: "poster-hash",
			BackdropURL: "/api/images/backdrop/heat.jpg?size=medium", BackdropThumbhash: "backdrop-hash",
			Versions: []catalogpkg.FileVersion{{
				FileID: 42, Resolution: "1080p", CodecVideo: "h264", CodecAudio: "eac3", Container: "mkv", FileSize: 1024, Duration: 10200, Bitrate: 8000000,
				AddedAt:        time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
				VideoTracks:    []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}},
				AudioTracks:    []models.AudioTrack{{Language: "eng", Codec: "eac3", Channels: 6, Default: true}},
				Chapters:       []catalogpkg.VersionChapter{{Index: 1, Title: "Opening", StartSeconds: 0, EndSeconds: 300, Source: "embedded"}},
				Intro:          &catalogpkg.Marker{Start: 0, End: 90},
				MarkerPreviews: []catalogpkg.VersionMarkerPreview{{MarkerSegment: models.MarkerSegment{Kind: "intro", StartSeconds: 0, EndSeconds: 90}, ThumbnailURL: "https://images.example.test/intro.webp", ThumbnailThumbhash: "AQID", ThumbnailCaptureSeconds: 0}},
				Trickplay:      f.trickplay,
			}},
			PlaybackVariants: []catalogpkg.PlaybackVariant{{VariantID: "v1", PartCount: 1, DefaultFileID: 42, VirtualRanking: ranking, Parts: []catalogpkg.PlaybackVariantPart{{PartIndex: 0, DefaultFileID: 42}}}},
			Subtitles:        []catalogpkg.SubtitleInfo{{Source: "embedded", Language: "eng"}},
			Credits:          &catalogpkg.Marker{Start: 10000, End: 10200},
		}
		if profileID != "" {
			detail.UserData = &catalogpkg.SeasonUserData{PositionSeconds: 1325.5, DurationSeconds: 10200, IsInProgress: true, LastFileID: &three}
		}
		_ = userID
		return detail, nil
	}
	return nil, &handlers.APIError{Status: http.StatusNotFound, Code: "not_found", Message: "Watch target not found"}
}

func (f *fakeWatch) SetWatchedState(_ context.Context, userID int, profileID, contentID string, played, recordRewatch bool, _ catalogpkg.AccessFilter) (handlers.WatchedStateView, error) {
	if f.err != nil {
		return handlers.WatchedStateView{}, f.err
	}
	if contentID != "movie:heat-1995" && contentID != "series:heat" {
		return handlers.WatchedStateView{}, &handlers.APIError{Status: http.StatusNotFound, Code: "not_found", Message: "Item not found"}
	}
	f.marks = append(f.marks, fakeMark{userID: userID, profileID: profileID, contentID: contentID, played: played, recordRewatch: recordRewatch})
	return handlers.WatchedStateView{ContentID: contentID, Type: "movie", AffectedCount: 1, Played: played}, nil
}

func (f *fakeWatch) ClearResumePosition(_ context.Context, userID int, profileID, contentID string, _ catalogpkg.AccessFilter) error {
	if f.err != nil {
		return f.err
	}
	switch contentID {
	case "series:heat":
		return &handlers.APIError{Status: http.StatusBadRequest, Code: "invalid_watch_target", Message: "Only a movie or an episode has a resume position"}
	case "movie:heat-1995":
		f.clears = append(f.clears, fakeMark{userID: userID, profileID: profileID, contentID: contentID})
		return nil
	}
	return &handlers.APIError{Status: http.StatusNotFound, Code: "not_found", Message: "Item not found"}
}

func watchDeps(watch WatchService) Dependencies {
	deps := pilotDeps(nil, nil)
	deps.Watch = watch
	return deps
}

func TestGetWatchStateRejects(t *testing.T) {
	h := newTestHandler(t, watchDeps(&fakeWatch{}))
	owner := with(bearer(memberToken), "X-Profile-Id", "p-owner")

	p := requireProblem(t, do(t, h, http.MethodGet, "/api/v2/watch/series:heat", "", owner), TypeValidationFailed)
	if len(p.Errors) != 1 || p.Errors[0].Location != "path.id" {
		t.Fatalf("errors = %+v", p.Errors)
	}
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/watch/movie:missing", "", owner), TypeNotFound)
	p = requireProblem(t, do(t, h, http.MethodGet, "/api/v2/watch/movie:heat-1995?file_id=abc", "", owner), TypeValidationFailed)
	if len(p.Errors) != 1 || p.Errors[0].Location != "query.file_id" {
		t.Fatalf("errors = %+v", p.Errors)
	}
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/watch/movie:heat-1995?image_size=huge", "", owner), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/watch/movie:heat-1995?fileId=1", "", owner), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/watch/movie:heat-1995", "", nil), TypeAuthenticationRequired)

	off := newTestHandler(t, parityDeps(false))
	requireProblem(t, do(t, off, http.MethodGet, "/api/v2/watch/movie:heat-1995", "", owner), TypeDependencyUnavailable)
}

// fakeWatchFilters fails only the access-filter read: the watch detail and
// mark paths need a filter before they can do anything else, so both mask the
// same failure. It keeps WatchDetail usable for content the fake needs.
type fakeWatchFilters struct {
	err error
}

func (f *fakeWatchFilters) ContextAccessFilter(context.Context, handlers.AccessFilterOptions) (catalogpkg.AccessFilter, error) {
	return catalogpkg.AccessFilter{}, f.err
}

func (f *fakeWatchFilters) WatchDetail(_ context.Context, userID int, profileID, contentID string, _ catalogpkg.AccessFilter) (*catalogpkg.WatchDetail, error) {
	return nil, &handlers.APIError{Status: http.StatusNotFound, Code: "not_found", Message: "Watch target not found"}
}

func (f *fakeWatchFilters) SetWatchedState(_ context.Context, userID int, profileID, contentID string, played bool, _ catalogpkg.AccessFilter) (handlers.WatchedStateView, error) {
	return handlers.WatchedStateView{}, &handlers.APIError{Status: http.StatusNotFound, Code: "not_found", Message: "Item not found"}
}

func (f *fakeWatchFilters) ClearResumePosition(context.Context, int, string, string, catalogpkg.AccessFilter) error {
	return nil
}

func TestMarkWatched(t *testing.T) {
	watch := &fakeWatch{}
	h := newTestHandler(t, watchDeps(watch))
	owner := with(bearer(memberToken), "X-Profile-Id", "p-owner")

	rec := do(t, h, http.MethodPost, "/api/v2/watched/series:heat", "", owner)
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodDelete, "/api/v2/watched/movie:heat-1995", "", owner)
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	// A v2 mark asks for a rewatch to be recorded; an unmark does not.
	if len(watch.marks) != 2 || watch.marks[0] != (fakeMark{userID: 1, profileID: "p-owner", contentID: "series:heat", played: true, recordRewatch: true}) ||
		watch.marks[1] != (fakeMark{userID: 1, profileID: "p-owner", contentID: "movie:heat-1995", played: false}) {
		t.Fatalf("marks = %+v", watch.marks)
	}

	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/watched/movie:missing", "", owner), TypeNotFound)
	requireProblem(t, do(t, h, http.MethodDelete, "/api/v2/watched/movie:missing", "", owner), TypeNotFound)
	// The mark needs a profile, as v1's RequireProfile group.
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/watched/movie:heat-1995", "", bearer(memberToken)), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodDelete, "/api/v2/watched/movie:heat-1995", "", bearer(memberToken)), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/watched/movie:heat-1995", "", nil), TypeAuthenticationRequired)

	watch.err = &handlers.APIError{Status: http.StatusInternalServerError, Code: "internal_error", Message: "Failed to update watched state"}
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/watched/movie:heat-1995", "", owner), TypeInternalError)

	// An access-filter failure masks the same way on both watch paths: the
	// body reports the generic error without echoing the cause marker, and
	// the request log carries the marker under the same request id.
	filterWatch := &fakeWatchFilters{err: fmt.Errorf("watch access private-watch-marker: %w", context.Canceled)}
	filterHandler := newTestHandler(t, watchDeps(filterWatch))
	buf := captureLogs(t)
	rec = do(t, filterHandler, http.MethodGet, "/api/v2/watch/movie:heat-1995", "", owner)
	requireProblem(t, rec, TypeInternalError)
	if !strings.Contains(rec.Body.String(), "An unexpected error occurred.") || strings.Contains(rec.Body.String(), "private-watch-marker") {
		t.Fatalf("masked watch body = %s", rec.Body.String())
	}
	if line := buf.String(); !strings.Contains(line, "private-watch-marker") || !strings.Contains(line, `"request_id":"`+requestIDHeader(rec)+`"`) {
		t.Fatalf("masked watch log lacks marker or request id: %s", line)
	}
	buf.Reset()
	rec = do(t, filterHandler, http.MethodPost, "/api/v2/watched/movie:heat-1995", "", owner)
	requireProblem(t, rec, TypeInternalError)
	if !strings.Contains(rec.Body.String(), "An unexpected error occurred.") || strings.Contains(rec.Body.String(), "private-watch-marker") {
		t.Fatalf("masked mark body = %s", rec.Body.String())
	}
	if line := buf.String(); !strings.Contains(line, "private-watch-marker") || !strings.Contains(line, `"request_id":"`+requestIDHeader(rec)+`"`) {
		t.Fatalf("masked mark log lacks marker or request id: %s", line)
	}

	off := newTestHandler(t, parityDeps(false))
	requireProblem(t, do(t, off, http.MethodPost, "/api/v2/watched/movie:heat-1995", "", owner), TypeDependencyUnavailable)
}

func TestClearResumePosition(t *testing.T) {
	watch := &fakeWatch{}
	h := newTestHandler(t, watchDeps(watch))
	owner := with(bearer(memberToken), "X-Profile-Id", "p-owner")
	path := Prefix + "/progress/movie:heat-1995/position"

	rec := do(t, h, http.MethodDelete, path, "", owner)
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if len(watch.clears) != 1 || watch.clears[0] != (fakeMark{userID: 1, profileID: "p-owner", contentID: "movie:heat-1995"}) {
		t.Fatalf("clears = %+v", watch.clears)
	}

	// A season or series has no single resume position to clear.
	problem := requireProblem(t, do(t, h, http.MethodDelete, Prefix+"/progress/series:heat/position", "", owner), TypeValidationFailed)
	if len(problem.Errors) != 1 || problem.Errors[0].Location != "path.media_item_id" {
		t.Fatalf("errors = %+v", problem.Errors)
	}
	requireProblem(t, do(t, h, http.MethodDelete, Prefix+"/progress/movie:missing/position", "", owner), TypeNotFound)
	requireProblem(t, do(t, h, http.MethodDelete, path, "", bearer(memberToken)), TypeValidationFailed)
	requireProblem(t, do(t, h, http.MethodDelete, path, "", nil), TypeAuthenticationRequired)
	if len(watch.clears) != 1 {
		t.Fatalf("rejected requests cleared progress: %+v", watch.clears)
	}

	watch.err = &handlers.APIError{Status: http.StatusInternalServerError, Code: "internal_error", Message: "Failed to clear resume position"}
	requireProblem(t, do(t, h, http.MethodDelete, path, "", owner), TypeInternalError)

	off := newTestHandler(t, parityDeps(false))
	requireProblem(t, do(t, off, http.MethodDelete, path, "", owner), TypeDependencyUnavailable)
}

func TestGetProgressCapabilities(t *testing.T) {
	path := Prefix + "/progress/capabilities"
	owner := with(bearer(memberToken), "X-Profile-Id", "p-owner")
	h := newTestHandler(t, watchDeps(&fakeWatch{}))
	requireProblem(t, do(t, h, http.MethodGet, path, "", nil), TypeAuthenticationRequired)

	rec := do(t, h, http.MethodGet, path, "", owner)
	var out ProgressCapabilities
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || out.State != StateAvailable || out.Allowed == nil || !*out.Allowed || out.Revision == "" || !out.ClearResumePosition {
		t.Fatalf("capabilities: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != cachePrivateNoCache {
		t.Fatalf("Cache-Control = %q", got)
	}
	tag := rec.Header().Get("ETag")
	if cached := do(t, h, http.MethodGet, path, "", with(owner, "If-None-Match", tag)); tag == "" || cached.Code != http.StatusNotModified {
		t.Fatalf("revalidation with %q: %d", tag, cached.Code)
	}

	deps := watchDeps(&fakeWatch{})
	deps.Watch = nil
	rec = do(t, newTestHandler(t, deps), http.MethodGet, path, "", owner)
	out = ProgressCapabilities{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || out.State != StateNotConfigured || out.Allowed == nil || *out.Allowed || out.ClearResumePosition {
		t.Fatalf("unconfigured: %d %s", rec.Code, rec.Body.String())
	}
}

// A marker's confidence is optional because 0 is a rating: the field is absent when
// nobody rated the range, and present as 0 when a source rated it worthless. A client
// renders those differently, so the distinction has to survive the wire.
func TestWatchMarkerCarriesConfidenceOnlyWhenRated(t *testing.T) {
	rated := 0.0
	ratedBody, err := json.Marshal(watchMarkerOf(&catalogpkg.Marker{Start: 0, End: 90, Confidence: &rated}))
	if err != nil {
		t.Fatalf("marshal rated marker: %v", err)
	}

	var ratedFields map[string]json.RawMessage
	if err := json.Unmarshal(ratedBody, &ratedFields); err != nil {
		t.Fatalf("unmarshal rated marker: %v", err)
	}
	value, ok := ratedFields["confidence"]
	if !ok {
		t.Fatalf("a rated 0 was dropped: %s", ratedBody)
	}
	if string(value) != "0" {
		t.Fatalf("confidence = %s, want 0", value)
	}

	unratedBody, err := json.Marshal(watchMarkerOf(&catalogpkg.Marker{Start: 0, End: 90}))
	if err != nil {
		t.Fatalf("marshal unrated marker: %v", err)
	}

	var unratedFields map[string]json.RawMessage
	if err := json.Unmarshal(unratedBody, &unratedFields); err != nil {
		t.Fatalf("unmarshal unrated marker: %v", err)
	}
	if value, ok := unratedFields["confidence"]; ok {
		t.Fatalf("an unrated marker serialized confidence %s: %s", value, unratedBody)
	}
}

// TestWatchVirtualRankingMapping pins the wire projection directly: a
// profile-derived ranking carries the label and its criteria, the built-in
// default carries no label but the default keys, and local content (nil)
// omits the object entirely.
func TestWatchVirtualRankingMapping(t *testing.T) {
	profile := watchVirtualRankingOf(&catalogpkg.VirtualRanking{
		ProfileLabel: "4K HDR",
		Source:       catalogpkg.VirtualRankingSourceProfile,
		Criteria: []catalogpkg.VirtualRankingCriterion{
			{Attribute: "score", Direction: "desc"},
			{Attribute: "resolution", Direction: "desc"},
		},
	})
	if profile == nil || profile.ProfileLabel != "4K HDR" || profile.Source != "profile" {
		t.Fatalf("profile ranking = %+v", profile)
	}
	if len(profile.Criteria) != 2 || profile.Criteria[1].Attribute != "resolution" || profile.Criteria[1].Direction != "desc" {
		t.Fatalf("profile criteria = %+v", profile.Criteria)
	}

	defaultRanking := watchVirtualRankingOf(&catalogpkg.VirtualRanking{
		Source:   catalogpkg.VirtualRankingSourceDefault,
		Criteria: []catalogpkg.VirtualRankingCriterion{{Attribute: "score", Direction: "desc"}},
	})
	if defaultRanking == nil || defaultRanking.ProfileLabel != "" || defaultRanking.Source != "default" {
		t.Fatalf("default ranking = %+v", defaultRanking)
	}
	if defaultRanking.Criteria == nil || len(defaultRanking.Criteria) != 1 {
		t.Fatalf("default criteria = %+v", defaultRanking.Criteria)
	}

	if got := watchVirtualRankingOf(nil); got != nil {
		t.Fatalf("nil ranking mapped to %+v, want nil", got)
	}
}

// TestWatchVersionMatchedFormatsSerialization pins the wire contract on the
// watch version payload: a scored row carries the matched format names, and an
// unscored row omits the field entirely.
func TestWatchVersionMatchedFormatsSerialization(t *testing.T) {
	score := 850
	scored, err := json.Marshal(watchVersionOf(catalogpkg.FileVersion{
		FileID:         1,
		AddedAt:        time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		FormatScore:    &score,
		MatchedFormats: []string{"English", "Remux Tier 1"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(scored, &body); err != nil {
		t.Fatal(err)
	}
	formats, ok := body["matched_formats"].([]any)
	if !ok || len(formats) != 2 || formats[0] != "English" || formats[1] != "Remux Tier 1" {
		t.Fatalf("matched_formats = %v, want [English Remux Tier 1]", body["matched_formats"])
	}

	unscored, err := json.Marshal(watchVersionOf(catalogpkg.FileVersion{FileID: 2, AddedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unscored), "matched_formats") {
		t.Fatalf("unscored row carried matched_formats: %s", unscored)
	}
}

// fakeWatchVirtualDetail answers one episode whose versions carry the liveness
// mix the warm hook must filter: an unprobed virtual row, a probed virtual row,
// a third virtual row beyond the cap, a local row, and a durably failed one.
type fakeWatchVirtualDetail struct {
	fakeWatch
	detail catalogpkg.WatchDetail
}

func (f *fakeWatchVirtualDetail) WatchDetail(_ context.Context, _ int, _ string, _ string, _ catalogpkg.AccessFilter) (*catalogpkg.WatchDetail, error) {
	detail := f.detail
	return &detail, nil
}

// Native clients open an episode through getWatchState without declaring a
// file_id, so the selected-file hook warms nothing. The watch read must warm
// the virtual rows it lists, with the same cap and memo as the catalog leaf
// read: unprobed rows get the probe and the warm-up, probed rows only the
// warm-up, local and failed rows are skipped, and a repeat read schedules
// nothing again.
func TestWatchDetailWarmsVirtualVersions(t *testing.T) {
	probedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	watch := &fakeWatchVirtualDetail{detail: catalogpkg.WatchDetail{
		ContentID: "episode:severance-s1e1", Type: "episode",
		Versions: []catalogpkg.FileVersion{
			{FileID: 1, FilePath: "virtual://series/tvdb/1/1/1?result=a", AddedAt: fixedTime()},
			{FileID: 2, FilePath: "virtual://series/tvdb/1/1/1?result=b", AddedAt: fixedTime(), ProbedAt: &probedAt},
			{FileID: 3, FilePath: "virtual://series/tvdb/1/1/1?result=c", AddedAt: fixedTime()},
			{FileID: 4, FilePath: "/media/tv/severance-s1e1.mkv", AddedAt: fixedTime()},
			{FileID: 5, FilePath: "virtual://series/tvdb/1/1/1?result=dead", AddedAt: fixedTime(), Failed: true},
		},
	}}
	deps := watchDeps(watch)
	warm := &fakeSelectionWarm{}
	deps.VirtualSelectionWarm = warm
	h := newTestHandler(t, deps)

	for range 2 {
		rec := do(t, h, http.MethodGet, "/api/v2/watch/episode:severance-s1e1", "", viewerHeaders())
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

// Only a leaf watch target has playable rows to warm.
func TestWatchDetailWarmSkipsNonLeafItems(t *testing.T) {
	for _, mediaType := range []string{"series", "season", "ebook", "audiobook"} {
		t.Run(mediaType, func(t *testing.T) {
			watch := &fakeWatchVirtualDetail{detail: catalogpkg.WatchDetail{
				ContentID: "ebook:heat", Type: mediaType,
				Versions: []catalogpkg.FileVersion{{FileID: 1, FilePath: "virtual://book?result=a", AddedAt: fixedTime()}},
			}}
			deps := watchDeps(watch)
			warm := &fakeSelectionWarm{}
			deps.VirtualSelectionWarm = warm
			rec := do(t, newTestHandler(t, deps), http.MethodGet, "/api/v2/watch/ebook:heat", "", viewerHeaders())
			if rec.Code != http.StatusOK {
				t.Fatal(rec.Body.String())
			}
			if len(warm.probes) != 0 || len(warm.warms) != 0 {
				t.Fatalf("probes = %v, warms = %v, want none", warm.probes, warm.warms)
			}
		})
	}
}

// An unwired warm seam leaves the watch read untouched rather than failing it.
func TestWatchDetailWarmIsNoOpWhenUnwired(t *testing.T) {
	watch := &fakeWatchVirtualDetail{detail: catalogpkg.WatchDetail{
		ContentID: "episode:severance-s1e1", Type: "episode",
		Versions: []catalogpkg.FileVersion{{FileID: 1, FilePath: "virtual://series/tvdb/1/1/1?result=a", AddedAt: fixedTime()}},
	}}
	rec := do(t, newTestHandler(t, watchDeps(watch)), http.MethodGet, "/api/v2/watch/episode:severance-s1e1", "", viewerHeaders())
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
}

// The watch version projection carries the same per-version liveness the
// catalog detail does, so a native client can choose from this read alone.
func TestWatchDetailProjectsVersionLiveness(t *testing.T) {
	unavailable := false
	probedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	watch := &fakeWatchVirtualDetail{detail: catalogpkg.WatchDetail{
		ContentID: "episode:severance-s1e1", Type: "episode",
		Versions: []catalogpkg.FileVersion{{
			FileID: 7, FilePath: "virtual://series/tvdb/1/1/1?result=a", AddedAt: fixedTime(),
			Failed: true, Available: &unavailable,
			SourceMismatch: &catalogpkg.VersionSourceMismatch{ServedBytes: 7427636945, DeclaredBytes: 8449999999},
			ProbedAt:       &probedAt,
		}},
	}}
	rec := do(t, newTestHandler(t, watchDeps(watch)), http.MethodGet, "/api/v2/watch/episode:severance-s1e1", "", viewerHeaders())
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	var body struct {
		Versions []struct {
			Failed         bool   `json:"failed"`
			Available      *bool  `json:"available"`
			ProbedAt       string `json:"probed_at"`
			SourceMismatch *struct {
				ServedBytes   int64 `json:"served_bytes"`
				DeclaredBytes int64 `json:"declared_bytes"`
			} `json:"source_mismatch"`
		} `json:"versions"`
	}
	decodeJSON(t, rec.Body, &body)
	if len(body.Versions) != 1 {
		t.Fatalf("versions = %+v", body.Versions)
	}
	version := body.Versions[0]
	if !version.Failed {
		t.Fatal("failed missing on a durably failed row")
	}
	if version.Available == nil || *version.Available {
		t.Fatalf("available = %v, want false", version.Available)
	}
	if version.SourceMismatch == nil || version.SourceMismatch.ServedBytes != 7427636945 || version.SourceMismatch.DeclaredBytes != 8449999999 {
		t.Fatalf("source_mismatch = %+v", version.SourceMismatch)
	}
	if version.ProbedAt == "" {
		t.Fatal("probed_at missing on a probed row")
	}

	// An available, unprobed row omits the health fields entirely.
	rec = do(t, newTestHandler(t, watchDeps(&fakeWatchVirtualDetail{detail: catalogpkg.WatchDetail{
		ContentID: "episode:severance-s1e1", Type: "episode",
		Versions: []catalogpkg.FileVersion{{FileID: 8, FilePath: "virtual://series/tvdb/1/1/1?result=b", AddedAt: fixedTime()}},
	}})), http.MethodGet, "/api/v2/watch/episode:severance-s1e1", "", viewerHeaders())
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	for _, field := range []string{"failed", "available", "source_mismatch", "probed_at"} {
		if strings.Contains(rec.Body.String(), `"`+field+`"`) {
			t.Fatalf("healthy unprobed row carried %s: %s", field, rec.Body.String())
		}
	}
}
