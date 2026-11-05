import { describe, expect, test } from 'vitest';
import { mount, unmount } from 'svelte';
import Logo from './Logo.svelte';
import { MarkPath, MarkViewBox } from './logo';

// The mark's geometry exists twice: in Logo.svelte, which the page
// renders, and in public/favicon.svg, which the browser fetches outside
// the page and which therefore cannot inherit currentColor. The two can
// only be kept in step by hand, and a divergence shows up nowhere except
// a browser tab — so it has to fail here instead.
//
// @decision(DL-056)

// Read as text at build time. Not node:fs: this project carries no
// @types/node, and `astro check` fails on the import even though vitest
// runs it fine. The stylesheet needs `css: true` in vitest.config.ts,
// or it arrives as an empty string.
const raw = (glob: Record<string, unknown>, key: string) => String(glob[key] ?? '');
const favicon = raw(
  import.meta.glob('../../public/favicon.svg', { query: '?raw', import: 'default', eager: true }),
  '../../public/favicon.svg',
);
const css = raw(
  import.meta.glob('../styles/global.css', { query: '?raw', import: 'default', eager: true }),
  '../styles/global.css',
);

const pathOf = (svg: string) => svg.match(/<path[^>]*\sd="([^"]+)"/)?.[1] ?? '';
const viewBoxOf = (svg: string) => svg.match(/viewBox="([^"]+)"/)?.[1] ?? '';

describe('the logo and the favicon are one mark', () => {
  test('the favicon carries the same geometry the component renders', () => {
    expect(favicon, 'public/favicon.svg was not readable').toBeTruthy();
    expect(pathOf(favicon)).toBe(MarkPath);
    expect(viewBoxOf(favicon)).toBe(MarkViewBox);
  });

  // The mark's interior is holes, not white shapes drawn on top. Without
  // evenodd the two enclosed subpaths fill in and the logo is a solid
  // blob; with white shapes instead of holes it would look right only on
  // a white page, which this site is half the time.
  test('the interior is holes, in the source and in the favicon', () => {
    // Three subpaths: the outline and the two cutouts. A path that lost
    // one is a mark missing its interior.
    expect(MarkPath.match(/M /g)?.length, 'subpaths in MarkPath').toBe(3);
    expect(favicon).toContain('fill-rule="evenodd"');
  });

  test('there is no white fill anywhere, which is what "transparent" means here', () => {
    expect(favicon.toLowerCase()).not.toMatch(/fill\s*[:=]\s*"?\s*(#fff|#ffffff|white|rgb\(255)/);
  });
});

describe('the colours', () => {
  // The favicon cannot inherit currentColor — it is fetched outside the
  // page — so it is the one place a literal colour lives. Both come from
  // logo.ts, which names global.css as their source.
  test("the favicon's literals are the stylesheet's own --ink values", () => {
    // Against global.css itself, not against a constant that also lives
    // in this repo: a copy checked against another copy passes while
    // both drift from the source.
    expect(css, 'global.css read back empty — vitest needs css: true').toContain('--ink:');
    const light = css.match(/:root\s*\{[^}]*?--ink:\s*(#[0-9a-f]{3,8})/i)?.[1];
    const dark = css.match(/:root\[data-theme="dark"\]\s*\{[^}]*?--ink:\s*(#[0-9a-f]{3,8})/i)?.[1];
    expect(light, 'light --ink not found in global.css').toBeTruthy();
    expect(dark, 'dark --ink not found in global.css').toBeTruthy();
    // A favicon with one colour is a favicon that vanishes on one theme.
    expect(light).not.toBe(dark);

    const faviconLight = favicon.match(/path\s*\{\s*fill:\s*(#[0-9a-f]{3,8})/i)?.[1];
    const faviconDark = favicon.match(
      /prefers-color-scheme:\s*dark[^}]*\{[^}]*fill:\s*(#[0-9a-f]{3,8})/i,
    )?.[1];
    expect(faviconLight?.toLowerCase(), 'light fill').toBe(light?.toLowerCase());
    expect(faviconDark?.toLowerCase(), 'dark fill').toBe(dark?.toLowerCase());
  });

  // The rendered mark has no colour of its own, so it follows the theme
  // through whatever the surrounding text is set to.
  test('the component paints in currentColor', () => {
    const host = document.createElement('div');
    document.body.append(host);
    const app = mount(Logo, { target: host, props: {} });
    expect(host.querySelector('svg')!.getAttribute('fill')).toBe('currentColor');
    unmount(app);
    host.remove();
  });
});

describe('rendering', () => {
  test('the box follows the mark, which is wider than it is tall', () => {
    const host = document.createElement('div');
    document.body.append(host);
    const app = mount(Logo, { target: host, props: { size: 100 } });
    const svg = host.querySelector('svg')!;
    // Against the viewBox rather than against MarkAspect: re-deriving
    // the implementation would pass for any aspect, including a wrong
    // one. Forcing a square would squash the mark.
    const [, , vbW, vbH] = MarkViewBox.split(/\s+/).map(Number);
    expect(svg.getAttribute('height')).toBe('100');
    expect(Number(svg.getAttribute('width'))).toBeCloseTo((100 * vbW) / vbH, 0);
    unmount(app);
    host.remove();
  });

  // The rendered path is what the browser draws, and none of the checks
  // above look at it: they read MarkPath and the favicon. Without this,
  // deleting fill-rule from the component leaves every test green while
  // every mark on the site becomes a solid blob.
  test('the rendered path carries the geometry and the fill rule', () => {
    const host = document.createElement('div');
    document.body.append(host);
    const app = mount(Logo, { target: host, props: {} });
    const path = host.querySelector('path')!;
    expect(path.getAttribute('fill-rule')).toBe('evenodd');
    expect(path.getAttribute('d')).toBe(MarkPath);
    expect(host.querySelector('svg')!.getAttribute('viewBox')).toBe(MarkViewBox);
    unmount(app);
    host.remove();
  });

  test('it is decorative unless given a name', () => {
    const host = document.createElement('div');
    document.body.append(host);

    const plain = mount(Logo, { target: host, props: {} });
    // Beside the `spov.` wordmark the name is already in text, and a
    // screen reader announcing it twice is worse than not at all.
    const hidden = host.querySelector('svg')!;
    expect(hidden.getAttribute('aria-hidden')).toBe('true');
    // No role beside it: ARIA ignores a presentation role on an element
    // carrying a global attribute, so the pair would be dead weight.
    expect(hidden.getAttribute('role')).toBeNull();
    unmount(plain);

    const named = mount(Logo, { target: host, props: { title: 'spov' } });
    const svg = host.querySelector('svg')!;
    expect(svg.getAttribute('aria-hidden')).toBeNull();
    expect(svg.getAttribute('aria-label')).toBe('spov');
    expect(svg.getAttribute('role')).toBe('img');
    unmount(named);

    host.remove();
  });
});
