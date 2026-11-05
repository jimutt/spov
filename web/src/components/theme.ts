// @decision(DL-055)
export type Theme = 'light' | 'dark';

/** Where an explicit choice is remembered. Shared with the inline head
 *  script in Base.astro, which must run before first paint. */
export const StorageKey = 'spov-theme';

/** The attribute the `dark:` variant keys on (src/styles/global.css). */
export const Attribute = 'data-theme';

function isTheme(v: unknown): v is Theme {
  return v === 'light' || v === 'dark';
}

/** The remembered choice, or null when the viewer has not chosen.
 *  Storage throws in private mode rather than returning null. */
export function stored(): Theme | null {
  try {
    const v = localStorage.getItem(StorageKey);
    return isTheme(v) ? v : null;
  } catch {
    return null;
  }
}

/** What the OS asks for. Defaults to light where matchMedia is absent. */
export function system(): Theme {
  return globalThis.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

/** The theme in effect: the pinned choice if there is one, else the OS.
 *  The attribute is checked first because the inline head script sets it
 *  before paint; storage is the fallback for when that script did not
 *  run, which is every test and any page that forgot the layout. */
export function resolve(): Theme {
  const pinned = document.documentElement.getAttribute(Attribute);
  if (isTheme(pinned)) return pinned;
  return stored() ?? system();
}

/** Pin a theme and remember it. A failed write still applies the theme,
 *  so the toggle works in private mode for the life of the page. */
export function choose(t: Theme): void {
  document.documentElement.setAttribute(Attribute, t);
  try {
    localStorage.setItem(StorageKey, t);
  } catch {
    /* storage unavailable; the choice lasts until reload */
  }
}
