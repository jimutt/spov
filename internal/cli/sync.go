package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
)

// syncTimeout bounds exporting and applying a sandbox's repository
// state when a session closes.
//
// @decision(DL-027)
const syncTimeout = 2 * time.Minute

// resolveSyncLaunch inspects the local git checkout, matches its remote
// against the configured platforms when possible (falling back to a
// full local bundle when the repo has no remote, is on an unconfigured
// host, or the server lacks a credential for that platform), and builds
// the RepoRef and SessionSyncRequest for a synced `spov fly` launch.
//
// @decision(DL-027)
func (e *Env) resolveSyncLaunch(ctx context.Context, c *client.Client, cfg config.Config, repoFlags []string) (server.RepoRef, *server.SessionSyncRequest, git.LocalCheckout, error) {
	if len(repoFlags) > 1 {
		return server.RepoRef{}, nil, git.LocalCheckout{}, fmt.Errorf("--sync supports a single repository only; drop the extra --repo flags or pass --no-sync")
	}
	checkout, err := git.InspectLocalCheckout(ctx, e.Workdir, platformKindsByHost(cfg), true)
	if err != nil {
		return server.RepoRef{}, nil, git.LocalCheckout{}, err
	}
	var wantPlatform string
	if len(repoFlags) == 1 {
		parsed, err := parseRepoFlag(repoFlags[0])
		if err != nil {
			return server.RepoRef{}, nil, git.LocalCheckout{}, err
		}
		if !strings.EqualFold(parsed.Owner, checkout.Owner) || !strings.EqualFold(parsed.Name, checkout.Name) {
			return server.RepoRef{}, nil, git.LocalCheckout{}, fmt.Errorf("--sync uses the current git checkout (%s/%s), which does not match --repo %s; drop --repo or pass --no-sync",
				checkout.Owner, checkout.Name, repoFlags[0])
		}
		wantPlatform = parsed.Platform
	}

	var platformName string
	if checkout.Remote != "" {
		if platform, err := matchPlatformForCheckout(cfg, checkout, wantPlatform); err == nil {
			platformName = platform.Name
		} else if wantPlatform != "" {
			return server.RepoRef{}, nil, git.LocalCheckout{}, err
		}
	} else if wantPlatform != "" {
		return server.RepoRef{}, nil, git.LocalCheckout{}, fmt.Errorf("local checkout in %s has no git remote; drop --repo %s to seed from the local repository", checkout.Root, repoFlags[0])
	}

	// When no platform matched, or the platform may lack a credential on
	// the server, pack a full bundle of HEAD so the sandbox can initialize
	// the repository without cloning from the forge.
	needFullBundle := platformName == ""
	if !needFullBundle && !e.hasPlatformCredential(ctx, c, cfg, platformName) {
		platformName = ""
		needFullBundle = true
	}
	if needFullBundle {
		fullBundle, err := git.BundleHEAD(ctx, checkout.Root)
		if err != nil {
			return server.RepoRef{}, nil, git.LocalCheckout{}, err
		}
		checkout.Bundle = fullBundle
	}

	ref := server.RepoRef{
		Platform: platformName,
		Owner:    checkout.Owner,
		Name:     checkout.Name,
	}
	req := &server.SessionSyncRequest{
		Branch:     checkout.Branch,
		BaseCommit: checkout.HeadCommit,
	}
	if len(checkout.Bundle) > 0 {
		req.Bundle = base64.StdEncoding.EncodeToString(checkout.Bundle)
	}
	return ref, req, checkout, nil
}

// hasPlatformCredential reports whether a credential is available for
// platformName, so a synced launch knows whether to send an incremental
// bundle or a full HEAD bundle.
func (e *Env) hasPlatformCredential(ctx context.Context, c *client.Client, cfg config.Config, platformName string) bool {
	p, err := findPlatform(cfg, platformName)
	if err != nil {
		return false
	}
	hosted, _ := e.hostedServer()
	if !hosted {
		if _, err := e.platformToken(ctx, p); err == nil {
			return true
		}
		return false
	}
	if c == nil {
		return true
	}
	var shared []string
	if p.Prefer == config.PreferShared {
		shared = []string{p.Name}
	}
	_, _, platforms, err := c.ListReposFor(ctx, shared)
	if err != nil {
		return true
	}
	for _, pt := range platforms {
		if pt.Platform == platformName {
			return pt.Tier != server.TierNone
		}
	}
	return true
}

func singleViewRepo(view server.SessionView) (server.RepoRef, bool) {
	if len(view.Repos) == 1 {
		return view.Repos[0], true
	}
	if len(view.Repos) == 0 && len(view.Clones) == 1 {
		owner, name, err := git.SplitPath(view.Clones[0])
		if err == nil {
			return server.RepoRef{Owner: owner, Name: name}, true
		}
	}
	return server.RepoRef{}, false
}

// verifySyncCheckout confirms that the current working directory is a
// git checkout of the session's single repository before syncing back.
//
// @decision(DL-027)
func (e *Env) verifySyncCheckout(ctx context.Context, cfg config.Config, view server.SessionView) (git.LocalCheckout, error) {
	if view.Job != nil {
		return git.LocalCheckout{}, fmt.Errorf("--sync is for interactive sessions, not autonomous jobs")
	}
	want, ok := singleViewRepo(view)
	if !ok {
		n := len(view.Repos)
		if n == 0 {
			n = len(view.Clones)
		}
		return git.LocalCheckout{}, fmt.Errorf("session %s has %d repositories; --sync supports single-repository sessions only",
			displayName(view), n)
	}
	root, err := gitRootIn(ctx, e.Workdir)
	if err != nil {
		return git.LocalCheckout{}, fmt.Errorf("not in a git repository; cd into your local checkout of %s/%s to sync",
			want.Owner, want.Name)
	}
	branch, _ := gitCurrentBranch(ctx, root)
	remote := gitBranchRemote(ctx, root, branch)
	remoteURL, err := gitRemoteURL(ctx, root, remote)
	var host, basePath, owner, name string
	if err == nil && strings.TrimSpace(remoteURL) != "" {
		host, basePath, owner, name, err = git.ParseRemoteURL(remoteURL, platformKindsByHost(cfg))
		if err != nil {
			return git.LocalCheckout{}, fmt.Errorf("remote %q: %w", remote, err)
		}
	} else {
		remote = ""
		owner = "local"
		name = filepath.Base(root)
	}
	if !strings.EqualFold(owner, want.Owner) || !strings.EqualFold(name, want.Name) {
		return git.LocalCheckout{}, fmt.Errorf("local checkout in %s is %s/%s, but session %s is for %s/%s",
			root, owner, name, displayName(view), want.Owner, want.Name)
	}
	if want.Platform != "" && remote != "" {
		if p, err := findPlatform(cfg, want.Platform); err == nil {
			pHost, pBase := platformHostAndBase(p)
			if !strings.EqualFold(host, pHost) || !strings.EqualFold(basePath, pBase) {
				return git.LocalCheckout{}, fmt.Errorf("local checkout remote is on %s%s, but session %s uses platform %s (%s%s)",
					host, basePath, displayName(view), p.Name, pHost, pBase)
			}
		}
	}
	return git.LocalCheckout{
		Root:      root,
		Remote:    remote,
		RemoteURL: remoteURL,
		Host:      host,
		BasePath:  basePath,
		Owner:     owner,
		Name:      name,
		Branch:    branch,
	}, nil
}

// syncSessionBack exports the repository state from the running session
// and applies it to the local checkout. It runs under a detached
// context bounded by syncTimeout so an interrupt that closed the TUI
// does not abort the sync.
//
// @decision(DL-027)
func (e *Env) syncSessionBack(ctx context.Context, c *client.Client, cfg config.Config, view server.SessionView) error {
	syncCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncTimeout)
	defer cancel()

	checkout, err := e.verifySyncCheckout(syncCtx, cfg, view)
	if err != nil {
		return err
	}
	repoRef, _ := singleViewRepo(view)
	_, _ = fmt.Fprintf(e.Err, "Syncing %s/%s from the sandbox...\n", repoRef.Owner, repoRef.Name)

	exportedView, err := c.ExportSessionSync(syncCtx, view.ID)
	if lostServer(syncCtx, err) && view.Name != "" {
		if fresh, rerr := e.reconnect(syncCtx, c); rerr == nil {
			exportedView, err = fresh.ExportSessionSync(syncCtx, view.Name)
		}
	}
	if client.IsNotFound(err) && view.Name != "" {
		exportedView, err = c.ExportSessionSync(syncCtx, view.Name)
	}
	if err != nil {
		return fmt.Errorf("exporting sandbox state: %w", err)
	}

	var bundleBytes, tarBytes []byte
	if exportedView.Bundle != "" {
		bundleBytes, err = base64.StdEncoding.DecodeString(exportedView.Bundle)
		if err != nil {
			return fmt.Errorf("decoding exported bundle: %w", err)
		}
	}
	if exportedView.WorktreeTar != "" {
		tarBytes, err = base64.StdEncoding.DecodeString(exportedView.WorktreeTar)
		if err != nil {
			return fmt.Errorf("decoding exported worktree archive: %w", err)
		}
	}
	baseCommit := exportedView.BaseCommit
	startBranch := ""
	if view.Sync != nil {
		if baseCommit == "" {
			baseCommit = view.Sync.BaseCommit
		}
		startBranch = view.Sync.Branch
	}
	if startBranch == "" {
		startBranch = exportedView.Branch
	}
	if baseCommit == "" && startBranch != "" && checkout.Branch == startBranch {
		if localHead, herr := gitCommandIn(syncCtx, checkout.Root, "rev-parse", "--verify", "HEAD"); herr == nil && git.ValidCommitSHA(localHead) {
			baseCommit = strings.ToLower(localHead)
		}
	}

	res, err := git.ApplySync(syncCtx, checkout.Root, displayName(view), startBranch, git.ExportedState{
		Branch:      exportedView.Branch,
		BaseCommit:  baseCommit,
		HeadCommit:  exportedView.HeadCommit,
		Bundle:      bundleBytes,
		WorktreeTar: tarBytes,
		Deleted:     exportedView.Deleted,
	})
	if err != nil {
		return err
	}
	printSyncResult(e, res)
	return nil
}

// printSyncResult reports how the sandbox's repository state landed
// locally.
//
// @decision(DL-027)
func printSyncResult(env *Env, res git.SyncResult) {
	if res.NoChanges {
		_, _ = fmt.Fprintln(env.Err, "No repository changes to sync.")
		return
	}
	if res.InPlace {
		_, _ = fmt.Fprintf(env.Err, "Synced to %s (%s).\n", res.Branch, syncCountsSummary(res.Commits, res.FilesChanged, false))
		return
	}
	_, _ = fmt.Fprintf(env.Err, "Synced to branch %s (%s; %s).\n",
		res.FallbackBranch, syncCountsSummary(res.Commits, res.FilesChanged, res.DirtyCommitted), res.Reason)
	_, _ = fmt.Fprintf(env.Err, "  checkout: git checkout %s\n", res.FallbackBranch)
	_, _ = fmt.Fprintf(env.Err, "  merge:    git merge %s\n", res.FallbackBranch)
}

func syncCountsSummary(commits, files int, dirtyCommitted bool) string {
	var parts []string
	if commits > 0 {
		word := "commits"
		if commits == 1 {
			word = "commit"
		}
		parts = append(parts, fmt.Sprintf("%d %s", commits, word))
	}
	if files > 0 {
		word := "files"
		if files == 1 {
			word = "file"
		}
		if dirtyCommitted {
			parts = append(parts, fmt.Sprintf("%d uncommitted %s committed", files, word))
		} else {
			parts = append(parts, fmt.Sprintf("%d uncommitted %s", files, word))
		}
	}
	if len(parts) == 0 {
		return "updated"
	}
	return strings.Join(parts, ", ")
}

// printSyncFailureHint explains that the sandbox was left running after
// a failed sync-back so no work is lost, and prints how to retry or
// discard.
//
// @decision(DL-027)
func printSyncFailureHint(env *Env, name string) {
	_, _ = fmt.Fprintf(env.Err, "\nThe sandbox was NOT terminated and is still running so your work is not lost.\n")
	_, _ = fmt.Fprintf(env.Err, "  retry sync and stop: spov stop %s --sync\n", name)
	_, _ = fmt.Fprintf(env.Err, "  attach:              spov attach %s --sync\n", name)
	_, _ = fmt.Fprintf(env.Err, "  discard and stop:    spov stop %s\n", name)
}

func platformKindsByHost(cfg config.Config) map[string]string {
	out := make(map[string]string, len(cfg.Platforms)*2)
	for _, p := range cfg.Platforms {
		h, base := platformHostAndBase(p)
		if h == "" {
			continue
		}
		out[h] = string(p.Kind)
		if base != "" {
			out[h+base] = string(p.Kind)
		}
	}
	return out
}

func platformHostAndBase(p config.Platform) (host, basePath string) {
	raw := strings.TrimRight(p.BaseURL, "/")
	if raw == "" {
		switch p.Kind {
		case config.KindGitHub:
			return "github.com", ""
		case config.KindGitLab:
			return "gitlab.com", ""
		default:
			return "", ""
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", ""
	}
	h := strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(u.Host), ":443"), ":80")
	bp := strings.TrimRight(u.Path, "/")
	return h, bp
}

func matchPlatformForCheckout(cfg config.Config, checkout git.LocalCheckout, explicit string) (config.Platform, error) {
	if explicit != "" {
		p, err := findPlatform(cfg, explicit)
		if err != nil {
			return config.Platform{}, err
		}
		h, bp := platformHostAndBase(p)
		if !strings.EqualFold(h, checkout.Host) || !strings.EqualFold(bp, checkout.BasePath) {
			return config.Platform{}, fmt.Errorf("local checkout remote is on %s%s, which does not match platform %s (%s%s)",
				checkout.Host, checkout.BasePath, p.Name, h, bp)
		}
		if err := git.CheckRepoPath(string(p.Kind), checkout.Owner, checkout.Name); err != nil {
			return config.Platform{}, err
		}
		return p, nil
	}
	var matches []config.Platform
	for _, p := range cfg.Platforms {
		h, bp := platformHostAndBase(p)
		if strings.EqualFold(h, checkout.Host) && strings.EqualFold(bp, checkout.BasePath) {
			matches = append(matches, p)
		}
	}
	if len(matches) != 1 && len(cfg.Repos) > 0 {
		for _, r := range cfg.Repos {
			if strings.EqualFold(r.Owner, checkout.Owner) && strings.EqualFold(r.Name, checkout.Name) {
				if p, err := findPlatform(cfg, r.Platform); err == nil {
					h, bp := platformHostAndBase(p)
					if len(matches) == 0 || (strings.EqualFold(h, checkout.Host) && strings.EqualFold(bp, checkout.BasePath)) {
						matches = []config.Platform{p}
						break
					}
				}
			}
		}
	}
	if len(matches) > 1 {
		var byAccount []config.Platform
		for _, p := range matches {
			if strings.EqualFold(p.Account, checkout.Owner) {
				byAccount = append(byAccount, p)
			}
		}
		if len(byAccount) == 1 {
			matches = byAccount
		}
	}
	switch len(matches) {
	case 1:
		p := matches[0]
		if err := git.CheckRepoPath(string(p.Kind), checkout.Owner, checkout.Name); err != nil {
			return config.Platform{}, err
		}
		return p, nil
	case 0:
		return config.Platform{}, fmt.Errorf("no configured platform matches %s%s (from remote %q); add a `platforms:` entry to %s",
			checkout.Host, checkout.BasePath, checkout.Remote, config.UserFileName)
	default:
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.Name
		}
		return config.Platform{}, fmt.Errorf("several configured platforms match %s (%s); pass --repo <platform>:%s/%s to choose one",
			checkout.Host, strings.Join(names, ", "), checkout.Owner, checkout.Name)
	}
}

func gitRootIn(ctx context.Context, dir string) (string, error) {
	out, err := gitCommandIn(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || out == "" {
		return "", errors.New("not in a git repository")
	}
	return out, nil
}

func gitCurrentBranch(ctx context.Context, dir string) (string, error) {
	return gitCommandIn(ctx, dir, "symbolic-ref", "-q", "--short", "HEAD")
}

func gitBranchRemote(ctx context.Context, dir, branch string) string {
	if branch != "" {
		if r, err := gitCommandIn(ctx, dir, "config", "--get", "branch."+branch+".remote"); err == nil && r != "" && r != "." {
			return r
		}
	}
	return "origin"
}

func gitRemoteURL(ctx context.Context, dir, remote string) (string, error) {
	return gitCommandIn(ctx, dir, "remote", "get-url", remote)
}

func gitCommandIn(ctx context.Context, dir string, args ...string) (string, error) {
	checkout, err := git.InspectCommandOutput(ctx, dir, args...)
	return checkout, err
}
