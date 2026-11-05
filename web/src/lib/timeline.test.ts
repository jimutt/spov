import { describe, expect, test } from 'vitest';
import {
  CollapseFrom,
  noteText,
  SeamTextLimit,
  blocksOf,
  buildTimeline,
  condense,
  messageKey,
  messageStatuses,
  timeDividers,
  runSummary,
  seamText,
  shown,
  toolInput,
  trimBlankLines,
  shownWarnings,
  stopNote,
  type Activity,
  type Block,
  type Conversation,
  type EventEntry,
} from './timeline';

/** ev builds one frame of the log, at the position the store gave it. */
function ev(pos: number, name: string, at = '', text?: string): Activity {
  return { pos, at, name, source: 'opencode', ...(text === undefined ? {} : { text }) };
}

/** marker builds one of spov's own frames — the only kind that is a seam. */
function marker(pos: number, name: string, at = '', text?: string): Activity {
  return { ...ev(pos, name, at, text), source: 'spov' };
}

/** conv builds a one-conversation transcript. */
function conv(...messages: { role?: string; text?: string; at?: string; id?: string }[]): Conversation[] {
  return [{ id: 'c1', messages }];
}

/** run builds a run of n structural events, all at the same instant. */
function runOf(n: number, at = '2026-09-01T12:00:00Z'): Activity[] {
  return Array.from({ length: n }, (_, i) => ev(i + 1, `session.step.${i}`, at));
}

describe('buildTimeline', () => {
  test('lays both sources on one list, ordered by time', () => {
    const t = buildTimeline(
      conv(
        { role: 'user', text: 'go', at: '2026-09-01T12:00:00Z' },
        { role: 'assistant', text: 'done', at: '2026-09-01T12:02:00Z' },
      ),
      [ev(0, 'session.execution.started', '2026-09-01T12:01:00Z')],
    );
    expect(t.map((e) => (e.kind === 'message' ? e.text : e.name))).toEqual([
      'go',
      'session.execution.started',
      'done',
    ]);
  });

  test('an undated entry keeps arrival order rather than being scattered by NaN', () => {
    const t = buildTimeline(conv({ text: 'first' }, { text: 'second' }), [
      ev(0, 'a.b'),
      ev(1, 'c.d'),
    ]);
    expect(t.map((e) => e.key)).toEqual(['m0.0', 'm0.1', 'e0', 'e1']);
  });

  test('a dated entry sorts after undated ones rather than among them', () => {
    const t = buildTimeline(conv({ text: 'undated' }), [
      ev(0, 'a.b', '2026-09-01T12:00:00Z'),
    ]);
    expect(t.map((e) => e.key)).toEqual(['m0.0', 'e0']);
  });
});

describe('promotion', () => {
  test('the two speaking events render as speech, not machinery', () => {
    const t = buildTimeline([], [
      ev(0, 'session.inbox.enqueued', '2026-09-01T12:00:00Z', 'try this'),
      ev(1, 'session.text.ended', '2026-09-01T12:01:00Z', 'here you go'),
    ]);
    expect(t).toHaveLength(2);
    expect(t[0]).toMatchObject({ kind: 'message', role: 'user', text: 'try this' });
    expect(t[1]).toMatchObject({ kind: 'message', role: 'assistant', text: 'here you go' });
  });

  test('an event that merely carries text stays an event', () => {
    // The machine talking *about* the session, not anyone talking to anyone.
    const t = buildTimeline([], [ev(0, 'session.retry.scheduled', '', 'provider said 503')]);
    expect(t[0]).toMatchObject({ kind: 'event', name: 'session.retry.scheduled' });
  });

  test('a speaking event with no text stays an event', () => {
    const t = buildTimeline([], [ev(0, 'session.text.ended', '')]);
    expect(t[0].kind).toBe('event');
  });

  test('the re-read replaces a promoted event rather than repeating it', () => {
    const said = { role: 'assistant', text: 'here you go', at: '2026-09-01T12:01:00Z' };
    const tail = [ev(0, 'session.text.ended', '2026-09-01T12:01:00Z', ' here you go ')];
    // Before the read the words come from the tail; after it, from the
    // conversation — and only once either way.
    expect(buildTimeline([], tail)).toHaveLength(1);
    const after = buildTimeline(conv(said), tail);
    expect(after).toHaveLength(1);
    expect(after[0].key).toBe('m0.0');
  });

  test('the same words promoted twice are spoken once', () => {
    const t = buildTimeline([], [
      ev(0, 'session.text.ended', '', 'here you go'),
      ev(1, 'session.text.ended', '', 'here you go'),
    ]);
    expect(t.filter((e) => e.kind === 'message')).toHaveLength(1);
  });

  test('a sibling event carrying the same text stays a structural event', () => {
    // Only the two names in `spokenBy` are promoted, so `inbox.delivered`
    // is a box rather than a second copy of the prompt. It still repeats
    // the words inside that box — which is one of the things a collapsed
    // run now covers.
    const t = buildTimeline([], [
      ev(0, 'session.inbox.enqueued', '', 'try this'),
      ev(1, 'session.inbox.delivered', '', 'try this'),
    ]);
    expect(t.map((e) => e.kind)).toEqual(['message', 'event']);
  });
});

describe('keys', () => {
  test('an event keeps its key when a re-read returns one more message', () => {
    // The regression this guards: keys derived from position renumber
    // every event on screen, so a run the reader expanded folds itself
    // up on the next reconnect.
    const tail = [ev(7, 'session.step.started', '2026-09-01T12:05:00Z')];
    const before = buildTimeline(conv({ text: 'one' }), tail);
    const after = buildTimeline(conv({ text: 'one' }, { text: 'two' }), tail);
    const keyOf = (t: typeof before) => t.find((e) => e.kind === 'event')?.key;
    expect(keyOf(before)).toBe('e7');
    expect(keyOf(after)).toBe('e7');
  });
});

describe('blocksOf', () => {
  test('consecutive events group into one run', () => {
    const blocks = blocksOf(buildTimeline([], runOf(4)));
    expect(blocks).toHaveLength(1);
    expect(blocks[0].kind).toBe('events');
    expect(blocks[0].kind === 'events' && blocks[0].events).toHaveLength(4);
  });

  test('a message breaks a run, so a collapse can never hide what was said', () => {
    const t = buildTimeline(
      conv({ role: 'user', text: 'go', at: '2026-09-01T12:00:30Z' }),
      [
        ev(0, 'session.step.a', '2026-09-01T12:00:00Z'),
        ev(1, 'session.step.b', '2026-09-01T12:01:00Z'),
        ev(2, 'session.step.c', '2026-09-01T12:02:00Z'),
      ],
    );
    expect(blocksOf(t).map((b) => b.kind)).toEqual(['events', 'message', 'events']);
  });

  test('a run is identified by its first event, which no later arrival renumbers', () => {
    const blocks = blocksOf(buildTimeline([], [ev(12, 'a.b'), ev(13, 'c.d'), ev(14, 'e.f')]));
    expect(blocks[0].key).toBe('e12');
  });

  // A gap in the recording is the one thing on this page that must stay
  // visible. Folded into a run it would sit behind a "+7" nobody opens.
  //
  // @decision(DL-058)
  test('a seam is its own block and breaks the run around it', () => {
    const blocks = blocksOf(
      buildTimeline(
        [],
        [
          ev(1, 'session.step.a', '2026-09-01T12:00:00Z'),
          marker(2, 'spov.recorder.gap', '2026-09-01T12:01:00Z', 'recording resumed'),
          ev(3, 'session.step.b', '2026-09-01T12:02:00Z'),
        ],
      ),
    );
    expect(blocks.map((b) => b.kind)).toEqual(['events', 'seam', 'events']);
    expect(blocks[1].kind === 'seam' && blocks[1].entry.text).toBe('recording resumed');
  });

  // The marker's key is its store position, so the seam the reader sees
  // before a break is the same block after it.
  //
  // @decision(DL-058)
  test('a seam is keyed by its position', () => {
    const blocks = blocksOf(buildTimeline([], [marker(41, 'spov.recorder.stopped', '', 'gave up')]));
    expect(blocks[0].key).toBe('e41');
  });
});

// A seam is decided by who wrote it, never by what it called itself.
//
// @decision(DL-058)
describe('a seam is decided by source, not by name', () => {
  test('a marker is never promoted to speech, even when it carries text', () => {
    const t = buildTimeline([], [marker(1, 'spov.recorder.text.ended', '', 'recording resumed')]);
    expect(t).toHaveLength(1);
    expect(t[0].kind).toBe('event');
  });

  test('a sandbox frame named like a marker is not a seam', () => {
    const blocks = blocksOf(
      buildTimeline([], [ev(1, 'spov.recorder.gap', '', 'your session was cancelled')]),
    );
    expect(blocks.map((b) => b.kind)).toEqual(['events']);
  });
});

describe('shown', () => {
  const events = (n: number) =>
    blocksOf(buildTimeline([], runOf(n))).flatMap((b) =>
      b.kind === 'events' ? b.events : [],
    ) as EventEntry[];

  // A folded run is drawn as its summary, so it shows no lines of its
  // own. @decision(DL-058)
  test('a run of two folds to its summary', () => {
    expect(CollapseFrom).toBe(2);
    expect(shown(events(1), false)).toHaveLength(1);
    expect(shown(events(2), false)).toEqual([]);
  });

  test('expanding shows every line', () => {
    const all = events(9);
    expect(shown(all, true)).toEqual(all);
  });

  test('a folded run shows no lines whatever its length', () => {
    for (let n = 2; n <= 12; n++) {
      expect(shown(events(n), false)).toEqual([]);
    }
  });
});

describe('seamText', () => {
  test('a real seam shows its own sentence', () => {
    expect(
      seamText({
        name: 'spov.recorder.gap',
        text: 'recording resumed; what happened before this was not captured',
      }),
    ).toBe('recording resumed; what happened before this was not captured');
  });

  test('a machine payload shows its name instead of its contents', () => {
    const evidence =
      '{"event":"setup","at":"2026-09-04T17:14:39.420Z","kind":"report"} ' +
      '{"event":"goal.hooked","at":"2026-09-04T17:14:39.420Z"} '.repeat(60);
    expect(evidence.length).toBeGreaterThan(SeamTextLimit);

    const got = seamText({ name: 'spov.job.evidence', text: evidence });
    expect(got).toBe('spov.job.evidence');
    expect(got).not.toContain('{"event"');
  });

  test('a multi-line marker shows its name however short it is', () => {
    expect(seamText({ name: 'spov.job.evidence', text: 'one\ntwo' })).toBe('spov.job.evidence');
  });

  test('a marker with no text falls back to its name', () => {
    expect(seamText({ name: 'spov.recorder.gave.up', text: '' })).toBe('spov.recorder.gave.up');
  });

  test('a nameless payload is cut rather than shown whole', () => {
    const long = 'x'.repeat(SeamTextLimit * 3);
    const got = seamText({ name: '', text: long });
    expect(got.length).toBe(SeamTextLimit);
  });
});

describe('a message with nothing to show', () => {
  test('is dropped rather than drawn as an empty speaker block', () => {
    const t = buildTimeline(
      [
        {
          id: 'c1',
          messages: [
            { role: 'assistant', text: 'Copying your readme into the delivery file.' },
            { role: 'assistant', text: '' },
            { role: 'assistant', text: '   ' },
            { role: 'assistant', text: 'Done.' },
          ],
        },
      ],
      [],
    );
    expect(t).toHaveLength(2);
    expect(t.map((e) => (e.kind === 'message' ? e.text : ''))).toEqual([
      'Copying your readme into the delivery file.',
      'Done.',
    ]);
  });

  test('an empty message does not suppress an event that quotes nothing', () => {
    // The `said` set stops a promoted event repeating words the
    // conversation already carries. An empty message must not put ""
    // into it, or the first event with no text of its own would be
    // treated as already said.
    const t = buildTimeline(
      [{ id: 'c1', messages: [{ role: 'assistant', text: '' }] }],
      [ev(0, 'session.text.ended', '2026-09-01T12:00:00Z', 'here you go')],
    );
    expect(t).toHaveLength(1);
    expect(t[0]).toMatchObject({ kind: 'message', text: 'here you go' });
  });
});

describe("who spoke", () => {
  // @decision(DL-058) @decision(DL-045)
  test("a synthetic is spov's, not the reader's", () => {
    const out = buildTimeline([], [
      {
        pos: 1,
        name: "session.inbox.enqueued",
        role: "synthetic",
        text: "SPOV_LOOP_STOPPED {\"reason\":\"declared\"}",
        at: "2026-09-05T13:04:00Z",
        source: "opencode",
      } as Activity,
    ]);
    const msg = out.find((e) => e.kind === "message");
    expect(msg, "the synthetic was not promoted to a message").toBeTruthy();
    expect(msg && msg.kind === "message" && msg.role).toBe("synthetic");
  });

  test("a person's prompt is still the reader's", () => {
    const out = buildTimeline([], [
      {
        pos: 1,
        name: "session.inbox.enqueued",
        role: "user",
        text: "please rerun it",
        at: "2026-09-05T13:04:00Z",
        source: "opencode",
      } as Activity,
    ]);
    const msg = out.find((e) => e.kind === "message");
    expect(msg && msg.kind === "message" && msg.role).toBe("user");
  });

  // @decision(DL-058)
  test("an event with no role falls back to the name", () => {
    const out = buildTimeline([], [
      {
        pos: 1,
        name: "session.inbox.enqueued",
        text: "please rerun it",
        at: "2026-09-05T13:04:00Z",
        source: "opencode",
      } as Activity,
    ]);
    const msg = out.find((e) => e.kind === "message");
    expect(msg && msg.kind === "message" && msg.role).toBe("user");
  });

  // `"\n"` is truthy, so an unguarded check would promote a bare
  // newline to a speaker block with nothing in it.
  //
  // @decision(DL-058)
  test("a whitespace-only message is not a message", () => {
    for (const text of ["\n", "   ", "\t\n "]) {
      const out = buildTimeline([], [
        { pos: 1, name: "session.text.ended", role: "assistant", text, at: "2026-09-05T13:04:00Z", source: "opencode" } as Activity,
      ]);
      expect(out.filter((e) => e.kind === "message"), JSON.stringify(text)).toHaveLength(0);
    }
  });
});

describe("the stop marker", () => {
  // @decision(DL-058) @decision(DL-048)
  test("a declared stop reads as a sentence", () => {
    const got = stopNote('SPOV_LOOP_STOPPED {"reason":"declared","continuations":0,"maxContinuations":40,"turns":0}');
    expect(got).toBeTruthy();
    expect(got).not.toContain("{");
    expect(got).not.toContain("SPOV_LOOP_STOPPED");
    expect(got).toContain("declared");
  });

  /**
   * The four reasons the plugin announces, listed here because the
   * plugin is outside the web project's root and Vite will not read it.
   * This pins the wording and shape only, not agreement with the
   * plugin — the Go tests comparing the marker literal to the rendered
   * plugin hold that contract. `paused` is absent: DL-039 says a pause
   * announces nothing.
   *
   * @decision(DL-058) @decision(DL-039)
   */
  test("every reason the plugin announces has wording of its own", () => {
    for (const reason of ["declared", "stopped", "continue-failed", "no-completion-tool", "wall-clock", "denied"]) {
      const got = stopNote(`SPOV_LOOP_STOPPED {"reason":"${reason}"}`);
      expect(got, reason).toBeTruthy();
      expect(got, reason).not.toContain("reason:");
    }
  });

  // Fails soft: null means the caller shows the marker as written.
  //
  // @decision(DL-058)
  test("an unparseable payload falls back to the raw line", () => {
    expect(stopNote("SPOV_LOOP_STOPPED {not json")).toBeNull();
    // A bare marker with no payload is still a stop.
    expect(stopNote("SPOV_LOOP_STOPPED")).toBeTruthy();
  });

  /**
   * The reason is sandbox-authored, so it is data rather than a key.
   *
   * `said["constructor"]` on an object literal yields the Object
   * function, which renders as "function Object() { [native code] }".
   */
  test("a crafted reason cannot reach an inherited property", () => {
    for (const reason of ["constructor", "__proto__", "toString", "hasOwnProperty"]) {
      const got = stopNote(`SPOV_LOOP_STOPPED {"reason":"${reason}"}`) ?? "";
      expect(got, reason).not.toContain("native code");
      expect(got, reason).not.toContain("[object");
      expect(got, reason).toContain(reason);
    }
  });

  /** Nothing bounds a sandbox-written reason but this. */
  test("an enormous reason is bounded", () => {
    const got = stopNote(`SPOV_LOOP_STOPPED {"reason":"${"x".repeat(5000)}"}`) ?? "";
    expect(got.length).toBeLessThan(200);
  });

  /** A turn count that is not a whole number did not happen. */
  test("a non-integer turn count is not shown", () => {
    expect(stopNote('SPOV_LOOP_STOPPED {"reason":"limit","turns":2.5}')).not.toContain("2.5");
    expect(stopNote('SPOV_LOOP_STOPPED {"reason":"limit","turns":1e21}')).not.toContain("e+21");
  });

  test("an unknown reason is shown rather than swallowed", () => {
    const got = stopNote('SPOV_LOOP_STOPPED {"reason":"something-new"}');
    expect(got).toContain("something-new");
  });

  /** Ordinary text is not a marker, and must not be rewritten. */
  test("anything else is not a stop marker", () => {
    expect(stopNote("I have finished the task.")).toBeNull();
    expect(stopNote("")).toBeNull();
    expect(stopNote(undefined)).toBeNull();
    // The token inside a longer message is not a match, exactly as the
    // server requires the text to START with it.
    expect(stopNote("the agent wrote SPOV_LOOP_STOPPED in its reply")).toBeNull();
  });

  test("a turn count is shown only when it says something", () => {
    expect(stopNote('SPOV_LOOP_STOPPED {"reason":"declared","turns":0}')).not.toContain("0 turn");
    expect(stopNote('SPOV_LOOP_STOPPED {"reason":"limit","turns":40}')).toContain("40 turns");
    expect(stopNote('SPOV_LOOP_STOPPED {"reason":"limit","turns":1}')).toContain("1 turn");
  });
});

describe("a transcript warning", () => {
  // @decision(DL-017)
  test("is dropped once anything is on screen", () => {
    const warnings = ["nothing has been recorded for this session"];
    expect(shownWarnings(warnings, 0)).toEqual(warnings);
    expect(shownWarnings(warnings, 1)).toEqual([]);
    expect(shownWarnings(warnings, 40)).toEqual([]);
  });

  /**
   * Derived rather than cleared: a re-read on a gap that finds the same
   * thing again must be able to say so again.
   */
  test("comes back when the transcript is empty again", () => {
    const warnings = ["recording gap: what happened before this was not captured"];
    expect(shownWarnings(warnings, 3)).toEqual([]);
    expect(shownWarnings(warnings, 0)).toEqual(warnings);
  });

  test("having no warnings is not a warning", () => {
    expect(shownWarnings([], 0)).toEqual([]);
    expect(shownWarnings([], 5)).toEqual([]);
  });
});

describe('an echo in the timeline', () => {
  /**
   * The echo and the transcript's copy of the same message have one
   * key, so the page keeps one element and only its fade changes.
   *
   * @decision(DL-059)
   */
  test('shares its key with the message the transcript later draws', () => {
    const [echo] = buildTimeline([], [], [{ key: 'msg_k', id: 'msg_k', at: 10, text: 'wait', note: 'sent' }]);
    const [stored] = buildTimeline(conv({ role: 'user', text: 'wait', id: 'msg_k', at: '2026-09-01T12:00:00Z' }), []);
    const [promoted] = buildTimeline([], [
      { ...ev(5, 'session.inbox.enqueued', '2026-09-01T12:00:00Z', 'wait'), role: 'user', message: 'msg_k' },
    ]);
    expect(echo.key).toBe(messageKey('msg_k'));
    expect(stored.key).toBe(echo.key);
    expect(promoted.key).toBe(echo.key);
  });

  // @decision(DL-059)
  test('lands last even when the browser clock is behind the sandbox', () => {
    const t = buildTimeline(
      conv(
        { role: 'user', text: 'go', at: '2026-09-01T12:00:00Z' },
        { role: 'assistant', text: 'working', at: '2026-09-01T12:05:00Z' },
      ),
      [],
      [{ key: 'msg_k', at: Date.parse('2026-09-01T11:00:00Z'), text: 'wait', note: 'sending…' }],
    );
    expect(t.map((e) => (e.kind === 'message' ? e.text : e.name))).toEqual([
      'go',
      'working',
      'wait',
    ]);
  });

  /** And after an entry that shares its instant exactly. */
  test('sorts after an entry at the same instant', () => {
    const at = '2026-09-01T12:00:00Z';
    const t = buildTimeline(conv({ role: 'assistant', text: 'done', at }), [ev(900, 'session.x', at)], [
      { key: 'msg_k', at: Date.parse(at), text: 'wait', note: 'sending…' },
    ]);
    expect(t[t.length - 1].kind === 'message' && t[t.length - 1].key).toBe('msg:msg_k');
  });

  /**
   * The key is the minted id and nothing renumbers it, so a row cannot
   * be pulled out from under a reader mid-send — and the note is what
   * the entry carries rather than a flag this file would have to
   * interpret. The key is the one the transcript's copy will have
   * (DL-059).
   */
  test('is a user message carrying its own note and key', () => {
    const [entry] = buildTimeline([], [], [
      { key: 'msg_k', at: 10, text: 'wait', note: 'sending…' },
    ]);
    expect(entry).toMatchObject({
      kind: 'message',
      key: 'msg:msg_k',
      role: 'user',
      text: 'wait',
      pending: 'sending…',
    });
  });

  /** Several at once keep the order they were sent in. */
  test('keeps two echoes in the order they were sent', () => {
    const t = buildTimeline([], [], [
      { key: 'msg_a', at: 10, text: 'first', note: 'sent' },
      { key: 'msg_b', at: 10, text: 'second', note: 'sending…' },
    ]);
    expect(t.map((e) => e.key)).toEqual(['msg:msg_a', 'msg:msg_b']);
  });

  /** Nothing pending changes nothing: the ordinary page is this one. */
  test('changes nothing when there is none', () => {
    const messages = conv({ role: 'user', text: 'go', at: '2026-09-01T12:00:00Z' });
    expect(buildTimeline(messages, [], [])).toEqual(buildTimeline(messages, []));
  });
});

describe('two messages that say the same thing', () => {
  /** A user frame, as the live stream delivers one. */
  function said(pos: number, message: string, text: string, role = 'user'): Activity {
    return { ...ev(pos, 'session.inbox.enqueued', `2026-09-01T12:0${pos}:00Z`, text), role, message };
  }

  // @decision(DL-059) @decision(DL-019)
  test('are both shown when a person sent them both', () => {
    const t = buildTimeline([], [said(1, 'msg_a', 'ok'), said(2, 'msg_b', 'ok')]);
    expect(t.filter((e) => e.kind === 'message').map((e) => e.text)).toEqual(['ok', 'ok']);
  });

  /**
   * And the same frame twice is still once: identity is the message,
   * not the arrival.
   */
  test('are one when they are the same message', () => {
    const t = buildTimeline([], [said(1, 'msg_a', 'ok'), said(2, 'msg_a', 'ok')]);
    expect(t.filter((e) => e.kind === 'message')).toHaveLength(1);
  });

  /**
   * spov's own continuation is the opposite case and keeps the text
   * rule: the same sentence said again is machinery repeating itself,
   * and a forty-turn job showing forty identical continuation blocks
   * would bury the turns between them.
   *
   * @decision(DL-058)
   */
  test('are one when spov said them both', () => {
    const t = buildTimeline(
      [],
      [said(1, 'msg_a', 'carry on', 'synthetic'), said(2, 'msg_b', 'carry on', 'synthetic')],
    );
    expect(t.filter((e) => e.kind === 'message')).toHaveLength(1);
  });

  /**
   * And still one once a PERSON has spoken in the same session, which
   * is the shape that isolates the rule. With no human message the
   * `identified` guard alone happens to collapse them, so a version
   * that applied identity to every speaker passed — and then drew a
   * continuation block per turn on any session that had been steered.
   *
   * @decision(DL-059) @decision(DL-039)
   */
  test('are one when spov said them both after a person spoke', () => {
    const t = buildTimeline(
      [],
      [
        said(1, 'msg_h', 'go on'),
        said(2, 'msg_a', 'carry on', 'synthetic'),
        said(3, 'msg_b', 'carry on', 'synthetic'),
      ],
    );
    expect(t.filter((e) => e.kind === 'message' && e.text === 'carry on')).toHaveLength(1);
  });

  /**
   * A transcript stored before the id existed has none to compare, so
   * the text rule is still the only answer there — comparing by an id
   * nothing carries would promote every frame over the message it is
   * repeating.
   */
  test('are one when the transcript carries no ids to tell them apart', () => {
    const t = buildTimeline(conv({ role: 'user', text: 'ok', at: '2026-09-01T12:00:00Z' }), [
      said(1, 'msg_a', 'ok'),
    ]);
    expect(t.filter((e) => e.kind === 'message')).toHaveLength(1);
  });

  /** And an event repeating a message the transcript already has, by id. */
  test('are one when the transcript already carries that message', () => {
    const t = buildTimeline(
      [{ id: 'c1', messages: [{ id: 'msg_a', role: 'user', text: 'ok', at: '2026-09-01T12:00:00Z' }] }],
      [said(1, 'msg_a', 'ok')],
    );
    expect(t.filter((e) => e.kind === 'message')).toHaveLength(1);
  });
});

describe('the id fallback', () => {
  function said(pos: number, message: string, text: string, role = 'user'): Activity {
    return { ...ev(pos, 'session.inbox.enqueued', `2026-09-01T12:0${pos}:00Z`, text), role, message };
  }

  // Only a person's message sets `identified`, or an assistant id in an
  // otherwise id-less transcript would disable the text fallback the
  // user messages still need.
  //
  // @decision(DL-059)
  test('survives a transcript whose only id is the agent\'s', () => {
    const t = buildTimeline(
      [
        {
          id: 'c1',
          messages: [
            { role: 'user', text: 'ok', at: '2026-09-01T12:00:00Z' },
            { id: 'msg_reply', role: 'assistant', text: 'sure', at: '2026-09-01T12:00:30Z' },
          ],
        },
      ],
      [said(1, 'msg_a', 'ok')],
    );
    expect(t.filter((e) => e.kind === 'message' && e.text === 'ok')).toHaveLength(1);
  });
});

describe("spov's own rows", () => {
  /** One of spov's markers, as the recorder stores it. */
  function mark(pos: number, name: string, text: string): Activity {
    return { ...marker(pos, name, `2026-09-01T12:0${pos}:00Z`, text), source: 'spov' };
  }

  // @decision(DL-059) @decision(DL-019)
  test('keep the audit trail out of the conversation', () => {
    const t = buildTimeline([], [
      mark(1, 'spov.session.steered', 'message sent by 01M2AF8F42XP9WXXHD8ARV3J48: msg_01a0ab24d164'),
      mark(2, 'spov.job.interrupted', 'turn stopped by 01M2AF8F42XP9WXXHD8ARV3J48'),
      mark(3, 'spov.job.resumed', 'resumed by 01M2AF8F42XP9WXXHD8ARV3J48'),
    ]);
    expect(t).toEqual([]);
  });

  // What hiding the interrupt marker costs, pinned deliberately: nothing
  // else on the page says a person ended this turn.
  //
  // @decision(DL-059) @decision(DL-053)
  test('and the turn ending is still drawn without them', () => {
    const t = buildTimeline([], [
      ev(1, 'session.execution.interrupted', '2026-09-01T12:01:00Z'),
      mark(2, 'spov.job.interrupted', 'turn stopped by 01M2AF8F42XP9WXXHD8ARV3J48'),
    ]);
    expect(t.map((e) => (e.kind === 'event' ? e.name : ''))).toEqual([
      'session.execution.interrupted',
    ]);
  });

  // @decision(DL-058) @decision(DL-017)
  test('but never the ones that say the recording is incomplete', () => {
    const t = buildTimeline([], [
      mark(1, 'spov.recorder.gap', 'recording gap: 58 events before this were not captured'),
      mark(2, 'spov.recorder.stopped', 'recording stopped'),
      mark(3, 'spov.job.collect.failed', 'the out directory could not be read'),
    ]);
    expect(t.map((e) => (e.kind === 'event' ? e.name : ''))).toEqual([
      'spov.recorder.gap',
      'spov.recorder.stopped',
      'spov.job.collect.failed',
    ]);
    expect(blocksOf(t).map((b) => b.kind)).toEqual(['seam', 'seam', 'seam']);
  });

  // The verdict is what these are read into; drawn, they are a marker's
  // name in the rust of a gap. @decision(DL-058) @decision(DL-051)
  test('keep the records the verdict is judged from out of the transcript', () => {
    const t = buildTimeline([], [
      mark(1, 'spov.job.evidence', '{"event":"check.ran"}'),
      mark(2, 'spov.job.outcome', '{"event":"completion.declared"}'),
    ]);
    expect(t).toEqual([]);
  });

  // @decision(DL-058)
  test('draw what spov did with the job as a quiet line, not a gap', () => {
    const blocks = blocksOf(buildTimeline([], [
      mark(1, 'spov.job.collected', 'Collected 2 reports and 1 image from /workspace/out.'),
      mark(2, 'spov.job.bounded', 'the job reached its 2m0s wall clock after 2m7s unattended'),
    ]));
    expect(blocks.map((b) => b.kind)).toEqual(['note', 'note']);
  });

  // @decision(DL-021)
  test('a failed token renewal is a quiet line too, drawn in full', () => {
    const text =
      "Could not renew the git token (resolving acme/widgets: no credential for platform \"github\": the organisation's credential this session launched with is gone, and no other credential is used in its place); git access through it ends at 20:18 UTC.";
    const blocks = blocksOf(buildTimeline([], [mark(1, 'spov.session.credentials.renewal.failed', text)]));
    expect(blocks.map((b) => b.kind)).toEqual(['note']);
    expect(noteText({ name: 'spov.session.credentials.renewal.failed', text })).toBe(text);
    expect(noteText({ name: 'n', text: 'a\nb' })).toBe('a b');
    expect(noteText({ name: 'n', text: 'x'.repeat(500) })).toHaveLength(400);
    expect(noteText({ name: 'n' })).toBe('n');
  });

  // @decision(DL-058)
  test('and a sandbox cannot draw a note or hide a record by borrowing the name', () => {
    const forged = (name: string): Activity => ({ ...ev(1, name, '', 'x'), source: 'opencode' });
    expect(buildTimeline([], [forged('spov.job.evidence')])).toHaveLength(1);
    expect(blocksOf(buildTimeline([], [forged('spov.job.collected')])).map((b) => b.kind)).toEqual(['events']);
  });

  /**
   * The default is SHOWN. A marker this build has never heard of is
   * drawn rather than swallowed — the posture DL-050 takes for a gate
   * line it cannot parse.
   */
  test('and never one this build has not heard of', () => {
    const t = buildTimeline([], [mark(1, 'spov.something.new', 'something happened')]);
    expect(t).toHaveLength(1);
  });

  /**
   * The hide is asked only of rows already known to be spov's.
   * `isSeam` stays keyed on `source`, which is the TRUST rule: a
   * sandbox chooses event names, so a sandbox naming its own event
   * `spov.session.steered` must not be able to make the page drop it —
   * or hide anything else by borrowing a name.
   *
   * @decision(DL-058)
   */
  test('and a sandbox cannot hide an event by borrowing the name', () => {
    const forged: Activity = {
      ...ev(1, 'spov.session.steered', '2026-09-01T12:01:00Z', 'nothing to see here'),
      source: 'opencode',
    };
    expect(buildTimeline([], [forged])).toHaveLength(1);
  });
});

// @decision(DL-058)
describe('condense', () => {
  /** A tool event as the stream carries it. */
  function tool(pos: number, name: string, call: string, extra: Partial<Activity> = {}): Activity {
    return { ...ev(pos, `session.tool.${name}`, `2026-09-01T12:00:0${pos}Z`), call, ...extra };
  }

  test('drops plumbing but keeps a name it has not heard of', () => {
    const t = condense(buildTimeline([], [
      ev(1, 'session.step.started'),
      ev(2, 'session.reasoning.started'),
      ev(3, 'session.inbox.delivered', '', 'try this'),
      ev(4, 'session.something.new', '', 'news'),
      ev(5, 'session.step.ended'),
      ev(6, 'session.agent.selected'),
    ]));
    expect(t.map((e) => (e.kind === 'event' ? e.name : ''))).toEqual(['session.something.new']);
  });

  test('drops a speech event that said nothing, and keeps one that failed to promote for another reason', () => {
    const t = condense(buildTimeline([], [ev(1, 'session.text.ended'), ev(2, 'session.retry.scheduled', '', '503')]));
    expect(t.map((e) => (e.kind === 'event' ? e.name : ''))).toEqual(['session.retry.scheduled']);
  });

  test('joins a call into one line at its first event', () => {
    const t = condense(buildTimeline([], [
      tool(1, 'input.started', 'call_a', { tool: 'shell' }),
      tool(2, 'input.started', 'call_b', { tool: 'read' }),
      tool(3, 'input.ended', 'call_a', { text: '{"command":"ls -la"}' }),
      tool(4, 'input.ended', 'call_b', { text: '{"path":"README.md"}' }),
      tool(5, 'called', 'call_a'),
      tool(6, 'success', 'call_a'),
      tool(7, 'failed', 'call_b', { text: 'no such file' }),
    ]));
    expect(t).toHaveLength(2);
    expect(t.map((e) => (e.kind === 'event' ? e.calls : undefined))).toEqual([
      { name: 'shell', input: '{"command":"ls -la"}', state: 'done', error: '' },
      { name: 'read', input: '{"path":"README.md"}', state: 'failed', error: 'no such file' },
    ]);
    expect(t[0].key).toBe('e1');
  });

  test('a call whose ending has not arrived is still running', () => {
    const t = condense(buildTimeline([], [tool(1, 'input.started', 'call_a', { tool: 'shell' })]));
    expect(t[0].kind === 'event' && t[0].calls?.state).toBe('running');
  });

  test('joins a call across a message, at the line where it started', () => {
    const t = condense(buildTimeline(conv({ role: 'assistant', text: 'working', at: '2026-09-01T12:00:02Z' }), [
      tool(1, 'input.started', 'call_a', { tool: 'shell' }),
      tool(3, 'success', 'call_a'),
    ]));
    expect(t.map((e) => e.kind)).toEqual(['event', 'message']);
    expect(t[0].kind === 'event' && t[0].calls?.state).toBe('done');
  });

  test('leaves a tool event with no call as it is, from a server that sends none', () => {
    const t = condense(buildTimeline([], [ev(1, 'session.tool.success'), ev(2, 'session.tool.called')]));
    expect(t).toHaveLength(2);
    expect(t.every((e) => e.kind === 'event' && !e.calls)).toBe(true);
  });

  test("never joins spov's own rows", () => {
    const t = condense(buildTimeline([], [{ ...marker(1, 'session.tool.success', '', 'x'), call: 'call_a' }]));
    expect(t[0].kind === 'event' && t[0].calls).toBeUndefined();
  });

  test('does not change what it was given', () => {
    const entries = buildTimeline([], [tool(1, 'input.started', 'call_a', { tool: 'shell' })]);
    condense(entries);
    expect(entries[0].kind === 'event' && entries[0].calls).toBeUndefined();
  });
});

// @decision(DL-058)
describe('toolInput', () => {
  test('picks the field that says what the call was about', () => {
    expect(toolInput('{"command":"ls -la","workdir":"/workspace"}')).toBe('ls -la');
    expect(toolInput('{"limit":20,"offset":3,"path":"/workspace/README.md"}')).toBe('/workspace/README.md');
    expect(toolInput('{"path":"/workspace","pattern":"*.go"}')).toBe('/workspace');
    expect(toolInput('{"url":"https://example.com"}')).toBe('https://example.com');
  });

  test('flattens a multi-line command', () => {
    expect(toolInput('{"command":"cd x &&\\n  ls"}')).toBe('cd x && ls');
  });

  test('finds a field in input that was cut short', () => {
    expect(toolInput('{"path":"/workspace/out/a.md","content":"# Report\\n\\nlong and cut')).toBe('/workspace/out/a.md');
  });

  test("says what the question tool asks", () => {
    const asked = JSON.stringify({
      questions: [
        { question: "Which language?", header: "Summary language", options: [{ label: "English" }] },
        { question: "How long?", header: "Length", options: [] },
      ],
    });
    expect(toolInput(asked)).toBe("Which language? · How long?");
    expect(toolInput('{"questions": [{"question": "Which language?", "header": "Summary language", "options": [{"label": "Eng')).toBe(
      "Which language?",
    );
  });

  test('falls back to the input itself, bounded', () => {
    expect(toolInput('{"questions":[1]}')).toBe('{"questions":[1]}');
    expect(toolInput('{}')).toBe('');
    expect(toolInput('')).toBe('');
    expect(toolInput(`{"command":"${'x'.repeat(500)}"}`).length).toBeLessThanOrEqual(201);
  });
});

// @decision(DL-058)
describe('runSummary', () => {
  const entry = (over: Partial<EventEntry>): EventEntry => ({
    kind: 'event', key: 'e', at: 0, seq: 0, name: 'session.tool.input.started', text: '', seam: false, ...over,
  });
  const call = (state: 'done' | 'failed') => entry({ calls: { name: 'shell', input: '', state, error: '' } });

  test('counts tool calls, failures and thoughts', () => {
    expect(runSummary([call('done'), call('failed'), call('done'), entry({ name: 'session.reasoning.ended' })])).toBe(
      '3 tool calls, 1 failed, 1 thought',
    );
  });

  test('names other events for what they are', () => {
    expect(runSummary([entry({ name: 'session.step.failed' }), entry({ name: 'session.renamed' })])).toBe('2 events');
    expect(runSummary([call('done'), entry({ name: 'session.renamed' })])).toBe('1 tool call, 1 other event');
  });

  // @decision(DL-058)
  test('names a model retry for what it is', () => {
    const retry = entry({ name: 'session.retry.scheduled', text: 'Provider returned error' });
    expect(runSummary([call('done'), retry, retry])).toBe('1 tool call, 2 model retries');
    expect(runSummary([retry])).toBe('1 model retry');
  });
});

// @decision(DL-058)
describe('trimBlankLines', () => {
  test('drops blank lines at either end and keeps the first line indented', () => {
    expect(trimBlankLines('Now the SVG diagram:\n\n')).toBe('Now the SVG diagram:');
    expect(trimBlankLines('\n  \n  indented\nnext  \n')).toBe('  indented\nnext');
    expect(trimBlankLines('a\n\nb')).toBe('a\n\nb');
  });
});

// @decision(DL-019) @decision(DL-048)
describe("a person's queued message", () => {
  const enqueued = (pos: number, id: string, at: string, text = 'later please'): Activity => ({
    ...ev(pos, 'session.inbox.enqueued', at, text),
    role: 'user',
    message: id,
    delivery: 'queue',
  });
  const about = (pos: number, name: string, id: string, at: string): Activity => ({
    ...ev(pos, name, at),
    message: id,
  });

  test('reads queued, processed and cancelled from the log', () => {
    const log = [
      enqueued(1, 'msg_a', '2026-09-01T12:00:00Z'),
      enqueued(2, 'msg_b', '2026-09-01T12:00:01Z'),
      enqueued(3, 'msg_c', '2026-09-01T12:00:02Z'),
      about(4, 'session.inbox.delivered', 'msg_a', '2026-09-01T12:00:05Z'),
      about(5, 'session.inbox.cancelled', 'msg_b', '2026-09-01T12:00:06Z'),
    ];
    const got = messageStatuses(log, false);
    expect(got.get('msg_a')?.status).toBe('processed');
    expect(got.get('msg_b')?.status).toBe('cancelled');
    expect(got.get('msg_c')?.status).toBe('queued');
    expect(messageStatuses(log, true).get('msg_c')?.status).toBe('unprocessed');
    // Processed and cancelled are final; only a waiting one becomes
    // unprocessed.
    expect(messageStatuses(log, true).get('msg_a')?.status).toBe('processed');
  });

  test("takes nothing from spov's rows or from spov's own items", () => {
    const log: Activity[] = [
      { ...enqueued(1, 'msg_s', '2026-09-01T12:00:00Z'), role: 'synthetic' },
      { ...about(2, 'session.inbox.delivered', 'msg_x', '2026-09-01T12:00:01Z'), source: 'spov' },
    ];
    expect(messageStatuses(log, false).size).toBe(0);
  });

  test('waits below the log, then moves to where it was read', () => {
    const work = ev(2, 'session.text.ended', '2026-09-01T12:00:03Z', 'still working');
    const waiting = buildTimeline([], [enqueued(1, 'msg_a', '2026-09-01T12:00:00Z'), work]);
    expect(waiting.map((e) => e.key)).toEqual(['e2', 'msg:msg_a']);
    expect(waiting[1]).toMatchObject({ kind: 'message', id: 'msg_a', status: 'queued' });

    const read = buildTimeline(
      [],
      [
        enqueued(1, 'msg_a', '2026-09-01T12:00:00Z'),
        work,
        about(3, 'session.inbox.delivered', 'msg_a', '2026-09-01T12:00:04Z'),
        ev(4, 'session.text.ended', '2026-09-01T12:00:05Z', 'done with that'),
      ],
    );
    expect(read.map((e) => e.key)).toEqual(['e2', 'msg:msg_a', 'e3', 'e4']);
    expect(read[1]).toMatchObject({ status: 'processed' });
  });

  test('carries how it was sent and what follows it', () => {
    const got = buildTimeline([], [{ ...enqueued(1, 'msg_a', '2026-09-01T12:00:00Z'), after: 'handover' }]);
    expect(got[0]).toMatchObject({ delivery: 'queue', after: 'handover' });
  });

  test('says when the run ended without reading it', () => {
    const got = buildTimeline([], [enqueued(1, 'msg_a', '2026-09-01T12:00:00Z')], [], true);
    expect(got[0]).toMatchObject({ status: 'unprocessed' });
  });
});

// @decision(DL-058)
describe('time dividers', () => {
  const msg = (key: string, iso: string): Block => ({
    kind: 'message',
    key,
    entry: { kind: 'message', key, at: iso ? Date.parse(iso) : 0, seq: 0, role: 'assistant', text: 'x' },
  });
  const now = new Date('2026-09-01T23:00:00');

  test('marks the start and a quiet gap, and nothing in between', () => {
    const got = timeDividers(
      [
        msg('a', '2026-09-01T12:00:00'),
        msg('b', '2026-09-01T12:05:00'),
        msg('c', '2026-09-01T12:30:00'),
      ],
      now,
    );
    expect([...got.keys()]).toEqual(['a', 'c']);
    expect(got.get('a')).toMatch(/12.00/);
  });

  test('adds the date on another day', () => {
    const got = timeDividers([msg('a', '2026-08-31T22:00:00'), msg('b', '2026-09-01T09:00:00')], now);
    expect(got.get('a')).toMatch(/,/);
    expect(got.get('b')).toMatch(/,/);
  });

  test('draws none next to an undated block', () => {
    const got = timeDividers([msg('a', ''), msg('b', '2026-09-01T12:00:00'), msg('c', '2026-09-01T13:00:00')], now);
    expect([...got.keys()]).toEqual(['c']);
  });
});
