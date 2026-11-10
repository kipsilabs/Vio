package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestResolveVirtualPlaybackSourceListsOnceAcrossTheCandidateWalk pins the
// single-list walk: a recovery or rotation resolve lists the provider once, and
// every later candidate in the walk resolves against that fresh answer instead
// of paying its own forced listing. Production replans paid five sequential
// 2.6-4.8s listings for one candidate set.
func TestResolveVirtualPlaybackSourceListsOnceAcrossTheCandidateWalk(t *testing.T) {
	const neutral = "virtual://movie/tt-walk"
	candidateURIs := []string{
		neutral + "?result=c0",
		neutral + "?result=c1",
		neutral + "?result=c2",
	}
	listerCalls := 0
	var forced []bool
	var resolvedURIs []string
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			listerCalls++
			streams := make([]VirtualPlaybackStream, 0, len(candidateURIs))
			for i, uri := range candidateURIs {
				streams = append(streams, VirtualPlaybackStream{
					ID:         "c" + string(rune('0'+i)),
					URI:        uri,
					Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac",
				})
			}
			return streams, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			return "http://127.0.0.1:9/stream?path=" + path, nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, forceRefresh bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			forced = append(forced, forceRefresh)
			resolvedURIs = append(resolvedURIs, uri)
			if strings.HasSuffix(uri, "c2") {
				return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/stream", URI: uri, CandidateID: "c2"}, nil
			}
			return ResolvedVirtualMedia{}, errors.New("provider hiccup")
		}),
	}
	file := &models.MediaFile{
		ID: 21, ContentID: "movie-walk", FilePath: neutral, VirtualOwnerInstallationID: 5,
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil)

	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false,
		virtualResolveOptionsV3{rotateCandidates: true, sessionBound: false})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := virtualResultCandidateID(resolved.URI); got != "c2" {
		t.Fatalf("resolved candidate = %q, want c2 after the two failing candidates", got)
	}
	if listerCalls != 1 {
		t.Fatalf("provider listings = %d, want exactly 1 for the walk", listerCalls)
	}
	if len(forced) != len(candidateURIs) {
		t.Fatalf("resolver calls = %d (%v), want one per candidate", len(forced), forced)
	}
	for i, relisted := range forced {
		if want := i == 0; relisted != want {
			t.Fatalf("forced re-list flags = %v, want only the first candidate to list (%v)", forced, resolvedURIs)
		}
	}
}

// TestCandidateVerdictBindsOnlyItsOwnCandidate pins the attribution fix: the
// session's pinned row is returned by the provider-neutral lookup for every
// sibling under the same key, but its failed stamp is evidence only about the
// candidate that row names. Binding it to a sibling made every index of a
// rotation walk reject with "is marked failed" without testing one.
func TestCandidateVerdictBindsOnlyItsOwnCandidate(t *testing.T) {
	const (
		neutral    = "virtual://movie/tt-verdict"
		pinnedURI  = neutral + "?result=pinned"
		siblingURI = neutral + "?result=sibling"
	)
	failedAt := time.Now().Add(-time.Minute)
	pinnedRow := &models.MediaFile{ID: 42, FilePath: pinnedURI, FailedAt: &failedAt}
	h := &PlaybackHandler{
		VirtualFileLookup: func(context.Context, string) (*models.MediaFile, error) {
			return nil, ErrVirtualCandidateNotFound
		},
		VirtualCandidateFileLookup: func(context.Context, string, string, string, int) (*models.MediaFile, error) {
			return pinnedRow, nil
		},
	}
	file := &models.MediaFile{ID: 7, ContentID: "movie-verdict", FilePath: neutral, VirtualOwnerInstallationID: 5}

	if err := h.virtualCandidateVerdictError(context.Background(), pinnedURI, file, 5, false); !errors.Is(err, ErrVirtualCandidateMarkedFailed) {
		t.Fatalf("pinned candidate verdict = %v, want its own %v", err, ErrVirtualCandidateMarkedFailed)
	}
	if err := h.virtualCandidateVerdictError(context.Background(), siblingURI, file, 5, false); err != nil {
		t.Fatalf("sibling candidate inherited the pinned row's verdict: %v", err)
	}
	if !virtualCandidateVerdictBindsRow(pinnedRow, pinnedURI) {
		t.Fatal("a row's verdict must bind its own candidate")
	}
	if virtualCandidateVerdictBindsRow(pinnedRow, siblingURI) {
		t.Fatal("a row's verdict bound a different sibling pick under the same neutral key")
	}
}

// TestResolveVirtualPlaybackSourceWalksPastAnIndictedPinnedRow is the
// production shape: an identity-less session row carries a failed stamp, the
// provider offers a pinned and a sibling candidate, and the neutral fallback
// lookup returns the pinned row for both. The walk must test the sibling instead
// of re-indicting it with the pin's verdict.
func TestResolveVirtualPlaybackSourceWalksPastAnIndictedPinnedRow(t *testing.T) {
	const (
		neutral    = "virtual://movie/tt-indicted-walk"
		pinnedURI  = neutral + "?result=pinned"
		siblingURI = neutral + "?result=sibling"
	)
	failedAt := time.Now().Add(-time.Minute)
	pinnedRow := &models.MediaFile{ID: 42, FilePath: pinnedURI, FailedAt: &failedAt}

	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{
				{ID: "pinned", URI: pinnedURI, Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac"},
				{ID: "sibling", URI: siblingURI, Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac"},
			}, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			return "http://127.0.0.1:9/stream?path=" + path, nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/stream", URI: uri, CandidateID: virtualResultCandidateID(uri)}, nil
		}),
		VirtualFileLookup: func(context.Context, string) (*models.MediaFile, error) {
			return nil, ErrVirtualCandidateNotFound
		},
		VirtualCandidateFileLookup: func(context.Context, string, string, string, int) (*models.MediaFile, error) {
			return pinnedRow, nil
		},
	}
	file := &models.MediaFile{ID: 7, ContentID: "movie-indicted-walk", FilePath: neutral, VirtualOwnerInstallationID: 5}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil)

	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false,
		virtualResolveOptionsV3{rotateCandidates: true, sessionBound: false})
	if err != nil {
		t.Fatalf("resolve failed instead of walking to the healthy sibling: %v", err)
	}
	if got := virtualResultCandidateID(resolved.URI); got != "sibling" {
		t.Fatalf("resolved candidate = %q, want the healthy sibling", got)
	}
}
