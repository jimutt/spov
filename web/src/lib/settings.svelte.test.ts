import { describe, expect, it } from "vitest";
import { flushSync } from "svelte";
import { emptyField, seedField, type Field } from "./settings";

/**
 * The one test in the app that runs runes rather than plain functions:
 * `seedField` is folded by an `$effect` that reads the field and writes
 * it back, and a fold returning a fresh object every pass re-dirties
 * its own dependency and reschedules itself forever. Nothing else in
 * `check.sh` catches that — `astro check` type-checks without running
 * anything, and the build compiles without mounting.
 *
 * @decision(DL-062)
 */
describe("the effect that folds a profile into a field", () => {
  function fold(current: () => string) {
    let runs = 0;
    let loaded = $state(false);
    let field = $state<Field>(emptyField());
    const stop = $effect.root(() => {
      $effect(() => {
        runs++;
        field = seedField(loaded, current(), field);
      });
    });
    flushSync();
    return {
      arrive() {
        loaded = true;
        flushSync();
      },
      get runs() {
        return runs;
      },
      get field() {
        return field;
      },
      stop,
    };
  }

  it("settles instead of rescheduling itself forever", () => {
    const f = fold(() => "anthropic/claude-sonnet-5");
    const before = f.runs;
    // Throws effect_update_depth_exceeded if seedField allocates on a
    // pass that changed nothing.
    f.arrive();
    expect(f.field.value).toBe("anthropic/claude-sonnet-5");
    expect(f.runs - before).toBeLessThan(4);
    f.stop();
  });

  it("settles again when the reader has diverged", () => {
    const f = fold(() => "anthropic/claude-sonnet-5");
    f.arrive();
    const before = f.runs;
    // The other branch allocates too, and loops just as readily.
    f.field.value = "anthropic/claude-opus-5";
    flushSync();
    expect(f.field.value).toBe("anthropic/claude-opus-5");
    expect(f.runs - before).toBeLessThan(4);
    f.stop();
  });

  it("settles when nothing was ever stored", () => {
    const f = fold(() => "");
    f.arrive();
    expect(f.field.stored).toBe("");
    f.stop();
  });
});
