import { describe, expect, it } from "vitest";
import { isSubtitleSourceChanged } from "./subtitleSourceChanged";

function response(status: number, body: unknown): Response {
  return new Response(typeof body === "string" ? body : JSON.stringify(body), { status });
}

describe("isSubtitleSourceChanged", () => {
  it("is true for a 409 carrying the subtitle_source_changed error code", async () => {
    const resp = response(409, { error: "subtitle_source_changed" });
    expect(await isSubtitleSourceChanged(resp)).toBe(true);
  });

  it("is true for the v2 conflict problem document", async () => {
    const resp = response(409, {
      type: "https://siloserver.org/docs/api/v2/problems/conflict",
      title: "Conflict",
      status: 409,
      detail: "Conflict",
    });
    expect(await isSubtitleSourceChanged(resp)).toBe(true);
  });

  it("is false for a 409 carrying a different error code", async () => {
    const resp = response(409, { error: "something_else" });
    expect(await isSubtitleSourceChanged(resp)).toBe(false);
  });

  it("is false for a non-409 status", async () => {
    const resp = response(500, { error: "subtitle_source_changed" });
    expect(await isSubtitleSourceChanged(resp)).toBe(false);
  });

  it("is false when the body is not JSON", async () => {
    const resp = response(409, "not json");
    expect(await isSubtitleSourceChanged(resp)).toBe(false);
  });
});
