import { useEffect, useRef, useState } from "react";
import {
  captureProfileRequestContext,
  isCapturedProfileAuthorityActive,
  StaleApiRequestContextError,
} from "@/api/client";
import {
  mintPlaybackControlSocketTicket,
  playbackControlSocketProtocols,
  playbackControlSocketURL,
} from "@/api/v2/playbackControlSocket";
import { usePlayerConfig } from "../context/PlayerConfigContext";
import { isPlaybackSessionGoneError } from "../playback-errors";
import { playerV2Origin } from "../player-v2";
import { sessionInstallation } from "../session-mutations";
import {
  buildPlaybackRealtimeAck,
  buildPlaybackRealtimeHello,
  buildPlaybackRealtimeResult,
  parsePlaybackRealtimeMessage,
  type PlaybackCommandName,
  type PlaybackRealtimeCommandEnvelope,
  type PlaybackRealtimeEventEnvelope,
} from "../realtime-protocol";

type ConnectionState = "disconnected" | "connecting" | "connected";

interface UsePlaybackRealtimeOptions {
  sessionId: string | null;
  onCommand: (command: PlaybackRealtimeCommandEnvelope) => Promise<void> | void;
  onEvent?: (event: PlaybackRealtimeEventEnvelope) => void;
  /**
   * The commands this surface can execute, announced in the hello. Defaults to
   * the shared set; a surface that handles more names them so it does not
   * announce a command it would only reject.
   */
  supportedCommands?: PlaybackCommandName[];
  /**
   * Bumped each time a plan is adopted for the session. Any adopted plan proves
   * the server still holds the session, so it re-arms a control lane that gave
   * up after its mint kept answering that the session was gone.
   */
  recoveryRevision?: number;
}

interface UsePlaybackRealtimeResult {
  connectionState: ConnectionState;
}

const reconnectDelays = [500, 1_000, 2_000, 5_000];

/**
 * How many times a mint may answer 404/410 before the control lane concludes
 * the playback session is gone. The ticket is minted against a per-process,
 * in-memory session map, so one 404 is replica-local: it proves this replica
 * does not hold the session, not that the session ended. A short ladder of
 * retries still covers a request that lands on the wrong replica. After it, the
 * player stops minting against the dead id and offers a restart; retrying
 * forever is what left prod tabs polling an expired session in a `setTimeout`
 * loop.
 */
export const SESSION_GONE_MINT_RETRY_LIMIT = 3;

export function usePlaybackRealtime({
  sessionId,
  onCommand,
  onEvent,
  supportedCommands,
  recoveryRevision = 0,
}: UsePlaybackRealtimeOptions): UsePlaybackRealtimeResult {
  const config = usePlayerConfig();
  const [connectionState, setConnectionState] = useState<ConnectionState>("disconnected");
  const onCommandRef = useRef(onCommand);
  const onEventRef = useRef(onEvent);
  const supportedCommandsRef = useRef(supportedCommands);
  const seenCommandsRef = useRef<Set<string>>(new Set());
  /**
   * Re-arms a transport loop that degraded, from the recovery effect below. The
   * loop owns its own timers and socket, so it publishes the one action the
   * outside needs rather than exposing its state.
   */
  const reviveRef = useRef<() => void>(() => {});

  useEffect(() => {
    onCommandRef.current = onCommand;
  }, [onCommand]);

  useEffect(() => {
    supportedCommandsRef.current = supportedCommands;
  }, [supportedCommands]);

  useEffect(() => {
    onEventRef.current = onEvent;
  }, [onEvent]);

  useEffect(() => {
    if (!sessionId) {
      seenCommandsRef.current.clear();
      return;
    }

    // The control socket is owner-bound: the ticket is minted under the
    // account and profile captured here, and frames after either changes
    // belong to a session this browser no longer owns.
    const authority = captureProfileRequestContext();
    if (!authority) return;
    const authorityActive = () => isCapturedProfileAuthorityActive(authority);
    const installationId = sessionInstallation(sessionId);
    const origin = playerV2Origin(config) || window.location.origin;

    let disposed = false;
    let attempt = 0;
    let socket: WebSocket | null = null;
    let reconnectTimer: number | null = null;
    // A mint answered 404/410. The ticket store is per-process and in-memory, so
    // that answer is replica-local: it proves this replica does not hold the
    // session, not that playback ended. Retry it a short, bounded number of
    // times with backoff; when even that keeps answering gone, degrade this
    // control lane only. Playback and the stream keep running, and an adopted
    // plan re-arms the lane through revive() below.
    let sessionGoneMints = 0;
    let degraded = false;

    let terminated = false;
    const scheduleReconnect = () => {
      if (disposed || terminated || degraded) return;
      const delay = reconnectDelays[Math.min(attempt, reconnectDelays.length - 1)];
      attempt += 1;
      reconnectTimer = window.setTimeout(connect, delay);
    };
    /**
     * Ends the loop for good. An administrator terminate must leave no reconnect
     * scheduled, so a backoff timer already armed does not fire behind it.
     * Unlike `degraded`, this is not recoverable.
     */
    const terminate = () => {
      terminated = true;
      if (reconnectTimer !== null) {
        window.clearTimeout(reconnectTimer);
        reconnectTimer = null;
      }
    };

    const attach = (opened: WebSocket) => {
      socket = opened;

      socket.addEventListener("open", () => {
        if (!socket || socket.readyState !== WebSocket.OPEN) return;
        if (!authorityActive()) {
          socket.close();
          return;
        }
        attempt = 0;
        sessionGoneMints = 0;
        setConnectionState("connected");
        seenCommandsRef.current.clear();
        socket.send(
          JSON.stringify(buildPlaybackRealtimeHello(sessionId, supportedCommandsRef.current)),
        );
      });

      socket.addEventListener("message", (event) => {
        // Frames that arrive after the captured account/profile authority
        // changed belong to a session this browser no longer owns.
        if (!authorityActive()) {
          socket?.close();
          return;
        }
        const message = parsePlaybackRealtimeMessage(String(event.data));
        if (!message || message.session_id !== sessionId || !socket) {
          return;
        }
        if (message.type === "event") {
          onEventRef.current?.(message);
          return;
        }

        const command = message;
        if (seenCommandsRef.current.has(command.command_id)) {
          return;
        }
        seenCommandsRef.current.add(command.command_id);
        if (command.name === "terminate" && command.issued_by?.kind === "admin") {
          // An administrator ended the session; there is nothing to reconnect to.
          terminate();
        }

        if (socket.readyState === WebSocket.OPEN) {
          socket.send(JSON.stringify(buildPlaybackRealtimeAck(sessionId, command.command_id)));
        }

        void Promise.resolve(onCommandRef.current(command))
          .then(() => {
            if (!authorityActive() || !socket || socket.readyState !== WebSocket.OPEN) return;
            socket.send(
              JSON.stringify(
                buildPlaybackRealtimeResult(sessionId, command.command_id, "completed"),
              ),
            );
          })
          .catch((error: unknown) => {
            if (!authorityActive() || !socket || socket.readyState !== WebSocket.OPEN) return;
            const message = error instanceof Error ? error.message : "command_failed";
            socket.send(
              JSON.stringify(
                buildPlaybackRealtimeResult(sessionId, command.command_id, "rejected", message),
              ),
            );
          });
      });

      socket.addEventListener("close", () => {
        setConnectionState("disconnected");
        socket = null;
        scheduleReconnect();
      });

      socket.addEventListener("error", () => {
        socket?.close();
      });
    };

    const connect = () => {
      if (disposed || terminated || degraded) return;
      setConnectionState("connecting");

      if (!authorityActive()) {
        setConnectionState("disconnected");
        return;
      }

      void mintPlaybackControlSocketTicket(sessionId, installationId, authority)
        .then((ticket) => {
          if (disposed || terminated || degraded || !authorityActive()) return;
          try {
            attach(
              new WebSocket(
                playbackControlSocketURL(sessionId, origin),
                playbackControlSocketProtocols(ticket.ticket),
              ),
            );
          } catch {
            scheduleReconnect();
          }
        })
        .catch((error: unknown) => {
          if (disposed || terminated || degraded) return;
          if (error instanceof StaleApiRequestContextError) {
            // The account or profile changed underneath this player; nothing
            // this browser can mint is valid for the session any more.
            setConnectionState("disconnected");
            return;
          }
          if (isPlaybackSessionGoneError(error)) {
            // A 404/410 here is replica-local (see `sessionGoneMints`): one
            // answer is not proof the session ended, and neither is a spent
            // ladder. Degrade this control lane and keep the stream playing;
            // reporting the session gone here killed healthy playback whenever
            // the mint requests kept landing on another replica.
            sessionGoneMints += 1;
            setConnectionState("disconnected");
            if (sessionGoneMints < SESSION_GONE_MINT_RETRY_LIMIT) {
              scheduleReconnect();
            } else {
              degraded = true;
            }
            return;
          }
          // A refused mint (403 non-owner, 409 installation mismatch) is
          // retried only under the original authority with bounded backoff.
          setConnectionState("disconnected");
          scheduleReconnect();
        });
    };

    /**
     * Re-arms the lane after it degraded on a mint 404. An adopted plan proves
     * the server holds the session, so the replica-local miss that stopped the
     * lane is gone. A terminated lane (an administrator ended the session) is
     * not revived.
     */
    const revive = () => {
      if (disposed || terminated || !degraded) return;
      degraded = false;
      sessionGoneMints = 0;
      attempt = 0;
      connect();
    };
    reviveRef.current = revive;

    connect();

    return () => {
      disposed = true;
      reviveRef.current = () => {};
      setConnectionState("disconnected");
      if (reconnectTimer !== null) {
        window.clearTimeout(reconnectTimer);
      }
      if (
        socket &&
        (socket.readyState === WebSocket.OPEN || socket.readyState === WebSocket.CONNECTING)
      ) {
        socket.close();
      }
    };
  }, [config, sessionId]);

  // A recovery (a plan adopted for the same session) re-arms a degraded lane.
  useEffect(() => {
    reviveRef.current();
  }, [recoveryRevision]);

  return { connectionState };
}
