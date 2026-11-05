import { describe, expect, it } from "vitest";

/**
 * The bundled faces' licence texts. The OFL asks for the licence to
 * travel with the fonts, and the Google builds Fontsource ships keep
 * only the copyright in their name tables, so `public/licenses/` holds
 * copies that every page links to. A bump of either package that
 * changes its licence fails here until the copy is refreshed.
 *
 * `import.meta.glob` rather than node:fs, which `astro check` cannot
 * type (see theme.test.ts).
 *
 * @decision(DL-056)
 */
const copies = import.meta.glob<string>("/public/licenses/*.txt", {
  query: "?raw",
  import: "default",
  eager: true,
});
const originals = import.meta.glob<string>("/node_modules/@fontsource-variable/atkinson-hyperlegible-*/LICENSE", {
  query: "?raw",
  import: "default",
  eager: true,
});

describe("the bundled faces' licences", () => {
  it("ship a copy of each package's own licence", () => {
    expect(Object.keys(originals)).toHaveLength(2);
    for (const [path, text] of Object.entries(originals)) {
      const face = path.split("/").at(-2);
      expect(copies[`/public/licenses/${face}.txt`], face).toBe(text);
      expect(text).toContain("SIL Open Font License");
    }
  });
});
