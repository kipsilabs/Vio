package handlers

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// storedURLLookup returns the fixed row for the exact candidate path and a
// not-found error otherwise, mirroring the production exact-path lookup.
func storedURLLookup(row *models.MediaFile) VirtualFileLookup {
	return func(_ context.Context, path string) (*models.MediaFile, error) {
		if row == nil || row.FilePath != path {
			return nil, ErrVirtualCandidateNotFound
		}
		return row, nil
	}
}

// countingDetailedResolver counts provider listings and returns a fixed
// resolution, standing in for the provider round-trip the shortcut must skip.
func countingDetailedResolver(calls *int, resolved ResolvedVirtualMedia) VirtualMediaDetailedResolver {
	return VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		*calls++
		return resolved, nil
	})
}

// TestEvaluateStoredVirtualURLCandidateTrustWindow pins the trust-window
// decision. A signed URL is never served past its own expiry; the window only
// changes whether an expired same-identity row is still trusted (so the caller
// keeps preferring it) or has fallen back to today's expired behavior. A nil
// expiry is always usable.
func TestEvaluateStoredVirtualURLCandidateTrustWindow(t *testing.T) {
	const pinned = "virtual://movie/tt-trust?result=cand-a"
	now := time.Now()
	expired := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	window := 720 * time.Hour

	newRow := func(updatedAt time.Time, expiresAt *time.Time) *models.MediaFile {
		return &models.MediaFile{
			ID:                         70,
			FilePath:                   pinned,
			UpdatedAt:                  updatedAt,
			ResolvedURL:                "https://93.184.216.34/stream/token=stored",
			ResolvedURLExpiresAt:       expiresAt,
			ProviderVideoHash:          "hash-a",
			ProviderReleaseName:        "Movie.2024.1080p",
			VirtualOwnerInstallationID: 5,
		}
	}

	tests := []struct {
		name      string
		updatedAt time.Time
		expiresAt *time.Time
		window    time.Duration
		wantState virtualStoredURLState
		wantURL   bool
	}{
		{
			name:      "nil expiry is usable regardless of window",
			updatedAt: now.Add(-100 * 24 * time.Hour),
			expiresAt: nil,
			window:    window,
			wantState: virtualStoredURLUsable,
			wantURL:   true,
		},
		{
			name:      "future expiry is usable",
			updatedAt: now,
			expiresAt: &future,
			window:    window,
			wantState: virtualStoredURLUsable,
			wantURL:   true,
		},
		{
			name:      "expired but inside the window is trusted, never served",
			updatedAt: now.Add(-time.Hour),
			expiresAt: &expired,
			window:    window,
			wantState: virtualStoredURLExpiredWithinWindow,
			wantURL:   false,
		},
		{
			name:      "expired beyond the window keeps the pre-window state",
			updatedAt: now.Add(-60 * 24 * time.Hour),
			expiresAt: &expired,
			window:    window,
			wantState: virtualStoredURLExpired,
			wantURL:   false,
		},
		{
			name:      "expired with the window disabled keeps the pre-window state",
			updatedAt: now,
			expiresAt: &expired,
			window:    0,
			wantState: virtualStoredURLExpired,
			wantURL:   false,
		},
		{
			name:      "expired row with no timestamp is outside the window",
			updatedAt: time.Time{},
			expiresAt: &expired,
			window:    window,
			wantState: virtualStoredURLExpired,
			wantURL:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, state := evaluateStoredVirtualURLCandidate(
				context.Background(), pinned, newRow(tc.updatedAt, tc.expiresAt), true, now, tc.window,
			)
			if state != tc.wantState {
				t.Fatalf("state = %v, want %v", state, tc.wantState)
			}
			if tc.wantURL && got.URL == "" {
				t.Fatalf("URL = empty, want the stored URL")
			}
			if !tc.wantURL && got.URL != "" {
				t.Fatalf("URL = %q, want empty (an expired URL is never served)", got.URL)
			}
		})
	}
}

func TestResolveVirtualInputUsesStoredURLWithoutListing(t *testing.T) {
	const pinned = "virtual://movie/tt-stored?result=cand-a"
	expiresAt := time.Now().Add(2 * time.Hour)
	row := &models.MediaFile{
		ID:                         71,
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		ResolvedURL:                "https://93.184.216.34/stream/token=stored",
		ResolvedURLExpiresAt:       &expiresAt,
	}
	calls := 0
	h := &PlaybackHandler{
		VirtualFileLookup: storedURLLookup(row),
		VirtualMediaDetailedResolver: countingDetailedResolver(&calls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=fresh", URI: pinned, CandidateID: "cand-a",
		}),
	}

	res, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", false, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 0 {
		t.Fatalf("provider listing calls = %d, want 0 for a usable stored URL", calls)
	}
	if res.URL != row.ResolvedURL {
		t.Fatalf("resolved URL = %q, want the stored URL %q", res.URL, row.ResolvedURL)
	}
	if res.CandidateID != "cand-a" {
		t.Fatalf("candidate id = %q, want cand-a", res.CandidateID)
	}
}

func TestResolveVirtualInputForceRefreshBypassesStoredURL(t *testing.T) {
	const pinned = "virtual://movie/tt-force?result=cand-a"
	expiresAt := time.Now().Add(2 * time.Hour)
	row := &models.MediaFile{
		ID:                   72,
		FilePath:             pinned,
		ResolvedURL:          "https://93.184.216.34/stream/token=stored",
		ResolvedURLExpiresAt: &expiresAt,
	}
	calls := 0
	h := &PlaybackHandler{
		VirtualFileLookup: storedURLLookup(row),
		VirtualMediaDetailedResolver: countingDetailedResolver(&calls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=fresh", URI: pinned, CandidateID: "cand-a",
		}),
	}

	res, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", true, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 1 {
		t.Fatalf("provider listing calls = %d, want 1 for an explicit forceRefresh", calls)
	}
	if res.URL != "https://93.184.216.34/stream/token=fresh" {
		t.Fatalf("resolved URL = %q, want the freshly listed URL", res.URL)
	}
}

func TestResolveVirtualInputExpiredStoredURLFallsBackAndRefreshes(t *testing.T) {
	const pinned = "virtual://movie/tt-expired?result=cand-a"
	expiredAt := time.Now().Add(-time.Hour)
	newExpiry := time.Now().Add(3 * time.Hour)
	row := &models.MediaFile{
		ID:                   73,
		FilePath:             pinned,
		ResolvedURL:          "https://93.184.216.34/stream/token=old",
		ResolvedURLExpiresAt: &expiredAt,
	}
	calls := 0
	h := &PlaybackHandler{
		VirtualFileLookup: storedURLLookup(row),
		VirtualMediaDetailedResolver: countingDetailedResolver(&calls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=new", URI: pinned, CandidateID: "cand-a",
			ExpiresAt: newExpiry,
		}),
	}
	var saved []models.VirtualFilePersistArgs
	h.VirtualFileSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
		saved = append(saved, args)
		return 1, nil
	}

	res, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", false, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 1 {
		t.Fatalf("provider listing calls = %d, want 1 for an expired stored URL", calls)
	}
	if res.URL != "https://93.184.216.34/stream/token=new" {
		t.Fatalf("resolved URL = %q, want the freshly listed URL", res.URL)
	}
	if len(saved) != 1 {
		t.Fatalf("stored-URL refresh writes = %d, want 1", len(saved))
	}
	if saved[0].ResolvedURL != "https://93.184.216.34/stream/token=new" {
		t.Fatalf("refreshed resolved_url = %q, want the fresh URL", saved[0].ResolvedURL)
	}
	if saved[0].ResolvedURLExpiresAt == nil || !saved[0].ResolvedURLExpiresAt.Equal(newExpiry) {
		t.Fatalf("refreshed expiry = %v, want %v", saved[0].ResolvedURLExpiresAt, newExpiry)
	}
	if saved[0].FileID != row.ID || saved[0].ExpectedFilePath != pinned {
		t.Fatalf("refresh targeted file %d %q, want %d %q", saved[0].FileID, saved[0].ExpectedFilePath, row.ID, pinned)
	}
}

func TestResolveVirtualInputStoredURLFailingValidationFallsBack(t *testing.T) {
	const pinned = "virtual://movie/tt-invalid?result=cand-a"
	expiresAt := time.Now().Add(time.Hour)
	row := &models.MediaFile{
		ID:                   74,
		FilePath:             pinned,
		ResolvedURL:          "http://127.0.0.1/private/stream",
		ResolvedURLExpiresAt: &expiresAt,
	}
	calls := 0
	h := &PlaybackHandler{
		VirtualFileLookup: storedURLLookup(row),
		VirtualMediaDetailedResolver: countingDetailedResolver(&calls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=fresh", URI: pinned, CandidateID: "cand-a",
		}),
	}

	res, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", false, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 1 {
		t.Fatalf("provider listing calls = %d, want 1 after the stored URL failed validation", calls)
	}
	if res.URL != "https://93.184.216.34/stream/token=fresh" {
		t.Fatalf("resolved URL = %q, want the freshly listed URL, never the unvalidated one", res.URL)
	}
}

func TestResolveVirtualInputNoStoredURLBehavesAsBefore(t *testing.T) {
	const pinned = "virtual://movie/tt-none?result=cand-a"
	row := &models.MediaFile{
		ID:       75,
		FilePath: pinned,
	}
	calls := 0
	h := &PlaybackHandler{
		VirtualFileLookup: storedURLLookup(row),
		VirtualMediaDetailedResolver: countingDetailedResolver(&calls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=fresh", URI: pinned, CandidateID: "cand-a",
		}),
	}
	saves := 0
	h.VirtualFileSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (int64, error) {
		saves++
		return 1, nil
	}

	res, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", false, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 1 {
		t.Fatalf("provider listing calls = %d, want 1 when no URL is stored", calls)
	}
	if res.URL != "https://93.184.216.34/stream/token=fresh" {
		t.Fatalf("resolved URL = %q, want the freshly listed URL", res.URL)
	}
	if saves != 0 {
		t.Fatalf("refresh writes = %d, want 0 for a row that never stored a URL", saves)
	}
}

func TestResolveVirtualInputStoredURLFromDifferentRowNotUsed(t *testing.T) {
	const pinned = "virtual://movie/tt-other?result=cand-a"
	expiresAt := time.Now().Add(time.Hour)
	// The lookup returns a row for a different concrete release, as could
	// happen if a caller resolved the path to the wrong owner row.
	otherRow := &models.MediaFile{
		ID:                   76,
		FilePath:             "virtual://movie/tt-other?result=cand-b",
		ResolvedURL:          "https://93.184.216.34/stream/token=other",
		ResolvedURLExpiresAt: &expiresAt,
	}
	calls := 0
	h := &PlaybackHandler{
		VirtualFileLookup: storedURLLookup(otherRow),
		VirtualMediaDetailedResolver: countingDetailedResolver(&calls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=fresh", URI: pinned, CandidateID: "cand-a",
		}),
	}

	res, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", false, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 1 {
		t.Fatalf("provider listing calls = %d, want 1; another row's stored URL must not be served", calls)
	}
	if res.URL != "https://93.184.216.34/stream/token=fresh" {
		t.Fatalf("resolved URL = %q, want the freshly listed URL, never another row's URL", res.URL)
	}
}

func TestResolveVirtualInputRefreshSkipsSubstitutedCandidate(t *testing.T) {
	const pinned = "virtual://movie/tt-sub?result=cand-a"
	expiredAt := time.Now().Add(-time.Hour)
	row := &models.MediaFile{
		ID:                   77,
		FilePath:             pinned,
		ResolvedURL:          "https://93.184.216.34/stream/token=old",
		ResolvedURLExpiresAt: &expiredAt,
	}
	calls := 0
	h := &PlaybackHandler{
		VirtualFileLookup: storedURLLookup(row),
		// The resolver substituted a different release; its URL must not be
		// deposited onto cand-a's row.
		VirtualMediaDetailedResolver: countingDetailedResolver(&calls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=sibling", URI: "virtual://movie/tt-sub?result=cand-b", CandidateID: "cand-b",
		}),
	}
	saves := 0
	h.VirtualFileSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (int64, error) {
		saves++
		return 1, nil
	}

	_, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", false, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 1 {
		t.Fatalf("provider listing calls = %d, want 1", calls)
	}
	if saves != 0 {
		t.Fatalf("refresh writes = %d, want 0 for a substituted sibling", saves)
	}
}

// TestResolveVirtualInputForceRefreshKeepsDurableIdentity proves a forced
// refresh bypasses cached transport but not identity: the provider renumbered
// the same release under a new result id, the resolver re-matches it by the
// row's durable identity, and the new id is adopted — instead of failing
// same-release recovery because the forced path skipped the identity lookup.
func TestResolveVirtualInputForceRefreshKeepsDurableIdentity(t *testing.T) {
	const pinned = "virtual://movie/tt-renumber?result=cand-old"
	const rematchedURI = "virtual://movie/tt-renumber?result=cand-new"
	expiresAt := time.Now().Add(2 * time.Hour)
	row := &models.MediaFile{
		ID:                         78,
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		MediaFolderID:              3,
		ResolvedURL:                "https://93.184.216.34/stream/token=stored",
		ResolvedURLExpiresAt:       &expiresAt,
		ProviderVideoHash:          "HASH1",
		ProviderReleaseName:        "Movie.2024.1080p",
		ProviderReleaseSize:        8_000_000_000,
		UpdatedAt:                  time.Now().Add(-time.Minute),
	}
	resolver := VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		if _, ok := virtuallibrary.PersistedCandidateIdentityFromContext(ctx); !ok {
			return ResolvedVirtualMedia{}, errors.New("durable identity missing on forced refresh")
		}
		return ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=rematched", URI: rematchedURI, CandidateID: "cand-new",
			IdentityRematched: true, ProviderVideoHash: "HASH1",
			ProviderReleaseName: "Movie.2024.1080p", ProviderReleaseSize: 8_000_000_000,
		}, nil
	})
	h := &PlaybackHandler{
		VirtualFileLookup:            storedURLLookup(row),
		VirtualMediaDetailedResolver: resolver,
	}
	var saved []models.VirtualFilePersistArgs
	h.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		saved = append(saved, args)
		return VirtualFileMetadataUpdateResult{RowsAffected: 1, MetadataUpdated: true, IdentityAdopted: true}, nil
	}

	res, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", true, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if res.URL != "https://93.184.216.34/stream/token=rematched" {
		t.Fatalf("resolved URL = %q, want the re-matched release URL", res.URL)
	}
	if res.CandidateID != "cand-new" {
		t.Fatalf("candidate id = %q, want the re-identified candidate", res.CandidateID)
	}
	if len(saved) != 1 || saved[0].AdoptPath != rematchedURI {
		t.Fatalf("adoption writes = %#v, want exactly 1 adopting %q", saved, rematchedURI)
	}
}

// storedURLStreamFileResolver is a serve-layer file resolver that supports the
// exact-path lookup the stored-URL shortcut uses.
type storedURLStreamFileResolver struct {
	row *models.MediaFile
}

func (r *storedURLStreamFileResolver) GetByID(_ context.Context, _ int) (*models.MediaFile, error) {
	if r.row == nil {
		return nil, ErrVirtualCandidateNotFound
	}
	return r.row, nil
}

func (r *storedURLStreamFileResolver) GetByPath(_ context.Context, path string) (*models.MediaFile, error) {
	if r.row == nil || r.row.FilePath != path {
		return nil, ErrVirtualCandidateNotFound
	}
	return r.row, nil
}

func TestStreamResolveVirtualInputUsesStoredURLWithoutListing(t *testing.T) {
	const pinned = "virtual://movie/tt-serve?result=cand-a"
	expiresAt := time.Now().Add(time.Hour)
	row := &models.MediaFile{
		ID:                   81,
		FilePath:             pinned,
		ResolvedURL:          "https://93.184.216.34/stream/token=stored",
		ResolvedURLExpiresAt: &expiresAt,
	}
	calls := 0
	h := NewStreamHandler(nil, &storedURLStreamFileResolver{row: row})
	h.VirtualMediaDetailedResolver = countingDetailedResolver(&calls, ResolvedVirtualMedia{
		URL: "https://93.184.216.34/stream/token=fresh", URI: pinned, CandidateID: "cand-a",
	})

	res, cleanup, err := h.resolveVirtualInputURIExcluding(context.Background(), row, 1, "profile", false, nil, false)
	if err != nil {
		t.Fatalf("resolveVirtualInputURIExcluding error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 0 {
		t.Fatalf("provider listing calls = %d, want 0 for a usable stored URL", calls)
	}
	if res.URL != row.ResolvedURL {
		t.Fatalf("resolved URL = %q, want the stored URL %q", res.URL, row.ResolvedURL)
	}
	if res.CandidateID != "cand-a" {
		t.Fatalf("candidate id = %q, want cand-a", res.CandidateID)
	}
}

func TestStreamResolveVirtualInputExpiredStoredURLRefreshes(t *testing.T) {
	const pinned = "virtual://movie/tt-serve-expired?result=cand-a"
	expiredAt := time.Now().Add(-time.Hour)
	newExpiry := time.Now().Add(2 * time.Hour)
	row := &models.MediaFile{
		ID:                   82,
		FilePath:             pinned,
		ResolvedURL:          "https://93.184.216.34/stream/token=old",
		ResolvedURLExpiresAt: &expiredAt,
	}
	calls := 0
	h := NewStreamHandler(nil, &storedURLStreamFileResolver{row: row})
	h.VirtualMediaDetailedResolver = countingDetailedResolver(&calls, ResolvedVirtualMedia{
		URL: "https://93.184.216.34/stream/token=new", URI: pinned, CandidateID: "cand-a",
		ExpiresAt: newExpiry,
	})
	saved := 0
	h.VirtualFileSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
		saved++
		if args.ResolvedURL != "https://93.184.216.34/stream/token=new" {
			t.Errorf("refreshed resolved_url = %q, want the fresh URL", args.ResolvedURL)
		}
		return 1, nil
	}

	res, cleanup, err := h.resolveVirtualInputURIExcluding(context.Background(), row, 1, "profile", false, nil, false)
	if err != nil {
		t.Fatalf("resolveVirtualInputURIExcluding error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 1 {
		t.Fatalf("provider listing calls = %d, want 1 for an expired stored URL", calls)
	}
	if res.URL != "https://93.184.216.34/stream/token=new" {
		t.Fatalf("resolved URL = %q, want the freshly listed URL", res.URL)
	}
	if saved != 1 {
		t.Fatalf("refresh writes = %d, want 1", saved)
	}
}

// TestFallbackServesPersistedCandidateWithinTrustWindow proves the stale-source
// fallback prefers the session's persisted same-identity candidate over a
// freshly listed sibling. The stored URL is the row's own, so a viewer replaying
// a release the provider re-listed without still gets the bytes they selected,
// and no sibling is substituted.
func TestFallbackServesPersistedCandidateWithinTrustWindow(t *testing.T) {
	const (
		neutral = "virtual://movie/tt-fallback-window"
		pinned  = neutral + "?result=cand-a"
		sibling = neutral + "?result=cand-b"
	)
	row := &models.MediaFile{
		ID:                         91,
		ContentID:                  "tt-fallback-window",
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		UpdatedAt:                  time.Now(),
		ResolvedURL:                "https://93.184.216.34/stream/token=stored",
		ProviderVideoHash:          "hash-a",
		ProviderReleaseName:        "Movie.2024.1080p",
	}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.VirtualCandidateTrustWindow = func() time.Duration { return 720 * time.Hour }
	h.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "cand-b", URI: sibling, Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac",
		}}, nil
	})
	elig := virtualFallbackEligibility{sessionBound: true, rotationAllowed: false, releaseID: "cand-a"}

	got := h.fallbackResolveStaleVirtualSource(context.Background(), row, 1, "profile", elig)
	if got == nil {
		t.Fatal("expected the persisted candidate to be served, got nil")
	}
	if got.URL != row.ResolvedURL || got.URI != pinned {
		t.Fatalf("served url=%q uri=%q, want the persisted row %q %q", got.URL, got.URI, row.ResolvedURL, pinned)
	}
}

// TestFallbackDisplayDrivenCannotSwapLiveRelease proves the display-driven
// fallback (session-bound without rotation) never substitutes a sibling when
// the persisted row's URL cannot be served inside the trust window. It returns
// nil so the caller surfaces the original failure, preserving the release under
// the viewer.
func TestFallbackDisplayDrivenCannotSwapLiveRelease(t *testing.T) {
	const (
		neutral = "virtual://movie/tt-display-window"
		pinned  = neutral + "?result=cand-a"
		sibling = neutral + "?result=cand-b"
	)
	expired := time.Now().Add(-time.Hour)
	row := &models.MediaFile{
		ID:                         92,
		ContentID:                  "tt-display-window",
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		UpdatedAt:                  time.Now(),
		ResolvedURL:                "https://93.184.216.34/stream/token=expired",
		ResolvedURLExpiresAt:       &expired,
		ProviderVideoHash:          "hash-a",
		ProviderReleaseName:        "Movie.2024.1080p",
	}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.VirtualCandidateTrustWindow = func() time.Duration { return 720 * time.Hour }
	h.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "cand-b", URI: sibling, Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac",
		}}, nil
	})
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{}, fmt.Errorf("trusted persisted virtual candidate %q is no longer listed and candidate rotation was not requested: %w", "cand-a", virtuallibrary.ErrPersistedCandidateTrusted)
	})
	elig := virtualFallbackEligibility{sessionBound: true, rotationAllowed: false, releaseID: "cand-a"}

	if got := h.fallbackResolveStaleVirtualSource(context.Background(), row, 1, "profile", elig); got != nil {
		t.Fatalf("fallback served %q, want nil (no sibling substitution)", got.URI)
	}
}

// TestResolveVirtualInputStoredURLLookupErrorFallsBack proves a lookup failure
// is treated as a cache miss: the resolve still lists the provider.
func TestResolveVirtualInputStoredURLLookupErrorFallsBack(t *testing.T) {
	const pinned = "virtual://movie/tt-lookup-error?result=cand-a"
	calls := 0
	h := &PlaybackHandler{
		VirtualFileLookup: func(_ context.Context, _ string) (*models.MediaFile, error) {
			return nil, errors.New("lookup unavailable")
		},
		VirtualMediaDetailedResolver: countingDetailedResolver(&calls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=fresh", URI: pinned, CandidateID: "cand-a",
		}),
	}

	res, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", false, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 1 {
		t.Fatalf("provider listing calls = %d, want 1 after a lookup failure", calls)
	}
	if res.URL != "https://93.184.216.34/stream/token=fresh" {
		t.Fatalf("resolved URL = %q, want the freshly listed URL", res.URL)
	}
}

// TestVirtualResolveContextTrustsDeliveredURLLessRow pins the URL-less trust
// path: a row that delivered bytes (last_delivered_at) and carries a durable
// identity is trusted inside the window even though it never persisted a
// resolved_url. The resolver then re-identifies the same release by identity
// instead of by URL.
func TestVirtualResolveContextTrustsDeliveredURLLessRow(t *testing.T) {
	const pinned = "virtual://movie/tt-urlless?result=cand-a"
	now := time.Now()
	window := 720 * time.Hour
	row := &models.MediaFile{
		ID:                  101,
		FilePath:            pinned,
		UpdatedAt:           now,
		LastDeliveredAt:     &now,
		ProviderVideoHash:   "hash-a",
		ProviderReleaseName: "Movie.2024.1080p",
	}
	ctx := virtualResolveContextWithPersistedTrust(context.Background(), row, now, window)
	if !virtuallibrary.PersistedCandidateTrusted(ctx) {
		t.Fatal("a delivered URL-less row with identity inside the window must be trusted")
	}
}

// TestVirtualResolveContextTrustsIdentitylessDeliveredRow proves the trust
// predicate accepts an identity-less row that carries transport evidence (a
// recorded delivery) inside the window. It is still the viewer's selection, so a
// delisted pin must refuse a sibling and re-serve the stored evidence rather than
// rotate; durable identity only adds the ability to re-match a renumbered same
// release on top of that.
func TestVirtualResolveContextTrustsIdentitylessDeliveredRow(t *testing.T) {
	const pinned = "virtual://movie/tt-urlless-noid?result=cand-a"
	now := time.Now()
	row := &models.MediaFile{
		ID:              102,
		FilePath:        pinned,
		UpdatedAt:       now,
		LastDeliveredAt: &now,
	}
	ctx := virtualResolveContextWithPersistedTrust(context.Background(), row, now, 720*time.Hour)
	if !virtuallibrary.PersistedCandidateTrusted(ctx) {
		t.Fatal("a delivered row inside the window must be trusted even without a durable identity")
	}
	if _, ok := virtuallibrary.PersistedCandidateIdentityFromContext(ctx); ok {
		t.Fatal("an identity-less row must not thread a durable identity")
	}
}

// TestVirtualResolveContextRejectsRowWithoutTransportEvidence proves trust still
// requires transport evidence: a row that never resolved and never delivered has
// no persisted candidate to prefer, so it keeps the pre-window refusal/rotation
// behavior even when the window is open.
func TestVirtualResolveContextRejectsRowWithoutTransportEvidence(t *testing.T) {
	const pinned = "virtual://movie/tt-no-evidence?result=cand-a"
	now := time.Now()
	row := &models.MediaFile{
		ID:                  105,
		FilePath:            pinned,
		UpdatedAt:           now,
		ProviderVideoHash:   "hash-a",
		ProviderReleaseName: "Movie.2024.1080p",
	}
	ctx := virtualResolveContextWithPersistedTrust(context.Background(), row, now, 720*time.Hour)
	if virtuallibrary.PersistedCandidateTrusted(ctx) {
		t.Fatal("a row with no stored URL and no delivery stamp must never be trusted")
	}
}

// TestVirtualResolveContextURLLessRowOutsideWindowNotTrusted proves the window
// still gates the URL-less path: the old refusal/rotation behavior applies once
// the row is outside it.
func TestVirtualResolveContextURLLessRowOutsideWindowNotTrusted(t *testing.T) {
	const pinned = "virtual://movie/tt-urlless-old?result=cand-a"
	now := time.Now()
	delivered := now.Add(-60 * 24 * time.Hour)
	row := &models.MediaFile{
		ID:                103,
		FilePath:          pinned,
		UpdatedAt:         delivered,
		LastDeliveredAt:   &delivered,
		ProviderVideoHash: "hash-a",
	}
	ctx := virtualResolveContextWithPersistedTrust(context.Background(), row, now, 720*time.Hour)
	if virtuallibrary.PersistedCandidateTrusted(ctx) {
		t.Fatal("a URL-less row outside the trust window must not be trusted")
	}
}

// TestResolveVirtualInputURLLessDeliveredRowCarriesTrust proves the handler
// boundary: resolving a URL-less delivered row inside the window marks the
// resolve trusted, so the resolver refuses a sibling instead of swapping the
// release. Outside the window the same resolve carries no trust and the
// ordinary rotation behavior holds.
func TestResolveVirtualInputURLLessDeliveredRowCarriesTrust(t *testing.T) {
	const pinned = "virtual://movie/tt-urlless-resolve?result=cand-a"
	now := time.Now()
	row := &models.MediaFile{
		ID:                  104,
		FilePath:            pinned,
		UpdatedAt:           now,
		LastDeliveredAt:     &now,
		ProviderVideoHash:   "hash-a",
		ProviderReleaseName: "Movie.2024.1080p",
	}
	var sawTrust []bool
	sibling := ResolvedVirtualMedia{
		URL: "https://93.184.216.34/stream/token=sibling",
		URI: "virtual://movie/tt-urlless-resolve?result=cand-b", CandidateID: "cand-b",
	}
	resolver := VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		trusted := virtuallibrary.PersistedCandidateTrusted(ctx)
		sawTrust = append(sawTrust, trusted)
		if trusted {
			return ResolvedVirtualMedia{}, fmt.Errorf("trusted persisted virtual candidate %q is no longer listed: %w", "cand-a", virtuallibrary.ErrPersistedCandidateTrusted)
		}
		return sibling, nil
	})

	h := &PlaybackHandler{
		VirtualFileLookup:            storedURLLookup(row),
		VirtualMediaDetailedResolver: resolver,
		VirtualCandidateTrustWindow:  func() time.Duration { return 720 * time.Hour },
	}
	if _, _, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", false, nil, ""); err == nil {
		t.Fatal("a trusted URL-less delivered row must refuse a sibling substitution")
	}
	// Inside the window every attempt (the initial resolve plus the bounded
	// provider-outage retries the handler now performs for a trusted
	// empty-listing refusal) threads trust, so no attempt may substitute.
	if len(sawTrust) == 0 {
		t.Fatalf("inside the window no resolve ran; want at least one trusted attempt")
	}
	for i, trusted := range sawTrust {
		if !trusted {
			t.Fatalf("inside the window attempt %d trust flag = false, want true for every attempt: %v", i, sawTrust)
		}
	}

	// Outside the window the same row resolves through the ordinary path: no
	// trust is threaded and the resolver may rotate.
	oldRow := *row
	oldRow.UpdatedAt = now.Add(-60 * 24 * time.Hour)
	oldRow.LastDeliveredAt = &oldRow.UpdatedAt
	h.VirtualFileLookup = storedURLLookup(&oldRow)
	sawTrust = nil
	res, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", false, nil, "")
	if err != nil {
		t.Fatalf("outside the window resolve error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if res.CandidateID != "cand-b" {
		t.Fatalf("outside the window resolved candidate = %q, want the ordinary sibling", res.CandidateID)
	}
	if len(sawTrust) != 1 || sawTrust[0] {
		t.Fatalf("outside the window the resolve trust flag = %v, want [false]", sawTrust)
	}
}
