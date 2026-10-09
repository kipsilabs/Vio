import { describe, expect, it } from "vitest";

import {
  describePlanTerminal,
  describePlaybackTransportError,
  isDeadPlaybackSessionError,
} from "./playback-errors";
import { PlayerFetchError } from "./player-fetch";

describe("describePlanTerminal", () => {
  it("describes disabled video transcoding", () => {
    expect(
      describePlanTerminal({
        reason: "transcoding_disabled",
        message: "The selected server adaptation is disabled for this user.",
        retryable: false,
      }),
    ).toEqual({
      title: "Transcoding is disabled",
      message: "Transcoding is disabled for your user. Ask your server administrator for access.",
      reason: "transcoding_disabled",
      retryable: false,
    });
  });

  it("tells an unreadable file apart from one that has not been scanned", () => {
    const unreadable = describePlanTerminal({
      reason: "source_unreadable",
      message: "The source file could not be read; it appears to be empty or damaged.",
      retryable: false,
    });
    expect(unreadable.title).toBe("This file can't be played");
    expect(unreadable.message).toContain("empty or damaged");
    expect(unreadable.message).not.toMatch(/scan finishes/);

    expect(
      describePlanTerminal({
        reason: "source_metadata_incomplete",
        message: "The source is missing video metadata required for a validated playback route.",
        retryable: true,
      }).title,
    ).toBe("This file hasn't finished scanning");
  });

  it("describes disabled audio transcoding", () => {
    expect(
      describePlanTerminal({
        reason: "audio_transcoding_disabled",
        message: "The selected server adaptation is disabled for this user.",
        retryable: false,
      }),
    ).toEqual({
      title: "Audio transcoding is disabled",
      message:
        "This item requires audio conversion, but audio transcoding is disabled for your user.",
      reason: "audio_transcoding_disabled",
      retryable: false,
    });
  });

  it("keeps the server's explanation of why a subtitle cannot be used", () => {
    expect(
      describePlanTerminal({
        reason: "subtitle_conversion_unsupported",
        message:
          "The selected subtitle must be burned into the video, but 4K transcoding is disabled.",
        retryable: false,
      }),
    ).toEqual({
      title: "That subtitle track can't be used",
      message:
        "The selected subtitle must be burned into the video, but 4K transcoding is disabled.",
      reason: "subtitle_conversion_unsupported",
      retryable: false,
    });
  });

  it("falls back to a generic subtitle sentence when the server sends no message", () => {
    expect(
      describePlanTerminal({
        reason: "subtitle_codec_unsupported",
        message: "  ",
        retryable: false,
      }),
    ).toEqual({
      title: "That subtitle track can't be used",
      message:
        "Vio couldn't prepare the selected subtitles for this device. Try a different track.",
      reason: "subtitle_codec_unsupported",
      retryable: false,
    });
  });

  it("keeps the server's reason for refusing every version of an item", () => {
    expect(
      describePlanTerminal({
        reason: "no_alternate_version",
        message: "A lower-resolution source is required because 4K transcoding is disabled.",
        retryable: false,
      }),
    ).toEqual({
      title: "No playable version found",
      message: "A lower-resolution source is required because 4K transcoding is disabled.",
      reason: "no_alternate_version",
      retryable: false,
    });
  });

  it("falls back to a generic sentence when no alternate version carries no message", () => {
    expect(
      describePlanTerminal({ reason: "no_alternate_version", message: "   ", retryable: false }),
    ).toEqual({
      title: "No playable version found",
      message:
        "Vio couldn't find a way to play this file on this device. Try another version if one is available.",
      reason: "no_alternate_version",
      retryable: false,
    });
  });

  it("keeps the generic sentence for an exhausted adaptation search", () => {
    expect(
      describePlanTerminal({
        reason: "adaptation_exhausted",
        message: "All compatible playback recipes have already failed for this output route.",
        retryable: false,
      }),
    ).toEqual({
      title: "No playable version found",
      message:
        "Vio couldn't find a way to play this file on this device. Try another version if one is available.",
      reason: "adaptation_exhausted",
      retryable: false,
    });
  });

  it("keeps the server's explanation of a failed conversion start", () => {
    expect(
      describePlanTerminal({
        reason: "transcode_start_failed",
        message: "Failed to start the playback transport.",
        retryable: true,
      }),
    ).toEqual({
      title: "Playback unavailable",
      message: "Failed to start the playback transport.",
      reason: "transcode_start_failed",
      retryable: true,
    });
  });

  it("falls back to a generic sentence when a conversion failure carries no message", () => {
    expect(
      describePlanTerminal({
        reason: "transcode_node_unavailable",
        message: " ",
        retryable: true,
      }),
    ).toEqual({
      title: "Playback unavailable",
      message: "The server couldn't start converting this file. Please try again.",
      reason: "transcode_node_unavailable",
      retryable: true,
    });
  });

  it("exposes the retry verdict of a virtual source that could not be resolved", () => {
    expect(
      describePlanTerminal({
        reason: "virtual_source_unavailable",
        message: "The virtual source could not be resolved for playback.",
        retryable: true,
      }),
    ).toEqual({
      title: "Playback unavailable",
      message: "The virtual source could not be resolved for playback.",
      reason: "virtual_source_unavailable",
      retryable: true,
    });
  });

  it("falls back to a generic sentence when a virtual source failure carries no message", () => {
    expect(
      describePlanTerminal({
        reason: "virtual_source_unavailable",
        message: "  ",
        retryable: true,
      }),
    ).toEqual({
      title: "Playback unavailable",
      message: "The virtual source could not be resolved for playback.",
      reason: "virtual_source_unavailable",
      retryable: true,
    });
  });

  it("falls back to the server's own message for reasons it does not name", () => {
    expect(
      describePlanTerminal({
        reason: "some_future_reason",
        message: "A newer server explained this precisely.",
        retryable: false,
      }),
    ).toEqual({
      title: "Playback unavailable",
      message: "A newer server explained this precisely.",
      reason: "some_future_reason",
      retryable: false,
    });
  });

  it("still says something useful when the server sends no message", () => {
    expect(
      describePlanTerminal({ reason: "some_future_reason", message: "", retryable: false }),
    ).toEqual({
      title: "Playback unavailable",
      message: "Vio could not start playback.",
      reason: "some_future_reason",
      retryable: false,
    });
  });
});

describe("describePlaybackTransportError", () => {
  it("renders 426 as an update-required state", () => {
    const error = new PlayerFetchError(426, "Client upgrade required", "client_upgrade_required");

    expect(describePlaybackTransportError(error)).toEqual({
      title: "Update required",
      message:
        "This server speaks a newer playback protocol than this app. Reload the page to pick up the current version.",
    });
  });

  it("ignores non-fetch errors", () => {
    expect(describePlaybackTransportError(new Error("boom"))).toBeNull();
  });

  it("distinguishes an expired playback session from a missing item", () => {
    expect(
      describePlaybackTransportError(
        new PlayerFetchError(404, "Playback session not found", "playback_session_not_found"),
      ),
    ).toEqual({
      title: "Playback session expired",
      message: "This playback session is no longer active. Start it again to keep watching.",
    });
  });

  it("describes 401 authentication errors distinctly from 403 permissions errors", () => {
    expect(describePlaybackTransportError(new PlayerFetchError(401, "Unauthorized"))).toEqual({
      title: "Authentication required",
      message: "Your sign-in session has expired. Sign in again to continue playback.",
    });

    expect(describePlaybackTransportError(new PlayerFetchError(403, "Forbidden"))).toEqual({
      title: "Playback unavailable",
      message: "You do not have permission to play this item.",
    });
  });

  it("does not call a failed change to a playing stream a failure to start", () => {
    const error = new PlayerFetchError(502, "Bad Gateway");

    expect(describePlaybackTransportError(error)?.message).toBe(
      "Vio could not start playback right now. Please try again.",
    );
    expect(describePlaybackTransportError(error, "update")?.message).toBe(
      "Vio could not update playback right now. Please try again.",
    );
  });

  it("ignores 4xx statuses it has nothing specific to say about", () => {
    expect(
      describePlaybackTransportError(new PlayerFetchError(409, "Conflict", "stale_playback_plan")),
    ).toBeNull();
  });
});

describe("isDeadPlaybackSessionError", () => {
  it("names the errors a fresh start can repair", () => {
    expect(
      isDeadPlaybackSessionError(
        new PlayerFetchError(404, "Playback session not found", "playback_session_not_found"),
      ),
    ).toBe(true);
    expect(
      isDeadPlaybackSessionError(
        new PlayerFetchError(410, "Playback session has ended", "playback_session_ended"),
      ),
    ).toBe(true);
    // A 401 has already survived the one token-refresh retry in player-fetch.
    expect(isDeadPlaybackSessionError(new PlayerFetchError(401, "Unauthorized"))).toBe(true);
  });

  it("keeps a missing source file and non-fetch failures out", () => {
    expect(
      isDeadPlaybackSessionError(
        new PlayerFetchError(404, "Source media file is missing", "not_found"),
      ),
    ).toBe(false);
    expect(isDeadPlaybackSessionError(new PlayerFetchError(500, "Server error"))).toBe(false);
    expect(isDeadPlaybackSessionError(new Error("boom"))).toBe(false);
  });
});
