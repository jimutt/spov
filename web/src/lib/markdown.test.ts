import { describe, expect, test } from 'vitest';
import DOMPurify from 'dompurify';
import { hasMarkup, render } from './markdown';
import { imageSource } from './deliverables';

/**
 * The renderer's tests, mostly about what does not come out. This
 * module is the only thing standing between agent-authored text and the
 * page's own markup.
 *
 * @decision(DL-063)
 */

/** rendered is render()'s output, failing the test if it declined. */
function rendered(source: string): string {
  const html = render(source);
  if (html === null) {
    throw new Error(`render() declined to render: ${JSON.stringify(source)}`);
  }
  return html;
}

describe('what a rendered document may not contain', () => {
  test('a script tag survives in no form', () => {
    const html = rendered('Here is a note.\n\n<script>alert(1)</script>\n\nAnd more.');
    expect(html).not.toContain('<script');
    expect(html).not.toContain('alert(1)');
    // Its source must not survive as visible prose either, which would
    // read as the agent having written it. Measured: DOMPurify drops
    // script, style, iframe and form WITH their content.
    expect(html).toContain('Here is a note.');
    expect(html).toContain('And more.');
  });

  test('an event handler attribute is dropped', () => {
    const html = rendered('<p onclick="steal()">click me</p>');
    expect(html).not.toContain('onclick');
    expect(html).not.toContain('steal');
  });

  test('a javascript: link is not a link', () => {
    const html = rendered('[click me](javascript:alert(1))');
    expect(html).not.toContain('javascript:');
  });

  test('a data: link is not a link', () => {
    // Looks harmless and is not: data:text/html opens attacker-authored
    // markup on this page's own origin.
    const html = rendered('[report](data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==)');
    expect(html).not.toContain('data:text/html');
  });

  test('an https link survives, because links are the point', () => {
    const html = rendered('[the pull request](https://codefloe.com/acme/widgets/pulls/77)');
    expect(html).toContain('https://codefloe.com/acme/widgets/pulls/77');
  });

  test('a link opens away from the page and hands over no referrer', () => {
    // An agent-authored link must not navigate the session page away,
    // and the opened page must not reach back through window.opener.
    const html = rendered('[somewhere](https://example.com/x)');
    expect(html).toContain('target="_blank"');
    expect(html).toContain('rel="noreferrer noopener"');
  });

  test('an image is not loaded, and its alt text stands in', () => {
    // A remote image is an outbound request from the reader's browser
    // to a URL the sandbox chose, which reports that the report was
    // read and by whom.
    const html = rendered('![a diagram of the results](https://tracker.example/pixel.png)');
    expect(html).not.toContain('<img');
    expect(html).not.toContain('tracker.example');
    // The alt text stands in, so a report whose only content was a
    // diagram does not render as an empty box.
    expect(html).toContain('a diagram of the results');
  });

  test('an image alt cannot smuggle markup back in', () => {
    // The alt is agent-authored too and is put back into the SOURCE
    // before parsing, so it has to be escaped as Markdown. A backtick
    // would otherwise open a code span, and emphasis markers would
    // style the page's own text.
    const html = rendered('![see `code` and **bold**](https://x.example/p.png)');
    expect(html).not.toContain('<code');
    expect(html).not.toContain('<strong');
    expect(html).toContain('bold');
  });

  test('an iframe is dropped whole', () => {
    const html = rendered('Before.\n\n<iframe src="https://evil.example"></iframe>\n\nAfter.');
    expect(html).not.toContain('<iframe');
    expect(html).not.toContain('evil.example');
    expect(html).toContain('Before.');
    expect(html).toContain('After.');
  });

  test('a style block cannot restyle the page around it', () => {
    const html = rendered('<style>body{display:none}</style>\n\nStill here.');
    expect(html).not.toContain('<style');
    expect(html).not.toContain('display:none');
    expect(html).toContain('Still here.');
  });

  test('a style attribute cannot cover the page', () => {
    // The remaining way to put one element over another, which is how
    // rendered content puts something misleading over real controls.
    const html = rendered('<p style="position:fixed;inset:0;z-index:99">gotcha</p>');
    expect(html).not.toContain('position:fixed');
  });

  test('a form cannot be put in front of a reader', () => {
    // Dropped whole, content included — so a document that was ONLY a
    // form renders as nothing and render() declines, which the caller
    // shows as the plain source text.
    expect(render('<form action="https://evil.example"><input name="token"></form>')).toBeNull();
    // In a document with other content, the form goes and the rest
    // stays.
    const html = rendered('Before.\n\n<form><input name="token"></form>\n\nAfter.');
    expect(html).not.toContain('<form');
    expect(html).not.toContain('<input');
    expect(html).toContain('Before.');
    expect(html).toContain('After.');
  });

  test('an id cannot be claimed, whatever emits it', () => {
    // Nothing this build emits carries one, so this pins the allowlist
    // rather than marked's behaviour: content must not be able to
    // collide with the page's own anchors or a label's `for` target.
    const html = rendered('<h1 id="main">Results</h1>');
    expect(html).not.toContain('id=');
    expect(html).toContain('Results');
  });

  test('a class cannot reach the page\'s own utilities', () => {
    // The one that matters most. Tailwind compiles the classes THIS
    // app uses, so every utility on the page is reachable by name from
    // rendered content — `fixed inset-0 z-30 bg-paper` is a full-page
    // opaque overlay carrying attacker text, which is the same attack
    // the style-attribute rule prevents.
    const html = rendered('<p class="fixed inset-0 z-30 bg-paper p-4">You were signed out.</p>');
    expect(html).not.toContain('fixed');
    expect(html).not.toContain('inset-0');
    expect(html).not.toContain('bg-paper');
    expect(html).toContain('You were signed out.');
  });

  test("a class cannot reproduce spov's own voice", () => {
    // The event rows are styled by class too. A sandbox drawing them
    // would be writing in spov's voice, which the source-based seam
    // rule exists to prevent.
    const html = rendered(
      '<p class="border border-rule bg-raised font-mono text-soft">spov: recording gap</p>',
    );
    expect(html).not.toContain('bg-raised');
    expect(html).not.toContain('border-rule');
  });

  test('the fence label is the one class that survives, and only on code', () => {
    const html = rendered('```diff\n- old\n```');
    expect(html).toContain('language-diff');
    // But not smuggled onto anything else.
    const sneaky = rendered('<p class="language-diff fixed inset-0">x</p>');
    expect(sneaky).not.toContain('language-diff');
    expect(sneaky).not.toContain('fixed');
  });

  test('data- and aria- attributes do not survive', () => {
    // Both default to allowed and are checked INDEPENDENTLY of the
    // attribute allowlist. global.css keys its dark palette on
    // [data-theme="dark"] *, and aria-* lets content lie to a screen
    // reader about what it is.
    const html = rendered('<p data-theme="dark" aria-hidden="true" aria-label="Sign in">hi</p>');
    expect(html).not.toContain('data-theme');
    expect(html).not.toContain('aria-hidden');
    expect(html).not.toContain('aria-label');
    expect(html).toContain('hi');
  });
});

describe('what a rendered document should contain', () => {
  test('a fenced code block keeps its language label', () => {
    const html = rendered('```diff\n- old\n+ new\n```');
    expect(html).toContain('<pre');
    expect(html).toContain('<code');
    // The label is how a reader tells a diff from a shell transcript.
    expect(html).toContain('language-diff');
  });

  test('code is escaped rather than interpreted', () => {
    const html = rendered('```html\n<script>alert(1)</script>\n```');
    // Visible as text inside the block, never as markup.
    expect(html).not.toContain('<script>alert(1)</script>');
    expect(html).toContain('&lt;script&gt;');
  });

  test('a GFM table renders, because agents write them', () => {
    const html = rendered('| gate | outcome |\n| --- | --- |\n| check | passed |');
    expect(html).toContain('<table');
    expect(html).toContain('passed');
  });

  test('a single newline breaks the line', () => {
    // Strict Markdown joins these into one paragraph, which is right
    // for prose and wrong for the line-oriented output agents produce.
    const html = rendered('file one\nfile two');
    expect(html).toContain('<br');
  });

  test('a task list keeps its state', () => {
    // The checkbox is an <input>, which the allowlist drops — leaving
    // done and not-done rendering identically. Agents write checklists
    // constantly, so that makes a report unreadable.
    const html = rendered('- [x] done\n- [ ] not done');
    expect(html).not.toContain('<input');
    expect(html).toContain('☑');
    expect(html).toContain('☐');
  });

  test('a fenced block is left exactly as written', () => {
    // The image substitution is a renderer override, not a rewrite of
    // the source — so a report DOCUMENTING Markdown keeps its examples.
    const html = rendered('```md\n![logo](x.png)\n```');
    expect(html).toContain('![logo](x.png)');
    expect(html).not.toContain('(image:');
  });

  test('inline code is left exactly as written', () => {
    const html = rendered('run `![a](b)` to see it');
    expect(html).toContain('![a](b)');
  });

  test('an image alt with brackets is still replaced', () => {
    // Rewriting the source would miss these; marked has already
    // decided what is an image by the time the override runs.
    const html = rendered('![a [nested] alt](https://x.example/p.png)');
    expect(html).not.toContain('<img');
    expect(html).not.toContain('x.example');
    expect(html).toContain('nested');
  });

  test('lists and emphasis render', () => {
    const html = rendered('- **one**\n- _two_');
    expect(html).toContain('<ul');
    expect(html).toContain('<strong>');
    expect(html).toContain('<em>');
  });
});

describe('when rendering declines', () => {
  test('empty and whitespace produce nothing to render', () => {
    expect(render('')).toBeNull();
    expect(render('   \n\t ')).toBeNull();
  });

  test('an environment that cannot sanitise renders nothing at all', () => {
    // DOMPurify's own unsupported path RETURNS THE INPUT UNCHANGED, and
    // `sanitize` exists either way — so a guard that checked for the
    // method would pass in exactly the environment where sanitising
    // silently does nothing.
    const was = DOMPurify.isSupported;
    try {
      Object.defineProperty(DOMPurify, 'isSupported', { value: false, configurable: true });
      expect(render('# heading\n\n<script>alert(1)</script>')).toBeNull();
    } finally {
      Object.defineProperty(DOMPurify, 'isSupported', { value: was, configurable: true });
    }
  });

  test('content that sanitises away entirely is null, not an empty box', () => {
    // The caller falls back to the plain text, which shows the reader
    // what was written.
    expect(render('<script>alert(1)</script>')).toBeNull();
  });
});

describe('deciding whether to render at all', () => {
  test('the check is linear, because nothing bounds a message', () => {
    // The obvious link pattern /\[[^\]\n]*\]\([^)\n]*\)/ rescans to
    // end-of-line from every `[`: measured at 14s on 200k open
    // brackets, against 72ms for the whole render. This runs on every
    // message of every transcript, and a stored message has no length
    // bound — so one message would freeze the tab for every viewer.
    const hostile = '['.repeat(200_000);
    const started = Date.now();
    hasMarkup(hostile);
    expect(Date.now() - started).toBeLessThan(1000);
  });

  test('ordinary prose is not put through the renderer', () => {
    // Most messages are prose, and rendering each one to produce
    // <p>the same sentence</p> is work with no result.
    expect(hasMarkup('I have finished the task and pushed the branch.')).toBe(false);
    expect(hasMarkup('')).toBe(false);
  });

  test('the markup agents actually write is detected', () => {
    for (const source of [
      '```sh\nls\n```',
      'run `go test ./...` first',
      '# Results',
      '- one\n- two',
      '1. first',
      '> a quote',
      '| a | b |\n| - | - |',
      'see [the PR](https://example.com)',
      'this is **important**',
    ]) {
      expect(hasMarkup(source), source).toBe(true);
    }
  });

  test('a stray asterisk is not markup', () => {
    // Over-eager is the safe direction — a false positive costs one
    // render, a false negative shows literal backticks — but a bare
    // asterisk in prose must not trip it.
    expect(hasMarkup('the cost is 5 * 3 dollars')).toBe(false);
  });
});

describe("a report's images, with the job's image source", () => {
  // @decision(DL-063)
  const origin = 'http://localhost:3000';
  const source = imageSource('j1', origin);
  const withImages = (md: string): string => {
    const html = render(md, source);
    if (html === null) throw new Error('render() declined');
    return html;
  };

  test('a link inside the out directory loads from spov, and keeps its alt', () => {
    const html = withImages('![the chart](images/chart.png)');
    expect(html).toContain(`<img src="${origin}/api/v1/jobs/j1/file?path=images%2Fchart.png"`);
    expect(html).toContain('alt="the chart"');
  });

  test('a reference-style image resolves the same way', () => {
    expect(withImages('![c][r]\n\n[r]: ./images/c.png')).toContain('path=images%2Fc.png');
  });

  test('a remote, data: or escaping image is still its alt text', () => {
    for (const md of [
      '![remote](https://tracker.example/pixel.png)',
      '![data](data:image/png;base64,AAAA)',
      '![up](../secret.png)',
      '![abs](/etc/passwd)',
    ]) {
      const html = withImages(md);
      expect(html, md).not.toContain('<img');
      expect(html, md).toContain('(image:');
    }
  });

  test('raw HTML cannot load an image from anywhere else', () => {
    for (const raw of [
      '<img src="https://tracker.example/pixel.png">',
      `<img src="${origin}/api/v1/jobs/other/file?path=a.png">`,
      '<img src="images/a.png" onerror="steal()">',
    ]) {
      const html = render(`text\n\n${raw}`, source) ?? '';
      expect(html, raw).not.toContain('<img');
      expect(html, raw).not.toContain('steal');
    }
  });

  test('without an image source nothing is loaded, as in a transcript message', () => {
    const html = rendered('![c](images/c.png)');
    expect(html).not.toContain('<img');
    expect(html).toContain('(image: c)');
  });

  test('a render with images leaves none allowed in the next one', () => {
    withImages('![c](images/c.png)');
    const next = rendered(`text\n\n<img src="${origin}/api/v1/jobs/j1/file?path=c.png">`);
    expect(next).toContain('text');
    expect(next).not.toContain('<img');
  });
});
