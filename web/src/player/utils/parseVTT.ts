export type SubtitleCueAlign = "left" | "center" | "right";
export type SubtitleCueLine = "top" | "middle" | "bottom";

export interface ParsedCue {
  start: number;
  end: number;
  text: string;
  /** Horizontal placement from the cue timing line, normalized for the overlay. */
  align?: SubtitleCueAlign;
  /** Vertical placement from the cue timing line, normalized for the overlay. */
  line?: SubtitleCueLine;
  /** Cue box position as a percentage of the video width, when the cue sets it. */
  position?: number;
  /** Cue box width as a percentage of the video width, when the cue sets it. */
  size?: number;
  /** Vertical writing mode, when the cue sets one. */
  vertical?: "rl" | "lr";
}

/**
 * Parse a timestamp string (HH:MM:SS.mmm or MM:SS.mmm) into seconds.
 */
function parseTimestamp(ts: string): number {
  const parts = ts.trim().split(":");
  if (parts.length === 3) {
    return parseInt(parts[0]!, 10) * 3600 + parseInt(parts[1]!, 10) * 60 + parseFloat(parts[2]!);
  }
  if (parts.length === 2) {
    return parseInt(parts[0]!, 10) * 60 + parseFloat(parts[1]!);
  }
  return 0;
}

// Captures the cue timings and the settings that follow them on the same line.
const TIMESTAMP_RE = /^([\d:.]+)\s*-->\s*([\d:.]+)(.*)$/;

// WebVTT's recognized inline tags. Only these are markup: literal text such as
// "Fish <3 U > 2 hearts" must survive parsing untouched, which a blanket
// `<[^>]+>` strip cannot guarantee.
const VTT_TAG_RE =
  /<\/?(?:[biu]|font|ruby|rt)(?:\s+[^>]*)?>|<\/?(?:c|v|lang)(?:\.[^.\s>]+)*(?:\s+[^>]*)?>/gi;

const VTT_ENTITY_RE = /&(?:#(\d+)|#x([0-9a-fA-F]+)|(amp|lt|gt|nbsp|quot|apos));/g;

const NAMED_ENTITIES: Record<string, string> = {
  amp: "&",
  lt: "<",
  gt: ">",
  nbsp: "\u00a0",
  quot: '"',
  apos: "'",
};

function decodeEntities(text: string): string {
  if (!text.includes("&")) return text;
  return text.replace(VTT_ENTITY_RE, (match, decimal?: string, hex?: string, named?: string) => {
    if (named) return NAMED_ENTITIES[named] ?? match;
    const codePoint = decimal !== undefined ? parseInt(decimal, 10) : parseInt(hex ?? "", 16);
    if (!Number.isFinite(codePoint) || codePoint < 0 || codePoint > 0x10ffff) return match;
    try {
      return String.fromCodePoint(codePoint);
    } catch {
      return match;
    }
  });
}

/**
 * Remove recognized inline WebVTT markup (b/i/u/c/v/lang/ruby/rt), leaving
 * literal angle-bracketed text such as "Fish <3 U > 2 hearts" alone.
 */
export function stripVTTTags(text: string): string {
  return text.replace(VTT_TAG_RE, "");
}

/**
 * Decode the text payload of a WebVTT cue: remove recognized inline markup and
 * resolve character entities. Markup is stripped before entities are decoded,
 * so `&lt;i&gt;` stays literal text rather than turning into a tag.
 */
export function decodeVTTText(text: string): string {
  return decodeEntities(stripVTTTags(text));
}

function parseAlign(value: string): SubtitleCueAlign | undefined {
  switch (value) {
    case "left":
    case "start":
      return "left";
    case "center":
      return "center";
    case "right":
    case "end":
      return "right";
    default:
      return undefined;
  }
}

/**
 * Map a WebVTT `line:` setting to an overlay placement. Percentages are the
 * common form (`line:0` is the top, `line:50%` the middle, `line:100%` the
 * bottom); bare numbers are line counts, where negative values count from the
 * bottom.
 */
function parseLine(value: string): SubtitleCueLine | undefined {
  if (value.endsWith("%")) {
    const pct = parseFloat(value);
    if (!Number.isFinite(pct)) return undefined;
    if (pct <= 25) return "top";
    if (pct >= 75) return "bottom";
    return "middle";
  }
  const line = parseFloat(value);
  if (!Number.isFinite(line)) return undefined;
  if (line < 0) return "bottom";
  return "top";
}

/** Parse a `position:`/`size:` percentage (the trailing `%` is optional). */
function parsePercentage(value: string): number | undefined {
  const pct = parseFloat(value.endsWith("%") ? value.slice(0, -1) : value);
  if (!Number.isFinite(pct) || pct < 0 || pct > 100) return undefined;
  return pct;
}

function parseCueSettings(
  settings: string,
): Pick<ParsedCue, "align" | "line" | "position" | "size" | "vertical"> {
  const placement: Pick<ParsedCue, "align" | "line" | "position" | "size" | "vertical"> = {};
  if (!settings.trim()) return placement;
  const settingRe = /(?:^|\s)(align|line|position|size|vertical):(\S+)/g;
  for (const match of settings.matchAll(settingRe)) {
    const key = match[1]!;
    const value = match[2]!;
    switch (key) {
      case "align":
        placement.align = parseAlign(value);
        break;
      case "line":
        placement.line = parseLine(value.split(",")[0]!);
        break;
      case "position":
        placement.position = parsePercentage(value.split(",")[0]!);
        break;
      case "size":
        placement.size = parsePercentage(value);
        break;
      case "vertical":
        placement.vertical = value === "rl" || value === "lr" ? value : undefined;
        break;
    }
  }
  // Keep the object limited to fields the cue actually set.
  for (const key of Object.keys(placement) as Array<keyof typeof placement>) {
    if (placement[key] === undefined) delete placement[key];
  }
  return placement;
}

/**
 * Parse WebVTT text into an array of cue objects.
 */
export function parseVTT(vttText: string): ParsedCue[] {
  const cues: ParsedCue[] = [];
  const lines = vttText.split(/\r?\n/);

  let i = 0;

  // Skip the WEBVTT header and any metadata before the first cue.
  while (i < lines.length && !TIMESTAMP_RE.test(lines[i]!)) {
    i++;
  }

  while (i < lines.length) {
    const match = lines[i]!.match(TIMESTAMP_RE);
    if (!match) {
      i++;
      continue;
    }

    const start = parseTimestamp(match[1]!);
    const end = parseTimestamp(match[2]!);
    const placement = parseCueSettings(match[3] ?? "");
    i++;

    // Collect text lines until a blank line or end of input.
    const textLines: string[] = [];
    while (i < lines.length && lines[i]!.trim() !== "") {
      // Skip numeric cue identifiers that precede timestamp lines.
      if (TIMESTAMP_RE.test(lines[i]!)) break;
      textLines.push(lines[i]!);
      i++;
    }

    const text = decodeVTTText(textLines.join("\n").trim());
    if (text) {
      cues.push({ start, end, text, ...placement });
    }
  }

  return cues;
}
