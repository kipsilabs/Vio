# Web PGS subtitles

The web player draws embedded PGS (Blu-ray bitmap) subtitles itself instead of
asking the server to burn them into a transcode. Paths are
repository-relative.

## Contract

The web player declares `embedded_bitmap: true` on every delivery class
(`web/src/player/client-context-v3.ts`). The server then keeps the plan's
delivery and publishes the embedded PGS track as a lossless `.sup` sidecar
(`subtitle.mode: "render"`), the same way it publishes embedded text tracks
(see [playback-protocol-v3.md](playback-protocol-v3.md) §8).
`embedded_bitmap` covers embedded PGS only. The server still burns in embedded
DVD (VobSub) and DVB bitmaps, and refuses external bitmap files
(`subtitle_burn_in_source_unsupported`). `sidecar_bitmap` stays `false`.

Auto-select and the initial start ask the same question through
`subtitleNeedsBurnIn` (`web/src/player/utils/subtitleCodecs.ts`): embedded
PGS does not need the server, so it is not ranked as a burn-in and a refused
start is not blamed on it.

## Rendering

- `web/src/player/utils/pgs.ts` parses the `.sup` stream: segments, palettes
  (YCbCr to RGBA, BT.709 for HD and BT.601 for SD), run-length objects,
  windows, object cropping and composition states. It parses incrementally
  and looks up the display set active at a media time.
- Palettes and objects persist through an epoch, so a display set can place
  objects an earlier one defined. A display set whose objects or palette the
  parser never saw is dropped, never treated as a clear.
- `web/src/player/hooks/usePGSSubtitles.ts` fetches positioned windows
  (`windowed=1&position=&duration=`) through the shared window fetcher
  (`web/src/player/utils/subtitleWindows.ts`), so seeks far into a file do not
  download the whole track. A fresh window after a seek or track change asks
  for 30 seconds, enough for first paint; extends ask for 120 seconds. The
  fetcher starts the next extend 30 seconds before the current one ends and
  seeks 30 seconds back on a fresh window, leaving room for a display set that
  was already on screen. An extend keeps the parser's epoch state; a fresh
  window starts a new parser.
- A screen stays up until the next display set replaces it, but never past
  what has loaded: the end of the last finished window or, beyond it, the
  latest segment read from the window on the wire. Screens more than one window
  behind the playhead are dropped.
- A screen whose epoch began before the fetched window cannot be reconstructed
  from that window. The 30-second seek backoff covers ordinary cases, but a
  display set that precedes the window by more than that is not shown until
  the next epoch or acquisition point. This is a property of the PGS stream:
  the window must contain the palette and object definitions, not just the
  placement.
- Each composition is drawn on a canvas that spans the player, scaled from the
  PGS video size onto the picture rectangle (`web/src/player/utils/videoFit.ts`)
  for both contain and cover fits. The plane matches the picture along its
  uncropped side, so subtitles authored in a letterbox or pillarbox stay
  there. Objects stay inside the player, shrinking when they do not fit (a
  Fill crop, or a tall object over a short player), and lower-half objects
  stay above the control bar while it is up, whatever the text position
  setting says.
- The subtitle delay and the stream's timeline origin apply as they do for text
  cues.
- A plan whose subtitle mode is still `burn_in` is left to the server, and the
  overlay stays off so the subtitle is not drawn twice.

## Failure and recovery

- A `409 subtitle_source_changed` means the virtual release rotated and the
  plan's subtitle URLs are stale. The fetcher stops that URL without spending a
  retry, the player refreshes the subtitle inventory, and the refreshed
  generation rebuilds the parser and window. If the refresh re-mints the same
  URL, the refresh revision still forces one retry; a repeated conflict ends in
  the visible error state rather than an endless "loading" overlay.
- A `503 subtitle_source_unavailable` carries `Retry-After`. The client waits at
  least that long, then uses its normal capped backoff. Four consecutive
  non-retryable failures or twelve retryable failures end the fetch for that
  mount; a seek, track change, replay, or manual retry starts a new window.
- On the server, windowed `.sup` extraction uses the same last-good window
  cache, in-flight coalescing, and per-identity extraction cooldown as text
  windows. A repeated or concurrent window does not spawn a second ffmpeg, and
  an upstream failure can answer 503 without reopening the source. Full-track
  `.sup` caching and Range/HEAD behavior are unchanged.

## Jellyfin compatibility

The Jellyfin surface burns embedded PGS into the transcode and does not publish
a `.sup` sidecar. That is deliberate parity with Jellyfin clients: they do not
render web PGS. The web player uses the sidecar path described above.

## Tests

`web/src/player/testdata/pgs/dam-cav.sup` is a real 1280x720 PGS stream with a
display set at 0 seconds and a clear at 60 seconds. Parser tests and the
pipeline test use it, so the renderer is exercised against real segments rather
than only hand-built ones.
