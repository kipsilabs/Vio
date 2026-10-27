// @vitest-environment jsdom

import { afterEach, describe, expect, it } from "vitest";

import { readRequestedFileId, rememberRequestedFileId } from "./requestedVersion";

const PROFILE = "profile-a";

afterEach(() => {
  localStorage.clear();
});

describe("requested version store", () => {
  it("remembers a requested row per item", () => {
    rememberRequestedFileId("movie-1", 12, PROFILE);

    expect(readRequestedFileId("movie-1", PROFILE)).toBe(12);
    expect(readRequestedFileId("movie-2", PROFILE)).toBeNull();
  });

  it("keeps the newest pick for an item", () => {
    rememberRequestedFileId("movie-1", 12, PROFILE);
    rememberRequestedFileId("movie-1", 34, PROFILE);

    expect(readRequestedFileId("movie-1", PROFILE)).toBe(34);
  });

  it("ignores empty or unusable rows", () => {
    rememberRequestedFileId("movie-1", null, PROFILE);
    rememberRequestedFileId("movie-1", undefined, PROFILE);
    rememberRequestedFileId("movie-1", 0, PROFILE);
    rememberRequestedFileId("movie-1", -3, PROFILE);
    rememberRequestedFileId("", 7, PROFILE);

    expect(readRequestedFileId("movie-1", PROFILE)).toBeNull();
    expect(readRequestedFileId("", PROFILE)).toBeNull();
  });

  it("survives an unreadable stored document", () => {
    localStorage.setItem("silo-requested-version", "{not json");

    expect(readRequestedFileId("movie-1", PROFILE)).toBeNull();
    rememberRequestedFileId("movie-1", 9, PROFILE);
    expect(readRequestedFileId("movie-1", PROFILE)).toBe(9);
  });

  it("drops malformed entries instead of reading them back", () => {
    localStorage.setItem(
      "silo-requested-version",
      JSON.stringify({
        "profile-a:movie-1": { fileId: "12" },
        "profile-a:movie-2": { fileId: 5 },
      }),
    );

    expect(readRequestedFileId("movie-1", PROFILE)).toBeNull();
    expect(readRequestedFileId("movie-2", PROFILE)).toBe(5);
  });

  it("keeps one profile's pick out of another profile's entries", () => {
    rememberRequestedFileId("movie-1", 12, "profile-a");
    expect(readRequestedFileId("movie-1", "profile-a")).toBe(12);

    expect(readRequestedFileId("movie-1", "profile-b")).toBeNull();
    rememberRequestedFileId("movie-1", 34, "profile-b");
    expect(readRequestedFileId("movie-1", "profile-b")).toBe(34);
    expect(readRequestedFileId("movie-1", "profile-a")).toBe(12);
  });

  // A read can race the profile load on a hard refresh. Until the profile
  // resolves there is no profile-scoped key, so the store must not fall back to
  // a shared bucket that another profile's pick could occupy.
  it("defers reads and writes until the profile resolves", () => {
    localStorage.setItem(
      "silo-requested-version",
      JSON.stringify({ "device:movie-1": { fileId: 3, updatedAt: 1 } }),
    );

    expect(readRequestedFileId("movie-1", null)).toBeNull();
    rememberRequestedFileId("movie-1", 7, null);

    expect(readRequestedFileId("movie-1", "profile-a")).toBeNull();
    const stored = JSON.parse(localStorage.getItem("silo-requested-version") ?? "{}") as Record<
      string,
      unknown
    >;
    expect(Object.keys(stored)).toEqual(["device:movie-1"]);
  });

  it("bounds the map so a long-lived browser cannot grow it forever", () => {
    for (let index = 0; index < 260; index += 1) {
      rememberRequestedFileId(`movie-${index}`, index + 1, PROFILE);
    }

    const stored = JSON.parse(localStorage.getItem("silo-requested-version") ?? "{}") as Record<
      string,
      unknown
    >;
    expect(Object.keys(stored).length).toBeLessThanOrEqual(200);
    // The newest pick always survives the trim.
    expect(readRequestedFileId("movie-259", PROFILE)).toBe(260);
  });
});
