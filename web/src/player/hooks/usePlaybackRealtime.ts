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
   * Reports that the session no longer exists on the server (the ticket mint
   * answered 404/410). The control lane stops reconnecting; the player runs its
   * one recovery or surfaces the lost state.
   */
  onSessionLost?: () => void;
}

interface UsePlaybackRealtimeResult {
  connectionState: ConnectionState;
}

const reconnectDelays = [500, 1_000, 2_000, 5_000];

export function usePlaybackRealtime({
  sessionId,
  onCommand,
  onEvent,
  supportedCommands,
  onSessionLost,
}: UsePlaybackRealtimeOptions): UsePlaybackRealtimeResult {
  const config = usePlayerConfig();
  const [connectionState, setConnectionState] = useState<ConnectionState>("disconnected");
  const onCommandRef = useRef(onCommand);
  const onEventRef = useRef(onEvent);
  const supportedCommandsRef = useRef(supportedCommands);
  const onSessionLostRef = useRef(onSessionLost);
  const seenCommandsRef = useRef<Set<string>>(new Set());

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
    onSessionLostRef.current = onSessionLost;
  }, [onSessionLost]);

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

    let terminated = false;
    const scheduleReconnect = () => {
      if (disposed || terminated) return;
      const delay = reconnectDelays[Math.min(attempt, reconnectDelays.length - 1)];
      attempt += 1;
      reconnectTimer = window.setTimeout(connect, delay);
    };
    /**
     * Ends the loop for good. A terminal answer (an administrator terminate or
     * a session the server no longer has) must leave no reconnect scheduled,
     * so a backoff timer already armed does not fire behind it.
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
      if (disposed || terminated) return;
      setConnectionState("connecting");

      if (!authorityActive()) {
        setConnectionState("disconnected");
        return;
      }

      void mintPlaybackControlSocketTicket(sessionId, installationId, authority)
        .then((ticket) => {
          if (disposed || !authorityActive()) return;
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
          if (disposed || terminated) return;
          if (error instanceof StaleApiRequestContextError) {
            // The account or profile changed underneath this player; nothing
            // this browser can mint is valid for the session any more.
            setConnectionState("disconnected");
            return;
          }
          if (isPlaybackSessionGoneError(error)) {
            // The session no longer exists server-side (a restart, expiry, or
            // terminate). Retrying the mint can never succeed, so the loop
            // stops; the player runs its one recovery or surfaces the lost
            // state. A later session id re-runs this effect and resets.
            terminate();
            setConnectionState("disconnected");
            onSessionLostRef.current?.();
            return;
          }
          // A refused mint (403 non-owner, 409 installation mismatch) is
          // retried only under the original authority with bounded backoff.
          setConnectionState("disconnected");
          scheduleReconnect();
        });
    };

    connect();

    return () => {
      disposed = true;
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

  return { connectionState };
}
