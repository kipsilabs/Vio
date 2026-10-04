package handlers

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/noderouting"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// TestStampStartVirtualCandidateFailedIndictsOnlyConfirmedDead pins the issue-3
// classification: a confirmed dead pin (no matching candidate, no usable
// stream) is stamped so a retry fails fast or rotates, while an empty provider
// listing — deliberately not a verdict about any release — is left unmarked.
func TestStampStartVirtualCandidateFailedIndictsOnlyConfirmedDead(t *testing.T) {
	row := &models.MediaFile{
		ID:                         11,
		ContentID:                  "movie-indict",
		FilePath:                   "virtual://movie/movie-indict?result=cand-a",
		ProviderVideoHash:          "hash-a",
		ProviderReleaseName:        "Movie.2024.1080p",
		VirtualOwnerInstallationID: 5,
	}
	var stamps []string
	h := &PlaybackHandler{
		VirtualCandidateFailMarker: func(_ context.Context, _ int, expectedFilePath string, _ *time.Time) error {
			stamps = append(stamps, expectedFilePath)
			return nil
		},
	}

	h.stampStartVirtualCandidateFailed(context.Background(), row, errors.New("resolve virtual playback: virtual stream provider returned no matching candidate"))
	if len(stamps) != 1 {
		t.Fatalf("confirmed-dead candidate stamps = %d, want 1", len(stamps))
	}

	h.stampStartVirtualCandidateFailed(context.Background(), row, errors.New("no streams available from provider"))
	if len(stamps) != 1 {
		t.Fatalf("empty provider listing stamped a candidate (%d stamps); it must stay unmarked", len(stamps))
	}

	// A row that already carries a verdict is never re-stamped.
	now := time.Now()
	row.FailedAt = &now
	h.stampStartVirtualCandidateFailed(context.Background(), row, errors.New("virtual stream provider returned no matching candidate"))
	if len(stamps) != 1 {
		t.Fatalf("already-failed row stamped again (%d stamps)", len(stamps))
	}
}

// TestStampStartVirtualCandidateFailedNeverStampsJoinedAbsentTemporary pins the
// PR #212 Blocker 1 fix: an identity-bearing pin whose resolve failed with a
// joined temporary cause AND the session-bound-absent sentinel must not be read
// as a confirmed verdict. Each joined shape below carries
// ErrSessionBoundCandidateAbsent — which on its own is a legitimate dead verdict
// for an identity-bearing row — alongside a transport-temporary cause
// (provider outage, deadline, pending release). Before the early temporary
// guard, the absent branch ran independently and stamped all three.
func TestStampStartVirtualCandidateFailedNeverStampsJoinedAbsentTemporary(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{
			"absent joined with provider outage",
			errors.Join(resolver.ErrProviderUnavailable, virtuallibrary.ErrSessionBoundCandidateAbsent),
		},
		{
			"absent joined with deadline",
			errors.Join(context.DeadlineExceeded, virtuallibrary.ErrSessionBoundCandidateAbsent),
		},
		{
			"absent joined with pending",
			errors.Join(virtuallibrary.ErrProviderPending, virtuallibrary.ErrSessionBoundCandidateAbsent),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := &models.MediaFile{
				ID:                         13,
				ContentID:                  "movie-joined-absent",
				FilePath:                   "virtual://movie/movie-joined-absent?result=cand-a",
				ProviderVideoHash:          "hash-a",
				ProviderReleaseName:        "Movie.2024.1080p",
				VirtualOwnerInstallationID: 5,
			}
			if _, hasIdentity := persistedVirtualIdentity(row); !hasIdentity {
				t.Fatal("precondition: row must carry durable identity to reach the absent branch")
			}
			var stamps []string
			h := &PlaybackHandler{
				VirtualCandidateFailMarker: func(_ context.Context, _ int, expectedFilePath string, _ *time.Time) error {
					stamps = append(stamps, expectedFilePath)
					return nil
				},
			}
			h.stampStartVirtualCandidateFailed(context.Background(), row, tc.err)
			if len(stamps) != 0 {
				t.Fatalf("joined temporary+absent error stamped candidate(s) %v, want none", stamps)
			}
		})
	}
}

// TestStampStartVirtualCandidateFailedBareAbsentStillStamps is the positive
// control for the fix: with no transient cause in the chain, the bare
// session-bound-absent sentinel on an identity-bearing pin is still a confirmed
// verdict and stamps. This guards against over-suppression — the early
// temporary guard must not disable the durable absent-pin indictment.
func TestStampStartVirtualCandidateFailedBareAbsentStillStamps(t *testing.T) {
	row := &models.MediaFile{
		ID:                         14,
		ContentID:                  "movie-bare-absent",
		FilePath:                   "virtual://movie/movie-bare-absent?result=cand-a",
		ProviderVideoHash:          "hash-a",
		ProviderReleaseName:        "Movie.2024.1080p",
		VirtualOwnerInstallationID: 5,
	}
	if _, hasIdentity := persistedVirtualIdentity(row); !hasIdentity {
		t.Fatal("precondition: row must carry durable identity to reach the absent branch")
	}
	if isVirtualProviderListingTemporaryError(virtuallibrary.ErrSessionBoundCandidateAbsent) {
		t.Fatalf("bare absent sentinel %v is transport-temporary; the control would be vacuous", virtuallibrary.ErrSessionBoundCandidateAbsent)
	}
	var stamps []string
	h := &PlaybackHandler{
		VirtualCandidateFailMarker: func(_ context.Context, _ int, expectedFilePath string, _ *time.Time) error {
			stamps = append(stamps, expectedFilePath)
			return nil
		},
	}
	h.stampStartVirtualCandidateFailed(context.Background(), row, fmt.Errorf("resolve virtual playback: %w", virtuallibrary.ErrSessionBoundCandidateAbsent))
	if len(stamps) != 1 {
		t.Fatalf("bare confirmed-absent pin stamped %d candidates, want 1", len(stamps))
	}
}

// TestClassifyVirtualReplanExhaustionPrefersRouteCapacity pins the issue-2 fix:
// when one candidate hit route-capacity exhaustion, that retryable verdict
// survives even though an earlier same-priority transport failure would
// otherwise win the highest-priority tie. Without the capacity scan the replan
// surfaced a masking reason and could not map to the honest 503.
func TestClassifyVirtualReplanExhaustionPrefersRouteCapacity(t *testing.T) {
	errs := []*candidateErrorV3{
		{Stage: candidateStageTransport, TransportErr: &transportErrorV3{reason: transcodeStartFailedReasonV3, retryable: true}},
		{Stage: candidateStageTransport, TransportErr: &transportErrorV3{reason: string(noderouting.OutcomeCapacityUnavailable), message: "no route", retryable: true}},
	}
	got := classifyVirtualReplanExhaustionV3(nil, errs)
	if got == nil || got.reason != string(noderouting.OutcomeCapacityUnavailable) || !got.retryable {
		t.Fatalf("classification = %#v, want retryable %s", got, noderouting.OutcomeCapacityUnavailable)
	}
}
