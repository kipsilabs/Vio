package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// absentSessionPinError mirrors the exact resolver shape: the human message the
// resolver has always returned, wrapped around the sentinel cause so callers can
// distinguish a renumbered/dead anchor from a generic resolve failure.
func absentSessionPinError(candidateID string) error {
	return fmt.Errorf("session-bound virtual candidate %q is no longer listed and candidate rotation was not requested: %w", candidateID, virtuallibrary.ErrSessionBoundCandidateAbsent)
}

// TestHandleStreamRotatesAbsentSessionPin proves the serve path recovers from a
// provider that renumbered or dropped the session's pinned candidate: the first
// resolve refuses with ErrSessionBoundCandidateAbsent, the serve layer retries
// once with a fresh relist and rotation declared, serves the live sibling, and
// commits it to the session binding. Before the fix the same resolve was a hard
// 502 virtual_resolve_failed.
func TestHandleStreamRotatesAbsentSessionPin(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-dead-pin"
		pinnedURI  = neutralURI + "?result=pinned"
		siblingURI = neutralURI + "?result=sibling"
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("sibling-media"))
	}))
	defer upstream.Close()

	file := &models.MediaFile{ID: 42, ContentID: "movie-dead-pin", FilePath: pinnedURI, VirtualOwnerInstallationID: 7}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, pinnedURI, file.VirtualOwnerInstallationID); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.RemoteStreamRelay = remotestream.NewRelay()
	defer func() { _ = handler.RemoteStreamRelay.Close(context.Background()) }()
	handler.AllowPrivateStreams = func(int) bool { return true }

	var calls int
	var gotRotate []bool
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, uri string, ownerInstallationID, userID int, profileID string, forceRefresh bool, excludedCandidateIDs []string, preferredCandidateID string) (ResolvedVirtualMedia, error) {
		calls++
		rotate := VirtualCandidateRotationAllowed(ctx)
		gotRotate = append(gotRotate, rotate)
		if uri != pinnedURI || ownerInstallationID != 7 || userID != 1 || profileID != "profile-1" {
			t.Fatalf("unexpected resolver arguments: uri=%q owner=%d user=%d profile=%q", uri, ownerInstallationID, userID, profileID)
		}
		if !rotate {
			// Session-bound first attempt: the provider no longer lists the pin.
			return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
		}
		if !forceRefresh || !containsStringExactV3(excludedCandidateIDs, "pinned") {
			t.Fatalf("rotation retry must relist and exclude the dead pin: refresh=%v excluded=%v", forceRefresh, excludedCandidateIDs)
		}
		return ResolvedVirtualMedia{URL: upstream.URL + "/provider/sibling.mp4", URI: siblingURI, CandidateID: "sibling"}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil).WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	rec := httptest.NewRecorder()
	handler.HandleStream(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "sibling-media" {
		t.Fatalf("body = %q, want sibling-media", rec.Body.String())
	}
	if calls != 2 || len(gotRotate) != 2 || gotRotate[0] || !gotRotate[1] {
		t.Fatalf("resolver calls = %d rotate=%v, want exactly [false true]", calls, gotRotate)
	}
	bound, err := sessionMgr.GetSession(session.ID)
	if err != nil || bound == nil {
		t.Fatalf("GetSession: %v", err)
	}
	if bound.VirtualSourceURI != siblingURI {
		t.Fatalf("session binding = %q, want the rotated sibling %q", bound.VirtualSourceURI, siblingURI)
	}
}

// recordingSourceCommittedNotifier records the session a rotation published.
type recordingSourceCommittedNotifier struct {
	published chan string
}

func (n *recordingSourceCommittedNotifier) PublishSourceCommitted(_ context.Context, sessionID string) {
	n.published <- sessionID
}

// byPathPlaybackFileResolver resolves a virtual candidate row by its URI so the
// rotation's effective-file association can be exercised.
type byPathPlaybackFileResolver struct {
	testPlaybackFileResolver
	byPath map[string]*models.MediaFile
}

func (r byPathPlaybackFileResolver) GetByPath(_ context.Context, path string) (*models.MediaFile, error) {
	return r.byPath[path], nil
}

// TestCommitRotatedVirtualSessionSourcePublishesCommittedSource pins the
// commit-time publish: the moment a rotation moves the session binding, the
// effective version is pushed to the session (detached from the media response)
// without waiting for a replan, and the session is associated with the rotated
// release's row while its requested selection is preserved.
func TestCommitRotatedVirtualSessionSourcePublishesCommittedSource(t *testing.T) {
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	const siblingURI = "virtual://movie/tt-rotated?result=sibling"
	siblingRow := &models.MediaFile{ID: 77, FilePath: siblingURI, Container: "virtual"}
	handler := NewStreamHandler(sessionMgr, byPathPlaybackFileResolver{
		byPath: map[string]*models.MediaFile{siblingURI: siblingRow},
	})
	notifier := &recordingSourceCommittedNotifier{published: make(chan string, 1)}
	handler.SourceCommittedNotifier = notifier

	handler.commitRotatedVirtualSessionSource(context.Background(), session.ID, ResolvedVirtualMedia{URI: siblingURI, OwnerID: 5})

	select {
	case got := <-notifier.published:
		if got != session.ID {
			t.Fatalf("published session = %q, want %q", got, session.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rotation did not publish the committed source")
	}
	bound, err := sessionMgr.GetSession(session.ID)
	if err != nil || bound == nil {
		t.Fatalf("GetSession: %v", err)
	}
	if bound.VirtualSourceURI != siblingURI {
		t.Fatalf("session binding = %q, want %q", bound.VirtualSourceURI, siblingURI)
	}
	if bound.MediaFileID != siblingRow.ID {
		t.Fatalf("session effective file id = %d, want the rotated row %d", bound.MediaFileID, siblingRow.ID)
	}
	if bound.RequestedMediaFileID != 42 {
		t.Fatalf("session requested file id = %d, want the original 42 preserved", bound.RequestedMediaFileID)
	}
}

// TestHandleStreamDoesNotRotateOnProviderFailure proves the serve-layer retry is
// narrowly scoped to the absent-pin cause: a generic provider resolve failure
// must keep its 502 and must not spend a misleading rotation attempt.
func TestHandleStreamDoesNotRotateOnProviderFailure(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-provider-down?result=pinned"

	file := &models.MediaFile{ID: 43, ContentID: "movie-provider-down", FilePath: pinnedURI, VirtualOwnerInstallationID: 7}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, pinnedURI, file.VirtualOwnerInstallationID); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	var calls int
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		calls++
		return ResolvedVirtualMedia{}, errors.New("virtual playback provider returned an unsafe stream URL")
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil).WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	rec := httptest.NewRecorder()
	handler.HandleStream(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want exactly 1 (no retry for a provider error)", calls)
	}
	bound, err := sessionMgr.GetSession(session.ID)
	if err != nil || bound == nil {
		t.Fatalf("GetSession: %v", err)
	}
	if bound.VirtualSourceURI != pinnedURI {
		t.Fatalf("session binding = %q, want the unchanged pin %q", bound.VirtualSourceURI, pinnedURI)
	}
}

// TestResolveRehydratedVirtualSourceRotatesAbsentAnchor proves the failure-replan
// rehydration retries the session-bound resolve with rotation declared when the
// anchor is absent from the provider list. Session-bound stays true and the
// retry excludes the dead pin, so only the anchor rotates.
func TestResolveRehydratedVirtualSourceRotatesAbsentAnchor(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-replan-absent"
		pinnedURI  = neutralURI + "?result=pinned"
		siblingURI = neutralURI + "?result=sibling"
	)
	file := &models.MediaFile{ID: 7, ContentID: "movie-replan-absent", FilePath: pinnedURI, VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024"}

	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
		return "http://127.0.0.1:9/unused", nil
	})
	var rotates []bool
	var sessionBound []bool
	var excluded [][]string
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, uri string, ownerInstallationID, userID int, profileID string, forceRefresh bool, excludedCandidateIDs []string, preferredCandidateID string) (ResolvedVirtualMedia, error) {
		rotates = append(rotates, VirtualCandidateRotationAllowed(ctx))
		sessionBound = append(sessionBound, VirtualSessionBinding(ctx))
		excluded = append(excluded, append([]string(nil), excludedCandidateIDs...))
		if !VirtualCandidateRotationAllowed(ctx) {
			return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
		}
		return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/sibling", URI: siblingURI, CandidateID: "sibling", IdentityRematched: true, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024"}, nil
	})

	r := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil).WithContext(newAuthorizedPlaybackContext())
	resolved, err := h.resolveRehydratedVirtualSourceV3(r, file, "profile-1", nil, "pinned", "auto", 0, virtualResolveOptionsV3{sessionBound: true, sessionAnchorURI: pinnedURI})
	if err != nil {
		t.Fatalf("resolveRehydratedVirtualSourceV3: %v", err)
	}
	if got := virtualResultCandidateID(resolved.URI); got != "sibling" {
		t.Fatalf("resolved candidate = %q, want the rotated sibling", got)
	}
	if len(rotates) != 2 || rotates[0] || !rotates[1] {
		t.Fatalf("rotation intents = %v, want exactly [false true]", rotates)
	}
	for i, bound := range sessionBound {
		if !bound {
			t.Fatalf("call %d dropped the session binding; the retry must stay session-bound", i)
		}
	}
	if !containsStringExactV3(excluded[1], "pinned") {
		t.Fatalf("retry exclusions = %v, want the dead pin excluded", excluded[1])
	}
}

// TestResolveRehydratedVirtualSourceRotatesMarkedFailedAnchor proves the failure-replan
// rehydration rotates when the session anchor carries an active failed_at verdict,
// rather than getting stuck refusing the marked-bad release.
func TestResolveRehydratedVirtualSourceRotatesMarkedFailedAnchor(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-replan-failed-anchor"
		pinnedURI  = neutralURI + "?result=pinned"
		siblingURI = neutralURI + "?result=sibling"
	)
	failedAt := time.Now().Add(-time.Minute)
	file := &models.MediaFile{ID: 77, ContentID: "movie-replan-failed-anchor", FilePath: pinnedURI, FailedAt: &failedAt, VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024"}

	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
		return "http://127.0.0.1:9/unused", nil
	})
	var rotates []bool
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, uri string, ownerInstallationID, userID int, profileID string, forceRefresh bool, excludedCandidateIDs []string, preferredCandidateID string) (ResolvedVirtualMedia, error) {
		rotates = append(rotates, VirtualCandidateRotationAllowed(ctx))
		if !VirtualCandidateRotationAllowed(ctx) {
			return ResolvedVirtualMedia{}, fmt.Errorf("%w: candidate %s is marked failed", ErrVirtualCandidateMarkedFailed, pinnedURI)
		}
		return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/sibling", URI: siblingURI, CandidateID: "sibling", IdentityRematched: true, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024"}, nil
	})

	r := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil).WithContext(newAuthorizedPlaybackContext())
	resolved, err := h.resolveRehydratedVirtualSourceV3(r, file, "profile-1", nil, "pinned", "auto", 0, virtualResolveOptionsV3{sessionBound: true, sessionAnchorURI: pinnedURI})
	if err != nil {
		t.Fatalf("resolveRehydratedVirtualSourceV3: %v", err)
	}
	if got := virtualResultCandidateID(resolved.URI); got != "sibling" {
		t.Fatalf("resolved candidate = %q, want the rotated sibling", got)
	}
	if len(rotates) != 2 || rotates[0] || !rotates[1] {
		t.Fatalf("rotation intents = %v, want exactly [false true]", rotates)
	}
}

// TestResolveRehydratedVirtualSourceRefusesDifferentRelease proves the
// rehydration rotation now carries the transport anchor's same-release
// assertion: when the retry resolves a genuinely different release, the
// resolver refuses with the original absent-pin cause instead of silently
// swapping the plan onto sibling bytes.
func TestResolveRehydratedVirtualSourceRefusesDifferentRelease(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-replan-swap"
		pinnedURI  = neutralURI + "?result=pinned"
		siblingURI = neutralURI + "?result=sibling"
	)
	file := &models.MediaFile{
		ID: 11, ContentID: "movie-replan-swap", FilePath: pinnedURI,
		VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
	}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
		return "http://127.0.0.1:9/unused", nil
	})
	var rotates []bool
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		rotates = append(rotates, VirtualCandidateRotationAllowed(ctx))
		if !VirtualCandidateRotationAllowed(ctx) {
			return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
		}
		// A different release under the same neutral key: no rematch, no
		// matching durable identity. The rotation must be refused.
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:9/sibling", URI: siblingURI, CandidateID: "sibling",
			ProviderVideoHash: "hash-b", ProviderReleaseName: "Movie.2024.OTHER",
		}, nil
	})

	r := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil).WithContext(newAuthorizedPlaybackContext())
	resolved, err := h.resolveRehydratedVirtualSourceV3(r, file, "profile-1", nil, "pinned", "auto", 0, virtualResolveOptionsV3{sessionBound: true, sessionAnchorURI: pinnedURI})
	if !errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent) {
		t.Fatalf("err = %v, want the original absent-pin refusal", err)
	}
	if got := virtualResultCandidateID(resolved.URI); got != "" {
		t.Fatalf("resolved candidate = %q, want no accepted rotation", got)
	}
	if len(rotates) != 2 || rotates[0] || !rotates[1] {
		t.Fatalf("rotation intents = %v, want exactly [false true]", rotates)
	}
}

// TestResolveRehydratedVirtualSourceDoesNotRotateDisplayRefusal proves the retry
// is scoped to the absent-pin sentinel: the resolver's display-driven
// pinned-candidate refusal (an excluded live pin) is not retried, so a live
// release is never silently swapped.
func TestResolveRehydratedVirtualSourceDoesNotRotateDisplayRefusal(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-display-refusal?result=pinned"
	file := &models.MediaFile{ID: 9, ContentID: "movie-display-refusal", FilePath: pinnedURI, VirtualOwnerInstallationID: 5}

	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
		return "http://127.0.0.1:9/unused", nil
	})
	var calls int
	displayErr := errors.New(`pinned virtual candidate "pinned" is excluded and candidate rotation was not requested`)
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		calls++
		return ResolvedVirtualMedia{}, displayErr
	})

	r := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil).WithContext(newAuthorizedPlaybackContext())
	_, err := h.resolveRehydratedVirtualSourceV3(r, file, "profile-1", nil, "pinned", "auto", 0, virtualResolveOptionsV3{sessionBound: true, sessionAnchorURI: pinnedURI})
	if !errors.Is(err, displayErr) {
		t.Fatalf("err = %v, want the original display-driven refusal", err)
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want exactly 1 (a display-driven refusal is never retried)", calls)
	}
}

// TestResolveRehydratedVirtualSourceDoesNotRotateProviderError proves the
// rehydration retry does not fire for a generic failure: a provider error is
// returned unchanged without a second resolve.
func TestResolveRehydratedVirtualSourceDoesNotRotateProviderError(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-replan-provider-down?result=pinned"
	file := &models.MediaFile{ID: 8, ContentID: "movie-replan-provider-down", FilePath: pinnedURI, VirtualOwnerInstallationID: 5}

	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
		return "http://127.0.0.1:9/unused", nil
	})
	var calls int
	providerErr := errors.New("virtual playback provider returned an unsafe stream URL")
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		calls++
		return ResolvedVirtualMedia{}, providerErr
	})

	r := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil).WithContext(newAuthorizedPlaybackContext())
	_, err := h.resolveRehydratedVirtualSourceV3(r, file, "profile-1", nil, "pinned", "auto", 0, virtualResolveOptionsV3{sessionBound: true, sessionAnchorURI: pinnedURI})
	if !errors.Is(err, providerErr) {
		t.Fatalf("err = %v, want the original provider error", err)
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want exactly 1 (no retry for a provider error)", calls)
	}
}

// TestResolveVirtualAnchorURIRotatesAbsentSessionPin proves the transport
// anchor resolve gains the same absent-pin rotation retry the serve layer and
// the replan rehydration already have: the first session-bound resolve refuses
// with ErrSessionBoundCandidateAbsent, the retry relists with rotation declared
// and the absent pin excluded, and a same-release (identity-rematched) candidate
// is accepted.
func TestResolveVirtualAnchorURIRotatesAbsentSessionPin(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-anchor-rotate"
		pinnedURI  = neutralURI + "?result=pinned"
		siblingURI = neutralURI + "?result=sibling"
	)
	file := &models.MediaFile{
		ID: 7, ContentID: "movie-anchor-rotate", FilePath: pinnedURI,
		VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
	}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	var rotates, refreshes []bool
	var excluded [][]string
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, uri string, _ int, _ int, _ string, forceRefresh bool, excludedCandidateIDs []string, _ string) (ResolvedVirtualMedia, error) {
		rotates = append(rotates, VirtualCandidateRotationAllowed(ctx))
		refreshes = append(refreshes, forceRefresh)
		excluded = append(excluded, append([]string(nil), excludedCandidateIDs...))
		if !VirtualCandidateRotationAllowed(ctx) {
			return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
		}
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:9/sibling", URI: siblingURI, CandidateID: "sibling",
			IdentityRematched: true, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
		}, nil
	})
	session := &playback.Session{ID: "anchor-rotate", UserID: 1, ProfileID: "profile-1"}

	resolved, cleanup, err := h.resolveVirtualAnchorURIWithRotationV3(context.Background(), session, file)
	if err != nil {
		t.Fatalf("resolveVirtualAnchorURIWithRotationV3: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if got := virtualResultCandidateID(resolved.URI); got != "sibling" {
		t.Fatalf("resolved anchor = %q, want the rotated same-release sibling", got)
	}
	if len(rotates) != 2 || rotates[0] || !rotates[1] {
		t.Fatalf("rotation intents = %v, want exactly [false true]", rotates)
	}
	if len(refreshes) != 2 || !refreshes[1] {
		t.Fatalf("relist intents = %v, want the retry to force a fresh listing", refreshes)
	}
	if !containsStringExactV3(excluded[1], "pinned") {
		t.Fatalf("retry exclusions = %v, want the absent pin excluded", excluded[1])
	}
}

// TestResolveVirtualAnchorURIDoesNotRotateDisplayRefusal proves the anchor retry
// is scoped to the absent-pin sentinel: a display-driven pinned-candidate
// refusal is never retried, so a live release is not silently swapped.
func TestResolveVirtualAnchorURIDoesNotRotateDisplayRefusal(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-anchor-display?result=pinned"
	file := &models.MediaFile{ID: 8, ContentID: "movie-anchor-display", FilePath: pinnedURI, VirtualOwnerInstallationID: 5}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	var calls int
	displayErr := errors.New(`pinned virtual candidate "pinned" is excluded and candidate rotation was not requested`)
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		calls++
		return ResolvedVirtualMedia{}, displayErr
	})
	session := &playback.Session{ID: "anchor-display", UserID: 1, ProfileID: "profile-1"}

	_, _, err := h.resolveVirtualAnchorURIWithRotationV3(context.Background(), session, file)
	if !errors.Is(err, displayErr) {
		t.Fatalf("err = %v, want the original display-driven refusal", err)
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want exactly 1 (a display-driven refusal is never retried)", calls)
	}
}

// TestResolveVirtualAnchorURIWithoutLiveCandidateFails proves an absent pin with
// no same-release candidate still fails retryably: the retry runs once, finds
// nothing it may silently anchor on, and returns the absent-pin cause.
func TestResolveVirtualAnchorURIWithoutLiveCandidateFails(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-anchor-gone?result=pinned"
	file := &models.MediaFile{ID: 9, ContentID: "movie-anchor-gone", FilePath: pinnedURI, VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-a"}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	var calls int
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		calls++
		return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
	})
	session := &playback.Session{ID: "anchor-gone", UserID: 1, ProfileID: "profile-1"}

	_, _, err := h.resolveVirtualAnchorURIWithRotationV3(context.Background(), session, file)
	if !errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent) {
		t.Fatalf("err = %v, want the retryable absent-pin cause", err)
	}
	if calls != 2 {
		t.Fatalf("resolver calls = %d, want exactly 2 (one bounded retry)", calls)
	}
}

// TestResolveVirtualAnchorURIRefusesDifferentRelease proves the retry never
// silently anchors the already-built plan on a sibling release: when the rotated
// candidate is not the same release, the original absent-pin cause is returned.
func TestResolveVirtualAnchorURIRefusesDifferentRelease(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-anchor-swap"
		pinnedURI  = neutralURI + "?result=pinned"
		siblingURI = neutralURI + "?result=sibling"
	)
	file := &models.MediaFile{ID: 10, ContentID: "movie-anchor-swap", FilePath: pinnedURI, VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024"}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		if !VirtualCandidateRotationAllowed(ctx) {
			return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
		}
		// A different release: no identity rematch and no matching identity.
		return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/sibling", URI: siblingURI, CandidateID: "sibling", ProviderVideoHash: "hash-b"}, nil
	})
	session := &playback.Session{ID: "anchor-swap", UserID: 1, ProfileID: "profile-1"}

	_, _, err := h.resolveVirtualAnchorURIWithRotationV3(context.Background(), session, file)
	if !errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent) {
		t.Fatalf("err = %v, want the absent-pin cause, not a silent sibling anchor", err)
	}
}

// TestPrepareTransportTimelineRotatesAbsentSessionPin proves the transport
// planner's remux seek anchor now recovers from an absent session pin: the
// anchor resolve rotates to a same-release candidate instead of returning the
// "Failed to resolve remux seek position." terminal.
func TestPrepareTransportTimelineRotatesAbsentSessionPin(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-timeline-rotate"
		pinnedURI  = neutralURI + "?result=pinned"
		siblingURI = neutralURI + "?result=sibling"
	)
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.RemoteStreamRelay = remotestream.NewRelay()
	defer func() { _ = handler.RemoteStreamRelay.Close(context.Background()) }()
	var calls int
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		calls++
		if !VirtualCandidateRotationAllowed(ctx) {
			return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
		}
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:9/sibling.mp4", URI: siblingURI, CandidateID: "sibling",
			IdentityRematched: true, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
		}, nil
	})
	probed := false
	handler.copySeekAnchor = func(_ context.Context, _ string, inputPath string, requested float64, _ int) (float64, int, error) {
		probed = true
		if inputPath == "" || strings.HasPrefix(inputPath, "virtual://") {
			t.Fatalf("copy anchor probed a virtual URI %q, want the rotated relay URL", inputPath)
		}
		return requested - 0.5, 0, nil
	}
	file := &models.MediaFile{ID: 11, ContentID: "movie-timeline-rotate", FilePath: pinnedURI, VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024"}
	session := &playback.Session{ID: "timeline-rotate", UserID: 1, ProfileID: "profile-1"}
	plan := &playback.PlanV3{
		PlanID:   "plan:timeline-rotate",
		Delivery: playback.DeliveryRemuxProgressiveV3,
		Timeline: playback.TimelineV3{SourceStartSeconds: 30, PlayerStartSeconds: 30},
	}

	timeline, timelineErr := handler.prepareTransportTimelineV3(context.Background(), session, file, playback.PlannerResultV3{Plan: plan, PlayMethod: playback.PlayRemux})
	if timelineErr != nil {
		t.Fatalf("prepareTransportTimelineV3: %v", timelineErr)
	}
	if !probed || !timeline.copySeekAnchorResolved {
		t.Fatalf("timeline = %#v probed=%v, want a resolved copy anchor", timeline, probed)
	}
	if calls != 2 {
		t.Fatalf("resolver calls = %d, want exactly 2 (session-bound then rotation)", calls)
	}
}

// TestResolveVirtualAnchorURIExcludesFailedCandidate proves a seek-anchor
// retry whose relay token 5xxes can walk to an alternate same-identity
// candidate instead of re-probing the token that just failed: the failed pin
// is excluded, the retry forces a fresh listing with rotation declared, and a
// same-release (identity-rematched) candidate is accepted.
func TestResolveVirtualAnchorURIExcludesFailedCandidate(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-anchor-5xx"
		pinnedURI  = neutralURI + "?result=pinned"
		siblingURI = neutralURI + "?result=sibling"
	)
	file := &models.MediaFile{
		ID: 9, ContentID: "movie-anchor-5xx", FilePath: pinnedURI,
		VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
	}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	var excluded [][]string
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, uri string, _ int, _ int, _ string, forceRefresh bool, excludedCandidateIDs []string, _ string) (ResolvedVirtualMedia, error) {
		excluded = append(excluded, append([]string(nil), excludedCandidateIDs...))
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:9/sibling", URI: siblingURI, CandidateID: "sibling",
			IdentityRematched: true, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
		}, nil
	})
	session := &playback.Session{ID: "anchor-5xx", UserID: 1, ProfileID: "profile-1"}

	rotated, cleanup, err := h.resolveVirtualAnchorURIExcludingFailedV3(context.Background(), session, file, "pinned")
	if err != nil {
		t.Fatalf("resolveVirtualAnchorURIExcludingFailedV3: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if got := virtualResultCandidateID(rotated.URI); got != "sibling" {
		t.Fatalf("resolved anchor = %q, want the rotated same-release sibling", got)
	}
	if len(excluded) != 1 || !containsStringExactV3(excluded[0], "pinned") {
		t.Fatalf("retry exclusions = %v, want the failed pin excluded", excluded)
	}
}

// TestResolveVirtualAnchorURIExcludingFailedRefusesDifferentRelease proves the
// failed-candidate rotation never silently swaps releases: a candidate that is
// neither identity-rematched nor identity-matching is refused.
func TestResolveVirtualAnchorURIExcludingFailedRefusesDifferentRelease(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-anchor-5xx-other?result=pinned"
	file := &models.MediaFile{
		ID: 10, ContentID: "movie-anchor-5xx-other", FilePath: pinnedURI,
		VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
	}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:9/other", URI: "virtual://movie/tt-anchor-5xx-other?result=other",
			ProviderVideoHash: "hash-B", ProviderReleaseName: "Other.2024",
		}, nil
	})
	session := &playback.Session{ID: "anchor-5xx-other", UserID: 1, ProfileID: "profile-1"}

	if _, _, err := h.resolveVirtualAnchorURIExcludingFailedV3(context.Background(), session, file, "pinned"); err == nil {
		t.Fatal("expected a refusal when rotation resolves a different release")
	}
}
