package handlers

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// versionsCheckTestResolver is a minimal FilePathResolver for driving
// checkVersion without a database. It returns a fixed row.
type versionsCheckTestResolver struct{ file *models.MediaFile }

func (r versionsCheckTestResolver) GetByID(context.Context, int) (*models.MediaFile, error) {
	if r.file == nil {
		return nil, errors.New("file not found")
	}
	return r.file, nil
}

// permissiveItemAccess authorizes every parent item, so a unit test can reach
// the provider classification without a catalog.
type permissiveItemAccess struct{}

func (permissiveItemAccess) EnsureAccessible(context.Context, string, catalog.AccessFilter) error {
	return nil
}

// virtualVersionRow builds a virtual media file row with durable provider
// identity (so the absent-pin/identity branches are reachable) and no failed_at.
func virtualVersionRow(result, hash string) *models.MediaFile {
	return &models.MediaFile{
		ID:                         1,
		ContentID:                  "movie-temporary",
		MediaFolderID:              1,
		FilePath:                   fmt.Sprintf("virtual://movie/tt-temporary?result=%s", result),
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
		ProviderVideoHash:          hash,
		ProviderReleaseName:        "Movie.2024.1080p",
	}
}

// TestVirtualProviderListingTemporaryErrorClassification pins the
// transport-temporary classifier independently of the version-check handler: a
// typed provider-outage sentinel, a pending release, a trusted-persisted
// absence, a context deadline, a wrapped RPC failure, and the empty-listing
// shape are all temporary; a bare confirmed-dead message is not.
func TestVirtualProviderListingTemporaryErrorClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"typed provider unavailable 503", fmt.Errorf("%w: streaming provider returned status 503", resolver.ErrProviderUnavailable), true},
		{"wrapped provider unavailable", fmt.Errorf("resolve virtual input: %w: x", resolver.ErrProviderUnavailable), true},
		{"provider failed recently", fmt.Errorf("%w: provider listing failed recently", resolver.ErrProviderUnavailable), true},
		{"provider pending", fmt.Errorf("%w (still fetching)", virtuallibrary.ErrProviderPending), true},
		{"persisted candidate trusted absent", fmt.Errorf("no longer listed: %w", virtuallibrary.ErrPersistedCandidateTrusted), true},
		{"context deadline", context.DeadlineExceeded, true},
		{"context canceled", context.Canceled, true},
		{"joined deadline with dead text", errors.Join(context.DeadlineExceeded, errors.New("no matching candidate")), true},
		{"plain request failed", errors.New("virtual stream provider 7 request failed"), true},
		{"resolver not installed", errors.New("virtual stream resolver is not installed"), true},
		{"empty listing", errors.New("no streams available from provider"), true},
		{"confirmed dead only", errors.New("virtual stream provider returned no matching candidate"), false},
		{"no usable stream only", errors.New("virtual transcode provider returned no usable stream"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isVirtualProviderListingTemporaryError(tc.err); got != tc.want {
				t.Fatalf("isVirtualProviderListingTemporaryError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestVirtualCandidateDeadErrorExcludesTemporary pins the negative half: the
// string-based dead classifier must never return true for a transport-temporary
// cause, even when the same joined error carries a "no matching candidate"
// fallback text. Without this a 5xx flap joined with an empty fallback listing
// would stamp a healthy release dead.
func TestVirtualCandidateDeadErrorExcludesTemporary(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"dead only", errors.New("virtual stream provider returned no matching candidate"), true},
		{"dead joined transient", errors.Join(errors.New("streaming provider returned status 503"), errors.New("no matching candidate")), false},
		{"dead joined typed outage", fmt.Errorf("%w; virtual stream provider returned no usable stream", resolver.ErrProviderUnavailable), false},
		{"dead joined request failed", errors.New("virtual stream provider 7 request failed; no matching candidate"), false},
		{"dead joined deadline", errors.Join(context.DeadlineExceeded, errors.New("no usable stream")), false},
		{"empty listing", errors.New("no streams available from provider"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isVirtualCandidateDeadError(tc.err); got != tc.want {
				t.Fatalf("isVirtualCandidateDeadError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestCheckVersionTemporaryFailureNeverStamps is the all-path guard for the
// liveness check: every transport-temporary shape (502, 503, 500, a joined
// owner-down fallback, a timeout, and a pending release), on an
// identity-bearing pin that the provider could not list, must leave the row
// unstamped and report the durable (healthy) signal.
func TestCheckVersionTemporaryFailureNeverStamps(t *testing.T) {
	temporaryErrors := []struct {
		name string
		err  error
	}{
		{"502 provider unavailable", fmt.Errorf("%w: streaming provider returned status 502", resolver.ErrProviderUnavailable)},
		{"503 provider unavailable", fmt.Errorf("%w: streaming provider returned status 503", resolver.ErrProviderUnavailable)},
		{"500 provider unavailable", fmt.Errorf("%w: streaming provider returned status 500", resolver.ErrProviderUnavailable)},
		{"failed recently", fmt.Errorf("%w: provider listing failed recently", resolver.ErrProviderUnavailable)},
		{"joined owner-down with dead text", errors.New("resolve virtual playback: virtual stream provider 7 request failed; virtual stream provider returned no matching candidate")},
		{"joined status 503 with dead text", errors.New("streaming provider returned status 503: no matching candidate")},
		{"timeout", context.DeadlineExceeded},
		{"pending", fmt.Errorf("waiting for import: %w", virtuallibrary.ErrProviderPending)},
		{"empty listing", errors.New("no streams available from provider")},
		{"identity-bearing session-bound absent joined with outage", fmt.Errorf("%w: session-bound candidate absent: %w", resolver.ErrProviderUnavailable, virtuallibrary.ErrSessionBoundCandidateAbsent)},
	}
	for _, tc := range temporaryErrors {
		t.Run(tc.name, func(t *testing.T) {
			file := virtualVersionRow("A", "hash-temp")
			var marks int
			h := &CatalogResourceHandler{
				FileResolver: versionsCheckTestResolver{file: file},
				ItemAccess:   permissiveItemAccess{},
				VirtualResolver: VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
					return ResolvedVirtualMedia{}, tc.err
				}),
				MarkVirtualFailed: func(context.Context, int, string, *time.Time) error {
					marks++
					return nil
				},
			}

			outcome := h.checkVersion(context.Background(), file.ID)
			if outcome.stamp {
				t.Fatalf("temporary error %v marked the pin for stamping", tc.err)
			}
			if !outcome.available {
				t.Fatalf("temporary error %v reported the healthy pin unavailable", tc.err)
			}
			if marks != 0 {
				t.Fatalf("temporary error %v performed %d durable stamps, want 0", tc.err, marks)
			}
		})
	}
}

// TestCheckVersionConfirmedDeadStillStamps guards against over-suppression: a
// genuine confirmed-dead answer (no transient cause in the chain) still marks
// the identity-bearing pin for stamping, and the batch gate approves it, so the
// fix does not disable the liveness verdict it was built to record.
func TestCheckVersionConfirmedDeadStillStamps(t *testing.T) {
	file := virtualVersionRow("A", "hash-dead")
	h := &CatalogResourceHandler{
		FileResolver: versionsCheckTestResolver{file: file},
		ItemAccess:   permissiveItemAccess{},
		VirtualResolver: VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{}, errors.New("virtual stream provider returned no matching candidate")
		}),
	}

	outcome := h.checkVersion(context.Background(), file.ID)
	if !outcome.stamp || outcome.available {
		t.Fatalf("confirmed-dead pin outcome = %#v, want stamp=true available=false", outcome)
	}
	_, toStamp := gateVersionCheckStamps([]versionCheckOutcome{outcome})
	if len(toStamp) != 1 || toStamp[0] != file.ID {
		t.Fatalf("confirmed-dead pin gate = %v, want it approved for stamping", toStamp)
	}
}

// TestGateVersionCheckStampsTemporaryBelowQuorum pins the interaction the
// blocker names: a batch that mixes temporary failures with a below-quorum
// number of confirmed-dead pins must not let the temporary failures count
// toward the dead quorum, so the genuine dead verdicts still apply and the
// transient ones never do.
func TestGateVersionCheckStampsTemporaryBelowQuorum(t *testing.T) {
	outcomes := []versionCheckOutcome{
		// One confirmed dead pin.
		{fileID: 1, available: false, stamp: true, durableAlive: true, checked: true},
		// Three temporary failures: checked, no stamp, durable-alive reported.
		{fileID: 2, available: true, durableAlive: true, checked: true},
		{fileID: 3, available: true, durableAlive: true, checked: true},
		{fileID: 4, available: true, durableAlive: true, checked: true},
	}
	_, toStamp := gateVersionCheckStamps(outcomes)
	if len(toStamp) != 1 || toStamp[0] != 1 {
		t.Fatalf("stamped %v, want exactly the one real dead pin [1]", toStamp)
	}
}
