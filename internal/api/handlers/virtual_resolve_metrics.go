package handlers

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// virtualResolveListReasonTotal sizes the convertible share of cold starts:
// every resolveVirtualPlaybackSource trace.log increments it once with the
// observational list_reason and the cache-miss cause. A day of this series
// tells how many "listed" starts could have reused a fresh cache (absent vs
// expired vs forced) without scraping log lines.
var virtualResolveListReasonTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "silo_virtual_resolve_list_reason_total",
	Help: "Virtual playback resolves by listing-gate outcome (list_reason) and cache-miss cause. Observational only; labels are bounded.",
}, []string{virtualResolveListReasonLabel, virtualResolveMissCauseLabel}) //nolint:goconst // metric label names, kept inline for readability.

// virtualPrefetchOutcomeTotal makes prefetch admission/drop/completion visible
// in production, where the per-drop slog lines are Debug and never emitted at
// the default Info level. Every outcome below is a bounded label; no content
// IDs, URIs, or user identity ever enter a label.
var virtualPrefetchOutcomeTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "silo_virtual_prefetch_outcomes_total",
	Help: "Virtual listing-prefetch attempts by outcome. List-only metadata work; never counts media-byte access.",
}, []string{virtualPrefetchOutcomeLabel}) //nolint:goconst // metric label name, kept inline for readability.

// Bounded label names for the virtual resolve/prefetch series.
const (
	virtualResolveListReasonLabel = "list_reason"
	virtualResolveMissCauseLabel  = "cache_miss_cause"
	virtualPrefetchOutcomeLabel   = "outcome"
)

// unknownVirtualAttribution is the fallback label when an attribution string
// is empty. It reuses the existing unknown-outcome vocabulary rather than
// minting a new constant.
const unknownVirtualAttribution = substitutionReasonUnknownV3

// Prefetch outcome labels. Kept in one place so logs, metrics, and tests use
// the same vocabulary. All labels are bounded; shutdown and context_done
// name lifecycle outcomes, never content or identity.
const (
	virtualPrefetchAdmitted      = "admitted"
	virtualPrefetchDuplicate     = "duplicate"
	virtualPrefetchInflightFull  = "inflight_full"
	virtualPrefetchQueueFull     = "queue_full"
	virtualPrefetchGateExhausted = "gate_exhausted"
	virtualPrefetchCompleted     = "completed"
	virtualPrefetchListError     = "list_error"
	virtualPrefetchEmpty         = "empty"
	virtualPrefetchFilteredEmpty = "filtered_empty"
	virtualPrefetchFileCollapsed = "file_collapsed"
	virtualPrefetchLoadSaturated = "load_saturated"
	virtualPrefetchShutdown      = "shutdown"
	virtualPrefetchContextDone   = "context_done"
)

func observeVirtualResolveListReason(listReason, missCause string) {
	if listReason == "" {
		listReason = unknownVirtualAttribution
	}
	if missCause == "" {
		missCause = unknownVirtualAttribution
	}
	virtualResolveListReasonTotal.WithLabelValues(listReason, missCause).Inc()
}

func observeVirtualPrefetchOutcome(outcome string) {
	virtualPrefetchOutcomeTotal.WithLabelValues(outcome).Inc()
}
