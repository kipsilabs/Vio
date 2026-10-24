// @vitest-environment node

import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { decodePGSObject, PGSStreamParser } from "./pgs";

const fixture = readFileSync(new URL("../testdata/pgs/dam-cav.sup", import.meta.url));

describe("PGS real .sup fixture", () => {
  it("parses a real 1280x720 PGS stream across arbitrary chunk boundaries", () => {
    const parser = new PGSStreamParser();
    const compositions = [];
    for (let offset = 0; offset < fixture.length; offset += 257) {
      compositions.push(...parser.push(fixture.subarray(offset, offset + 257)));
    }

    expect(compositions).toHaveLength(2);
    expect(compositions[0]).toMatchObject({ start: 0, width: 1280, height: 720 });
    expect(compositions[0]!.objects).toHaveLength(1);
    expect(compositions[1]).toMatchObject({ start: 60, width: 1280, height: 720 });
    expect(compositions[1]!.objects).toHaveLength(0);
  });

  it("decodes the real object's bitmap", () => {
    const parser = new PGSStreamParser();
    const [composition] = parser.push(fixture);
    const object = composition!.objects[0]!;
    expect(object.width).toBeGreaterThan(0);
    expect(object.height).toBeGreaterThan(0);
    expect(Array.from(decodePGSObject(object)).some((pixel) => pixel !== 0)).toBe(true);
  });
});
