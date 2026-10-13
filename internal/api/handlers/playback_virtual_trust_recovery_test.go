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
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// --- Evidence-coherent enrichment -----------------------------------------

// TestEnrichSessionBoundCandidateIdentityRejectsContradictoryIdentity proves the
// overlay never mixes one row's identity with another row's transport evidence:
// when the anchor and the owning row carry different durable identities, the
// enrichment leaves the anchor untouched so the ordinary resolve path runs.
func TestEnrichSessionBoundCandidateIdentityRejectsContradictoryIdentity(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-conflict?result=cand-a"
	anchor := &models.MediaFile{
		ID: 660, ContentID: "movie-conflict", EpisodeID: "ep-1", FilePath: pinnedURI, MediaFolderID: 7,
		ProviderReleaseName: "Movie.2024.1080p.WEB-DL", ProviderReleaseSize: 8_000_000_000,
		UpdatedAt: time.Now().Add(-time.Hour),
	}
	owner := &models.MediaFile{
		ID: 661, ContentID: "movie-conflict", EpisodeID: "ep-1", FilePath: pinnedURI, MediaFolderID: 7,
		ProviderReleaseName: "Movie.2024.2160p.WEB-DL", ProviderReleaseSize: 9_000_000_000,
		ResolvedURL: "https://93.184.216.34/stream/token=other-release",
		UpdatedAt:   time.Now(),
	}
	h := &PlaybackHandler{fileResolver: &churnIdentityFileResolver{byPath: map[string]*models.MediaFile{pinnedURI: owner}}}

	got := h.enrichSessionBoundCandidateIdentity(context.Background(), anchor)
	if got != anchor {
		t.Fatalf("contradictory rows were mixed: identity %q/%d with URL %q, want the anchor unchanged",
			got.ProviderReleaseName, got.ProviderReleaseSize, got.ResolvedURL)
	}
}

// TestEnrichSessionBoundCandidateIdentityCoLocatesIdentityAndURL proves the
// other mixed direction: an anchor that carries a stored URL but no identity
// must not receive a foreign identity over its own URL. The owning row's
// identity and URL travel together, so the URL is replaced with the
// same-provenance one.
func TestEnrichSessionBoundCandidateIdentityCoLocatesIdentityAndURL(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-colocate?result=cand-a"
	anchor := &models.MediaFile{
		ID: 662, ContentID: "movie-colocate", EpisodeID: "ep-1", FilePath: pinnedURI, MediaFolderID: 7,
		ResolvedURL: "https://93.184.216.34/stream/token=anchor",
	}
	owner := &models.MediaFile{
		ID: 663, ContentID: "movie-colocate", EpisodeID: "ep-1", FilePath: pinnedURI, MediaFolderID: 7,
		ProviderReleaseName: "Movie.2024.1080p.WEB-DL", ProviderReleaseSize: 8_000_000_000,
		ResolvedURL: "https://93.184.216.34/stream/token=owner",
	}
	h := &PlaybackHandler{fileResolver: &churnIdentityFileResolver{byPath: map[string]*models.MediaFile{pinnedURI: owner}}}

	got := h.enrichSessionBoundCandidateIdentity(context.Background(), anchor)
	if got == anchor {
		t.Fatal("enrichment dropped the owning row's identity and URL")
	}
	if got.ProviderReleaseName != owner.ProviderReleaseName || got.ProviderReleaseSize != owner.ProviderReleaseSize {
		t.Fatalf("identity = %q/%d, want the owning row's", got.ProviderReleaseName, got.ProviderReleaseSize)
	}
	if got.ResolvedURL != owner.ResolvedURL {
		t.Fatalf("URL = %q, want the owning row's %q (identity and URL must share provenance)", got.ResolvedURL, owner.ResolvedURL)
	}
}

// TestEnrichSessionBoundCandidateIdentityDoesNotAttachIdentityToForeignURL
// proves an anchor URL is never paired with an identity the owning row cannot
// back with transport evidence of its own: identity and URL must share a
// provenance, so a URL-less owning row contributes nothing when the anchor
// already carries an unmatched URL.
func TestEnrichSessionBoundCandidateIdentityDoesNotAttachIdentityToForeignURL(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-nourl?result=cand-a"
	anchor := &models.MediaFile{
		ID: 664, ContentID: "movie-nourl", EpisodeID: "ep-1", FilePath: pinnedURI, MediaFolderID: 7,
		ResolvedURL: "https://93.184.216.34/stream/token=anchor",
	}
	owner := &models.MediaFile{
		ID: 665, ContentID: "movie-nourl", EpisodeID: "ep-1", FilePath: pinnedURI, MediaFolderID: 7,
		ProviderReleaseName: "Movie.2024.1080p.WEB-DL", ProviderReleaseSize: 8_000_000_000,
		UpdatedAt: time.Now(),
	}
	h := &PlaybackHandler{fileResolver: &churnIdentityFileResolver{byPath: map[string]*models.MediaFile{pinnedURI: owner}}}

	if got := h.enrichSessionBoundCandidateIdentity(context.Background(), anchor); got != anchor {
		t.Fatalf("identity was attached to a foreign URL: identity %q with URL %q", got.ProviderReleaseName, got.ResolvedURL)
	}
}

// TestEnrichSessionBoundCandidateIdentityRequiresProvenContentOwnership proves
// ownership is asserted, not skipped: an anchor with no content id cannot prove
// the owning row is its candidate, so nothing is imported.
func TestEnrichSessionBoundCandidateIdentityRequiresProvenContentOwnership(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-nocontent?result=cand-a"
	anchor := &models.MediaFile{ID: 666, FilePath: pinnedURI, MediaFolderID: 7}
	owner := &models.MediaFile{
		ID: 667, ContentID: "movie-nocontent", FilePath: pinnedURI, MediaFolderID: 7,
		ProviderReleaseName: "Movie.2024.1080p.WEB-DL", ProviderReleaseSize: 8_000_000_000,
		ResolvedURL: "https://93.184.216.34/stream/token=owner",
	}
	h := &PlaybackHandler{fileResolver: &churnIdentityFileResolver{byPath: map[string]*models.MediaFile{pinnedURI: owner}}}

	if got := h.enrichSessionBoundCandidateIdentity(context.Background(), anchor); got != anchor {
		t.Fatalf("enrichment trusted an unproven (empty-content) ownership: %#v", got)
	}
}

// TestEnrichSessionBoundCandidateIdentityClonesRequestHeaders proves the
// overlay does not alias the owning row's header map: mutating the enriched row
// must not mutate the catalog row it borrowed from.
func TestEnrichSessionBoundCandidateIdentityClonesRequestHeaders(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-clone?result=cand-a"
	anchor := &models.MediaFile{ID: 668, ContentID: "movie-clone", EpisodeID: "ep-1", FilePath: pinnedURI, MediaFolderID: 7}
	owner := &models.MediaFile{
		ID: 669, ContentID: "movie-clone", EpisodeID: "ep-1", FilePath: pinnedURI, MediaFolderID: 7,
		ProviderReleaseName: "Movie.2024.1080p.WEB-DL", ProviderReleaseSize: 8_000_000_000,
		ResolvedURL:            "https://93.184.216.34/stream/token=owner",
		ProviderRequestHeaders: map[string]string{"Referer": "https://provider.example/player"},
	}
	h := &PlaybackHandler{fileResolver: &churnIdentityFileResolver{byPath: map[string]*models.MediaFile{pinnedURI: owner}}}

	got := h.enrichSessionBoundCandidateIdentity(context.Background(), anchor)
	if got == anchor || got.ProviderRequestHeaders == nil {
		t.Fatalf("headers were not overlaid: %#v", got)
	}
	got.ProviderRequestHeaders["Referer"] = "mutated"
	if owner.ProviderRequestHeaders["Referer"] != "https://provider.example/player" {
		t.Fatal("enriched headers alias the owning row's map")
	}
}

// TestEnrichSessionBoundCandidateIdentityDoesNotBorrowFreshnessWithoutURL
// proves the trust-window clock is not advanced by a row whose transport
// evidence was not imported: a URL-less owning row must not lend its newer
// timestamp to the anchor's older, unmatched evidence.
func TestEnrichSessionBoundCandidateIdentityDoesNotBorrowFreshnessWithoutURL(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-freshness?result=cand-a"
	anchorOld := time.Now().Add(-60 * 24 * time.Hour)
	anchor := &models.MediaFile{
		ID: 670, ContentID: "movie-freshness", EpisodeID: "ep-1", FilePath: pinnedURI, MediaFolderID: 7,
		ProviderReleaseName: "Movie.2024.1080p.WEB-DL", ProviderReleaseSize: 8_000_000_000,
		UpdatedAt: anchorOld,
	}
	// No identity, so the owning row cannot share provenance with the anchor's
	// identity; nothing may be imported, so the anchor keeps its timestamp.
	owner := &models.MediaFile{
		ID: 671, ContentID: "movie-freshness", EpisodeID: "ep-1", FilePath: pinnedURI, MediaFolderID: 7,
		ResolvedURL: "https://93.184.216.34/stream/token=owner",
		UpdatedAt:   time.Now(),
	}
	h := &PlaybackHandler{fileResolver: &churnIdentityFileResolver{byPath: map[string]*models.MediaFile{pinnedURI: owner}}}

	got := h.enrichSessionBoundCandidateIdentity(context.Background(), anchor)
	if !got.UpdatedAt.Equal(anchorOld) {
		t.Fatalf("updated_at = %v, want the anchor's %v (no URL imported means no fresh provenance)", got.UpdatedAt, anchorOld)
	}
	if strings.TrimSpace(got.ResolvedURL) != "" {
		t.Fatalf("URL = %q, want empty (no common provenance)", got.ResolvedURL)
	}
}

// TestOwningSessionBoundCandidateRowLookupParity proves both lookup
// implementations (the injected VirtualFileLookup and the path resolver) assert
// the same ownership: a matching content id recovers the row, an anchor with no
// content id proves nothing and is refused, and a lookup failure fails closed.
func TestOwningSessionBoundCandidateRowLookupParity(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-parity?result=cand-a"
	owner := &models.MediaFile{
		ID: 680, ContentID: "movie-parity", EpisodeID: "ep-1", FilePath: pinnedURI, MediaFolderID: 7,
		ProviderReleaseName: "Movie.2024",
	}
	for _, tc := range []struct {
		name string
		h    *PlaybackHandler
	}{
		{"injected lookup", &PlaybackHandler{VirtualFileLookup: storedURLLookup(owner)}},
		{"path resolver", &PlaybackHandler{fileResolver: &churnIdentityFileResolver{byPath: map[string]*models.MediaFile{pinnedURI: owner}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proven := &models.MediaFile{ID: 681, ContentID: "movie-parity", EpisodeID: "ep-1", FilePath: pinnedURI, MediaFolderID: 7}
			if got := tc.h.owningSessionBoundCandidateRow(context.Background(), proven); got == nil || got.ID != owner.ID {
				t.Fatalf("%s: ownership not recovered: %#v", tc.name, got)
			}
			unproven := &models.MediaFile{ID: 682, FilePath: pinnedURI, MediaFolderID: 7}
			if got := tc.h.owningSessionBoundCandidateRow(context.Background(), unproven); got != nil {
				t.Fatalf("%s: ownership accepted without a content id: %#v", tc.name, got)
			}
		})
	}

	// A lookup that cannot run fails closed: no row means no overlay, and the
	// caller keeps its own row.
	failing := &models.MediaFile{ID: 683, ContentID: "movie-parity", EpisodeID: "ep-1", FilePath: pinnedURI, MediaFolderID: 7}
	for _, tc := range []struct {
		name string
		h    *PlaybackHandler
	}{
		{"injected lookup error", &PlaybackHandler{VirtualFileLookup: func(context.Context, string) (*models.MediaFile, error) {
			return nil, errors.New("lookup unavailable")
		}}},
		{"path resolver error", &PlaybackHandler{fileResolver: erroringPathResolver{}}},
		{"path resolver not found", &PlaybackHandler{fileResolver: &churnIdentityFileResolver{}}},
		{"no resolver", &PlaybackHandler{}},
	} {
		if got := tc.h.owningSessionBoundCandidateRow(context.Background(), failing); got != nil {
			t.Fatalf("%s: expected nil (fail closed), got %#v", tc.name, got)
		}
	}
}

// erroringPathResolver is a path resolver whose lookups fail, for the
// fail-closed parity cases.
type erroringPathResolver struct{}

func (erroringPathResolver) GetByID(context.Context, int) (*models.MediaFile, error) {
	return nil, errors.New("lookup unavailable")
}

func (erroringPathResolver) GetByPath(context.Context, string) (*models.MediaFile, error) {
	return nil, errors.New("lookup unavailable")
}

// --- Trust gate ------------------------------------------------------------

// TestVirtualResolveContextIdentitylessTrustRequiresUsableStoredURL pins the
// relaxed trust gate: a row without durable identity is trusted only when it
// carries a stored URL that can actually be served (non-empty, unexpired). A
// delivery stamp alone is evidence bytes flowed once, not that a URL can be
// served now, so it must not claim resumability; an expired or empty URL cannot
// back the recovery either.
func TestVirtualResolveContextIdentitylessTrustRequiresUsableStoredURL(t *testing.T) {
	now := time.Now()
	const window = 720 * time.Hour
	recent := now.Add(-time.Minute)
	expired := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	const usableURL = "https://93.184.216.34/stream/token=stored"

	tests := []struct {
		name      string
		row       *models.MediaFile
		wantTrust bool
	}{
		{
			name:      "delivery stamp alone is not resumable evidence",
			row:       &models.MediaFile{ID: 710, UpdatedAt: recent, LastDeliveredAt: &recent},
			wantTrust: false,
		},
		{
			name:      "empty URL with a delivery stamp is not trusted",
			row:       &models.MediaFile{ID: 711, UpdatedAt: recent, LastDeliveredAt: &recent, ResolvedURL: "  "},
			wantTrust: false,
		},
		{
			name:      "expired URL is not trusted",
			row:       &models.MediaFile{ID: 712, UpdatedAt: recent, ResolvedURL: usableURL, ResolvedURLExpiresAt: &expired},
			wantTrust: false,
		},
		{
			name:      "usable URL is trusted",
			row:       &models.MediaFile{ID: 713, UpdatedAt: recent, ResolvedURL: usableURL, ResolvedURLExpiresAt: &future},
			wantTrust: true,
		},
		{
			name:      "usable URL outside the window is not trusted",
			row:       &models.MediaFile{ID: 714, UpdatedAt: now.Add(-60 * 24 * time.Hour), ResolvedURL: usableURL, ResolvedURLExpiresAt: &future},
			wantTrust: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := virtualResolveContextWithPersistedTrust(context.Background(), tc.row, now, window)
			if got := virtuallibrary.PersistedCandidateTrusted(ctx); got != tc.wantTrust {
				t.Fatalf("trust = %v, want %v", got, tc.wantTrust)
			}
			if _, ok := virtuallibrary.PersistedCandidateIdentityFromContext(ctx); ok {
				t.Fatal("an identity-less row must never thread a durable identity")
			}
		})
	}
}

// --- Handler-level absent pin: stored-URL recovery ------------------------

// pinlessIdentitylessRow is an identity-less persisted candidate whose concrete
// ?result= pin can be looked up and whose stored URL is the only transport
// evidence. ProbeUpdatedAt is set so the durable-resume derivation is skipped
// and the resolve reaches the provider resolver.
func pinlessIdentitylessRow(storedURL string, expiresAt *time.Time) *models.MediaFile {
	return &models.MediaFile{
		ID:                         850,
		ContentID:                  "movie-pinless",
		FilePath:                   "virtual://movie/tt-pinless?result=cand-a",
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
		UpdatedAt:                  time.Now(),
		ResolvedURL:                storedURL,
		ResolvedURLExpiresAt:       expiresAt,
		ProbeUpdatedAt:             timePtr(time.Now()),
	}
}

// pinlessIdentityBackedRow adds a durable identity and a delivery stamp so the
// trust flag is still set when the stored URL itself is unusable.
func pinlessIdentityBackedRow(storedURL string, expiresAt *time.Time) *models.MediaFile {
	row := pinlessIdentitylessRow(storedURL, expiresAt)
	row.ProviderReleaseName = "Movie.2024.1080p"
	row.ProviderReleaseSize = 8_000_000_000
	row.LastDeliveredAt = timePtr(time.Now())
	return row
}

// trustedAbsentResolver reports the trust sentinel only when the resolve was
// threaded with trust, mirroring the resolver contract.
func trustedAbsentResolver(t *testing.T, sawTrust *bool) VirtualMediaDetailedResolver {
	t.Helper()
	return VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		trusted := virtuallibrary.PersistedCandidateTrusted(ctx)
		if sawTrust != nil {
			*sawTrust = trusted
		}
		if !trusted {
			return ResolvedVirtualMedia{}, errors.New("resolve was not threaded with persisted-candidate trust")
		}
		return ResolvedVirtualMedia{}, fmt.Errorf("trusted persisted virtual candidate %q is no longer listed: %w", "cand-a", virtuallibrary.ErrPersistedCandidateTrusted)
	})
}

// TestAbsentPinRecoversUsableStoredURL proves a trusted-but-pinless session
// serves the row's own stored URL: the resolver reports the pin absent, and the
// handler recovers the same release from the stored URL instead of wedging.
func TestAbsentPinRecoversUsableStoredURL(t *testing.T) {
	expiresAt := time.Now().Add(time.Hour)
	file := pinlessIdentitylessRow("https://93.184.216.34/stream/token=stored", &expiresAt)

	sawTrust := false
	detailedCalls := 0
	h := &PlaybackHandler{
		VirtualCandidateTrustWindow: func() time.Duration { return 720 * time.Hour },
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
			func(context.Context, string, int, string, int) (string, error) {
				return "https://93.184.216.34/legacy", nil
			}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			detailedCalls++
			return trustedAbsentResolver(t, &sawTrust).ResolveVirtualMediaDetailed(ctx, "", 0, 0, "", false, nil, "")
		}),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false,
		virtualResolveOptionsV3{explicitSelection: true})
	if !sawTrust {
		t.Fatal("an identity-less row with a usable stored URL must be trusted")
	}
	if err != nil {
		t.Fatalf("trusted pinless resolve wedged instead of recovering: %v", err)
	}
	if detailedCalls != 1 {
		t.Fatalf("detailed resolver called %d times, want 1", detailedCalls)
	}
	if resolved.URL != file.ResolvedURL {
		t.Fatalf("resolved URL = %q, want the stored URL %q", resolved.URL, file.ResolvedURL)
	}
	if resolved.URI != file.FilePath {
		t.Fatalf("resolved URI = %q, want the pinned candidate %q", resolved.URI, file.FilePath)
	}
}

// TestAbsentPinWithoutUsableStoredURLFailsBoundedly proves the recovery is
// bounded and never substitutes a sibling: when the trusted row's stored URL is
// missing, expired, or fails validation, the resolve fails with the trust
// sentinel instead of serving the listed sibling.
func TestAbsentPinWithoutUsableStoredURLFailsBoundedly(t *testing.T) {
	const siblingURI = "virtual://movie/tt-pinless?result=cand-b"
	tests := []struct {
		name      string
		storedURL string
		expiresAt *time.Time
	}{
		{name: "missing url", storedURL: "", expiresAt: nil},
		{name: "expired url", storedURL: "https://93.184.216.34/stream/token=expired", expiresAt: timePtr(time.Now().Add(-time.Hour))},
		{name: "invalid url", storedURL: "http://127.0.0.1/private/stream", expiresAt: timePtr(time.Now().Add(time.Hour))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			file := pinlessIdentityBackedRow(tc.storedURL, tc.expiresAt)
			h := &PlaybackHandler{
				VirtualCandidateTrustWindow: func() time.Duration { return 720 * time.Hour },
				VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
					func(context.Context, string, int, string, int) (string, error) {
						return "https://93.184.216.34/legacy", nil
					}),
				VirtualMediaDetailedResolver: trustedAbsentResolver(t, nil),
				VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(
					func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
						return []VirtualPlaybackStream{{
							ID: "cand-b", URI: siblingURI,
							Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
						}}, nil
					}),
			}

			req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
			resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false,
				virtualResolveOptionsV3{explicitSelection: true})
			if !errors.Is(err, virtuallibrary.ErrPersistedCandidateTrusted) {
				t.Fatalf("err = %v, want the trust sentinel (bounded failure)", err)
			}
			if resolved.URI == siblingURI {
				t.Fatal("a sibling was substituted for the trusted pin")
			}
		})
	}
}

// TestIdentitylessDeliveredOnlyRowIsNotTrusted proves the handler boundary that
// closes the wedge: a row with only a delivery stamp (no URL, no identity) does
// not thread trust, so the resolver reports the ordinary absent/rotation cause
// and the handler does not wedge on the trust sentinel.
func TestIdentitylessDeliveredOnlyRowIsNotTrusted(t *testing.T) {
	now := time.Now()
	file := &models.MediaFile{
		ID: 851, ContentID: "movie-delivered-only",
		FilePath: "virtual://movie/tt-delivered-only?result=cand-a", Container: "virtual",
		UpdatedAt: now, LastDeliveredAt: &now,
	}
	sawTrust := true
	h := &PlaybackHandler{
		VirtualCandidateTrustWindow: func() time.Duration { return 720 * time.Hour },
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
			func(context.Context, string, int, string, int) (string, error) {
				return "https://93.184.216.34/legacy", nil
			}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			sawTrust = virtuallibrary.PersistedCandidateTrusted(ctx)
			return ResolvedVirtualMedia{URL: "https://93.184.216.34/stream/token=fresh", URI: file.FilePath, CandidateID: "cand-a"}, nil
		}),
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false,
		virtualResolveOptionsV3{explicitSelection: true}); err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if sawTrust {
		t.Fatal("a delivery stamp alone must not thread persisted-candidate trust")
	}
}
