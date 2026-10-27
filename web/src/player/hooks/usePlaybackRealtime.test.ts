import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { createElement } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { V2ProblemError } from "@/api/v2/request";
import { PlayerConfigProvider, type PlayerConfig } from "../context/PlayerConfigContext";
import { SESSION_GONE_MINT_RETRY_LIMIT, usePlaybackRealtime } from "./usePlaybackRealtime";

const mocks = vi.hoisted(() => ({
  capture: vi.fn(),
  current: vi.fn(),
  installation: vi.fn(),
  mint: vi.fn(),
}));
vi.mock("@/api/client", () => ({
  captureProfileRequestContext: mocks.capture,
  isCapturedProfileAuthorityActive: mocks.current,
  StaleApiRequestContextError: class extends Error {},
}));
vi.mock("../session-mutations", () => ({ sessionInstallation: mocks.installation }));
vi.mock("@/api/v2/playbackControlSocket", async (original) => ({
  ...(await original<object>()),
  mintPlaybackControlSocketTicket: mocks.mint,
}));
const config: PlayerConfig = {
  apiBaseUrl: "https://silo.example.test/api/v1",
  getAccessToken: () => "private-token",
  getProfileId: () => "profile",
  getDeviceId: () => "device",
};
const authority = { profileId: "profile" };
class Socket extends EventTarget {
  static OPEN = 1;
  static CONNECTING = 0;
  static instances: Socket[] = [];
  readyState = 0;
  send = vi.fn();
  close = vi.fn(() => {
    this.readyState = 3;
    this.dispatchEvent(new Event("close"));
  });
  constructor(
    readonly url: string,
    readonly protocols: string[],
  ) {
    super();
    Socket.instances.push(this);
  }
}
const wrapper = ({ children }: { children: React.ReactNode }) =>
  createElement(PlayerConfigProvider, { config, children });
beforeEach(() => {
  vi.clearAllMocks();
  Socket.instances = [];
  mocks.capture.mockReturnValue(authority);
  mocks.current.mockReturnValue(true);
  mocks.installation.mockReturnValue("install");
  mocks.mint.mockResolvedValue({ ticket: "ticket", protocol: "silo.playback-control.v2" });
  vi.stubGlobal("WebSocket", Socket);
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});
it("opens only installation-bound v2 control without a URL credential", async () => {
  renderHook(() => usePlaybackRealtime({ sessionId: "session", onCommand: vi.fn() }), { wrapper });
  await waitFor(() => expect(Socket.instances).toHaveLength(1));
  expect(mocks.mint).toHaveBeenCalledWith("session", "install", authority);
  expect(Socket.instances[0]!.url).toBe(
    "wss://silo.example.test/api/v2/playback/sessions/session/control/ws",
  );
  expect(Socket.instances[0]!.protocols).toEqual([
    "silo.playback-control.v2",
    "silo.ticket.ticket",
  ]);
});
it.each([503, 403])("never opens bridge control after ticket failure %s", async (status) => {
  vi.useFakeTimers();
  mocks.mint.mockRejectedValue(
    new V2ProblemError("createPlaybackControlSocketTicket", {
      type: `https://siloserver.org/docs/api/v2/problems/${status === 403 ? "permission_denied" : "unavailable"}`,
      title: "Refused",
      status,
      detail: "Refused.",
      instance: "/api/v2/playback/sessions/session/control/ws-ticket",
    }),
  );
  const onCommand = vi.fn();
  const { result } = renderHook(() => usePlaybackRealtime({ sessionId: "session", onCommand }), {
    wrapper,
  });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(510);
  });
  expect(mocks.mint).toHaveBeenCalledTimes(2);
  expect(Socket.instances).toHaveLength(0);
  expect(result.current.connectionState).toBe("disconnected");
  expect(onCommand).not.toHaveBeenCalled();
});

function notFoundProblem() {
  return new V2ProblemError("createPlaybackControlSocketTicket", {
    type: "https://siloserver.org/docs/api/v2/problems/not_found",
    title: "Not found",
    status: 404,
    detail: "Playback session not found.",
    instance: "/api/v2/playback/sessions/session/control/ws-ticket",
  });
}

it("retries a replica-local mint 404 with backoff instead of treating it as gone", async () => {
  vi.useFakeTimers();
  mocks.mint.mockRejectedValue(notFoundProblem());
  const { result } = renderHook(
    () => usePlaybackRealtime({ sessionId: "session", onCommand: vi.fn() }),
    { wrapper },
  );

  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(mocks.mint).toHaveBeenCalledTimes(1);
  expect(Socket.instances).toHaveLength(0);
  expect(result.current.connectionState).toBe("disconnected");

  // The mint's 404 is replica-local: it arms the backoff and the lane mints
  // again rather than concluding the session ended.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(500);
  });
  expect(mocks.mint).toHaveBeenCalledTimes(2);

  // A later mint on the same 404 is still not terminal while the lane has
  // retries left.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(5_000);
  });
  expect(mocks.mint.mock.calls.length).toBeGreaterThan(2);
  expect(result.current.connectionState).toBe("disconnected");
});

it("reports session gone after the bounded mint 404s and re-arms on a recovery revision", async () => {
  vi.useFakeTimers();
  mocks.mint.mockRejectedValue(notFoundProblem());
  const onSessionGone = vi.fn();
  const { result, rerender } = renderHook(
    ({ revision }: { revision: number }) =>
      usePlaybackRealtime({
        sessionId: "session",
        onCommand: vi.fn(),
        recoveryRevision: revision,
        onSessionGone,
      }),
    { wrapper, initialProps: { revision: 0 } },
  );

  // A short backoff ladder, then the lane concludes the session is gone and
  // hands the player its restart action. It never keeps minting forever.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(1_600);
  });
  expect(mocks.mint).toHaveBeenCalledTimes(SESSION_GONE_MINT_RETRY_LIMIT);
  expect(onSessionGone).toHaveBeenCalledTimes(1);
  expect(result.current.connectionState).toBe("disconnected");

  // No further mint and no reconnect timer left armed.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(60_000);
  });
  expect(mocks.mint).toHaveBeenCalledTimes(SESSION_GONE_MINT_RETRY_LIMIT);
  expect(onSessionGone).toHaveBeenCalledTimes(1);
  expect(vi.getTimerCount()).toBe(0);

  // An adopted plan (a recovery under the same session id) re-arms the lane; a
  // second loss reports it again.
  rerender({ revision: 1 });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(mocks.mint).toHaveBeenCalledTimes(SESSION_GONE_MINT_RETRY_LIMIT + 1);
  await act(async () => {
    await vi.advanceTimersByTimeAsync(60_000);
  });
  expect(mocks.mint).toHaveBeenCalledTimes(SESSION_GONE_MINT_RETRY_LIMIT * 2);
  expect(onSessionGone).toHaveBeenCalledTimes(2);
  expect(result.current.connectionState).toBe("disconnected");
  expect(vi.getTimerCount()).toBe(0);
});

it("ignores a gone answer for a session the player replaced before it landed", async () => {
  vi.useFakeTimers();
  const pending = new Map<string, { reject: (error: unknown) => void }>();
  mocks.mint.mockImplementation((sessionId: string) => {
    let reject!: (error: unknown) => void;
    const promise = new Promise<never>((_resolve, rej) => {
      reject = rej;
    });
    pending.set(sessionId, { reject });
    return promise;
  });
  const { rerender } = renderHook(
    ({ id }: { id: string }) => usePlaybackRealtime({ sessionId: id, onCommand: vi.fn() }),
    { wrapper, initialProps: { id: "session-a" } },
  );
  await act(async () => {
    await Promise.resolve();
  });
  expect(mocks.mint).toHaveBeenNthCalledWith(1, "session-a", "install", authority);

  // The player moves on to a new session; session A's loop is disposed.
  rerender({ id: "session-b" });
  await act(async () => {
    await Promise.resolve();
  });
  expect(mocks.mint).toHaveBeenCalledTimes(2);

  // Session A answers 404 only after B replaced it. It belongs to the old id
  // and must not schedule a retry behind B, nor mint for A again.
  await act(async () => {
    pending.get("session-a")?.reject(notFoundProblem());
    await Promise.resolve();
    await Promise.resolve();
  });

  expect(mocks.mint).toHaveBeenCalledTimes(2);
  expect(Socket.instances).toHaveLength(0);
});

it.each(["stop", "terminate", "server_restarting"])(
  "delivers an explicit %s only for the captured session and acknowledges it once",
  async (name) => {
    const onCommand = vi.fn().mockResolvedValue(undefined);
    renderHook(() => usePlaybackRealtime({ sessionId: "session", onCommand }), { wrapper });
    await waitFor(() => expect(Socket.instances).toHaveLength(1));
    const socket = Socket.instances[0]!;
    socket.readyState = Socket.OPEN;
    act(() => socket.dispatchEvent(new Event("open")));
    socket.send.mockClear();
    const command = {
      type: "command",
      session_id: "session",
      command_id: "terminal",
      name,
      payload: name === "server_restarting" ? { message: "Restarting soon" } : {},
    };
    const receive = (value: typeof command) =>
      socket.dispatchEvent(new MessageEvent("message", { data: JSON.stringify(value) }));

    await act(async () => {
      receive({ ...command, session_id: "another-session" });
    });
    expect(onCommand).not.toHaveBeenCalled();
    expect(socket.send).not.toHaveBeenCalled();

    await act(async () => {
      receive(command);
      receive(command);
    });
    expect(onCommand).toHaveBeenCalledExactlyOnceWith(expect.objectContaining(command));
    expect(socket.send.mock.calls.map(([data]) => JSON.parse(data))).toEqual([
      { type: "ack", session_id: "session", command_id: "terminal", status: "accepted" },
      { type: "result", session_id: "session", command_id: "terminal", status: "completed" },
    ]);

    mocks.current.mockReturnValue(false);
    await act(async () => receive({ ...command, command_id: "late-terminal" }));
    expect(onCommand).toHaveBeenCalledTimes(1);
  },
);
it("mints without an installation for a bridge-started session", async () => {
  mocks.installation.mockReturnValue(undefined);
  renderHook(() => usePlaybackRealtime({ sessionId: "session", onCommand: vi.fn() }), { wrapper });
  await waitFor(() => expect(Socket.instances).toHaveLength(1));
  expect(mocks.mint).toHaveBeenCalledWith("session", undefined, authority);
});
it("does not mint without profile authority", () => {
  mocks.capture.mockReturnValue(null);
  renderHook(() => usePlaybackRealtime({ sessionId: "session", onCommand: vi.fn() }), { wrapper });
  expect(mocks.mint).not.toHaveBeenCalled();
  expect(Socket.instances).toHaveLength(0);
});
it("does not reconnect an old session under a replacement account", async () => {
  vi.useFakeTimers();
  renderHook(() => usePlaybackRealtime({ sessionId: "session", onCommand: vi.fn() }), { wrapper });
  await act(async () => {
    await Promise.resolve();
  });
  expect(Socket.instances).toHaveLength(1);
  mocks.current.mockReturnValue(false);
  mocks.capture.mockReturnValue({ profileId: "replacement" });
  await act(async () => {
    Socket.instances[0]!.close();
    await vi.advanceTimersByTimeAsync(600);
  });
  expect(mocks.mint).toHaveBeenCalledTimes(1);
  expect(Socket.instances).toHaveLength(1);
});

it("stops reconnecting after an administrator terminate but not after a stop", async () => {
  vi.useFakeTimers();
  for (const name of ["stop", "terminate"]) {
    Socket.instances = [];
    mocks.mint.mockClear();
    const { unmount } = renderHook(
      () => usePlaybackRealtime({ sessionId: "session", onCommand: vi.fn() }),
      { wrapper },
    );
    await act(async () => {
      await Promise.resolve();
    });
    const socket = Socket.instances[0]!;
    socket.readyState = Socket.OPEN;
    act(() => socket.dispatchEvent(new Event("open")));
    await act(async () =>
      socket.dispatchEvent(
        new MessageEvent("message", {
          data: JSON.stringify({
            type: "command",
            session_id: "session",
            command_id: name,
            name,
            issued_by: { kind: "admin" },
          }),
        }),
      ),
    );
    await act(async () => {
      socket.close();
      await vi.advanceTimersByTimeAsync(600);
    });
    expect(mocks.mint).toHaveBeenCalledTimes(name === "terminate" ? 1 : 2);
    unmount();
  }
});
