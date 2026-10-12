package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/watchstate"
)

type playbackCommandRecord struct {
	SessionID string
	Name      playback.CommandName
}

// stopPlaybackSession stops a session. userInitiated must be true only for a
// genuine user/admin stop (the recipe card is then deleted); false for system
// teardown such as an ffmpeg-exit cleanup, where the card is kept so the client
// can reconstruct.
func (h *PlaybackHandler) stopPlaybackSession(ctx context.Context, session *playback.Session, userInitiated bool) error {
	_, err := h.stopPlaybackSessionWithResult(ctx, session, userInitiated, "")
	return err
}

// stopPlaybackSessionWithResult is stopPlaybackSession reporting the history
// writer's result, which the v2 stop receipt carries. historyID is the row this
// stop must write, derived from the durable stop identity, so a v2 retry reuses
// it instead of adding a second row; empty means this path has no durable stop
// identity and the writer mints a fresh row per stop.
func (h *PlaybackHandler) stopPlaybackSessionWithResult(ctx context.Context, session *playback.Session, userInitiated bool, historyID string) (watchstate.PlaybackStopResult, error) {
	if h == nil || session == nil || session.ID == "" {
		return watchstate.PlaybackStopResult{}, playback.ErrSessionNotFound
	}
	// A stop is a teardown: once the live session is removed, the durable half
	// (authority, transport cancel, deny marker, history) must finish even if
	// the request that issued the stop is gone. A client that drops the
	// connection right after its stop, or a canceled DELETE, must not leave a
	// half-stopped session.
	ctx, cancelStop := teardownContext(ctx)
	defer cancelStop()
	// Replacement preparation holds this lifecycle lock until its new live
	// session state and durable plan are committed (or rolled back). Wait for
	// that boundary, then reload the route: callers commonly hold a copy taken
	// before the replacement started, and deleting authority from that stale
	// copy would leave the successor transport playable after a successful stop.
	//
	// The wait is bounded by the teardown deadline. A finalization under a v2
	// stop holds a lease another replica may take over once it lapses; blocking
	// on the holder past the deadline and then starting teardown would resume
	// work the store has already handed to someone else. Give up instead.
	unlock, err := h.tm.LockSessionLifecycleContext(ctx, session.ID)
	if err != nil {
		return watchstate.PlaybackStopResult{}, fmt.Errorf("wait for session lifecycle: %w", err)
	}
	defer unlock()
	// The wait may have consumed the budget at the instant the lock became
	// available. Do not begin a teardown that cannot finish inside it.
	if err := ctx.Err(); err != nil {
		return watchstate.PlaybackStopResult{}, err
	}
	current, err := h.sessionMgr.GetSession(session.ID)
	if err != nil {
		return watchstate.PlaybackStopResult{}, err
	}
	session = current
	if userInitiated {
		if err := h.deleteRequiredProgressiveRemuxAuthorityV3(ctx, session); err != nil {
			return watchstate.PlaybackStopResult{}, fmt.Errorf("persist progressive remux stop: %w", err)
		}
		if requiresProgressiveRemuxAuthorityV3(session) {
			if err := h.tm.CancelRemoteTranscode(ctx, remoteTransportID(session), session.TranscodeNodeURL); err != nil {
				return watchstate.PlaybackStopResult{}, fmt.Errorf("cancel progressive remux transport: %w", err)
			}
		}
	}

	if err := h.sessionMgr.StopSession(session.ID); err != nil {
		return watchstate.PlaybackStopResult{}, err
	}
	if userInitiated {
		// A user or admin stop is final: revoke the session's stream tokens on
		// every replica, not just the live entry here.
		h.StreamDeny.Deny(ctx, session.ID)
	}
	// Report whether the durable stop finished; the v2 finalization finalizes
	// the receipt only on success.
	return h.finalizeSessionStopWithResult(ctx, session, true, "stop", userInitiated, historyID)
}

func (h *PlaybackHandler) stopPlaybackSessionByID(ctx context.Context, sessionID string, userInitiated bool) error {
	if h == nil || sessionID == "" {
		return playback.ErrSessionNotFound
	}
	session, err := h.sessionMgr.GetSession(sessionID)
	if err != nil {
		return err
	}
	return h.stopPlaybackSession(ctx, session, userInitiated)
}

func (h *PlaybackHandler) abortPlaybackSession(ctx context.Context, session *playback.Session) error {
	if h == nil || session == nil || session.ID == "" {
		return playback.ErrSessionNotFound
	}

	if err := h.sessionMgr.StopSession(session.ID); err != nil {
		return err
	}
	h.finalizeSessionAbort(ctx, session, true, "abort")
	h.markAttemptStoppedServerSide(ctx, session.ID)
	return nil
}

//nolint:unused // Retained for compatibility with dormant integration paths.
func (h *PlaybackHandler) abortPlaybackSessionByID(ctx context.Context, sessionID string) error {
	if h == nil || sessionID == "" {
		return playback.ErrSessionNotFound
	}
	session, err := h.sessionMgr.GetSession(sessionID)
	if err != nil {
		return err
	}
	return h.abortPlaybackSession(ctx, session)
}

func (h *PlaybackHandler) rememberRealtimeCommand(commandID, sessionID string, name playback.CommandName) {
	if h == nil || commandID == "" || sessionID == "" {
		return
	}

	h.realtimeCommandMu.Lock()
	h.realtimeCommands[commandID] = playbackCommandRecord{
		SessionID: sessionID,
		Name:      name,
	}
	h.realtimeCommandMu.Unlock()
}

func (h *PlaybackHandler) forgetRealtimeCommand(commandID string) {
	if h == nil || commandID == "" {
		return
	}

	h.realtimeCommandMu.Lock()
	delete(h.realtimeCommands, commandID)
	h.realtimeCommandMu.Unlock()
}

func (h *PlaybackHandler) getRealtimeCommand(commandID string) (playbackCommandRecord, bool) {
	if h == nil || commandID == "" {
		return playbackCommandRecord{}, false
	}

	h.realtimeCommandMu.Lock()
	record, ok := h.realtimeCommands[commandID]
	h.realtimeCommandMu.Unlock()
	return record, ok
}

func (h *PlaybackHandler) setRealtimeConnectionState(sessionID string, connected bool) bool {
	if h == nil || sessionID == "" {
		return false
	}

	type realtimeStateSetter interface {
		SetRealtimeConnection(sessionID string, connected bool) error
	}

	mgr, ok := h.sessionMgr.(realtimeStateSetter)
	if !ok {
		return false
	}

	session, err := h.sessionMgr.GetSession(sessionID)
	if err == nil && session != nil && session.HasRealtimeConnection == connected {
		return false
	}

	if err := mgr.SetRealtimeConnection(sessionID, connected); err != nil {
		if errors.Is(err, playback.ErrSessionNotFound) {
			return false
		}
		slog.Warn("failed to update realtime connection state", "session", sessionID, "connected", connected, "error", err)
		return false
	}
	return true
}
