package remotestream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRelayConcurrentRepeated5xxRegistrationChurn is the bounded-concurrency
// evidence for Blocker 4. Several streams drive the relay at once while the
// upstream answers 5xx to every open, and each stream registers a fresh content
// handle around every request. It pins the per-request attempt budget under
// contention, asserts every registration is cleaned up (no leak), and records
// the observed latency distribution.
//
// The exercise is deliberately small and time-bounded: it must finish in a few
// seconds, so it is evidence of behavior under bounded concurrency, not a
// throughput benchmark.
func TestRelayConcurrentRepeated5xxRegistrationChurn(t *testing.T) {
	const (
		concurrentStreams = 12
		requestsPerStream = 3
		upstreamOpenDelay = 15 * time.Millisecond
		totalRequests     = concurrentStreams * requestsPerStream
		// Each upstream open contributes upstreamOpenDelay and each request
		// absorbs exactly one relay backoff before its second open. The slack
		// only covers scheduler noise under contention.
		perRequestSlack = 1500 * time.Millisecond
	)

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		select {
		case <-time.After(upstreamOpenDelay):
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	sourceURL := upstream.URL + "/media.mp4"

	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	// peakInFlight is the evidence that the streams really ran concurrently.
	var inFlight, peakInFlight atomic.Int32

	var mu sync.Mutex
	latencies := make([]time.Duration, 0, totalRequests)
	statuses := make([]int, 0, totalRequests)
	var errs []error
	recordErr := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wallStart := time.Now()
	for stream := 0; stream < concurrentStreams; stream++ {
		wg.Add(1)
		go func(stream int) {
			defer wg.Done()
			<-start

			current := inFlight.Add(1)
			for {
				peak := peakInFlight.Load()
				if current <= peak || peakInFlight.CompareAndSwap(peak, current) {
					break
				}
			}
			defer inFlight.Add(-1)

			for request := 0; request < requestsPerStream; request++ {
				relayURL, release, err := relay.RegisterInsecure(context.Background(), sourceURL)
				if err != nil {
					recordErr(fmt.Errorf("stream %d register %d: %w", stream, request, err))
					return
				}

				requestStart := time.Now()
				resp, err := http.Get(relayURL)
				elapsed := time.Since(requestStart)
				if err != nil {
					recordErr(fmt.Errorf("stream %d GET %d: %w", stream, request, err))
					release()
					return
				}
				temporary := RelayTemporaryFailure(resp)
				status := resp.StatusCode
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				release()

				mu.Lock()
				latencies = append(latencies, elapsed)
				statuses = append(statuses, status)
				mu.Unlock()

				if status != http.StatusServiceUnavailable {
					recordErr(fmt.Errorf("stream %d request %d status = %d, want 503", stream, request, status))
				}
				if !temporary {
					recordErr(fmt.Errorf("stream %d request %d missing %s on an exhausted 5xx", stream, request, relayTemporaryFailureHeader))
				}
			}
		}(stream)
	}
	close(start)
	wg.Wait()
	wall := time.Since(wallStart)

	mu.Lock()
	observedErrs := append([]error(nil), errs...)
	observedStatuses := append([]int(nil), statuses...)
	observedLatencies := append([]time.Duration(nil), latencies...)
	mu.Unlock()

	for _, err := range observedErrs {
		t.Errorf("concurrent relay exercise: %v", err)
	}
	if len(observedStatuses) != totalRequests {
		t.Fatalf("completed requests = %d, want %d", len(observedStatuses), totalRequests)
	}
	for i, status := range observedStatuses {
		if status != http.StatusServiceUnavailable {
			t.Fatalf("request %d status = %d, want 503", i, status)
		}
	}

	// Exactly one open-phase retry per request: two upstream attempts, never
	// more, no matter how many streams contend.
	if want := int32(2 * totalRequests); upstreamCalls.Load() != want {
		t.Fatalf("upstream calls = %d, want %d (2 attempts x %d requests)", upstreamCalls.Load(), want, totalRequests)
	}
	if got := relay.ActiveRegistrations(); got != 0 {
		t.Fatalf("live relay registrations after the exercise = %d, want 0 (no leak)", got)
	}
	if got := relay.readAhead.used(); got != 0 {
		t.Fatalf("read-ahead budget used after the exercise = %d, want 0 (no stranded reservation)", got)
	}
	if got := relay.readAhead.peak(); got > relay.readAhead.capacity() {
		t.Fatalf("read-ahead peak %d exceeded capacity %d", got, relay.readAhead.capacity())
	}
	if got := peakInFlight.Load(); got != concurrentStreams {
		t.Fatalf("peak concurrent streams = %d, want %d (the exercise did not run concurrently)", got, concurrentStreams)
	}

	var min, max, sum time.Duration
	for _, latency := range observedLatencies {
		if min == 0 || latency < min {
			min = latency
		}
		if latency > max {
			max = latency
		}
		sum += latency
	}
	avg := sum / time.Duration(len(observedLatencies))
	// One retry backoff plus two upstream opens is the whole per-request stack.
	if floor := relayUpstreamOpenRetryBackoff / 2; min < floor {
		t.Fatalf("min request latency %v < %v; the open retry was skipped under contention", min, floor)
	}
	upper := relayUpstreamOpenRetryBackoff + 2*upstreamOpenDelay + perRequestSlack
	if max > upper {
		t.Fatalf("max request latency %v exceeds the bounded stack %v; retries stacked under contention", max, upper)
	}
	if wall > 15*time.Second {
		t.Fatalf("exercise wall time %v exceeded the time bound; not a small bounded exercise", wall)
	}

	t.Logf("relay bounded-concurrency: streams=%d requests=%d peak_in_flight=%d upstream_calls=%d attempts_per_request=2 registrations_after=0 readahead_used=%d readahead_peak=%d latency_min=%v latency_avg=%v latency_max=%v wall=%v",
		concurrentStreams, totalRequests, peakInFlight.Load(), upstreamCalls.Load(),
		relay.readAhead.used(), relay.readAhead.peak(),
		min.Round(time.Millisecond), avg.Round(time.Millisecond), max.Round(time.Millisecond), wall.Round(time.Millisecond))
}
