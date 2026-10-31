package handlers

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

// A virtual subtitle extraction that fails on an upstream 5xx gets exactly one
// bounded recovery: re-resolve with forceRefresh and a fresh relay
// registration, release the failed registration, and retry against the new
// input under the same credential-free cache identity.
func TestRetryVirtualSubtitleWithFreshSourceReplacesDeadInput(t *testing.T) {
	file := &models.MediaFile{
		ID:                         11,
		ContentID:                  "episode-fresh-subtitle",
		FilePath:                   "virtual://episode/episode-fresh-subtitle?result=cand-a",
		VirtualOwnerInstallationID: 7,
	}
	relay := remotestream.NewRelay()
	t.Cleanup(func() { _ = relay.Close(context.Background()) })

	forceRefreshes := 0
	h := &StreamHandler{
		RemoteStreamRelay:   relay,
		AllowPrivateStreams: func(int) bool { return true },
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, forceRefresh bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			if forceRefresh {
				forceRefreshes++
				return ResolvedVirtualMedia{URL: "http://provider.local/fresh.mp4", URI: uri, CandidateID: "cand-a"}, nil
			}
			return ResolvedVirtualMedia{URL: "http://provider.local/dead.mp4", URI: uri, CandidateID: "cand-a"}, nil
		}),
	}
	session := &playback.Session{ID: "session-1", UserID: 1, ProfileID: "profile-1", VirtualSourceURI: file.FilePath}

	dead, releaseDead, err := h.resolveVirtualInputURI(context.Background(), file, session.UserID, session.ProfileID, false)
	if err != nil {
		t.Fatalf("initial resolve: %v", err)
	}
	released := 0
	oldRelease := func() {
		released++
		releaseDead()
	}
	opts := playback.StreamExtractOpts{
		InputPath:     dead.URL,
		TrackIndex:    2,
		SourceCodec:   "subrip",
		CacheIdentity: virtualSubtitleCacheIdentity(file, session.VirtualSourceURI, 2),
	}
	identity := opts.CacheIdentity

	var retried []playback.StreamExtractOpts
	freshRelease, retriedFlag, retryErr := h.retryVirtualSubtitleWithFreshSource(
		context.Background(), file, session, &opts, oldRelease,
		func(retryOpts playback.StreamExtractOpts) error {
			retried = append(retried, retryOpts)
			return nil
		},
	)
	if retryErr != nil {
		t.Fatalf("fresh-source retry: %v", retryErr)
	}
	if !retriedFlag {
		t.Fatal("fresh-source retry did not run")
	}
	if freshRelease != nil {
		defer freshRelease()
	}
	if forceRefreshes != 1 {
		t.Fatalf("force refreshes = %d, want 1", forceRefreshes)
	}
	if released != 1 {
		t.Fatalf("failed registration releases = %d, want 1", released)
	}
	if opts.InputPath == dead.URL {
		t.Fatalf("input still points at the failed relay URL %q", dead.URL)
	}
	if opts.CacheIdentity != identity {
		t.Fatalf("cache identity = %q, want the unchanged %q", opts.CacheIdentity, identity)
	}
	if len(retried) != 1 {
		t.Fatalf("retry calls = %d, want 1", len(retried))
	}
	if retried[0].InputPath != opts.InputPath {
		t.Fatalf("retry input = %q, want the fresh URL %q", retried[0].InputPath, opts.InputPath)
	}
	if !retried[0].FreshSourceRetry {
		t.Fatal("retry did not mark FreshSourceRetry, so the identity backoff would suppress it")
	}
}

func TestShouldRetryVirtualSubtitleWithFreshSource(t *testing.T) {
	upstream := errors.New(`ffmpeg subtitle stream failed: exit status 8 (stderr: [http @ 0x1] Server returned 5XX Server Error reply)`)
	rotation := errors.New(`ffmpeg subtitle stream failed: exit status 8 (stderr: [http @ 0x1] Server returned 5XX Server Error reply; Stream map '0:s:3' matches no streams)`)
	other := errors.New("ffmpeg subtitle stream failed: exit status 8 (stderr: Invalid data found when processing input)")
	cases := []struct {
		name    string
		virtual bool
		status  int
		err     error
		want    bool
	}{
		{"virtual upstream before commit", true, 0, upstream, true},
		{"local upstream", false, 0, upstream, false},
		{"committed response", true, http.StatusOK, upstream, false},
		{"rotation stays a re-probe", true, 0, rotation, false},
		{"non-upstream failure", true, 0, other, false},
		{"no failure", true, 0, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRetryVirtualSubtitleWithFreshSource(tc.virtual, tc.status, tc.err); got != tc.want {
				t.Fatalf("shouldRetryVirtualSubtitleWithFreshSource = %v, want %v", got, tc.want)
			}
		})
	}
}

// A failed fresh resolve leaves the original input and registration in place
// and does not retry, so the handler keeps its existing upstream-failure
// response instead of inventing a new one.
func TestRetryVirtualSubtitleWithFreshSourceResolveFailureKeepsFailedInput(t *testing.T) {
	file := &models.MediaFile{
		ID:                         12,
		ContentID:                  "episode-fresh-subtitle-failure",
		FilePath:                   "virtual://episode/episode-fresh-subtitle-failure?result=cand-a",
		VirtualOwnerInstallationID: 7,
	}
	h := &StreamHandler{
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, forceRefresh bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			if forceRefresh {
				return ResolvedVirtualMedia{}, errors.New("provider listing unavailable")
			}
			return ResolvedVirtualMedia{URL: "http://provider.local/dead.mp4", CandidateID: "cand-a"}, nil
		}),
	}
	session := &playback.Session{ID: "session-1", UserID: 1, ProfileID: "profile-1", VirtualSourceURI: file.FilePath}
	opts := playback.StreamExtractOpts{
		InputPath:     "http://127.0.0.1:1/source/dead/stream",
		TrackIndex:    0,
		SourceCodec:   "subrip",
		CacheIdentity: virtualSubtitleCacheIdentity(file, session.VirtualSourceURI, 0),
	}
	input := opts.InputPath
	released := 0
	retries := 0
	_, retried, err := h.retryVirtualSubtitleWithFreshSource(
		context.Background(), file, session, &opts,
		func() { released++ },
		func(playback.StreamExtractOpts) error {
			retries++
			return nil
		},
	)
	if err == nil {
		t.Fatal("fresh resolve failure returned no error")
	}
	if retried {
		t.Fatal("fresh resolve failure still ran the extraction")
	}
	if retries != 0 {
		t.Fatalf("retry calls = %d, want 0", retries)
	}
	if released != 0 {
		t.Fatalf("failed registration releases = %d, want 0", released)
	}
	if opts.InputPath != input {
		t.Fatalf("input = %q, want the unchanged %q", opts.InputPath, input)
	}
}
