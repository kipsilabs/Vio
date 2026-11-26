package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

const seekAnchorRetryTestProfile = "profile-seek-retry"

func shortenSeekAnchorRetryWindows(t *testing.T, first, reserve, retry time.Duration) {
	t.Helper()
	previousFirst, previousReserve, previousRetry := seekAnchorFirstOutputWindowV3, seekAnchorTransportReserveV3, seekAnchorRetryProbeWindowV3
	seekAnchorFirstOutputWindowV3, seekAnchorTransportReserveV3, seekAnchorRetryProbeWindowV3 = first, reserve, retry
	previousHint := copySeekAnchorProbeBudgetHint
	copySeekAnchorProbeBudgetHint = func(string, string, int) time.Duration { return 0 }
	t.Cleanup(func() {
		seekAnchorFirstOutputWindowV3, seekAnchorTransportReserveV3, seekAnchorRetryProbeWindowV3 = previousFirst, previousReserve, previousRetry
		copySeekAnchorProbeBudgetHint = previousHint
	})
}

func seekAnchorRetryFixture(t *testing.T, name string, target float64) (*PlaybackHandler, *playback.Session, *models.MediaFile, *playback.PlanV3) {
	t.Helper()
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	file := &models.MediaFile{ID: 71, ContentID: "movie-" + name, FilePath: "/media/" + name + ".mkv"}
	session := &playback.Session{ID: "session-" + name, UserID: 1, ProfileID: seekAnchorRetryTestProfile, RequestedMediaFileID: file.ID}
	plan := &playback.PlanV3{
		PlanID:   "plan:" + name,
		Delivery: playback.DeliveryRemuxProgressiveV3,
		Timeline: playback.TimelineV3{SourceStartSeconds: target, PlayerStartSeconds: target},
	}
	return handler, session, file, plan
}

func probeThatWaitsForBudget(calls *atomic.Int32, budgets *[]time.Duration, mu *sync.Mutex) copySeekAnchorResolver {
	return func(ctx context.Context, _ string, _ string, _ float64, _ int) (float64, int, error) {
		if calls != nil {
			calls.Add(1)
		}
		if deadline, ok := ctx.Deadline(); ok && budgets != nil {
			mu.Lock()
			*budgets = append(*budgets, time.Until(deadline))
			mu.Unlock()
		}
		<-ctx.Done()
		return 0, 0, ctx.Err()
	}
}

func TestPrepareTransportTimelineFirstSeekAnchorAttemptKeepsItsWindow(t *testing.T) {
	const target = 919.0
	firstWindow, reserve, retryWindow := 240*time.Millisecond, 40*time.Millisecond, 700*time.Millisecond
	shortenSeekAnchorRetryWindows(t, firstWindow, reserve, retryWindow)
	handler, session, file, plan := seekAnchorRetryFixture(t, "first-attempt", target)

	var calls atomic.Int32
	var mu sync.Mutex
	var budgets []time.Duration
	handler.copySeekAnchor = probeThatWaitsForBudget(&calls, &budgets, &mu)

	_, transportErr := handler.prepareTransportTimelineV3(context.Background(), session, file, playback.PlannerResultV3{Plan: plan, PlayMethod: playback.PlayRemux})
	if transportErr == nil {
		t.Fatal("first attempt = nil error, want the retryable seek refusal")
	}
	if transportErr.reason != transcodeStartFailedReasonV3 || !transportErr.retryable || !transportErr.seekFailure {
		t.Fatalf("first attempt error = %#v, want the existing retryable transcode_start_failed seek verdict", transportErr)
	}
	if transportErr.reason == seekAnchorUnavailableReasonV3 {
		t.Fatal("a first attempt must not answer the retry-only unavailable terminal")
	}
	if calls.Load() != 1 {
		t.Fatalf("first attempt probes = %d, want exactly 1", calls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(budgets) != 1 {
		t.Fatalf("first attempt budgets = %v, want exactly one", budgets)
	}
	if budgets[0] > firstWindow-reserve+75*time.Millisecond {
		t.Fatalf("first attempt probe budget = %v, want the %v first-output window", budgets[0], firstWindow-reserve)
	}
}

func TestCopySeekAnchorRefusalRetryEscalatesExactlyOnce(t *testing.T) {
	const target = 918.161
	handler, _, file, _ := seekAnchorRetryFixture(t, "once", target)
	handler.recordCopySeekAnchorRefusalV3(seekAnchorRetryTestProfile, file.ID, target, &transportErrorV3{
		reason:      transcodeStartFailedReasonV3,
		message:     remuxSeekPositionFailureMessageV3,
		retryable:   true,
		seekFailure: true,
	})

	ctx, ok := handler.copySeekAnchorRetryContextV3(context.Background(), seekAnchorRetryTestProfile, file.ID, target, true)
	if !ok {
		t.Fatal("retry of the recorded target did not get an escalation grant")
	}
	if seekAnchorRetryGrantFromContextV3(ctx) == nil {
		t.Fatal("escalation grant missing from the retry context")
	}
	if _, again := handler.copySeekAnchorRetryContextV3(context.Background(), seekAnchorRetryTestProfile, file.ID, target, true); again {
		t.Fatal("the same refusal escalated twice")
	}

	handler.recordCopySeekAnchorRefusalV3(seekAnchorRetryTestProfile, file.ID, target, &transportErrorV3{
		reason: transcodeStartFailedReasonV3, retryable: true, seekFailure: true,
	})
	if _, ok := handler.copySeekAnchorRetryContextV3(context.Background(), seekAnchorRetryTestProfile, file.ID, target, false); ok {
		t.Fatal("a start without force_relink consumed the retry grant")
	}
	if _, ok := handler.copySeekAnchorRetryContextV3(context.Background(), seekAnchorRetryTestProfile, file.ID, target+5, true); ok {
		t.Fatal("a different seek target consumed the retry grant")
	}
	if _, ok := handler.copySeekAnchorRetryContextV3(context.Background(), seekAnchorRetryTestProfile, file.ID, target+0.4, true); !ok {
		t.Fatal("a retry within the target tolerance did not escalate")
	}
}

func TestPrepareTransportTimelineSeekAnchorRetryPlays(t *testing.T) {
	const target = 918.161
	firstWindow, reserve, retryWindow := 240*time.Millisecond, 40*time.Millisecond, 700*time.Millisecond
	shortenSeekAnchorRetryWindows(t, firstWindow, reserve, retryWindow)
	handler, session, file, plan := seekAnchorRetryFixture(t, "retry-plays", target)

	var calls atomic.Int32
	var mu sync.Mutex
	var budgets []time.Duration
	handler.copySeekAnchor = func(ctx context.Context, _ string, _ string, requested float64, segmentDuration int) (float64, int, error) {
		call := calls.Add(1)
		if deadline, ok := ctx.Deadline(); ok {
			mu.Lock()
			budgets = append(budgets, time.Until(deadline))
			mu.Unlock()
		}
		if call == 1 {
			<-ctx.Done()
			return 0, 0, ctx.Err()
		}
		return requested - 1, computeStartSegment(requested-1, segmentDuration), nil
	}

	_, transportErr := handler.prepareTransportTimelineV3(context.Background(), session, file, playback.PlannerResultV3{Plan: plan, PlayMethod: playback.PlayRemux})
	if transportErr == nil || transportErr.reason != transcodeStartFailedReasonV3 || !transportErr.retryable {
		t.Fatalf("first attempt = %#v, want the retryable seek refusal", transportErr)
	}
	handler.recordCopySeekAnchorRefusalV3(seekAnchorRetryTestProfile, file.ID, target, transportErr)

	retryCtx, ok := handler.copySeekAnchorRetryContextV3(context.Background(), seekAnchorRetryTestProfile, file.ID, target, true)
	if !ok {
		t.Fatal("retry of the recorded target did not get an escalation grant")
	}
	retryPlan := &playback.PlanV3{
		PlanID:   "plan:retry-plays-retry",
		Delivery: playback.DeliveryRemuxProgressiveV3,
		Timeline: playback.TimelineV3{SourceStartSeconds: target, PlayerStartSeconds: target},
	}
	timeline, retryErr := handler.prepareTransportTimelineV3(retryCtx, session, file, playback.PlannerResultV3{Plan: retryPlan, PlayMethod: playback.PlayRemux})
	if retryErr != nil {
		t.Fatalf("retry = %#v, want a playable anchor", retryErr)
	}
	if !timeline.copySeekAnchorResolved {
		t.Fatalf("retry timeline = %#v, want a resolved copy anchor", timeline)
	}
	if calls.Load() != 2 {
		t.Fatalf("anchor probes = %d, want the first attempt plus one escalated retry", calls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(budgets) != 2 {
		t.Fatalf("probe budgets = %v, want two observations", budgets)
	}
	if budgets[1] < retryWindow-75*time.Millisecond {
		t.Fatalf("retry probe budget = %v, want about the %v retry window", budgets[1], retryWindow)
	}
}

func TestPrepareTransportTimelineSeekAnchorRetryFailureIsExplicit(t *testing.T) {
	const target = 918.161
	firstWindow, reserve, retryWindow := 240*time.Millisecond, 40*time.Millisecond, 500*time.Millisecond
	shortenSeekAnchorRetryWindows(t, firstWindow, reserve, retryWindow)
	handler, session, file, plan := seekAnchorRetryFixture(t, "retry-fails", target)

	var calls atomic.Int32
	handler.copySeekAnchor = probeThatWaitsForBudget(&calls, nil, nil)

	_, transportErr := handler.prepareTransportTimelineV3(context.Background(), session, file, playback.PlannerResultV3{Plan: plan, PlayMethod: playback.PlayRemux})
	if transportErr == nil || transportErr.reason != transcodeStartFailedReasonV3 || !transportErr.retryable {
		t.Fatalf("first attempt = %#v, want the retryable seek refusal", transportErr)
	}
	handler.recordCopySeekAnchorRefusalV3(seekAnchorRetryTestProfile, file.ID, target, transportErr)

	retryCtx, ok := handler.copySeekAnchorRetryContextV3(context.Background(), seekAnchorRetryTestProfile, file.ID, target, true)
	if !ok {
		t.Fatal("retry of the recorded target did not get an escalation grant")
	}
	retryPlan := &playback.PlanV3{
		PlanID:   "plan:retry-fails-retry",
		Delivery: playback.DeliveryRemuxProgressiveV3,
		Timeline: playback.TimelineV3{SourceStartSeconds: target, PlayerStartSeconds: target},
	}
	_, retryErr := handler.prepareTransportTimelineV3(retryCtx, session, file, playback.PlannerResultV3{Plan: retryPlan, PlayMethod: playback.PlayRemux})
	if retryErr == nil {
		t.Fatal("retry = nil error, want an explicit unavailable terminal")
	}
	if retryErr.reason != seekAnchorUnavailableReasonV3 {
		t.Fatalf("retry reason = %q, want %q", retryErr.reason, seekAnchorUnavailableReasonV3)
	}
	if retryErr.retryable {
		t.Fatal("the escalated retry answered a retryable verdict; it must terminate")
	}
	if !retryErr.seekFailure || retryErr.message != seekAnchorUnavailableMessageV3 {
		t.Fatalf("retry error = %#v, want the explicit unavailable seek verdict", retryErr)
	}
	if calls.Load() != 2 {
		t.Fatalf("anchor probes = %d, want the first attempt plus one escalated retry", calls.Load())
	}
}

// TestHandleStartPlaybackSeekAnchorRetryEscalatesOnce maps the whole retry
// path: a progressive remux start refuses the seek anchor and records the
// refusal; the viewer's Try again is a fresh start with force_relink at the
// same target. That start must get one extended probe window, not the first
// attempt's short window, and it must play when the probe then answers.
func TestHandleStartPlaybackSeekAnchorRetryEscalatesOnce(t *testing.T) {
	firstWindow, reserve, retryWindow := 300*time.Millisecond, 100*time.Millisecond, 900*time.Millisecond
	shortenSeekAnchorRetryWindows(t, firstWindow, reserve, retryWindow)

	const (
		neutral   = "virtual://movie/tt-seek-retry-escalation"
		pinnedURI = neutral + "?result=pinned"
	)
	stamp := time.Now()
	source := v3HandlerFixtureFile(t)
	source.ID = 913
	source.ContentID = "movie-seek-retry-escalation"
	source.Container = "mkv"
	source.FilePath = pinnedURI
	source.VirtualOwnerInstallationID = 5
	source.ProbeUpdatedAt = &stamp

	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), mapPlaybackFileResolver{files: map[int]*models.MediaFile{source.ID: source}})
	handler.FileVersionFetcher = testPlaybackFileVersionFetcher{byContent: map[string][]*models.MediaFile{source.ContentID: {source}}}
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.PlaybackConfig = playbackTestConfig("", "")
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	relay := remotestream.NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	handler.RemoteStreamRelay = relay
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:9/stream?path=" + path, nil
	})
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/pinned.mkv", URI: pinnedURI, CandidateID: "pinned"}, nil
	})
	handler.copySeekAnchorBackoff = func(context.Context, time.Duration) bool { return false }
	var probes atomic.Int32
	var mu sync.Mutex
	var budgets []time.Duration
	handler.copySeekAnchor = func(ctx context.Context, _ string, _ string, requested float64, segmentDuration int) (float64, int, error) {
		call := probes.Add(1)
		if deadline, ok := ctx.Deadline(); ok {
			mu.Lock()
			budgets = append(budgets, time.Until(deadline))
			mu.Unlock()
		}
		if call == 1 {
			return 0, 0, fmt.Errorf("%w: Server returned 5XX Server Error reply", playback.ErrTransientProvider)
		}
		return requested, computeStartSegment(requested, segmentDuration), nil
	}

	start := v3HandlerStartRequest()
	start.FileID = source.ID
	start.QualityPreference = "auto"
	start.ClientPlaybackContext.Deliveries = map[string]playback.DeliveryCapabilityV3{
		playback.DeliveryClassProgressiveV3: {Enabled: true, SupportedOnDevice: true},
	}
	position := 30.0
	start.StartPosition = &position

	first := httptest.NewRecorder()
	handler.HandleStartPlayback(first, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, start))).WithContext(newAuthorizedPlaybackContext()))
	if first.Code != http.StatusCreated {
		t.Fatalf("first start status = %d, body = %s", first.Code, first.Body.String())
	}
	var firstResponse playback.DecisionResponseV3
	if err := json.Unmarshal(first.Body.Bytes(), &firstResponse); err != nil {
		t.Fatalf("first start response invalid: %v", err)
	}
	if firstResponse.Terminal == nil || firstResponse.Terminal.Reason != transcodeStartFailedReasonV3 || !firstResponse.Terminal.Retryable {
		t.Fatalf("first start terminal = %#v, want the retryable seek refusal", firstResponse.Terminal)
	}

	start.PlaybackAttemptID = "attempt-handler-seek-retry-0002"
	start.ForceRelink = true
	second := httptest.NewRecorder()
	handler.HandleStartPlayback(second, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, start))).WithContext(newAuthorizedPlaybackContext()))
	if second.Code != http.StatusCreated {
		t.Fatalf("retry start status = %d, body = %s", second.Code, second.Body.String())
	}
	var secondResponse playback.DecisionResponseV3
	if err := json.Unmarshal(second.Body.Bytes(), &secondResponse); err != nil {
		t.Fatalf("retry start response invalid: %v", err)
	}
	if secondResponse.Terminal != nil || secondResponse.PlaybackPlan == nil {
		t.Fatalf("retry terminal = %#v, plan = %#v, want a played retry", secondResponse.Terminal, secondResponse.PlaybackPlan)
	}
	if probes.Load() != 2 {
		t.Fatalf("anchor probes = %d, want the first attempt plus one escalated retry", probes.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(budgets) != 2 {
		t.Fatalf("probe budgets = %v, want two observations", budgets)
	}
	if budgets[0] > firstWindow-reserve+75*time.Millisecond {
		t.Fatalf("first probe budget = %v, want the %v first-output window", budgets[0], firstWindow-reserve)
	}
	if budgets[1] < retryWindow-100*time.Millisecond {
		t.Fatalf("retry probe budget = %v, want about the %v retry window", budgets[1], retryWindow)
	}
}
