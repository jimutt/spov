package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// SelectReposInteractively asks the user which repositories to bootstrap.
//
// @decision(DL-028)
func SelectReposInteractively(repos []RepoChoice) ([]RepoChoice, error) {
	// Never show a form with nothing in it: a prompt the user cannot
	// satisfy is worse than a plain message.
	if len(repos) == 0 {
		return nil, fmt.Errorf("the account has no repositories to choose from")
	}

	picker := newRepoPicker(repos, terminalIsDark(), time.Now())
	// On stderr, so stdout carries only the command's own output
	// (`spov jobs create --json | jq`).
	//
	// @decision(DL-023)
	final, err := tea.NewProgram(picker, tea.WithOutput(os.Stderr)).Run()
	if err != nil {
		return nil, fmt.Errorf("selecting repositories: %w", err)
	}

	done, ok := final.(*repoPicker)
	if !ok {
		return nil, fmt.Errorf("selecting repositories: unexpected picker state")
	}
	if done.cancelled {
		return nil, errors.New("selecting repositories: cancelled")
	}
	return done.chosen(), nil
}

// slugColumnWidth is the width to pad repository slugs to so the
// platform names beside them line up, or zero when every repository
// comes from the same platform and the column would be noise.
//
// @decision(DL-028)
func slugColumnWidth(repos []RepoChoice) int {
	names := map[string]bool{}
	width := 0
	for _, r := range repos {
		names[r.Platform] = true
		if n := lipgloss.Width(r.Repo.Slug()); n > width {
			width = n
		}
	}
	if len(names) < 2 {
		return 0
	}
	return width
}
