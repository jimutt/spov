import type { Injected } from "./timeline";

/**
 * The words for what OpenCode or a plugin added to a conversation by
 * itself (`Injected`, named by the server). Every one is drawn folded and
 * never tagged spov.
 *
 * @decision(DL-180)
 */
export type ShellResult = NonNullable<Injected["shell"]>;

/** The fold's label for anything but a shell result. */
export function injectedLabel(injected: Injected | undefined): string {
  return injected?.summary?.trim() || "Added by OpenCode or a plugin";
}

/** Who ran a shell command: the agent in the background, or a person. */
export function shellWho(shell: ShellResult): string {
  return shell.by_person ? "Run by a person" : "Background command";
}

/** The states a shell result's words are known for; any other is shown as OpenCode names it. */
const KnownStates = new Set(["", "completed", "cancelled", "error"]);

/** How a shell command ended, in a few words. */
export function shellEnd(shell: ShellResult): string {
  const state = shell.state ?? "";
  if (state === "cancelled") return "cancelled";
  if (state === "error") return shell.by_person ? "failed to start" : "failed";
  if (!KnownStates.has(state)) return state;
  if (shell.exit !== undefined && shell.exit !== 0) return `exit ${shell.exit}`;
  return "finished";
}

/** A shell result's end reads as a failure: anything but a clean finish or a cancel. */
export function shellFailed(shell: ShellResult): boolean {
  const state = shell.state ?? "";
  if (state === "cancelled") return false;
  return !KnownStates.has(state) || state === "error" || (shell.exit !== undefined && shell.exit !== 0);
}

/** The command's first line, for the folded line; the whole is shown opened. */
export function commandLine(command: string): string {
  const lines = command.trim().split("\n");
  return lines.length > 1 ? `${lines[0]} …` : lines[0];
}
