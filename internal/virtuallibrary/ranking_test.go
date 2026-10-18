package virtuallibrary_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/quality"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

// --- fixtures ---------------------------------------------------------------

type classifyByURL map[string]struct{ failed, confirmed bool }

func (c classifyByURL) ClassifyCandidates(candidates []resolver.StreamCandidate) {
	for i := range candidates {
		state := c[candidates[i].URL]
		if state.failed {
			candidates[i].SourceFailed = true
		}
		if state.confirmed {
			candidates[i].SourceConfirmed = true
		}
	}
}

type fakeGate struct {
	released bool
	air      *time.Time
}

func (g fakeGate) IsReleased(string, string, int, int) (bool, *time.Time) {
	return g.released, g.air
}

func streamEntry(name, rawURL string) map[string]any {
	return map[string]any{"name": name, "title": name, "url": rawURL}
}

func tokenFormat(name, pattern string, score int, reject bool) quality.CustomFormat {
	return quality.CustomFormat{
		Name: name, Pattern: pattern, PatternType: "token",
		Score: score, Reject: reject, Enabled: true,
	}
}

// newProviderService starts a Stremio provider stub that serves streams and
// builds the core service pointed at it.
func newProviderService(t *testing.T, logger *slog.Logger, cfg virtuallibrary.Config, streams ...map[string]any) *virtuallibrary.Service {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"streams": streams})
	if err != nil {
		t.Fatalf("marshal provider streams: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/manifest.json" {
			_, _ = w.Write([]byte(`{"id":"org.stremio.test","resources":["stream"],"types":["movie","series"]}`))
			return
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)

	cfg.Enabled = true
	cfg.ManifestURL = server.URL + "/manifest.json"
	cfg.AllowInsecureHTTP = true
	cfg.AllowPrivateStreams = true
	svc := virtuallibrary.New(cfg, nil, logger)
	if svc == nil {
		t.Fatal("service is nil")
	}
	return svc
}

// --- ranking ----------------------------------------------------------------

// TestListStreamsAppliesCustomFormatRanking proves the custom-format score,
// not the provider's order or raw resolution, decides the version list.
func TestListStreamsAppliesCustomFormatRanking(t *testing.T) {
	cfg := virtuallibrary.Config{Quality: quality.QualityConfig{
		CustomFormats: []quality.CustomFormat{tokenFormat("Prefer 720p", "720p", 500, false)},
	}}
	svc := newProviderService(t, nil, cfg,
		streamEntry("1080p", "http://192.168.1.10/1080.mkv"),
		streamEntry("720p", "http://192.168.1.10/720.mkv"),
	)

	streams, err := svc.ListStreams(context.Background(), "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	if len(streams) != 2 {
		t.Fatalf("streams = %d, want 2", len(streams))
	}
	if streams[0].Resolution != "720p" {
		t.Fatalf("ranked order = %q, %q; want the higher-scored 720p first", streams[0].Resolution, streams[1].Resolution)
	}
	if streams[0].QualityScore == 0 {
		t.Fatal("QualityScore was not populated on the ranked stream")
	}
}

// TestServerOrderPreservesProviderPreference covers both listing and auto-selection,
// including confirmed-source promotion and stale custom scoring configuration.
func TestServerOrderPreservesProviderPreference(t *testing.T) {
	cfg := virtuallibrary.Config{Quality: quality.QualityConfig{
		CustomFormatPreset: "server-order",
		EnableProfiles:     true,
		Profiles: []quality.QualityProfile{{
			Label: "All",
			Sort:  []quality.SortCriterion{{Attribute: "resolution", Direction: "desc"}},
		}},
		CustomFormats: []quality.CustomFormat{
			tokenFormat("Prefer 4K", "2160p", 500, false),
			tokenFormat("Reject 720p", "720p", -2000, true),
		},
	}}
	svc := newProviderService(t, nil, cfg,
		streamEntry("720p", "http://192.168.1.10/720.mkv"),
		streamEntry("2160p", "http://192.168.1.10/2160.mkv"),
		streamEntry("1080p", "http://192.168.1.10/1080.mkv"),
	)
	svc.Resolver.SetCandidateClassifier(classifyByURL{
		"http://192.168.1.10/2160.mkv": {confirmed: true},
	})
	ranking := svc.RankingForPath("virtual://movie/tt100?profile=All")
	if len(ranking.Criteria) != 0 {
		t.Fatalf("server order advertises sorting criteria: %+v", ranking)
	}
	ctx := context.Background()
	for _, fresh := range []bool{false, true} {
		var streams []virtuallibrary.PlaybackStream
		var err error
		if fresh {
			streams, err = svc.ListStreamsFresh(ctx, "virtual://movie/tt100?profile=All")
		} else {
			streams, err = svc.ListStreams(ctx, "virtual://movie/tt100?profile=All")
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(streams) != 3 {
			t.Fatalf("streams = %d, want 3", len(streams))
		}
		for i, resolution := range []string{"720p", "2160p", "1080p"} {
			if streams[i].Resolution != resolution {
				t.Fatalf("fresh=%t stream[%d] = %s, want %s", fresh, i, streams[i].Resolution, resolution)
			}
			if streams[i].QualityScore != 0 || streams[i].Rejected || len(streams[i].MatchedFormats) != 0 {
				t.Fatalf("server-order stream carries format scoring: %+v", streams[i])
			}
		}
		resolved, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?profile=All", fresh, nil, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.CandidateID != streams[0].ID {
			t.Fatalf("auto-selection did not use the first provider stream")
		}
	}
}

// TestListStreamsKeepsRejectedBehindAccepted proves a rejected candidate is
// rank-last but still returned, never dropped.
func TestListStreamsKeepsRejectedBehindAccepted(t *testing.T) {
	cfg := virtuallibrary.Config{Quality: quality.QualityConfig{
		CustomFormats: []quality.CustomFormat{tokenFormat("No 1080p", "1080p", 0, true)},
	}}
	svc := newProviderService(t, nil, cfg,
		streamEntry("1080p", "http://192.168.1.10/1080.mkv"),
		streamEntry("720p", "http://192.168.1.10/720.mkv"),
	)

	streams, err := svc.ListStreams(context.Background(), "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	if len(streams) != 2 {
		t.Fatalf("streams = %d, want 2 (reject is rank-last, not a drop)", len(streams))
	}
	if streams[0].Rejected || !streams[1].Rejected {
		t.Fatalf("rejected ordering = accepted:%t rejected:%t, want an accepted candidate first", streams[0].Rejected, streams[1].Rejected)
	}
}

// TestListStreamsWarnsWhenAllRejected proves the last-resort path is loud:
// every candidate is returned and a Warn names the profile, count and the
// rejecting formats.
func TestListStreamsWarnsWhenAllRejected(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	cfg := virtuallibrary.Config{Quality: quality.QualityConfig{
		CustomFormats: []quality.CustomFormat{tokenFormat("No 1080p", "1080p", 0, true)},
	}}
	svc := newProviderService(t, logger, cfg,
		streamEntry("1080p", "http://192.168.1.10/a.mkv"),
		streamEntry("1080p Remux", "http://192.168.1.10/b.mkv"),
	)

	streams, err := svc.ListStreams(context.Background(), "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	if len(streams) != 2 {
		t.Fatalf("streams = %d, want 2", len(streams))
	}
	for _, s := range streams {
		if !s.Rejected {
			t.Fatalf("candidate %q not marked rejected", s.Label)
		}
	}
	if !strings.Contains(logs.String(), "every virtual candidate is rejected") {
		t.Fatalf("all-rejected warning not logged: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "No 1080p") {
		t.Fatalf("warning did not name the rejecting format: %s", logs.String())
	}
}

// TestResolveDetailedPinOverridesReject proves an explicit ?result= pin wins
// over both the ranking and the custom-format reject verdict.
func TestResolveDetailedPinOverridesReject(t *testing.T) {
	cfg := virtuallibrary.Config{Quality: quality.QualityConfig{
		CustomFormats: []quality.CustomFormat{tokenFormat("No 1080p", "1080p", 0, true)},
	}}
	svc := newProviderService(t, nil, cfg,
		streamEntry("1080p", "http://192.168.1.10/1080.mkv"),
		streamEntry("720p", "http://192.168.1.10/720.mkv"),
	)

	streams, err := svc.ListStreams(context.Background(), "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	var pinnedID string
	for _, s := range streams {
		if s.Resolution == "1080p" {
			pinnedID = s.ID
		}
	}
	if pinnedID == "" {
		t.Fatal("no 1080p candidate to pin")
	}

	resolved, err := svc.ResolveDetailed(context.Background(), "virtual://movie/tt100?result="+pinnedID, false, nil, "", false)
	if err != nil {
		t.Fatalf("ResolveDetailed: %v", err)
	}
	if resolved.CandidateID != pinnedID {
		t.Fatalf("resolved candidate = %q, want the rejected pin %q", resolved.CandidateID, pinnedID)
	}
}

// TestResolveDetailedProfileRemovedPinFreshVsSessionBound proves the corrected
// precedence: a quality profile is a selection preference, not a gate. A fresh
// selection (no session binding) falls through to the best profile-satisfying
// candidate, while a session bound to a profile-removed candidate is served
// that candidate — with substitution refused or allowed — never refused.
func TestResolveDetailedProfileRemovedPinFreshVsSessionBound(t *testing.T) {
	cfg := virtuallibrary.Config{Quality: quality.QualityConfig{
		EnableProfiles: true,
		Profiles:       []quality.QualityProfile{{Label: "fhd", Resolution: "1080p"}},
	}}
	svc := newProviderService(t, nil, cfg,
		streamEntry("1080p", "http://192.168.1.10/1080.mkv"),
		streamEntry("720p", "http://192.168.1.10/720.mkv"),
	)
	ctx := context.Background()

	streams, err := svc.ListStreams(ctx, "virtual://movie/tt100?profile=fhd")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	var pinned720, fhd1080 string
	for _, s := range streams {
		switch s.Resolution {
		case "720p":
			pinned720 = s.ID
		case "1080p":
			fhd1080 = s.ID
		}
	}
	if pinned720 == "" || fhd1080 == "" {
		t.Fatalf("failed to identify candidates: 720p=%q 1080p=%q", pinned720, fhd1080)
	}
	pinURI := "virtual://movie/tt100?profile=fhd&result=" + pinned720

	// Fresh selection: no session binding, so the profile-removed pin must not
	// fail the start; the profile-satisfying candidate is served instead.
	fresh, err := svc.ResolveDetailed(ctx, pinURI, false, nil, "", false, false)
	if err != nil {
		t.Fatalf("fresh start with a profile-removed pin failed: %v", err)
	}
	if fresh.CandidateID != fhd1080 {
		t.Fatalf("fresh start resolved to %q, want the profile-satisfying %q", fresh.CandidateID, fhd1080)
	}

	// Session-bound: the session's own candidate exists in the provider list, so
	// it is served even though it fails the profile, with substitution refused...
	bound, err := svc.ResolveDetailed(ctx, pinURI, false, nil, pinned720, true, false)
	if err != nil {
		t.Fatalf("session-bound profile-removed candidate refused: %v", err)
	}
	if bound.CandidateID != pinned720 {
		t.Fatalf("session-bound resolve = %q, want the bound candidate %q", bound.CandidateID, pinned720)
	}
	// ...and with substitution allowed the bound pin still wins.
	rotated, err := svc.ResolveDetailed(ctx, pinURI, false, nil, pinned720, true, true)
	if err != nil {
		t.Fatalf("session-bound substitution-allowed resolve: %v", err)
	}
	if rotated.CandidateID != pinned720 {
		t.Fatalf("session-bound substitution-allowed resolve = %q, want the bound candidate %q", rotated.CandidateID, pinned720)
	}
}

// TestResolveDetailedServeResolveProfileRemovedServed proves the serve-layer
// shape: a session-bound re-resolve passes an empty preferred candidate id (the
// session's candidate is the URI's ?result=) and serves that candidate even
// when it fails the quality profile, logging the mismatch at Info instead of
// refusing. Refusing would not prevent a release swap — nothing is substituted
// — it would only break playback.
func TestResolveDetailedServeResolveProfileRemovedServed(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := virtuallibrary.Config{Quality: quality.QualityConfig{
		EnableProfiles: true,
		Profiles:       []quality.QualityProfile{{Label: "fhd", Resolution: "1080p"}},
	}}
	svc := newProviderService(t, logger, cfg,
		streamEntry("1080p", "http://192.168.1.10/1080.mkv"),
		streamEntry("720p", "http://192.168.1.10/720.mkv"),
	)
	ctx := context.Background()

	streams, err := svc.ListStreams(ctx, "virtual://movie/tt100?profile=fhd")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	var pinned720 string
	for _, s := range streams {
		if s.Resolution == "720p" {
			pinned720 = s.ID
		}
	}
	if pinned720 == "" {
		t.Fatal("no 720p candidate to pin")
	}
	// Session-bound, empty preferred, substitution refused: exactly what the
	// serve layer (stream.go / playback_transport.go) declares.
	resolved, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?profile=fhd&result="+pinned720, false, nil, "", true, false)
	if err != nil {
		t.Fatalf("session-bound serve resolve refused a profile-removed candidate: %v", err)
	}
	if resolved.CandidateID != pinned720 {
		t.Fatalf("served candidate = %q, want the bound candidate %q", resolved.CandidateID, pinned720)
	}
	if !strings.Contains(logs.String(), `level=INFO msg="session-bound virtual candidate does not satisfy the quality profile; serving the bound candidate"`) {
		t.Fatalf("profile mismatch not logged at Info: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "candidate_id="+pinned720) || !strings.Contains(logs.String(), "profile=fhd") {
		t.Fatalf("profile mismatch log missing the candidate/profile identity: %s", logs.String())
	}
}

// TestResolveDetailedFreshStartFallsThroughToProfileMatch proves a fresh start
// whose highest-ranked candidates fail the profile still succeeds: it serves
// the best live candidate that satisfies the profile rather than failing. The
// profile-matching candidate is ranked last by the provider, so this also
// exercises the resolver's profile filter rather than luck of ordering.
func TestResolveDetailedFreshStartFallsThroughToProfileMatch(t *testing.T) {
	cfg := virtuallibrary.Config{Quality: quality.QualityConfig{
		EnableProfiles: true,
		Profiles:       []quality.QualityProfile{{Label: "fhd", Resolution: "1080p"}},
	}}
	svc := newProviderService(t, nil, cfg,
		streamEntry("720p", "http://192.168.1.10/low-0.mkv"),
		streamEntry("720p", "http://192.168.1.10/low-1.mkv"),
		streamEntry("1080p", "http://192.168.1.10/match.mkv"),
	)
	ctx := context.Background()

	streams, err := svc.ListStreams(ctx, "virtual://movie/tt100?profile=fhd")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	var pinned720, fhd1080 string
	for _, s := range streams {
		switch s.Resolution {
		case "720p":
			if pinned720 == "" {
				pinned720 = s.ID
			}
		case "1080p":
			fhd1080 = s.ID
		}
	}
	if pinned720 == "" || fhd1080 == "" {
		t.Fatalf("failed to identify candidates: 720p=%q 1080p=%q", pinned720, fhd1080)
	}

	resolved, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?profile=fhd&result="+pinned720, false, nil, "", false, false)
	if err != nil {
		t.Fatalf("fresh start with profile-removed candidates failed: %v", err)
	}
	if resolved.CandidateID != fhd1080 {
		t.Fatalf("fresh start resolved to %q, want the profile-satisfying %q", resolved.CandidateID, fhd1080)
	}
}

// TestResolveDetailedAutoPickedProfileZeroMatchFallsBack proves the zero-match
// policy with fallback_to_any_stream=false: an auto-picked profile that matches
// no candidate degrades to the best-ranked candidate, while an explicit pick
// (no auto flag) keeps the refusal so the client can choose another version.
func TestResolveDetailedAutoPickedProfileZeroMatchFallsBack(t *testing.T) {
	cfg := virtuallibrary.Config{Quality: quality.QualityConfig{
		EnableProfiles: true,
		Profiles:       []quality.QualityProfile{{Label: "4k", Resolution: "2160p"}},
		// FallbackToAnyStream deliberately false: the auto-picked profile is
		// the only reason a fallback is legal here.
	}}
	svc := newProviderService(t, nil, cfg,
		streamEntry("1080p", "http://192.168.1.10/a.mkv"),
		streamEntry("1080p", "http://192.168.1.10/b.mkv"),
	)
	const uri = "virtual://movie/tt100?profile=4k"

	// An explicit pick keeps the refusal: the viewer chose this profile and can
	// be shown the version list instead of a silent downgrade.
	if _, err := svc.ResolveDetailed(context.Background(), uri, false, nil, "", false, false); err == nil {
		t.Fatal("explicit-pick zero-match resolved instead of refusing")
	} else if !strings.Contains(err.Error(), "no stream matches profile") {
		t.Fatalf("explicit-pick zero-match error = %v, want the no-stream-matches-profile refusal", err)
	}

	// An auto-picked profile degrades to the best-ranked candidate.
	autoCtx := virtuallibrary.WithAutoProfileFallback(context.Background(), true)
	resolved, err := svc.ResolveDetailed(autoCtx, uri, false, nil, "", false, false)
	if err != nil {
		t.Fatalf("auto-picked zero-match failed instead of falling back: %v", err)
	}
	if resolved.CandidateID == "" {
		t.Fatal("auto-picked zero-match produced no candidate")
	}
}

// TestResolveDetailedSessionBoundExclusionStillRefuses proves a session-bound
// pin with an exclusion and no rotation still refuses, preserving the
// no-silent-swap invariant independently of the profile gate.
func TestResolveDetailedSessionBoundExclusionStillRefuses(t *testing.T) {
	svc := newProviderService(t, nil, virtuallibrary.Config{},
		streamEntry("1080p", "http://192.168.1.10/a.mkv"),
		streamEntry("720p", "http://192.168.1.10/b.mkv"),
	)
	ctx := context.Background()
	streams, err := svc.ListStreams(ctx, "virtual://movie/tt100")
	if err != nil || len(streams) < 2 {
		t.Fatalf("ListStreams: count=%d err=%v", len(streams), err)
	}
	pinned := streams[0].ID
	pinURI := "virtual://movie/tt100?result=" + pinned

	if _, err := svc.ResolveDetailed(ctx, pinURI, false, []string{pinned}, pinned, true, false); err == nil {
		t.Fatal("expected refusal for a session-bound excluded pin without substitution")
	}
	resolved, err := svc.ResolveDetailed(ctx, pinURI, false, []string{pinned}, pinned, true, true)
	if err != nil {
		t.Fatalf("session-bound rotation resolve: %v", err)
	}
	if resolved.CandidateID == pinned {
		t.Fatal("rotation returned the excluded pin")
	}
}

// TestResolveDetailedStaleFailedPinFreshStartFallsThrough proves a stale stored
// preference (a result= pin the provider no longer lists, as a failed row is
// after a re-list) does not fail a fresh start: with no session binding it takes
// the dead-pin fallback and serves a live candidate.
func TestResolveDetailedStaleFailedPinFreshStartFallsThrough(t *testing.T) {
	cfg := virtuallibrary.Config{Quality: quality.QualityConfig{
		EnableProfiles: true,
		Profiles:       []quality.QualityProfile{{Label: "fhd", Resolution: "1080p"}},
	}}
	svc := newProviderService(t, nil, cfg,
		streamEntry("1080p", "http://192.168.1.10/match.mkv"),
		streamEntry("720p", "http://192.168.1.10/low.mkv"),
	)
	ctx := context.Background()

	// No session binding; the stored pin is absent from the provider list.
	resolved, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?profile=fhd&result=ffffffffffffffffffffffff", false, nil, "", false, false)
	if err != nil {
		t.Fatalf("fresh start with a stale failed pin failed: %v", err)
	}
	if resolved.CandidateID == "" {
		t.Fatal("stale failed pin produced no candidate")
	}
	if resolved.CandidateID == "ffffffffffffffffffffffff" {
		t.Fatal("stale failed pin was served")
	}
}

// TestServeRerunsClassificationAndNeverResurrectsFailed proves the classifier
// is applied on a cache hit (no TTL wait), a SourceFailed candidate is dropped
// from the version list, and rotation cannot bring it back.
func TestServeRerunsClassificationAndNeverResurrectsFailed(t *testing.T) {
	svc := newProviderService(t, nil, virtuallibrary.Config{},
		streamEntry("1080p", "http://192.168.1.10/a.mkv"),
		streamEntry("720p", "http://192.168.1.10/b.mkv"),
	)
	ctx := context.Background()

	streams, err := svc.ListStreams(ctx, "virtual://movie/tt100")
	if err != nil || len(streams) != 2 {
		t.Fatalf("initial ListStreams: count=%d err=%v", len(streams), err)
	}
	var failedID, liveID string
	for _, s := range streams {
		if s.Resolution == "1080p" {
			failedID = s.ID
		} else {
			liveID = s.ID
		}
	}

	svc.Resolver.SetCandidateClassifier(classifyByURL{
		"http://192.168.1.10/a.mkv": {failed: true},
	})

	// The cache entry is still fresh; the serve-time classifier must drop the
	// failed release without waiting for the TTL.
	streams, err = svc.ListStreams(ctx, "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("reclassified ListStreams: %v", err)
	}
	if len(streams) != 1 || streams[0].ID != liveID {
		t.Fatalf("failed candidate survived reclassification: %+v", streams)
	}

	// Rotation that excludes the (now absent) failed candidate must not
	// resurrect it.
	resolved, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+failedID, false, []string{failedID}, "", false, true)
	if err != nil {
		t.Fatalf("rotation resolve: %v", err)
	}
	if resolved.CandidateID == failedID {
		t.Fatal("rotation resurrected a SourceFailed candidate")
	}
}

// TestIngestionDedupesPerFileVariantsBeforeCap proves one release offered as
// many per-file variants cannot flood the version list: the shared content
// hash collapses them to a single candidate.
func TestIngestionDedupesPerFileVariantsBeforeCap(t *testing.T) {
	entries := make([]map[string]any, 0, 60)
	for i := 0; i < 60; i++ {
		entries = append(entries, map[string]any{
			"name":          fmt.Sprintf("Movie.2024.1080p.WEB-DL-%d", i),
			"title":         "Movie.2024.1080p.WEB-DL",
			"url":           fmt.Sprintf("http://192.168.1.10/file-%d.mkv", i),
			"behaviorHints": map[string]any{"videoHash": "same-content-hash"},
		})
	}
	svc := newProviderService(t, nil, virtuallibrary.Config{}, entries...)

	streams, err := svc.ListStreams(context.Background(), "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("dedup left %d candidates, want 1", len(streams))
	}
}

// collapsedPinFixture builds a listing with one multi-file release X (variants
// file-0 keeper and file-1 dropped) plus a distinct, higher-ranked release C.
// It returns the service, the dropped variant's id (the pin), the keeper's id,
// and C's id. The higher-ranked C is what makes the collapse test discriminating:
// an unfixed resolver returns C, a fixed one returns the keeper.
func collapsedPinFixture(t *testing.T) (svc *virtuallibrary.Service, droppedID, keeperID, otherID string) {
	t.Helper()
	entries := []map[string]any{
		{
			"name": "Movie.2024.1080p.WEB-DL-0", "title": "Movie.2024.1080p.WEB-DL",
			"url": "http://192.168.1.10/file-0.mkv", "behaviorHints": map[string]any{"videoHash": "release-x"},
		},
		{
			"name": "Movie.2024.1080p.WEB-DL-1", "title": "Movie.2024.1080p.WEB-DL",
			"url": "http://192.168.1.10/file-1.mkv", "behaviorHints": map[string]any{"videoHash": "release-x"},
		},
		{
			"name": "Other.2024.2160p.WEB-DL", "title": "Other.2024.2160p.WEB-DL",
			"url": "http://192.168.1.10/other.mkv",
		},
	}
	svc = newProviderService(t, nil, virtuallibrary.Config{}, entries...)
	streams, err := svc.ListStreams(context.Background(), "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	if len(streams) != 2 {
		t.Fatalf("ranked candidates = %d, want 2 (release X keeper + release C)", len(streams))
	}
	for _, s := range streams {
		switch s.Resolution {
		case "1080p":
			keeperID = s.ID
		case "2160p":
			otherID = s.ID
		}
	}
	if keeperID == "" || otherID == "" {
		t.Fatalf("failed to identify candidates: keeper=%q other=%q", keeperID, otherID)
	}
	if streams[0].ID != otherID {
		t.Fatalf("higher-ranked release is not first: %+v", streams)
	}

	// Recompute the dropped variant's stable id the same way the resolver does.
	dropped := stream.StreamCandidate{
		Name: "Movie.2024.1080p.WEB-DL-1", Title: "Movie.2024.1080p.WEB-DL",
		URL: "http://192.168.1.10/file-1.mkv",
	}
	dropped.BehaviorHints.VideoHash = "release-x"
	stream.ParseStreamDetails(&dropped)
	droppedID = stream.CandidateVariantID(dropped)
	if droppedID == "" || droppedID == keeperID {
		t.Fatalf("dropped variant id = %q, keeper = %q", droppedID, keeperID)
	}
	return svc, droppedID, keeperID, otherID
}

// TestResolveDetailedPinCollapsedByDedupResolvesToKeeper proves a pin whose
// variant dedup collapsed resolves to the surviving keeper of its release even
// when a different, higher-ranked release is present: a file swap inside a
// release, never a release swap and never a hard failure.
func TestResolveDetailedPinCollapsedByDedupResolvesToKeeper(t *testing.T) {
	svc, droppedID, keeperID, otherID := collapsedPinFixture(t)

	resolved, err := svc.ResolveDetailed(context.Background(), "virtual://movie/tt100?result="+droppedID, false, nil, "", false)
	if err != nil {
		t.Fatalf("ResolveDetailed(collapsed pin): %v", err)
	}
	if resolved.CandidateID != keeperID {
		t.Fatalf("collapsed pin resolved to %q, want its release keeper %q (not the higher-ranked %q)", resolved.CandidateID, keeperID, otherID)
	}
}

// TestResolveDetailedCollapsedPinHonoursExclusion proves the translated pin is
// still governed by the caller's exclusion list: excluding the collapsed
// variant refuses without substitution, and an explicit rotation substitutes a
// different release rather than the excluded release's keeper.
func TestResolveDetailedCollapsedPinHonoursExclusion(t *testing.T) {
	svc, droppedID, keeperID, otherID := collapsedPinFixture(t)
	ctx := context.Background()

	if _, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+droppedID, false, []string{droppedID}, "", false, false); err == nil {
		t.Fatal("expected refusal when the collapsed pin's release is excluded without rotation")
	}
	// A session-bound collapsed-pin exclusion must refuse too: the exclusion
	// gate is ungated by the session-binding intent, so a bound serve-layer
	// re-resolve of an excluded (or collapsed) pin cannot substitute.
	if _, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+droppedID, false, []string{droppedID}, "", true, false); err == nil {
		t.Fatal("expected refusal when a session-bound collapsed pin's release is excluded without rotation")
	}
	resolved, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+droppedID, false, []string{droppedID}, "", false, true)
	if err != nil {
		t.Fatalf("rotation resolve: %v", err)
	}
	if resolved.CandidateID == keeperID {
		t.Fatal("rotation returned the excluded release's keeper")
	}
	if resolved.CandidateID != otherID {
		t.Fatalf("rotation resolved to %q, want the alternative release %q", resolved.CandidateID, otherID)
	}
}

// TestResolveDetailedDeadPinStillFallsBack proves a genuinely dead pin (absent
// with no keeper) still resolves to an alternative under substitution, so a
// genuinely unavailable provider recovers.
func TestResolveDetailedDeadPinStillFallsBack(t *testing.T) {
	svc, _, _, otherID := collapsedPinFixture(t)
	resolved, err := svc.ResolveDetailed(context.Background(), "virtual://movie/tt100?result=ffffffffffffffffffffffff", false, nil, "", false, true)
	if err != nil {
		t.Fatalf("dead-pin resolve: %v", err)
	}
	if resolved.CandidateID != otherID {
		t.Fatalf("dead pin resolved to %q, want the top alternative %q", resolved.CandidateID, otherID)
	}
}

// TestResolveDetailedSessionBoundDeadPinRefusesWithoutRotation proves the
// session-binding guard covers a genuinely dead pin: a session-bound pin absent
// from the provider list with no keeper must not fall through to a sibling
// release when substitution was not declared, while declaring substitution
// still recovers through the dead-pin fallback and a fresh (unbound) resolve is
// unchanged.
func TestResolveDetailedSessionBoundDeadPinRefusesWithoutRotation(t *testing.T) {
	svc, _, _, otherID := collapsedPinFixture(t)
	ctx := context.Background()
	const deadPin = "ffffffffffffffffffffffff"

	if _, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+deadPin, false, nil, deadPin, true, false); err == nil {
		t.Fatal("expected refusal for a session-bound dead pin without declared rotation")
	}
	rotated, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+deadPin, false, nil, deadPin, true, true)
	if err != nil {
		t.Fatalf("declared-rotation dead-pin resolve: %v", err)
	}
	if rotated.CandidateID != otherID {
		t.Fatalf("declared-rotation dead pin resolved to %q, want %q", rotated.CandidateID, otherID)
	}
	// A fresh (unbound) dead pin still falls back without substitution, so a
	// genuinely unavailable provider still recovers on a new selection.
	fresh, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+deadPin, false, nil, "", false, false)
	if err != nil {
		t.Fatalf("fresh dead-pin resolve: %v", err)
	}
	if fresh.CandidateID != otherID {
		t.Fatalf("fresh dead pin resolved to %q, want %q", fresh.CandidateID, otherID)
	}
}

// TestResolveDetailedRefusesDifferentReleaseForSessionPin reproduces the
// handler's per-candidate shape: the session pinned variant A of release X
// (dedup collapsed A -> keeper B) and the request's resultID names a
// higher-ranked different release C while substitution is refused. The resolver
// must refuse C rather than serve it under the session binding; with
// substitution allowed the explicit resultID still wins; and probing the
// session's own keeper is always allowed. The ordinary non-pinned path is
// unaffected.
func TestResolveDetailedRefusesDifferentReleaseForSessionPin(t *testing.T) {
	svc, droppedID, keeperID, otherID := collapsedPinFixture(t)
	ctx := context.Background()

	// Substitution refused, probing the different release: refuse instead of
	// swapping the release under the session binding.
	if _, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+otherID, false, nil, droppedID, true, false); err == nil {
		t.Fatalf("expected refusal when probing %q for session pin %q with substitution refused", otherID, droppedID)
	}
	// Substitution allowed: the explicit resultID wins.
	resolved, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+otherID, false, nil, droppedID, true, true)
	if err != nil {
		t.Fatalf("substitution-allowed resolve: %v", err)
	}
	if resolved.CandidateID != otherID {
		t.Fatalf("substitution-allowed resolve = %q, want the explicit candidate %q", resolved.CandidateID, otherID)
	}
	// Probing the session's own resolved keeper is allowed even when
	// substitution is refused.
	resolved, err = svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+keeperID, false, nil, droppedID, true, false)
	if err != nil {
		t.Fatalf("session keeper resolve: %v", err)
	}
	if resolved.CandidateID != keeperID {
		t.Fatalf("session keeper resolve = %q, want %q", resolved.CandidateID, keeperID)
	}
	// The ordinary non-pinned path is unaffected.
	resolved, err = svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+otherID, false, nil, "", false, false)
	if err != nil {
		t.Fatalf("non-pinned resolve: %v", err)
	}
	if resolved.CandidateID != otherID {
		t.Fatalf("non-pinned resolve = %q, want %q", resolved.CandidateID, otherID)
	}
}

// TestResolveDetailedServesListedPinOverKeeperRemappedProbe reproduces issue
// #154: the probed result id (the plan URI) is absent from a fresh listing and
// keeper-remaps to a surviving keeper, while the session's own pin is still
// listed. The session binding is satisfiable by its listed pin, so the resolve
// must serve the pin rather than refuse with the mismatch error.
func TestResolveDetailedServesListedPinOverKeeperRemappedProbe(t *testing.T) {
	svc, droppedID, keeperID, otherID := collapsedPinFixture(t)
	ctx := context.Background()

	resolved, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+droppedID, false, nil, otherID, true, false)
	if err != nil {
		t.Fatalf("ResolveDetailed(listed pin, keeper-remapped probe): %v", err)
	}
	if resolved.CandidateID != otherID {
		t.Fatalf("resolved candidate = %q, want the listed session pin %q (not the keeper %q)", resolved.CandidateID, otherID, keeperID)
	}

	// The normalization keys on the pin being directly listed. A collapsed
	// session pin (reachable only through the keeper map) still refuses a
	// different release under the binding, so this is not a silent swap.
	if _, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+otherID, false, nil, droppedID, true, false); err == nil {
		t.Fatal("expected refusal when the session pin is only reachable through the keeper map")
	}
}

// TestKeeperMapDropsKeepersBeyondTheCandidateCap proves the dropped -> keeper
// map is filtered to the surviving candidates: when a release's keeper ranks
// beyond the selectable cap, its entry is removed so a pin on the collapsed
// variant is treated as a genuinely dead release and takes the existing
// dead-pin fallback, instead of being translated to a keeper that is not in the
// list.
func TestKeeperMapDropsKeepersBeyondTheCandidateCap(t *testing.T) {
	const others = 51 // more than the cap, so the low-ranked keeper is cut
	entries := make([]map[string]any, 0, others+2)
	for i := 0; i < others; i++ {
		entries = append(entries, map[string]any{
			"name":  fmt.Sprintf("Other.%02d.2160p.WEB-DL", i),
			"title": fmt.Sprintf("Other.%02d.2160p.WEB-DL", i),
			"url":   fmt.Sprintf("http://192.168.1.10/other-%d.mkv", i),
		})
	}
	// Release X has two per-file variants sharing a content hash and is 1080p,
	// so its keeper sorts below every 2160p release and falls beyond the cap.
	entries = append(entries,
		map[string]any{
			"name": "Pinned.2024.1080p.WEB-DL-0", "title": "Pinned.2024.1080p.WEB-DL",
			"url": "http://192.168.1.10/pinned-0.mkv", "behaviorHints": map[string]any{"videoHash": "release-pinned"},
		},
		map[string]any{
			"name": "Pinned.2024.1080p.WEB-DL-1", "title": "Pinned.2024.1080p.WEB-DL",
			"url": "http://192.168.1.10/pinned-1.mkv", "behaviorHints": map[string]any{"videoHash": "release-pinned"},
		},
	)
	svc := newProviderService(t, nil, virtuallibrary.Config{}, entries...)
	ctx := context.Background()

	candidates, keepers, _, _, err := svc.Resolver.GetCandidatesWithKeepers(ctx, "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("GetCandidatesWithKeepers: %v", err)
	}
	if len(candidates) != 50 {
		t.Fatalf("surviving candidates = %d, want the 50-candidate cap", len(candidates))
	}

	variantID := func(name, rawURL string) string {
		candidate := stream.StreamCandidate{
			Name: name, Title: "Pinned.2024.1080p.WEB-DL", URL: rawURL,
		}
		candidate.BehaviorHints.VideoHash = "release-pinned"
		stream.ParseStreamDetails(&candidate)
		return stream.CandidateVariantID(candidate)
	}
	keeperID := variantID("Pinned.2024.1080p.WEB-DL-0", "http://192.168.1.10/pinned-0.mkv")
	droppedID := variantID("Pinned.2024.1080p.WEB-DL-1", "http://192.168.1.10/pinned-1.mkv")
	if keeperID == "" || droppedID == "" || keeperID == droppedID {
		t.Fatalf("bad variant ids: keeper=%q dropped=%q", keeperID, droppedID)
	}

	for _, c := range candidates {
		if stream.CandidateVariantID(c) == keeperID {
			t.Fatal("test setup: the low-ranked keeper unexpectedly survived the cap")
		}
	}
	if _, ok := keepers[droppedID]; ok {
		t.Fatalf("keeper map retained an entry for %q whose keeper was truncated", droppedID)
	}

	// A pin on the collapsed variant therefore takes the dead-pin fallback and
	// resolves to a surviving candidate, never the truncated keeper.
	resolved, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+droppedID, false, nil, "", false, true)
	if err != nil {
		t.Fatalf("dead-pin fallback resolve: %v", err)
	}
	if resolved.CandidateID == keeperID {
		t.Fatal("resolve returned the truncated keeper")
	}
	if resolved.CandidateID == "" {
		t.Fatal("dead-pin fallback returned no candidate")
	}
}

// TestIngestionMovesConfirmedFirst proves the classifier's confirmation is
// authoritative for order at ingestion.
func TestIngestionMovesConfirmedFirst(t *testing.T) {
	svc := newProviderService(t, nil, virtuallibrary.Config{},
		streamEntry("1080p", "http://192.168.1.10/a.mkv"),
		streamEntry("720p", "http://192.168.1.10/b.mkv"),
	)
	svc.Resolver.SetCandidateClassifier(classifyByURL{
		"http://192.168.1.10/b.mkv": {confirmed: true},
	})

	streams, err := svc.ListStreams(context.Background(), "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	if len(streams) != 2 || streams[0].Resolution != "720p" {
		t.Fatalf("confirmed candidate not first: %+v", streams)
	}
}

// TestReleaseGateBlocksFutureAndFailsOpen proves the wired gate blocks only a
// concrete future air date.
func TestReleaseGateBlocksFutureAndFailsOpen(t *testing.T) {
	ctx := context.Background()

	future := time.Now().Add(72 * time.Hour)
	blocked := newProviderService(t, nil, virtuallibrary.Config{}, streamEntry("1080p", "http://192.168.1.10/a.mkv"))
	blocked.Resolver.SetReleaseGate(fakeGate{released: false, air: &future})
	if _, err := blocked.ListStreams(ctx, "virtual://movie/tt100"); err == nil {
		t.Fatal("future release was not blocked")
	}

	past := time.Now().Add(-72 * time.Hour)
	allowed := newProviderService(t, nil, virtuallibrary.Config{}, streamEntry("1080p", "http://192.168.1.10/a.mkv"))
	allowed.Resolver.SetReleaseGate(fakeGate{released: true, air: &past})
	if _, err := allowed.ListStreams(ctx, "virtual://movie/tt100"); err != nil {
		t.Fatalf("past release was blocked: %v", err)
	}

	unknown := newProviderService(t, nil, virtuallibrary.Config{}, streamEntry("1080p", "http://192.168.1.10/a.mkv"))
	unknown.Resolver.SetReleaseGate(fakeGate{released: true})
	if _, err := unknown.ListStreams(ctx, "virtual://movie/tt100"); err != nil {
		t.Fatalf("unknown release was blocked: %v", err)
	}
}

// TestAllFailedListingWarnsAndRecoversWithoutNegativeCache proves a non-empty
// provider answer the classifier drops entirely is not a two-minute negative
// cache entry: it warns with the count and recovers as soon as the classifier
// state changes, without waiting for a TTL.
func TestAllFailedListingWarnsAndRecoversWithoutNegativeCache(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := newProviderService(t, logger, virtuallibrary.Config{},
		streamEntry("1080p", "http://192.168.1.10/a.mkv"),
		streamEntry("720p", "http://192.168.1.10/b.mkv"),
	)
	ctx := context.Background()

	svc.Resolver.SetCandidateClassifier(classifyByURL{
		"http://192.168.1.10/a.mkv": {failed: true},
		"http://192.168.1.10/b.mkv": {failed: true},
	})
	streams, err := svc.ListStreams(ctx, "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("all-failed ListStreams: %v", err)
	}
	if len(streams) != 0 {
		t.Fatalf("all-failed listing returned %d candidates, want 0", len(streams))
	}
	if !strings.Contains(logs.String(), "every provider candidate was dropped as failed") {
		t.Fatalf("all-failed warning not logged: %s", logs.String())
	}

	// The same cached answer must recover immediately when the classifier
	// changes: a non-empty all-failed answer is cached as data, not as a
	// negative entry.
	svc.Resolver.SetCandidateClassifier(classifyByURL{})
	streams, err = svc.ListStreams(ctx, "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("recovered ListStreams: %v", err)
	}
	if len(streams) != 2 {
		t.Fatalf("recovered listing = %d candidates, want 2 (all-failed must not negative-cache)", len(streams))
	}
}

func TestServerOrderCapsBeforeConfirmedSourcePromotion(t *testing.T) {
	entries := make([]map[string]any, 0, 51)
	for i := 0; i < 50; i++ {
		entries = append(entries, streamEntry(fmt.Sprintf("Release.%02d.720p", i), fmt.Sprintf("http://192.168.1.10/release-%d.mkv", i)))
	}
	entries = append(entries, streamEntry("Last.2160p", "http://192.168.1.10/last.mkv"))
	svc := newProviderService(t, nil, virtuallibrary.Config{Quality: quality.QualityConfig{CustomFormatPreset: "server-order"}}, entries...)
	svc.Resolver.SetCandidateClassifier(classifyByURL{"http://192.168.1.10/last.mkv": {confirmed: true}})
	streams, err := svc.ListStreams(context.Background(), "virtual://movie/tt100")
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 50 {
		t.Fatalf("streams = %d, want 50", len(streams))
	}
	for _, stream := range streams {
		if stream.Resolution != "720p" {
			t.Fatal("confirmed source beyond provider cap displaced an earlier stream")
		}
	}
}
