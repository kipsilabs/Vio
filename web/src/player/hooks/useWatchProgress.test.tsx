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
