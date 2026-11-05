package opencode

import (
	"strings"
	"testing"
)

func TestAgentsMarkdown(t *testing.T) {
	md := AgentsMarkdown([]WorkspaceRepo{
		{Slug: "acme/widgets", Dir: "widgets", Platform: "codefloe", Host: "codefloe.com", CLI: CLITea},
		{Slug: "acme/api", Dir: "api", Platform: "gh", Host: "github.com", CLI: CLIGh},
	}, "")

	for _, want := range []string{
		"acme/widgets", "/workspace/widgets", "codefloe.com", "`tea`",
		"acme/api", "/workspace/api", "github.com", "`gh`",
		"tea pr create", "gh pr create",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("guidance does not mention %q:\n%s", want, md)
		}
	}
	// The scope caveat matters: a repo-scoped token clones fine and then
	// fails to open a PR elsewhere.
	if !strings.Contains(md, "scoped to a subset") {
		t.Error("guidance does not warn about token scope")
	}
}

// TestAgentsMarkdownMentionsOnlyRelevantCLIs: telling the agent about a
// tool none of its repositories use is noise it has to reason past.
//
// @decision(DL-037)
func TestAgentsMarkdownMentionsOnlyRelevantCLIs(t *testing.T) {
	forgejoOnly := AgentsMarkdown([]WorkspaceRepo{
		{Slug: "acme/widgets", Dir: "widgets", Host: "codefloe.com", CLI: CLITea},
	}, "")
	if strings.Contains(forgejoOnly, "gh pr create") {
		t.Error("guidance describes gh for a workspace with no GitHub repositories")
	}
	if !strings.Contains(forgejoOnly, "tea pr create") {
		t.Error("guidance omits tea for a Forgejo repository")
	}
}

func TestAgentsMarkdownWithNoRepos(t *testing.T) {
	md := AgentsMarkdown(nil, "")
	if !strings.Contains(md, "No repositories") {
		t.Errorf("guidance = %q", md)
	}
	// Nothing to open a PR against, so no CLI advice.
	if strings.Contains(md, "pr create") {
		t.Error("guidance describes opening pull requests with no repositories")
	}
}

// TestTheWorkspaceGuidanceAsksForEachRepositorysOwnInstructions pins the
// sentence that tells the agent to read a repository's own AGENTS.md and
// CLAUDE.md, naming both files.
//
// @decision(DL-037)
func TestTheWorkspaceGuidanceAsksForEachRepositorysOwnInstructions(t *testing.T) {
	md := AgentsMarkdown([]WorkspaceRepo{
		{Slug: "acme/widgets", Dir: "widgets", Host: "git.example.com", CLI: CLITea},
	}, "")
	for _, want := range []string{"AGENTS.md", "CLAUDE.md"} {
		if !strings.Contains(md, want) {
			t.Errorf("the guidance never asks the agent to read %s:\n%s", want, md)
		}
	}
	// A repository with no instruction files must not read as an error,
	// so the instruction is conditional on its face.
	if !strings.Contains(md, "if they exist") {
		t.Errorf("the instruction is unconditional; a repository carrying neither file"+
			" reads as something missing:\n%s", md)
	}
}

// TestTheGuidanceSaysWhyTheInstructionsAreNotAlreadyLoaded pins that the
// guidance names where the session is.
//
// @decision(DL-037)
func TestTheGuidanceSaysWhyTheInstructionsAreNotAlreadyLoaded(t *testing.T) {
	md := AgentsMarkdown([]WorkspaceRepo{
		{Slug: "acme/widgets", Dir: "widgets", Host: "git.example.com", CLI: CLITea},
	}, "")
	if !strings.Contains(md, "workspace root") {
		t.Errorf("the guidance does not say where the agent is:\n%s", md)
	}
}

func TestAgentsMarkdownWithAnUnknownPlatform(t *testing.T) {
	md := AgentsMarkdown([]WorkspaceRepo{
		{Slug: "o/r", Dir: "r", Host: "git.example.com", CLI: CLINone},
	}, "")
	if !strings.Contains(md, "web UI") {
		t.Errorf("guidance does not say what to do without a CLI:\n%s", md)
	}
}

// TestAgentsMarkdownCarriesNoCredential: this file is written into the
// workspace and read by the agent.
func TestAgentsMarkdownCarriesNoCredential(t *testing.T) {
	md := AgentsMarkdown([]WorkspaceRepo{
		{Slug: "acme/widgets", Dir: "widgets", Host: "codefloe.com", CLI: CLITea},
	}, "")
	for _, leak := range []string{"token", "password", "@codefloe.com"} {
		if strings.Contains(strings.ToLower(md), leak) && !strings.Contains(md, "The token each CLI holds") {
			t.Errorf("guidance may contain a credential (%q):\n%s", leak, md)
		}
	}
}

func TestCLIForKind(t *testing.T) {
	tests := []struct {
		kind string
		want ForgeCLI
	}{
		{"forgejo", CLITea},
		{"gitea", CLITea},
		{"github", CLIGh},
		{"gitlab", CLIGlab},
		{"bitbucket", CLINone},
		{"", CLINone},
	}
	for _, tt := range tests {
		if got := CLIForKind(tt.kind); got != tt.want {
			t.Errorf("CLIForKind(%q) = %q, want %q", tt.kind, got, tt.want)
		}
	}
}

// Every workspace guide names the scratchpad, and a report job's also
// says its deliverable does not go there.
//
// @decision(DL-037)
func TestTheWorkspaceGuideNamesTheScratchpad(t *testing.T) {
	repos := []WorkspaceRepo{{Slug: "o/r", Dir: "r", Host: "git.example.com", CLI: CLITea}}
	for _, kind := range []string{"", "pr", "report"} {
		for _, md := range []string{AgentsMarkdown(repos, kind), AgentsMarkdown(nil, kind)} {
			if !strings.Contains(md, "`"+ScratchDir+"`") {
				t.Errorf("kind %q: the guide does not name the scratchpad:\n%s", kind, md)
			}
			if !strings.Contains(md, "not in a repository") {
				t.Errorf("kind %q: the guide does not say what the scratchpad is instead of:\n%s", kind, md)
			}
			says := strings.Contains(md, "`"+OutDir+"`, one Markdown file each")
			if says != (kind == "report") {
				t.Errorf("kind %q: names the report path: %v", kind, says)
			}
		}
	}
}

// A GitLab repository's guidance names glab and calls the change a merge
// request.
//
// @decision(DL-032)
func TestTheGuideNamesGlabForGitLab(t *testing.T) {
	md := AgentsMarkdown([]WorkspaceRepo{{Slug: "g/s/r", Dir: "r", Platform: "gl", Host: "gitlab.com", CLI: CLIGlab}}, "")
	if !strings.Contains(md, "glab mr create") || !strings.Contains(md, "merge request") {
		t.Errorf("guide:\n%s", md)
	}
}
