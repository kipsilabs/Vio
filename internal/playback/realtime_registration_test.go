package playback

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type registrationTestConn struct{}

func (registrationTestConn) WriteJSON(any) error { return nil }

func TestCurrentRealtimeRegistrationRefusesStaleConnections(t *testing.T) {
	hub := NewRealtimeHub()
	old := hub.Register("session", registrationTestConn{})
	current := hub.Register("session", registrationTestConn{})
	calls := 0
	apply := func() error { calls++; return nil }
	for _, reg := range []*RealtimeRegistration{nil, old} {
		if err := hub.WithCurrentRegistration(reg, apply); !errors.Is(err, ErrStaleRealtimeRegistration) {
			t.Fatalf("stale registration: %v", err)
		}
	}
	if err := NewRealtimeHub().WithCurrentRegistration(current, apply); !errors.Is(err, ErrStaleRealtimeRegistration) {
		t.Fatalf("foreign hub registration: %v", err)
	}
	if calls != 0 {
		t.Fatal("stale callback executed")
	}
	want := errors.New("authority refused")
	if err := hub.WithCurrentRegistration(current, func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("callback error lost: %v", err)
	}
	if err := hub.WithCurrentRegistration(current, apply); err != nil || calls != 1 {
		t.Fatalf("current callback: calls=%d err=%v", calls, err)
	}
	if !hub.Unregister(current) {
		t.Fatal("unregister current")
	}
	replacement := hub.Register("session", registrationTestConn{})
	if err := hub.WithCurrentRegistration(current, apply); !errors.Is(err, ErrStaleRealtimeRegistration) || calls != 1 {
		t.Fatalf("deleted lane admitted old callback: %v", err)
	}
	if err := hub.WithCurrentRegistration(replacement, apply); err != nil || calls != 2 {
		t.Fatalf("new lane: %v", err)
	}
}

func TestCurrentRealtimeRegistrationSerializesReplacement(t *testing.T) {
	hub := NewRealtimeHub()
	old := hub.Register("session", registrationTestConn{})
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- hub.WithCurrentRegistration(old, func() error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-t.Context().Done():
				return t.Context().Err()
			}
		})
	}()
	<-entered
	lookup := make(chan struct{})
	hub.onRegisterLaneLookup = func(string, *sessionLane) { close(lookup) }
	replaced := make(chan *RealtimeRegistration, 1)
	go func() { replaced <- hub.Register("session", registrationTestConn{}) }()
	<-lookup
	// The callback holds the lane: replacement cannot become visible until it
	// finishes. Observe that lock directly rather than relying on a sleep.
	if old.lane.mu.TryLock() {
		old.lane.mu.Unlock()
		t.Fatal("callback did not retain lane ownership")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current := <-replaced
	if current == nil {
		t.Fatal("replacement failed")
	}
	if err := hub.WithCurrentRegistration(old, func() error { t.Error("superseded callback executed"); return nil }); !errors.Is(err, ErrStaleRealtimeRegistration) {
		t.Fatalf("superseded registration: %v", err)
	}
	if hub.Unregister(old) {
		t.Fatal("old disconnect removed successor")
	}
}

// TestRealtimeHubRecentlyReleasedIsBounded covers the reconnect mark's retention.
// A session that ends for good never registers again, so an unbounded mark would
// accumulate one entry per session the process ever served.
func TestRealtimeHubRecentlyReleasedIsBounded(t *testing.T) {
	hub := NewRealtimeHub()

	// Churn through many sessions that register and disconnect without returning.
	for i := 0; i < 500; i++ {
		reg := hub.Register(uuid.NewString(), registrationTestConn{})
		if reg == nil {
			t.Fatalf("register %d: no registration", i)
		}
		if !hub.Unregister(reg) {
			t.Fatalf("unregister %d: not released", i)
		}
	}

	hub.mu.RLock()
	retention := hub.recentReleaseRetention
	hub.mu.RUnlock()
	if retention <= 0 {
		t.Fatal("the reconnect mark needs a positive retention window to be bounded")
	}
	// Every mark belongs to a session that just disconnected, so they are all
	// fresh: the bound is time, not count. Assert the mark is a timestamp and that
	// sweeping drops the stale ones.
	hub.mu.Lock()
	for sessionID := range hub.recentlyReleased {
		hub.recentlyReleased[sessionID] = time.Now().Add(-2 * retention)
	}
	hub.sweepRecentReleasesLocked(time.Now())
	held := len(hub.recentlyReleased)
	hub.mu.Unlock()
	if held != 0 {
		t.Fatalf("reconnect marks held = %d after every mark went stale, want 0", held)
	}
}

// TestRealtimeHubReconnectMarkFiresWithinRetention pins that a genuine
// disconnect-then-reconnect inside the window is still recognized, which is the
// lifecycle the mark exists for, and that neither a first connection nor a lane
// release is reported as one.
func TestRealtimeHubReconnectMarkFiresWithinRetention(t *testing.T) {
	hub := NewRealtimeHub()
	var reconnected []string
	hub.SetOnNewConnection(func(sessionID string) { reconnected = append(reconnected, sessionID) })

	sessionID := uuid.NewString()
	reg := hub.Register(sessionID, registrationTestConn{})
	if len(reconnected) != 0 {
		t.Fatal("a session's first connection is not a reconnect")
	}
	if !hub.Unregister(reg) {
		t.Fatal("release the first connection")
	}
	if len(reconnected) != 0 {
		t.Fatal("releasing a lane is not itself a delivery event")
	}

	next := hub.Register(sessionID, registrationTestConn{})
	if next == nil {
		t.Fatal("the reconnect must register")
	}
	if len(reconnected) != 1 || reconnected[0] != sessionID {
		t.Fatalf("reconnect hook fired %v, want exactly one call for %s", reconnected, sessionID)
	}

	// A takeover of a still-live lane is also a reconnect.
	if !hub.Unregister(next) {
		t.Fatal("release the reconnected lane")
	}
	third := hub.Register(sessionID, registrationTestConn{})
	if third == nil {
		t.Fatal("the third connection must register")
	}
	if len(reconnected) != 2 {
		t.Fatalf("reconnect hook fired %d times, want 2", len(reconnected))
	}
}
