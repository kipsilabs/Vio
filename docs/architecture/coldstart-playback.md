# Cold-start playback for a virgin virtual release

Status: working spec. Amend here when behavior or constraints change; the
code is the source of truth and this doc must track it.

## Scope

A release candidate that was never played nor cached: zero stored
information. AltMount holds it in an incomplete/queue state behind NZB. The
workflow below is the contract for taking that candidate to confident,
seekable playback. It covers audio, video, and subtitles together — a fix
that handles only one track kind is incomplete.

## The workflow

1. **Find the release.** Candidate search resolves the playable stream
   without requiring a completed download.
2. **Play early, from incomplete.** Playback starts once AltMount can serve
   bytes (order of 2–10s into queue state). It never waits for completion.
3. **Temporary true information at start.** The cold plan carries the real
   discovered or declared track inventory — audio and subtitles, zero or
   more — never fabricated entries and never an empty menu masquerading as
   final. Fast discovery runs pre-plan inside a hard budget; declared data
   is the fallback, honestly marked pending.
4. **Correct selection at start, including multi-track.** Default audio and
   subtitle policies resolve against the step-3 inventory, so a multi-audio
   release picks the preferred language and multi-subtitles offer the full
   list. Single-track files must keep working vacuously.
5. **Live deferral of the rich probe.** The full probe verifies past the
   transport commit and publishes; menus and output adopt it in place.
6. **No block, freeze, or spurious replan.** Playback starts fast and stays
   on its route: what plays stays played. Inventory arrival, seeks, and
   track changes never switch rows/files/routes and never restart the
   element.
7. **Seek with full knowledge.** Once verified info is live, fast and random
   seeks use the known stream table directly — no re-discovery loop, modern
   playback behavior with all needed information present.

## Non-goals of this doc

Managed (prepare-then-download) flows, library scanning/indexing, and
client-specific player UI states are covered elsewhere.
