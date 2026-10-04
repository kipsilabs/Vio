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
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// TestResolveVirtualPlaybackSourceUserRelinkBypassesFloor pins behavior 1: a
// deliberate user relink (force_relink) marks the resolve as an outage re-list
// so the provider re-lists past the fresh-serve floor and the
// provider-failure fail-fast. An automatic resolve and a session-bound rotation
// are server-initiated and stay on the floor.
func TestResolveVirtualPlaybackSourceUserRelinkBypassesFloor(t *testing.T) {
	newHandler := func(seen *bool) *PlaybackHandler {
		return &PlaybackHandler{
			VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
				return "", errors.New("simple resolver must not be used when the detailed resolver is set")
			}),
			VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(ctx context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
				*seen = virtuallibrary.ProviderOutageRelistFromContext(ctx)
				return ResolvedVirtualMedia{URL: "https://cdn.example/x.mkv", URI: uri, CandidateID: "cand-x", OwnerID: 5}, nil
			}),
		}
	}
	file := &models.MediaFile{ID: 1, ContentID: "movie-relink", FilePath: "virtual://movie/movie-relink?result=A", VirtualOwnerInstallationID: 5}
	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)

	userSeen := false
	if _, err := newHandler(&userSeen).resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "auto", 0, true, virtualResolveOptionsV3{sessionBound: false}); err != nil {
		t.Fatalf("user relink resolve: %v", err)
	}
	if !userSeen {
		t.Fatal("a user force_relink must mark the resolve as an outage re-list so it bypasses the floor and provider-failure fail-fast")
	}

	autoSeen := false
	if _, err := newHandler(&autoSeen).resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "auto", 0, false, virtualResolveOptionsV3{sessionBound: false}); err != nil {
		t.Fatalf("automatic resolve: %v", err)
	}
	if autoSeen {
		t.Fatal("an automatic resolve must stay on the floor")
	}

	rotationSeen := false
	if _, err := newHandler(&rotationSeen).resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "auto", 0, true, virtualResolveOptionsV3{sessionBound: true}); err != nil {
		t.Fatalf("session-bound rotation resolve: %v", err)
	}
	if rotationSeen {
		t.Fatal("a session-bound rotation is server-initiated and must stay on the floor")
	}
}

// TestResolveVirtualPlaybackSourceRecoveryBypassesFloor pins behavior 2: a
// declared recovery — a candidate rotation or an automatic alternate-version
// fallback — re-lists past the floor and hands the resolver a forced re-list,
// while an ordinary automatic resolve stays on the floor.
func TestResolveVirtualPlaybackSourceRecoveryBypassesFloor(t *testing.T) {
	type resolveCall struct {
		relist      bool
		forceRelist bool
	}
	newHandler := func(seen *resolveCall) *PlaybackHandler {
		return &PlaybackHandler{
			VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
				return "", errors.New("simple resolver must not be used when the detailed resolver is set")
			}),
			VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(ctx context.Context, uri string, _ int, _ int, _ string, forceRefresh bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
				seen.relist = virtuallibrary.ProviderOutageRelistFromContext(ctx)
				seen.forceRelist = forceRefresh
				return ResolvedVirtualMedia{URL: "https://cdn.example/x.mkv", URI: uri, CandidateID: "cand-x", OwnerID: 5}, nil
			}),
		}
	}
	file := &models.MediaFile{ID: 1, ContentID: "movie-recovery", FilePath: "virtual://movie/movie-recovery?result=A", VirtualOwnerInstallationID: 5}
	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)

	rotation := resolveCall{}
	if _, err := newHandler(&rotation).resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "auto", 0, false, virtualResolveOptionsV3{rotateCandidates: true, sessionBound: false}); err != nil {
		t.Fatalf("candidate rotation resolve: %v", err)
	}
	if !rotation.relist || !rotation.forceRelist {
		t.Fatalf("candidate rotation = %+v, want an outage re-list with a forced provider re-list", rotation)
	}

	fallback := resolveCall{}
	if _, err := newHandler(&fallback).resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "auto", 0, false, virtualResolveOptionsV3{sessionBound: false, bypassProviderFloor: true}); err != nil {
		t.Fatalf("alternate-version fallback resolve: %v", err)
	}
	if !fallback.relist || !fallback.forceRelist {
		t.Fatalf("alternate-version fallback = %+v, want an outage re-list with a forced provider re-list", fallback)
	}

	auto := resolveCall{}
	if _, err := newHandler(&auto).resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "auto", 0, false, virtualResolveOptionsV3{sessionBound: false}); err != nil {
		t.Fatalf("automatic resolve: %v", err)
	}
	if auto.relist || auto.forceRelist {
		t.Fatalf("ordinary automatic resolve = %+v, want it to stay on the floor", auto)
	}
}

// TestResolveVirtualStartWithVersionFallbackSkipsAltMountFailedVersion pins
// behavior 2: when the pinned release's listing fails on a fresh start, the
// walk tries alternate versions and returns the first that resolves. A version
// carrying an active AltMount SourceFailed verdict is skipped without a
// listing attempt, and the primary empty-listing failure is the honest cause
// returned only when no other version resolves.
func TestResolveVirtualStartWithVersionFallbackSkipsAltMountFailedVersion(t *testing.T) {
	const (
		primaryURI = "virtual://movie/movie-versions?result=A"
		failedURI  = "virtual://movie/movie-versions?result=B"
		workingURI = "virtual://movie/movie-versions?result=C"
	)
	primary := &models.MediaFile{ID: 1, ContentID: "movie-versions", FilePath: primaryURI, VirtualOwnerInstallationID: 5}
	failedAt := time.Now()
	failedAlt := &models.MediaFile{ID: 2, ContentID: "movie-versions", FilePath: failedURI, VirtualOwnerInstallationID: 5, FailedAt: &failedAt, ProviderVideoHash: "hash-b"}
	workingAlt := &models.MediaFile{ID: 3, ContentID: "movie-versions", FilePath: workingURI, VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-c"}

	var callsA, callsB, callsC int
	h := &PlaybackHandler{
		FileVersionFetcher: testPlaybackFileVersionFetcher{byContent: map[string][]*models.MediaFile{
			"movie-versions": {primary, failedAlt, workingAlt},
		}},
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
			return "", errors.New("simple resolver must not be used when the detailed resolver is set")
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			switch {
			case strings.Contains(uri, "result=A"):
				callsA++
				return ResolvedVirtualMedia{}, errors.New("resolve virtual playback: no streams available from provider")
			case strings.Contains(uri, "result=B"):
				callsB++
				return ResolvedVirtualMedia{}, errors.New("resolve virtual playback: no streams available from provider")
			case strings.Contains(uri, "result=C"):
				callsC++
				return ResolvedVirtualMedia{URL: "https://cdn.example/c.mkv", URI: uri, CandidateID: "cand-c", OwnerID: 5}, nil
			default:
				return ResolvedVirtualMedia{}, fmt.Errorf("unexpected URI %q", uri)
			}
		}),
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)
	start := playback.StartRequestV3{QualityPreference: "auto"}

	resolved, err := h.resolveVirtualStartWithVersionFallback(req, primary, "profile-1", start, false, 0)
	if err != nil {
		t.Fatalf("cross-version fallback resolve: %v", err)
	}
	if resolved.File == nil || !strings.Contains(resolved.URI, "result=C") {
		t.Fatalf("resolved = %#v, want the working alternate version C", resolved)
	}
	if callsA == 0 {
		t.Fatal("the primary pinned release was never attempted")
	}
	if callsC == 0 {
		t.Fatal("the working alternate version was never attempted")
	}
	if callsB != 0 {
		t.Fatalf("the AltMount-failed version was attempted %d times; it must be skipped", callsB)
	}
}

// TestResolveVirtualStartWithVersionFallbackReturnsHonestEmptyListing pins that
// when every alternate's listing is empty, the original empty-listing cause is
// returned (not a fabricated all-versions verdict), so the caller can report
// the honest transient condition.
func TestResolveVirtualStartWithVersionFallbackReturnsHonestEmptyListing(t *testing.T) {
	primary := &models.MediaFile{ID: 1, ContentID: "movie-empty", FilePath: "virtual://movie/movie-empty?result=A", VirtualOwnerInstallationID: 5}
	alt := &models.MediaFile{ID: 2, ContentID: "movie-empty", FilePath: "virtual://movie/movie-empty?result=B", VirtualOwnerInstallationID: 5}
	h := &PlaybackHandler{
		FileVersionFetcher: testPlaybackFileVersionFetcher{byContent: map[string][]*models.MediaFile{
			"movie-empty": {primary, alt},
		}},
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
			return "", errors.New("simple resolver must not be used when the detailed resolver is set")
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{}, errors.New("resolve virtual playback: no streams available from provider")
		}),
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)

	_, err := h.resolveVirtualStartWithVersionFallback(req, primary, "profile-1", playback.StartRequestV3{QualityPreference: "auto"}, false, 0)
	if err == nil {
		t.Fatal("expected the empty-listing resolve to fail")
	}
	terminal := virtualStartUnresolvedTerminalV3(err).Terminal
	if terminal == nil || terminal.Reason != "virtual_source_unavailable" || !strings.Contains(terminal.Message, "no streams") {
		t.Fatalf("terminal = %#v, want the honest empty-listing cause", terminal)
	}
	if !terminal.Retryable {
		t.Fatal("an empty provider listing is transient and must stay retryable")
	}
}

// TestVirtualStartUnresolvedTerminalHonestCause pins the exhaustion
// classification: an edge provider failure is the retryable
// provider_unavailable dependency condition, an empty listing names the empty
// answer, and every other cause keeps the generic resolve message instead of a
// fabricated verdict.
func TestVirtualStartUnresolvedTerminalHonestCause(t *testing.T) {
	edge := virtualStartUnresolvedTerminalV3(fmt.Errorf("resolve virtual input: %w: provider listing failed recently", resolver.ErrProviderUnavailable))
	if edge.Terminal == nil || edge.Terminal.Reason != providerUnavailableReasonV3 || !edge.Terminal.Retryable {
		t.Fatalf("edge failure terminal = %#v, want retryable %s", edge.Terminal, providerUnavailableReasonV3)
	}
	empty := virtualStartUnresolvedTerminalV3(errors.New("no streams available from provider"))
	if empty.Terminal == nil || empty.Terminal.Reason != "virtual_source_unavailable" || !strings.Contains(empty.Terminal.Message, "no streams") {
		t.Fatalf("empty listing terminal = %#v, want the empty-listing message", empty.Terminal)
	}
	dead := virtualStartUnresolvedTerminalV3(errors.New("virtual stream provider returned no matching candidate"))
	if dead.Terminal == nil || dead.Terminal.Reason != "virtual_source_unavailable" || strings.Contains(dead.Terminal.Message, "no streams") {
		t.Fatalf("other terminal = %#v, want the generic resolve message", dead.Terminal)
	}
}

// TestResolveVirtualPlaybackSourceBypassDamperIsBounded pins finding 1: the
// declared recovery bypass (the alternate-version walk's per-version resolve)
// draws on the same per-provider recovery budget as the stale fallback, so a
// provider that keeps failing its listing cannot have the floor bypassed on
// every version of every press. Once the budget is spent, a bypass resolve
// honors the floor instead of forcing another re-list.
func TestResolveVirtualPlaybackSourceBypassDamperIsBounded(t *testing.T) {
	resetVirtualRecoveryRelists(t)
	file := &models.MediaFile{ID: 1, ContentID: "movie-bypass-damper", FilePath: "virtual://movie/movie-bypass-damper?result=A", VirtualOwnerInstallationID: 5}
	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)

	seen := make([]bool, 0, virtualRecoveryRelistMax+1)
	h := &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
			return "", errors.New("simple resolver must not be used when the detailed resolver is set")
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(ctx context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			seen = append(seen, virtuallibrary.ProviderOutageRelistFromContext(ctx))
			return ResolvedVirtualMedia{}, errors.New("resolve virtual playback: no streams available from provider")
		}),
	}
	for i := 0; i < virtualRecoveryRelistMax; i++ {
		_, _ = h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "auto", 0, false, virtualResolveOptionsV3{sessionBound: false, bypassProviderFloor: true})
	}
	if len(seen) != virtualRecoveryRelistMax {
		t.Fatalf("resolver calls = %d, want %d admitted bypasses", len(seen), virtualRecoveryRelistMax)
	}
	for i, relist := range seen {
		if !relist {
			t.Fatalf("admitted bypass %d did not re-list past the floor", i)
		}
	}

	// The budget is now spent: the next bypass must honor the floor.
	_, _ = h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "auto", 0, false, virtualResolveOptionsV3{sessionBound: false, bypassProviderFloor: true})
	if len(seen) != virtualRecoveryRelistMax+1 {
		t.Fatalf("resolver calls = %d, want the exhausted bypass to still resolve on the floor", len(seen))
	}
	if seen[len(seen)-1] {
		t.Fatal("the exhausted bypass still forced a re-list past the floor")
	}

	// A non-bypass resolve shares the same budget key and stays on the floor.
	_, _ = h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "auto", 0, false, virtualResolveOptionsV3{sessionBound: false})
	if seen[len(seen)-1] {
		t.Fatal("an ordinary resolve must stay on the floor")
	}
}

// TestResolveVirtualPlaybackSourceBypassBudgetClearsOnAnswer pins the
// answer-clears-budget half of findings 1 and 2 on the bypass path: a declared
// recovery whose provider listing answers with candidates clears its budget, so
// a later failure starts from a full window instead of inheriting the earlier
// exhaustion.
func TestResolveVirtualPlaybackSourceBypassBudgetClearsOnAnswer(t *testing.T) {
	resetVirtualRecoveryRelists(t)
	const (
		neutral = "virtual://movie/movie-bypass-clear"
		goodURI = neutral + "?result=good"
	)
	file := &models.MediaFile{ID: 1, ContentID: "movie-bypass-clear", FilePath: neutral + "?result=A", VirtualOwnerInstallationID: 5}
	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)

	// Spend two of the three slots first, so only a real clear admits the full
	// fresh window asserted below.
	failing := errors.New("resolve virtual playback: no streams available from provider")
	for i := 0; i < virtualRecoveryRelistMax-1; i++ {
		h := &PlaybackHandler{
			VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
				return "", errors.New("simple resolver must not be used when the detailed resolver is set")
			}),
			VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
				return ResolvedVirtualMedia{}, failing
			}),
		}
		_, _ = h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "auto", 0, false, virtualResolveOptionsV3{sessionBound: false, bypassProviderFloor: true})
	}
	// The provider answers with a healthy candidate. The candidate listing
	// clears the budget and the healthy stream resolves, so the resolve returns
	// nil.
	answering := &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
			return "", errors.New("simple resolver must not be used when the detailed resolver is set")
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{ID: "good", URI: goodURI, Resolution: "1080p"}}, nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{URL: "https://cdn.example/good.mp4", URI: uri, CandidateID: "good", OwnerID: 5}, nil
		}),
	}
	if _, err := answering.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "auto", 0, false, virtualResolveOptionsV3{sessionBound: false, bypassProviderFloor: true}); err != nil {
		t.Fatalf("answering bypass resolve: %v", err)
	}

	// A fresh window of bypasses is now admitted. The loop's first
	// virtualRecoveryRelistMax attempts each force a re-list past the floor;
	// the overflow attempt must not. An inherited exhaustion would stop the
	// first attempt; a budget that only ever decremented would keep re-listing
	// the overflow.
	relisted := make([]bool, 0, virtualRecoveryRelistMax+1)
	for i := 0; i < virtualRecoveryRelistMax+1; i++ {
		h := &PlaybackHandler{
			VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
				return "", errors.New("simple resolver must not be used when the detailed resolver is set")
			}),
		}
		probeSeen := false
		h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			probeSeen = virtuallibrary.ProviderOutageRelistFromContext(ctx)
			return ResolvedVirtualMedia{}, failing
		})
		_, _ = h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "auto", 0, false, virtualResolveOptionsV3{sessionBound: false, bypassProviderFloor: true})
		relisted = append(relisted, probeSeen)
	}
	for i := 0; i < virtualRecoveryRelistMax; i++ {
		if !relisted[i] {
			t.Fatalf("post-clear bypass %d honored the floor; the answering resolve did not clear the budget", i)
		}
	}
	if relisted[virtualRecoveryRelistMax] {
		t.Fatal("the overflow bypass still re-listed; the fresh window was not bounded")
	}
}
