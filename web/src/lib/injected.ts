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

/** How a shell command ended, in a few words. */
export function shellEnd(shell: ShellResult): string {
  if (shell.state === "cancelled") return "cancelled";
  if (shell.state === "error") return "failed to start";
  if (shell.exit !== undefined && shell.exit !== 0) return `exit ${shell.exit}`;
  return "finished";
}

/** A shell result's end reads as a failure. */
export function shellFailed(shell: ShellResult): boolean {
  return shell.state === "error" || (shell.exit !== undefined && shell.exit !== 0);
}

/** The command's first line, for the folded line; the whole is shown opened. */
export function commandLine(command: string): string {
  const lines = command.trim().split("\n");
  return lines.length > 1 ? `${lines[0]} …` : lines[0];
}
