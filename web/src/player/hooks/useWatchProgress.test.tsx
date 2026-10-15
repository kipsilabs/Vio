import { act, cleanup, renderHook } from "@testing-library/react";
import { createElement } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { PlayerConfigProvider, type PlayerConfig } from "../context/PlayerConfigContext";
import { PlayerFetchError } from "../player-fetch";
import { useWatchProgress } from "./useWatchProgress";

const mocks = vi.hoisted(() => ({
  send: vi.fn(),
  observe: vi.fn(),
  capture: vi.fn(),
}));
vi.mock("../session-mutations", () => ({
  sendSessionProgress: mocks.send,
  observeSessionProgress: mocks.observe,
  captureSessionProgress: mocks.capture,
}));

const config: PlayerConfig = {
  apiBaseUrl: "https://silo.example.test/api/v1",
  getAccessToken: () => "private-token",
  getProfileId: () => "profile",
  getDeviceId: () => "device",
};
const wrapper = ({ children }: { children: React.ReactNode }) =>
  createElement(PlayerConfigProvider, { config, children });

const videoRef = {
  current: {
    currentTime: 30,
    paused: false,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  },
} as unknown as React.RefObject<HTMLVideoElement | null>;

beforeEach(() => {
  vi.clearAllMocks();
  mocks.observe.mockReturnValue(() => {});
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

it("stops polling and reports the session gone when progress answers 404", async () => {
  vi.useFakeTimers();
  mocks.send.mockRejectedValue(
    new PlayerFetchError(404, "Playback session not found.", "not_found"),
  );
  const onSessionLost = vi.fn();
  renderHook(() => useWatchProgress("session", videoRef, undefined, onSessionLost), { wrapper });

  await act(async () => {
    await vi.advanceTimersByTimeAsync(10_000);
  });
  expect(mocks.send).toHaveBeenCalledTimes(1);
  expect(onSessionLost).toHaveBeenCalledTimes(1);

  // A dead session must not keep sampling every interval.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(60_000);
  });
  expect(mocks.send).toHaveBeenCalledTimes(1);
  expect(onSessionLost).toHaveBeenCalledTimes(1);
});

it("keeps polling through a transient progress failure", async () => {
  vi.useFakeTimers();
  mocks.send.mockRejectedValue(new PlayerFetchError(503, "Server busy.", "unavailable"));
  const onSessionLost = vi.fn();
  renderHook(() => useWatchProgress("session", videoRef, undefined, onSessionLost), { wrapper });

  await act(async () => {
    await vi.advanceTimersByTimeAsync(30_000);
  });
  expect(mocks.send).toHaveBeenCalledTimes(3);
  expect(onSessionLost).not.toHaveBeenCalled();
});

it("ignores a gone answer from a session the effect has left", async () => {
  vi.useFakeTimers();
  let rejectA: ((error: unknown) => void) | undefined;
  mocks.send.mockImplementationOnce(
    () =>
      new Promise((_resolve, reject) => {
        rejectA = reject;
      }),
  );
  const lostA = vi.fn();
  const lostB = vi.fn();
  const { rerender } = renderHook(
    ({ id, onLost }: { id: string; onLost: () => void }) =>
      useWatchProgress(id, videoRef, undefined, onLost),
    { wrapper, initialProps: { id: "session-a", onLost: lostA } },
  );

  await act(async () => {
    await vi.advanceTimersByTimeAsync(10_000);
  });
  expect(mocks.send).toHaveBeenCalledTimes(1);

  // The player moves on to a new session; the old sampler is disposed.
  rerender({ id: "session-b", onLost: lostB });
  await act(async () => {
    await Promise.resolve();
  });

  // Session A's sample answers 404 only after B replaced it.
  await act(async () => {
    rejectA?.(new PlayerFetchError(404, "Playback session not found.", "not_found"));
    await Promise.resolve();
    await Promise.resolve();
  });

  expect(lostA).not.toHaveBeenCalled();
  expect(lostB).not.toHaveBeenCalled();
});

it("leaves no interval armed once progress answers the session is gone", async () => {
  vi.useFakeTimers();
  mocks.send.mockRejectedValue(
    new PlayerFetchError(404, "Playback session not found.", "not_found"),
  );
  const onSessionLost = vi.fn();
  renderHook(() => useWatchProgress("session", videoRef, undefined, onSessionLost), { wrapper });

  await act(async () => {
    await vi.advanceTimersByTimeAsync(10_000);
  });
  expect(onSessionLost).toHaveBeenCalledTimes(1);
  expect(vi.getTimerCount()).toBe(0);

  await act(async () => {
    await vi.advanceTimersByTimeAsync(60_000);
  });
  expect(mocks.send).toHaveBeenCalledTimes(1);
  expect(onSessionLost).toHaveBeenCalledTimes(1);
});

it("keeps polling when a progress 404 is a proxy page with no problem code", async () => {
  vi.useFakeTimers();
  // `mutationFetch` types every non-2xx response, but an HTML proxy 404 parses
  // to no problem: the code is undefined. It must not stop progress polling.
  mocks.send.mockRejectedValue(
    new PlayerFetchError(404, "Not Found", undefined, "<html>404 Not Found</html>"),
  );
  const onSessionLost = vi.fn();
  renderHook(() => useWatchProgress("session", videoRef, undefined, onSessionLost), { wrapper });

  await act(async () => {
    await vi.advanceTimersByTimeAsync(30_000);
  });
  expect(mocks.send).toHaveBeenCalledTimes(3);
  expect(onSessionLost).not.toHaveBeenCalled();
});

it("does not send a final keepalive to a session that is gone", async () => {
  vi.useFakeTimers();
  mocks.send.mockRejectedValue(
    new PlayerFetchError(404, "Playback session not found.", "not_found"),
  );
  const onSessionLost = vi.fn();
  const { unmount } = renderHook(
    () => useWatchProgress("session", videoRef, undefined, onSessionLost),
    { wrapper },
  );

  await act(async () => {
    await vi.advanceTimersByTimeAsync(10_000);
  });
  expect(onSessionLost).toHaveBeenCalledTimes(1);
  expect(mocks.send).toHaveBeenCalledTimes(1);

  // Unmounting must not fire the cleanup's keepalive report at a dead session.
  act(() => unmount());
  expect(mocks.send).toHaveBeenCalledTimes(1);
});
