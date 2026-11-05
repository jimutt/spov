package git

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
)

// AppStatus is what checking a GitHub App found: where it is installed,
// what it may do beside what a sandbox asks for, and what it reaches. It
// carries nothing secret.
//
// @decision(DL-042)
type AppStatus struct {
	InstallationID int64
	// Account is the user or organisation the App is installed on.
	Account string
	// Permissions are the installation's grants; Missing are the ones
	// SandboxPermissions asks for that it lacks or holds only as read.
	Permissions map[string]string
	Missing     []string
	// RepoCount is how many repositories it reaches, and Repos the
	// first few of them, newest first.
	RepoCount int
	Repos     []string
}

// CheckShown is how many repositories a check names.
const CheckShown = 20

// Check verifies the App the way a sandbox will use it: it finds the
// installation, reads its grants, and mints, uses and revokes one token
// to list what it reaches. Any failing step fails the check, naming it.
//
// @decision(DL-042)
func (g *GitHub) Check(ctx context.Context) (AppStatus, error) {
	installID, err := g.resolveInstallation(ctx)
	if err != nil {
		return AppStatus{}, fmt.Errorf("finding the installation: %w", err)
	}
	jwt, err := g.jwt()
	if err != nil {
		return AppStatus{}, fmt.Errorf("signing in as the App: %w", err)
	}
	var inst struct {
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
		Permissions map[string]string `json:"permissions"`
	}
	path := fmt.Sprintf("/app/installations/%d", installID)
	if err := g.do(ctx, http.MethodGet, path, "Bearer "+jwt, nil, &inst); err != nil {
		return AppStatus{}, fmt.Errorf("reading the installation: %w", err)
	}
	status := AppStatus{InstallationID: installID, Account: inst.Account.Login, Permissions: inst.Permissions}
	for name, want := range SandboxPermissions {
		have := inst.Permissions[name]
		if have == "" || (want == "write" && have != "write") {
			status.Missing = append(status.Missing, name+":"+want)
		}
	}
	sort.Strings(status.Missing)
	repos, err := g.ListRepos(ctx)
	if err != nil && !errors.Is(err, ErrNoRepos) {
		return AppStatus{}, fmt.Errorf("listing what it reaches: %w", err)
	}
	status.RepoCount = len(repos)
	for _, r := range repos[:min(len(repos), CheckShown)] {
		status.Repos = append(status.Repos, r.Owner+"/"+r.Name)
	}
	return status, nil
}
