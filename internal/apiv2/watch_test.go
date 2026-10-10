package apiv2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	err       error
	trickplay *catalogpkg.TrickplayGrid
}

type fakeMark struct {
	userID    int
	profileID string
	contentID string
	played    bool
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
			Versions: []catalogpkg.FileVersion{{
				FileID: 42, Resolution: "1080p", CodecVideo: "h264", CodecAudio: "eac3", Container: "mkv", FileSize: 1024, Duration: 10200, Bitrate: 8000000,
				AddedAt:     time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
				VideoTracks: []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}},
				AudioTracks: []models.AudioTrack{{Language: "eng", Codec: "eac3", Channels: 6, Default: true}},
				Chapters:    []catalogpkg.VersionChapter{{Index: 1, Title: "Opening", StartSeconds: 0, EndSeconds: 300, Source: "embedded"}},
				Intro:       &catalogpkg.Marker{Start: 0, End: 90},
				Trickplay:   f.trickplay,
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

func (f *fakeWatch) SetWatchedState(_ context.Context, userID int, profileID, contentID string, played bool, _ catalogpkg.AccessFilter) (handlers.WatchedStateView, error) {
	if f.err != nil {
		return handlers.WatchedStateView{}, f.err
	}
	if contentID != "movie:heat-1995" && contentID != "series:heat" {
		return handlers.WatchedStateView{}, &handlers.APIError{Status: http.StatusNotFound, Code: "not_found", Message: "Item not found"}
	}
	f.marks = append(f.marks, fakeMark{userID: userID, profileID: profileID, contentID: contentID, played: played})
	return handlers.WatchedStateView{ContentID: contentID, Type: "movie", AffectedCount: 1, Played: played}, nil
}

func watchDeps(watch WatchService) Dependencies {
	deps := pilotDeps(nil, nil)
	deps.Watch = watch
	return deps
}

func TestGetWatchState(t *testing.T) {
	watch := &fakeWatch{trickplay: &catalogpkg.TrickplayGrid{Width: 300}}
	h := newTestHandler(t, watchDeps(watch))
	owner := with(bearer(memberToken), "X-Profile-Id", "p-owner")

	rec := do(t, h, http.MethodGet, "/api/v2/watch/movie:heat-1995?file_id=42&library_id=1&image_size=medium", "", owner)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["content_id"] != "movie:heat-1995" || body["effective_subtitle_language"] != "eng" || body["effective_subtitle_mode"] != nil {
		t.Fatalf("body = %s", rec.Body.String())
	}
	versions := body["versions"].([]any)
	v := versions[0].(map[string]any)
	if v["file_id"] != "42" || v["duration_seconds"].(float64) != 10200 || v["added_at"] != "2026-01-02T03:04:05.000Z" || v["intro"].(map[string]any)["end_seconds"].(float64) != 90 || v["trickplay_available"] != true {
		t.Fatalf("version = %v", v)
	}
	if variant := body["playback_variants"].([]any)[0].(map[string]any); variant["default_file_id"] != "42" || variant["parts"].([]any)[0].(map[string]any)["versions"] == nil {
		t.Fatalf("variant = %v", variant)
	}
	// The virtual ranking is projected at the item level and on the variant,
	// with the profile label and the ordered criteria.
	ranking, ok := body["virtual_ranking"].(map[string]any)
	if !ok {
		t.Fatalf("virtual_ranking missing: %s", rec.Body.String())
	}
	if ranking["profile_label"] != "4K HDR" || ranking["source"] != "profile" {
		t.Fatalf("item virtual_ranking = %v", ranking)
	}
	criteria := ranking["criteria"].([]any)
	if len(criteria) != 2 || criteria[0].(map[string]any)["attribute"] != "score" || criteria[0].(map[string]any)["direction"] != "desc" {
		t.Fatalf("item virtual_ranking criteria = %v", criteria)
	}
	variantRanking := body["playback_variants"].([]any)[0].(map[string]any)["virtual_ranking"].(map[string]any)
	if variantRanking["profile_label"] != "4K HDR" {
		t.Fatalf("variant virtual_ranking = %v", variantRanking)
	}
	if ud := body["user_data"].(map[string]any); ud["last_file_id"] != "3" || ud["position_seconds"].(float64) != 1325.5 || ud["played"] != false {
		t.Fatalf("user_data = %v", ud)
	}
	// The query reached the access filter.
	last := watch.filters[len(watch.filters)-1]
	if last.SelectedFileID != 42 || last.PresentationLibraryID == nil || *last.PresentationLibraryID != 1 || string(last.ImageSize) != "medium" || last.DeviceID != "" {
		t.Fatalf("filter = %+v", last)
	}

	// The profile header is optional: an account-only caller gets the
	// catalog answer without user_data.
	watch.trickplay = nil
	rec = do(t, h, http.MethodGet, "/api/v2/watch/movie:heat-1995", "", bearer(memberToken))
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	body = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["user_data"]; ok {
		t.Fatalf("user_data present without a profile: %s", rec.Body.String())
	}
	if v := body["versions"].([]any)[0].(map[string]any); v["trickplay_available"] != false {
		t.Fatalf("version without previews = %v", v)
	}
	if subs, ok := body["subtitles"].([]any); !ok || len(subs) != 1 {
		t.Fatalf("subtitles = %v", body["subtitles"])
	}
}

// fakeIndexerReleases is the optional watch-detail seam.
type fakeIndexerReleases struct {
	calls     int
	contentID string
	views     []handlers.IndexerReleaseView
	err       error
}

func (f *fakeIndexerReleases) IndexerReleasesForWatch(_ context.Context, contentID string) ([]handlers.IndexerReleaseView, error) {
	f.calls++
	f.contentID = contentID
	if f.err != nil {
		return nil, f.err
	}
	return f.views, nil
}

// TestGetWatchStateMergesIndexerReleases pins the additive merge: the rows
// appear with their parsed metadata, the release id is the opaque row id, and
// an absent seam leaves an empty (never null) array.
func TestGetWatchStateMergesIndexerReleases(t *testing.T) {
	published := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	score := 9
	seam := &fakeIndexerReleases{views: []handlers.IndexerReleaseView{{
		ReleaseID: "17", Title: "Heat 1995 2160p WEB-DL x265-GRP", Resolution: "2160p",
		CodecVideo: "hevc", CodecAudio: "eac3", HDR: true, SizeBytes: 8_000_000_000,
		Indexer: "idx", PublishedAt: &published, FormatScore: &score, Protocol: "usenet",
		DownloadState: "not_downloaded",
	}}}
	deps := watchDeps(&fakeWatch{})
	deps.IndexerReleases = seam
	h := newTestHandler(t, deps)
	owner := with(bearer(memberToken), "X-Profile-Id", "p-owner")

	rec := do(t, h, http.MethodGet, "/api/v2/watch/movie:heat-1995", "", owner)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var body struct {
		IndexerReleases []map[string]any `json:"indexer_releases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.IndexerReleases) != 1 {
		t.Fatalf("indexer_releases = %v", body.IndexerReleases)
	}
	row := body.IndexerReleases[0]
	if row["release_id"] != "17" || row["download_state"] != "not_downloaded" || row["protocol"] != "usenet" {
		t.Fatalf("row = %v", row)
	}
	if row["published_at"] != "2026-02-03T04:05:06.000Z" || row["size_bytes"].(float64) != 8_000_000_000 {
		t.Fatalf("row = %v", row)
	}
	if seam.calls != 1 || seam.contentID != "movie:heat-1995" {
		t.Fatalf("seam calls = %+v", seam)
	}

	// With no seam the array is present and empty, never null.
	plain := do(t, newTestHandler(t, watchDeps(&fakeWatch{})), http.MethodGet, "/api/v2/watch/movie:heat-1995", "", owner)
	var empty struct {
		IndexerReleases []map[string]any `json:"indexer_releases"`
	}
	if err := json.Unmarshal(plain.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.IndexerReleases == nil || len(empty.IndexerReleases) != 0 {
		t.Fatalf("empty indexer_releases = %#v", empty.IndexerReleases)
	}
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
	if len(watch.marks) != 2 || watch.marks[0] != (fakeMark{userID: 1, profileID: "p-owner", contentID: "series:heat", played: true}) ||
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

func TestGetWatchStatePreservesDevicePreferenceContext(t *testing.T) {
	watch := &fakeWatch{}
	h := newTestHandler(t, watchDeps(watch))
	owner := with(bearer(memberToken), "X-Profile-Id", "p-owner")
	for _, tc := range []struct{ header, want string }{{"tv-1", "tv-1"}, {" tablet-2 ", "tablet-2"}, {"", ""}} {
		rec := do(t, h, http.MethodGet, "/api/v2/watch/movie:heat-1995", "", with(owner, deviceIDHeader, tc.header))
		if rec.Code != http.StatusOK {
			t.Fatal(rec.Body.String())
		}
		filter := watch.filters[len(watch.filters)-1]
		if filter.DeviceID != tc.want {
			t.Fatalf("device context = %q, want %q", filter.DeviceID, tc.want)
		}
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
