// @vitest-environment node

import { describe, expect, it } from "vitest";
import { parseVTT } from "./parseVTT";

describe("parseVTT", () => {
  it("parses standard cues with HH:MM:SS.mmm timestamps", () => {
    const vtt = `WEBVTT
Kind: captions
Language: en

1
00:00:01.000 --> 00:00:04.000
Hello, world!

2
00:00:05.000 --> 00:00:08.000
Second subtitle line.
`;
    const cues = parseVTT(vtt);
    expect(cues).toHaveLength(2);
    expect(cues[0]!).toEqual({ start: 1, end: 4, text: "Hello, world!" });
    expect(cues[1]!).toEqual({ start: 5, end: 8, text: "Second subtitle line." });
  });

  it("parses MM:SS.mmm timestamps", () => {
    const vtt = `WEBVTT

01:23.456 --> 02:34.789
Short format timestamp.
`;
    const cues = parseVTT(vtt);
    expect(cues).toHaveLength(1);
    expect(cues[0]!.start).toBeCloseTo(83.456, 3);
    expect(cues[0]!.end).toBeCloseTo(154.789, 3);
  });

  it("handles multi-line cue text", () => {
    const vtt = `WEBVTT

00:00:10.000 --> 00:00:15.000
Line one
Line two
Line three
`;
    const cues = parseVTT(vtt);
    expect(cues).toHaveLength(1);
    expect(cues[0]!.text).toBe("Line one\nLine two\nLine three");
  });

  it("returns empty array for empty input", () => {
    expect(parseVTT("")).toEqual([]);
    expect(parseVTT("WEBVTT\n\n")).toEqual([]);
  });

  it("handles Windows-style line endings", () => {
    const vtt = "WEBVTT\r\n\r\n00:00:01.000 --> 00:00:02.000\r\nHello!\r\n";
    const cues = parseVTT(vtt);
    expect(cues).toHaveLength(1);
    expect(cues[0]!.text).toBe("Hello!");
  });

  it("decodes recognized inline tags but keeps literal angle brackets", () => {
    const vtt = `WEBVTT

00:00:01.000 --> 00:00:04.000
<b>Bold</b> and <i>italic</i> <c.yellow>class</c> <v Roger>speaker</v>
Fish <3 U > 2 hearts
`;
    const cues = parseVTT(vtt);
    expect(cues).toHaveLength(1);
    expect(cues[0]!.text).toBe("Bold and italic class speaker\nFish <3 U > 2 hearts");
  });

  it("strips SRT font markup copied into the WebVTT stream", () => {
    const vtt = `WEBVTT

00:00:01.000 --> 00:00:04.000
<font color="#ffffff">Colored line</font>
`;
    const cues = parseVTT(vtt);
    expect(cues).toHaveLength(1);
    expect(cues[0]!.text).toBe("Colored line");
  });

  it("decodes named, numeric, and hex character entities", () => {
    const vtt = `WEBVTT

00:00:01.000 --> 00:00:04.000
Tom &amp; Jerry &lt;3 &gt; 2 &#39;quotes&#39; &#x27;hex&#x27;&nbsp;end
Escaped &lt;b&gt;markup&lt;/b&gt; stays literal
`;
    const cues = parseVTT(vtt);
    expect(cues).toHaveLength(1);
    expect(cues[0]!.text).toBe(
      "Tom & Jerry <3 > 2 'quotes' 'hex'\u00a0end\nEscaped <b>markup</b> stays literal",
    );
  });

  it("maps cue timing-line placement settings", () => {
    const vtt = `WEBVTT

00:00:01.000 --> 00:00:04.000 align:left line:0
Top left.

00:00:05.000 --> 00:00:08.000 align:end line:50% position:80% size:40% vertical:rl
Middle right.
`;
    const cues = parseVTT(vtt);
    expect(cues[0]).toEqual({
      start: 1,
      end: 4,
      text: "Top left.",
      align: "left",
      line: "top",
    });
    expect(cues[1]).toEqual({
      start: 5,
      end: 8,
      text: "Middle right.",
      align: "right",
      line: "middle",
      position: 80,
      size: 40,
      vertical: "rl",
    });
  });

  it("maps start/end align and bottom line percentages", () => {
    const vtt = `WEBVTT

00:00:01.000 --> 00:00:04.000 align:start line:100%
Bottom left.

00:00:05.000 --> 00:00:08.000 align:center line:-1
Last line.
`;
    const cues = parseVTT(vtt);
    expect(cues[0]!.align).toBe("left");
    expect(cues[0]!.line).toBe("bottom");
    expect(cues[1]!.align).toBe("center");
    expect(cues[1]!.line).toBe("bottom");
  });
});
