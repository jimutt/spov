import { ApiError } from "./api/client";

/**
 * Options for a Resource.
 *
 * `every` polls; without it a resource loads once and then only on the
 * events below. A polled resource is paused while the document is
 * hidden — a background tab that keeps asking is a request the viewer
 * cannot see the answer to, and on the usage page it would be one a
 * minute for as long as the tab exists.
 */
export interface ResourceOptions {
  /** Poll interval in milliseconds. Omitted means load once. */
  every?: number;
  /**
   * Load only while this returns true, and start as soon as it turns
   * true. Read synchronously inside the effect, so a rune it depends on
   * is tracked. Not an optimisation: a resource that loads before its
   * page knows whether anyone is signed in turns a 401 into a redirect.
   *
   * @decision(DL-055)
   */
  when?: () => boolean;
}

/**
 * Resource is one endpoint's state: the last body that arrived, the last
 * failure, and whether a request is in flight. Loads on mount, reloads
 * when the tab becomes visible again, aborts in flight when the
 * component goes away. Construct it during component initialisation —
 * it registers an `$effect`, only legal there.
 *
 * @decision(DL-055)
 */
export class Resource<T> {
  /** The most recent successful body. */
  data = $state<T | undefined>(undefined);
  /** The most recent failure, cleared by the next success. */
  error = $state<ApiError | Error | undefined>(undefined);
  /** Whether a request is in flight. */
  loading = $state(false);
  /** Whether a body has ever arrived, so a page can tell "empty" from
   *  "not yet". */
  loaded = $state(false);

  #load: (signal: AbortSignal) => Promise<T>;
  #every: number | undefined;
  #when: (() => boolean) | undefined;
  #inflight: AbortController | undefined;

  constructor(load: (signal: AbortSignal) => Promise<T>, opts: ResourceOptions = {}) {
    this.#load = load;
    this.#every = opts.every;
    this.#when = opts.when;

    $effect(() => {
      // Read first and synchronously, so the effect re-runs when the
      // gate opens.
      if (this.#when && !this.#when()) return;
      void this.refresh();

      const onVisible = () => {
        if (document.visibilityState === "visible") void this.refresh();
      };
      document.addEventListener("visibilitychange", onVisible);

      // A hidden tab is not polled: the timer keeps ticking, but the
      // request is skipped and the visibility handler above catches the
      // page up the moment it is looked at again.
      const timer =
        this.#every === undefined
          ? undefined
          : setInterval(() => {
              if (document.visibilityState === "visible") void this.refresh();
            }, this.#every);

      return () => {
        document.removeEventListener("visibilitychange", onVisible);
        if (timer !== undefined) clearInterval(timer);
        this.#inflight?.abort();
        this.#inflight = undefined;
      };
    });
  }

  /**
   * refresh reloads now. A request already in flight is abandoned, so a
   * visibility change during a slow load cannot leave the older answer
   * arriving last and overwriting the newer one.
   */
  async refresh(): Promise<void> {
    this.#inflight?.abort();
    const ctl = new AbortController();
    this.#inflight = ctl;
    this.loading = true;
    try {
      const data = await this.#load(ctl.signal);
      if (ctl.signal.aborted) return;
      this.data = data;
      this.error = undefined;
      this.loaded = true;
    } catch (err) {
      if (ctl.signal.aborted) return;
      // The previous body is kept: a blip must not blank a page that
      // was showing something true a second ago.
      this.error = err instanceof Error ? err : new Error(String(err));
    } finally {
      // Only the newest request owns the flag; an aborted one has
      // already been replaced by the request that aborted it.
      if (this.#inflight === ctl) {
        this.loading = false;
        this.#inflight = undefined;
      }
    }
  }
}

/**
 * resource is the constructor as a function, for the common case.
 *
 * @decision(DL-055)
 */
export function resource<T>(
  load: (signal: AbortSignal) => Promise<T>,
  opts: ResourceOptions = {},
): Resource<T> {
  return new Resource(load, opts);
}
