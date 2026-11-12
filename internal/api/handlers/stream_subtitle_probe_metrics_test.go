package handlers

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// gatheredCounter reads a labeled counter from the default registry, which is
// where the playback package's promauto counters are registered.
func gatheredCounter(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if labelsMatch(metric, labels) {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func labelsMatch(metric *dto.Metric, want map[string]string) bool {
	got := make(map[string]string, len(metric.GetLabel()))
	for _, pair := range metric.GetLabel() {
		got[pair.GetName()] = pair.GetValue()
	}
	for name, value := range want {
		if got[name] != value {
			return false
		}
	}
	return true
}

// TestReuseVirtualSubtitleLayoutCountsTheProbeMetric keeps the layout-probe
// reuse counter on the memo's hit path: a live verdict counts once, and an
// expired verdict is a miss that counts nothing.
func TestReuseVirtualSubtitleLayoutCountsTheProbeMetric(t *testing.T) {
	handler := &StreamHandler{}
	opts := &playback.StreamExtractOpts{
		CacheIdentity: "identity-metric",
		TrackIndex:    0,
		SourceCodec:   "subrip",
	}
	verifiedAt := time.Now()
	handler.rememberVirtualSubtitleLayout(
		virtualSubtitleLayoutKey(opts.CacheIdentity, opts.TrackIndex),
		playback.StreamExtractOpts{CacheIdentity: opts.CacheIdentity, TrackIndex: 2, SourceCodec: "ass"},
		verifiedAt)

	const metric = "vio_subtitle_layout_probe_total"
	before := gatheredCounter(t, metric, map[string]string{"result": "reused"})
	if _, ok := handler.reuseVirtualSubtitleLayout(opts, verifiedAt.Add(time.Second)); !ok {
		t.Fatal("verdict inside the TTL was not reused")
	}
	if got := gatheredCounter(t, metric, map[string]string{"result": "reused"}) - before; got != 1 {
		t.Fatalf("reuse delta = %v, want 1", got)
	}

	after := gatheredCounter(t, metric, map[string]string{"result": "reused"})
	if _, ok := handler.reuseVirtualSubtitleLayout(opts, verifiedAt.Add(virtualSubtitleLayoutCacheTTL+time.Second)); ok {
		t.Fatal("verdict past the TTL was reused")
	}
	if got := gatheredCounter(t, metric, map[string]string{"result": "reused"}) - after; got != 0 {
		t.Fatalf("expired verdict counted %v reuses, want 0", got)
	}
}
