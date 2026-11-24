package opencode

import (
	"fmt"
	"path"
	"strings"
)

// AgentsFile is where the generated workspace guidance is written.
// OpenCode discovers AGENTS.md by walking up from its working directory,
// so a file at the workspace root is visible from every cloned repo.
//
// @decision(DL-037)
const AgentsFile = WorkspaceRoot + "/AGENTS.md"

// WorkspaceLanding is the landing that names the workspace root itself,
// for a session with one repository that should not start inside it.
//
// @decision(DL-170)
const WorkspaceLanding = "."

// ForgeCLI identifies the CLI that talks to a repository's platform.
type ForgeCLI string

const (
	// CLITea is the Forgejo/Gitea CLI.
	CLITea ForgeCLI = "tea"
	// CLIGh is the GitHub CLI.
	CLIGh ForgeCLI = "gh"
	// CLIGlab is the GitLab CLI.
	//
	// @decision(DL-032)
	CLIGlab ForgeCLI = "glab"
	// CLINone means spov knows of no CLI for the platform.
	CLINone ForgeCLI = ""
)

// WorkspaceRepo describes one cloned repository for the generated
// guidance.
type WorkspaceRepo struct {
	// Slug is "owner/name". It must not contain a credential.
	Slug string
	// Dir is the directory under the workspace root.
	Dir string
	// Platform is the configured platform name.
	Platform string
	// Host is the platform's hostname.
	Host string
	// CLI is the tool that talks to this platform.
	CLI ForgeCLI
}

// AgentsMarkdown renders the workspace guidance. It lists what was
// actually cloned, so it cannot describe repositories that are not there.
// jobKind is the job's kind, or empty for an interactive session.
//
// @decision(DL-037)
func AgentsMarkdown(repos []WorkspaceRepo, jobKind string) string {
	return agentsMarkdown(repos, jobKind, "", "")
}

// agentsMarkdown is AgentsMarkdown with a pr job's pull request policy,
// which is what the entrypoint renders for a job.
//
// @decision(DL-037) @decision(DL-137)
func agentsMarkdown(repos []WorkspaceRepo, jobKind, prPolicy, worksIn string) string {
	var b strings.Builder
	b.WriteString("# Workspace\n\n")
	b.WriteString("This sandbox was prepared by spov. The repositories below are already\n")
	b.WriteString("cloned and their git credentials are configured.\n\n")

	if len(repos) == 0 {
		b.WriteString("No repositories were bootstrapped into this workspace.\n")
		writeScratchSection(&b, jobKind)
		return b.String()
	}

	b.WriteString("| Directory | Repository | Host | CLI |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, r := range repos {
		cli := string(r.CLI)
		if cli == "" {
			cli = "(none available)"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | `%s` |\n",
			path.Join(WorkspaceRoot, r.Dir), r.Slug, r.Host, cli)
	}

	b.WriteString("\n## Read each repository's own instructions\n\n")
	// @decision(DL-174) @decision(DL-170)
	if worksIn != "" {
		fmt.Fprintf(&b, "You are working inside `%s`, and that repository's own `AGENTS.md`\n", path.Join(WorkspaceRoot, worksIn))
		b.WriteString("and OpenCode configuration have been loaded for you. Paths are relative to\n")
		b.WriteString("it. Before you change anything in another directory above, read its\n")
		b.WriteString("`AGENTS.md` and `CLAUDE.md` if they exist, and read this repository's\n")
		b.WriteString("`CLAUDE.md` if it has one. They carry the conventions your work is judged\n")
		b.WriteString("against.\n")
	} else {
		b.WriteString("You are working from the workspace root, not from inside a repository, so\n")
		b.WriteString("no repository's own instruction files have been loaded for you. Before you\n")
		b.WriteString("change anything in one of the directories above, read its `AGENTS.md` and\n")
		b.WriteString("`CLAUDE.md` if they exist. They carry the conventions your work is judged\n")
		b.WriteString("against, and nothing else will tell you what they say.\n")
	}

	// @decision(DL-100)
	if jobKind == "sync" {
		writeSyncSection(&b, repos)
		writeScratchSection(&b, jobKind)
		return b.String()
	}

	// @decision(DL-137)
	if jobKind == "pr" && prPolicy == PRPolicyGoal {
		writeGoalFlowSection(&b, repos)
		writeRefusalSection(&b)
		writeScratchSection(&b, jobKind)
		return b.String()
	}

	b.WriteString("\n## Opening pull requests\n\n")
	b.WriteString("Use the CLI named for the repository you are working in; they are not\n")
	b.WriteString("interchangeable, and are already authenticated:\n\n")

	if usesCLI(repos, CLITea) {
		b.WriteString("- **`tea`** for Forgejo and Gitea hosts: `tea pr create --title '...' --description '...'`.\n")
		b.WriteString("  Run it from inside the repository directory.\n")
	}
	if usesCLI(repos, CLIGh) {
		b.WriteString("- **`gh`** for GitHub: `gh pr create --title '...' --body '...'`.\n")
	}
	// @decision(DL-032)
	if usesCLI(repos, CLIGlab) {
		b.WriteString("- **`glab`** for GitLab, where a pull request is a merge request:\n")
		b.WriteString("  `glab mr create --title '...' --description '...' --yes`. Run it from inside\n")
		b.WriteString("  the repository directory.\n")
	}
	if usesCLI(repos, CLINone) {
		b.WriteString("- Some repositories have no CLI configured; push a branch and open the\n")
		b.WriteString("  pull request in the web UI.\n")
	}

	b.WriteString("\nThe token each CLI holds may be scoped to a subset of repositories, so a\n")
	b.WriteString("command can fail on permissions even though the clone worked.\n")
	writeRefusalSection(&b)
	writeScratchSection(&b, jobKind)
	return b.String()
}

// writeGoalFlowSection replaces the pull request section for a pr job
// whose goal says how its pull request is opened. It names the forge
// CLIs present and gives no create command.
//
// @decision(DL-137)
func writeGoalFlowSection(b *strings.Builder, repos []WorkspaceRepo) {
	b.WriteString("\n## This job's pull request\n\n")
	b.WriteString("This job's goal says which branch to work on and how its pull request is\n")
	b.WriteString("opened or updated. Follow the goal for both. The forge CLIs here are already\n")
	b.WriteString("authenticated:\n\n")
	if usesCLI(repos, CLITea) {
		b.WriteString("- **`tea`** for Forgejo and Gitea hosts. Run it from inside the repository directory.\n")
	}
	if usesCLI(repos, CLIGh) {
		b.WriteString("- **`gh`** for GitHub.\n")
	}
	if usesCLI(repos, CLIGlab) {
		b.WriteString("- **`glab`** for GitLab, where a pull request is a merge request. Run it from\n")
		b.WriteString("  inside the repository directory.\n")
	}
	if usesCLI(repos, CLINone) {
		b.WriteString("- Some repositories have no CLI configured.\n")
	}
	b.WriteString("\nThe token each CLI holds may be scoped to a subset of repositories, so a\n")
	b.WriteString("command can fail on permissions even though the clone worked.\n")
}

// writeSyncSection replaces the pull request section for a sync job,
// whose result is the repository it leaves. A repository with no
// platform was seeded from a bundle alone and has no origin.
//
// @decision(DL-100)
func writeSyncSection(b *strings.Builder, repos []WorkspaceRepo) {
	b.WriteString("\n## Where your work goes\n\n")
	b.WriteString("This is a sync job. Its result is the repository's state when you finish:\n")
	b.WriteString("the commits on the branch you started on, plus any uncommitted changes. They\n")
	b.WriteString("are taken back into the checkout the job was started from. Do not push, and\n")
	b.WriteString("do not open a pull request.\n")
	for _, r := range repos {
		if r.Platform == "" {
			b.WriteString("\nThe repository has no `origin` remote and no git credentials: it was\n")
			b.WriteString("seeded from the local checkout alone, so fetching or pushing will fail.\n")
			return
		}
	}
	writeRefusalSection(b)
}

// writeRefusalSection says what a refused forge command means, so an
// agent does not take it for a token to replace.
//
// @decision(DL-108)
func writeRefusalSection(b *strings.Builder) {
	b.WriteString("\n## When a forge command is refused\n\n")
	b.WriteString("The credentials here are managed by spov, not by you. A forge command that\n")
	b.WriteString("answers 401, 403 or 404 on something you would expect to reach is not a sign\n")
	b.WriteString("that you must log in again. Do not run `gh auth login` or another CLI's login,\n")
	b.WriteString("and do not ask for a token. Retry once in case the failure was transient, then\n")
	b.WriteString("report the exact command and its exact error message, and carry on with\n")
	b.WriteString("whatever does not need it.\n")
}

// writeSubagentSection tells a job's agent that its subagents and its
// background commands report on their own and that the job ends
// whatever they are doing. An interactive session has no completion
// tool and gets none.
//
// @decision(DL-143) @decision(DL-182)
func writeSubagentSection(b *strings.Builder, jobKind string) {
	if jobKind == "" {
		return
	}
	b.WriteString("\n## Subagents and background commands\n\n")
	b.WriteString("A subagent you start runs in the background: the call returns at once, and\n")
	b.WriteString("its report reaches you as a message of its own when it finishes, which starts\n")
	b.WriteString("your next turn. You need not poll or sleep for it. The job ends when you call\n")
	fmt.Fprintf(b, "`%s`, whatever is still running then, and work a subagent has not reported\n", CompletionTool)
	b.WriteString("by then is lost, so wait for the reports you need before calling it.\n")
	b.WriteString("\nA shell command you run in the background, or one moved there when it outran\n")
	b.WriteString("its timeout, works the same way: its result reaches you as a message of its\n")
	b.WriteString("own when it ends, which starts your next turn. A command still running when\n")
	b.WriteString("the job completes is ended with the sandbox.\n")
}

// writeScratchSection names the scratchpad, after a job's subagent
// section. A report job is also told
// that its deliverable does not go there.
//
// @decision(DL-037) @decision(DL-051)
func writeScratchSection(b *strings.Builder, jobKind string) {
	// @decision(DL-143)
	writeSubagentSection(b, jobKind)
	b.WriteString("\n## Scratchpad\n\n")
	fmt.Fprintf(b, "`%s` is yours for anything that is not the work itself: temporary\n", ScratchDir)
	b.WriteString("files, one-off scripts, downloads, notes and intermediate results. Put them\n")
	b.WriteString("there, not in a repository and not in `/tmp`. Nothing in it is collected,\n")
	b.WriteString("delivered or committed, and it goes when the sandbox does.\n")
	if jobKind == "report" {
		fmt.Fprintf(b, "\nThis is a report job: its reports go in `%s`, one Markdown file each, not in the scratchpad.\n",
			OutDir)
	}
}

// usesCLI reports whether any repository uses the given CLI.
func usesCLI(repos []WorkspaceRepo, cli ForgeCLI) bool {
	for _, r := range repos {
		if r.CLI == cli {
			return true
		}
	}
	return false
}

// CLIForKind maps a platform kind to the CLI that talks to it.
//
// @decision(DL-037) @decision(DL-041) @decision(DL-032)
func CLIForKind(kind string) ForgeCLI {
	switch kind {
	case "forgejo", "gitea":
		return CLITea
	case "github":
		return CLIGh
	case "gitlab":
		return CLIGlab
	default:
		return CLINone
	}
}
