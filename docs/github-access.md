# GitHub access on a hosted spov

Four separate things decide what a sandbox can do with GitHub. They are
easy to mix up, so this page takes them one at a time (DL-042).

## 1. Signing in to spov

`spov login` (the device flow) or the browser login proves who you are to
spov. It gives no repository access at all: it only says which user's
credentials and quotas apply.

## 2. The organisation's GitHub App

One GitHub App can serve everyone on the deployment. Install it on the
GitHub account or organisation whose repositories people should use, then
give spov the App's ID and private key in one of two ways:

- **In Settings, as an admin.** The Organisation section stores the App
  sealed in the database under `SPOV_CREDENTIAL_KEY`. The form takes the
  App's ID, its private key and, optionally, the account it is installed on
  (the platform's configured account when left empty; with neither, the
  store is refused). spov checks it first (it finds the installation, reads
  its permissions, mints one token and lists what it reaches) and stores
  nothing that fails. The key is never shown again, and every change is
  recorded with who made it (DL-042).
- **In the deployment's environment**, as `SPOV_GITHUB_APP_ID` and
  `SPOV_GITHUB_APP_PRIVATE_KEY` (both; one alone is ignored), or
  `SPOV_PLATFORM_TOKEN_<NAME>`.

**The environment wins.** When it sets an App, Settings shows its status and
says it is configured there, and refuses to change it. To manage the App in
Settings instead, remove those variables, deploy, and store the App there.

The status shows where the App is installed, how many repositories it
reaches, and any permission sandboxes ask for that it lacks (contents, pull
requests and issues write, metadata read).

If the credential key changes so the stored App cannot be opened, launches
that need it are told to ask an admin, and the Organisation section says so
and offers to replace or remove it.

**Replacing or removing it** applies from the next token. A running session
keeps the token it holds until it expires, which is at most an hour. Its next
renewal mints from the new App. If the new App does not reach that session's
repositories, the renewal fails and the session's log gets a quiet note, once
per run of failures, saying the git token could not be renewed and when git
access through it ends (DL-021). A session nobody is using is not renewed,
so its note appears once someone uses it again. After a removal, nobody has
the organisation's credential for that platform.

## 3. Being allowed onto spov

The allowlist (an admin's, on the Settings page or in the API) decides who may sign in. **Everyone on
it may use every repository the organisation's App reaches.** There are no
per-repository grants. The App's installation scope and the allowlist together
are the boundary.

## 4. A sandbox's git access

Each launch's git credentials reach either the session's own repositories
(`repos`, the default) or every repository the credential reaches (`all`;
`--git-access`, the job form, or your defaults). On the organisation's
credential, `all` also needs the deployment's consent (`git.allow_all` /
`SPOV_GIT_ALLOW_ALL`). Without it, a launch that named `all` itself is
refused, and one that got `all` from a saved default or your CLI config runs
on `repos` and says so (DL-043). Your own token needs no such consent.

A sandbox does not merge a pull request over GitHub's REST endpoint
(`PUT /repos/<owner>/<name>/pulls/<n>/merge`): the credential proxy forwards
that request with no credential, for the organisation's App token as for a
personal access token, and the agent sees `401 Requires authentication`
(DL-139). This is a guardrail and not a boundary. `POST /graphql` is allowed
for both kinds of token and its body is not read, so the `mergePullRequest`
mutation still merges, and either token can push to a branch that is not
protected. A project that must hold a merge sets branch protection or a ruleset
on GitHub.

## Whose credential a launch uses

Someone who stores their own GitHub token (`spov credentials add github`) has
two tiers on the platform: their own and the organisation's. A launch uses
**their own first**, and the organisation's only when they have none. A launch
from the CLI can prefer the shared one per platform, in the user config file:

```yaml
platforms:
  - name: github
    kind: github
    prefer: shared   # personal (the default) | shared
```

The other tier is used only when the preferred one has no credential. A
credential that exists but cannot be read is an error, never a reason to switch
(DL-042).

A launch on your own token first asks GitHub whether the token can see each
selected repository. If GitHub answers that it cannot (a 404), the launch is
refused at once, before any sandbox exists, naming the credential and the
repository. Any other answer, a rate limit among them, lets the launch go on.
The check is about reach, not write access (DL-043). The organisation's App
needs no check: minting its token for a repository it cannot reach already
fails.

**The tier a launch used is recorded and kept.** Token renewal, a recreated
proxy sidecar, picking a job up again after a server restart, and checking a
job's pull requests all use the recorded tier. If that tier's credential is
gone, the step fails rather than using the other one.

In the browser's repository picker, a repository only the other tier reaches
is shown, disabled, with the reason: for example "only the organisation's
credential reaches this, and a launch from the browser uses your own
credential". The CLI's picker leaves such a repository out and says how many,
and whether `prefer: shared` (or removing it) would reach them. The session
page names whose credential each platform used (DL-042).

A fine-grained token lists every public repository its account can read, and
GitHub reports the account's role there, not what the token was granted. Such
a public repository is listed with a note that your token can read it and can
push only if you gave it that repository, never hidden. A launch through the
organisation's credential drops the note (DL-042).
