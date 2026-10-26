// @vitest-environment jsdom

import { afterEach, describe, expect, it } from "vitest";

import { readRequestedFileId, rememberRequestedFileId } from "./requestedVersion";

afterEach(() => {
  localStorage.clear();
});

describe("requested version store", () => {
  it("remembers a requested row per item", () => {
    rememberRequestedFileId("movie-1", 12);

    expect(readRequestedFileId("movie-1")).toBe(12);
    expect(readRequestedFileId("movie-2")).toBeNull();
  });

  it("keeps the newest pick for an item", () => {
    rememberRequestedFileId("movie-1", 12);
    rememberRequestedFileId("movie-1", 34);

    expect(readRequestedFileId("movie-1")).toBe(34);
  });

  it("ignores empty or unusable rows", () => {
    rememberRequestedFileId("movie-1", null);
    rememberRequestedFileId("movie-1", undefined);
    rememberRequestedFileId("movie-1", 0);
    rememberRequestedFileId("movie-1", -3);
    rememberRequestedFileId("", 7);

    expect(readRequestedFileId("movie-1")).toBeNull();
    expect(readRequestedFileId("")).toBeNull();
  });

  it("survives an unreadable stored document", () => {
    localStorage.setItem("silo-requested-version", "{not json");

    expect(readRequestedFileId("movie-1")).toBeNull();
    rememberRequestedFileId("movie-1", 9);
    expect(readRequestedFileId("movie-1")).toBe(9);
  });

  it("drops malformed entries instead of reading them back", () => {
    localStorage.setItem(
      "silo-requested-version",
      JSON.stringify({ "device:movie-1": { fileId: "12" }, "device:movie-2": { fileId: 5 } }),
    );

    expect(readRequestedFileId("movie-1")).toBeNull();
    expect(readRequestedFileId("movie-2")).toBe(5);
  });

  it("scopes the pick to the active profile", () => {
    localStorage.setItem("profile_id", "profile-a");
    rememberRequestedFileId("movie-1", 12);
    expect(readRequestedFileId("movie-1")).toBe(12);

    localStorage.setItem("profile_id", "profile-b");
    expect(readRequestedFileId("movie-1")).toBeNull();

    localStorage.setItem("profile_id", "profile-a");
    expect(readRequestedFileId("movie-1")).toBe(12);
  });

  it("bounds the map so a long-lived browser cannot grow it forever", () => {
    for (let index = 0; index < 260; index += 1) {
      rememberRequestedFileId(`movie-${index}`, index + 1);
    }

    const stored = JSON.parse(localStorage.getItem("silo-requested-version") ?? "{}") as Record<
      string,
      unknown
    >;
    expect(Object.keys(stored).length).toBeLessThanOrEqual(200);
    // The newest pick always survives the trim.
    expect(readRequestedFileId("movie-259")).toBe(260);
  });
});
