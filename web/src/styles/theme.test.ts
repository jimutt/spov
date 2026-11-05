import { describe, expect, it } from "vitest";
// The stylesheet as text. `?raw` rather than node:fs, because
// `astro check` types this file against `astro/client` alone and has
// no node types — a `readFileSync` here passes vitest and fails the
// build step, which is the one that knows.
import css from "./global.css?raw";

/**
 * The stylesheet's two dark blocks.
 *
 * Dark mode is declared TWICE — once under `prefers-color-scheme` for a
 * reader who never touched the toggle, and once under
 * `[data-theme="dark"]` for one who did — and the file's own comment
 * says the values are "written once", which they are not. Editing one
 * and not the other splits the theme by how the reader arrived at it:
 * the same page, two different colours, and nothing says so.
 *
 * Contrast is checked here too, for both themes. `--faint` carries the
 * smallest text on the page, including the composer's note about what a
 * message does to a job, and must stay above WCAG AA's 4.5:1 for text
 * this size on `--paper`, in the light block and the dark one alike.
 *
 * @decision(DL-056)
 */

/** The custom properties one block declares, as a name → value map. */
function block(source: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [, name, value] of source.matchAll(/--([a-z0-9-]+):\s*([^;]+);/g)) {
    out[name] = value.trim();
  }
  return out;
}

/**
 * Everything between a selector's opening brace and its matching close.
 *
 * The selector is matched at the START of a line, which is not fussiness:
 * `:root:not([data-theme="light"])` also appears inside the `@theme`
 * rule near the top of the file, and an `indexOf` finds that one — which
 * parses to an empty map and makes an equality assertion pass against
 * nothing.
 */
function body(selector: RegExp): string {
  const found = selector.exec(css);
  if (!found) throw new Error(`the stylesheet no longer contains ${selector}`);
  const at = found.index;
  let depth = 0;
  for (let i = at; i < css.length; i++) {
    if (css[i] === "{") depth++;
    if (css[i] === "}") {
      depth--;
      if (depth === 0) return css.slice(at, i);
    }
  }
  throw new Error(`unbalanced braces after ${selector}`);
}

const byPreference = block(body(/^\s*:root:not\(\[data-theme="light"\]\)\s*\{/m));
const byToggle = block(body(/^\s*:root\[data-theme="dark"\]\s*\{/m));
// Anchored like the two above: `:root {` is the light block, and the
// only place that selector opens a rule.
const light = block(body(/^:root\s*\{/m));

/**
 * Every token that sets words, and the surfaces words are drawn on.
 * `--gold` and `--olive` are absent on purpose: they are marks and
 * fills, and `--gold-t` and `--olive-b` are their text twins.
 */
const TEXT = ["ink", "soft", "faint", "olive-b", "rust", "gold-t", "stone"] as const;
const SURFACES = ["paper", "raised"] as const;

function luminance(hex: string): number {
  const h = hex.replace("#", "");
  const channel = (at: number) => {
    const c = parseInt(h.slice(at, at + 2), 16) / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * channel(0) + 0.7152 * channel(2) + 0.0722 * channel(4);
}

function contrast(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

describe("the dark theme", () => {
  it("says the same thing however the reader reached it", () => {
    expect(Object.keys(byPreference).length).toBeGreaterThan(10);
    expect(byToggle).toEqual(byPreference);
  });
});

describe.each([
  ["light", light],
  ["dark", byToggle],
] as const)("the %s theme", (_name, theme) => {
  it("declares every token the checks below read", () => {
    expect(Object.keys(theme).sort()).toEqual(
      expect.arrayContaining([...TEXT, ...SURFACES, "on-accent", "olive"]),
    );
  });

  // AA is 4.5:1 for text at these sizes. Both surfaces, because text
  // is drawn on the page and inside a raised box alike.
  it("keeps every text colour readable on both surfaces", () => {
    for (const ink of TEXT) {
      for (const surface of SURFACES) {
        expect(
          contrast(theme[ink], theme[surface]),
          `--${ink} on --${surface}`,
        ).toBeGreaterThanOrEqual(4.5);
      }
    }
  });

  // A primary button is filled olive and a confirming danger press is
  // filled rust, both lettered in --on-accent.
  it("keeps the filled buttons' words readable", () => {
    for (const fill of ["olive-b", "rust"] as const) {
      expect(
        contrast(theme["on-accent"], theme[fill]),
        `--on-accent on --${fill}`,
      ).toBeGreaterThanOrEqual(4.5);
    }
  });

  // And it stays quieter than --soft, or the two ranks collapse and
  // the page loses the distinction it draws them for.
  it("keeps faint quieter than soft", () => {
    expect(contrast(theme.faint, theme.paper)).toBeLessThan(contrast(theme.soft, theme.paper));
  });
});
