package sandbox

import (
	"strconv"
	"strings"
	"time"
)

// Tags spov attaches to every sandbox it creates. Tags are the only
// filterable discovery mechanism, and the only way `spov ls` and
// `spov stop` can find sandboxes after the creating process is gone.
//
// @decision(DL-029)
const (
	// TagSpov marks a sandbox as spov's. Always "1".
	TagSpov = "spov"
	// TagName is the human-facing session name.
	TagName = "name"
	// TagCreatedAt is an RFC3339 timestamp, used to report age and cost.
	TagCreatedAt = "created_at"
	// TagUser identifies who launched the session. In hosted mode it is
	// the user's ULID, which survives a renamed account and is the only
	// stable key for attribution; local mode tags "local".
	//
	// @decision(DL-009)
	TagUser = "user"
	// TagOrg is the organisation the session belongs to.
	//
	// @decision(DL-009)
	TagOrg = "org"
	// TagRepos is a comma-separated list of bootstrapped repo slugs.
	TagRepos = "repos"
	// TagJob is the job a sandbox is running, when it runs one.
	//
	// @decision(DL-046)
	TagJob = "job"
	// TagProxy is "1" on a sandbox launched with the credential proxy
	// sidecar, so its forge tokens are never written into its main
	// container, even when the sidecar has died.
	//
	// @decision(DL-034)
	TagProxy = "proxy"
	// TagProxyRecreated is "1" once the server has recreated the sandbox's
	// proxy sidecar, so a second death ends the sandbox instead.
	//
	// @decision(DL-034)
	TagProxyRecreated = "proxy-recreated"
	// TagRun is the session's run the sandbox is, from 1. A resumed
	// session's sandboxes differ by it.
	//
	// @decision(DL-084) @decision(DL-087)
	TagRun = "run"
)

// JobTags is the tag filter matching the sandbox running one job.
//
// @decision(DL-046)
func JobTags(jobID string) map[string]string {
	return map[string]string{TagSpov: "1", TagJob: jobID}
}

// OwnedTags is the tag filter matching sandboxes spov created.
func OwnedTags() map[string]string {
	return map[string]string{TagSpov: "1"}
}

// TagSet is what BuildTags needs. It is a struct rather than positional
// arguments because the set grew past what a reader can keep straight.
//
// @decision(DL-009)
type TagSet struct {
	Name string
	// User and Org attribute the sandbox. In hosted mode both are
	// ULIDs; in local mode both are "local".
	User      string
	Org       string
	Repos     []string
	CreatedAt time.Time
	// Job is the job this sandbox runs, empty for an interactive
	// session.
	//
	// @decision(DL-046)
	Job string
	// Proxy marks a sandbox launched with the credential proxy.
	//
	// @decision(DL-034)
	Proxy bool
	// Run is the session's run, or 0 when untagged.
	//
	// @decision(DL-084)
	Run int
}

// BuildTags assembles the tag set for a new sandbox.
//
// @decision(DL-029) @decision(DL-009)
func BuildTags(t TagSet) map[string]string {
	tags := map[string]string{
		TagSpov:      "1",
		TagCreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
	}
	if t.Name != "" {
		tags[TagName] = t.Name
	}
	if t.User != "" {
		tags[TagUser] = t.User
	}
	if t.Org != "" {
		tags[TagOrg] = t.Org
	}
	if len(t.Repos) > 0 {
		tags[TagRepos] = strings.Join(t.Repos, ",")
	}
	if t.Job != "" {
		tags[TagJob] = t.Job
	}
	if t.Proxy {
		tags[TagProxy] = "1"
	}
	if t.Run > 0 {
		tags[TagRun] = strconv.Itoa(t.Run)
	}
	return tags
}

// isSandboxID reports whether ref looks like a Modal sandbox ID rather
// than a session name.
func isSandboxID(ref string) bool {
	return strings.HasPrefix(ref, "sb-")
}

// ReposFromTags reads back the repository slugs BuildTags wrote.
func ReposFromTags(tags map[string]string) []string {
	raw := tags[TagRepos]
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}
