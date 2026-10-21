import { useCallback, useEffect, useRef } from "react";
import { usePlayerConfig } from "../context/PlayerConfigContext";
import { isPlaybackSessionGoneError } from "../playback-errors";
import {
  sendSessionProgress,
  observeSessionProgress,
  captureSessionProgress,
} from "../session-mutations";
import { isPlaybackSessionEndedError } from "../player-fetch";
import { toMediaTime } from "../utils/mediaTimeline";

/**
 * Reports watch progress to the server every 10 seconds.
 * Reads currentTime and paused state from the video element.
 * An optional streamOriginRef maps player-local time back onto the original
 * media timeline for restarted HLS sessions.
 *
 * A progress sample answered 404/410 means the session no longer exists
 * server-side; `onSessionLost` is reported once and polling stops, so the
 * player is not nudged every interval forever. The one recovery or lost UI
 * behind it is the caller's. A recovery that keeps the same session id (an
 * adopted plan) re-arms the sampler through `recoveryRevision`, so a later loss
 * is reported again.
 *
 * On cleanup (player unmount), sends one final progress report with
 * keepalive so the backend records the last known position even when
 * the user seeks and closes quickly.
 *
 * `onSessionEnded` runs when a report learns the server ended the session for
 * good (an administrator stopped it while the player was unreachable).
 */
export function useWatchProgress(
  sessionId: string | null,
  videoRef: React.RefObject<HTMLVideoElement | null>,
  streamOriginRef?: React.RefObject<number>,
  onSessionLost?: () => void,
  recoveryRevision = 0,
  onSessionEnded?: () => void,
) {
  const onSessionEndedRef = useRef(onSessionEnded);
  useEffect(() => {
    onSessionEndedRef.current = onSessionEnded;
  }, [onSessionEnded]);
  const config = usePlayerConfig();
  const configRef = useRef(config);
  configRef.current = config;
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const skipCleanupReportRef = useRef(false);
  const onSessionLostRef = useRef(onSessionLost);
  const reportedLostRef = useRef(false);
  /**
   * Restarts the poll interval after a recovery, published by the session
   * effect so the recovery effect can resume polling without re-running that
   * effect (which would send a spurious paused keepalive).
   */
  const startPollingRef = useRef<() => void>(() => {});

  useEffect(() => {
    onSessionLostRef.current = onSessionLost;
  }, [onSessionLost]);

  const getProgressSnapshot = useCallback(() => {
    const video = videoRef.current;
    // Before metadata (startup, or a transport swap) currentTime is not a
    // position on this stream yet; reporting it would overwrite the resume point.
    if (!video || video.readyState === HTMLMediaElement.HAVE_NOTHING) return null;

    return {
      position: toMediaTime(video.currentTime ?? 0, streamOriginRef?.current ?? 0),
      isPaused: video.paused ?? true,
    };
  }, [streamOriginRef, videoRef]);

  useEffect(() => {
    if (!sessionId) return;
    const read = () => {
      const sample = getProgressSnapshot();
      return sample ? { position: sample.position, is_paused: sample.isPaused } : null;
    };
    const forget = observeSessionProgress(sessionId, read);
    const capture = () => captureSessionProgress(sessionId, read());
    const video = videoRef.current;
    const events = ["timeupdate", "pause", "seeked", "ended"];
    capture();
    events.forEach((event) => video?.addEventListener(event, capture));
    return () => {
      events.forEach((event) => video?.removeEventListener(event, capture));
      forget();
    };
  }, [getProgressSnapshot, sessionId, videoRef]);

  const reportProgress = useCallback(
    async (options?: { keepalive?: boolean; isPaused?: boolean }) => {
      if (!sessionId) return;

      const snapshot = getProgressSnapshot();
      if (!snapshot) return;

      await sendSessionProgress(
        configRef.current,
        sessionId,
        {
          position: snapshot.position,
          is_paused: options?.isPaused ?? snapshot.isPaused,
        },
        options?.keepalive,
      );
    },
    [getProgressSnapshot, sessionId],
  );

  const flushProgress = useCallback(async () => {
    if (!sessionId) return;

    await reportProgress({ isPaused: true });
    skipCleanupReportRef.current = true;
  }, [reportProgress, sessionId]);

  useEffect(() => {
    if (!sessionId) return;
    skipCleanupReportRef.current = false;
    reportedLostRef.current = false;
    let disposed = false;

    const stopPolling = () => {
      if (intervalRef.current) clearInterval(intervalRef.current);
      intervalRef.current = null;
    };

    // Serialized samples can outlive the interval and each reject on their own,
    // so latch the terminal state to hand the player exactly one report. The
    // latch lives in a ref because a recovery under the same session id re-arms
    // it without re-running this effect.
    const startPolling = () => {
      stopPolling();
      reportedLostRef.current = false;
      skipCleanupReportRef.current = false;
      intervalRef.current = setInterval(() => {
        reportProgress().catch((error: unknown) => {
          // Every other failure is best effort — a transient sample failure
          // must not disrupt playback. A session that no longer exists is
          // terminal: stop polling and hand the player its one report. A sample
          // from a session this effect has already left belongs to the old id.
          if (disposed || reportedLostRef.current) return;
          if (isPlaybackSessionGoneError(error)) {
            reportedLostRef.current = true;
            stopPolling();
            // The session row is gone, so the cleanup's final keepalive would
            // only be answered 404. Skip it.
            skipCleanupReportRef.current = true;
            // An administrator-ended session is a session-gone answer with a
            // specific problem code; report both the recovery hook and the
            // ended hook so the player can leave instead of reconnecting.
            if (isPlaybackSessionEndedError(error)) onSessionEndedRef.current?.();
            onSessionLostRef.current?.();
          }
        });
      }, 10_000);
    };
    startPollingRef.current = startPolling;
    startPolling();

    return () => {
      disposed = true;
      startPollingRef.current = () => {};
      stopPolling();

      if (skipCleanupReportRef.current) return;

      // Send one final progress report so the backend has the latest
      // position before the session is deleted (e.g. after a seek).
      reportProgress({ keepalive: true, isPaused: true }).catch(() => {});
    };
  }, [reportProgress, sessionId]);

  // A recovery can keep the session id: an adopted plan proves the server still
  // holds the session, so polling that stopped on a gone answer resumes. Without
  // this the latch would re-arm only on a new session id, and the same session
  // would stop reporting for the rest of its life.
  useEffect(() => {
    if (!sessionId) return;
    if (!reportedLostRef.current) return;
    startPollingRef.current();
  }, [recoveryRevision, sessionId]);

  return flushProgress;
}
