// The app's controls, as class strings shared by every component. Three
// button kinds and one field style. A form or a section has at most one
// primary button; danger is for acts that end something.
//
// @decision(DL-056)

const focus = 'focus-visible:outline-2 focus-visible:outline-olive focus-visible:outline-offset-2';

const shape =
  'inline-flex items-center justify-center gap-1.5 cursor-pointer rounded-ctl border ' +
  'leading-tight font-medium no-underline whitespace-nowrap ' +
  'disabled:cursor-default disabled:opacity-50 aria-disabled:cursor-default aria-disabled:opacity-50 ' +
  focus;
const base = `${shape} px-3 py-1.5 text-md`;

const filledOlive = 'border-olive-b bg-olive-b text-on-accent hover:brightness-110';

/** Filled olive: the one action a view is for. */
export const primary = `${base} ${filledOlive}`;

/** The same, a size up, for the button that spends: Start job. */
export const primaryLarge = `${shape} px-4.5 py-2 text-base ${filledOlive}`;

const raised = 'border-rule-2 bg-raised text-ink hover:bg-sunk';

/** Raised and bordered: every other action. */
export const secondary = `${base} ${raised}`;

/** The same, a size down, for a floating control: Jump to latest. */
export const secondarySmall = `${shape} px-3 py-1.5 text-sm ${raised}`;

/** An act that ends something, before it is confirmed. */
export const danger = `${base} border-rule-2 bg-raised text-rust hover:border-rust`;

/** The confirming second press of a danger act. */
export const dangerConfirm = `${base} border-rust bg-rust text-on-accent hover:brightness-110`;

/** A link-like text button: "6 more events", "Show fewer". */
export const textButton =
  'inline-flex cursor-pointer items-center gap-1 border-0 bg-transparent p-0 text-sm text-soft underline ' +
  `decoration-rule-2 underline-offset-[3px] hover:text-ink ${focus}`;

/**
 * A field's box without its padding or size, for a caller that sets its
 * own. Two utilities for one property do not override each other by
 * their order in the class, so the variants below never stack.
 */
export const fieldBase =
  'block w-full rounded-ctl border border-rule-2 bg-raised text-ink placeholder:text-faint ' +
  'disabled:opacity-60 focus-visible:outline-2 focus-visible:outline-olive focus-visible:outline-offset-1';

/** A text input, select or textarea. */
export const field = `${fieldBase} px-2.5 py-2 text-base`;

/** The same field, for a value that is machine text. */
export const fieldMono = `${fieldBase} px-2.5 py-2 font-mono text-md`;

/** A form label or legend. */
export const label = 'mb-1.5 block p-0 text-base font-semibold text-ink';

/** One sentence of help under a field. */
export const help = 'mt-1.5 max-w-[64ch] text-md text-soft';

/** A disclosure under a field, for the help that does not fit one sentence. */
export const more = help;

/** Its summary: a text button that names the question it answers. */
export const moreSummary =
  'inline-flex cursor-pointer list-none items-center gap-1 underline decoration-rule-2 ' +
  `underline-offset-[3px] hover:text-ink ${focus}`;

/** A page's own heading. */
export const pageTitle = 'm-0 text-xl font-semibold tracking-[-0.01em]';

/** A section's heading inside a page. */
export const sectionTitle = 'm-0 text-lg font-semibold';

/** A link set in running text. It takes its colour from the caller. */
export const link = `underline decoration-rule-2 underline-offset-[3px] hover:decoration-ink ${focus}`;
