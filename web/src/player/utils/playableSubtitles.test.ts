// @vitest-environment node

import { describe, expect, it } from "vitest";
import type { PlayerSubtitleInfo } from "../types";
import {
  hasSelectableSessionSubtitles,
  isSameSubtitleTrack,
  pendingServerSubtitleSelection,
  resolvePlayableSubtitles,
  subtitleArtifactIdentity,
  subtitleIdentitiesNameSameAsset,
  subtitleSelectionAcknowledgesPending,
  subtitleTrackIdentityFromInfo,
  subtitleTrackIdentityKey,
  type SubtitleTrackIdentity,
} from "./playableSubtitles";

function makeSubtitle(overrides: Partial<PlayerSubtitleInfo> = {}): PlayerSubtitleInfo {
  return {
    index: 0,
    language: "eng",
    label: "English",
    url: "",
    ...overrides,
  };
}

describe("resolvePlayableSubtitles", () => {
  it("prefers playback-session subtitle tracks when they include stream urls", () => {
    const sessionTrack = makeSubtitle({
      index: 2,
      source: "embedded",
      url: "/stream/session/subtitles/2",
    });
    const detailTrack = makeSubtitle({
      index: 0,
      source: "embedded",
      url: "",
    });

    expect(resolvePlayableSubtitles([sessionTrack], [detailTrack])).toEqual([sessionTrack]);
  });

  it("falls back to watch-detail evidence when the plan inventory is empty", () => {
    const detailTrack = makeSubtitle({
      index: 0,
      source: "embedded",
      codec: "hdmv_pgs_subtitle",
      url: "",
    });

    // The plan published no inventory at all; the watch detail is the only
    // evidence, so the menu lists it and the bounded poll repairs the plan.
    expect(resolvePlayableSubtitles([], [detailTrack])).toEqual([detailTrack]);
  });

  it("keeps filtering unplayable fallback rows when the plan published a placeholder", () => {
    const placeholder = makeSubtitle({ index: 0, url: "" });
    const detailTrack = makeSubtitle({ index: 1, source: "embedded", url: "" });

    expect(resolvePlayableSubtitles([placeholder], [detailTrack])).toEqual([]);
  });

  it("keeps burn-in-only session tracks even though they have no url", () => {
    const burnInTrack = makeSubtitle({
      index: 3,
      source: "embedded",
      codec: "hdmv_pgs_subtitle",
      burn_in_only: true,
      url: "",
    });
    const sidecarTrack = makeSubtitle({
      index: 4,
      source: "embedded",
      url: "/stream/session/subtitles/4",
    });

    expect(resolvePlayableSubtitles([burnInTrack, sidecarTrack], [])).toEqual([
      burnInTrack,
      sidecarTrack,
    ]);
  });

  it("keeps fallback tracks that already have playable urls", () => {
    const fallbackTrack = makeSubtitle({
      index: 1,
      source: "downloaded",
      url: "/stream/fallback/subtitles/1",
    });

    expect(resolvePlayableSubtitles([], [fallbackTrack])).toEqual([fallbackTrack]);
  });
});

describe("hasSelectableSessionSubtitles", () => {
  it("is true when a session track carries a stream url", () => {
    expect(hasSelectableSessionSubtitles([makeSubtitle({ url: "/stream/session/0" })])).toBe(true);
  });

  it("is true for a burn-in-only track without a url", () => {
    expect(hasSelectableSessionSubtitles([makeSubtitle({ burn_in_only: true, url: "" })])).toBe(
      true,
    );
  });

  it("is false for a published track that is neither fetchable nor burn-in-only", () => {
    expect(hasSelectableSessionSubtitles([makeSubtitle({ url: "" })])).toBe(false);
  });

  it("is false for an empty inventory", () => {
    expect(hasSelectableSessionSubtitles([])).toBe(false);
  });
});

describe("subtitle track identity", () => {
  function identity(overrides: Partial<SubtitleTrackIdentity> = {}): SubtitleTrackIdentity {
    return { index: 0, ...overrides };
  }

  it("matches two descriptors that name the same server track_id", () => {
    expect(
      isSameSubtitleTrack(
        identity({ index: 3, trackId: "file:7:subtitle:2" }),
        identity({ index: 9, trackId: "file:7:subtitle:2" }),
      ),
    ).toBe(true);
  });

  it("does not match different server track_ids even with equal descriptors", () => {
    expect(
      isSameSubtitleTrack(
        identity({ index: 3, trackId: "file:7:subtitle:2", language: "en", codec: "srt" }),
        identity({ index: 4, trackId: "file:7:subtitle:3", language: "en", codec: "srt" }),
      ),
    ).toBe(false);
  });

  it("falls back to language/codec/forced/hearing-impaired when no track_id is present", () => {
    expect(
      isSameSubtitleTrack(
        identity({ index: 1, language: "English", codec: "SRT", forced: false }),
        identity({ index: 5, language: "english", codec: "srt", forced: false }),
      ),
    ).toBe(true);
    expect(
      isSameSubtitleTrack(
        identity({ index: 1, language: "en", codec: "srt", forced: true }),
        identity({ index: 5, language: "en", codec: "srt", forced: false }),
      ),
    ).toBe(false);
  });

  it("keys the stable identity on track_id when available", () => {
    expect(subtitleTrackIdentityKey(identity({ index: 3, trackId: "file:7:subtitle:2" }))).toBe(
      "id:file:7:subtitle:2",
    );
    expect(subtitleTrackIdentityKey(identity({ index: 3, language: "en", codec: "srt" }))).toBe(
      "desc:en|srt||",
    );
  });

  it("keeps descriptor-identical embedded tracks distinct without a track_id", () => {
    const first = identity({
      index: 1,
      language: "en",
      codec: "srt",
      source: "embedded",
      streamIndex: 0,
    });
    const second = identity({
      index: 2,
      language: "en",
      codec: "srt",
      source: "embedded",
      streamIndex: 1,
    });
    expect(isSameSubtitleTrack(first, second)).toBe(false);
    expect(subtitleTrackIdentityKey(first)).not.toBe(subtitleTrackIdentityKey(second));
    // Selecting one does not settle the other: the second must be requested,
    // not silently dropped as already-satisfied.
    expect(pendingServerSubtitleSelection(first, second)).toBe(2);
  });

  it("distinguishes external sidecars by their artifact key", () => {
    const first = identity({
      index: 0,
      language: "en",
      codec: "srt",
      source: "external",
      artifactId: "aaa",
    });
    const second = identity({
      index: 1,
      language: "en",
      codec: "srt",
      source: "external",
      artifactId: "bbb",
    });
    expect(isSameSubtitleTrack(first, second)).toBe(false);
  });

  it("treats the same artifact at a re-minted ordinal as settled", () => {
    const planSelected = identity({
      index: 7,
      language: "en",
      codec: "srt",
      source: "embedded",
      streamIndex: 2,
    });
    const active = identity({
      index: 2,
      language: "en",
      codec: "srt",
      source: "embedded",
      streamIndex: 2,
    });
    expect(isSameSubtitleTrack(planSelected, active)).toBe(true);
  });

  it("keeps the ordinal of a same-language sibling the inventory still lists", () => {
    const first = identity({
      index: 0,
      trackId: "file:7:subtitle:0",
      language: "eng",
      codec: "pgs",
      burnInOnly: true,
      source: "embedded",
    });
    const second = identity({
      index: 1,
      trackId: "file:7:subtitle:1",
      language: "eng",
      codec: "pgs",
      burnInOnly: true,
      source: "embedded",
    });
    // Both ids are rows of one inventory, so the ordinal is the asset's and
    // not the plan's: two burn-in tracks, not one track re-minted.
    expect(subtitleIdentitiesNameSameAsset(first, second, [first, second])).toBe(false);
    // Only the ordinal moved across a replacement inventory that no longer
    // lists the committed ordinal: the descriptor fallback still settles it.
    expect(subtitleIdentitiesNameSameAsset(first, second, [second])).toBe(true);
  });

  it("settles a pick the plan acknowledged by re-minting another language slot", () => {
    const committedFrench = identity({
      index: 0,
      trackId: "file:7:subtitle:0",
      language: "fra",
      codec: "subrip",
      source: "embedded",
    });
    const pendingFrench = identity({
      index: 1,
      trackId: "file:7:subtitle:1",
      language: "fra",
      codec: "subrip",
      source: "embedded",
    });
    const shiftedEnglish = identity({
      index: 1,
      trackId: "file:7:subtitle:1",
      language: "eng",
      codec: "subrip",
      source: "embedded",
    });
    // The inventory's `:1` is English, so the captured French id no longer
    // names a row of its own: this is the re-mint, not a sibling.
    expect(
      subtitleSelectionAcknowledgesPending(committedFrench, pendingFrench, [
        committedFrench,
        shiftedEnglish,
      ]),
    ).toBe(true);
  });
});

describe("subtitleSelectionAcknowledgesPending", () => {
  function identity(overrides: Partial<SubtitleTrackIdentity> = {}): SubtitleTrackIdentity {
    return { index: 0, ...overrides };
  }

  it("acknowledges a manual pick after the plan re-mints its combined ordinal", () => {
    // Viewer picked French at slot 1; the plan committed the same asset at 0.
    expect(
      subtitleSelectionAcknowledgesPending(
        identity({
          index: 0,
          trackId: "file:7:subtitle:0",
          language: "fra",
          codec: "subrip",
          source: "embedded",
        }),
        identity({
          index: 1,
          trackId: "file:7:subtitle:1",
          language: "fra",
          codec: "subrip",
          source: "embedded",
        }),
      ),
    ).toBe(true);
  });

  it("does not acknowledge a different-language row that inherited the clicked ordinal", () => {
    expect(
      subtitleSelectionAcknowledgesPending(
        identity({
          index: 1,
          trackId: "file:7:subtitle:1",
          language: "eng",
          codec: "subrip",
          source: "embedded",
        }),
        identity({
          index: 1,
          trackId: "file:7:subtitle:1",
          language: "fra",
          codec: "subrip",
          source: "embedded",
        }),
      ),
    ).toBe(false);
  });

  it("does not acknowledge a sibling asset in the same id family", () => {
    expect(
      subtitleSelectionAcknowledgesPending(
        identity({
          index: 0,
          trackId: "file:7:subtitle:0",
          language: "eng",
          codec: "subrip",
          source: "embedded",
          streamIndex: 0,
        }),
        identity({
          index: 1,
          trackId: "file:7:subtitle:1",
          language: "eng",
          codec: "subrip",
          source: "embedded",
          streamIndex: 1,
        }),
      ),
    ).toBe(false);
  });

  it("acknowledges a re-minted sidecar by its artifact identity", () => {
    expect(
      subtitleSelectionAcknowledgesPending(
        identity({
          index: 0,
          language: "fra",
          codec: "subrip",
          source: "external",
          artifactId: "fr-key",
        }),
        identity({
          index: 2,
          language: "fra",
          codec: "subrip",
          source: "external",
          artifactId: "fr-key",
        }),
      ),
    ).toBe(true);
  });

  it("does not acknowledge without a committed plan selection", () => {
    expect(
      subtitleSelectionAcknowledgesPending(
        null,
        identity({ index: 1, trackId: "file:7:subtitle:1", language: "fra" }),
      ),
    ).toBe(false);
  });
});

describe("subtitleArtifactIdentity", () => {
  it("extracts the embedded container stream index", () => {
    expect(
      subtitleArtifactIdentity(
        "/api/v1/stream/s/subtitles/5.ass?file_id=7&embedded_stream_index=2&token=t",
      ),
    ).toEqual({ streamIndex: 2, artifactId: null });
  });

  it("extracts the external sidecar path key", () => {
    expect(
      subtitleArtifactIdentity(
        "/api/v1/stream/s/subtitles/0.vtt?file_id=7&external_subtitle_key=abc",
      ),
    ).toEqual({ streamIndex: null, artifactId: "abc" });
  });

  it("extracts the downloaded row identity", () => {
    expect(
      subtitleArtifactIdentity(
        "/api/v1/stream/s/subtitles/1.vtt?file_id=7&downloaded_subtitle_id=44",
      ),
    ).toEqual({ streamIndex: null, artifactId: "downloaded:44" });
  });

  it("returns no discriminator for an empty or unparseable url", () => {
    expect(subtitleArtifactIdentity("")).toEqual({ streamIndex: null, artifactId: null });
    expect(subtitleArtifactIdentity(null)).toEqual({ streamIndex: null, artifactId: null });
  });
});

describe("subtitleTrackIdentityFromInfo", () => {
  function track(overrides: Partial<PlayerSubtitleInfo>): PlayerSubtitleInfo {
    return makeSubtitle(overrides);
  }

  it("builds collision-safe identities for track_id-less embedded tracks", () => {
    const first = subtitleTrackIdentityFromInfo(
      track({
        index: 1,
        language: "en",
        codec: "srt",
        source: "embedded",
        url: "/api/v1/stream/s/subtitles/1.vtt?file_id=7&embedded_stream_index=0",
      }),
    );
    const second = subtitleTrackIdentityFromInfo(
      track({
        index: 2,
        language: "en",
        codec: "srt",
        source: "embedded",
        url: "/api/v1/stream/s/subtitles/2.vtt?file_id=7&embedded_stream_index=1",
      }),
    );
    expect(isSameSubtitleTrack(first, second)).toBe(false);
    expect(pendingServerSubtitleSelection(first, second)).toBe(2);
  });

  it("prefers track_id over the sidecar artifact", () => {
    const id = subtitleTrackIdentityFromInfo(
      track({
        index: 1,
        track_id: "file:7:subtitle:1",
        language: "en",
        codec: "srt",
        url: "/api/v1/stream/s/subtitles/1.vtt?file_id=7&embedded_stream_index=0",
      }),
    );
    expect(subtitleTrackIdentityKey(id)).toBe("id:file:7:subtitle:1");
  });
});

describe("pendingServerSubtitleSelection", () => {
  function identity(overrides: Partial<SubtitleTrackIdentity> = {}): SubtitleTrackIdentity {
    return { index: 0, ...overrides };
  }

  it("settles an already-selected burn-in plan without another replan", () => {
    expect(
      pendingServerSubtitleSelection(
        identity({ index: 2, trackId: "file:7:subtitle:2", burnInOnly: true }),
        identity({ index: 2, trackId: "file:7:subtitle:2", burnInOnly: true }),
      ),
    ).toBeUndefined();
  });

  it("does not re-request a sidecar artifact selected by a burn-in plan", () => {
    expect(
      pendingServerSubtitleSelection(
        identity({ index: 0, trackId: "file:7:subtitle:0" }),
        identity({ index: 0, trackId: "file:7:subtitle:0" }),
      ),
    ).toBeUndefined();
  });

  it("preserves a sidecar selection while replacing burn-in", () => {
    expect(
      pendingServerSubtitleSelection(
        identity({ index: 2, trackId: "file:7:subtitle:2", burnInOnly: true }),
        identity({ index: 0, trackId: "file:7:subtitle:0" }),
      ),
    ).toBe(0);
  });

  it("turns burn-in off explicitly rather than looping", () => {
    expect(
      pendingServerSubtitleSelection(
        identity({ index: 2, trackId: "file:7:subtitle:2", burnInOnly: true }),
        null,
      ),
    ).toBeNull();
  });

  it("requests a burn-in track from a sidecar plan", () => {
    expect(
      pendingServerSubtitleSelection(
        identity({ index: 0, trackId: "file:7:subtitle:0" }),
        identity({ index: 2, trackId: "file:7:subtitle:2", burnInOnly: true }),
      ),
    ).toBe(2);
  });

  it("persists a newly selected sidecar track when the plan has no selection", () => {
    expect(pendingServerSubtitleSelection(null, identity({ index: 0 }))).toBe(0);
  });

  it("persists a switch between sidecar tracks", () => {
    expect(
      pendingServerSubtitleSelection(
        identity({ index: 0, trackId: "file:7:subtitle:0" }),
        identity({ index: 1, trackId: "file:7:subtitle:1" }),
      ),
    ).toBe(1);
  });

  it("persists turning a sidecar track off", () => {
    expect(
      pendingServerSubtitleSelection(identity({ index: 0, trackId: "file:7:subtitle:0" }), null),
    ).toBeNull();
  });

  it("treats a re-minted ordinal for the same track_id as settled", () => {
    expect(
      pendingServerSubtitleSelection(
        identity({ index: 7, trackId: "file:7:subtitle:2" }),
        identity({ index: 2, trackId: "file:7:subtitle:2" }),
      ),
    ).toBeUndefined();
  });

  it("treats a reordered descriptor match without track_id as settled", () => {
    expect(
      pendingServerSubtitleSelection(
        identity({ index: 7, language: "en", codec: "srt", forced: false }),
        identity({ index: 2, language: "en", codec: "srt", forced: false }),
      ),
    ).toBeUndefined();
  });

  it("does not settle an equal ordinal-derived id that names another language", () => {
    // The menu kept verified rows (English at 0, French at 1) while the
    // replacement plan re-minted the same ids in the other order and selects
    // English at 1. The clicked French row carries `:1` too; the id alone
    // would read the pick as already playing and send nothing.
    const planSelectedEnglish = identity({
      index: 1,
      trackId: "file:7:subtitle:1",
      language: "eng",
      codec: "subrip",
      source: "embedded",
    });
    const displayedFrench = identity({
      index: 1,
      trackId: "file:7:subtitle:1",
      language: "fra",
      codec: "subrip",
      source: "embedded",
    });
    expect(isSameSubtitleTrack(planSelectedEnglish, displayedFrench)).toBe(true);
    expect(pendingServerSubtitleSelection(planSelectedEnglish, displayedFrench)).toBe(1);
  });

  it("requests the other burn-in track when two same-language rows share every descriptor", () => {
    // Two embedded bitmap tracks with the same language, codec and flags: the
    // trailing ordinal is the only difference, and both are real tracks of the
    // inventory the menu lists.
    const first = identity({
      index: 0,
      trackId: "file:7:subtitle:0",
      language: "eng",
      codec: "pgs",
      burnInOnly: true,
      source: "embedded",
    });
    const second = identity({
      index: 1,
      trackId: "file:7:subtitle:1",
      language: "eng",
      codec: "pgs",
      burnInOnly: true,
      source: "embedded",
    });
    const inventory = [first, second];
    // Picking the second while the plan burns in the first must reach the
    // server; reading the two ids as one asset would drop the request and keep
    // the first track burning.
    expect(pendingServerSubtitleSelection(first, second, inventory)).toBe(1);
    expect(pendingServerSubtitleSelection(second, first, inventory)).toBe(0);
  });

  it("settles a burn-in row the inventory re-minted onto another ordinal", () => {
    const committed = identity({
      index: 2,
      trackId: "file:7:subtitle:2",
      language: "eng",
      codec: "pgs",
      burnInOnly: true,
      source: "embedded",
    });
    const reminted = identity({
      index: 3,
      trackId: "file:7:subtitle:3",
      language: "eng",
      codec: "pgs",
      burnInOnly: true,
      source: "embedded",
    });
    // The plan's ordinal is gone from the replacement inventory, so the row is
    // the same asset at a new ordinal and no request is needed.
    expect(pendingServerSubtitleSelection(committed, reminted, [reminted])).toBeUndefined();
  });

  it("offers the selection when the server resolved it to off (caller guard dedupes)", () => {
    expect(
      pendingServerSubtitleSelection(null, identity({ index: 2, trackId: "file:7:subtitle:2" })),
    ).toBe(2);
  });
});
