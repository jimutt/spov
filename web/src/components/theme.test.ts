import { afterEach, beforeEach, describe, expect, test } from 'vitest';
import { mount, tick, unmount } from 'svelte';
import ThemeToggle from './ThemeToggle.svelte';
import { Attribute, StorageKey, choose, resolve, stored } from './theme';

function setSystem(dark: boolean) {
  Object.defineProperty(globalThis, 'matchMedia', {
    configurable: true,
    writable: true,
    value: (q: string) => ({ matches: dark && q.includes('dark'), media: q }),
  });
}

beforeEach(() => {
  localStorage.clear();
  document.documentElement.removeAttribute(Attribute);
  setSystem(false);
});

describe('theme', () => {
  // Base.astro repeats these two literals inline, because a module
  // import in the head would be deferred and the page would flash the
  // wrong theme. They can only be kept in step by hand, so changing one
  // here has to fail rather than silently diverge.
  test('the key and attribute Base.astro hardcodes', () => {
    expect(StorageKey).toBe('spov-theme');
    expect(Attribute).toBe('data-theme');
  });

  test('no choice follows the system', () => {
    expect(stored()).toBeNull();
    expect(resolve()).toBe('light');
    setSystem(true);
    expect(resolve()).toBe('dark');
  });

  test('a choice pins the theme and outranks the system', () => {
    setSystem(true);
    choose('light');
    expect(document.documentElement.getAttribute(Attribute)).toBe('light');
    expect(localStorage.getItem(StorageKey)).toBe('light');
    expect(resolve()).toBe('light');
  });

  test('a junk stored value is ignored', () => {
    localStorage.setItem(StorageKey, 'sepia');
    expect(stored()).toBeNull();
  });
});

describe('ThemeToggle', () => {
  let component: Record<string, unknown> | undefined;

  afterEach(() => {
    if (component) unmount(component);
    component = undefined;
    document.body.innerHTML = '';
  });

  test('marks the active theme and pins the other on click', async () => {
    setSystem(true);
    component = mount(ThemeToggle, { target: document.body });
    await tick();

    const [light, dark] = Array.from(document.querySelectorAll('button'));
    expect(dark.getAttribute('aria-pressed')).toBe('true');
    expect(light.getAttribute('aria-pressed')).toBe('false');

    light.click();
    await tick();

    expect(document.documentElement.getAttribute(Attribute)).toBe('light');
    expect(light.getAttribute('aria-pressed')).toBe('true');
  });
});
