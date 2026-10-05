package playback

import (
	"errors"
	"sync"
	"time"
)

// ErrRealtimeConnectionNotFound is returned when a session has no active realtime connection.
var ErrRealtimeConnectionNotFound = errors.New("realtime connection not found")

// RealtimeConnection is the minimal send interface required by the hub.
// Implementations must ensure WriteJSON is bounded by an external deadline or
// cancellation policy; the hub serializes writes per session and assumes each
// write returns in finite time.
type RealtimeConnection interface {
	WriteJSON(v any) error
}

type sessionLane struct {
	conn       RealtimeConnection
	mu         sync.Mutex
	closed     bool
	generation uint64
}

// RealtimeRegistration is an opaque ownership token for a realtime connection.
type RealtimeRegistration struct {
	sessionID  string
	lane       *sessionLane
	generation uint64
}

// RealtimeHub stores one active realtime connection per playback session.
type RealtimeHub struct {
	mu                   sync.RWMutex
	connections          map[string]*sessionLane
	onInitialRegister    func()
	onRegisterLaneLookup func(sessionID string, lane *sessionLane)
	// onNewConnection fires when a session that had a live lane gets a new one:
	// either a registration takes over the existing lane, or a client
	// reconnects after Unregister deleted it. It never fires for a session's
	// first connection and never for another replica's lane lookup.
	onNewConnection func(sessionID string)
	// recentlyReleased records when a session's lane was released, so the next
	// registration for one is recognized as a reconnect. Entries are consumed by
	// that registration and expire on their own: a session that ends for good
	// never registers again, so a count-only bound would grow without limit.
	// Retention must comfortably exceed a client reconnect gap while bounding
	// memory to sessions released within it.
	recentlyReleased       map[string]time.Time
	recentReleaseRetention time.Duration
}

// NewRealtimeHub creates an empty realtime hub.
func NewRealtimeHub() *RealtimeHub {
	return &RealtimeHub{
		connections:            make(map[string]*sessionLane),
		recentlyReleased:       make(map[string]time.Time),
		recentReleaseRetention: 10 * time.Minute,
	}
}

// Register associates a realtime connection with a playback session.
// A later registration for the same session replaces the prior connection.
func (h *RealtimeHub) Register(sessionID string, conn RealtimeConnection) *RealtimeRegistration {
	if h == nil || sessionID == "" || conn == nil {
		return nil
	}

	h.mu.Lock()
	lane := h.connections[sessionID]
	if lane == nil {
		lane = &sessionLane{conn: conn, generation: 1}
		h.connections[sessionID] = lane
		// Whether this is the session's first connection or a reconnect after a
		// disconnect cannot be told apart from the lane map alone: Unregister
		// DELETES the lane, so a reconnecting client lands in exactly this
		// branch. Deciding by "had a lane before" would miss the common case, so
		// the reconnect signal is the registration itself.
		releasedAt, reconnected := h.recentlyReleased[sessionID]
		if reconnected && time.Since(releasedAt) > h.recentReleaseRetention {
			// Too old to be a reconnect this can rely on; treat it as a first
			// connection rather than re-arming an exhausted budget on it.
			reconnected = false
		}
		delete(h.recentlyReleased, sessionID)
		h.mu.Unlock()
		if h.onInitialRegister != nil {
			h.onInitialRegister()
		}
		if reconnected && h.onNewConnection != nil {
			h.onNewConnection(sessionID)
		}
		return &RealtimeRegistration{sessionID: sessionID, lane: lane, generation: 1}
	}
	h.mu.Unlock()

	// A LANE LOOKUP is not a new connection: another replica inspecting this
	// session's lane must not be observable as a delivery event. Only the
	// successful takeover below counts.
	if h.onRegisterLaneLookup != nil {
		h.onRegisterLaneLookup(sessionID, lane)
	}
	lane.mu.Lock()
	if lane.closed {
		lane.mu.Unlock()
		return nil
	}
	lane.conn = conn
	lane.closed = false
	lane.generation++
	reg := &RealtimeRegistration{
		sessionID:  sessionID,
		lane:       lane,
		generation: lane.generation,
	}
	lane.mu.Unlock()

	if h.onNewConnection != nil {
		// This connection replaced a live one, so a consumer that gives up on
		// an undeliverable command (audio withdrawal, download telemetry) can
		// rearm here: a reconnecting client is a fresh delivery candidate.
		h.onNewConnection(sessionID)
	}
	return reg
}

// EmitNewConnectionForTest fires the new-connection hook for one session so a
// test can exercise rearm behavior without synthesizing a lane takeover. It
// reports whether a hook was installed.
func (h *RealtimeHub) EmitNewConnectionForTest(sessionID string) bool {
	if h == nil || sessionID == "" {
		return false
	}
	h.mu.RLock()
	hook := h.onNewConnection
	h.mu.RUnlock()
	if hook == nil {
		return false
	}
	hook(sessionID)
	return true
}

// sweepRecentReleasesLocked drops reconnect marks older than the retention
// window. The caller holds h.mu.
func (h *RealtimeHub) sweepRecentReleasesLocked(now time.Time) {
	for sessionID, releasedAt := range h.recentlyReleased {
		if now.Sub(releasedAt) > h.recentReleaseRetention {
			delete(h.recentlyReleased, sessionID)
		}
	}
}

// SetOnNewConnection installs a hook that fires only when a registration takes
// over an existing lane (a reconnect), never for a session's first registration
// and never for another replica's lane lookup. It lets a consumer that gives up
// on undeliverable per-session commands rearm itself on reconnect. Only the
// latest hook is retained.
func (h *RealtimeHub) SetOnNewConnection(hook func(sessionID string)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onNewConnection = hook
}

// Unregister removes the active realtime connection for the given registration
// token only if it still matches the currently registered connection.
func (h *RealtimeHub) Unregister(reg *RealtimeRegistration) bool {
	if h == nil || reg == nil || reg.sessionID == "" || reg.lane == nil {
		return false
	}

	h.mu.RLock()
	lane, ok := h.connections[reg.sessionID]
	h.mu.RUnlock()
	if !ok || lane == nil || lane != reg.lane {
		return false
	}

	lane.mu.Lock()
	if lane.generation != reg.generation || lane.closed {
		lane.mu.Unlock()
		return false
	}
	lane.closed = true
	lane.conn = nil
	lane.generation++
	nextGeneration := lane.generation
	h.mu.Lock()
	if current, ok := h.connections[reg.sessionID]; ok && current == lane && lane.closed && lane.conn == nil && lane.generation == nextGeneration {
		delete(h.connections, reg.sessionID)
	}
	// This session had a live lane, so the next registration for it is a
	// reconnect rather than a first connection. The mark is what lets the hook
	// fire for the disconnect-then-reconnect lifecycle, which is the common one.
	// Register consumes it and re-arms on the actual registration; releasing a
	// lane is not itself a delivery event, so no hook fires here.
	now := time.Now()
	h.recentlyReleased[reg.sessionID] = now
	h.sweepRecentReleasesLocked(now)
	h.mu.Unlock()
	lane.mu.Unlock()

	return true
}

// Send writes a message to the active connection for the given session.
func (h *RealtimeHub) Send(sessionID string, message any) error {
	if h == nil || sessionID == "" {
		return ErrRealtimeConnectionNotFound
	}

	h.mu.RLock()
	lane, ok := h.connections[sessionID]
	if !ok || lane == nil {
		h.mu.RUnlock()
		return ErrRealtimeConnectionNotFound
	}
	h.mu.RUnlock()

	lane.mu.Lock()
	if lane.closed || lane.conn == nil {
		lane.mu.Unlock()
		return ErrRealtimeConnectionNotFound
	}
	err := lane.conn.WriteJSON(message)
	lane.mu.Unlock()
	return err
}

// PublishDownloadProgress builds and sends a download.progress event to the
// active connection for a session. It is best-effort telemetry: a session with
// no live connection (or a write failure) reports false and the caller keeps
// playing. A malformed payload also reports false rather than panicking.
func (h *RealtimeHub) PublishDownloadProgress(sessionID string, payload DownloadProgressPayload) bool {
	if h == nil || sessionID == "" {
		return false
	}
	event, err := NewDownloadProgressEvent(sessionID, payload)
	if err != nil {
		return false
	}
	return h.Send(sessionID, event) == nil
}
