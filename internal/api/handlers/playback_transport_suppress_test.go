package handlers

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/nodepool"
	"github.com/Silo-Server/silo-server/internal/transcodenode"
)

// nodePoolPlanner resolves the concrete pool behind the SessionPlanner seam
// in tests the same way production does.
func nodePoolForSuppression(t *testing.T, pool *nodepool.TranscodePool) *nodepool.Planner {
	t.Helper()
	return nodepool.NewPlanner(nodepool.NewProxyPool(), pool)
}

func TestSuppress5xxSkipsNodeOnNextStart(t *testing.T) {
	var hits int32
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer node.Close()

	sibling := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer sibling.Close()

	pool := nodepool.NewTranscodePool()
	pool.SetNodes([]*nodepool.Node{
		{ID: 1, URL: node.URL, Enabled: true, Healthy: true},
		{ID: 2, URL: sibling.URL, Enabled: true, Healthy: true},
	})
	h := &PlaybackHandler{NodePlanner: nodePoolForSuppression(t, pool)}

	req := transcodenode.TranscodeStartRequest{InputPath: "/media/a.mkv"}
	_, status, _ := h.startRemotePlaybackTransport(context.Background(), node.URL, req)
	if status != http.StatusInternalServerError {
		t.Fatalf("first start status = %d, want 500", status)
	}
	if !pool.SuppressedForTest(node.URL) {
		t.Fatal("500 must suppress the node")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("node hits = %d, want exactly 1", got)
	}
	// The planner must now route around the suppressed node.
	plan := h.NodePlanner.(*nodepool.Planner).PlanSession("s1", "", true, 0)
	if plan.TranscodeNode == nil || plan.TranscodeNode.URL != sibling.URL {
		t.Fatalf("plan node = %+v, want healthy sibling %s", plan.TranscodeNode, sibling.URL)
	}
}

func TestSuppressIgnores4xx(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer node.Close()

	pool := nodepool.NewTranscodePool()
	pool.SetNodes([]*nodepool.Node{{ID: 1, URL: node.URL, Enabled: true, Healthy: true}})
	h := &PlaybackHandler{NodePlanner: nodePoolForSuppression(t, pool)}

	_, status, _ := h.startRemotePlaybackTransport(context.Background(), node.URL, transcodenode.TranscodeStartRequest{InputPath: "/media/a.mkv"})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if pool.SuppressedForTest(node.URL) {
		t.Fatal("4xx is configuration, not node sickness: must not suppress")
	}
}

func TestSuppressIgnoresParentCancellation(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer node.Close()

	pool := nodepool.NewTranscodePool()
	pool.SetNodes([]*nodepool.Node{{ID: 1, URL: node.URL, Enabled: true, Healthy: true}})
	h := &PlaybackHandler{NodePlanner: nodePoolForSuppression(t, pool)}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := h.startRemotePlaybackTransport(ctx, node.URL, transcodenode.TranscodeStartRequest{InputPath: "/media/a.mkv"})
	if err == nil {
		t.Fatal("canceled parent must fail the start")
	}
	if pool.SuppressedForTest(node.URL) {
		t.Fatal("parent cancellation is not node evidence: must not suppress")
	}
}

func TestSuppressConnectionRefused(t *testing.T) {
	// Nothing listens on this port: dial refused, a genuine transport failure.
	pool := nodepool.NewTranscodePool()
	pool.SetNodes([]*nodepool.Node{{ID: 1, URL: "http://127.0.0.1:1", Enabled: true, Healthy: true}})
	h := &PlaybackHandler{NodePlanner: nodePoolForSuppression(t, pool)}

	_, _, err := h.startRemotePlaybackTransport(context.Background(), "http://127.0.0.1:1", transcodenode.TranscodeStartRequest{InputPath: "/media/a.mkv"})
	if err == nil {
		t.Fatal("connection refused must fail")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) && !isTimeoutLike(err) {
		t.Logf("non-OpError transport failure (still must suppress): %v", err)
	}
	if !pool.SuppressedForTest("http://127.0.0.1:1") {
		t.Fatal("connection refused must suppress the node")
	}
}

func isTimeoutLike(err error) bool {
	type timeout interface{ Timeout() bool }
	var te timeout
	return errors.As(err, &te) && te.Timeout()
}

func TestSuppressBodyReadTimeoutSuppresses(t *testing.T) {
	// A 202 whose body stalls until the request deadline is a transport
	// failure (the node never delivered a usable answer): it must suppress.
	// The handler's real readiness budget (35s) cannot be shrunk
	// per-request, so this test drives the classifier directly with a bare
	// context.DeadlineExceeded — the exact shape a stalled body read
	// surfaces (no *url.Error wrapper) — with a live parent.
	pool := nodepool.NewTranscodePool()
	pool.SetNodes([]*nodepool.Node{{ID: 1, URL: "http://tc-stalled", Enabled: true, Healthy: true}})
	h := &PlaybackHandler{NodePlanner: nodePoolForSuppression(t, pool)}

	h.suppressUnhealthyTranscodeNode("http://tc-stalled", 0, context.Background(), context.DeadlineExceeded)
	if !pool.SuppressedForTest("http://tc-stalled") {
		t.Fatal("bare DeadlineExceeded with live parent must suppress the node")
	}
}

func TestSuppressBodyTruncatedNoSuppress(t *testing.T) {
	// A 202 whose body the node delivered in full but unparseable (truncated
	// JSON, UnexpectedEOF) is LOCAL evidence: the node answered, the bytes
	// do not parse. It must fail the start WITHOUT suppressing. io.ErrUnexpectedEOF
	// proves truncation rather than full delivery, but it is still a framing
	// outcome, not a timeout — and claiming it proves transport would
	// suppress on every malformed payload.
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"session":"x"`))
	}))
	defer node.Close()

	pool := nodepool.NewTranscodePool()
	pool.SetNodes([]*nodepool.Node{{ID: 1, URL: node.URL, Enabled: true, Healthy: true}})
	h := &PlaybackHandler{NodePlanner: nodePoolForSuppression(t, pool)}

	_, _, err := h.startRemotePlaybackTransport(context.Background(), node.URL, transcodenode.TranscodeStartRequest{InputPath: "/media/a.mkv"})
	if err == nil {
		t.Fatal("truncated body must fail the start")
	}
	if pool.SuppressedForTest(node.URL) {
		t.Fatal("delivered-but-unparseable body is local, must not suppress")
	}
}

func TestSuppressRecoveryThenStaleFailure(t *testing.T) {
	// Handler-level epoch wiring: a start admitted at epoch 0 fails and
	// suppresses; the sweep recovers; a stale failure carrying the old epoch
	// is refused; a fresh failure carrying the new epoch suppresses again.
	pool := nodepool.NewTranscodePool()
	pool.SetNodes([]*nodepool.Node{{ID: 1, URL: "http://tc-epoch", Enabled: true, Healthy: true}})
	h := &PlaybackHandler{NodePlanner: nodePoolForSuppression(t, pool)}

	admitted := h.transcodeRecoveryEpoch("http://tc-epoch")
	h.suppressUnhealthyTranscodeNodeStatus("http://tc-epoch", admitted, http.StatusInternalServerError)
	if !pool.SuppressedForTest("http://tc-epoch") {
		t.Fatal("fresh 500 must suppress")
	}
	pool.ApplyHealth(1, "http://tc-epoch", true, 0, 0, "", nil, nil, time.Now())
	if pool.SuppressedForTest("http://tc-epoch") {
		t.Fatal("healthy sweep must clear suppression")
	}
	// Stale failure from the pre-recovery admission.
	h.suppressUnhealthyTranscodeNodeStatus("http://tc-epoch", admitted, http.StatusInternalServerError)
	if pool.SuppressedForTest("http://tc-epoch") {
		t.Fatal("stale failure must not re-suppress after recovery")
	}
	// Fresh failure admitted post-recovery.
	fresh := h.transcodeRecoveryEpoch("http://tc-epoch")
	if fresh <= admitted {
		t.Fatalf("epoch did not advance: %d -> %d", admitted, fresh)
	}
	h.suppressUnhealthyTranscodeNodeStatus("http://tc-epoch", fresh, http.StatusInternalServerError)
	if !pool.SuppressedForTest("http://tc-epoch") {
		t.Fatal("fresh failure after recovery must suppress again")
	}
}

func TestSuppressLocalExecutionUnaffected(t *testing.T) {
	// Local execution never consults the pool: suppressing every remote node
	// must not change local planning. A planner with no transcode nodes and
	// no suppression plans nothing remote; local path is separate.
	pool := nodepool.NewTranscodePool()
	pool.SetNodes([]*nodepool.Node{{ID: 1, URL: "http://tc-dead", Enabled: true, Healthy: true}})
	pool.SuppressNode("http://tc-dead", 0, time.Minute)
	planner := nodePoolForSuppression(t, pool)
	plan := planner.PlanSession("s1", "", true, 0)
	if plan.TranscodeNode != nil {
		t.Fatalf("suppressed sole node must yield no remote plan, got %+v", plan.TranscodeNode)
	}
}
