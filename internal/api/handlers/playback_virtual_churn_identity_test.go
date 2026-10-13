package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// churnIdentityFileResolver is the minimal PlaybackHandler file resolver the
// owning-candidate identity recovery needs: an exact-path lookup keyed by the
// concrete ?result= path.
type churnIdentityFileResolver struct {
	byID   map[int]*models.MediaFile
	byPath map[string]*models.MediaFile
}

func (r *churnIdentityFileResolver) GetByID(_ context.Context, id int) (*models.MediaFile, error) {
	if row, ok := r.byID[id]; ok {
		return row, nil
	}
	return nil, ErrVirtualCandidateNotFound
}

func (r *churnIdentityFileResolver) GetByPath(_ context.Context, path string) (*models.MediaFile, error) {
	if row, ok := r.byPath[path]; ok {
		return row, nil
	}
	return nil, ErrVirtualCandidateNotFound
}

// TestEnrichSessionBoundCandidateIdentityRecoversOwningRow proves the session's
// durable identity and stored URL are recovered from the catalog row that owns
// the concrete candidate path when the row in hand is the provider-neutral
// placeholder: only the identity/URL evidence is overlaid, and the caller's row
// keeps its id, ownership and verdict.
func TestEnrichSessionBoundCandidateIdentityRecoversOwningRow(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-enrich?result=cand-a"
	expires := time.Now().Add(time.Hour)
	updated := time.Now().Add(-2 * time.Minute)
	anchor := &models.MediaFile{
		ID: 601, ContentID: "movie-enrich", EpisodeID: "ep-1",
		FilePath: pinnedURI, MediaFolderID: 7, VirtualOwnerInstallationID: 0,
	}
	owner := &models.MediaFile{
		ID: 602, ContentID: "movie-enrich", EpisodeID: "ep-1",
		FilePath: pinnedURI, MediaFolderID: 7, VirtualOwnerInstallationID: 1,
		ProviderReleaseName: "Movie.2024.1080p.WEB-DL", ProviderReleaseSize: 8_000_000_000,
		ResolvedURL:          "https://93.184.216.34/stream/token=stored",
		ResolvedURLExpiresAt: &expires,
		ProviderRequestHeaders: map[string]string{
			"Referer": "https://provider.example/player",
		},
		UpdatedAt: updated,
	}
	h := &PlaybackHandler{fileResolver: &churnIdentityFileResolver{byPath: map[string]*models.MediaFile{pinnedURI: owner}}}

	got := h.enrichSessionBoundCandidateIdentity(context.Background(), anchor)
	if got == anchor {
		t.Fatal("enrichment did not recover the owning row's identity")
	}
	if got.ID != anchor.ID || got.VirtualOwnerInstallationID != anchor.VirtualOwnerInstallationID {
		t.Fatalf("enriched row = id %d owner %d, want the caller's row unchanged", got.ID, got.VirtualOwnerInstallationID)
	}
	if got.ProviderReleaseName != owner.ProviderReleaseName || got.ProviderReleaseSize != owner.ProviderReleaseSize {
		t.Fatalf("enriched identity = (%q, %d), want the owning row's name+size tier", got.ProviderReleaseName, got.ProviderReleaseSize)
	}
	if got.ResolvedURL != owner.ResolvedURL || got.ResolvedURLExpiresAt == nil || !got.ResolvedURLExpiresAt.Equal(expires) {
		t.Fatalf("enriched stored URL = (%q, %v), want the owning row's", got.ResolvedURL, got.ResolvedURLExpiresAt)
	}
	if got.ProviderRequestHeaders["Referer"] != "https://provider.example/player" {
		t.Fatalf("enriched headers = %#v, want the owning row's stored set", got.ProviderRequestHeaders)
	}
	if !got.UpdatedAt.Equal(updated) {
		t.Fatalf("enriched updated_at = %v, want the owning row's %v", got.UpdatedAt, updated)
	}
	if (got.FailedAt != nil) != (anchor.FailedAt != nil) {
		t.Fatalf("enriched failed_at = %v, want the caller's row's verdict untouched", got.FailedAt)
	}
}

// TestEnrichSessionBoundCandidateIdentityRefusesForeignRow proves ownership is
// asserted beyond the exact path: a different episode's or a different
// content's row that happens to collide on the path is never used, so the
// recovery cannot anchor a session on bytes the viewer never selected.
func TestEnrichSessionBoundCandidateIdentityRefusesForeignRow(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-foreign?result=cand-a"
	anchor := &models.MediaFile{
		ID: 611, ContentID: "movie-foreign", EpisodeID: "ep-1",
		FilePath: pinnedURI, MediaFolderID: 7,
	}
	for _, tc := range []struct {
		name  string
		owner *models.MediaFile
	}{
		{
			name: "different episode",
			owner: &models.MediaFile{
				ID: 612, ContentID: "movie-foreign", EpisodeID: "ep-2",
				FilePath: pinnedURI, MediaFolderID: 7, ProviderReleaseName: "Movie.2024",
			},
		},
		{
			name: "different content",
			owner: &models.MediaFile{
				ID: 613, ContentID: "movie-other", EpisodeID: "ep-1",
				FilePath: pinnedURI, MediaFolderID: 7, ProviderReleaseName: "Movie.2024",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &PlaybackHandler{fileResolver: &churnIdentityFileResolver{byPath: map[string]*models.MediaFile{pinnedURI: tc.owner}}}
			if got := h.enrichSessionBoundCandidateIdentity(context.Background(), anchor); got != anchor {
				t.Fatalf("enrichment used a foreign row: %#v", got)
			}
		})
	}
}

// TestEnrichSessionBoundCandidateIdentityNoopsWhenComplete proves the common
// warm path pays nothing: a row that already carries both a durable identity
// and a stored URL is returned unchanged even when a resolver is wired.
func TestEnrichSessionBoundCandidateIdentityNoopsWhenComplete(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-complete?result=cand-a"
	row := &models.MediaFile{
		ID: 621, FilePath: pinnedURI, ContentID: "movie-complete",
		ProviderReleaseName: "Movie.2024", ResolvedURL: "https://93.184.216.34/stream/x",
	}
	lookups := 0
	resolver := &countingPathResolver{calls: &lookups}
	h := &PlaybackHandler{fileResolver: resolver}
	if got := h.enrichSessionBoundCandidateIdentity(context.Background(), row); got != row {
		t.Fatalf("complete row was replaced: %#v", got)
	}
	if lookups != 0 {
		t.Fatalf("path lookups = %d, want 0 for a complete row", lookups)
	}
	// A provider-neutral placeholder path names no concrete candidate, so it is
	// never enriched even when identity-less.
	neutral := &models.MediaFile{ID: 622, FilePath: "virtual://movie/tt-complete", ContentID: "movie-complete"}
	if got := h.enrichSessionBoundCandidateIdentity(context.Background(), neutral); got != neutral {
		t.Fatalf("neutral placeholder was enriched: %#v", got)
	}
}

type countingPathResolver struct {
	calls *int
}

func (r *countingPathResolver) GetByID(_ context.Context, _ int) (*models.MediaFile, error) {
	*r.calls++
	return nil, ErrVirtualCandidateNotFound
}

func (r *countingPathResolver) GetByPath(_ context.Context, _ string) (*models.MediaFile, error) {
	*r.calls++
	return nil, ErrVirtualCandidateNotFound
}

// TestRehydratedIdentityCapturedFromOwningCandidateRow is the churn simulation
// at the handler seam: a session bound to candidate X whose anchor row in hand
// is the provider-neutral placeholder, with the identity living on the row that
// owns X. The rehydration must thread that identity into the resolve so a fresh
// listing that re-lists the same release under a new token Y is re-matched and
// adopted, with no refusal.
func TestRehydratedIdentityCapturedFromOwningCandidateRow(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-churn-identity"
		pinnedURI  = neutralURI + "?result=old"
		renumbered = neutralURI + "?result=new"
	)
	anchor := &models.MediaFile{
		ID: 631, ContentID: "movie-churn-identity", FilePath: pinnedURI, MediaFolderID: 7,
	}
	owner := &models.MediaFile{
		ID: 632, ContentID: "movie-churn-identity", FilePath: pinnedURI, MediaFolderID: 7,
		ProviderReleaseName: "Movie.2024.1080p.WEB-DL", ProviderReleaseSize: 8_000_000_000,
		UpdatedAt: time.Now().Add(-time.Minute),
	}
	if _, ok := persistedVirtualIdentity(anchor); ok {
		t.Fatal("fixture precondition: the anchor row in hand must be identity-less")
	}

	h := NewPlaybackHandler(playback.NewSessionManager(0, 0), &churnIdentityFileResolver{byPath: map[string]*models.MediaFile{pinnedURI: owner}})
	h.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
		return "http://127.0.0.1:9/unused", nil
	})
	sawIdentity := false
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		identity, ok := virtuallibrary.PersistedCandidateIdentityFromContext(ctx)
		if !ok || identity.ReleaseName != owner.ProviderReleaseName || identity.ReleaseSize != owner.ProviderReleaseSize {
			// Without the owning row's identity the resolver has nothing to
			// re-match, which is exactly the production refusal.
			return ResolvedVirtualMedia{}, absentSessionPinError("old")
		}
		sawIdentity = true
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:9/renumbered", URI: renumbered, CandidateID: "new",
			IdentityRematched:   true,
			ProviderReleaseName: owner.ProviderReleaseName, ProviderReleaseSize: owner.ProviderReleaseSize,
		}, nil
	})

	r := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil).WithContext(newAuthorizedPlaybackContext())
	resolved, err := h.resolveRehydratedVirtualSourceV3(r, anchor, "profile-1", nil, "old", "auto", 0, virtualResolveOptionsV3{sessionBound: true, sessionAnchorURI: pinnedURI})
	if err != nil {
		t.Fatalf("resolveRehydratedVirtualSourceV3 refused the renumbered same release: %v", err)
	}
	if !sawIdentity {
		t.Fatal("the resolver never received the owning row's durable identity")
	}
	if got := virtualResultCandidateID(resolved.URI); got != "new" {
		t.Fatalf("resolved candidate = %q, want the rematched renumbered token", got)
	}
}

// TestRehydratedIdentityLegacyRowServesStoredURL proves the other half of the
// churn contract: when the row in hand carries no durable identity and no owning
// row adds one, a still-live stored URL is served directly with no provider
// listing, so an identity-less legacy row is never wedged into the absent-pin
// refusal.
func TestRehydratedIdentityLegacyRowServesStoredURL(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-churn-legacy?result=old"
	expires := time.Now().Add(time.Hour)
	anchor := &models.MediaFile{
		ID: 641, ContentID: "movie-churn-legacy", FilePath: pinnedURI, MediaFolderID: 7,
		ResolvedURL:          "https://93.184.216.34/stream/token=stored",
		ResolvedURLExpiresAt: &expires,
		UpdatedAt:            time.Now().Add(-time.Minute),
	}
	calls := 0
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
		return "http://127.0.0.1:9/unused", nil
	})
	h.VirtualMediaDetailedResolver = countingDetailedResolver(&calls, ResolvedVirtualMedia{
		URL: "http://127.0.0.1:9/fresh", URI: pinnedURI, CandidateID: "old",
	})

	r := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil).WithContext(newAuthorizedPlaybackContext())
	resolved, err := h.resolveRehydratedVirtualSourceV3(r, anchor, "profile-1", nil, "old", "auto", 0, virtualResolveOptionsV3{sessionBound: true, sessionAnchorURI: pinnedURI})
	if err != nil {
		t.Fatalf("legacy stored-URL rehydration: %v", err)
	}
	if calls != 0 {
		t.Fatalf("provider listing calls = %d, want 0 for a usable stored URL", calls)
	}
	if resolved.URL != "https://93.184.216.34/stream/token=stored" {
		t.Fatalf("resolved URL = %q, want the stored URL", resolved.URL)
	}
}

// TestRehydratedIdentityDifferentReleaseStillRefused proves the recovered
// identity never weakens the invariant: when the owning row's release is
// re-listed under a genuinely different release, the rotation is still refused
// and the original absent-pin cause is preserved.
func TestRehydratedIdentityDifferentReleaseStillRefused(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-churn-different"
		pinnedURI  = neutralURI + "?result=old"
		siblingURI = neutralURI + "?result=sibling"
	)
	anchor := &models.MediaFile{
		ID: 651, ContentID: "movie-churn-different", FilePath: pinnedURI, MediaFolderID: 7,
	}
	owner := &models.MediaFile{
		ID: 652, ContentID: "movie-churn-different", FilePath: pinnedURI, MediaFolderID: 7,
		ProviderReleaseName: "Movie.2024.1080p.WEB-DL", ProviderReleaseSize: 8_000_000_000,
		UpdatedAt: time.Now().Add(-time.Minute),
	}
	sawIdentity := false
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0), &churnIdentityFileResolver{byPath: map[string]*models.MediaFile{pinnedURI: owner}})
	h.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
		return "http://127.0.0.1:9/unused", nil
	})
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		if _, ok := virtuallibrary.PersistedCandidateIdentityFromContext(ctx); ok {
			sawIdentity = true
		}
		if !VirtualCandidateRotationAllowed(ctx) {
			return ResolvedVirtualMedia{}, absentSessionPinError("old")
		}
		// A genuinely different release: no rematch and a name that does not
		// equal the owning row's identity, so the explicit comparison refuses it.
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:9/sibling", URI: siblingURI, CandidateID: "sibling",
			ProviderReleaseName: "Movie.2024.1080p.OTHER", ProviderReleaseSize: 1,
		}, nil
	})

	r := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil).WithContext(newAuthorizedPlaybackContext())
	resolved, err := h.resolveRehydratedVirtualSourceV3(r, anchor, "profile-1", nil, "old", "auto", 0, virtualResolveOptionsV3{sessionBound: true, sessionAnchorURI: pinnedURI})
	if !errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent) {
		t.Fatalf("err = %v, want the original absent-pin refusal for a different release", err)
	}
	if !sawIdentity {
		t.Fatal("fixture did not exercise the recovered identity on the refusal path")
	}
	if got := virtualResultCandidateID(resolved.URI); got != "" {
		t.Fatalf("resolved candidate = %q, want no accepted rotation", got)
	}
}
