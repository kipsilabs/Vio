package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// erroringPathFileResolver implements the exact-path lookup seam and always
// fails it, so tests can pin the fail-closed ownership guard.
type erroringPathFileResolver struct {
	err error
}

func (r erroringPathFileResolver) GetByID(context.Context, int) (*models.MediaFile, error) {
	return nil, r.err
}

func (r erroringPathFileResolver) GetByPath(context.Context, string) (*models.MediaFile, error) {
	return nil, r.err
}

// resetVirtualRecoveryRelists clears the process-wide recovery damper so a test
// starts from a full budget.
func resetVirtualRecoveryRelists(t *testing.T) {
	t.Helper()
	virtualRecoveryRelists.mu.Lock()
	virtualRecoveryRelists.marks = make(map[string]virtualRecoveryRelistMark)
	virtualRecoveryRelists.mu.Unlock()
}

// TestVirtualPathOwnerRowIdentityGuard pins the rotation ownership guard: a
// path match alone is not ownership. Rotation requires the row to carry this
// content and episode identity and to name the exact concrete path, not just a
// matching installation and library.
func TestVirtualPathOwnerRowIdentityGuard(t *testing.T) {
	const (
		uri      = "virtual://movie/tt-owner-guard?result=new"
		content  = "movie-owner-guard"
		folderID = 9
		ownerID  = 5
	)
	baseFile := &models.MediaFile{ID: 100, ContentID: content, FilePath: uri, MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID}
	matchingRow := func() *models.MediaFile {
		return &models.MediaFile{ID: 200, ContentID: content, FilePath: uri, MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID}
	}

	tests := []struct {
		name    string
		file    *models.MediaFile
		row     *models.MediaFile
		wantNil bool
	}{
		{name: "matching identity and path rotates", file: baseFile, row: matchingRow()},
		{name: "content mismatch is not ownership", file: baseFile, row: func() *models.MediaFile {
			row := matchingRow()
			row.ContentID = "other-content"
			return row
		}(), wantNil: true},
		{name: "episode mismatch is not ownership", file: &models.MediaFile{ID: 100, ContentID: content, EpisodeID: "ep-1", FilePath: uri, MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID}, row: matchingRow(), wantNil: true},
		{name: "nil vs non-nil episode is a mismatch", file: baseFile, row: func() *models.MediaFile {
			row := matchingRow()
			row.EpisodeID = "ep-2"
			return row
		}(), wantNil: true},
		{name: "path mismatch is not ownership", file: baseFile, row: func() *models.MediaFile {
			row := matchingRow()
			row.FilePath = "virtual://movie/tt-owner-guard?result=other"
			return row
		}(), wantNil: true},
		{name: "owner mismatch is not ownership", file: baseFile, row: func() *models.MediaFile {
			row := matchingRow()
			row.VirtualOwnerInstallationID = ownerID + 1
			return row
		}(), wantNil: true},
		{name: "library mismatch is not ownership", file: baseFile, row: func() *models.MediaFile {
			row := matchingRow()
			row.MediaFolderID = folderID + 1
			return row
		}(), wantNil: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewPlaybackHandler(playback.NewSessionManager(0, 0), byPathPlaybackFileResolver{
				testPlaybackFileResolver: testPlaybackFileResolver{file: tc.file},
				byPath:                   map[string]*models.MediaFile{uri: tc.row},
			})
			got, err := h.virtualPathOwnerRow(context.Background(), tc.file, uri, ownerID, folderID)
			if err != nil {
				t.Fatalf("virtualPathOwnerRow error: %v", err)
			}
			if tc.wantNil && got != nil {
				t.Fatalf("owner row = %#v, want nil (identity mismatch must not rotate)", got)
			}
			if !tc.wantNil && (got == nil || got.ID != tc.row.ID) {
				t.Fatalf("owner row = %#v, want the matching row %d", got, tc.row.ID)
			}
		})
	}
}

// TestVirtualPathOwnerRowFailsClosed pins that an unanswered ownership question
// is never treated as safe absence: a lookup error and an incomplete row both
// return an error, while a genuine not-found and an absent lookup capability
// report no owner with no error.
func TestVirtualPathOwnerRowFailsClosed(t *testing.T) {
	const (
		uri      = "virtual://movie/tt-owner-fail?result=new"
		content  = "movie-owner-fail"
		folderID = 9
		ownerID  = 5
	)
	file := &models.MediaFile{ID: 100, ContentID: content, FilePath: uri, MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID}

	lookupErr := errors.New("catalog unavailable")
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0), erroringPathFileResolver{err: lookupErr})
	if _, err := h.virtualPathOwnerRow(context.Background(), file, uri, ownerID, folderID); err == nil || !errors.Is(err, lookupErr) {
		t.Fatalf("lookup failure err = %v, want it to wrap the lookup error", err)
	}

	incomplete := NewPlaybackHandler(playback.NewSessionManager(0, 0), byPathPlaybackFileResolver{
		testPlaybackFileResolver: testPlaybackFileResolver{file: file},
		byPath:                   map[string]*models.MediaFile{uri: {FilePath: uri, MediaFolderID: folderID}},
	})
	if _, err := incomplete.virtualPathOwnerRow(context.Background(), file, uri, ownerID, folderID); err == nil {
		t.Fatal("an incomplete owner row was treated as safe absence")
	}

	notFound := NewPlaybackHandler(playback.NewSessionManager(0, 0), byPathPlaybackFileResolver{
		testPlaybackFileResolver: testPlaybackFileResolver{file: file},
		byPath:                   map[string]*models.MediaFile{},
	})
	if row, err := notFound.virtualPathOwnerRow(context.Background(), file, uri, ownerID, folderID); err != nil || row != nil {
		t.Fatalf("genuine not-found = (%#v, %v), want (nil, nil)", row, err)
	}

	noCapability := &PlaybackHandler{}
	if row, err := noCapability.virtualPathOwnerRow(context.Background(), file, uri, ownerID, folderID); err != nil || row != nil {
		t.Fatalf("absent lookup capability = (%#v, %v), want (nil, nil)", row, err)
	}
}

// TestFallbackRotationPreservesProbedMetadata pins the freshly probed evidence
// surviving a rotation to an existing alternate-version row: the owner row's
// catalog identity is adopted, but the verified video/audio/subtitle tracks the
// resolver just probed are retained while the source still reports
// ProbeProvenanceVerified.
func TestFallbackRotationPreservesProbedMetadata(t *testing.T) {
	const (
		neutral = "virtual://movie/tt-rotate-metadata"
		oldURI  = neutral + "?result=old"
		newURI  = neutral + "?result=new"
		content = "movie-rotate-metadata"
	)
	sessionRow := &models.MediaFile{
		ID: 100, ContentID: content, FilePath: oldURI,
		MediaFolderID: 9, VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-old",
	}
	ownerRow := &models.MediaFile{
		ID: 200, ContentID: content, FilePath: newURI,
		MediaFolderID: 9, VirtualOwnerInstallationID: 5, ProbeSource: "virtual", ProviderVideoHash: "hash-new",
	}
	streams := []VirtualPlaybackStream{{
		ID: "new", URI: newURI, Resolution: "2160p", ProviderVideoHash: "hash-new",
	}}

	var saved int
	h := &PlaybackHandler{
		PlaybackConfig: func() config.PlaybackConfig {
			return config.PlaybackConfig{MaxVirtualFailoverAttempts: 3}
		},
		fileResolver: byPathPlaybackFileResolver{
			testPlaybackFileResolver: testPlaybackFileResolver{file: sessionRow},
			byPath:                   map[string]*models.MediaFile{newURI: ownerRow},
		},
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return streams, nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{URL: "https://cdn.example/new.mp4", URI: uri, CandidateID: "new", ProviderVideoHash: "hash-new"}, nil
		}),
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, base *models.MediaFile) (*models.MediaFile, error) {
			probed := *base
			probed.VideoTracks = []models.VideoTrack{{Codec: "hevc", Width: 3840, Height: 2160, BitDepth: 10}}
			probed.AudioTracks = []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "eng"}}
			probed.SubtitleTracks = []models.SubtitleTrack{{Language: "eng", Codec: "subrip"}}
			probed.Resolution = "2160p"
			probed.CodecVideo = "hevc"
			probed.CodecAudio = "eac3"
			probed.Container = "mkv"
			return &probed, nil
		},
	}
	h.VirtualFileSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (int64, error) {
		saved++
		return 1, nil
	}

	result := h.fallbackResolveStaleVirtualSource(context.Background(), sessionRow, 1, "profile-1",
		virtualFallbackEligibility{sessionBound: true, rotationAllowed: true, releaseID: "old"})
	if result == nil {
		t.Fatal("fallback returned nil, want the rotated alternate version")
	}
	if result.Provenance != ProbeProvenanceVerified {
		t.Fatalf("provenance = %q, want %q", result.Provenance, ProbeProvenanceVerified)
	}
	if result.File == nil || result.File.ID != ownerRow.ID {
		t.Fatalf("rotated file = %#v, want the owner row id %d", result.File, ownerRow.ID)
	}
	if result.File.ContentID != content || result.File.FilePath != newURI || result.File.MediaFolderID != 9 {
		t.Fatalf("rotated identity = %#v, want the owner row's catalog identity", result.File)
	}
	if result.File.ProviderVideoHash != "hash-new" {
		t.Fatalf("provider identity = %q, want the owner row's %q", result.File.ProviderVideoHash, "hash-new")
	}
	if result.File.CodecVideo != "hevc" || result.File.CodecAudio != "eac3" || result.File.Resolution != "2160p" || result.File.Container != "mkv" {
		t.Fatalf("stream facts = %#v, want the freshly probed codecs/resolution/container", result.File)
	}
	if len(result.File.VideoTracks) != 1 || result.File.VideoTracks[0].Codec != "hevc" {
		t.Fatalf("video tracks = %#v, want the freshly probed hevc track retained", result.File.VideoTracks)
	}
	if len(result.File.AudioTracks) != 1 || result.File.AudioTracks[0].Codec != "eac3" {
		t.Fatalf("audio tracks = %#v, want the freshly probed eac3 track retained", result.File.AudioTracks)
	}
	if len(result.File.SubtitleTracks) != 1 || result.File.SubtitleTracks[0].Language != "eng" {
		t.Fatalf("subtitle tracks = %#v, want the freshly probed subtitle retained", result.File.SubtitleTracks)
	}
	if saved != 0 {
		t.Fatalf("persist calls = %d, want 0 (an existing row needs no adoption)", saved)
	}
}

// TestFallbackRotationRefusedOnIdentityMismatch proves the ownership guard has
// teeth in the fallback: a row that owns the path but belongs to another content
// is not rotated to, so the session is never bound to an unrelated title.
func TestFallbackRotationRefusedOnIdentityMismatch(t *testing.T) {
	const (
		neutral = "virtual://movie/tt-rotation-mismatch"
		oldURI  = neutral + "?result=old"
		newURI  = neutral + "?result=new"
	)
	sessionRow := &models.MediaFile{
		ID: 100, ContentID: "movie-rotation-mismatch", FilePath: oldURI,
		MediaFolderID: 9, VirtualOwnerInstallationID: 5,
	}
	ownerRow := &models.MediaFile{
		ID: 200, ContentID: "different-content", FilePath: newURI,
		MediaFolderID: 9, VirtualOwnerInstallationID: 5,
	}
	h := &PlaybackHandler{
		fileResolver: byPathPlaybackFileResolver{
			testPlaybackFileResolver: testPlaybackFileResolver{file: sessionRow},
			byPath:                   map[string]*models.MediaFile{newURI: ownerRow},
		},
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{ID: "new", URI: newURI, Resolution: "1080p"}}, nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{URL: "https://cdn.example/new.mp4", URI: uri, CandidateID: "new"}, nil
		}),
	}

	result := h.fallbackResolveStaleVirtualSource(context.Background(), sessionRow, 1, "profile-1",
		virtualFallbackEligibility{sessionBound: true, rotationAllowed: true, releaseID: "old"})
	if result == nil {
		t.Fatal("fallback returned nil, want the non-rotating resolution")
	}
	if result.File == nil || result.File.ID == ownerRow.ID {
		t.Fatalf("rotated to an unrelated content's row: %#v", result.File)
	}
	if result.File.ID != sessionRow.ID {
		t.Fatalf("effective row = %d, want the original session row %d", result.File.ID, sessionRow.ID)
	}
}

// TestFallbackRefusesSubstituteOnOwnerLookupError pins the fail-closed guard:
// when the ownership lookup errors, the fallback must refuse the substitute
// rather than adopt a path whose owner is unknown.
func TestFallbackRefusesSubstituteOnOwnerLookupError(t *testing.T) {
	const (
		neutral = "virtual://movie/tt-owner-lookup-error"
		oldURI  = neutral + "?result=old"
		newURI  = neutral + "?result=new"
	)
	sessionRow := &models.MediaFile{ID: 100, ContentID: "movie-owner-lookup-error", FilePath: oldURI, MediaFolderID: 9, VirtualOwnerInstallationID: 5}
	h := &PlaybackHandler{
		fileResolver: erroringPathFileResolver{err: errors.New("ownership lookup unavailable")},
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{ID: "new", URI: newURI, Resolution: "1080p"}}, nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{URL: "https://cdn.example/new.mp4", URI: uri, CandidateID: "new"}, nil
		}),
	}
	h.VirtualFileSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (int64, error) {
		t.Fatal("adoption attempted while the path owner was unknown")
		return 0, nil
	}

	if result := h.fallbackResolveStaleVirtualSource(context.Background(), sessionRow, 1, "profile-1",
		virtualFallbackEligibility{sessionBound: true, rotationAllowed: true, releaseID: "old"}); result != nil {
		t.Fatalf("fallback = %#v, want nil (fail closed on an unknown owner)", result)
	}
}

// TestFallbackThreadsExclusionsAndRefusesExcludedSubstitute pins item 5's two
// halves together: the durable exclusion chain reaches the detailed resolver,
// and a substitute the resolver returns under an excluded id is refused even
// though the caller's listed-stream filter never saw it.
func TestFallbackThreadsExclusionsAndRefusesExcludedSubstitute(t *testing.T) {
	const (
		neutral = "virtual://movie/tt-exclusion-thread"
		oldURI  = neutral + "?result=old"
		goodURI = neutral + "?result=good"
		deadURI = neutral + "?result=dead"
	)
	sessionRow := &models.MediaFile{ID: 100, ContentID: "movie-exclusion-thread", FilePath: oldURI, MediaFolderID: 9, VirtualOwnerInstallationID: 5}

	var gotExclusions []string
	// A misbehaving resolver substitutes the excluded release rather than the
	// listed one: the post-resolve validation must catch it.
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{ID: "good", URI: goodURI, Resolution: "1080p"}}, nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, excludedCandidateIDs []string, _ string) (ResolvedVirtualMedia, error) {
			gotExclusions = append([]string(nil), excludedCandidateIDs...)
			return ResolvedVirtualMedia{URL: "https://cdn.example/dead.mp4", URI: deadURI, CandidateID: "dead"}, nil
		}),
	}
	h.VirtualFileSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (int64, error) {
		t.Fatal("adoption attempted for an excluded substitute")
		return 0, nil
	}

	result := h.fallbackResolveStaleVirtualSource(context.Background(), sessionRow, 1, "profile-1",
		virtualFallbackEligibility{rotationAllowed: true, excludedCandidateIDs: []string{"dead"}})
	if result != nil {
		t.Fatalf("fallback = %#v, want nil (an excluded substitute must be refused)", result)
	}
	if !containsStringExactV3(gotExclusions, "dead") {
		t.Fatalf("resolver exclusions = %#v, want the recovery chain passed through", gotExclusions)
	}
}

// TestFallbackRecoveryRelistBudgetIsBounded proves a provider whose listing
// keeps failing cannot have recovery re-list it without bound: after
// virtualRecoveryRelistMax attempts in the window the fallback stops listing
// and preserves the provider error, while a different provider listing keeps
// its own budget.
func TestFallbackRecoveryRelistBudgetIsBounded(t *testing.T) {
	resetVirtualRecoveryRelists(t)
	const neutral = "virtual://movie/tt-relist-budget"
	file := &models.MediaFile{ID: 100, ContentID: "movie-relist-budget", FilePath: neutral + "?result=old", VirtualOwnerInstallationID: 5}
	listerErr := errors.New("provider listing failed")
	var calls int
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			calls++
			return nil, listerErr
		}),
	}
	for i := 0; i < virtualRecoveryRelistMax+1; i++ {
		if got := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1", virtualFallbackEligibility{}); got != nil {
			t.Fatalf("attempt %d returned %#v, want nil", i, got)
		}
	}
	if calls != virtualRecoveryRelistMax {
		t.Fatalf("provider listings = %d, want exactly %d inside the window", calls, virtualRecoveryRelistMax)
	}

	// A different provider listing has its own budget.
	other := &models.MediaFile{ID: 101, ContentID: "movie-relist-budget-2", FilePath: "virtual://movie/tt-relist-budget-2?result=old", VirtualOwnerInstallationID: 6}
	if got := h.fallbackResolveStaleVirtualSource(context.Background(), other, 1, "profile-1", virtualFallbackEligibility{}); got != nil {
		t.Fatalf("other provider returned %#v, want nil", got)
	}
	if calls != virtualRecoveryRelistMax+1 {
		t.Fatalf("provider listings = %d, want the other provider to get its own re-list", calls)
	}
}

// TestFallbackRecoveryRelistBudgetClearsOnProviderAnswer proves the budget only
// accumulates on listing failures: a listing that answers clears it, so a
// provider that flaps and recovers is not locked out by earlier failures.
func TestFallbackRecoveryRelistBudgetClearsOnProviderAnswer(t *testing.T) {
	resetVirtualRecoveryRelists(t)
	const (
		neutral = "virtual://movie/tt-relist-clear"
		oldURI  = neutral + "?result=old"
		goodURI = neutral + "?result=new"
	)
	file := &models.MediaFile{ID: 100, ContentID: "movie-relist-clear", FilePath: oldURI, VirtualOwnerInstallationID: 5}
	listerErr := errors.New("provider listing failed")
	var calls int
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			calls++
			// The third listing answers, then the provider fails again.
			if calls == 3 {
				return []VirtualPlaybackStream{{ID: "new", URI: goodURI, Resolution: "1080p"}}, nil
			}
			return nil, listerErr
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{URL: "https://cdn.example/new.mp4", URI: uri, CandidateID: "new"}, nil
		}),
	}
	elig := virtualFallbackEligibility{rotationAllowed: true}

	// Two failures accumulate, the third answers and clears the budget.
	for i := 0; i < 2; i++ {
		if got := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1", elig); got != nil {
			t.Fatalf("attempt %d returned %#v, want nil", i, got)
		}
	}
	if got := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1", elig); got == nil {
		t.Fatal("answering listing did not resolve")
	}
	// The cleared budget allows a fresh window of failures.
	for i := 0; i < virtualRecoveryRelistMax; i++ {
		if got := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1", elig); got != nil {
			t.Fatalf("post-clear attempt %d returned %#v, want nil", i, got)
		}
	}
	if calls != 3+virtualRecoveryRelistMax {
		t.Fatalf("provider listings = %d, want the cleared budget to grant %d more", calls, virtualRecoveryRelistMax)
	}
}

// TestFallbackEmptyListingAccumulatesTowardBound pins the empty-answer fix: a
// provider that keeps answering [] is a hiccup, not a recovery, so it must not
// clear the budget. Each empty answer is admitted but the budget still
// accumulates, so the (max+1)-th press stops listing instead of granting an
// unbounded re-list per press.
func TestFallbackEmptyListingAccumulatesTowardBound(t *testing.T) {
	resetVirtualRecoveryRelists(t)
	const neutral = "virtual://movie/tt-empty-answer"
	file := &models.MediaFile{ID: 100, ContentID: "movie-empty-answer", FilePath: neutral + "?result=old", VirtualOwnerInstallationID: 5}
	var calls int
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			calls++
			return nil, nil
		}),
	}
	for i := 0; i < virtualRecoveryRelistMax+1; i++ {
		if got := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1", virtualFallbackEligibility{}); got != nil {
			t.Fatalf("attempt %d returned %#v, want nil", i, got)
		}
	}
	if calls != virtualRecoveryRelistMax {
		t.Fatalf("provider listings = %d, want exactly %d: empty answers must accumulate toward the bound", calls, virtualRecoveryRelistMax)
	}
}

// TestFallbackBudgetExhaustionSurfacesTransientCause pins finding 3: when the
// re-list budget is spent, the fallback must not silently abandon a session
// that has no anchor callback (a serve-layer re-resolve, where anchorErr is
// nil). It records the transient provider cause on the eligibility so the
// caller can preserve the retryable outcome instead of a misleading generic
// failure.
func TestFallbackBudgetExhaustionSurfacesTransientCause(t *testing.T) {
	resetVirtualRecoveryRelists(t)
	const (
		neutral = "virtual://movie/tt-budget-cause"
		oldURI  = neutral + "?result=old"
	)
	file := &models.MediaFile{ID: 100, ContentID: "movie-budget-cause", FilePath: oldURI, VirtualOwnerInstallationID: 5}
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return nil, errors.New("provider listing failed")
		}),
	}
	// Spend the budget.
	for i := 0; i < virtualRecoveryRelistMax; i++ {
		_ = h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1", virtualFallbackEligibility{})
	}
	var anchorErr error
	elig := virtualFallbackEligibility{anchorErr: &anchorErr}
	if got := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1", elig); got != nil {
		t.Fatalf("exhausted fallback = %#v, want nil", got)
	}
	if !errors.Is(anchorErr, resolver.ErrProviderUnavailable) {
		t.Fatalf("anchor cause = %v, want the transient provider cause so the caller can still walk alternates", anchorErr)
	}
}
