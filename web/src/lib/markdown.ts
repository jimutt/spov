/**
 * Rendering Markdown that an agent wrote, for a report job's reports and
 * for transcript messages. Both inputs are attacker-influenced and this
 * bypasses Svelte's escaping, so everything goes through one sanitiser
 * configuration; the two uses differ only in presentation.
 *
 * @decision(DL-063)
 */
import DOMPurify from 'dompurify';
import { Marked } from 'marked';

/**
 * The elements a rendered document may contain: an allowlist, narrower
 * than DOMPurify's default. `img` is absent (the `image` renderer override
 * emits the alt text instead) except in a report rendered with an image
 * source, as are `style`, `link`, form controls and embedding elements.
 */
const ALLOWED_TAGS = [
  'p',
  'br',
  'hr',
  'strong',
  'em',
  'del',
  'code',
  'pre',
  'blockquote',
  'ul',
  'ol',
  'li',
  'a',
  'h1',
  'h2',
  'h3',
  'h4',
  'h5',
  'h6',
  'table',
  'thead',
  'tbody',
  'tr',
  'th',
  'td',
];

/**
 * The attributes a rendered document may carry. No `class` (any compiled
 * Tailwind utility would be reachable; the fence's `language-x` label is
 * kept by the hook below), no `id` and no `style`.
 */
const ALLOWED_ATTR = ['href', 'title', 'align', 'colspan', 'rowspan'];

/**
 * The URL schemes a link may use; `javascript:` and `data:` are excluded.
 * It governs an image's `src` too, which the image source makes absolute
 * on this origin and the hook narrows to the job's file endpoint.
 */
const ALLOWED_URI_REGEXP = /^(?:https?|mailto):/i;

/**
 * The image source of the render in progress, set only for its duration:
 * parsing and sanitising are synchronous, so no other render can see it.
 *
 * @decision(DL-063)
 */
export type ImageSource = { prefix: string; resolve: (dest: string) => string | undefined };
let images: ImageSource | undefined;

/**
 * The Markdown dialect: GFM, with `breaks` so a single newline is a `<br>`,
 * since agents write line-oriented output (file lists, stack traces). A
 * fresh instance so no other importer of `marked` shares this config.
 */
const renderer = new Marked({
  gfm: true,
  breaks: true,
  async: false,
  renderer: {
    /**
     * An image becomes its escaped alt text, unless the render has an
     * image source and the link resolves to one of the job's collected
     * files. Done in the renderer rather than by rewriting the source, so
     * code blocks, inline code and reference-style images are handled by
     * marked's own parse.
     *
     * @decision(DL-063)
     */
    image({ href, text }) {
      const alt = escapeHTML(String(text ?? '').trim());
      const src = images?.resolve(String(href ?? ''));
      if (src) return `<img src="${escapeHTML(src)}" alt="${alt}" loading="lazy">`;
      return alt ? `(image: ${alt})` : '(image)';
    },
    /**
     * A GFM task list keeps its state as ☑/☐, since the allowlist drops
     * the checkbox `input`.
     */
    checkbox({ checked }) {
      return checked ? '☑ ' : '☐ ';
    },
  },
});

/** escapeHTML makes text safe to emit as markup. The sanitiser runs
 *  afterwards regardless; this keeps the renderer's own output honest. */
function escapeHTML(s: string): string {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

/**
 * `render` takes no mode: a report and a message share one allowlist. The
 * caller wraps the output in `md-body` or `md-body md-message`, and the
 * stylesheet demotes a message's headings rather than the sanitiser
 * withholding them.
 */

/**
 * Whether DOMPurify can run. It needs a DOM, which Astro's build-time
 * render lacks; when it cannot sanitise, nothing is rendered as HTML.
 */
function canSanitise(): boolean {
  // Do not weaken to `typeof sanitize === 'function'`: DOMPurify's
  // unsupported path returns its input unchanged. See DL-063.
  return typeof window !== 'undefined' && DOMPurify.isSupported === true;
}

/**
 * The link and code-fence hooks, registered once on the shared DOMPurify
 * instance and written to be harmless to any other caller.
 *
 * @decision(DL-063)
 */
let hooked = false;
function installHooks(): void {
  if (hooked) return;
  hooked = true;
  DOMPurify.addHook('afterSanitizeAttributes', (node) => {
    const el = node as Element;
    // A link an agent wrote must not navigate the session page away,
    // and must not hand the destination this page's URL. `noopener` is
    // what stops the opened page reaching back through window.opener.
    if (el.tagName === 'A' && el.hasAttribute('href')) {
      el.setAttribute('target', '_blank');
      el.setAttribute('rel', 'noreferrer noopener');
    }
    // An image only from the job's own file endpoint, whatever wrote
    // it: raw HTML in the Markdown reaches here too.
    //
    // @decision(DL-063)
    if (el.tagName === 'IMG') {
      const src = el.getAttribute('src') ?? '';
      if (!images || !src.startsWith(images.prefix)) {
        el.remove();
        return;
      }
    }
    // Keep only marked's `language-x` fence label on `code`.
    if (el.hasAttribute('class')) {
      const kept = (el.getAttribute('class') ?? '')
        .split(/\s+/)
        .filter((c) => el.tagName === 'CODE' && /^language-[\w+#.-]+$/.test(c));
      if (kept.length) el.setAttribute('class', kept.join(' '));
      else el.removeAttribute('class');
    }
  });
}

/**
 * render turns agent-authored Markdown into sanitised HTML, or returns
 * null when it cannot render safely; the caller then shows the plain
 * `whitespace-pre-wrap` text. It must never return the source as HTML.
 * Only a report's preview passes an image source; without one every
 * image is its alt text.
 *
 * @decision(DL-063)
 */
export function render(source: string, imageSource?: ImageSource): string | null {
  if (!source.trim()) return null;
  const text = source;
  if (!canSanitise()) return null;
  images = imageSource;
  try {
    return sanitise(text);
  } finally {
    images = undefined;
  }
}

function sanitise(text: string): string | null {
  let html: string;
  try {
    html = renderer.parse(text) as string;
  } catch {
    // A malformed document is the agent's, not the page's. Falling back
    // to text shows the reader what was written rather than an error
    // about it.
    return null;
  }
  installHooks();
  const clean = DOMPurify.sanitize(html, {
    ALLOWED_TAGS: images ? [...ALLOWED_TAGS, 'img'] : ALLOWED_TAGS,
    ALLOWED_ATTR: images ? [...ALLOWED_ATTR, 'src', 'alt', 'loading'] : ALLOWED_ATTR,
    ALLOWED_URI_REGEXP,
    // Both default to true and are checked independently of ALLOWED_ATTR.
    ALLOW_DATA_ATTR: false,
    ALLOW_ARIA_ATTR: false,
    // Admitted here and narrowed by the hook: the allowlist runs before
    // `afterSanitizeAttributes`, so the hook cannot keep what it removed.
    ADD_ATTR: ['class'],
    // Leave KEEP_CONTENT at its default: `false` strips the text of every
    // element. See DL-063.
    // No `<html>`/`<body>` wrapper: this goes inside a paragraph's
    // worth of page, not into a document.
    RETURN_DOM: false,
    RETURN_DOM_FRAGMENT: false,
  });
  return clean.trim() || null;
}

/**
 * hasMarkup reports whether rendering would show anything the plain text
 * does not, so plain prose skips the renderer. Deliberately over-eager: a
 * false positive costs one render, a false negative shows literal backticks.
 */
export function hasMarkup(text: string): boolean {
  return (
    // A fence, inline code, a heading, a list, a quote, a rule, a
    // table, a link, or emphasis around a word.
    /(^|\n)\s{0,3}(```|~~~|#{1,6}\s|[-*+]\s|\d+\.\s|>\s|\|)/.test(text) ||
    /`[^`\n]+`/.test(text) ||
    // `](` rather than a full link pattern, which rescans from every `[`
    // on unbounded text. This must stay linear; see DL-063.
    text.includes('](') ||
    /(\*\*|__)[^\s][^\n]*?\1/.test(text)
  );
}
