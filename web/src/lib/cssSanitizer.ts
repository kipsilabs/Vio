import { AVAILABLE_FONTS, MAX_CSS_SIZE, THEME_TOKENS, type ThemeVarOverrides } from "./themeTokens";

// Deliberately small language: paint, spacing and shape, not arbitrary CSS.
// Every accepted declaration is reconstructed from parsed tokens. No URLs,
// substitutions, at-rules, strings or unknown functions survive this boundary.
const properties = new Set([
  "color", "background-color", "border-color", "border-radius", "border-width",
  "border-style", "opacity", "padding", "padding-top", "padding-bottom",
  "padding-left", "padding-right", "margin", "margin-top", "margin-bottom",
  "margin-left", "margin-right", "gap", "width", "height", "max-width",
  "min-height", "font-size", "font-weight", "line-height", "letter-spacing",
]);
const words = new Set(["transparent", "currentcolor", "black", "white", "red", "green", "blue", "gray", "grey", "none", "solid", "dashed", "dotted", "auto", "normal", "bold"]);
const functions = new Set(["rgb", "rgba", "hsl", "hsla", "oklch", "oklab"]);
const tokens = new Map(THEME_TOKENS.map((meta) => [meta.token as string, meta.inputType]));

/** Decode CSS escapes before lexing; reject invalid Unicode and escaped newlines. */
function decode(source: string): string {
  let out = "";
  for (let i = 0; i < source.length; i++) {
    const char = source[i];
    if (char !== "\\") { out += char; continue; }
    i++;
    if (i === source.length || /[\n\r\f]/.test(source[i])) throw new Error("Invalid escape");
    let hex = "";
    while (i < source.length && hex.length < 6 && /[0-9a-f]/i.test(source[i])) hex += source[i++];
    if (hex) {
      const point = parseInt(hex, 16);
      if (!point || point > 0x10ffff || (point >= 0xd800 && point <= 0xdfff)) throw new Error("Invalid escape");
      out += String.fromCodePoint(point);
      if (!/\s/.test(source[i] ?? "")) i--;
    } else out += source[i];
  }
  return out;
}

function lex(source: string): string[] {
  const input = decode(source);
  const result: string[] = [];
  let i = 0;
  while (i < input.length) {
    if (/\s/.test(input[i])) { i++; continue; }
    if (input.startsWith("/*", i)) {
      const end = input.indexOf("*/", i + 2);
      if (end < 0) throw new Error("Unclosed comment");
      i = end + 2;
      continue;
    }
    const match = input.slice(i).match(/^(?:--[a-z][a-z0-9-]*|#[a-z0-9_-]+|-?(?:\d*\.\d+|\d+)(?:%|[a-z]+)?|[a-z_][a-z0-9_-]*|[{}:;,.>+~*()\/-])/i);
    if (!match) throw new Error("Unsupported CSS token");
    result.push(match[0].toLowerCase());
    if (result.length > 8192) throw new Error("Too many CSS tokens");
    i += match[0].length;
  }
  return result;
}

function value(parts: string[]): string {
  if (!parts.length || parts.length > 64) throw new Error("Invalid value");
  let depth = 0;
  for (let i = 0; i < parts.length; i++) {
    const part = parts[i];
    if (functions.has(part) && parts[i + 1] === "(" && depth === 0) { depth++; i++; continue; }
    if (part === ")" && depth === 1) { depth--; continue; }
    if (/^-?(?:\d*\.\d+|\d+)(?:%|px|rem|em|vh|vw|deg)?$/.test(part)) continue;
    if (depth && (part === "," || part === "/")) continue;
    if (!depth && (/^#(?:[a-f0-9]{3}|[a-f0-9]{4}|[a-f0-9]{6}|[a-f0-9]{8})$/.test(part) || words.has(part))) continue;
    throw new Error("Unsupported value");
  }
  if (depth) throw new Error("Unclosed function");
  return parts.join(" ");
}

export function sanitizeTokenValue(name: string, raw: unknown): string | undefined {
  if (typeof raw !== "string" || raw.length > 256 || !tokens.has(name)) return undefined;
  const kind = tokens.get(name);
  if (kind === "font") return AVAILABLE_FONTS.some((font) => raw === `"${font}", sans-serif`) ? raw : undefined;
  try {
    const parts = lex(raw);
    const safe = value(parts);
    if (kind === "radius") return parts.length === 1 && /^(?:0|(?:\d*\.\d+|\d+)(?:rem|px|em))$/.test(safe) ? safe : undefined;
    if (parts.length === 1 && (/^#/.test(safe) || words.has(safe) && !["none", "solid", "dashed", "dotted", "auto", "normal", "bold"].includes(safe))) return safe;
    return functions.has(parts[0]) && parts[1] === "(" && parts[parts.length - 1] === ")" ? safe : undefined;
  } catch { return undefined; }
}

export function sanitizeVars(input: unknown): ThemeVarOverrides {
  if (!input || typeof input !== "object" || Array.isArray(input) || Object.keys(input).length > THEME_TOKENS.length) return {};
  const safe: Record<string, string> = {};
  for (const [name, raw] of Object.entries(input)) {
    const cleaned = sanitizeTokenValue(name, raw);
    if (cleaned !== undefined) safe[name] = cleaned;
  }
  return safe;
}

/** Fail closed for the entire sheet. Supported selectors exclude attributes and functions. */
export function sanitizeCss(css: string): string {
  if (new TextEncoder().encode(css).length > MAX_CSS_SIZE) return "";
  try {
    const parts = lex(css);
    let i = 0;
    const rules: string[] = [];
    while (i < parts.length) {
      const selector: string[] = [];
      while (i < parts.length && parts[i] !== "{") {
        const part = parts[i++];
        if (!/^(?:[a-z_][a-z0-9_-]*|#[a-z0-9_-]+|[.:,>+~*])$/.test(part)) throw new Error("Invalid selector");
        selector.push(part);
      }
      if (!selector.length || parts[i++] !== "{") throw new Error("Missing rule");
      const declarations: string[] = [];
      while (i < parts.length && parts[i] !== "}") {
        const name = parts[i++];
        if (parts[i++] !== ":") throw new Error("Missing colon");
        const body: string[] = [];
        while (i < parts.length && parts[i] !== ";" && parts[i] !== "}") body.push(parts[i++]);
        let safe: string | undefined;
        if (name.startsWith("--")) safe = sanitizeTokenValue(name.slice(2), body.join(" "));
        else if (properties.has(name)) safe = value(body);
        if (!safe) throw new Error("Unsupported declaration");
        declarations.push(`${name}: ${safe};`);
        if (parts[i] === ";") i++;
      }
      if (parts[i++] !== "}") throw new Error("Unclosed rule");
      // Joining punctuation without spaces preserves class and pseudo selectors.
      const target = selector.join(" ").replace(/([.:])\s+/g, "$1");
      rules.push(`${target} { ${declarations.join(" ")} }`);
    }
    return rules.join("\n");
  } catch { return ""; }
}

export function buildVarOverrideCSS(input: unknown): string {
  const declarations = Object.entries(sanitizeVars(input)).filter(([name]) => name !== "font-body").map(([name, val]) => `--${name}: ${val};`);
  return declarations.length ? sanitizeCss(`:root { ${declarations.join(" ")} }`) : "";
}

export function hasUnsafeCss(css: string): boolean { return Boolean(css.trim()) && !sanitizeCss(css); }
