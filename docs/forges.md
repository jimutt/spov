# Forges

spov clones from, pushes to and opens change requests on four kinds of forge.
This page says what each needs from you, what a sandbox can do with your token,
and which forges have been tested. GitHub has its own page on the App and the two
credential tiers: [github-access.md](github-access.md).

## The kinds

A platform in your config names its kind (DL-041):

| Kind | Where | `base_url` | Token | CLI in the sandbox |
|---|---|---|---|---|
| `github` | github.com, GitHub Enterprise | optional (github.com) | a fine-grained PAT (a classic one is accepted, and reaches everything its account can), or the organisation's App | `gh` |
| `forgejo` | Codefloe, Codeberg, your own Forgejo | required | an access token | `tea` |
| `gitea` | gitea.com, your own Gitea | required | an access token | `tea` |
| `gitlab` | gitlab.com, a self-managed GitLab | optional (gitlab.com) | a personal access token | `glab` |

```yaml
platforms:
  - name: codefloe
    kind: forgejo
    base_url: https://codefloe.com
    account: alice             # the git username for a Forgejo or Gitea token
  - name: gitlab
    kind: gitlab               # base_url defaults to https://gitlab.com
```

Forgejo and Gitea share one implementation in spov, since their APIs agree
where spov uses them. Each still has its own name.

## Repository paths

A repository is `owner/name`. On GitLab the owner may be a group path, and a
project in a subgroup is named in full: `--repo gitlab:group/subgroup/project`.
The other kinds take one owner segment. Every segment may use letters, digits,
`.`, `_` and `-`, may not start with `-`, and may not be `.` or `..` (DL-041).

## Storing a token

On a hosted server a credential is named `github`, or `<kind>:<host>`:
`forgejo:codefloe.com`, `gitea:gitea.com`, `gitlab:gitlab.com`. Store it with
`spov credentials add <name>` or on the Settings page. Locally, name the platform
from your config.

The token is checked before it is stored (DL-041):

- spov reads whose it is (on Forgejo and Gitea, unless the token may not read
  its user, as a repository-scoped one may not). On GitLab it also reads the
  token's scopes and expiry,
  and refuses a token without the `api` scope, saying what the token has.
- A token that the forge refuses, one that lacks a permission, and a server that
  lacks what spov needs are reported as different problems. A token that
  reaches no repository is refused too.
- Scopes and expiry are shown on the Settings page, and `spov credentials add`
  prints them; `spov credentials list` shows the expiry. spov never renews or
  narrows a stored token, so replace it before it expires.

A deployment can also hold one shared token per platform, in
`SPOV_PLATFORM_TOKEN_<NAME>` (the platform's name, upper-cased). A launch uses
your own token first and the shared one only when you have none; the tiers are
the ones [github-access.md](github-access.md) describes.

At launch, a stored token (not a GitHub App's) is checked once more: spov asks
the forge whether the token can see each selected repository, and refuses the
launch before any sandbox exists if the forge says it cannot (DL-043). That is
a 404 on GitHub and GitLab, and a 404 or 403 on Forgejo and Gitea. Any other
answer, or none, lets the launch go on. It checks reach, not write access.

What to create:

- **Forgejo:** a token limited to "Specific repositories", with `write:repository`
  and `write:issue`. tea reads a repository's labels when it opens a pull
  request, and labels need the issue scope (DL-041).
- **Gitea:** a token with `write:repository` and `write:issue`, for the same
  reason.
- **GitLab:** a personal access token with `api` and a short expiry. Prefer a
  dedicated account that is a member of only the projects spov should reach:
  a GitLab PAT reaches every project its account can.
- **GitHub:** see [github-access.md](github-access.md).

A session can hold one credential per host, so one session cannot use two
accounts on the same forge (DL-034).

## What a sandbox can do with it

When the credential proxy runs, which it does on every hosted server and on a
local one built with `scripts/build-proxy.sh`, the token never enters the
sandbox's main container. A proxy sidecar holds it and adds it only to requests
it allows; the CLIs hold a placeholder (DL-034). Without the proxy (a
local build without it, or a hosted server with `SPOV_TOKENS_IN_SANDBOX=1`) the
token is inside the sandbox, where the agent can read it.

If the sidecar exits, the server recreates it once, with the CA the launch made
(a new one only when none was kept), so the running agent keeps trusting it
(DL-034). A second exit ends the sandbox.

For a stored token, the proxy allows:

- git clone, fetch, push and LFS on the session's repositories. A push reaches
  every branch the forge itself does not protect, the default branch included,
  so **protect your default branch on the forge**: that, not spov, is what stops
  a push or force-push to it;
- on their REST API: reading the repository and its code, branches, commits,
  labels and milestones, and statuses and checks (pipelines and jobs on GitLab);
  writing code and commits; opening and editing pull or merge requests and
  issues, and commenting on them. On GitHub, Forgejo and Gitea this includes
  reviews, so a token can approve a pull request there; on GitLab approving is
  refused;
- on GitHub, a personal access token also reads the session's repositories'
  Actions (runs, jobs, logs, workflows and artifacts) and releases, and searches
  (DL-107). It cannot rerun, cancel or dispatch a workflow;
- on GitLab, the project by its full path or by the numeric ID spov looked up
  at launch (DL-034).

It refuses the merge endpoints, deleting or changing the repository itself, and
its settings, members, collaborators, hooks, keys, secrets and variables, and
protection rules. On GitLab it also refuses deleting a merge request or issue,
and moving or cloning an issue into another project. Deleting files and
comments through the API, and deleting a branch with `git push --delete`, is
allowed wherever writing is; the branches endpoints themselves are read only.

With git access `all` (DL-043), the same endpoint rules apply to every
repository the token reaches rather than the session's.

GraphQL names no repository, so a GitHub personal access token reaches there
whatever it was created to reach, and a GitLab one whatever its account can,
merging included. GitHub's search names none either, and reads across whatever
the token reaches. The session page says so for those tokens.

Under the `install` and `agent` egress presets, a session can reach its own
forges' hosts (DL-031), unless a forge is addressed by an IP or a single-label
name, which only the `open` preset reaches. A forge on a private network cannot
be reached through the proxy, which only dials public addresses.

## The forge's own package registry

A session's forge token also reads its forge's package registry, for the
session's own namespaces, and never publishes:

- **GitLab:** the packages of the session's projects, and of the groups above
  them, under `/api/v4/projects/<project>/packages/` and
  `/api/v4/groups/<group>/-/packages/`. The instance-level endpoints
  (`/api/v4/packages/...`) get no token.
- **Forgejo and Gitea:** `/api/packages/<owner>/...` for an owner of one of the
  session's repositories.
- **GitHub:** GitHub Packages accepts only a classic personal access token, not
  an App's token or a fine-grained one. With a classic PAT the sandbox reads
  `npm.pkg.github.com`, `maven.pkg.github.com` and `nuget.pkg.github.com` for the
  session's owners. Otherwise store a classic PAT with only `read:packages` as a
  routed credential: `spov credentials add gh-npm --preset npm --url
  https://npm.pkg.github.com/ --scope @your-org`. Container images (`ghcr.io`)
  are not covered.

With git access `all`, the namespace rule is lifted as the repository rule is;
the token still only reads.

## Pull requests and merge requests

A `pr` job opens a pull request, which is a merge request on GitLab. The agent
opens it with the forge's CLI and records it. spov then asks the forge for an
open request from the job's branch: on GitLab one from the project itself, not
a fork (DL-051); on GitHub one whose head is the repository's owner's branch;
on Forgejo and Gitea one whose head branch has that name. Each request is
named the forge's way, "Merge request !12" rather than "Pull request #12"; the
job's kind is still called a pull request job.

## Tested forges

spov is tested end to end on github.com, Codefloe (Forgejo) and gitlab.com
(GitLab) (DL-041). End to end means checking a token, listing and choosing
repositories, clone and push, the forge's CLI opening a change request, the
job's check of it, and the proxy's refusals, in a real sandbox. Forgejo and
Gitea are also run in CI.

| Forge | Status |
|---|---|
| github.com | real sandboxes (2026-11-01): the organisation's App and a fine-grained token, tier choice, clone and push, a pull request job verified by gate 2, token renewal and a recreated sidecar; the proxy's refusals were not part of that round |
| Codefloe (Forgejo) | integration tests against it cover repository listing and a clone with a stored token; no sandbox round on Codefloe itself |
| Forgejo 17.0.1, Codefloe's version | the whole round in a real sandbox: token check, clone, push, a pull request opened with tea, gate 2, and merging, reading and pushing to another user's private repository refused (2026-11-03); `TestScenario4` runs against it in CI |
| Gitea 28.3.0 | `TestScenario4` in CI: token check, listing, a launch refused on an unreachable repository, gate 2; no sandbox round |
| gitlab.com (GitLab) | a subgroup project's token check, clone, push, merge request and gate 2 passed in a real sandbox, with glab 1.120.0 measured through the proxy and the proxy's refusals checked (2026-10-24); on the hosted server, a merge request job passed, and merging and pushing to another project were refused inside the sandbox (2026-11-03) |

Expected to work and untested: Codeberg, gitea.com (Gitea), a self-hosted
Forgejo or Gitea, and a self-managed GitLab. A self-managed GitLab served under
a path such as `/gitlab` is configured for glab and the proxy, and has not been
run.
