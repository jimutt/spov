import type { components } from "./api/types";
import { withoutVariant } from "./jobform";

/**
 * A running session's reasoning modes: the variants each model offers
 * in that session's sandbox, as `GET /sessions/{ref}/model-variants`
 * serves them, and how a choice of model or mode becomes the reference
 * the model switch takes.
 *
 * @decision(DL-161) @decision(DL-160)
 */

/** What the server says the session's sandbox offers. */
export type ModelVariants = components["schemas"]["ModelVariantsView"];

/** How often, and how many times, a list not read yet is asked again. */
export const VariantsRetryMs = 15_000;
export const VariantsRetries = 8;

/**
 * modeOf is a reference's `#variant`, "" for none: the half
 * withoutVariant drops.
 *
 * @decision(DL-161)
 */
export function modeOf(ref: string): string {
  const bare = withoutVariant(ref);
  return bare === ref ? "" : ref.slice(bare.length + 1);
}

/**
 * modesOf is what the list offers for a reference's model, in OpenCode's
 * order; none while the list is not read or when the model has none.
 *
 * @decision(DL-161)
 */
export function modesOf(list: ModelVariants | undefined, ref: string): string[] {
  if (!list?.read || !ref) return [];
  return list.models[withoutVariant(ref)] ?? [];
}

/**
 * withMode is a reference's model with a mode, or with none for "",
 * which selects the model's default.
 *
 * @decision(DL-161)
 */
export function withMode(ref: string, mode: string): string {
  const model = withoutVariant(ref);
  return mode ? `${model}#${mode}` : model;
}

/**
 * switchedTo is the reference a choice of another model sends: the
 * current mode when the new model offers one of that id, else its
 * default.
 *
 * @decision(DL-161)
 */
export function switchedTo(list: ModelVariants | undefined, current: string, next: string): string {
  const mode = modeOf(current);
  return withMode(next, mode && modesOf(list, next).includes(mode) ? mode : "");
}

/**
 * modelChip is how the phone's chip names a model: its id without the
 * provider, then its mode, if any (`claude-opus-5 · high`); "" for none.
 *
 * @decision(DL-161) @decision(DL-117)
 */
export function modelChip(ref: string): string {
  if (!ref) return "";
  const model = withoutVariant(ref);
  const short = model.split("/").pop() || model;
  const mode = modeOf(ref);
  return mode ? `${short} · ${mode}` : short;
}
